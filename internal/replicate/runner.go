// SPDX-License-Identifier: AGPL-3.0-or-later

// Package replicate uploads local vzdump archives to offsite
// repositories. Archives are uploaded bit-identical as fixed-size
// segments read straight from the local file, so no local copy is made.
// Progress is recorded after every segment and an interrupted upload
// resumes where it stopped; segments already on the remote (found after a
// lost state database) are verified against the local data and reused.
// The manifest is written last and commits the backup.
package replicate

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/catalog"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/jobs"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/manifest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/storages"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/version"
)

// Timing of archive settling: an archive must be unmodified for
// SettleDelay, and vzdump's task log must exist (or LogWait have passed),
// before it is uploaded.
const (
	SettleDelay = 60 * time.Second
	LogWait     = 10 * time.Minute
	// notReadyDelay is how long a job waits for its repository to open.
	notReadyDelay = 5 * time.Minute
	// integrityRetries bounds uploads of a segment that failed its
	// integrity check, and verification rounds.
	integrityRetries = 3
	maxLogSize       = 16 << 20
)

// Options configures a Runner.
type Options struct {
	Log    *slog.Logger
	Store  *store.Store
	Node   string
	PVEDir string
	// Repos returns a storage's open repository; storages.ErrNotReady
	// postpones the job.
	Repos     func(storeID string) (*repo.Repo, *config.Storage, error)
	Extractor Extractor
	// Identity returns this installation's identity and cluster name.
	Identity func() (*Identity, string, error)
	// OnCommit is called after a backup was committed or found present.
	OnCommit func(b *store.Backup)
	Now      func() time.Time
}

// Runner implements jobs.Runner for replication jobs.
type Runner struct {
	opts Options
	log  *slog.Logger

	mu         sync.Mutex
	limiters   map[string]limiterEntry
	registered map[string]bool
}

type limiterEntry struct {
	spec string
	l    *transport.Limiter
}

// New returns a runner.
func New(opts Options) *Runner {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Extractor == nil {
		opts.Extractor = PVETools{}
	}
	return &Runner{opts: opts, log: opts.Log, limiters: map[string]limiterEntry{}, registered: map[string]bool{}}
}

// params are the decisions of a job's first run, kept so that a resumed
// upload continues with the same layout.
type params struct {
	Generation  int          `json:"generation,omitzero"`
	Collision   int          `json:"collision,omitzero"`
	SegmentSize int64        `json:"segment_size,omitzero"`
	Guest       *GuestConfig `json:"guest,omitempty"`
	GuestError  string       `json:"guest_error,omitempty"`
}

func loadParams(j *store.Job) params {
	var p params
	_ = json.Unmarshal([]byte(cmp.Or(j.ParamsJSON, "{}")), &p)
	return p
}

// upload is the state of one run of a job.
type upload struct {
	t      *jobs.Task
	rp     *repo.Repo
	cfg    *config.Storage
	gen    int
	id     layout.BackupID
	src    *source
	segs   []store.Segment
	segLen int64
	tgt    *transport.Target
	lim    *transport.Limiter
}

