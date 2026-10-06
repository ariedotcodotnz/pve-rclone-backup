// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	rconfig "github.com/rclone/rclone/fs/config"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/pve/cluster"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/recoverykit"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/replicate"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/secrets"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

func (d *Daemon) identity() (*replicate.Identity, string, error) {
	id, err := replicate.LoadIdentity(context.Background(), d.opts.PVEDir, d.opts.Locker)
	members, _ := cluster.ReadMembers(d.opts.PVEDir, d.opts.Node)
	return id, members.Cluster, err
}

func preconditionf(format string, args ...any) error {
	return api.Errorf(http.StatusPreconditionFailed, apiv1.CodePreconditionFailed, format, args...)
}

func conflictf(format string, args ...any) error {
	return api.Errorf(http.StatusConflict, apiv1.CodeConflict, format, args...)
}

func (d *Daemon) setupRoutes() {
	d.api.HandleIdempotent("POST /v1/storages/{storage}/init", d.initStorage)
	d.api.Handle("POST /v1/storages/{storage}/resync", func(w http.ResponseWriter, r *http.Request) error {
		if !d.storages.RequestResync(r.PathValue("storage")) {
			return notConfigured(r.PathValue("storage"))
		}
		return api.WriteJSON(w, http.StatusAccepted, struct{}{})
	})
	d.api.Handle("POST /v1/recovery-kit/export", d.exportKit)
	d.api.Handle("POST /v1/recovery-kit/confirm", func(w http.ResponseWriter, r *http.Request) error {
		var req apiv1.KitConfirmRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return err
		}
		repos, err := d.ledger.Confirm(r.Context(), req.Checksum, time.Now())
		if err != nil {
			return err
		}
		if len(repos) == 0 {
			return api.Invalid("no exported recovery kit has the checksum %q", req.Checksum)
		}
		d.log.Info("recovery kit confirmed", "repos", repos)
		d.discovery.Refresh()
		return api.WriteJSON(w, http.StatusOK, apiv1.KitConfirmResponse{Repos: repos})
	})
	d.api.Handle("POST /v1/recovery-kit/import", d.importKit)
	d.api.Handle("POST /v1/recovery-kit/verify", d.verifyKit)
	d.api.Handle("GET /v1/recovery-kit/status", d.kitStatus)
	d.api.Handle("GET /v1/config/schema/{kind}", func(w http.ResponseWriter, r *http.Request) error {
		kind := r.PathValue("kind")
		if !slices.Contains([]string{"storage", "node", "daemon"}, kind) {
			return api.NotFound("no %q configuration schema", kind)
		}
		data, err := config.Schemas.ReadFile(kind + ".schema.json")
		if err != nil {
			return err
		}
		return api.WriteJSON(w, http.StatusOK, json.RawMessage(data))
	})
}

