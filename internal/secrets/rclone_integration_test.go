// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package secrets_test

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/rc"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/pve/cfs"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/secrets"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

// rclone itself must read and persist remotes through RemoteStore.
func TestRcloneUsesRemoteStore(t *testing.T) {
	dir := t.TempDir()
	lock := &cfs.Locker{Dir: filepath.Join(dir, "lock"), AcquireTimeout: 10 * time.Second}
	path := secrets.RemotesPath(dir)
	if err := os.WriteFile(path, []byte("[existing]\ntype = local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := secrets.NewRemoteStore(path, lock, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := transport.Init(transport.Options{ConfigPath: path, Storage: store}); err != nil {
		t.Fatal(err)
	}

	if _, err := config.CreateRemote(t.Context(), "scratch", "local", rc.Params{"nounc": "true"},
		config.UpdateRemoteOpt{NonInteractive: true}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[existing]", "[scratch]", "type = local", "nounc = true"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("remotes.conf lacks %q:\n%s", want, raw)
		}
	}
	if !config.LoadedData().HasSection("existing") {
		t.Fatal("rclone does not see remotes from the file")
	}
	tgt, err := transport.NewTarget(t.Context(), "scratch:"+filepath.Join(dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	if tgt.Encrypted() {
		t.Fatal("unexpected crypt target")
	}
}
