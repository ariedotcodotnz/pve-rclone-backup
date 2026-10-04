// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/catalog"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/pve/cluster"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/retention"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

// keepOptions converts PVE's keep hash to prune options.
func keepOptions(keep map[string]any) (*config.PruneOptions, error) {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(keep)) {
		parts = append(parts, fmt.Sprintf("%s=%v", k, keep[k]))
	}
	return config.ParsePruneOptions(strings.Join(parts, ","))
}

// prune marks a storage's backups and, unless dryRun, tombstones those
// marked for removal.
func (d *Daemon) prune(ctx context.Context, cfg *config.Storage, keep config.PruneOptions, vmid int, vmtype string, dryRun bool, by string) ([]*retention.Entry, error) {
	list, err := d.store.ListBackups(ctx, cfg.ID, store.BackupFilter{})
	if err != nil {
		return nil, err
	}
	rails := retention.Rails{Now: time.Now(), MinAge: cfg.MinAge, KeepMin: cfg.KeepMin, MaxDeletes: cfg.MaxDeletes}
	entries := retention.Plan(list, keep, rails, vmid, vmtype, time.Local)
	if dryRun {
		return entries, nil
	}
	now := time.Now()
	removed := 0
	for _, e := range entries {
		if e.Mark != retention.MarkRemove {
			continue
		}
		b, err := d.store.UpdateBackup(ctx, cfg.ID, e.Volname, func(b *store.Backup) error {
			if b.Protected || b.State == "tombstoned" {
				return errSkip
			}
			at, after := now.Unix(), now.Add(cfg.DeleteGrace).Unix()
			b.State, b.TombstoneAt, b.DeleteAfter, b.TombstoneReason, b.TombstoneBy = "tombstoned", &at, &after, "retention", by
			return nil
		})
		if errors.Is(err, errSkip) {
			continue
		}
		if err != nil {
			return entries, err
		}
		removed++
		d.metaChanged(b)
	}
	if removed > 0 {
		d.log.Info("retention marked offsite backups for deletion", "storage", cfg.ID, "backups", removed,
			"delete_after", now.Add(cfg.DeleteGrace).Format(time.DateTime), "by", by)
	}
	return entries, nil
}

var errSkip = errors.New("skip")

func (d *Daemon) retentionRoutes() {
	d.api.HandleIdempotent("POST /v1/storages/{storage}/retention", d.runRetention)
	d.api.HandleIdempotent("POST /v1/storages/{storage}/prune", func(w http.ResponseWriter, r *http.Request) error {
		id := r.PathValue("storage")
		cfg, ok := d.storages.Config(id)
		if !ok {
			return notConfigured(id)
		}
		var req apiv1.PruneRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return err
		}
		keep := cfg.Base.PruneBackups
		if req.Keep != nil {
			var err error
			if keep, err = keepOptions(req.Keep); err != nil {
				return api.Invalid("%v", err)
			}
		}
		if keep == nil {
			keep = &config.PruneOptions{KeepAll: true}
		}
		if !req.DryRun && cfg.Immutable {
			return api.Errorf(http.StatusForbidden, apiv1.CodeImmutable, "storage %s is immutable: offsite backups cannot be pruned", id)
		}
		vmid := 0
		if req.VMID != nil {
			vmid = *req.VMID
		}
		if req.Type != "" && req.Type != "qemu" && req.Type != "lxc" {
			return api.Invalid("invalid guest type %q", req.Type)
		}
		entries, err := d.prune(r.Context(), cfg, *keep, vmid, req.Type, req.DryRun, "prune request")
		if err != nil {
			return err
		}
		out := make([]apiv1.PruneEntry, 0, len(entries))
		for _, e := range entries {
			out = append(out, apiv1.PruneEntry{Volid: id + ":" + e.Volname, CTime: e.CTime, Type: e.VMType, VMID: e.VMID,
				Mark: e.Mark, Reason: e.Reason})
		}
		return api.WriteJSON(w, http.StatusOK, out)
	})
}

