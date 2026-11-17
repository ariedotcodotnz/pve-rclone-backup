// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/catalog"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/jobs"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/pve/cluster"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/retention"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/textfmt"
)

// Uploads that never finish leave their segments in the cloud: one whose
// local archive vanished (source_lost), a job that failed or was cancelled,
// or one whose state database was lost. They take quota but are never
// listed. Each resync finds them as backup directories without a manifest;
// the daemon records them, removes them once incomplete-upload-grace has
// passed, and lets the operator list and purge them.

// incompleteFrom returns the directories without a manifest a resync
// found.
func incompleteFrom(rep *catalog.Report) []store.IncompleteUpload {
	var out []store.IncompleteUpload
	for _, b := range rep.Pending {
		state := "incomplete"
		if b.State == repo.StateDeleting {
			state = "deleting"
		}
		u := store.IncompleteUpload{RemoteDir: fmt.Sprintf("g%d/%s", b.Generation, b.ID.Dir()), State: state,
			VMType: b.ID.VMType, VMID: b.ID.VMID}
		if ts, err := time.ParseInLocation("2006_01_02-15_04_05", b.ID.TSLabel, time.Local); err == nil {
			u.BackupTime = ts.Unix() // like the upload job's
		}
		var names []string
		for _, f := range b.Files {
			if f.Dir {
				continue
			}
			u.Files++
			u.StoredBytes += f.StoredSize
			names = append(names, f.Name+" "+strconv.FormatInt(f.StoredSize, 10))
		}
		slices.Sort(names)
		sum := sha256.Sum256([]byte(strings.Join(names, "\n")))
		u.Fingerprint = hex.EncodeToString(sum[:8])
		out = append(out, u)
	}
	return out
}

// recordIncomplete records the incomplete uploads a resync found and
// removes those whose time has come.
func (d *Daemon) recordIncomplete(storeID string, rep *catalog.Report) {
	ctx := context.Background()
	if err := d.store.ReplaceIncomplete(ctx, storeID, incompleteFrom(rep), time.Now().Unix()); err != nil {
		d.log.Warn("record incomplete uploads", "storage", storeID, "err", err)
		return
	}
	cfg, ok := d.storages.Config(storeID)
	if !ok {
		return
	}
	list, err := d.incompleteUploads(ctx, cfg)
	if err != nil {
		d.log.Warn("list incomplete uploads", "storage", storeID, "err", err)
		return
	}
	now := time.Now()
	for _, u := range list {
		if u.RemoveAfter != nil && !now.Before(*u.RemoveAfter) {
			reason := "incomplete upload unchanged for incomplete-upload-grace"
			if u.JobState == jobs.StateSourceLost {
				reason = "incomplete upload of an archive that is gone"
			}
			_, _ = d.queuePurge(ctx, storeID, u, reason, false) // failures are logged
		}
	}
}

// incompleteUpload is an incomplete upload as the API shows it, with the
// fingerprint of its files.
type incompleteUpload struct {
	apiv1.IncompleteUpload
	fingerprint string
}

// incompleteUploads returns a storage's incomplete uploads with this
// node's upload jobs of them and when they are removed automatically.
func (d *Daemon) incompleteUploads(ctx context.Context, cfg *config.Storage) ([]incompleteUpload, error) {
	list, err := d.store.ListIncomplete(ctx, cfg.ID)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	jobList, err := d.store.ReplicateJobs(ctx, cfg.ID)
	if err != nil {
		return nil, err
	}
	type guestKey struct {
		vmtype string
		vmid   int
		time   int64
	}
	byGuest := map[guestKey][]store.GuestJob{} // newest first
	for _, j := range jobList {
		k := guestKey{j.VMType, j.VMID, j.BackupTime}
		byGuest[k] = append(byGuest[k], j)
	}
	// Uploads are removed automatically only from storages that upload
	// (whose source is this installation's), never from immutable ones.
	auto := !d.incompleteGrace.Off && len(cfg.ReplicateFrom) > 0 && !cfg.Immutable
	// Another node's upload jobs are not known here: without a job of
	// this node, an upload is only taken as abandoned on a single node.
	alone := d.standalone()
	out := make([]incompleteUpload, 0, len(list))
	for _, u := range list {
		v := apiv1.IncompleteUpload{Dir: u.RemoteDir, State: u.State, VMType: u.VMType, VMID: u.VMID, CTime: u.BackupTime,
			Files: u.Files, Bytes: u.StoredBytes, FirstSeenAt: time.Unix(u.FirstSeenAt, 0).UTC(),
			UnchangedSince: time.Unix(u.ChangedAt, 0).UTC()}
		js := byGuest[guestKey{u.VMType, u.VMID, u.BackupTime}]
		for i, j := range js {
			if !slices.Contains(finishedJobStates, j.State) {
				v.Job, v.JobState, v.InUse = j.ID, j.State, true
				break
			}
			if i == 0 {
				v.Job, v.JobState = j.ID, j.State
			}
		}
		var at time.Time
		switch {
		case !auto || v.InUse || u.State != "incomplete":
			// Interrupted deletions are finished by delete jobs.
		case v.JobState == jobs.StateSourceLost:
			at = time.Unix(u.FirstSeenAt, 0) // it can never complete
		case v.Job != 0 || alone:
			at = time.Unix(u.ChangedAt, 0).Add(d.incompleteGrace.Duration)
		}
		if !at.IsZero() {
			at = at.UTC()
			v.RemoveAfter = &at
		}
		out = append(out, incompleteUpload{v, u.Fingerprint})
	}
	return out, nil
}

