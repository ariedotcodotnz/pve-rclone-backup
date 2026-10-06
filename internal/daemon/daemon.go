// SPDX-License-Identifier: AGPL-3.0-or-later

// Package daemon wires the pve-rclone-backupd components together and runs
// them until the context is cancelled.
package daemon

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/catalog"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/discovery"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/jobs"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/pve/cfs"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/recoverykit"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/remotes"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/replicate"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/restore"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/retention"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/secrets"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/storages"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/version"
)

// Defaults for production installs.
const (
	DefaultSocket   = "/run/pve-rclone-backup/api.sock"
	DefaultStateDir = "/var/lib/pve-rclone-backup"
	DefaultPVEDir   = "/etc/pve"
)

// Job states counted as running work in status reports.
var activeStates = append(slices.Clone(jobs.ActiveStates), "deleting")

// Options configures the daemon.
type Options struct {
	Logger    *slog.Logger
	Socket    string
	StateDir  string
	Node      string   // PVE node name (default: short host name)
	AllowUIDs []uint32 // API peers allowed (default: root)
	// PVEDir is the cluster file system root holding storage.cfg and the
	// cluster-wide daemon configuration (default /etc/pve).
	PVEDir string
	// KeyStore stores repository keys cluster-wide. The rclone engine must
	// have been initialized with the remote store beforehand.
	KeyStore KeyStore
	// Keys loads repository keys (default: KeyStore.Load).
	Keys repo.KeyLoader
	// Locker takes cluster-wide locks (default: pmxcfs locks below PVEDir).
	Locker secrets.Locker
	// RestoreTools overrides the PVE commands restores run (tests).
	RestoreTools *restore.Tools
	// Ready, if set, is called once the API is accepting connections.
	Ready func()
}

// KeyStore loads and saves repository keys (secrets.KeyStore).
type KeyStore interface {
	Load(repoUUID string) (*secrets.RepoKeys, error)
	Save(ctx context.Context, k *secrets.RepoKeys) error
}

// Daemon is a running pve-rclone-backupd instance.
type Daemon struct {
	log       *slog.Logger
	opts      Options
	store     *store.Store
	api       *api.Server
	storages  *storages.Manager
	discovery *discovery.Discoverer
	scheduler *jobs.Scheduler
	fetches   *jobs.Scheduler
	restores  *jobs.Scheduler
	verifies  *jobs.Scheduler
	deletes   *jobs.Scheduler
	remotes   *remotes.Manager
	keyStore  KeyStore
	ledger    *recoverykit.Ledger
	metaWake  chan struct{}

	backupLocksMu sync.Mutex
	backupLocks   map[string]*backupLock
	startedAt     time.Time
	instanceID    string
	problems      []string
}

// NodeName returns the PVE node name, which is the short host name.
func NodeName() string {
	h, err := os.Hostname()
	if err != nil {
		return "localhost"
	}
	name, _, _ := strings.Cut(h, ".")
	return name
}

