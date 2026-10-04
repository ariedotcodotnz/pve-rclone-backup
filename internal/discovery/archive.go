// SPDX-License-Identifier: AGPL-3.0-or-later

package discovery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
)

// OriginXattr marks archives fetched from an offsite storage, so they are
// never uploaded again.
const OriginXattr = "user.pve-rclone-backup.origin"

// Archive is a finished vzdump archive in a dump directory.
type Archive struct {
	layout.Archive
	Path       string
	Name       string
	Size       int64
	MtimeNs    int64
	Dev, Ino   uint64
	BackupTime int64 // vzdump timestamp in the node's local time zone, like PVE
}

// tsLayout is the vzdump file name timestamp.
const tsLayout = "2006_01_02-15_04_05"

// statArchive inspects one directory entry. It returns nil for anything
// that is not a finished archive: other names, vzdump's temporary files,
// symlinks and non-regular files.
func statArchive(dir, name string) (*Archive, error) {
	parsed, err := layout.ParseArchiveName(name)
	if err != nil || parsed.Collision != 0 {
		return nil, nil
	}
	path := filepath.Join(dir, name)
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, nil
	}
	ts, err := time.ParseInLocation(tsLayout, parsed.TSLabel, time.Local)
	if err != nil {
		return nil, nil
	}
	a := &Archive{Archive: parsed, Path: path, Name: name, Size: fi.Size(), MtimeNs: fi.ModTime().UnixNano(), BackupTime: ts.Unix()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		a.Dev, a.Ino = uint64(st.Dev), st.Ino //nolint:unconvert // Dev is not uint64 on every platform
	}
	return a, nil
}

// listArchives returns the finished archives in a dump directory, oldest
// backup first.
func listArchives(dir string) ([]*Archive, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []*Archive
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "vzdump-") || e.IsDir() {
			continue
		}
		a, err := statArchive(dir, e.Name())
		if err != nil {
			return nil, fmt.Errorf("discovery: %w", err)
		}
		if a != nil {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b *Archive) int {
		if a.BackupTime != b.BackupTime {
			return int(a.BackupTime - b.BackupTime)
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// hasOrigin reports whether an archive carries the fetched-from-offsite
// marker.
func hasOrigin(path string) bool {
	_, err := unix.Lgetxattr(path, OriginXattr, nil)
	return err == nil
}
