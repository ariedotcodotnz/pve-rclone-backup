// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/client"
)

func (a *App) recoverCommand() *cobra.Command {
	var (
		passFile, suffix     string
		importToken, noPVESH bool
	)
	cmd := &cobra.Command{
		Use:   "recover <kit-file>",
		Short: "Disaster recovery: import a recovery kit and add its storages read-only",
		Long: `Import the keys (and with --import-token the credentials) from a recovery kit, then add a
read-only storage for each repository in it, named after the original storage with a suffix.
Their backups can then be listed and restored; nothing is uploaded into them.

See /usr/share/doc/pve-rclone-backup/dr-runbook.md.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			text, err := a.readKit(args[0])
			if err != nil {
				return err
			}
			pass, err := readPassphrase(passFile)
			if err != nil {
				return err
			}
			var results []apiv1.KitRepoResult
			if err := a.do(ctx, http.MethodPost, "/v1/recovery-kit/import", apiv1.KitRequest{Kit: text, Passphrase: pass,
				ImportToken: importToken}, &results); err != nil {
				return err
			}
			problems := false
			for _, r := range results {
				if !r.OK {
					fmt.Fprintf(a.Out, "Repository %s: import failed: %s\n", r.RepoUUID, r.Error)
					problems = true
					continue
				}
				fmt.Fprintf(a.Out, "Repository %s (%s:%s, source %s): keys %s, remote %s.\n", r.RepoUUID, r.Remote, r.Path,
					r.Source, dash(r.Keys), dash(r.RemoteConfig))
				id := r.Storage
				if id == "" {
					id = "offsite"
				}
				id += suffix
				err := a.do(ctx, http.MethodGet, "/v1/storages/"+url.PathEscape(id), nil, nil)
				if err == nil {
					fmt.Fprintf(a.Out, "Storage %s exists already; skipped.\n", id)
					continue
				}
				if !client.IsCode(err, apiv1.CodeNotFound) {
					return err
				}
				if err := a.initStorage(ctx, id, initOpts{remote: r.Remote, path: r.Path, source: r.Source,
					encryption: r.Encryption, readOnly: true, noPVESH: noPVESH}); err != nil {
					fmt.Fprintf(a.Out, "Storage %s not added: %v\n", id, err)
					if r.RemoteConfig != "exists" && r.RemoteConfig != "created" {
						fmt.Fprintf(a.Out, "  Connect the remote first ('pve-rclone-backup remote add %s'), then run recover again.\n", r.Remote)
					}
					problems = true
				}
			}
			fmt.Fprintln(a.Out, "\nNext: 'pve-rclone-backup backup list' shows the offsite backups once the catalogues are rebuilt,")
			fmt.Fprintln(a.Out, "and 'pve-rclone-backup restore <volid> --vmid <id>' restores a guest.")
			if problems {
				return errFindings
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&passFile, "passphrase-file", "", "file holding the kit passphrase")
	cmd.Flags().BoolVar(&importToken, "import-token", false, "create missing remotes from credentials in the kit")
	cmd.Flags().StringVar(&suffix, "suffix", "-dr", "appended to the original storage names")
	cmd.Flags().BoolVar(&noPVESH, "no-pvesh", false, "print the pvesh commands instead of running them")
	return cmd
}
