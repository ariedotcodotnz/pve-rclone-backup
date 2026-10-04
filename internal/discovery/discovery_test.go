// SPDX-License-Identifier: AGPL-3.0-or-later

package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/pve/storagecfg"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSources(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"a/dump", "b/custom/backups", "c/dump", "e/dump"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg, _ := storagecfg.Parse(fmt.Appendf(nil, `dir: sa
	path %[1]s/a
	content backup

dir: sb
	path %[1]s/b
	content-dirs backup=/custom/backups,iso=iso
	shared 1

nfs: sc
	path %[1]s/c
	server 10.0.0.1
	export /x

dir: sd
	path %[1]s/d

dir: se
	path %[1]s/e
	is_mountpoint yes

dir: sf
	path %[1]s/a
	disable 1

dir: sg
	path %[1]s/a
	nodes other

pbs: sh
	server 10.0.0.2
	datastore x

dir: si
	path %[1]s/a
	content-dirs backup=../../etc
`, root))
	target := &config.Storage{ID: "offsite", ReplicateFrom: []string{"sa", "sb", "sc", "sd", "se", "sf", "sg", "sh", "si", "missing"}}
	mounted := func(p string) bool { return p == root+"/e" }
	got := map[string]*Source{}
	for _, s := range ResolveSources(cfg, []*config.Storage{target}, "pve1", mounted) {
		got[s.StoreID] = s
	}
	want := map[string]string{
		"sa": "", "sb": "", "sc": "is not mounted", "sd": "does not exist", "se": "", "sf": "disabled",
		"sh": "no absolute path", "si": "invalid backup directory", "missing": "does not exist",
	}
	if len(got) != len(want) {
		t.Errorf("sources = %v", slices.Collect(func(yield func(string) bool) {
			for k := range got {
				if !yield(k) {
					return
				}
			}
		}))
	}
	for id, problem := range want {
		s := got[id]
		if s == nil || (problem == "") != (s.Problem == "") || !strings.Contains(s.Problem, problem) {
			t.Errorf("%s: %+v, want problem %q", id, s, problem)
		}
	}
	if got["sb"].Dir != root+"/b/custom/backups" || !got["sb"].Shared || got["sa"].Shared || !got["sc"].Shared {
		t.Errorf("sb = %+v", got["sb"])
	}
	if len(got["sa"].Targets) != 1 || got["sa"].Targets[0] != target {
		t.Errorf("targets of sa = %v", got["sa"].Targets)
	}
}

func TestUnescapeMount(t *testing.T) {
	if got := unescapeMount(`/mnt/with\040space\134x`); got != `/mnt/with space\x` {
		t.Fatalf("unescape = %q", got)
	}
}

// fixture is a PVE directory with one local dump directory.
type fixture struct {
	t       *testing.T
	pveDir  string
	dump    string
	st      *store.Store
	targets []*config.Storage
	now     time.Time
	ready   error
	jobs    atomic.Int64
}

func newFixture(t *testing.T, sourceOpts string, targets ...string) *fixture {
	t.Helper()
	f := &fixture{t: t, pveDir: t.TempDir(), now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local)}
	f.dump = filepath.Join(t.TempDir(), "backups", "dump")
	if err := os.MkdirAll(f.dump, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("dir: backups\n\tpath %s\n\tcontent backup\n%s\n", filepath.Dir(f.dump), sourceOpts)
	for i, props := range targets {
		cfg += fmt.Sprintf("rclone-backup: t%d\n\trclone-remote od\n\trclone-source homelab\n\trclone-replicate-from backups\n%s\n", i+1, props)
	}
	write(t, filepath.Join(f.pveDir, "storage.cfg"), cfg)
	parsed, _ := storagecfg.Parse([]byte(cfg))
	for _, sec := range parsed.OfType(config.StorageType) {
		sc, err := config.DecodeStorage(sec.ID, sec.Props)
		if err != nil {
			t.Fatal(err)
		}
		f.targets = append(f.targets, sc)
	}
	write(t, filepath.Join(f.pveDir, ".vmlist"), `{"ids":{"100":{"node":"pve1","type":"qemu"},"200":{"node":"pve1","type":"lxc"},"300":{"node":"pve2","type":"qemu"}}}`)
	write(t, filepath.Join(f.pveDir, "nodes/pve1/qemu-server/100.conf"), "tags: offsite;web\n")
	write(t, filepath.Join(f.pveDir, "nodes/pve1/lxc/200.conf"), "hostname: ct\n")
	write(t, filepath.Join(f.pveDir, "nodes/pve2/qemu-server/300.conf"), "tags: offsite\n")
	var err error
	if f.st, err = store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.st.Close() })
	return f
}

func (f *fixture) discoverer(inotify bool) *Discoverer {
	return New(Options{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Store: f.st, Node: "pve1", PVEDir: f.pveDir,
		Targets: func() []*config.Storage { return f.targets },
		Ready:   func(string) error { return f.ready },
		Mounted: func(string) bool { return false }, Inotify: inotify, ScanInterval: time.Hour,
		OnJob: func(int64, string) { f.jobs.Add(1) },
		Now:   func() time.Time { return f.now },
	})
}

// archive creates a finished archive with the given age relative to the
// fixture's clock.
func (f *fixture) archive(vmtype string, vmid int, day int, age time.Duration) string {
	f.t.Helper()
	name := fmt.Sprintf("vzdump-%s-%d-2026_10_%02d-02_00_01.%s", vmtype, vmid, day, map[string]string{"qemu": "vma.zst", "lxc": "tar.zst"}[vmtype])
	path := filepath.Join(f.dump, name)
	write(f.t, path, name)
	mt := f.now.Add(-age)
	if err := os.Chtimes(path, mt, mt); err != nil {
		f.t.Fatal(err)
	}
	return path
}

// states returns job states by target and archive name.
func (f *fixture) states() map[string]string {
	f.t.Helper()
	jobs, err := f.st.ListJobs(f.t.Context(), store.JobFilter{Kind: "replicate"})
	if err != nil {
		f.t.Fatal(err)
	}
	out := map[string]string{}
	for _, j := range jobs {
		out[j.StoreID+"/"+filepath.Base(j.SourcePath)] = j.State
	}
	return out
}

func TestScanQueuesAndFilters(t *testing.T) {
	f := newFixture(t, "",
		"\trclone-backfill all\n\trclone-exclude-vmids 300\n",
		"\trclone-guests tagged\n\trclone-backfill all\n\trclone-supersede 0\n")
	// Existing archives (before replication starts) and noise.
	f.archive("qemu", 100, 1, 48*time.Hour)
	f.archive("qemu", 100, 2, 24*time.Hour)
	f.archive("lxc", 200, 2, 24*time.Hour)
	f.archive("qemu", 300, 2, 24*time.Hour)
	write(t, filepath.Join(f.dump, "vzdump-qemu-101-2026_10_09-02_00_01.vma.dat"), "partial")
	write(t, filepath.Join(f.dump, "vzdump-qemu-100-2026_10_02-02_00_01.log"), "log")
	write(t, filepath.Join(f.dump, "vzdump-qemu-100-2026_10_02-02_00_01.vma.zst.notes"), "notes")
	if err := os.Symlink("/etc/passwd", filepath.Join(f.dump, "vzdump-qemu-102-2026_10_02-02_00_01.vma.zst")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(f.dump, "vzdump-qemu-103-2026_10_02-02_00_01.vma.zst"), 0o700); err != nil {
		t.Fatal(err)
	}

	d := f.discoverer(false)
	sum, err := d.Scan(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	if sum.Sources != 1 || sum.Archives != 4 {
		t.Fatalf("summary = %+v", sum)
	}
	want := map[string]string{
		"t1/vzdump-qemu-100-2026_10_01-02_00_01.vma.zst": "superseded",
		"t1/vzdump-qemu-100-2026_10_02-02_00_01.vma.zst": "queued",
		"t1/vzdump-lxc-200-2026_10_02-02_00_01.tar.zst":  "queued",
		"t1/vzdump-qemu-300-2026_10_02-02_00_01.vma.zst": "skipped",
		"t2/vzdump-qemu-100-2026_10_01-02_00_01.vma.zst": "queued",
		"t2/vzdump-qemu-100-2026_10_02-02_00_01.vma.zst": "queued",
		"t2/vzdump-lxc-200-2026_10_02-02_00_01.tar.zst":  "skipped",
		"t2/vzdump-qemu-300-2026_10_02-02_00_01.vma.zst": "queued",
	}
	if got := f.states(); !equalMaps(got, want) {
		t.Fatalf("jobs = %v\nwant %v", got, want)
	}
	if f.jobs.Load() != 9 { // 8 created + 1 superseded
		t.Errorf("OnJob calls = %d", f.jobs.Load())
	}

	jobs, _ := f.st.ListJobs(t.Context(), store.JobFilter{StoreID: "t1", States: []string{"queued"}})
	for _, j := range jobs {
		if j.VMType == "qemu" {
			fi, _ := os.Stat(j.SourcePath)
			if j.BackupVolname != "backup/vzdump-qemu-100-2026_10_02-02_00_01.vma.zst" || j.VMID != 100 ||
				*j.SourceSize != fi.Size() || *j.SourceMtimeNs != fi.ModTime().UnixNano() || j.SourceStorage != "backups" ||
				*j.NextAttemptAt != fi.ModTime().Add(SettleDelay).Unix() || j.OwnerNode != "pve1" {
				t.Errorf("job = %+v", j)
			}
		}
	}
	skipped, _ := f.st.ListJobs(t.Context(), store.JobFilter{StoreID: "t2", States: []string{"skipped"}})
	if len(skipped) != 1 || !strings.Contains(skipped[0].LastError, "none of the tags") {
		t.Errorf("skipped = %+v", skipped)
	}

	// A second scan finds nothing new.
	sum, err = d.Scan(t.Context(), "")
	if err != nil || sum.Known != 8 || sum.Queued+sum.Skipped+sum.Superseded != 0 {
		t.Fatalf("rescan = %+v, %v", sum, err)
	}
	// A modified archive is a new candidate.
	if err := os.Chtimes(filepath.Join(f.dump, "vzdump-lxc-200-2026_10_02-02_00_01.tar.zst"), f.now, f.now); err != nil {
		t.Fatal(err)
	}
	if sum, _ := d.Scan(t.Context(), "t1"); sum.Queued != 1 || sum.Known != 3 {
		t.Fatalf("scan after modification = %+v", sum)
	}
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestBackfill(t *testing.T) {
	for _, c := range []struct {
		policy string
		want   map[string]string
	}{
		{"none", map[string]string{"d1": "skipped", "d2": "skipped", "new": "queued"}},
		{"latest", map[string]string{"d1": "skipped", "d2": "superseded", "new": "queued"}},
		{"all", map[string]string{"d1": "superseded", "d2": "superseded", "new": "queued"}},
	} {
		t.Run(c.policy, func(t *testing.T) {
			f := newFixture(t, "", "\trclone-backfill "+c.policy+"\n")
			names := map[string]string{
				"d1": filepath.Base(f.archive("qemu", 100, 1, 48*time.Hour)),
				"d2": filepath.Base(f.archive("qemu", 100, 2, 24*time.Hour)),
			}
			d := f.discoverer(false)
			if _, err := d.Scan(t.Context(), ""); err != nil {
				t.Fatal(err)
			}
			// Replication is enabled now; an archive written afterwards is new.
			f.now = f.now.Add(2 * time.Hour)
			names["new"] = filepath.Base(f.archive("qemu", 100, 10, time.Minute))
			if _, err := d.Scan(t.Context(), ""); err != nil {
				t.Fatal(err)
			}
			got := f.states()
			for k, state := range c.want {
				if got["t1/"+names[k]] != state {
					t.Errorf("%s: %s, want %s", k, got["t1/"+names[k]], state)
				}
			}
		})
	}
}

func TestMinInterval(t *testing.T) {
	f := newFixture(t, "", "\trclone-backfill all\n\trclone-min-interval 7d\n\trclone-supersede 0\n")
	d1 := filepath.Base(f.archive("qemu", 100, 1, 9*24*time.Hour))
	d3 := filepath.Base(f.archive("qemu", 100, 3, 7*24*time.Hour))
	d9 := filepath.Base(f.archive("qemu", 100, 9, 24*time.Hour))
	ct := filepath.Base(f.archive("lxc", 200, 3, 7*24*time.Hour))
	d := f.discoverer(false)
	if _, err := d.Scan(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	got := f.states()
	for name, want := range map[string]string{d1: "queued", d3: "skipped", d9: "queued", ct: "queued"} {
		if got["t1/"+name] != want {
			t.Errorf("%s: %s, want %s", name, got["t1/"+name], want)
		}
	}
	// An offsite backup in the catalogue counts too.
	if err := f.st.PutStorage(t.Context(), &store.StorageRow{StoreID: "t1", Remote: "od", BasePath: "p", Source: "homelab", ConfigHash: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := f.st.PutBackup(t.Context(), &store.Backup{StoreID: "t1", Volname: "backup/vzdump-lxc-200-2026_10_05-02_00_01.tar.zst",
		VMType: "lxc", VMID: 200, BackupTime: f.now.Add(-5 * 24 * time.Hour).Unix(), TSLabel: "2026_10_05-02_00_01",
		RemoteDir: "g1/x", Generation: 1, State: "complete", ArchiveSize: 1, ArchiveSHA256: strings.Repeat("0", 64),
		ArchiveFormat: "tar", SegmentSize: 1, SegmentCount: 1, ManifestJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	ct8 := filepath.Base(f.archive("lxc", 200, 8, 2*24*time.Hour))
	if _, err := d.Scan(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	if got := f.states()["t1/"+ct8]; got != "skipped" {
		t.Errorf("container archive within the interval of an offsite backup: %s", got)
	}
}

func TestSharedSourceOwnership(t *testing.T) {
	f := newFixture(t, "\tshared 1\n", "\trclone-backfill all\n")
	mine := filepath.Base(f.archive("qemu", 100, 1, time.Hour))
	theirs := filepath.Base(f.archive("qemu", 300, 1, time.Hour))
	gone := filepath.Base(f.archive("qemu", 400, 1, time.Hour))
	d := f.discoverer(false)
	if _, err := d.Scan(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	got := f.states()
	if got["t1/"+mine] != "queued" || got["t1/"+gone] != "queued" {
		t.Errorf("states = %v", got)
	}
	if _, ok := got["t1/"+theirs]; ok {
		t.Errorf("archive of a guest on another node was recorded: %v", got)
	}
	// When another node is primary, archives of deleted guests are theirs.
	write(t, filepath.Join(f.pveDir, ".members"), `{"nodename":"pve1","nodelist":{"pve0":{"online":1},"pve1":{"online":1}}}`)
	gone2 := filepath.Base(f.archive("qemu", 401, 1, time.Hour))
	if _, err := d.Scan(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.states()["t1/"+gone2]; ok {
		t.Error("non-primary node took an archive of a deleted guest")
	}
}

func TestFetchedArchivesAreSkipped(t *testing.T) {
	f := newFixture(t, "", "\trclone-backfill all\n")
	byRow := f.archive("qemu", 100, 1, time.Hour)
	byXattr := f.archive("qemu", 100, 2, time.Hour)
	fi, _ := os.Stat(byRow)
	ino := int64(fi.Sys().(*syscall.Stat_t).Ino)
	if err := f.st.AddFetchedArchive(t.Context(), store.FetchedArchive{Path: byRow, Ino: &ino, StoreID: "t1", Volname: "backup/x"}); err != nil {
		t.Fatal(err)
	}
	xattrOK := unix.Lsetxattr(byXattr, OriginXattr, []byte("t1"), 0) == nil
	d := f.discoverer(false)
	if _, err := d.Scan(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	got := f.states()
	if got["t1/"+filepath.Base(byRow)] != "skipped" {
		t.Errorf("fetched archive: %v", got)
	}
	if xattrOK && got["t1/"+filepath.Base(byXattr)] != "skipped" {
		t.Errorf("archive with origin marker: %v", got)
	}
}

func TestNotReadyTargetsRecordNothing(t *testing.T) {
	f := newFixture(t, "", "\trclone-backfill none\n")
	f.archive("qemu", 100, 1, time.Hour)
	f.ready = errors.New("recovery kit not exported")
	d := f.discoverer(false)
	if _, err := d.Scan(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	if got := f.states(); len(got) != 0 {
		t.Fatalf("jobs for a target that is not ready: %v", got)
	}
	// Replication starts when the target becomes ready: archives from
	// before then are pre-existing.
	f.ready = nil
	f.now = f.now.Add(time.Hour)
	if _, err := d.Scan(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	for _, state := range f.states() {
		if state != "skipped" {
			t.Fatalf("pre-existing archive not subject to backfill: %v", f.states())
		}
	}
}

func TestInotifyAndHook(t *testing.T) {
	f := newFixture(t, "", "\trclone-backfill none\n")
	d := f.discoverer(true)
	done := make(chan struct{})
	runCtx, stop := context.WithCancel(t.Context())
	go func() { d.Run(runCtx); close(done) }()
	defer func() { stop(); <-done }()

	// vzdump writes a temporary file and renames it on success.
	waitFor(t, func() bool { _, ok, _ := f.st.Meta(t.Context(), "discovery.enabled:t1:backups"); return ok })
	final := filepath.Join(f.dump, "vzdump-qemu-100-2026_10_10-02_00_01.vma.zst")
	tmp := strings.TrimSuffix(final, ".vma.zst") + ".vma.dat"
	write(t, tmp, "data")
	future := f.now.Add(time.Hour)
	if err := os.Chtimes(tmp, future, future); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, final); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return f.states()["t1/"+filepath.Base(final)] == "queued" })

	// The hook reports archives too (and duplicates are harmless). A hard
	// link creates the archive without an event inotify watches for.
	other := filepath.Join(f.dump, "vzdump-lxc-200-2026_10_10-02_00_01.tar.zst")
	staged := filepath.Join(filepath.Dir(f.dump), "staged")
	write(t, staged, "ct")
	if err := os.Chtimes(staged, future, future); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(staged, other); err != nil {
		t.Fatal(err)
	}
	d.Notify(other)
	d.Notify(other)
	d.Notify("/elsewhere/vzdump-qemu-1-x")
	waitFor(t, func() bool { return f.states()["t1/"+filepath.Base(other)] == "queued" })
	if n := len(f.states()); n != 2 {
		t.Fatalf("jobs = %v", f.states())
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
