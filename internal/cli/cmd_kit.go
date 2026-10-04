// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/spf13/cobra"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/recoverykit"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

func readPassphrase(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // the operator names the file
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

func (a *App) readKit(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // the operator names the file
	return string(b), err
}

func (a *App) kitCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "recovery-kit", Short: "Export, confirm, verify and import recovery kits"}

	var storages []string
	var kf kitFlags
	export := &cobra.Command{Use: "export", Short: "Export a recovery kit (all storages by default)", Args: exactArgs(0), RunE: func(cmd *cobra.Command, _ []string) error {
		if kf.file == "" {
			return usagef("--kit-file is required")
		}
		var targets []apiv1.KitTarget
		for _, s := range storages {
			targets = append(targets, apiv1.KitTarget{Storage: s})
		}
		return a.exportAndConfirm(cmd.Context(), targets, kf)
	}}
	export.Flags().StringSliceVar(&storages, "storage", nil, "storages to include (comma separated; default all)")
	kf.register(export, "where to write the kit")

	confirm := &cobra.Command{Use: "confirm <checksum>", Short: "Confirm an exported kit by its checksum", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		var res apiv1.KitConfirmResponse
		if err := a.do(cmd.Context(), http.MethodPost, "/v1/recovery-kit/confirm", apiv1.KitConfirmRequest{Checksum: args[0]}, &res); err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "Confirmed the kit of %d repositories.\n", len(res.Repos))
		return nil
	}}

	var passFile string
	var importToken bool
	verify := &cobra.Command{Use: "verify <file>", Short: "Check a kit against the live repositories", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.kitAction(cmd, args[0], passFile, "/v1/recovery-kit/verify", false)
	}}
	verify.Flags().StringVar(&passFile, "passphrase-file", "", "file holding the kit passphrase")
	imp := &cobra.Command{Use: "import <file>", Short: "Import keys (and optionally credentials) from a kit", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return a.kitAction(cmd, args[0], passFile, "/v1/recovery-kit/import", importToken)
	}}
	imp.Flags().StringVar(&passFile, "passphrase-file", "", "file holding the kit passphrase")
	imp.Flags().BoolVar(&importToken, "import-token", false, "create missing remotes from credentials in the kit")

	status := &cobra.Command{Use: "status", Short: "Show which repositories have a confirmed kit", Args: exactArgs(0), RunE: func(cmd *cobra.Command, _ []string) error {
		var list []apiv1.KitStatus
		if err := a.do(cmd.Context(), http.MethodGet, "/v1/recovery-kit/status", nil, &list); err != nil {
			return err
		}
		if a.Output == "json" {
			return a.json(list)
		}
		rows := make([][]string, 0, len(list))
		for _, k := range list {
			rows = append(rows, []string{k.RepoUUID, dash(strings.Join(k.Storages, ",")), yesNo(k.Encrypted), humanTime(k.ExportedAt), humanTime(k.ConfirmedAt)})
		}
		a.table([]string{"REPOSITORY", "STORAGES", "ENCRYPTED", "EXPORTED", "CONFIRMED"}, rows)
		return nil
	}}

	var showSecrets bool
	show := &cobra.Command{
		Use:   "show <file>",
		Short: "Show a kit's contents (works without the daemon)",
		Long:  "Show a kit's repositories. With --show-secrets, print an rclone configuration for manual recovery with stock rclone.",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			text, err := a.readKit(args[0])
			if err != nil {
				return err
			}
			pass, err := readPassphrase(passFile)
			if err != nil {
				return err
			}
			kit, err := recoverykit.Decode(text, pass)
			if err != nil {
				return err
			}
			return a.showKit(kit, showSecrets)
		},
	}
	show.Flags().StringVar(&passFile, "passphrase-file", "", "file holding the kit passphrase")
	show.Flags().BoolVar(&showSecrets, "show-secrets", false, "print keys and credentials as an rclone configuration")
	cmd.AddCommand(export, confirm, verify, imp, status, show)
	return cmd
}

