// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"
	"time"
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
	if err != nil || !ok || v != "1" || LatestSchemaVersion() != 1 {
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
	if _, err := os.Stat(path + ".pre-1"); !errors.Is(err, os.ErrNotExist) {
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
	backup, err := Open(t.Context(), path+".pre-2")
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
