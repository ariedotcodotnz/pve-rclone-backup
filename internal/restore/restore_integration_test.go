// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package restore

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/catalog"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/jobs"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo/repotest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
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

type env struct {
	t       *testing.T
	rp      *repo.Repo
	st      *store.Store
	pveDir  string
	dump    string
	tools   string // directory of fake PVE tools and their logs
	staging string
}

// fakeTool writes a script that logs its arguments and stores its input
// (stdin for "-", else the named archive) in <name>.out.
func (e *env) fakeTool(name string, archiveArg int, fail bool) string {
	path := filepath.Join(e.tools, name)
	exit := "0"
	if fail {
		exit = "1"
	}
	script := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %[1]s/%[2]s.log
arg=$(eval echo \${%[3]d})
if [ "$arg" = "-" ]; then cat > %[1]s/%[2]s.out; elif [ -n "$arg" ]; then cp "$arg" %[1]s/%[2]s.out; fi
echo "progress 100%%"
exit %[4]s
`, e.tools, name, archiveArg, exit)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // test script must be executable
		e.t.Fatal(err)
	}
	return path
}

func gz(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, _ = w.Write(data)
	_ = w.Close()
	return buf.Bytes()
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, pveDir: t.TempDir(), tools: t.TempDir(), staging: filepath.Join(t.TempDir(), "staging")}
	var err error
	if e.st, err = store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.st.Close() })
	e.rp, _ = repotest.Init(t)
	e.dump = filepath.Join(t.TempDir(), "backups", "dump")
	if err := os.MkdirAll(e.dump, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("dir: backups\n\tpath %s\n\tcontent backup\n\nlvmthin: local-lvm\n\tthinpool data\n\tvgname pve\n\tcontent images,rootdir\n\ndir: isos\n\tpath /tmp\n\tcontent iso\n\n", filepath.Dir(e.dump))
	if err := os.WriteFile(filepath.Join(e.pveDir, "storage.cfg"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.pveDir, ".vmlist"), []byte(`{"ids":{"500":{"node":"pve1","type":"qemu"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := e.st.PutStorage(t.Context(), &store.StorageRow{StoreID: "offsite", Remote: "r", BasePath: "p", Source: "homelab", ConfigHash: "x"}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) write(b repotest.Backup) {
	e.t.Helper()
	repotest.Write(e.t, e.rp, b)
	if _, err := catalog.Resync(e.t.Context(), e.st, "offsite", e.rp, "homelab"); err != nil {
		e.t.Fatal(err)
	}
}

// run executes one job with a scheduler for its kind.
func (e *env) run(kind, volname string, params any) *store.Job {
	e.t.Helper()
	pj, _ := json.Marshal(params)
	id, _, err := e.st.InsertJob(e.t.Context(), &store.Job{Kind: kind, StoreID: "offsite", State: jobs.StateQueued,
		DedupeKey: fmt.Sprintf("%s:%s:%d", kind, volname, time.Now().UnixNano()), OwnerNode: "pve1",
		BackupVolname: volname, ParamsJSON: string(pj)})
	if err != nil {
		e.t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	runner := New(Options{Log: log, Store: e.st, Node: "pve1", PVEDir: e.pveDir, StagingDir: e.staging,
		Repos:   func(string) (*repo.Repo, *config.Storage, error) { return e.rp, &config.Storage{ID: "offsite"}, nil },
		Mounted: func(string) bool { return false },
		Tools:   Tools{QMRestore: e.fakeTool("qmrestore", 1, false), PCT: e.fakeTool("pct", 3, false), QM: e.fakeTool("qm", 0, false)}})
	s := jobs.New(jobs.Options{Log: log, Store: e.st, Node: "pve1", Kind: kind, Workers: 1, Runner: runner, Poll: 20 * time.Millisecond,
		Targets: func() []*config.Storage { return []*config.Storage{{ID: "offsite", Remote: "r", Transfers: 1}} }})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(20 * time.Second)
	for {
		j, err := e.st.GetJob(e.t.Context(), id)
		if err == nil && (j.State == jobs.StateComplete || j.State == jobs.StateFailed || j.State == jobs.StateRetryWait) {
			return j
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("job %d did not finish: %+v", id, j)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *env) toolLog(name string) string {
	b, _ := os.ReadFile(filepath.Join(e.tools, name+".log"))
	return string(b)
}

func (e *env) toolOut(name string) []byte {
	b, _ := os.ReadFile(filepath.Join(e.tools, name+".out"))
	return b
}

func TestFetch(t *testing.T) {
	e := newEnv(t)
	b := repotest.Backup{VMID: 100, Size: 200 << 10, Notes: "nightly", Log: "INFO: done\n"}
	e.write(b)
	vol := b.ID().Volname("vma.zst")
	j := e.run("fetch", vol, FetchParams{TargetStorage: "backups", Protect: true})
	if j.State != jobs.StateComplete || j.ProgressBytes != 200<<10 {
		t.Fatalf("fetch = %+v", j)
	}
	name := strings.TrimPrefix(vol, "backup/")
	final := filepath.Join(e.dump, name)
	got, err := os.ReadFile(final)
	if err != nil || !bytes.Equal(got, b.Data()) {
		t.Fatalf("fetched archive differs: %v", err)
	}
	if fi, _ := os.Stat(final); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", fi.Mode())
	}
	notes, _ := os.ReadFile(final + ".notes")
	if !strings.HasPrefix(string(notes), "Fetched from offsite storage offsite on ") || !strings.HasSuffix(string(notes), "\nnightly") {
		t.Fatalf("notes = %q", notes)
	}
	if _, err := os.Stat(final + ".protected"); err != nil {
		t.Fatal("not protected")
	}
	if log, _ := os.ReadFile(filepath.Join(e.dump, strings.TrimSuffix(name, ".vma.zst")+".log")); string(log) != "INFO: done\n" {
		t.Fatalf("log = %q", log)
	}
	fi, _ := os.Stat(final)
	fa, err := e.st.FetchedArchive(t.Context(), final)
	if err != nil || *fa.Ino != int64(fi.Sys().(*syscall.Stat_t).Ino) || fa.Volname != vol {
		t.Fatalf("fetched archive record = %+v, %v", fa, err)
	}
	entries, _ := os.ReadDir(e.dump)
	for _, en := range entries {
		if strings.HasPrefix(en.Name(), ".") {
			t.Fatalf("temporary file left: %s", en.Name())
		}
	}
	// A second fetch never overwrites.
	if j := e.run("fetch", vol, FetchParams{TargetStorage: "backups"}); j.State != jobs.StateFailed || !strings.Contains(j.LastError, "already exists") {
		t.Fatalf("second fetch = %+v", j)
	}
	if j := e.run("fetch", vol, FetchParams{TargetStorage: "nonexistent"}); j.State != jobs.StateFailed {
		t.Fatalf("fetch to a missing storage = %+v", j)
	}
}

func TestStreamRestore(t *testing.T) {
	e := newEnv(t)
	vma := bytes.Repeat([]byte("VMA image data "), 30000)
	b := repotest.Backup{VMID: 100, Content: gz(t, vma), Ext: "vma.gz", SegmentSize: 64 << 10}
	e.write(b)
	vol := b.ID().Volname("vma.gz")
	j := e.run("restore", vol, RestoreParams{Mode: "stream", TargetVMID: 900, TargetStorage: "local-lvm", Unique: true})
	if j.State != jobs.StateComplete {
		t.Fatalf("restore = %+v\n%s", j, e.toolLog("qmrestore"))
	}
	if !bytes.Equal(e.toolOut("qmrestore"), vma) {
		t.Fatal("qmrestore received different data")
	}
	if got := e.toolLog("qmrestore"); got != "- 900 --storage local-lvm --unique 1\n" {
		t.Fatalf("qmrestore args = %q", got)
	}
	events, _ := e.st.JobEvents(t.Context(), j.ID)
	if !strings.Contains(fmt.Sprint(events), "qmrestore: progress 100%") {
		t.Fatalf("tool output not recorded: %+v", events)
	}

	// Existing guests are only overwritten with force.
	if j := e.run("restore", vol, RestoreParams{Mode: "stream", TargetVMID: 500}); j.State != jobs.StateFailed || !strings.Contains(j.LastError, "exists") {
		t.Fatalf("restore over an existing guest = %+v", j)
	}
	if j := e.run("restore", vol, RestoreParams{Mode: "stream", TargetVMID: 500, Force: true}); j.State != jobs.StateComplete {
		t.Fatalf("forced restore = %+v", j)
	}
	if j := e.run("restore", vol, RestoreParams{Mode: "stream", TargetVMID: 901, TargetStorage: "isos"}); j.State != jobs.StateFailed {
		t.Fatalf("restore to a storage without images = %+v", j)
	}
}

func TestStreamRestoreIntegrityFailureRemovesGuest(t *testing.T) {
	e := newEnv(t)
	vma := make([]byte, 200<<10) // incompressible: several segments
	_, _ = rand.NewChaCha8([32]byte{1}).Read(vma)
	b := repotest.Backup{VMID: 100, Content: gz(t, vma), Ext: "vma.gz", SegmentSize: 16 << 10}
	e.write(b)
	// Replace a stored segment with other bytes of the same size.
	tgt, _ := e.rp.Target(1)
	part := b.ID().Path(layout.PartName(1))
	if _, err := tgt.PutBytes(t.Context(), part, bytes.Repeat([]byte{0x55}, 16<<10)); err != nil {
		t.Fatal(err)
	}
	j := e.run("restore", b.ID().Volname("vma.gz"), RestoreParams{Mode: "stream", TargetVMID: 902})
	if j.State != jobs.StateFailed || !strings.Contains(j.LastError, "segment 1") {
		t.Fatalf("restore of a tampered backup = %+v", j)
	}
	if got := e.toolLog("qm"); got != "destroy 902 --purge 1\n" {
		t.Fatalf("guest not removed: qm %q", got)
	}
}

func TestStagedRestore(t *testing.T) {
	e := newEnv(t)
	ct := repotest.Backup{VMID: 200, VMType: "lxc", Size: 100 << 10}
	e.write(ct)
	vol := ct.ID().Volname("tar.zst")
	j := e.run("restore", vol, RestoreParams{Mode: "stage", TargetVMID: 300, TargetStorage: "local-lvm"})
	if j.State != jobs.StateComplete {
		t.Fatalf("staged restore = %+v", j)
	}
	if got := e.toolLog("pct"); !strings.HasPrefix(got, "restore 300 ") || !strings.HasSuffix(got, "/"+strings.TrimPrefix(vol, "backup/")+" --storage local-lvm\n") {
		t.Fatalf("pct args = %q", got)
	}
	if !bytes.Equal(e.toolOut("pct"), ct.Data()) {
		t.Fatal("pct received different data")
	}
	if entries, _ := os.ReadDir(e.staging); len(entries) != 0 {
		t.Fatalf("staging not cleaned: %v", entries)
	}
	if j := e.run("restore", vol, RestoreParams{Mode: "stream", TargetVMID: 301}); j.State != jobs.StateFailed {
		t.Fatalf("stream restore of a container = %+v", j)
	}

	// Damaged backups need explicit consent.
	dmg := repotest.Backup{VMID: 201, VMType: "lxc", Size: 100 << 10}
	repotest.Write(t, e.rp, dmg)
	tgt, _ := e.rp.Target(1)
	_ = tgt.Remove(t.Context(), dmg.ID().Path(layout.PartName(0)))
	if _, err := catalog.Resync(t.Context(), e.st, "offsite", e.rp, "homelab"); err != nil {
		t.Fatal(err)
	}
	if j := e.run("restore", dmg.ID().Volname("tar.zst"), RestoreParams{Mode: "stage", TargetVMID: 302}); j.State != jobs.StateFailed || !strings.Contains(j.LastError, "damaged") {
		t.Fatalf("restore of a damaged backup = %+v", j)
	}
}
