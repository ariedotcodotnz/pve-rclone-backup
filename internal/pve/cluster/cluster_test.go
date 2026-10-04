// SPDX-License-Identifier: AGPL-3.0-or-later

package cluster

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestVMListAndTags(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".vmlist"), `{"version":7,"ids":{
		"100":{"node":"pve1","type":"qemu","version":3},
		"200":{"node":"pve2","type":"lxc","version":1},
		"300":{"node":"pve1","type":"openvz"},
		"abc":{"node":"pve1","type":"qemu"},
		"400":{"node":"../etc","type":"qemu"}}}`)
	guests, err := VMList(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(guests) != 2 || guests[100].Node != "pve1" || guests[200].Type != "lxc" {
		t.Fatalf("guests = %+v", guests)
	}
	write(t, filepath.Join(dir, "nodes/pve1/qemu-server/100.conf"),
		"boot: order=scsi0\ntags: offsite;Web,db prod\nmemory: 2048\n\n[snap1]\ntags: old\n")
	write(t, filepath.Join(dir, "nodes/pve2/lxc/200.conf"), "hostname: ct\n[snap]\ntags: offsite\n")
	if tags, err := GuestTags(dir, guests[100]); err != nil || !slices.Equal(tags, []string{"offsite", "Web", "db", "prod"}) {
		t.Fatalf("tags = %v, %v", tags, err)
	}
	if tags, err := GuestTags(dir, guests[200]); err != nil || len(tags) != 0 {
		t.Fatalf("snapshot tags leaked: %v, %v", tags, err)
	}
	if _, err := GuestTags(dir, Guest{VMID: 999, Node: "pve1", Type: "qemu"}); !os.IsNotExist(err) {
		t.Fatalf("missing guest: %v", err)
	}
	if got := SplitTags(" a;;b, c\t$bad "); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("SplitTags = %v", got)
	}
}

func TestMembers(t *testing.T) {
	dir := t.TempDir()
	m, err := ReadMembers(dir, "pve2")
	if err != nil || m.Primary() != "pve2" {
		t.Fatalf("standalone = %+v, %v", m, err)
	}
	write(t, filepath.Join(dir, ".members"), `{"nodename":"pve2","version":9,"cluster":{"name":"lab","quorate":1},
		"nodelist":{"pve3":{"id":3,"online":1},"pve1":{"id":1,"online":0},"pve2":{"id":2,"online":1}}}`)
	m, err = ReadMembers(dir, "pve2")
	if err != nil || !slices.Equal(m.Online, []string{"pve2", "pve3"}) || m.Primary() != "pve2" {
		t.Fatalf("cluster = %+v, %v", m, err)
	}
	write(t, filepath.Join(dir, ".members"), `{"nodename":"pve2","version":1}`)
	if m, _ := ReadMembers(dir, "pve2"); !slices.Equal(m.Online, []string{"pve2"}) {
		t.Fatalf("standalone .members = %+v", m)
	}
}
