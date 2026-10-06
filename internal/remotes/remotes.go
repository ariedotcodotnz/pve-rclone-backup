// SPDX-License-Identifier: AGPL-3.0-or-later

// Package remotes manages transport remotes: listing them without their
// credentials, testing them, and configuring or reconnecting them through
// rclone's non-interactive configuration state machine. OAuth providers
// are authorized with a relay: the user opens the provider URL on any
// device and pastes back the localhost redirect their browser could not
// load, which the daemon forwards to rclone's loopback listener.
package remotes

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/rc"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

var (
	// ErrNotFound means the remote is not configured.
	ErrNotFound = errors.New("remotes: remote not configured")
	// ErrInUse means a storage uses the remote.
	ErrInUse = errors.New("remotes: remote is used by a storage")
	// ErrExists means a remote with the name exists already.
	ErrExists = errors.New("remotes: remote exists already")
	// ErrBusy means another setup session is active (rclone's OAuth
	// listener can serve one at a time).
	ErrBusy = errors.New("remotes: another remote setup is in progress")
	// ErrNoSession means the setup session does not exist (any more).
	ErrNoSession = errors.New("remotes: no such setup session")
	// ErrInvalid marks invalid input.
	ErrInvalid = errors.New("remotes: invalid request")
)

// Options configures a Manager.
type Options struct {
	Log *slog.Logger
	// Users maps remote names to the storages using them.
	Users func() map[string][]string
	// StepWait is how long an answer waits for rclone before reporting
	// the session as still working (default 3s).
	StepWait time.Duration
	// IdleTimeout aborts abandoned sessions (default 15m).
	IdleTimeout time.Duration
	Now         func() time.Time
	// OnConfigured is called after a remote was set up or reconnected.
	OnConfigured func(name string)
}

// Manager owns the remotes and the setup session.
type Manager struct {
	opts Options
	log  *slog.Logger

	mu      sync.Mutex
	session *session
}

