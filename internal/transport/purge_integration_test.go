// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package transport

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/fspath"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/secrets"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport/faultfs"
)

// putAndDelete stores d/<name> through tgt, then deletes it and d.
func putAndDelete(t *testing.T, tgt *Target, name string) {
	t.Helper()
	if _, err := tgt.PutBytes(t.Context(), "d/"+name, []byte(name)); err != nil {
		t.Fatal(err)
	}
	if err := tgt.Remove(t.Context(), "d/"+name); err != nil {
		t.Fatal(err)
	}
	if err := tgt.Rmdir(t.Context(), "d"); err != nil {
		t.Fatal(err)
	}
	if _, err := tgt.Stat(t.Context(), "d/"+name); Classify(err) != ClassNotFound {
		t.Fatalf("d/%s still there: %v", name, err)
	}
}

func kinds(deletions []string) []string {
	out := make([]string, len(deletions))
	for i, d := range deletions {
		out[i], _, _ = strings.Cut(d, ":")
	}
	return out
}

// TestDeletionsBypassRecycleBin: deletions through a backend with a
// permanent-delete option use it, for plain and encrypted targets, unless
// the recycle bin is asked for, whatever the remote's own setting.
func TestDeletionsBypassRecycleBin(t *testing.T) {
	ctl := &faultfs.Controller{}
	name := fmt.Sprintf("pd%d", time.Now().UnixNano())
	faultfs.Register(name, ctl)
	testStorage.SetSection(name, map[string]string{"type": "faulty", "id": name, "remote": t.TempDir(), "hard_delete": "true"})
	loc := RepoLocation{Remote: name, Path: "pve backups"}
	base, err := OpenBase(t.Context(), loc)
	if err != nil {
		t.Fatal(err)
	}
	g, err := secrets.NewGeneration(1, "base32768", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	crypted, err := OpenCrypt(t.Context(), loc, g)
	if err != nil {
		t.Fatal(err)
	}

	putAndDelete(t, base, "a")
	putAndDelete(t, crypted, "b")
	if got := ctl.Deletions(); !slices.Equal(got[:2], []string{"permanent:d/a", "permanent:d"}) ||
		!slices.Equal(kinds(got), []string{"permanent", "permanent", "permanent", "permanent"}) {
		t.Fatalf("deletions = %v", got)
	}

	crypted.SetRecycleBin(true)
	putAndDelete(t, crypted, "c")
	crypted.SetRecycleBin(false)
	putAndDelete(t, crypted, "d")
	if got := kinds(ctl.Deletions()[4:]); !slices.Equal(got, []string{"recycled", "recycled", "permanent", "permanent"}) {
		t.Fatalf("deletions after switching = %v", ctl.Deletions())
	}
}

// TestDeletionsKeepTheirLocation: a target deletes from the folder it
// opened, even when the remote points elsewhere by the time its first
// deletion opens the permanent-delete twin.
func TestDeletionsKeepTheirLocation(t *testing.T) {
	ctl := &faultfs.Controller{}
	name := fmt.Sprintf("pl%d", time.Now().UnixNano())
	faultfs.Register(name, ctl)
	first, second := t.TempDir(), t.TempDir()
	testStorage.SetSection(name, map[string]string{"type": "faulty", "id": name, "remote": first})
	loc := RepoLocation{Remote: name, Path: "pve-backups"}
	g, err := secrets.NewGeneration(1, "base32768", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	open := func() (*Target, *Target) {
		t.Helper()
		base, err := OpenBase(t.Context(), loc)
		if err != nil {
			t.Fatal(err)
		}
		crypted, err := OpenCrypt(t.Context(), loc, g)
		if err != nil {
			t.Fatal(err)
		}
		for _, tgt := range []*Target{base, crypted} {
			if _, err := tgt.PutBytes(t.Context(), "d/a", []byte("a")); err != nil {
				t.Fatal(err)
			}
		}
		return base, crypted
	}
	base, crypted := open()
	testStorage.SetSection(name, map[string]string{"type": "faulty", "id": name, "remote": second})
	otherBase, otherCrypted := open()

	for _, tgt := range []*Target{base, crypted} {
		if err := tgt.Remove(t.Context(), "d/a"); err != nil {
			t.Fatal(err)
		}
		if _, err := tgt.Stat(t.Context(), "d/a"); Classify(err) != ClassNotFound {
			t.Fatalf("d/a still there: %v", err)
		}
	}
	for _, tgt := range []*Target{otherBase, otherCrypted} {
		if _, err := tgt.Stat(t.Context(), "d/a"); err != nil {
			t.Fatalf("d/a in the other folder: %v", err)
		}
	}
	if got := kinds(ctl.Deletions()); !slices.Equal(got, []string{"permanent", "permanent"}) {
		t.Fatalf("deletions = %v", ctl.Deletions())
	}
}

// TestRefusedPermanentDeletion: when the provider refuses permanent
// deletions, the deletion goes to the recycle bin instead, and so do later
// ones without asking again.
func TestRefusedPermanentDeletion(t *testing.T) {
	var refusals atomic.Int32
	ctl := &faultfs.Controller{HardDeleteError: func(string) error {
		refusals.Add(1)
		return onedriveError("accessDenied")
	}}
	_, base, _ := faultyRepo(t, ctl)
	putAndDelete(t, base, "a")
	putAndDelete(t, base, "b")
	if got := ctl.Deletions(); !slices.Equal(got, []string{"recycled:d/a", "recycled:d",
		"recycled:d/b", "recycled:d"}) || refusals.Load() != 1 {
		t.Fatalf("deletions = %v after %d refusals", got, refusals.Load())
	}
}

// TestFailedPermanentDeletion: other failures of a permanent deletion are
// reported as they are, and do not switch to the recycle bin.
func TestFailedPermanentDeletion(t *testing.T) {
	var throttled atomic.Bool
	throttled.Store(true)
	ctl := &faultfs.Controller{HardDeleteError: func(string) error {
		if throttled.Load() {
			return onedriveError("activityLimitReached")
		}
		return nil
	}}
	_, base, _ := faultyRepo(t, ctl)
	if _, err := base.PutBytes(t.Context(), "x", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := base.Remove(t.Context(), "x"); Classify(err) != ClassThrottled {
		t.Fatalf("Remove = %v (%s)", err, Classify(err))
	}
	if got := ctl.Deletions(); len(got) != 0 {
		t.Fatalf("deletions = %v", got)
	}
	throttled.Store(false)
	if err := base.Remove(t.Context(), "x"); err != nil {
		t.Fatal(err)
	}
	if got := ctl.Deletions(); !slices.Equal(got, []string{"permanent:x"}) {
		t.Fatalf("deletions = %v", got)
	}
}

func TestDeletionSpecs(t *testing.T) {
	od := fmt.Sprintf("od%d", time.Now().UnixNano())
	testStorage.SetSection(od, map[string]string{"type": "onedrive", "drive_id": "b!x,y", "drive_type": "business",
		"hard_delete": "true"})
	local := fmt.Sprintf("lo%d", time.Now().UnixNano())
	testStorage.SetSection(local, map[string]string{"type": "local"})

	recycle, permanent, ok := deletionSpecs(od + ":pve backups/g1")
	if !ok {
		t.Fatal("no specs for onedrive")
	}
	for spec, hardDelete := range map[string]string{recycle: "false", permanent: "true"} {
		p, err := fspath.Parse(spec)
		if err != nil {
			t.Fatal(err)
		}
		want := configmap.Simple{"region": "global", "tenant_url": "", "drive_id": "b!x,y", "drive_type": "business",
			"root_folder_id": "", "hard_delete": hardDelete}
		if p.Name != od || p.Path != "pve backups/g1" || !maps.Equal(p.Config, want) {
			t.Errorf("spec %s = %+v", spec, p)
		}
	}
	gd := fmt.Sprintf("gd%d", time.Now().UnixNano())
	testStorage.SetSection(gd, map[string]string{"type": "drive", "team_drive": "0Abc", "use_trash": "false"})
	recycle, permanent, ok = deletionSpecs(gd + ":pve backups")
	if !ok {
		t.Fatal("no specs for Google Drive")
	}
	// use_trash keeps deletions in the trash when true.
	for spec, useTrash := range map[string]string{recycle: "true", permanent: "false"} {
		p, err := fspath.Parse(spec)
		if err != nil {
			t.Fatal(err)
		}
		want := configmap.Simple{"root_folder_id": "", "team_drive": "0Abc", "shared_with_me": "false", "impersonate": "",
			"use_trash": useTrash}
		if p.Name != gd || p.Path != "pve backups" || !maps.Equal(p.Config, want) {
			t.Errorf("spec %s = %+v", spec, p)
		}
	}
	for _, remote := range []string{local + ":/srv/x", ":faulty,id=x,remote=/tmp:", "missing" + od + ":x", od} {
		if recycle, permanent, ok := deletionSpecs(remote); ok {
			t.Errorf("deletionSpecs(%q) = %q, %q", remote, recycle, permanent)
		}
	}
}
