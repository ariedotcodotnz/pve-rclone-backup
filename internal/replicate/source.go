// SPDX-License-Identifier: AGPL-3.0-or-later

package replicate

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/jobs"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

// source is an open local archive. The descriptor is held for the whole
// upload: a local prune that unlinks the archive cannot take the data
// away mid-upload, and a replacement of the file is not followed.
type source struct {
	f       *os.File
	dir     string
	name    string
	archive layout.Archive
	size    int64
	mtimeNs int64
	dev     uint64
	ino     uint64
}

// openSource opens an archive below its dump directory without following
// symlinks in the final component or escaping the directory.
func openSource(dir, name string) (*source, error) {
	a, err := layout.ParseArchiveName(name)
	if err != nil || strings.ContainsRune(name, '/') {
		return nil, fmt.Errorf("replicate: %q is not an archive name", name)
	}
	dfd, err := unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open %s: %w", jobs.ErrSourceLost, dir, err)
	}
	defer func() { _ = unix.Close(dfd) }()
	fd, err := unix.Openat2(dfd, name, &unix.OpenHow{
		Flags:   unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	})
	if errors.Is(err, unix.ENOSYS) {
		fd, err = unix.Openat(dfd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	}
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("%w: %s: %w", jobs.ErrSourceLost, filepath.Join(dir, name), err)
		}
		return nil, fmt.Errorf("replicate: open %s: %w", filepath.Join(dir, name), err)
	}
	f := os.NewFile(uintptr(fd), filepath.Join(dir, name))
	s := &source{f: f, dir: dir, name: name, archive: a}
	if err := s.stat(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return s, nil
}

func (s *source) stat() error {
	fi, err := s.f.Stat()
	if err != nil {
		return fmt.Errorf("replicate: stat %s: %w", s.f.Name(), err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", jobs.ErrSourceLost, s.f.Name())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("replicate: no inode information for %s", s.f.Name())
	}
	s.size, s.mtimeNs, s.dev, s.ino = fi.Size(), fi.ModTime().UnixNano(), uint64(st.Dev), st.Ino //nolint:unconvert // Dev is not uint64 on every platform
	return nil
}

func (s *source) Close() error { return s.f.Close() }

// matches checks the open file against the identity recorded at discovery.
func (s *source) matches(j *store.Job) error {
	switch {
	case j.SourceDev != nil && uint64(*j.SourceDev) != s.dev, j.SourceIno != nil && uint64(*j.SourceIno) != s.ino: //nolint:gosec // device and inode numbers are positive
		return fmt.Errorf("%w: %s was replaced by another file", jobs.ErrSourceLost, s.f.Name())
	case j.SourceSize != nil && *j.SourceSize != s.size, j.SourceMtimeNs != nil && *j.SourceMtimeNs != s.mtimeNs:
		return fmt.Errorf("%w: %s changed since it was discovered", jobs.ErrSourceLost, s.f.Name())
	}
	return nil
}

// unchanged re-checks the open file before commit: an archive modified
// during the upload no longer matches what was uploaded.
func (s *source) unchanged() error {
	size, mtime := s.size, s.mtimeNs
	if err := s.stat(); err != nil {
		return err
	}
	if s.size != size || s.mtimeNs != mtime {
		return fmt.Errorf("%w: %s was modified during the upload", jobs.ErrSourceLost, s.f.Name())
	}
	return nil
}

// basename is the archive name without its extension, which vzdump uses
// for the task log (<basename>.log).
func (s *source) basename() string {
	return strings.TrimSuffix(s.name, "."+s.archive.Ext)
}

// logPath is vzdump's copy of the task log next to the archive.
func (s *source) logPath() string { return filepath.Join(s.dir, s.basename()+".log") }

// sidecars reads the notes and protection markers PVE keeps next to the
// archive.
func (s *source) sidecars() (notes string, protected bool, err error) {
	path := filepath.Join(s.dir, s.name)
	if data, err := readSmall(path+".notes", 1<<20); err == nil {
		notes = string(data)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	if _, err := os.Lstat(path + ".protected"); err == nil {
		protected = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	return notes, protected, nil
}

// readSmall reads a regular file (no symlinks) up to limit bytes.
func readSmall(path string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0) //nolint:gosec // sidecar paths are built from validated archive names
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("replicate: %s is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("replicate: %s is larger than %d bytes", path, limit)
	}
	return data, nil
}
