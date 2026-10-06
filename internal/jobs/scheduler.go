// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

// ErrSourceLost is returned by runners when the local archive vanished or
// changed; such jobs end as source_lost.
var ErrSourceLost = errors.New("source archive vanished or changed")

// ErrPermanent marks failures that retrying cannot fix; such jobs fail.
var ErrPermanent = errors.New("permanent failure")

// Permanent marks err as a permanent failure.
func Permanent(err error) error { return fmt.Errorf("%w: %w", ErrPermanent, err) }

// PostponeError asks the scheduler to run a job again later without
// counting a failed attempt (e.g. an archive that is still settling).
type PostponeError struct {
	Until  time.Time
	Reason string
}

func (e *PostponeError) Error() string { return "postponed: " + e.Reason }

// Postpone returns a PostponeError.
func Postpone(until time.Time, reason string) error {
	return &PostponeError{Until: until, Reason: reason}
}

var (
	errCancelled = errors.New("job cancelled by the operator")
	errShutdown  = errors.New("daemon stopping")
)

// Runner executes a claimed job. It advances the job through its active
// states with the Task and returns nil once the job is complete.
type Runner interface {
	Run(ctx context.Context, t *Task) error
}

// Options configures a Scheduler.
type Options struct {
	Log   *slog.Logger
	Store *store.Store
	Node  string
	Kind  string // job kind this scheduler runs (default replicate)
	// Workers caps concurrent jobs on this node (default 2).
	Workers int
	// Targets returns the storages jobs may run for; their rclone-transfers
	// caps concurrent jobs per storage.
	Targets func() []*config.Storage
	// Ready, if set, reports whether a storage can run jobs now (its
	// repository is open); jobs of other storages wait.
	Ready    func(storeID string) error
	Runner   Runner
	OnUpdate func(apiv1.JobUpdate)
	Now      func() time.Time
	Rand     func() float64
	// Poll is how often runnable jobs are looked for without a wake-up
	// (default 5s).
	Poll         time.Duration
	LeaseSeconds int64 // default 300
}

// Scheduler claims and runs jobs.
type Scheduler struct {
	opts Options
	log  *slog.Logger
	wake chan struct{}
	wg   sync.WaitGroup

	mu      sync.Mutex
	running map[int64]*runningJob
	paused  map[string]Pause // by remote
	served  string           // storage of the job started last (round-robin)
}

type runningJob struct {
	storeID string
	cancel  context.CancelCauseFunc
}

// Pause holds back all jobs of a remote.
type Pause struct {
	Until  time.Time
	Reason string
}

// New returns a scheduler.
func New(opts Options) *Scheduler {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	opts.Kind = cmp.Or(opts.Kind, "replicate")
	opts.Workers = cmp.Or(opts.Workers, 2)
	opts.Poll = cmp.Or(opts.Poll, 5*time.Second)
	opts.LeaseSeconds = cmp.Or(opts.LeaseSeconds, int64(300))
	return &Scheduler{opts: opts, log: opts.Log, wake: make(chan struct{}, 1),
		running: map[int64]*runningJob{}, paused: map[string]Pause{}}
}

// Wake makes the scheduler look for runnable jobs now.
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Scheduler) publish(j *store.Job) {
	if s.opts.OnUpdate != nil {
		s.opts.OnUpdate(apiv1.JobUpdate{ID: j.ID, State: j.State, ProgressBytes: j.ProgressBytes, TotalBytes: j.TotalBytes})
	}
}

// Run claims and executes jobs until ctx is cancelled; running jobs are
// then interrupted and returned to the queue.
func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(s.opts.Poll)
	defer t.Stop()
	for {
		s.fill(ctx)
		select {
		case <-ctx.Done():
			s.mu.Lock()
			for _, r := range s.running {
				r.cancel(errShutdown)
			}
			s.mu.Unlock()
			s.wg.Wait()
			return
		case <-t.C:
		case <-s.wake:
		}
	}
}

