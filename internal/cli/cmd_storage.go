// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
)

func (a *App) storageCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "storage", Short: "Manage offsite (rclone-backup) storages"}
	cmd.AddCommand(
		&cobra.Command{Use: "list", Short: "List offsite storages", Args: exactArgs(0), RunE: func(cmd *cobra.Command, _ []string) error {
			var list []apiv1.Storage
			if err := a.do(cmd.Context(), http.MethodGet, "/v1/storages", nil, &list); err != nil {
				return err
			}
			if a.Output == "json" {
				return a.json(list)
			}
			rows := make([][]string, 0, len(list))
			for _, s := range list {
				rows = append(rows, []string{s.ID, s.Health, yesNo(s.Active), s.Remote + ":" + s.Path, s.Source, s.Encryption,
					dash(strings.Join(s.ReplicateFrom, ",")), kitString(s)})
			}
			a.table([]string{"STORAGE", "HEALTH", "ACTIVE", "LOCATION", "SOURCE", "ENCRYPTION", "REPLICATES", "KIT"}, rows)
			return nil
		}},
		&cobra.Command{Use: "show <storage>", Short: "Show an offsite storage", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			var s apiv1.Storage
			if err := a.do(cmd.Context(), http.MethodGet, "/v1/storages/"+url.PathEscape(args[0]), nil, &s); err != nil {
				return err
			}
			if a.Output == "json" {
				return a.json(s)
			}
			fmt.Fprintf(a.Out, "Storage:      %s\nHealth:       %s %s\nActive:       %s\nLocation:     %s:%s\nSource:       %s\nEncryption:   %s\nRepository:   %s (generation %d)\nReplicates:   %s\nResynced:     %s\nUsage:        %s\nRecovery kit: %s\n",
				s.ID, s.Health, s.HealthDetail, yesNo(s.Active), s.Remote, s.Path, s.Source, s.Encryption, dash(s.RepoUUID), s.Generation,
				dash(strings.Join(s.ReplicateFrom, ", ")), humanTime(s.LastResyncAt), usageString(s.Usage), kitString(s))
			if s.ConfigError != "" {
				fmt.Fprintln(a.Out, "Config error:", s.ConfigError)
			}
			return nil
		}},
		a.storageInitCommand(),
		&cobra.Command{
			Use:   "set <storage> <property>=<value>...",
			Short: "Change storage properties (through the PVE API)",
			Args:  minArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				pv := []string{"set", "/storage/" + args[0]}
				for _, kv := range args[1:] {
					k, v, ok := strings.Cut(kv, "=")
					if !ok || k == "" || strings.HasPrefix(k, "-") {
						return usagef("%q is not PROPERTY=VALUE", kv)
					}
					pv = append(pv, "--"+k, v)
				}
				return a.pvesh(cmd.Context(), false, pv...)
			},
		},
		&cobra.Command{Use: "resync <storage>", Short: "Rebuild the catalogue from the remote", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.do(cmd.Context(), http.MethodPost, "/v1/storages/"+url.PathEscape(args[0])+"/resync", nil, nil); err != nil {
				return err
			}
			fmt.Fprintln(a.Out, "Catalogue resync scheduled.")
			return nil
		}},
	)
	return cmd
}

// pvesh runs (or with print only shows) a pvesh command.
func (a *App) pvesh(ctx context.Context, print bool, args ...string) error {
	if print {
		fmt.Fprintln(a.Out, "pvesh "+strings.Join(quoteAll(args), " "))
		return nil
	}
	out, err := a.PVESH(ctx, args...)
	if err != nil {
		return fmt.Errorf("pvesh %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return nil
}

func quoteAll(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t'\"$\\;&|<>*?()") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		out[i] = a
	}
	return out
}

type kitFlags struct {
	file, passphraseFile, confirm string
	includeToken, force           bool
}

func (f *kitFlags) register(cmd *cobra.Command, fileUsage string) {
	cmd.Flags().StringVar(&f.file, "kit-file", "", fileUsage)
	cmd.Flags().StringVar(&f.passphraseFile, "kit-passphrase-file", "", "encrypt the kit with the passphrase in this file")
	cmd.Flags().BoolVar(&f.includeToken, "include-token", false, "include the remote's credentials (OAuth token) in the kit")
	cmd.Flags().StringVar(&f.confirm, "confirm-checksum", "", "confirm the kit non-interactively with its checksum")
	cmd.Flags().BoolVar(&f.force, "force", false, "overwrite an existing kit file")
}

// initOpts configures storage initialization.
type initOpts struct {
	remote, path, source, encryption string
	replicateFrom                    []string
	adoptSource, readOnly, noPVESH   bool
	kf                               kitFlags
}

// initStorage creates or adopts a storage's repository, has its recovery
// kit exported and confirmed when needed, and adds the storage to PVE.
func (a *App) initStorage(ctx context.Context, id string, o initOpts) error {
	if o.readOnly && len(o.replicateFrom) > 0 {
		return usagef("a read-only storage does not replicate")
	}
	var res apiv1.StorageInitResponse
	if err := a.do(ctx, http.MethodPost, "/v1/storages/"+url.PathEscape(id)+"/init", apiv1.StorageInitRequest{
		Remote: o.remote, Path: o.path, Source: o.source, Encryption: o.encryption, AdoptSource: o.adoptSource,
		ReadOnly: o.readOnly}, &res); err != nil {
		return err
	}
	verb := "Created"
	if !res.Created {
		verb = "Adopted existing"
	}
	fmt.Fprintf(a.Out, "%s repository %s at %s:%s.\n", verb, res.RepoUUID, o.remote, o.path)
	if res.KitRequired {
		target := apiv1.KitTarget{Storage: id, Remote: o.remote, Path: o.path, Source: o.source}
		if o.kf.file == "" {
			o.kf.file = id + "-recovery-kit.txt"
		}
		if err := a.exportAndConfirm(ctx, []apiv1.KitTarget{target}, o.kf); err != nil {
			return err
		}
	}
	pv := []string{"create", "/storage", "--storage", id, "--type", "rclone-backup", "--rclone-remote", o.remote,
		"--rclone-path", o.path, "--rclone-source", o.source, "--rclone-encryption", res.Encryption, "--content", "backup"}
	if len(o.replicateFrom) > 0 {
		pv = append(pv, "--rclone-replicate-from", strings.Join(o.replicateFrom, ","))
	}
	if o.readOnly {
		// Uploads are already refused (the source belongs to another
		// installation); the daemon also refuses every deletion and prune
		// of an immutable storage.
		pv = append(pv, "--rclone-immutable", "1")
	}
	if o.noPVESH {
		fmt.Fprintln(a.Out, "Add the storage with:")
		return a.pvesh(ctx, true, pv...)
	}
	if err := a.pvesh(ctx, false, pv...); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "Storage %s added.\n", id)
	return nil
}