// initStorage creates the repository of a storage that is about to be
// added, or adopts the repository already at the location.
func (d *Daemon) initStorage(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("storage")
	if !config.ValidStorageID(id) {
		return api.Invalid("invalid storage ID %q", id)
	}
	var req apiv1.StorageInitRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return err
	}
	req.Encryption = cmp.Or(req.Encryption, "crypt")
	req.Path = cmp.Or(req.Path, "pve-backups")
	if req.Encryption != "crypt" && req.Encryption != "none" {
		return api.Invalid("encryption must be crypt or none")
	}
	if req.ReadOnly && req.AdoptSource {
		return api.Invalid("a read-only storage does not adopt the source")
	}
	if !layout.ValidSource(req.Source) {
		return api.Invalid("invalid source name %q (lower case letters, digits and '-', at most 32)", req.Source)
	}
	loc := transport.RepoLocation{Remote: req.Remote, Path: req.Path}
	profile, err := loc.Profile()
	if err != nil {
		return api.Invalid("%v", err)
	}
	if req.Encryption == "crypt" && d.keyStore == nil {
		return errors.New("no key store configured")
	}
	ident, clusterName, err := d.identity()
	if err != nil {
		return fmt.Errorf("installation identity: %w", err)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	resp := apiv1.StorageInitResponse{Encryption: req.Encryption}
	var rp *repo.Repo
	marker, _, err := repo.ReadMarker(ctx, loc)
	switch {
	case errors.Is(err, repo.ErrNotInitialized) && req.ReadOnly:
		return preconditionf("there is no repository at %s:%s", loc.Remote, loc.Path)
	case errors.Is(err, repo.ErrNotInitialized):
		var keys *secrets.RepoKeys
		uuid := repo.NewUUID()
		if req.Encryption == "crypt" {
			if keys, err = secrets.NewRepoKeys(uuid, profile.FilenameEncoding, time.Now()); err != nil {
				return err
			}
			// Keys are stored before anything is encrypted with them.
			if err := d.keyStore.Save(ctx, keys); err != nil {
				return fmt.Errorf("store repository keys: %w", err)
			}
		}
		if rp, err = repo.Init(ctx, loc, repo.InitOptions{Keys: keys}); err != nil {
			return transportError(err)
		}
		resp.Created = true
	case err != nil:
		return transportError(err)
	default:
		if marker.Encryption != req.Encryption {
			return conflictf("the repository at %s:%s uses encryption %q", loc.Remote, loc.Path, marker.Encryption)
		}
		rp, err = repo.Open(ctx, loc, repo.OpenOptions{Encryption: req.Encryption, Keys: d.opts.Keys})
		switch {
		case errors.Is(err, secrets.ErrNotFound):
			return preconditionf("a repository exists at %s:%s but its keys are not on this cluster; import its recovery kit", loc.Remote, loc.Path)
		case errors.Is(err, repo.ErrWrongKeys):
			return preconditionf("%v", err)
		case err != nil:
			return transportError(err)
		}
	}
	resp.RepoUUID = rp.UUID()
	if req.ReadOnly {
		sources, err := rp.Sources(ctx)
		if err != nil {
			return transportError(err)
		}
		if !slices.Contains(sources, req.Source) {
			return preconditionf("the repository at %s:%s has no source %q (it has: %s)", loc.Remote, loc.Path, req.Source,
				strings.Join(sources, ", "))
		}
	} else if err := rp.RegisterSource(ctx, req.Source, ident.UUID, clusterName, req.AdoptSource); err != nil {
		if errors.Is(err, repo.ErrSourceTaken) {
			return conflictf("%v; choose another source name, or adopt it if this installation replaces the old one", err)
		}
		return transportError(err)
	}
	if err := d.store.PutRepository(ctx, &store.Repository{UUID: rp.UUID(), Remote: loc.Remote, BasePath: loc.Path, Encryption: req.Encryption}); err != nil {
		return err
	}
	resp.KitRequired = !req.ReadOnly && req.Encryption == "crypt" && !d.ledger.Confirmed(rp.UUID())
	d.log.Info("repository ready for storage", "storage", id, "repo", rp.UUID(), "created", resp.Created,
		"remote", loc.Remote, "path", loc.Path, "source", req.Source)
	return api.WriteJSON(w, http.StatusOK, resp)
}

// kitRepo resolves a kit target to its repository.
func (d *Daemon) kitRepo(ctx context.Context, t apiv1.KitTarget) (recoverykit.Repo, error) {
	kr := recoverykit.Repo{StorageID: t.Storage, Remote: t.Remote, Path: t.Path, Source: t.Source}
	if kr.Remote == "" {
		s, ok := d.storages.Get(t.Storage)
		if !ok {
			return kr, api.Invalid("storage %q is not configured; give its remote, path and source", t.Storage)
		}
		kr.Remote, kr.Path, kr.Source = s.Remote, s.Path, s.Source
	}
	rec, err := d.store.FindRepository(ctx, kr.Remote, kr.Path)
	if errors.Is(err, store.ErrNotFound) {
		return kr, preconditionf("no repository is known at %s:%s; run 'pve-rclone-backup storage init' first", kr.Remote, kr.Path)
	}
	if err != nil {
		return kr, err
	}
	kr.RepoUUID, kr.Encryption = rec.UUID, rec.Encryption
	if rec.Encryption == "crypt" {
		if kr.Keys, err = d.opts.Keys(rec.UUID); err != nil {
			return kr, preconditionf("keys of repository %s: %v", rec.UUID, err)
		}
	}
	return kr, nil
}

