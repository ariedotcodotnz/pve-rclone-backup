// SPDX-License-Identifier: AGPL-3.0-or-later

// Package secrets stores credentials in the root-only, cluster-wide
// /etc/pve/priv/pve-rclone-backup directory: rclone transport remotes
// (OAuth tokens) in remotes.conf and crypt keys in keys/<repo-uuid>.json.
// Nothing secret is ever written to storage.cfg.
package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/rclone/rclone/fs/config"
)

// DefaultDir is where secrets live on a PVE node.
const DefaultDir = "/etc/pve/priv/pve-rclone-backup"

// Locker is a cluster-wide lock (implemented by cfs.Locker).
type Locker interface {
	Do(ctx context.Context, id string, fn func(ctx context.Context) error) error
}

const remotesLockID = "pve-rclone-backup-remotes"

type change struct {
	section       string
	key           string
	value         *string // nil deletes the key
	deleteSection bool
}

// RemoteStore implements rclone's config.Storage on top of remotes.conf.
//
// Every node's daemon may change the file (OAuth token refreshes, remote
// setup), so changes are recorded and merged into the current file under
// the cluster lock on Save instead of overwriting it with a stale view.
// Changes that cannot be persisted (no quorum) stay pending and are
// retried by the next Save.
type RemoteStore struct {
	path string
	lock Locker
	log  *slog.Logger

	flushMu sync.Mutex // serializes Flush
	mu      sync.Mutex
	base    sections // last content read from disk
	data    sections // base plus pending changes
	pending []change
	loaded  bool
}

var _ config.Storage = (*RemoteStore)(nil)

// NewRemoteStore returns a store for the remotes file at path.
func NewRemoteStore(path string, lock Locker, log *slog.Logger) *RemoteStore {
	if log == nil {
		log = slog.Default()
	}
	return &RemoteStore{path: path, lock: lock, log: log, base: sections{}, data: sections{}}
}

// Path returns the file backing the store.
func (s *RemoteStore) Path() string { return s.path }

func apply(dst sections, changes []change) {
	for _, c := range changes {
		switch {
		case c.deleteSection:
			delete(dst, c.section)
		case c.value == nil:
			delete(dst[c.section], c.key)
		default:
			if dst[c.section] == nil {
				dst[c.section] = map[string]string{}
			}
			dst[c.section][c.key] = *c.value
		}
	}
}

func (s *RemoteStore) record(c change) {
	s.pending = append(s.pending, c)
	apply(s.data, []change{c})
}

func (s *RemoteStore) GetSectionList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.data))
}

func (s *RemoteStore) HasSection(section string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data[section]
	return ok
}

func (s *RemoteStore) DeleteSection(section string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[section]; ok {
		s.record(change{section: section, deleteSection: true})
	}
}

func (s *RemoteStore) GetKeyList(section string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.data[section]))
}

func (s *RemoteStore) GetValue(section, key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[section][key]
	return v, ok
}

func (s *RemoteStore) SetValue(section, key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.data[section][key]; ok && cur == value {
		return
	}
	s.record(change{section: section, key: key, value: &value})
}

func (s *RemoteStore) DeleteKey(section, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[section][key]; !ok {
		return false
	}
	s.record(change{section: section, key: key})
	return true
}

func (s *RemoteStore) readFile() (sections, error) {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	return parseINI(raw)
}

// Load (re)reads the file and reapplies pending changes.
//
// rclone exits the process if Load returns anything other than nil or
// config.ErrorConfigFileNotFound, so read and parse errors are logged and
// the last good content is kept.
func (s *RemoteStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	base, err := s.readFile()
	switch {
	case errors.Is(err, os.ErrNotExist):
		if !s.loaded {
			s.loaded = true
			return config.ErrorConfigFileNotFound
		}
		base = sections{}
	case err != nil:
		s.log.Error("cannot read remotes file; keeping the previous configuration", "path", s.path, "err", err)
		return nil
	}
	s.loaded = true
	s.base = base
	s.data = base.clone()
	apply(s.data, s.pending)
	return nil
}

// Save persists pending changes, merged into the current file under the
// cluster lock.
func (s *RemoteStore) Save() error { return s.Flush(context.Background()) }

// Flush persists pending changes. It returns an error (and keeps the
// changes pending) when the cluster filesystem is not writable.
func (s *RemoteStore) Flush(ctx context.Context) error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	if len(s.pending) == 0 {
		s.mu.Unlock()
		return nil
	}
	pending := slices.Clone(s.pending)
	s.mu.Unlock()

	var merged sections
	err := s.lock.Do(ctx, remotesLockID, func(context.Context) error {
		cur, err := s.readFile()
		switch {
		case errors.Is(err, os.ErrNotExist):
			cur = sections{}
		case err != nil:
			return fmt.Errorf("read %s: %w", s.path, err)
		}
		apply(cur, pending)
		if err := writeFileAtomic(s.path, encodeINI(cur)); err != nil {
			return err
		}
		merged = cur
		return nil
	})
	if err != nil {
		return fmt.Errorf("secrets: save remotes: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Changes recorded while we were writing remain pending.
	s.pending = s.pending[len(pending):]
	s.base = merged
	s.data = merged.clone()
	apply(s.data, s.pending)
	return nil
}

// Pending reports how many changes are not yet persisted.
func (s *RemoteStore) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// Serialize returns the configuration as JSON. It contains credentials.
func (s *RemoteStore) Serialize() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(s.data)
	return string(b), err
}

// RemotesPath returns the remotes file inside a secrets directory.
func RemotesPath(dir string) string { return filepath.Join(dir, "remotes.conf") }
