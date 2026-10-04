// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
)

// fakeDaemon serves scripted API routes on a unix socket.
func fakeDaemon(t *testing.T, routes map[string]api.HandlerFunc) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "prbcli")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "api.sock")
	srv := api.New(api.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), AllowUIDs: []uint32{uint32(os.Getuid())}})
	for pattern, h := range routes {
		srv.Handle(pattern, h)
	}
	l, err := api.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, l); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return socket
}

type result struct {
	code     int
	out, err string
}

func run(t *testing.T, socket string, in io.Reader, interactive bool, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	if in == nil {
		in = strings.NewReader("")
	}
	a := &App{In: in, Out: &out, Err: &errb, Socket: socket, Output: "table", Interactive: interactive, Now: time.Now,
		PVESH: func(context.Context, ...string) ([]byte, error) { return nil, nil }}
	code := a.Run(t.Context(), args)
	return result{code, out.String(), errb.String()}
}

func TestExitCodes(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "none.sock")
	for _, c := range []struct {
		args []string
		code int
	}{
		{[]string{"nonsense"}, ExitUsage},
		{[]string{"status", "--bogus"}, ExitUsage},
		{[]string{"status", "extra"}, ExitUsage},
		{[]string{"-o", "yaml", "status"}, ExitUsage},
		{[]string{"backup", "inspect", "not-a-volid"}, ExitUsage},
		{[]string{"queue", "show", "x"}, ExitUsage},
		{[]string{"status"}, ExitUnavailable},
		{[]string{"version"}, ExitOK},
		{[]string{"--version"}, ExitOK},
	} {
		if r := run(t, missing, nil, false, c.args...); r.code != c.code {
			t.Errorf("%v: exit %d, want %d (%s)", c.args, r.code, c.code, r.err)
		}
	}
}

func TestHelpers(t *testing.T) {
	if s, n, err := parseVolid("offsite:backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst"); err != nil || s != "offsite" || n != "vzdump-qemu-100-2026_10_04-02_00_01.vma.zst" {
		t.Fatalf("parseVolid = %q %q %v", s, n, err)
	}
	for _, bad := range []string{"offsite", "offsite:images/x", ":backup/x", "offsite:backup/../x/y"} {
		if _, _, err := parseVolid(bad); err == nil {
			t.Errorf("parseVolid(%q) accepted", bad)
		}
	}
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1536: "1.5 KiB", 5 << 30: "5.0 GiB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q", n, got)
		}
	}
	if got := clean("a\tb\x1b[31mc\nd"); got != "a b[31mc d" {
		t.Errorf("clean = %q", got)
	}
	if got := strings.Join(quoteAll([]string{"create", "--a", "x y", "it's"}), " "); got != `create --a 'x y' 'it'\''s'` {
		t.Errorf("quoteAll = %s", got)
	}
}

