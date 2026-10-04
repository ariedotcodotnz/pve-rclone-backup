// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/manifest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

// maxNotes bounds notes like PVE's own backup notes.
const maxNotes = 64 << 10

func (d *Daemon) backupRoutes() {
	d.api.Handle("PATCH /v1/storages/{storage}/backups/{name}", func(w http.ResponseWriter, r *http.Request) error {
		b, err := d.backup(r)
		if err != nil {
			return err
		}
		var req apiv1.BackupUpdate
		if err := api.DecodeJSON(r, &req); err != nil {
			return err
		}
		if req.Notes != nil && (len(*req.Notes) > maxNotes || !utf8.ValidString(*req.Notes)) {
			return api.Invalid("notes must be valid UTF-8 of at most %d bytes", maxNotes)
		}
		b, err = d.store.UpdateBackup(r.Context(), b.StoreID, b.Volname, func(b *store.Backup) error {
			if req.Notes != nil {
				b.Notes = *req.Notes
			}
			if req.Protected != nil {
				b.Protected = *req.Protected
			}
			return nil
		})
		if err != nil {
			return err
		}
		d.metaChanged(b)
		return api.WriteJSON(w, http.StatusOK, backupView(b))
	})
	// Deleting an offsite backup only tombstones it: it disappears from
	// PVE's listing and is removed after rclone-delete-grace, until when
	// it can be restored with undelete.
	d.api.Handle("DELETE /v1/storages/{storage}/backups/{name}", func(w http.ResponseWriter, r *http.Request) error {
		b, err := d.backup(r)
		if err != nil {
			return err
		}
		cfg, ok := d.storages.Config(b.StoreID)
		if !ok {
			return notConfigured(b.StoreID)
		}
		if r.URL.Query().Get("immediate") != "" {
			return api.Invalid("immediate deletion is not supported; deleted backups are kept for rclone-delete-grace")
		}
		switch {
		case cfg.Immutable:
			return api.Errorf(http.StatusForbidden, apiv1.CodeImmutable, "storage %s is immutable: offsite backups cannot be deleted", b.StoreID)
		case b.Protected:
			return api.Errorf(http.StatusConflict, apiv1.CodeProtected, "backup %s is protected", b.Volname)
		case b.State == "tombstoned":
			return api.WriteJSON(w, http.StatusOK, backupView(b))
		case b.State != "complete" && b.State != "damaged":
			return api.Errorf(http.StatusConflict, apiv1.CodeConflict, "backup %s is %s", b.Volname, b.State)
		}
		now := time.Now()
		who := "api"
		if p, ok := api.PeerFromContext(r.Context()); ok {
			who = "uid " + strconv.FormatUint(uint64(p.UID), 10)
		}
		b, err = d.store.UpdateBackup(r.Context(), b.StoreID, b.Volname, func(b *store.Backup) error {
			at, after := now.Unix(), now.Add(cfg.DeleteGrace).Unix()
			b.State, b.TombstoneAt, b.DeleteAfter, b.TombstoneReason, b.TombstoneBy = "tombstoned", &at, &after, "user", who
			return nil
		})
		if err != nil {
			return err
		}
		d.log.Info("offsite backup marked for deletion", "storage", b.StoreID, "volname", b.Volname,
			"delete_after", time.Unix(*b.DeleteAfter, 0))
		d.metaChanged(b)
		return api.WriteJSON(w, http.StatusOK, backupView(b))
	})
	d.api.HandleIdempotent("POST /v1/storages/{storage}/backups/{name}/undelete", func(w http.ResponseWriter, r *http.Request) error {
		b, err := d.backup(r)
		if err != nil {
			return err
		}
		if b.State != "tombstoned" {
			return api.Errorf(http.StatusConflict, apiv1.CodeConflict, "backup %s is not marked for deletion", b.Volname)
		}
		b, err = d.store.UpdateBackup(r.Context(), b.StoreID, b.Volname, func(b *store.Backup) error {
			b.State = "complete"
			if b.VerifyResult == "damaged" {
				b.State = "damaged"
			}
			b.TombstoneAt, b.DeleteAfter, b.TombstoneReason, b.TombstoneBy = nil, nil, "", ""
			return nil
		})
		if err != nil {
			return err
		}
		d.log.Info("offsite backup restored from deletion", "storage", b.StoreID, "volname", b.Volname)
		d.metaChanged(b)
		return api.WriteJSON(w, http.StatusOK, backupView(b))
	})
}

func (d *Daemon) metaChanged(b *store.Backup) {
	d.api.Events().Publish("backup.updated", backupView(b))
	select {
	case d.metaWake <- struct{}{}:
	default:
	}
}

// pushMeta writes changed notes, protection and tombstones to the remote
// meta documents, which survive the loss of the local database.
func (d *Daemon) pushMeta(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		d.pushDirty(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.metaWake:
		}
	}
}

func (d *Daemon) pushDirty(ctx context.Context) {
	rows, err := d.store.DirtyBackups(ctx, 100)
	if err != nil {
		if ctx.Err() == nil {
			d.log.Warn("list pending meta changes", "err", err)
		}
		return
	}
	for _, b := range rows {
		if err := d.pushOne(ctx, b); err != nil {
			if ctx.Err() != nil {
				return
			}
			d.log.Debug("push meta document", "storage", b.StoreID, "volname", b.Volname, "err", err)
		}
	}
}

// remoteID returns the generation and identity of a catalogue entry.
func remoteID(b *store.Backup) (int, layout.BackupID, error) {
	root, dir, ok := strings.Cut(b.RemoteDir, "/")
	gen, err := strconv.Atoi(strings.TrimPrefix(root, "g"))
	if !ok || err != nil || !strings.HasPrefix(root, "g") {
		return 0, layout.BackupID{}, fmt.Errorf("invalid remote directory %q", b.RemoteDir)
	}
	id, err := layout.ParseDir(dir)
	return gen, id, err
}

func (d *Daemon) pushOne(ctx context.Context, b *store.Backup) error {
	rp, _, err := d.storages.Repo(b.StoreID)
	if err != nil {
		return err
	}
	gen, id, err := remoteID(b)
	if err != nil {
		return err
	}
	meta, err := rp.ReadMeta(ctx, gen, id)
	if errors.Is(err, repo.ErrInvalidDocument) {
		meta, err = manifest.NewMeta(time.Now()), nil
	}
	if err != nil {
		return err
	}
	if meta.Tombstone != nil && meta.Tombstone.State == "deleting" {
		return fmt.Errorf("%s is being deleted", b.Volname)
	}
	meta.Notes, meta.Protected, meta.Tombstone = b.Notes, b.Protected, nil
	if b.State == "tombstoned" && b.DeleteAfter != nil {
		ts := &manifest.Tombstone{DeleteAfter: time.Unix(*b.DeleteAfter, 0).UTC(), Reason: b.TombstoneReason, By: b.TombstoneBy, State: "pending"}
		if b.TombstoneAt != nil {
			ts.RequestedAt = time.Unix(*b.TombstoneAt, 0).UTC()
		}
		meta.Tombstone = ts
	}
	if err := rp.WriteMeta(ctx, gen, id, meta); err != nil {
		return err
	}
	return d.store.MetaPushed(ctx, b.StoreID, b.Volname, b.MetaRev)
}
