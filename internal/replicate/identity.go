// SPDX-License-Identifier: AGPL-3.0-or-later

package replicate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/manifest"
)

// Identity identifies this PVE installation (cluster or standalone node)
// as the owner of a source namespace in repositories.
type Identity struct {
	UUID      string    `json:"uuid"`
	CreatedAt time.Time `json:"created_at"`
}

// Locker serializes work across the cluster (cfs.Locker).
type Locker interface {
	Do(ctx context.Context, id string, fn func(ctx context.Context) error) error
}

// LoadIdentity reads the installation identity from the cluster file
// system, creating it on first use. Creation happens under a cluster lock:
// nodes starting together must not each write their own identity (a rename
// replaces an existing file, and pmxcfs has no hard links to avoid that).
func LoadIdentity(ctx context.Context, pveDir string, lock Locker) (*Identity, error) {
	path := filepath.Join(pveDir, "pve-rclone-backup", "source.json")
	if id, err := readIdentity(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return id, err
	}
	var id *Identity
	err := lock.Do(ctx, "pve-rclone-backup-identity", func(context.Context) error {
		// Another node may have created it before this one got the lock.
		var err error
		if id, err = readIdentity(path); err == nil || !errors.Is(err, os.ErrNotExist) {
			return err
		}
		id, err = createIdentity(path)
		return err
	})
	return id, err
}

func createIdentity(path string) (*Identity, error) {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b)
	id := &Identity{UUID: h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], CreatedAt: time.Now().UTC()}
	data, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // pmxcfs manages permissions
		return nil, err
	}
	tmp := path + ".tmp." + h[:8]
	if err := os.WriteFile(tmp, append(data, '\n'), 0o640); err != nil { //nolint:gosec // not secret
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	return readIdentity(path)
}

func readIdentity(path string) (*Identity, error) {
	data, err := os.ReadFile(path) //nolint:gosec // fixed path below the PVE directory
	if err != nil {
		return nil, err
	}
	var id Identity
	if err := json.Unmarshal(data, &id); err != nil {
		return nil, fmt.Errorf("replicate: parse %s: %w", path, err)
	}
	if !manifest.ValidUUID(id.UUID) {
		return nil, fmt.Errorf("replicate: %s holds an invalid UUID", path)
	}
	return &id, nil
}
