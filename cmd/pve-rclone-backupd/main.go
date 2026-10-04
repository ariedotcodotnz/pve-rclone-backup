// SPDX-License-Identifier: AGPL-3.0-or-later

// Command pve-rclone-backupd is the pve-rclone-backup daemon. It discovers
// completed vzdump archives, replicates them offsite through an embedded
// rclone engine and serves the local API used by the Perl storage plugin,
// the CLI and the TUI.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("pve-rclone-backupd", version.Get())
		fmt.Println("rclone engine", transport.RcloneVersion())
		return
	}

	fmt.Fprintln(os.Stderr, "pve-rclone-backupd: daemon not implemented yet (see docs/adr/0001-architecture.md)")
	os.Exit(1)
}
