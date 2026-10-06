// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package replicate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rclone/rclone/fs/fserrors"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/jobs"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo/repotest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport/faultfs"
)

func TestMain(m *testing.M) {
	cleanup, err := repotest.InitTransport()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

const identityUUID = "1b2c3d4e-5f60-4a7b-8c9d-0e1f2a3b4c5d"

type fakeExtractor struct{}

func (fakeExtractor) Extract(context.Context, layout.Archive, io.ReaderAt, int64) (*GuestConfig, error) {
	fw := "[OPTIONS]\nenable: 1\n"
	return &GuestConfig{Config: "name: web01\nmemory: 512\n", Firewall: &fw, FirewallKnown: true}, nil
}

// faults controls failure injection on the remote.
type faults struct {
	mu       sync.Mutex
	failPuts bool // fail every upload while set
	corrupt  int  // corrupt the next n uploads
	hide     string
	puts     []string // encrypted paths in upload order
	attempts map[string]int
	onPut    func()
}

func (f *faults) controller() *faultfs.Controller {
	return &faultfs.Controller{
		PutError: func(remote string, attempt int) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.puts = append(f.puts, remote)
			if f.attempts == nil {
				f.attempts = map[string]int{}
			}
			f.attempts[remote] = attempt
			if f.onPut != nil {
				f.onPut()
			}
			if f.failPuts {
				return fserrors.RetryErrorf("injected network failure")
			}
			return nil
		},
		Corrupt: func(string) bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.corrupt > 0 {
				f.corrupt--
				return true
			}
			return false
		},
		Hide: func(remote string) bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.hide != "" && remote == f.hide
		},
	}
}

func (f *faults) set(fn func(f *faults)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *faults) putCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.puts)
}

// env is a source dump directory, a repository and a scheduler running
// the replication runner.
type env struct {
	t      *testing.T
	f      *faults
	rp     *repo.Repo
	cfg    *config.Storage
	dump   string
	st     *store.Store
	s      *jobs.Scheduler
	stop   func()
	commit []string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, f: &faults{}}
	loc, _ := repotest.Remote(t, e.f.controller())
	var err error
	if e.rp, err = repo.Init(t.Context(), loc, repo.InitOptions{Keys: repotest.NewKeys(t)}); err != nil {
		t.Fatal(err)
	}
	if err := e.rp.RegisterSource(t.Context(), "homelab", identityUUID, "lab", false); err != nil {
		t.Fatal(err)
	}
	e.cfg = &config.Storage{ID: "offsite", Remote: loc.Remote, Path: loc.Path, Encryption: "crypt", Source: "homelab",
		SegmentSize: 64 << 10, Transfers: 1, Supersede: true}
	e.dump = filepath.Join(t.TempDir(), "dump")
	if err := os.MkdirAll(e.dump, 0o700); err != nil {
		t.Fatal(err)
	}
	e.openStore()
	return e
}

func (e *env) openStore() {
	st, err := store.Open(e.t.Context(), filepath.Join(e.t.TempDir(), "state.db"))
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = st.Close() })
	if err := st.PutStorage(e.t.Context(), &store.StorageRow{StoreID: "offsite", Remote: e.cfg.Remote, BasePath: e.cfg.Path,
		Source: "homelab", ConfigHash: "x"}); err != nil {
		e.t.Fatal(err)
	}
	e.st = st
}

