// SPDX-License-Identifier: AGPL-3.0-or-later

package retention

import (
	"strings"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

var utc = time.UTC

// backup returns a complete, verified catalogue entry of guest 100 at a
// UTC time "2006-01-02 15:04".
func backup(at string) *store.Backup {
	t, err := time.ParseInLocation("2006-01-02 15:04", at, utc)
	if err != nil {
		panic(err)
	}
	label := t.Format("2006_01_02-15_04_05")
	return &store.Backup{Volname: "backup/vzdump-qemu-100-" + label + ".vma.zst", VMType: "qemu", VMID: 100,
		BackupTime: t.Unix(), TSLabel: label, State: "complete", VerifyLevel: 2}
}

// marks returns "time:mark" for each entry, newest first.
func marks(entries []*Entry) string {
	var out []string
	for _, e := range entries {
		out = append(out, time.Unix(e.CTime, 0).In(utc).Format("01-02 15:04")+":"+e.Mark)
	}
	return strings.Join(out, " ")
}

func plan(bs []*store.Backup, keep config.PruneOptions) []*Entry {
	return Plan(bs, keep, Rails{Now: time.Date(2030, 1, 1, 0, 0, 0, 0, utc)}, 0, "", utc)
}

func TestMarkLikePVE(t *testing.T) {
	week := []*store.Backup{
		backup("2026-10-05 10:00"), backup("2026-10-05 02:00"), backup("2026-10-04 02:00"),
		backup("2026-10-03 14:00"), backup("2026-10-03 02:00"), backup("2026-09-27 02:00"), backup("2026-08-30 02:00"),
	}
	for _, c := range []struct {
		name string
		keep config.PruneOptions
		want string
	}{
		{"keep-last", config.PruneOptions{KeepLast: 3},
			"10-05 10:00:keep 10-05 02:00:keep 10-04 02:00:keep 10-03 14:00:remove 10-03 02:00:remove 09-27 02:00:remove 08-30 02:00:remove"},
		{"keep-daily", config.PruneOptions{KeepDaily: 2},
			"10-05 10:00:keep 10-05 02:00:remove 10-04 02:00:keep 10-03 14:00:remove 10-03 02:00:remove 09-27 02:00:remove 08-30 02:00:remove"},
		// keep-last covers 10-05, so the two daily slots go to 10-04 and 10-03.
		{"last and daily", config.PruneOptions{KeepLast: 1, KeepDaily: 2},
			"10-05 10:00:keep 10-05 02:00:remove 10-04 02:00:keep 10-03 14:00:keep 10-03 02:00:remove 09-27 02:00:remove 08-30 02:00:remove"},
		{"weekly", config.PruneOptions{KeepWeekly: 2},
			"10-05 10:00:keep 10-05 02:00:remove 10-04 02:00:keep 10-03 14:00:remove 10-03 02:00:remove 09-27 02:00:remove 08-30 02:00:remove"},
		{"monthly", config.PruneOptions{KeepMonthly: 3},
			"10-05 10:00:keep 10-05 02:00:remove 10-04 02:00:remove 10-03 14:00:remove 10-03 02:00:remove 09-27 02:00:keep 08-30 02:00:keep"},
		{"hourly", config.PruneOptions{KeepHourly: 2},
			"10-05 10:00:keep 10-05 02:00:keep 10-04 02:00:remove 10-03 14:00:remove 10-03 02:00:remove 09-27 02:00:remove 08-30 02:00:remove"},
		{"yearly", config.PruneOptions{KeepYearly: 1},
			"10-05 10:00:keep 10-05 02:00:remove 10-04 02:00:remove 10-03 14:00:remove 10-03 02:00:remove 09-27 02:00:remove 08-30 02:00:remove"},
		{"keep-all", config.PruneOptions{KeepAll: true, KeepLast: 1},
			"10-05 10:00:keep 10-05 02:00:keep 10-04 02:00:keep 10-03 14:00:keep 10-03 02:00:keep 09-27 02:00:keep 08-30 02:00:keep"},
		{"no options", config.PruneOptions{},
			"10-05 10:00:keep 10-05 02:00:keep 10-04 02:00:keep 10-03 14:00:keep 10-03 02:00:keep 09-27 02:00:keep 08-30 02:00:keep"},
	} {
		if got := marks(plan(week, c.keep)); got != c.want {
			t.Errorf("%s:\n got %s\nwant %s", c.name, got, c.want)
		}
	}
}

func TestISOWeeksAcrossTheYear(t *testing.T) {
	// 2026-12-31 (Thu) and 2027-01-01 (Fri) are both in ISO week 53/2026.
	bs := []*store.Backup{backup("2027-01-01 02:00"), backup("2026-12-31 02:00"), backup("2026-12-27 02:00")}
	if got := marks(plan(bs, config.PruneOptions{KeepWeekly: 2})); got != "01-01 02:00:keep 12-31 02:00:remove 12-27 02:00:keep" {
		t.Fatalf("weeks = %s", got)
	}
}

func TestProtectedAndRenamed(t *testing.T) {
	prot := backup("2026-10-05 02:00")
	prot.Protected = true
	renamed := backup("2026-10-04 02:00")
	renamed.Volname = strings.Replace(renamed.Volname, ".vma.zst", ".1.vma.zst", 1)
	bs := []*store.Backup{prot, renamed, backup("2026-10-03 02:00"), backup("2026-10-02 02:00")}
	// Protected backups do not take a keep slot, as in PVE.
	if got := marks(plan(bs, config.PruneOptions{KeepLast: 1})); got != "10-05 02:00:protected 10-04 02:00:renamed 10-03 02:00:keep 10-02 02:00:remove" {
		t.Fatalf("marks = %s", got)
	}
}

func TestGroupsAndFilters(t *testing.T) {
	ct := backup("2026-10-05 02:00")
	ct.VMType, ct.VMID, ct.Volname = "lxc", 200, "backup/vzdump-lxc-200-2026_10_05-02_00_00.tar.zst"
	tomb := backup("2026-10-06 02:00")
	tomb.State = "tombstoned"
	bs := []*store.Backup{backup("2026-10-05 02:00"), backup("2026-10-04 02:00"), ct, tomb}
	got := plan(bs, config.PruneOptions{KeepLast: 1})
	if len(got) != 3 {
		t.Fatalf("entries = %d", len(got))
	}
	if got := Plan(bs, config.PruneOptions{KeepLast: 1}, Rails{}, 200, "", utc); len(got) != 1 || got[0].Mark != MarkKeep {
		t.Fatalf("guest filter = %+v", got)
	}
	if got := Plan(bs, config.PruneOptions{KeepLast: 1}, Rails{}, 0, "qemu", utc); len(got) != 2 {
		t.Fatalf("type filter = %+v", got)
	}
}

func TestRails(t *testing.T) {
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, utc)
	bs := []*store.Backup{backup("2026-10-05 02:00"), backup("2026-10-04 02:00"), backup("2026-10-01 02:00"), backup("2026-09-20 02:00")}
	keep := config.PruneOptions{KeepLast: 1}

	got := Plan(bs, keep, Rails{Now: now, MinAge: 3 * 24 * time.Hour}, 0, "", utc)
	if marks(got) != "10-05 02:00:keep 10-04 02:00:keep 10-01 02:00:remove 09-20 02:00:remove" || got[1].Reason == "" {
		t.Fatalf("min-age: %s", marks(got))
	}
	got = Plan(bs, keep, Rails{Now: now, KeepMin: 3}, 0, "", utc)
	if marks(got) != "10-05 02:00:keep 10-04 02:00:keep 10-01 02:00:keep 09-20 02:00:remove" {
		t.Fatalf("keep-min: %s", marks(got))
	}
	got = Plan(bs, keep, Rails{Now: now, MaxDeletes: 1}, 0, "", utc)
	if marks(got) != "10-05 02:00:keep 10-04 02:00:keep 10-01 02:00:keep 09-20 02:00:remove" {
		t.Fatalf("max-deletes removes the oldest first: %s", marks(got))
	}

	// Unverified backups do not count towards keep-min.
	unverified := []*store.Backup{backup("2026-10-05 02:00"), backup("2026-10-04 02:00")}
	unverified[0].VerifyLevel = 0
	got = Plan(unverified, keep, Rails{Now: now, KeepMin: 1}, 0, "", utc)
	if marks(got) != "10-05 02:00:keep 10-04 02:00:keep" {
		t.Fatalf("keep-min with an unverified newest backup: %s", marks(got))
	}

	// The newest backup is damaged: the newest good one stays.
	damaged := []*store.Backup{backup("2026-10-05 02:00"), backup("2026-10-04 02:00"), backup("2026-10-03 02:00")}
	damaged[0].State = "damaged"
	got = Plan(damaged, keep, Rails{Now: now}, 0, "", utc)
	if marks(got) != "10-05 02:00:keep 10-04 02:00:keep 10-03 02:00:remove" || !strings.Contains(got[1].Reason, "damaged") {
		t.Fatalf("damaged newest: %s (%q)", marks(got), got[1].Reason)
	}
}

func TestLocalTimeBuckets(t *testing.T) {
	// 23:30 and 00:30 UTC are the same day in UTC-2 but not in UTC.
	loc := time.FixedZone("UTC-2", -2*3600)
	bs := []*store.Backup{backup("2026-10-05 00:30"), backup("2026-10-04 23:30")}
	if got := marks(Plan(bs, config.PruneOptions{KeepDaily: 2}, Rails{}, 0, "", utc)); got != "10-05 00:30:keep 10-04 23:30:keep" {
		t.Fatalf("UTC days: %s", got)
	}
	if got := marks(Plan(bs, config.PruneOptions{KeepDaily: 2}, Rails{}, 0, "", loc)); got != "10-05 00:30:keep 10-04 23:30:remove" {
		t.Fatalf("local days: %s", got)
	}
}
