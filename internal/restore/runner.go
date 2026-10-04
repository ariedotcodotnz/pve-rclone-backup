// SPDX-License-Identifier: AGPL-3.0-or-later

package restore

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/catalog"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/discovery"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/jobs"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/manifest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/pve/cluster"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/pve/storagecfg"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/storages"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

// Options configures a Runner.
type Options struct {
	Log    *slog.Logger
	Store  *store.Store
	Node   string
	PVEDir string
	Repos  func(storeID string) (*repo.Repo, *config.Storage, error)
	// Mounted reports whether a path is a mount point (default
	// discovery.Mounted).
	Mounted func(string) bool
	// StagingDir holds archives staged for restores; StagingReserve must
	// stay free on its file system.
	StagingDir     string
	StagingReserve int64
	Tools          Tools
	// OnDamaged is called when a content verification finds a backup
	// damaged.
	OnDamaged func(storeID, volname, problem string)
	Now       func() time.Time
}

// Runner implements jobs.Runner for fetch and restore jobs.
type Runner struct {
	opts Options
	log  *slog.Logger
}

// New returns a runner.
func New(opts Options) *Runner {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Mounted == nil {
		opts.Mounted = discovery.Mounted
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	opts.StagingDir = cmp.Or(opts.StagingDir, config.DefaultNodeConfig().StagingDir)
	opts.Tools = opts.Tools.withDefaults()
	return &Runner{opts: opts, log: opts.Log}
}

// FetchParams are the parameters of a fetch job.
type FetchParams struct {
	TargetStorage string `json:"target_storage"`
	Protect       bool   `json:"protect"`
}

// RestoreParams are the parameters of a restore job.
type RestoreParams struct {
	Mode          string `json:"mode"` // stream or stage
	TargetVMID    int    `json:"target_vmid"`
	TargetStorage string `json:"target_storage,omitempty"`
	Unique        bool   `json:"unique,omitzero"`
	Force         bool   `json:"force,omitzero"`
	AllowDamaged  bool   `json:"allow_damaged,omitzero"`
}

// CleanStaging removes staging directories left by an earlier daemon run.
func (r *Runner) CleanStaging() {
	entries, err := os.ReadDir(r.opts.StagingDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "job-") {
			if err := os.RemoveAll(filepath.Join(r.opts.StagingDir, e.Name())); err == nil {
				r.log.Info("removed stale restore staging", "dir", e.Name())
			}
		}
	}
}

// Run implements jobs.Runner.
func (r *Runner) Run(ctx context.Context, t *jobs.Task) error {
	j := t.Job()
	rp, _, err := r.opts.Repos(j.StoreID)
	if errors.Is(err, storages.ErrNotReady) {
		return jobs.Postpone(r.opts.Now().Add(time.Minute), err.Error())
	}
	if err != nil {
		return jobs.Permanent(err)
	}
	b, err := r.opts.Store.GetBackup(ctx, j.StoreID, j.BackupVolname)
	if errors.Is(err, store.ErrNotFound) {
		return jobs.Permanent(fmt.Errorf("backup %s is not in the catalogue of %s", j.BackupVolname, j.StoreID))
	}
	if err != nil {
		return err
	}
	gen, id, err := catalog.RemoteID(b)
	if err != nil {
		return jobs.Permanent(err)
	}
	m, err := rp.ReadManifest(ctx, gen, id)
	if err != nil {
		if errors.Is(err, repo.ErrInvalidDocument) {
			return jobs.Permanent(err)
		}
		return err
	}
	tgt, err := rp.Target(gen)
	if err != nil {
		return jobs.Permanent(err)
	}
	src := &source{rp: rp, tgt: tgt, gen: gen, id: id, m: m, b: b}
	switch j.Kind {
	case "fetch":
		var p FetchParams
		if err := json.Unmarshal([]byte(j.ParamsJSON), &p); err != nil {
			return jobs.Permanent(err)
		}
		return r.fetch(ctx, t, src, p)
	case "restore":
		var p RestoreParams
		if err := json.Unmarshal([]byte(j.ParamsJSON), &p); err != nil {
			return jobs.Permanent(err)
		}
		if b.State == "damaged" && !p.AllowDamaged {
			return jobs.Permanent(errors.New("the backup is damaged; restore an older backup or pass allow_damaged"))
		}
		return r.restore(ctx, t, src, p)
	case "verify":
		var p VerifyParams
		if err := json.Unmarshal([]byte(j.ParamsJSON), &p); err != nil {
			return jobs.Permanent(err)
		}
		return r.verify(ctx, t, src, p)
	}
	return jobs.Permanent(fmt.Errorf("unsupported job kind %q", j.Kind))
}