func (d *Daemon) exportKit(w http.ResponseWriter, r *http.Request) error {
	var req apiv1.KitExportRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return err
	}
	targets := req.Targets
	if len(targets) == 0 {
		for _, s := range d.storages.List() {
			targets = append(targets, apiv1.KitTarget{Storage: s.ID})
		}
	}
	if len(targets) == 0 {
		return api.Invalid("no storages to export")
	}
	ident, _, err := d.identity()
	if err != nil {
		return err
	}
	kit := recoverykit.New(d.opts.Node, time.Now())
	var uuids []string
	for _, t := range targets {
		kr, err := d.kitRepo(r.Context(), t)
		if err != nil {
			return err
		}
		if slices.Contains(uuids, kr.RepoUUID) && kr.Source == "" {
			continue
		}
		kr.SourceUUID = ident.UUID
		if req.IncludeToken {
			kr.RemoteConfig = remoteSection(kr.Remote)
		}
		kit.Repos = append(kit.Repos, kr)
		if !slices.Contains(uuids, kr.RepoUUID) {
			uuids = append(uuids, kr.RepoUUID)
		}
	}
	text, checksum, err := recoverykit.Encode(kit, req.Passphrase)
	if err != nil {
		return err
	}
	if err := d.ledger.Exported(r.Context(), uuids, checksum, time.Now()); err != nil {
		return err
	}
	d.log.Info("recovery kit exported", "repos", uuids, "include_token", req.IncludeToken,
		"passphrase", req.Passphrase != "", "checksum", checksum)
	return api.WriteJSON(w, http.StatusOK, apiv1.KitExportResponse{Kit: text, Checksum: checksum, Repos: uuids,
		Encrypted: req.Passphrase != ""})
}

// remoteSection returns all settings of a transport remote, credentials
// included (for recovery kits only).
func remoteSection(name string) map[string]string {
	data := rconfig.LoadedData()
	if !data.HasSection(name) {
		return nil
	}
	out := map[string]string{}
	for _, k := range data.GetKeyList(name) {
		if v, ok := data.GetValue(name, k); ok {
			out[k] = v
		}
	}
	return out
}

func kitError(err error) error {
	if errors.Is(err, recoverykit.ErrPassphraseRequired) || errors.Is(err, recoverykit.ErrBadPassphrase) || errors.Is(err, recoverykit.ErrDamaged) {
		return api.Invalid("%v", err)
	}
	return api.Invalid("not a recovery kit: %v", err)
}

func (d *Daemon) importKit(w http.ResponseWriter, r *http.Request) error {
	var req apiv1.KitRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return err
	}
	kit, err := recoverykit.Decode(req.Kit, req.Passphrase)
	if err != nil {
		return kitError(err)
	}
	if d.keyStore == nil {
		return errors.New("no key store configured")
	}
	ctx := r.Context()
	var results []apiv1.KitRepoResult
	var imported []string
	for _, kr := range kit.Repos {
		res := apiv1.KitRepoResult{RepoUUID: kr.RepoUUID, Storage: kr.StorageID, Remote: kr.Remote, Path: kr.Path,
			Source: kr.Source, Encryption: kr.Encryption, Keys: "none"}
		if err := (transport.RepoLocation{Remote: kr.Remote, Path: kr.Path}).Validate(); err != nil {
			res.Error = err.Error()
			results = append(results, res)
			continue
		}
		if kr.Keys != nil {
			local, err := d.keyStore.Load(kr.RepoUUID)
			if errors.Is(err, secrets.ErrNotFound) {
				local, err = nil, nil
			}
			var merged *secrets.RepoKeys
			var changed bool
			if err == nil {
				merged, changed, err = secrets.MergeKeys(local, kr.Keys)
			}
			if err == nil && changed {
				err = d.keyStore.Save(ctx, merged)
			}
			if err != nil {
				res.Error = err.Error()
				results = append(results, res)
				continue
			}
			res.Keys = map[bool]string{true: "merged", false: "present"}[changed]
			if local == nil {
				res.Keys = "imported"
			}
		}
		res.RemoteConfig = d.importRemote(kr, req.ImportToken)
		if err := d.store.PutRepository(ctx, &store.Repository{UUID: kr.RepoUUID, Remote: kr.Remote, BasePath: kr.Path,
			Encryption: kr.Encryption}); err != nil {
			return err
		}
		res.OK = true
		imported = append(imported, kr.RepoUUID)
		results = append(results, res)
	}
	if len(imported) > 0 {
		if err := d.ledger.MarkConfirmed(ctx, imported, time.Now()); err != nil {
			return err
		}
	}
	d.log.Info("recovery kit imported", "repos", imported)
	d.storages.Wake()
	d.discovery.Refresh()
	return api.WriteJSON(w, http.StatusOK, results)
}

