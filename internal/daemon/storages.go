// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/manifest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/storages"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

// States listed when a client does not ask for specific ones: tombstoned
// backups are hidden from PVE until they are restored or purged.
var defaultListStates = []string{"complete", "damaged"}

var backupStates = []string{"complete", "tombstoned", "deleting", "damaged"}

func (d *Daemon) storageRoutes() {
	d.api.Handle("GET /v1/storages", func(w http.ResponseWriter, r *http.Request) error {
		return api.WriteJSON(w, http.StatusOK, d.storages.List())
	})
	d.api.Handle("GET /v1/storages/{storage}", func(w http.ResponseWriter, r *http.Request) error {
		s, err := d.storage(r)
		if err != nil {
			return err
		}
		return api.WriteJSON(w, http.StatusOK, s)
	})
	d.api.Handle("GET /v1/storages/{storage}/status", func(w http.ResponseWriter, r *http.Request) error {
		st, ok := d.storages.Status(r.PathValue("storage"))
		if !ok {
			return notConfigured(r.PathValue("storage"))
		}
		return api.WriteJSON(w, http.StatusOK, st)
	})
	d.api.Handle("POST /v1/storages/{storage}/validate", func(w http.ResponseWriter, r *http.Request) error {
		var req apiv1.ValidateRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return err
		}
		resp, err := d.storages.Validate(r.Context(), r.PathValue("storage"), req)
		switch {
		case errors.Is(err, storages.ErrInvalid):
			return api.Invalid("%s", strings.TrimPrefix(err.Error(), storages.ErrInvalid.Error()+": "))
		case errors.Is(err, storages.ErrPrecondition):
			return api.Errorf(http.StatusPreconditionFailed, apiv1.CodePreconditionFailed, "%s",
				strings.TrimPrefix(err.Error(), storages.ErrPrecondition.Error()+": "))
		case err != nil:
			return err
		}
		return api.WriteJSON(w, http.StatusOK, resp)
	})
	d.api.Handle("GET /v1/storages/{storage}/backups", d.listBackups)
	d.api.Handle("GET /v1/storages/{storage}/backups/{name}", func(w http.ResponseWriter, r *http.Request) error {
		b, err := d.backup(r)
		if err != nil {
			return err
		}
		return api.WriteJSON(w, http.StatusOK, apiv1.BackupDetail{Backup: backupView(b), Manifest: json.RawMessage(b.ManifestJSON)})
	})
	d.api.Handle("GET /v1/storages/{storage}/backups/{name}/config", func(w http.ResponseWriter, r *http.Request) error {
		b, err := d.backup(r)
		if err != nil {
			return err
		}
		m, err := manifest.DecodeManifest([]byte(b.ManifestJSON))
		if err != nil {
			return err
		}
		return api.WriteJSON(w, http.StatusOK, apiv1.GuestConfig{Config: m.Guest.Config, Firewall: m.Guest.Firewall,
			FirewallKnown: m.Guest.FirewallKnown})
	})
}

func notConfigured(id string) error {
	return api.NotFound("storage %q is not an rclone-backup storage", id)
}

func (d *Daemon) storage(r *http.Request) (apiv1.Storage, error) {
	id := r.PathValue("storage")
	s, ok := d.storages.Get(id)
	if !ok {
		return s, notConfigured(id)
	}
	return s, nil
}

func (d *Daemon) listBackups(w http.ResponseWriter, r *http.Request) error {
	s, err := d.storage(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	f := store.BackupFilter{States: defaultListStates}
	if v := q.Get("vmid"); v != "" {
		if f.VMID, err = strconv.Atoi(v); err != nil || f.VMID < 1 {
			return api.Invalid("invalid vmid %q", v)
		}
	}
	switch t := q.Get("type"); t {
	case "", "qemu", "lxc":
		f.VMType = t
	default:
		return api.Invalid("invalid guest type %q", t)
	}
	switch v := q.Get("state"); v {
	case "":
	case "all":
		f.States = nil
	default:
		f.States = strings.Split(v, ",")
		for _, st := range f.States {
			if !slices.Contains(backupStates, st) {
				return api.Invalid("invalid backup state %q", st)
			}
		}
	}
	list, err := d.store.ListBackups(r.Context(), s.ID, f)
	if err != nil {
		return err
	}
	out := make([]apiv1.Backup, 0, len(list))
	for _, b := range list {
		out = append(out, backupView(b))
	}
	return api.WriteJSON(w, http.StatusOK, out)
}

// backup resolves {storage} and {name}, the archive name without the
// "backup/" prefix of the volume name.
func (d *Daemon) backup(r *http.Request) (*store.Backup, error) {
	s, err := d.storage(r)
	if err != nil {
		return nil, err
	}
	volname := "backup/" + r.PathValue("name")
	if _, err := layout.ParseVolname(volname); err != nil {
		return nil, api.Invalid("invalid backup name %q", r.PathValue("name"))
	}
	b, err := d.store.GetBackup(r.Context(), s.ID, volname)
	if errors.Is(err, store.ErrNotFound) {
		return nil, api.NotFound("backup %s not found on storage %s", volname, s.ID)
	}
	return b, err
}

func unixTime(p *int64) *time.Time {
	if p == nil {
		return nil
	}
	t := time.Unix(*p, 0).UTC()
	return &t
}

func backupView(b *store.Backup) apiv1.Backup {
	return apiv1.Backup{
		Volname: b.Volname, VMType: b.VMType, VMID: b.VMID, CTime: b.BackupTime, Size: b.ArchiveSize,
		Format: b.ArchiveFormat, Compression: b.Compression, State: b.State, Protected: b.Protected,
		Notes: b.Notes, GuestName: b.GuestName, Generation: b.Generation, UploadedAt: time.Unix(b.UploadedAt, 0).UTC(),
		VerifyLevel: b.VerifyLevel, VerifiedAt: unixTime(b.VerifiedAt), VerifyResult: b.VerifyResult,
		DeleteAfter: unixTime(b.DeleteAfter),
	}
}