// Run implements jobs.Runner.
func (r *Runner) Run(ctx context.Context, t *jobs.Task) error {
	j := t.Job()
	now := r.opts.Now()
	rp, cfg, err := r.opts.Repos(j.StoreID)
	if errors.Is(err, storages.ErrNotReady) {
		return jobs.Postpone(now.Add(notReadyDelay), err.Error())
	}
	if err != nil {
		return jobs.Permanent(err)
	}
	if j.SourceMtimeNs == nil || j.SourcePath == "" {
		return jobs.Permanent(errors.New("job has no source archive"))
	}
	mtime := time.Unix(0, *j.SourceMtimeNs)
	if now.Before(mtime.Add(SettleDelay)) {
		return jobs.Postpone(mtime.Add(SettleDelay), "waiting for the archive to settle")
	}
	src, err := openSource(filepath.Split(j.SourcePath))
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	if err := src.matches(&j); err != nil {
		return err
	}
	if _, err := os.Lstat(src.logPath()); errors.Is(err, os.ErrNotExist) && now.Before(mtime.Add(LogWait)) {
		return jobs.Postpone(mtime.Add(LogWait), "waiting for vzdump to finish (no task log yet)")
	}

	if err := r.ensureSource(ctx, rp, cfg.Source); err != nil {
		return err
	}
	p := loadParams(&j)
	u := &upload{t: t, rp: rp, cfg: cfg, src: src, lim: r.limiter(cfg)}
	u.gen = cmp.Or(p.Generation, rp.ActiveGeneration())
	if u.tgt, err = rp.Target(u.gen); err != nil {
		return jobs.Permanent(err)
	}
	base := layout.BackupID{Source: cfg.Source, VMType: src.archive.VMType, VMID: src.archive.VMID, TSLabel: src.archive.TSLabel}
	id, existing, err := r.chooseIdentity(ctx, u, base, p.Collision)
	if err != nil {
		return err
	}
	u.id = id
	if existing != nil {
		return r.present(ctx, u, existing)
	}

	if p.Guest == nil {
		gc, err := r.opts.Extractor.Extract(ctx, src.archive, src.f, src.size)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			p.GuestError = err.Error()
			gc = &GuestConfig{}
			_ = t.Event(ctx, "warn", "guest configuration not recorded: "+err.Error())
		}
		p.Guest = gc
	}
	p.Generation, p.Collision = u.gen, id.Collision
	u.segLen = cmp.Or(p.SegmentSize, cfg.SegmentSize)
	p.SegmentSize = u.segLen
	pj, err := json.Marshal(p)
	if err != nil {
		return err
	}
	size := src.size
	if err := t.Update(ctx, func(j *store.Job) {
		j.BackupVolname, j.ParamsJSON, j.TotalBytes = id.Volname(src.archive.Ext), string(pj), &size
	}); err != nil {
		return err
	}
	if err := u.planSegments(ctx); err != nil {
		return err
	}

	if err := t.Advance(ctx, jobs.StateUploading, fmt.Sprintf("uploading %d segments to %s", len(u.segs), id.Volname(src.archive.Ext)), nil); err != nil {
		return err
	}
	if err := u.uploadSegments(ctx); err != nil {
		return err
	}
	for round := 1; ; round++ {
		if err := t.Advance(ctx, jobs.StateVerifying, "verifying stored segments", nil); err != nil {
			return err
		}
		bad, err := u.verify(ctx)
		if err != nil {
			return err
		}
		if bad == 0 {
			break
		}
		if round >= integrityRetries {
			return fmt.Errorf("%w: %d segments still do not match after %d uploads", transport.ErrIntegrity, bad, round)
		}
		if err := t.Advance(ctx, jobs.StateUploading, fmt.Sprintf("re-uploading %d segments that failed verification", bad), nil); err != nil {
			return err
		}
		if err := u.uploadSegments(ctx); err != nil {
			return err
		}
	}
	if err := t.Advance(ctx, jobs.StateCommitting, "writing manifest", nil); err != nil {
		return err
	}
	return r.commit(ctx, u, p)
}

// limiter returns the shared bandwidth limiter of a storage.
func (r *Runner) limiter(cfg *config.Storage) *transport.Limiter {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.limiters[cfg.ID]; ok && e.spec == cfg.BwLimit {
		return e.l
	}
	var l *transport.Limiter
	if cfg.BwLimit != "" {
		var err error
		if l, err = transport.NewLimiter(cfg.BwLimit); err != nil {
			r.log.Warn("invalid bandwidth limit; uploading without limit", "storage", cfg.ID, "err", err)
		}
	}
	r.limiters[cfg.ID] = limiterEntry{spec: cfg.BwLimit, l: l}
	return l
}

// ensureSource registers this installation as the owner of the source
// namespace (once per repository and process).
func (r *Runner) ensureSource(ctx context.Context, rp *repo.Repo, name string) error {
	key := rp.UUID() + "/" + name
	r.mu.Lock()
	done := r.registered[key]
	r.mu.Unlock()
	if done {
		return nil
	}
	if r.opts.Identity == nil {
		return jobs.Permanent(errors.New("no installation identity"))
	}
	ident, clusterName, err := r.opts.Identity()
	if err != nil {
		return fmt.Errorf("replicate: installation identity: %w", err)
	}
	if err := rp.RegisterSource(ctx, name, ident.UUID, clusterName, false); err != nil {
		if errors.Is(err, repo.ErrSourceTaken) {
			return jobs.Permanent(fmt.Errorf("%w; set another rclone-source for this storage", err))
		}
		return err
	}
	r.mu.Lock()
	r.registered[key] = true
	r.mu.Unlock()
	return nil
}

