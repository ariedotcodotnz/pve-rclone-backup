// SPDX-License-Identifier: AGPL-3.0-or-later

package transport

import (
	"strings"
	"testing"
	"unicode/utf16"
)

func TestEncryptedPathLengths(t *testing.T) {
	rel := "v1/homelab/qemu/100/2026_10_04-02_00_01/part.000000"
	lengths := map[string]int{}
	for _, enc := range []string{"base32", "base64", "base32768"} {
		p, err := EncryptedPath(rel, enc, true)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(p, "/") != strings.Count(rel, "/") || strings.Contains(p, "homelab") {
			t.Fatalf("%s: %q does not look encrypted", enc, p)
		}
		lengths[enc] = len(utf16.Encode([]rune(p)))
	}
	if lengths["base32768"] >= lengths["base64"] || lengths["base64"] >= lengths["base32"] {
		t.Fatalf("unexpected UTF-16 lengths %v", lengths)
	}
}

func TestOneDrivePathBudget(t *testing.T) {
	od, err := ProfileFor("onedrive")
	if err != nil {
		t.Fatal(err)
	}
	rel := "v1/homelab/qemu/100/2026_10_04-02_00_01/part.000000"
	if err := od.CheckPath("pve-backups/g1", rel, "base32768"); err != nil {
		t.Fatalf("typical path rejected: %v", err)
	}
	long := "v1/" + strings.Repeat("s", 32) + "/qemu/999999999/2026_10_04-02_00_01.12/" + strings.Repeat("x", 200)
	if err := od.CheckPath(strings.Repeat("deep/", 30)+"g1", long, "base32"); err == nil {
		t.Fatal("over-long path accepted")
	}
	if err := od.CheckSegmentSize(1<<30, true); err != nil {
		t.Fatal(err)
	}
	if err := od.CheckSegmentSize(250<<30, true); err == nil {
		t.Fatal("segment above the 250 GiB object limit accepted once encrypted")
	}
	if _, err := ProfileFor("ftp"); err == nil {
		t.Fatal("unprofiled backend accepted")
	}
	local, _ := ProfileFor("local")
	if err := local.CheckPath("/x", strings.Repeat("n", 300), ""); err == nil {
		t.Fatal("over-long local name accepted")
	}
}
