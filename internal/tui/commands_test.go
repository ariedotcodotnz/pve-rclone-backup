// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
)

func TestCommandFormPreservesArguments(t *testing.T) {
	m := New(t.Context(), Options{})
	c := Command{Path: []string{"storage", "set"}, Arguments: []Argument{
		{Name: "storage", Required: true}, {Name: "property=value", Required: true, Repeated: true},
	}, Flags: []Flag{{Name: "output", Type: "string", Default: "table"}}}
	f := m.commandForm(c, map[string]string{"storage": "offsite"})
	f.fields[1].area.SetValue("rclone-path=Backups with spaces\ncomment=$(touch /tmp/never); 'literal'\nrclone-enabled=0")
	o, cmd := f.finish()
	if o != nil || cmd == nil {
		t.Fatalf("submit: %#v %q", o, f.err)
	}
	msg := cmd().(startCommand)
	want := []string{"storage", "set", "--", "offsite", "rclone-path=Backups with spaces", "comment=$(touch /tmp/never); 'literal'", "rclone-enabled=0"}
	if !reflect.DeepEqual(msg.args, want) {
		t.Fatalf("argv %q, want %q", msg.args, want)
	}

	c = Command{Path: []string{"remote", "add"}, Arguments: []Argument{{Name: "remote", Required: true}},
		Flags: []Flag{{Name: "param", Type: "stringArray"}, {Name: "yes", Type: "bool", Default: "false"}}}
	f = m.commandForm(c, map[string]string{"remote": "existing"})
	if f.fields[0].value() != "" {
		t.Fatal("creation form reuses existing object's name")
	}
	f.fields[0].input.SetValue("-literal")
	f.fields[1].area.SetValue("path=a,b\npassword=@/root/secret file\nempty=")
	f.focus = 2
	_, _ = f.update(m, tea.KeyPressMsg{Code: ' ', Text: " "})
	_, cmd = f.finish()
	msg = cmd().(startCommand)
	want = []string{"remote", "add", "--param=path=a,b", "--param=password=@/root/secret file", "--param=empty=", "--yes=true", "--", "-literal"}
	if !reflect.DeepEqual(msg.args, want) {
		t.Fatalf("argv %q, want %q", msg.args, want)
	}
}

func TestExplicitEmptyAndReset(t *testing.T) {
	m := New(t.Context(), Options{})
	c := Command{Path: []string{"backup", "notes"}, Arguments: []Argument{{Name: "volid", Required: true}}, Flags: []Flag{{Name: "set", Type: "string"}}}
	f := m.commandForm(c, map[string]string{"volid": "offsite:backup/x"})
	f.focus = 1
	ctrlX := tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl}
	_, _ = f.update(m, ctrlX)
	_, cmd := f.finish()
	if got := cmd().(startCommand).args; !reflect.DeepEqual(got, []string{"backup", "notes", "--set=", "--", "offsite:backup/x"}) {
		t.Fatalf("empty notes: %q", got)
	}
	f.fields[1].area.SetValue("new notes")
	_, _ = f.update(m, ctrlX)
	if f.fields[1].included || f.fields[1].value() != "" {
		t.Fatal("reset did not restore the default")
	}
	_, cmd = f.finish()
	if got := cmd().(startCommand).args; !reflect.DeepEqual(got, []string{"backup", "notes", "--", "offsite:backup/x"}) {
		t.Fatalf("default notes: %q", got)
	}
}

func TestActionsSearchAndContext(t *testing.T) {
	m := newModel(t, &fake{h: baseRoutes()})
	m.opts.Commands = []Command{
		{Path: []string{"queue", "priority"}, Summary: "Change job priority", Arguments: []Argument{{Name: "job", Required: true}, {Name: "priority", Required: true}}},
		{Path: []string{"queue", "scan"}, Summary: "Look for archives"},
		{Path: []string{"recover"}, Summary: "Disaster recovery"},
	}
	m.opts.Execute = func(context.Context, []string, io.Writer, AskFunc) int { return 0 }
	press(t, m, "7", "/")
	f := m.overlay.(*form)
	f.fields[0].input.SetValue("queue priority")
	press(t, m, "enter")
	if len(m.actions) != 1 || m.actions[0].name() != "queue priority" {
		t.Fatalf("search: %+v", m.actions)
	}
	press(t, m, "esc")
	if len(m.actions) != 3 {
		t.Fatal("clearing search did not restore actions")
	}
	press(t, m, "3", "A")
	menu := m.overlay.(*commandMenu)
	if len(menu.commands) != 2 || menu.defaults["job"] != "7" {
		t.Fatalf("context: %+v", menu)
	}
	press(t, m, "enter")
	f = m.overlay.(*form)
	if f.fields[0].value() != "7" {
		t.Fatal("selected job was not supplied to priority form")
	}
}

