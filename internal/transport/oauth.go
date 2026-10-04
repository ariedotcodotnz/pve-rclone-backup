// SPDX-License-Identifier: AGPL-3.0-or-later

package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/rclone/rclone/fs/rc"
)

// rclone's OAuth helper binds this loopback address while it waits for the
// provider to redirect back with an authorization code.
const oauthBindAddress = "127.0.0.1:53682"

// ErrOAuthNotRunning means no OAuth flow is currently waiting for a code.
var ErrOAuthNotRunning = errors.New("transport: no OAuth authorization in progress")

// noRedirectClient talks to rclone's loopback auth server without following
// redirects.
var noRedirectClient = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func oauthStatus(ctx context.Context) (localURL string, running bool, err error) {
	call := rc.Calls.Get("config/oauthstatus")
	if call == nil {
		return "", false, errors.New("transport: rclone has no config/oauthstatus call")
	}
	out, err := call.Fn(ctx, rc.Params{})
	if err != nil {
		return "", false, fmt.Errorf("transport: oauth status: %w", err)
	}
	if status, _ := out["status"].(string); status != "running" {
		return "", false, nil
	}
	localURL, _ = out["authUrl"].(string)
	return localURL, localURL != "", nil
}

// ProviderAuthURL returns the provider's authorization URL for the OAuth
// flow that is currently waiting for a code.
//
// rclone only publishes a loopback URL that redirects to the provider. That
// is useless to a browser on another machine, so the redirect is resolved
// here and the provider URL itself is returned.
func ProviderAuthURL(ctx context.Context) (string, error) {
	localURL, running, err := oauthStatus(ctx)
	if err != nil {
		return "", err
	}
	if !running {
		return "", ErrOAuthNotRunning
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, localURL, nil)
	if err != nil {
		return "", fmt.Errorf("transport: oauth status URL: %w", err)
	}
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("transport: resolve provider auth URL: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTemporaryRedirect && resp.StatusCode != http.StatusFound {
		return "", fmt.Errorf("transport: resolve provider auth URL: unexpected status %s", resp.Status)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", errors.New("transport: resolve provider auth URL: no Location header")
	}
	return loc, nil
}

// RelayRedirect completes a pending OAuth flow with the redirect URL the
// user copied from their browser, typically
// http://localhost:53682/?code=...&state=... The browser could not load it
// because rclone's listener runs on this host, not on the user's machine.
//
// Only loopback redirect URLs for rclone's port are accepted; the state is
// checked by rclone itself.
func RelayRedirect(ctx context.Context, pasted string) error {
	u, err := url.Parse(pasted)
	if err != nil {
		return fmt.Errorf("transport: not a URL: %w", err)
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil || port != "53682" || (host != "localhost" && host != "127.0.0.1") {
		return fmt.Errorf("transport: expected a redirect to http://localhost:53682/, got host %q", u.Host)
	}
	if u.Scheme != "http" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("transport: expected a redirect to http://localhost:53682/, got %s://%s%s", u.Scheme, u.Host, u.Path)
	}
	q := u.Query()
	if q.Get("state") == "" || (q.Get("code") == "" && q.Get("error") == "") {
		return errors.New("transport: redirect URL lacks state and code; copy the complete address from the browser")
	}
	if _, running, err := oauthStatus(ctx); err != nil {
		return err
	} else if !running {
		return ErrOAuthNotRunning
	}

	relay := url.URL{Scheme: "http", Host: oauthBindAddress, Path: "/", RawQuery: q.Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, relay.String(), nil)
	if err != nil {
		return fmt.Errorf("transport: relay redirect: %w", err)
	}
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return fmt.Errorf("transport: relay redirect: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("transport: relay redirect: auth server answered %s", resp.Status)
	}
	return nil
}
