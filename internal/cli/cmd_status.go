// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
)

func (a *App) statusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon health, storages and the upload queue",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			st, err := a.api().Status(ctx)
			if err != nil {
				return err
			}
			var storages []apiv1.Storage
			if err := a.do(ctx, http.MethodGet, "/v1/storages", nil, &storages); err != nil {
				return err
			}
			if a.Output == "json" {
				return a.json(map[string]any{"status": st, "storages": storages})
			}
			health := "healthy"
			if !st.Healthy {
				health = "attention needed"
			}
			fmt.Fprintf(a.Out, "Daemon:  %s on %s, up %s, %s\n", st.Version, st.Node, (time.Duration(st.UptimeSec) * time.Second).String(), health)
			fmt.Fprintf(a.Out, "Queue:   %d queued, %d active, %d waiting to retry, %d failed\n",
				st.Jobs.Queued, st.Jobs.Active, st.Jobs.RetryWait, st.Jobs.Failed)
			for _, p := range st.Problems {
				fmt.Fprintln(a.Out, "Problem:", p)
			}
			for _, al := range st.Alerts {
				fmt.Fprintf(a.Out, "Alert:   [%s] %s\n", al.Severity, al.Message)
			}
			if len(storages) == 0 {
				fmt.Fprintln(a.Out, "\nNo rclone-backup storages are configured. Start with 'pve-rclone-backup remote add'.")
				return nil
			}
			fmt.Fprintln(a.Out)
			rows := make([][]string, 0, len(storages))
			for _, s := range storages {
				rows = append(rows, []string{s.ID, s.Health, yesNo(s.Active), s.Remote + ":" + s.Path, s.Source,
					usageString(s.Usage), humanTime(s.LastResyncAt), kitString(s)})
			}
			a.table([]string{"STORAGE", "HEALTH", "ACTIVE", "LOCATION", "SOURCE", "USAGE", "RESYNCED", "KIT"}, rows)
			return nil
		},
	}
}

func usageString(u *apiv1.Usage) string {
	if u == nil || u.Used == nil {
		return "-"
	}
	if u.Total == nil || *u.Total == 0 {
		return humanBytes(*u.Used)
	}
	return humanBytes(*u.Used) + " / " + humanBytes(*u.Total)
}

func kitString(s apiv1.Storage) string {
	switch {
	case s.Encryption != "crypt":
		return "n/a"
	case s.KitConfirmed:
		return "confirmed"
	}
	return "MISSING"
}

// pluginPath is where PVE loads the storage plugin from.
var pluginPath = "/usr/share/perl5/PVE/Storage/Custom/RcloneBackupPlugin.pm"

type check struct {
	level, subject, message string
}

func (a *App) doctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the installation and report problems",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks := a.doctor(cmd.Context())
			bad := false
			for _, c := range checks {
				bad = bad || c.level != "ok"
			}
			if a.Output == "json" {
				out := make([]map[string]string, 0, len(checks))
				for _, c := range checks {
					out = append(out, map[string]string{"level": c.level, "subject": c.subject, "message": c.message})
				}
				if err := a.json(out); err != nil {
					return err
				}
			} else {
				rows := make([][]string, 0, len(checks))
				for _, c := range checks {
					rows = append(rows, []string{map[string]string{"ok": "OK", "warn": "WARN", "fail": "FAIL"}[c.level], c.subject, c.message})
				}
				a.table([]string{"", "CHECK", "DETAIL"}, rows)
			}
			if bad {
				return errFindings
			}
			return nil
		},
	}
}

func (a *App) doctor(ctx context.Context) []check {
	var out []check
	add := func(level, subject, format string, args ...any) {
		out = append(out, check{level, subject, fmt.Sprintf(format, args...)})
	}
	if _, err := os.Stat(pluginPath); err != nil {
		add("warn", "PVE plugin", "%s not found: PVE cannot list offsite backups", pluginPath)
	} else {
		add("ok", "PVE plugin", "installed")
	}
	v, err := a.api().Version(ctx)
	if err != nil {
		add("fail", "daemon", "%v", err)
		return out
	}
	if v.APIMinClient > apiv1.Revision || v.API < apiv1.Revision {
		add("fail", "daemon", "API %d does not support this CLI (API %d)", v.API, apiv1.Revision)
	} else {
		add("ok", "daemon", "%s (rclone %s) on %s", v.Daemon, v.Rclone, v.Node)
	}
	if st, err := a.api().Status(ctx); err == nil {
		for _, p := range st.Problems {
			add("warn", "daemon", "%s", p)
		}
		for _, al := range st.Alerts {
			add(map[bool]string{true: "fail", false: "warn"}[al.Severity == "error"], "alert "+al.ID, "%s", al.Message)
		}
		if st.Jobs.Failed > 0 {
			add("warn", "queue", "%d failed uploads; see 'pve-rclone-backup queue list --state failed'", st.Jobs.Failed)
		}
	}
	var storages []apiv1.Storage
	if err := a.do(ctx, http.MethodGet, "/v1/storages", nil, &storages); err == nil {
		for _, s := range storages {
			switch {
			case !s.Enabled:
			case s.Health != apiv1.HealthOK:
				add("fail", "storage "+s.ID, "%s: %s", s.Health, dash(s.HealthDetail))
			case s.Encryption == "crypt" && !s.KitConfirmed:
				add("fail", "storage "+s.ID, "no confirmed recovery kit; run 'pve-rclone-backup recovery-kit export --storage %s'", s.ID)
			default:
				add("ok", "storage "+s.ID, "healthy")
			}
		}
	}
	var remotes []apiv1.Remote
	if err := a.do(ctx, http.MethodGet, "/v1/remotes", nil, &remotes); err == nil {
		for _, r := range remotes {
			if !r.Supported || len(r.Storages) == 0 {
				continue
			}
			if r.Type == "onedrive" && !r.Authorized {
				add("fail", "remote "+r.Name, "not authorized; run 'pve-rclone-backup remote reconnect %s'", r.Name)
			} else {
				add("ok", "remote "+r.Name, "%s", r.Type)
			}
		}
	}
	return out
}
