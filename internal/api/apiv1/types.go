// SPDX-License-Identifier: AGPL-3.0-or-later

// Package apiv1 defines the wire types of the daemon API served under /v1
// on the daemon's unix socket. Changes within v1 must be additive.
package apiv1

import "time"

// ClientAPIHeader carries the API revision a client speaks. Clients that
// send an unsupported revision are refused with CodeIncompatibleVersion.
const ClientAPIHeader = "X-Client-API"

// IdempotencyHeader makes a mutating request safe to retry.
const IdempotencyHeader = "Idempotency-Key"

// Revision is the API revision served by this daemon.
const Revision = 1

// Error codes.
const (
	CodeInvalidArgument     = "invalid_argument"
	CodeNotFound            = "not_found"
	CodeConflict            = "conflict"
	CodePreconditionFailed  = "precondition_failed"
	CodePermissionDenied    = "permission_denied"
	CodeRemoteUnreachable   = "remote_unreachable"
	CodeRemoteAuthRequired  = "remote_auth_required"
	CodeQuotaExceeded       = "quota_exceeded"
	CodeStagingInsufficient = "staging_insufficient"
	CodeBusy                = "busy"
	CodeImmutable           = "immutable"
	CodeProtected           = "protected"
	CodeIncompatibleVersion = "incompatible_version"
	CodeUnavailable         = "unavailable"
	CodeInternal            = "internal"
)

// Error is the body of every non-2xx response: {"error": {...}}.
type Error struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable,omitzero"`
	Details   map[string]any `json:"details,omitempty"`
}

// ErrorResponse wraps Error.
type ErrorResponse struct {
	Error Error `json:"error"`
}

// Version is returned by GET /v1/version.
type Version struct {
	API          int    `json:"api"`
	APIMinClient int    `json:"api_min_client"`
	Daemon       string `json:"daemon"`
	Commit       string `json:"commit,omitempty"`
	Rclone       string `json:"rclone"`
	Schema       int    `json:"schema"`
	InstanceID   string `json:"instance_id"`
	Node         string `json:"node"`
}

// Status is returned by GET /v1/status.
type Status struct {
	Node      string    `json:"node"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
	UptimeSec int64     `json:"uptime_seconds"`
	Healthy   bool      `json:"healthy"`
	Problems  []string  `json:"problems,omitempty"`
	Alerts    []Alert   `json:"alerts,omitempty"`
	Jobs      JobCounts `json:"jobs"`
}

// JobCounts summarizes the queue by state.
type JobCounts struct {
	Queued    int `json:"queued"`
	Active    int `json:"active"`
	RetryWait int `json:"retry_wait"`
	Failed    int `json:"failed"`
}

// Alert is an unresolved operator-facing problem.
type Alert struct {
	ID       string    `json:"id"`
	Severity string    `json:"severity"`
	Storage  string    `json:"storage,omitempty"`
	Message  string    `json:"message"`
	RaisedAt time.Time `json:"raised_at"`
}

// Event is one server-sent event on GET /v1/events.
type Event struct {
	ID   uint64    `json:"id"`
	Type string    `json:"type"`
	Time time.Time `json:"time"`
	Data any       `json:"data,omitempty"`
}
