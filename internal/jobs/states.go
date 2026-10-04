// SPDX-License-Identifier: AGPL-3.0-or-later

// Package jobs runs queued jobs: it claims them in priority order within
// node-wide and per-storage limits, hands them to a runner, and applies
// the retry, pause and failure policy to the outcome.
package jobs

import (
	"math/rand/v2"
	"slices"
	"time"
)

// Replication job states.
const (
	StateQueued     = "queued"
	StateSkipped    = "skipped"
	StateSuperseded = "superseded"
	StatePreparing  = "preparing"
	StateUploading  = "uploading"
	StateVerifying  = "verifying"
	StateCommitting = "committing"
	// Fetch and restore jobs.
	StateTransferring = "transferring"
	StateApplying     = "applying"
	StateComplete     = "complete"
	StateRetryWait    = "retry_wait"
	StateFailed       = "failed"
	StateCancelled    = "cancelled"
	StateSourceLost   = "source_lost"
)

// ActiveStates are states of a job a worker is executing.
var ActiveStates = []string{StatePreparing, StateUploading, StateVerifying, StateCommitting, StateTransferring, StateApplying}

// transitions lists the legal state changes. Active states may return to
// queued when the daemon stops.
var transitions = map[string][]string{
	StateQueued:    {StatePreparing, StateSuperseded, StateCancelled},
	StateRetryWait: {StatePreparing, StateQueued, StateSuperseded, StateCancelled},
	StatePreparing: {StateUploading, StateTransferring, StateComplete, StateRetryWait, StateFailed, StateSourceLost,
		StateCancelled, StateQueued},
	StateUploading: {StateVerifying, StateRetryWait, StateFailed, StateSourceLost, StateCancelled, StateQueued},
	StateVerifying: {StateUploading, StateCommitting, StateApplying, StateComplete, StateRetryWait, StateFailed,
		StateSourceLost, StateCancelled, StateQueued},
	StateTransferring: {StateVerifying, StateRetryWait, StateFailed, StateCancelled, StateQueued},
	StateApplying:     {StateComplete, StateFailed, StateCancelled, StateQueued},
	StateCommitting:   {StateComplete, StateRetryWait, StateFailed, StateSourceLost, StateCancelled, StateQueued},
	StateFailed:       {StateQueued},
	StateCancelled:    {StateQueued},
	StateSkipped:      {StateQueued},
	StateSuperseded:   {StateQueued},
}

// CanTransition reports whether a job may move from one state to another.
func CanTransition(from, to string) bool { return slices.Contains(transitions[from], to) }

// Retry policy.
const (
	backoffBase = 30 * time.Second
	backoffMax  = 6 * time.Hour
	// maxUnknownAttempts bounds retries of unclassified errors.
	maxUnknownAttempts = 10
	authPause          = time.Hour
	quotaPause         = 30 * time.Minute
	stallAlertAfter    = 24 * time.Hour
)

// Backoff returns the delay before attempt n+1 after n failed attempts:
// 30s doubling up to 6h, with ±20% jitter.
func Backoff(n int, rnd func() float64) time.Duration {
	if rnd == nil {
		rnd = rand.Float64
	}
	d := min(backoffBase<<min(max(n-1, 0), 20), backoffMax)
	return time.Duration(float64(d) * (0.8 + 0.4*rnd()))
}
