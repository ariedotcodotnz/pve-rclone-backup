// SPDX-License-Identifier: AGPL-3.0-or-later

// Package manifest defines the versioned JSON documents stored in a
// repository. Remote documents are untrusted input: decoding checks the
// format and major version and enforces size limits, and paths are never
// taken from documents but derived from validated identities.
package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
)

// Document formats and the major versions this build reads.
const (
	FormatRepo     = "pve-rclone-backup.repo"
	FormatSource   = "pve-rclone-backup.source"
	FormatManifest = "pve-rclone-backup.manifest"
	FormatMeta     = "pve-rclone-backup.meta"

	// MaxManifestSize bounds manifests (guest configs can be large).
	MaxManifestSize = 16 << 20
	// MaxSmallDocSize bounds markers, source and meta documents.
	MaxSmallDocSize = 1 << 20
	// MaxSegments bounds the segments of one archive so that its manifest
	// (about 300 bytes per segment, plus a guest configuration of up to
	// 2 MiB) stays below MaxManifestSize: 2 TiB with the smallest segment
	// size, 32 TiB with the default 1 GiB.
	MaxSegments = 32768
)

// ErrUnsupported means a document has an unknown format or a newer major
// version than this build understands.
var ErrUnsupported = errors.New("manifest: unsupported document")

