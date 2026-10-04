// SPDX-License-Identifier: AGPL-3.0-or-later

package transport

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"testing"
	"time"

	"github.com/rclone/rclone/backend/crypt"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
)

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWholeHashResumesAcrossSegments(t *testing.T) {
	data := randomBytes(t, 1<<20+17)
	want := sha256.Sum256(data)

	state := NewWholeHashState()
	for off := 0; off < len(data); off += 300_000 {
		h, err := restoreWholeHash(state)
		if err != nil {
			t.Fatal(err)
		}
		h.Write(data[off:min(off+300_000, len(data))])
		if state, err = saveWholeHash(h); err != nil {
			t.Fatal(err)
		}
	}
	got, err := WholeHashSum(state)
	if err != nil {
		t.Fatal(err)
	}
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("resumed hash %s != one-pass %x", got, want)
	}
}

func TestSegmentObjectOpenRanges(t *testing.T) {
	data := randomBytes(t, 1000)
	o := &segmentObject{seg: Segment{Source: bytes.NewReader(data), Offset: 100, Size: 500, ModTime: time.Unix(1, 0)}}
	part := data[100:600]

	tests := []struct {
		name string
		opts []fs.OpenOption
		want []byte
	}{
		{"full", nil, part},
		{"range", []fs.OpenOption{&fs.RangeOption{Start: 10, End: 19}}, part[10:20]},
		{"open-ended range", []fs.OpenOption{&fs.RangeOption{Start: 490, End: -1}}, part[490:]},
		{"suffix range", []fs.OpenOption{&fs.RangeOption{Start: -1, End: 5}}, part[495:]},
		{"seek", []fs.OpenOption{&fs.SeekOption{Offset: 250}}, part[250:]},
		{"seek past end", []fs.OpenOption{&fs.SeekOption{Offset: 900}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc, err := o.Open(t.Context(), tt.opts...)
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("got %d bytes, want %d", len(got), len(tt.want))
			}
		})
	}
}

func TestSegmentObjectHashesOnlyFullReads(t *testing.T) {
	data := randomBytes(t, 4096)
	o := &segmentObject{seg: Segment{Source: bytes.NewReader(data), Size: int64(len(data))}, wholeBase: NewWholeHashState()}

	// A ranged open must not produce a hashing attempt.
	rc, err := o.Open(t.Context(), &fs.RangeOption{Start: 0, End: 9})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(rc)
	if o.lastCompleteAttempt() != nil {
		t.Fatal("ranged read produced a hashing attempt")
	}

	// An aborted full read followed by a retry must hash each byte once.
	rc, _ = o.Open(t.Context())
	_, _ = io.CopyN(io.Discard, rc, 100)
	if o.lastCompleteAttempt() != nil {
		t.Fatal("partial read reported as complete")
	}
	rc, _ = o.Open(t.Context())
	_, _ = io.ReadAll(rc)
	a := o.lastCompleteAttempt()
	if a == nil {
		t.Fatal("full read not reported as complete")
	}
	want := sha256.Sum256(data)
	if got := a.seg.Sum(nil); !bytes.Equal(got, want[:]) {
		t.Fatal("segment hash mismatch after retry")
	}
	if got := a.whole.Sum(nil); !bytes.Equal(got, want[:]) {
		t.Fatal("whole-archive hash mismatch after retry")
	}
}

func TestCryptStoredSizeMatchesRclone(t *testing.T) {
	c, err := crypt.NewCipher(configmap.Simple{
		"password":            obscure.MustObscure("correct horse battery staple"),
		"filename_encryption": "standard",
		"filename_encoding":   "base32",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int64{0, 1, 65535, 65536, 65537, 1 << 20, 1<<30 + 123} {
		if got, want := CryptStoredSize(n), c.EncryptedSize(n); got != want {
			t.Errorf("CryptStoredSize(%d) = %d, rclone says %d", n, got, want)
		}
	}
}
