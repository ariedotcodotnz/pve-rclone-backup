// SPDX-License-Identifier: AGPL-3.0-or-later

// Package discovery finds finished vzdump archives in the local backup
// storages that offsite storages replicate from, applies each offsite
// storage's selection rules and queues upload jobs. inotify reports new
// archives immediately; periodic scans catch everything else (other
// nodes writing to shared storage, missed events, daemon downtime).
package discovery

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/pve/cluster"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/pve/storagecfg"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

// SettleDelay is how long an archive must be unmodified before upload.
const SettleDelay = 60 * time.Second

// pendingStates are replication job states that will still upload.
var pendingStates = []string{"queued", "retry_wait", "preparing", "uploading", "verifying", "committing"}

// Options configures a Discoverer.
type Options struct {
	Log    *slog.Logger
	Store  *store.Store
	Node   string
	PVEDir string
	// Targets returns the offsite storages enabled on this node.
	Targets func() []*config.Storage
	// Ready reports whether a target may replicate now; archives are not
	// recorded for targets that are not ready, so they are considered
	// once the target is ready.
	Ready func(target string) error
	// Mounted reports whether a path is a mount point (default Mounted).
	Mounted      func(string) bool
	Inotify      bool
	ScanInterval time.Duration
	// OnJob is called for every job created.
	OnJob func(id int64, state string)
	Now   func() time.Time
}

// Discoverer runs discovery.
type Discoverer struct {
	opts    Options
	log     *slog.Logger
	paths   chan string
	rescan  chan struct{}
	refresh chan struct{}
	scanMu  sync.Mutex // one scan or event at a time

	mu       sync.Mutex
	sources  []*Source
	problems map[string]string // source → problem last logged
}