// chooseIdentity finds the backup directory for the archive: the first
// collision index that is free or already holds this archive (same name,
// size and modification time, or the same content).
func (r *Runner) chooseIdentity(ctx context.Context, u *upload, base layout.BackupID, start int) (layout.BackupID, *manifest.Manifest, error) {
	var sum string
	for n := start; n <= 9999; n++ {
		id := base
		id.Collision = n
		m, err := u.rp.ReadManifest(ctx, u.gen, id)
		switch {
		case transport.Classify(err) == transport.ClassNotFound:
			return id, nil, nil
		case errors.Is(err, repo.ErrInvalidDocument):
			_ = u.t.Event(ctx, "warn", fmt.Sprintf("%s holds an unreadable manifest; using another directory", id))
			continue
		case err != nil:
			return id, nil, err
		}
		a := m.Archive
		if a.Filename == u.src.name && a.Size == u.src.size && a.SourceMtime.UnixNano() == u.src.mtimeNs {
			return id, m, nil
		}
		if a.Size == u.src.size {
			if sum == "" {
				if sum, _, err = transport.HashSection(u.src.f, 0, u.src.size, nil); err != nil {
					return id, nil, err
				}
			}
			if sum == a.SHA256 {
				return id, m, nil
			}
		}
	}
	return base, nil, jobs.Permanent(fmt.Errorf("no free backup directory for %s", base))
}

// present records a backup that is already in the repository.
func (r *Runner) present(ctx context.Context, u *upload, m *manifest.Manifest) error {
	meta, err := u.rp.ReadMeta(ctx, u.gen, u.id)
	if errors.Is(err, repo.ErrInvalidDocument) {
		meta, err = nil, nil
	}
	if err != nil {
		return err
	}
	state := repo.StateComplete
	if meta != nil && meta.Tombstone != nil {
		state = repo.StateTombstoned
	}
	b, err := r.catalogue(ctx, u, m, meta, state)
	if err != nil {
		return err
	}
	size := u.src.size
	return u.t.Advance(ctx, jobs.StateComplete, "already replicated as "+b.Volname, func(j *store.Job) {
		j.BackupVolname, j.ProgressBytes = b.Volname, size
	})
}

func (r *Runner) catalogue(ctx context.Context, u *upload, m *manifest.Manifest, meta *manifest.Meta, state string) (*store.Backup, error) {
	b, err := catalog.Entry(u.cfg.ID, repo.ScannedBackup{Generation: u.gen, ID: u.id, State: state, Manifest: m, Meta: meta}, r.opts.Now())
	if err != nil {
		return nil, err
	}
	if err := r.opts.Store.PutBackup(ctx, b); err != nil {
		return nil, err
	}
	if r.opts.OnCommit != nil {
		r.opts.OnCommit(b)
	}
	return b, nil
}

// segmentCount returns the number of segments of an archive (at least
// one, so that empty archives have a part too).
func segmentCount(size, segLen int64) int {
	return max(1, int((size+segLen-1)/segLen))
}

// planSegments creates the job's segment rows, keeping those of an earlier
// run.
func (u *upload) planSegments(ctx context.Context) error {
	j := u.t.Job()
	existing, err := u.t.Store().Segments(ctx, j.ID)
	if err != nil {
		return err
	}
	n := segmentCount(u.src.size, u.segLen)
	if n > manifest.MaxSegments {
		// Refused before uploading for hours: its manifest could not be
		// committed.
		needMiB := (u.src.size/manifest.MaxSegments + 1<<20 - 1) >> 20
		return jobs.Permanent(fmt.Errorf("the archive needs %d segments of %d bytes, more than %d; set rclone-segment-size to at least %dM",
			n, u.segLen, manifest.MaxSegments, needMiB+1))
	}
	u.segs = make([]store.Segment, n)
	for i := range n {
		off := int64(i) * u.segLen
		seg := store.Segment{JobID: j.ID, Index: i, Offset: off, Size: min(u.segLen, u.src.size-off), State: "pending"}
		if i < len(existing) && existing[i].Offset == seg.Offset && existing[i].Size == seg.Size {
			seg = existing[i]
		} else if err := u.t.Store().PutSegment(ctx, seg); err != nil {
			return err
		}
		u.segs[i] = seg
	}
	return nil
}

