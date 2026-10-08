// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package remotes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"golang.org/x/sys/unix"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo/repotest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

func TestMain(m *testing.M) {
	cleanup, err := repotest.InitTransport()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// lockOAuthPort serializes tests that use rclone's fixed OAuth port
// across test processes.
func lockOAuthPort(t *testing.T) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(os.TempDir(), "pve-rclone-backup-oauth-test.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() })
}

func newManager(users map[string][]string) *Manager {
	var mu sync.Mutex
	return New(Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), StepWait: 2 * time.Second,
		Users: func() map[string][]string { mu.Lock(); defer mu.Unlock(); return users }})
}

// fakeProvider is an OAuth token endpoint.
func fakeProvider(t *testing.T) (*httptest.Server, *string) {
	t.Helper()
	var code string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		code = r.PostForm.Get("code")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fake-access", "refresh_token": "fake-refresh",
			"token_type": "Bearer", "expires_in": 3600})
	}))
	t.Cleanup(srv.Close)
	return srv, &code
}

func oauthParams(srv *httptest.Server) map[string]string {
	return map[string]string{"client_id": "test-client", "client_secret": "test-secret",
		"auth_url": srv.URL + "/authorize", "token_url": srv.URL + "/token"}
}

func TestOAuthSetupWithRelay(t *testing.T) {
	lockOAuthPort(t)
	srv, code := fakeProvider(t)
	m := newManager(nil)
	ctx := t.Context()

	s, err := m.Start(ctx, apiv1.RemoteSetupRequest{Name: "od", Provider: "onedrive", Params: oauthParams(srv)}, false)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != apiv1.SetupQuestion || s.Option == nil || s.Option.Name != "config_is_local" {
		t.Fatalf("first step = %+v", s)
	}
	if _, err := m.Start(ctx, apiv1.RemoteSetupRequest{Name: "other", Provider: "onedrive"}, false); !errors.Is(err, ErrBusy) {
		t.Fatalf("second session: %v", err)
	}
	if _, err := m.Answer(ctx, s.ID, apiv1.RemoteSetupAnswer{State: "stale", Result: "true"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("answer to another question: %v", err)
	}

	s, err = m.Answer(ctx, s.ID, apiv1.RemoteSetupAnswer{State: s.State, Result: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != apiv1.SetupAuthorizing || !strings.HasPrefix(s.AuthURL, srv.URL+"/authorize?") {
		t.Fatalf("authorization step = %+v", s)
	}
	if st, err := m.Status(s.ID); err != nil || st.AuthURL != s.AuthURL {
		t.Fatalf("status = %+v, %v", st, err)
	}
	u, _ := url.Parse(s.AuthURL)
	if _, err := m.Redirect(ctx, s.ID, "https://evil.example/?code=x&state="+u.Query().Get("state")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("foreign redirect: %v", err)
	}
	s, err = m.Redirect(ctx, s.ID, "http://localhost:53682/?code=the-code&state="+url.QueryEscape(u.Query().Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	// OneDrive asks which drive to use next; reaching it proves the token
	// exchange worked.
	if s.Status != apiv1.SetupQuestion || s.Option == nil || s.Option.Name != "config_type" || *code != "the-code" {
		t.Fatalf("after authorization = %+v (code %q)", s, *code)
	}
	r, err := m.Get("od")
	if err != nil || !r.Authorized || r.TokenExpiry == nil || !r.CustomApp || r.Type != "onedrive" {
		t.Fatalf("remote = %+v, %v", r, err)
	}
	raw, _ := json.Marshal(m.List())
	for _, secret := range []string{"fake-refresh", "fake-access", "test-secret"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("remote listing leaks %q: %s", secret, raw)
		}
	}

	// Abandoning the incomplete setup removes the new remote.
	if err := m.Finish(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get("od"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("incomplete remote kept: %v", err)
	}
	if _, err := m.Status(s.ID); !errors.Is(err, ErrNoSession) {
		t.Fatalf("finished session: %v", err)
	}
}

func TestAbortWhileAuthorizing(t *testing.T) {
	lockOAuthPort(t)
	srv, _ := fakeProvider(t)
	m := newManager(nil)
	ctx := t.Context()
	s, err := m.Start(ctx, apiv1.RemoteSetupRequest{Name: "od2", Provider: "onedrive", Params: oauthParams(srv)}, false)
	if err != nil {
		t.Fatal(err)
	}
	s, err = m.Answer(ctx, s.ID, apiv1.RemoteSetupAnswer{State: s.State, Result: "true"})
	if err != nil || s.Status != apiv1.SetupAuthorizing {
		t.Fatalf("authorizing = %+v, %v", s, err)
	}
	if err := m.Finish(s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := transport.ProviderAuthURL(ctx); !errors.Is(err, transport.ErrOAuthNotRunning) {
		t.Fatalf("OAuth listener still running: %v", err)
	}
	if config.LoadedData().HasSection("od2") {
		t.Fatal("aborted remote kept")
	}
	// A new session can start afterwards.
	s, err = m.Start(ctx, apiv1.RemoteSetupRequest{Name: "od3", Provider: "onedrive", Params: oauthParams(srv)}, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.Finish(s.ID)
}

func TestStartValidation(t *testing.T) {
	m := newManager(nil)
	ctx := t.Context()
	loc, _ := repotest.Remote(t, nil)
	for name, req := range map[string]apiv1.RemoteSetupRequest{
		"bad name":        {Name: "Bad Name", Provider: "onedrive"},
		"unknown option":  {Name: "x1", Provider: "onedrive", Params: map[string]string{"nonsense": "1"}},
		"unsupported":     {Name: "x2", Provider: "local"},
		"existing remote": {Name: loc.Remote, Provider: "onedrive"},
	} {
		if _, err := m.Start(ctx, req, false); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := m.Start(ctx, apiv1.RemoteSetupRequest{Name: "missing"}, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("reconnect of a missing remote: %v", err)
	}
}

func TestReconnectWithoutQuestions(t *testing.T) {
	m := newManager(nil)
	loc, _ := repotest.Remote(t, nil)
	s, err := m.Start(t.Context(), apiv1.RemoteSetupRequest{Name: loc.Remote}, true)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != apiv1.SetupDone || !s.Reconnect {
		t.Fatalf("reconnect = %+v", s)
	}
	if err := m.Finish(s.ID); err != nil {
		t.Fatal(err)
	}
	if !config.LoadedData().HasSection(loc.Remote) {
		t.Fatal("reconnected remote removed")
	}
}

// The colourtest backend asks for a colour and, like OneDrive's drive check, sends
// a refused answer back to the question with an error and no question of
// its own.
func init() {
	fs.Register(&fs.RegInfo{
		Name: "colourtest",
		NewFs: func(context.Context, string, string, configmap.Mapper) (fs.Fs, error) {
			return nil, errors.New("not a real backend")
		},
		Config: func(_ context.Context, _ string, _ configmap.Mapper, in fs.ConfigIn) (*fs.ConfigOut, error) {
			switch in.State {
			case "", "ask":
				return fs.ConfigInput("check", "colour", "Favourite colour?")
			case "check":
				if in.Result != "blue" {
					return fs.ConfigError("ask", "only blue works")
				}
				return nil, nil
			}
			return nil, fmt.Errorf("unknown state %q", in.State)
		},
	})
}

func TestRefusedAnswerIsAskedAgain(t *testing.T) {
	m := newManager(nil)
	config.LoadedData().SetValue("colours", "type", "colourtest")
	t.Cleanup(func() { config.LoadedData().DeleteSection("colours") })
	s, err := m.Start(t.Context(), apiv1.RemoteSetupRequest{Name: "colours"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != apiv1.SetupQuestion || s.Option == nil || s.Option.Name != "colour" {
		t.Fatalf("first step = %+v", s)
	}
	s, err = m.Answer(t.Context(), s.ID, apiv1.RemoteSetupAnswer{State: s.State, Result: "red"})
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != apiv1.SetupQuestion || s.Option == nil || s.Option.Name != "colour" || s.Error != "only blue works" {
		t.Fatalf("after a refused answer = %+v", s)
	}
	s, err = m.Answer(t.Context(), s.ID, apiv1.RemoteSetupAnswer{State: s.State, Result: "blue"})
	if err != nil || s.Status != apiv1.SetupDone {
		t.Fatalf("after a good answer = %+v, %v", s, err)
	}
	_ = m.Finish(s.ID)
}

func TestListDeleteTest(t *testing.T) {
	loc, _ := repotest.Remote(t, nil)
	other, _ := repotest.Remote(t, nil)
	m := newManager(map[string][]string{loc.Remote: {"offsite"}})
	// Supported means the backend has a profile (the test backend does),
	// whether or not remote setup offers it.
	r, err := m.Get(loc.Remote)
	if err != nil || len(r.Storages) != 1 || !r.Supported {
		t.Fatalf("remote = %+v, %v", r, err)
	}
	repotest.Storage.SetSection("unprofiled", map[string]string{"type": "sftp"})
	defer repotest.Storage.DeleteSection("unprofiled")
	if r, err := m.Get("unprofiled"); err != nil || r.Supported {
		t.Fatalf("remote without a profile = %+v, %v", r, err)
	}
	if err := m.Delete(loc.Remote); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete of a used remote: %v", err)
	}
	res, err := m.Test(t.Context(), other.Remote)
	if err != nil || res.HashType == "" {
		t.Fatalf("probe = %+v, %v", res, err)
	}
	if u, err := m.About(t.Context(), other.Remote); err != nil || u != nil {
		t.Fatalf("about = %+v, %v", u, err)
	}
	if err := m.Delete(other.Remote); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(other.Remote); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted remote: %v", err)
	}
	if err := m.Delete(other.Remote); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if len(Providers()) != 1 || Providers()[0].Name != "onedrive" || !Providers()[0].RecycleBin {
		t.Fatalf("providers = %+v", Providers())
	}
}
