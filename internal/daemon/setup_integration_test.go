// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package daemon

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/client"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/pve/cfs"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/recoverykit"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo/repotest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/secrets"
)

func withKeyStore(t *testing.T, e env) env {
	t.Helper()
	dir := t.TempDir()
	e.keyStore = secrets.NewKeyStore(filepath.Join(dir, "secrets"), &cfs.Locker{Dir: filepath.Join(dir, "lock")})
	return e
}

func TestStorageInitAndRecoveryKit(t *testing.T) {
	ctx := t.Context()
	loc, _ := repotest.Remote(t, nil)
	e := withKeyStore(t, newEnv(t))
	dump := filepath.Join(t.TempDir(), "dump")
	if err := os.MkdirAll(dump, 0o700); err != nil {
		t.Fatal(err)
	}
	writeStorageCfg(t, e, fmt.Sprintf("dir: backups\n\tpath %s\n\tcontent backup\n\n", filepath.Dir(dump)))
	stop := start(t, e)
	defer func() { _ = stop() }()
	c := client.New(e.socket)

	initReq := apiv1.StorageInitRequest{Remote: loc.Remote, Path: "pve-backups", Source: "homelab"}
	var res apiv1.StorageInitResponse
	if err := c.Do(ctx, http.MethodPost, "/v1/storages/offsite/init", initReq, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Created || !res.KitRequired || res.Encryption != "crypt" || res.RepoUUID == "" {
		t.Fatalf("init = %+v", res)
	}
	if _, err := e.keyStore.Load(res.RepoUUID); err != nil {
		t.Fatalf("keys not stored: %v", err)
	}
	// Repeating init adopts the existing repository.
	var again apiv1.StorageInitResponse
	if err := c.Do(ctx, http.MethodPost, "/v1/storages/offsite/init", initReq, &again); err != nil || again.Created || again.RepoUUID != res.RepoUUID {
		t.Fatalf("second init = %+v, %v", again, err)
	}
	for name, req := range map[string]apiv1.StorageInitRequest{
		"wrong encryption": {Remote: loc.Remote, Path: "pve-backups", Source: "homelab", Encryption: "none"},
		"bad source":       {Remote: loc.Remote, Path: "pve-backups", Source: "Home Lab"},
		"unknown remote":   {Remote: "nosuchremote", Path: "pve-backups", Source: "homelab"},
	} {
		if err := c.Do(ctx, http.MethodPost, "/v1/storages/offsite/init", req, nil); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	var plain apiv1.StorageInitResponse
	if err := c.Do(ctx, http.MethodPost, "/v1/storages/plain/init",
		apiv1.StorageInitRequest{Remote: loc.Remote, Path: "plain", Source: "homelab", Encryption: "none"}, &plain); err != nil || plain.KitRequired {
		t.Fatalf("unencrypted init = %+v, %v", plain, err)
	}

	// The storage entry can now be validated and added.
	if _, err := validateAdd(t, c, "offsite", loc.Remote); err != nil {
		t.Fatalf("validate after init: %v", err)
	}
	writeStorageCfg(t, e, fmt.Sprintf("dir: backups\n\tpath %s\n\tcontent backup\n\n", filepath.Dir(dump))+
		strings.TrimSuffix(offsiteSection("offsite", loc.Remote, "homelab"), "\n")+"\trclone-replicate-from backups\n\trclone-backfill all\n\n")
	s := waitStorage(t, c, "offsite", resynced)
	if s.KitConfirmed {
		t.Fatal("kit confirmed before export")
	}
	archive := filepath.Join(dump, "vzdump-lxc-200-2026_10_04-02_00_01.tar.zst")
	if err := os.WriteFile(archive, []byte("container archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(archive, old, old)

	// Replication waits for the recovery kit.
	var sum apiv1.ScanSummary
	if err := c.Do(ctx, http.MethodPost, "/v1/discovery/scan", nil, &sum); err != nil || sum.Queued+sum.Known != 0 {
		t.Fatalf("scan before the kit = %+v, %v", sum, err)
	}
	status, err := c.Status(ctx)
	if err != nil || status.Healthy || !strings.Contains(strings.Join(status.Problems, "\n"), "recovery kit") {
		t.Fatalf("status = %+v, %v", status, err)
	}

	var kit apiv1.KitExportResponse
	if err := c.Do(ctx, http.MethodPost, "/v1/recovery-kit/export", apiv1.KitExportRequest{
		Targets: []apiv1.KitTarget{{Storage: "offsite"}}, IncludeToken: true, Passphrase: "correct horse"}, &kit); err != nil {
		t.Fatal(err)
	}
	if !kit.Encrypted || len(kit.Repos) != 1 || !strings.Contains(kit.Kit, "BEGIN PVE-RCLONE-BACKUP RECOVERY KIT") {
		t.Fatalf("kit = %+v", kit)
	}
	if err := c.Do(ctx, http.MethodPost, "/v1/recovery-kit/confirm", apiv1.KitConfirmRequest{Checksum: "0000-0000-0000-0000"}, nil); !client.IsCode(err, apiv1.CodeInvalidArgument) {
		t.Fatalf("wrong checksum: %v", err)
	}
	if err := c.Do(ctx, http.MethodPost, "/v1/recovery-kit/confirm", apiv1.KitConfirmRequest{Checksum: kit.Checksum}, nil); err != nil {
		t.Fatal(err)
	}
	// Now the archive is uploaded.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var list []apiv1.Backup
		if err := c.Do(ctx, http.MethodGet, "/v1/storages/offsite/backups", nil, &list); err != nil {
			t.Fatal(err)
		}
		if len(list) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("archive not replicated after confirming the kit")
		}
		time.Sleep(50 * time.Millisecond)
	}

	var verified []apiv1.KitRepoResult
	if err := c.Do(ctx, http.MethodPost, "/v1/recovery-kit/verify", apiv1.KitRequest{Kit: kit.Kit, Passphrase: "correct horse"}, &verified); err != nil ||
		len(verified) != 1 || !verified[0].OK {
		t.Fatalf("verify = %+v, %v", verified, err)
	}
	if err := c.Do(ctx, http.MethodPost, "/v1/recovery-kit/verify", apiv1.KitRequest{Kit: kit.Kit, Passphrase: "wrong"}, nil); !client.IsCode(err, apiv1.CodeInvalidArgument) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	var statuses []apiv1.KitStatus
	if err := c.Do(ctx, http.MethodGet, "/v1/recovery-kit/status", nil, &statuses); err != nil || len(statuses) != 2 {
		t.Fatalf("kit status = %+v, %v", statuses, err)
	}
	if i := slices.IndexFunc(statuses, func(s apiv1.KitStatus) bool { return s.RepoUUID == res.RepoUUID }); i < 0 ||
		statuses[i].ConfirmedAt == nil || !slices.Equal(statuses[i].Storages, []string{"offsite"}) {
		t.Fatalf("kit status = %+v", statuses)
	}

	// Disaster recovery: a fresh installation imports the kit.
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	dr := withKeyStore(t, newEnv(t))
	stop = start(t, dr)
	dc := client.New(dr.socket)
	if _, err := validateAdd(t, dc, "offsite-dr", loc.Remote); !client.IsCode(err, apiv1.CodePreconditionFailed) {
		t.Fatalf("validate before import: %v", err)
	}
	var imported []apiv1.KitRepoResult
	if err := dc.Do(ctx, http.MethodPost, "/v1/recovery-kit/import", apiv1.KitRequest{Kit: kit.Kit, Passphrase: "correct horse", ImportToken: true}, &imported); err != nil {
		t.Fatal(err)
	}
	if len(imported) != 1 || !imported[0].OK || imported[0].Keys != "imported" || imported[0].RemoteConfig != "exists" || imported[0].Source != "homelab" {
		t.Fatalf("import = %+v", imported)
	}
	if _, err := dr.keyStore.Load(res.RepoUUID); err != nil {
		t.Fatalf("keys not imported: %v", err)
	}
	if _, err := validateAdd(t, dc, "offsite-dr", loc.Remote); err != nil {
		t.Fatalf("validate after import: %v", err)
	}
	if err := dc.Do(ctx, http.MethodPost, "/v1/recovery-kit/import", apiv1.KitRequest{Kit: kit.Kit, Passphrase: "correct horse"}, &imported); err != nil || imported[0].Keys != "present" {
		t.Fatalf("second import = %+v, %v", imported, err)
	}
	var schema map[string]any
	if err := dc.Do(ctx, http.MethodGet, "/v1/config/schema/storage", nil, &schema); err != nil || schema["properties"] == nil {
		t.Fatalf("schema = %v, %v", schema, err)
	}
}

func validateAdd(t *testing.T, c *client.Client, id, remote string) (*apiv1.ValidateResponse, error) {
	var resp apiv1.ValidateResponse
	err := c.Do(t.Context(), http.MethodPost, "/v1/storages/"+id+"/validate", apiv1.ValidateRequest{Config: map[string]any{
		"type": "rclone-backup", "rclone-remote": remote, "rclone-path": "pve-backups", "rclone-source": "homelab",
		"content": map[string]any{"backup": 1}}}, &resp)
	return &resp, err
}

func TestRemoteEndpoints(t *testing.T) {
	ctx := t.Context()
	r, _ := repotest.Init(t)
	free, _ := repotest.Remote(t, nil)
	e := newEnv(t)
	e.keys = repotest.Loader(r.Keys)
	writeStorageCfg(t, e, localSection+offsiteSection("offsite", r.Loc.Remote, "homelab"))
	stop := start(t, e)
	defer func() { _ = stop() }()
	c := client.New(e.socket)

	var providers []apiv1.Provider
	if err := c.Do(ctx, http.MethodGet, "/v1/providers", nil, &providers); err != nil || len(providers) != 1 || providers[0].Name != "onedrive" {
		t.Fatalf("providers = %+v, %v", providers, err)
	}
	var list []apiv1.Remote
	if err := c.Do(ctx, http.MethodGet, "/v1/remotes", nil, &list); err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(list, func(x apiv1.Remote) bool { return x.Name == r.Loc.Remote })
	if i < 0 || !slices.Equal(list[i].Storages, []string{"offsite"}) {
		t.Fatalf("remotes = %+v", list)
	}
	if err := c.Do(ctx, http.MethodDelete, "/v1/remotes/"+r.Loc.Remote, nil, nil); !client.IsCode(err, apiv1.CodeConflict) {
		t.Fatalf("delete of a used remote: %v", err)
	}
	var probe apiv1.ProbeResult
	if err := c.Do(ctx, http.MethodPost, "/v1/remotes/"+free.Remote+"/test", nil, &probe); err != nil || probe.HashType == "" {
		t.Fatalf("test = %+v, %v", probe, err)
	}
	if err := c.Do(ctx, http.MethodDelete, "/v1/remotes/"+free.Remote, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(ctx, http.MethodGet, "/v1/remotes/"+free.Remote, nil, nil); !client.IsCode(err, apiv1.CodeNotFound) {
		t.Fatalf("deleted remote: %v", err)
	}
	if err := c.Do(ctx, http.MethodPost, "/v1/remote-setup", apiv1.RemoteSetupRequest{Name: "Bad", Provider: "onedrive"}, nil); !client.IsCode(err, apiv1.CodeInvalidArgument) {
		t.Fatalf("invalid setup: %v", err)
	}
	if err := c.Do(ctx, http.MethodGet, "/v1/remote-setup/nope", nil, nil); !client.IsCode(err, apiv1.CodeNotFound) {
		t.Fatalf("unknown session: %v", err)
	}
}

// TestKitImportRefusesInjectedSettings: remote settings from a kit that
// could not be stored as they are (a line break would add a section to
// remotes.conf) do not create the remote.
func TestKitImportRefusesInjectedSettings(t *testing.T) {
	e := withKeyStore(t, newEnv(t))
	stop := start(t, e)
	defer func() { _ = stop() }()
	k := recoverykit.New("pve-old", time.Now())
	k.Repos = append(k.Repos, recoverykit.Repo{RepoUUID: repo.NewUUID(), StorageID: "offsite", Remote: "crafted",
		Path: "pve-backups", Source: "homelab", Encryption: "none",
		RemoteConfig: map[string]string{"type": "onedrive", "token": "{}\n\n[injected]\ntype = local"}})
	text, _, err := recoverykit.Encode(k, "")
	if err != nil {
		t.Fatal(err)
	}
	var imported []apiv1.KitRepoResult
	if err := client.New(e.socket).Do(t.Context(), http.MethodPost, "/v1/recovery-kit/import",
		apiv1.KitRequest{Kit: text, ImportToken: true}, &imported); err != nil {
		t.Fatal(err)
	}
	if len(imported) != 1 || imported[0].RemoteConfig != "invalid" {
		t.Fatalf("import = %+v", imported)
	}
	if repotest.Storage.HasSection("crafted") || repotest.Storage.HasSection("injected") {
		t.Fatal("remote created from injected settings")
	}
}
