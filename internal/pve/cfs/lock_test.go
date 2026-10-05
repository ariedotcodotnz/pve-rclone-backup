// SPDX-License-Identifier: AGPL-3.0-or-later

package cfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMutualExclusion(t *testing.T) {
	t.Run("pmxcfs", func(t *testing.T) {
		testMutualExclusion(t, &Locker{Dir: filepath.Join(t.TempDir(), "lock"), AcquireTimeout: 10 * time.Second})
	})
	// Emulated expiry must not take over locks that are held.
	t.Run("stale-emulation", func(t *testing.T) {
		testMutualExclusion(t, &Locker{Dir: filepath.Join(t.TempDir(), "lock"), AcquireTimeout: 10 * time.Second,
			StaleAfter: time.Hour})
	})
}

func testMutualExclusion(t *testing.T, l *Locker) {
	var inside, maxInside, total atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 5 {
				err := l.Do(t.Context(), "pve-rclone-backup-test", func(context.Context) error {
					n := inside.Add(1)
					for {
						m := maxInside.Load()
						if n <= m || maxInside.CompareAndSwap(m, n) {
							break
						}
					}
					time.Sleep(time.Millisecond)
					inside.Add(-1)
					total.Add(1)
					return nil
				})
				if err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	if maxInside.Load() != 1 || total.Load() != 40 {
		t.Fatalf("max concurrent holders %d, completed %d", maxInside.Load(), total.Load())
	}
	if entries, _ := os.ReadDir(l.Dir); len(entries) != 0 {
		t.Fatalf("lock not released: %v", entries)
	}
}

func TestAcquireTimeoutAndStaleLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lock")
	if err := os.MkdirAll(filepath.Join(dir, "held"), 0o700); err != nil {
		t.Fatal(err)
	}
	l := &Locker{Dir: dir, AcquireTimeout: 300 * time.Millisecond}
	start := time.Now()
	err := l.Do(t.Context(), "held", func(context.Context) error { return nil })
	if !errors.Is(err, ErrTimeout) || time.Since(start) > 2*time.Second {
		t.Fatalf("held lock: %v after %v", err, time.Since(start))
	}

	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(filepath.Join(dir, "held"), old, old)
	l.StaleAfter = time.Minute
	ran := false
	if err := l.Do(t.Context(), "held", func(context.Context) error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("stale lock not taken over: %v", err)
	}
}

func TestHoldTimeoutCancelsCriticalSection(t *testing.T) {
	l := &Locker{Dir: filepath.Join(t.TempDir(), "lock"), HoldTimeout: 50 * time.Millisecond}
	err := l.Do(t.Context(), "slow", func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "held longer than") {
		t.Fatalf("overlong critical section not reported: %v", err)
	}
	errFn := errors.New("boom")
	if err := l.Do(t.Context(), "slow", func(context.Context) error { return errFn }); !errors.Is(err, errFn) {
		t.Fatalf("callback error not returned: %v", err)
	}
}

func TestReadOnlyClusterFilesystem(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	ro := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	l := &Locker{Dir: filepath.Join(ro, "lock")}
	if err := l.Do(t.Context(), "x", func(context.Context) error { return nil }); !errors.Is(err, ErrNoQuorum) {
		t.Fatalf("read-only cluster filesystem: %v", err)
	}
	if err := l.Do(t.Context(), "../escape", func(context.Context) error { return nil }); err == nil {
		t.Fatal("invalid lock id accepted")
	}
}
