// SPDX-License-Identifier: AGPL-3.0-or-later

// Package repotest builds repositories on local disk for tests. It must
// not be imported by the daemon.
package repotest

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/manifest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/secrets"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport/faultfs"
)

// Storage holds the remotes configured by this package.
var Storage = transport.NewMemoryStorage()

// InitTransport initializes the rclone engine for a test binary; call it
// from TestMain and run the returned cleanup after the tests.
func InitTransport() (func(), error) {
	transport.RegisterProfile(transport.Profile{Backend: "faulty", MaxPathLength: 4096, MaxNameLength: 255,
		FilenameEncoding: "base32768", Tested: true})
	dir, err := os.MkdirTemp("", "pve-rclone-backup-repotest-")
	if err != nil {
		return nil, err
	}
	err = transport.Init(transport.Options{ConfigPath: filepath.Join(dir, "remotes.conf"), Storage: Storage, LowLevelRetries: 2})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	return func() { _ = os.RemoveAll(dir) }, nil
}

var remoteSeq atomic.Int64

// Remote configures a fault-injecting remote over a fresh directory and
// returns a repository location on it plus the directory. ctl may be nil.
func Remote(t testing.TB, ctl *faultfs.Controller) (transport.RepoLocation, string) {
	t.Helper()
	if ctl == nil {
		ctl = &faultfs.Controller{}
	}
	dir := t.TempDir()
	name := fmt.Sprintf("rt%d-%d", time.Now().UnixNano(), remoteSeq.Add(1))
	faultfs.Register(name, ctl)
	Storage.SetSection(name, map[string]string{"type": "faulty", "id": name, "remote": dir})
	return transport.RepoLocation{Remote: name, Path: "pve-backups"}, dir
}

