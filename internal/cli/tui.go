// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"context"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/tui"
)

// tuiOptions builds the action catalogue from the same command tree used by
// the CLI. Each execution gets a fresh App: Cobra flag bindings and buffered
// input must never be shared with another invocation or the dashboard client.
func (a *App) tuiOptions() tui.Options {
	root := a.rootCommand()
	root.InitDefaultCompletionCmd()
	root.InitDefaultHelpCmd()
	opts := tui.Options{Client: a.api(), PVESH: a.PVESH}
	var walk func(*cobra.Command, []string)
	walk = func(cmd *cobra.Command, path []string) {
		if cmd.Name() == "tui" {
			return // launching a second TUI inside the first is never useful
		}
		if cmd != root {
			path = append(append([]string(nil), path...), cmd.Name())
		}
		if cmd != root && cmd.Runnable() {
			action := tui.Command{Path: path, Summary: cmd.Short, Help: cmd.Long}
			for _, arg := range strings.Fields(cmd.Use)[1:] {
				action.Arguments = append(action.Arguments, tui.Argument{
					Name:     strings.NewReplacer("<", "", ">", "", "[", "", "]", "").Replace(strings.TrimSuffix(arg, "...")),
					Required: strings.HasPrefix(arg, "<"),
					Repeated: strings.HasSuffix(arg, "...") || cmd.Name() == "help",
				})
			}
			cmd.InheritedFlags() // populate inherited flags before visiting them
			flags := pflag.NewFlagSet("tui", pflag.ContinueOnError)
			flags.AddFlagSet(cmd.Flags())
			flags.AddFlagSet(cmd.InheritedFlags())
			flags.VisitAll(func(f *pflag.Flag) {
				if f.Hidden || f.Deprecated != "" || f.Name == "help" {
					return
				}
				typ := f.Value.Type()
				def := f.DefValue
				if typ == "stringArray" || typ == "stringSlice" {
					def = strings.TrimSuffix(strings.TrimPrefix(def, "["), "]")
				}
				action.Flags = append(action.Flags, tui.Flag{
					Name: f.Name, Help: f.Usage, Type: typ, Default: def,
					Required: len(f.Annotations[cobra.BashCompOneRequiredFlag]) > 0,
				})
			})
			opts.Commands = append(opts.Commands, action)
		}
		for _, child := range cmd.Commands() {
			walk(child, path)
		}
	}
	walk(root, nil)
	opts.Execute = func(ctx context.Context, args []string, out io.Writer, ask tui.AskFunc) int {
		app := &App{In: strings.NewReader(""), Out: out, Err: out, Socket: a.Socket, Output: "table",
			Interactive: true, PVESH: a.PVESH, Now: a.Now, Prompt: ask}
		return app.Run(ctx, args)
	}
	return opts
}
