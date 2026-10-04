// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeFileAtomic replaces path with data via a temporary file in the same
// directory and a rename, which pmxcfs performs atomically cluster-wide.
// pmxcfs fixes permissions itself (priv/ is root-only); on ordinary
// filesystems the file is created with mode 0600.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
