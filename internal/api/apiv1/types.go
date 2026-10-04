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
	// KitConfirmed reports a confirmed recovery kit for the repository;
	// replication of encrypted repositories waits for it.
	KitConfirmed bool `json:"kit_confirmed"`
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
	ID            int64  `json:"id"`
	State         string `json:"state"`
	ProgressBytes int64  `json:"progress_bytes,omitzero"`
	TotalBytes    *int64 `json:"total_bytes,omitempty"`
}

// Job is a queued or finished job.
type Job struct {
	ID            int64      `json:"id"`
	Kind          string     `json:"kind"`
	Storage       string     `json:"storage"`
	State         string     `json:"state"`
	Priority      int        `json:"priority"`
	Volname       string     `json:"volname,omitempty"`
	SourceStorage string     `json:"source_storage,omitempty"`
	SourcePath    string     `json:"source_path,omitempty"`
	VMType        string     `json:"vmtype,omitempty"`
	VMID          int        `json:"vmid,omitzero"`
	BackupTime    int64      `json:"backup_time,omitzero"`
	Attempts      int        `json:"attempts"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	ErrorClass    string     `json:"error_class,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	ProgressBytes int64      `json:"progress_bytes"`
	TotalBytes    *int64     `json:"total_bytes,omitempty"`
	NextSegment   int        `json:"next_segment"`
	OwnerNode     string     `json:"owner_node"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
}

// JobSegment is an uploaded or pending segment of a replication job.
type JobSegment struct {
	Index      int    `json:"index"`
	Offset     int64  `json:"offset"`
	Size       int64  `json:"size"`
	State      string `json:"state"`
	SHA256     string `json:"sha256,omitempty"`
	StoredSize *int64 `json:"stored_size,omitempty"`
}

// JobEvent is an entry of a job's history.
type JobEvent struct {
	Time      time.Time `json:"time"`
	Level     string    `json:"level"`
	FromState string    `json:"from_state,omitempty"`
	ToState   string    `json:"to_state,omitempty"`
	Message   string    `json:"message"`
}

// JobDetail adds segments and history to a job.
type JobDetail struct {
	Job
	Segments []JobSegment `json:"segments"`
	Events   []JobEvent   `json:"events"`
}

// Provider is a supported storage backend.
type Provider struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	MaxObjectSize int64  `json:"max_object_size,omitzero"`
	RecycleBin    bool   `json:"recycle_bin"`
}

// Remote is a configured transport remote. Credentials are never
// included.
type Remote struct {
	Name        string     `json:"name"`
	Type        string     `json:"type"`
	Supported   bool       `json:"supported"`
	Authorized  bool       `json:"authorized"` // has an OAuth refresh token (OAuth backends)
	TokenExpiry *time.Time `json:"token_expiry,omitempty"`
	CustomApp   bool       `json:"custom_app"` // uses its own OAuth client ID
	DriveType   string     `json:"drive_type,omitempty"`
	DriveID     string     `json:"drive_id,omitempty"`
	Storages    []string   `json:"storages"` // storages using this remote
}

// ProbeResult reports a remote connectivity test.
type ProbeResult struct {
	LatencyMS int64  `json:"latency_ms"`
	HashType  string `json:"hash_type,omitempty"`
	Usage     *Usage `json:"usage,omitempty"`
}

// RemoteSetupRequest starts configuring a remote.
type RemoteSetupRequest struct {
	Name     string            `json:"name"`
	Provider string            `json:"provider"`
	Params   map[string]string `json:"params,omitempty"` // backend options, e.g. client_id
}

// RemoteSetupAnswer answers the current question of a setup session.
type RemoteSetupAnswer struct {
	State  string `json:"state"`
	Result string `json:"result"`
}

// OAuthRedirect carries the redirect URL the user's browser could not
// load (http://localhost:53682/?code=...&state=...).
type OAuthRedirect struct {
	URL string `json:"url"`
}

// Remote setup session statuses.
const (
	SetupQuestion    = "question"    // answer Option with State
	SetupAuthorizing = "authorizing" // open AuthURL, then send the redirect URL
	SetupDone        = "done"
	SetupFailed      = "failed"
)

// RemoteSetup is the state of a remote setup or reconnect session.
type RemoteSetup struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Provider  string       `json:"provider"`
	Reconnect bool         `json:"reconnect"`
	Status    string       `json:"status"`
	State     string       `json:"state,omitempty"`
	Option    *SetupOption `json:"option,omitempty"`
	Error     string       `json:"error,omitempty"`
	AuthURL   string       `json:"auth_url,omitempty"`
}

// SetupOption is a question asked while configuring a remote.
type SetupOption struct {
	Name       string         `json:"name"`
	Help       string         `json:"help"`
	Type       string         `json:"type"`
	Default    string         `json:"default,omitempty"`
	Examples   []SetupExample `json:"examples,omitempty"`
	Required   bool           `json:"required"`
	IsPassword bool           `json:"is_password"`
	Exclusive  bool           `json:"exclusive"`
}

// SetupExample is a suggested answer.
type SetupExample struct {
	Value string `json:"value"`
	Help  string `json:"help"`
}

// StorageInitRequest creates (or adopts) the repository of a storage
// before its storage.cfg entry is added.
type StorageInitRequest struct {
	Remote     string `json:"remote"`
	Path       string `json:"path"`
	Source     string `json:"source"`
	Encryption string `json:"encryption"` // crypt (default) or none
	// AdoptSource takes over a source name registered by another
	// installation (disaster recovery).
	AdoptSource bool `json:"adopt_source,omitempty"`
}

// StorageInitResponse reports the repository a storage is bound to.
type StorageInitResponse struct {
	RepoUUID   string `json:"repo_uuid"`
	Created    bool   `json:"created"` // false: an existing repository was adopted
	Encryption string `json:"encryption"`
	// KitRequired means replication waits for a confirmed recovery kit.
	KitRequired bool `json:"kit_required"`
}

// KitTarget selects a repository for a recovery kit, by storage ID (for
// configured storages) or by location.
type KitTarget struct {
	Storage string `json:"storage"`
	Remote  string `json:"remote,omitempty"`
	Path    string `json:"path,omitempty"`
	Source  string `json:"source,omitempty"`
}

// KitExportRequest exports a recovery kit.
type KitExportRequest struct {
	Targets      []KitTarget `json:"targets"` // empty: all configured storages
	IncludeToken bool        `json:"include_token"`
	Passphrase   string      `json:"passphrase,omitempty"`
}

// KitExportResponse carries the kit text.
type KitExportResponse struct {
	Kit       string   `json:"kit"`
	Checksum  string   `json:"checksum"`
	Repos     []string `json:"repos"`
	Encrypted bool     `json:"encrypted"`
}

// KitConfirmRequest confirms that a kit was stored safely.
type KitConfirmRequest struct {
	Checksum string `json:"checksum"`
}

// KitConfirmResponse lists the confirmed repositories.
type KitConfirmResponse struct {
	Repos []string `json:"repos"`
}

// KitRequest carries a kit for import or verification.
type KitRequest struct {
	Kit         string `json:"kit"`
	Passphrase  string `json:"passphrase,omitempty"`
	ImportToken bool   `json:"import_token,omitempty"`
}

// KitRepoResult reports one repository of an imported or verified kit.
type KitRepoResult struct {
	RepoUUID     string `json:"repo_uuid"`
	Storage      string `json:"storage"`
	Remote       string `json:"remote"`
	Path         string `json:"path"`
	Source       string `json:"source"`
	Encryption   string `json:"encryption"`
	Keys         string `json:"keys,omitempty"`          // imported | merged | present | none
	RemoteConfig string `json:"remote_config,omitempty"` // created | exists | not_in_kit | skipped
	OK           bool   `json:"ok"`
	Error        string `json:"error,omitempty"`
}

// KitStatus is the recovery kit state of a repository.
type KitStatus struct {
	RepoUUID    string     `json:"repo_uuid"`
	Storages    []string   `json:"storages"`
	Encrypted   bool       `json:"encrypted"`
	ExportedAt  *time.Time `json:"exported_at,omitempty"`
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
}

// BackupUpdate changes a backup's notes or protection.
type BackupUpdate struct {
	Notes     *string `json:"notes,omitempty"`
	Protected *bool   `json:"protected,omitempty"`
}

// FetchRequest downloads an offsite backup into a local backup storage.
type FetchRequest struct {
	TargetStorage string `json:"target_storage"`
	// Protect marks the fetched backup protected (default true), so the
	// next local prune does not remove it.
	Protect *bool `json:"protect,omitempty"`
}

// RestoreRequest restores a guest from an offsite backup.
type RestoreRequest struct {
	Storage       string `json:"storage"`
	Volname       string `json:"volname"`        // backup/<archive>
	Mode          string `json:"mode,omitempty"` // stream (VMs) or stage; default per guest type
	TargetVMID    int    `json:"target_vmid"`
	TargetStorage string `json:"target_storage,omitempty"`
	Unique        bool   `json:"unique,omitzero"`
	Force         bool   `json:"force,omitzero"`
	AllowDamaged  bool   `json:"allow_damaged,omitzero"`
}
