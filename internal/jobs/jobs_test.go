// SPDX-License-Identifier: AGPL-3.0-or-later

package jobs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rclone/rclone/lib/pacer"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

func TestTransitions(t *testing.T) {
	for _, c := range []struct {
		from, to string
		ok       bool
	}{
		{StateQueued, StatePreparing, true},
		{StateRetryWait, StatePreparing, true},
		{StatePreparing, StateComplete, true}, // already uploaded
		{StateUploading, StateComplete, false},
		{StateVerifying, StateUploading, true}, // re-upload after an integrity failure
		{StateCommitting, StateComplete, true},
		{StateComplete, StateQueued, false},
		{StateSourceLost, StateQueued, false},
		{StateFailed, StateQueued, true},
		{StateQueued, StateComplete, false},
	} {
		if got := CanTransition(c.from, c.to); got != c.ok {
			t.Errorf("%s → %s: %v", c.from, c.to, got)
		}
	}
}

func TestBackoff(t *testing.T) {
	lo, hi := func() float64 { return 0 }, func() float64 { return 1 }
	if d := Backoff(1, lo); d != 24*time.Second {
		t.Errorf("first retry (low jitter) = %v", d)
	}
	if d := Backoff(2, hi); d != 72*time.Second {
		t.Errorf("second retry (high jitter) = %v", d)
	}
	if d := Backoff(50, hi); d != time.Duration(float64(6*time.Hour)*1.2) {
		t.Errorf("capped retry = %v", d)
	}
}

// harness runs a scheduler over a temporary store with a scripted runner.
type harness struct {
	t       *testing.T
	st      *store.Store
	s       *Scheduler
	targets []*config.Storage
	cancel  context.CancelFunc
	done    chan struct{}

	mu      sync.Mutex
	script  func(ctx context.Context, task *Task) error
	running map[int64]bool
	maxPer  map[string]int
	order   []int64
	updates []apiv1.JobUpdate
}

func newHarness(t *testing.T, workers int, targets ...*config.Storage) *harness {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := &harness{t: t, st: st, targets: targets, running: map[int64]bool{}, maxPer: map[string]int{}}
	h.s = New(Options{
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Store: st, Node: "pve1", Workers: workers,
		Targets: func() []*config.Storage { return h.targets }, Runner: h, Poll: 20 * time.Millisecond,
		Rand:     func() float64 { return 0.5 },
		OnUpdate: func(u apiv1.JobUpdate) { h.mu.Lock(); h.updates = append(h.updates, u); h.mu.Unlock() },
	})
	return h
}

func (h *harness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel, h.done = cancel, make(chan struct{})
	go func() { h.s.Run(ctx); close(h.done) }()
	h.t.Cleanup(h.stop)
}

func (h *harness) stop() {
	if h.cancel != nil {
		h.cancel()
		<-h.done
		h.cancel = nil
	}
}

// Run implements Runner.
func (h *harness) Run(ctx context.Context, task *Task) error {
	j := task.Job()
	h.mu.Lock()
	h.running[j.ID] = true
	h.order = append(h.order, j.ID)
	n := 0
	for id := range h.running {
		if jj, _ := h.st.GetJob(context.Background(), id); jj != nil && jj.StoreID == j.StoreID {
			n++
		}
	}
	h.maxPer[j.StoreID] = max(h.maxPer[j.StoreID], n)
	script := h.script
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.running, j.ID); h.mu.Unlock() }()
	return script(ctx, task)
}

func (h *harness) setScript(fn func(ctx context.Context, task *Task) error) {
	h.mu.Lock()
	h.script = fn
	h.mu.Unlock()
}

// complete is the runner script of a successful upload.
func complete(ctx context.Context, task *Task) error {
	for _, st := range []string{StateUploading, StateVerifying, StateCommitting} {
		if err := task.Advance(ctx, st, "", nil); err != nil {
			return err
		}
	}
	return nil
}

