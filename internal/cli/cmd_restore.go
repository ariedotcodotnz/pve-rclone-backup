// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
)

// pollInterval is how often a waiting command checks its job.
var pollInterval = time.Second

// waitJob follows a job until it finishes, printing progress and history.
func (a *App) waitJob(ctx context.Context, id int64) error {
	seen := 0
	lastProgress := time.Time{}
	var lastRetry int64
	for {
		var d apiv1.JobDetail
		if err := a.do(context.WithoutCancel(ctx), http.MethodGet, fmt.Sprintf("/v1/jobs/%d", id), nil, &d); err != nil {
			return err
		}
		for ; seen < len(d.Events); seen++ {
			fmt.Fprintln(a.Err, "  "+d.Events[seen].Message)
		}
		switch d.State {
		case "complete":
			fmt.Fprintf(a.Out, "Job %d complete.\n", id)
			return nil
		case "failed", "cancelled", "source_lost":
			return fmt.Errorf("job %d %s: %s", id, d.State, dash(d.LastError))
		case "retry_wait":
			if d.NextAttemptAt != nil && d.NextAttemptAt.Unix() != lastRetry {
				lastRetry = d.NextAttemptAt.Unix()
				fmt.Fprintf(a.Err, "  waiting until %s: %s\n", humanTime(d.NextAttemptAt), d.LastError)
			}
		}
		if d.TotalBytes != nil && *d.TotalBytes > 0 && time.Since(lastProgress) >= 5*time.Second {
			lastProgress = time.Now()
			fmt.Fprintf(a.Err, "  %s: %s\n", d.State, progress(d.Job))
		}
		select {
		case <-ctx.Done():
			fmt.Fprintf(a.Err, "Stopped waiting; job %d continues in the background ('pve-rclone-backup queue cancel %d' stops it).\n", id, id)
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func (a *App) finishJob(ctx context.Context, j apiv1.Job, wait bool) error {
	if a.Output == "json" && !wait {
		return a.json(j)
	}
	fmt.Fprintf(a.Out, "Started job %d.\n", j.ID)
	if !wait {
		return nil
	}
	err := a.waitJob(ctx, j.ID)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (a *App) fetchCommand() *cobra.Command {
	var (
		target           string
		noProtect, async bool
	)
	cmd := &cobra.Command{
		Use:   "fetch <volid>",
		Short: "Download an offsite backup into a local backup storage",
		Long: `Download an offsite backup, verify it and place it in a local backup storage, where it appears
as a native backup that PVE can restore. It is protected by default so that the next local
prune does not remove it, and it is never uploaded again.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			storage, name, err := parseVolid(args[0])
			if err != nil {
				return err
			}
			if target == "" {
				return usagef("--to-storage is required")
			}
			protect := !noProtect
			var j apiv1.Job
			if err := a.do(cmd.Context(), http.MethodPost, "/v1/storages/"+url.PathEscape(storage)+"/backups/"+url.PathEscape(name)+"/fetch",
				apiv1.FetchRequest{TargetStorage: target, Protect: &protect}, &j); err != nil {
				return err
			}
			return a.finishJob(cmd.Context(), j, !async)
		},
	}
	cmd.Flags().StringVar(&target, "to-storage", "", "local backup storage to place the archive in")
	cmd.Flags().BoolVar(&noProtect, "no-protect", false, "do not protect the fetched backup")
	cmd.Flags().BoolVar(&async, "no-wait", false, "return once the job is queued")
	return cmd
}

func (a *App) restoreCommand() *cobra.Command {
	var (
		req   apiv1.RestoreRequest
		async bool
	)
	cmd := &cobra.Command{
		Use:   "restore <volid>",
		Short: "Restore a guest from an offsite backup",
		Long: `Restore a guest from an offsite backup.

VMs are streamed straight into qmrestore by default (mode stream, nothing is staged); a digest
mismatch at the end fails the restore and removes the new VM. Mode stage downloads and verifies
the archive first. Containers are always staged and restored with pct restore, which also works
for privileged containers.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			storage, name, err := parseVolid(args[0])
			if err != nil {
				return err
			}
			if req.TargetVMID == 0 {
				return usagef("--vmid is required")
			}
			if req.Mode == "auto" {
				req.Mode = ""
			}
			if req.Force {
				ok, err := a.confirm(fmt.Sprintf("Overwrite guest %d if it exists?", req.TargetVMID))
				if err != nil || !ok {
					return err
				}
			}
			req.Storage, req.Volname = storage, "backup/"+name
			var j apiv1.Job
			if err := a.do(cmd.Context(), http.MethodPost, "/v1/restores", req, &j); err != nil {
				return err
			}
			return a.finishJob(cmd.Context(), j, !async)
		},
	}
	f := cmd.Flags()
	f.IntVar(&req.TargetVMID, "vmid", 0, "VMID of the restored guest")
	f.StringVar(&req.TargetStorage, "target-storage", "", "storage for the guest's disks (default: as in the backup)")
	f.StringVar(&req.Mode, "mode", "auto", "auto, stream (VMs only) or stage")
	f.BoolVar(&req.Unique, "unique", false, "assign new unique MAC addresses and other identifiers")
	f.BoolVar(&req.Force, "force", false, "overwrite an existing guest with the same VMID")
	f.BoolVar(&req.AllowDamaged, "allow-damaged", false, "attempt to restore a backup with missing or altered segments")
	f.BoolVar(&async, "no-wait", false, "return once the job is queued")
	return cmd
}
