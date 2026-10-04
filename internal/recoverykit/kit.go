// SPDX-License-Identifier: AGPL-3.0-or-later

// Package recoverykit encodes everything needed to read the offsite
// repositories after losing the PVE host: repository locations, rclone
// crypt keys and, optionally, the transport remote's OAuth token.
//
// The kit is armored text. Its header is human readable and never contains
// secrets; the payload is either passphrase-encrypted (scrypt and
// XChaCha20-Poly1305) or plain base64. A short checksum lets the user
// confirm that the copy they stored is complete.
package recoverykit

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/scrypt"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/secrets"
)

const (
	format     = "pve-rclone-backup.recovery-kit"
	beginArmor = "-----BEGIN PVE-RCLONE-BACKUP RECOVERY KIT-----"
	endArmor   = "-----END PVE-RCLONE-BACKUP RECOVERY KIT-----"

	blobVersion   = 1
	modePlain     = 0
	modeEncrypted = 1
	saltLen       = 16
)

// scrypt cost; tests may lower it.
var scryptN = 1 << 17

var (
	// ErrPassphraseRequired means the kit is encrypted and no passphrase was given.
	ErrPassphraseRequired = errors.New("recoverykit: kit is encrypted; a passphrase is required")
	// ErrBadPassphrase means decryption failed: wrong passphrase or damaged kit.
	ErrBadPassphrase = errors.New("recoverykit: wrong passphrase or damaged kit")
	// ErrDamaged means the kit text is incomplete or altered.
	ErrDamaged = errors.New("recoverykit: kit is damaged or incomplete")
)

