// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

// progressInterval limits job.updated events for progress.
const progressInterval = time.Second

// Task is a runner's handle on a claimed job.
type Task struct {
	s *Scheduler

	mu        sync.Mutex
	job       *store.Job
	published time.Time
}

// Job returns a snapshot of the job.
func (t *Task) Job() store.Job {
	t.mu.Lock()
	defer t.mu.Unlock()
	return *t.job
}

// Advance moves the job to another state; illegal transitions are
// refused. mutate may change other fields in the same update.
func (t *Task) Advance(ctx context.Context, to, message string, mutate func(*store.Job)) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	from := t.job.State
	if !CanTransition(from, to) {
		return fmt.Errorf("jobs: illegal transition of job %d from %s to %s", t.job.ID, from, to)
	}
	j, err := t.s.opts.Store.UpdateJob(ctx, t.job.ID, []string{from}, message, func(j *store.Job) error {
		j.State = to
		if mutate != nil {
			mutate(j)
		}
		return nil
	})
	if err != nil {
		return err
	}
	t.job = j
	t.published = t.s.opts.Now()
	t.s.publish(j)
	return nil
}

// Update changes fields other than the state (progress, hash state). Its
// events are rate limited.
func (t *Task) Update(ctx context.Context, mutate func(*store.Job)) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	j, err := t.s.opts.Store.UpdateJob(ctx, t.job.ID, []string{t.job.State}, "", func(j *store.Job) error {
		state := j.State
		mutate(j)
		j.State = state
		return nil
	})
	if err != nil {
		return err
	}
	t.job = j
	if now := t.s.opts.Now(); now.Sub(t.published) >= progressInterval {
		t.published = now
		t.s.publish(j)
	}
	return nil
}

// Event records a message in the job's history.
func (t *Task) Event(ctx context.Context, level, message string) error {
	return t.s.opts.Store.AddJobEvent(ctx, t.Job().ID, level, message)
}
