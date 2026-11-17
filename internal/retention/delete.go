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
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/storages"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

// DeleteParams are the parameters of a delete job.
type DeleteParams struct {
	RemoteDir string `json:"remote_dir"` // g<N>/v1/... of the backup
	Reason    string `json:"reason"`
	// Incomplete removes an upload that never finished: a directory
	// without a manifest, which the catalogue does not list.
	Incomplete bool `json:"incomplete,omitzero"`
}

// Deleter implements jobs.Runner for delete jobs: it removes tombstoned
// backups whose grace period ended, finishes deletions a crash
// interrupted, and removes incomplete uploads.
type Deleter struct {
	Log       *slog.Logger
	Store     *store.Store
	Repos     func(storeID string) (*repo.Repo, *config.Storage, error)
	OnRemoved func(storeID, volname string)
	// InUse, if set, reports whether an upload job works on a backup
	// directory; an incomplete upload it works on is not removed.
	InUse func(ctx context.Context, storeID string, id layout.BackupID) bool
	// OnPurged is called after an incomplete upload was removed.
	OnPurged func(storeID, remoteDir string)
	Now      func() time.Time
	// Lock, if set, serializes the deletion with other writes of the
	// backup's remote documents (meta pushes).
	Lock func(storeID, volname string) (unlock func())
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
	if d.Lock != nil {
		defer d.Lock(j.StoreID, j.BackupVolname)()
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
	if p.Incomplete {
		return d.purge(ctx, t, j.StoreID, p, rp)
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
	deleted := false
	if b == nil {
		// Not catalogued: only finish a deletion the remote records, or the
		// remains of one that ran unmarked on a full remote (repo.Delete):
		// a directory without a manifest. A resync drops such a backup from
		// the catalogue. A committed backup is left alone.
		meta, err := rp.ReadMeta(ctx, gen, id)
		if err != nil {
			return err
		}
		if meta.Tombstone == nil || meta.Tombstone.State != "deleting" {
			_, _, err := rp.PurgeIncomplete(ctx, gen, id)
			if errors.Is(err, repo.ErrCommitted) {
				return jobs.Permanent(fmt.Errorf("%s is not being deleted", p.RemoteDir))
			}
			if err != nil {
				return err
			}
			deleted = true
		}
	}
	if !deleted {
		if err := rp.Delete(ctx, gen, id, p.Reason); err != nil {
			return err
		}
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

// purge removes an incomplete upload, unless an upload works on it again
// or it was committed meanwhile.
func (d *Deleter) purge(ctx context.Context, t *jobs.Task, storeID string, p DeleteParams, rp *repo.Repo) error {
	gen, id, err := catalog.RemoteID(&store.Backup{RemoteDir: p.RemoteDir})
	if err != nil {
		return jobs.Permanent(err)
	}
	if d.InUse != nil && d.InUse(ctx, storeID, id) {
		_ = t.Event(ctx, "info", "not removed: an upload job works on it again")
		return nil
	}
	files, bytes, err := rp.PurgeIncomplete(ctx, gen, id)
	if errors.Is(err, repo.ErrCommitted) {
		// The upload finished meanwhile: nothing to remove.
		_ = t.Event(ctx, "info", "not removed: it holds a committed backup now")
		return nil
	}
	if err != nil {
		return err
	}
	d.Log.Info("incomplete upload removed", "storage", storeID, "dir", p.RemoteDir, "files", files, "bytes", bytes,
		"reason", p.Reason)
	_ = t.Event(ctx, "info", fmt.Sprintf("removed %d files, %d bytes", files, bytes))
	if d.OnPurged != nil {
		d.OnPurged(storeID, p.RemoteDir)
	}
	return nil
}