// finishedJobStates are the states of jobs that no longer upload.
var finishedJobStates = []string{jobs.StateComplete, jobs.StateSkipped, jobs.StateSuperseded, jobs.StateCancelled,
	jobs.StateSourceLost, jobs.StateFailed}

// standalone reports whether this node is the only one of its cluster.
func (d *Daemon) standalone() bool {
	m, err := cluster.ReadMembers(d.opts.PVEDir, d.opts.Node)
	return err == nil && len(m.All) == 1
}

// uploadInUse reports whether an upload job of this node works on a backup
// directory (of any collision index).
func (d *Daemon) uploadInUse(ctx context.Context, storeID string, id layout.BackupID) bool {
	ts, err := time.ParseInLocation("2006_01_02-15_04_05", id.TSLabel, time.Local)
	if err != nil {
		return true
	}
	list, err := d.store.ReplicateJobs(ctx, storeID)
	if err != nil {
		return true
	}
	return slices.ContainsFunc(list, func(j store.GuestJob) bool {
		return j.VMType == id.VMType && j.VMID == id.VMID && j.BackupTime == ts.Unix() && !slices.Contains(finishedJobStates, j.State)
	})
}

// queuePurge queues the removal of an incomplete upload and returns the
// job's ID. A removal of the same files that finished before without
// removing them (an upload worked on them then) is queued again, unless
// the operator cancelled it and this is not their request.
func (d *Daemon) queuePurge(ctx context.Context, storeID string, u incompleteUpload, reason string, operator bool) (int64, error) {
	pj, _ := json.Marshal(retention.DeleteParams{RemoteDir: u.Dir, Reason: reason, Incomplete: true})
	// The fingerprint tells a later upload into the same directory from
	// this one.
	id, created, err := d.store.InsertJob(ctx, &store.Job{Kind: "delete", StoreID: storeID, State: jobs.StateQueued,
		DedupeKey: "purge:" + storeID + ":" + u.Dir + ":" + u.fingerprint, OwnerNode: d.opts.Node, VMType: u.VMType, VMID: u.VMID,
		BackupTime: u.CTime, ParamsJSON: string(pj)})
	if err == nil && !created {
		again := []string{jobs.StateComplete, jobs.StateFailed}
		if operator {
			again = append(again, jobs.StateCancelled)
		}
		_, err = d.store.UpdateJob(ctx, id, again, "queued again: "+reason, func(j *store.Job) error {
			now := time.Now().Unix()
			j.State, j.Attempts, j.ErrorClass, j.LastError, j.ParamsJSON = jobs.StateQueued, 0, "", "", string(pj)
			j.NextAttemptAt, j.FinishedAt = &now, nil
			return nil
		})
		created = err == nil
		if errors.Is(err, store.ErrStateConflict) {
			err = nil // queued or running already, or cancelled
		}
	}
	if err != nil {
		d.log.Warn("queue removal of an incomplete upload", "storage", storeID, "dir", u.Dir, "err", err)
		return 0, err
	}
	if created {
		d.log.Info("removing incomplete upload", "storage", storeID, "dir", u.Dir, "bytes", u.Bytes, "reason", reason)
		d.api.Events().Publish("job.updated", apiv1.JobUpdate{ID: id, State: jobs.StateQueued})
		d.deletes.Wake()
	}
	return id, nil
}

