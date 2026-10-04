// SPDX-License-Identifier: AGPL-3.0-or-later

// Command pve-rclone-backup is the administration CLI for
// pve-rclone-backupd. It is a thin client of the daemon API.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.New().Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
