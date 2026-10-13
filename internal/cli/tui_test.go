// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/tui"
)

func tuiApp(socket string) *App {
	return &App{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard, Socket: socket, Output: "table", Now: time.Now,
		PVESH: func(context.Context, ...string) ([]byte, error) { return nil, nil }}
}

func TestTUICoversCLICommandsAndFlags(t *testing.T) {
	a := tuiApp("/tmp/tui-test.sock")
	opts := a.tuiOptions()
	byName := map[string]tui.Command{}
	for _, action := range opts.Commands {
		byName[strings.Join(action.Path, " ")] = action
	}
	for _, name := range []string{"storage set", "storage init", "queue priority", "queue scan", "backup notes", "backup verify", "restore",
		"retention preview", "retention run", "recovery-kit export", "recovery-kit confirm", "recovery-kit verify", "recovery-kit import", "recovery-kit show", "recovery-kit status",
		"recover", "config schema", "hook install", "hook uninstall", "hook status", "hook notify", "remote add", "remote reconnect", "remote about", "verify history", "version", "completion bash", "help"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("CLI action missing: %s", name)
		}
	}
	if _, ok := byName["tui"]; ok {
		t.Fatal("TUI can recursively launch itself")
	}
	root := a.rootCommand()
	root.InitDefaultCompletionCmd()
	root.InitDefaultHelpCmd()
	var check func(*cobra.Command)
	check = func(cmd *cobra.Command) {
		if cmd.Name() == "tui" {
			return
		}
		if cmd != root && cmd.Runnable() {
			name := strings.TrimPrefix(cmd.CommandPath(), root.Name()+" ")
			action, ok := byName[name]
			if !ok {
				t.Errorf("missing %s", name)
				return
			}
			var want, got []string
			cmd.LocalFlags().VisitAll(func(f *pflag.Flag) {
				if !f.Hidden && f.Deprecated == "" && f.Name != "help" {
					want = append(want, f.Name)
				}
			})
			cmd.InheritedFlags().VisitAll(func(f *pflag.Flag) {
				if !f.Hidden && f.Deprecated == "" && f.Name != "help" {
					want = append(want, f.Name)
				}
			})
			for _, flag := range action.Flags {
				got = append(got, flag.Name)
			}
			slices.Sort(want)
			slices.Sort(got)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s flags %v, CLI flags %v", name, got, want)
			}
		}
		for _, child := range cmd.Commands() {
			check(child)
		}
	}
	check(root)
	args := byName["storage set"].Arguments
	if len(args) != 2 || !args[1].Repeated || args[1].Name != "property=value" {
		t.Fatalf("storage settings args: %+v", args)
	}
}

func TestTUIExecutesSharedCLIWithConfirmation(t *testing.T) {
	deleted := false
	socket := fakeDaemon(t, map[string]api.HandlerFunc{
		"DELETE /v1/remotes/od": func(w http.ResponseWriter, _ *http.Request) error {
			deleted = true
			return api.WriteJSON(w, 200, map[string]bool{"removed": true})
		},
	})
	opts := tuiApp(socket).tuiOptions()
	var out bytes.Buffer
	questions := 0
	ask := func(question string, secret bool) (string, error) {
		questions++
		if secret || !strings.Contains(question, "Remove remote od") {
			t.Fatalf("unexpected prompt: %q, secret %v", question, secret)
		}
		return "n", nil
	}
	if code := opts.Execute(t.Context(), []string{"remote", "remove", "--", "od"}, &out, ask); code != 0 || deleted || questions != 1 {
		t.Fatalf("declined removal: code %d deleted %v prompts %d", code, deleted, questions)
	}
	ask = func(string, bool) (string, error) { return "y", nil }
	if code := opts.Execute(t.Context(), []string{"remote", "remove", "--", "od"}, &out, ask); code != 0 || !deleted {
		t.Fatalf("confirmed removal: code %d output %q", code, out.String())
	}
}

