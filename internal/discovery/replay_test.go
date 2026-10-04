// SPDX-License-Identifier: AGPL-3.0-or-later

package discovery

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
)

// recordedEvent is a line of a fixture recorded by test/e2e/evlog on a
// real Proxmox VE node (test/e2e TestVzdumpEvents).
type recordedEvent struct {
	Op     string `json:"op"`
	Name   string `json:"name"`
	Cookie uint32 `json:"cookie"`
	Size   *int64 `json:"size"`
}

func loadRecording(t *testing.T, path string) []recordedEvent {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []recordedEvent
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e recordedEvent
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		out = append(out, e)
	}
	return out
}

// replay performs a recording's file operations in dir, so that inotify
// reports the same kinds of events in the same order. File contents are
// shortened: discovery only looks at names and metadata.
func replay(t *testing.T, dir, outside string, events []recordedEvent) {
	t.Helper()
	moved := map[uint32]string{} // cookie -> name moved out of dir
	grow := func(name string, size *int64) {
		n := int64(1)
		if size != nil {
			n = min(*size, 64<<10)
		}
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		st, _ := f.Stat()
		if st.Size() < n {
			_, _ = f.Write(make([]byte, n-st.Size()))
		}
		_ = f.Close()
	}
	for _, e := range events {
		p := filepath.Join(dir, e.Name)
		switch e.Op {
		case "create":
			write(t, p, "")
		case "modify":
			grow(e.Name, e.Size)
		case "close_write":
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
		case "attrib":
			now := time.Now()
			_ = os.Chtimes(p, now, now)
		case "moved_from":
			moved[e.Cookie] = e.Name
		case "moved_to":
			from, ok := moved[e.Cookie]
			if !ok {
				// Moved in from elsewhere.
				from = filepath.Join("..", filepath.Base(outside), e.Name)
				write(t, filepath.Join(outside, e.Name), "x")
			}
			delete(moved, e.Cookie)
			if err := os.Rename(filepath.Join(dir, from), p); err != nil {
				t.Fatal(err)
			}
		case "delete":
			_ = os.Remove(p)
		}
	}
	// Moved out of the directory.
	for _, name := range moved {
		_ = os.Rename(filepath.Join(dir, name), filepath.Join(outside, name))
	}
}

// TestReplayVzdumpEvents replays recorded vzdump runs against the inotify
// discovery: exactly the archives renamed into place get one job each, and
// nothing is queued for temporary files, logs, sidecars or failed and
// aborted backups.
func TestReplayVzdumpEvents(t *testing.T) {
	files, err := filepath.Glob("testdata/vzdump/*.jsonl")
	if err != nil || len(files) == 0 {
		t.Fatalf("no recordings in testdata/vzdump: %v", err)
	}
	for _, file := range files {
		t.Run(strings.TrimSuffix(filepath.Base(file), ".jsonl"), func(t *testing.T) {
			events := loadRecording(t, file)
			f := newFixture(t, "", "\trclone-backfill all\n")
			outside := filepath.Join(filepath.Dir(f.dump), "outside")
			if err := os.Mkdir(outside, 0o700); err != nil {
				t.Fatal(err)
			}

			// Archives the recording removes without creating (local prune)
			// existed before; so does a sentinel that shows when the
			// initial scan, and with it the watch, is done.
			var want []string
			created := map[string]bool{}
			for _, e := range events {
				if e.Op == "create" || e.Op == "moved_to" {
					created[e.Name] = true
				}
				if e.Op == "delete" && !created[e.Name] {
					write(t, filepath.Join(f.dump, e.Name), "old")
					created[e.Name] = true
					if archiveName(e.Name) {
						want = append(want, e.Name)
					}
				}
				if e.Op == "moved_to" && archiveName(e.Name) {
					want = append(want, e.Name)
				}
			}
			const first, last = "vzdump-qemu-998-2026_01_01-00_00_00.vma.zst", "vzdump-qemu-999-2026_01_01-00_00_00.vma.zst"
			write(t, filepath.Join(f.dump, first), "x")
			want = append(want, first, last)

			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			d := f.discoverer(true)
			go func() { d.Run(ctx); close(done) }()
			defer func() { cancel(); <-done }()
			f.waitJob(t, first)

			replay(t, f.dump, outside, events)
			// Events are handled in order: once the last one is, all are.
			write(t, filepath.Join(outside, last), "x")
			if err := os.Rename(filepath.Join(outside, last), filepath.Join(f.dump, last)); err != nil {
				t.Fatal(err)
			}
			f.waitJob(t, last)

			var got []string
			for k := range f.states() {
				got = append(got, strings.TrimPrefix(k, "t1/"))
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("jobs for %v, want %v", got, want)
			}
		})
	}
}

func archiveName(name string) bool {
	_, err := layout.ParseArchiveName(name)
	return err == nil
}

// waitJob waits until discovery has recorded a job for an archive.
func (f *fixture) waitJob(t *testing.T, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := f.states()["t1/"+name]; ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no job for %s; jobs: %v", name, f.states())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
