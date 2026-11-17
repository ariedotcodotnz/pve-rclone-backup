// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
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

func TestRetentionWarning(t *testing.T) {
	const day = 24 * time.Hour
	cfg := &config.Storage{Base: config.BaseOptions{PruneBackups: &config.PruneOptions{KeepDaily: 7, KeepWeekly: 4}},
		MinAge: 7 * day, DeleteGrace: 7 * day}
	// The reported case: about 180 GiB a night against 750 GiB of space.
	fit := &apiv1.RetentionFit{PerGuest: 19, NeedBytes: 19 * 180 << 30, TotalNeedBytes: 19 * 180 << 30, AvailableBytes: 750 << 30}
	want := "retention does not fit in the cloud account: it stores up to 19 backups of a guest at once (prune-backups " +
		"keep-daily=7,keep-weekly=4, rclone-min-age 7d, rclone-delete-grace 7d), about 3.3 TiB, but the account has about " +
		"750.0 GiB for backups (300.0 GiB free, 450.0 GiB stored now). Keep fewer backups, shorten rclone-min-age or " +
		"rclone-delete-grace, or free space"
	if got := retentionWarning(cfg, fit, 300<<30, 0, nil); got != want {
		t.Fatalf("warning =\n%s\nwant\n%s", got, want)
	}
	// Another storage on the account that needs nothing is not mentioned.
	if got := retentionWarning(cfg, fit, 300<<30, 0, []string{"office"}); strings.Contains(got, "together") {
		t.Fatalf("warning with a storage that needs nothing = %s", got)
	}
	cfg.RecycleBin = true
	fit.TotalNeedBytes += 100 << 30
	if got := retentionWarning(cfg, fit, 300<<30, 0, []string{"office"}); !strings.Contains(got,
		"rclone-delete-grace 7d plus 30d in the recycle bin (rclone-recycle-bin)), about 3.3 TiB, 3.4 TiB together with office on the same account") {
		t.Fatalf("shared warning = %s", got)
	}
	fit.Fits = true
	if got := retentionWarning(cfg, fit, 300<<30, 0, nil); got != "" {
		t.Fatalf("warning for retention that fits = %s", got)
	}
	// Without prune-backups: a warning once the account is full within 30 days.
	unlimited := &apiv1.RetentionFit{Unlimited: true}
	if got := retentionWarning(cfg, unlimited, 300<<30, 10<<30, nil); got != "" {
		t.Fatalf("warning 30 days ahead = %s", got)
	}
	if got := retentionWarning(cfg, unlimited, 300<<30, 180<<30, nil); got !=
		"prune-backups is not set, so offsite backups are never removed: at about 180.0 GiB a day, the cloud account is full in 1.7d. Set prune-backups on the storage, or free space" {
		t.Fatalf("unlimited warning = %s", got)
	}
	cfg.Immutable = true
	if got := retentionWarning(cfg, unlimited, 300<<30, 180<<30, nil); got !=
		"rclone-immutable is set, so offsite backups are never removed: at about 180.0 GiB a day, the cloud account is full in 1.7d. Free space" {
		t.Fatalf("immutable warning = %s", got)
	}
	if days(7*day) != "7d" || days(36*time.Hour) != "1.5d" || days(time.Hour) != "1h0m0s" || days(0) != "0" {
		t.Fatal("days")
	}
}
