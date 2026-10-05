// SPDX-License-Identifier: AGPL-3.0-or-later

package transport

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	rhash "github.com/rclone/rclone/fs/hash"
)

// Segment is a byte range of a local source that is stored as one remote
// object.
type Segment struct {
	// Source must support concurrent ReadAt calls (*os.File does).
	Source  io.ReaderAt
	Offset  int64
	Size    int64
	ModTime time.Time
	// Limiter, if set, limits how fast the segment is read.
	Limiter *Limiter
}

func (s Segment) validate() error {
	switch {
	case s.Source == nil:
		return errors.New("transport: segment has no source")
	case s.Offset < 0 || s.Size < 0:
		return fmt.Errorf("transport: invalid segment range offset=%d size=%d", s.Offset, s.Size)
	}
	return nil
}

// segmentFs is the fs.Info reported for segment source objects. It disables
// multi-threaded copies so rclone reads each segment as one sequential
// stream, which lets us hash the bytes while they are uploaded.
type segmentFs struct{}

var segmentFeatures = &fs.Features{NoMultiThreading: true}

func (segmentFs) Name() string             { return "pve-rclone-backup-segment" }
func (segmentFs) Root() string             { return "" }
func (segmentFs) String() string           { return "pve-rclone-backup segment source" }
func (segmentFs) Precision() time.Duration { return time.Nanosecond }
func (segmentFs) Hashes() rhash.Set        { return rhash.Set(rhash.None) }
func (segmentFs) Features() *fs.Features   { return segmentFeatures }

// attempt holds the hashers fed by one full sequential read of a segment.
// rclone may re-open the source on low-level retries; every full open starts
// a new attempt so retried bytes are never hashed twice.
type attempt struct {
	mu    sync.Mutex
	seg   hash.Hash
	whole hash.Hash          // nil when no whole-archive state is tracked
	plain *rhash.MultiHasher // provider hash of the plaintext, nil if unused
	n     int64
}

func (a *attempt) write(p []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seg.Write(p)
	if a.whole != nil {
		a.whole.Write(p)
	}
	if a.plain != nil {
		_, _ = a.plain.Write(p)
	}
	a.n += int64(len(p))
}

// segmentObject is a read-only fs.Object over a Segment.
type segmentObject struct {
	seg       Segment
	remote    string
	wholeBase []byte     // whole-archive hash state at the segment start
	plainType rhash.Type // provider hash to compute over plaintext, or None

	mu   sync.Mutex
	last *attempt
}

var _ fs.Object = (*segmentObject)(nil)

func (o *segmentObject) Fs() fs.Info                                 { return segmentFs{} }
func (o *segmentObject) Remote() string                              { return o.remote }
func (o *segmentObject) String() string                              { return o.remote }
func (o *segmentObject) ModTime(context.Context) time.Time           { return o.seg.ModTime }
func (o *segmentObject) Size() int64                                 { return o.seg.Size }
func (o *segmentObject) Storable() bool                              { return true }
func (o *segmentObject) SetModTime(context.Context, time.Time) error { return fs.ErrorCantSetModTime }
func (o *segmentObject) Remove(context.Context) error {
	return errors.New("transport: segment source is read-only")
}

func (o *segmentObject) Hash(context.Context, rhash.Type) (string, error) {
	return "", rhash.ErrUnsupported
}

func (o *segmentObject) Update(context.Context, io.Reader, fs.ObjectInfo, ...fs.OpenOption) error {
	return errors.New("transport: segment source is read-only")
}

// Open returns a reader over the segment, honouring rclone range and seek
// options. Only a full read from offset zero is hashed.
func (o *segmentObject) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	offset, limit := int64(0), int64(-1)
	for _, option := range options {
		switch x := option.(type) {
		case *fs.RangeOption:
			offset, limit = x.Decode(o.seg.Size)
		case *fs.SeekOption:
			offset = x.Offset
		default:
			if option.Mandatory() {
				return nil, fmt.Errorf("transport: unsupported mandatory open option %v", option)
			}
		}
	}
	offset = min(max(offset, 0), o.seg.Size)
	n := o.seg.Size - offset
	if limit >= 0 {
		n = min(n, limit)
	}
	var section io.Reader = io.NewSectionReader(o.seg.Source, o.seg.Offset+offset, n)
	if o.seg.Limiter != nil {
		section = &limitedReader{ctx: ctx, r: section, l: o.seg.Limiter}
	}
	if offset != 0 || n != o.seg.Size {
		return io.NopCloser(section), nil
	}

	a, err := o.newAttempt()
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	o.last = a
	o.mu.Unlock()
	return io.NopCloser(&hashingReader{r: section, a: a}), nil
}

func (o *segmentObject) newAttempt() (*attempt, error) {
	a := &attempt{seg: sha256.New()}
	if o.wholeBase != nil {
		h, err := restoreWholeHash(o.wholeBase)
		if err != nil {
			return nil, err
		}
		a.whole = h
	}
	if o.plainType != rhash.None {
		mh, err := rhash.NewMultiHasherTypes(rhash.NewHashSet(o.plainType))
		if err != nil {
			return nil, fmt.Errorf("transport: plaintext hasher: %w", err)
		}
		a.plain = mh
	}
	return a, nil
}

// lastCompleteAttempt returns the most recent attempt if it saw every byte.
func (o *segmentObject) lastCompleteAttempt() *attempt {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.last == nil {
		return nil
	}
	o.last.mu.Lock()
	defer o.last.mu.Unlock()
	if o.last.n != o.seg.Size {
		return nil
	}
	return o.last
}

// hashFully reads the whole segment once more and returns a completed
// attempt. It is the fallback when the upload did not consume the source
// as a single full sequential read. A source that ends early (truncated
// since the upload) is an error, not the hash of a shorter segment.
func (o *segmentObject) hashFully() (*attempt, error) {
	a, err := o.newAttempt()
	if err != nil {
		return nil, err
	}
	r := &hashingReader{r: io.NewSectionReader(o.seg.Source, o.seg.Offset, o.seg.Size), a: a}
	n, err := io.Copy(io.Discard, r)
	if err != nil {
		return nil, fmt.Errorf("transport: re-read segment for hashing: %w", err)
	}
	if n != o.seg.Size {
		return nil, fmt.Errorf("transport: re-read segment for hashing: read %d of %d bytes: %w", n, o.seg.Size, io.ErrUnexpectedEOF)
	}
	return a, nil
}

type hashingReader struct {
	r io.Reader
	a *attempt
}

func (h *hashingReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if n > 0 {
		h.a.write(p[:n])
	}
	return n, err
}