// source is the backup a job reads.
type source struct {
	rp  *repo.Repo
	tgt *transport.Target
	gen int
	id  layout.BackupID
	m   *manifest.Manifest
	b   *store.Backup
}

// basename is the archive name without extension (the log's name).
func (s *source) basename() string {
	a, err := layout.ParseArchiveName(s.m.Archive.Filename)
	if err != nil {
		return s.m.Archive.Filename
	}
	return strings.TrimSuffix(s.m.Archive.Filename, "."+a.Ext)
}

// progressWriter records transfer progress in the job.
type progressWriter struct {
	ctx  context.Context
	t    *jobs.Task
	n    int64
	last time.Time
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	if time.Since(w.last) >= time.Second {
		w.last = time.Now()
		n := w.n
		_ = w.t.Update(w.ctx, func(j *store.Job) { j.ProgressBytes = n })
	}
	return len(p), nil
}

func storageConfig(pveDir string) (*storagecfg.Config, error) {
	raw, err := os.ReadFile(filepath.Join(pveDir, "storage.cfg")) //nolint:gosec // fixed path below the PVE directory
	if err != nil {
		return nil, err
	}
	cfg, _ := storagecfg.Parse(raw)
	return cfg, nil
}

// freeSpace reports the bytes available to root below dir.
func freeSpace(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * st.Bsize, nil //nolint:gosec // block counts fit in int64
}

// download writes the verified archive to path (created exclusively).
func (r *Runner) download(ctx context.Context, t *jobs.Task, s *source, path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o640) //nolint:gosec // path built from validated names
	if err != nil {
		return err
	}
	rd := NewReader(ctx, s.tgt, s.id, s.m)
	defer func() { _ = rd.Close() }()
	pw := &progressWriter{ctx: ctx, t: t}
	_, err = io.Copy(io.MultiWriter(f, pw), rd)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return err
	}
	size := s.m.Archive.Size
	return t.Update(ctx, func(j *store.Job) { j.ProgressBytes = size })
}

// fetch places the archive into a local backup storage as a native
// backup, with its log, notes and (by default) protection.
func (r *Runner) fetch(ctx context.Context, t *jobs.Task, s *source, p FetchParams) error {
	cfg, err := storageConfig(r.opts.PVEDir)
	if err != nil {
		return err
	}
	dst := discovery.ResolveDumpDir(cfg, p.TargetStorage, r.opts.Node, r.opts.Mounted)
	switch {
	case dst == nil:
		return jobs.Permanent(fmt.Errorf("storage %s is not available on this node", p.TargetStorage))
	case dst.Problem != "":
		return jobs.Permanent(fmt.Errorf("storage %s: %s", p.TargetStorage, dst.Problem))
	}
	final := filepath.Join(dst.Dir, s.m.Archive.Filename)
	if _, err := os.Lstat(final); err == nil {
		return jobs.Permanent(fmt.Errorf("%s already exists", final))
	}
	free, err := freeSpace(dst.Dir)
	if err != nil {
		return err
	}
	if need := s.m.Archive.Size + r.opts.StagingReserve; free < need {
		return jobs.Permanent(fmt.Errorf("%s has %d bytes free, %d needed", dst.Dir, free, need))
	}
	if err := t.Advance(ctx, jobs.StateTransferring, "downloading to "+final, nil); err != nil {
		return err
	}
	tmp := filepath.Join(dst.Dir, fmt.Sprintf(".%s.fetch-%d", s.m.Archive.Filename, t.Job().ID))
	if fi, err := os.Lstat(tmp); err == nil && fi.Mode().IsRegular() {
		_ = os.Remove(tmp) // an interrupted earlier attempt
	}
	if err := r.download(ctx, t, s, tmp); err != nil {
		return err
	}
	placed := false
	defer func() {
		if !placed {
			_ = os.Remove(tmp)
		}
	}()
	if err := t.Advance(ctx, jobs.StateVerifying, "archive matches its manifest", nil); err != nil {
		return err
	}
	if err := t.Advance(ctx, jobs.StateApplying, "placing the archive", nil); err != nil {
		return err
	}
	base := filepath.Join(dst.Dir, s.basename())
	if s.m.Sidecars.Log != nil {
		if data, err := s.tgt.ReadAll(ctx, s.id.Path(layout.LogName), 16<<20); err == nil {
			_ = writeNew(base+".log", data)
		}
	}
	notes := fmt.Sprintf("Fetched from offsite storage %s on %s.", s.b.StoreID, r.opts.Now().Format("2006-01-02 15:04"))
	if s.b.Notes != "" {
		notes += "\n" + s.b.Notes
	}
	if err := writeNew(final+".notes", []byte(notes)); err != nil {
		return err
	}
	if p.Protect {
		// Protected: the next local prune must not remove it.
		if err := writeNew(final+".protected", nil); err != nil {
			return err
		}
	}
	// Mark the archive so discovery never uploads it again.
	origin := s.b.StoreID + ":" + s.b.Volname
	if err := unix.Setxattr(tmp, discovery.OriginXattr, []byte(origin), 0); err != nil && !errors.Is(err, unix.ENOTSUP) {
		r.log.Debug("set origin marker", "path", tmp, "err", err)
	}
	fi, err := os.Stat(tmp)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("no inode information for %s", tmp)
	}
	dev, ino := int64(st.Dev), int64(st.Ino) //nolint:gosec // device and inode numbers fit
	if err := r.opts.Store.AddFetchedArchive(ctx, store.FetchedArchive{Path: final, Dev: &dev, Ino: &ino,
		StoreID: s.b.StoreID, Volname: s.b.Volname, SHA256: s.m.Archive.SHA256}); err != nil {
		return err
	}
	if err := renameNoReplace(tmp, final); err != nil {
		return jobs.Permanent(err)
	}
	placed = true
	syncDir(dst.Dir)
	r.log.Info("fetched offsite backup", "storage", s.b.StoreID, "volname", s.b.Volname, "path", final)
	return nil
}