// exerciseCommand runs Tea commands concurrently as the actual program does.
// In particular, a command awaiting an answer must not block the event reader.
func exerciseCommand(t *testing.T, m *Model, onMessage func(tea.Msg)) *commandOutput {
	t.Helper()
	inbox := make(chan tea.Msg, 128)
	var launch func(tea.Cmd)
	launch = func(cmd tea.Cmd) {
		if cmd == nil {
			return
		}
		go func() {
			msg := cmd()
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, c := range batch {
					launch(c)
				}
				return
			}
			if msg != nil {
				select {
				case inbox <- msg:
				case <-t.Context().Done():
				}
			}
		}()
	}
	_, cmd := m.Update(startCommand{"test", []string{"test"}})
	launch(cmd)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case msg := <-inbox:
			_, next := m.Update(msg)
			if onMessage != nil {
				onMessage(msg)
			}
			if _, ok := msg.(commandFinished); ok {
				return m.overlay.(*commandOutput)
			}
			launch(next)
		case <-deadline.C:
			if m.running != nil {
				m.running.cancel()
			}
			t.Fatal("command did not finish")
		}
	}
}

func TestCommandOutputAndPrompts(t *testing.T) {
	m := newModel(t, &fake{h: baseRoutes()})
	secret := " keep surrounding spaces "
	m.opts.Execute = func(_ context.Context, _ []string, out io.Writer, ask AskFunc) int {
		_, _ = fmt.Fprintln(out, "Open https://example.test/sign-in\x1b]0;injected\x07")
		value, err := ask("Client secret: ", true)
		if err != nil || value != secret {
			return 9
		}
		_, _ = fmt.Fprintln(out, "Remote ready")
		return 0
	}
	o := exerciseCommand(t, m, func(msg tea.Msg) {
		if _, ok := msg.(commandQuestion); ok {
			f := m.overlay.(*form)
			if f.fields[0].input.EchoMode != textinput.EchoPassword || !strings.Contains(f.intro, "https://example.test/sign-in") {
				t.Fatal("prompt lacks context or secret masking")
			}
			f.fields[0].input.SetValue(secret)
			if strings.Contains(f.view(80, 12), secret) {
				t.Fatal("secret rendered")
			}
			_, answer := f.finish()
			_, _ = m.Update(answer())
		}
	})
	if !o.finished || o.code != 0 || !strings.Contains(o.info.text, "Remote ready") || strings.Contains(o.info.text, secret) || strings.ContainsAny(o.info.text, "\x1b\x07") {
		t.Fatalf("output: %+v %q", o, o.info.text)
	}
	if m.busy != "" || m.running != nil {
		t.Fatal("execution state was not cleared")
	}
	press(t, m, "esc")
	if m.overlay != nil {
		t.Fatal("completed command output did not close")
	}
}

func TestCommandCancellationAtPrompt(t *testing.T) {
	m := newModel(t, &fake{h: baseRoutes()})
	m.opts.Execute = func(ctx context.Context, _ []string, _ io.Writer, ask AskFunc) int {
		_, err := ask("Confirm? [y/N]", false)
		if err == nil || ctx.Err() == nil {
			return 9
		}
		return 1
	}
	o := exerciseCommand(t, m, func(msg tea.Msg) {
		if _, ok := msg.(commandQuestion); ok {
			_, _ = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
		}
	})
	if o.code != 1 || !o.finished {
		t.Fatalf("cancelled: %+v", o)
	}
}

func TestConsecutiveCommandPrompts(t *testing.T) {
	m := newModel(t, &fake{h: baseRoutes()})
	m.opts.Execute = func(_ context.Context, _ []string, _ io.Writer, ask AskFunc) int {
		for _, question := range []string{"First", "Second", "Third"} {
			value, err := ask(question, false)
			if err != nil || value != question {
				return 9
			}
		}
		return 0
	}
	questions := 0
	o := exerciseCommand(t, m, func(msg tea.Msg) {
		if q, ok := msg.(commandQuestion); ok {
			questions++
			f, ok := m.overlay.(*form)
			if !ok || !strings.Contains(f.intro, q.question) {
				t.Fatal("a consecutive prompt was hidden")
			}
			f.fields[0].input.SetValue(q.question)
			_, answer := f.finish()
			_, _ = m.Update(answer())
		}
	})
	if questions != 3 || o.code != 0 {
		t.Fatalf("prompts %d exit %d", questions, o.code)
	}
}

