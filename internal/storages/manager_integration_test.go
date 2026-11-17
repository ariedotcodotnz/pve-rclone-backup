// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package storages

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo/repotest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport/faultfs"
)

func TestMain(m *testing.M) {
	cleanup, err := repotest.InitTransport()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// TestRefreshUsage: after a deletion or an upload, the quota is refreshed
// soon rather than at the next about-interval.
func TestRefreshUsage(t *testing.T) {
	ctx := t.Context()
	ctl := &faultfs.Controller{}
	ctl.SetCapacity(1 << 30)
	loc, dir := repotest.Remote(t, ctl)
	r, err := repo.Init(ctx, loc, repo.InitOptions{Keys: repotest.NewKeys(t)})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	pveDir := t.TempDir()
	cfg := fmt.Sprintf("rclone-backup: offsite\n\trclone-remote %s\n\trclone-path pve-backups\n\trclone-source homelab\n\tcontent backup\n", loc.Remote)
	if err := os.WriteFile(filepath.Join(pveDir, "storage.cfg"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(Options{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Store: st, Node: "pve1", PVEDir: pveDir,
		Keys: repotest.Loader(r.Keys), AboutInterval: time.Hour, RefreshDelay: 200 * time.Millisecond, Tick: 20 * time.Millisecond})
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(runCtx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	used := func() int64 {
		s, _ := m.Get("offsite")
		if s.Usage == nil || s.Usage.Used == nil {
			return -1
		}
		return *s.Usage.Used
	}
	deadline := time.Now().Add(10 * time.Second)
	for used() < 0 {
		if time.Now().After(deadline) {
			t.Fatal("no quota")
		}
		time.Sleep(20 * time.Millisecond)
	}
	before := used()
	if err := os.WriteFile(filepath.Join(dir, "upload.bin"), make([]byte, 1<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if used() != before {
		t.Fatal("the quota was refreshed before its interval without a request")
	}
	m.RefreshUsage("another-remote")
	m.RefreshUsage(loc.Remote)
	deadline = time.Now().Add(5 * time.Second)
	for used() < before+1<<20 {
		if time.Now().After(deadline) {
			t.Fatalf("quota not refreshed: used %d, before %d", used(), before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
