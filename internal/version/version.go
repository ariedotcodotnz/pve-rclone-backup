// SPDX-License-Identifier: AGPL-3.0-or-later

// Package version reports build and API version information shared by the
// daemon, the CLI and the Perl integration handshake.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Version and Commit are overridden at build time via
// -ldflags "-X github.com/ariedotcodotnz/pve-rclone-backup/internal/version.Version=...".
var (
	Version = "dev"
	Commit  = ""
)

// APIMajor is the daemon IPC API major version (served under /v1).
const APIMajor = 1

// Info describes the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	GoVersion string `json:"go_version"`
	APIMajor  int    `json:"api_major"`
}

// Get returns version information, falling back to VCS data embedded by the
// Go toolchain when no ldflags were supplied.
func Get() Info {
	info := Info{
		Version:   Version,
		Commit:    Commit,
		GoVersion: runtime.Version(),
		APIMajor:  APIMajor,
	}
	if info.Commit == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" {
					info.Commit = s.Value
				}
			}
		}
	}
	return info
}

// String renders a one-line human readable version string.
func (i Info) String() string {
	s := fmt.Sprintf("%s (api v%d, %s)", i.Version, i.APIMajor, i.GoVersion)
	if i.Commit != "" {
		c := i.Commit
		if len(c) > 12 {
			c = c[:12]
		}
		s = fmt.Sprintf("%s (commit %s, api v%d, %s)", i.Version, c, i.APIMajor, i.GoVersion)
	}
	return s
}
