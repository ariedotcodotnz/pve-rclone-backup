// SPDX-License-Identifier: AGPL-3.0-or-later

// Package apiv1 defines the wire types of the daemon API served under /v1
// on the daemon's unix socket. Changes within v1 must be additive.
package apiv1

import (
	"encoding/json"
	"time"
)

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

// Storage is an rclone-backup storage as served by this node.
type Storage struct {
	ID            string     `json:"id"`
	Remote        string     `json:"remote"`
	Path          string     `json:"path"`
	Source        string     `json:"source"`
	Encryption    string     `json:"encryption"`
	ReplicateFrom []string   `json:"replicate_from"`
	Enabled       bool       `json:"enabled"` // configured for this node and not disabled
	Active        bool       `json:"active"`  // the catalogue can be served
	Health        string     `json:"health"`
	HealthDetail  string     `json:"health_detail,omitempty"`
	ConfigError   string     `json:"config_error,omitempty"`
	RepoUUID      string     `json:"repo_uuid,omitempty"`
	Generation    int        `json:"generation,omitzero"`
	LastResyncAt  *time.Time `json:"last_resync_at,omitempty"`
	Usage         *Usage     `json:"usage,omitempty"`
}

// Storage health values.
const (
	HealthOK            = "ok"
	HealthOpening       = "opening"        // not opened yet since start or reconfiguration
	HealthDegraded      = "degraded"       // throttled by the provider
	HealthUnreachable   = "unreachable"    // network or provider failure
	HealthAuthRequired  = "auth_required"  // the remote must be reconnected
	HealthQuotaExceeded = "quota_exceeded" // the remote is full
	HealthMisconfigured = "misconfigured"  // configuration, keys or repository identity are wrong
	HealthUninitialized = "uninitialized"  // no repository at the configured location
	HealthDisabled      = "disabled"       // disabled or not configured for this node
)

// Usage is the quota of a storage's remote. Fields are nil when the
// provider does not report them.
type Usage struct {
	Total     *int64    `json:"total,omitempty"`
	Used      *int64    `json:"used,omitempty"`
	Free      *int64    `json:"free,omitempty"`
	Trashed   *int64    `json:"trashed,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// StorageStatus answers PVE's status() poll. It is served from cached state
// only.
type StorageStatus struct {
	Total  int64  `json:"total"`
	Avail  int64  `json:"avail"`
	Used   int64  `json:"used"`
	Active bool   `json:"active"`
	Health string `json:"health"`
}

// Backup is a catalogue entry.
type Backup struct {
	Volname      string     `json:"volname"` // backup/<archive name>
	VMType       string     `json:"vmtype"`
	VMID         int        `json:"vmid"`
	CTime        int64      `json:"ctime"` // backup time (Unix seconds)
	Size         int64      `json:"size"`  // archive size
	Format       string     `json:"format"`
	Compression  string     `json:"compression,omitempty"`
	State        string     `json:"state"`
	Protected    bool       `json:"protected"`
	Notes        string     `json:"notes,omitempty"`
	GuestName    string     `json:"guest_name,omitempty"`
	Generation   int        `json:"generation"`
	UploadedAt   time.Time  `json:"uploaded_at"`
	VerifyLevel  int        `json:"verify_level"`
	VerifiedAt   *time.Time `json:"verified_at,omitempty"`
	VerifyResult string     `json:"verify_result,omitempty"`
	DeleteAfter  *time.Time `json:"delete_after,omitempty"`
}

// BackupDetail adds the manifest to a catalogue entry.
type BackupDetail struct {
	Backup
	Manifest json.RawMessage `json:"manifest"`
}

// GuestConfig is the guest configuration recorded in a backup's manifest.
type GuestConfig struct {
	Config        string  `json:"config"`
	Firewall      *string `json:"firewall"`
	FirewallKnown bool    `json:"firewall_known"`
}

// ValidateRequest carries a storage.cfg section from PVE's storage hooks.
// Config is the stored section (PVE's parsed form); for updates, Update
// holds changed properties and Delete the removed ones.
type ValidateRequest struct {
	Config map[string]any `json:"config"`
	Update map[string]any `json:"update,omitempty"`
	Delete any            `json:"delete,omitempty"`
}

// ValidateResponse reports a successful validation.
type ValidateResponse struct {
	Warnings []string `json:"warnings,omitempty"`
}

// ScanSummary reports a discovery scan.
type ScanSummary struct {
	Sources    int      `json:"sources"`    // usable source storages scanned
	Archives   int      `json:"archives"`   // archives seen
	Queued     int      `json:"queued"`     // new uploads queued
	Skipped    int      `json:"skipped"`    // archives recorded as not replicated
	Superseded int      `json:"superseded"` // queued uploads replaced by newer archives
	Known      int      `json:"known"`      // archives already handled earlier
	Problems   []string `json:"problems,omitempty"`
}

// DiscoveryNotify is sent by the optional vzdump hook.
type DiscoveryNotify struct {
	Path  string `json:"path"`
	Phase string `json:"phase,omitempty"`
}

// JobUpdate is the payload of job.updated events.
type JobUpdate struct {
	ID    int64  `json:"id"`
	State string `json:"state"`
}