func (e *env) start() {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner := New(Options{
		Log: log, Store: e.st, Node: "pve1",
		Repos:     func(string) (*repo.Repo, *config.Storage, error) { return e.rp, e.cfg, nil },
		Extractor: fakeExtractor{},
		Identity:  func() (*Identity, string, error) { return &Identity{UUID: identityUUID}, "lab", nil },
		OnCommit:  func(b *store.Backup) { e.commit = append(e.commit, b.Volname) },
	})
	e.s = jobs.New(jobs.Options{Log: log, Store: e.st, Node: "pve1", Workers: 1,
		Targets: func() []*config.Storage { return []*config.Storage{e.cfg} }, Runner: runner, Poll: 20 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.s.Run(ctx); close(done) }()
	e.stop = func() { cancel(); <-done }
	e.t.Cleanup(func() {
		if e.stop != nil {
			e.stop()
		}
	})
}

func (e *env) restart() {
	e.stop()
	e.stop = nil
	e.start()
}

// archive writes an archive with settled timestamps and a task log.
func (e *env) archive(vmid, size int, seed uint64) (string, []byte) {
	e.t.Helper()
	data := make([]byte, size)
	rng := rand.New(rand.NewPCG(seed, 7))
	for i := range data {
		data[i] = byte(rng.Uint32())
	}
	name := fmt.Sprintf("vzdump-qemu-%d-2026_10_04-02_00_01.vma.zst", vmid)
	path := filepath.Join(e.dump, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(strings.TrimSuffix(path, ".vma.zst")+".log", []byte("INFO: backup finished\n"), 0o600); err != nil {
		e.t.Fatal(err)
	}
	e.age(path, time.Hour)
	return path, data
}

func (e *env) age(path string, by time.Duration) {
	e.t.Helper()
	mt := time.Now().Add(-by)
	if err := os.Chtimes(path, mt, mt); err != nil {
		e.t.Fatal(err)
	}
}

// queue inserts a job for an archive as discovery does.
func (e *env) queue(path string) int64 {
	e.t.Helper()
	return e.queueKey(path, "")
}

// queueKey is queue with a suffix for the dedupe key (a second event for
// the same archive).
func (e *env) queueKey(path, suffix string) int64 {
	e.t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		e.t.Fatal(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	a, err := layout.ParseArchiveName(filepath.Base(path))
	if err != nil {
		e.t.Fatal(err)
	}
	dev, ino, size, mtime := int64(st.Dev), int64(st.Ino), fi.Size(), fi.ModTime().UnixNano()
	now := time.Now().Unix()
	id, _, err := e.st.InsertJob(e.t.Context(), &store.Job{Kind: "replicate", StoreID: "offsite", State: jobs.StateQueued,
		DedupeKey: fmt.Sprintf("replicate:offsite:local:%s:%d:%d%s", filepath.Base(path), size, mtime, suffix), OwnerNode: "pve1",
		BackupVolname: "backup/" + filepath.Base(path), SourceStorage: "local", SourcePath: path,
		SourceDev: &dev, SourceIno: &ino, SourceSize: &size, SourceMtimeNs: &mtime, TotalBytes: &size, NextAttemptAt: &now,
		VMType: a.VMType, VMID: a.VMID, BackupTime: 1790992801})
	if err != nil {
		e.t.Fatal(err)
	}
	if e.s != nil {
		e.s.Wake()
	}
	return id
}

func (e *env) wait(id int64, states ...string) *store.Job {
	e.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		j, err := e.st.GetJob(e.t.Context(), id)
		if err == nil {
			for _, s := range states {
				if j.State == s {
					return j
				}
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("job %d is %+v, want %v", id, j, states)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// retryNow makes a waiting job due immediately.
func (e *env) retryNow(id int64) {
	e.t.Helper()
	if _, err := e.st.UpdateJob(e.t.Context(), id, []string{jobs.StateRetryWait}, "", func(j *store.Job) error {
		now := time.Now().Unix()
		j.NextAttemptAt = &now
		return nil
	}); err != nil {
		e.t.Fatal(err)
	}
	e.s.Wake()
}

func (e *env) events(id int64) string {
	ev, err := e.st.JobEvents(e.t.Context(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	var b strings.Builder
	for _, x := range ev {
		b.WriteString(x.Message + "\n")
	}
	return b.String()
}

// remoteArchive reassembles an archive from the repository.
func (e *env) remoteArchive(id layout.BackupID, count int) []byte {
	e.t.Helper()
	tgt, err := e.rp.Target(1)
	if err != nil {
		e.t.Fatal(err)
	}
	var out []byte
	for i := range count {
		part, err := tgt.ReadAll(e.t.Context(), id.Path(layout.PartName(i)), 1<<30)
		if err != nil {
			e.t.Fatal(err)
		}
		out = append(out, part...)
	}
	return out
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

var backupID = layout.BackupID{Source: "homelab", VMType: "qemu", VMID: 100, TSLabel: "2026_10_04-02_00_01"}

func TestReplicate(t *testing.T) {
	e := newEnv(t)
	path, data := e.archive(100, 300<<10+17, 1)
	if err := os.WriteFile(path+".notes", []byte("nightly web"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".protected", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e.start()
	id := e.queue(path)
	j := e.wait(id, jobs.StateComplete)
	if j.ProgressBytes != int64(len(data)) || j.NextSegment != 5 || j.BackupVolname != "backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst" {
		t.Fatalf("job = %+v", j)
	}

	m, err := e.rp.ReadManifest(t.Context(), 1, backupID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Archive.SHA256 != sha(data) || m.Archive.Size != int64(len(data)) || m.Segments.Count != 5 || m.Backup.SourceUUID != identityUUID ||
		m.Guest.Name != "web01" || m.Guest.Firewall == nil || m.Sidecars.Log == nil || m.Sidecars.NotesAtUpload != "nightly web" ||
		!m.Sidecars.ProtectedAtUpload || m.Archive.SourceNode != "pve1" || m.UploadVerification.Method != "crypt-ciphertext-hash+listing" {
		t.Fatalf("manifest = %+v", m)
	}
	if got := e.remoteArchive(backupID, 5); !bytes.Equal(got, data) {
		t.Fatal("reassembled archive differs from the source")
	}
	scanned, err := e.rp.Scan(t.Context(), "homelab")
	if err != nil || len(scanned) != 1 || scanned[0].State != repo.StateComplete || !scanned[0].Meta.Protected {
		t.Fatalf("scan = %+v, %v", scanned, err)
	}
	b, err := e.st.GetBackup(t.Context(), "offsite", j.BackupVolname)
	if err != nil || b.State != "complete" || b.VerifyLevel != 2 || b.Notes != "nightly web" || !b.Protected || b.GuestName != "web01" {
		t.Fatalf("catalogue = %+v, %v", b, err)
	}
	if len(e.commit) != 1 {
		t.Fatalf("commit notifications = %v", e.commit)
	}
	segs, _ := e.st.Segments(t.Context(), id)
	if len(segs) != 5 || segs[4].Size != 44<<10+17 || segs[4].State != "uploaded" || segs[0].StoredHash == "" {
		t.Fatalf("segments = %+v", segs)
	}

	// The same archive again (a duplicate event): nothing is uploaded.
	puts := e.f.putCount()
	again := e.queueKey(path, "-again")
	if j := e.wait(again, jobs.StateComplete); !strings.Contains(e.events(again), "already replicated") || j.ProgressBytes != int64(len(data)) {
		t.Fatalf("duplicate job = %+v\n%s", j, e.events(again))
	}
	// A touched archive with the same content is recognized by its digest.
	e.age(path, 30*time.Minute)
	touched := e.queue(path)
	e.wait(touched, jobs.StateComplete)
	if !strings.Contains(e.events(touched), "already replicated") || e.f.putCount() != puts {
		t.Fatalf("touched archive uploaded again (%d puts)\n%s", e.f.putCount()-puts, e.events(touched))
	}

	// Another archive with the same name and different content collides.
	path2, data2 := e.archive(100, 70<<10, 2)
	other := e.queue(path2)
	j = e.wait(other, jobs.StateComplete)
	if j.BackupVolname != "backup/vzdump-qemu-100-2026_10_04-02_00_01.1.vma.zst" {
		t.Fatalf("collision job = %+v", j)
	}
	collided := backupID
	collided.Collision = 1
	if got := e.remoteArchive(collided, 2); !bytes.Equal(got, data2) {
		t.Fatal("collided archive differs")
	}
}

func TestEmptyArchive(t *testing.T) {
	e := newEnv(t)
	path, _ := e.archive(100, 0, 1)
	e.start()
	j := e.wait(e.queue(path), jobs.StateComplete)
	m, err := e.rp.ReadManifest(t.Context(), 1, backupID)
	if err != nil || m.Segments.Count != 1 || m.Archive.Size != 0 || m.Archive.SHA256 != sha(nil) {
		t.Fatalf("manifest = %+v, %v (job %+v)", m, err, j)
	}
}

func TestResumeAfterFailure(t *testing.T) {
	e := newEnv(t)
	path, data := e.archive(100, 300<<10, 3)
	uploads := 0
	e.f.set(func(f *faults) {
		f.onPut = func() {
			uploads++
			if uploads == 3 { // the third segment fails until released
				f.failPuts = true
			}
		}
	})
	e.start()
	id := e.queue(path)
	j := e.wait(id, jobs.StateRetryWait)
	if j.NextSegment != 2 || j.Attempts != 1 || j.ErrorClass != "transient_network" {
		t.Fatalf("failed job = %+v", j)
	}
	e.f.set(func(f *faults) { f.failPuts, f.onPut = false, nil })
	e.retryNow(id)
	e.wait(id, jobs.StateComplete)
	e.f.mu.Lock()
	reuploaded := 0
	for _, n := range e.f.attempts {
		if n > 1 {
			reuploaded++
		}
	}
	e.f.mu.Unlock()
	if reuploaded != 1 {
		t.Fatalf("%d objects uploaded more than once, want only the failed segment", reuploaded)
	}
	if got := e.remoteArchive(backupID, 5); !bytes.Equal(got, data) {
		t.Fatal("resumed archive differs from the source")
	}
}

// After losing the state database, segments already on the remote are
// verified against the local archive and reused.
func TestReuseAfterStateLoss(t *testing.T) {
	e := newEnv(t)
	path, data := e.archive(100, 300<<10, 4)
	uploads := 0
	e.f.set(func(f *faults) {
		f.onPut = func() {
			uploads++
			if uploads == 4 {
				f.failPuts = true
			}
		}
	})
	e.start()
	e.wait(e.queue(path), jobs.StateRetryWait)
	e.stop()
	e.stop = nil

	e.f.set(func(f *faults) { f.failPuts, f.onPut = false, nil })
	before := e.f.putCount()
	e.openStore()
	e.start()
	id := e.queue(path)
	e.wait(id, jobs.StateComplete)
	if ev := e.events(id); !strings.Contains(ev, "reused 3 segments") {
		t.Fatalf("segments not reused:\n%s", ev)
	}
	// Two segments, the log, the meta document and the manifest.
	if n := e.f.putCount() - before; n != 5 {
		t.Fatalf("%d uploads after the restart", n)
	}
	if got := e.remoteArchive(backupID, 5); !bytes.Equal(got, data) {
		t.Fatal("archive differs after reuse")
	}
}

func TestSourceProblems(t *testing.T) {
	e := newEnv(t)
	e.start()

	gone, _ := e.archive(101, 10, 1)
	goneJob := e.queue(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if j := e.wait(goneJob, jobs.StateSourceLost); j.ErrorClass != "source_lost" {
		t.Fatalf("missing source = %+v", j)
	}

	changed, _ := e.archive(102, 10, 1)
	changedJob := e.queue(changed)
	if err := os.WriteFile(changed, []byte("different content"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.age(changed, time.Hour)
	e.wait(changedJob, jobs.StateSourceLost)

	link := filepath.Join(e.dump, "vzdump-qemu-103-2026_10_04-02_00_01.vma.zst")
	target, _ := e.archive(104, 10, 1)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	e.wait(e.queue(link), jobs.StateSourceLost)

	// Modified while uploading.
	during, _ := e.archive(105, 200<<10, 1)
	e.f.set(func(f *faults) {
		f.onPut = func() {
			if fh, err := os.OpenFile(during, os.O_APPEND|os.O_WRONLY, 0); err == nil {
				_, _ = fh.WriteString("more")
				_ = fh.Close()
			}
			f.onPut = nil
		}
	})
	if j := e.wait(e.queue(during), jobs.StateSourceLost); !strings.Contains(j.LastError, "modified during the upload") {
		t.Fatalf("modified source = %+v", j)
	}
}

func TestSettling(t *testing.T) {
	e := newEnv(t)
	e.start()
	fresh, _ := e.archive(100, 10, 1)
	e.age(fresh, 0)
	j := e.wait(e.queue(fresh), jobs.StateRetryWait)
	if j.Attempts != 0 || j.LastError != "waiting for the archive to settle" {
		t.Fatalf("fresh archive = %+v", j)
	}

	nolog, _ := e.archive(101, 10, 1)
	if err := os.Remove(strings.TrimSuffix(nolog, ".vma.zst") + ".log"); err != nil {
		t.Fatal(err)
	}
	e.age(nolog, 2*time.Minute)
	j = e.wait(e.queue(nolog), jobs.StateRetryWait)
	if !strings.Contains(j.LastError, "no task log") || *j.NextAttemptAt < time.Now().Add(7*time.Minute).Unix() {
		t.Fatalf("archive without log = %+v", j)
	}
	// Old enough without a log: uploaded anyway.
	old, _ := e.archive(102, 10, 1)
	if err := os.Remove(strings.TrimSuffix(old, ".vma.zst") + ".log"); err != nil {
		t.Fatal(err)
	}
	e.wait(e.queue(old), jobs.StateComplete)
}

func TestIntegrityFailures(t *testing.T) {
	e := newEnv(t)
	path, data := e.archive(100, 130<<10, 5)
	e.f.set(func(f *faults) { f.corrupt = 1 })
	e.start()
	id := e.queue(path)
	e.wait(id, jobs.StateComplete)
	if !strings.Contains(e.events(id), "failed its integrity check") {
		t.Fatalf("corruption not reported:\n%s", e.events(id))
	}
	if got := e.remoteArchive(backupID, 3); !bytes.Equal(got, data) {
		t.Fatal("archive differs after re-upload")
	}

	// Corrupted on every attempt: the job fails.
	path2, _ := e.archive(101, 10, 5)
	e.f.set(func(f *faults) { f.corrupt = 1000 })
	if j := e.wait(e.queue(path2), jobs.StateFailed); j.ErrorClass != "integrity" {
		t.Fatalf("persistent corruption = %+v", j)
	}
	e.f.set(func(f *faults) { f.corrupt = 0 })

	// A segment missing from listings after upload fails verification.
	path3, _ := e.archive(102, 10, 6)
	e.f.set(func(f *faults) {
		f.onPut = func() {
			f.hide = f.puts[len(f.puts)-1] // the first segment
			f.onPut = nil
		}
	})
	j := e.wait(e.queue(path3), jobs.StateFailed)
	if j.ErrorClass != "integrity" || !strings.Contains(j.LastError, "do not match") {
		t.Fatalf("verification failure = %+v", j)
	}
	if ev := e.events(j.ID); !strings.Contains(ev, "re-uploading 1 segments") {
		t.Fatalf("no re-upload after verification failure:\n%s", ev)
	}
}

func TestShutdownRequeuesAndResumes(t *testing.T) {
	e := newEnv(t)
	path, data := e.archive(100, 300<<10, 8)
	release, blocked := make(chan struct{}), make(chan struct{})
	uploads := 0
	e.f.set(func(f *faults) {
		f.onPut = func() {
			uploads++
			if uploads == 3 { // the third segment
				close(blocked)
				f.mu.Unlock()
				<-release
				f.mu.Lock()
			}
		}
	})
	e.start()
	id := e.queue(path)
	<-blocked
	go func() { time.Sleep(100 * time.Millisecond); close(release) }()
	e.restart()
	j := e.wait(id, jobs.StateComplete)
	if got := e.remoteArchive(backupID, 5); !bytes.Equal(got, data) || j.ProgressBytes != int64(len(data)) {
		t.Fatal("archive differs after a restart mid-upload")
	}
	if !strings.Contains(e.events(id), "interrupted") {
		t.Fatalf("no interruption recorded:\n%s", e.events(id))
	}
}

// TestRediscoveryKeepsLocalMetadata: finding an archive that is already
// replicated must not reset the catalogue entry: unpushed local changes and
// deeper verification survive, and so do notes, protection and tombstone
// when the remote meta document is unusable.
func TestRediscoveryKeepsLocalMetadata(t *testing.T) {
	ctx := t.Context()
	e := newEnv(t)
	path, _ := e.archive(100, 70<<10, 1)
	e.start()
	j := e.wait(e.queue(path), jobs.StateComplete)

	b, err := e.st.GetBackup(ctx, "offsite", j.BackupVolname)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Unix()
	b.Notes, b.Protected, b.MetaDirty, b.VerifyLevel, b.VerifiedAt, b.VerifyResult = "changed here", true, true, 3, &at, "ok"
	if err := e.st.PutBackup(ctx, b); err != nil {
		t.Fatal(err)
	}
	e.wait(e.queueKey(path, "-again"), jobs.StateComplete)
	if got, _ := e.st.GetBackup(ctx, "offsite", j.BackupVolname); got.Notes != "changed here" || !got.Protected || !got.MetaDirty || got.VerifyLevel != 3 {
		t.Fatalf("rediscovery reset the local state: %+v", got)
	}

	b.MetaDirty = false
	if err := e.st.PutBackup(ctx, b); err != nil {
		t.Fatal(err)
	}
	tgt, err := e.rp.Target(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tgt.PutBytes(ctx, backupID.Path(layout.MetaName), []byte("unusable")); err != nil {
		t.Fatal(err)
	}
	e.wait(e.queueKey(path, "-third"), jobs.StateComplete)
	if got, _ := e.st.GetBackup(ctx, "offsite", j.BackupVolname); got.Notes != "changed here" || !got.Protected {
		t.Fatalf("an unusable remote meta document reset the entry: %+v", got)
	}
}
