// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
)

func (a *App) printPrune(entries []apiv1.PruneEntry) error {
	if a.Output == "json" {
		return a.json(entries)
	}
	rows := make([][]string, 0, len(entries))
	remove := 0
	for _, e := range entries {
		if e.Mark == "remove" {
			remove++
		}
		rows = append(rows, []string{e.Volid, humanTime(unixTime(e.CTime)), e.Mark, dash(e.Reason)})
	}
	a.table([]string{"BACKUP", "TIME", "MARK", "REASON"}, rows)
	fmt.Fprintf(a.Out, "\n%d of %d backups marked for removal.\n", remove, len(entries))
	return nil
}

func (a *App) retentionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "retention",
		Short: "Preview and apply offsite retention (the storage's prune-backups)",
		Long: `Offsite retention uses the storage's prune-backups setting, marked exactly like PVE's own prune,
with safety rails: rclone-min-age, rclone-keep-min, keeping the newest good copy while a newer backup
is damaged, and rclone-max-deletes per run. Removed backups are tombstoned and deleted after
rclone-delete-grace; until then 'backup undelete' restores them.`,
	}
	var vmid int
	var vmtype string
	preview := &cobra.Command{Use: "preview <storage>", Short: "Show what retention would remove", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		req := apiv1.PruneRequest{Type: vmtype, DryRun: true}
		if vmid != 0 {
			req.VMID = &vmid
		}
		var entries []apiv1.PruneEntry
		if err := a.do(cmd.Context(), http.MethodPost, "/v1/storages/"+url.PathEscape(args[0])+"/prune", req, &entries); err != nil {
			return err
		}
		return a.printPrune(entries)
	}}
	preview.Flags().IntVar(&vmid, "vmid", 0, "only this guest")
	preview.Flags().StringVar(&vmtype, "type", "", "only qemu or lxc guests")
	run := &cobra.Command{Use: "run <storage>", Short: "Apply retention now and delete backups whose grace period ended", Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ok, err := a.confirm(fmt.Sprintf("Apply retention to %s now?", args[0]))
			if err != nil || !ok {
				return err
			}
			var entries []apiv1.PruneEntry
			if err := a.do(cmd.Context(), http.MethodPost, "/v1/storages/"+url.PathEscape(args[0])+"/retention", nil, &entries); err != nil {
				return err
			}
			return a.printPrune(entries)
		}}
	cmd.AddCommand(preview, run)
	return cmd
}

// backupEditCommands are delete, undelete, protect, unprotect and notes.
func (a *App) backupEditCommands() []*cobra.Command {
	path := func(volid string) (string, error) {
		storage, name, err := parseVolid(volid)
		if err != nil {
			return "", err
		}
		return "/v1/storages/" + url.PathEscape(storage) + "/backups/" + url.PathEscape(name), nil
	}
	show := func(b apiv1.Backup) error {
		if a.Output == "json" {
			return a.json(b)
		}
		msg := fmt.Sprintf("%s: %s", b.Volname, b.State)
		if b.DeleteAfter != nil {
			msg += ", deleted after " + humanTime(b.DeleteAfter)
		}
		if b.Protected {
			msg += ", protected"
		}
		fmt.Fprintln(a.Out, msg)
		return nil
	}
	patch := func(use, short string, upd func() apiv1.BackupUpdate) *cobra.Command {
		return &cobra.Command{Use: use + " <volid>", Short: short, Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			p, err := path(args[0])
			if err != nil {
				return err
			}
			var b apiv1.Backup
			if err := a.do(cmd.Context(), http.MethodPatch, p, upd(), &b); err != nil {
				return err
			}
			return show(b)
		}}
	}
	t, f := true, false
	var notes string
	notesCmd := &cobra.Command{Use: "notes <volid>", Short: "Show or (with --set) change a backup's notes", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		p, err := path(args[0])
		if err != nil {
			return err
		}
		if !cmd.Flags().Changed("set") {
			var d apiv1.BackupDetail
			if err := a.do(cmd.Context(), http.MethodGet, p, nil, &d); err != nil {
				return err
			}
			fmt.Fprintln(a.Out, d.Notes)
			return nil
		}
		var b apiv1.Backup
		if err := a.do(cmd.Context(), http.MethodPatch, p, apiv1.BackupUpdate{Notes: &notes}, &b); err != nil {
			return err
		}
		return show(b)
	}}
	notesCmd.Flags().StringVar(&notes, "set", "", "new notes")
	return []*cobra.Command{
		{Use: "delete <volid>", Short: "Mark an offsite backup for deletion (after rclone-delete-grace)", Args: exactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				p, err := path(args[0])
				if err != nil {
					return err
				}
				ok, err := a.confirm(fmt.Sprintf("Delete %s after the grace period?", args[0]))
				if err != nil || !ok {
					return err
				}
				var b apiv1.Backup
				if err := a.do(cmd.Context(), http.MethodDelete, p, nil, &b); err != nil {
					return err
				}
				return show(b)
			}},
		{Use: "undelete <volid>", Short: "Keep a backup marked for deletion", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			p, err := path(args[0])
			if err != nil {
				return err
			}
			var b apiv1.Backup
			if err := a.do(cmd.Context(), http.MethodPost, p+"/undelete", nil, &b); err != nil {
				return err
			}
			return show(b)
		}},
		patch("protect", "Protect a backup from deletion and retention", func() apiv1.BackupUpdate { return apiv1.BackupUpdate{Protected: &t} }),
		patch("unprotect", "Remove a backup's protection", func() apiv1.BackupUpdate { return apiv1.BackupUpdate{Protected: &f} }),
		notesCmd,
	}
}
