// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
)

func (a *App) queueCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "queue", Short: "Show and control the upload queue"}
	var states []string
	var storage string
	list := &cobra.Command{Use: "list", Short: "List jobs", Args: exactArgs(0), RunE: func(cmd *cobra.Command, _ []string) error {
		q := url.Values{}
		if len(states) > 0 {
			q.Set("state", strings.Join(states, ","))
		}
		if storage != "" {
			q.Set("storage", storage)
		}
		var jobs []apiv1.Job
		if err := a.do(cmd.Context(), http.MethodGet, "/v1/jobs?"+q.Encode(), nil, &jobs); err != nil {
			return err
		}
		if a.Output == "json" {
			return a.json(jobs)
		}
		rows := make([][]string, 0, len(jobs))
		for _, j := range jobs {
			guest := "-"
			if j.VMID != 0 {
				guest = fmt.Sprintf("%s/%d", j.VMType, j.VMID)
			}
			note := j.LastError
			if j.State == "retry_wait" && j.NextAttemptAt != nil {
				note = "next " + humanTime(j.NextAttemptAt) + ": " + note
			}
			rows = append(rows, []string{strconv.FormatInt(j.ID, 10), j.State, j.Storage, guest, progress(j), strconv.Itoa(j.Attempts), dash(note)})
		}
		a.table([]string{"ID", "STATE", "STORAGE", "GUEST", "PROGRESS", "TRIES", "NOTE"}, rows)
		return nil
	}}
	list.Flags().StringSliceVar(&states, "state", nil, "only jobs in these states (comma separated)")
	list.Flags().StringVar(&storage, "storage", "", "only jobs of this storage")

	var scanStorage string
	scan := &cobra.Command{Use: "scan", Short: "Look for new archives now", Args: exactArgs(0), RunE: func(cmd *cobra.Command, _ []string) error {
		var sum apiv1.ScanSummary
		if err := a.do(cmd.Context(), http.MethodPost, "/v1/discovery/scan", map[string]string{"storage": scanStorage}, &sum); err != nil {
			return err
		}
		if a.Output == "json" {
			return a.json(sum)
		}
		fmt.Fprintf(a.Out, "Scanned %d sources, %d archives: %d queued, %d skipped, %d superseded, %d already known.\n",
			sum.Sources, sum.Archives, sum.Queued, sum.Skipped, sum.Superseded, sum.Known)
		for _, p := range sum.Problems {
			fmt.Fprintln(a.Out, "Problem:", p)
		}
		return nil
	}}
	scan.Flags().StringVar(&scanStorage, "storage", "", "only this source or offsite storage")

	action := func(use, short, path string) *cobra.Command {
		return &cobra.Command{Use: use + " <job>", Short: short, Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			id, err := jobID(args[0])
			if err != nil {
				return err
			}
			var j apiv1.Job
			if err := a.do(cmd.Context(), http.MethodPost, fmt.Sprintf("/v1/jobs/%d/%s", id, path), nil, &j); err != nil {
				return err
			}
			if a.Output == "json" {
				return a.json(j)
			}
			fmt.Fprintf(a.Out, "Job %d is %s.\n", j.ID, j.State)
			return nil
		}}
	}
	cmd.AddCommand(list, scan,
		&cobra.Command{Use: "show <job>", Short: "Show a job and its history", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			id, err := jobID(args[0])
			if err != nil {
				return err
			}
			var d apiv1.JobDetail
			if err := a.do(cmd.Context(), http.MethodGet, fmt.Sprintf("/v1/jobs/%d", id), nil, &d); err != nil {
				return err
			}
			if a.Output == "json" {
				return a.json(d)
			}
			fmt.Fprintf(a.Out, "Job:       %d (%s)\nState:     %s\nStorage:   %s\nBackup:    %s\nSource:    %s\nProgress:  %s, segment %d of %d\nAttempts:  %d\n",
				d.ID, d.Kind, d.State, d.Storage, dash(d.Volname), dash(d.SourcePath), progress(d.Job), d.NextSegment, len(d.Segments), d.Attempts)
			if d.LastError != "" {
				fmt.Fprintf(a.Out, "Error:     %s (%s)\n", d.LastError, dash(d.ErrorClass))
			}
			fmt.Fprintln(a.Out, "\nHistory:")
			for _, e := range d.Events {
				change := ""
				if e.ToState != "" && e.FromState != e.ToState {
					change = fmt.Sprintf("%s -> %s: ", dash(e.FromState), e.ToState)
				}
				fmt.Fprintf(a.Out, "  %s %-5s %s%s\n", humanTime(&e.Time), e.Level, change, clean(e.Message))
			}
			return nil
		}},
		action("retry", "Queue a failed, cancelled or skipped job again", "retry"),
		action("cancel", "Cancel a job", "cancel"),
		&cobra.Command{Use: "priority <job> <priority>", Short: "Change a job's priority (-100..100, higher first)", Args: exactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
			id, err := jobID(args[0])
			if err != nil {
				return err
			}
			p, err := strconv.Atoi(args[1])
			if err != nil {
				return usagef("priority must be a number")
			}
			return a.do(cmd.Context(), http.MethodPost, fmt.Sprintf("/v1/jobs/%d/priority", id), map[string]int{"priority": p}, nil)
		}},
	)
	return cmd
}

func jobID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id < 1 {
		return 0, usagef("%q is not a job ID", s)
	}
	return id, nil
}

func progress(j apiv1.Job) string {
	if j.TotalBytes == nil || *j.TotalBytes == 0 {
		return "-"
	}
	return fmt.Sprintf("%d%% of %s", j.ProgressBytes*100 / *j.TotalBytes, humanBytes(*j.TotalBytes))
}
