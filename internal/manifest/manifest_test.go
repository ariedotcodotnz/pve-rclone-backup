// SPDX-License-Identifier: AGPL-3.0-or-later

package manifest

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
)

var update = flag.Bool("update", false, "rewrite golden files")

func validManifest() *Manifest {
	fw := "[OPTIONS]\nenable: 1\n"
	return &Manifest{
		Format: FormatManifest, Version: 1, RepoUUID: "6f0c2f1e-3a7b-4c2d-9e8f-0123456789ab", Generation: 1,
		Backup: BackupInfo{Source: "homelab", SourceUUID: "1b2c3d4e-5f60-4a7b-8c9d-0e1f2a3b4c5d", VMType: "qemu",
			VMID: 100, TSLabel: "2026_10_04-02_00_01", BackupTime: 1790992801,
			Volname: "backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst"},
		Archive: ArchiveInfo{Filename: "vzdump-qemu-100-2026_10_04-02_00_01.vma.zst", Format: "vma", Compression: "zst",
			Size: 3<<20 + 5, SHA256: strings.Repeat("ab", 32), SourceStorage: "local", SourceNode: "pve1",
			SourceMtime: time.Date(2026, 10, 4, 2, 14, 55, 0, time.UTC)},
		Segments: Segments{Size: 1 << 20, Count: 4, Naming: "part.%06d", List: []SegmentInfo{
			{Index: 0, Size: 1 << 20, SHA256: strings.Repeat("01", 32), StoredSize: 1048864, StoredHash: map[string]string{"quickxor": "q0"}},
			{Index: 1, Size: 1 << 20, SHA256: strings.Repeat("02", 32), StoredSize: 1048864, StoredHash: map[string]string{"quickxor": "q1"}},
			{Index: 2, Size: 1 << 20, SHA256: strings.Repeat("03", 32), StoredSize: 1048864, StoredHash: map[string]string{"quickxor": "q2"}},
			{Index: 3, Size: 5, SHA256: strings.Repeat("04", 32), StoredSize: 53, StoredHash: map[string]string{"quickxor": "q3"}},
		}},
		Guest:              GuestInfo{Name: "web01", Config: "name: web01\nmemory: 2048\n", Firewall: &fw, FirewallKnown: true},
		Sidecars:           Sidecars{Log: &FileRef{Name: "vzdump.log", Size: 123, SHA256: strings.Repeat("cd", 32)}, NotesAtUpload: "nightly"},
		PVE:                PVEInfo{Version: "9.2.21", StorageAPIVer: 15},
		Tool:               ToolInfo{Version: "0.1.0", Rclone: "v1.75.1"},
		UploadedAt:         time.Date(2026, 10, 4, 5, 12, 9, 0, time.UTC),
		UploadVerification: Verification{Level: 2, Method: "crypt-ciphertext-hash+listing"},
	}
}

func TestManifestGoldenRoundTrip(t *testing.T) {
	m := validManifest()
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	golden := "testdata/manifest.v1.json"
	if *update {
		if err := os.WriteFile(golden, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("encoding changed; review and run go test -update\n%s", data)
	}
	back, err := DecodeManifest(want)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := Encode(back)
	if !bytes.Equal(again, want) {
		t.Fatal("decode/encode is not lossless")
	}
}

func TestManifestCompatibility(t *testing.T) {
	data, _ := Encode(validManifest())
	// Fields added by a later minor revision are ignored.
	extended := bytes.Replace(data, []byte(`"version": 1,`), []byte(`"version": 1, "future_field": {"x": 1},`), 1)
	if _, err := DecodeManifest(extended); err != nil {
		t.Fatalf("unknown field rejected: %v", err)
	}
	v2 := bytes.Replace(data, []byte(`"version": 1,`), []byte(`"version": 2,`), 1)
	if _, err := DecodeManifest(v2); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("newer major version: %v", err)
	}
	other := bytes.Replace(data, []byte(FormatManifest), []byte("something.else"), 1)
	if _, err := DecodeManifest(other); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("wrong format: %v", err)
	}
	huge := append(bytes.Repeat([]byte(" "), MaxManifestSize), data...)
	if _, err := DecodeManifest(huge); err == nil {
		t.Fatal("oversized manifest accepted")
	}
}

func TestManifestValidation(t *testing.T) {
	for name, mut := range map[string]func(*Manifest){
		"segment sum":      func(m *Manifest) { m.Archive.Size++ },
		"segment order":    func(m *Manifest) { m.Segments.List[1].Index = 2 },
		"short middle":     func(m *Manifest) { m.Segments.List[1].Size--; m.Archive.Size-- },
		"count":            func(m *Manifest) { m.Segments.Count = 3 },
		"digest":           func(m *Manifest) { m.Archive.SHA256 = "nothex" },
		"stored too small": func(m *Manifest) { m.Segments.List[0].StoredSize = 10 },
		"volname":          func(m *Manifest) { m.Backup.Volname = "backup/vzdump-qemu-101-2026_10_04-02_00_01.vma.zst" },
		"archive name":     func(m *Manifest) { m.Archive.Filename = "vzdump-qemu-100-2026_10_04-02_00_02.vma.zst" },
		"source":           func(m *Manifest) { m.Backup.Source = "../x" },
		"generation":       func(m *Manifest) { m.Generation = 0 },
		"naming":           func(m *Manifest) { m.Segments.Naming = "../%d" },
	} {
		m := validManifest()
		mut(m)
		if m.Validate() == nil {
			t.Errorf("%s: invalid manifest accepted", name)
		}
	}
	m := validManifest()
	m.Backup.CollisionIndex = 2
	m.Backup.Volname = "backup/vzdump-qemu-100-2026_10_04-02_00_01.2.vma.zst"
	if err := m.Validate(); err != nil {
		t.Fatalf("collision variant rejected: %v", err)
	}
}

