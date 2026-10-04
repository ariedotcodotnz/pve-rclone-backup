// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
)

func (a *App) backupVerifyCommand() *cobra.Command {
	var (
		req   apiv1.VerifyRequest
		async bool
	)
	cmd := &cobra.Command{
		Use:   "verify <volid>",
		Short: "Verify an offsite backup",
		Long: `Verify an offsite backup.

Level 2 rechecks presence, sizes and provider checksums of the storage's backups without
downloading. Level 3 (default) downloads the archive, checks every digest and, when the tools are
installed, the archive's structure. Level 4 restores it into a scratch guest (--scratch-vmid) and
removes that guest again.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			storage, name, err := parseVolid(args[0])
			if err != nil {
				return err
			}
			if req.Level == 4 && req.ScratchVMID == 0 {
				return usagef("level 4 needs --scratch-vmid")
			}
			path := "/v1/storages/" + url.PathEscape(storage) + "/backups/" + url.PathEscape(name) + "/verify"
			if req.Level <= 2 {
				if err := a.do(cmd.Context(), http.MethodPost, path, req, nil); err != nil {
					return err
				}
				fmt.Fprintf(a.Out, "Listing verification of %s scheduled.\n", storage)
				return nil
			}
			var j apiv1.Job
			if err := a.do(cmd.Context(), http.MethodPost, path, req, &j); err != nil {
				return err
			}
			return a.finishJob(cmd.Context(), j, !async)
		},
	}
	cmd.Flags().IntVar(&req.Level, "level", 3, "verification level: 2, 3 or 4")
	cmd.Flags().IntVar(&req.ScratchVMID, "scratch-vmid", 0, "unused VMID for the level 4 restore test")
	cmd.Flags().StringVar(&req.ScratchStorage, "scratch-storage", "", "storage for the scratch guest's disks")
	cmd.Flags().BoolVar(&async, "no-wait", false, "return once the job is queued")
	return cmd
}

func (a *App) verifyCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "verify", Short: "Verification history"}
	var storage, volid string
	var limit int
	history := &cobra.Command{Use: "history", Short: "Show verification runs", Args: exactArgs(0), RunE: func(cmd *cobra.Command, _ []string) error {
		q := url.Values{"limit": {strconv.Itoa(limit)}}
		if volid != "" {
			s, name, err := parseVolid(volid)
			if err != nil {
				return err
			}
			storage = s
			q.Set("volname", "backup/"+name)
		}
		if storage != "" {
			q.Set("storage", storage)
		}
		var list []apiv1.Verification
		if err := a.do(cmd.Context(), http.MethodGet, "/v1/verifications?"+q.Encode(), nil, &list); err != nil {
			return err
		}
		if a.Output == "json" {
			return a.json(list)
		}
		rows := make([][]string, 0, len(list))
		for _, v := range list {
			var det struct {
				Structural string `json:"structural"`
				Error      string `json:"error"`
			}
			_ = json.Unmarshal(v.Details, &det)
			note := det.Error
			if note == "" && det.Structural != "" {
				note = "structure: " + det.Structural
			}
			rows = append(rows, []string{humanTime(&v.StartedAt), v.Storage + ":" + v.Volname, "L" + strconv.Itoa(v.Level), dash(v.Result), dash(note)})
		}
		a.table([]string{"STARTED", "BACKUP", "LEVEL", "RESULT", "NOTE"}, rows)
		return nil
	}}
	history.Flags().StringVar(&storage, "storage", "", "only this storage")
	history.Flags().StringVar(&volid, "volid", "", "only this backup")
	history.Flags().IntVar(&limit, "limit", 50, "maximum number of runs per storage")
	cmd.AddCommand(history)
	return cmd
}
