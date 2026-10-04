// SPDX-License-Identifier: AGPL-3.0-or-later

package discovery

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// watcher reports archives that appear in dump directories. vzdump writes
// to a temporary name and renames the archive only on success, so
// IN_MOVED_TO announces finished archives; IN_CLOSE_WRITE covers archives
// copied in place.
type watcher struct {
	f        *os.File
	fd       int // f's descriptor; f.Fd() would switch it to blocking mode
	paths    chan<- string
	overflow chan<- struct{}

	mu   sync.Mutex
	dirs map[int32]string
	wds  map[string]int32
}

const watchMask = unix.IN_MOVED_TO | unix.IN_CLOSE_WRITE | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF | unix.IN_ONLYDIR

func newWatcher(paths chan<- string, overflow chan<- struct{}) (*watcher, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, err
	}
	// A non-blocking descriptor is served by the runtime poller, so Close
	// interrupts a pending Read.
	return &watcher{f: os.NewFile(uintptr(fd), "inotify"), fd: fd, paths: paths, overflow: overflow,
		dirs: map[int32]string{}, wds: map[string]int32{}}, nil
}

// sync watches exactly dirs; it returns the directories it could not
// watch.
func (w *watcher) sync(dirs []string) map[string]error {
	w.mu.Lock()
	defer w.mu.Unlock()
	want := map[string]bool{}
	failed := map[string]error{}
	for _, d := range dirs {
		want[d] = true
		if _, ok := w.wds[d]; ok {
			continue
		}
		wd, err := unix.InotifyAddWatch(w.fd, d, watchMask)
		if err != nil {
			failed[d] = err
			continue
		}
		w.wds[d], w.dirs[int32(wd)] = int32(wd), d //nolint:gosec // watch descriptors are small positive ints
	}
	for d, wd := range w.wds {
		if !want[d] {
			_, _ = unix.InotifyRmWatch(w.fd, uint32(wd)) //nolint:gosec // wd is positive
			delete(w.wds, d)
			delete(w.dirs, wd)
		}
	}
	return failed
}

func (w *watcher) close() error { return w.f.Close() }

// run reads events until the watcher is closed.
func (w *watcher) run() {
	buf := make([]byte, 64<<10)
	for {
		n, err := w.f.Read(buf)
		if err != nil {
			if errors.Is(err, os.ErrClosed) {
				return
			}
			continue
		}
		w.parse(buf[:n])
	}
}

func (w *watcher) parse(buf []byte) {
	const header = unix.SizeofInotifyEvent
	for off := 0; off+header <= len(buf); {
		wd := int32(binary.NativeEndian.Uint32(buf[off:])) //nolint:gosec // the kernel writes an int32 here
		mask := binary.NativeEndian.Uint32(buf[off+4:])
		nameLen := int(binary.NativeEndian.Uint32(buf[off+12:]))
		end := off + header + nameLen
		if end > len(buf) {
			return
		}
		name := string(bytes.TrimRight(buf[off+header:end], "\x00"))
		off = end

		switch {
		case mask&unix.IN_Q_OVERFLOW != 0:
			select {
			case w.overflow <- struct{}{}:
			default:
			}
		case mask&unix.IN_IGNORED != 0, mask&(unix.IN_DELETE_SELF|unix.IN_MOVE_SELF) != 0:
			// The directory is gone; the next sync watches it again.
			w.mu.Lock()
			if d, ok := w.dirs[wd]; ok {
				delete(w.dirs, wd)
				delete(w.wds, d)
			}
			w.mu.Unlock()
		case mask&(unix.IN_MOVED_TO|unix.IN_CLOSE_WRITE) != 0 && name != "":
			w.mu.Lock()
			dir, ok := w.dirs[wd]
			w.mu.Unlock()
			if ok {
				select {
				case w.paths <- filepath.Join(dir, name):
				default:
					// Too many events: let a scan catch up.
					select {
					case w.overflow <- struct{}{}:
					default:
					}
				}
			}
		}
	}
}