func TestTUIRemoteSetupUsesPromptCallback(t *testing.T) {
	var secretValue string
	socket := fakeDaemon(t, map[string]api.HandlerFunc{
		"POST /v1/remote-setup": func(w http.ResponseWriter, r *http.Request) error {
			var req apiv1.RemoteSetupRequest
			if err := api.DecodeJSON(r, &req); err != nil {
				return err
			}
			secretValue = req.Params["client_secret"]
			return api.WriteJSON(w, 200, apiv1.RemoteSetup{ID: "s1", Name: "od", Status: apiv1.SetupAuthorizing, AuthURL: "https://login.example.test/authorize"})
		},
		"POST /v1/remote-setup/s1/oauth-redirect": func(w http.ResponseWriter, r *http.Request) error {
			var req apiv1.OAuthRedirect
			if err := api.DecodeJSON(r, &req); err != nil {
				return err
			}
			if req.URL != "http://localhost:53682/?state=x&code=y" {
				return api.Invalid("wrong redirect")
			}
			return api.WriteJSON(w, 200, apiv1.RemoteSetup{ID: "s1", Name: "od", Status: apiv1.SetupDone})
		},
		"DELETE /v1/remote-setup/s1": okJSON(map[string]bool{"removed": true}),
	})
	opts := tuiApp(socket).tuiOptions()
	var out bytes.Buffer
	var questions []bool
	ask := func(question string, secret bool) (string, error) {
		questions = append(questions, secret)
		if secret {
			return " secret with spaces ", nil
		}
		if !strings.Contains(out.String(), "https://login.example.test/authorize") || !strings.Contains(question, "Redirect URL") {
			return "", fmt.Errorf("missing sign-in instructions")
		}
		return "http://localhost:53682/?state=x&code=y", nil
	}
	code := opts.Execute(t.Context(), []string{"remote", "add", "--client-id=custom-app", "--", "od"}, &out, ask)
	if code != 0 || !reflect.DeepEqual(questions, []bool{true, false}) || secretValue != " secret with spaces " || strings.Contains(out.String(), secretValue) {
		t.Fatalf("setup code %d prompts %v secret %q output %q", code, questions, secretValue, out.String())
	}
}

func okJSON(v any) api.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) error { return api.WriteJSON(w, 200, v) }
}

func TestTUIStorageSettingsPassLiteralValuesToPVE(t *testing.T) {
	a := tuiApp("/tmp/no-daemon-needed.sock")
	var args []string
	a.PVESH = func(_ context.Context, pv ...string) ([]byte, error) {
		args = append([]string(nil), pv...)
		return nil, nil
	}
	opts := a.tuiOptions()
	var out bytes.Buffer
	code := opts.Execute(t.Context(), []string{"storage", "set", "--", "offsite", "rclone-path=Folder with spaces", "comment=$(touch /tmp/never)"}, &out, nil)
	want := []string{"set", "/storage/offsite", "--rclone-path", "Folder with spaces", "--comment", "$(touch /tmp/never)"}
	if code != 0 || !reflect.DeepEqual(args, want) {
		t.Fatalf("code %d pvesh %q output %q", code, args, out.String())
	}
}

func TestTUICancelStopsBlockedJobPolling(t *testing.T) {
	polling := make(chan struct{})
	socket := fakeDaemon(t, map[string]api.HandlerFunc{
		"POST /v1/storages/offsite/backups/test.vma.zst/fetch": okJSON(apiv1.Job{ID: 7}),
		"GET /v1/jobs/7": func(_ http.ResponseWriter, r *http.Request) error {
			close(polling)
			<-r.Context().Done()
			return r.Context().Err()
		},
	})
	opts := tuiApp(socket).tuiOptions()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var out bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- opts.Execute(ctx, []string{"backup", "fetch", "--to-storage=local", "--", "offsite:backup/test.vma.zst"}, &out, nil)
	}()
	select {
	case <-polling:
	case <-time.After(3 * time.Second):
		t.Fatal("job was not polled")
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 || !strings.Contains(out.String(), "job 7 continues in the background") {
			t.Fatalf("cancelled wait: %d %q", code, out.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not interrupt the in-flight poll")
	}
}
