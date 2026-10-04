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
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/rc"
)

// TestOAuthRelayFlow drives rclone's non-interactive OneDrive configuration
// against a fake OAuth provider: the daemon resolves the provider URL for
// the user, the user "pastes" the failed localhost redirect, and the daemon
// relays it so rclone can exchange the code for a token.
func TestOAuthRelayFlow(t *testing.T) {
	if l, err := net.Listen("tcp", oauthBindAddress); err != nil {
		t.Skipf("rclone OAuth port busy: %v", err)
	} else {
		_ = l.Close()
	}

	var gotCode string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		gotCode = r.PostForm.Get("code")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "fake-access",
			"refresh_token": "fake-refresh",
			"token_type":    "Bearer",
			"expires_in":    3600,
		})
	}))
	defer provider.Close()

	ctx := t.Context()
	const name = "oauthtest"
	defer testStorage.DeleteSection(name)

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

	type result struct {
		out *fs.ConfigOut
		err error
	}
	done := make(chan result, 1)
	go func() {
		o, err := config.UpdateRemote(ctx, name, rc.Params{config.ConfigAuthNoBrowser: "true"},
			config.UpdateRemoteOpt{Continue: true, State: out.State, Result: "true"})
		done <- result{o, err}
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
	state := au.Query().Get("state")
	if state == "" || au.Query().Get("redirect_uri") != "http://localhost:53682/" {
		t.Fatalf("unexpected provider URL parameters: %v", au.Query())
	}

	// Rejected inputs never reach rclone.
	for _, bad := range []string{
		"https://example.com/?code=x&state=" + state,
		"http://localhost:53682/?state=" + state,
		"http://localhost:9999/?code=x&state=" + state,
		"not a url\x7f",
	} {
		if err := RelayRedirect(ctx, bad); err == nil {
			t.Errorf("RelayRedirect(%q) accepted", bad)
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

	if gotCode != "the-code" {
		t.Fatalf("provider received code %q", gotCode)
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
