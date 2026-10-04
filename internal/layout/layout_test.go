// SPDX-License-Identifier: AGPL-3.0-or-later

package layout

import "testing"

func TestBackupIDPaths(t *testing.T) {
	id := BackupID{Source: "homelab", VMType: "qemu", VMID: 100, TSLabel: "2026_10_04-02_00_01"}
	if err := id.Validate(); err != nil {
		t.Fatal(err)
	}
	if id.Dir() != "v1/homelab/qemu/100/2026_10_04-02_00_01" || id.Path(ManifestName) != id.Dir()+"/manifest.json" {
		t.Fatalf("dir = %s", id.Dir())
	}
	if v := id.Volname("vma.zst"); v != "backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst" {
		t.Fatalf("volname = %s", v)
	}
	id.Collision = 2
	if id.Dir() != "v1/homelab/qemu/100/2026_10_04-02_00_01.2" || id.Volname("vma.zst") != "backup/vzdump-qemu-100-2026_10_04-02_00_01.2.vma.zst" {
		t.Fatalf("collision paths %s %s", id.Dir(), id.Volname("vma.zst"))
	}
	back, err := ParseDir(id.Dir())
	if err != nil || back != id {
		t.Fatalf("ParseDir round trip: %+v %v", back, err)
	}
	a, err := ParseVolname(id.Volname("vma.zst"))
	if err != nil || a.ID("homelab") != id {
		t.Fatalf("ParseVolname round trip: %+v %v", a, err)
	}
}

func TestValidationRejectsHostileComponents(t *testing.T) {
	base := BackupID{Source: "homelab", VMType: "qemu", VMID: 100, TSLabel: "2026_10_04-02_00_01"}
	for name, mut := range map[string]func(*BackupID){
		"source traversal": func(id *BackupID) { id.Source = ".." },
		"source slash":     func(id *BackupID) { id.Source = "a/b" },
		"upper source":     func(id *BackupID) { id.Source = "Home" },
		"type":             func(id *BackupID) { id.VMType = "openvz" },
		"vmid low":         func(id *BackupID) { id.VMID = 99 },
		"ts":               func(id *BackupID) { id.TSLabel = "2026-10-04" },
		"collision":        func(id *BackupID) { id.Collision = 10000 },
	} {
		id := base
		mut(&id)
		if id.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	for _, dir := range []string{
		"v1/homelab/qemu/100", "v2/homelab/qemu/100/2026_10_04-02_00_01", "v1/homelab/qemu/0100/2026_10_04-02_00_01",
		"v1/homelab/qemu/100/2026_10_04-02_00_01.0", "v1/homelab/qemu/100/2026_10_04-02_00_01.01", "v1/../qemu/100/2026_10_04-02_00_01",
	} {
		if _, err := ParseDir(dir); err == nil {
			t.Errorf("ParseDir(%q) accepted", dir)
		}
	}
}

func TestParseArchiveName(t *testing.T) {
	cases := map[string]Archive{
		"vzdump-qemu-100-2026_10_04-02_00_01.vma.zst":   {VMType: "qemu", VMID: 100, TSLabel: "2026_10_04-02_00_01", Ext: "vma.zst", Format: "vma", Compression: "zst"},
		"vzdump-lxc-201-2026_10_04-03_00_01.tar":        {VMType: "lxc", VMID: 201, TSLabel: "2026_10_04-03_00_01", Ext: "tar", Format: "tar"},
		"vzdump-openvz-300-2020_01_01-00_00_00.tgz":     {VMType: "lxc", VMID: 300, TSLabel: "2020_01_01-00_00_00", Ext: "tgz", Format: "tar", Compression: "gz"},
		"vzdump-qemu-100-2026_10_04-02_00_01.3.vma.lzo": {VMType: "qemu", VMID: 100, TSLabel: "2026_10_04-02_00_01", Collision: 3, Ext: "vma.lzo", Format: "vma", Compression: "lzo"},
	}
	for name, want := range cases {
		got, err := ParseArchiveName(name)
		if err != nil || got != want {
			t.Errorf("%s: %+v %v", name, got, err)
		}
	}
	for _, bad := range []string{"vzdump-qemu-100-2026_10_04-02_00_01.dat", "vzdump-qemu-100-2026_10_04-02_00_01.vma.zst.notes",
		"vzdump-qemu-100-2026_10_04-02_00_01.log", "x-qemu-100-2026_10_04-02_00_01.vma", "vzdump-qemu-100-2026_10_04-02_00_01.vma.xz"} {
		if _, err := ParseArchiveName(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if n, ok := ParsePart("part.000042"); !ok || n != 42 {
		t.Fatal("ParsePart")
	}
	if _, ok := ParsePart("part.42"); ok {
		t.Fatal("short part name accepted")
	}
	if PartName(7) != "part.000007" {
		t.Fatal("PartName")
	}
}