// New returns a discoverer.
func New(opts Options) *Discoverer {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Mounted == nil {
		opts.Mounted = Mounted
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Ready == nil {
		opts.Ready = func(string) error { return nil }
	}
	opts.ScanInterval = cmp.Or(opts.ScanInterval, config.DefaultNodeConfig().ScanInterval)
	return &Discoverer{opts: opts, log: opts.Log, paths: make(chan string, 256), rescan: make(chan struct{}, 1),
		refresh: make(chan struct{}, 1), problems: map[string]string{}}
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Refresh re-resolves sources (after storage configuration changes).
func (d *Discoverer) Refresh() { signal(d.refresh) }

// Notify reports a path from the vzdump hook. It never blocks.
func (d *Discoverer) Notify(path string) {
	select {
	case d.paths <- filepath.Clean(path):
	default:
		signal(d.rescan)
	}
}

func (d *Discoverer) resolve() ([]*Source, error) {
	raw, err := os.ReadFile(filepath.Join(d.opts.PVEDir, "storage.cfg"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	cfg, _ := storagecfg.Parse(raw)
	sources := ResolveSources(cfg, d.opts.Targets(), d.opts.Node, d.opts.Mounted)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sources = sources
	seen := map[string]bool{}
	for _, s := range sources {
		seen[s.StoreID] = true
		if s.Problem != d.problems[s.StoreID] {
			if s.Problem != "" {
				d.log.Warn("replication source unusable", "storage", s.StoreID, "problem", s.Problem)
			} else if d.problems[s.StoreID] != "" {
				d.log.Info("replication source usable again", "storage", s.StoreID)
			}
			d.problems[s.StoreID] = s.Problem
		}
	}
	for id := range d.problems {
		if !seen[id] {
			delete(d.problems, id)
		}
	}
	return sources, nil
}

// Run watches and scans until ctx is cancelled.
func (d *Discoverer) Run(ctx context.Context) {
	var w *watcher
	if d.opts.Inotify {
		var err error
		if w, err = newWatcher(d.paths, d.rescan); err != nil {
			d.log.Warn("inotify unavailable; relying on periodic scans", "err", err)
		} else {
			go w.run()
			defer func() { _ = w.close() }()
		}
	}
	syncWatches := func() {
		sources, err := d.resolve()
		if err != nil {
			d.log.Warn("resolve replication sources", "err", err)
			return
		}
		if w == nil {
			return
		}
		var dirs []string
		for _, s := range sources {
			if s.Problem == "" {
				dirs = append(dirs, s.Dir)
			}
		}
		for dir, err := range w.sync(dirs) {
			d.log.Warn("watch dump directory", "dir", dir, "err", err)
		}
	}
	syncWatches()
	d.scanLogged(ctx, "")

	scan := time.NewTicker(d.opts.ScanInterval)
	defer scan.Stop()
	resync := time.NewTicker(time.Minute) // mounts come and go
	defer resync.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-d.paths:
			if err := d.handlePath(ctx, p); err != nil {
				d.log.Warn("discover archive", "path", p, "err", err)
			}
		case <-d.refresh:
			syncWatches()
			d.scanLogged(ctx, "")
		case <-resync.C:
			syncWatches()
		case <-d.rescan:
			syncWatches()
			d.scanLogged(ctx, "")
		case <-scan.C:
			syncWatches()
			d.scanLogged(ctx, "")
		}
	}
}

func (d *Discoverer) scanLogged(ctx context.Context, only string) {
	sum, err := d.Scan(ctx, only)
	if err != nil {
		if ctx.Err() == nil {
			d.log.Warn("discovery scan", "err", err)
		}
		return
	}
	if sum.Queued > 0 || sum.Skipped > 0 || sum.Superseded > 0 {
		d.log.Info("discovery scan", "sources", sum.Sources, "archives", sum.Archives, "queued", sum.Queued,
			"skipped", sum.Skipped, "superseded", sum.Superseded)
	}
}

// Scan examines every usable source, or only the given source or offsite
// storage.
func (d *Discoverer) Scan(ctx context.Context, only string) (*apiv1.ScanSummary, error) {
	sources, err := d.resolve()
	if err != nil {
		return nil, err
	}
	d.scanMu.Lock()
	defer d.scanMu.Unlock()
	sum := &apiv1.ScanSummary{}
	env := d.newEnv()
	for _, src := range sources {
		if only != "" && src.StoreID != only && !hasTarget(src, only) {
			continue
		}
		if src.Problem != "" {
			sum.Problems = append(sum.Problems, src.StoreID+": "+src.Problem)
			continue
		}
		archives, err := listArchives(src.Dir)
		if err != nil {
			sum.Problems = append(sum.Problems, fmt.Sprintf("%s: %v", src.StoreID, err))
			continue
		}
		sum.Sources++
		sum.Archives += len(archives)
		for _, t := range src.Targets {
			if only != "" && only != src.StoreID && only != t.ID {
				continue
			}
			if err := d.considerAll(ctx, env, src, t, archives, false, sum); err != nil {
				return sum, err
			}
		}
	}
	return sum, nil
}

func hasTarget(src *Source, id string) bool {
	for _, t := range src.Targets {
		if t.ID == id {
			return true
		}
	}
	return false
}

// handlePath processes an archive reported by inotify or the hook.
func (d *Discoverer) handlePath(ctx context.Context, path string) error {
	dir, name := filepath.Split(path)
	dir = filepath.Clean(dir)
	d.mu.Lock()
	var src *Source
	for _, s := range d.sources {
		if s.Problem == "" && s.Dir == dir {
			src = s
		}
	}
	d.mu.Unlock()
	if src == nil {
		return nil
	}
	a, err := statArchive(dir, name)
	if err != nil || a == nil {
		return err
	}
	d.scanMu.Lock()
	defer d.scanMu.Unlock()
	env := d.newEnv()
	sum := &apiv1.ScanSummary{}
	for _, t := range src.Targets {
		if err := d.considerAll(ctx, env, src, t, []*Archive{a}, true, sum); err != nil {
			return err
		}
	}
	return nil
}

// env caches cluster state for one scan.
type env struct {
	guests  map[int]cluster.Guest
	members cluster.Members
	tags    map[int][]string
}

func (d *Discoverer) newEnv() *env {
	e := &env{tags: map[int][]string{}}
	var err error
	if e.guests, err = cluster.VMList(d.opts.PVEDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		d.log.Warn("read guest list", "err", err)
	}
	if e.members, err = cluster.ReadMembers(d.opts.PVEDir, d.opts.Node); err != nil {
		d.log.Warn("read cluster members", "err", err)
	}
	return e
}

func (e *env) guestTags(pveDir string, vmid int) ([]string, bool) {
	g, ok := e.guests[vmid]
	if !ok {
		return nil, false
	}
	if tags, ok := e.tags[vmid]; ok {
		return tags, true
	}
	tags, err := cluster.GuestTags(pveDir, g)
	if err != nil {
		return nil, false
	}
	e.tags[vmid] = tags
	return tags, true
}

// considerAll evaluates archives of one source for one target. viaEvent
// marks archives this node just wrote (inotify or hook).
func (d *Discoverer) considerAll(ctx context.Context, e *env, src *Source, t *config.Storage, archives []*Archive, viaEvent bool, sum *apiv1.ScanSummary) error {
	if err := d.opts.Ready(t.ID); err != nil {
		return nil
	}
	enabledAt, err := d.enabledAt(ctx, t.ID, src.StoreID)
	if err != nil {
		return err
	}
	// rclone-backfill latest: the newest pre-existing archive per guest.
	newestOld := map[string]string{}
	for _, a := range archives {
		if a.MtimeNs < enabledAt {
			newestOld[a.VMType+"/"+strconv.Itoa(a.VMID)] = a.Name // archives are sorted oldest first
		}
	}
	for _, a := range archives {
		if err := d.consider(ctx, e, src, t, a, viaEvent, enabledAt, newestOld, sum); err != nil {
			return err
		}
	}
	return nil
}

// enabledAt returns when replication from a source to a target started
// (recorded the first time the pair is seen ready).
func (d *Discoverer) enabledAt(ctx context.Context, target, source string) (int64, error) {
	key := "discovery.enabled:" + target + ":" + source
	v, ok, err := d.opts.Store.Meta(ctx, key)
	if err != nil {
		return 0, err
	}
	if ok {
		return strconv.ParseInt(v, 10, 64)
	}
	now := d.opts.Now().UnixNano()
	return now, d.opts.Store.SetMeta(ctx, key, strconv.FormatInt(now, 10))
}

func dedupeKey(target, source string, a *Archive) string {
	return fmt.Sprintf("replicate:%s:%s:%s:%d:%d", target, source, a.Name, a.Size, a.MtimeNs)
}

func (d *Discoverer) consider(ctx context.Context, e *env, src *Source, t *config.Storage, a *Archive, viaEvent bool,
	enabledAt int64, newestOld map[string]string, sum *apiv1.ScanSummary) error {
	// On shared storage every node sees every archive: the node running
	// the guest uploads it (the primary node if the guest is gone).
	if src.Shared && !viaEvent {
		g, ok := e.guests[a.VMID]
		if ok && g.Node != d.opts.Node || !ok && e.members.Primary() != d.opts.Node {
			return nil
		}
	}
	key := dedupeKey(t.ID, src.StoreID, a)
	j := &store.Job{Kind: "replicate", StoreID: t.ID, State: "queued", DedupeKey: key, OwnerNode: d.opts.Node,
		BackupVolname: "backup/" + layoutVolname(a), SourceStorage: src.StoreID, SourcePath: a.Path,
		VMType: a.VMType, VMID: a.VMID, BackupTime: a.BackupTime}
	dev, ino, size, mtime := int64(a.Dev), int64(a.Ino), a.Size, a.MtimeNs //nolint:gosec // device and inode numbers fit
	j.SourceDev, j.SourceIno, j.SourceSize, j.SourceMtimeNs, j.TotalBytes = &dev, &ino, &size, &mtime, &size
	settle := time.Unix(0, a.MtimeNs).Add(SettleDelay).Unix()
	j.NextAttemptAt = &settle

	if reason, err := d.skipReason(ctx, e, t, a, enabledAt, newestOld); err != nil {
		return err
	} else if reason != "" {
		j.State, j.LastError, j.NextAttemptAt = "skipped", reason, nil
	}
	id, created, superseded, err := d.opts.Store.InsertReplicateJob(ctx, j, t.Supersede)
	if err != nil {
		return err
	}
	if !created {
		sum.Known++
		return nil
	}
	switch j.State {
	case "queued":
		sum.Queued++
		d.log.Info("queued upload", "storage", t.ID, "archive", a.Path, "job", id)
	case "superseded":
		sum.Superseded++
	default:
		sum.Skipped++
		d.log.Debug("not replicating archive", "storage", t.ID, "archive", a.Path, "reason", j.LastError)
	}
	sum.Superseded += len(superseded)
	if d.opts.OnJob != nil {
		d.opts.OnJob(id, j.State)
		for _, s := range superseded {
			d.opts.OnJob(s, "superseded")
		}
	}
	return nil
}

func layoutVolname(a *Archive) string {
	return fmt.Sprintf("vzdump-%s-%d-%s.%s", a.VMType, a.VMID, a.TSLabel, a.Ext)
}

// skipReason applies the target's rules; an empty reason means upload.
func (d *Discoverer) skipReason(ctx context.Context, e *env, t *config.Storage, a *Archive, enabledAt int64, newestOld map[string]string) (string, error) {
	if fetched, err := d.opts.Store.FetchedArchive(ctx, a.Path); err == nil && fetched.Ino != nil && uint64(*fetched.Ino) == a.Ino { //nolint:gosec // inode numbers are positive
		return "archive was fetched from an offsite storage", nil
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if hasOrigin(a.Path) {
		return "archive was fetched from an offsite storage", nil
	}
	tags, known := e.guestTags(d.opts.PVEDir, a.VMID)
	if ok, reason := guestSelected(t, a.VMID, tags, known); !ok {
		return reason, nil
	}
	if a.MtimeNs < enabledAt {
		switch t.Backfill {
		case "none":
			return "archive existed before replication was enabled (rclone-backfill none)", nil
		case "latest":
			if newestOld[a.VMType+"/"+strconv.Itoa(a.VMID)] != a.Name {
				return "a newer archive of this guest existed before replication was enabled (rclone-backfill latest)", nil
			}
		}
	}
	if t.MinInterval > 0 {
		offsite, err := d.opts.Store.NewestBackup(ctx, t.ID, a.VMType, a.VMID, []string{"complete", "tombstoned", "damaged"})
		if err != nil {
			return "", err
		}
		queued, err := d.opts.Store.GuestReplication(ctx, t.ID, a.VMType, a.VMID, append([]string{"complete"}, pendingStates...))
		if err != nil {
			return "", err
		}
		newest := max(offsite, queued)
		if newest != 0 && newest > a.BackupTime-int64(t.MinInterval/time.Second) && newest != a.BackupTime {
			return fmt.Sprintf("an offsite backup of this guest from %s is within rclone-min-interval",
				time.Unix(newest, 0).Format(time.DateTime)), nil
		}
	}
	return "", nil
}
