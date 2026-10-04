// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("store: not found")

// ErrStateConflict is returned when a job is not in an expected state.
var ErrStateConflict = errors.New("store: job state changed concurrently")

// Job is a unit of work in the queue. State transition rules live in the
// jobs package; the store only persists them atomically with an event.
type Job struct {
	ID            int64
	Kind          string
	StoreID       string
	State         string
	Priority      int
	DedupeKey     string
	BackupVolname string
	SourceStorage string
	SourcePath    string
	SourceDev     *int64
	SourceIno     *int64
	SourceSize    *int64
	SourceMtimeNs *int64
	Attempts      int
	NextAttemptAt *int64
	ErrorClass    string
	LastError     string
	ProgressBytes int64
	TotalBytes    *int64
	NextSegment   int
	HashState     []byte
	OwnerNode     string
	LeaseUntil    *int64
	ParamsJSON    string
	CreatedAt     int64
	UpdatedAt     int64
	StartedAt     *int64
	FinishedAt    *int64
}

const jobColumns = `id, kind, storeid, state, priority, dedupe_key, backup_volname, source_storage,
	source_path, source_dev, source_ino, source_size, source_mtime_ns, attempts, next_attempt_at,
	error_class, last_error, progress_bytes, total_bytes, next_segment, hash_state, owner_node,
	lease_until, params_json, created_at, updated_at, started_at, finished_at`

type scanner interface{ Scan(dest ...any) error }

func scanJob(row scanner) (*Job, error) {
	var j Job
	var volname, srcStorage, srcPath, errClass, lastErr sql.NullString
	err := row.Scan(&j.ID, &j.Kind, &j.StoreID, &j.State, &j.Priority, &j.DedupeKey, &volname, &srcStorage,
		&srcPath, &j.SourceDev, &j.SourceIno, &j.SourceSize, &j.SourceMtimeNs, &j.Attempts, &j.NextAttemptAt,
		&errClass, &lastErr, &j.ProgressBytes, &j.TotalBytes, &j.NextSegment, &j.HashState, &j.OwnerNode,
		&j.LeaseUntil, &j.ParamsJSON, &j.CreatedAt, &j.UpdatedAt, &j.StartedAt, &j.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	j.BackupVolname, j.SourceStorage, j.SourcePath = volname.String, srcStorage.String, srcPath.String
	j.ErrorClass, j.LastError = errClass.String, lastErr.String
	return &j, nil
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// InsertJob adds a job unless one with the same dedupe key exists. It
// returns the id of the new or existing job and whether it was created.
func (s *Store) InsertJob(ctx context.Context, j *Job) (id int64, created bool, err error) {
	if j.ParamsJSON == "" {
		j.ParamsJSON = "{}"
	}
	now := s.unix()
	err = s.Tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, "SELECT id FROM jobs WHERE dedupe_key = ?", j.DedupeKey).Scan(&id)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO jobs (kind, storeid, state, priority, dedupe_key,
			backup_volname, source_storage, source_path, source_dev, source_ino, source_size, source_mtime_ns,
			next_attempt_at, total_bytes, owner_node, params_json, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			j.Kind, j.StoreID, j.State, j.Priority, j.DedupeKey, nullString(j.BackupVolname),
			nullString(j.SourceStorage), nullString(j.SourcePath), j.SourceDev, j.SourceIno, j.SourceSize,
			j.SourceMtimeNs, j.NextAttemptAt, j.TotalBytes, j.OwnerNode, j.ParamsJSON, now, now)
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		created = true
		return insertEvent(ctx, tx, id, now, "info", "", j.State, "job created")
	})
	if err != nil {
		return 0, false, fmt.Errorf("store: insert job: %w", err)
	}
	return id, created, nil
}

// GetJob returns a job by id.
func (s *Store) GetJob(ctx context.Context, id int64) (*Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id = ?", id))
}

// JobFilter selects jobs. Zero values match everything.
type JobFilter struct {
	Kind    string
	StoreID string
	States  []string
	Limit   int
}