func (a *App) storageInitCommand() *cobra.Command {
	var o initOpts
	cmd := &cobra.Command{
		Use:   "init <storage>",
		Short: "Create the repository, export its recovery kit and add the storage to PVE",
		Long: `Create (or adopt) the offsite repository for a new storage, export and confirm its recovery
kit, then add the storage to /etc/pve/storage.cfg through the PVE API.

The recovery kit holds the encryption keys. Without it, encrypted backups cannot be restored
after the loss of this host; replication only starts once the kit is confirmed.

With --read-only, an existing repository is bound to browse and restore the backups of another
installation (its --source): the storage replicates nothing and is immutable, so nothing in it can be
deleted or pruned.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if o.remote == "" || o.source == "" {
				return usagef("--remote and --source are required")
			}
			return a.initStorage(cmd.Context(), args[0], o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.remote, "remote", "", "transport remote (see 'remote list')")
	f.StringVar(&o.path, "path", "pve-backups", "repository path inside the remote")
	f.StringVar(&o.source, "source", "", "name of this installation inside the repository (e.g. the cluster name)")
	f.StringVar(&o.encryption, "encryption", "crypt", "crypt or none")
	f.StringSliceVar(&o.replicateFrom, "replicate-from", nil, "local backup storages to replicate (comma separated)")
	f.BoolVar(&o.adoptSource, "adopt-source", false, "take over a source name registered by another installation (disaster recovery)")
	f.BoolVar(&o.readOnly, "read-only", false, "only browse and restore an existing repository's backups of --source")
	f.BoolVar(&o.noPVESH, "no-pvesh", false, "print the pvesh command instead of running it")
	o.kf.register(cmd, "where to write the recovery kit (default <storage>-recovery-kit.txt)")
	return cmd
}

// checksumShape shows the form of a checksum, not its value: typing it
// back is the confirmation.
func checksumShape(sum string) string {
	return strings.Map(func(r rune) rune {
		if r == '-' {
			return r
		}
		return 'x'
	}, sum)
}

// exportAndConfirm exports a kit to a file and confirms it by checksum.
func (a *App) exportAndConfirm(ctx context.Context, targets []apiv1.KitTarget, kf kitFlags) error {
	pass := ""
	if kf.passphraseFile != "" {
		b, err := os.ReadFile(kf.passphraseFile)
		if err != nil {
			return err
		}
		pass = strings.TrimRight(string(b), "\r\n")
	}
	var kit apiv1.KitExportResponse
	if err := a.do(ctx, http.MethodPost, "/v1/recovery-kit/export",
		apiv1.KitExportRequest{Targets: targets, IncludeToken: kf.includeToken, Passphrase: pass}, &kit); err != nil {
		return err
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if kf.force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(filepath.Clean(kf.file), flags, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s exists; choose another --kit-file or pass --force", kf.file)
	}
	if err != nil {
		return err
	}
	if _, err := f.WriteString(kit.Kit); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Fprintf(a.Out, "\nRecovery kit written to %s (checksum %s).\n", kf.file, kit.Checksum)
	fmt.Fprintln(a.Out, "Store it offline now (password manager, printed copy). It holds the encryption keys:")
	fmt.Fprintln(a.Out, "without it, encrypted offsite backups cannot be restored if this host is lost.")
	if !kit.Encrypted {
		fmt.Fprintln(a.Out, "The kit is not passphrase-protected: keep the file secret.")
	}
	fmt.Fprintln(a.Out, "Copy the file off this host (for example with scp); do not paste its contents into a terminal or a chat.")
	sum := kf.confirm
	if sum == "" {
		if !a.Interactive {
			return usagef("confirm the kit with --confirm-checksum %s once it is stored safely", kit.Checksum)
		}
		if sum, err = a.prompt(fmt.Sprintf("\nOnce the kit is stored, type its checksum (%s) to confirm: ", checksumShape(kit.Checksum))); err != nil {
			return err
		}
	}
	var conf apiv1.KitConfirmResponse
	if err := a.do(ctx, http.MethodPost, "/v1/recovery-kit/confirm", apiv1.KitConfirmRequest{Checksum: strings.TrimSpace(sum)}, &conf); err != nil {
		return err
	}
	fmt.Fprintln(a.Out, "Recovery kit confirmed.")
	return nil
}
