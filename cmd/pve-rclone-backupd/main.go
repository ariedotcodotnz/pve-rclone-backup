// SPDX-License-Identifier: AGPL-3.0-or-later

// Command pve-rclone-backupd is the pve-rclone-backup daemon. It discovers
// completed vzdump archives, replicates them offsite through an embedded
// rclone engine and serves the local API used by the Perl storage plugin,
// the CLI and the TUI.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/daemon"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/logging"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/pve/cfs"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/secrets"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/version"
)

type uidList []uint32

func (u *uidList) String() string {
	parts := make([]string, len(*u))
	for i, v := range *u {
		parts[i] = strconv.FormatUint(uint64(v), 10)
	}
	return strings.Join(parts, ",")
}

func (u *uidList) Set(s string) error {
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return fmt.Errorf("invalid uid %q", s)
	}
	*u = append(*u, uint32(n))
	return nil
}

func main() {
	var (
		showVersion = flag.Bool("version", false, "print version information and exit")
		socket      = flag.String("socket", daemon.DefaultSocket, "API socket path")
		stateDir    = flag.String("state-dir", daemon.DefaultStateDir, "state directory")
		pveDir      = flag.String("pve-dir", daemon.DefaultPVEDir, "root of the Proxmox VE cluster file system")
		logLevel    = flag.String("log-level", "info", "log level: debug, info, warn, error")
		logFormat   = flag.String("log-format", "text", "log format: text or json")
		allowUIDs   uidList
	)
	flag.Var(&allowUIDs, "allow-uid", "additional uid allowed to use the API (repeatable; root is always allowed)")
	flag.Parse()

	if *showVersion {
		fmt.Println("pve-rclone-backupd", version.Get())
		fmt.Println("rclone engine", transport.RcloneVersion())
		return
	}

	lv := new(slog.LevelVar)
	level, err := logging.ParseLevel(*logLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pve-rclone-backupd:", err)
		os.Exit(2)
	}
	lv.Set(level)
	log, err := logging.New(os.Stderr, *logFormat, lv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pve-rclone-backupd:", err)
		os.Exit(2)
	}
	slog.SetDefault(log)

	// Secrets live in pmxcfs's root-only priv directory, written under
	// cluster locks. rclone reads and refreshes OAuth tokens through the
	// remote store, so the engine is initialized with it before anything
	// else touches rclone.
	secretsDir := filepath.Join(*pveDir, "priv", "pve-rclone-backup")
	locker := &cfs.Locker{Dir: filepath.Join(*pveDir, "priv", "lock")}
	remotes := secrets.NewRemoteStore(secrets.RemotesPath(secretsDir), locker, log)
	if err := transport.Init(transport.Options{ConfigPath: remotes.Path(), Storage: remotes, Logger: log.Handler()}); err != nil {
		log.Error("initialize rclone engine", "err", err)
		os.Exit(1)
	}
	keys := secrets.NewKeyStore(secretsDir, locker)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	// Configuration reload on SIGHUP arrives with the configuration watcher;
	// until then the signal is acknowledged and ignored instead of killing
	// the daemon.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			log.Info("SIGHUP received; configuration is re-read automatically")
		}
	}()

	err = daemon.Run(ctx, daemon.Options{
		Logger:    log,
		Socket:    *socket,
		StateDir:  *stateDir,
		PVEDir:    *pveDir,
		Keys:      keys.Load,
		AllowUIDs: append([]uint32{0}, allowUIDs...),
	})
	if err != nil {
		log.Error("daemon failed", "err", err)
		os.Exit(1)
	}
}