// purged forgets an incomplete upload once it was removed.
func (d *Daemon) purged(storeID, remoteDir string) {
	if err := d.store.DeleteIncomplete(context.Background(), storeID, remoteDir); err != nil {
		d.log.Warn("forget removed incomplete upload", "storage", storeID, "dir", remoteDir, "err", err)
	}
	d.spaceFreed(storeID)
}

func (d *Daemon) incompleteRoutes() {
	d.api.Handle("GET /v1/storages/{storage}/incomplete", func(w http.ResponseWriter, r *http.Request) error {
		id := r.PathValue("storage")
		cfg, ok := d.storages.Config(id)
		if !ok {
			return notConfigured(id)
		}
		list, err := d.incompleteUploads(r.Context(), cfg)
		if err != nil {
			return err
		}
		out := make([]apiv1.IncompleteUpload, 0, len(list))
		for _, u := range list {
			out = append(out, u.IncompleteUpload)
		}
		return api.WriteJSON(w, http.StatusOK, out)
	})
	d.api.HandleIdempotent("POST /v1/storages/{storage}/incomplete/purge", func(w http.ResponseWriter, r *http.Request) error {
		id := r.PathValue("storage")
		cfg, ok := d.storages.Config(id)
		if !ok {
			return notConfigured(id)
		}
		var req apiv1.PurgeIncompleteRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return err
		}
		if cfg.Immutable {
			return api.Errorf(http.StatusForbidden, apiv1.CodeImmutable, "storage %s is immutable", id)
		}
		if len(cfg.ReplicateFrom) == 0 {
			return api.Errorf(http.StatusConflict, apiv1.CodeConflict,
				"storage %s does not upload (rclone-replicate-from is empty): its incomplete uploads belong to the installation that uploads to source %s", id, cfg.Source)
		}
		list, err := d.incompleteUploads(r.Context(), cfg)
		if err != nil {
			return err
		}
		byDir := map[string]incompleteUpload{}
		for _, u := range list {
			byDir[u.Dir] = u
		}
		var chosen []incompleteUpload
		if len(req.Dirs) == 0 {
			for _, u := range list {
				if !u.InUse {
					chosen = append(chosen, u)
				}
			}
		}
		for _, dir := range req.Dirs {
			u, ok := byDir[dir]
			switch {
			case !ok:
				return api.NotFound("no incomplete upload %s on storage %s (as of its last resync)", dir, id)
			case u.InUse:
				return api.Errorf(http.StatusConflict, apiv1.CodeConflict, "upload job %d works on %s; cancel it first", u.Job, dir)
			}
			chosen = append(chosen, u)
		}
		out := make([]apiv1.PurgedUpload, 0, len(chosen))
		for _, u := range chosen {
			p := apiv1.PurgedUpload{IncompleteUpload: u.IncompleteUpload}
			if !req.DryRun {
				if p.PurgeJob, err = d.queuePurge(r.Context(), id, u, "incomplete upload purged by the operator", true); err != nil {
					return err
				}
			}
			out = append(out, p)
		}
		return api.WriteJSON(w, http.StatusOK, out)
	})
}

// incompleteSpace adds a storage's incomplete uploads to its space and
// warnings.
func (d *Daemon) incompleteSpace(ctx context.Context, s *apiv1.Storage) {
	cfg, ok := d.storages.Config(s.ID)
	if !ok || s.Space == nil {
		return
	}
	list, err := d.incompleteUploads(ctx, cfg)
	if err != nil {
		d.log.Warn("list incomplete uploads", "storage", s.ID, "err", err)
		return
	}
	sp := s.Space
	for _, u := range list {
		if u.InUse {
			sp.InProgress++
			sp.InProgressBytes += u.Bytes
		} else {
			sp.Incomplete++
			sp.IncompleteBytes += u.Bytes
		}
	}
	switch {
	case sp.Incomplete == 1:
		s.Warnings = append(s.Warnings, fmt.Sprintf("an upload that never finished takes %s in the cloud; see 'pve-rclone-backup storage incomplete list %s'",
			textfmt.Bytes(sp.IncompleteBytes), s.ID))
	case sp.Incomplete > 1:
		s.Warnings = append(s.Warnings, fmt.Sprintf("%d uploads that never finished take %s in the cloud; see 'pve-rclone-backup storage incomplete list %s'",
			sp.Incomplete, textfmt.Bytes(sp.IncompleteBytes), s.ID))
	}
}
