// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"golang.org/x/sys/unix"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenCreatesLatestSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.db")
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	v, ok, err := s.Meta(t.Context(), "schema_version")
	if err != nil || !ok || v != strconv.Itoa(LatestSchemaVersion()) || LatestSchemaVersion() < 3 {
		t.Fatalf("schema_version = %q %v %v", v, ok, err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("database mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(path)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode %v", fi.Mode().Perm())
	}
	_ = s.Close()
	s, err = Open(t.Context(), path) // reopening is a no-op migration
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if m, _ := filepath.Glob(path + ".schema-*"); len(m) != 0 {
		t.Fatal("no-op open wrote a pre-migration copy")
	}
}

func TestMigrationCopiesDatabaseFirst(t *testing.T) {
	orig := migrationFS
	t.Cleanup(func() { migrationFS = orig })
	m1, _ := embeddedMigrations.ReadFile("migrations/0001_init.sql")

	migrationFS = fstest.MapFS{"migrations/0001_init.sql": {Data: m1}}
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMeta(t.Context(), "marker", "before"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	migrationFS = fstest.MapFS{
		"migrations/0001_init.sql":  {Data: m1},
		"migrations/0002_extra.sql": {Data: []byte("CREATE TABLE extra (x INTEGER);")},
	}
	s, err = Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if v, _, _ := s.Meta(t.Context(), "schema_version"); v != "2" {
		t.Fatalf("schema_version = %s", v)
	}
	migrationFS = fstest.MapFS{"migrations/0001_init.sql": {Data: m1}}
	backup, err := Open(t.Context(), path+".schema-1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = backup.Close() }()
	if v, _, _ := backup.Meta(t.Context(), "marker"); v != "before" {
		t.Fatalf("pre-migration copy missing data: %q", v)
	}
}

func TestMigrationNumberingIsChecked(t *testing.T) {
	orig := migrationFS
	t.Cleanup(func() { migrationFS = orig })
	migrationFS = fstest.MapFS{"migrations/0002_gap.sql": {Data: []byte("SELECT 1;")}}
	if _, err := migrations(); err == nil {
		t.Fatal("gap in migration numbering accepted")
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	s := openTest(t)
	if err := s.SetMeta(t.Context(), "schema_version", "99"); err != nil {
		t.Fatal(err)
	}
	path := s.Path()
	_ = s.Close()
	if _, err := Open(t.Context(), path); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("newer schema: %v", err)
	}
}

func TestCorruptDatabaseDetectedAndMovedAside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(path, []byte("this is not an sqlite database, not even close............"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(t.Context(), path); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt database: %v", err)
	}
	moved, err := MoveAside(path, "corrupt", time.Unix(1790000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if moved != path+".corrupt-1790000000" {
		t.Fatalf("moved to %s", moved)
	}
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatalf("fresh database after moving aside: %v", err)
	}
	_ = s.Close()
}

func newJob(key string) *Job {
	return &Job{Kind: "replicate", StoreID: "offsite", State: "queued", DedupeKey: key, OwnerNode: "pve1"}
}

func TestJobQueue(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	clock := time.Unix(1790000000, 0)
	s.now = func() time.Time { return clock }

	id1, created, err := s.InsertJob(ctx, newJob("a"))
	if err != nil || !created {
		t.Fatal(err)
	}
	if dup, created, _ := s.InsertJob(ctx, newJob("a")); created || dup != id1 {
		t.Fatal("duplicate dedupe key created a second job")
	}
	low := newJob("b")
	low.Priority = -1
	idLow, _, _ := s.InsertJob(ctx, low)
	later := newJob("c")
	due := clock.Unix() + 60
	later.NextAttemptAt = &due
	later.Priority = 10
	idLater, _, _ := s.InsertJob(ctx, later)

	got, err := s.ClaimRunnable(ctx, "replicate", []string{"queued"}, "preparing", "pve1", 600)
	if err != nil || got.ID != id1 || got.State != "preparing" || got.LeaseUntil == nil || got.StartedAt == nil {
		t.Fatalf("first claim = %+v, %v", got, err)
	}
	got, _ = s.ClaimRunnable(ctx, "replicate", []string{"queued"}, "preparing", "pve1", 600)
	if got.ID != idLow {
		t.Fatalf("expected low-priority job while the other is not due, got %d", got.ID)
	}
	if _, err := s.ClaimRunnable(ctx, "replicate", []string{"queued"}, "preparing", "pve1", 600); !errors.Is(err, ErrNotFound) {
		t.Fatalf("job claimed before it was due: %v", err)
	}
	clock = clock.Add(2 * time.Minute)
	if got, _ = s.ClaimRunnable(ctx, "replicate", []string{"queued"}, "preparing", "pve1", 600); got.ID != idLater {
		t.Fatalf("due job not claimed: %+v", got)
	}

	// Optimistic state checks.
	if _, err := s.UpdateJob(ctx, id1, []string{"uploading"}, "x", func(*Job) error { return nil }); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("state conflict not detected: %v", err)
	}
	j, err := s.UpdateJob(ctx, id1, []string{"preparing"}, "upload started", func(j *Job) error {
		j.State = "uploading"
		j.HashState = []byte{1, 2, 3}
		j.NextSegment = 4
		return nil
	})
	if err != nil || j.State != "uploading" {
		t.Fatal(err)
	}
	if again, _ := s.GetJob(ctx, id1); string(again.HashState) != "\x01\x02\x03" || again.NextSegment != 4 {
		t.Fatalf("job fields not persisted: %+v", again)
	}
	events, _ := s.JobEvents(ctx, id1)
	if len(events) != 3 || events[2].FromState != "preparing" || events[2].ToState != "uploading" || events[2].Message != "upload started" {
		t.Fatalf("events = %+v", events)
	}

	// Crash recovery.
	n, err := s.RequeueInterrupted(ctx, []string{"preparing", "uploading"}, "queued")
	if err != nil || n != 3 {
		t.Fatalf("requeued %d, %v", n, err)
	}
	if j, _ := s.GetJob(ctx, id1); j.State != "queued" || j.LeaseUntil != nil {
		t.Fatalf("job not requeued: %+v", j)
	}
	if list, _ := s.ListJobs(ctx, JobFilter{States: []string{"queued"}}); len(list) != 3 {
		t.Fatalf("queued jobs = %d", len(list))
	}

	// Segments.
	ss := int64(1074004000)
	for i := range 3 {
		if err := s.PutSegment(ctx, Segment{JobID: id1, Index: i, Offset: int64(i) << 30, Size: 1 << 30, State: "pending"}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.PutSegment(ctx, Segment{JobID: id1, Index: 1, Offset: 1 << 30, Size: 1 << 30, SHA256: "ab", StoredSize: &ss,
		StoredHashType: "quickxor", StoredHash: "qx", State: "uploaded"})
	segs, _ := s.Segments(ctx, id1)
	if len(segs) != 3 || segs[1].State != "uploaded" || *segs[1].StoredSize != ss || segs[1].StoredHash != "qx" {
		t.Fatalf("segments = %+v", segs)
	}
}

func TestReplicateJobsSupersede(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	job := func(day int, state string) *Job {
		return &Job{Kind: "replicate", StoreID: "offsite", State: state, OwnerNode: "pve1",
			DedupeKey: fmt.Sprintf("replicate:offsite:local:%d", day), VMType: "qemu", VMID: 100,
			BackupTime: int64(day) * 86400}
	}
	id1, _, sup, err := s.InsertReplicateJob(ctx, job(1, "queued"), true)
	if err != nil || len(sup) != 0 {
		t.Fatal(sup, err)
	}
	// A job that already uploaded a segment is kept.
	id2, _, _, _ := s.InsertReplicateJob(ctx, job(2, "queued"), false)
	if _, err := s.UpdateJob(ctx, id2, []string{"queued"}, "started", func(j *Job) error {
		j.State, j.NextSegment = "retry_wait", 1
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	other := job(2, "queued")
	other.VMID, other.DedupeKey = 101, "replicate:offsite:local:other"
	idOther, _, _, _ := s.InsertReplicateJob(ctx, other, true)

	id3, created, sup, err := s.InsertReplicateJob(ctx, job(3, "queued"), true)
	if err != nil || !created || len(sup) != 1 || sup[0] != id1 {
		t.Fatalf("superseded %v, %v", sup, err)
	}
	for id, want := range map[int64]string{id1: "superseded", id2: "retry_wait", idOther: "queued", id3: "queued"} {
		if j, _ := s.GetJob(ctx, id); j.State != want {
			t.Errorf("job %d is %s, want %s", id, j.State, want)
		}
	}
	if j, _ := s.GetJob(ctx, id1); j.FinishedAt == nil || j.VMType != "qemu" || j.VMID != 100 || j.BackupTime != 86400 {
		t.Errorf("superseded job = %+v", j)
	}
	// An older archive discovered later is superseded on arrival, and a
	// repeated insert changes nothing.
	idOld, created, _, _ := s.InsertReplicateJob(ctx, job(0, "queued"), true)
	if j, _ := s.GetJob(ctx, idOld); !created || j.State != "superseded" {
		t.Fatalf("late older job = %+v", j)
	}
	if again, created, sup, _ := s.InsertReplicateJob(ctx, job(3, "queued"), true); created || again != id3 || len(sup) != 0 {
		t.Fatal("repeated insert created or superseded jobs")
	}
	newest, err := s.GuestReplication(ctx, "offsite", "qemu", 100, []string{"queued", "retry_wait", "complete"})
	if err != nil || newest != 3*86400 {
		t.Fatalf("newest = %d, %v", newest, err)
	}
	if n, _ := s.GuestReplication(ctx, "offsite", "lxc", 100, []string{"queued"}); n != 0 {
		t.Fatalf("other guest type newest = %d", n)
	}
}

func TestConcurrentClaimsNeverShareAJob(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const jobs = 40
	for i := range jobs {
		if _, _, err := s.InsertJob(ctx, newJob(string(rune('A'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[int64]bool{}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for {
				j, err := s.ClaimRunnable(ctx, "replicate", []string{"queued"}, "preparing", "w", 60)
				if errors.Is(err, ErrNotFound) {
					return
				}
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				if seen[j.ID] {
					t.Errorf("job %d claimed twice", j.ID)
				}
				seen[j.ID] = true
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(seen) != jobs {
		t.Fatalf("claimed %d of %d jobs", len(seen), jobs)
	}
}

func testBackup(vol string, vmid int, sha string) *Backup {
	return &Backup{StoreID: "offsite", Volname: vol, VMType: "qemu", VMID: vmid, BackupTime: 1790992801,
		TSLabel: "2026_10_04-02_00_01", RemoteDir: "v1/homelab/qemu/" + vol, Generation: 1, State: "complete",
		ArchiveSize: 10, ArchiveSHA256: sha, ArchiveFormat: "vma", Compression: "zst", SegmentSize: 1 << 30,
		SegmentCount: 1, UploadedAt: 1790999999, ManifestJSON: "{}"}
}

func TestCatalog(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	if err := s.PutStorage(ctx, &StorageRow{StoreID: "offsite", Remote: "od", BasePath: "pve-backups", Source: "homelab", ConfigHash: "h1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBackup(ctx, testBackup("b/1", 100, "s1")); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBackup(ctx, testBackup("b/2", 200, "s2")); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListBackups(ctx, "offsite", BackupFilter{VMID: 100}); len(list) != 1 || list[0].Volname != "b/1" {
		t.Fatalf("filtered list = %+v", list)
	}

	// Local verification state survives a resync when the archive is unchanged.
	b1, _ := s.GetBackup(ctx, "offsite", "b/1")
	at := int64(1791000000)
	b1.VerifyLevel, b1.VerifiedAt, b1.VerifyResult, b1.Notes = 3, &at, "ok", "old notes"
	_ = s.PutBackup(ctx, b1)
	fresh := testBackup("b/1", 100, "s1")
	fresh.VerifyLevel, fresh.Notes = 2, "new notes"
	changed := testBackup("b/2", 200, "s2-changed")
	if err := s.ReplaceCatalog(ctx, "offsite", []*Backup{fresh, changed, testBackup("b/3", 300, "s3")}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetBackup(ctx, "offsite", "b/1")
	if got.VerifyLevel != 3 || got.VerifyResult != "ok" || got.Notes != "new notes" {
		t.Fatalf("after resync b/1 = %+v", got)
	}
	if list, _ := s.ListBackups(ctx, "offsite", BackupFilter{}); len(list) != 3 {
		t.Fatalf("catalogue size %d", len(list))
	}
	bad := testBackup("x", 1, "s")
	bad.StoreID = "other"
	if err := s.ReplaceCatalog(ctx, "offsite", []*Backup{bad}); err == nil {
		t.Fatal("foreign entry accepted")
	}
	if list, _ := s.ListBackups(ctx, "offsite", BackupFilter{}); len(list) != 3 {
		t.Fatal("failed resync was not rolled back")
	}

	if err := s.DeleteStorage(ctx, "offsite"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListBackups(ctx, "offsite", BackupFilter{}); len(list) != 0 {
		t.Fatal("catalogue not removed with its storage")
	}
	if _, err := s.GetStorage(ctx, "offsite"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestRepositories(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	const a, b = "6f0c2f1e-3a7b-4c2d-9e8f-0123456789ab", "7a1d3e2f-4b8c-4d3e-8f90-123456789abc"
	if _, err := s.FindRepository(ctx, "od", "pve-backups"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.PutRepository(ctx, &Repository{UUID: a, Remote: "od", BasePath: "pve-backups", Encryption: "crypt"}); err != nil {
		t.Fatal(err)
	}
	if r, err := s.FindRepository(ctx, "od", "pve-backups"); err != nil || r.UUID != a || r.Encryption != "crypt" {
		t.Fatalf("repository = %+v, %v", r, err)
	}
	// A new repository at the same location replaces the old record.
	if err := s.PutRepository(ctx, &Repository{UUID: b, Remote: "od", BasePath: "pve-backups", Encryption: "none"}); err != nil {
		t.Fatal(err)
	}
	if r, err := s.FindRepository(ctx, "od", "pve-backups"); err != nil || r.UUID != b {
		t.Fatalf("repository = %+v, %v", r, err)
	}
	if err := s.PutRepository(ctx, &Repository{UUID: b, Remote: "od", BasePath: "elsewhere", Encryption: "none"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FindRepository(ctx, "od", "pve-backups"); !errors.Is(err, ErrNotFound) {
		t.Fatal("moved repository still found at its old location")
	}
	if err := s.PutRepository(ctx, &Repository{UUID: a, Remote: "od", BasePath: "x", Encryption: "bogus"}); err == nil {
		t.Fatal("invalid encryption mode accepted")
	}
}

func TestMiscTables(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	clock := time.Unix(1790000000, 0)
	s.now = func() time.Time { return clock }

	id, err := s.AddVerification(ctx, &Verification{StoreID: "offsite", Volname: "b/1", Level: 2, StartedAt: clock.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.FinishVerification(ctx, id, "ok", `{"segments":3}`)
	if v, _ := s.Verifications(ctx, "offsite", "b/1", 10); len(v) != 1 || v[0].Result != "ok" || v[0].FinishedAt == nil {
		t.Fatalf("verifications = %+v", v)
	}

	_ = s.PutIdempotentResponse(ctx, "k1", `{"a":1}`, 3600)
	if r, ok, _ := s.IdempotentResponse(ctx, "k1", 3600); !ok || r != `{"a":1}` {
		t.Fatal("idempotent response not found")
	}
	clock = clock.Add(2 * time.Hour)
	if _, ok, _ := s.IdempotentResponse(ctx, "k1", 3600); ok {
		t.Fatal("expired idempotent response returned")
	}

	dev, ino := int64(1), int64(2)
	_ = s.AddFetchedArchive(ctx, FetchedArchive{Path: "/var/lib/vz/dump/x.vma.zst", Dev: &dev, Ino: &ino, StoreID: "offsite", Volname: "b/1"})
	if f, err := s.FetchedArchive(ctx, "/var/lib/vz/dump/x.vma.zst"); err != nil || *f.Ino != 2 || f.Volname != "b/1" {
		t.Fatalf("fetched archive = %+v %v", f, err)
	}
	if _, err := s.FetchedArchive(ctx, "/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}

	_ = s.RaiseAlert(ctx, Alert{ID: "auth:od", Severity: "error", Message: "reconnect remote od"})
	firstRaised := clock.Unix()
	clock = clock.Add(time.Minute)
	_ = s.RaiseAlert(ctx, Alert{ID: "auth:od", Severity: "error", Message: "still failing"})
	alerts, _ := s.ActiveAlerts(ctx)
	if len(alerts) != 1 || alerts[0].Message != "still failing" || alerts[0].RaisedAt != firstRaised {
		t.Fatalf("alerts = %+v", alerts)
	}
	_ = s.ClearAlert(ctx, "auth:od")
	if alerts, _ := s.ActiveAlerts(ctx); len(alerts) != 0 {
		t.Fatal("cleared alert still active")
	}
	clock = clock.Add(time.Minute)
	_ = s.RaiseAlert(ctx, Alert{ID: "auth:od", Severity: "error", Message: "again"})
	if alerts, _ := s.ActiveAlerts(ctx); len(alerts) != 1 || alerts[0].RaisedAt != clock.Unix() {
		t.Fatalf("reopened alert = %+v", alerts)
	}
}

func TestDamageOverridesEarlierVerification(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	_ = s.PutStorage(ctx, &StorageRow{StoreID: "offsite", Remote: "od", BasePath: "p", Source: "lab", ConfigHash: "h"})
	b := testBackup("b/1", 100, "s1")
	at := int64(1)
	b.VerifyLevel, b.VerifiedAt, b.VerifyResult = 3, &at, "ok"
	_ = s.PutBackup(ctx, b)
	damaged := testBackup("b/1", 100, "s1")
	damaged.State, damaged.VerifyResult = "damaged", "damaged"
	if err := s.ReplaceCatalog(ctx, "offsite", []*Backup{damaged}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetBackup(ctx, "offsite", "b/1"); got.VerifyLevel != 0 || got.VerifyResult != "damaged" {
		t.Fatalf("damaged entry kept its old verification: %+v", got)
	}
}

func TestPendingMetaChanges(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	if err := s.PutStorage(ctx, &StorageRow{StoreID: "offsite", Remote: "od", BasePath: "p", Source: "lab", ConfigHash: "h"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutBackup(ctx, testBackup("b/1", 100, "s1")); err != nil {
		t.Fatal(err)
	}
	b, err := s.UpdateBackup(ctx, "offsite", "b/1", func(b *Backup) error {
		at, after := int64(10), int64(20)
		b.Notes, b.Protected, b.State, b.TombstoneAt, b.DeleteAfter, b.TombstoneReason = "keep", true, "tombstoned", &at, &after, "user"
		return nil
	})
	if err != nil || !b.MetaDirty || b.MetaRev != 1 {
		t.Fatalf("update = %+v, %v", b, err)
	}
	if _, err := s.UpdateBackup(ctx, "offsite", "missing", func(*Backup) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of a missing entry: %v", err)
	}
	dirty, err := s.DirtyBackups(ctx, 10)
	if err != nil || len(dirty) != 1 || dirty[0].TombstoneReason != "user" || *dirty[0].TombstoneAt != 10 {
		t.Fatalf("dirty = %+v, %v", dirty, err)
	}

	// A resync keeps changes that were not pushed yet.
	if err := s.ReplaceCatalog(ctx, "offsite", []*Backup{testBackup("b/1", 100, "s1")}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetBackup(ctx, "offsite", "b/1")
	if got.Notes != "keep" || !got.Protected || got.State != "tombstoned" || !got.MetaDirty || got.MetaRev != 1 || got.DeleteAfter == nil {
		t.Fatalf("after resync = %+v", got)
	}

	// A push of an older revision does not clear a newer change.
	if _, err := s.UpdateBackup(ctx, "offsite", "b/1", func(b *Backup) error { b.Notes = "newer"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.MetaPushed(ctx, "offsite", "b/1", 1); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetBackup(ctx, "offsite", "b/1"); !got.MetaDirty {
		t.Fatal("stale push cleared the dirty flag")
	}
	if err := s.MetaPushed(ctx, "offsite", "b/1", 2); err != nil {
		t.Fatal(err)
	}
	if dirty, _ := s.DirtyBackups(ctx, 10); len(dirty) != 0 {
		t.Fatalf("dirty after push = %+v", dirty)
	}
	// Once pushed, the remote state is authoritative again.
	if err := s.ReplaceCatalog(ctx, "offsite", []*Backup{testBackup("b/1", 100, "s1")}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetBackup(ctx, "offsite", "b/1"); got.Notes == "newer" || got.State != "complete" {
		t.Fatalf("after pushed resync = %+v", got)
	}
}

func TestVerificationBookkeeping(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	clock := time.Unix(1790000000, 0)
	s.now = func() time.Time { return clock }
	_ = s.PutStorage(ctx, &StorageRow{StoreID: "offsite", Remote: "od", BasePath: "p", Source: "lab", ConfigHash: "h"})
	for i, vol := range []string{"b/1", "b/2", "b/3"} {
		b := testBackup(vol, 100+i, fmt.Sprintf("s%d", i))
		b.ArchiveSize = int64(10 * (i + 1))
		_ = s.PutBackup(ctx, b)
	}
	if err := s.SetVerification(ctx, "offsite", "b/1", 3, "ok", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVerification(ctx, "offsite", "missing", 3, "ok", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing entry: %v", err)
	}
	cands, err := s.VerifyCandidates(ctx, "offsite", clock.Add(-time.Hour).Unix(), 10)
	if err != nil || len(cands) != 2 {
		t.Fatalf("candidates = %+v, %v", cands, err)
	}
	// After the interval, verified backups are due again (last).
	cands, _ = s.VerifyCandidates(ctx, "offsite", clock.Add(time.Hour).Unix(), 10)
	if len(cands) != 3 || cands[2].Volname != "b/1" {
		t.Fatalf("candidates after interval = %v", cands)
	}
	if err := s.SetVerification(ctx, "offsite", "b/2", 3, "damaged", true); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.GetBackup(ctx, "offsite", "b/2"); b.State != "damaged" || b.VerifyLevel != 0 {
		t.Fatalf("damaged entry = %+v", b)
	}
	_, _ = s.AddVerification(ctx, &Verification{StoreID: "offsite", Volname: "b/1", Level: 3, StartedAt: clock.Unix()})
	_, _ = s.AddVerification(ctx, &Verification{StoreID: "offsite", Volname: "b/3", Level: 3, StartedAt: clock.Unix() - 100000})
	_, _ = s.AddVerification(ctx, &Verification{StoreID: "offsite", Volname: "b/3", Level: 2, StartedAt: clock.Unix()})
	if n, err := s.ContentVerifiedBytes(ctx, "offsite", clock.Add(-24*time.Hour).Unix()); err != nil || n != 10 {
		t.Fatalf("verified bytes = %d, %v", n, err)
	}
}

// TestCancelledOpenIsNotCorruption: an interrupted start-up must not be
// mistaken for a corrupt database, which the daemon moves aside.
func TestCancelledOpenIsNotCorruption(t *testing.T) {
	s := openTest(t)
	path := s.Path()
	_ = s.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Open(ctx, path)
	if err == nil || errors.Is(err, ErrCorrupt) || !errors.Is(err, context.Canceled) {
		t.Fatalf("open with a cancelled context = %v, want context.Canceled and not ErrCorrupt", err)
	}
}

// TestDatabaseFilesArePrivate: the database, its WAL files and migration
// copies are 0600 whatever the umask, also for a file created by an
// earlier version with wider permissions.
func TestDatabaseFilesArePrivate(t *testing.T) {
	old := unix.Umask(0o022)
	defer unix.Umask(old)
	orig := migrationFS
	t.Cleanup(func() { migrationFS = orig })
	m1, _ := embeddedMigrations.ReadFile("migrations/0001_init.sql")
	migrationFS = fstest.MapFS{"migrations/0001_init.sql": {Data: m1}}

	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	migrationFS = fstest.MapFS{
		"migrations/0001_init.sql":  {Data: m1},
		"migrations/0002_extra.sql": {Data: []byte("CREATE TABLE extra (x INTEGER);")},
	}
	if s, err = Open(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.SetMeta(t.Context(), "k", "v"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal", path + "-shm", path + ".schema-1"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v, want 0600", filepath.Base(p), fi.Mode().Perm())
		}
	}
}

// TestInterruptedUpgradeKeepsOriginalCopy: when an upgrade over several
// versions fails halfway, retrying from the intermediate version keeps the
// copy of the original database.
func TestInterruptedUpgradeKeepsOriginalCopy(t *testing.T) {
	orig := migrationFS
	t.Cleanup(func() { migrationFS = orig })
	m1, _ := embeddedMigrations.ReadFile("migrations/0001_init.sql")
	migrationFS = fstest.MapFS{"migrations/0001_init.sql": {Data: m1}}
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMeta(t.Context(), "marker", "original"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	upgrade := fstest.MapFS{
		"migrations/0001_init.sql":   {Data: m1},
		"migrations/0002_two.sql":    {Data: []byte("CREATE TABLE two (x INTEGER);")},
		"migrations/0003_broken.sql": {Data: []byte("this is not SQL;")},
	}
	migrationFS = upgrade
	if _, err := Open(t.Context(), path); err == nil {
		t.Fatal("broken migration applied")
	}
	upgrade["migrations/0003_broken.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE three (x INTEGER);")}
	if s, err = Open(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	migrationFS = fstest.MapFS{"migrations/0001_init.sql": {Data: m1}}
	first, err := Open(t.Context(), path+".schema-1")
	if err != nil {
		t.Fatalf("copy of the original database: %v", err)
	}
	defer func() { _ = first.Close() }()
	if v, _, _ := first.Meta(t.Context(), "schema_version"); v != "1" {
		t.Errorf("original copy has schema version %s", v)
	}
	if v, _, _ := first.Meta(t.Context(), "marker"); v != "original" {
		t.Errorf("original copy lost its data: %q", v)
	}
	if _, err := os.Stat(path + ".schema-2"); err != nil {
		t.Errorf("copy before the retry: %v", err)
	}
}

// TestPathWithURICharacters: a path is a file name, not a URI.
func TestPathWithURICharacters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state?x=1#frag%41.db")
	s, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMeta(t.Context(), "k", "v"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Contains(names, "state?x=1#frag%41.db") || slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(n, "state") && !strings.HasPrefix(n, "state?x=1#frag%41.db") }) {
		t.Fatalf("files = %q", names)
	}
}

// TestEqualLevelVerificationKept: an incoming entry at the same level
// replaces the local verification only if it is newer.
func TestEqualLevelVerificationKept(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	_ = s.PutStorage(ctx, &StorageRow{StoreID: "offsite", Remote: "od", BasePath: "p", Source: "lab", ConfigHash: "h"})
	b := testBackup("b/1", 100, "s1")
	at := int64(100)
	b.VerifyLevel, b.VerifiedAt, b.VerifyResult = 2, &at, "missing part.000003"
	_ = s.PutBackup(ctx, b)

	undated := testBackup("b/1", 100, "s1")
	undated.VerifyLevel = 2
	if err := s.ReplaceCatalog(ctx, "offsite", []*Backup{undated}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetBackup(ctx, "offsite", "b/1"); got.VerifyResult != "missing part.000003" || got.VerifiedAt == nil || *got.VerifiedAt != 100 {
		t.Fatalf("local result lost to an undated entry: %+v", got)
	}

	later := int64(200)
	fresh := testBackup("b/1", 100, "s1")
	fresh.VerifyLevel, fresh.VerifiedAt, fresh.VerifyResult = 2, &later, "ok"
	if err := s.ReplaceCatalog(ctx, "offsite", []*Backup{fresh}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetBackup(ctx, "offsite", "b/1"); got.VerifyResult != "ok" || *got.VerifiedAt != 200 {
		t.Fatalf("newer verification not taken: %+v", got)
	}
}
