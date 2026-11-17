// SPDX-License-Identifier: AGPL-3.0-or-later

package retention

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

// RecycleBinHold is how long OneDrive and Google Drive keep deleted files in
// their recycle bin, where they still count towards the quota.
const RecycleBinHold = 30 * 24 * time.Hour

// StoredSize returns the bytes a catalogued backup's segments take in the
// cloud.
func StoredSize(b store.BackupSize, encrypted bool) int64 {
	if !encrypted || b.SegmentCount < 1 {
		return b.ArchiveSize
	}
	full := int64(b.SegmentCount - 1)
	return full*transport.CryptStoredSize(b.SegmentSize) + transport.CryptStoredSize(b.ArchiveSize-full*b.SegmentSize)
}

type footprintKey struct {
	keep                   config.PruneOptions
	minAge, hold, interval time.Duration
	keepMin                int
}

var footprints sync.Map // footprintKey → int

// footprintBudget bounds the work of one simulation, in backups handled by
// its retention runs: it runs while PVE validates a storage change.
const footprintBudget = 1_000_000

// Footprint returns how many backups of a guest that is backed up every
// interval are stored at most, once retention has settled: those it keeps
// under keep and the rails rclone-min-age and rclone-keep-min, those it
// removed that still take space for hold (rclone-delete-grace, plus the
// provider's recycle bin when deletions use it), and the newest from when
// it is uploaded until retention next runs, once a day. It returns 0 when
// keep keeps every backup. Intervals are rounded to the hour, between an
// hour and a year.
//
// It simulates retention until every bucket has filled and then for a
// period of its coarsest bucket. A simulation that would take too long
// gives way to an upper bound, which may be well above the real number.
func Footprint(keep config.PruneOptions, minAge time.Duration, keepMin int, hold, interval time.Duration) int {
	if keep.IsKeepAll() {
		return 0
	}
	const day = 24 * time.Hour
	interval = min(max(interval.Round(time.Hour), time.Hour), 366*day)
	k := footprintKey{keep: keep, minAge: minAge, hold: hold, interval: interval, keepMin: keepMin}
	if n, ok := footprints.Load(k); ok {
		return n.(int)
	}
	n := simulate(keep, minAge, keepMin, hold, interval)
	footprints.Store(k, n)
	return n
}

func simulate(keep config.PruneOptions, minAge time.Duration, keepMin int, hold, interval time.Duration) int {
	const day = 24 * time.Hour
	// At a retention run, at most kept backups are kept: each keep option
	// and rclone-keep-min keep at most their count, rclone-min-age those
	// younger than it. Until the next run a day of new backups is added,
	// and those removed within hold are still held: as many as were added
	// meanwhile, plus however many fewer are kept than at most.
	kept := keep.KeepLast + keep.KeepHourly + keep.KeepDaily + keep.KeepWeekly + keep.KeepMonthly + keep.KeepYearly +
		keepMin + int(minAge/interval) + 1
	bound := 2*kept + int((hold+2*day)/interval) + 2

	// Long enough for every bucket to fill and the rails and grace periods
	// to pass, at most 20 years; then a period of the coarsest bucket.
	span := max(time.Duration(keep.KeepLast)*interval, time.Duration(keep.KeepHourly)*time.Hour,
		time.Duration(keep.KeepDaily)*day, time.Duration(keep.KeepWeekly)*7*day, time.Duration(keep.KeepMonthly)*31*day,
		time.Duration(keep.KeepYearly)*366*day, time.Duration(keepMin)*interval)
	span = min(span+minAge+hold+2*day, 20*366*day)
	cycle := day
	switch {
	case keep.KeepYearly > 0:
		cycle = 366 * day
	case keep.KeepMonthly > 0:
		cycle = 31 * day
	case keep.KeepWeekly > 0:
		cycle = 7 * day
	}
	start := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	settled := start.Add(span)
	end := settled.Add(max(cycle, interval) + minAge + hold + 2*day)

	var alive []*Entry
	var held []time.Time // when removed backups stop taking space, in order
	release := func(now time.Time) {
		n := 0
		for n < len(held) && !held[n].After(now) {
			n++
		}
		held = held[n:]
	}
	most, work := 0, 0
	nextBackup, nextPrune := start.Add(2*time.Hour), start.Add(5*time.Hour)
	for nextBackup.Before(end) {
		if nextPrune.Before(nextBackup) {
			now := nextPrune
			if work += len(alive); work > footprintBudget {
				return bound
			}
			release(now)
			for _, e := range alive {
				e.Mark, e.Reason = "", ""
			}
			MarkGroup(alive, keep, time.UTC)
			applyRails(alive, Rails{Now: now, MinAge: minAge, KeepMin: keepMin})
			next := make([]*Entry, 0, len(alive))
			for _, e := range alive {
				if e.Mark == MarkRemove {
					held = append(held, now.Add(hold))
				} else {
					next = append(next, e)
				}
			}
			alive = next
			nextPrune = nextPrune.Add(day)
			continue
		}
		now := nextBackup
		release(now)
		alive = append(alive, &Entry{CTime: now.Unix(), state: "complete", verified: true})
		if !now.Before(settled) {
			most = max(most, len(alive)+len(held))
		}
		nextBackup = nextBackup.Add(interval)
	}
	return most
}

