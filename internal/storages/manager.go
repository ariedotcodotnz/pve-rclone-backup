// SPDX-License-Identifier: AGPL-3.0-or-later

// Package storages tracks the rclone-backup storages configured in
// storage.cfg. It opens their repositories in the background and caches
// what PVE asks for: health, quota and the catalogue. Requests are answered
// from that cache and never wait for the network.
package storages

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/catalog"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/pve/storagecfg"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/secrets"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

// Options configures a Manager.
type Options struct {
	Log   *slog.Logger
	Store *store.Store
	Node  string
	// PVEDir is the root of the cluster file system (/etc/pve).
	PVEDir string
	Keys   repo.KeyLoader
	// Intervals of background work (defaults from the daemon schema).
	AboutInterval  time.Duration
	ResyncInterval time.Duration
	// OpenTimeout bounds opening a repository and each background
	// operation (default 2m).
	OpenTimeout time.Duration
	// Tick is how often storage.cfg is checked for changes (default 5s).
	Tick time.Duration
	// OnChange is called after a storage's state changed.
	OnChange func(id string)
	Now      func() time.Time
}

// Manager owns the per-storage state.
type Manager struct {
	opts  Options
	log   *slog.Logger
	wake  chan struct{}
	wg    sync.WaitGroup
	cfgMu sync.Mutex // serializes reloads

	mu      sync.Mutex
	entries map[string]*entry
	stamp   fileStamp
	cfg     *storagecfg.Config
}

type fileStamp struct {
	exists bool
	mod    time.Time
	size   int64
	ino    uint64
}

type entry struct {
	id      string
	binding string // hash of what identifies the repository
	cfg     *config.Storage
	cfgErr  error
	enabled bool

	repo         *repo.Repo
	repoUUID     string
	generation   int
	health       string
	detail       string
	usage        *apiv1.Usage
	lastResync   time.Time
	failures     int
	nextAttempt  time.Time
	nextAbout    time.Time
	resyncWanted bool
	busy         bool
}

// New returns a manager. Call Reload once before serving and Run in the
// background.
func New(opts Options) *Manager {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	defaults := config.DefaultDaemonConfig()
	opts.AboutInterval = cmp.Or(opts.AboutInterval, defaults.AboutInterval)
	opts.ResyncInterval = cmp.Or(opts.ResyncInterval, defaults.CatalogResyncInterval)
	opts.OpenTimeout = cmp.Or(opts.OpenTimeout, 2*time.Minute)
	opts.Tick = cmp.Or(opts.Tick, 5*time.Second)
	return &Manager{opts: opts, log: opts.Log, wake: make(chan struct{}, 1), entries: map[string]*entry{}}
}

func (m *Manager) storageCfgPath() string { return filepath.Join(m.opts.PVEDir, "storage.cfg") }

// Wake triggers background work now.
func (m *Manager) Wake() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) changed(id string) {
	if m.opts.OnChange != nil {
		m.opts.OnChange(id)
	}
}

// bindingHash covers the settings that identify the repository and
// namespace a storage reads; changing any of them invalidates its
// catalogue.
func bindingHash(c *config.Storage) string {
	h := sha256.Sum256([]byte(strings.Join([]string{c.Remote, c.Path, c.Encryption, c.Source}, "\x00")))
	return hex.EncodeToString(h[:16])
}

func stampOf(path string) (fileStamp, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileStamp{}, nil
	}
	if err != nil {
		return fileStamp{}, err
	}
	st := fileStamp{exists: true, mod: fi.ModTime(), size: fi.Size()}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		st.ino = sys.Ino
	}
	return st, nil
}