// Kit is the decoded content.
type Kit struct {
	Format    string    `json:"format"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	CreatedOn string    `json:"created_on"`
	Repos     []Repo    `json:"repos"`
}

// Repo describes one offsite repository.
type Repo struct {
	StorageID    string            `json:"storage_id"`
	RepoUUID     string            `json:"repo_uuid"`
	Remote       string            `json:"remote"`
	Path         string            `json:"path"`
	Source       string            `json:"source"`
	SourceUUID   string            `json:"source_uuid,omitempty"`
	Encryption   string            `json:"encryption"`
	RemoteConfig map[string]string `json:"remote_config,omitempty"`
	Keys         *secrets.RepoKeys `json:"keys,omitempty"`
}

// New returns an empty kit.
func New(node string, now time.Time) *Kit {
	return &Kit{Format: format, Version: 1, CreatedAt: now.UTC(), CreatedOn: node}
}

func aad() []byte { return []byte(format + "/1") }

// Encode renders the kit. An empty passphrase stores the payload
// unencrypted (the header then says so prominently).
func Encode(k *Kit, passphrase string) (text, checksum string, err error) {
	payload, err := json.Marshal(k)
	if err != nil {
		return "", "", err
	}
	var blob []byte
	if passphrase == "" {
		blob = append([]byte{blobVersion, modePlain}, payload...)
	} else {
		salt := make([]byte, saltLen)
		nonce := make([]byte, chacha20poly1305.NonceSizeX)
		if _, err := rand.Read(salt); err != nil {
			return "", "", err
		}
		if _, err := rand.Read(nonce); err != nil {
			return "", "", err
		}
		key, err := scrypt.Key([]byte(passphrase), salt, scryptN, 8, 1, chacha20poly1305.KeySize)
		if err != nil {
			return "", "", err
		}
		aead, err := chacha20poly1305.NewX(key)
		if err != nil {
			return "", "", err
		}
		blob = append([]byte{blobVersion, modeEncrypted}, salt...)
		blob = append(blob, nonce...)
		blob = aead.Seal(blob, nonce, payload, aad())
	}
	checksum = Checksum(blob)

	var b strings.Builder
	b.WriteString(header(k, passphrase != "", checksum))
	b.WriteString(beginArmor + "\n")
	enc := base64.StdEncoding.EncodeToString(blob)
	for len(enc) > 64 {
		b.WriteString(enc[:64] + "\n")
		enc = enc[64:]
	}
	b.WriteString(enc + "\n" + endArmor + "\n")
	return b.String(), checksum, nil
}

// Checksum returns the short confirmation checksum of a payload blob.
func Checksum(blob []byte) string {
	sum := sha256.Sum256(blob)
	h := hex.EncodeToString(sum[:8])
	return h[0:4] + "-" + h[4:8] + "-" + h[8:12] + "-" + h[12:16]
}

func extract(text string) ([]byte, error) {
	start := strings.Index(text, beginArmor)
	end := strings.Index(text, endArmor)
	if start < 0 || end < start {
		return nil, fmt.Errorf("%w: armor markers not found", ErrDamaged)
	}
	body := strings.Join(strings.Fields(text[start+len(beginArmor):end]), "")
	blob, err := base64.StdEncoding.DecodeString(body)
	if err != nil || len(blob) < 2 || blob[0] != blobVersion {
		return nil, fmt.Errorf("%w: invalid payload", ErrDamaged)
	}
	return blob, nil
}

// TextChecksum returns the checksum of a kit's payload, to compare with
// the checksum shown when it was created.
func TextChecksum(text string) (string, error) {
	blob, err := extract(text)
	if err != nil {
		return "", err
	}
	return Checksum(blob), nil
}

// Decode parses a kit, decrypting it with passphrase if needed.
func Decode(text, passphrase string) (*Kit, error) {
	blob, err := extract(text)
	if err != nil {
		return nil, err
	}
	if want := headerChecksum(text); want != "" && want != Checksum(blob) {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrDamaged)
	}
	var payload []byte
	switch blob[1] {
	case modePlain:
		payload = blob[2:]
	case modeEncrypted:
		if passphrase == "" {
			return nil, ErrPassphraseRequired
		}
		rest := blob[2:]
		if len(rest) < saltLen+chacha20poly1305.NonceSizeX+chacha20poly1305.Overhead {
			return nil, fmt.Errorf("%w: truncated payload", ErrDamaged)
		}
		salt, nonce, ct := rest[:saltLen], rest[saltLen:saltLen+chacha20poly1305.NonceSizeX], rest[saltLen+chacha20poly1305.NonceSizeX:]
		key, err := scrypt.Key([]byte(passphrase), salt, scryptN, 8, 1, chacha20poly1305.KeySize)
		if err != nil {
			return nil, err
		}
		aead, err := chacha20poly1305.NewX(key)
		if err != nil {
			return nil, err
		}
		if payload, err = aead.Open(nil, nonce, ct, aad()); err != nil {
			return nil, ErrBadPassphrase
		}
	default:
		return nil, fmt.Errorf("%w: unknown payload mode", ErrDamaged)
	}
	var k Kit
	dec := json.NewDecoder(bytes.NewReader(payload))
	if err := dec.Decode(&k); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDamaged, err)
	}
	if k.Format != format || k.Version != 1 {
		return nil, fmt.Errorf("recoverykit: unsupported kit format %q version %d", k.Format, k.Version)
	}
	return &k, nil
}

func headerChecksum(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if v, ok := strings.CutPrefix(line, "Checksum: "); ok {
			return strings.TrimSpace(v)
		}
		if strings.HasPrefix(line, beginArmor) {
			break
		}
	}
	return ""
}

func header(k *Kit, encrypted bool, checksum string) string {
	var b strings.Builder
	b.WriteString("pve-rclone-backup recovery kit\n")
	b.WriteString("==============================\n\n")
	fmt.Fprintf(&b, "Created: %s on node %s\n", k.CreatedAt.Format(time.RFC3339), k.CreatedOn)
	if encrypted {
		b.WriteString("Encrypted: yes - the passphrase chosen at export is required to use this kit\n")
	} else {
		b.WriteString("Encrypted: NO - this file contains plaintext credentials; store it like a password\n")
	}
	fmt.Fprintf(&b, "Checksum: %s\n\n", checksum)
	b.WriteString("Without the keys in this kit, encrypted offsite backups cannot be read by anyone.\n\n")
	b.WriteString("Repositories:\n")
	for _, r := range k.Repos {
		fmt.Fprintf(&b, "  - storage %s: remote %s, path %s, source %s, encryption %s\n",
			r.StorageID, r.Remote, r.Path, r.Source, r.Encryption)
		if r.Keys != nil {
			for _, g := range r.Keys.Generations {
				fmt.Fprintf(&b, "      key generation %d (%s): crypt root %s/g%d, filename_encryption=%s, directory_name_encryption=%t, filename_encoding=%s, suffix=%s\n",
					g.ID, g.State, r.Path, g.ID, g.FilenameEncryption, g.DirectoryNameEncryption, g.FilenameEncoding, g.Suffix)
			}
		}
		if r.RemoteConfig != nil && r.RemoteConfig["token"] != "" {
			b.WriteString("      includes the remote's OAuth token\n")
		}
	}
	b.WriteString(`
Restoring on a new Proxmox VE host:
  1. apt install pve-rclone-backup
  2. pve-rclone-backup recover --kit <this file>
  3. pve-rclone-backup restore <volume> --vmid <id> --target-storage <storage>

Restoring without pve-rclone-backup (stock rclone 1.62 or newer):
  1. Decode the payload below: pve-rclone-backup recovery-kit show <this file>,
     or base64-decode it (unencrypted kits) to get JSON with the crypt keys.
  2. rclone config: create the transport remote (e.g. OneDrive), then a crypt
     remote with remote=<transport>:<path>/g<generation>, the two passwords,
     and the filename settings listed above.
  3. rclone lsf -R <crypt>:v1/<source>/
  4. rclone cat <crypt>:v1/<source>/<qemu|lxc>/<vmid>/<timestamp>/part.000000 ... (all
     parts in order) > vzdump-<qemu|lxc>-<vmid>-<timestamp>.<vma|tar>.zst
  5. qmrestore <archive> <vmid>   or   pct restore <vmid> <archive>

`)
	return b.String()
}