// NewKeys returns fresh keys for a new repository.
func NewKeys(t testing.TB) *secrets.RepoKeys {
	t.Helper()
	k, err := secrets.NewRepoKeys(repo.NewUUID(), "base32768", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// Loader returns a key loader serving keys.
func Loader(keys ...*secrets.RepoKeys) repo.KeyLoader {
	return func(uuid string) (*secrets.RepoKeys, error) {
		for _, k := range keys {
			if k.RepoUUID == uuid {
				return k, nil
			}
		}
		return nil, fmt.Errorf("%w: %s", secrets.ErrNotFound, uuid)
	}
}

// Init creates an encrypted repository on a fresh remote.
func Init(t testing.TB) (*repo.Repo, string) {
	t.Helper()
	loc, dir := Remote(t, nil)
	r, err := repo.Init(t.Context(), loc, repo.InitOptions{Keys: NewKeys(t)})
	if err != nil {
		t.Fatal(err)
	}
	return r, dir
}

// Backup describes a synthetic backup.
type Backup struct {
	Source      string // default "homelab"
	SourceUUID  string // default a fixed UUID
	VMType      string // default "qemu"
	VMID        int    // default 100
	Time        time.Time
	Collision   int
	Size        int64 // archive size
	SegmentSize int64 // default 64 KiB
	Seed        uint64
	GuestName   string
	Config      string
	Notes       string
	Protected   bool
	Log         string
	// Content replaces the pseudo-random archive data (Size is ignored).
	Content []byte
	// Ext is the archive extension (default vma.zst or tar.zst).
	Ext string
}

func (b *Backup) defaults() {
	if b.Source == "" {
		b.Source = "homelab"
	}
	if b.SourceUUID == "" {
		b.SourceUUID = "1b2c3d4e-5f60-4a7b-8c9d-0e1f2a3b4c5d"
	}
	if b.VMType == "" {
		b.VMType = "qemu"
	}
	if b.VMID == 0 {
		b.VMID = 100
	}
	if b.Time.IsZero() {
		b.Time = time.Date(2026, 10, 4, 2, 0, 1, 0, time.UTC)
	}
	if b.SegmentSize == 0 {
		b.SegmentSize = 64 << 10
	}
	if b.Config == "" {
		b.Config = "name: guest\nmemory: 512\n"
	}
}

// ID returns the backup's identity.
func (b Backup) ID() layout.BackupID {
	b.defaults()
	return layout.BackupID{Source: b.Source, VMType: b.VMType, VMID: b.VMID, TSLabel: b.Time.Format("2006_01_02-15_04_05"), Collision: b.Collision}
}

func (b Backup) ext() string {
	switch {
	case b.Ext != "":
		return b.Ext
	case b.VMType == "lxc":
		return "tar.zst"
	}
	return "vma.zst"
}

// Data returns the archive bytes of the backup.
func (b Backup) Data() []byte {
	b.defaults()
	if b.Content != nil {
		return b.Content
	}
	data := make([]byte, b.Size)
	rng := rand.New(rand.NewPCG(b.Seed, uint64(b.VMID)))
	for i := range data {
		data[i] = byte(rng.Uint32())
	}
	return data
}

// Upload stores the segments (and log) of a backup and returns the
// manifest that would commit it, without committing.
func Upload(t testing.TB, r *repo.Repo, b Backup) *manifest.Manifest {
	t.Helper()
	b.defaults()
	id := b.ID()
	gen := r.ActiveGeneration()
	tgt, err := r.Target(gen)
	if err != nil {
		t.Fatal(err)
	}
	data := b.Data()
	b.Size = int64(len(data))
	src := bytes.NewReader(data)
	count := max(1, int((b.Size+b.SegmentSize-1)/b.SegmentSize))
	whole := transport.NewWholeHashState()
	segs := make([]manifest.SegmentInfo, 0, count)
	for i := range count {
		off := int64(i) * b.SegmentSize
		size := min(b.SegmentSize, b.Size-off)
		res, err := tgt.PutSegment(t.Context(), id.Path(layout.PartName(i)),
			transport.Segment{Source: src, Offset: off, Size: size, ModTime: b.Time}, whole)
		if err != nil {
			t.Fatal(err)
		}
		whole = res.WholeState
		seg := manifest.SegmentInfo{Index: i, Size: size, SHA256: res.SHA256, StoredSize: res.StoredSize}
		if res.StoredHashType != "" {
			seg.StoredHash = map[string]string{res.StoredHashType: res.StoredHash}
		}
		segs = append(segs, seg)
	}
	sum, err := transport.WholeHashSum(whole)
	if err != nil {
		t.Fatal(err)
	}
	ext := b.ext()
	parsed, err := layout.ParseArchiveName(fmt.Sprintf("vzdump-%s-%d-%s.%s", b.VMType, b.VMID, id.TSLabel, ext))
	if err != nil {
		t.Fatal(err)
	}
	m := &manifest.Manifest{
		Format: manifest.FormatManifest, Version: 1, RepoUUID: r.UUID(), Generation: gen,
		Backup: manifest.BackupInfo{Source: b.Source, SourceUUID: b.SourceUUID, VMType: b.VMType, VMID: b.VMID,
			TSLabel: id.TSLabel, BackupTime: b.Time.Unix(), CollisionIndex: b.Collision, Volname: id.Volname(ext)},
		Archive: manifest.ArchiveInfo{Filename: fmt.Sprintf("vzdump-%s-%d-%s.%s", b.VMType, b.VMID, id.TSLabel, ext),
			Format: parsed.Format, Compression: parsed.Compression,
			Size: b.Size, SHA256: sum, SourceStorage: "local", SourceNode: "pve1", SourceMtime: b.Time.UTC()},
		Segments:           manifest.Segments{Size: b.SegmentSize, Count: count, Naming: "part.%06d", List: segs},
		Guest:              manifest.GuestInfo{Name: b.GuestName, Config: b.Config, FirewallKnown: true},
		Sidecars:           manifest.Sidecars{NotesAtUpload: b.Notes, ProtectedAtUpload: b.Protected},
		Tool:               manifest.ToolInfo{Version: "test", Rclone: transport.RcloneVersion()},
		UploadedAt:         b.Time.Add(time.Hour).UTC(),
		UploadVerification: manifest.Verification{Level: 2, Method: "crypt-ciphertext-hash+listing"},
	}
	if b.Log != "" {
		ref, err := r.PutLog(t.Context(), gen, id, []byte(b.Log))
		if err != nil {
			t.Fatal(err)
		}
		m.Sidecars.Log = ref
	}
	return m
}

// Write uploads and commits a backup.
func Write(t testing.TB, r *repo.Repo, b Backup) *manifest.Manifest {
	t.Helper()
	m := Upload(t, r, b)
	meta := manifest.NewMeta(b.Time)
	meta.Notes, meta.Protected = b.Notes, b.Protected
	if err := r.Commit(t.Context(), m, meta); err != nil {
		t.Fatal(err)
	}
	return m
}
