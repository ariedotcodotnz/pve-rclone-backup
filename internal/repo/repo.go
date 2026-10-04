// SPDX-License-Identifier: AGPL-3.0-or-later

// Package repo implements repository-level operations on top of the
// transport: initialization, opening with key verification, source
// registration, committing and deleting backups, and scanning the remote
// state that the catalogue is rebuilt from.
package repo

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"slices"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/manifest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/secrets"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

var (
	// ErrNotInitialized means the location holds no repository marker.
	ErrNotInitialized = errors.New("repo: no repository at this location")
	// ErrAlreadyInitialized means a repository marker already exists.
	ErrAlreadyInitialized = errors.New("repo: a repository already exists at this location")
	// ErrWrongKeys means the available keys do not match the repository.
	ErrWrongKeys = errors.New("repo: the keys do not match this repository")
	// ErrSourceTaken means another installation uses the source name.
	ErrSourceTaken = errors.New("repo: the source name is registered by another installation")
	// ErrEncryptionMismatch means the repository marker claims another
	// encryption mode than the local configuration expects.
	ErrEncryptionMismatch = errors.New("repo: the repository's encryption mode differs from the configuration")
	// ErrInvalidDocument marks a document on the remote that cannot be
	// used: unparsable, too large, or describing another backup.
	ErrInvalidDocument = errors.New("repo: invalid document")
	// ErrReplaced means the location holds another repository than the one
	// recorded locally.
	ErrReplaced = errors.New("repo: a different repository now exists at this location")
)

// readDocument reads a small document, marking oversized or undecryptable
// objects as invalid documents rather than remote failures.
func readDocument(ctx context.Context, t *transport.Target, remote string, limit int64) ([]byte, error) {
	data, err := t.ReadAll(ctx, remote, limit)
	if errors.Is(err, transport.ErrTooLarge) || transport.Classify(err) == transport.ClassIntegrity {
		return nil, fmt.Errorf("%w: %w", ErrInvalidDocument, err)
	}
	return data, err
}

// sentinel is stored at each generation root to prove the data keys.
const sentinel = "pve-rclone-backup key check v1\n"

// Repo is an opened repository.
type Repo struct {
	Loc    transport.RepoLocation
	Marker *manifest.RepoMarker
	Keys   *secrets.RepoKeys // nil for unencrypted repositories

	base *transport.Target
	gens map[int]*transport.Target
	now  func() time.Time
}

// UUID returns the repository UUID.
func (r *Repo) UUID() string { return r.Marker.RepoUUID }

// ActiveGeneration returns the generation new backups are written to. For
// encrypted repositories the local keys decide, not the remote marker.
func (r *Repo) ActiveGeneration() int {
	if r.Keys != nil {
		if g, err := r.Keys.Active(); err == nil {
			return g.ID
		}
	}
	for i := len(r.Marker.Generations) - 1; i >= 0; i-- {
		if r.Marker.Generations[i].State == "active" {
			return r.Marker.Generations[i].ID
		}
	}
	return r.Marker.Generations[len(r.Marker.Generations)-1].ID
}

// Target returns the data target of a generation.
func (r *Repo) Target(gen int) (*transport.Target, error) {
	t, ok := r.gens[gen]
	if !ok {
		return nil, fmt.Errorf("repo: generation %d is not open", gen)
	}
	return t, nil
}

// Base returns the unencrypted repository base target.
func (r *Repo) Base() *transport.Target { return r.base }

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// NewUUID returns a random (version 4) UUID.
func NewUUID() string { return newUUID() }

func openGen(ctx context.Context, loc transport.RepoLocation, keys *secrets.RepoKeys, gen int) (*transport.Target, error) {
	if keys == nil {
		return transport.OpenBase(ctx, transport.RepoLocation{Remote: loc.Remote, Path: path.Join(loc.Path, transport.CryptRoot(gen))})
	}
	g, err := keys.Generation(gen)
	if err != nil {
		return nil, err
	}
	return transport.OpenCrypt(ctx, loc, *g)
}

