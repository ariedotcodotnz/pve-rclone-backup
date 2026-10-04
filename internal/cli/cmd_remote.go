// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
)

func (a *App) remoteCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "remote", Short: "Manage transport remotes (cloud accounts)"}
	cmd.AddCommand(
		&cobra.Command{Use: "list", Short: "List remotes", Args: exactArgs(0), RunE: func(cmd *cobra.Command, _ []string) error {
			var list []apiv1.Remote
			if err := a.do(cmd.Context(), http.MethodGet, "/v1/remotes", nil, &list); err != nil {
				return err
			}
			if a.Output == "json" {
				return a.json(list)
			}
			rows := make([][]string, 0, len(list))
			for _, r := range list {
				auth := "-"
				if r.Type == "onedrive" {
					auth = yesNo(r.Authorized)
				}
				rows = append(rows, []string{r.Name, r.Type, auth, dash(r.DriveType), dash(strings.Join(r.Storages, ","))})
			}
			a.table([]string{"REMOTE", "TYPE", "AUTHORIZED", "DRIVE", "STORAGES"}, rows)
			return nil
		}},
		&cobra.Command{Use: "show <remote>", Short: "Show a remote", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			var r apiv1.Remote
			if err := a.do(cmd.Context(), http.MethodGet, "/v1/remotes/"+url.PathEscape(args[0]), nil, &r); err != nil {
				return err
			}
			if a.Output == "json" {
				return a.json(r)
			}
			fmt.Fprintf(a.Out, "Remote:      %s\nType:        %s\nSupported:   %s\nAuthorized:  %s\nToken until: %s\nOwn app:     %s\nDrive:       %s %s\nStorages:    %s\n",
				r.Name, r.Type, yesNo(r.Supported), yesNo(r.Authorized), humanTime(r.TokenExpiry), yesNo(r.CustomApp),
				dash(r.DriveType), r.DriveID, dash(strings.Join(r.Storages, ", ")))
			return nil
		}},
		a.remoteAddCommand(),
		&cobra.Command{Use: "test <remote>", Short: "Write, read and delete a probe object", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			var res apiv1.ProbeResult
			if err := a.do(cmd.Context(), http.MethodPost, "/v1/remotes/"+url.PathEscape(args[0])+"/test", nil, &res); err != nil {
				return err
			}
			if a.Output == "json" {
				return a.json(res)
			}
			fmt.Fprintf(a.Out, "OK: round trip in %d ms, provider hash %s, usage %s\n", res.LatencyMS, dash(res.HashType), usageString(res.Usage))
			return nil
		}},
		&cobra.Command{Use: "about <remote>", Short: "Show the quota of a remote", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			var u *apiv1.Usage
			if err := a.do(cmd.Context(), http.MethodGet, "/v1/remotes/"+url.PathEscape(args[0])+"/about", nil, &u); err != nil {
				return err
			}
			if a.Output == "json" {
				return a.json(u)
			}
			if u == nil {
				fmt.Fprintln(a.Out, "The provider does not report a quota.")
				return nil
			}
			for _, f := range []struct {
				name string
				v    *int64
			}{{"Total", u.Total}, {"Used", u.Used}, {"Free", u.Free}, {"Recycle bin", u.Trashed}} {
				if f.v != nil {
					fmt.Fprintf(a.Out, "%-12s %s\n", f.name+":", humanBytes(*f.v))
				}
			}
			return nil
		}},
		a.remoteReconnectCommand(),
		&cobra.Command{Use: "remove <remote>", Short: "Remove a remote no storage uses", Args: exactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			ok, err := a.confirm(fmt.Sprintf("Remove remote %s and its credentials?", args[0]))
			if err != nil || !ok {
				return err
			}
			return a.do(cmd.Context(), http.MethodDelete, "/v1/remotes/"+url.PathEscape(args[0]), nil, nil)
		}},
	)
	return cmd
}

type setupFlags struct {
	answers []string
	auth    string
}

