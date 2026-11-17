// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"fmt"
)

// IncompleteUpload is a backup directory without a manifest found by a
// resync: an upload in progress or abandoned, or a deletion that stopped
// half-way ("deleting").
type IncompleteUpload struct {
	StoreID     string
	RemoteDir   string // g<N>/v1/<source>/<type>/<vmid>/<timestamp>[.<n>]
	State       string // incomplete | deleting
	VMType      string
	VMID        int
	BackupTime  int64
	Files       int
	StoredBytes int64
	Fingerprint string // names and sizes of the files
	FirstSeenAt int64
	ChangedAt   int64 // when its files were last seen to change
}

const incompleteColumns = `storeid, remote_dir, state, vmtype, vmid, backup_time, files, stored_bytes,
	fingerprint, first_seen_at, changed_at`

func scanIncomplete(row scanner) (*IncompleteUpload, error) {
	var u IncompleteUpload
	err := row.Scan(&u.StoreID, &u.RemoteDir, &u.State, &u.VMType, &u.VMID, &u.BackupTime, &u.Files, &u.StoredBytes,
		&u.Fingerprint, &u.FirstSeenAt, &u.ChangedAt)
	return &u, err
}

// ReplaceIncomplete records the incomplete uploads a resync of a storage
// found, replacing those recorded before; a resync records them through
// CatalogSync.Replace. One seen before keeps when it was first seen and,
// unless its files changed, when they last changed. A directory that holds
// a catalogued backup is not an incomplete upload.
func (s *Store) ReplaceIncomplete(ctx context.Context, storeID string, list []IncompleteUpload) error {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	return s.Tx(ctx, func(tx *sql.Tx) error { return replaceIncomplete(ctx, tx, storeID, list, nil, s.unix()) })
}

// replaceIncomplete replaces a storage's incomplete uploads, leaving out
// those named in gone and the directories of catalogued backups.
func replaceIncomplete(ctx context.Context, tx *sql.Tx, storeID string, list []IncompleteUpload, gone map[string]bool, now int64) error {
	prev := map[string]*IncompleteUpload{}
	committed := map[string]bool{}
	rows, err := tx.QueryContext(ctx, "SELECT "+incompleteColumns+" FROM incomplete_uploads WHERE storeid = ?", storeID)
	if err != nil {
		return err
	}
	for rows.Next() {
		u, err := scanIncomplete(rows)
		if err != nil {
			_ = rows.Close()
			return err
		}
		prev[u.RemoteDir] = u
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if rows, err = tx.QueryContext(ctx, "SELECT remote_dir FROM backups WHERE storeid = ?", storeID); err != nil {
		return err
	}
	for rows.Next() {
		var dir string
		if err := rows.Scan(&dir); err != nil {
			_ = rows.Close()
			return err
		}
		committed[dir] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM incomplete_uploads WHERE storeid = ?", storeID); err != nil {
		return err
	}
	for _, u := range list {
		if gone[u.RemoteDir] || committed[u.RemoteDir] {
			continue
		}
		u.StoreID, u.FirstSeenAt, u.ChangedAt = storeID, now, now
		if p, ok := prev[u.RemoteDir]; ok {
			u.FirstSeenAt = p.FirstSeenAt
			if p.Fingerprint == u.Fingerprint {
				u.ChangedAt = p.ChangedAt
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO incomplete_uploads ("+incompleteColumns+") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			u.StoreID, u.RemoteDir, u.State, u.VMType, u.VMID, u.BackupTime, u.Files, u.StoredBytes, u.Fingerprint,
			u.FirstSeenAt, u.ChangedAt); err != nil {
			return fmt.Errorf("store: record incomplete upload %s: %w", u.RemoteDir, err)
		}
	}
	return nil
}

// ListIncomplete returns a storage's incomplete uploads ordered by guest
// and time.
func (s *Store) ListIncomplete(ctx context.Context, storeID string) ([]*IncompleteUpload, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+incompleteColumns+
		" FROM incomplete_uploads WHERE storeid = ? ORDER BY vmtype, vmid, backup_time, remote_dir", storeID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*IncompleteUpload
	for rows.Next() {
		u, err := scanIncomplete(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeleteIncomplete forgets an incomplete upload or interrupted deletion
// once its directory was removed. A resync running meanwhile, whose scan
// may have seen the directory, does not record it again.
func (s *Store) DeleteIncomplete(ctx context.Context, storeID, remoteDir string) error {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	for c := range s.catalogSyncs {
		if c.storeID == storeID {
			c.gone[remoteDir] = true
		}
	}
	_, err := s.db.ExecContext(ctx, "DELETE FROM incomplete_uploads WHERE storeid = ? AND remote_dir = ?", storeID, remoteDir)
	return err
}

// GuestJob is a replication job's guest and state.
type GuestJob struct {
	ID         int64
	VMType     string
	VMID       int
	BackupTime int64
	State      string
	ParamsJSON string
}

// ReplicateJobs returns the guest and state of a storage's replication
// jobs, newest first.
func (s *Store) ReplicateJobs(ctx context.Context, storeID string) ([]GuestJob, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, vmtype, vmid, backup_time, state, params_json FROM jobs
		WHERE kind = 'replicate' AND storeid = ? AND vmtype IS NOT NULL ORDER BY id DESC`, storeID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []GuestJob
	for rows.Next() {
		var j GuestJob
		var vmid, bt sql.NullInt64
		if err := rows.Scan(&j.ID, &j.VMType, &vmid, &bt, &j.State, &j.ParamsJSON); err != nil {
			return nil, err
		}
		j.VMID, j.BackupTime = int(vmid.Int64), bt.Int64
		out = append(out, j)
	}
	return out, rows.Err()
}