// InitOptions configures a new repository.
type InitOptions struct {
	// Keys enables encryption with the keys' active generation. Nil
	// creates an unencrypted repository.
	Keys *secrets.RepoKeys
	Now  time.Time
}

// Init creates a repository. The marker is written last, so an
// interrupted initialization can simply be repeated.
func Init(ctx context.Context, loc transport.RepoLocation, opts InitOptions) (*Repo, error) {
	base, err := transport.OpenBase(ctx, loc)
	if err != nil {
		return nil, err
	}
	if _, err := base.Stat(ctx, layout.MarkerName); err == nil {
		return nil, ErrAlreadyInitialized
	} else if transport.Classify(err) != transport.ClassNotFound {
		return nil, err
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	marker := &manifest.RepoMarker{
		Format: manifest.FormatRepo, Version: 1, RepoUUID: newUUID(), CreatedAt: now.UTC(),
		Encryption: "none", MinReaderVersion: 1, SegmentNaming: "part.%06d",
	}
	gen := manifest.RepoGeneration{ID: 1, Root: transport.CryptRoot(1), State: "active", CreatedAt: now.UTC()}
	if opts.Keys != nil {
		marker.Encryption = "crypt"
		marker.RepoUUID = opts.Keys.RepoUUID
		g, err := opts.Keys.Generation(1)
		if err != nil {
			return nil, err
		}
		gen.Crypt = &manifest.CryptParams{FilenameEncryption: g.FilenameEncryption,
			DirectoryNameEncryption: g.DirectoryNameEncryption, FilenameEncoding: g.FilenameEncoding, Suffix: g.Suffix}
		name, err := transport.KeyCheckName(*g)
		if err != nil {
			return nil, err
		}
		gen.KeyCheck = &manifest.KeyCheck{Method: "enc-name+sentinel", EncryptedName: name}
	}
	marker.Generations = []manifest.RepoGeneration{gen}
	if err := marker.Validate(); err != nil {
		return nil, err
	}

	t, err := openGen(ctx, loc, opts.Keys, 1)
	if err != nil {
		return nil, err
	}
	if _, err := t.PutBytes(ctx, layout.KeyCheckName, []byte(sentinel)); err != nil {
		return nil, fmt.Errorf("repo: write key check: %w", err)
	}
	data, err := manifest.Encode(marker)
	if err != nil {
		return nil, err
	}
	if _, err := base.PutBytes(ctx, layout.MarkerName, data); err != nil {
		return nil, fmt.Errorf("repo: write marker: %w", err)
	}
	return &Repo{Loc: loc, Marker: marker, Keys: opts.Keys, base: base, gens: map[int]*transport.Target{1: t}, now: time.Now}, nil
}

// KeyLoader returns the keys of a repository (secrets.KeyStore.Load).
type KeyLoader func(repoUUID string) (*secrets.RepoKeys, error)

// ReadMarker reads the plaintext marker of a repository.
func ReadMarker(ctx context.Context, loc transport.RepoLocation) (*manifest.RepoMarker, *transport.Target, error) {
	base, err := transport.OpenBase(ctx, loc)
	if err != nil {
		return nil, nil, err
	}
	data, err := base.ReadAll(ctx, layout.MarkerName, manifest.MaxSmallDocSize)
	if err != nil {
		if transport.Classify(err) == transport.ClassNotFound {
			return nil, nil, ErrNotInitialized
		}
		return nil, nil, err
	}
	m, err := manifest.DecodeRepoMarker(data)
	if err != nil {
		return nil, nil, err
	}
	return m, base, nil
}

// OpenOptions configures Open. The plaintext marker is not authenticated:
// anyone with access to the remote can edit it, so it must never decide on
// its own whether data is encrypted or which repository this is.
type OpenOptions struct {
	// Encryption is the mode the local configuration expects: "crypt" or
	// "none".
	Encryption string
	// Keys loads the keys of an encrypted repository.
	Keys KeyLoader
	// UUID, if set, is the repository UUID recorded locally; a marker with
	// another UUID is refused.
	UUID string
}

// Open opens a repository and verifies that the keys match every
// generation before anything is decrypted.
func Open(ctx context.Context, loc transport.RepoLocation, opts OpenOptions) (*Repo, error) {
	if opts.Encryption != "crypt" && opts.Encryption != "none" {
		return nil, fmt.Errorf("repo: invalid expected encryption mode %q", opts.Encryption)
	}
	marker, base, err := ReadMarker(ctx, loc)
	if err != nil {
		return nil, err
	}
	if marker.Encryption != opts.Encryption {
		return nil, fmt.Errorf("%w (configured %s, repository marker says %s)", ErrEncryptionMismatch, opts.Encryption, marker.Encryption)
	}
	if opts.UUID != "" && marker.RepoUUID != opts.UUID {
		return nil, fmt.Errorf("%w (expected %s, found %s)", ErrReplaced, opts.UUID, marker.RepoUUID)
	}
	r := &Repo{Loc: loc, Marker: marker, base: base, gens: map[int]*transport.Target{}, now: time.Now}
	if marker.Encryption == "crypt" {
		if opts.Keys == nil {
			return nil, errors.New("repo: no key loader for an encrypted repository")
		}
		if r.Keys, err = opts.Keys(marker.RepoUUID); err != nil {
			return nil, fmt.Errorf("repo: keys of repository %s: %w", marker.RepoUUID, err)
		}
		for _, mg := range marker.Generations {
			if err := checkKeys(mg, r.Keys); err != nil {
				return nil, err
			}
		}
		active, err := r.Keys.Active()
		if err != nil {
			return nil, err
		}
		if _, ok := marker.Generation(active.ID); !ok {
			return nil, fmt.Errorf("%w: active key generation %d is not in the repository marker", ErrWrongKeys, active.ID)
		}
	}
	for _, mg := range marker.Generations {
		t, err := openGen(ctx, loc, r.Keys, mg.ID)
		if err != nil {
			return nil, err
		}
		got, err := t.ReadAll(ctx, layout.KeyCheckName, 1<<10)
		if err != nil {
			if transport.Classify(err) == transport.ClassNotFound || transport.Classify(err) == transport.ClassIntegrity {
				return nil, fmt.Errorf("%w (generation %d sentinel unreadable)", ErrWrongKeys, mg.ID)
			}
			return nil, err
		}
		if !bytes.Equal(got, []byte(sentinel)) {
			return nil, fmt.Errorf("%w (generation %d sentinel mismatch)", ErrWrongKeys, mg.ID)
		}
		r.gens[mg.ID] = t
	}
	return r, nil
}

func checkKeys(mg manifest.RepoGeneration, keys *secrets.RepoKeys) error {
	g, err := keys.Generation(mg.ID)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrWrongKeys, err)
	}
	c := mg.Crypt
	if c.FilenameEncryption != g.FilenameEncryption || c.DirectoryNameEncryption != g.DirectoryNameEncryption ||
		c.FilenameEncoding != g.FilenameEncoding || c.Suffix != g.Suffix {
		return fmt.Errorf("%w: crypt settings of generation %d differ", ErrWrongKeys, mg.ID)
	}
	if mg.KeyCheck != nil && mg.KeyCheck.EncryptedName != "" {
		name, err := transport.KeyCheckName(*g)
		if err != nil {
			return err
		}
		if name != mg.KeyCheck.EncryptedName {
			return fmt.Errorf("%w (generation %d)", ErrWrongKeys, mg.ID)
		}
	}
	return nil
}