func TestRemoteAddRelay(t *testing.T) {
	var mu sync.Mutex
	var answers []apiv1.RemoteSetupAnswer
	var redirect string
	finished := false
	socket := fakeDaemon(t, map[string]api.HandlerFunc{
		"POST /v1/remote-setup": func(w http.ResponseWriter, r *http.Request) error {
			var req apiv1.RemoteSetupRequest
			_ = api.DecodeJSON(r, &req)
			if req.Params["client_id"] != "app" || req.Params["region"] != "global" {
				return api.Invalid("params %v", req.Params)
			}
			return api.WriteJSON(w, 200, apiv1.RemoteSetup{ID: "s1", Name: req.Name, Status: apiv1.SetupQuestion, State: "q1",
				Option: &apiv1.SetupOption{Name: "config_is_local", Help: "Use web browser?"}})
		},
		"POST /v1/remote-setup/s1/answer": func(w http.ResponseWriter, r *http.Request) error {
			var a apiv1.RemoteSetupAnswer
			_ = api.DecodeJSON(r, &a)
			mu.Lock()
			answers = append(answers, a)
			n := len(answers)
			mu.Unlock()
			if n == 1 {
				return api.WriteJSON(w, 200, apiv1.RemoteSetup{ID: "s1", Name: "od", Status: apiv1.SetupAuthorizing})
			}
			return api.WriteJSON(w, 200, apiv1.RemoteSetup{ID: "s1", Name: "od", Status: apiv1.SetupDone})
		},
		"GET /v1/remote-setup/s1": func(w http.ResponseWriter, r *http.Request) error {
			return api.WriteJSON(w, 200, apiv1.RemoteSetup{ID: "s1", Name: "od", Status: apiv1.SetupAuthorizing,
				AuthURL: "https://login.example/authorize?state=abc"})
		},
		"POST /v1/remote-setup/s1/oauth-redirect": func(w http.ResponseWriter, r *http.Request) error {
			var req apiv1.OAuthRedirect
			_ = api.DecodeJSON(r, &req)
			mu.Lock()
			redirect = req.URL
			mu.Unlock()
			return api.WriteJSON(w, 200, apiv1.RemoteSetup{ID: "s1", Name: "od", Status: apiv1.SetupQuestion, State: "q2",
				Option: &apiv1.SetupOption{Name: "config_type", Help: "Type of connection", Required: true,
					Examples: []apiv1.SetupExample{{Value: "onedrive", Help: "OneDrive Personal or Business"}, {Value: "sharepoint"}}}})
		},
		"DELETE /v1/remote-setup/s1": func(w http.ResponseWriter, r *http.Request) error {
			mu.Lock()
			finished = true
			mu.Unlock()
			return api.WriteJSON(w, 200, struct{}{})
		},
	})

	// Interactive: no client secret, the redirect is pasted, the drive
	// type chosen by number.
	in := strings.NewReader("\nhttp://localhost:53682/?code=c&state=abc\n1\n")
	r := run(t, socket, in, true, "remote", "add", "od", "--client-id", "app", "--param", "region=global")
	if r.code != 0 {
		t.Fatalf("exit %d: %s%s", r.code, r.out, r.err)
	}
	if !strings.Contains(r.err, "https://login.example/authorize?state=abc") || !strings.Contains(r.out, "Remote od is ready") {
		t.Fatalf("output:\n%s\n%s", r.out, r.err)
	}
	mu.Lock()
	if len(answers) != 2 || answers[0].Result != "true" || answers[0].State != "q1" || answers[1].Result != "onedrive" ||
		redirect != "http://localhost:53682/?code=c&state=abc" || !finished {
		t.Fatalf("answers %+v, redirect %q, finished %v", answers, redirect, finished)
	}
	answers, finished = nil, false
	mu.Unlock()

	// Non-interactive: everything comes from flags.
	r = run(t, socket, nil, false, "remote", "add", "od", "--client-id", "app", "--param", "region=global",
		"--answer", "redirect=http://localhost:53682/?code=c&state=abc", "--answer", "config_type=sharepoint")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.err)
	}
	mu.Lock()
	if len(answers) != 2 || answers[1].Result != "sharepoint" || !finished {
		t.Fatalf("answers %+v", answers)
	}
	answers = nil
	mu.Unlock()

	// Non-interactive without the redirect: a usage error naming the URL.
	r = run(t, socket, nil, false, "remote", "add", "od", "--client-id", "app", "--param", "region=global")
	if r.code != ExitUsage || !strings.Contains(r.err, "--answer redirect=") {
		t.Fatalf("missing redirect: exit %d: %s", r.code, r.err)
	}
}