func (f *setupFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringArrayVar(&f.answers, "answer", nil, "answer a setup question non-interactively (NAME=VALUE, repeatable; 'redirect' is the pasted OAuth redirect URL)")
	cmd.Flags().StringVar(&f.auth, "auth", "relay", "authorization method: relay (open a URL anywhere, paste back the redirect) or token (paste the output of 'rclone authorize')")
}

func (f *setupFlags) parse() (map[string]string, error) {
	if f.auth != "relay" && f.auth != "token" {
		return nil, usagef("--auth must be relay or token")
	}
	out := map[string]string{}
	for _, a := range f.answers {
		k, v, ok := strings.Cut(a, "=")
		if !ok || k == "" {
			return nil, usagef("--answer %q is not NAME=VALUE", a)
		}
		out[k] = v
	}
	return out, nil
}

func (a *App) remoteAddCommand() *cobra.Command {
	var (
		provider, clientID, clientSecret string
		extra                            []string
		sf                               setupFlags
	)
	cmd := &cobra.Command{
		Use:   "add <remote>",
		Short: "Configure a new remote (OneDrive)",
		Long: `Configure a new transport remote. For OneDrive the default authorization is a relay:
the command shows a Microsoft sign-in URL that can be opened on any device; after signing in,
the browser is redirected to http://localhost:53682/... and shows an error page. Copy that
complete address and paste it back here.

Registering your own Microsoft app (client ID and secret) avoids throttling of rclone's shared app.`,
		Args: exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			answers, err := sf.parse()
			if err != nil {
				return err
			}
			params := map[string]string{}
			for _, kv := range extra {
				k, v, ok := strings.Cut(kv, "=")
				if !ok || k == "" {
					return usagef("--param %q is not NAME=VALUE", kv)
				}
				params[k] = v
			}
			if clientID != "" {
				params["client_id"] = clientID
			}
			if clientSecret != "" {
				params["client_secret"] = clientSecret
			}
			var s apiv1.RemoteSetup
			if err := a.do(cmd.Context(), http.MethodPost, "/v1/remote-setup",
				apiv1.RemoteSetupRequest{Name: args[0], Provider: provider, Params: params}, &s); err != nil {
				return err
			}
			return a.runSetup(cmd.Context(), s, answers, sf.auth)
		},
	}
	cmd.Flags().StringVar(&provider, "provider", "onedrive", "storage provider")
	cmd.Flags().StringVar(&clientID, "client-id", "", "OAuth client ID of your own app registration")
	cmd.Flags().StringVar(&clientSecret, "client-secret", "", "OAuth client secret of your own app registration")
	cmd.Flags().StringArrayVar(&extra, "param", nil, "set another backend option (NAME=VALUE, repeatable)")
	sf.register(cmd)
	return cmd
}

func (a *App) remoteReconnectCommand() *cobra.Command {
	var sf setupFlags
	cmd := &cobra.Command{
		Use:   "reconnect <remote>",
		Short: "Authorize a remote again (expired or revoked credentials)",
		Args:  exactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			answers, err := sf.parse()
			if err != nil {
				return err
			}
			if _, ok := answers["config_refresh_token"]; !ok {
				answers["config_refresh_token"] = "true"
			}
			var s apiv1.RemoteSetup
			if err := a.do(cmd.Context(), http.MethodPost, "/v1/remotes/"+url.PathEscape(args[0])+"/reconnect", nil, &s); err != nil {
				return err
			}
			return a.runSetup(cmd.Context(), s, answers, sf.auth)
		},
	}
	sf.register(cmd)
	return cmd
}

