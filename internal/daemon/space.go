// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/jobs"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/retention"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/textfmt"
)

// storageSpace returns what a storage's catalogued backups take in the
// cloud.
func storageSpace(sizes []store.BackupSize, encrypted bool) *apiv1.StorageSpace {
	sp := &apiv1.StorageSpace{}
	for _, b := range sizes {
		n := retention.StoredSize(b, encrypted)
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

// storedNow returns everything a storage stores in the cloud.
func storedNow(sp *apiv1.StorageSpace) int64 {
	return sp.BackupBytes + sp.TombstonedBytes + sp.IncompleteBytes + sp.InProgressBytes
}

// storageViews completes the API views of storages with what the daemon
// knows beyond the storage manager: the recovery kit, the space their
// backups take, and whether their retention fits in their cloud accounts.
// override, if set, replaces the configuration of its storage (a change
// being validated).
func (d *Daemon) storageViews(ctx context.Context, list []apiv1.Storage, override *config.Storage) []apiv1.Storage {
	type account struct {
		free         *int64
		stored, need int64
		growth       int64
		members      []int
		estimates    []retention.Estimate
	}
	accounts := map[string]*account{}
	var order []string
	for i := range list {
		s := &list[i]
		s.KitConfirmed = d.kitConfirmed(*s)
		cfg, ok := d.storages.Config(s.ID)
		enabled := s.Enabled
		if override != nil && override.ID == s.ID {
			cfg, ok = override, true
			enabled = !cfg.Base.Disable && (len(cfg.Base.Nodes) == 0 || slices.Contains(cfg.Base.Nodes, d.opts.Node))
		}
		if !ok {
			continue
		}
		sizes, err := d.store.BackupSizes(ctx, s.ID)
		if err != nil {
			d.log.Warn("sizes of catalogued backups", "storage", s.ID, "err", err)
			continue
		}
		s.Space = storageSpace(sizes, s.Encryption == "crypt")
		d.incompleteSpace(ctx, s)
		if !enabled {
			continue
		}
		est := retention.EstimateSpace(cfg, sizes, d.backupTimes(ctx, s.ID))
		a := accounts[s.Remote]
		if a == nil {
			a = &account{}
			accounts[s.Remote] = a
			order = append(order, s.Remote)
		}
		if u := s.Usage; u != nil && u.Free != nil {
			a.free = u.Free
		}
		a.members, a.estimates = append(a.members, i), append(a.estimates, est)
		switch {
		case cfg.Immutable, len(cfg.ReplicateFrom) == 0:
			// It deletes nothing, or it does not upload: nothing it
			// stores is counted on to make room.
		case est.Unlimited:
			// Its backups are never removed; only what is being deleted
			// or never finished uploading makes room.
			a.stored += s.Space.TombstonedBytes + s.Space.IncompleteBytes
		default:
			a.stored += storedNow(s.Space)
		}
		a.need += est.Bytes
		a.growth += est.DailyGrowth
	}
	for _, remote := range order {
		a := accounts[remote]
		if a.free == nil {
			continue
		}
		avail := max(*a.free, 0) + a.stored
		for k, i := range a.members {
			s, est := &list[i], a.estimates[k]
			if est.Guests == 0 {
				continue
			}
			fit := &apiv1.RetentionFit{PerGuest: est.PerGuest, NeedBytes: est.Bytes, TotalNeedBytes: a.need,
				AvailableBytes: avail, Unlimited: est.Unlimited, DailyGrowthBytes: est.DailyGrowth}
			fit.Fits = !est.Unlimited && a.need <= avail
			s.Space.Retention = fit
			var others []string // the other storages that need space too
			for l, j := range a.members {
				if j != i && a.estimates[l].Bytes > 0 {
					others = append(others, list[j].ID)
				}
			}
			cfg, _ := d.storages.Config(s.ID)
			if override != nil && override.ID == s.ID {
				cfg = override
			}
			if w := retentionWarning(cfg, fit, *a.free, a.growth, others); w != "" {
				s.Warnings = append(s.Warnings, w)
			}
		}
	}
	return list
}

// backupTimes returns the backup times of this node's uploads to a storage
// by guest ("qemu/100"), which tell how often guests are backed up beyond
// what retention left in the catalogue.
func (d *Daemon) backupTimes(ctx context.Context, storeID string) map[string][]int64 {
	list, err := d.store.ReplicateJobs(ctx, storeID)
	if err != nil {
		d.log.Warn("upload jobs", "storage", storeID, "err", err)
		return nil
	}
	out := map[string][]int64{}
	for _, j := range list {
		if j.State == jobs.StateSkipped || j.State == jobs.StateSuperseded || j.BackupTime == 0 {
			continue // never meant to be uploaded
		}
		key := fmt.Sprintf("%s/%d", j.VMType, j.VMID)
		out[key] = append(out[key], j.BackupTime)
	}
	return out
}

// fullSoon is how close to full an account must get before backups that
// are never removed are warned about.
const fullSoon = 30 * 24 * time.Hour

// How retention warnings begin.
const (
	doesNotFit            = "retention does not fit in the cloud account"
	neverRemovedPrune     = "prune-backups is not set, so offsite backups are never removed"
	neverRemovedImmutable = "rclone-immutable is set, so offsite backups are never removed"
)

// retentionWarning explains why a storage's retention does not fit in its
// cloud account, or "" if it does. free is the account's free space,
// growth what backups never removed add to it a day, and others the
// other storages on the account.
func retentionWarning(cfg *config.Storage, fit *apiv1.RetentionFit, free, growth int64, others []string) string {
	if fit.Unlimited {
		if growth <= 0 || time.Duration(float64(max(free, 0))/float64(growth)*float64(24*time.Hour)) >= fullSoon {
			return ""
		}
		why, fix := neverRemovedPrune, "Set prune-backups on the storage, or free space"
		if cfg.Immutable {
			why, fix = neverRemovedImmutable, "Free space"
		}
		return fmt.Sprintf("%s: at about %s a day, the cloud account is full in %s. %s", why,
			textfmt.Bytes(growth), days(time.Duration(float64(max(free, 0))/float64(growth)*float64(24*time.Hour))), fix)
	}
	if fit.Fits {
		return ""
	}
	settings := "prune-backups " + retention.KeepString(cfg.Base.PruneBackups) + ", rclone-min-age " + days(cfg.MinAge) +
		", rclone-delete-grace " + days(cfg.DeleteGrace)
	if cfg.RecycleBin {
		settings += " plus " + days(retention.RecycleBinHold) + " in the recycle bin (rclone-recycle-bin)"
	}
	need := "about " + textfmt.Bytes(fit.NeedBytes)
	if len(others) > 0 && fit.TotalNeedBytes != fit.NeedBytes {
		need += fmt.Sprintf(", %s together with %s on the same account", textfmt.Bytes(fit.TotalNeedBytes), strings.Join(slices.Sorted(slices.Values(others)), ", "))
	}
	return fmt.Sprintf(doesNotFit+": it stores up to %d backups of a guest at once (%s), %s, but the account has about %s for backups (%s free, %s stored now). Keep fewer backups, shorten rclone-min-age or rclone-delete-grace, or free space",
		fit.PerGuest, settings, need, textfmt.Bytes(fit.AvailableBytes), textfmt.Bytes(max(free, 0)), textfmt.Bytes(fit.AvailableBytes-max(free, 0)))
}

// days formats a duration in days where it is a whole number of them.
func days(d time.Duration) string {
	const day = 24 * time.Hour
	switch {
	case d == 0:
		return "0"
	case d%day == 0:
		return fmt.Sprintf("%dd", d/day)
	case d >= day:
		return fmt.Sprintf("%.1fd", float64(d)/float64(day))
	}
	return d.String()
}

// retentionAdvice warns, for a storage configuration being validated, if
// its retention does not fit in its cloud account. It uses the catalogue
// and the last known quota, without network access.
func (d *Daemon) retentionAdvice(ctx context.Context, cfg *config.Storage) []string {
	// The catalogue of a storage bound to another repository or source
	// says nothing about its new one; a new storage has none.
	cur, ok := d.storages.Config(cfg.ID)
	if !ok || cur.Remote != cfg.Remote || cur.Path != cfg.Path || cur.Encryption != cfg.Encryption || cur.Source != cfg.Source {
		return nil
	}
	list := d.storageViews(ctx, d.storages.List(), cfg)
	for _, s := range list {
		if s.ID != cfg.ID || s.Space == nil || s.Space.Retention == nil {
			continue
		}
		for _, w := range s.Warnings {
			if strings.HasPrefix(w, doesNotFit) || strings.HasPrefix(w, neverRemovedPrune) || strings.HasPrefix(w, neverRemovedImmutable) {
				return []string{w}
			}
		}
	}
	return nil
}
