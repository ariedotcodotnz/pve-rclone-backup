// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package daemon

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/client"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/manifest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo/repotest"
)

func TestBackupMutations(t *testing.T) {
	ctx := t.Context()
	r, _ := repotest.Init(t)
	b := repotest.Backup{VMID: 100, Size: 10, Notes: "nightly"}
	repotest.Write(t, r, b)
	e := newEnv(t)
	e.keys = repotest.Loader(r.Keys)
	writeStorageCfg(t, e, localSection+strings.TrimSuffix(offsiteSection("offsite", r.Loc.Remote, "homelab"), "\n")+"\trclone-delete-grace 3d\n\n")
	stop := start(t, e)
	defer func() { _ = stop() }()
	c := client.New(e.socket)
	waitStorage(t, c, "offsite", resynced)
	path := "/v1/storages/offsite/backups/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst"

	// waitMeta polls the remote meta document until cond holds.
	waitMeta := func(cond func(m *manifest.Meta) bool) *manifest.Meta {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			m, err := r.ReadMeta(ctx, 1, b.ID())
			if err == nil && cond(m) {
				return m
			}
			if time.Now().After(deadline) {
				t.Fatalf("remote meta = %+v, %v", m, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	notes, prot := "keep for audit", true
	var got apiv1.Backup
	if err := c.Do(ctx, http.MethodPatch, path, apiv1.BackupUpdate{Notes: &notes, Protected: &prot}, &got); err != nil || got.Notes != notes || !got.Protected {
		t.Fatalf("patch = %+v, %v", got, err)
	}
	waitMeta(func(m *manifest.Meta) bool { return m.Notes == notes && m.Protected })
	if err := c.Do(ctx, http.MethodDelete, path, nil, nil); !client.IsCode(err, apiv1.CodeProtected) {
		t.Fatalf("delete of a protected backup: %v", err)
	}

	prot = false
	if err := c.Do(ctx, http.MethodPatch, path, apiv1.BackupUpdate{Protected: &prot}, nil); err != nil {
		t.Fatal(err)
	}
	got = apiv1.Backup{}
	if err := c.Do(ctx, http.MethodDelete, path, nil, &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "tombstoned" || got.DeleteAfter == nil || time.Until(*got.DeleteAfter) < 71*time.Hour {
		t.Fatalf("tombstoned = %+v", got)
	}
	m := waitMeta(func(m *manifest.Meta) bool { return m.Tombstone != nil })
	if m.Tombstone.State != "pending" || m.Tombstone.Reason != "user" || !strings.HasPrefix(m.Tombstone.By, "uid ") || m.Notes != notes {
		t.Fatalf("remote tombstone = %+v", m.Tombstone)
	}
	var list []apiv1.Backup
	if err := c.Do(ctx, http.MethodGet, "/v1/storages/offsite/backups", nil, &list); err != nil || len(list) != 0 {
		t.Fatalf("PVE listing still shows the deleted backup: %+v, %v", list, err)
	}
	if err := c.Do(ctx, http.MethodDelete, path, nil, nil); err != nil {
		t.Fatalf("repeated delete: %v", err)
	}
	if err := c.Do(ctx, http.MethodDelete, path+"?immediate=1", nil, nil); !client.IsCode(err, apiv1.CodeInvalidArgument) {
		t.Fatalf("immediate delete: %v", err)
	}

	got = apiv1.Backup{}
	if err := c.Do(ctx, http.MethodPost, path+"/undelete", nil, &got); err != nil || got.State != "complete" || got.DeleteAfter != nil {
		t.Fatalf("undelete = %+v, %v", got, err)
	}
	waitMeta(func(m *manifest.Meta) bool { return m.Tombstone == nil })
	if err := c.Do(ctx, http.MethodPost, path+"/undelete", nil, nil); !client.IsCode(err, apiv1.CodeConflict) {
		t.Fatalf("undelete of a live backup: %v", err)
	}
	long := strings.Repeat("x", maxNotes+1)
	if err := c.Do(ctx, http.MethodPatch, path, apiv1.BackupUpdate{Notes: &long}, nil); !client.IsCode(err, apiv1.CodeInvalidArgument) {
		t.Fatalf("oversized notes: %v", err)
	}

	// An immutable storage refuses deletion. The replication source is
	// changed too, to see when the daemon has read the new configuration.
	writeStorageCfg(t, e, localSection+strings.TrimSuffix(offsiteSection("offsite", r.Loc.Remote, "homelab"), "\n")+
		"\trclone-immutable 1\n\trclone-replicate-from local\n\n")
	waitStorage(t, c, "offsite", func(s apiv1.Storage) bool { return len(s.ReplicateFrom) == 1 })
	if err := c.Do(ctx, http.MethodDelete, path, nil, nil); !client.IsCode(err, apiv1.CodeImmutable) {
		t.Fatalf("delete on an immutable storage: %v", err)
	}
}

// TestNewerMetaIsNotOverwritten: in a cluster being upgraded, a meta
// document written by a newer daemon must not be replaced by this daemon's
// pending change, which cannot carry over what it does not understand.
func TestNewerMetaIsNotOverwritten(t *testing.T) {
	ctx := t.Context()
	r, _ := repotest.Init(t)
	b := repotest.Backup{VMID: 100, Size: 10}
	repotest.Write(t, r, b)
	e := newEnv(t)
	e.keys = repotest.Loader(r.Keys)
	writeStorageCfg(t, e, localSection+offsiteSection("offsite", r.Loc.Remote, "homelab"))
	stop := start(t, e)
	defer func() { _ = stop() }()
	c := client.New(e.socket)
	waitStorage(t, c, "offsite", resynced)

	tgt, err := r.Target(1)
	if err != nil {
		t.Fatal(err)
	}
	newer := []byte(`{"format":"pve-rclone-backup.meta","version":2,"notes":"from a newer daemon"}`)
	if _, err := tgt.PutBytes(ctx, b.ID().Path(layout.MetaName), newer); err != nil {
		t.Fatal(err)
	}
	notes := "changed on an older node"
	path := "/v1/storages/offsite/backups/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst"
	if err := c.Do(ctx, http.MethodPatch, path, apiv1.BackupUpdate{Notes: &notes}, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second) // the change is pushed right away, if at all
	got, err := tgt.ReadAll(ctx, b.ID().Path(layout.MetaName), 1<<20)
	if err != nil || !bytes.Equal(got, newer) {
		t.Fatalf("newer meta document replaced: %s, %v", got, err)
	}
}