// writeNew creates a sidecar file that must not exist yet.
func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0o640) //nolint:gosec // path built from validated names
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// renameNoReplace moves tmp to final unless final exists, falling back to
// link and unlink on file systems without RENAME_NOREPLACE.
func renameNoReplace(tmp, final string) error {
	err := unix.Renameat2(unix.AT_FDCWD, tmp, unix.AT_FDCWD, final, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
		if err = os.Link(tmp, final); err == nil {
			err = os.Remove(tmp)
		}
	}
	if err != nil {
		return fmt.Errorf("place %s: %w", final, err)
	}
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil { //nolint:gosec // storage directory
		_ = d.Sync()
		_ = d.Close()
	}
}

// restore creates a guest from the backup.
func (r *Runner) restore(ctx context.Context, t *jobs.Task, s *source, p RestoreParams) error {
	vmtype := s.m.Backup.VMType
	if !config.ValidVMID(strconv.Itoa(p.TargetVMID)) {
		return jobs.Permanent(fmt.Errorf("invalid target VMID %d", p.TargetVMID))
	}
	cfg, err := storageConfig(r.opts.PVEDir)
	if err != nil {
		return err
	}
	if p.TargetStorage != "" {
		sec := cfg.Get(p.TargetStorage)
		want := map[string]string{"qemu": "images", "lxc": "rootdir"}[vmtype]
		if sec == nil {
			return jobs.Permanent(fmt.Errorf("storage %s does not exist", p.TargetStorage))
		}
		if content, ok := sec.Content(); ok && !content[want] {
			return jobs.Permanent(fmt.Errorf("storage %s does not hold %s", p.TargetStorage, want))
		}
	}
	guests, err := cluster.VMList(r.opts.PVEDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, existed := guests[p.TargetVMID]
	if existed && !p.Force {
		return jobs.Permanent(fmt.Errorf("guest %d exists; choose another VMID or overwrite it with force", p.TargetVMID))
	}
	// Only a guest seen here is overwritten: one created meanwhile makes
	// the restore tool refuse rather than replace it.
	p.Force = existed
	switch {
	case p.Mode == "stream" && vmtype == "qemu" && existed:
		// Streaming would replace the guest's disks before the archive's
		// digest is known.
		_ = t.Event(ctx, "info", fmt.Sprintf("guest %d is overwritten only after the archive has been downloaded and verified", p.TargetVMID))
		return r.staged(ctx, t, s, p, vmtype)
	case p.Mode == "stream" && vmtype == "qemu":
		return r.streamVM(ctx, t, s, p)
	case p.Mode == "stage":
		return r.staged(ctx, t, s, p, vmtype)
	}
	return jobs.Permanent(fmt.Errorf("restore mode %q is not available for %s backups", p.Mode, vmtype))
}

func restoreArgs(p RestoreParams) []string {
	var args []string
	if p.TargetStorage != "" {
		args = append(args, "--storage", p.TargetStorage)
	}
	if p.Unique {
		args = append(args, "--unique", "1")
	}
	if p.Force {
		args = append(args, "--force", "1")
	}
	return args
}

// streamVM pipes the archive through the decompressor into qmrestore for
// a new guest; nothing is staged. If qmrestore succeeds but the archive
// does not match its digest, the guest it just created is removed. If
// qmrestore fails, it cleans up itself: the guest might not be ours.
func (r *Runner) streamVM(ctx context.Context, t *jobs.Task, s *source, p RestoreParams) error {
	if err := t.Advance(ctx, jobs.StateTransferring, fmt.Sprintf("streaming into qmrestore as VM %d", p.TargetVMID), nil); err != nil {
		return err
	}
	rd := NewReader(ctx, s.tgt, s.id, s.m)
	defer func() { _ = rd.Close() }()
	pr, pw := io.Pipe()
	copyErr := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.MultiWriter(pw, &progressWriter{ctx: ctx, t: t}), rd)
		_ = pw.CloseWithError(err)
		copyErr <- err
	}()
	argv := append([]string{r.opts.Tools.QMRestore, "-", strconv.Itoa(p.TargetVMID)}, restoreArgs(p)...)
	runErr := r.pipeline(ctx, t, pr, s.m.Archive.Compression, argv...)
	_ = pr.CloseWithError(errors.New("restore finished"))
	cerr := <-copyErr
	if errors.Is(cerr, io.ErrClosedPipe) {
		cerr = nil // qmrestore stopped reading; its error explains why
	}
	if runErr == nil && cerr != nil {
		r.removeGuest(t, p.TargetVMID)
	}
	if cerr != nil || runErr != nil {
		return jobs.Permanent(errors.Join(runErr, cerr))
	}
	size := s.m.Archive.Size
	if err := t.Advance(ctx, jobs.StateVerifying, "archive matched its manifest", func(j *store.Job) { j.ProgressBytes = size }); err != nil {
		return err
	}
	r.log.Info("restored guest from offsite backup", "vmid", p.TargetVMID, "volname", s.b.Volname, "mode", "stream")
	return nil
}

