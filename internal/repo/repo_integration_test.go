// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package repo_test

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/manifest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo/repotest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/secrets"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
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

func TestInitAndOpen(t *testing.T) {
	loc, dir := repotest.Remote(t, nil)
	if _, err := repo.Open(t.Context(), loc, repotest.Loader()); !errors.Is(err, repo.ErrNotInitialized) {
		t.Fatalf("open before init: %v", err)
	}
	keys := repotest.NewKeys(t)
	r, err := repo.Init(t.Context(), loc, repo.InitOptions{Keys: keys})
	if err != nil {
		t.Fatal(err)
	}
	if r.UUID() != keys.RepoUUID || r.ActiveGeneration() != 1 || r.Marker.Encryption != "crypt" {
		t.Fatalf("marker = %+v", r.Marker)
	}
	if _, err := repo.Init(t.Context(), loc, repo.InitOptions{Keys: repotest.NewKeys(t)}); !errors.Is(err, repo.ErrAlreadyInitialized) {
		t.Fatalf("second init: %v", err)
	}

	opened, err := repo.Open(t.Context(), loc, repotest.Loader(keys))
	if err != nil {
		t.Fatal(err)
	}
	if opened.UUID() != r.UUID() {
		t.Fatalf("opened %s, created %s", opened.UUID(), r.UUID())
	}

	// Same repository UUID, different secrets.
	other, err := secrets.NewRepoKeys(keys.RepoUUID, "base32768", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Open(t.Context(), loc, repotest.Loader(other)); !errors.Is(err, repo.ErrWrongKeys) {
		t.Fatalf("open with other keys: %v", err)
	}
	if _, err := repo.Open(t.Context(), loc, repotest.Loader()); err == nil {
		t.Fatal("open without keys succeeded")
	}

	// Only the marker is stored in plaintext; everything below g1 is
	// encrypted and nothing reveals the source or guest.
	if _, err := os.Stat(filepath.Join(dir, "pve-backups", layout.MarkerName)); err != nil {
		t.Fatal("marker not stored in plaintext")
	}
	repotest.Write(t, opened, repotest.Backup{Size: 1000})
	err = filepath.WalkDir(filepath.Join(dir, "pve-backups", "g1"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		for _, leak := range []string{"homelab", "qemu", "manifest", "part.", "keycheck", "v1"} {
			if strings.Contains(d.Name(), leak) {
				t.Errorf("plaintext name on remote: %s", p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOpenDetectsWrongKeysWithoutKeyCheck(t *testing.T) {
	loc, _ := repotest.Remote(t, nil)
	keys := repotest.NewKeys(t)
	if _, err := repo.Init(t.Context(), loc, repo.InitOptions{Keys: keys}); err != nil {
		t.Fatal(err)
	}
	// Drop the encrypted-name check from the marker: the sentinel object
	// must still catch wrong keys.
	marker, base, err := repo.ReadMarker(t.Context(), loc)
	if err != nil {
		t.Fatal(err)
	}
	marker.Generations[0].KeyCheck = nil
	data, err := manifest.Encode(marker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.PutBytes(t.Context(), layout.MarkerName, data); err != nil {
		t.Fatal(err)
	}
	other, err := secrets.NewRepoKeys(keys.RepoUUID, "base32768", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Open(t.Context(), loc, repotest.Loader(other)); !errors.Is(err, repo.ErrWrongKeys) {
		t.Fatalf("open with other keys: %v", err)
	}
	if _, err := repo.Open(t.Context(), loc, repotest.Loader(keys)); err != nil {
		t.Fatal(err)
	}
}

func TestUnencryptedRepository(t *testing.T) {
	loc, dir := repotest.Remote(t, nil)
	r, err := repo.Init(t.Context(), loc, repo.InitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Marker.Encryption != "none" || r.Keys != nil {
		t.Fatalf("marker = %+v", r.Marker)
	}
	b := repotest.Backup{Size: 70 << 10}
	repotest.Write(t, r, b)
	opened, err := repo.Open(t.Context(), loc, func(string) (*secrets.RepoKeys, error) {
		t.Fatal("keys requested for an unencrypted repository")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Stock tools can reassemble the archive from the plain segments.
	var got []byte
	for i := range 2 {
		part, err := os.ReadFile(filepath.Join(dir, "pve-backups", "g1", b.ID().Path(layout.PartName(i))))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, part...)
	}
	if string(got) != string(b.Data()) {
		t.Fatal("concatenated segments differ from the archive")
	}
	scanned, err := opened.Scan(t.Context(), "homelab")
	if err != nil || len(scanned) != 1 || scanned[0].State != repo.StateComplete {
		t.Fatalf("scan = %+v, %v", scanned, err)
	}
}

func TestRegisterSource(t *testing.T) {
	r, _ := repotest.Init(t)
	const a, b = "6f0c2f1e-3a7b-4c2d-9e8f-0123456789ab", "7a1d3e2f-4b8c-4d3e-8f90-123456789abc"
	if err := r.RegisterSource(t.Context(), "homelab", a, "cluster1", false); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterSource(t.Context(), "homelab", a, "cluster1", false); err != nil {
		t.Fatalf("re-register by owner: %v", err)
	}
	if err := r.RegisterSource(t.Context(), "homelab", b, "", false); !errors.Is(err, repo.ErrSourceTaken) {
		t.Fatalf("register by another installation: %v", err)
	}
	if err := r.RegisterSource(t.Context(), "homelab", b, "", true); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if err := r.RegisterSource(t.Context(), "homelab", a, "", false); !errors.Is(err, repo.ErrSourceTaken) {
		t.Fatalf("old owner after adoption: %v", err)
	}
	if err := r.RegisterSource(t.Context(), "Bad Name", a, "", false); err == nil {
		t.Fatal("invalid source name accepted")
	}
	if err := r.RegisterSource(t.Context(), "office", a, "", false); err != nil {
		t.Fatal(err)
	}
	sources, err := r.Sources(t.Context())
	if err != nil || !slices.Equal(sources, []string{"homelab", "office"}) {
		t.Fatalf("sources = %v, %v", sources, err)
	}
}

func TestCommitRequiresAllSegments(t *testing.T) {
	r, _ := repotest.Init(t)
	b := repotest.Backup{Size: 200 << 10}
	m := repotest.Upload(t, r, b)
	tgt, err := r.Target(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := tgt.Remove(t.Context(), b.ID().Path(layout.PartName(2))); err != nil {
		t.Fatal(err)
	}
	if err := r.Commit(t.Context(), m, nil); !errors.Is(err, transport.ErrIntegrity) {
		t.Fatalf("commit with a missing segment: %v", err)
	}
	if _, err := tgt.Stat(t.Context(), b.ID().Path(layout.ManifestName)); transport.Classify(err) != transport.ClassNotFound {
		t.Fatalf("manifest written despite failed commit: %v", err)
	}
	// A manifest of another repository is refused.
	m.RepoUUID = repo.NewUUID()
	if err := r.Commit(t.Context(), m, nil); err == nil {
		t.Fatal("foreign manifest committed")
	}
}

func TestScanClassifiesBackups(t *testing.T) {
	r, _ := repotest.Init(t)
	ctx := t.Context()
	tgt, err := r.Target(1)
	if err != nil {
		t.Fatal(err)
	}
	day := func(d int) time.Time { return time.Date(2026, 10, d, 2, 0, 1, 0, time.UTC) }

	complete := repotest.Backup{VMID: 100, Time: day(1), Size: 150 << 10, Log: "INFO: done\n", Notes: "web"}
	repotest.Write(t, r, complete)

	empty := repotest.Backup{VMID: 101, VMType: "lxc", Time: day(1), Size: 0}
	repotest.Write(t, r, empty)

	tombstoned := repotest.Backup{VMID: 100, Time: day(2), Size: 10 << 10}
	repotest.Write(t, r, tombstoned)
	meta, err := r.ReadMeta(ctx, 1, tombstoned.ID())
	if err != nil {
		t.Fatal(err)
	}
	meta.Tombstone = &manifest.Tombstone{RequestedAt: day(3), DeleteAfter: day(10), Reason: "user", State: "pending"}
	if err := r.WriteMeta(ctx, 1, tombstoned.ID(), meta); err != nil {
		t.Fatal(err)
	}

	damaged := repotest.Backup{VMID: 100, Time: day(3), Size: 150 << 10}
	repotest.Write(t, r, damaged)
	if err := tgt.Remove(ctx, damaged.ID().Path(layout.PartName(1))); err != nil {
		t.Fatal(err)
	}

	incomplete := repotest.Backup{VMID: 100, Time: day(4), Size: 150 << 10}
	repotest.Upload(t, r, incomplete)

	deleting := repotest.Backup{VMID: 102, Time: day(1), Size: 1 << 10}
	repotest.Write(t, r, deleting)
	meta = manifest.NewMeta(day(5))
	meta.Tombstone = &manifest.Tombstone{RequestedAt: day(5), DeleteAfter: day(5), Reason: "retention", State: "deleting"}
	if err := r.WriteMeta(ctx, 1, deleting.ID(), meta); err != nil {
		t.Fatal(err)
	}

	invalid := repotest.Backup{VMID: 103, Time: day(1), Size: 1 << 10}
	repotest.Upload(t, r, invalid)
	if _, err := tgt.PutBytes(ctx, invalid.ID().Path(layout.ManifestName), []byte(`{"format":"pve-rclone-backup.manifest","version":1}`)); err != nil {
		t.Fatal(err)
	}

	// A manifest copied to the wrong directory is not trusted.
	misplaced := repotest.Backup{VMID: 104, Time: day(1), Size: 1 << 10}
	m := repotest.Write(t, r, misplaced)
	other := repotest.Backup{VMID: 104, Time: day(2), Size: 1 << 10}
	repotest.Upload(t, r, other)
	data, err := manifest.Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tgt.PutBytes(ctx, other.ID().Path(layout.ManifestName), data); err != nil {
		t.Fatal(err)
	}

	// Another source and stray names are ignored.
	repotest.Write(t, r, repotest.Backup{Source: "office", VMID: 100, Size: 1})
	if _, err := tgt.PutBytes(ctx, "v1/homelab/qemu/notanumber/x", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := tgt.PutBytes(ctx, "v1/homelab/qemu/100/garbage/x", []byte("x")); err != nil {
		t.Fatal(err)
	}

	scanned, err := r.Scan(ctx, "homelab")
	if err != nil {
		t.Fatal(err)
	}
	got := map[layout.BackupID]repo.ScannedBackup{}
	for _, b := range scanned {
		got[b.ID] = b
	}
	want := map[layout.BackupID]string{
		complete.ID(): repo.StateComplete, empty.ID(): repo.StateComplete, tombstoned.ID(): repo.StateTombstoned,
		damaged.ID(): repo.StateDamaged, incomplete.ID(): repo.StateIncomplete, deleting.ID(): repo.StateDeleting,
		invalid.ID(): repo.StateInvalid, misplaced.ID(): repo.StateComplete, other.ID(): repo.StateInvalid,
	}
	if len(got) != len(want) {
		t.Errorf("scanned %d backups, want %d: %v", len(got), len(want), slices.Collect(maps.Keys(got)))
	}
	for id, state := range want {
		if got[id].State != state {
			t.Errorf("%s: state %q, want %q (problems %v)", id, got[id].State, state, got[id].Problems)
		}
	}
	if c := got[complete.ID()]; c.Manifest == nil || c.Meta == nil || c.Meta.Notes != "web" || c.Manifest.Sidecars.Log == nil {
		t.Errorf("complete backup: %+v", c)
	}
	if d := got[damaged.ID()]; len(d.Problems) != 1 || !strings.Contains(d.Problems[0], "segment 1 is missing") {
		t.Errorf("damaged problems = %v", d.Problems)
	}
	if ts := got[tombstoned.ID()]; ts.Meta == nil || ts.Meta.Tombstone == nil || !ts.Meta.Tombstone.DeleteAfter.Equal(day(10)) {
		t.Errorf("tombstoned meta = %+v", ts.Meta)
	}
}

func TestScanDetectsChangedProviderHash(t *testing.T) {
	r, dir := repotest.Init(t)
	b := repotest.Backup{Size: 100 << 10}
	repotest.Write(t, r, b)
	// Corrupt a stored segment in place, keeping its size.
	var parts []string
	err := filepath.WalkDir(filepath.Join(dir, "pve-backups", "g1"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, _ := d.Info(); info.Size() > 64<<10 {
				parts = append(parts, p)
			}
		}
		return err
	})
	if err != nil || len(parts) != 1 {
		t.Fatalf("found segments %v, %v", parts, err)
	}
	data, err := os.ReadFile(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	data[1000] ^= 0xff
	if err := os.WriteFile(parts[0], data, 0o600); err != nil {
		t.Fatal(err)
	}
	scanned, err := r.Scan(t.Context(), "homelab")
	if err != nil || len(scanned) != 1 {
		t.Fatalf("scan = %+v, %v", scanned, err)
	}
	if s := scanned[0]; s.State != repo.StateDamaged || !strings.Contains(strings.Join(s.Problems, ";"), "hash changed") {
		t.Fatalf("state %q, problems %v", s.State, s.Problems)
	}
}

func TestDelete(t *testing.T) {
	r, _ := repotest.Init(t)
	ctx := t.Context()
	keep := repotest.Backup{VMID: 100, Time: time.Date(2026, 10, 1, 2, 0, 1, 0, time.UTC), Size: 1 << 10}
	gone := repotest.Backup{VMID: 100, Time: time.Date(2026, 10, 2, 2, 0, 1, 0, time.UTC), Size: 100 << 10, Log: "log\n"}
	alone := repotest.Backup{VMID: 200, VMType: "lxc", Size: 1 << 10}
	if err := r.RegisterSource(ctx, "homelab", "6f0c2f1e-3a7b-4c2d-9e8f-0123456789ab", "", false); err != nil {
		t.Fatal(err)
	}
	for _, b := range []repotest.Backup{keep, gone, alone} {
		repotest.Write(t, r, b)
	}
	if err := r.Delete(ctx, 1, gone.ID(), "user"); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, 1, alone.ID(), "retention"); err != nil {
		t.Fatal(err)
	}
	scanned, err := r.Scan(ctx, "homelab")
	if err != nil || len(scanned) != 1 || scanned[0].ID != keep.ID() || scanned[0].State != repo.StateComplete {
		t.Fatalf("scan after delete = %+v, %v", scanned, err)
	}
	tgt, err := r.Target(1)
	if err != nil {
		t.Fatal(err)
	}
	// Empty directories are removed up to, but not including, the source.
	top, err := tgt.List(ctx, layout.SourceDir("homelab"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range top {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{"qemu", layout.SourceDoc}) {
		t.Fatalf("source directory holds %v", names)
	}
}

func TestDeleteResumesAfterCrash(t *testing.T) {
	r, _ := repotest.Init(t)
	ctx := t.Context()
	b := repotest.Backup{Size: 150 << 10}
	repotest.Write(t, r, b)
	tgt, err := r.Target(1)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the manifest and one segment were removed.
	meta, err := r.ReadMeta(ctx, 1, b.ID())
	if err != nil {
		t.Fatal(err)
	}
	meta.Tombstone = &manifest.Tombstone{RequestedAt: time.Now(), DeleteAfter: time.Now(), Reason: "retention", State: "deleting"}
	if err := r.WriteMeta(ctx, 1, b.ID(), meta); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{layout.ManifestName, layout.PartName(0)} {
		if err := tgt.Remove(ctx, b.ID().Path(name)); err != nil {
			t.Fatal(err)
		}
	}
	scanned, err := r.Scan(ctx, "homelab")
	if err != nil || len(scanned) != 1 || scanned[0].State != repo.StateDeleting {
		t.Fatalf("scan = %+v, %v", scanned, err)
	}
	if err := r.Delete(ctx, 1, b.ID(), "retention"); err != nil {
		t.Fatal(err)
	}
	if scanned, err := r.Scan(ctx, "homelab"); err != nil || len(scanned) != 0 {
		t.Fatalf("scan after resumed delete = %+v, %v", scanned, err)
	}
}
