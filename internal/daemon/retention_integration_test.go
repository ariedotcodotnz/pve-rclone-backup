// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package daemon

import (
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/client"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/manifest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo/repotest"
)

func TestRetention(t *testing.T) {
	ctx := t.Context()
	r, _ := repotest.Init(t)
	day := func(d int) time.Time { return time.Date(2026, 10, d, 2, 0, 1, 0, time.Local) }
	for _, d := range []int{1, 2, 3} {
		repotest.Write(t, r, repotest.Backup{VMID: 100, Time: day(d), Size: 10})
	}
	repotest.Write(t, r, repotest.Backup{VMID: 200, VMType: "lxc", Time: day(1), Size: 10})
	e := newEnv(t)
	e.keys = repotest.Loader(r.Keys)
	section := strings.TrimSuffix(offsiteSection("offsite", r.Loc.Remote, "homelab"), "\n") +
		"\tprune-backups keep-last=1\n\trclone-min-age 0\n\trclone-keep-min 0\n\trclone-delete-grace 0\n"
	writeStorageCfg(t, e, localSection+section+"\n")
	stop := start(t, e)
	defer func() { _ = stop() }()
	c := client.New(e.socket)
	waitStorage(t, c, "offsite", resynced)

	marks := func(entries []apiv1.PruneEntry) string {
		var out []string
		for _, en := range entries {
			out = append(out, strings.TrimPrefix(en.Volid, "offsite:backup/vzdump-")+"="+en.Mark)
		}
		return strings.Join(out, " ")
	}
	var entries []apiv1.PruneEntry
	if err := c.Do(ctx, http.MethodPost, "/v1/storages/offsite/prune", apiv1.PruneRequest{DryRun: true}, &entries); err != nil {
		t.Fatal(err)
	}
	if got := marks(entries); got != "lxc-200-2026_10_01-02_00_01.tar.zst=keep qemu-100-2026_10_03-02_00_01.vma.zst=keep qemu-100-2026_10_02-02_00_01.vma.zst=remove qemu-100-2026_10_01-02_00_01.vma.zst=remove" {
		t.Fatalf("preview = %s", got)
	}
	// PVE's prune dialog passes its own keep options.
	vmid := 100
	if err := c.Do(ctx, http.MethodPost, "/v1/storages/offsite/prune", apiv1.PruneRequest{Keep: map[string]any{"keep-last": "2"}, VMID: &vmid, DryRun: true}, &entries); err != nil {
		t.Fatal(err)
	}
	if got := marks(entries); got != "qemu-100-2026_10_03-02_00_01.vma.zst=keep qemu-100-2026_10_02-02_00_01.vma.zst=keep qemu-100-2026_10_01-02_00_01.vma.zst=remove" {
		t.Fatalf("preview with keep-last=2 = %s", got)
	}
	if err := c.Do(ctx, http.MethodPost, "/v1/storages/offsite/prune", apiv1.PruneRequest{Keep: map[string]any{"keep-bogus": 1}, DryRun: true}, nil); !client.IsCode(err, apiv1.CodeInvalidArgument) {
		t.Fatalf("invalid keep options: %v", err)
	}

	// Apply: removed backups are tombstoned and, with no grace period,
	// deleted from the remote.
	if err := c.Do(ctx, http.MethodPost, "/v1/storages/offsite/retention", nil, &entries); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		scanned, err := r.Scan(ctx, "homelab")
		if err != nil {
			t.Fatal(err)
		}
		var list []apiv1.Backup
		_ = c.Do(ctx, http.MethodGet, "/v1/storages/offsite/backups?state=all", nil, &list)
		if len(scanned) == 2 && len(list) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not deleted: remote %d, catalogue %+v", len(scanned), list)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// A deletion interrupted by a crash is finished after the next resync.
	crashed := repotest.Backup{VMID: 300, Size: 10}
	repotest.Write(t, r, crashed)
	meta := manifest.NewMeta(time.Now())
	meta.Tombstone = &manifest.Tombstone{RequestedAt: time.Now(), DeleteAfter: time.Now(), Reason: "retention", State: "deleting"}
	if err := r.WriteMeta(ctx, 1, crashed.ID(), meta); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(ctx, http.MethodPost, "/v1/storages/offsite/resync", nil, nil); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(20 * time.Second)
	for {
		scanned, _ := r.Scan(ctx, "homelab")
		if !slices.ContainsFunc(scanned, func(b repo.ScannedBackup) bool { return b.ID.VMID == 300 }) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("interrupted deletion not finished")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// An immutable storage previews but never prunes.
	writeStorageCfg(t, e, localSection+section+"\trclone-immutable 1\n\trclone-replicate-from local\n\n")
	waitStorage(t, c, "offsite", func(s apiv1.Storage) bool { return len(s.ReplicateFrom) == 1 })
	if err := c.Do(ctx, http.MethodPost, "/v1/storages/offsite/prune", apiv1.PruneRequest{}, nil); !client.IsCode(err, apiv1.CodeImmutable) {
		t.Fatalf("prune on an immutable storage: %v", err)
	}
	if err := c.Do(ctx, http.MethodPost, "/v1/storages/offsite/prune", apiv1.PruneRequest{DryRun: true}, nil); err != nil {
		t.Fatalf("preview on an immutable storage: %v", err)
	}
}