// New returns a manager.
func New(opts Options) *Manager {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Users == nil {
		opts.Users = func() map[string][]string { return nil }
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	opts.StepWait = cmp.Or(opts.StepWait, 3*time.Second)
	opts.IdleTimeout = cmp.Or(opts.IdleTimeout, 15*time.Minute)
	return &Manager{opts: opts, log: opts.Log}
}

// Providers lists the supported backends.
func Providers() []apiv1.Provider {
	var out []apiv1.Provider
	for _, name := range transport.SetupProviders() {
		p, err := transport.ProfileFor(name)
		if err != nil {
			continue
		}
		desc := name
		if ri, err := fs.Find(name); err == nil {
			desc = ri.Description
		}
		out = append(out, apiv1.Provider{Name: name, Description: desc, MaxObjectSize: p.MaxObjectSize, RecycleBin: p.RecycleBin})
	}
	return out
}

func configured(name string) bool { return config.LoadedData().HasSection(name) }

// List returns the configured remotes.
func (m *Manager) List() []apiv1.Remote {
	users := m.opts.Users()
	var out []apiv1.Remote
	for _, name := range slices.Sorted(slices.Values(config.LoadedData().GetSectionList())) {
		out = append(out, view(name, users[name]))
	}
	return out
}

// Get returns one remote.
func (m *Manager) Get(name string) (apiv1.Remote, error) {
	if !configured(name) {
		return apiv1.Remote{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return view(name, m.opts.Users()[name]), nil
}

// view describes a remote from its non-secret settings.
func view(name string, users []string) apiv1.Remote {
	data := config.LoadedData()
	get := func(k string) string { v, _ := data.GetValue(name, k); return v }
	r := apiv1.Remote{Name: name, Type: get("type"), DriveType: get("drive_type"), DriveID: get("drive_id"),
		CustomApp: get("client_id") != "", Storages: append([]string{}, users...)}
	_, err := transport.ProfileFor(r.Type)
	r.Supported = err == nil
	if tok := get("token"); tok != "" {
		var t struct {
			RefreshToken string    `json:"refresh_token"`
			Expiry       time.Time `json:"expiry"`
		}
		if json.Unmarshal([]byte(tok), &t) == nil {
			r.Authorized = t.RefreshToken != ""
			if !t.Expiry.IsZero() {
				e := t.Expiry.UTC()
				r.TokenExpiry = &e
			}
		}
	}
	return r
}

// Delete removes a remote that no storage uses.
func (m *Manager) Delete(name string) error {
	if !configured(name) {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if users := m.opts.Users()[name]; len(users) > 0 {
		return fmt.Errorf("%w: %s is used by %s", ErrInUse, name, strings.Join(users, ", "))
	}
	m.mu.Lock()
	busy := m.session != nil && m.session.name == name
	m.mu.Unlock()
	if busy {
		return ErrBusy
	}
	config.DeleteRemote(name)
	m.log.Info("remote deleted", "remote", name)
	return nil
}

func root(ctx context.Context, name string) (*transport.Target, error) {
	if !configured(name) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return transport.NewTarget(ctx, name+":")
}

// Test writes, reads and deletes a probe object and queries the quota.
func (m *Manager) Test(ctx context.Context, name string) (*apiv1.ProbeResult, error) {
	t, err := root(ctx, name)
	if err != nil {
		return nil, err
	}
	res, err := t.Probe(ctx)
	if err != nil {
		return nil, err
	}
	out := &apiv1.ProbeResult{LatencyMS: res.Latency.Milliseconds(), HashType: res.HashType}
	if u := res.Usage; u != nil {
		out.Usage = &apiv1.Usage{Total: u.Total, Used: u.Used, Free: u.Free, Trashed: u.Trashed, UpdatedAt: m.opts.Now().UTC()}
	}
	return out, nil
}

// About returns the quota of a remote (nil when it reports none).
func (m *Manager) About(ctx context.Context, name string) (*apiv1.Usage, error) {
	t, err := root(ctx, name)
	if err != nil {
		return nil, err
	}
	u, err := t.About(ctx)
	if errors.Is(err, transport.ErrNoQuota) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &apiv1.Usage{Total: u.Total, Used: u.Used, Free: u.Free, Trashed: u.Trashed, UpdatedAt: m.opts.Now().UTC()}, nil
}

// session is a remote setup in progress.
type session struct {
	id        string
	name      string
	provider  string
	reconnect bool
	cancel    context.CancelFunc
	ctx       context.Context

	status  string
	out     *fs.ConfigOut
	errMsg  string
	authURL string
	running chan struct{} // closed when the current step finishes; nil when idle
	touched time.Time
}

func (s *session) view() apiv1.RemoteSetup {
	v := apiv1.RemoteSetup{ID: s.id, Name: s.name, Provider: s.provider, Reconnect: s.reconnect, Status: s.status,
		Error: s.errMsg, AuthURL: s.authURL}
	if s.status == apiv1.SetupQuestion && s.out != nil {
		v.State = s.out.State
		if s.out.Error != "" {
			v.Error = s.out.Error
		}
		if o := s.out.Option; o != nil {
			v.Option = optionView(o)
		}
	}
	return v
}

func optionView(o *fs.Option) *apiv1.SetupOption {
	v := &apiv1.SetupOption{Name: o.Name, Help: o.Help, Type: o.Type(), Required: o.Required, IsPassword: o.IsPassword, Exclusive: o.Exclusive}
	if o.Default != nil && !o.IsPassword {
		v.Default = fmt.Sprint(o.Default)
	}
	for _, e := range o.Examples {
		v.Examples = append(v.Examples, apiv1.SetupExample{Value: e.Value, Help: e.Help})
	}
	return v
}

// ephemeral are rclone settings passed with every step and never stored:
// the daemon has no browser to open.
var ephemeral = rc.Params{config.ConfigAuthNoBrowser: "true"}

// Start begins configuring a new remote, or reconnecting an existing one.
func (m *Manager) Start(ctx context.Context, req apiv1.RemoteSetupRequest, reconnect bool) (apiv1.RemoteSetup, error) {
	if !validName(req.Name) {
		return apiv1.RemoteSetup{}, fmt.Errorf("%w: invalid remote name %q (lower case letters, digits, '-' and '_')", ErrInvalid, req.Name)
	}
	provider := req.Provider
	if reconnect {
		if !configured(req.Name) {
			return apiv1.RemoteSetup{}, fmt.Errorf("%w: %s", ErrNotFound, req.Name)
		}
		provider, _ = config.LoadedData().GetValue(req.Name, "type")
	} else {
		if configured(req.Name) {
			return apiv1.RemoteSetup{}, fmt.Errorf("%w: %s", ErrExists, req.Name)
		}
		if !slices.Contains(transport.SetupProviders(), provider) {
			return apiv1.RemoteSetup{}, fmt.Errorf("%w: provider %q is not supported (supported: %s)", ErrInvalid, provider,
				strings.Join(transport.SetupProviders(), ", "))
		}
	}
	params, err := backendParams(provider, req.Params)
	if err != nil {
		return apiv1.RemoteSetup{}, err
	}

	m.mu.Lock()
	m.expireLocked()
	if m.session != nil {
		m.mu.Unlock()
		return apiv1.RemoteSetup{}, ErrBusy
	}
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &session{id: rand.Text()[:16], name: req.Name, provider: provider, reconnect: reconnect, ctx: sctx, cancel: cancel,
		touched: m.opts.Now()}
	m.session = s
	m.mu.Unlock()

	m.log.Info("remote setup started", "remote", req.Name, "provider", provider, "reconnect", reconnect)
	return m.step(s, func(ctx context.Context) (*fs.ConfigOut, error) {
		if reconnect {
			return config.UpdateRemote(ctx, s.name, params, config.UpdateRemoteOpt{NonInteractive: true, Obscure: true})
		}
		return config.CreateRemote(ctx, s.name, provider, params, config.UpdateRemoteOpt{NonInteractive: true, Obscure: true})
	})
}

func validName(name string) bool {
	if name == "" || len(name) > 63 || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for _, r := range name {
		lower, digit := r >= 'a' && r <= 'z', r >= '0' && r <= '9'
		if !lower && !digit && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// backendParams accepts only the backend's own options.
func backendParams(provider string, in map[string]string) (rc.Params, error) {
	ri, err := fs.Find(provider)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	out := maps.Clone(ephemeral)
	for k, v := range in {
		if !slices.ContainsFunc(ri.Options, func(o fs.Option) bool { return o.Name == k }) {
			return nil, fmt.Errorf("%w: %s has no option %q", ErrInvalid, provider, k)
		}
		out[k] = v
	}
	return out, nil
}

// Answer answers the current question.
func (m *Manager) Answer(ctx context.Context, id string, a apiv1.RemoteSetupAnswer) (apiv1.RemoteSetup, error) {
	s, err := m.get(id)
	if err != nil {
		return apiv1.RemoteSetup{}, err
	}
	m.mu.Lock()
	if s.running != nil || s.status != apiv1.SetupQuestion || s.out == nil || a.State != s.out.State {
		v := s.view()
		m.mu.Unlock()
		return v, fmt.Errorf("%w: the session is not waiting for an answer to this question", ErrInvalid)
	}
	m.mu.Unlock()
	return m.step(s, func(ctx context.Context) (*fs.ConfigOut, error) {
		return config.UpdateRemote(ctx, s.name, maps.Clone(ephemeral),
			config.UpdateRemoteOpt{NonInteractive: true, Continue: true, State: a.State, Result: a.Result, Obscure: true})
	})
}

// step runs one step of rclone's configuration in the background and
// waits briefly for it; OAuth steps keep running until the redirect is
// relayed.
func (m *Manager) step(s *session, fn func(context.Context) (*fs.ConfigOut, error)) (apiv1.RemoteSetup, error) {
	done := make(chan struct{})
	m.mu.Lock()
	s.running, s.status, s.authURL, s.errMsg = done, apiv1.SetupAuthorizing, "", ""
	s.touched = m.opts.Now()
	m.mu.Unlock()
	go func() {
		out, err := fn(s.ctx)
		m.mu.Lock()
		defer m.mu.Unlock()
		defer close(done)
		s.running, s.authURL, s.touched = nil, "", m.opts.Now()
		switch {
		case err != nil:
			s.status, s.errMsg = apiv1.SetupFailed, err.Error()
			m.log.Warn("remote setup failed", "remote", s.name, "err", err)
		case out == nil || out.State == "":
			s.status, s.out = apiv1.SetupDone, nil
			m.log.Info("remote configured", "remote", s.name, "reconnect", s.reconnect)
			if m.opts.OnConfigured != nil {
				go m.opts.OnConfigured(s.name)
			}
		default:
			s.status, s.out = apiv1.SetupQuestion, out
		}
	}()
	return m.wait(s, done), nil
}

// wait returns the session state once the step finished or, for steps
// that wait for an OAuth redirect, once the provider URL is known.
func (m *Manager) wait(s *session, done chan struct{}) apiv1.RemoteSetup {
	deadline := time.After(m.opts.StepWait)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			m.mu.Lock()
			defer m.mu.Unlock()
			return s.view()
		case <-tick.C:
			if u, err := transport.ProviderAuthURL(s.ctx); err == nil {
				m.mu.Lock()
				defer m.mu.Unlock()
				if s.running == done {
					s.authURL = u
				}
				return s.view()
			}
		case <-deadline:
			m.mu.Lock()
			defer m.mu.Unlock()
			return s.view()
		}
	}
}

// Status returns the session state.
func (m *Manager) Status(id string) (apiv1.RemoteSetup, error) {
	s, err := m.get(id)
	if err != nil {
		return apiv1.RemoteSetup{}, err
	}
	m.mu.Lock()
	running := s.running
	needURL := running != nil && s.authURL == ""
	m.mu.Unlock()
	if needURL {
		if u, err := transport.ProviderAuthURL(s.ctx); err == nil {
			m.mu.Lock()
			if s.running == running {
				s.authURL = u
			}
			m.mu.Unlock()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s.touched = m.opts.Now()
	return s.view(), nil
}

// Redirect relays the pasted OAuth redirect and waits for rclone to
// exchange the code.
func (m *Manager) Redirect(ctx context.Context, id, pasted string) (apiv1.RemoteSetup, error) {
	s, err := m.get(id)
	if err != nil {
		return apiv1.RemoteSetup{}, err
	}
	m.mu.Lock()
	done := s.running
	m.mu.Unlock()
	if done == nil {
		return apiv1.RemoteSetup{}, fmt.Errorf("%w: the session is not waiting for an authorization", ErrInvalid)
	}
	if err := transport.RelayRedirect(ctx, pasted); err != nil {
		return apiv1.RemoteSetup{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
	case <-ctx.Done():
		return apiv1.RemoteSetup{}, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return s.view(), nil
}

// Finish ends a completed or failed session.
func (m *Manager) Finish(id string) error {
	s, err := m.get(id)
	if err != nil {
		return err
	}
	m.abort(s, "finished")
	return nil
}

func (m *Manager) get(id string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.expireLocked()
	if m.session == nil || m.session.id != id {
		return nil, ErrNoSession
	}
	return m.session, nil
}

// expireLocked aborts an idle session in the background.
func (m *Manager) expireLocked() {
	if s := m.session; s != nil && m.opts.Now().Sub(s.touched) > m.opts.IdleTimeout {
		go m.abort(s, "idle")
	}
}

// abort ends a session: a pending OAuth wait is released with an error
// redirect, and a new remote that was not completed is removed.
func (m *Manager) abort(s *session, why string) {
	m.mu.Lock()
	if m.session != s {
		m.mu.Unlock()
		return
	}
	m.session = nil
	done, authURL := s.running, s.authURL
	m.mu.Unlock()

	if done != nil {
		if authURL == "" {
			authURL, _ = transport.ProviderAuthURL(context.Background())
		}
		if u, err := url.Parse(authURL); err == nil && u.Query().Get("state") != "" {
			deny := "http://localhost:53682/?error=access_denied&state=" + url.QueryEscape(u.Query().Get("state"))
			_ = transport.RelayRedirect(context.Background(), deny)
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			m.log.Warn("remote setup step did not stop", "remote", s.name)
		}
	}
	s.cancel()
	m.mu.Lock()
	status := s.status
	m.mu.Unlock()
	if !s.reconnect && status != apiv1.SetupDone {
		config.DeleteRemote(s.name)
	}
	m.log.Info("remote setup ended", "remote", s.name, "status", status, "reason", why)
}
