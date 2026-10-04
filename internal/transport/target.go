// SPDX-License-Identifier: AGPL-3.0-or-later

package transport

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/rclone/rclone/backend/crypt"
	"github.com/rclone/rclone/fs"
	rhash "github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/fs/operations"
)

// ErrNoProviderHash is returned when the storage backend reports no hash, so
// content can only be checked by size or by downloading it.
var ErrNoProviderHash = errors.New("transport: storage backend provides no hash")

// Target is a remote location objects are written to, optionally through a
// crypt layer.
type Target struct {
	f        fs.Fs     // Fs written through (crypt or plain)
	crypt    *crypt.Fs // non-nil when f encrypts
	base     fs.Fs     // underlying storage Fs
	baseHash rhash.Type
}

// NewTarget opens an rclone remote such as "repo-crypt:" or "base:path".
func NewTarget(ctx context.Context, remote string) (*Target, error) {
	f, err := fs.NewFs(ctx, remote)
	if err != nil {
		if errors.Is(err, fs.ErrorIsFile) {
			return nil, fmt.Errorf("transport: remote %q points at a file, not a directory", remote)
		}
		return nil, fmt.Errorf("transport: open remote %q: %w", remote, err)
	}
	t := &Target{f: f, base: f}
	if c, ok := f.(*crypt.Fs); ok {
		t.crypt = c
		t.base = c.UnWrap()
	}
	t.baseHash = t.base.Hashes().GetOne()
	return t, nil
}

// Encrypted reports whether objects are encrypted with rclone crypt.
func (t *Target) Encrypted() bool { return t.crypt != nil }

// ProviderHashType is the hash the storage backend reports for stored
// (possibly encrypted) bytes, or rhash.None.
func (t *Target) ProviderHashType() rhash.Type { return t.baseHash }

// StoredSize returns the number of bytes the backend stores for size bytes
// of plaintext.
func (t *Target) StoredSize(size int64) int64 {
	if t.crypt == nil {
		return size
	}
	return CryptStoredSize(size)
}

// rclone crypt file format constants (backend/crypt/cipher.go).
const (
	cryptHeaderSize      = 32        // magic + nonce
	cryptBlockHeaderSize = 16        // Poly1305 tag per block
	cryptBlockDataSize   = 64 * 1024 // plaintext bytes per block
)

// CryptStoredSize returns the size rclone crypt stores for size bytes of
// plaintext.
func CryptStoredSize(size int64) int64 {
	blocks, residue := size/cryptBlockDataSize, size%cryptBlockDataSize
	stored := int64(cryptHeaderSize) + blocks*(cryptBlockHeaderSize+cryptBlockDataSize)
	if residue != 0 {
		stored += cryptBlockHeaderSize + residue
	}
	return stored
}

// SegmentResult describes a successfully uploaded segment.
type SegmentResult struct {
	SHA256         string // hex SHA-256 of the plaintext segment
	WholeState     []byte // whole-archive hash state after this segment (nil if not tracked)
	StoredSize     int64  // bytes stored by the backend
	StoredHashType string // provider hash type name, empty if none
	StoredHash     string // provider hash of the stored bytes, empty if none
}

// PutSegment uploads seg to remote. wholeState is the whole-archive hash
// state at the start of the segment (nil to skip whole-archive hashing).
//
// The upload is integrity-checked against the provider-reported hash: by
// rclone crypt for encrypted targets, and by comparing a plaintext hash we
// compute ourselves for plain targets. On a mismatch the object is removed
// and an error is returned.
func (t *Target) PutSegment(ctx context.Context, remote string, seg Segment, wholeState []byte) (*SegmentResult, error) {
	if err := seg.validate(); err != nil {
		return nil, err
	}
	src := &segmentObject{seg: seg, remote: remote, wholeBase: wholeState}
	if t.crypt == nil {
		src.plainType = t.baseHash
	}

	dst, err := operations.Copy(ctx, t.f, nil, remote, src)
	if err != nil {
		return nil, fmt.Errorf("transport: upload %s: %w", remote, err)
	}
	if dst == nil {
		return nil, fmt.Errorf("transport: upload %s: backend returned no object", remote)
	}

	a := src.lastCompleteAttempt()
	if a == nil {
		if a, err = src.hashFully(); err != nil {
			return nil, err
		}
	}

	base := fs.UnWrapObject(dst)
	res := &SegmentResult{
		SHA256:     hex.EncodeToString(a.seg.Sum(nil)),
		StoredSize: base.Size(),
	}
	if a.whole != nil {
		if res.WholeState, err = saveWholeHash(a.whole); err != nil {
			return nil, err
		}
	}
	if want := t.StoredSize(seg.Size); res.StoredSize != want {
		_ = dst.Remove(ctx)
		return nil, fmt.Errorf("transport: upload %s: stored size %d, want %d", remote, res.StoredSize, want)
	}
	if t.baseHash != rhash.None {
		res.StoredHashType = t.baseHash.String()
		if res.StoredHash, err = base.Hash(ctx, t.baseHash); err != nil {
			return nil, fmt.Errorf("transport: read provider hash of %s: %w", remote, err)
		}
		if a.plain != nil && res.StoredHash != "" {
			local, err := a.plain.SumString(t.baseHash, false)
			if err != nil {
				return nil, fmt.Errorf("transport: plaintext hash of %s: %w", remote, err)
			}
			if local != res.StoredHash {
				_ = dst.Remove(ctx)
				return nil, fmt.Errorf("transport: upload %s corrupted on transfer: %v %q != %q", remote, t.baseHash, local, res.StoredHash)
			}
		}
	}
	return res, nil
}

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Size           int64  // plaintext size
	StoredSize     int64  // size as stored by the backend
	StoredHashType string // provider hash type name, empty if none
	StoredHash     string // provider hash, empty if none
}