func TestQueueAndStatusOutput(t *testing.T) {
	total := int64(4 << 30)
	next := time.Now().Add(time.Hour)
	socket := fakeDaemon(t, map[string]api.HandlerFunc{
		"GET /v1/status": func(w http.ResponseWriter, r *http.Request) error {
			return api.WriteJSON(w, 200, apiv1.Status{Node: "pve1", Version: "0.1.0", Healthy: false, Problems: []string{"kit missing"},
				Jobs: apiv1.JobCounts{Queued: 2, Failed: 1}})
		},
		"GET /v1/storages": func(w http.ResponseWriter, r *http.Request) error {
			used := int64(10 << 30)
			return api.WriteJSON(w, 200, []apiv1.Storage{{ID: "offsite", Health: "ok", Active: true, Remote: "od", Path: "pve",
				Source: "lab", Encryption: "crypt", Usage: &apiv1.Usage{Used: &used}}})
		},
		"GET /v1/jobs": func(w http.ResponseWriter, r *http.Request) error {
			if r.URL.Query().Get("state") != "retry_wait,failed" {
				return api.Invalid("state %q", r.URL.Query().Get("state"))
			}
			return api.WriteJSON(w, 200, []apiv1.Job{{ID: 7, State: "retry_wait", Storage: "offsite", VMType: "qemu", VMID: 100,
				ProgressBytes: 1 << 30, TotalBytes: &total, Attempts: 2, NextAttemptAt: &next, LastError: "network\tdown"}})
		},
	})
	r := run(t, socket, nil, false, "status")
	if r.code != 0 || !strings.Contains(r.out, "2 queued") || !strings.Contains(r.out, "kit missing") ||
		!strings.Contains(r.out, "MISSING") || !strings.Contains(r.out, "10.0 GiB") {
		t.Fatalf("status (%d):\n%s%s", r.code, r.out, r.err)
	}
	r = run(t, socket, nil, false, "queue", "list", "--state", "retry_wait,failed")
	if r.code != 0 || !strings.Contains(r.out, "25% of 4.0 GiB") || !strings.Contains(r.out, "qemu/100") || !strings.Contains(r.out, "network down") {
		t.Fatalf("queue (%d):\n%s%s", r.code, r.out, r.err)
	}
	r = run(t, socket, nil, false, "-o", "json", "queue", "list", "--state", "retry_wait,failed")
	var jobs []apiv1.Job
	if err := json.Unmarshal([]byte(r.out), &jobs); err != nil || len(jobs) != 1 || jobs[0].ID != 7 {
		t.Fatalf("json queue: %v %s", err, r.out)
	}
}

func TestSafeWriter(t *testing.T) {
	var buf bytes.Buffer
	w := newSafeWriter(&buf)
	// An escape sequence, a C1 control split across writes, and normal text.
	_, _ = w.Write([]byte("ok \x1b[2J\x1b]0;title\x07 tab\there\r\nnext "))
	_, _ = w.Write([]byte("caf\xc3"))
	_, _ = w.Write([]byte("\xa9 \xc2"))
	_, _ = w.Write([]byte("\x9b31m end\xff\n"))
	if got := buf.String(); got != "ok [2J]0;title tab\there\nnext café 31m end\n" {
		t.Fatalf("sanitized = %q", got)
	}
	if newSafeWriter(w) != w {
		t.Fatal("double wrapping")
	}
}

func TestSecretsStayOffTheCommandLine(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secret")
	_ = os.WriteFile(secretFile, []byte("s3cr3t\n"), 0o600)
	var got apiv1.RemoteSetupRequest
	socket := fakeDaemon(t, map[string]api.HandlerFunc{
		"POST /v1/remote-setup": func(w http.ResponseWriter, r *http.Request) error {
			_ = api.DecodeJSON(r, &got)
			return api.WriteJSON(w, 200, apiv1.RemoteSetup{ID: "s1", Name: got.Name, Status: apiv1.SetupDone})
		},
		"DELETE /v1/remote-setup/s1": func(w http.ResponseWriter, r *http.Request) error { return api.WriteJSON(w, 200, struct{}{}) },
	})
	r := run(t, socket, nil, false, "remote", "add", "od", "--client-id", "app", "--client-secret-file", secretFile,
		"--param", "region=@"+secretFile)
	if r.code != 0 || got.Params["client_secret"] != "s3cr3t" || got.Params["region"] != "s3cr3t" {
		t.Fatalf("exit %d, params %v: %s", r.code, got.Params, r.err)
	}
	if r := run(t, socket, nil, false, "remote", "add", "od", "--client-secret", "x"); r.code != ExitUsage {
		t.Fatalf("--client-secret still accepted: %d", r.code)
	}
	// Escape sequences from the daemon are not printed.
	socket = fakeDaemon(t, map[string]api.HandlerFunc{
		"GET /v1/jobs/{id}": func(w http.ResponseWriter, r *http.Request) error {
			return api.WriteJSON(w, 200, apiv1.JobDetail{Job: apiv1.Job{ID: 1, State: "failed", LastError: "boom\x1b]52;c;cm0gLXJmIH4=\x07"}})
		},
	})
	if r := run(t, socket, nil, false, "queue", "show", "1"); strings.ContainsRune(r.out, 0x1b) || !strings.Contains(r.out, "boom") {
		t.Fatalf("queue show printed %q", r.out)
	}
}

