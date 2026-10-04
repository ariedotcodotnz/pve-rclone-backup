// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/jobs"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

func (d *Daemon) jobRoutes() {
	d.api.Handle("GET /v1/jobs", func(w http.ResponseWriter, r *http.Request) error {
		q := r.URL.Query()
		f := store.JobFilter{Kind: q.Get("kind"), StoreID: q.Get("storage"), Limit: 500}
		if v := q.Get("state"); v != "" {
			f.States = strings.Split(v, ",")
		}
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 10000 {
				return api.Invalid("invalid limit %q", v)
			}
			f.Limit = n
		}
		list, err := d.store.ListJobs(r.Context(), f)
		if err != nil {
			return err
		}
		out := make([]apiv1.Job, 0, len(list))
		for _, j := range list {
			out = append(out, jobView(j))
		}
		return api.WriteJSON(w, http.StatusOK, out)
	})
	d.api.Handle("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) error {
		j, err := d.job(r)
		if err != nil {
			return err
		}
		detail := apiv1.JobDetail{Job: jobView(j), Segments: []apiv1.JobSegment{}, Events: []apiv1.JobEvent{}}
		segs, err := d.store.Segments(r.Context(), j.ID)
		if err != nil {
			return err
		}
		for _, s := range segs {
			detail.Segments = append(detail.Segments, apiv1.JobSegment{Index: s.Index, Offset: s.Offset, Size: s.Size,
				State: s.State, SHA256: s.SHA256, StoredSize: s.StoredSize})
		}
		events, err := d.store.JobEvents(r.Context(), j.ID)
		if err != nil {
			return err
		}
		for _, e := range events {
			detail.Events = append(detail.Events, apiv1.JobEvent{Time: time.Unix(e.TS, 0).UTC(), Level: e.Level,
				FromState: e.FromState, ToState: e.ToState, Message: e.Message})
		}
		return api.WriteJSON(w, http.StatusOK, detail)
	})
	action := func(do func(r *http.Request, j *store.Job) error) api.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) error {
			j, err := d.job(r)
			if err != nil {
				return err
			}
			if err := do(r, j); err != nil {
				if errors.Is(err, jobs.ErrWrongState) {
					return api.Errorf(http.StatusConflict, apiv1.CodeConflict, "job %d is %s", j.ID, j.State)
				}
				return err
			}
			j, err = d.store.GetJob(r.Context(), j.ID)
			if err != nil {
				return err
			}
			return api.WriteJSON(w, http.StatusOK, jobView(j))
		}
	}
	d.api.HandleIdempotent("POST /v1/jobs/{id}/cancel", action(func(r *http.Request, j *store.Job) error {
		return d.schedulerFor(j.Kind).Cancel(r.Context(), j.ID)
	}))
	d.api.HandleIdempotent("POST /v1/jobs/{id}/retry", action(func(r *http.Request, j *store.Job) error {
		return d.schedulerFor(j.Kind).Retry(r.Context(), j.ID)
	}))
	d.api.HandleIdempotent("POST /v1/jobs/{id}/priority", action(func(r *http.Request, j *store.Job) error {
		var req struct {
			Priority *int `json:"priority"`
		}
		if err := api.DecodeJSON(r, &req); err != nil {
			return err
		}
		if req.Priority == nil || *req.Priority < -100 || *req.Priority > 100 {
			return api.Invalid("priority must be between -100 and 100")
		}
		return d.schedulerFor(j.Kind).SetPriority(r.Context(), j.ID, *req.Priority)
	}))
}

func (d *Daemon) job(r *http.Request) (*store.Job, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return nil, api.Invalid("invalid job ID %q", r.PathValue("id"))
	}
	j, err := d.store.GetJob(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, api.NotFound("job %d not found", id)
	}
	return j, err
}

func jobView(j *store.Job) apiv1.Job {
	return apiv1.Job{
		ID: j.ID, Kind: j.Kind, Storage: j.StoreID, State: j.State, Priority: j.Priority, Volname: j.BackupVolname,
		SourceStorage: j.SourceStorage, SourcePath: j.SourcePath, VMType: j.VMType, VMID: j.VMID, BackupTime: j.BackupTime,
		Attempts: j.Attempts, NextAttemptAt: unixTime(j.NextAttemptAt), ErrorClass: j.ErrorClass, LastError: j.LastError,
		ProgressBytes: j.ProgressBytes, TotalBytes: j.TotalBytes, NextSegment: j.NextSegment, OwnerNode: j.OwnerNode,
		CreatedAt: time.Unix(j.CreatedAt, 0).UTC(), UpdatedAt: time.Unix(j.UpdatedAt, 0).UTC(),
		StartedAt: unixTime(j.StartedAt), FinishedAt: unixTime(j.FinishedAt),
	}
}