// fill starts jobs while capacity is free.
func (s *Scheduler) fill(ctx context.Context) {
	for ctx.Err() == nil {
		storeIDs := s.eligibleStorages()
		if len(storeIDs) == 0 {
			return
		}
		var job *store.Job
		for _, id := range storeIDs {
			j, err := s.opts.Store.ClaimJob(ctx, store.ClaimOptions{Kind: s.opts.Kind,
				States: []string{StateQueued, StateRetryWait}, ToState: StatePreparing, Owner: s.opts.Node,
				LeaseSeconds: s.opts.LeaseSeconds, StoreIDs: []string{id}})
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				if ctx.Err() == nil {
					s.log.Warn("claim job", "err", err)
				}
				return
			}
			job = j
			break
		}
		if job == nil {
			return
		}
		s.start(ctx, job)
	}
}

// eligibleStorages returns storages that may start a job now, rotated for
// round-robin fairness; nil when the node is at capacity.
func (s *Scheduler) eligibleStorages() []string {
	targets := s.opts.Targets()
	now := s.opts.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.running) >= s.opts.Workers {
		return nil
	}
	perStorage := map[string]int{}
	for _, r := range s.running {
		perStorage[r.storeID]++
	}
	// Round-robin over the configured storages, starting after the one
	// served last. Rotating the filtered list instead would let storages
	// that come and go with free capacity shift the turn and starve others.
	first := 0
	for i, t := range targets {
		if t.ID == s.served {
			first = i + 1
			break
		}
	}
	var ids []string
	for k := range targets {
		t := targets[(first+k)%len(targets)]
		if p, ok := s.paused[t.Remote]; ok && now.Before(p.Until) {
			continue
		}
		if s.opts.Ready != nil && s.opts.Ready(t.ID) != nil {
			continue
		}
		if perStorage[t.ID] < max(t.Transfers, 1) {
			ids = append(ids, t.ID)
		}
	}
	return ids
}

func (s *Scheduler) remoteOf(storeID string) string {
	for _, t := range s.opts.Targets() {
		if t.ID == storeID {
			return t.Remote
		}
	}
	return ""
}

func (s *Scheduler) start(ctx context.Context, job *store.Job) {
	jctx, cancel := context.WithCancelCause(ctx)
	attempt := &runningJob{storeID: job.StoreID, cancel: cancel}
	s.mu.Lock()
	s.running[job.ID] = attempt
	s.served = job.StoreID
	s.mu.Unlock()
	s.publish(job)
	s.wg.Go(func() {
		defer func() {
			s.mu.Lock()
			// A retry may already run the job again once its outcome is
			// recorded: only this attempt's entry is removed.
			if s.running[job.ID] == attempt {
				delete(s.running, job.ID)
			}
			s.mu.Unlock()
			cancel(nil)
			s.Wake()
		}()
		task := &Task{s: s, job: job}
		stopLease := s.keepLease(jctx, job.ID)
		var err error
		if s.opts.Runner == nil {
			err = errors.New("no job runner configured")
		} else {
			err = s.opts.Runner.Run(jctx, task)
		}
		stopLease()
		s.finish(task, err, context.Cause(jctx))
		if testHookFinished != nil {
			testHookFinished(job.ID)
		}
	})
}

// testHookFinished, set by tests, runs after a job's outcome is recorded
// and before its worker is cleaned up.
var testHookFinished func(id int64)

// keepLease renews the job's lease while it runs.
func (s *Scheduler) keepLease(ctx context.Context, id int64) func() {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		t := time.NewTicker(time.Duration(s.opts.LeaseSeconds) * time.Second / 5)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.opts.Store.RenewLease(context.WithoutCancel(ctx), id, s.opts.Node, s.opts.LeaseSeconds); err != nil {
					s.log.Warn("renew job lease", "job", id, "err", err)
				}
			}
		}
	})
	return func() { close(done); wg.Wait() }
}

