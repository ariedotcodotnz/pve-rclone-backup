// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/doctor"
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

func (a *App) doctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the installation and report problems",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			checks := doctor.Run(cmd.Context(), a.api())
			if a.Output == "json" {
				if err := a.json(checks); err != nil {
					return err
				}
			} else {
				rows := make([][]string, 0, len(checks))
				for _, c := range checks {
					rows = append(rows, []string{map[string]string{doctor.OK: "OK", doctor.Warn: "WARN", doctor.Fail: "FAIL"}[c.Level], c.Subject, c.Message})
				}
				a.table([]string{"", "CHECK", "DETAIL"}, rows)
			}
			if !doctor.Healthy(checks) {
				return errFindings
			}
			return nil
		},
	}
}