// Reload re-reads storage.cfg if it changed.
func (m *Manager) Reload(ctx context.Context) error {
	m.cfgMu.Lock()
	defer m.cfgMu.Unlock()
	path := m.storageCfgPath()
	stamp, err := stampOf(path)
	if err != nil {
		return fmt.Errorf("storages: stat %s: %w", path, err)
	}
	m.mu.Lock()
	unchanged := m.cfg != nil && stamp == m.stamp
	m.mu.Unlock()
	if unchanged {
		return nil
	}
	var raw []byte
	if stamp.exists {
		if raw, err = os.ReadFile(path); err != nil { //nolint:gosec // fixed path below the PVE directory
			return fmt.Errorf("storages: read %s: %w", path, err)
		}
	}
	cfg, warnings := storagecfg.Parse(raw)
	for _, w := range warnings {
		m.log.Debug("storage.cfg", "warning", w.String())
	}

	next := map[string]*entry{}
	m.mu.Lock()
	for _, sec := range cfg.OfType(config.StorageType) {
		sc, derr := config.DecodeStorage(sec.ID, sec.Props)
		binding := bindingHash(sc)
		e := m.entries[sec.ID]
		if e == nil || e.binding != binding {
			e = &entry{id: sec.ID, binding: binding, health: apiv1.HealthOpening}
		}
		e.cfg, e.cfgErr = sc, derr
		e.enabled = !sc.Base.Disable && (len(sc.Base.Nodes) == 0 || slices.Contains(sc.Base.Nodes, m.opts.Node))
		next[sec.ID] = e
	}
	removed := []string{}
	for id := range m.entries {
		if next[id] == nil {
			removed = append(removed, id)
		}
	}
	m.entries, m.stamp, m.cfg = next, stamp, cfg
	entries := slices.Collect(maps.Values(next))
	m.mu.Unlock()

	// Without storage.cfg (pmxcfs not mounted) nothing is known about the
	// storages: keep their cached catalogues.
	if stamp.exists {
		for _, id := range removed {
			if err := m.opts.Store.DeleteStorage(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
				m.log.Warn("drop catalogue of removed storage", "storage", id, "err", err)
			}
			m.changed(id)
		}
	}
	for _, e := range entries {
		if err := m.adoptRow(ctx, e); err != nil {
			m.log.Warn("record storage", "storage", e.id, "err", err)
		}
		m.changed(e.id)
	}
	m.Wake()
	return nil
}

// adoptRow reconciles an entry with its database row: a row with the same
// binding carries the repository identity and cached state over a daemon
// restart; a row with another binding is reset together with its
// catalogue.
func (m *Manager) adoptRow(ctx context.Context, e *entry) error {
	row, err := m.opts.Store.GetStorage(ctx, e.id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	m.mu.Lock()
	if m.entries[e.id] != e {
		m.mu.Unlock()
		return nil
	}
	if row != nil && row.ConfigHash == e.binding {
		if e.repoUUID == "" {
			e.repoUUID = row.RepoUUID
			if row.Generation != nil {
				e.generation = int(*row.Generation)
			}
			if row.LastResyncAt != nil {
				e.lastResync = time.Unix(*row.LastResyncAt, 0)
			}
			if row.AboutJSON != "" {
				var u apiv1.Usage
				if json.Unmarshal([]byte(row.AboutJSON), &u) == nil {
					e.usage = &u
					e.nextAbout = u.UpdatedAt.Add(m.opts.AboutInterval)
				}
			}
		}
		m.mu.Unlock()
		return nil
	}
	cfg := e.cfg
	m.mu.Unlock()
	if err := m.opts.Store.PutStorage(ctx, &store.StorageRow{StoreID: e.id, Remote: cfg.Remote, BasePath: cfg.Path,
		Source: cfg.Source, ConfigHash: e.binding, Health: apiv1.HealthOpening}); err != nil {
		return err
	}
	// The cached catalogue described another repository or namespace.
	return m.opts.Store.ReplaceCatalog(ctx, e.id, nil)
}

// persist writes an entry's runtime state to its row (if the entry is
// still current).
func (m *Manager) persist(ctx context.Context, e *entry) {
	m.mu.Lock()
	if m.entries[e.id] != e {
		m.mu.Unlock()
		return
	}
	row := &store.StorageRow{StoreID: e.id, Remote: e.cfg.Remote, BasePath: e.cfg.Path, Source: e.cfg.Source,
		RepoUUID: e.repoUUID, ConfigHash: e.binding, Health: e.health, HealthDetail: e.detail}
	if e.generation > 0 {
		g := int64(e.generation)
		row.Generation = &g
	}
	if !e.lastResync.IsZero() {
		t := e.lastResync.Unix()
		row.LastResyncAt = &t
	}
	if e.usage != nil {
		if b, err := json.Marshal(e.usage); err == nil {
			row.AboutJSON = string(b)
		}
	}
	m.mu.Unlock()
	if err := m.opts.Store.PutStorage(ctx, row); err != nil {
		m.log.Warn("record storage state", "storage", e.id, "err", err)
	}
	m.changed(e.id)
}

// Run performs background work until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.opts.Tick)
	defer t.Stop()
	defer m.wg.Wait()
	var lastErr string
	for {
		if err := m.Reload(ctx); err != nil && err.Error() != lastErr {
			lastErr = err.Error()
			m.log.Warn("reload storage configuration", "err", err)
		} else if err == nil {
			lastErr = ""
		}
		m.schedule(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-m.wake:
		}
	}
}

func (m *Manager) schedule(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.opts.Now()
	for _, e := range m.entries {
		if e.busy || !e.enabled || e.cfgErr != nil {
			continue
		}
		var job func(context.Context, *entry)
		switch {
		case now.Before(e.nextAttempt):
		case e.repo == nil:
			job = m.open
		case e.resyncWanted || now.Sub(e.lastResync) >= m.opts.ResyncInterval:
			job = m.resync
		case !now.Before(e.nextAbout):
			job = m.about
		}
		if job == nil {
			continue
		}
		e.busy = true
		m.wg.Go(func() {
			job(ctx, e)
			m.mu.Lock()
			e.busy = false
			m.mu.Unlock()
		})
	}
}