// finish applies the policy to a runner's outcome.
func (s *Scheduler) finish(t *Task, err, cause error) {
	ctx := context.Background()
	now := s.opts.Now()
	j := t.Job()
	remote := s.remoteOf(j.StoreID)
	var (
		state   string
		message string
		update  func(*store.Job)
	)
	pe, isPostpone := errors.AsType[*PostponeError](err)
	switch {
	case err == nil:
		state, message = StateComplete, "replication complete"
		update = func(j *store.Job) { j.ErrorClass, j.LastError, j.NextAttemptAt = "", "", nil }
		s.clearAlerts(ctx, j, remote)
	case errors.Is(cause, errCancelled):
		state, message = StateCancelled, errCancelled.Error()
	case errors.Is(cause, errShutdown) || errors.Is(err, context.Canceled):
		state, message = StateQueued, "interrupted: "+errShutdown.Error()
	case errors.Is(err, ErrSourceLost):
		state, message = StateSourceLost, err.Error()
		update = func(j *store.Job) { j.ErrorClass, j.LastError = "source_lost", truncate(err.Error()) }
	case errors.Is(err, ErrPermanent):
		state, message = StateFailed, err.Error()
		update = func(j *store.Job) {
			j.ErrorClass, j.LastError, j.Attempts = "permanent", truncate(err.Error()), j.Attempts+1
		}
	case isPostpone:
		state, message = StateRetryWait, pe.Error()
		update = func(j *store.Job) {
			at := pe.Until.Unix()
			j.ErrorClass, j.LastError, j.NextAttemptAt, j.LeaseUntil = "", pe.Reason, &at, nil
		}
	default:
		class := transport.Classify(err)
		state, message, update = s.retryPolicy(ctx, j, remote, class, err, now)
	}
	if state == StateComplete || state == StateCancelled || state == StateSourceLost || state == StateFailed {
		prev := update
		update = func(j *store.Job) {
			if prev != nil {
				prev(j)
			}
			at := now.Unix()
			j.FinishedAt, j.LeaseUntil = &at, nil
		}
	}
	if j.State == state {
		// The runner already moved the job there (e.g. complete).
		if update != nil {
			_ = t.Update(ctx, update)
		}
		return
	}
	if aerr := t.Advance(ctx, state, message, update); aerr != nil {
		s.log.Error("record job outcome", "job", j.ID, "state", state, "err", aerr)
		return
	}
	level := slog.LevelInfo
	if state != StateComplete && state != StateQueued {
		level = slog.LevelWarn
	}
	s.log.Log(ctx, level, "job finished", "job", j.ID, "storage", j.StoreID, "volname", j.BackupVolname,
		"state", state, "message", message)
}

func truncate(s string) string {
	const limit = 2000
	if len(s) > limit {
		return s[:limit] + "…"
	}
	return s
}

// retryPolicy decides what a failed attempt leads to.
func (s *Scheduler) retryPolicy(ctx context.Context, j store.Job, remote string, class transport.Class, err error, now time.Time) (string, string, func(*store.Job)) {
	msg := truncate(err.Error())
	setErr := func(attempts int, next time.Time) func(*store.Job) {
		return func(j *store.Job) {
			j.ErrorClass, j.LastError, j.Attempts = string(class), msg, attempts
			at := next.Unix()
			j.NextAttemptAt, j.LeaseUntil = &at, nil
		}
	}
	switch class {
	case transport.ClassAuth:
		until := now.Add(authPause)
		s.pause(remote, until, "the remote needs to be reconnected")
		s.raise(ctx, "auth:"+remote, "error", j.StoreID, fmt.Sprintf(
			"Remote %q rejected its credentials; uploads are paused. Reconnect it with 'pve-rclone-backup remote reconnect %s'.", remote, remote))
		return StateRetryWait, "paused: " + msg, setErr(j.Attempts, until)
	case transport.ClassQuota:
		until := now.Add(quotaPause)
		s.pause(remote, until, "the remote is full")
		s.raise(ctx, "quota:"+remote, "error", j.StoreID, fmt.Sprintf(
			"Remote %q is full; uploads are paused and retried every %s. Free space or empty the provider's recycle bin.", remote, quotaPause))
		return StateRetryWait, "paused: " + msg, setErr(j.Attempts, until)
	case transport.ClassIntegrity, transport.ClassConfig:
		return StateFailed, msg, setErr(j.Attempts+1, now)
	case transport.ClassUnknown, transport.ClassNotFound:
		if j.Attempts+1 >= maxUnknownAttempts {
			return StateFailed, fmt.Sprintf("giving up after %d attempts: %s", j.Attempts+1, msg), setErr(j.Attempts+1, now)
		}
	}
	// Transient, throttled, local I/O and (bounded) unknown errors.
	attempts := j.Attempts + 1
	next := now.Add(Backoff(attempts, s.opts.Rand))
	if j.StartedAt != nil && now.Sub(time.Unix(*j.StartedAt, 0)) > stallAlertAfter {
		s.raise(ctx, fmt.Sprintf("stalled:%d", j.ID), "warning", j.StoreID, fmt.Sprintf(
			"Upload of %s to %s has been failing for over a day: %s", j.BackupVolname, j.StoreID, msg))
	}
	return StateRetryWait, fmt.Sprintf("attempt %d failed, retrying at %s: %s", attempts, next.Format(time.DateTime), msg),
		setErr(attempts, next)
}