// Run starts the daemon and blocks until ctx is cancelled or a fatal error
// occurs.
func Run(ctx context.Context, opts Options) error {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Socket == "" {
		opts.Socket = DefaultSocket
	}
	if opts.StateDir == "" {
		opts.StateDir = DefaultStateDir
	}
	if opts.Node == "" {
		opts.Node = NodeName()
	}
	if opts.PVEDir == "" {
		opts.PVEDir = DefaultPVEDir
	}
	if opts.Keys == nil && opts.KeyStore != nil {
		opts.Keys = opts.KeyStore.Load
	}
	if opts.Keys == nil {
		opts.Keys = func(string) (*secrets.RepoKeys, error) { return nil, secrets.ErrNotFound }
	}
	if opts.Locker == nil {
		opts.Locker = &cfs.Locker{Dir: filepath.Join(opts.PVEDir, "priv", "lock")}
	}
	d := &Daemon{log: opts.Logger, opts: opts, startedAt: time.Now(), instanceID: rand.Text(), keyStore: opts.KeyStore,
		metaWake: make(chan struct{}, 1), backupLocks: map[string]*backupLock{},
		ledger: recoverykit.NewLedger(filepath.Join(opts.PVEDir, "pve-rclone-backup", "kits.json"), opts.Locker)}

	// One daemon per state directory: the store is opened, and interrupted
	// jobs requeued, only by the daemon that owns it.
	unlock, err := lockStateDir(opts.StateDir)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := d.openStore(ctx)
	if err != nil {
		return err
	}
	d.store = st
	defer func() {
		if err := st.Close(); err != nil {
			d.log.Warn("close state database", "err", err)
		}
	}()
	if n, err := st.RequeueInterrupted(ctx, activeStates, "queued"); err != nil {
		return fmt.Errorf("requeue interrupted jobs: %w", err)
	} else if n > 0 {
		d.log.Warn("requeued jobs interrupted by the previous daemon run", "jobs", n)
	}

	d.api = api.New(api.Options{Logger: d.log, AllowUIDs: opts.AllowUIDs, Idempotency: st})
	dcfg := d.loadDaemonConfig()
	d.storages = storages.New(storages.Options{
		Log: d.log, Store: st, Node: opts.Node, PVEDir: opts.PVEDir, Keys: opts.Keys,
		AboutInterval: dcfg.AboutInterval, ResyncInterval: dcfg.CatalogResyncInterval,
		OnResync: func(id string, rep *catalog.Report) {
			d.damagedAlert(context.Background(), id, rep)
			d.finishDeletions(id, rep)
		},
		OnChange: func(id string) {
			if s, ok := d.storages.Get(id); ok {
				d.api.Events().Publish("storage.status", s)
			}
			if d.discovery != nil {
				d.discovery.Refresh()
			}
		},
	})
	d.remotes = remotes.New(remotes.Options{Log: d.log, Users: d.remoteUsers, OnConfigured: func(name string) {
		// A reconnected remote takes jobs again at once, not when the
		// pause for its rejected credentials would have ended.
		for _, s := range []*jobs.Scheduler{d.scheduler, d.fetches, d.restores, d.verifies, d.deletes} {
			if s != nil {
				s.ResumeRemote(ctx, name)
			}
		}
	}})
	ncfg := d.loadNodeConfig()
	d.discovery = discovery.New(discovery.Options{
		Log: d.log, Store: st, Node: opts.Node, PVEDir: opts.PVEDir,
		Targets: d.storages.Targets, Ready: d.replicationReady,
		Inotify: ncfg.Inotify, ScanInterval: ncfg.ScanInterval,
		OnJob: func(id int64, state string) {
			d.api.Events().Publish("job.updated", apiv1.JobUpdate{ID: id, State: state})
			if d.scheduler != nil && state == jobs.StateQueued {
				d.scheduler.Wake()
			}
		},
	})
	runner := replicate.New(replicate.Options{
		Log: d.log, Store: st, Node: opts.Node, PVEDir: opts.PVEDir, Repos: d.storages.Repo,
		Identity: d.identity,
		OnCommit: func(b *store.Backup) { d.api.Events().Publish("backup.added", backupView(b)) },
	})
	d.scheduler = jobs.New(jobs.Options{
		Log: d.log, Store: st, Node: opts.Node, Workers: ncfg.UploadWorkers, Targets: d.storages.Targets,
		Runner: runner,
		Ready: func(id string) error {
			_, _, err := d.storages.Repo(id)
			return err
		},
		OnUpdate: func(u apiv1.JobUpdate) { d.api.Events().Publish("job.updated", u) },
	})
	restorer := restore.New(restore.Options{
		Log: d.log, Store: st, Node: opts.Node, PVEDir: opts.PVEDir, Repos: d.storages.Repo,
		StagingDir: ncfg.StagingDir, StagingReserve: ncfg.StagingReserve,
		Tools: d.restoreTools(ncfg), OnDamaged: d.contentDamaged,
	})
	restorer.CleanStaging()
	for kind, sched := range map[string]**jobs.Scheduler{"fetch": &d.fetches, "restore": &d.restores, "verify": &d.verifies} {
		*sched = jobs.New(jobs.Options{
			Log: d.log, Store: st, Node: opts.Node, Kind: kind, Workers: 1, Targets: d.storages.Targets, Runner: restorer,
			Ready: func(id string) error {
				_, _, err := d.storages.Repo(id)
				return err
			},
			OnUpdate: func(u apiv1.JobUpdate) { d.api.Events().Publish("job.updated", u) },
		})
	}
	d.deletes = jobs.New(jobs.Options{
		Log: d.log, Store: st, Node: opts.Node, Kind: "delete", Workers: 1, Targets: d.storages.Targets,
		Runner: &retention.Deleter{Log: d.log, Store: st, Repos: d.storages.Repo, Lock: d.lockBackup, OnRemoved: func(storeID, volname string) {
			d.api.Events().Publish("backup.removed", map[string]string{"storage": storeID, "volname": volname})
		}},
		Ready: func(id string) error {
			_, _, err := d.storages.Repo(id)
			return err
		},
		OnUpdate: func(u apiv1.JobUpdate) { d.api.Events().Publish("job.updated", u) },
	})
	if err := d.storages.Reload(ctx); err != nil {
		d.log.Warn("load storage configuration", "err", err)
	}
	d.routes()
	d.storageRoutes()
	d.discoveryRoutes()
	d.jobRoutes()
	d.remoteRoutes()
	d.setupRoutes()
	d.backupRoutes()
	d.restoreRoutes()
	d.verifyRoutes()
	d.retentionRoutes()

	l, err := api.Listen(opts.Socket)
	if err != nil {
		return err
	}
	d.log.Info("pve-rclone-backupd started", "version", version.Get().Version, "node", opts.Node,
		"socket", opts.Socket, "state_dir", opts.StateDir, "rclone", transport.RcloneVersion())

	serveCtx, stopServe := context.WithCancel(ctx)
	defer stopServe()
	errc := make(chan error, 1)
	go func() { errc <- d.api.Serve(serveCtx, l) }()
	var workers sync.WaitGroup
	defer func() { // stop background work before the store is closed
		stopServe()
		workers.Wait()
	}()
	workers.Go(func() { d.storages.Run(serveCtx) })
	workers.Go(func() { d.discovery.Run(serveCtx) })
	workers.Go(func() { d.scheduler.Run(serveCtx) })
	workers.Go(func() { d.fetches.Run(serveCtx) })
	workers.Go(func() { d.restores.Run(serveCtx) })
	workers.Go(func() { d.verifies.Run(serveCtx) })
	workers.Go(func() { d.planVerification(serveCtx) })
	workers.Go(func() { d.deletes.Run(serveCtx) })
	workers.Go(func() { d.retentionLoop(serveCtx, dcfg.RetentionTime) })
	workers.Go(func() { d.pushMeta(serveCtx) })

	if opts.Ready != nil {
		opts.Ready()
	}
	if err := sdNotify("READY=1\nSTATUS=serving API on " + opts.Socket); err != nil {
		d.log.Warn("notify systemd", "err", err)
	}
	if iv := watchdogInterval(); iv > 0 {
		go d.watchdog(serveCtx, iv)
	}

	select {
	case err := <-errc:
		if ctx.Err() == nil {
			return fmt.Errorf("api server stopped: %w", cmp.Or(err, errors.New("no error reported")))
		}
		// Shutting down: the server stopped first because its context
		// is ctx's child. Handled below like any shutdown.
		errc <- err
	case <-ctx.Done():
	}
	_ = sdNotify("STOPPING=1")
	d.log.Info("shutting down")
	stopServe()
	return <-errc
}