// RegisterSource records a source in the active generation. A name already
// registered by an installation with another UUID is refused unless adopt
// is set (disaster recovery: continue backing up into the old namespace).
func (r *Repo) RegisterSource(ctx context.Context, name, uuid, clusterName string, adopt bool) error {
	if !layout.ValidSource(name) || !manifest.ValidUUID(uuid) {
		return fmt.Errorf("repo: invalid source %q / %q", name, uuid)
	}
	t, err := r.Target(r.ActiveGeneration())
	if err != nil {
		return err
	}
	docPath := path.Join(layout.SourceDir(name), layout.SourceDoc)
	if data, err := t.ReadAll(ctx, docPath, manifest.MaxSmallDocSize); err == nil {
		existing, err := manifest.DecodeSource(data)
		if err != nil {
			return err
		}
		if existing.UUID == uuid {
			return nil
		}
		if !adopt {
			return fmt.Errorf("%w (%s is owned by %s)", ErrSourceTaken, name, existing.UUID)
		}
	} else if transport.Classify(err) != transport.ClassNotFound {
		return err
	}
	doc, err := manifest.Encode(&manifest.Source{Format: manifest.FormatSource, Version: 1, Name: name,
		UUID: uuid, ClusterName: clusterName, CreatedAt: r.now().UTC()})
	if err != nil {
		return err
	}
	_, err = t.PutBytes(ctx, docPath, doc)
	return err
}

