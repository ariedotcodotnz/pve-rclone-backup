// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/manifest"
)

// parseVolid splits "<storage>:backup/<archive>".
func parseVolid(volid string) (storage, name string, err error) {
	storage, vol, ok := strings.Cut(volid, ":")
	name, isBackup := strings.CutPrefix(vol, "backup/")
	if !ok || storage == "" || !isBackup || name == "" || strings.Contains(name, "/") {
		return "", "", usagef("%q is not a backup volume ID (<storage>:backup/<archive>)", volid)
	}
	return storage, name, nil
}

func (a *App) backupCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "backup", Short: "List and inspect offsite backups"}
	var (
		storage, vmtype, state string
		vmid                   int
	)
	list := &cobra.Command{Use: "list", Short: "List offsite backups", Args: exactArgs(0), RunE: func(cmd *cobra.Command, _ []string) error {
		ids := []string{storage}
		if storage == "" {
			var storages []apiv1.Storage
			if err := a.do(cmd.Context(), http.MethodGet, "/v1/storages", nil, &storages); err != nil {
				return err
			}
			ids = ids[:0]
			for _, s := range storages {
				ids = append(ids, s.ID)
			}
		}
		q := url.Values{}
		if vmid != 0 {
			q.Set("vmid", strconv.Itoa(vmid))
		}
		if vmtype != "" {
			q.Set("type", vmtype)
		}
		if state != "" {
			q.Set("state", state)
		}
		type row struct {
			Storage string `json:"storage"`
			apiv1.Backup
		}
		var all []row
		for _, id := range ids {
			var list []apiv1.Backup
			if err := a.do(cmd.Context(), http.MethodGet, "/v1/storages/"+url.PathEscape(id)+"/backups?"+q.Encode(), nil, &list); err != nil {
				return err
			}
			for _, b := range list {
				all = append(all, row{id, b})
			}
		}
		if a.Output == "json" {
			return a.json(all)
		}
		rows := make([][]string, 0, len(all))
		for _, r := range all {
			rows = append(rows, []string{r.Storage + ":" + r.Volname, r.State, humanBytes(r.Size), humanTime(unixTime(r.CTime)),
				fmt.Sprintf("L%d %s", r.VerifyLevel, humanTime(r.VerifiedAt)), yesNo(r.Protected), dash(r.Notes)})
		}
		a.table([]string{"VOLID", "STATE", "SIZE", "TIME", "VERIFIED", "PROTECTED", "NOTES"}, rows)
		return nil
	}}
	list.Flags().StringVar(&storage, "storage", "", "only this storage")
	list.Flags().IntVar(&vmid, "vmid", 0, "only this guest")
	list.Flags().StringVar(&vmtype, "type", "", "only qemu or lxc guests")
	list.Flags().StringVar(&state, "state", "", "states (comma separated, or 'all'; default complete,damaged)")

	inspect := &cobra.Command{Use: "inspect <volid>", Short: "Show a backup's manifest summary", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		storage, name, err := parseVolid(args[0])
		if err != nil {
			return err
		}
		var d apiv1.BackupDetail
		if err := a.do(cmd.Context(), http.MethodGet, "/v1/storages/"+url.PathEscape(storage)+"/backups/"+url.PathEscape(name), nil, &d); err != nil {
			return err
		}
		if a.Output == "json" {
			return a.json(d)
		}
		var m manifest.Manifest
		if err := json.Unmarshal(d.Manifest, &m); err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "Backup:      %s:%s\nState:       %s\nGuest:       %s %d %s\nTime:        %s\nArchive:     %s, %s, sha256 %s\nSegments:    %d of %s\nUploaded:    %s from %s/%s\nVerified:    level %d %s %s\nProtected:   %s\nNotes:       %s\nRepository:  %s generation %d\n",
			storage, d.Volname, d.State, d.VMType, d.VMID, clean(d.GuestName), humanTime(unixTime(d.CTime)),
			m.Archive.Filename, humanBytes(m.Archive.Size), m.Archive.SHA256, m.Segments.Count, humanBytes(m.Segments.Size),
			humanTime(&m.UploadedAt), m.Archive.SourceNode, m.Archive.SourceStorage, d.VerifyLevel, humanTime(d.VerifiedAt),
			d.VerifyResult, yesNo(d.Protected), dash(clean(d.Notes)), m.RepoUUID, m.Generation)
		if d.DeleteAfter != nil {
			fmt.Fprintln(a.Out, "Deletion:    scheduled after", humanTime(d.DeleteAfter))
		}
		return nil
	}}
	cmd.AddCommand(list, inspect)
	return cmd
}
