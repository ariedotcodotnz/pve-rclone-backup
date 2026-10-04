// SPDX-License-Identifier: AGPL-3.0-or-later

package store

import (
	"context"
	"database/sql"
	"errors"
)

// Repository is a repository known to this node.
type Repository struct {
	UUID       string
	Remote     string
	BasePath   string
	Encryption string // crypt | none
	CreatedAt  int64
	UpdatedAt  int64
}

// PutRepository records a repository. A repository previously recorded at
// the same location is replaced: only one repository can live there.
func (s *Store) PutRepository(ctx context.Context, r *Repository) error {
	now := s.unix()
	return s.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM repositories WHERE remote = ? AND base_path = ? AND uuid <> ?",
			r.Remote, r.BasePath, r.UUID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO repositories (uuid, remote, base_path, encryption, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (uuid) DO UPDATE SET remote = excluded.remote, base_path = excluded.base_path,
				encryption = excluded.encryption, updated_at = excluded.updated_at`,
			r.UUID, r.Remote, r.BasePath, r.Encryption, now, now)
		return err
	})
}

// FindRepository returns the repository known at a location or ErrNotFound.
func (s *Store) FindRepository(ctx context.Context, remote, basePath string) (*Repository, error) {
	var r Repository
	err := s.db.QueryRowContext(ctx, `SELECT uuid, remote, base_path, encryption, created_at, updated_at
		FROM repositories WHERE remote = ? AND base_path = ?`, remote, basePath).
		Scan(&r.UUID, &r.Remote, &r.BasePath, &r.Encryption, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ListRepositories returns all known repositories.
func (s *Store) ListRepositories(ctx context.Context) ([]*Repository, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT uuid, remote, base_path, encryption, created_at, updated_at
		FROM repositories ORDER BY remote, base_path`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*Repository
	for rows.Next() {
		var r Repository
		if err := rows.Scan(&r.UUID, &r.Remote, &r.BasePath, &r.Encryption, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}