// runSetup drives a setup session to completion.
func (a *App) runSetup(ctx context.Context, s apiv1.RemoteSetup, answers map[string]string, auth string) error {
	id := s.ID
	defer func() { _ = a.do(context.WithoutCancel(ctx), http.MethodDelete, "/v1/remote-setup/"+id, nil, nil) }()
	for {
		switch s.Status {
		case apiv1.SetupDone:
			fmt.Fprintf(a.Out, "Remote %s is ready. Test it with 'pve-rclone-backup remote test %s'.\n", s.Name, s.Name)
			return nil
		case apiv1.SetupFailed:
			return fmt.Errorf("setting up %s failed: %s", s.Name, s.Error)
		case apiv1.SetupAuthorizing:
			next, err := a.authorize(ctx, s, answers)
			if err != nil {
				return err
			}
			s = next
		case apiv1.SetupQuestion:
			if s.Option == nil {
				return fmt.Errorf("setup of %s stopped without a question", s.Name)
			}
			ans, err := a.answer(s, answers, auth)
			if err != nil {
				return err
			}
			var next apiv1.RemoteSetup
			if err := a.do(ctx, http.MethodPost, "/v1/remote-setup/"+id+"/answer", apiv1.RemoteSetupAnswer{State: s.State, Result: ans}, &next); err != nil {
				return err
			}
			s = next
		default:
			return fmt.Errorf("unexpected setup status %q", s.Status)
		}
	}
}

func (a *App) answer(s apiv1.RemoteSetup, answers map[string]string, auth string) (string, error) {
	o := s.Option
	if v, ok := answers[o.Name]; ok {
		delete(answers, o.Name)
		return v, nil
	}
	if o.Name == "config_is_local" {
		// "Local" means rclone's loopback listener is used: the relay.
		return strconv.FormatBool(auth == "relay"), nil
	}
	if s.Error != "" {
		fmt.Fprintln(a.Err, "Error:", s.Error)
	}
	if !a.Interactive {
		if o.Default != "" || !o.Required {
			return o.Default, nil
		}
		return "", usagef("setup question %q needs an answer: pass --answer %s=VALUE", o.Name, o.Name)
	}
	fmt.Fprintf(a.Err, "\n%s\n", strings.TrimSpace(o.Help))
	for i, e := range o.Examples {
		fmt.Fprintf(a.Err, "  %d) %s  %s\n", i+1, e.Value, firstLine(e.Help))
	}
	q := o.Name
	if o.Default != "" {
		q += " [" + o.Default + "]"
	}
	var v string
	var err error
	if o.IsPassword {
		v, err = a.promptSecret(q + ": ")
	} else {
		v, err = a.prompt(q + ": ")
	}
	if err != nil {
		return "", err
	}
	if v == "" {
		return o.Default, nil
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= len(o.Examples) {
		return o.Examples[n-1].Value, nil
	}
	return v, nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// authorize shows the provider URL and relays the pasted redirect.
func (a *App) authorize(ctx context.Context, s apiv1.RemoteSetup, answers map[string]string) (apiv1.RemoteSetup, error) {
	deadline := time.Now().Add(30 * time.Second)
	for s.AuthURL == "" && s.Status == apiv1.SetupAuthorizing {
		if time.Now().After(deadline) {
			return s, fmt.Errorf("the authorization of %s did not start", s.Name)
		}
		time.Sleep(200 * time.Millisecond)
		if err := a.do(ctx, http.MethodGet, "/v1/remote-setup/"+s.ID, nil, &s); err != nil {
			return s, err
		}
	}
	if s.Status != apiv1.SetupAuthorizing {
		return s, nil
	}
	redirect, ok := answers["redirect"]
	if !ok {
		if !a.Interactive {
			return s, usagef("authorization needs the redirect URL: open %s and pass --answer redirect=URL", s.AuthURL)
		}
		fmt.Fprintf(a.Err, "\n1. Open this address in a browser on any device and sign in:\n\n   %s\n\n", s.AuthURL)
		fmt.Fprintln(a.Err, "2. After you approve access, the browser opens http://localhost:53682/... and shows an")
		fmt.Fprintln(a.Err, "   error page. That is expected.")
		fmt.Fprintln(a.Err, "3. Copy the complete address from the browser's address bar and paste it here.")
		var err error
		if redirect, err = a.prompt("\nRedirect URL: "); err != nil {
			return s, err
		}
	}
	delete(answers, "redirect")
	var next apiv1.RemoteSetup
	if err := a.do(ctx, http.MethodPost, "/v1/remote-setup/"+s.ID+"/oauth-redirect", apiv1.OAuthRedirect{URL: redirect}, &next); err != nil {
		return s, err
	}
	return next, nil
}