// ListJobs returns jobs, newest first.
func (s *Store) ListJobs(ctx context.Context, f JobFilter) ([]*Job, error) {
	var where []string
	var args []any
	if f.Kind != "" {
		where, args = append(where, "kind = ?"), append(args, f.Kind)
	}
	if f.StoreID != "" {
		where, args = append(where, "storeid = ?"), append(args, f.StoreID)
	}
	if len(f.States) > 0 {
		where = append(where, "state IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.States)), ",")+")")
		for _, st := range f.States {
			args = append(args, st)
		}
	}
	q := "SELECT " + jobColumns + " FROM jobs"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC"
	if f.Limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// UpdateJob applies mutate to a job inside a transaction. If expected is
// non-empty the job must currently be in one of those states, otherwise
// ErrStateConflict is returned. A state change is recorded as an event
// with message.
func (s *Store) UpdateJob(ctx context.Context, id int64, expected []string, message string, mutate func(j *Job) error) (*Job, error) {
	var out *Job
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		j, err := scanJob(tx.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id = ?", id))
		if err != nil {
			return err
		}
		if len(expected) > 0 && !slices.Contains(expected, j.State) {
			return fmt.Errorf("%w: job %d is %s, expected %s", ErrStateConflict, id, j.State, strings.Join(expected, "|"))
		}
		from := j.State
		if err := mutate(j); err != nil {
			return err
		}
		now := s.unix()
		j.UpdatedAt = now
		_, err = tx.ExecContext(ctx, `UPDATE jobs SET state = ?, priority = ?, backup_volname = ?,
			attempts = ?, next_attempt_at = ?, error_class = ?, last_error = ?, progress_bytes = ?,
			total_bytes = ?, next_segment = ?, hash_state = ?, owner_node = ?, lease_until = ?,
			params_json = ?, updated_at = ?, started_at = ?, finished_at = ? WHERE id = ?`,
			j.State, j.Priority, nullString(j.BackupVolname), j.Attempts, j.NextAttemptAt,
			nullString(j.ErrorClass), nullString(j.LastError), j.ProgressBytes, j.TotalBytes, j.NextSegment,
			j.HashState, j.OwnerNode, j.LeaseUntil, j.ParamsJSON, j.UpdatedAt, j.StartedAt, j.FinishedAt, id)
		if err != nil {
			return err
		}
		if from != j.State {
			level := "info"
			if j.State == "failed" || j.State == "retry_wait" || j.State == "source_lost" {
				level = "warn"
			}
			if err := insertEvent(ctx, tx, id, now, level, from, j.State, message); err != nil {
				return err
			}
		}
		out = j
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("store: update job %d: %w", id, err)
	}
	return out, nil
}

// ClaimRunnable leases the next runnable job: the highest priority, oldest
// job in one of states whose next attempt is due. It moves the job to
// toState and sets the lease. It returns ErrNotFound if nothing is runnable.
func (s *Store) ClaimRunnable(ctx context.Context, kind string, states []string, toState, owner string, leaseSeconds int64) (*Job, error) {
	var out *Job
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		now := s.unix()
		args := []any{kind}
		for _, st := range states {
			args = append(args, st)
		}
		args = append(args, now)
		row := tx.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE kind = ? AND state IN ("+
			strings.TrimSuffix(strings.Repeat("?,", len(states)), ",")+
			") AND (next_attempt_at IS NULL OR next_attempt_at <= ?) ORDER BY priority DESC, id ASC LIMIT 1", args...)
		j, err := scanJob(row)
		if err != nil {
			return err
		}
		from := j.State
		lease := now + leaseSeconds
		j.State, j.OwnerNode, j.LeaseUntil, j.UpdatedAt = toState, owner, &lease, now
		if j.StartedAt == nil {
			j.StartedAt = &now
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE jobs SET state = ?, owner_node = ?, lease_until = ?, started_at = ?, updated_at = ? WHERE id = ?",
			j.State, owner, lease, j.StartedAt, now, j.ID); err != nil {
			return err
		}
		out = j
		return insertEvent(ctx, tx, j.ID, now, "info", from, toState, "claimed by "+owner)
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: claim job: %w", err)
	}
	return out, nil
}

