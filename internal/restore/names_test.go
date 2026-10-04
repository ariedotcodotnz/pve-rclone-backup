// SPDX-License-Identifier: AGPL-3.0-or-later

package restore

import (
	"testing"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/manifest"
)

func TestArchiveName(t *testing.T) {
	src := func(name string) *source {
		m := &manifest.Manifest{}
		m.Backup.VMID, m.Backup.TSLabel = 100, "2026_10_04-02_00_01"
		m.Archive.Filename = name
		return &source{m: m}
	}
	name, base, err := src("vzdump-qemu-100-2026_10_04-02_00_01.vma.zst").archiveName()
	if err != nil || name != "vzdump-qemu-100-2026_10_04-02_00_01.vma.zst" || base != "vzdump-qemu-100-2026_10_04-02_00_01" {
		t.Fatalf("archiveName = %q %q %v", name, base, err)
	}
	for _, bad := range []string{
		"../vzdump-qemu-100-2026_10_04-02_00_01.vma.zst",
		"x/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst",
		".vzdump-qemu-100-2026_10_04-02_00_01.vma.zst",
		"vzdump-qemu-101-2026_10_04-02_00_01.vma.zst", // another guest
		"vzdump-qemu-100-2026_10_05-02_00_01.vma.zst", // another time
		"notes.txt",
		"",
	} {
		if _, _, err := src(bad).archiveName(); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if p, err := inside("/var/lib/vz/dump", "vzdump-qemu-100.vma"); err != nil || p != "/var/lib/vz/dump/vzdump-qemu-100.vma" {
		t.Fatalf("inside = %q %v", p, err)
	}
	for _, bad := range []string{"../x", "a/b", "..", "/etc/passwd"} {
		if _, err := inside("/var/lib/vz/dump", bad); err == nil {
			t.Errorf("inside accepted %q", bad)
		}
	}
}
