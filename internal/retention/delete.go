// SPDX-License-Identifier: AGPL-3.0-or-later

package retention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/catalog"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/jobs"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/storages"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

// DeleteParams are the parameters of a delete job.
type DeleteParams struct {
	RemoteDir string `json:"remote_dir"` // g<N>/v1/... of the backup
	Reason    string `json:"reason"`
}

// Deleter implements jobs.Runner for delete jobs: it removes tombstoned
// backups whose grace period ended, and finishes deletions a crash
// interrupted.
type Deleter struct {
	Log       *slog.Logger
	Store     *store.Store
	Repos     func(storeID string) (*repo.Repo, *config.Storage, error)
	OnRemoved func(storeID, volname string)
	Now       func() time.Time
}

// Run implements jobs.Runner.
func (d *Deleter) Run(ctx context.Context, t *jobs.Task) error {
	j := t.Job()
	now := time.Now()
	if d.Now != nil {
		now = d.Now()
	}
	var p DeleteParams
	if err := json.Unmarshal([]byte(j.ParamsJSON), &p); err != nil {
		return jobs.Permanent(err)
	}
	rp, cfg, err := d.Repos(j.StoreID)
	if errors.Is(err, storages.ErrNotReady) {
		return jobs.Postpone(now.Add(10*time.Minute), err.Error())
	}
	if err != nil {
		return jobs.Permanent(err)
	}
	if cfg.Immutable {
		return jobs.Permanent(fmt.Errorf("storage %s is immutable", j.StoreID))
	}
	b, err := d.Store.GetBackup(ctx, j.StoreID, j.BackupVolname)
	switch {
	case errors.Is(err, store.ErrNotFound):
		b = nil
	case err != nil:
		return err
	}
	if b != nil {
		// The operator may have changed their mind during the grace period.
		switch {
		case b.Protected:
			return jobs.Permanent(fmt.Errorf("%s is protected", b.Volname))
		case b.State != "tombstoned" && b.State != "deleting":
			return jobs.Permanent(fmt.Errorf("%s is no longer marked for deletion", b.Volname))
		case b.DeleteAfter != nil && now.Unix() < *b.DeleteAfter:
			return jobs.Postpone(time.Unix(*b.DeleteAfter, 0), "waiting for the end of the grace period")
		}
		p.RemoteDir = b.RemoteDir
	}
	gen, id, err := catalog.RemoteID(&store.Backup{RemoteDir: p.RemoteDir})
	if err != nil {
		return jobs.Permanent(err)
	}
	if b == nil {
		// Not catalogued: only finish a deletion the remote records.
		meta, err := rp.ReadMeta(ctx, gen, id)
		if err != nil {
			return err
		}
		if meta.Tombstone == nil || meta.Tombstone.State != "deleting" {
			return jobs.Permanent(fmt.Errorf("%s is not being deleted", p.RemoteDir))
		}
	}
	if err := rp.Delete(ctx, gen, id, p.Reason); err != nil {
		return err
	}
	if err := d.Store.DeleteBackup(ctx, j.StoreID, j.BackupVolname); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	d.Log.Info("offsite backup deleted", "storage", j.StoreID, "backup", strings.TrimPrefix(j.BackupVolname, "backup/"),
		"reason", p.Reason)
	if d.OnRemoved != nil {
		d.OnRemoved(j.StoreID, j.BackupVolname)
	}
	return nil
}
