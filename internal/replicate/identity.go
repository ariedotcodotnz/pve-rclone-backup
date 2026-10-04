// SPDX-License-Identifier: AGPL-3.0-or-later

package replicate

import (
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

// LoadIdentity reads the installation identity from the cluster file
// system, creating it on first use.
func LoadIdentity(pveDir string) (*Identity, error) {
	path := filepath.Join(pveDir, "pve-rclone-backup", "source.json")
	if id, err := readIdentity(path); err == nil || !errors.Is(err, os.ErrNotExist) {
		return id, err
	}
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
	// Another node may have created it meanwhile: never replace an
	// existing identity.
	if _, err := os.Stat(path); err == nil {
		_ = os.Remove(tmp)
	} else if err := os.Rename(tmp, path); err != nil {
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