// Sources lists the source names in the active generation.
func (r *Repo) Sources(ctx context.Context) ([]string, error) {
	t, err := r.Target(r.ActiveGeneration())
	if err != nil {
		return nil, err
	}
	entries, err := t.List(ctx, layout.Version)
	if transport.Classify(err) == transport.ClassNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.Dir && layout.ValidSource(e.Name) {
			out = append(out, e.Name)
		}
	}
	return out, nil
}

// ReadManifest reads and validates a backup manifest.
func (r *Repo) ReadManifest(ctx context.Context, gen int, id layout.BackupID) (*manifest.Manifest, error) {
	t, err := r.Target(gen)
	if err != nil {
		return nil, err
	}
	data, err := readDocument(ctx, t, id.Path(layout.ManifestName), manifest.MaxManifestSize)
	if err != nil {
		return nil, err
	}
	m, err := manifest.DecodeManifest(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidDocument, err)
	}
	if m.Backup.ID() != id || m.RepoUUID != r.UUID() || m.Generation != gen {
		return nil, fmt.Errorf("%w: manifest at %s describes %s in generation %d of %s", ErrInvalidDocument, id, m.Backup.ID(), m.Generation, m.RepoUUID)
	}
	return m, nil
}

// ReadMeta reads a meta document; a missing one yields an empty document.
// A document that belongs to another backup is refused with
// ErrInvalidDocument (and manifest.ErrMisplaced).
func (r *Repo) ReadMeta(ctx context.Context, gen int, id layout.BackupID) (*manifest.Meta, error) {
	t, err := r.Target(gen)
	if err != nil {
		return nil, err
	}
	data, err := readDocument(ctx, t, id.Path(layout.MetaName), manifest.MaxSmallDocSize)
	if transport.Classify(err) == transport.ClassNotFound {
		return r.newMeta(gen, id), nil
	}
	if err != nil {
		return nil, err
	}
	meta, err := manifest.DecodeMeta(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidDocument, err)
	}
	if err := meta.CheckOwner(r.UUID(), gen, id); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidDocument, err)
	}
	return meta, nil
}

func (r *Repo) newMeta(gen int, id layout.BackupID) *manifest.Meta {
	meta := manifest.NewMeta(r.now())
	meta.RepoUUID, meta.Generation, meta.Backup = r.UUID(), gen, id.Dir()
	return meta
}

// WriteMeta replaces a backup's meta document.
func (r *Repo) WriteMeta(ctx context.Context, gen int, id layout.BackupID, meta *manifest.Meta) error {
	t, err := r.Target(gen)
	if err != nil {
		return err
	}
	meta.Format, meta.Version, meta.UpdatedAt = manifest.FormatMeta, 1, r.now().UTC()
	meta.RepoUUID, meta.Generation, meta.Backup = r.UUID(), gen, id.Dir()
	data, err := manifest.Encode(meta)
	if err != nil {
		return err
	}
	_, err = t.PutBytes(ctx, id.Path(layout.MetaName), data)
	return err
}

// PutLog stores the vzdump task log of a backup.
func (r *Repo) PutLog(ctx context.Context, gen int, id layout.BackupID, data []byte) (*manifest.FileRef, error) {
	t, err := r.Target(gen)
	if err != nil {
		return nil, err
	}
	if _, err := t.PutBytes(ctx, id.Path(layout.LogName), data); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	return &manifest.FileRef{Name: layout.LogName, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}, nil
}