func (r *Runner) removeGuest(t *jobs.Task, vmid int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	_ = t.Event(ctx, "warn", fmt.Sprintf("removing guest %d created by the failed restore", vmid))
	if err := r.command(ctx, t, nil, r.opts.Tools.QM, "destroy", strconv.Itoa(vmid), "--purge", "1"); err != nil {
		_ = t.Event(ctx, "warn", "could not remove the guest: "+err.Error())
	}
}

// staged downloads the archive under its original name, then restores it
// with the native tools (which also handle privileged containers).
func (r *Runner) staged(ctx context.Context, t *jobs.Task, s *source, p RestoreParams, vmtype string) error {
	dir := filepath.Join(r.opts.StagingDir, fmt.Sprintf("job-%d", t.Job().ID))
	if err := os.MkdirAll(r.opts.StagingDir, 0o700); err != nil {
		return jobs.Permanent(fmt.Errorf("staging directory (staging-dir): %w", err))
	}
	_ = os.RemoveAll(dir)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return jobs.Permanent(fmt.Errorf("staging directory (staging-dir): %w", err))
	}
	defer func() { _ = os.RemoveAll(dir) }()
	free, err := freeSpace(dir)
	if err != nil {
		return err
	}
	if need := s.m.Archive.Size + r.opts.StagingReserve; free < need {
		return jobs.Permanent(fmt.Errorf("staging directory %s has %d bytes free, %d needed", r.opts.StagingDir, free, need))
	}
	if err := t.Advance(ctx, jobs.StateTransferring, "staging the archive in "+dir, nil); err != nil {
		return err
	}
	path := filepath.Join(dir, s.m.Archive.Filename)
	if err := r.download(ctx, t, s, path); err != nil {
		return err
	}
	if err := t.Advance(ctx, jobs.StateVerifying, "archive matches its manifest", nil); err != nil {
		return err
	}
	if err := t.Advance(ctx, jobs.StateApplying, "restoring", nil); err != nil {
		return err
	}
	vmid := strconv.Itoa(p.TargetVMID)
	argv := []string{r.opts.Tools.QMRestore, path, vmid}
	if vmtype == "lxc" {
		argv = []string{r.opts.Tools.PCT, "restore", vmid, path}
	}
	if err := r.command(ctx, t, nil, append(argv, restoreArgs(p)...)...); err != nil {
		return jobs.Permanent(err)
	}
	r.log.Info("restored guest from offsite backup", "vmid", p.TargetVMID, "volname", s.b.Volname, "mode", "stage")
	return nil
}

// Modes returns the restore modes available for a guest type, the default
// first.
func Modes(vmtype string) []string {
	if vmtype == "qemu" {
		return []string{"stream", "stage"}
	}
	return []string{"stage"}
}

// ValidMode reports whether a mode applies to a guest type.
func ValidMode(vmtype, mode string) bool { return slices.Contains(Modes(vmtype), mode) }
