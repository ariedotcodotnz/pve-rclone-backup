// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/client"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/recoverykit"
)

// fake is a scripted daemon that records mutating requests.
type fake struct {
	mu    sync.Mutex
	calls []string
	h     map[string]api.HandlerFunc
}

func (f *fake) record(r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
}

func (f *fake) called(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == call {
			return true
		}
	}
	return false
}

func serve(t *testing.T, f *fake) *client.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "prbtui")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	srv := api.New(api.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), AllowUIDs: []uint32{uint32(os.Getuid())}})
	for pattern, h := range f.h {
		srv.Handle(pattern, func(w http.ResponseWriter, r *http.Request) error { f.record(r); return h(w, r) })
	}
	socket := filepath.Join(dir, "api.sock")
	l, err := api.Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, l); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return client.New(socket)
}

func ok(v any) api.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) error { return api.WriteJSON(w, 200, v) }
}

// baseRoutes serve a daemon with one storage, a backup and a job.
func baseRoutes() map[string]api.HandlerFunc {
	used := int64(5 << 30)
	total := int64(4 << 30)
	return map[string]api.HandlerFunc{
		"GET /v1/status": ok(apiv1.Status{Node: "pve1", Version: "0.1.0", Healthy: false,
			Alerts: []apiv1.Alert{{ID: "a", Severity: "error", Message: "remote od needs reconnecting\x1b[2J"}}, Jobs: apiv1.JobCounts{Queued: 1}}),
		"GET /v1/storages": ok([]apiv1.Storage{{ID: "offsite", Health: "ok", Active: true, Remote: "od", Path: "pve-backups",
			Source: "lab", Encryption: "crypt", KitConfirmed: true, Usage: &apiv1.Usage{Used: &used}}}),
		"GET /v1/storages/offsite/backups": ok([]apiv1.Backup{{Volname: "backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst",
			VMType: "qemu", VMID: 100, CTime: 1790992801, Size: 10 << 20, State: "complete", VerifyLevel: 2, Notes: "web\x1b]0;evil\x07"}}),
		"GET /v1/jobs": ok([]apiv1.Job{{ID: 7, Kind: "replicate", State: "uploading", Storage: "offsite", VMType: "qemu", VMID: 100,
			ProgressBytes: 1 << 30, TotalBytes: &total, Volname: "backup/x"}}),
		"GET /v1/remotes": ok([]apiv1.Remote{{Name: "od", Type: "onedrive", Supported: true, Authorized: true, Storages: []string{"offsite"}}}),
	}
}

// drive runs commands and feeds their messages to the model, dropping
// timer ticks and quitting.
func drive(t *testing.T, m *Model, cmd tea.Cmd) {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for n := 0; len(queue) > 0; n++ {
		if n > 200 {
			t.Fatal("too many commands")
		}
		c := queue[0]
		queue = queue[1:]
		if c == nil {
			continue
		}
		switch msg := c().(type) {
		case tea.BatchMsg:
			queue = append(queue, msg...)
		case tickMsg, tea.QuitMsg, nil:
		default:
			_, next := m.Update(msg)
			queue = append(queue, next)
		}
	}
}

func press(t *testing.T, m *Model, keys ...string) {
	t.Helper()
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "tab":
			msg = tea.KeyPressMsg{Code: tea.KeyTab}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		default:
			msg = tea.KeyPressMsg{Code: rune(k[0]), Text: k}
		}
		_, cmd := m.Update(msg)
		drive(t, m, cmd)
	}
}

func newModel(t *testing.T, f *fake) *Model {
	t.Helper()
	m := New(t.Context(), Options{Client: serve(t, f), NoEvents: true, Refresh: time.Millisecond,
		PVESH: func(_ context.Context, args ...string) ([]byte, error) {
			f.mu.Lock()
			f.calls = append(f.calls, "pvesh "+strings.Join(args, " "))
			f.mu.Unlock()
			return nil, nil
		}})
	_, _ = m.Update(tea.WindowSizeMsg{Width: 160, Height: 40})
	drive(t, m, m.Init())
	return m
}

