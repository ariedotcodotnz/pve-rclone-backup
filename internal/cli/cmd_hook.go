// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
)

// Locations of the vzdump hook (variables for tests).
var (
	vzdumpConf = "/etc/vzdump.conf"
	hookScript = "/usr/share/pve-rclone-backup/vzdump-hook"
	hookChain  = "/var/lib/pve-rclone-backup/vzdump-hook.chain"
)

var scriptLine = regexp.MustCompile(`^\s*script\s*:\s*(.*?)\s*$`)

// readVzdumpConf returns the lines of vzdump.conf and its script setting.
func readVzdumpConf() ([]string, string, error) {
	data, err := os.ReadFile(vzdumpConf)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	script := ""
	for _, l := range lines {
		if m := scriptLine.FindStringSubmatch(l); m != nil {
			script = m[1]
		}
	}
	return lines, script, nil
}

// writeVzdumpConf sets (or with "" removes) the script setting.
func writeVzdumpConf(lines []string, script string) error {
	var out []string
	for _, l := range lines {
		if !scriptLine.MatchString(l) {
			out = append(out, l)
		}
	}
	if script != "" {
		out = append(out, "script: "+script)
	}
	tmp := vzdumpConf + ".pve-rclone-backup.tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(out, "\n")+"\n"), 0o644); err != nil { //nolint:gosec // vzdump.conf is world-readable
		return err
	}
	return os.Rename(tmp, vzdumpConf)
}

func readChain() string {
	data, err := os.ReadFile(hookChain)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (a *App) hookCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hook",
		Short: "Optional vzdump hook that reports finished archives to the daemon",
		Long: `The daemon finds finished archives on its own (inotify and periodic scans). The optional vzdump
hook reports them as soon as a backup ends, which helps on network storages written by other nodes.
It is set node-wide in /etc/vzdump.conf; a 'script' set on a backup job overrides it.`,
	}
	var phase, path string
	notify := &cobra.Command{Use: "notify", Short: "Report a finished archive (used by the hook)", Args: exactArgs(0), Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if path == "" {
				return usagef("--path is required")
			}
			abs, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			return a.do(cmd.Context(), http.MethodPost, "/v1/discovery/notify", apiv1.DiscoveryNotify{Path: abs, Phase: phase}, nil)
		}}
	notify.Flags().StringVar(&phase, "phase", "", "vzdump hook phase")
	notify.Flags().StringVar(&path, "path", "", "archive path")

	status := &cobra.Command{Use: "status", Short: "Show whether the hook is installed", Args: exactArgs(0), RunE: func(*cobra.Command, []string) error {
		_, script, err := readVzdumpConf()
		if err != nil {
			return err
		}
		switch script {
		case hookScript:
			fmt.Fprintf(a.Out, "Installed in %s.\n", vzdumpConf)
			if prev := readChain(); prev != "" {
				fmt.Fprintf(a.Out, "It then runs the previous hook %s.\n", prev)
			}
		case "":
			fmt.Fprintln(a.Out, "Not installed (archives are found by inotify and periodic scans).")
		default:
			fmt.Fprintf(a.Out, "Not installed: %s uses the hook %s ('hook install --chain' runs both).\n", vzdumpConf, script)
		}
		return nil
	}}

	var chain bool
	install := &cobra.Command{Use: "install", Short: "Set the hook in /etc/vzdump.conf", Args: exactArgs(0), RunE: func(*cobra.Command, []string) error {
		lines, script, err := readVzdumpConf()
		if err != nil {
			return err
		}
		if script == hookScript {
			fmt.Fprintln(a.Out, "Already installed.")
			return nil
		}
		if script != "" && !chain {
			return fmt.Errorf("%s already uses the hook %s; pass --chain to run it after ours", vzdumpConf, script)
		}
		ok, err := a.confirm(fmt.Sprintf("Set 'script: %s' in %s?", hookScript, vzdumpConf))
		if err != nil || !ok {
			return err
		}
		if script != "" {
			if err := os.MkdirAll(filepath.Dir(hookChain), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(hookChain, []byte(script+"\n"), 0o600); err != nil {
				return err
			}
		}
		if err := writeVzdumpConf(lines, hookScript); err != nil {
			return err
		}
		fmt.Fprintln(a.Out, "Hook installed.")
		return nil
	}}
	install.Flags().BoolVar(&chain, "chain", false, "keep an existing hook script and run it after ours")

	uninstall := &cobra.Command{Use: "uninstall", Short: "Remove the hook (restoring a chained one)", Args: exactArgs(0), RunE: func(*cobra.Command, []string) error {
		lines, script, err := readVzdumpConf()
		if err != nil {
			return err
		}
		if script != hookScript {
			fmt.Fprintln(a.Out, "Not installed.")
			return nil
		}
		ok, err := a.confirm(fmt.Sprintf("Remove the hook from %s?", vzdumpConf))
		if err != nil || !ok {
			return err
		}
		prev := readChain()
		if err := writeVzdumpConf(lines, prev); err != nil {
			return err
		}
		_ = os.Remove(hookChain)
		if prev != "" {
			fmt.Fprintf(a.Out, "Hook removed; %s is the hook again.\n", prev)
		} else {
			fmt.Fprintln(a.Out, "Hook removed.")
		}
		return nil
	}}
	cmd.AddCommand(notify, status, install, uninstall)
	return cmd
}