func TestVzdumpHook(t *testing.T) {
	dir := t.TempDir()
	vzdumpConf, hookChain = filepath.Join(dir, "vzdump.conf"), filepath.Join(dir, "state", "vzdump-hook.chain")
	_ = os.WriteFile(vzdumpConf, []byte("# vzdump defaults\nbwlimit: 1000\nscript: /usr/local/bin/my-hook\n"), 0o600)
	var notified apiv1.DiscoveryNotify
	socket := fakeDaemon(t, map[string]api.HandlerFunc{
		"POST /v1/discovery/notify": func(w http.ResponseWriter, r *http.Request) error {
			_ = api.DecodeJSON(r, &notified)
			return api.WriteJSON(w, 202, struct{}{})
		},
	})
	conf := func() string { b, _ := os.ReadFile(vzdumpConf); return string(b) }

	if r := run(t, socket, nil, false, "hook", "status"); !strings.Contains(r.out, "uses the hook /usr/local/bin/my-hook") {
		t.Fatalf("status: %s", r.out)
	}
	if r := run(t, socket, nil, false, "hook", "install", "--yes"); r.code == 0 || !strings.Contains(r.err, "--chain") {
		t.Fatalf("install over another hook (%d): %s", r.code, r.err)
	}
	if r := run(t, socket, nil, false, "hook", "install", "--chain"); r.code != ExitUsage {
		t.Fatalf("install without confirmation: %d", r.code)
	}
	if r := run(t, socket, nil, false, "hook", "install", "--chain", "--yes"); r.code != 0 {
		t.Fatalf("install: %s", r.err)
	}
	if c := conf(); !strings.Contains(c, "bwlimit: 1000") || !strings.Contains(c, "script: "+hookScript) || strings.Contains(c, "my-hook") {
		t.Fatalf("vzdump.conf after install:\n%s", c)
	}
	if r := run(t, socket, nil, false, "hook", "status"); !strings.Contains(r.out, "runs the previous hook /usr/local/bin/my-hook") {
		t.Fatalf("status: %s", r.out)
	}
	if r := run(t, socket, nil, false, "hook", "notify", "--phase", "backup-end", "--path", "/var/lib/vz/dump/vzdump-qemu-100-x.vma.zst"); r.code != 0 ||
		notified.Path != "/var/lib/vz/dump/vzdump-qemu-100-x.vma.zst" || notified.Phase != "backup-end" {
		t.Fatalf("notify (%d): %+v %s", r.code, notified, r.err)
	}
	if r := run(t, socket, nil, false, "hook", "uninstall", "--yes"); r.code != 0 || !strings.Contains(r.out, "my-hook is the hook again") {
		t.Fatalf("uninstall: %s%s", r.out, r.err)
	}
	if c := conf(); !strings.Contains(c, "script: /usr/local/bin/my-hook") || strings.Contains(c, hookScript) {
		t.Fatalf("vzdump.conf after uninstall:\n%s", c)
	}
	if _, err := os.Stat(hookChain); !os.IsNotExist(err) {
		t.Fatal("chain file left")
	}
}