func (u *upload) segment(i int) transport.Segment {
	s := u.segs[i]
	return transport.Segment{Source: u.src.f, Offset: s.Offset, Size: s.Size, ModTime: time.Unix(0, u.src.mtimeNs), Limiter: u.lim}
}

// uploadSegments uploads every segment not yet uploaded. Segments are
// first uploaded in order, so the whole-archive hash advances with the
// frontier; later repairs of verified-bad segments leave it alone.
func (u *upload) uploadSegments(ctx context.Context) error {
	entries, err := u.tgt.List(ctx, u.id.Dir())
	if err != nil && transport.Classify(err) != transport.ClassNotFound {
		return err
	}
	onRemote := map[string]transport.Entry{}
	for _, e := range entries {
		onRemote[e.Name] = e
	}
	j := u.t.Job()
	next, whole := j.NextSegment, j.HashState
	if next == 0 || whole == nil {
		next, whole = 0, transport.NewWholeHashState()
	}
	reused := 0
	for i := range u.segs {
		if u.segs[i].State != "pending" {
			continue
		}
		if i > next {
			return fmt.Errorf("replicate: segment %d pending beyond the upload frontier %d", i, next)
		}
		var wholeIn []byte
		if i == next {
			wholeIn = whole
		}
		seg := u.segs[i]
		remote := u.id.Path(layout.PartName(i))
		var rec *store.Segment
		var wholeOut []byte
		if e, ok := onRemote[layout.PartName(i)]; ok && e.Size == seg.Size && e.StoredSize == u.tgt.StoredSize(seg.Size) {
			if same, err := u.tgt.VerifySegment(ctx, remote, u.segment(i)); err == nil && same {
				sha, state, err := transport.HashSection(u.src.f, seg.Offset, seg.Size, wholeIn)
				if err != nil {
					return err
				}
				stored := e.StoredSize
				rec = &store.Segment{JobID: seg.JobID, Index: i, Offset: seg.Offset, Size: seg.Size, SHA256: sha,
					StoredSize: &stored, StoredHashType: u.tgt.ProviderHashType().String(), StoredHash: e.StoredHash, State: "uploaded"}
				wholeOut = state
				reused++
			} else if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		if rec == nil {
			res, err := u.putWithRetries(ctx, remote, i, wholeIn)
			if err != nil {
				return err
			}
			stored := res.StoredSize
			rec = &store.Segment{JobID: seg.JobID, Index: i, Offset: seg.Offset, Size: seg.Size, SHA256: res.SHA256,
				StoredSize: &stored, StoredHashType: res.StoredHashType, StoredHash: res.StoredHash, State: "uploaded"}
			wholeOut = res.WholeState
		}
		progress := u.t.Job().ProgressBytes
		if i == next {
			next, whole, progress = i+1, wholeOut, seg.Offset+seg.Size
		}
		if err := u.t.RecordSegment(ctx, *rec, next, whole, progress); err != nil {
			return err
		}
		u.segs[i] = *rec
	}
	if reused > 0 {
		_ = u.t.Event(ctx, "info", fmt.Sprintf("reused %d segments already on the remote", reused))
	}
	return nil
}

func (u *upload) putWithRetries(ctx context.Context, remote string, i int, whole []byte) (*transport.SegmentResult, error) {
	for attempt := 1; ; attempt++ {
		res, err := u.tgt.PutSegment(ctx, remote, u.segment(i), whole)
		if err == nil {
			return res, nil
		}
		if transport.Classify(err) != transport.ClassIntegrity || attempt >= integrityRetries {
			return nil, err
		}
		_ = u.t.Event(ctx, "warn", fmt.Sprintf("segment %d failed its integrity check, uploading again: %v", i, err))
	}
}

// verify compares the stored segments with what was uploaded (presence,
// sizes and provider hashes), marks mismatches for upload and removes
// stray parts. It returns the number of bad segments.
func (u *upload) verify(ctx context.Context) (int, error) {
	entries, err := u.tgt.List(ctx, u.id.Dir())
	if err != nil && transport.Classify(err) != transport.ClassNotFound {
		return 0, err
	}
	byName := map[string]transport.Entry{}
	for _, e := range entries {
		byName[e.Name] = e
		if idx, ok := layout.ParsePart(e.Name); ok && idx >= len(u.segs) {
			if err := u.tgt.Remove(ctx, u.id.Path(e.Name)); err != nil && transport.Classify(err) != transport.ClassNotFound {
				return 0, err
			}
		}
	}
	bad := 0
	for i, seg := range u.segs {
		e, ok := byName[layout.PartName(i)]
		if ok && e.Size == seg.Size && seg.StoredSize != nil && e.StoredSize == *seg.StoredSize &&
			(e.StoredHash == "" || seg.StoredHash == "" || e.StoredHash == seg.StoredHash) {
			continue
		}
		bad++
		seg.State = "pending"
		if err := u.t.Store().PutSegment(ctx, seg); err != nil {
			return 0, err
		}
		u.segs[i] = seg
	}
	return bad, nil
}

// commit writes the log, meta document and manifest, then records the
// backup in the catalogue.
func (r *Runner) commit(ctx context.Context, u *upload, p params) error {
	if err := u.src.unchanged(); err != nil {
		return err
	}
	j := u.t.Job()
	sum, err := transport.WholeHashSum(j.HashState)
	if err != nil {
		return err
	}
	now := r.opts.Now()
	notes, protected, err := u.src.sidecars()
	if err != nil {
		return err
	}
	m := &manifest.Manifest{
		Format: manifest.FormatManifest, Version: 1, RepoUUID: u.rp.UUID(), Generation: u.gen,
		Backup: manifest.BackupInfo{Source: u.id.Source, VMType: u.id.VMType, VMID: u.id.VMID, TSLabel: u.id.TSLabel,
			BackupTime: j.BackupTime, CollisionIndex: u.id.Collision, Volname: u.id.Volname(u.src.archive.Ext)},
		Archive: manifest.ArchiveInfo{Filename: u.src.name, Format: u.src.archive.Format, Compression: u.src.archive.Compression,
			Size: u.src.size, SHA256: sum, SourceStorage: j.SourceStorage, SourceNode: r.opts.Node,
			SourceMtime: time.Unix(0, u.src.mtimeNs).UTC()},
		Segments:           manifest.Segments{Size: u.segLen, Count: len(u.segs), Naming: "part.%06d"},
		Guest:              manifest.GuestInfo{Config: p.Guest.Config, Firewall: p.Guest.Firewall, FirewallKnown: p.Guest.FirewallKnown},
		Sidecars:           manifest.Sidecars{NotesAtUpload: notes, ProtectedAtUpload: protected},
		Tool:               manifest.ToolInfo{Version: version.Get().Version, Rclone: transport.RcloneVersion()},
		UploadedAt:         now.UTC(),
		UploadVerification: manifest.Verification{Level: 2, Method: verificationMethod(u.tgt)},
	}
	if ident, _, err := r.identity(); err == nil {
		m.Backup.SourceUUID = ident.UUID
	}
	m.Guest.Name = guestName(u.id.VMType, p.Guest.Config)
	for _, s := range u.segs {
		info := manifest.SegmentInfo{Index: s.Index, Size: s.Size, SHA256: s.SHA256, StoredSize: *s.StoredSize}
		if s.StoredHash != "" {
			info.StoredHash = map[string]string{s.StoredHashType: s.StoredHash}
		}
		m.Segments.List = append(m.Segments.List, info)
	}
	if data, err := readSmall(u.src.logPath(), maxLogSize); err == nil {
		if m.Sidecars.Log, err = u.rp.PutLog(ctx, u.gen, u.id, data); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = u.t.Event(ctx, "warn", "task log not uploaded: "+err.Error())
	}
	meta := manifest.NewMeta(now)
	meta.Notes, meta.Protected = notes, protected
	if err := u.rp.Commit(ctx, m, meta); err != nil {
		return err
	}
	b, err := r.catalogue(ctx, u, m, meta, repo.StateComplete)
	if err != nil {
		return err
	}
	size := u.src.size
	return u.t.Advance(ctx, jobs.StateComplete, "committed "+b.Volname, func(j *store.Job) { j.ProgressBytes = size })
}

func (r *Runner) identity() (*Identity, string, error) {
	if r.opts.Identity == nil {
		return nil, "", errors.New("no installation identity")
	}
	return r.opts.Identity()
}

func verificationMethod(t *transport.Target) string {
	switch {
	case t.Encrypted():
		return "crypt-ciphertext-hash+listing"
	case t.ProviderHashType().String() != "none":
		return "provider-hash+listing"
	}
	return "size+listing"
}
