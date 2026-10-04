// SPDX-License-Identifier: AGPL-3.0-or-later

// Package cli implements the pve-rclone-backup command line. Every
// command is a thin client of the daemon API; the CLI holds no state.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/client"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/version"
)

// Exit codes.
const (
	ExitOK           = 0
	ExitError        = 1
	ExitUsage        = 2
	ExitUnavailable  = 3 // the daemon cannot be reached
	ExitIncompatible = 4 // CLI and daemon API versions do not match
	ExitFindings     = 5 // the command completed and found problems
)

// usageError marks invalid command lines.
type usageError struct{ error }

func usagef(format string, args ...any) error { return usageError{fmt.Errorf(format, args...)} }

// errFindings is returned by commands that report problems (doctor,
// verification).
var errFindings = errors.New("problems found")

// App holds the state of one CLI invocation.
type App struct {
	In       io.Reader
	Out, Err io.Writer

	Socket string
	Output string // table or json
	Yes    bool

	// Interactive reports whether questions can be asked (stdin is a
	// terminal). Tests set it explicitly.
	Interactive bool
	// PVESH runs pvesh (replaced in tests).
	PVESH func(ctx context.Context, args ...string) ([]byte, error)
	Now   func() time.Time

	client *client.Client
	reader *bufio.Reader
}

// New returns an app wired to the process's standard streams.
func New() *App {
	return &App{
		In: os.Stdin, Out: os.Stdout, Err: os.Stderr, Socket: client.DefaultSocket, Output: "table",
		Interactive: term.IsTerminal(int(os.Stdin.Fd())),
		PVESH: func(ctx context.Context, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, "pvesh", args...).CombinedOutput() //nolint:gosec // argv only, no shell
		},
		Now: time.Now,
	}
}

func (a *App) api() *client.Client {
	if a.client == nil {
		a.client = client.New(a.Socket)
	}
	return a.client
}

// do calls the daemon.
func (a *App) do(ctx context.Context, method, path string, in, out any) error {
	return a.api().Do(ctx, method, path, in, out)
}

// Run executes a command line and returns the exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	a.Out, a.Err = newSafeWriter(a.Out), newSafeWriter(a.Err)
	root := a.rootCommand()
	root.SetArgs(args)
	root.SetIn(a.In)
	root.SetOut(a.Out)
	root.SetErr(a.Err)
	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}
	code := ExitError
	var ue usageError
	switch {
	case errors.As(err, &ue), strings.HasPrefix(err.Error(), "unknown command"), strings.HasPrefix(err.Error(), "unknown flag"),
		strings.Contains(err.Error(), "arg(s), received"):
		code = ExitUsage
	case client.IsCode(err, apiv1.CodeUnavailable):
		code = ExitUnavailable
	case client.IsCode(err, apiv1.CodeIncompatibleVersion):
		code = ExitIncompatible
	case errors.Is(err, errFindings):
		code = ExitFindings
	}
	if !errors.Is(err, errFindings) {
		fmt.Fprintln(a.Err, "pve-rclone-backup:", err)
	}
	return code
}

func (a *App) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "pve-rclone-backup",
		Short:         "Offsite replication of Proxmox VE backups through rclone",
		Long:          "pve-rclone-backup manages pve-rclone-backupd: transport remotes, offsite storages, the upload queue,\noffsite backups and recovery kits.",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Get().String(),
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if a.Output != "table" && a.Output != "json" {
				return usagef("--output must be table or json")
			}
			return nil
		},
	}
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	f := root.PersistentFlags()
	f.StringVar(&a.Socket, "socket", a.Socket, "daemon API socket")
	f.StringVarP(&a.Output, "output", "o", a.Output, "output format: table or json")
	f.BoolVarP(&a.Yes, "yes", "y", false, "do not ask for confirmation")
	root.AddCommand(a.versionCommand(), a.statusCommand(), a.doctorCommand(), a.remoteCommand(), a.storageCommand(),
		a.queueCommand(), a.backupCommand(), a.restoreCommand(), a.verifyCommand(), a.retentionCommand(), a.kitCommand(), a.configCommand())
	return root
}

// args validators that report usage errors.
func exactArgs(n int) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != n {
			return usagef("expected %d argument(s), got %d", n, len(args))
		}
		return nil
	}
}

func minArgs(n int) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) < n {
			return usagef("expected at least %d argument(s), got %d", n, len(args))
		}
		return nil
	}
}

func (a *App) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show CLI and daemon versions",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cli := version.Get()
			v, err := a.api().Version(cmd.Context())
			if a.Output == "json" {
				out := map[string]any{"cli": cli}
				if err == nil {
					out["daemon"] = v
				}
				return a.json(out)
			}
			fmt.Fprintln(a.Out, "pve-rclone-backup", cli)
			if err != nil {
				fmt.Fprintln(a.Out, "daemon: unavailable:", err)
				return nil
			}
			fmt.Fprintf(a.Out, "pve-rclone-backupd %s (API %d, rclone %s) on %s\n", v.Daemon, v.API, v.Rclone, v.Node)
			return nil
		},
	}
}