// Stat returns information about remote without downloading it. It returns
// an error wrapping fs.ErrorObjectNotFound if the object does not exist.
func (t *Target) Stat(ctx context.Context, remote string) (*ObjectInfo, error) {
	o, err := t.f.NewObject(ctx, remote)
	if err != nil {
		return nil, fmt.Errorf("transport: stat %s: %w", remote, err)
	}
	base := fs.UnWrapObject(o)
	info := &ObjectInfo{Size: o.Size(), StoredSize: base.Size()}
	if t.baseHash != rhash.None {
		info.StoredHashType = t.baseHash.String()
		if info.StoredHash, err = base.Hash(ctx, t.baseHash); err != nil {
			return nil, fmt.Errorf("transport: read provider hash of %s: %w", remote, err)
		}
	}
	return info, nil
}

// VerifySegment reports whether remote holds exactly the bytes of seg,
// without downloading the object: it recomputes the provider hash of what
// the stored bytes should be (re-encrypting with the object's nonce for
// crypt targets) and compares it with the provider-reported hash.
func (t *Target) VerifySegment(ctx context.Context, remote string, seg Segment) (bool, error) {
	if err := seg.validate(); err != nil {
		return false, err
	}
	if t.baseHash == rhash.None {
		return false, ErrNoProviderHash
	}
	o, err := t.f.NewObject(ctx, remote)
	if err != nil {
		return false, fmt.Errorf("transport: verify %s: %w", remote, err)
	}
	if o.Size() != seg.Size {
		return false, nil
	}
	stored, err := fs.UnWrapObject(o).Hash(ctx, t.baseHash)
	if err != nil {
		return false, fmt.Errorf("transport: read provider hash of %s: %w", remote, err)
	}
	if stored == "" {
		return false, ErrNoProviderHash
	}

	src := &segmentObject{seg: seg, remote: remote}
	var want string
	if t.crypt != nil {
		co, ok := o.(*crypt.Object)
		if !ok {
			return false, fmt.Errorf("transport: verify %s: unexpected object type %T", remote, o)
		}
		if want, err = t.crypt.ComputeHash(ctx, co, src, t.baseHash); err != nil {
			return false, fmt.Errorf("transport: compute encrypted hash of %s: %w", remote, err)
		}
	} else {
		src.plainType = t.baseHash
		a, err := src.hashFully()
		if err != nil {
			return false, err
		}
		if want, err = a.plain.SumString(t.baseHash, false); err != nil {
			return false, fmt.Errorf("transport: plaintext hash of %s: %w", remote, err)
		}
	}
	return want == stored, nil
}

// Open opens remote for reading. For crypt targets every 64 KiB block is
// authenticated while it is read.
func (t *Target) Open(ctx context.Context, remote string) (io.ReadCloser, error) {
	o, err := t.f.NewObject(ctx, remote)
	if err != nil {
		return nil, fmt.Errorf("transport: open %s: %w", remote, err)
	}
	rc, err := o.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("transport: open %s: %w", remote, err)
	}
	return rc, nil
}

// Remove deletes remote.
func (t *Target) Remove(ctx context.Context, remote string) error {
	o, err := t.f.NewObject(ctx, remote)
	if err != nil {
		return fmt.Errorf("transport: remove %s: %w", remote, err)
	}
	if err := o.Remove(ctx); err != nil {
		return fmt.Errorf("transport: remove %s: %w", remote, err)
	}
	return nil
}

// Usage is the storage quota reported by the backend. Nil fields are unknown.
type Usage struct {
	Total   *int64 // quota in bytes
	Used    *int64 // bytes in use
	Free    *int64 // bytes that can still be uploaded
	Trashed *int64 // bytes in the provider's recycle bin
}

// ErrNoQuota is returned when the backend cannot report its quota.
var ErrNoQuota = errors.New("transport: storage backend does not report quota")

// About returns the backend's quota.
func (t *Target) About(ctx context.Context) (*Usage, error) {
	about := t.base.Features().About
	if about == nil {
		return nil, ErrNoQuota
	}
	u, err := about(ctx)
	if err != nil {
		return nil, fmt.Errorf("transport: about: %w", err)
	}
	return &Usage{Total: u.Total, Used: u.Used, Free: u.Free, Trashed: u.Trashed}, nil
}
