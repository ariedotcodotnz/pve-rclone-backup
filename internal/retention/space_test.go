// SPDX-License-Identifier: AGPL-3.0-or-later

package retention

import (
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

const day = 24 * time.Hour

func TestFootprint(t *testing.T) {
	for _, c := range []struct {
		name     string
		keep     config.PruneOptions
		minAge   time.Duration
		keepMin  int
		hold, iv time.Duration
		want     int
	}{
		// Three kept, and the newest until retention runs.
		{"keep-last", config.PruneOptions{KeepLast: 3}, 0, 0, 0, day, 4},
		// 7 days and 4 more weeks: 11, and the newest.
		{"daily and weekly", config.PruneOptions{KeepDaily: 7, KeepWeekly: 4}, 0, 0, 0, day, 12},
		// The reported setup, with the default rails: one backup a day is
		// removed and held for the 7 days of rclone-delete-grace.
		{"defaults", config.PruneOptions{KeepDaily: 7, KeepWeekly: 4}, 7 * day, 1, 7 * day, day, 19},
		// Hourly backups are removed once a day: up to a day of them waits.
		{"hourly backups", config.PruneOptions{KeepDaily: 2}, 0, 0, 0, time.Hour, 26},
		// The recycle bin holds a removed backup for 30 days.
		{"recycle bin", config.PruneOptions{KeepLast: 1}, 0, 0, 30 * day, day, 32},
		// Weekly backups: the fourth newest is removed a week after its
		// successor, and held for a week.
		{"weekly backups", config.PruneOptions{KeepLast: 3}, 7 * day, 1, 7 * day, 7 * day, 5},
		// rclone-min-age keeps the 7 daily backups younger than a week,
		// whatever keep-last says, and the newest.
		{"min-age", config.PruneOptions{KeepLast: 1}, 7 * day, 0, 0, day, 8},
		{"keep-all", config.PruneOptions{KeepAll: true}, 0, 0, 0, day, 0},
	} {
		if got := Footprint(c.keep, c.minAge, c.keepMin, c.hold, c.iv); got != c.want {
			t.Errorf("%s: %d backups, want %d", c.name, got, c.want)
		}
	}
}

// TestFootprintIsBounded: Footprint runs while PVE validates a storage
// change, so its work is bounded. A heavy but usual policy is simulated
// within the bound; one too heavy to simulate gives way to the bound.
func TestFootprintIsBounded(t *testing.T) {
	heavy := config.PruneOptions{KeepHourly: 24, KeepDaily: 7, KeepWeekly: 4, KeepMonthly: 12, KeepYearly: 5}
	// A week of hourly backups (rclone-min-age), 37 days of removed ones
	// held, and the buckets; below the bound of 2*222+39*24+2.
	if n := Footprint(heavy, 7*day, 1, 7*day+RecycleBinHold, time.Hour); n < 168+37*24 || n >= 2*222+39*24+2 {
		t.Fatalf("hourly backups kept for years: %d backups", n)
	}
	if n := Footprint(config.PruneOptions{KeepHourly: 20000}, 0, 0, 0, time.Hour); n != 2*(20000+1)+48+2 {
		t.Fatalf("too heavy to simulate: %d backups, want the bound", n)
	}
}

func TestEstimateSpace(t *testing.T) {
	keep := &config.PruneOptions{KeepDaily: 7, KeepWeekly: 4}
	cfg := &config.Storage{Base: config.BaseOptions{PruneBackups: keep}, MinAge: 7 * day, KeepMin: 1, DeleteGrace: 7 * day, Encryption: "none"}
	at := func(d int) int64 { return time.Date(2026, 10, d, 21, 0, 0, 0, time.UTC).Unix() }
	var sizes []store.BackupSize
	// Guest 100: nightly, growing to 100 GiB. Guest 101: one backup, 80 GiB,
	// and the times of earlier uploads, a week apart. Guest 102 is being
	// deleted.
	for d := 1; d <= 10; d++ {
		sizes = append(sizes, store.BackupSize{VMType: "qemu", VMID: 100, BackupTime: at(d), State: "complete",
			ArchiveSize: int64(90+d) << 30, SegmentSize: 1 << 30, SegmentCount: 90 + d})
	}
	sizes = append(sizes,
		store.BackupSize{VMType: "lxc", VMID: 101, BackupTime: at(22), State: "complete", ArchiveSize: 80 << 30, SegmentSize: 1 << 30, SegmentCount: 80},
		store.BackupSize{VMType: "qemu", VMID: 102, BackupTime: at(3), State: "tombstoned", ArchiveSize: 500 << 30, SegmentSize: 1 << 30, SegmentCount: 500})
	times := map[string][]int64{"lxc/101": {at(1), at(8), at(15)}}
	est := EstimateSpace(cfg, sizes, times)
	weekly := Footprint(*keep, 7*day, 1, 7*day, 7*day)
	if est.Guests != 2 || est.PerGuest != 19 || est.Bytes != 19*(100<<30)+int64(weekly)*(80<<30) || est.Unlimited {
		t.Fatalf("estimate = %+v (weekly footprint %d)", est, weekly)
	}
	// Encrypted segments are a little larger.
	cfg.Encryption = "crypt"
	if enc := EstimateSpace(cfg, sizes, times); enc.Bytes <= est.Bytes {
		t.Fatalf("encrypted estimate %d <= %d", enc.Bytes, est.Bytes)
	}
	if got := StoredSize(sizes[0], true); got != 90*transport.CryptStoredSize(1<<30)+transport.CryptStoredSize(1<<30) {
		t.Fatalf("stored size = %d", got)
	}
	// Without prune-backups nothing is removed: about a backup a day of
	// guest 100 and a seventh of guest 101's.
	cfg.Encryption, cfg.Base.PruneBackups = "none", nil
	if est := EstimateSpace(cfg, sizes, times); !est.Unlimited || est.DailyGrowth != 100<<30+(80<<30)/7 {
		t.Fatalf("unlimited estimate = %+v", est)
	}
}

func TestKeepString(t *testing.T) {
	if got := KeepString(&config.PruneOptions{KeepDaily: 7, KeepWeekly: 4}); got != "keep-daily=7,keep-weekly=4" {
		t.Errorf("KeepString = %q", got)
	}
	if got := KeepString(nil); got != "keep-all=1" {
		t.Errorf("KeepString(nil) = %q", got)
	}
}
