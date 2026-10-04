// SPDX-License-Identifier: AGPL-3.0-or-later

package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
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

func (l RepoLocation) validate() error {
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
	if err := l.validate(); err != nil {
		return nil, err
	}
	return NewTarget(ctx, l.Remote+":"+l.Path)
}

// CryptRoot is the base-relative directory of a key generation.
func CryptRoot(gen int) string { return fmt.Sprintf("g%d", gen) }

func quoteConn(v string) string {
	// Connection string values are quoted with single quotes; a literal
	// single quote is written twice.
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// OpenCrypt opens the crypt root of a key generation. The keys are passed
// in an rclone connection string, so they are never written to
// remotes.conf and rclone names the remote by a hash, not the keys.
func OpenCrypt(ctx context.Context, l RepoLocation, g secrets.Generation) (*Target, error) {
	if err := l.validate(); err != nil {
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
	conn := fmt.Sprintf(":crypt,remote=%s,password=%s,password2=%s,filename_encryption=%s,directory_name_encryption=%t,filename_encoding=%s,suffix=%s:",
		quoteConn(l.Remote+":"+path.Join(l.Path, CryptRoot(g.ID))), quoteConn(pw), quoteConn(pw2),
		g.FilenameEncryption, g.DirectoryNameEncryption, g.FilenameEncoding, quoteConn(g.Suffix))
	// The connection string carries the (reversibly obscured) keys: never
	// put it into an error or log message.
	f, err := fs.NewFs(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("transport: open crypt root %s of %s: %s", CryptRoot(g.ID), l.Remote+":"+l.Path,
			strings.ReplaceAll(err.Error(), conn, ":crypt:"))
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
	StoredHash string
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
