// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/obscure"
)

var testStorage = NewMemoryStorage()

func TestMain(m *testing.M) {
	cfgDir, err := os.MkdirTemp("", "pve-rclone-backup-transport-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	err = Init(Options{
		ConfigPath:      filepath.Join(cfgDir, "remotes.conf"),
		Storage:         testStorage,
		LowLevelRetries: 2,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(cfgDir)
	os.Exit(code)
}

// newCryptTarget configures a local base remote plus a crypt remote on top
// of it and returns the crypt target, the base target and the base path.
func newCryptTarget(t *testing.T, encoding string) (crypted, base *Target, dir string) {
	t.Helper()
	dir = t.TempDir()
	name := fmt.Sprintf("t%d", time.Now().UnixNano())
	testStorage.SetSection(name+"-base", map[string]string{"type": "local"})
	testStorage.SetSection(name+"-crypt", map[string]string{
		"type":                      "crypt",
		"remote":                    name + "-base:" + filepath.Join(dir, "g1"),
		"password":                  obscure.MustObscure("test password one"),
		"password2":                 obscure.MustObscure("test salt two"),
		"filename_encryption":       "standard",
		"directory_name_encryption": "true",
		"filename_encoding":         encoding,
	})
	var err error
	if crypted, err = NewTarget(t.Context(), name+"-crypt:"); err != nil {
		t.Fatal(err)
	}
	if base, err = NewTarget(t.Context(), name+"-base:"+filepath.Join(dir, "g1")); err != nil {
		t.Fatal(err)
	}
	return crypted, base, dir
}

func writeSparseSource(t *testing.T, size int64) (*os.File, []byte) {
	t.Helper()
	data := make([]byte, size)
	// Random islands separated by holes, like a VM image with free space.
	for off := int64(0); off < size; off += 1 << 20 {
		copy(data[off:], randomBytes(t, int(min(256<<10, size-off))))
	}
	f, err := os.Create(filepath.Join(t.TempDir(), "vzdump-qemu-100-2026_10_04-02_00_01.vma.zst"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, data
}

type uploaded struct {
	remote string
	seg    Segment
	res    *SegmentResult
}

func uploadAll(t *testing.T, tgt *Target, src io.ReaderAt, size, segSize int64) ([]uploaded, []byte) {
	t.Helper()
	var out []uploaded
	state := NewWholeHashState()
	count := max((size+segSize-1)/segSize, 1)
	for i := range count {
		off := i * segSize
		seg := Segment{Source: src, Offset: off, Size: min(segSize, size-off), ModTime: time.Unix(1790000000, 0)}
		remote := fmt.Sprintf("v1/homelab/qemu/100/2026_10_04-02_00_01/part.%06d", i)
		res, err := tgt.PutSegment(t.Context(), remote, seg, state)
		if err != nil {
			t.Fatalf("segment %d: %v", i, err)
		}
		state = res.WholeState
		out = append(out, uploaded{remote, seg, res})
	}
	return out, state
}

func TestSegmentRoundTripThroughCrypt(t *testing.T) {
	for _, encoding := range []string{"base32", "base64", "base32768"} {
		t.Run(encoding, func(t *testing.T) {
			// Any spooling to a temporary file would show up here.
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)

			tgt, base, dir := newCryptTarget(t, encoding)
			if !tgt.Encrypted() {
				t.Fatal("target not encrypted")
			}
			const size, segSize = 10<<20 + 12345, 3 << 20
			src, data := writeSparseSource(t, size)

			parts, state := uploadAll(t, tgt, src, size, segSize)
			if len(parts) != 4 {
				t.Fatalf("got %d segments, want 4", len(parts))
			}

			whole, err := WholeHashSum(state)
			if err != nil {
				t.Fatal(err)
			}
			if want := sha256.Sum256(data); whole != hex.EncodeToString(want[:]) {
				t.Fatalf("whole-archive hash %s != %x", whole, want)
			}

			var reassembled bytes.Buffer
			for _, p := range parts {
				want := sha256.Sum256(data[p.seg.Offset : p.seg.Offset+p.seg.Size])
				if p.res.SHA256 != hex.EncodeToString(want[:]) {
					t.Errorf("%s: segment hash mismatch", p.remote)
				}
				if p.res.StoredSize != CryptStoredSize(p.seg.Size) {
					t.Errorf("%s: stored size %d, want %d", p.remote, p.res.StoredSize, CryptStoredSize(p.seg.Size))
				}
				if p.res.StoredHash == "" || p.res.StoredHashType == "" {
					t.Errorf("%s: no provider hash recorded", p.remote)
				}

				info, err := tgt.Stat(t.Context(), p.remote)
				if err != nil {
					t.Fatal(err)
				}
				if info.Size != p.seg.Size || info.StoredHash != p.res.StoredHash {
					t.Errorf("%s: stat %+v disagrees with upload result %+v", p.remote, info, p.res)
				}

				ok, err := tgt.VerifySegment(t.Context(), p.remote, p.seg)
				if err != nil || !ok {
					t.Errorf("%s: VerifySegment = %v, %v", p.remote, ok, err)
				}

				rc, err := tgt.Open(t.Context(), p.remote)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := io.Copy(&reassembled, rc); err != nil {
					t.Fatal(err)
				}
				_ = rc.Close()
			}
			if !bytes.Equal(reassembled.Bytes(), data) {
				t.Fatal("decrypted reassembly differs from source")
			}

			// Names must be encrypted on the base remote.
			entries, err := base.f.List(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Remote() == "v1" {
				t.Fatalf("base listing %v does not look encrypted", entries)
			}

			if leftovers, _ := os.ReadDir(tmp); len(leftovers) != 0 {
				t.Fatalf("rclone left temporary files: %v", leftovers)
			}
			_ = dir
		})
	}
}

func TestVerifySegmentDetectsDifferentContent(t *testing.T) {
	tgt, _, _ := newCryptTarget(t, "base32")
	src, data := writeSparseSource(t, 1<<20)
	parts, _ := uploadAll(t, tgt, src, int64(len(data)), 1<<20)

	other := bytes.Clone(data)
	other[12345] ^= 0xff
	seg := parts[0].seg
	seg.Source = bytes.NewReader(other)
	ok, err := tgt.VerifySegment(t.Context(), parts[0].remote, seg)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("VerifySegment accepted different content")
	}
}

// failingReaderAt fails reads that touch failAt while armed. With once set
// it disarms itself after the first failure (a transient fault).
type failingReaderAt struct {
	r        io.ReaderAt
	failAt   int64
	once     bool
	armed    atomic.Bool
	failures atomic.Int64
}

var errInjected = errors.New("injected read failure")

func (f *failingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off <= f.failAt && f.failAt < off+int64(len(p)) && f.armed.Load() {
		if !f.once || f.armed.CompareAndSwap(true, false) {
			f.failures.Add(1)
			return 0, errInjected
		}
	}
	return f.r.ReadAt(p, off)
}

func TestTransientFailureIsRetriedWithoutDoubleHashing(t *testing.T) {
	tgt, _, _ := newCryptTarget(t, "base32")
	const size = 3<<20 + 7
	file, data := writeSparseSource(t, size)
	src := &failingReaderAt{r: file, failAt: 2<<20 + 5, once: true}
	src.armed.Store(true)

	parts, state := uploadAll(t, tgt, src, size, size)
	if src.failures.Load() != 1 {
		t.Fatalf("fault fired %d times, want 1", src.failures.Load())
	}
	want := sha256.Sum256(data)
	if parts[0].res.SHA256 != hex.EncodeToString(want[:]) {
		t.Fatal("segment hash wrong after low-level retry")
	}
	if whole, _ := WholeHashSum(state); whole != hex.EncodeToString(want[:]) {
		t.Fatal("whole-archive hash wrong after low-level retry")
	}
}

func TestResumeAfterInjectedFailure(t *testing.T) {
	tgt, _, _ := newCryptTarget(t, "base32768")
	const size, segSize = 7<<20 + 1, 2 << 20
	file, data := writeSparseSource(t, size)
	src := &failingReaderAt{r: file, failAt: 5<<20 + 100} // inside segment 2, persistent
	src.armed.Store(true)

	state := NewWholeHashState()
	var done []string
	failed := -1
	for i := range int64(4) {
		off := i * segSize
		seg := Segment{Source: src, Offset: off, Size: min(segSize, size-off), ModTime: time.Unix(1, 0)}
		remote := fmt.Sprintf("job/part.%06d", i)
		res, err := tgt.PutSegment(t.Context(), remote, seg, state)
		if err != nil {
			if !errors.Is(err, errInjected) {
				t.Fatalf("segment %d: unexpected error %v", i, err)
			}
			failed = int(i)
			break
		}
		state = res.WholeState
		done = append(done, remote)
	}
	if failed != 2 {
		t.Fatalf("expected failure in segment 2, got %d", failed)
	}
	if _, err := tgt.Stat(t.Context(), "job/part.000002"); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatalf("failed segment left an object behind: %v", err)
	}

	// Resume: completed segments are verified in place, never re-uploaded.
	for i, remote := range done {
		off := int64(i) * segSize
		ok, err := tgt.VerifySegment(t.Context(), remote, Segment{Source: file, Offset: off, Size: segSize})
		if err != nil || !ok {
			t.Fatalf("segment %d does not verify after failure: %v %v", i, ok, err)
		}
	}
	src.armed.Store(false)
	for i := int64(failed); i < 4; i++ {
		off := i * segSize
		seg := Segment{Source: src, Offset: off, Size: min(segSize, size-off), ModTime: time.Unix(1, 0)}
		res, err := tgt.PutSegment(t.Context(), fmt.Sprintf("job/part.%06d", i), seg, state)
		if err != nil {
			t.Fatalf("resumed segment %d: %v", i, err)
		}
		state = res.WholeState
	}
	whole, err := WholeHashSum(state)
	if err != nil {
		t.Fatal(err)
	}
	if want := sha256.Sum256(data); whole != hex.EncodeToString(want[:]) {
		t.Fatal("whole-archive hash wrong after resume")
	}
}

func TestEmptyArchiveIsOneEmptySegment(t *testing.T) {
	tgt, _, _ := newCryptTarget(t, "base32")
	parts, state := uploadAll(t, tgt, bytes.NewReader(nil), 0, 1<<20)
	if len(parts) != 1 || parts[0].res.StoredSize != CryptStoredSize(0) {
		t.Fatalf("unexpected empty-archive result %+v", parts)
	}
	whole, _ := WholeHashSum(state)
	if want := sha256.Sum256(nil); whole != hex.EncodeToString(want[:]) {
		t.Fatal("empty archive hash mismatch")
	}
}

func TestPlainTargetVerifiesProviderHash(t *testing.T) {
	dir := t.TempDir()
	name := fmt.Sprintf("plain%d", time.Now().UnixNano())
	testStorage.SetSection(name, map[string]string{"type": "local"})
	tgt, err := NewTarget(context.Background(), name+":"+dir)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Encrypted() {
		t.Fatal("plain target reports encryption")
	}
	src, data := writeSparseSource(t, 3<<20)
	parts, _ := uploadAll(t, tgt, src, int64(len(data)), 1<<20)
	for _, p := range parts {
		if p.res.StoredHash == "" {
			t.Fatalf("%s: no provider hash", p.remote)
		}
		ok, err := tgt.VerifySegment(t.Context(), p.remote, p.seg)
		if err != nil || !ok {
			t.Fatalf("%s: VerifySegment = %v, %v", p.remote, ok, err)
		}
	}
}