func TestSmallDocuments(t *testing.T) {
	marker := &RepoMarker{Format: FormatRepo, Version: 1, RepoUUID: "6f0c2f1e-3a7b-4c2d-9e8f-0123456789ab",
		Encryption: "crypt", MinReaderVersion: 1, SegmentNaming: "part.%06d",
		Generations: []RepoGeneration{{ID: 1, Root: "g1", State: "active",
			Crypt:    &CryptParams{FilenameEncryption: "standard", DirectoryNameEncryption: true, FilenameEncoding: "base32768", Suffix: ".bin"},
			KeyCheck: &KeyCheck{Method: "enc-name+sentinel", EncryptedName: "x"}}}}
	data, _ := Encode(marker)
	got, err := DecodeRepoMarker(data)
	if err != nil || got.Generations[0].Crypt.FilenameEncoding != "base32768" {
		t.Fatalf("marker: %+v %v", got, err)
	}
	marker.MinReaderVersion = 2
	data, _ = Encode(marker)
	if _, err := DecodeRepoMarker(data); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("marker requiring a newer reader: %v", err)
	}
	marker.MinReaderVersion, marker.Generations[0].Crypt = 1, nil
	data, _ = Encode(marker)
	if _, err := DecodeRepoMarker(data); err == nil {
		t.Fatal("crypt marker without crypt parameters accepted")
	}

	src, _ := Encode(&Source{Format: FormatSource, Version: 1, Name: "homelab", UUID: "1b2c3d4e-5f60-4a7b-8c9d-0e1f2a3b4c5d"})
	if s, err := DecodeSource(src); err != nil || s.Name != "homelab" {
		t.Fatalf("source: %v", err)
	}
	meta := NewMeta(time.Unix(0, 0))
	meta.Tombstone = &Tombstone{State: "bogus"}
	md, _ := Encode(meta)
	if _, err := DecodeMeta(md); err == nil {
		t.Fatal("invalid tombstone state accepted")
	}
}

func TestMetaOwner(t *testing.T) {
	const repoUUID = "6f0c2f1e-3a7b-4c2d-9e8f-0123456789ab"
	id := layout.BackupID{Source: "homelab", VMType: "qemu", VMID: 100, TSLabel: "2026_10_04-02_00_01"}
	meta := NewMeta(time.Unix(0, 0))
	meta.RepoUUID, meta.Generation, meta.Backup = repoUUID, 1, id.Dir()
	if err := meta.CheckOwner(repoUUID, 1, id); err != nil {
		t.Fatal(err)
	}
	other := id
	other.VMID = 101
	for name, check := range map[string]error{
		"other backup":     meta.CheckOwner(repoUUID, 1, other),
		"other generation": meta.CheckOwner(repoUUID, 2, id),
		"other repository": meta.CheckOwner("7a1d3e2f-4b8c-4d3e-8f90-123456789abc", 1, id),
		"unbound":          NewMeta(time.Unix(0, 0)).CheckOwner(repoUUID, 1, id),
	} {
		if !errors.Is(check, ErrMisplaced) {
			t.Errorf("%s: %v", name, check)
		}
	}
}

// TestMaxSegmentsFitInAManifest: a manifest with the most segments and the
// largest guest configuration is still accepted by readers.
func TestMaxSegmentsFitInAManifest(t *testing.T) {
	m := validManifest()
	segLen := int64(64 << 20)
	m.Segments = Segments{Size: segLen, Count: MaxSegments, Naming: "part.%06d"}
	for i := range MaxSegments {
		m.Segments.List = append(m.Segments.List, SegmentInfo{Index: i, Size: segLen, SHA256: strings.Repeat("ab", 32),
			StoredSize: 67125280, StoredHash: map[string]string{"quickxor": strings.Repeat("A", 28)}})
	}
	m.Archive.Size = segLen * MaxSegments
	cfg := strings.Repeat("x", 1<<20)
	m.Guest.Config, m.Guest.Firewall = cfg, &cfg
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err := Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > MaxManifestSize {
		t.Fatalf("a manifest with %d segments is %d bytes, more than %d", MaxSegments, len(data), MaxManifestSize)
	}
	if _, err := DecodeManifest(data); err != nil {
		t.Fatal(err)
	}

	m.Segments.List = append(m.Segments.List, m.Segments.List[0])
	m.Segments.List[MaxSegments].Index = MaxSegments
	m.Segments.Count++
	m.Archive.Size += segLen
	if err := m.Validate(); err == nil {
		t.Fatalf("%d segments accepted", m.Segments.Count)
	}
}
