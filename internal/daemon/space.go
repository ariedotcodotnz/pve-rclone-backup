// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"context"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

// storedSize returns the bytes a backup's segments take in the cloud.
func storedSize(b store.BackupSize, encrypted bool) int64 {
	if !encrypted || b.SegmentCount < 1 {
		return b.ArchiveSize
	}
	full := int64(b.SegmentCount - 1)
	return full*transport.CryptStoredSize(b.SegmentSize) + transport.CryptStoredSize(b.ArchiveSize-full*b.SegmentSize)
}

// storageSpace returns what a storage's catalogued backups take in the
// cloud.
func storageSpace(sizes []store.BackupSize, encrypted bool) *apiv1.StorageSpace {
	sp := &apiv1.StorageSpace{}
	for _, b := range sizes {
		n := storedSize(b, encrypted)
		switch b.State {
		case "tombstoned", "deleting":
			sp.Tombstoned++
			sp.TombstonedBytes += n
		default:
			sp.Backups++
			sp.BackupBytes += n
		}
	}
	return sp
}

// storageView completes the API view of a storage with what the daemon
// knows beyond the storage manager: the recovery kit and the space its
// backups take.
func (d *Daemon) storageView(ctx context.Context, s apiv1.Storage) apiv1.Storage {
	s.KitConfirmed = d.kitConfirmed(s)
	sizes, err := d.store.BackupSizes(ctx, s.ID)
	if err != nil {
		d.log.Warn("sizes of catalogued backups", "storage", s.ID, "err", err)
		return s
	}
	s.Space = storageSpace(sizes, s.Encryption == "crypt")
	d.incompleteSpace(ctx, &s)
	return s
}