// loadDaemonConfig reads the cluster-wide daemon configuration; problems
// are reported and the defaults used.
func (d *Daemon) loadDaemonConfig() *config.DaemonConfig {
	path := filepath.Join(d.opts.PVEDir, "pve-rclone-backup", "daemon.cfg")
	raw, err := os.ReadFile(path) //nolint:gosec // fixed path below the PVE directory
	if errors.Is(err, os.ErrNotExist) {
		return config.DefaultDaemonConfig()
	}
	if err == nil {
		var c *config.DaemonConfig
		if c, err = config.ParseDaemonConfig(raw); err == nil {
			return c
		}
	}
	d.log.Warn("daemon configuration unusable; using defaults", "path", path, "err", err)
	d.problems = append(d.problems, fmt.Sprintf("%s: %v", path, err))
	return config.DefaultDaemonConfig()
}

// loadNodeConfig reads this node's configuration; problems are reported
// and the defaults used.
func (d *Daemon) loadNodeConfig() *config.NodeConfig {
	path := filepath.Join(d.opts.PVEDir, "nodes", d.opts.Node, "pve-rclone-backup.cfg")
	raw, err := os.ReadFile(path) //nolint:gosec // fixed path below the PVE directory
	if errors.Is(err, os.ErrNotExist) {
		return config.DefaultNodeConfig()
	}
	if err == nil {
		var c *config.NodeConfig
		if c, err = config.ParseNodeConfig(raw); err == nil {
			return c
		}
	}
	d.log.Warn("node configuration unusable; using defaults", "path", path, "err", err)
	d.problems = append(d.problems, fmt.Sprintf("%s: %v", path, err))
	return config.DefaultNodeConfig()
}

func (d *Daemon) restoreTools(ncfg *config.NodeConfig) restore.Tools {
	if d.opts.RestoreTools != nil {
		return *d.opts.RestoreTools
	}
	return restore.Tools{IOnice: ioniceArgs(ncfg.RestoreIOnice)}
}

// replicationReady reports whether archives may be queued for a storage:
// its repository must be known and usable (it may be temporarily
// unreachable; uploads wait).
func (d *Daemon) replicationReady(id string) error {
	s, ok := d.storages.Get(id)
	switch {
	case !ok:
		return fmt.Errorf("storage %s is not configured", id)
	case !s.Active:
		return fmt.Errorf("storage %s is not ready (%s)", id, s.Health)
	case s.Encryption == "crypt" && !d.ledger.Confirmed(s.RepoUUID):
		return fmt.Errorf("storage %s waits for a confirmed recovery kit", id)
	}
	return nil
}

