// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/daemon"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/doctor"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/pve/cfs"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/recoverykit"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo/repotest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/restore"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/secrets"
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

// cluster is a PVE directory, dump directory and running daemon.
type cluster struct {
	t        *testing.T
	tools    string
	target   string
	pveDir   string
	dump     string
	socket   string
	baseCfg  string
	mu       sync.Mutex
	pveshLog [][]string
}

func startCluster(t *testing.T) *cluster {
	t.Helper()
	dir, err := os.MkdirTemp("", "prbcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	c := &cluster{t: t, pveDir: filepath.Join(dir, "pve"), dump: filepath.Join(dir, "backups", "dump"), socket: filepath.Join(dir, "run", "api.sock"),
		tools: filepath.Join(dir, "tools"), target: filepath.Join(dir, "restored", "dump")}
	for _, d := range []string{filepath.Join(c.pveDir, "priv", "lock"), c.dump, c.tools, c.target} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	c.baseCfg = fmt.Sprintf("dir: backups\n\tpath %s\n\tcontent backup\n\ndir: restored\n\tpath %s\n\tcontent backup\n\n"+
		"lvmthin: local-lvm\n\tthinpool data\n\tvgname pve\n\tcontent images,rootdir\n\n", filepath.Dir(c.dump), filepath.Dir(c.target))
	qmrestore := filepath.Join(c.tools, "qmrestore")
	script := fmt.Sprintf("#!/bin/sh\necho \"$@\" > %s/qmrestore.log\ncp \"$1\" %s/qmrestore.out\necho restored\n", c.tools, c.tools)
	if err := os.WriteFile(qmrestore, []byte(script), 0o700); err != nil { //nolint:gosec // test script must be executable
		t.Fatal(err)
	}
	c.writeCfg(c.baseCfg)
	nodeCfg := filepath.Join(c.pveDir, "nodes", "pve1", "pve-rclone-backup.cfg")
	_ = os.MkdirAll(filepath.Dir(nodeCfg), 0o700)
	if err := os.WriteFile(nodeCfg, []byte("staging-dir: "+filepath.Join(dir, "staging")+"\nstaging-reserve: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	locker := &cfs.Locker{Dir: filepath.Join(c.pveDir, "priv", "lock")}
	keys := secrets.NewKeyStore(filepath.Join(c.pveDir, "priv", "pve-rclone-backup"), locker)
	ctx, cancel := context.WithCancel(context.Background())
	ready, done := make(chan struct{}), make(chan error, 1)
	go func() {
		done <- daemon.Run(ctx, daemon.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Socket: c.socket,
			StateDir: filepath.Join(dir, "state"), PVEDir: c.pveDir, Node: "pve1", KeyStore: keys, Locker: locker,
			RestoreTools: &restore.Tools{QMRestore: qmrestore},
			AllowUIDs:    []uint32{uint32(os.Getuid())}, Ready: func() { close(ready) }})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("daemon: %v", err)
	}
	t.Cleanup(func() { cancel(); <-done })
	return c
}

func (c *cluster) writeCfg(content string) {
	c.t.Helper()
	tmp := filepath.Join(c.pveDir, ".storage.cfg.tmp")
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		c.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(c.pveDir, "storage.cfg")); err != nil {
		c.t.Fatal(err)
	}
}

// pvesh emulates "pvesh create /storage" by adding the section.
func (c *cluster) pvesh(_ context.Context, args ...string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pveshLog = append(c.pveshLog, args)
	if len(args) < 2 || args[0] != "create" || args[1] != "/storage" {
		return nil, nil
	}
	opts := map[string]string{}
	for i := 2; i+1 < len(args); i += 2 {
		opts[strings.TrimPrefix(args[i], "--")] = args[i+1]
	}
	section := fmt.Sprintf("%s: %s\n", opts["type"], opts["storage"])
	for _, k := range []string{"rclone-remote", "rclone-path", "rclone-source", "rclone-encryption", "rclone-replicate-from", "rclone-immutable", "content"} {
		if v, ok := opts[k]; ok {
			section += fmt.Sprintf("\t%s %s\n", k, v)
		}
	}
	c.baseCfg += section + "\n"
	c.writeCfg(c.baseCfg)
	return nil, nil
}

func (c *cluster) app(in io.Reader, interactive bool) (*App, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	if in == nil {
		in = strings.NewReader("")
	}
	return &App{In: in, Out: &out, Err: &errb, Socket: c.socket, Output: "table", Interactive: interactive, Now: time.Now, PVESH: c.pvesh}, &out, &errb
}

func (c *cluster) run(args ...string) result {
	c.t.Helper()
	a, out, errb := c.app(nil, false)
	code := a.Run(c.t.Context(), args)
	return result{code, out.String(), errb.String()}
}

