// SPDX-License-Identifier: AGPL-3.0-or-later

// Command evlog records the inotify events of one directory as JSON lines,
// to capture how vzdump writes, renames and removes backup files (the
// fixtures in internal/discovery/testdata). Bursts of IN_MODIFY for one
// file are folded into a single line with a count. It runs until SIGINT or
// SIGTERM.
//
//	evlog /var/lib/vz/dump >events.jsonl
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Event is one recorded line.
type Event struct {
	// MS is the time since the recorder started, in milliseconds.
	MS     int64  `json:"ms"`
	Op     string `json:"op"`
	Name   string `json:"name"`
	Cookie uint32 `json:"cookie,omitempty"`
	// Count is the number of folded IN_MODIFY events.
	Count int `json:"count,omitempty"`
	// Size is the file size when the event was read, if the file exists.
	Size *int64 `json:"size,omitempty"`
}

var ops = []struct {
	mask uint32
	name string
}{
	{unix.IN_CREATE, "create"},
	{unix.IN_MODIFY, "modify"},
	{unix.IN_ATTRIB, "attrib"},
	{unix.IN_CLOSE_WRITE, "close_write"},
	{unix.IN_MOVED_FROM, "moved_from"},
	{unix.IN_MOVED_TO, "moved_to"},
	{unix.IN_DELETE, "delete"},
	{unix.IN_DELETE_SELF, "delete_self"},
	{unix.IN_MOVE_SELF, "move_self"},
	{unix.IN_Q_OVERFLOW, "overflow"},
}

const mask = unix.IN_CREATE | unix.IN_MODIFY | unix.IN_ATTRIB | unix.IN_CLOSE_WRITE | unix.IN_MOVED_FROM |
	unix.IN_MOVED_TO | unix.IN_DELETE | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: evlog <directory>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "evlog:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	// Non-blocking, so the runtime poller serves reads and Close ends them.
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return err
	}
	if _, err := unix.InotifyAddWatch(fd, dir, mask); err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), "inotify")
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		_ = f.Close()
	}()
	fmt.Fprintln(os.Stderr, "evlog: watching", dir)

	start := time.Now()
	enc := json.NewEncoder(os.Stdout)
	var pending *Event // a run of IN_MODIFY not yet written
	flush := func() error {
		if pending == nil {
			return nil
		}
		e := pending
		pending = nil
		return enc.Encode(e)
	}
	buf := make([]byte, 64<<10)
	for {
		n, err := f.Read(buf)
		if errors.Is(err, os.ErrClosed) {
			return flush()
		}
		if err != nil {
			return err
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			m := binary.NativeEndian.Uint32(buf[off+4:])
			cookie := binary.NativeEndian.Uint32(buf[off+8:])
			l := int(binary.NativeEndian.Uint32(buf[off+12:]))
			name := string(bytes.TrimRight(buf[off+unix.SizeofInotifyEvent:off+unix.SizeofInotifyEvent+l], "\x00"))
			off += unix.SizeofInotifyEvent + l
			var size *int64
			if name != "" {
				if st, err := os.Lstat(filepath.Join(dir, name)); err == nil { //nolint:gosec // an entry of the watched directory
					s := st.Size()
					size = &s
				}
			}
			for _, o := range ops {
				if m&o.mask == 0 {
					continue
				}
				if o.mask == unix.IN_MODIFY && pending != nil && pending.Name == name {
					pending.Count++
					pending.Size = size
					continue
				}
				if err := flush(); err != nil {
					return err
				}
				e := &Event{MS: time.Since(start).Milliseconds(), Op: o.name, Name: name, Cookie: cookie, Size: size}
				if o.mask == unix.IN_MODIFY {
					e.Count = 1
					pending = e
					continue
				}
				if err := enc.Encode(e); err != nil {
					return err
				}
			}
		}
	}
}