// Commit makes a backup visible: it checks that every segment is stored
// with the expected size, writes the meta document and finally the
// manifest, which is the commit point.
func (r *Repo) Commit(ctx context.Context, m *manifest.Manifest, meta *manifest.Meta) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if m.RepoUUID != r.UUID() {
		return fmt.Errorf("repo: manifest belongs to repository %s", m.RepoUUID)
	}
	id := m.Backup.ID()
	t, err := r.Target(m.Generation)
	if err != nil {
		return err
	}
	entries, err := t.List(ctx, id.Dir())
	if err != nil {
		return fmt.Errorf("repo: commit %s: %w", id, err)
	}
	if problems := checkParts(m, entries); len(problems) > 0 {
		return fmt.Errorf("%w: commit %s: %s", transport.ErrIntegrity, id, problems[0])
	}
	if meta == nil {
		meta = r.newMeta(m.Generation, id)
	}
	if err := r.WriteMeta(ctx, m.Generation, id, meta); err != nil {
		return err
	}
	data, err := manifest.Encode(m)
	if err != nil {
		return err
	}
	if _, err := t.PutBytes(ctx, id.Path(layout.ManifestName), data); err != nil {
		return fmt.Errorf("repo: write manifest of %s: %w", id, err)
	}
	return nil
}

// checkParts compares stored segments with a manifest (presence, sizes and,
// where both sides have one, the provider hash).
func checkParts(m *manifest.Manifest, entries []transport.Entry) []string {
	byName := map[string]transport.Entry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	var problems []string
	for _, seg := range m.Segments.List {
		e, ok := byName[layout.PartName(seg.Index)]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("segment %d is missing", seg.Index))
		case e.Size != seg.Size || e.StoredSize != seg.StoredSize:
			problems = append(problems, fmt.Sprintf("segment %d has size %d/%d, expected %d/%d",
				seg.Index, e.Size, e.StoredSize, seg.Size, seg.StoredSize))
		case e.StoredHash != "" && len(seg.StoredHash) > 0 && !slices.Contains(mapValues(seg.StoredHash), e.StoredHash):
			problems = append(problems, fmt.Sprintf("segment %d provider hash changed", seg.Index))
		}
	}
	return problems
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// Delete removes a backup in an order that a crash cannot turn into an
// apparently complete but damaged backup: the meta document is marked as
// deleting, then the manifest (the commit record) goes first, then the
// data, then the meta document and empty directories.
func (r *Repo) Delete(ctx context.Context, gen int, id layout.BackupID, reason string) error {
	t, err := r.Target(gen)
	if err != nil {
		return err
	}
	meta, err := r.ReadMeta(ctx, gen, id)
	if errors.Is(err, ErrInvalidDocument) {
		// A foreign or unreadable meta document carries nothing worth
		// keeping; it is replaced by our own tombstone.
		meta, err = r.newMeta(gen, id), nil
	}
	if err != nil {
		return err
	}
	now := r.now().UTC()
	if meta.Tombstone == nil {
		meta.Tombstone = &manifest.Tombstone{RequestedAt: now, DeleteAfter: now, Reason: reason}
	}
	meta.Tombstone.State = "deleting"
	if err := r.WriteMeta(ctx, gen, id, meta); err != nil {
		return err
	}
	remove := func(name string) error {
		if err := t.Remove(ctx, id.Path(name)); err != nil && transport.Classify(err) != transport.ClassNotFound {
			return err
		}
		return nil
	}
	if err := remove(layout.ManifestName); err != nil {
		return err
	}
	entries, err := t.List(ctx, id.Dir())
	if err != nil && transport.Classify(err) != transport.ClassNotFound {
		return err
	}
	for _, e := range entries {
		if e.Dir || e.Name == layout.MetaName {
			continue
		}
		if err := remove(e.Name); err != nil {
			return err
		}
	}
	if err := remove(layout.MetaName); err != nil {
		return err
	}
	// Remove now-empty directories up to the source directory.
	for dir := id.Dir(); dir != layout.SourceDir(id.Source); dir = path.Dir(dir) {
		if err := t.Rmdir(ctx, dir); err != nil {
			break
		}
	}
	return nil
}
