// SPDX-License-Identifier: AGPL-3.0-or-later

package recoverykit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/secrets"
)

// Record tracks the recovery kit of a repository. Replication of an
// encrypted repository waits until a kit holding its keys was exported
// and its checksum confirmed by the operator.
type Record struct {
	ExportedAt  *time.Time `json:"exported_at,omitempty"`
	Checksum    string     `json:"checksum,omitempty"` // of the last exported kit
	ConfirmedAt *time.Time `json:"confirmed_at,omitempty"`
}

// Ledger stores records cluster-wide (no secrets) under a cluster lock.
type Ledger struct {
	path string
	lock secrets.Locker
}

// NewLedger returns a ledger stored at path.
func NewLedger(path string, lock secrets.Locker) *Ledger { return &Ledger{path: path, lock: lock} }

// Load returns all records by repository UUID.
func (l *Ledger) Load() (map[string]Record, error) {
	data, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]Record{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("recoverykit: parse %s: %w", l.path, err)
	}
	return out, nil
}

// Confirmed reports whether the repository's kit was confirmed.
func (l *Ledger) Confirmed(repoUUID string) bool {
	recs, err := l.Load()
	return err == nil && recs[repoUUID].ConfirmedAt != nil
}

func (l *Ledger) update(ctx context.Context, fn func(map[string]Record) error) error {
	return l.lock.Do(ctx, "pve-rclone-backup-kits", func(context.Context) error {
		recs, err := l.Load()
		if err != nil {
			return err
		}
		if err := fn(recs); err != nil {
			return err
		}
		data, err := json.MarshalIndent(recs, "", "  ")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil { //nolint:gosec // pmxcfs manages permissions
			return err
		}
		tmp := l.path + ".tmp"
		if err := os.WriteFile(tmp, append(data, '\n'), 0o640); err != nil { //nolint:gosec // not secret
			return err
		}
		return os.Rename(tmp, l.path)
	})
}

// Exported records a kit export. A repository confirmed earlier stays
// confirmed: any confirmed kit is enough to recover it.
func (l *Ledger) Exported(ctx context.Context, repos []string, checksum string, at time.Time) error {
	return l.update(ctx, func(recs map[string]Record) error {
		for _, r := range repos {
			rec := recs[r]
			t := at.UTC()
			rec.ExportedAt, rec.Checksum = &t, checksum
			recs[r] = rec
		}
		return nil
	})
}

// Confirm confirms the repositories whose last exported kit has the
// checksum and returns them.
func (l *Ledger) Confirm(ctx context.Context, checksum string, at time.Time) ([]string, error) {
	var confirmed []string
	err := l.update(ctx, func(recs map[string]Record) error {
		for r, rec := range recs {
			if rec.Checksum != "" && rec.Checksum == checksum {
				t := at.UTC()
				rec.ConfirmedAt = &t
				recs[r] = rec
				confirmed = append(confirmed, r)
			}
		}
		return nil
	})
	slices.Sort(confirmed)
	return confirmed, err
}

// MarkConfirmed confirms repositories directly (their keys were imported
// from a kit the operator holds).
func (l *Ledger) MarkConfirmed(ctx context.Context, repos []string, at time.Time) error {
	return l.update(ctx, func(recs map[string]Record) error {
		for _, r := range repos {
			rec := recs[r]
			t := at.UTC()
			rec.ConfirmedAt = &t
			recs[r] = rec
		}
		return nil
	})
}