func (a *App) kitAction(cmd *cobra.Command, file, passFile, path string, importToken bool) error {
	text, err := a.readKit(file)
	if err != nil {
		return err
	}
	pass, err := readPassphrase(passFile)
	if err != nil {
		return err
	}
	var res []apiv1.KitRepoResult
	if err := a.do(cmd.Context(), http.MethodPost, path, apiv1.KitRequest{Kit: text, Passphrase: pass, ImportToken: importToken}, &res); err != nil {
		return err
	}
	if a.Output == "json" {
		return a.json(res)
	}
	rows := make([][]string, 0, len(res))
	bad := false
	for _, r := range res {
		status := "OK"
		if !r.OK {
			status, bad = "FAIL: "+r.Error, true
		}
		rows = append(rows, []string{r.RepoUUID, dash(r.Storage), r.Remote + ":" + r.Path, r.Source, dash(r.Keys), dash(r.RemoteConfig), status})
	}
	a.table([]string{"REPOSITORY", "STORAGE", "LOCATION", "SOURCE", "KEYS", "REMOTE", "RESULT"}, rows)
	if bad {
		return errFindings
	}
	return nil
}

func (a *App) showKit(kit *recoverykit.Kit, secrets bool) error {
	fmt.Fprintf(a.Out, "Recovery kit created %s on %s\n", humanTime(&kit.CreatedAt), kit.CreatedOn)
	for _, r := range kit.Repos {
		fmt.Fprintf(a.Out, "\nRepository %s (storage %s)\n  Location:   %s:%s\n  Source:     %s\n  Encryption: %s\n  Credentials included: %s\n",
			r.RepoUUID, dash(r.StorageID), r.Remote, r.Path, r.Source, r.Encryption, yesNo(len(r.RemoteConfig) > 0))
		if r.Keys != nil {
			for _, g := range r.Keys.Generations {
				fmt.Fprintf(a.Out, "  Key generation %d (%s): %s names, %s encoding, suffix %s\n", g.ID, g.State, g.FilenameEncryption,
					g.FilenameEncoding, g.Suffix)
			}
		}
	}
	if !secrets {
		fmt.Fprintln(a.Out, "\nUse --show-secrets to print an rclone configuration for manual recovery.")
		return nil
	}
	fmt.Fprintln(a.Out, "\n# rclone configuration for manual recovery. Keep it secret.")
	for _, r := range kit.Repos {
		if len(r.RemoteConfig) > 0 {
			fmt.Fprintf(a.Out, "\n[%s]\n", r.Remote)
			for k, v := range r.RemoteConfig {
				fmt.Fprintf(a.Out, "%s = %s\n", k, v)
			}
		}
		if r.Keys == nil {
			continue
		}
		for _, g := range r.Keys.Generations {
			pw, err := obscure.Obscure(g.Password)
			if err != nil {
				return err
			}
			pw2, err := obscure.Obscure(g.Password2)
			if err != nil {
				return err
			}
			fmt.Fprintf(a.Out, "\n[%s-g%d]\ntype = crypt\nremote = %s:%s/%s\nfilename_encryption = %s\ndirectory_name_encryption = %t\nfilename_encoding = %s\nsuffix = %s\npassword = %s\npassword2 = %s\n",
				r.Remote, g.ID, r.Remote, r.Path, transport.CryptRoot(g.ID), g.FilenameEncryption, g.DirectoryNameEncryption,
				g.FilenameEncoding, g.Suffix, pw, pw2)
		}
		fmt.Fprintf(a.Out, "\n# Backups of source %s: %s-g1:v1/%s/<qemu|lxc>/<vmid>/<time>/\n# Reassemble an archive: rclone cat %s-g1:v1/%s/qemu/100/<time>/part.* > vzdump-qemu-100-<time>.vma.zst\n",
			r.Source, r.Remote, r.Source, r.Remote, r.Source)
	}
	return nil
}
