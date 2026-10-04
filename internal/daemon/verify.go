// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/catalog"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/pve/cluster"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/restore"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

// damagedAlert raises or clears the damaged-backups alert of a storage.
func (d *Daemon) damagedAlert(ctx context.Context, storeID string, rep *catalog.Report) {
	id := "damaged:" + storeID
	if rep.Damaged == 0 {
		_ = d.store.ClearAlert(ctx, id)
		return
	}
	msg := fmt.Sprintf("%d offsite backups on %s are damaged (segments missing or altered); see 'pve-rclone-backup backup list --storage %s --state damaged'",
		rep.Damaged, storeID, storeID)
	if err := d.store.RaiseAlert(ctx, store.Alert{ID: id, Severity: "error", StoreID: storeID, Message: msg}); err != nil {
		d.log.Warn("raise alert", "alert", id, "err", err)
	}
}

func (d *Daemon) contentDamaged(storeID, volname, problem string) {
	id := "damaged:" + storeID + ":" + volname
	msg := fmt.Sprintf("Content verification of %s:%s failed: %s", storeID, volname, problem)
	if err := d.store.RaiseAlert(context.Background(), store.Alert{ID: id, Severity: "error", StoreID: storeID, Message: msg}); err != nil {
		d.log.Warn("raise alert", "alert", id, "err", err)
	}
}

func (d *Daemon) verifyRoutes() {
	d.api.HandleIdempotent("POST /v1/storages/{storage}/backups/{name}/verify", func(w http.ResponseWriter, r *http.Request) error {
		b, err := d.backup(r)
		if err != nil {
			return err
		}
		var req apiv1.VerifyRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return err
		}
		switch req.Level {
		case 1, 2:
			d.storages.RequestResync(b.StoreID)
			return api.WriteJSON(w, http.StatusAccepted, struct{}{})
		case restore.LevelContent:
		case restore.LevelRestore:
			if !config.ValidVMID(strconv.Itoa(req.ScratchVMID)) {
				return api.Invalid("a restore test needs a scratch VMID")
			}
			if req.ScratchStorage != "" && !config.ValidStorageID(req.ScratchStorage) {
				return api.Invalid("invalid scratch storage %q", req.ScratchStorage)
			}
		default:
			return api.Invalid("verification level must be 2, 3 or 4")
		}
		size := b.ArchiveSize
		j, err := d.queueJob(r, d.verifies, &store.Job{Kind: "verify", StoreID: b.StoreID, BackupVolname: b.Volname,
			TotalBytes: &size, VMType: b.VMType, VMID: b.VMID, BackupTime: b.BackupTime},
			restore.VerifyParams{Level: req.Level, ScratchVMID: req.ScratchVMID, ScratchStorage: req.ScratchStorage})
		if err != nil {
			return err
		}
		return api.WriteJSON(w, http.StatusAccepted, jobView(j))
	})
	d.api.Handle("GET /v1/verifications", func(w http.ResponseWriter, r *http.Request) error {
		q := r.URL.Query()
		ids := []string{q.Get("storage")}
		if ids[0] == "" {
			ids = ids[:0]
			for _, s := range d.storages.List() {
				ids = append(ids, s.ID)
			}
		}
		limit := 100
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 10000 {
				return api.Invalid("invalid limit %q", v)
			}
			limit = n
		}
		out := []apiv1.Verification{}
		for _, id := range ids {
			list, err := d.store.Verifications(r.Context(), id, q.Get("volname"), limit)
			if err != nil {
				return err
			}
			for _, v := range list {
				av := apiv1.Verification{Storage: v.StoreID, Volname: v.Volname, Level: v.Level, Result: v.Result,
					StartedAt: time.Unix(v.StartedAt, 0).UTC(), FinishedAt: unixTime(v.FinishedAt)}
				if json.Valid([]byte(v.DetailsJSON)) {
					av.Details = json.RawMessage(v.DetailsJSON)
				}
				out = append(out, av)
			}
		}
		return api.WriteJSON(w, http.StatusOK, out)
	})
}

// planVerification queues rolling content verifications within each
// storage's daily budget. Only the cluster's primary node plans them.
func (d *Daemon) planVerification(ctx context.Context) {
	timer := time.NewTimer(5 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		d.queueVerifications(ctx)
		timer.Reset(time.Hour)
	}
}

func (d *Daemon) queueVerifications(ctx context.Context) {
	if m, _ := cluster.ReadMembers(d.opts.PVEDir, d.opts.Node); m.Primary() != d.opts.Node {
		return
	}
	now := time.Now()
	for _, cfg := range d.storages.Targets() {
		if cfg.VerifyContent.Off || cfg.VerifyContent.Duration <= 0 {
			continue
		}
		if _, _, err := d.storages.Repo(cfg.ID); err != nil {
			continue
		}
		used, err := d.store.ContentVerifiedBytes(ctx, cfg.ID, now.Add(-24*time.Hour).Unix())
		if err != nil {
			d.log.Warn("verification budget", "storage", cfg.ID, "err", err)
			continue
		}
		cands, err := d.store.VerifyCandidates(ctx, cfg.ID, now.Add(-cfg.VerifyContent.Duration).Unix(), 100)
		if err != nil {
			d.log.Warn("verification candidates", "storage", cfg.ID, "err", err)
			continue
		}
		for _, b := range withinBudget(cands, used, cfg.VerifyBudget) {
			size := b.ArchiveSize
			pj, _ := json.Marshal(restore.VerifyParams{Level: restore.LevelContent})
			id, created, err := d.store.InsertJob(ctx, &store.Job{Kind: "verify", StoreID: cfg.ID, State: "queued",
				DedupeKey: fmt.Sprintf("verify:3:%s:%s:%s", cfg.ID, b.Volname, now.Format("2006-01-02")), OwnerNode: d.opts.Node,
				BackupVolname: b.Volname, TotalBytes: &size, VMType: b.VMType, VMID: b.VMID, BackupTime: b.BackupTime, ParamsJSON: string(pj)})
			if err != nil {
				d.log.Warn("queue verification", "storage", cfg.ID, "err", err)
				break
			}
			if created {
				d.api.Events().Publish("job.updated", apiv1.JobUpdate{ID: id, State: "queued"})
			}
		}
	}
	d.verifies.Wake()
}

// withinBudget picks candidates, in order, while the bytes verified in the
// last day stay within the budget. A backup larger than the whole budget
// is still taken on a day with nothing else verified.
func withinBudget(cands []*store.Backup, used, budget int64) []*store.Backup {
	var out []*store.Backup
	for _, b := range cands {
		if used > 0 && used+b.ArchiveSize > budget {
			break
		}
		used += b.ArchiveSize
		out = append(out, b)
	}
	return out
}
