// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// StorageRow caches an rclone-backup storage and its repository binding.
type StorageRow struct {
	StoreID      string
	Remote       string
	BasePath     string
	Source       string
	RepoUUID     string
	SourceUUID   string
	Generation   *int64
	ConfigHash   string
	LastResyncAt *int64
	AboutJSON    string
	Health       string
	HealthDetail string
	UpdatedAt    int64
}

const storageColumns = `storeid, remote, base_path, source, repo_uuid, source_uuid, generation,
	config_hash, last_resync_at, about_json, health, health_detail, updated_at`

func scanStorage(row scanner) (*StorageRow, error) {
	var r StorageRow
	var repo, src, about, health, detail sql.NullString
	err := row.Scan(&r.StoreID, &r.Remote, &r.BasePath, &r.Source, &repo, &src, &r.Generation,
		&r.ConfigHash, &r.LastResyncAt, &about, &health, &detail, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.RepoUUID, r.SourceUUID, r.AboutJSON, r.Health, r.HealthDetail =
		repo.String, src.String, about.String, health.String, detail.String
	return &r, nil
}

// PutStorage inserts or replaces a storage row.
func (s *Store) PutStorage(ctx context.Context, r *StorageRow) error {
	r.UpdatedAt = s.unix()
	_, err := s.db.ExecContext(ctx, `INSERT INTO storages (`+storageColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (storeid) DO UPDATE SET remote = excluded.remote, base_path = excluded.base_path,
		source = excluded.source, repo_uuid = excluded.repo_uuid, source_uuid = excluded.source_uuid,
		generation = excluded.generation, config_hash = excluded.config_hash,
		last_resync_at = excluded.last_resync_at, about_json = excluded.about_json,
		health = excluded.health, health_detail = excluded.health_detail, updated_at = excluded.updated_at`,
		r.StoreID, r.Remote, r.BasePath, r.Source, nullString(r.RepoUUID), nullString(r.SourceUUID),
		r.Generation, r.ConfigHash, r.LastResyncAt, nullString(r.AboutJSON), nullString(r.Health),
		nullString(r.HealthDetail), r.UpdatedAt)
	if err != nil {
		return fmt.Errorf("store: put storage %s: %w", r.StoreID, err)
	}
	return nil
}

// GetStorage returns a storage row.
func (s *Store) GetStorage(ctx context.Context, storeID string) (*StorageRow, error) {
	return scanStorage(s.db.QueryRowContext(ctx, "SELECT "+storageColumns+" FROM storages WHERE storeid = ?", storeID))
}

// ListStorages returns all storage rows ordered by ID.
func (s *Store) ListStorages(ctx context.Context) ([]*StorageRow, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+storageColumns+" FROM storages ORDER BY storeid")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*StorageRow
	for rows.Next() {
		r, err := scanStorage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteStorage removes a storage row and, by cascade, its catalogue.
func (s *Store) DeleteStorage(ctx context.Context, storeID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM storages WHERE storeid = ?", storeID)
	return err
}

// Backup is a catalogue entry: the cached content of a remote manifest
// plus its meta document and local verification state.
type Backup struct {
	ID             int64
	StoreID        string
	Volname        string
	VMType         string
	VMID           int
	BackupTime     int64
	TSLabel        string
	CollisionIndex int
	RemoteDir      string
	Generation     int
	State          string // complete | tombstoned | deleting | damaged
	ArchiveSize    int64
	ArchiveSHA256  string
	ArchiveFormat  string
	Compression    string
	SegmentSize    int64
	SegmentCount   int
	GuestName      string
	Notes          string
	Protected      bool
	MetaDirty      bool
	DeleteAfter    *int64
	UploadedAt     int64
	ManifestJSON   string
	VerifyLevel    int
	VerifiedAt     *int64
	VerifyResult   string
}

const backupColumns = `id, storeid, volname, vmtype, vmid, backup_time, ts_label, collision_index,
	remote_dir, generation, state, archive_size, archive_sha256, archive_format, compression,
	segment_size, segment_count, guest_name, notes, protected, meta_dirty, delete_after, uploaded_at,
	manifest_json, verify_level, verified_at, verify_result`

func scanBackup(row scanner) (*Backup, error) {
	var b Backup
	var comp, guest, notes, vres sql.NullString
	err := row.Scan(&b.ID, &b.StoreID, &b.Volname, &b.VMType, &b.VMID, &b.BackupTime, &b.TSLabel,
		&b.CollisionIndex, &b.RemoteDir, &b.Generation, &b.State, &b.ArchiveSize, &b.ArchiveSHA256,
		&b.ArchiveFormat, &comp, &b.SegmentSize, &b.SegmentCount, &guest, &notes, &b.Protected,
		&b.MetaDirty, &b.DeleteAfter, &b.UploadedAt, &b.ManifestJSON, &b.VerifyLevel, &b.VerifiedAt, &vres)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	b.Compression, b.GuestName, b.Notes, b.VerifyResult = comp.String, guest.String, notes.String, vres.String
	return &b, nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func putBackup(ctx context.Context, e execer, b *Backup) error {
	_, err := e.ExecContext(ctx, `INSERT INTO backups (storeid, volname, vmtype, vmid, backup_time,
		ts_label, collision_index, remote_dir, generation, state, archive_size, archive_sha256,
		archive_format, compression, segment_size, segment_count, guest_name, notes, protected, meta_dirty,
		delete_after, uploaded_at, manifest_json, verify_level, verified_at, verify_result)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (storeid, volname) DO UPDATE SET vmtype = excluded.vmtype, vmid = excluded.vmid,
		backup_time = excluded.backup_time, ts_label = excluded.ts_label,
		collision_index = excluded.collision_index, remote_dir = excluded.remote_dir,
		generation = excluded.generation, state = excluded.state, archive_size = excluded.archive_size,
		archive_sha256 = excluded.archive_sha256, archive_format = excluded.archive_format,
		compression = excluded.compression, segment_size = excluded.segment_size,
		segment_count = excluded.segment_count, guest_name = excluded.guest_name, notes = excluded.notes,
		protected = excluded.protected, meta_dirty = excluded.meta_dirty,
		delete_after = excluded.delete_after, uploaded_at = excluded.uploaded_at,
		manifest_json = excluded.manifest_json, verify_level = excluded.verify_level,
		verified_at = excluded.verified_at, verify_result = excluded.verify_result`,
		b.StoreID, b.Volname, b.VMType, b.VMID, b.BackupTime, b.TSLabel, b.CollisionIndex, b.RemoteDir,
		b.Generation, b.State, b.ArchiveSize, b.ArchiveSHA256, b.ArchiveFormat, nullString(b.Compression),
		b.SegmentSize, b.SegmentCount, nullString(b.GuestName), nullString(b.Notes), b.Protected, b.MetaDirty,
		b.DeleteAfter, b.UploadedAt, b.ManifestJSON, b.VerifyLevel, b.VerifiedAt, nullString(b.VerifyResult))
	return err
}

// PutBackup inserts or replaces a catalogue entry (keyed by storage and
// volname).
func (s *Store) PutBackup(ctx context.Context, b *Backup) error {
	if err := putBackup(ctx, s.db, b); err != nil {
		return fmt.Errorf("store: put backup %s:%s: %w", b.StoreID, b.Volname, err)
	}
	return nil
}

// GetBackup returns a catalogue entry.
func (s *Store) GetBackup(ctx context.Context, storeID, volname string) (*Backup, error) {
	return scanBackup(s.db.QueryRowContext(ctx,
		"SELECT "+backupColumns+" FROM backups WHERE storeid = ? AND volname = ?", storeID, volname))
}

// BackupFilter selects catalogue entries. Zero values match everything.
type BackupFilter struct {
	VMID   int
	VMType string
	States []string
}

// ListBackups returns a storage's catalogue ordered by guest and time.
func (s *Store) ListBackups(ctx context.Context, storeID string, f BackupFilter) ([]*Backup, error) {
	q := "SELECT " + backupColumns + " FROM backups WHERE storeid = ?"
	args := []any{storeID}
	if f.VMID != 0 {
		q, args = q+" AND vmid = ?", append(args, f.VMID)
	}
	if f.VMType != "" {
		q, args = q+" AND vmtype = ?", append(args, f.VMType)
	}
	if len(f.States) > 0 {
		q += " AND state IN ("
		for i, st := range f.States {
			if i > 0 {
				q += ","
			}
			q += "?"
			args = append(args, st)
		}
		q += ")"
	}
	q += " ORDER BY vmtype, vmid, backup_time, collision_index"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// DeleteBackup removes a catalogue entry.
func (s *Store) DeleteBackup(ctx context.Context, storeID, volname string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM backups WHERE storeid = ? AND volname = ?", storeID, volname)
	return err
}

// ReplaceCatalog atomically replaces a storage's catalogue with the
// entries rebuilt from the remote repository. Local verification state of
// entries whose archive digest is unchanged is preserved.
func (s *Store) ReplaceCatalog(ctx context.Context, storeID string, backups []*Backup) error {
	return s.Tx(ctx, func(tx *sql.Tx) error {
		type verified struct {
			sha    string
			level  int
			at     *int64
			result sql.NullString
		}
		prev := map[string]verified{}
		rows, err := tx.QueryContext(ctx,
			"SELECT volname, archive_sha256, verify_level, verified_at, verify_result FROM backups WHERE storeid = ?", storeID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var vol string
			var v verified
			if err := rows.Scan(&vol, &v.sha, &v.level, &v.at, &v.result); err != nil {
				_ = rows.Close()
				return err
			}
			prev[vol] = v
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM backups WHERE storeid = ?", storeID); err != nil {
			return err
		}
		for _, b := range backups {
			if b.StoreID != storeID {
				return fmt.Errorf("store: catalogue entry %s belongs to storage %s, not %s", b.Volname, b.StoreID, storeID)
			}
			if v, ok := prev[b.Volname]; ok && v.sha == b.ArchiveSHA256 && v.level > b.VerifyLevel {
				b.VerifyLevel, b.VerifiedAt, b.VerifyResult = v.level, v.at, v.result.String
			}
			if err := putBackup(ctx, tx, b); err != nil {
				return fmt.Errorf("store: replace catalogue %s: %w", b.Volname, err)
			}
		}
		return nil
	})
}

// NewestBackup returns the newest backup time of a guest in a storage's
// catalogue among the given states (0 if none).
func (s *Store) NewestBackup(ctx context.Context, storeID, vmtype string, vmid int, states []string) (int64, error) {
	if len(states) == 0 {
		return 0, nil
	}
	args := []any{storeID, vmtype, vmid}
	for _, st := range states {
		args = append(args, st)
	}
	var newest sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(backup_time) FROM backups WHERE storeid = ? AND vmtype = ? AND vmid = ?
		AND state IN (`+strings.TrimSuffix(strings.Repeat("?,", len(states)), ",")+`)`, args...).Scan(&newest)
	return newest.Int64, err
}
