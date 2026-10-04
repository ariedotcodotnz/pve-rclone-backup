// SPDX-License-Identifier: AGPL-3.0-or-later

package transport

import (
	"encoding/json"
	"maps"
	"slices"
	"sync"

	"github.com/rclone/rclone/fs/config"
)

// MemoryStorage is an in-memory rclone config.Storage. It backs tests and
// the fixture server; the daemon uses a pmxcfs-backed implementation.
type MemoryStorage struct {
	mu       sync.RWMutex
	sections map[string]map[string]string
}

var _ config.Storage = (*MemoryStorage)(nil)

// NewMemoryStorage returns an empty MemoryStorage.
func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{sections: map[string]map[string]string{}}
}

// SetSection replaces a whole section.
func (s *MemoryStorage) SetSection(section string, kv map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sections[section] = maps.Clone(kv)
}

func (s *MemoryStorage) GetSectionList() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Sorted(maps.Keys(s.sections))
}

func (s *MemoryStorage) HasSection(section string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.sections[section]
	return ok
}

func (s *MemoryStorage) DeleteSection(section string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sections, section)
}

func (s *MemoryStorage) GetKeyList(section string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Sorted(maps.Keys(s.sections[section]))
}

func (s *MemoryStorage) GetValue(section, key string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.sections[section][key]
	return v, ok
}

func (s *MemoryStorage) SetValue(section, key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sections[section] == nil {
		s.sections[section] = map[string]string{}
	}
	s.sections[section][key] = value
}

func (s *MemoryStorage) DeleteKey(section, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sections[section][key]; !ok {
		return false
	}
	delete(s.sections[section], key)
	return true
}

func (s *MemoryStorage) Load() error { return nil }
func (s *MemoryStorage) Save() error { return nil }

func (s *MemoryStorage) Serialize() (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, err := json.Marshal(s.sections)
	return string(b), err
}
