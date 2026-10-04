// SPDX-License-Identifier: AGPL-3.0-or-later

// Package restore reads offsite backups back: fetching an archive into a
// local backup storage, and restoring guests by streaming a VM archive
// into qmrestore or staging an archive for pct restore and qmrestore.
// Every byte read is checked against the manifest: each segment's
// SHA-256 as it completes, and the whole archive's digest at the end.
package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/manifest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

// Reader streams an archive from its segments and verifies it.
type Reader struct {
	ctx  context.Context
	tgt  *transport.Target
	id   layout.BackupID
	m    *manifest.Manifest
	idx  int
	cur  io.ReadCloser
	seg  hash.Hash
	got  int64 // bytes of the current segment
	all  hash.Hash
	read int64
	err  error
}

// NewReader returns a reader of the archive described by m, stored below
// id in tgt (the manifest's generation).
func NewReader(ctx context.Context, tgt *transport.Target, id layout.BackupID, m *manifest.Manifest) *Reader {
	return &Reader{ctx: ctx, tgt: tgt, id: id, m: m, all: sha256.New()}
}

// Read implements io.Reader. A segment or archive that does not match
// the manifest fails with an error wrapping transport.ErrIntegrity; the
// final digest is checked before io.EOF is returned.
func (r *Reader) Read(p []byte) (int, error) {
	for r.err == nil {
		if r.cur == nil {
			if r.idx >= len(r.m.Segments.List) {
				if sum := hex.EncodeToString(r.all.Sum(nil)); sum != r.m.Archive.SHA256 {
					r.err = fmt.Errorf("%w: archive digest %s, manifest has %s", transport.ErrIntegrity, sum, r.m.Archive.SHA256)
				} else {
					r.err = io.EOF
				}
				break
			}
			rc, err := r.tgt.Open(r.ctx, r.id.Path(layout.PartName(r.idx)))
			if err != nil {
				r.err = err
				break
			}
			r.cur, r.seg, r.got = rc, sha256.New(), 0
		}
		n, err := r.cur.Read(p)
		if n > 0 {
			r.seg.Write(p[:n])
			r.all.Write(p[:n])
			r.got += int64(n)
			r.read += int64(n)
			if want := r.m.Segments.List[r.idx].Size; r.got > want {
				r.err = fmt.Errorf("%w: segment %d is longer than %d bytes", transport.ErrIntegrity, r.idx, want)
				return 0, r.err
			}
			if err == io.EOF {
				err = nil
				if cerr := r.finishSegment(); cerr != nil {
					return n, cerr
				}
			}
			return n, err
		}
		if err == io.EOF {
			if cerr := r.finishSegment(); cerr != nil {
				return 0, cerr
			}
			continue
		}
		if err != nil {
			r.err = fmt.Errorf("restore: read segment %d: %w", r.idx, err)
		}
	}
	return 0, r.err
}

func (r *Reader) finishSegment() error {
	want := r.m.Segments.List[r.idx]
	_ = r.cur.Close()
	r.cur = nil
	switch {
	case r.got != want.Size:
		r.err = fmt.Errorf("%w: segment %d has %d bytes, manifest has %d", transport.ErrIntegrity, r.idx, r.got, want.Size)
	case hex.EncodeToString(r.seg.Sum(nil)) != want.SHA256:
		r.err = fmt.Errorf("%w: segment %d does not match its digest", transport.ErrIntegrity, r.idx)
	}
	r.idx++
	return r.err
}

// BytesRead returns the number of bytes read so far.
func (r *Reader) BytesRead() int64 { return r.read }

// Close releases the current segment.
func (r *Reader) Close() error {
	if r.cur != nil {
		_ = r.cur.Close()
		r.cur = nil
	}
	return nil
}
