// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Verification is one verification run of a backup.
type Verification struct {
	ID          int64
	StoreID     string
	Volname     string
	Level       int
	StartedAt   int64
	FinishedAt  *int64
	Result      string // ok | failed | error, empty while running
	DetailsJSON string
}

// AddVerification records a verification run and returns its id.
func (s *Store) AddVerification(ctx context.Context, v *Verification) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO verifications (storeid, volname, level, started_at,
		finished_at, result, details_json) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		v.StoreID, v.Volname, v.Level, v.StartedAt, v.FinishedAt, nullString(v.Result), nullString(v.DetailsJSON))
	if err != nil {
		return 0, fmt.Errorf("store: add verification: %w", err)
	}
	return res.LastInsertId()
}

// FinishVerification stores the outcome of a verification run.
func (s *Store) FinishVerification(ctx context.Context, id int64, result, detailsJSON string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE verifications SET finished_at = ?, result = ?, details_json = ? WHERE id = ?",
		s.unix(), result, nullString(detailsJSON), id)
	return err
}

// Verifications returns the verification history of a storage (newest
// first), optionally for one backup.
func (s *Store) Verifications(ctx context.Context, storeID, volname string, limit int) ([]Verification, error) {
	q := `SELECT id, storeid, volname, level, started_at, finished_at, result, details_json
		FROM verifications WHERE storeid = ?`
	args := []any{storeID}
	if volname != "" {
		q, args = q+" AND volname = ?", append(args, volname)
	}
	q += " ORDER BY id DESC"
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Verification
	for rows.Next() {
		var v Verification
		var result, details sql.NullString
		if err := rows.Scan(&v.ID, &v.StoreID, &v.Volname, &v.Level, &v.StartedAt, &v.FinishedAt, &result, &details); err != nil {
			return nil, err
		}
		v.Result, v.DetailsJSON = result.String, details.String
		out = append(out, v)
	}
	return out, rows.Err()
}

// IdempotentResponse returns a stored response for an idempotency key that
// is younger than maxAgeSeconds.
func (s *Store) IdempotentResponse(ctx context.Context, key string, maxAgeSeconds int64) (string, bool, error) {
	var resp string
	err := s.db.QueryRowContext(ctx,
		"SELECT response_json FROM idempotency WHERE key = ? AND created_at >= ?", key, s.unix()-maxAgeSeconds).Scan(&resp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return resp, err == nil, err
}

// PutIdempotentResponse stores the response for an idempotency key and
// drops entries older than maxAgeSeconds.
func (s *Store) PutIdempotentResponse(ctx context.Context, key, responseJSON string, maxAgeSeconds int64) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		now := s.unix()
		if _, err := tx.ExecContext(ctx, "DELETE FROM idempotency WHERE created_at < ?", now-maxAgeSeconds); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO idempotency (key, response_json, created_at) VALUES (?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET response_json = excluded.response_json, created_at = excluded.created_at`,
			key, responseJSON, now)
		return err
	})
}

// FetchedArchive records an archive placed into a local storage by fetch.
type FetchedArchive struct {
	Path    string
	Dev     *int64
	Ino     *int64
	StoreID string
	Volname string
	SHA256  string
}

// AddFetchedArchive records a fetched archive.
func (s *Store) AddFetchedArchive(ctx context.Context, f FetchedArchive) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO fetched_archives (path, dev, ino, storeid, volname, sha256, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (path) DO UPDATE SET dev = excluded.dev, ino = excluded.ino,
		storeid = excluded.storeid, volname = excluded.volname, sha256 = excluded.sha256, created_at = excluded.created_at`,
		f.Path, f.Dev, f.Ino, nullString(f.StoreID), nullString(f.Volname), nullString(f.SHA256), s.unix())
	return err
}

// FetchedArchive returns the fetch record for a local path.
func (s *Store) FetchedArchive(ctx context.Context, path string) (*FetchedArchive, error) {
	var f FetchedArchive
	var storeID, volname, sha sql.NullString
	err := s.db.QueryRowContext(ctx,
		"SELECT path, dev, ino, storeid, volname, sha256 FROM fetched_archives WHERE path = ?", path).
		Scan(&f.Path, &f.Dev, &f.Ino, &storeID, &volname, &sha)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	f.StoreID, f.Volname, f.SHA256 = storeID.String, volname.String, sha.String
	return &f, nil
}

// Alert is an operator-facing problem such as "reconnect remote".
type Alert struct {
	ID        string
	Severity  string // info | warning | error
	StoreID   string
	Message   string
	RaisedAt  int64
	ClearedAt *int64
}

// RaiseAlert raises or refreshes an alert by id. Re-raising a cleared
// alert reopens it.
func (s *Store) RaiseAlert(ctx context.Context, a Alert) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO alerts (id, severity, storeid, message, raised_at, cleared_at)
		VALUES (?, ?, ?, ?, ?, NULL) ON CONFLICT (id) DO UPDATE SET severity = excluded.severity,
		storeid = excluded.storeid, message = excluded.message,
		raised_at = CASE WHEN alerts.cleared_at IS NULL THEN alerts.raised_at ELSE excluded.raised_at END,
		cleared_at = NULL`,
		a.ID, a.Severity, nullString(a.StoreID), a.Message, s.unix())
	return err
}

// ClearAlert marks an alert as resolved.
func (s *Store) ClearAlert(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE alerts SET cleared_at = ? WHERE id = ? AND cleared_at IS NULL", s.unix(), id)
	return err
}

// ActiveAlerts returns unresolved alerts, oldest first.
func (s *Store) ActiveAlerts(ctx context.Context) ([]Alert, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, severity, storeid, message, raised_at, cleared_at FROM alerts WHERE cleared_at IS NULL ORDER BY raised_at, id")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Alert
	for rows.Next() {
		var a Alert
		var storeID sql.NullString
		if err := rows.Scan(&a.ID, &a.Severity, &storeID, &a.Message, &a.RaisedAt, &a.ClearedAt); err != nil {
			return nil, err
		}
		a.StoreID = storeID.String
		out = append(out, a)
	}
	return out, rows.Err()
}
