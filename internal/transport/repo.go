// SPDX-License-Identifier: AGPL-3.0-or-later

package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/backend/crypt"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
	rhash "github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/operations"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/secrets"
)

var (
	remoteNameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	repoPathRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*(?:/[A-Za-z0-9][A-Za-z0-9._-]*)*$`)
)

// RemoteType returns the rclone backend type of a configured remote.
func RemoteType(remote string) (string, error) {
	if !remoteNameRe.MatchString(remote) {
		return "", fmt.Errorf("transport: invalid remote name %q", remote)
	}
	typ, ok := config.LoadedData().GetValue(remote, "type")
	if !ok || typ == "" {
		return "", fmt.Errorf("transport: remote %q is not configured", remote)
	}
	return typ, nil
}

// RepoLocation is a repository on a transport remote.
type RepoLocation struct {
	Remote string // transport remote name in remotes.conf
	Path   string // repository base path inside the remote
}

// Validate checks the remote name and repository path.
func (l RepoLocation) Validate() error {
	if !remoteNameRe.MatchString(l.Remote) {
		return fmt.Errorf("transport: invalid remote name %q", l.Remote)
	}
	if !repoPathRe.MatchString(l.Path) || slices.Contains(strings.Split(l.Path, "/"), "..") {
		return fmt.Errorf("transport: invalid repository path %q", l.Path)
	}
	return nil
}

// Profile returns the capability profile of the location's backend.
func (l RepoLocation) Profile() (Profile, error) {
	typ, err := RemoteType(l.Remote)
	if err != nil {
		return Profile{}, err
	}
	return ProfileFor(typ)
}

// OpenBase opens the repository base path without encryption. It holds the
// plaintext repository marker and, below gN/, the crypt roots.
func OpenBase(ctx context.Context, l RepoLocation) (*Target, error) {
	if err := l.Validate(); err != nil {
		return nil, err
	}
	return NewTarget(ctx, l.Remote+":"+l.Path)
}

// CryptRoot is the base-relative directory of a key generation.
func CryptRoot(gen int) string { return fmt.Sprintf("g%d", gen) }

// OpenCrypt opens the crypt root of a key generation. The keys are handed
// to the crypt backend in an in-memory configuration map, so they are never
// written to remotes.conf. The backend is constructed directly rather than
// with fs.NewFs, which logs the remote string it is given at debug level:
// the keys never become part of any string rclone could log.
func OpenCrypt(ctx context.Context, l RepoLocation, g secrets.Generation) (*Target, error) {
	if err := l.Validate(); err != nil {
		return nil, err
	}
	pw, err := obscure.Obscure(g.Password)
	if err != nil {
		return nil, err
	}
	pw2, err := obscure.Obscure(g.Password2)
	if err != nil {
		return nil, err
	}
	ri, err := fs.Find("crypt")
	if err != nil {
		return nil, err
	}
	base := l.Remote + ":" + path.Join(l.Path, CryptRoot(g.ID))
	// Named like fs.NewFs names configured-on-the-fly remotes, after a hash
	// of the location (not of the keys).
	sum := sha256.Sum256([]byte(base))
	name := ":crypt{" + base64.RawURLEncoding.EncodeToString(sum[:])[:8] + "}"
	m := fs.ConfigMap(ri.Prefix, ri.Options, name, configmap.Simple{
		"remote":                    base,
		"password":                  pw,
		"password2":                 pw2,
		"filename_encryption":       g.FilenameEncryption,
		"directory_name_encryption": strconv.FormatBool(g.DirectoryNameEncryption),
		"filename_encoding":         g.FilenameEncoding,
		"suffix":                    g.Suffix,
	})
	f, err := ri.NewFs(ctx, name, "", m)
	if err != nil {
		return nil, fmt.Errorf("transport: open crypt root %s of %s: %w", CryptRoot(g.ID), l.Remote+":"+l.Path, err)
	}
	t := targetFromFs(f)
	if !t.Encrypted() {
		return nil, errors.New("transport: crypt remote did not open as crypt")
	}
	return t, nil
}

// PutBytes stores a small document (manifest, marker) as one object.
func (t *Target) PutBytes(ctx context.Context, remote string, data []byte) (*SegmentResult, error) {
	return t.PutSegment(ctx, remote, Segment{Source: bytes.NewReader(data), Size: int64(len(data)), ModTime: time.Now()}, nil)
}

// ErrTooLarge is returned by ReadAll for objects above the size limit.
var ErrTooLarge = errors.New("transport: object exceeds the size limit")

// ReadAll reads a whole object, refusing objects larger than limit.
func (t *Target) ReadAll(ctx context.Context, remote string, limit int64) ([]byte, error) {
	rc, err := t.Open(ctx, remote)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("transport: read %s: %w", remote, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: %s is larger than %d bytes", ErrTooLarge, remote, limit)
	}
	return data, nil
}

// Entry is a directory listing entry.
type Entry struct {
	Name       string // base name (decrypted)
	Dir        bool
	Size       int64 // plaintext size (objects)
	StoredSize int64 // stored size (objects)
	// StoredHash is the provider hash of the stored bytes, of algorithm
	// StoredHashType (both empty if the backend has none).
	StoredHash     string
	StoredHashType string
}

// List lists dir non-recursively. A missing directory yields
// fs.ErrorDirNotFound.
func (t *Target) List(ctx context.Context, dir string) ([]Entry, error) {
	entries, err := t.f.List(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("transport: list %q: %w", dir, err)
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		ent := Entry{Name: path.Base(e.Remote())}
		switch x := e.(type) {
		case fs.Directory:
			ent.Dir = true
		case fs.Object:
			base := fs.UnWrapObject(x)
			ent.Size, ent.StoredSize = x.Size(), base.Size()
			if t.baseHash != rhash.None {
				if ent.StoredHash, err = base.Hash(ctx, t.baseHash); err != nil {
					return nil, fmt.Errorf("transport: hash of %s: %w", e.Remote(), err)
				}
				ent.StoredHashType = t.baseHash.String()
			}
		}
		out = append(out, ent)
	}
	slices.SortFunc(out, func(a, b Entry) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// Rmdir removes an empty directory.
func (t *Target) Rmdir(ctx context.Context, dir string) error {
	if err := t.f.Rmdir(ctx, dir); err != nil {
		return fmt.Errorf("transport: remove directory %q: %w", dir, err)
	}
	return nil
}

// ProbeResult reports a connectivity check.
type ProbeResult struct {
	Latency  time.Duration
	Usage    *Usage // nil if the backend reports no quota
	HashType string
}

// Probe writes, reads back and deletes a small object below
// .pve-rclone-backup-probe/, then queries the quota.
func (t *Target) Probe(ctx context.Context) (*ProbeResult, error) {
	start := time.Now()
	name := ".pve-rclone-backup-probe/" + rand.Text()
	payload := []byte("pve-rclone-backup probe " + time.Now().UTC().Format(time.RFC3339Nano))
	if _, err := t.PutBytes(ctx, name, payload); err != nil {
		return nil, err
	}
	got, err := t.ReadAll(ctx, name, 1<<10)
	rmErr := t.Remove(ctx, name)
	_ = operations.Rmdirs(ctx, t.f, ".pve-rclone-backup-probe", false)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(got, payload) {
		return nil, fmt.Errorf("%w: probe object read back differently", ErrIntegrity)
	}
	if rmErr != nil {
		return nil, rmErr
	}
	res := &ProbeResult{Latency: time.Since(start)}
	if t.baseHash != rhash.None {
		res.HashType = t.baseHash.String()
	}
	if u, err := t.About(ctx); err == nil {
		res.Usage = u
	} else if !errors.Is(err, ErrNoQuota) {
		return nil, err
	}
	return res, nil
}

// keyCheckPlain is the constant whose encrypted name proves a key generation.
const keyCheckPlain = "pve-rclone-backup-key-check"

// KeyCheckName returns the deterministic encrypted form of a constant name
// under a key generation (standard filename encryption only). Comparing
// it with the repository marker tells whether keys are right without
// decrypting any data.
func KeyCheckName(g secrets.Generation) (string, error) {
	if g.FilenameEncryption != "standard" {
		return "", nil
	}
	pw, err := obscure.Obscure(g.Password)
	if err != nil {
		return "", err
	}
	pw2, err := obscure.Obscure(g.Password2)
	if err != nil {
		return "", err
	}
	c, err := crypt.NewCipher(configmap.Simple{
		"password": pw, "password2": pw2, "filename_encryption": g.FilenameEncryption,
		"filename_encoding": g.FilenameEncoding, "suffix": g.Suffix,
		"directory_name_encryption": fmt.Sprint(g.DirectoryNameEncryption),
	})
	if err != nil {
		return "", err
	}
	return c.EncryptFileName(keyCheckPlain), nil
}
