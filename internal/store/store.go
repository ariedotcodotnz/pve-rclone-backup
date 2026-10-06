// SPDX-License-Identifier: AGPL-3.0-or-later

// Package store persists the daemon's operational state in SQLite.
//
// The database is a cache plus a work queue: remote manifests are the
// authority for what exists offsite, and a lost or corrupt database is
// rebuilt from them. It is therefore safe to move a damaged file aside.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite" // database/sql driver "sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// migrationFS holds the migrations directory; tests substitute it.
var migrationFS fs.FS = embeddedMigrations

var (
	// ErrNewerSchema means the database was written by a newer daemon.
	ErrNewerSchema = errors.New("store: database schema is newer than this daemon supports")
	// ErrCorrupt means SQLite reported an integrity problem.
	ErrCorrupt = errors.New("store: database is corrupt")
)

// Store is the daemon's state database.
type Store struct {
	db   *sql.DB
	path string
	now  func() time.Time
}

type migration struct {
	version int
	name    string
	sql     string
}

func migrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || !strings.HasSuffix(e.Name(), ".sql") {
			return nil, fmt.Errorf("store: bad migration file name %q", e.Name())
		}
		b, err := fs.ReadFile(migrationFS, "migrations/"+e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(b)})
	}
	slices.SortFunc(out, func(a, b migration) int { return a.version - b.version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("store: migrations must be numbered 1..n without gaps, found %q", m.name)
		}
	}
	return out, nil
}

// LatestSchemaVersion is the schema version this binary creates.
func LatestSchemaVersion() int {
	m, err := migrations()
	if err != nil {
		panic(err)
	}
	return len(m)
}

// Open opens (creating if needed) the database at path and migrates it to
// the latest schema. Before migrating an existing database a consistent
// copy is written next to it as <path>.schema-<version it had>.
//
// It returns ErrCorrupt only when SQLite reports the file as corrupt or not
// a database, and ErrNewerSchema when the schema is newer than supported;
// the caller decides whether to rebuild (see MoveAside). Other errors, a
// cancelled context included, say nothing about the file.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: create directory: %w", err)
	}
	if err := privateFiles(path); err != nil {
		return nil, err
	}
	dsn := "file:" + uriPathEscaper.Replace(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(FULL)" +
		"&_pragma=foreign_keys(ON)" +
		"&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(4)
	s := &Store{db: db, path: path, now: time.Now}

	if err := s.checkIntegrity(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// placeholders returns n comma-separated SQL parameter placeholders.
func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

// sqlLimit converts a limit where zero or less means unlimited, as the
// listing functions take it, to SQLite's (-1 is unlimited).
func sqlLimit(limit int) int {
	if limit <= 0 {
		return -1
	}
	return limit
}

// uriPathEscaper escapes a file name for the "file:" URI SQLite is given,
// in which "?", "#" and "%" would otherwise start the query, start a
// fragment or be decoded.
var uriPathEscaper = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")

// privateFiles makes the database and its WAL files private before SQLite
// opens them: SQLite creates the WAL and shared-memory files with the
// database file's permissions, whatever the process umask.
func privateFiles(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600) //nolint:gosec // our state file
	if err != nil {
		return fmt.Errorf("store: create %s: %w", path, err)
	}
	_ = f.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("store: chmod: %w", err)
		}
	}
	return nil
}

// isCorruption reports whether SQLite found the file corrupt or not a
// database at all.
func isCorruption(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	code := se.Code() & 0xff // primary result code
	return code == sqlite3.SQLITE_CORRUPT || code == sqlite3.SQLITE_NOTADB
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

func (s *Store) checkIntegrity(ctx context.Context) error {
	var res string
	if err := s.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&res); err != nil {
		if isCorruption(err) {
			return fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
		return fmt.Errorf("store: integrity check: %w", err)
	}
	if res != "ok" {
		return fmt.Errorf("%w: %s", ErrCorrupt, res)
	}
	return nil
}

func (s *Store) schemaVersion(ctx context.Context) (int, error) {
	var exists int
	err := s.db.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'meta'").Scan(&exists)
	if err != nil || exists == 0 {
		return 0, err
	}
	var v string
	err = s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'schema_version'").Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(v)
}

func (s *Store) migrate(ctx context.Context) error {
	all, err := migrations()
	if err != nil {
		return err
	}
	current, err := s.schemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if current > len(all) {
		return fmt.Errorf("%w (database %d, supported %d)", ErrNewerSchema, current, len(all))
	}
	if current == len(all) {
		return nil
	}
	if current > 0 {
		// Named after the version it holds: when an upgrade over several
		// versions stops halfway, the next attempt starts from a newer
		// version and must not replace the copy of the original one.
		backup := fmt.Sprintf("%s.schema-%d", s.path, current)
		_ = os.Remove(backup)
		// VACUUM INTO fills an existing empty file and keeps its mode.
		f, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // next to our state file
		if err != nil {
			return fmt.Errorf("store: copy database before migration: %w", err)
		}
		_ = f.Close()
		if _, err := s.db.ExecContext(ctx, "VACUUM INTO ?", backup); err != nil {
			return fmt.Errorf("store: copy database before migration: %w", err)
		}
	}
	for _, m := range all[current:] {
		err := s.Tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return fmt.Errorf("apply %s: %w", m.name, err)
			}
			_, err := tx.ExecContext(ctx,
				"INSERT INTO meta (key, value) VALUES ('schema_version', ?) "+
					"ON CONFLICT (key) DO UPDATE SET value = excluded.value", strconv.Itoa(m.version))
			return err
		})
		if err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
	}
	return nil
}

// Tx runs fn in a write transaction (BEGIN IMMEDIATE) and commits it if fn
// returns nil.
func (s *Store) Tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// Meta returns a value from the meta table.
func (s *Store) Meta(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// SetMeta stores a value in the meta table.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value",
		key, value)
	return err
}

// MoveAside renames a database that cannot be used (corrupt or newer) to
// <path>.<reason>-<unix time>, together with its WAL files, so a fresh one
// can be created and rebuilt from the remote repositories.
func MoveAside(path, reason string, now time.Time) (string, error) {
	dst := fmt.Sprintf("%s.%s-%d", path, reason, now.Unix())
	if err := os.Rename(path, dst); err != nil {
		return "", fmt.Errorf("store: move %s aside: %w", path, err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Rename(path+suffix, dst+suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return dst, fmt.Errorf("store: move %s aside: %w", path+suffix, err)
		}
	}
	return dst, nil
}

func (s *Store) unix() int64 { return s.now().Unix() }