// importRemote creates the transport remote from a kit if asked to and it
// does not exist yet.
func (d *Daemon) importRemote(kr recoverykit.Repo, enabled bool) string {
	data := rconfig.LoadedData()
	switch {
	case data.HasSection(kr.Remote):
		return "exists"
	case len(kr.RemoteConfig) == 0:
		return "not_in_kit"
	case !enabled:
		return "skipped"
	}
	if _, err := transport.ProfileFor(kr.RemoteConfig["type"]); err != nil {
		return "skipped"
	}
	for k, v := range kr.RemoteConfig {
		data.SetValue(kr.Remote, k, v)
	}
	rconfig.SaveConfig()
	d.log.Info("transport remote created from recovery kit", "remote", kr.Remote)
	return "created"
}

func (d *Daemon) verifyKit(w http.ResponseWriter, r *http.Request) error {
	var req apiv1.KitRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return err
	}
	kit, err := recoverykit.Decode(req.Kit, req.Passphrase)
	if err != nil {
		return kitError(err)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	var results []apiv1.KitRepoResult
	for _, kr := range kit.Repos {
		res := apiv1.KitRepoResult{RepoUUID: kr.RepoUUID, Storage: kr.StorageID, Remote: kr.Remote, Path: kr.Path,
			Source: kr.Source, Encryption: kr.Encryption}
		loc := transport.RepoLocation{Remote: kr.Remote, Path: kr.Path}
		keys := kr.Keys
		rp, err := repo.Open(ctx, loc, repo.OpenOptions{Encryption: kr.Encryption, UUID: kr.RepoUUID,
			Keys: func(string) (*secrets.RepoKeys, error) {
				if keys == nil {
					return nil, secrets.ErrNotFound
				}
				return keys, nil
			}})
		if err == nil && kr.Source != "" {
			var sources []string
			if sources, err = rp.Sources(ctx); err == nil && !slices.Contains(sources, kr.Source) {
				err = fmt.Errorf("source %q is not in the repository", kr.Source)
			}
		}
		if err != nil {
			res.Error = err.Error()
		} else {
			res.OK = true
		}
		results = append(results, res)
	}
	return api.WriteJSON(w, http.StatusOK, results)
}

func (d *Daemon) kitStatus(w http.ResponseWriter, r *http.Request) error {
	recs, err := d.ledger.Load()
	if err != nil {
		return err
	}
	repos, err := d.store.ListRepositories(r.Context())
	if err != nil {
		return err
	}
	byRepo := map[string][]string{}
	for _, s := range d.storages.List() {
		if s.RepoUUID != "" {
			byRepo[s.RepoUUID] = append(byRepo[s.RepoUUID], s.ID)
		}
	}
	out := []apiv1.KitStatus{}
	for _, rp := range repos {
		rec := recs[rp.UUID]
		out = append(out, apiv1.KitStatus{RepoUUID: rp.UUID, Storages: append([]string{}, byRepo[rp.UUID]...),
			Encrypted: rp.Encryption == "crypt", ExportedAt: rec.ExportedAt, ConfirmedAt: rec.ConfirmedAt})
	}
	return api.WriteJSON(w, http.StatusOK, out)
}