// runRetention applies a storage's prune-backups now and queues the
// deletions whose grace period ended.
func (d *Daemon) runRetention(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("storage")
	cfg, ok := d.storages.Config(id)
	if !ok {
		return notConfigured(id)
	}
	if cfg.Immutable {
		return api.Errorf(http.StatusForbidden, apiv1.CodeImmutable, "storage %s is immutable", id)
	}
	out := []apiv1.PruneEntry{}
	if keep := cfg.Base.PruneBackups; keep != nil && !keep.KeepAll {
		entries, err := d.prune(r.Context(), cfg, *keep, 0, "", false, "retention run")
		if err != nil {
			return err
		}
		for _, e := range entries {
			out = append(out, apiv1.PruneEntry{Volid: id + ":" + e.Volname, CTime: e.CTime, Type: e.VMType, VMID: e.VMID,
				Mark: e.Mark, Reason: e.Reason})
		}
	}
	d.queueDueDeletions(r.Context())
	return api.WriteJSON(w, http.StatusOK, out)
}

// isPrimary reports whether this node runs cluster-wide scheduled work.
func (d *Daemon) isPrimary() bool {
	m, _ := cluster.ReadMembers(d.opts.PVEDir, d.opts.Node)
	return m.Primary() == d.opts.Node
}

// retentionLoop applies scheduled retention once a day and deletes
// tombstoned backups whose grace period ended.
func (d *Daemon) retentionLoop(ctx context.Context, retentionTime string) {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for {
		if d.isPrimary() {
			d.scheduledPrune(ctx, retentionTime, time.Now())
			d.queueDueDeletions(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// scheduledPrune applies each storage's prune-backups once a day after
// retention-time.
func (d *Daemon) scheduledPrune(ctx context.Context, retentionTime string, now time.Time) {
	at, err := time.ParseInLocation("15:04", retentionTime, time.Local)
	if err != nil {
		return
	}
	due := time.Date(now.Year(), now.Month(), now.Day(), at.Hour(), at.Minute(), 0, 0, time.Local)
	if now.Before(due) {
		return
	}
	today := now.Format("2006-01-02")
	for _, cfg := range d.storages.Targets() {
		if cfg.Base.PruneBackups == nil || cfg.Base.PruneBackups.KeepAll || cfg.Immutable {
			continue
		}
		if _, _, err := d.storages.Repo(cfg.ID); err != nil {
			continue
		}
		key := "retention.last:" + cfg.ID
		if last, _, _ := d.store.Meta(ctx, key); last == today {
			continue
		}
		if _, err := d.prune(ctx, cfg, *cfg.Base.PruneBackups, 0, "", false, "scheduled retention"); err != nil {
			d.log.Warn("scheduled retention", "storage", cfg.ID, "err", err)
			continue
		}
		if err := d.store.SetMeta(ctx, key, today); err != nil {
			d.log.Warn("record retention run", "storage", cfg.ID, "err", err)
		}
	}
}

func (d *Daemon) queueDueDeletions(ctx context.Context) {
	due, err := d.store.TombstonesDue(ctx, time.Now().Unix())
	if err != nil {
		d.log.Warn("list due deletions", "err", err)
		return
	}
	for _, b := range due {
		d.queueDeletion(ctx, b.StoreID, b.Volname, b.RemoteDir, cmpOr(b.TombstoneReason, "user"))
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (d *Daemon) queueDeletion(ctx context.Context, storeID, volname, remoteDir, reason string) {
	pj, _ := json.Marshal(retention.DeleteParams{RemoteDir: remoteDir, Reason: reason})
	id, created, err := d.store.InsertJob(ctx, &store.Job{Kind: "delete", StoreID: storeID, State: "queued",
		DedupeKey: "delete:" + storeID + ":" + remoteDir, OwnerNode: d.opts.Node, BackupVolname: volname, ParamsJSON: string(pj)})
	if err != nil {
		d.log.Warn("queue deletion", "storage", storeID, "backup", volname, "err", err)
		return
	}
	if created {
		d.api.Events().Publish("job.updated", apiv1.JobUpdate{ID: id, State: "queued"})
		d.deletes.Wake()
	}
}

// finishDeletions queues deletions a crash interrupted (found by a
// resync).
func (d *Daemon) finishDeletions(storeID string, rep *catalog.Report) {
	if !d.isPrimary() {
		return
	}
	for _, b := range rep.Pending {
		if b.State == repo.StateDeleting {
			d.queueDeletion(context.Background(), storeID, "backup/"+b.ID.String(), fmt.Sprintf("g%d/%s", b.Generation, b.ID.Dir()), "interrupted deletion")
		}
	}
}