func TestCLIEndToEnd(t *testing.T) {
	loc, _ := repotest.Remote(t, nil)
	c := startCluster(t)
	archive := filepath.Join(c.dump, "vzdump-qemu-100-2026_10_04-02_00_01.vma.zst")
	if err := os.WriteFile(archive, bytes.Repeat([]byte("vma"), 5000), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(c.dump, "vzdump-qemu-100-2026_10_04-02_00_01.log"), []byte("INFO: done\n"), 0o600)
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(archive, old, old)

	if r := c.run("remote", "list"); r.code != 0 || !strings.Contains(r.out, loc.Remote) {
		t.Fatalf("remote list (%d): %s%s", r.code, r.out, r.err)
	}

	// Interactive init: the operator types the checksum of the kit file.
	kitFile := filepath.Join(t.TempDir(), "kit.txt")
	pr, pw := io.Pipe()
	go func() {
		for {
			if text, err := os.ReadFile(kitFile); err == nil && len(text) > 0 {
				sum, _ := recoverykit.TextChecksum(string(text))
				_, _ = fmt.Fprintln(pw, sum)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	a, out, errb := c.app(pr, true)
	code := a.Run(t.Context(), []string{"storage", "init", "offsite", "--remote", loc.Remote, "--source", "homelab",
		"--replicate-from", "backups", "--kit-file", kitFile})
	if code != 0 || !strings.Contains(out.String(), "Recovery kit confirmed") || !strings.Contains(out.String(), "Storage offsite added") {
		t.Fatalf("storage init (%d):\n%s%s", code, out, errb)
	}
	if fi, err := os.Stat(kitFile); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("kit file: %v %v", fi, err)
	}
	c.mu.Lock()
	if got := strings.Join(c.pveshLog[0], " "); !strings.Contains(got, "create /storage --storage offsite --type rclone-backup") ||
		!strings.Contains(got, "--rclone-replicate-from backups") || !strings.Contains(got, "--rclone-encryption crypt") {
		t.Fatalf("pvesh = %s", got)
	}
	c.mu.Unlock()

	// The archive replicates once the storage is up.
	deadline := time.Now().Add(30 * time.Second)
	var backups []map[string]any
	for {
		r := c.run("-o", "json", "backup", "list")
		if r.code != 0 {
			t.Fatalf("backup list (%d): %s", r.code, r.err)
		}
		_ = json.Unmarshal([]byte(r.out), &backups)
		if len(backups) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not replicated: %s\n%s", r.out, c.run("queue", "list").out)
		}
		time.Sleep(100 * time.Millisecond)
	}
	volid := "offsite:backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst"
	if r := c.run("backup", "inspect", volid); r.code != 0 || !regexp.MustCompile(`sha256 [0-9a-f]{64}`).MatchString(r.out) {
		t.Fatalf("inspect (%d): %s%s", r.code, r.out, r.err)
	}
	if r := c.run("backup", "list"); r.code != 0 || !strings.Contains(r.out, volid) || !strings.Contains(r.out, "complete") {
		t.Fatalf("backup list: %s", r.out)
	}
	if r := c.run("queue", "list", "--state", "complete"); r.code != 0 || !strings.Contains(r.out, "qemu/100") {
		t.Fatalf("queue list: %s%s", r.out, r.err)
	}
	if r := c.run("status"); r.code != 0 || !strings.Contains(r.out, "offsite") || !strings.Contains(r.out, "confirmed") {
		t.Fatalf("status: %s%s", r.out, r.err)
	}
	if r := c.run("storage", "show", "offsite"); r.code != 0 || !strings.Contains(r.out, "Recovery kit: confirmed") {
		t.Fatalf("storage show: %s", r.out)
	}
	if r := c.run("storage", "resync", "offsite"); r.code != 0 {
		t.Fatalf("resync: %s", r.err)
	}

	// Fetch into another local storage and restore through qmrestore.
	pollInterval = 50 * time.Millisecond
	if r := c.run("backup", "fetch", volid, "--to-storage", "restored"); r.code != 0 || !strings.Contains(r.out, "complete") {
		t.Fatalf("fetch (%d): %s%s", r.code, r.out, r.err)
	}
	fetched := filepath.Join(c.target, "vzdump-qemu-100-2026_10_04-02_00_01.vma.zst")
	if got, err := os.ReadFile(fetched); err != nil || !bytes.Equal(got, bytes.Repeat([]byte("vma"), 5000)) {
		t.Fatalf("fetched archive: %v", err)
	}
	if _, err := os.Stat(fetched + ".protected"); err != nil {
		t.Fatal("fetched archive not protected")
	}
	if r := c.run("restore", volid, "--vmid", "900", "--target-storage", "local-lvm", "--mode", "stage"); r.code != 0 ||
		!strings.Contains(r.err, "qmrestore: restored") {
		t.Fatalf("restore (%d): %s%s", r.code, r.out, r.err)
	}
	if args, _ := os.ReadFile(filepath.Join(c.tools, "qmrestore.log")); !strings.Contains(string(args), " 900 --storage local-lvm") {
		t.Fatalf("qmrestore args = %q", args)
	}
	if r := c.run("restore", volid, "--vmid", "901", "--mode", "bogus"); r.code != ExitError || !strings.Contains(r.err, "not available") {
		t.Fatalf("invalid mode (%d): %s", r.code, r.err)
	}

	plugin := filepath.Join(t.TempDir(), "RcloneBackupPlugin.pm")
	_ = os.WriteFile(plugin, nil, 0o600)
	oldPath := doctor.PluginPath
	doctor.PluginPath = plugin
	defer func() { doctor.PluginPath = oldPath }()
	if r := c.run("doctor"); r.code != 0 {
		t.Fatalf("doctor (%d): %s%s", r.code, r.out, r.err)
	}

	if r := c.run("recovery-kit", "show", kitFile, "--show-secrets"); r.code != 0 || !strings.Contains(r.out, "type = crypt") ||
		!strings.Contains(r.out, "password = ") || !strings.Contains(r.out, "homelab") {
		t.Fatalf("kit show: %s%s", r.out, r.err)
	}
	if r := c.run("recovery-kit", "verify", kitFile); r.code != 0 || !strings.Contains(r.out, "OK") {
		t.Fatalf("kit verify (%d): %s%s", r.code, r.out, r.err)
	}

	// Non-interactive init: confirm the checksum separately, then add.
	second := filepath.Join(t.TempDir(), "second.txt")
	r := c.run("storage", "init", "second", "--remote", loc.Remote, "--path", "second", "--source", "homelab", "--kit-file", second)
	sum := regexp.MustCompile(`--confirm-checksum (\S+)`).FindStringSubmatch(r.err)
	if r.code != ExitUsage || sum == nil {
		t.Fatalf("non-interactive init (%d): %s%s", r.code, r.out, r.err)
	}
	if r := c.run("recovery-kit", "confirm", sum[1]); r.code != 0 {
		t.Fatalf("confirm: %s", r.err)
	}
	r = c.run("storage", "init", "second", "--remote", loc.Remote, "--path", "second", "--source", "homelab")
	if r.code != 0 || !strings.Contains(r.out, "Adopted existing repository") || !strings.Contains(r.out, "Storage second added") {
		t.Fatalf("second init (%d): %s%s", r.code, r.out, r.err)
	}
	if r := c.run("recovery-kit", "status"); r.code != 0 || strings.Count(r.out, "yes") < 2 {
		t.Fatalf("kit status: %s", r.out)
	}

	// Disaster recovery on a fresh installation with the first kit.
	dr := startCluster(t)
	r = dr.run("recover", kitFile)
	if r.code != 0 || !strings.Contains(r.out, "keys imported") || !strings.Contains(r.out, "Storage offsite-dr added") {
		t.Fatalf("recover (%d): %s%s", r.code, r.out, r.err)
	}
	dr.mu.Lock()
	if got := strings.Join(dr.pveshLog[0], " "); !strings.Contains(got, "--storage offsite-dr") || strings.Contains(got, "replicate-from") ||
		!strings.HasSuffix(got, "--rclone-immutable 1") {
		t.Fatalf("DR pvesh = %s", got)
	}
	dr.mu.Unlock()
	deadline = time.Now().Add(30 * time.Second)
	for {
		r := dr.run("backup", "list", "--storage", "offsite-dr")
		if strings.Contains(r.out, "offsite-dr:backup/vzdump-qemu-100-2026_10_04-02_00_01") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("DR catalogue: %s%s\n%s", r.out, r.err, dr.run("storage", "show", "offsite-dr").out)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if r := dr.run("recover", kitFile); r.code != 0 || !strings.Contains(r.out, "exists already") {
		t.Fatalf("second recover (%d): %s%s", r.code, r.out, r.err)
	}
	// The recovered storage cannot delete the lost installation's backups.
	if r := dr.run("backup", "delete", "offsite-dr:backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst", "--yes"); r.code == 0 ||
		!strings.Contains(r.err, "immutable") {
		t.Fatalf("delete on a recovered storage (%d): %s", r.code, r.err)
	}
	r = dr.run("storage", "init", "xx", "--remote", loc.Remote, "--source", "nosuch", "--read-only")
	if r.code == 0 || !strings.Contains(r.err, `no source "nosuch"`) {
		t.Fatalf("read-only init of a missing source (%d): %s", r.code, r.err)
	}
	if r := dr.run("storage", "init", "xx", "--remote", loc.Remote, "--source", "homelab", "--read-only", "--replicate-from", "backups"); r.code != ExitUsage {
		t.Fatalf("read-only with replication: %d", r.code)
	}
}
