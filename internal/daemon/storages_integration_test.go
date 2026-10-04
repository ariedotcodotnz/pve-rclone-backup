// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package daemon

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/client"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/manifest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo/repotest"
)

func TestMain(m *testing.M) {
	cleanup, err := repotest.InitTransport()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

func writeStorageCfg(t *testing.T, e env, content string) {
	t.Helper()
	if err := os.MkdirAll(e.pveDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Write via rename so the manager sees a new inode, as with pmxcfs.
	tmp := filepath.Join(e.pveDir, ".storage.cfg.tmp")
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(e.pveDir, "storage.cfg")); err != nil {
		t.Fatal(err)
	}
}

func offsiteSection(id, remote, source string) string {
	return fmt.Sprintf("rclone-backup: %s\n\trclone-remote %s\n\trclone-path pve-backups\n\trclone-source %s\n\tcontent backup\n\tshared 1\n\n", id, remote, source)
}

const localSection = "dir: local\n\tpath /var/lib/vz\n\tcontent backup,iso\n\n"

// waitStorage polls a storage until cond holds.
func waitStorage(t *testing.T, c *client.Client, id string, cond func(apiv1.Storage) bool) apiv1.Storage {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var s apiv1.Storage
	var err error
	for time.Now().Before(deadline) {
		s = apiv1.Storage{}
		err = c.Do(t.Context(), http.MethodGet, "/v1/storages/"+id, nil, &s)
		if err == nil && cond(s) {
			return s
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("storage %s did not reach the expected state: %+v, %v", id, s, err)
	return s
}

func resynced(s apiv1.Storage) bool { return s.Health == apiv1.HealthOK && s.LastResyncAt != nil }

func TestStorageEndpoints(t *testing.T) {
	ctx := t.Context()
	r, _ := repotest.Init(t)
	const srcUUID = "1b2c3d4e-5f60-4a7b-8c9d-0e1f2a3b4c5d"
	if err := r.RegisterSource(ctx, "homelab", srcUUID, "", false); err != nil {
		t.Fatal(err)
	}
	day := func(d int) time.Time { return time.Date(2026, 10, d, 2, 0, 1, 0, time.UTC) }
	repotest.Write(t, r, repotest.Backup{VMID: 100, Time: day(1), Size: 70 << 10, GuestName: "web01",
		Config: "name: web01\nmemory: 2048\n", Notes: "nightly"})
	repotest.Write(t, r, repotest.Backup{VMID: 200, VMType: "lxc", Time: day(1), Size: 10})
	tomb := repotest.Backup{VMID: 100, Time: day(2), Size: 10}
	repotest.Write(t, r, tomb)
	meta, err := r.ReadMeta(ctx, 1, tomb.ID())
	if err != nil {
		t.Fatal(err)
	}
	meta.Tombstone = &manifest.Tombstone{RequestedAt: day(3), DeleteAfter: day(10), Reason: "user", State: "pending"}
	if err := r.WriteMeta(ctx, 1, tomb.ID(), meta); err != nil {
		t.Fatal(err)
	}
	damaged := repotest.Backup{VMID: 100, Time: day(3), Size: 70 << 10}
	repotest.Write(t, r, damaged)
	tgt, err := r.Target(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := tgt.Remove(ctx, damaged.ID().Path(layout.PartName(0))); err != nil {
		t.Fatal(err)
	}
	repotest.Write(t, r, repotest.Backup{Source: "office", VMID: 300, Size: 10})

	e := newEnv(t)
	e.keys = repotest.Loader(r.Keys)
	writeStorageCfg(t, e, localSection+offsiteSection("offsite", r.Loc.Remote, "homelab"))
	stop := start(t, e)
	defer func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	}()
	c := client.New(e.socket)

	s := waitStorage(t, c, "offsite", resynced)
	if !s.Active || s.RepoUUID != r.UUID() || s.Generation != 1 || s.Source != "homelab" || s.Encryption != "crypt" || !s.Enabled {
		t.Fatalf("storage = %+v", s)
	}
	var list []apiv1.Storage
	if err := c.Do(ctx, http.MethodGet, "/v1/storages", nil, &list); err != nil || len(list) != 1 || list[0].ID != "offsite" {
		t.Fatalf("storages = %+v, %v", list, err)
	}
	var st apiv1.StorageStatus
	if err := c.Do(ctx, http.MethodGet, "/v1/storages/offsite/status", nil, &st); err != nil || !st.Active || st.Health != apiv1.HealthOK {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if err := c.Do(ctx, http.MethodGet, "/v1/storages/nope/status", nil, &st); !client.IsCode(err, apiv1.CodeNotFound) {
		t.Fatalf("unknown storage status: %v", err)
	}

	backups := func(query string) []apiv1.Backup {
		t.Helper()
		var out []apiv1.Backup
		if err := c.Do(ctx, http.MethodGet, "/v1/storages/offsite/backups"+query, nil, &out); err != nil {
			t.Fatalf("list %s: %v", query, err)
		}
		return out
	}
	if got := backups(""); len(got) != 3 {
		t.Fatalf("default listing = %+v", got)
	}
	if got := backups("?state=all"); len(got) != 4 {
		t.Fatalf("full listing has %d entries", len(got))
	}
	if got := backups("?state=tombstoned"); len(got) != 1 || got[0].DeleteAfter == nil || !got[0].DeleteAfter.Equal(day(10)) {
		t.Fatalf("tombstoned listing = %+v", got)
	}
	if got := backups("?type=lxc"); len(got) != 1 || got[0].VMID != 200 || got[0].Format != "tar" {
		t.Fatalf("container listing = %+v", got)
	}
	got := backups("?vmid=100")
	if len(got) != 2 {
		t.Fatalf("guest listing = %+v", got)
	}
	first := got[0]
	if first.Volname != "backup/vzdump-qemu-100-2026_10_01-02_00_01.vma.zst" || first.CTime != day(1).Unix() ||
		first.Size != 70<<10 || first.Notes != "nightly" || first.State != "complete" || first.VerifyLevel != 2 || first.GuestName != "web01" {
		t.Fatalf("first backup = %+v", first)
	}
	if got[1].State != "damaged" {
		t.Fatalf("second backup = %+v", got[1])
	}
	for _, q := range []string{"?vmid=abc", "?type=openvz", "?state=bogus"} {
		if err := c.Do(ctx, http.MethodGet, "/v1/storages/offsite/backups"+q, nil, nil); !client.IsCode(err, apiv1.CodeInvalidArgument) {
			t.Errorf("list %s: %v", q, err)
		}
	}

	var detail apiv1.BackupDetail
	if err := c.Do(ctx, http.MethodGet, "/v1/storages/offsite/backups/vzdump-qemu-100-2026_10_01-02_00_01.vma.zst", nil, &detail); err != nil {
		t.Fatal(err)
	}
	if m, err := manifest.DecodeManifest(detail.Manifest); err != nil || m.Archive.Size != 70<<10 || detail.Notes != "nightly" {
		t.Fatalf("detail = %+v, %v", detail.Backup, err)
	}
	var gc apiv1.GuestConfig
	if err := c.Do(ctx, http.MethodGet, "/v1/storages/offsite/backups/vzdump-qemu-100-2026_10_01-02_00_01.vma.zst/config", nil, &gc); err != nil ||
		gc.Config != "name: web01\nmemory: 2048\n" || !gc.FirewallKnown || gc.Firewall != nil {
		t.Fatalf("config = %+v, %v", gc, err)
	}
	if err := c.Do(ctx, http.MethodGet, "/v1/storages/offsite/backups/vzdump-qemu-100-2026_10_09-02_00_01.vma.zst", nil, nil); !client.IsCode(err, apiv1.CodeNotFound) {
		t.Fatalf("missing backup: %v", err)
	}
	if err := c.Do(ctx, http.MethodGet, "/v1/storages/offsite/backups/..%2Fetc%2Fpasswd", nil, nil); !client.IsCode(err, apiv1.CodeInvalidArgument) {
		t.Fatalf("hostile backup name: %v", err)
	}

	// Validation, as called by PVE's storage hooks with its parsed config.
	validate := func(id string, req apiv1.ValidateRequest) (*apiv1.ValidateResponse, error) {
		var resp apiv1.ValidateResponse
		err := c.Do(ctx, http.MethodPost, "/v1/storages/"+id+"/validate", req, &resp)
		return &resp, err
	}
	pveConfig := func(remote, path string, extra map[string]any) map[string]any {
		cfg := map[string]any{"type": "rclone-backup", "rclone-remote": remote, "rclone-path": path,
			"rclone-source": "dr", "content": map[string]any{"backup": 1}, "shared": 1}
		for k, v := range extra {
			cfg[k] = v
		}
		return cfg
	}
	resp, err := validate("offsite-dr", apiv1.ValidateRequest{Config: pveConfig(r.Loc.Remote, "pve-backups",
		map[string]any{"rclone-replicate-from": "local", "prune-backups": map[string]any{"keep-last": 3}})})
	if err != nil || len(resp.Warnings) != 0 {
		t.Fatalf("validate a storage on the known repository: %+v, %v", resp, err)
	}
	if resp, err := validate("offsite-dr", apiv1.ValidateRequest{Config: pveConfig(r.Loc.Remote, "pve-backups", nil)}); err != nil || len(resp.Warnings) != 2 {
		t.Fatalf("read-only storage without retention: %+v, %v", resp, err)
	}
	for name, cfg := range map[string]map[string]any{
		"unknown remote":         pveConfig("nosuchremote", "pve-backups", nil),
		"missing source storage": pveConfig(r.Loc.Remote, "pve-backups", map[string]any{"rclone-replicate-from": "nfs1"}),
		"self replication":       pveConfig(r.Loc.Remote, "pve-backups", map[string]any{"rclone-replicate-from": "offsite-dr"}),
		"offsite as source":      pveConfig(r.Loc.Remote, "pve-backups", map[string]any{"rclone-replicate-from": "offsite"}),
		"bad property":           pveConfig(r.Loc.Remote, "pve-backups", map[string]any{"rclone-transfers": 99}),
		"tiny segments":          pveConfig(r.Loc.Remote, "pve-backups", map[string]any{"rclone-segment-size": "1M"}),
		"wrong encryption":       pveConfig(r.Loc.Remote, "pve-backups", map[string]any{"rclone-encryption": "none"}),
	} {
		if _, err := validate("offsite-dr", apiv1.ValidateRequest{Config: cfg}); !client.IsCode(err, apiv1.CodeInvalidArgument) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := validate("offsite-dr", apiv1.ValidateRequest{Config: pveConfig(r.Loc.Remote, "elsewhere", nil)}); !client.IsCode(err, apiv1.CodePreconditionFailed) ||
		!strings.Contains(err.Error(), "storage init") {
		t.Fatalf("unknown repository: %v", err)
	}
	// Updates pass PVE's stored section plus the changes; keeping the
	// binding needs no known repository.
	stored := pveConfig(r.Loc.Remote, "pve-backups", nil)
	stored["rclone-source"] = "homelab"
	if _, err := validate("offsite", apiv1.ValidateRequest{Config: stored,
		Update: map[string]any{"rclone-keep-min": 2}, Delete: []any{"rclone-tags"}}); err != nil {
		t.Fatalf("update: %v", err)
	}

	// Changing the source rebinds the storage: its catalogue is rebuilt
	// for the new namespace.
	writeStorageCfg(t, e, localSection+offsiteSection("offsite", r.Loc.Remote, "office"))
	waitStorage(t, c, "offsite", func(s apiv1.Storage) bool { return s.Source == "office" && resynced(s) })
	if got := backups("?state=all"); len(got) != 1 || got[0].VMID != 300 {
		t.Fatalf("catalogue after rebinding = %+v", got)
	}

	// Removing the storage drops it and its catalogue.
	writeStorageCfg(t, e, localSection)
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := c.Do(ctx, http.MethodGet, "/v1/storages/offsite", nil, nil)
		if client.IsCode(err, apiv1.CodeNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("removed storage still served: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestStorageHealth(t *testing.T) {
	ctx := t.Context()
	good, _ := repotest.Init(t)
	other, _ := repotest.Init(t)
	emptyLoc, _ := repotest.Remote(t, nil)

	e := newEnv(t)
	e.keys = repotest.Loader(good.Keys) // no keys for "other"
	writeStorageCfg(t, e, localSection+
		offsiteSection("good", good.Loc.Remote, "homelab")+
		offsiteSection("nokeys", other.Loc.Remote, "homelab")+
		offsiteSection("empty", emptyLoc.Remote, "homelab")+
		offsiteSection("noremote", "missing", "homelab")+
		strings.TrimSuffix(offsiteSection("elsewhere", good.Loc.Remote, "homelab"), "\n")+"\tnodes other-node\n\n")
	stop := start(t, e)
	defer func() { _ = stop() }()
	c := client.New(e.socket)

	waitStorage(t, c, "good", resynced)
	nokeys := waitStorage(t, c, "nokeys", func(s apiv1.Storage) bool { return s.Health == apiv1.HealthMisconfigured })
	if nokeys.Active || !strings.Contains(nokeys.HealthDetail, "recovery kit") {
		t.Fatalf("storage without keys = %+v", nokeys)
	}
	empty := waitStorage(t, c, "empty", func(s apiv1.Storage) bool { return s.Health == apiv1.HealthUninitialized })
	if empty.Active {
		t.Fatalf("uninitialized storage active: %+v", empty)
	}
	if s := waitStorage(t, c, "noremote", func(s apiv1.Storage) bool { return s.Health == apiv1.HealthMisconfigured }); !strings.Contains(s.HealthDetail, "not configured") {
		t.Fatalf("storage with a missing remote = %+v", s)
	}
	var st apiv1.StorageStatus
	if err := c.Do(ctx, http.MethodGet, "/v1/storages/elsewhere/status", nil, &st); err != nil || st.Active || st.Health != apiv1.HealthDisabled {
		t.Fatalf("storage of another node: %+v, %v", st, err)
	}

	// A restarted daemon serves the cached catalogue before the remote is
	// reachable again: the storage stays active while it reopens.
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	repotest.Storage.SetSection(good.Loc.Remote, map[string]string{"type": "faulty", "id": "unregistered", "remote": "/nonexistent"})
	stop = start(t, e)
	s := waitStorage(t, c, "good", func(s apiv1.Storage) bool { return s.Health != apiv1.HealthOpening })
	if !s.Active || s.RepoUUID != good.UUID() || s.LastResyncAt == nil {
		t.Fatalf("storage with an unreachable remote after restart = %+v", s)
	}
}
