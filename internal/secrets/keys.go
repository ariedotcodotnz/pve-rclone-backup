// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"
)

// Generation is one set of rclone crypt keys of a repository. Rotating
// keys adds a generation; older generations stay readable.
type Generation struct {
	ID                      int       `json:"id"`
	Password                string    `json:"password"`
	Password2               string    `json:"password2"`
	FilenameEncryption      string    `json:"filename_encryption"`
	DirectoryNameEncryption bool      `json:"directory_name_encryption"`
	FilenameEncoding        string    `json:"filename_encoding"`
	Suffix                  string    `json:"suffix"`
	State                   string    `json:"state"` // active | retired
	CreatedAt               time.Time `json:"created_at"`
}

// RepoKeys holds every key generation of one repository.
type RepoKeys struct {
	Format      string       `json:"format"`
	Version     int          `json:"version"`
	RepoUUID    string       `json:"repo_uuid"`
	Generations []Generation `json:"generations"`
}

const keysFormat = "pve-rclone-backup.keys"

var (
	// ErrNotFound means no keys exist for a repository.
	ErrNotFound = errors.New("secrets: no keys for repository")
	// ErrWouldDropKeys refuses a save that would lose a key generation:
	// losing crypt keys makes the repository unreadable.
	ErrWouldDropKeys = errors.New("secrets: refusing to remove an existing key generation")
	uuidRe           = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	encodings        = []string{"base32", "base64", "base32768"}
)

// Active returns the generation new backups are written with.
func (k *RepoKeys) Active() (*Generation, error) {
	for i := len(k.Generations) - 1; i >= 0; i-- {
		if k.Generations[i].State == "active" {
			return &k.Generations[i], nil
		}
	}
	return nil, fmt.Errorf("secrets: repository %s has no active key generation", k.RepoUUID)
}

// Generation returns the generation with id.
func (k *RepoKeys) Generation(id int) (*Generation, error) {
	for i := range k.Generations {
		if k.Generations[i].ID == id {
			return &k.Generations[i], nil
		}
	}
	return nil, fmt.Errorf("secrets: repository %s has no key generation %d", k.RepoUUID, id)
}

func (k *RepoKeys) validate() error {
	if k.Format != keysFormat || k.Version != 1 {
		return fmt.Errorf("secrets: unsupported keys file format %q version %d", k.Format, k.Version)
	}
	if !uuidRe.MatchString(k.RepoUUID) {
		return fmt.Errorf("secrets: invalid repository UUID %q", k.RepoUUID)
	}
	seen := map[int]bool{}
	for _, g := range k.Generations {
		switch {
		case g.ID < 1 || seen[g.ID]:
			return fmt.Errorf("secrets: invalid or duplicate key generation %d", g.ID)
		case g.Password == "" || g.Password2 == "":
			return fmt.Errorf("secrets: key generation %d lacks passwords", g.ID)
		case !slices.Contains(encodings, g.FilenameEncoding):
			return fmt.Errorf("secrets: key generation %d has unknown filename encoding %q", g.ID, g.FilenameEncoding)
		case g.FilenameEncryption != "standard" && g.FilenameEncryption != "obfuscate" && g.FilenameEncryption != "off":
			return fmt.Errorf("secrets: key generation %d has unknown filename encryption %q", g.ID, g.FilenameEncryption)
		case g.State != "active" && g.State != "retired":
			return fmt.Errorf("secrets: key generation %d has unknown state %q", g.ID, g.State)
		}
		seen[g.ID] = true
	}
	return nil
}

func randomSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// NewGeneration creates a generation with random 256-bit password and salt.
func NewGeneration(id int, filenameEncoding string, now time.Time) (Generation, error) {
	if !slices.Contains(encodings, filenameEncoding) {
		return Generation{}, fmt.Errorf("secrets: unknown filename encoding %q", filenameEncoding)
	}
	p1, err := randomSecret()
	if err != nil {
		return Generation{}, err
	}
	p2, err := randomSecret()
	if err != nil {
		return Generation{}, err
	}
	return Generation{
		ID: id, Password: p1, Password2: p2,
		FilenameEncryption: "standard", DirectoryNameEncryption: true,
		FilenameEncoding: filenameEncoding, Suffix: ".bin",
		State: "active", CreatedAt: now.UTC(),
	}, nil
}

// NewRepoKeys returns keys for a new repository with one active generation.
func NewRepoKeys(repoUUID, filenameEncoding string, now time.Time) (*RepoKeys, error) {
	g, err := NewGeneration(1, filenameEncoding, now)
	if err != nil {
		return nil, err
	}
	k := &RepoKeys{Format: keysFormat, Version: 1, RepoUUID: repoUUID, Generations: []Generation{g}}
	return k, k.validate()
}

// KeyStore keeps RepoKeys in <dir>/keys/<repo-uuid>.json.
type KeyStore struct {
	dir  string
	lock Locker
}

// NewKeyStore returns a key store rooted at a secrets directory.
func NewKeyStore(dir string, lock Locker) *KeyStore { return &KeyStore{dir: dir, lock: lock} }

func (s *KeyStore) path(repoUUID string) (string, error) {
	if !uuidRe.MatchString(repoUUID) {
		return "", fmt.Errorf("secrets: invalid repository UUID %q", repoUUID)
	}
	return filepath.Join(s.dir, "keys", repoUUID+".json"), nil
}

// Load returns the keys of a repository or ErrNotFound.
func (s *KeyStore) Load(repoUUID string) (*RepoKeys, error) {
	path, err := s.path(repoUUID)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path) //nolint:gosec // path is built from a validated UUID
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("secrets: read keys: %w", err)
	}
	var k RepoKeys
	if err := json.Unmarshal(raw, &k); err != nil {
		return nil, fmt.Errorf("secrets: parse %s: %w", path, err)
	}
	if err := k.validate(); err != nil {
		return nil, err
	}
	if k.RepoUUID != repoUUID {
		return nil, fmt.Errorf("secrets: %s holds keys of repository %s", path, k.RepoUUID)
	}
	return &k, nil
}

// Save writes the keys of a repository. It refuses to remove a generation
// or change the secrets of an existing one.
func (s *KeyStore) Save(ctx context.Context, k *RepoKeys) error {
	if err := k.validate(); err != nil {
		return err
	}
	path, err := s.path(k.RepoUUID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return err
	}
	return s.lock.Do(ctx, "pve-rclone-backup-keys", func(context.Context) error {
		old, err := s.Load(k.RepoUUID)
		switch {
		case errors.Is(err, ErrNotFound):
		case err != nil:
			return err
		default:
			for _, og := range old.Generations {
				ng, err := k.Generation(og.ID)
				if err != nil {
					return fmt.Errorf("%w (generation %d)", ErrWouldDropKeys, og.ID)
				}
				if ng.Password != og.Password || ng.Password2 != og.Password2 ||
					ng.FilenameEncoding != og.FilenameEncoding || ng.FilenameEncryption != og.FilenameEncryption ||
					ng.DirectoryNameEncryption != og.DirectoryNameEncryption || ng.Suffix != og.Suffix {
					return fmt.Errorf("%w (generation %d would change)", ErrWouldDropKeys, og.ID)
				}
			}
		}
		return writeFileAtomic(path, append(data, '\n'))
	})
}
