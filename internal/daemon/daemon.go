// SPDX-License-Identifier: AGPL-3.0-or-later

// Package daemon wires the pve-rclone-backupd components together and runs
// them until the context is cancelled.
package daemon

import (
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
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/version"
)

// Defaults for production installs.
const (
	DefaultSocket   = "/run/pve-rclone-backup/api.sock"
	DefaultStateDir = "/var/lib/pve-rclone-backup"
)

// Job states counted as running work in status reports.
var activeStates = []string{"preparing", "uploading", "verifying", "committing", "transferring", "deleting"}

// Options configures the daemon.
type Options struct {
	Logger    *slog.Logger
	Socket    string
	StateDir  string
	Node      string   // PVE node name (default: short host name)
	AllowUIDs []uint32 // API peers allowed (default: root)
	// Ready, if set, is called once the API is accepting connections.
	Ready func()
}

// Daemon is a running pve-rclone-backupd instance.
type Daemon struct {
	log        *slog.Logger
	opts       Options
	store      *store.Store
	api        *api.Server
	startedAt  time.Time
	instanceID string
	problems   []string
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
	d := &Daemon{log: opts.Logger, opts: opts, startedAt: time.Now(), instanceID: rand.Text()}

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
	d.routes()

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
		return fmt.Errorf("api server: %w", err)
	case <-ctx.Done():
	}
	_ = sdNotify("STOPPING=1")
	d.log.Info("shutting down")
	stopServe()
	return <-errc
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

// openStore opens the state database. A corrupt database or one written by
// a newer daemon is moved aside and recreated: the catalogue is rebuilt
// from the remote repositories and local archives are rediscovered.
func (d *Daemon) openStore(ctx context.Context) (*store.Store, error) {
	path := filepath.Join(d.opts.StateDir, "state.db")
	st, err := store.Open(ctx, path)
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
		Problems:  d.problems,
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
	s.Healthy = len(s.Problems) == 0
	return s, nil
}