func (h *harness) add(storeID string, day int) int64 {
	h.t.Helper()
	id, _, err := h.st.InsertJob(h.t.Context(), &store.Job{Kind: "replicate", StoreID: storeID, State: StateQueued,
		DedupeKey: fmt.Sprintf("%s:%d:%d", storeID, day, time.Now().UnixNano()), OwnerNode: "pve1",
		VMType: "qemu", VMID: 100 + day, BackupTime: int64(day) * 86400})
	if err != nil {
		h.t.Fatal(err)
	}
	h.s.Wake()
	return id
}

func (h *harness) waitState(id int64, state string) *store.Job {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		j, err := h.st.GetJob(h.t.Context(), id)
		if err == nil && j.State == state {
			return j
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("job %d is %v, want %s", id, j, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func target(id, remote string, transfers int) *config.Storage {
	return &config.Storage{ID: id, Remote: remote, Transfers: transfers}
}

func TestCapsAndOrder(t *testing.T) {
	h := newHarness(t, 2, target("a", "r1", 1), target("b", "r2", 2))
	release := make(chan struct{})
	h.setScript(func(ctx context.Context, task *Task) error {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return complete(ctx, task)
	})
	a1, a2, a3 := h.add("a", 1), h.add("a", 3), h.add("a", 2)
	b1 := h.add("b", 1)
	h.start()
	// Node cap 2 with storage a capped at 1: a's newest backup and b's job.
	h.waitState(a2, StatePreparing)
	h.waitState(b1, StatePreparing)
	time.Sleep(100 * time.Millisecond)
	if j, _ := h.st.GetJob(t.Context(), a3); j.State != StateQueued {
		t.Fatalf("second job of storage a started: %s", j.State)
	}
	close(release)
	for _, id := range []int64{a1, a2, a3, b1} {
		h.waitState(id, StateComplete)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.maxPer["a"] != 1 {
		t.Errorf("storage a ran %d jobs at once", h.maxPer["a"])
	}
	var aOrder []int64
	for _, id := range h.order {
		if id != b1 {
			aOrder = append(aOrder, id)
		}
	}
	if fmt.Sprint(aOrder) != fmt.Sprint([]int64{a2, a3, a1}) {
		t.Errorf("storage a ran %v, want newest backup first %v", aOrder, []int64{a2, a3, a1})
	}
	if len(h.updates) == 0 {
		t.Error("no job updates published")
	}
}

func TestFailurePolicy(t *testing.T) {
	h := newHarness(t, 4, target("a", "r1", 4), target("b", "r2", 4))
	var mu sync.Mutex
	errs := map[int64]error{}
	h.setScript(func(ctx context.Context, task *Task) error {
		mu.Lock()
		err := errs[task.Job().ID]
		mu.Unlock()
		if err != nil {
			return err
		}
		return complete(ctx, task)
	})
	transient := h.add("a", 1)
	integrity := h.add("a", 2)
	lost := h.add("a", 3)
	auth := h.add("b", 1)
	mu.Lock()
	errs[transient] = fmt.Errorf("upload: %w", context.DeadlineExceeded)
	errs[integrity] = errors.New("corrupted on transfer: quickxor hashes differ")
	errs[lost] = fmt.Errorf("archive was pruned: %w", ErrSourceLost)
	errs[auth] = errors.New("couldn't fetch token: invalid_grant: AADSTS70000")
	mu.Unlock()
	h.start()

	j := h.waitState(transient, StateRetryWait)
	if j.Attempts != 1 || j.ErrorClass != "transient_network" || j.NextAttemptAt == nil ||
		*j.NextAttemptAt < time.Now().Unix()+25 || *j.NextAttemptAt > time.Now().Unix()+35 {
		t.Errorf("transient failure = %+v", j)
	}
	if j := h.waitState(integrity, StateFailed); j.FinishedAt == nil || j.ErrorClass != "integrity" {
		t.Errorf("integrity failure = %+v", j)
	}
	if j := h.waitState(lost, StateSourceLost); j.FinishedAt == nil {
		t.Errorf("lost source = %+v", j)
	}
	j = h.waitState(auth, StateRetryWait)
	if j.Attempts != 0 || j.ErrorClass != "auth_required" {
		t.Errorf("auth failure = %+v", j)
	}
	if p, ok := h.s.Paused()["r2"]; !ok || time.Until(p.Until) < 50*time.Minute {
		t.Fatalf("remote not paused: %+v", h.s.Paused())
	}
	alerts, _ := h.st.ActiveAlerts(t.Context())
	if len(alerts) != 1 || alerts[0].ID != "auth:r2" || alerts[0].StoreID != "b" {
		t.Fatalf("alerts = %+v", alerts)
	}
	// Jobs of a paused remote wait even when due.
	b2 := h.add("b", 2)
	time.Sleep(100 * time.Millisecond)
	if j, _ := h.st.GetJob(t.Context(), b2); j.State != StateQueued {
		t.Fatalf("job of a paused remote is %s", j.State)
	}
	// After reconnecting, the paused job is due at once (not when the pause
	// would have ended), and a successful job clears the pause and alert.
	mu.Lock()
	delete(errs, auth)
	mu.Unlock()
	h.s.ResumeRemote(t.Context(), "r2")
	h.waitState(b2, StateComplete)
	h.waitState(auth, StateComplete)
	if alerts, _ := h.st.ActiveAlerts(t.Context()); len(alerts) != 0 {
		t.Fatalf("alerts after success = %+v", alerts)
	}
	if len(h.s.Paused()) != 0 {
		t.Fatalf("pauses after success = %+v", h.s.Paused())
	}
}

func TestUnknownErrorsAreBounded(t *testing.T) {
	h := newHarness(t, 1, target("a", "r1", 1))
	h.setScript(func(context.Context, *Task) error { return errors.New("something odd") })
	id := h.add("a", 1)
	// Pretend nine attempts already failed.
	if _, err := h.st.UpdateJob(t.Context(), id, nil, "", func(j *store.Job) error { j.Attempts = 9; return nil }); err != nil {
		t.Fatal(err)
	}
	h.start()
	if j := h.waitState(id, StateFailed); j.Attempts != 10 {
		t.Fatalf("job = %+v", j)
	}
}

func TestCancelRetryAndShutdown(t *testing.T) {
	h := newHarness(t, 1, target("a", "r1", 1))
	started := make(chan int64, 4)
	h.setScript(func(ctx context.Context, task *Task) error {
		if err := task.Advance(ctx, StateUploading, "uploading", nil); err != nil {
			return err
		}
		if err := task.Update(ctx, func(j *store.Job) { j.ProgressBytes = 42; j.State = "bogus" }); err != nil {
			return err
		}
		started <- task.Job().ID
		<-ctx.Done()
		return ctx.Err()
	})
	running := h.add("a", 2)
	waiting := h.add("a", 1)
	h.start()
	if id := <-started; id != running {
		t.Fatalf("started %d", id)
	}
	if j, _ := h.st.GetJob(t.Context(), running); j.State != StateUploading || j.ProgressBytes != 42 {
		t.Fatalf("progress update = %+v", j)
	}
	if err := h.s.Cancel(t.Context(), waiting); err != nil {
		t.Fatal(err)
	}
	h.waitState(waiting, StateCancelled)
	if err := h.s.Cancel(t.Context(), running); err != nil {
		t.Fatal(err)
	}
	h.waitState(running, StateCancelled)
	if err := h.s.Cancel(t.Context(), running); !errors.Is(err, ErrWrongState) {
		t.Fatalf("cancel a cancelled job: %v", err)
	}

	// Retrying puts it back in the queue; stopping the daemon requeues it.
	if err := h.s.Retry(t.Context(), running); err != nil {
		t.Fatal(err)
	}
	<-started
	h.stop()
	j := h.waitState(running, StateQueued)
	if j.ProgressBytes != 42 || j.Attempts != 0 {
		t.Fatalf("requeued job = %+v", j)
	}
	if err := h.s.Retry(t.Context(), running); !errors.Is(err, ErrWrongState) {
		t.Fatalf("retry a queued job: %v", err)
	}
	if err := h.s.SetPriority(t.Context(), waiting, 5); err != nil {
		t.Fatal(err)
	}
	if j, _ := h.st.GetJob(t.Context(), waiting); j.Priority != 5 {
		t.Fatalf("priority = %d", j.Priority)
	}
}

func TestIllegalTransitionRefused(t *testing.T) {
	h := newHarness(t, 1, target("a", "r1", 1))
	result := make(chan error, 1)
	h.setScript(func(ctx context.Context, task *Task) error {
		err := task.Advance(ctx, StateComplete, "skip ahead", nil)
		result <- err
		if err == nil {
			return nil
		}
		return complete(ctx, task)
	})
	id := h.add("a", 1)
	h.start()
	// preparing → complete is the "already uploaded" path and is legal.
	if err := <-result; err != nil {
		t.Fatalf("preparing → complete refused: %v", err)
	}
	h.waitState(id, StateComplete)

	h.setScript(func(ctx context.Context, task *Task) error {
		if err := task.Advance(ctx, StateUploading, "", nil); err != nil {
			return err
		}
		err := task.Advance(ctx, StateComplete, "skip verification", nil)
		result <- err
		return complete2(ctx, task)
	})
	id2 := h.add("a", 2)
	if err := <-result; err == nil {
		t.Fatal("uploading → complete accepted")
	}
	h.waitState(id2, StateComplete)
}

func complete2(ctx context.Context, task *Task) error {
	for _, st := range []string{StateVerifying, StateCommitting} {
		if err := task.Advance(ctx, st, "", nil); err != nil {
			return err
		}
	}
	return nil
}

func TestPostponeAndPermanent(t *testing.T) {
	h := newHarness(t, 2, target("a", "r1", 2))
	until := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	var mu sync.Mutex
	outcome := map[int64]error{}
	h.setScript(func(ctx context.Context, task *Task) error {
		mu.Lock()
		defer mu.Unlock()
		return outcome[task.Job().ID]
	})
	settling := h.add("a", 1)
	broken := h.add("a", 2)
	mu.Lock()
	outcome[settling] = Postpone(until, "waiting for vzdump to finish")
	outcome[broken] = Permanent(errors.New("source name registered by another installation"))
	mu.Unlock()
	h.start()
	j := h.waitState(settling, StateRetryWait)
	if j.Attempts != 0 || j.ErrorClass != "" || j.LastError != "waiting for vzdump to finish" || *j.NextAttemptAt != until.Unix() {
		t.Errorf("postponed job = %+v", j)
	}
	if j := h.waitState(broken, StateFailed); j.ErrorClass != "permanent" || j.FinishedAt == nil {
		t.Errorf("permanent failure = %+v", j)
	}
}

func TestNotReadyStoragesWait(t *testing.T) {
	h := newHarness(t, 1, target("a", "r1", 1))
	var ready sync.Map
	h.s.opts.Ready = func(id string) error {
		if _, ok := ready.Load(id); ok {
			return nil
		}
		return errors.New("repository not open")
	}
	h.setScript(complete)
	id := h.add("a", 1)
	h.start()
	time.Sleep(100 * time.Millisecond)
	if j, _ := h.st.GetJob(t.Context(), id); j.State != StateQueued {
		t.Fatalf("job of a storage that is not ready is %s", j.State)
	}
	ready.Store("a", true)
	h.s.Wake()
	h.waitState(id, StateComplete)
}

// TestRetryDuringCleanup: a job retried right after its outcome is
// recorded, before the old worker is cleaned up, keeps its new attempt
// registered: it can be cancelled and counts toward the limits.
func TestRetryDuringCleanup(t *testing.T) {
	h := newHarness(t, 2, target("a", "r1", 2))
	finished, proceed := make(chan int64, 1), make(chan struct{})
	testHookFinished = func(id int64) {
		select {
		case finished <- id:
			<-proceed
		default:
		}
	}
	t.Cleanup(func() { testHookFinished = nil })
	var attempts atomic.Int32
	cancelled := make(chan struct{})
	h.setScript(func(ctx context.Context, task *Task) error {
		if attempts.Add(1) == 1 {
			return Permanent(errors.New("first attempt fails"))
		}
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	})
	id := h.add("a", 1)
	h.start()
	<-finished // failed, but the old worker is not cleaned up yet
	if err := h.s.Retry(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	for attempts.Load() < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	close(proceed) // the old worker's cleanup runs now
	time.Sleep(50 * time.Millisecond)
	if err := h.s.Cancel(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the retried attempt could not be cancelled")
	}
	h.waitState(id, StateCancelled)
}

// TestRoundRobinIsFair: with more busy storages than workers, every
// storage gets its turn, also when both workers become free at once.
func TestRoundRobinIsFair(t *testing.T) {
	h := newHarness(t, 2, target("a", "r1", 1), target("b", "r2", 1), target("c", "r3", 1))
	release := make(chan struct{})
	h.setScript(func(ctx context.Context, task *Task) error {
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return complete(ctx, task)
	})
	storeOf := map[int64]string{}
	for day := 1; day <= 4; day++ {
		for _, s := range []string{"a", "b", "c"} {
			storeOf[h.add(s, day)] = s
		}
	}
	// The scheduler publishes each job as it starts it, in order.
	starts := func() []string {
		h.mu.Lock()
		defer h.mu.Unlock()
		var out []string
		for _, u := range h.updates {
			if u.State == StatePreparing {
				out = append(out, storeOf[u.ID])
			}
		}
		return out
	}
	waitStarts := func(n int) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); len(starts()) < n; time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%d jobs started, want %d", len(starts()), n)
			}
		}
	}
	h.start()
	waitStarts(2)
	for _, n := range []int{4, 6} {
		release <- struct{}{}
		release <- struct{}{}
		waitStarts(n)
	}
	count := map[string]int{}
	for _, s := range starts()[:6] {
		count[s]++
	}
	if count["a"] != 2 || count["b"] != 2 || count["c"] != 2 {
		t.Fatalf("first six starts %v: %v", starts()[:6], count)
	}
	close(release)
}

// TestRetryAfterIsAMinimum: a provider's Retry-After delays the next
// attempt beyond the normal backoff.
func TestRetryAfterIsAMinimum(t *testing.T) {
	h := newHarness(t, 1, target("a", "r1", 1))
	h.setScript(func(context.Context, *Task) error {
		return pacer.RetryAfterError(errors.New("429 Too Many Requests"), 10*time.Minute)
	})
	id := h.add("a", 1)
	h.start()
	j := h.waitState(id, StateRetryWait)
	if j.ErrorClass != "throttled" || j.NextAttemptAt == nil || *j.NextAttemptAt < time.Now().Add(9*time.Minute).Unix() {
		t.Fatalf("throttled job = %+v (next attempt in %ds)", j, *j.NextAttemptAt-time.Now().Unix())
	}
}

// TestResumeAfterRestart: jobs deferred by a pause are made due when the
// remote is resumed, also after a restart lost the in-memory pause.
func TestResumeAfterRestart(t *testing.T) {
	h := newHarness(t, 1, target("a", "r1", 1))
	h.setScript(complete)
	id := h.add("a", 1)
	later := time.Now().Add(time.Hour).Unix()
	if _, err := h.st.UpdateJob(t.Context(), id, nil, "", func(j *store.Job) error {
		j.State, j.ErrorClass, j.NextAttemptAt = StateRetryWait, "auth_required", &later
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.start()
	time.Sleep(100 * time.Millisecond)
	if j, _ := h.st.GetJob(t.Context(), id); j.State != StateRetryWait {
		t.Fatalf("deferred job ran early: %s", j.State)
	}
	h.s.ResumeRemote(t.Context(), "r1")
	h.waitState(id, StateComplete)
}
