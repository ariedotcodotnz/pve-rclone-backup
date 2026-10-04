// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/client"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

type env struct {
	socket, stateDir, pveDir string
	keys                     repo.KeyLoader
}

func newEnv(t *testing.T) env {
	t.Helper()
	dir, err := os.MkdirTemp("", "prbd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return env{socket: filepath.Join(dir, "run", "api.sock"), stateDir: filepath.Join(dir, "state"), pveDir: filepath.Join(dir, "pve")}
}

// start runs the daemon and returns a stop function that waits for exit.
func start(t *testing.T, e env) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			Socket:    e.socket,
			StateDir:  e.stateDir,
			PVEDir:    e.pveDir,
			Keys:      e.keys,
			Node:      "pve-test",
			AllowUIDs: []uint32{uint32(os.Getuid())},
			Ready:     func() { close(ready) },
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("daemon exited early: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("daemon did not become ready")
	}
	return func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(15 * time.Second):
			t.Fatal("daemon did not stop")
			return nil
		}
	}
}

func TestRunServesAPIAndNotifiesSystemd(t *testing.T) {
	e := newEnv(t)
	notifyPath := filepath.Join(filepath.Dir(e.socket), "notify.sock")
	_ = os.MkdirAll(filepath.Dir(notifyPath), 0o700)
	notify, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: notifyPath, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = notify.Close() }()
	t.Setenv("NOTIFY_SOCKET", notifyPath)

	stop := start(t, e)
	c := client.New(e.socket)
	v, err := c.Version(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if v.API != 1 || v.Node != "pve-test" || v.Schema != store.LatestSchemaVersion() || v.InstanceID == "" || v.Rclone == "" {
		t.Fatalf("version = %+v", v)
	}
	st, err := c.Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Healthy || st.Node != "pve-test" || len(st.Alerts) != 0 {
		t.Fatalf("status = %+v", st)
	}
	if err := stop(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if _, err := os.Stat(e.socket); !os.IsNotExist(err) {
		t.Fatal("socket left behind after shutdown")
	}

	var msgs []string
	buf := make([]byte, 4096)
	for {
		_ = notify.SetReadDeadline(time.Now().Add(time.Second))
		n, err := notify.Read(buf)
		if err != nil {
			break
		}
		msgs = append(msgs, string(buf[:n]))
	}
	all := strings.Join(msgs, "\n")
	if !strings.Contains(all, "READY=1") || !strings.Contains(all, "STOPPING=1") {
		t.Fatalf("systemd notifications = %q", msgs)
	}
}

func TestCorruptDatabaseIsRebuilt(t *testing.T) {
	e := newEnv(t)
	_ = os.MkdirAll(e.stateDir, 0o700)
	db := filepath.Join(e.stateDir, "state.db")
	if err := os.WriteFile(db, []byte(strings.Repeat("garbage", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := start(t, e)
	defer func() { _ = stop() }()
	st, err := client.New(e.socket).Status(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Alerts) != 1 || st.Alerts[0].ID != "state-db-rebuilt" {
		t.Fatalf("alerts = %+v", st.Alerts)
	}
	matches, _ := filepath.Glob(db + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("corrupt database not kept aside: %v", matches)
	}
}

func TestInterruptedJobsAreRequeued(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(e.stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := st.InsertJob(ctx, &store.Job{Kind: "replicate", StoreID: "offsite", State: "queued", DedupeKey: "k", OwnerNode: "pve-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClaimRunnable(ctx, "replicate", []string{"queued"}, "uploading", "pve-test", 600); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	stop := start(t, e)
	status, err := client.New(e.socket).Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Jobs.Queued != 1 || status.Jobs.Active != 0 {
		t.Fatalf("job counts = %+v", status.Jobs)
	}
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	st, _ = store.Open(ctx, filepath.Join(e.stateDir, "state.db"))
	defer func() { _ = st.Close() }()
	if j, _ := st.GetJob(ctx, id); j.State != "queued" || j.LeaseUntil != nil {
		t.Fatalf("job = %+v", j)
	}
}

func TestSecondDaemonRefused(t *testing.T) {
	e := newEnv(t)
	stop := start(t, e)
	defer func() { _ = stop() }()
	err := Run(t.Context(), Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Socket: e.socket, StateDir: t.TempDir(), AllowUIDs: []uint32{uint32(os.Getuid())}})
	if err == nil || !strings.Contains(err.Error(), "already listening") {
		t.Fatalf("second daemon: %v", err)
	}
}
