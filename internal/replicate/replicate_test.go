// SPDX-License-Identifier: AGPL-3.0-or-later

package replicate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/jobs"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/manifest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

func TestOpenSource(t *testing.T) {
	dir := t.TempDir()
	name := "vzdump-lxc-200-2026_10_04-02_00_01.tar.zst"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := openSource(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	if src.size != 7 || src.archive.VMID != 200 || src.basename() != "vzdump-lxc-200-2026_10_04-02_00_01" ||
		src.logPath() != filepath.Join(dir, "vzdump-lxc-200-2026_10_04-02_00_01.log") {
		t.Fatalf("source = %+v", src)
	}
	fi, _ := os.Stat(path)
	st := fi.Sys().(*syscall.Stat_t)
	dev, ino, size, mtime := int64(st.Dev), int64(st.Ino), int64(7), fi.ModTime().UnixNano()
	j := &store.Job{SourceDev: &dev, SourceIno: &ino, SourceSize: &size, SourceMtimeNs: &mtime}
	if err := src.matches(j); err != nil {
		t.Fatal(err)
	}
	other := mtime + 1
	if err := src.matches(&store.Job{SourceMtimeNs: &other}); !errors.Is(err, jobs.ErrSourceLost) {
		t.Fatalf("changed mtime: %v", err)
	}

	// Sidecars.
	if notes, prot, err := src.sidecars(); err != nil || notes != "" || prot {
		t.Fatalf("no sidecars: %q %v %v", notes, prot, err)
	}
	_ = os.WriteFile(path+".notes", []byte("web"), 0o600)
	_ = os.WriteFile(path+".protected", nil, 0o600)
	if notes, prot, err := src.sidecars(); err != nil || notes != "web" || !prot {
		t.Fatalf("sidecars: %q %v %v", notes, prot, err)
	}
	_ = os.Remove(path + ".notes")
	if err := os.Symlink("/etc/passwd", path+".notes"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := src.sidecars(); err == nil {
		t.Fatal("symlinked notes file read")
	}

	// Modified in place after opening.
	if err := src.unchanged(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("archive grown"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := src.unchanged(); !errors.Is(err, jobs.ErrSourceLost) {
		t.Fatalf("modified archive: %v", err)
	}

	// Refused: symlinks, missing files, other names.
	link := "vzdump-lxc-201-2026_10_04-02_00_01.tar.zst"
	if err := os.Symlink(path, filepath.Join(dir, link)); err != nil {
		t.Fatal(err)
	}
	if _, err := openSource(dir, link); !errors.Is(err, jobs.ErrSourceLost) {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := openSource(dir, "vzdump-lxc-202-2026_10_04-02_00_01.tar.zst"); !errors.Is(err, jobs.ErrSourceLost) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := openSource(dir, "../etc/passwd"); err == nil {
		t.Fatal("path outside the dump directory accepted")
	}
}

func ctArchive(t *testing.T, fw bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := []struct{ name, body string }{
		{"./etc/vzdump/pct.conf", "arch: amd64\nhostname: ct1\nmemory: 512\n"},
	}
	if fw {
		files = append(files, struct{ name, body string }{"./etc/vzdump/pct.fw", "[OPTIONS]\nenable: 1\n"})
	}
	files = append(files, struct{ name, body string }{"./etc/hostname", "ct1\n"})
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), ModTime: time.Unix(0, 0)}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func TestPVEToolsContainer(t *testing.T) {
	for _, tool := range []string{"tar", "gzip"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " not installed")
		}
	}
	a := layout.Archive{VMType: "lxc", VMID: 200, Format: "tar", Compression: "gz", Ext: "tgz"}
	data := ctArchive(t, true)
	gc, err := PVETools{}.Extract(t.Context(), a, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if gc.Config != "arch: amd64\nhostname: ct1\nmemory: 512\n" || gc.Firewall == nil || *gc.Firewall != "[OPTIONS]\nenable: 1\n" || !gc.FirewallKnown {
		t.Fatalf("config = %+v", gc)
	}
	if name := guestName("lxc", gc.Config); name != "ct1" {
		t.Fatalf("guest name = %q", name)
	}

	data = ctArchive(t, false)
	gc, err = PVETools{}.Extract(t.Context(), a, bytes.NewReader(data), int64(len(data)))
	if err != nil || gc.Firewall != nil || !gc.FirewallKnown {
		t.Fatalf("archive without firewall: %+v, %v", gc, err)
	}
	if _, err := (PVETools{Limit: 10}).Extract(t.Context(), a, bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatal("output limit not enforced")
	}
	garbage := []byte("not an archive at all")
	if _, err := (PVETools{}).Extract(t.Context(), a, bytes.NewReader(garbage), int64(len(garbage))); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestPVEToolsWithoutVMA(t *testing.T) {
	if _, err := exec.LookPath("vma"); err == nil {
		t.Skip("vma is installed")
	}
	a := layout.Archive{VMType: "qemu", VMID: 100, Format: "vma", Ext: "vma"}
	if _, err := (PVETools{}).Extract(t.Context(), a, bytes.NewReader([]byte("x")), 1); err == nil {
		t.Fatal("extraction without vma succeeded")
	}
}

func TestGuestName(t *testing.T) {
	cfg := "boot: order=scsi0\nname: web01\nmemory: 2048\n\n[snap]\nname: old\n"
	if got := guestName("qemu", cfg); got != "web01" {
		t.Fatalf("name = %q", got)
	}
	if got := guestName("qemu", "memory: 1\n[snap]\nname: old\n"); got != "" {
		t.Fatalf("snapshot name leaked: %q", got)
	}
}

func TestLoadIdentity(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadIdentity(dir)
	if err != nil || b.UUID != a.UUID {
		t.Fatalf("identity changed: %v %v %v", a, b, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pve-rclone-backup", "source.json"), []byte(`{"uuid":"bogus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentity(dir); err == nil {
		t.Fatal("invalid identity accepted")
	}
}

func TestSegmentCount(t *testing.T) {
	for _, c := range []struct {
		size, seg int64
		want      int
	}{{0, 10, 1}, {1, 10, 1}, {10, 10, 1}, {11, 10, 2}, {100, 10, 10}} {
		if got := segmentCount(c.size, c.seg); got != c.want {
			t.Errorf("segmentCount(%d, %d) = %d", c.size, c.seg, got)
		}
	}
}

func TestChooseSegmentSize(t *testing.T) {
	const mib = int64(1) << 20
	big := manifest.MaxSegments*64*mib + 1 // one segment too many at 64 MiB
	uploaded := []store.Segment{{Index: 0, State: "uploaded"}}
	pending := []store.Segment{{Index: 0, State: "pending"}}
	cases := []struct {
		name                         string
		segs                         []store.Segment
		planned, configured, archive int64
		want                         int64
	}{
		{"new upload", nil, 0, 1024 * mib, big, 1024 * mib},
		{"keeps its planned size", pending, 1024 * mib, 2048 * mib, big, 1024 * mib},
		// Refused earlier, nothing uploaded: the corrected size applies.
		{"refused size replaced", pending, 64 * mib, 128 * mib, big, 128 * mib},
		{"refused size, nothing planned", nil, 64 * mib, 128 * mib, big, 128 * mib},
		// Started before the limit existed: it continues as planned.
		{"upload under way", uploaded, 64 * mib, 128 * mib, big, 64 * mib},
	}
	for _, c := range cases {
		got, err := chooseSegmentSize(c.segs, c.planned, c.configured, c.archive)
		if err != nil || got != c.want {
			t.Errorf("%s: %d, %v; want %d", c.name, got, err, c.want)
		}
	}
	_, err := chooseSegmentSize(nil, 0, 64*mib, big)
	if !errors.Is(err, jobs.ErrPermanent) || !strings.Contains(err.Error(), "set rclone-segment-size to at least 65M") {
		t.Errorf("too many segments: %v", err)
	}
}