// backoff returns the delay before the next attempt after n failures.
func backoff(n int) time.Duration {
	d := 30 * time.Second << min(n-1, 5)
	return min(d, 15*time.Minute)
}

// fail records a failed background operation.
func (m *Manager) fail(ctx context.Context, e *entry, op string, err error) {
	if ctx.Err() != nil {
		return
	}
	health, detail := healthOf(err)
	m.mu.Lock()
	e.failures++
	e.nextAttempt = m.opts.Now().Add(backoff(e.failures))
	changed := e.health != health || e.detail != detail
	e.health, e.detail = health, detail
	m.mu.Unlock()
	if changed {
		m.log.Warn("storage "+op+" failed", "storage", e.id, "health", health, "err", err)
	}
	m.persist(ctx, e)
}

// healthOf maps an error to a health value and an operator-facing detail.
func healthOf(err error) (string, string) {
	detail := err.Error()
	switch {
	case errors.Is(err, repo.ErrNotInitialized):
		return apiv1.HealthUninitialized, "no repository at the configured location; create it with 'pve-rclone-backup storage init'"
	case errors.Is(err, secrets.ErrNotFound):
		return apiv1.HealthMisconfigured, "the keys of this repository are missing; import a recovery kit"
	case errors.Is(err, repo.ErrWrongKeys), errors.Is(err, repo.ErrEncryptionMismatch), errors.Is(err, repo.ErrReplaced),
		errors.Is(err, errRemote):
		return apiv1.HealthMisconfigured, detail
	}
	switch transport.Classify(err) {
	case transport.ClassAuth:
		return apiv1.HealthAuthRequired, detail
	case transport.ClassQuota:
		return apiv1.HealthQuotaExceeded, detail
	case transport.ClassThrottled:
		return apiv1.HealthDegraded, detail
	case transport.ClassConfig:
		return apiv1.HealthMisconfigured, detail
	}
	return apiv1.HealthUnreachable, detail
}

var errRemote = errors.New("storages: transport remote unusable")

// checkRemote verifies that the storage's transport remote is configured
// with a supported backend.
func checkRemote(c *config.Storage) (transport.Profile, error) {
	typ, err := transport.RemoteType(c.Remote)
	if err != nil {
		return transport.Profile{}, fmt.Errorf("%w: %w", errRemote, err)
	}
	p, err := transport.ProfileFor(typ)
	if err != nil {
		return transport.Profile{}, fmt.Errorf("%w: %w", errRemote, err)
	}
	return p, nil
}

func (m *Manager) open(ctx context.Context, e *entry) {
	m.mu.Lock()
	cfg, knownUUID := e.cfg, e.repoUUID
	m.mu.Unlock()
	if _, err := checkRemote(cfg); err != nil {
		m.fail(ctx, e, "open", err)
		return
	}
	octx, cancel := context.WithTimeout(ctx, m.opts.OpenTimeout)
	defer cancel()
	loc := transport.RepoLocation{Remote: cfg.Remote, Path: cfg.Path}
	r, err := repo.Open(octx, loc, repo.OpenOptions{Encryption: cfg.Encryption, Keys: m.opts.Keys, UUID: knownUUID})
	if err != nil {
		m.fail(ctx, e, "open", err)
		return
	}
	if err := m.opts.Store.PutRepository(ctx, &store.Repository{UUID: r.UUID(), Remote: cfg.Remote,
		BasePath: cfg.Path, Encryption: cfg.Encryption}); err != nil {
		m.log.Warn("record repository", "storage", e.id, "err", err)
	}
	m.mu.Lock()
	e.repo, e.repoUUID, e.generation = r, r.UUID(), r.ActiveGeneration()
	e.failures, e.nextAttempt = 0, time.Time{}
	e.health, e.detail = apiv1.HealthOK, ""
	e.resyncWanted = true
	m.mu.Unlock()
	m.log.Info("opened repository", "storage", e.id, "repo", r.UUID(), "remote", cfg.Remote, "path", cfg.Path)
	m.persist(ctx, e)
	m.resync(ctx, e)
}