// Estimate is what a storage's retention stores at most, from the
// backups in its catalogue.
type Estimate struct {
	Guests int
	// Bytes is what retention stores at most once it has settled: for
	// each guest, its Footprint times the size of its recent backups.
	Bytes int64
	// PerGuest is the largest Footprint of a guest.
	PerGuest int
	// Unlimited means prune-backups keeps every backup: nothing is
	// removed, and the backups grow by about DailyGrowth bytes a day.
	Unlimited   bool
	DailyGrowth int64
}

// EstimateSpace estimates what a storage's retention stores at most. sizes
// are the storage's catalogue; backupTimes adds the times of other
// backups of each guest ("qemu/100"), such as recent uploads, to tell
// how often guests are backed up. Each guest's backups are taken to be
// as large as the largest of its three newest, and as frequent as the
// median gap between its newest ones (daily if unknown).
func EstimateSpace(cfg *config.Storage, sizes []store.BackupSize, backupTimes map[string][]int64) Estimate {
	type guest struct {
		times []int64
		sizes []store.BackupSize // newest last
		live  bool
	}
	guests := map[string]*guest{}
	for _, b := range sizes {
		key := fmt.Sprintf("%s/%d", b.VMType, b.VMID)
		g := guests[key]
		if g == nil {
			g = &guest{}
			guests[key] = g
		}
		g.sizes = append(g.sizes, b)
		g.times = append(g.times, b.BackupTime)
		g.live = g.live || b.State == "complete" || b.State == "damaged"
	}
	keep := config.PruneOptions{KeepAll: true}
	if cfg.Base.PruneBackups != nil {
		keep = *cfg.Base.PruneBackups
	}
	hold := cfg.DeleteGrace
	if cfg.RecycleBin {
		hold += RecycleBinHold
	}
	est := Estimate{Unlimited: keep.IsKeepAll()}
	for key, g := range guests {
		if !g.live {
			continue // being deleted
		}
		est.Guests++
		slices.SortFunc(g.sizes, func(a, b store.BackupSize) int { return cmp.Compare(a.BackupTime, b.BackupTime) })
		var size int64
		for _, b := range g.sizes[max(len(g.sizes)-3, 0):] {
			size = max(size, StoredSize(b, cfg.Encryption == "crypt"))
		}
		interval := cadence(append(g.times, backupTimes[key]...))
		if est.Unlimited {
			est.DailyGrowth += int64(float64(size) * float64(24*time.Hour) / float64(interval))
			continue
		}
		n := Footprint(keep, cfg.MinAge, cfg.KeepMin, hold, interval)
		est.PerGuest = max(est.PerGuest, n)
		est.Bytes += int64(n) * size
	}
	return est
}

// cadence returns the median gap between a guest's newest backups, a day
// if there are too few.
func cadence(times []int64) time.Duration {
	slices.Sort(times)
	times = slices.Compact(times)
	times = times[max(len(times)-9, 0):]
	if len(times) < 2 {
		return 24 * time.Hour
	}
	gaps := make([]int64, 0, len(times)-1)
	for i := 1; i < len(times); i++ {
		gaps = append(gaps, times[i]-times[i-1])
	}
	slices.Sort(gaps)
	return time.Duration(gaps[len(gaps)/2]) * time.Second
}

// KeepString formats prune options like PVE ("keep-daily=7,keep-weekly=4").
func KeepString(p *config.PruneOptions) string {
	if p.IsKeepAll() {
		return "keep-all=1"
	}
	var parts []string
	for _, o := range []struct {
		name string
		n    int
	}{{"keep-last", p.KeepLast}, {"keep-hourly", p.KeepHourly}, {"keep-daily", p.KeepDaily}, {"keep-weekly", p.KeepWeekly},
		{"keep-monthly", p.KeepMonthly}, {"keep-yearly", p.KeepYearly}} {
		if o.n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", o.name, o.n))
		}
	}
	return strings.Join(parts, ",")
}
