// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"testing"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

func TestStorageSpace(t *testing.T) {
	// The segments of a backup, encrypted one by one.
	segs := func(size, seg int64) int64 {
		var n int64
		for off := int64(0); off < size || off == 0; off += seg {
			n += transport.CryptStoredSize(min(seg, size-off))
			if size == 0 {
				break
			}
		}
		return n
	}
	sizes := []store.BackupSize{
		{State: "complete", ArchiveSize: 5<<30 + 12345, SegmentSize: 1 << 30, SegmentCount: 6},
		{State: "damaged", ArchiveSize: 100 << 20, SegmentSize: 1000000, SegmentCount: 105},
		{State: "complete", ArchiveSize: 0, SegmentSize: 1 << 30, SegmentCount: 1},
		{State: "tombstoned", ArchiveSize: 3 << 30, SegmentSize: 1 << 30, SegmentCount: 3},
	}
	sp := storageSpace(sizes, true)
	want := segs(5<<30+12345, 1<<30) + segs(100<<20, 1000000) + segs(0, 1<<30)
	if sp.Backups != 3 || sp.BackupBytes != want || sp.Tombstoned != 1 || sp.TombstonedBytes != segs(3<<30, 1<<30) {
		t.Fatalf("encrypted space = %+v, want %d backup bytes", sp, want)
	}
	if sp := storageSpace(sizes, false); sp.BackupBytes != 5<<30+12345+100<<20 || sp.StoredBytes() != sp.BackupBytes+3<<30 {
		t.Fatalf("plain space = %+v", sp)
	}
}