func TestTerminalSizesAndScrolling(t *testing.T) {
	m := newModel(t, &fake{h: baseRoutes()})
	var fields []*field
	for i := 0; i < 18; i++ {
		fields = append(fields, newField(fmt.Sprint(i), fmt.Sprintf("Field %02d", i), "", false, false))
	}
	for _, size := range [][2]int{{80, 24}, {40, 16}, {160, 40}} {
		_, _ = m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for tab := tabDashboard; tab <= tabActions; tab++ {
			m.switchTab(tab)
			view := m.render()
			if lipgloss.Width(view) > size[0] || lipgloss.Height(view) > size[1] {
				t.Fatalf("%v tab %d does not fit (%dx%d)", size, tab, lipgloss.Width(view), lipgloss.Height(view))
			}
		}
		f := newForm("Large form", "Instructions", fields, func(map[string]string) (tea.Cmd, string) { return nil, "" })
		m.overlay = f
		_ = m.render()
		f.focus = len(fields) - 1
		view := m.render()
		if !strings.Contains(view, "Field 17") || lipgloss.Height(view) > size[1] {
			t.Fatalf("focused field is hidden at %v:\n%s", size, view)
		}
		m.overlay = newInfo("Long output", strings.Repeat("line with text\n", 70)+"LAST LINE")
		_ = m.render()
		_, _ = m.overlay.update(m, tea.KeyPressMsg{Code: tea.KeyEnd})
		if !strings.Contains(m.render(), "LAST LINE") {
			t.Fatal("end did not reach final output line")
		}
		m.overlay = nil
	}
	if m.View().MouseMode != tea.MouseModeNone {
		t.Fatal("TUI enables mouse reporting")
	}
}

func TestActionsAvailableWithoutDaemon(t *testing.T) {
	m := New(t.Context(), Options{Commands: []Command{{Path: []string{"recovery-kit", "show"}, Summary: "Show a kit offline"}}})
	m.loadErr = fmt.Errorf("daemon offline")
	m.switchTab(tabActions)
	if view := m.render(); !strings.Contains(view, "recovery-kit show") || strings.Contains(view, "Cannot reach the daemon") {
		t.Fatalf("offline actions:\n%s", view)
	}
}

func TestCancelConfirmationDoesNotLeaveBusyState(t *testing.T) {
	m := newModel(t, &fake{h: baseRoutes()})
	press(t, m, "2", "d", "n")
	if m.busy != "" {
		t.Fatal("declining a confirmation left the TUI busy")
	}
}

func TestRefreshKeepsSelectedObject(t *testing.T) {
	m := newModel(t, &fake{h: baseRoutes()})
	press(t, m, "3")
	msg := refreshMsg{status: m.status, storages: m.storages, backups: m.backups, remotes: m.remotes, jobs: append([]apiv1.Job{{ID: 20}}, m.jobs...)}
	_, _ = m.Update(msg)
	j, ok := m.selectedJob()
	if !ok || j.ID != 7 {
		t.Fatalf("selection changed during refresh: %+v", j)
	}
}

func TestSaveOutputDoesNotOverwriteFiles(t *testing.T) {
	m := newModel(t, &fake{h: baseRoutes()})
	o := &commandOutput{info: newInfo("Results", "line one\nline two"), finished: true}
	m.overlay = o
	file := filepath.Join(t.TempDir(), "results.txt")
	press(t, m, "s")
	f := m.overlay.(*form)
	f.fields[0].input.SetValue(file)
	press(t, m, "enter")
	data, err := os.ReadFile(file)
	if err != nil || string(data) != "line one\nline two\n" {
		t.Fatalf("saved output: %q %v", data, err)
	}
	stat, err := os.Stat(file)
	if err != nil || stat.Mode().Perm() != 0o600 {
		t.Fatalf("permissions: %v %v", stat, err)
	}
	press(t, m, "s")
	f = m.overlay.(*form)
	f.fields[0].input.SetValue(file)
	press(t, m, "enter")
	if !m.flashErr || !strings.Contains(m.flash, "exists") {
		t.Fatalf("overwrite not refused: %q", m.flash)
	}
	data, _ = os.ReadFile(file)
	if string(data) != "line one\nline two\n" {
		t.Fatal("existing output file changed")
	}
}
