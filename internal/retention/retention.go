// SPDX-License-Identifier: AGPL-3.0-or-later

// Package retention decides which offsite backups a prune removes. The
// marking is a port of PVE's prune_mark_backup_group, so previews in the
// PVE GUI and the CLI match; safety rails specific to offsite copies are
// applied on top. Removal only tombstones a backup: it is deleted after
// the storage's grace period.
package retention

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

// Marks, as in PVE.
const (
	MarkKeep      = "keep"
	MarkRemove    = "remove"
	MarkProtected = "protected"
	MarkRenamed   = "renamed" // not a standard vzdump name: never pruned
)

// Entry is a backup considered by a prune.
type Entry struct {
	Volname string
	VMType  string
	VMID    int
	CTime   int64
	Mark    string
	// Reason explains a removal kept by a safety rail.
	Reason string

	state    string
	verified bool
}

// MarkGroup marks the backups of one guest like PVE's
// prune_mark_backup_group. Entries already marked (protected) keep their
// mark; times are bucketed in loc, PVE's local time.
func MarkGroup(group []*Entry, keep config.PruneOptions, loc *time.Location) {
	positive := keep.KeepLast > 0 || keep.KeepHourly > 0 || keep.KeepDaily > 0 || keep.KeepWeekly > 0 ||
		keep.KeepMonthly > 0 || keep.KeepYearly > 0
	if keep.KeepAll || !positive {
		for _, e := range group {
			if e.Mark == "" || e.Mark == MarkRemove {
				e.Mark = MarkKeep
			}
		}
		return
	}
	list := slices.Clone(group)
	slices.SortStableFunc(list, func(a, b *Entry) int { return cmp.Compare(b.CTime, a.CTime) })
	at := func(ctime int64) time.Time { return time.Unix(ctime, 0).In(loc) }
	mark(list, keep.KeepLast, func(c int64) string { return strconv.FormatInt(c, 10) })
	mark(list, keep.KeepHourly, func(c int64) string { t := at(c); return t.Format("2006-01-02 15") })
	mark(list, keep.KeepDaily, func(c int64) string { return at(c).Format("2006-01-02") })
	mark(list, keep.KeepWeekly, func(c int64) string {
		y, w := at(c).ISOWeek()
		return fmt.Sprintf("%d/%d", w, y)
	})
	mark(list, keep.KeepMonthly, func(c int64) string { return at(c).Format("2006-01") })
	mark(list, keep.KeepYearly, func(c int64) string { return strconv.Itoa(at(c).Year()) })
	for _, e := range list {
		if e.Mark == "" {
			e.Mark = MarkRemove
		}
	}
}

// mark is PVE's $prune_mark: keep the newest entry of up to count buckets
// not already covered by a kept entry.
func mark(list []*Entry, count int, id func(int64) string) {
	if count <= 0 {
		return
	}
	already := map[string]bool{}
	for _, e := range list {
		if e.Mark == MarkKeep {
			already[id(e.CTime)] = true
		}
	}
	newly := map[string]bool{}
	for _, e := range list {
		k := id(e.CTime)
		if e.Mark != "" || already[k] {
			continue
		}
		if !newly[k] {
			if len(newly) >= count {
				break
			}
			newly[k] = true
			e.Mark = MarkKeep
		} else {
			e.Mark = MarkRemove
		}
	}
}

// Rails are the safety limits applied to a prune.
type Rails struct {
	Now        time.Time
	MinAge     time.Duration // never remove younger backups
	KeepMin    int           // keep at least this many complete, verified backups per guest
	MaxDeletes int           // remove at most this many backups per run (0: no limit)
}

// Plan marks a storage's backups. vmid and vmtype restrict the prune to a
// guest or guest type (0 and "" for all). Tombstoned backups are already
// being removed and are left out.
func Plan(backups []*store.Backup, keep config.PruneOptions, rails Rails, vmid int, vmtype string, loc *time.Location) []*Entry {
	var all []*Entry
	groups := map[string][]*Entry{}
	for _, b := range backups {
		if b.State == "tombstoned" || b.State == "deleting" || vmid != 0 && b.VMID != vmid || vmtype != "" && b.VMType != vmtype {
			continue
		}
		e := &Entry{Volname: b.Volname, VMType: b.VMType, VMID: b.VMID, CTime: b.BackupTime, state: b.State,
			verified: b.State == "complete" && b.VerifyLevel >= 2}
		if a, err := layout.ParseVolname(b.Volname); err != nil || a.Collision != 0 {
			e.Mark = MarkRenamed
		} else {
			key := b.VMType + "/" + strconv.Itoa(b.VMID)
			groups[key] = append(groups[key], e)
		}
		if b.Protected {
			e.Mark = MarkProtected
		}
		all = append(all, e)
	}
	for _, g := range groups {
		MarkGroup(g, keep, loc)
		applyRails(g, rails)
	}
	capDeletes(all, rails.MaxDeletes)
	slices.SortStableFunc(all, func(a, b *Entry) int {
		return cmp.Or(cmp.Compare(a.VMType, b.VMType), cmp.Compare(a.VMID, b.VMID), cmp.Compare(b.CTime, a.CTime))
	})
	return all
}

func keepBecause(e *Entry, reason string) {
	e.Mark, e.Reason = MarkKeep, reason
}

// applyRails turns removals that would break a safety limit into keeps.
func applyRails(group []*Entry, r Rails) {
	list := slices.Clone(group)
	slices.SortStableFunc(list, func(a, b *Entry) int { return cmp.Compare(b.CTime, a.CTime) })
	if r.MinAge > 0 {
		for _, e := range list {
			if e.Mark == MarkRemove && r.Now.Sub(time.Unix(e.CTime, 0)) < r.MinAge {
				keepBecause(e, "younger than rclone-min-age")
			}
		}
	}
	// While the newest backup is damaged, keep the newest good one.
	if len(list) > 0 && list[0].state == "damaged" {
		for _, e := range list[1:] {
			if e.state == "complete" {
				if e.Mark == MarkRemove {
					keepBecause(e, "newest good copy while a newer backup is damaged")
				}
				break
			}
		}
	}
	kept := 0
	for _, e := range list {
		if e.verified && (e.Mark == MarkKeep || e.Mark == MarkProtected) {
			kept++
		}
	}
	for _, e := range list {
		if kept >= r.KeepMin {
			break
		}
		if e.verified && e.Mark == MarkRemove {
			keepBecause(e, "rclone-keep-min")
			kept++
		}
	}
}

// capDeletes limits removals per run, removing the oldest first.
func capDeletes(all []*Entry, max int) {
	if max <= 0 {
		return
	}
	var removals []*Entry
	for _, e := range all {
		if e.Mark == MarkRemove {
			removals = append(removals, e)
		}
	}
	if len(removals) <= max {
		return
	}
	slices.SortStableFunc(removals, func(a, b *Entry) int { return cmp.Compare(a.CTime, b.CTime) })
	for _, e := range removals[max:] {
		keepBecause(e, "rclone-max-deletes reached for this run")
	}
}
