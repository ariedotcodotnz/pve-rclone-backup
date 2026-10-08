// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package transport

import (
	"encoding/json"
	"errors"
	"net"
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
	"github.com/rclone/rclone/fs/rc"
	"golang.org/x/sys/unix"
)

// lockOAuthPort serializes tests that use rclone's fixed OAuth port
// across test processes (internal/remotes has the same helper).
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

type oauthResult struct {
	out *fs.ConfigOut
	err error
}

// beginOAuth starts rclone's non-interactive OneDrive configuration of
// remote name against a fake OAuth provider and waits until rclone's
// loopback listener runs. It returns the flow's state parameter, the
// configuration step's result, and the code the provider received.
func beginOAuth(t *testing.T, name string) (state string, done <-chan oauthResult, gotCode func() string) {
	t.Helper()
	lockOAuthPort(t)
	if l, err := net.Listen("tcp", oauthBindAddress); err != nil {
		t.Skipf("rclone OAuth port busy: %v", err)
	} else {
		_ = l.Close()
	}

	var mu sync.Mutex
	var code string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		mu.Lock()
		code = r.PostForm.Get("code")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fake-access",
			"refresh_token": "fake-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	t.Cleanup(provider.Close)

	ctx := t.Context()
	t.Cleanup(func() { testStorage.DeleteSection(name) })
	out, err := config.CreateRemote(ctx, name, "onedrive", rc.Params{
		"client_id":     "test-client",
		"client_secret": "test-secret",
		"auth_url":      provider.URL + "/authorize",
		"token_url":     provider.URL + "/token",
	}, config.UpdateRemoteOpt{NonInteractive: true})
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || out.Option == nil || out.Option.Name != "config_is_local" {
		t.Fatalf("first question = %+v, want config_is_local", out)
	}

	results := make(chan oauthResult, 1)
	go func() {
		o, err := config.UpdateRemote(ctx, name, rc.Params{config.ConfigAuthNoBrowser: "true"},
			config.UpdateRemoteOpt{Continue: true, State: out.State, Result: "true"})
		results <- oauthResult{o, err}
	}()

	var authURL string
	deadline := time.Now().Add(10 * time.Second)
	for {
		authURL, err = ProviderAuthURL(ctx)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrOAuthNotRunning) || time.Now().After(deadline) {
			t.Fatalf("ProviderAuthURL: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.HasPrefix(authURL, provider.URL+"/authorize?") {
		t.Fatalf("provider URL %q does not point at the provider", authURL)
	}
	au, _ := url.Parse(authURL)
	state = au.Query().Get("state")
	if state == "" || au.Query().Get("redirect_uri") != "http://localhost:53682/" {
		t.Fatalf("unexpected provider URL parameters: %v", au.Query())
	}
	return state, results, func() string { mu.Lock(); defer mu.Unlock(); return code }
}

// TestOAuthRelayFlow drives rclone's non-interactive OneDrive configuration
// against a fake OAuth provider: the daemon resolves the provider URL for
// the user, the user "pastes" the failed localhost redirect, and the daemon
// relays it so rclone can exchange the code for a token.
func TestOAuthRelayFlow(t *testing.T) {
	ctx := t.Context()
	const name = "oauthtest"
	state, done, gotCode := beginOAuth(t, name)

	// Rejected inputs never reach rclone.
	for _, bad := range []string{
		"https://example.com/?code=x&state=" + state,
		"http://localhost:53682/?state=" + state,
		"http://localhost:9999/?code=x&state=" + state,
		"not a url\x7f",
	} {
		if err := RelayRedirect(ctx, bad); !errors.Is(err, ErrInvalidRedirect) {
			t.Errorf("RelayRedirect(%q) = %v, want ErrInvalidRedirect", bad, err)
		}
	}

	pasted := "http://localhost:53682/?code=the-code&state=" + url.QueryEscape(state)
	if err := RelayRedirect(ctx, pasted); err != nil {
		t.Fatal(err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("config continued with error: %v", r.err)
		}
		// OneDrive asks for the connection type next; reaching it proves
		// the token exchange succeeded.
		if r.out == nil || r.out.Option == nil || r.out.Option.Name != "config_type" {
			t.Fatalf("next question = %+v, want config_type", r.out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("config did not continue after relaying the redirect")
	}

	if got := gotCode(); got != "the-code" {
		t.Fatalf("provider received code %q", got)
	}
	token, _ := testStorage.GetValue(name, "token")
	if !strings.Contains(token, "fake-refresh") {
		t.Fatalf("token not stored in config: %q", token)
	}
	if _, ok := testStorage.GetValue(name, config.ConfigAuthNoBrowser); ok {
		t.Fatal("ephemeral config_auth_no_browser was persisted")
	}
	if _, running, _ := oauthStatus(ctx); running {
		t.Fatal("OAuth listener still running after completion")
	}
}

// TestOAuthRelayRefusesAnotherSignIn: an address from an earlier sign-in
// carries another state. It is refused without being relayed, since rclone
// would end the authorization over it, and the right address still works.
func TestOAuthRelayRefusesAnotherSignIn(t *testing.T) {
	ctx := t.Context()
	state, done, gotCode := beginOAuth(t, "oauthstale")

	stale := "http://localhost:53682/?code=old-code&state=" + url.QueryEscape(state+"-old")
	if err := RelayRedirect(ctx, stale); !errors.Is(err, ErrInvalidRedirect) {
		t.Fatalf("relaying an address from another sign-in = %v, want ErrInvalidRedirect", err)
	}
	select {
	case r := <-done:
		t.Fatalf("authorization ended over a refused address: %+v, %v", r.out, r.err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := RelayRedirect(ctx, "http://localhost:53682/?code=new-code&state="+url.QueryEscape(state)); err != nil {
		t.Fatalf("relaying the right address after a refused one: %v", err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("authorization failed: %v", r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("authorization did not complete")
	}
	if got := gotCode(); got != "new-code" {
		t.Fatalf("provider received code %q, want new-code", got)
	}
}