func TestDashboardAndTabs(t *testing.T) {
	m := newModel(t, &fake{h: baseRoutes()})
	view := m.render()
	for _, want := range []string{"Daemon 0.1.0 on pve1", "1 queued", "remote od needs reconnecting", "offsite", "5.0 GiB", "#7 replicate"} {
		if !strings.Contains(view, want) {
			t.Errorf("dashboard lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "\x1b[2J") {
		t.Error("escape sequence from the daemon rendered")
	}
	press(t, m, "2")
	view = m.render()
	if !strings.Contains(view, "offsite:backup/vzdump-qemu-100") || !strings.Contains(view, "10.0 MiB") {
		t.Fatalf("backups view:\n%s", view)
	}
	if strings.Contains(view, "\x1b]0;evil") || strings.ContainsRune(view, 0x07) {
		t.Fatal("control characters from notes rendered")
	}
	press(t, m, "tab")
	if view := m.render(); !strings.Contains(view, "25% of 4.0 GiB") {
		t.Fatalf("queue view:\n%s", view)
	}
	press(t, m, "?")
	if _, ok := m.overlay.(*info); !ok {
		t.Fatal("help not shown")
	}
	press(t, m, "esc")
	if m.overlay != nil {
		t.Fatal("help not closed")
	}
}

func TestDaemonDown(t *testing.T) {
	m := New(t.Context(), Options{Client: client.New(filepath.Join(t.TempDir(), "none.sock")), NoEvents: true, Refresh: time.Millisecond})
	drive(t, m, m.Init())
	if !strings.Contains(m.render(), "Cannot reach the daemon") {
		t.Fatalf("view:\n%s", m.render())
	}
}

func TestBackupActions(t *testing.T) {
	r := baseRoutes()
	b := "/v1/storages/offsite/backups/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst"
	r["PATCH "+b] = ok(apiv1.Backup{})
	r["DELETE "+b] = ok(apiv1.Backup{})
	r["POST "+b+"/verify"] = ok(apiv1.Job{ID: 8})
	r["POST /v1/restores"] = func(w http.ResponseWriter, req *http.Request) error {
		var rr apiv1.RestoreRequest
		_ = api.DecodeJSON(req, &rr)
		if rr.TargetVMID != 900 || rr.Mode != "stream" || rr.Volname != "backup/vzdump-qemu-100-2026_10_04-02_00_01.vma.zst" {
			return api.Invalid("request %+v", rr)
		}
		return api.WriteJSON(w, 202, apiv1.Job{ID: 9})
	}
	f := &fake{h: r}
	m := newModel(t, f)
	press(t, m, "2", "p")
	if !f.called("PATCH " + b) {
		t.Fatal("protect not sent")
	}
	press(t, m, "d")
	if _, ok := m.overlay.(*confirm); !ok {
		t.Fatal("delete not confirmed first")
	}
	press(t, m, "n")
	if f.called("DELETE " + b) {
		t.Fatal("deleted without confirmation")
	}
	press(t, m, "d", "y")
	if !f.called("DELETE " + b) {
		t.Fatal("delete not sent")
	}
	press(t, m, "v")
	if !f.called("POST "+b+"/verify") || !strings.Contains(m.flash, "verification") {
		t.Fatalf("verify: %q", m.flash)
	}
	press(t, m, "R")
	fm, ok := m.overlay.(*form)
	if !ok {
		t.Fatal("restore form not shown")
	}
	fm.fields[0].input.SetValue("900")
	press(t, m, "enter", "enter", "enter", "enter")
	if !f.called("POST /v1/restores") || m.tab != tabQueue || !strings.Contains(m.flash, "Restore job 9") {
		t.Fatalf("restore: tab %d flash %q", m.tab, m.flash)
	}
}

func TestRemoteSetupWizard(t *testing.T) {
	r := baseRoutes()
	var answers []apiv1.RemoteSetupAnswer
	r["POST /v1/remote-setup"] = func(w http.ResponseWriter, req *http.Request) error {
		var sr apiv1.RemoteSetupRequest
		_ = api.DecodeJSON(req, &sr)
		if sr.Name != "od2" || sr.Params["client_id"] != "app" {
			return api.Invalid("request %+v", sr)
		}
		return api.WriteJSON(w, 200, apiv1.RemoteSetup{ID: "s1", Name: "od2", Status: apiv1.SetupQuestion, State: "q1",
			Option: &apiv1.SetupOption{Name: "config_is_local"}})
	}
	r["POST /v1/remote-setup/s1/answer"] = func(w http.ResponseWriter, req *http.Request) error {
		var a apiv1.RemoteSetupAnswer
		_ = api.DecodeJSON(req, &a)
		answers = append(answers, a)
		if a.State == "q1" {
			return api.WriteJSON(w, 200, apiv1.RemoteSetup{ID: "s1", Name: "od2", Status: apiv1.SetupAuthorizing, AuthURL: "https://login.example/a?state=x"})
		}
		return api.WriteJSON(w, 200, apiv1.RemoteSetup{ID: "s1", Name: "od2", Status: apiv1.SetupDone})
	}
	r["POST /v1/remote-setup/s1/oauth-redirect"] = ok(apiv1.RemoteSetup{ID: "s1", Name: "od2", Status: apiv1.SetupQuestion, State: "q2",
		Option: &apiv1.SetupOption{Name: "config_type", Help: "Type of connection", Examples: []apiv1.SetupExample{{Value: "onedrive"}}}})
	r["DELETE /v1/remote-setup/s1"] = ok(struct{}{})
	f := &fake{h: r}
	m := newModel(t, f)
	press(t, m, "4", "a")
	fm := m.overlay.(*form)
	fm.fields[0].input.SetValue("od2")
	fm.fields[1].input.SetValue("app")
	press(t, m, "enter", "enter", "enter")
	fm, ok := m.overlay.(*form)
	if !ok || !strings.Contains(fm.view(100), "https://login.example/a?state=x") {
		t.Fatalf("authorization step: %#v", m.overlay)
	}
	fm.fields[0].input.SetValue("http://localhost:53682/?code=c&state=x")
	press(t, m, "enter")
	fm = m.overlay.(*form)
	if !strings.Contains(fm.view(100), "1) onedrive") {
		t.Fatalf("question step:\n%s", fm.view(100))
	}
	fm.fields[0].input.SetValue("1")
	press(t, m, "enter")
	if len(answers) != 2 || answers[0].Result != "true" || answers[1].Result != "onedrive" || !f.called("DELETE /v1/remote-setup/s1") ||
		!strings.Contains(m.flash, "od2 is ready") {
		t.Fatalf("answers %+v, flash %q", answers, m.flash)
	}
}

func TestStorageInitWizard(t *testing.T) {
	r := baseRoutes()
	kit, sum := recoveryKit(t)
	r["POST /v1/storages/vault/init"] = ok(apiv1.StorageInitResponse{RepoUUID: "6f0c2f1e-3a7b-4c2d-9e8f-0123456789ab", Created: true, Encryption: "crypt", KitRequired: true})
	r["POST /v1/recovery-kit/export"] = ok(apiv1.KitExportResponse{Kit: kit, Checksum: sum})
	r["POST /v1/recovery-kit/confirm"] = ok(apiv1.KitConfirmResponse{Repos: []string{"x"}})
	f := &fake{h: r}
	m := newModel(t, f)
	press(t, m, "5", "i")
	fm := m.overlay.(*form)
	fm.fields[0].input.SetValue("vault")
	for range fm.fields {
		press(t, m, "enter")
	}
	kf, ok := m.overlay.(*form)
	if !ok || kf.title != "Export a recovery kit" {
		t.Fatalf("kit step: %#v", m.overlay)
	}
	file := filepath.Join(t.TempDir(), "kit.txt")
	kf.fields[0].input.SetValue(file)
	press(t, m, "enter", "enter", "enter")
	cf, ok := m.overlay.(*form)
	if !ok || !strings.Contains(cf.view(100), sum) {
		t.Fatalf("confirm step: %#v", m.overlay)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("kit file: %v %v", fi, err)
	}
	cf.fields[0].input.SetValue("0000")
	press(t, m, "enter")
	if !strings.Contains(cf.err, "does not match") || f.called("POST /v1/recovery-kit/confirm") {
		t.Fatal("wrong checksum accepted")
	}
	cf.fields[0].input.SetValue(sum)
	press(t, m, "enter")
	f.mu.Lock()
	calls := strings.Join(f.calls, "\n")
	f.mu.Unlock()
	if !strings.Contains(calls, "POST /v1/recovery-kit/confirm") ||
		!strings.Contains(calls, "pvesh create /storage --storage vault --type rclone-backup --rclone-remote od") ||
		!strings.Contains(calls, "--rclone-replicate-from local") || m.tab != tabStorages {
		t.Fatalf("calls:\n%s\nflash %q", calls, m.flash)
	}
}

func recoveryKit(t *testing.T) (string, string) {
	t.Helper()
	text, sum, err := recoverykit.Encode(recoverykit.New("pve1", time.Now()), "")
	if err != nil {
		t.Fatal(err)
	}
	return text, sum
}