// remoteUsers maps transport remotes to the storages using them.
func (d *Daemon) remoteUsers() map[string][]string {
	out := map[string][]string{}
	for _, s := range d.storages.List() {
		out[s.Remote] = append(out[s.Remote], s.ID)
	}
	return out
}

func (d *Daemon) watchdog(ctx context.Context, iv time.Duration) {
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := sdNotify("WATCHDOG=1"); err != nil {
				d.log.Warn("notify systemd watchdog", "err", err)
			}
		}
	}
}

// lockStateDir takes an exclusive lock on the state directory, released by
// the returned function, or fails if another daemon holds it.
func lockStateDir(dir string) (unlock func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	path := filepath.Join(dir, "daemon.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // our state directory
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("another pve-rclone-backupd is running with state directory %s", dir)
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}

// openStore opens the state database. A corrupt database or one written by
// a newer daemon is moved aside and recreated: the catalogue is rebuilt
// from the remote repositories and local archives are rediscovered.
func (d *Daemon) openStore(ctx context.Context) (*store.Store, error) {
	path := filepath.Join(d.opts.StateDir, "state.db")
	st, err := store.Open(ctx, path)
	if err != nil && ctx.Err() != nil {
		// Interrupted, which says nothing about the database.
		return nil, fmt.Errorf("open state database: %w", err)
	}
	reason := ""
	switch {
	case errors.Is(err, store.ErrCorrupt):
		reason = "corrupt"
	case errors.Is(err, store.ErrNewerSchema):
		reason = "newer"
	case err != nil:
		return nil, fmt.Errorf("open state database: %w", err)
	default:
		return st, nil
	}
	moved, mvErr := store.MoveAside(path, reason, time.Now())
	if mvErr != nil {
		return nil, fmt.Errorf("open state database: %w (and could not move it aside: %w)", err, mvErr)
	}
	d.log.Warn("state database unusable; moved aside and starting with a fresh one", "reason", err, "moved_to", moved)
	if st, err = store.Open(ctx, path); err != nil {
		return nil, fmt.Errorf("create fresh state database: %w", err)
	}
	if aerr := st.RaiseAlert(ctx, store.Alert{ID: "state-db-rebuilt", Severity: "warning",
		Message: fmt.Sprintf("The state database was %s and has been recreated (old copy: %s). Job history was lost; offsite backups are unaffected.", reason, moved)}); aerr != nil {
		d.log.Warn("raise alert", "err", aerr)
	}
	return st, nil
}

func (d *Daemon) routes() {
	d.api.Handle("GET /v1/version", func(w http.ResponseWriter, r *http.Request) error {
		v := version.Get()
		return api.WriteJSON(w, http.StatusOK, apiv1.Version{
			API: apiv1.Revision, APIMinClient: apiv1.Revision, Daemon: v.Version, Commit: v.Commit,
			Rclone: transport.RcloneVersion(), Schema: store.LatestSchemaVersion(),
			InstanceID: d.instanceID, Node: d.opts.Node,
		})
	})
	d.api.Handle("GET /v1/status", func(w http.ResponseWriter, r *http.Request) error {
		s, err := d.status(r.Context())
		if err != nil {
			return err
		}
		return api.WriteJSON(w, http.StatusOK, s)
	})
}

func (d *Daemon) status(ctx context.Context) (*apiv1.Status, error) {
	s := &apiv1.Status{
		Node:      d.opts.Node,
		Version:   version.Get().Version,
		StartedAt: d.startedAt.UTC(),
		UptimeSec: int64(time.Since(d.startedAt).Seconds()),
		Problems:  slices.Clone(d.problems),
	}
	counts, err := d.store.JobCounts(ctx)
	if err != nil {
		return nil, err
	}
	for state, n := range counts {
		switch state {
		case "discovered", "queued":
			s.Jobs.Queued += n
		case "retry_wait":
			s.Jobs.RetryWait += n
		case "failed":
			s.Jobs.Failed += n
		default:
			if slices.Contains(activeStates, state) {
				s.Jobs.Active += n
			}
		}
	}
	alerts, err := d.store.ActiveAlerts(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range alerts {
		s.Alerts = append(s.Alerts, apiv1.Alert{ID: a.ID, Severity: a.Severity, Storage: a.StoreID,
			Message: a.Message, RaisedAt: time.Unix(a.RaisedAt, 0).UTC()})
	}
	for _, st := range d.storages.List() {
		if st.Enabled && st.Active && st.Encryption == "crypt" && len(st.ReplicateFrom) > 0 && !d.ledger.Confirmed(st.RepoUUID) {
			s.Problems = append(s.Problems, fmt.Sprintf("replication to %s waits for a recovery kit: run 'pve-rclone-backup recovery-kit export'", st.ID))
		}
	}
	s.Healthy = len(s.Problems) == 0
	return s, nil
}