// RequeueInterrupted moves jobs left in any of active states by a crashed or
// stopped daemon back to toState and clears their leases. It returns the
// number of jobs requeued.
func (s *Store) RequeueInterrupted(ctx context.Context, active []string, toState string) (int, error) {
	var n int
	err := s.Tx(ctx, func(tx *sql.Tx) error {
		args := make([]any, 0, len(active))
		for _, st := range active {
			args = append(args, st)
		}
		rows, err := tx.QueryContext(ctx, "SELECT id, state FROM jobs WHERE state IN ("+
			strings.TrimSuffix(strings.Repeat("?,", len(active)), ",")+")", args...)
		if err != nil {
			return err
		}
		type item struct {
			id    int64
			state string
		}
		var items []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.id, &it.state); err != nil {
				_ = rows.Close()
				return err
			}
			items = append(items, it)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		now := s.unix()
		for _, it := range items {
			if _, err := tx.ExecContext(ctx,
				"UPDATE jobs SET state = ?, lease_until = NULL, updated_at = ? WHERE id = ?", toState, now, it.id); err != nil {
				return err
			}
			if err := insertEvent(ctx, tx, it.id, now, "warn", it.state, toState, "requeued after daemon restart"); err != nil {
				return err
			}
		}
		n = len(items)
		return nil
	})
	return n, err
}

func insertEvent(ctx context.Context, tx *sql.Tx, jobID, ts int64, level, from, to, msg string) error {
	_, err := tx.ExecContext(ctx,
		"INSERT INTO job_events (job_id, ts, level, from_state, to_state, message) VALUES (?, ?, ?, ?, ?, ?)",
		jobID, ts, level, nullString(from), nullString(to), msg)
	return err
}

// JobEvent is a recorded job state change or message.
type JobEvent struct {
	ID        int64
	JobID     int64
	TS        int64
	Level     string
	FromState string
	ToState   string
	Message   string
}

// AddJobEvent records a message for a job without changing its state.
func (s *Store) AddJobEvent(ctx context.Context, jobID int64, level, message string) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		return insertEvent(ctx, tx, jobID, s.unix(), level, "", "", message)
	})
}

// JobEvents returns the events of a job, oldest first.
func (s *Store) JobEvents(ctx context.Context, jobID int64) ([]JobEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, job_id, ts, level, from_state, to_state, message FROM job_events WHERE job_id = ? ORDER BY id", jobID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []JobEvent
	for rows.Next() {
		var e JobEvent
		var from, to sql.NullString
		if err := rows.Scan(&e.ID, &e.JobID, &e.TS, &e.Level, &from, &to, &e.Message); err != nil {
			return nil, err
		}
		e.FromState, e.ToState = from.String, to.String
		out = append(out, e)
	}
	return out, rows.Err()
}

// Segment is the persisted progress of one archive segment.
type Segment struct {
	JobID          int64
	Index          int
	Offset         int64
	Size           int64
	SHA256         string
	StoredSize     *int64
	StoredHashType string
	StoredHash     string
	State          string // pending | uploaded | verified
}

// PutSegment inserts or replaces a segment row.
func (s *Store) PutSegment(ctx context.Context, seg Segment) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO job_segments (job_id, idx, start_offset, size, sha256,
		stored_size, stored_hash_type, stored_hash, state) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (job_id, idx) DO UPDATE SET start_offset = excluded.start_offset, size = excluded.size,
		sha256 = excluded.sha256, stored_size = excluded.stored_size, stored_hash_type = excluded.stored_hash_type,
		stored_hash = excluded.stored_hash, state = excluded.state`,
		seg.JobID, seg.Index, seg.Offset, seg.Size, nullString(seg.SHA256), seg.StoredSize,
		nullString(seg.StoredHashType), nullString(seg.StoredHash), seg.State)
	if err != nil {
		return fmt.Errorf("store: put segment %d/%d: %w", seg.JobID, seg.Index, err)
	}
	return nil
}

// Segments returns the segments of a job in order.
func (s *Store) Segments(ctx context.Context, jobID int64) ([]Segment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id, idx, start_offset, size, sha256, stored_size,
		stored_hash_type, stored_hash, state FROM job_segments WHERE job_id = ? ORDER BY idx`, jobID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Segment
	for rows.Next() {
		var seg Segment
		var sha, htype, h sql.NullString
		if err := rows.Scan(&seg.JobID, &seg.Index, &seg.Offset, &seg.Size, &sha, &seg.StoredSize, &htype, &h, &seg.State); err != nil {
			return nil, err
		}
		seg.SHA256, seg.StoredHashType, seg.StoredHash = sha.String, htype.String, h.String
		out = append(out, seg)
	}
	return out, rows.Err()
}

// JobCounts returns the number of jobs per state.
func (s *Store) JobCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT state, count(*) FROM jobs GROUP BY state")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[state] = n
	}
	return out, rows.Err()
}
