// SPDX-License-Identifier: AGPL-3.0-or-later

// Package cfs implements Proxmox VE cluster filesystem (pmxcfs) locks.
//
// A lock is a directory /etc/pve/priv/lock/<id> created with mkdir, exactly
// like PVE::Cluster::cfs_lock: creation is atomic cluster-wide, pmxcfs
// expires locks after 120 seconds, and an utime(0, 0) on a held lock asks
// pmxcfs to release it if it has expired. Locks are for short critical
// sections only and require quorum (/etc/pve is read-only without it).
package cfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

// DefaultLockDir is pmxcfs' lock directory.
const DefaultLockDir = "/etc/pve/priv/lock"

var (
	// ErrNoQuorum means /etc/pve is not writable, usually because the node
	// has no cluster quorum.
	ErrNoQuorum = errors.New("cfs: cluster filesystem not writable (no quorum?)")
	// ErrNotOnline means the cluster filesystem is not mounted.
	ErrNotOnline = errors.New("cfs: cluster filesystem not online")
	// ErrTimeout means the lock could not be acquired in time.
	ErrTimeout = errors.New("cfs: timeout acquiring lock")
)

// Locker acquires cluster-wide locks.
type Locker struct {
	// Dir is the lock directory (default /etc/pve/priv/lock).
	Dir string
	// AcquireTimeout bounds waiting for the lock (default 10s, like PVE).
	AcquireTimeout time.Duration
	// HoldTimeout bounds the critical section (default 60s; pmxcfs expires
	// locks after 120s).
	HoldTimeout time.Duration
	// StaleAfter, if set, removes locks older than this. pmxcfs expires
	// locks itself; this emulates that on ordinary filesystems (tests).
	StaleAfter time.Duration
}

var lockIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,99}$`)

// Do runs fn while holding the lock id. fn's context is cancelled when
// the hold timeout expires; fn must stop promptly then.
func (l *Locker) Do(ctx context.Context, id string, fn func(ctx context.Context) error) error {
	if !lockIDRe.MatchString(id) {
		return fmt.Errorf("cfs: invalid lock id %q", id)
	}
	dir := l.Dir
	if dir == "" {
		dir = DefaultLockDir
	}
	acquire := cmpOr(l.AcquireTimeout, 10*time.Second)
	hold := cmpOr(l.HoldTimeout, 60*time.Second)

	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		if quorumErr(err) {
			return ErrNoQuorum
		}
		return fmt.Errorf("%w: %w", ErrNotOnline, err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return ErrNotOnline
	}

	path := filepath.Join(dir, id)
	deadline := time.Now().Add(acquire)
	for {
		err := os.Mkdir(path, 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			if quorumErr(err) {
				return ErrNoQuorum
			}
			return fmt.Errorf("cfs: acquire lock %s: %w", id, err)
		}
		// Ask pmxcfs to drop the lock if its holder vanished.
		_ = os.Chtimes(path, time.Unix(0, 0), time.Unix(0, 0))
		if l.StaleAfter > 0 {
			if fi, err := os.Stat(path); err == nil && time.Since(fi.ModTime()) > l.StaleAfter {
				_ = os.Remove(path)
				continue
			}
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w %s", ErrTimeout, id)
		}
		wait := min(time.Until(deadline), 100*time.Millisecond)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	defer func() { _ = os.Remove(path) }()

	holdCtx, cancel := context.WithTimeoutCause(ctx, hold,
		fmt.Errorf("cfs: lock %s held longer than %s", id, hold))
	defer cancel()
	if err := fn(holdCtx); err != nil {
		return err
	}
	if cause := context.Cause(holdCtx); cause != nil && !errors.Is(cause, context.Canceled) {
		return cause
	}
	return nil
}

func quorumErr(err error) bool {
	return errors.Is(err, syscall.EROFS) || errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM)
}

func cmpOr(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
