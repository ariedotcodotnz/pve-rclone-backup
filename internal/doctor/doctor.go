// SPDX-License-Identifier: AGPL-3.0-or-later

// Package doctor checks an installation through the daemon API. The CLI
// and the TUI show its findings.
package doctor

import (
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/client"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/textfmt"
)

// PluginPath is where PVE loads the storage plugin from.
var PluginPath = "/usr/share/perl5/PVE/Storage/Custom/RcloneBackupPlugin.pm"

// Check levels.
const (
	OK   = "ok"
	Warn = "warn"
	Fail = "fail"
)

// Check is one finding.
type Check struct {
	Level   string `json:"level"`
	Subject string `json:"subject"`
	Message string `json:"message"`
}

// Run checks the installation.
func Run(ctx context.Context, c *client.Client) []Check {
	var out []Check
	add := func(level, subject, format string, args ...any) {
		out = append(out, Check{level, subject, fmt.Sprintf(format, args...)})
	}
	if _, err := os.Stat(PluginPath); err != nil {
		add(Warn, "PVE plugin", "%s not found: PVE cannot list offsite backups", PluginPath)
	} else {
		add(OK, "PVE plugin", "installed")
	}
	v, err := c.Version(ctx)
	if err != nil {
		add(Fail, "daemon", "%v", err)
		return out
	}
	if v.APIMinClient > apiv1.Revision || v.API < apiv1.Revision {
		add(Fail, "daemon", "API %d does not support this client (API %d)", v.API, apiv1.Revision)
	} else {
		add(OK, "daemon", "%s (rclone %s) on %s", v.Daemon, v.Rclone, v.Node)
	}
	if st, err := c.Status(ctx); err == nil {
		for _, p := range st.Problems {
			add(Warn, "daemon", "%s", p)
		}
		for _, al := range st.Alerts {
			level := Warn
			if al.Severity == "error" {
				level = Fail
			}
			add(level, "alert "+al.ID, "%s", al.Message)
		}
		if st.Jobs.Failed > 0 {
			add(Warn, "queue", "%d failed uploads; see 'pve-rclone-backup queue list --state failed'", st.Jobs.Failed)
		}
	}
	var storages []apiv1.Storage
	if err := c.Do(ctx, http.MethodGet, "/v1/storages", nil, &storages); err == nil {
		for _, s := range storages {
			switch {
			case !s.Enabled:
			case s.Health != apiv1.HealthOK:
				detail := s.HealthDetail
				if detail == "" {
					detail = "-"
				}
				add(Fail, "storage "+s.ID, "%s: %s", s.Health, detail)
			case s.Encryption == "crypt" && !s.KitConfirmed:
				add(Fail, "storage "+s.ID, "no confirmed recovery kit; run 'pve-rclone-backup recovery-kit export --storage %s'", s.ID)
			default:
				add(OK, "storage "+s.ID, "healthy")
			}
			if s.Enabled {
				if level, msg := space(s); msg != "" {
					add(level, "space "+s.ID, "%s", msg)
				}
				for _, w := range s.Warnings {
					add(Warn, "space "+s.ID, "%s", w)
				}
			}
		}
	}
	var remotes []apiv1.Remote
	if err := c.Do(ctx, http.MethodGet, "/v1/remotes", nil, &remotes); err == nil {
		for _, r := range remotes {
			if !r.Supported || len(r.Storages) == 0 {
				continue
			}
			if r.OAuth && !r.Authorized {
				add(Fail, "remote "+r.Name, "not authorized; run 'pve-rclone-backup remote reconnect %s'", r.Name)
			} else {
				add(OK, "remote "+r.Name, "%s", r.Type)
			}
		}
	}
	return out
}

// space reports what a storage's backups take in the cloud, next to the
// quota of the whole cloud account, which other files count towards too.
// A nearly full account is a warning.
func space(s apiv1.Storage) (level, message string) {
	if s.Space == nil && s.Usage == nil {
		return "", ""
	}
	backups := "-"
	if sp := s.Space; sp != nil {
		backups = textfmt.Backups(sp.Backups, sp.BackupBytes, sp.Tombstoned, sp.TombstonedBytes)
	}
	message = "backups " + backups
	if sp := s.Space; sp != nil && sp.Incomplete+sp.InProgress > 0 {
		message += fmt.Sprintf("; incomplete uploads %s, of which %s in progress",
			textfmt.Bytes(sp.IncompleteBytes+sp.InProgressBytes), textfmt.Bytes(sp.InProgressBytes))
	}
	level = OK
	if u := s.Usage; u != nil {
		message += "; account " + textfmt.Account(u.Total, u.Used, u.Free, u.Trashed) +
			" (the whole cloud account, as of " + textfmt.Time(&u.UpdatedAt) + ")"
		if u.Free != nil && (*u.Free <= 0 || u.Total != nil && *u.Free < *u.Total/100) {
			level, message = Warn, "the cloud account is full: "+message
		}
	}
	return level, message
}

// Healthy reports whether every check passed.
func Healthy(checks []Check) bool {
	for _, c := range checks {
		if c.Level != OK {
			return false
		}
	}
	return true
}
