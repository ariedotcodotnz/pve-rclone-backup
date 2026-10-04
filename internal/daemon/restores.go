// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/jobs"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/restore"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

// ioniceArgs translates the restore-ionice setting.
func ioniceArgs(v string) []string {
	if v == "idle" {
		return []string{"ionice", "-c3"}
	}
	if level, ok := strings.CutPrefix(v, "best-effort:"); ok {
		return []string{"ionice", "-c2", "-n" + level}
	}
	return nil
}

// schedulerFor returns the scheduler running jobs of a kind.
func (d *Daemon) schedulerFor(kind string) *jobs.Scheduler {
	switch kind {
	case "fetch":
		return d.fetches
	case "restore":
		return d.restores
	case "verify":
		return d.verifies
	}
	return d.scheduler
}

func (d *Daemon) queueJob(r *http.Request, sched *jobs.Scheduler, j *store.Job, params any) (*store.Job, error) {
	pj, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	j.ParamsJSON, j.State, j.OwnerNode = string(pj), jobs.StateQueued, d.opts.Node
	j.DedupeKey = fmt.Sprintf("%s:%s:%s:%s", j.Kind, j.StoreID, j.BackupVolname, rand.Text())
	id, _, err := d.store.InsertJob(r.Context(), j)
	if err != nil {
		return nil, err
	}
	sched.Wake()
	d.api.Events().Publish("job.updated", apiv1.JobUpdate{ID: id, State: jobs.StateQueued})
	return d.store.GetJob(r.Context(), id)
}

func (d *Daemon) restoreRoutes() {
	d.api.HandleIdempotent("POST /v1/storages/{storage}/backups/{name}/fetch", func(w http.ResponseWriter, r *http.Request) error {
		b, err := d.backup(r)
		if err != nil {
			return err
		}
		var req apiv1.FetchRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return err
		}
		if !config.ValidStorageID(req.TargetStorage) {
			return api.Invalid("invalid target storage %q", req.TargetStorage)
		}
		p := restore.FetchParams{TargetStorage: req.TargetStorage, Protect: req.Protect == nil || *req.Protect}
		size := b.ArchiveSize
		j, err := d.queueJob(r, d.fetches, &store.Job{Kind: "fetch", StoreID: b.StoreID, BackupVolname: b.Volname,
			TotalBytes: &size, VMType: b.VMType, VMID: b.VMID, BackupTime: b.BackupTime}, p)
		if err != nil {
			return err
		}
		return api.WriteJSON(w, http.StatusAccepted, jobView(j))
	})
	d.api.HandleIdempotent("POST /v1/restores", func(w http.ResponseWriter, r *http.Request) error {
		var req apiv1.RestoreRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return err
		}
		if _, err := layout.ParseVolname(req.Volname); err != nil {
			return api.Invalid("invalid backup %q", req.Volname)
		}
		r.SetPathValue("storage", req.Storage)
		r.SetPathValue("name", strings.TrimPrefix(req.Volname, "backup/"))
		b, err := d.backup(r)
		if err != nil {
			return err
		}
		if req.Mode == "" {
			req.Mode = restore.Modes(b.VMType)[0]
		}
		if !restore.ValidMode(b.VMType, req.Mode) {
			return api.Invalid("restore mode %q is not available for %s backups (use %s)", req.Mode, b.VMType,
				strings.Join(restore.Modes(b.VMType), " or "))
		}
		if !config.ValidVMID(strconv.Itoa(req.TargetVMID)) {
			return api.Invalid("invalid target VMID %d", req.TargetVMID)
		}
		if req.TargetStorage != "" && !config.ValidStorageID(req.TargetStorage) {
			return api.Invalid("invalid target storage %q", req.TargetStorage)
		}
		p := restore.RestoreParams{Mode: req.Mode, TargetVMID: req.TargetVMID, TargetStorage: req.TargetStorage,
			Unique: req.Unique, Force: req.Force, AllowDamaged: req.AllowDamaged}
		size := b.ArchiveSize
		j, err := d.queueJob(r, d.restores, &store.Job{Kind: "restore", StoreID: b.StoreID, BackupVolname: b.Volname,
			TotalBytes: &size, VMType: b.VMType, VMID: b.VMID, BackupTime: b.BackupTime}, p)
		if err != nil {
			return err
		}
		return api.WriteJSON(w, http.StatusAccepted, jobView(j))
	})
}