func (s *Scheduler) raise(ctx context.Context, id, severity, storeID, message string) {
	if err := s.opts.Store.RaiseAlert(ctx, store.Alert{ID: id, Severity: severity, StoreID: storeID, Message: message}); err != nil {
		s.log.Warn("raise alert", "alert", id, "err", err)
	}
}

func (s *Scheduler) clearAlerts(ctx context.Context, j store.Job, remote string) {
	ids := []string{fmt.Sprintf("stalled:%d", j.ID)}
	if remote != "" {
		ids = append(ids, "auth:"+remote, "quota:"+remote)
		s.ResumeRemote(remote)
	}
	for _, id := range ids {
		if err := s.opts.Store.ClearAlert(ctx, id); err != nil {
			s.log.Warn("clear alert", "alert", id, "err", err)
		}
	}
}

func (s *Scheduler) pause(remote string, until time.Time, reason string) {
	if remote == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.paused[remote]; !ok || p.Until.Before(until) {
		s.log.Warn("pausing uploads to remote", "remote", remote, "until", until, "reason", reason)
		s.paused[remote] = Pause{Until: until, Reason: reason}
	}
}

// ResumeRemote lifts a pause (after the remote was reconnected).
func (s *Scheduler) ResumeRemote(remote string) {
	s.mu.Lock()
	_, ok := s.paused[remote]
	delete(s.paused, remote)
	s.mu.Unlock()
	if ok {
		s.log.Info("resuming uploads to remote", "remote", remote)
		s.Wake()
	}
}

// Paused returns the current pauses by remote.
func (s *Scheduler) Paused() map[string]Pause {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.opts.Now()
	out := map[string]Pause{}
	for r, p := range s.paused {
		if now.Before(p.Until) {
			out[r] = p
		}
	}
	return out
}

// ErrWrongState reports an operator action that does not apply to the
// job's current state.
var ErrWrongState = errors.New("the job is not in a state that allows this")

// Cancel stops a job: running jobs are interrupted, waiting jobs are
// cancelled at once.
func (s *Scheduler) Cancel(ctx context.Context, id int64) error {
	s.mu.Lock()
	r, ok := s.running[id]
	s.mu.Unlock()
	if ok {
		r.cancel(errCancelled)
		return nil
	}
	j, err := s.opts.Store.UpdateJob(ctx, id, []string{StateQueued, StateRetryWait}, errCancelled.Error(), func(j *store.Job) error {
		at := s.opts.Now().Unix()
		j.State, j.FinishedAt, j.NextAttemptAt = StateCancelled, &at, nil
		return nil
	})
	if errors.Is(err, store.ErrStateConflict) {
		return fmt.Errorf("%w: %w", ErrWrongState, err)
	}
	if err == nil {
		s.publish(j)
	}
	return err
}

// Retry queues a finished job again.
func (s *Scheduler) Retry(ctx context.Context, id int64) error {
	j, err := s.opts.Store.UpdateJob(ctx, id, []string{StateFailed, StateCancelled, StateSkipped, StateSuperseded},
		"retry requested by the operator", func(j *store.Job) error {
			now := s.opts.Now().Unix()
			j.State, j.Attempts, j.ErrorClass, j.LastError = StateQueued, 0, "", ""
			j.NextAttemptAt, j.FinishedAt = &now, nil
			return nil
		})
	if errors.Is(err, store.ErrStateConflict) {
		return fmt.Errorf("%w: %w", ErrWrongState, err)
	}
	if err != nil {
		return err
	}
	s.publish(j)
	s.Wake()
	return nil
}

// SetPriority changes a job's priority (higher runs first).
func (s *Scheduler) SetPriority(ctx context.Context, id int64, priority int) error {
	_, err := s.opts.Store.UpdateJob(ctx, id, nil, "", func(j *store.Job) error {
		j.Priority = priority
		return nil
	})
	if err == nil {
		s.Wake()
	}
	return err
}

// Running returns the IDs of jobs being executed.
func (s *Scheduler) Running() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int64, 0, len(s.running))
	for id := range s.running {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