var (
	uuidRe   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	sha256Re = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidUUID reports whether s is a lowercase RFC 4122 UUID.
func ValidUUID(s string) bool { return uuidRe.MatchString(s) }

// CryptParams are the non-secret crypt settings of a key generation.
type CryptParams struct {
	FilenameEncryption      string `json:"filename_encryption"`
	DirectoryNameEncryption bool   `json:"directory_name_encryption"`
	FilenameEncoding        string `json:"filename_encoding"`
	Suffix                  string `json:"suffix"`
}

// KeyCheck lets a user's keys be validated before decrypting anything.
type KeyCheck struct {
	Method        string `json:"method"`                   // "enc-name+sentinel"
	EncryptedName string `json:"encrypted_name,omitempty"` // standard name encryption only
}

// RepoGeneration describes a key generation in the repository marker.
type RepoGeneration struct {
	ID        int          `json:"id"`
	Root      string       `json:"root"`
	State     string       `json:"state"`
	CreatedAt time.Time    `json:"created_at"`
	Crypt     *CryptParams `json:"crypt,omitempty"`
	KeyCheck  *KeyCheck    `json:"key_check,omitempty"`
}

// RepoMarker is the plaintext pve-rclone-backup.repo.json at the
// repository base. It contains no secrets.
type RepoMarker struct {
	Format           string           `json:"format"`
	Version          int              `json:"version"`
	RepoUUID         string           `json:"repo_uuid"`
	CreatedAt        time.Time        `json:"created_at"`
	Encryption       string           `json:"encryption"` // crypt | none
	MinReaderVersion int              `json:"min_reader_version"`
	SegmentNaming    string           `json:"segment_naming"`
	Generations      []RepoGeneration `json:"generations"`
}

// Validate checks the marker's consistency.
func (m *RepoMarker) Validate() error {
	if !ValidUUID(m.RepoUUID) {
		return fmt.Errorf("manifest: invalid repository UUID %q", m.RepoUUID)
	}
	if m.Encryption != "crypt" && m.Encryption != "none" {
		return fmt.Errorf("manifest: invalid encryption %q", m.Encryption)
	}
	if len(m.Generations) == 0 {
		return errors.New("manifest: repository has no generations")
	}
	for i, g := range m.Generations {
		if g.ID != i+1 || g.Root != fmt.Sprintf("g%d", g.ID) {
			return fmt.Errorf("manifest: invalid generation %d (root %q)", g.ID, g.Root)
		}
		if m.Encryption == "crypt" && g.Crypt == nil {
			return fmt.Errorf("manifest: generation %d lacks crypt parameters", g.ID)
		}
	}
	return nil
}

// Generation returns a generation by id.
func (m *RepoMarker) Generation(id int) (*RepoGeneration, bool) {
	for i := range m.Generations {
		if m.Generations[i].ID == id {
			return &m.Generations[i], true
		}
	}
	return nil, false
}

// Source registers a PVE installation (cluster or standalone node) in a
// repository.
type Source struct {
	Format      string    `json:"format"`
	Version     int       `json:"version"`
	Name        string    `json:"name"`
	UUID        string    `json:"uuid"`
	ClusterName string    `json:"cluster_name,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// BackupInfo is the identity part of a manifest.
type BackupInfo struct {
	Source         string `json:"source"`
	SourceUUID     string `json:"source_uuid"`
	VMType         string `json:"vmtype"`
	VMID           int    `json:"vmid"`
	TSLabel        string `json:"ts_label"`
	BackupTime     int64  `json:"backup_time"`
	CollisionIndex int    `json:"collision_index"`
	Volname        string `json:"volname"`
}

// ID returns the layout identity.
func (b BackupInfo) ID() layout.BackupID {
	return layout.BackupID{Source: b.Source, VMType: b.VMType, VMID: b.VMID, TSLabel: b.TSLabel, Collision: b.CollisionIndex}
}

// ArchiveInfo describes the replicated vzdump archive.
type ArchiveInfo struct {
	Filename      string    `json:"filename"`
	Format        string    `json:"format"`
	Compression   string    `json:"compression,omitempty"`
	Size          int64     `json:"size"`
	SHA256        string    `json:"sha256"`
	SourceStorage string    `json:"source_storage,omitempty"`
	SourceNode    string    `json:"source_node,omitempty"`
	SourceMtime   time.Time `json:"source_mtime"`
}

// SegmentInfo describes one stored segment.
type SegmentInfo struct {
	Index      int               `json:"index"`
	Size       int64             `json:"size"`
	SHA256     string            `json:"sha256"`
	StoredSize int64             `json:"stored_size"`
	StoredHash map[string]string `json:"stored_hash,omitempty"`
}

// Segments describes how the archive is split.
type Segments struct {
	Size   int64         `json:"size"`
	Count  int           `json:"count"`
	Naming string        `json:"naming"`
	List   []SegmentInfo `json:"list"`
}

// GuestInfo holds the guest configuration captured from the archive.
type GuestInfo struct {
	Name          string  `json:"name,omitempty"`
	Config        string  `json:"config"`
	Firewall      *string `json:"firewall"`
	FirewallKnown bool    `json:"firewall_known"`
}

// FileRef describes a stored side file such as the task log.
type FileRef struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Sidecars records side data captured at upload time.
type Sidecars struct {
	Log               *FileRef `json:"log,omitempty"`
	NotesAtUpload     string   `json:"notes_at_upload,omitempty"`
	ProtectedAtUpload bool     `json:"protected_at_upload,omitzero"`
}

// Verification records an integrity check.
type Verification struct {
	Level  int    `json:"level"`
	Method string `json:"method"`
}

// Manifest is the immutable commit record of a backup. A backup exists
// offsite if and only if its manifest exists.
type Manifest struct {
	Format             string       `json:"format"`
	Version            int          `json:"version"`
	RepoUUID           string       `json:"repo_uuid"`
	Generation         int          `json:"generation"`
	Backup             BackupInfo   `json:"backup"`
	Archive            ArchiveInfo  `json:"archive"`
	Segments           Segments     `json:"segments"`
	Guest              GuestInfo    `json:"guest"`
	Sidecars           Sidecars     `json:"sidecars"`
	PVE                PVEInfo      `json:"pve"`
	Tool               ToolInfo     `json:"tool"`
	UploadedAt         time.Time    `json:"uploaded_at"`
	UploadVerification Verification `json:"upload_verification"`
	Reconstructed      bool         `json:"reconstructed,omitzero"`
}

// PVEInfo records the Proxmox VE version at upload time.
type PVEInfo struct {
	Version       string `json:"version,omitempty"`
	StorageAPIVer int    `json:"storage_apiver,omitzero"`
}

// ToolInfo records the uploading software.
type ToolInfo struct {
	Version string `json:"version"`
	Rclone  string `json:"rclone"`
}

// Validate checks a manifest's internal consistency. It does not touch
// the remote.
func (m *Manifest) Validate() error {
	id := m.Backup.ID()
	if err := id.Validate(); err != nil {
		return err
	}
	if !ValidUUID(m.RepoUUID) || m.Generation < 1 {
		return errors.New("manifest: invalid repository or generation")
	}
	a, err := layout.ParseVolname(m.Backup.Volname)
	if err != nil {
		return err
	}
	if a.ID(id.Source) != id || m.Backup.Volname != id.Volname(a.Ext) {
		return fmt.Errorf("manifest: volname %q does not match backup %s", m.Backup.Volname, id)
	}
	// The original archive name carries no collision suffix.
	orig, err := layout.ParseArchiveName(m.Archive.Filename)
	if err != nil {
		return err
	}
	if orig.VMType != id.VMType || orig.VMID != id.VMID || orig.TSLabel != id.TSLabel || orig.Collision != 0 || orig.Ext != a.Ext {
		return fmt.Errorf("manifest: archive %q does not match backup %s", m.Archive.Filename, id)
	}
	if !sha256Re.MatchString(m.Archive.SHA256) || m.Archive.Size < 0 {
		return errors.New("manifest: invalid archive digest or size")
	}
	s := m.Segments
	if s.Size <= 0 || s.Count != len(s.List) || s.Count < 1 || s.Naming != "part.%06d" {
		return errors.New("manifest: invalid segment description")
	}
	if s.Count > MaxSegments {
		return fmt.Errorf("manifest: %d segments, more than the %d a manifest can describe", s.Count, MaxSegments)
	}
	var total int64
	for i, seg := range s.List {
		switch {
		case seg.Index != i:
			return fmt.Errorf("manifest: segment %d out of order", i)
		case !sha256Re.MatchString(seg.SHA256):
			return fmt.Errorf("manifest: segment %d has an invalid digest", i)
		case seg.Size < 0 || seg.Size > s.Size || (i < s.Count-1 && seg.Size != s.Size):
			return fmt.Errorf("manifest: segment %d has an invalid size %d", i, seg.Size)
		case seg.StoredSize < seg.Size:
			return fmt.Errorf("manifest: segment %d stored size %d is smaller than its size", i, seg.StoredSize)
		}
		total += seg.Size
	}
	if total != m.Archive.Size {
		return fmt.Errorf("manifest: segments add up to %d bytes, archive has %d", total, m.Archive.Size)
	}
	return nil
}

// Meta is the mutable per-backup document. Objects can be copied between
// directories by anyone with access to the remote, even without the keys,
// so a meta document names the backup it belongs to and readers reject it
// anywhere else.
type Meta struct {
	Format     string     `json:"format"`
	Version    int        `json:"version"`
	RepoUUID   string     `json:"repo_uuid"`
	Generation int        `json:"generation"`
	Backup     string     `json:"backup"` // backup directory relative to the generation root
	Notes      string     `json:"notes,omitempty"`
	Protected  bool       `json:"protected,omitzero"`
	Tombstone  *Tombstone `json:"tombstone"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// ErrMisplaced means a document describes another backup or repository
// than the location it was read from.
var ErrMisplaced = errors.New("manifest: document belongs to another backup")

// CheckOwner verifies that a meta document belongs to a backup.
func (m *Meta) CheckOwner(repoUUID string, gen int, id layout.BackupID) error {
	if m.RepoUUID != repoUUID || m.Generation != gen || m.Backup != id.Dir() {
		return fmt.Errorf("%w: meta of %q (generation %d of %s) found at %s", ErrMisplaced, m.Backup, m.Generation, m.RepoUUID, id)
	}
	return nil
}

// Tombstone marks a backup for deletion.
type Tombstone struct {
	RequestedAt time.Time `json:"requested_at"`
	DeleteAfter time.Time `json:"delete_after"`
	Reason      string    `json:"reason"` // retention | user
	By          string    `json:"by,omitempty"`
	State       string    `json:"state"` // pending | deleting
}

// NewMeta returns an empty meta document.
func NewMeta(now time.Time) *Meta {
	return &Meta{Format: FormatMeta, Version: 1, UpdatedAt: now.UTC()}
}

// Encode renders a document as indented JSON.
func Encode(v any) ([]byte, error) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

type header struct {
	Format  string `json:"format"`
	Version int    `json:"version"`
}

func decode(data []byte, format string, maxSize int, v any) error {
	if len(data) > maxSize {
		return fmt.Errorf("manifest: %s document of %d bytes exceeds %d", format, len(data), maxSize)
	}
	var h header
	if err := json.Unmarshal(data, &h); err != nil {
		return fmt.Errorf("manifest: parse %s: %w", format, err)
	}
	if h.Format != format {
		return fmt.Errorf("%w: expected %s, got %q", ErrUnsupported, format, h.Format)
	}
	if h.Version != 1 {
		return fmt.Errorf("%w: %s version %d", ErrUnsupported, format, h.Version)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("manifest: parse %s: %w", format, err)
	}
	return nil
}

// DecodeRepoMarker parses and validates a repository marker.
func DecodeRepoMarker(data []byte) (*RepoMarker, error) {
	var m RepoMarker
	if err := decode(data, FormatRepo, MaxSmallDocSize, &m); err != nil {
		return nil, err
	}
	if m.MinReaderVersion > 1 {
		return nil, fmt.Errorf("%w: repository requires reader version %d", ErrUnsupported, m.MinReaderVersion)
	}
	return &m, m.Validate()
}

// DecodeSource parses a source document.
func DecodeSource(data []byte) (*Source, error) {
	var s Source
	if err := decode(data, FormatSource, MaxSmallDocSize, &s); err != nil {
		return nil, err
	}
	if !layout.ValidSource(s.Name) || !ValidUUID(s.UUID) {
		return nil, errors.New("manifest: invalid source document")
	}
	return &s, nil
}

// DecodeManifest parses and validates a backup manifest.
func DecodeManifest(data []byte) (*Manifest, error) {
	var m Manifest
	if err := decode(data, FormatManifest, MaxManifestSize, &m); err != nil {
		return nil, err
	}
	return &m, m.Validate()
}

// DecodeMeta parses a meta document.
func DecodeMeta(data []byte) (*Meta, error) {
	var m Meta
	if err := decode(data, FormatMeta, MaxSmallDocSize, &m); err != nil {
		return nil, err
	}
	if t := m.Tombstone; t != nil && t.State != "pending" && t.State != "deleting" {
		return nil, fmt.Errorf("manifest: invalid tombstone state %q", t.State)
	}
	return &m, nil
}