func (m *Manager) resync(ctx context.Context, e *entry) {
	m.mu.Lock()
	r, source := e.repo, e.cfg.Source
	m.mu.Unlock()
	octx, cancel := context.WithTimeout(ctx, m.opts.OpenTimeout)
	defer cancel()
	rep, err := catalog.Resync(octx, m.opts.Store, e.id, r, source)
	if err != nil {
		m.fail(ctx, e, "catalogue resync", err)
		return
	}
	for _, p := range rep.Problems {
		m.log.Warn("catalogue problem", "storage", e.id, "problem", p)
	}
	m.log.Info("catalogue resynced", "storage", e.id, "complete", rep.Complete, "tombstoned", rep.Tombstoned,
		"damaged", rep.Damaged, "incomplete", rep.Incomplete, "deleting", rep.Deleting, "invalid", rep.Invalid)
	m.mu.Lock()
	e.lastResync, e.resyncWanted = m.opts.Now(), false
	e.failures, e.nextAttempt = 0, time.Time{}
	e.health, e.detail = apiv1.HealthOK, ""
	m.mu.Unlock()
	m.persist(ctx, e)
}

func (m *Manager) about(ctx context.Context, e *entry) {
	m.mu.Lock()
	r := e.repo
	m.mu.Unlock()
	octx, cancel := context.WithTimeout(ctx, m.opts.OpenTimeout)
	defer cancel()
	u, err := r.Base().About(octx)
	now := m.opts.Now()
	switch {
	case errors.Is(err, transport.ErrNoQuota):
		u = nil
	case err != nil:
		m.mu.Lock()
		e.nextAbout = now.Add(min(m.opts.AboutInterval, 5*time.Minute))
		m.mu.Unlock()
		m.fail(ctx, e, "quota query", err)
		return
	}
	m.mu.Lock()
	e.nextAbout = now.Add(m.opts.AboutInterval)
	if u != nil {
		e.usage = &apiv1.Usage{Total: u.Total, Used: u.Used, Free: u.Free, Trashed: u.Trashed, UpdatedAt: now.UTC()}
	}
	if e.health != apiv1.HealthOK {
		e.health, e.detail = apiv1.HealthOK, ""
	}
	m.mu.Unlock()
	m.persist(ctx, e)
}

// RequestResync schedules a catalogue resync of a storage.
func (m *Manager) RequestResync(id string) bool {
	m.mu.Lock()
	e := m.entries[id]
	if e != nil {
		e.resyncWanted, e.nextAttempt = true, time.Time{}
	}
	m.mu.Unlock()
	m.Wake()
	return e != nil
}

// Get returns the API view of a storage.
func (m *Manager) Get(id string) (apiv1.Storage, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[id]
	if e == nil {
		return apiv1.Storage{}, false
	}
	return e.view(), true
}

// List returns the API views of all storages, sorted by ID.
func (m *Manager) List() []apiv1.Storage {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]apiv1.Storage, 0, len(m.entries))
	for _, id := range slices.Sorted(maps.Keys(m.entries)) {
		out = append(out, m.entries[id].view())
	}
	return out
}

// Targets returns the configurations of the storages enabled on this node
// with a valid configuration, sorted by ID.
func (m *Manager) Targets() []*config.Storage {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*config.Storage
	for _, id := range slices.Sorted(maps.Keys(m.entries)) {
		if e := m.entries[id]; e.enabled && e.cfgErr == nil {
			out = append(out, e.cfg)
		}
	}
	return out
}

// Status answers PVE's status() poll from cached state.
func (m *Manager) Status(id string) (apiv1.StorageStatus, bool) {
	s, ok := m.Get(id)
	if !ok {
		return apiv1.StorageStatus{}, false
	}
	st := apiv1.StorageStatus{Active: s.Active, Health: s.Health}
	if u := s.Usage; u != nil {
		st.Total, st.Used, st.Avail = deref(u.Total), deref(u.Used), deref(u.Free)
	}
	return st, true
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return max(*p, 0)
}

// view must be called with the manager lock held.
func (e *entry) view() apiv1.Storage {
	s := apiv1.Storage{ID: e.id, Enabled: e.enabled, Health: e.health, HealthDetail: e.detail,
		RepoUUID: e.repoUUID, Generation: e.generation, Usage: e.usage}
	if c := e.cfg; c != nil {
		s.Remote, s.Path, s.Source, s.Encryption = c.Remote, c.Path, c.Source, c.Encryption
		s.ReplicateFrom = append([]string{}, c.ReplicateFrom...)
	}
	if e.cfgErr != nil {
		s.ConfigError = e.cfgErr.Error()
		s.Health, s.HealthDetail = apiv1.HealthMisconfigured, s.ConfigError
	}
	if !e.enabled {
		s.Health = apiv1.HealthDisabled
	}
	if !e.lastResync.IsZero() {
		t := e.lastResync.UTC()
		s.LastResyncAt = &t
	}
	// The catalogue stays browsable while the remote is unreachable; it is
	// not served for a repository that is wrong or missing.
	s.Active = e.enabled && e.cfgErr == nil && e.repoUUID != "" &&
		e.health != apiv1.HealthMisconfigured && e.health != apiv1.HealthUninitialized
	return s
}
