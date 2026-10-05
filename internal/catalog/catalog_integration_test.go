// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package catalog_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/catalog"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/manifest"
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

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.PutStorage(t.Context(), &store.StorageRow{StoreID: "offsite", Remote: "r", BasePath: "pve-backups",
		Source: "homelab", ConfigHash: "x"}); err != nil {
		t.Fatal(err)
	}
	return st
}

// stable drops fields that legitimately differ between two resyncs.
func stable(bs []*store.Backup) []store.Backup {
	out := make([]store.Backup, 0, len(bs))
	for _, b := range bs {
		c := *b
		c.ID, c.VerifiedAt = 0, nil
		out = append(out, c)
	}
	return out
}

func TestResyncRebuildsCatalogue(t *testing.T) {
	ctx := t.Context()
	r, _ := repotest.Init(t)
	day := func(d int) time.Time { return time.Date(2026, 10, d, 2, 0, 1, 0, time.UTC) }

	good := repotest.Backup{VMID: 100, Time: day(1), Size: 130 << 10, GuestName: "web01", Notes: "nightly", Protected: true}
	repotest.Write(t, r, good)
	ct := repotest.Backup{VMID: 200, VMType: "lxc", Time: day(1), Size: 5}
	repotest.Write(t, r, ct)
	collided := repotest.Backup{VMID: 100, Time: day(1), Collision: 1, Size: 7}
	repotest.Write(t, r, collided)

	tomb := repotest.Backup{VMID: 100, Time: day(2), Size: 1 << 10}
	repotest.Write(t, r, tomb)
	meta, err := r.ReadMeta(ctx, 1, tomb.ID())
	if err != nil {
		t.Fatal(err)
	}
	meta.Notes = "changed after upload"
	meta.Tombstone = &manifest.Tombstone{RequestedAt: day(3), DeleteAfter: day(10), Reason: "user", State: "pending"}
	if err := r.WriteMeta(ctx, 1, tomb.ID(), meta); err != nil {
		t.Fatal(err)
	}

	damaged := repotest.Backup{VMID: 100, Time: day(3), Size: 130 << 10}
	repotest.Write(t, r, damaged)
	tgt, err := r.Target(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := tgt.Remove(ctx, damaged.ID().Path(layout.PartName(0))); err != nil {
		t.Fatal(err)
	}
	incomplete := repotest.Backup{VMID: 100, Time: day(4), Size: 1 << 10}
	repotest.Upload(t, r, incomplete)

	st := openStore(t)
	rep, err := catalog.Resync(ctx, st, "offsite", r, "homelab")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Complete != 3 || rep.Tombstoned != 1 || rep.Damaged != 1 || rep.Incomplete != 1 || len(rep.Pending) != 1 {
		t.Fatalf("report = %+v", rep)
	}
	list, err := st.ListBackups(ctx, "offsite", store.BackupFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 5 {
		t.Fatalf("catalogue has %d entries", len(list))
	}
	byVol := map[string]*store.Backup{}
	for _, b := range list {
		byVol[b.Volname] = b
	}
	g := byVol["backup/vzdump-qemu-100-2026_10_01-02_00_01.vma.zst"]
	if g == nil || g.State != "complete" || g.ArchiveSize != 130<<10 || g.SegmentCount != 3 || g.GuestName != "web01" ||
		g.Notes != "nightly" || !g.Protected || g.VerifyLevel != 2 || g.RemoteDir != "g1/"+good.ID().Dir() ||
		g.BackupTime != day(1).Unix() || g.TSLabel != "2026_10_01-02_00_01" {
		t.Fatalf("good entry = %+v", g)
	}
	if c := byVol["backup/vzdump-qemu-100-2026_10_01-02_00_01.1.vma.zst"]; c == nil || c.CollisionIndex != 1 {
		t.Fatalf("collision entry = %+v", c)
	}
	if c := byVol["backup/vzdump-lxc-200-2026_10_01-02_00_01.tar.zst"]; c == nil || c.VMType != "lxc" || c.ArchiveFormat != "tar" {
		t.Fatalf("container entry = %+v", c)
	}
	tb := byVol["backup/vzdump-qemu-100-2026_10_02-02_00_01.vma.zst"]
	if tb == nil || tb.State != "tombstoned" || tb.DeleteAfter == nil || *tb.DeleteAfter != day(10).Unix() || tb.Notes != "changed after upload" {
		t.Fatalf("tombstoned entry = %+v", tb)
	}
	if d := byVol["backup/vzdump-qemu-100-2026_10_03-02_00_01.vma.zst"]; d == nil || d.State != "damaged" || d.VerifyResult != "damaged" {
		t.Fatalf("damaged entry = %+v", d)
	}
	m, err := manifest.DecodeManifest([]byte(g.ManifestJSON))
	if err != nil || m.Archive.SHA256 != g.ArchiveSHA256 {
		t.Fatalf("stored manifest: %v", err)
	}

	// A fresh database rebuilt from a freshly opened repository holds the
	// same catalogue.
	reopened, err := repo.Open(ctx, r.Loc, repo.OpenOptions{Encryption: "crypt", Keys: repotest.Loader(r.Keys), UUID: r.UUID()})
	if err != nil {
		t.Fatal(err)
	}
	fresh := openStore(t)
	if _, err := catalog.Resync(ctx, fresh, "offsite", reopened, "homelab"); err != nil {
		t.Fatal(err)
	}
	again, err := fresh.ListBackups(ctx, "offsite", store.BackupFilter{})
	if err != nil {
		t.Fatal(err)
	}
	a, b := stable(list), stable(again)
	if len(a) != len(b) {
		t.Fatalf("rebuilt catalogue has %d entries, want %d", len(b), len(a))
	}
	for i := range a {
		if !reflect.DeepEqual(a[i], b[i]) {
			t.Errorf("entry %d differs:\n%+v\n%+v", i, a[i], b[i])
		}
	}

	// Resync drops entries that vanished remotely and keeps a higher local
	// verification level for unchanged archives.
	at := time.Now().Unix()
	g.VerifyLevel, g.VerifiedAt, g.VerifyResult = 3, &at, "ok"
	if err := st.PutBackup(ctx, g); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, 1, ct.ID(), "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Resync(ctx, st, "offsite", r, "homelab"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetBackup(ctx, "offsite", "backup/vzdump-lxc-200-2026_10_01-02_00_01.tar.zst"); err == nil {
		t.Fatal("deleted backup still catalogued")
	}
	if g2, err := st.GetBackup(ctx, "offsite", g.Volname); err != nil || g2.VerifyLevel != 3 {
		t.Fatalf("verification level after resync = %+v, %v", g2, err)
	}
}

// TestResyncKeepsMetadataWhenMetaIsUnusable: a meta document that cannot be
// read must not reset notes, protection and tombstone to their upload-time
// values; the catalogue keeps them, reports the problem and writes them
// back.
func TestResyncKeepsMetadataWhenMetaIsUnusable(t *testing.T) {
	ctx := t.Context()
	r, _ := repotest.Init(t)
	b := repotest.Backup{VMID: 100, Time: time.Date(2026, 10, 1, 2, 0, 1, 0, time.UTC), Size: 1 << 10, Notes: "at upload"}
	repotest.Write(t, r, b)
	meta, err := r.ReadMeta(ctx, 1, b.ID())
	if err != nil {
		t.Fatal(err)
	}
	meta.Notes, meta.Protected = "changed later", true
	meta.Tombstone = &manifest.Tombstone{RequestedAt: time.Now().UTC(), DeleteAfter: time.Now().Add(time.Hour).UTC(), Reason: "user", State: "pending"}
	if err := r.WriteMeta(ctx, 1, b.ID(), meta); err != nil {
		t.Fatal(err)
	}
	st := openStore(t)
	if _, err := catalog.Resync(ctx, st, "offsite", r, "homelab"); err != nil {
		t.Fatal(err)
	}

	tgt, err := r.Target(1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tgt.PutBytes(ctx, b.ID().Path(layout.MetaName), []byte("not a meta document")); err != nil {
		t.Fatal(err)
	}
	rep, err := catalog.Resync(ctx, st, "offsite", r, "homelab")
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Problems) != 1 || !strings.Contains(rep.Problems[0], "meta:") {
		t.Errorf("report problems = %q", rep.Problems)
	}
	got, err := st.ListBackups(ctx, "offsite", store.BackupFilter{})
	if err != nil || len(got) != 1 {
		t.Fatalf("catalogue = %v, %v", got, err)
	}
	if e := got[0]; e.Notes != "changed later" || !e.Protected || e.State != "tombstoned" || e.DeleteAfter == nil || e.MetaDirty {
		t.Fatalf("entry after an unusable meta document = %+v", e)
	}

	// Without a catalogue entry to fall back on, the protection is unknown:
	// the backup is treated as protected and nothing is written back.
	fresh := openStore(t)
	if _, err := catalog.Resync(ctx, fresh, "offsite", r, "homelab"); err != nil {
		t.Fatal(err)
	}
	got, err = fresh.ListBackups(ctx, "offsite", store.BackupFilter{})
	if err != nil || len(got) != 1 {
		t.Fatalf("catalogue = %v, %v", got, err)
	}
	if e := got[0]; !e.Protected || e.MetaDirty || e.State != "complete" {
		t.Fatalf("unknown metadata not treated as protected: %+v", e)
	}
}
