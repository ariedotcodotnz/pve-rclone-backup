// SPDX-License-Identifier: AGPL-3.0-or-later

package transport

import (
	"fmt"
	"path"
	"strings"
	"unicode/utf16"

	"github.com/rclone/rclone/backend/crypt"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
)

// Profile describes what a storage backend supports. rclone's Features()
// do not cover object size or path limits, so they are kept here per
// backend, and only backends with a profile are offered.
type Profile struct {
	Backend string
	// MaxObjectSize is the largest object the provider accepts (0: no limit).
	MaxObjectSize int64
	// MaxPathLength bounds the full remote path, measured in UTF-16 code
	// units when PathCountsUTF16 is set and in bytes otherwise (0: none).
	MaxPathLength   int
	PathCountsUTF16 bool
	// MaxNameLength bounds a single path component in bytes (0: none).
	MaxNameLength int
	// FilenameEncoding is the crypt filename encoding to use.
	FilenameEncoding string
	// RecycleBin means deletions are kept by the provider and may still
	// count against the quota.
	RecycleBin bool
	// StrictTokenRotation means refresh tokens are single-use, so only one
	// node may refresh tokens of a remote.
	StrictTokenRotation bool
	// Tested reports whether the backend is covered by the test suite.
	Tested bool
}

var profiles = map[string]Profile{
	"onedrive": {
		Backend: "onedrive", MaxObjectSize: 250 << 30, MaxPathLength: 400, PathCountsUTF16: true,
		FilenameEncoding: "base32768", RecycleBin: true, Tested: true,
	},
	"local": {
		Backend: "local", MaxPathLength: 4096, MaxNameLength: 255, FilenameEncoding: "base32", Tested: true,
	},
	"memory": {
		Backend: "memory", FilenameEncoding: "base32", Tested: true,
	},
}

// ProfileFor returns the profile of an rclone backend type.
func ProfileFor(backend string) (Profile, error) {
	p, ok := profiles[backend]
	if !ok {
		return Profile{}, fmt.Errorf("transport: backend %q is not supported by this release", backend)
	}
	return p, nil
}

// SupportedBackends lists backends with a profile.
func SupportedBackends() []string { return []string{"onedrive"} }

// CheckSegmentSize validates a segment size against the profile.
func (p Profile) CheckSegmentSize(size int64, encrypted bool) error {
	stored := size
	if encrypted {
		stored = CryptStoredSize(size)
	}
	if p.MaxObjectSize > 0 && stored > p.MaxObjectSize {
		return fmt.Errorf("transport: segment size %d exceeds the %s object limit of %d bytes", size, p.Backend, p.MaxObjectSize)
	}
	return nil
}

// EncryptedPath returns plain as rclone crypt would store it below a crypt
// root with standard name encryption. Encrypted name lengths depend only on
// the plaintext and the encoding, so a fixed dummy key gives exact lengths.
func EncryptedPath(plain, encoding string, encryptDirs bool) (string, error) {
	c, err := crypt.NewCipher(configmap.Simple{
		"password":                  obscure.MustObscure("length-probe"),
		"filename_encryption":       "standard",
		"filename_encoding":         encoding,
		"directory_name_encryption": fmt.Sprint(encryptDirs),
		"suffix":                    ".bin",
	})
	if err != nil {
		return "", fmt.Errorf("transport: crypt cipher: %w", err)
	}
	return c.EncryptFileName(plain), nil
}

// CheckPath verifies that a path below base fits the provider's limits.
// rel is relative to the crypt root base (when encoding is not empty) or
// to base itself (unencrypted repositories).
func (p Profile) CheckPath(base, rel, encoding string) error {
	stored := rel
	if encoding != "" {
		enc, err := EncryptedPath(rel, encoding, true)
		if err != nil {
			return err
		}
		stored = enc
	}
	full := path.Join(base, stored)
	n := len(full)
	if p.PathCountsUTF16 {
		n = len(utf16.Encode([]rune(full)))
	}
	if p.MaxPathLength > 0 && n > p.MaxPathLength {
		return fmt.Errorf("transport: path of %d characters exceeds the %s limit of %d (shorten the repository path or source name)",
			n, p.Backend, p.MaxPathLength)
	}
	if p.MaxNameLength > 0 {
		for part := range strings.SplitSeq(full, "/") {
			if len(part) > p.MaxNameLength {
				return fmt.Errorf("transport: path component of %d bytes exceeds the %s limit of %d", len(part), p.Backend, p.MaxNameLength)
			}
		}
	}
	return nil
}
