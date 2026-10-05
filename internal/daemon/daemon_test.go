// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/client"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

type env struct {
	socket, stateDir, pveDir string
	keys                     repo.KeyLoader
	keyStore                 KeyStore
}

func newEnv(t *testing.T) env {
	t.Helper()
	dir, err := os.MkdirTemp("", "prbd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// pmxcfs provides the lock directory.
	if err := os.MkdirAll(filepath.Join(dir, "pve", "priv", "lock"), 0o700); err != nil {
		t.Fatal(err)
	}
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
			KeyStore:  e.keyStore,
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

func TestJobEndpoints(t *testing.T) {
	e := newEnv(t)
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(e.stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	queued, _, _ := st.InsertJob(ctx, &store.Job{Kind: "replicate", StoreID: "offsite", State: "queued", DedupeKey: "q",
		OwnerNode: "pve-test", BackupVolname: "backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst", VMType: "qemu", VMID: 100})
	failed, _, _ := st.InsertJob(ctx, &store.Job{Kind: "replicate", StoreID: "offsite", State: "failed", DedupeKey: "f",
		OwnerNode: "pve-test", LastError: "boom"})
	if err := st.PutSegment(ctx, store.Segment{JobID: queued, Index: 0, Offset: 0, Size: 10, State: "uploaded"}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	stop := start(t, e)
	defer func() { _ = stop() }()
	c := client.New(e.socket)
	var list []apiv1.Job
	if err := c.Do(ctx, http.MethodGet, "/v1/jobs?state=queued,failed&storage=offsite", nil, &list); err != nil || len(list) != 2 {
		t.Fatalf("jobs = %+v, %v", list, err)
	}
	var detail apiv1.JobDetail
	if err := c.Do(ctx, http.MethodGet, fmt.Sprintf("/v1/jobs/%d", queued), nil, &detail); err != nil {
		t.Fatal(err)
	}
	if detail.VMID != 100 || len(detail.Segments) != 1 || len(detail.Events) != 1 || detail.Events[0].ToState != "queued" {
		t.Fatalf("detail = %+v", detail)
	}

	var j apiv1.Job
	if err := c.Do(ctx, http.MethodPost, fmt.Sprintf("/v1/jobs/%d/cancel", queued), nil, &j); err != nil || j.State != "cancelled" {
		t.Fatalf("cancel = %+v, %v", j, err)
	}
	if err := c.Do(ctx, http.MethodPost, fmt.Sprintf("/v1/jobs/%d/retry", failed), nil, &j); err != nil || j.State != "queued" || j.LastError != "" {
		t.Fatalf("retry = %+v, %v", j, err)
	}
	if err := c.Do(ctx, http.MethodPost, fmt.Sprintf("/v1/jobs/%d/retry", failed), nil, nil); !client.IsCode(err, apiv1.CodeConflict) {
		t.Fatalf("retry a queued job: %v", err)
	}
	if err := c.Do(ctx, http.MethodPost, fmt.Sprintf("/v1/jobs/%d/priority", failed), map[string]int{"priority": 7}, &j); err != nil || j.Priority != 7 {
		t.Fatalf("priority = %+v, %v", j, err)
	}
	if err := c.Do(ctx, http.MethodPost, fmt.Sprintf("/v1/jobs/%d/priority", failed), map[string]int{"priority": 1000}, nil); !client.IsCode(err, apiv1.CodeInvalidArgument) {
		t.Fatalf("out of range priority: %v", err)
	}
	for path, code := range map[string]string{"/v1/jobs/999": apiv1.CodeNotFound, "/v1/jobs/abc": apiv1.CodeInvalidArgument, "/v1/jobs?limit=0": apiv1.CodeInvalidArgument} {
		if err := c.Do(ctx, http.MethodGet, path, nil, nil); !client.IsCode(err, code) {
			t.Errorf("%s: %v", path, err)
		}
	}
}

// TestSameStateDirRefusedBeforeTouchingJobs: a second daemon on the same
// state directory stops before it opens the database, so it cannot requeue
// the running daemon's jobs.
func TestSameStateDirRefusedBeforeTouchingJobs(t *testing.T) {
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

	unlock, err := lockStateDir(e.stateDir) // the running daemon
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	err = Run(ctx, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Socket: e.socket,
		StateDir: e.stateDir, PVEDir: e.pveDir, AllowUIDs: []uint32{uint32(os.Getuid())}})
	if err == nil || !strings.Contains(err.Error(), "another pve-rclone-backupd") {
		t.Fatalf("second daemon on the same state directory: %v", err)
	}
	st, _ = store.Open(ctx, filepath.Join(e.stateDir, "state.db"))
	defer func() { _ = st.Close() }()
	if j, _ := st.GetJob(ctx, id); j.State != "uploading" || j.LeaseUntil == nil {
		t.Fatalf("the refused daemon changed the running daemon's job: %+v", j)
	}
}

// TestCancelledStartKeepsDatabase: a start-up interrupted while opening the
// database must not move a healthy database aside.
func TestCancelledStartKeepsDatabase(t *testing.T) {
	e := newEnv(t)
	path := filepath.Join(e.stateDir, "state.db")
	st, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetMeta(t.Context(), "marker", "kept"); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := Run(ctx, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Socket: e.socket,
		StateDir: e.stateDir, PVEDir: e.pveDir, AllowUIDs: []uint32{uint32(os.Getuid())}}); err == nil {
		t.Fatal("cancelled start succeeded")
	}
	if moved, _ := filepath.Glob(path + ".corrupt-*"); len(moved) != 0 {
		t.Fatalf("healthy database moved aside: %v", moved)
	}
	st, err = store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if v, _, _ := st.Meta(t.Context(), "marker"); v != "kept" {
		t.Fatalf("database contents lost: marker %q", v)
	}
}

// TestShutdownIsClean: stopping the daemon returns no error, also when the
// API server finishes shutting down before the daemon notices the signal.
func TestShutdownIsClean(t *testing.T) {
	e := newEnv(t)
	for i := range 15 {
		stop := start(t, e)
		if err := stop(); err != nil {
			t.Fatalf("shutdown %d: %v", i, err)
		}
	}
}
