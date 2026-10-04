// SPDX-License-Identifier: AGPL-3.0-or-later

// Command pve-rclone-backup is the administration CLI and TUI for
// pve-rclone-backupd. It is a thin client of the daemon API.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Parse()

	if *showVersion || flag.Arg(0) == "version" {
		fmt.Println("pve-rclone-backup", version.Get())
		return
	}

	fmt.Fprintln(os.Stderr, "pve-rclone-backup: commands not implemented yet (see docs/adr/0001-architecture.md)")
	os.Exit(2)
}
