// SPDX-License-Identifier: AGPL-3.0-or-later

// Package tui is the interactive terminal interface of pve-rclone-backup.
// Like the CLI it is a client of the daemon API and keeps no state of its
// own: it shows the daemon's status, storages, backups, queue, remotes and
// diagnostics, refreshed on daemon events, and runs the setup and restore
// wizards.
package tui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/client"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/doctor"
)

// Options configures the TUI.
type Options struct {
	Client *client.Client
	// PVESH runs pvesh (replaced in tests).
	PVESH func(ctx context.Context, args ...string) ([]byte, error)
	// NoEvents disables the event subscription (tests).
	NoEvents bool
	// Refresh is the polling interval (default 5s).
	Refresh time.Duration
}

// Run shows the TUI until the user quits.
func Run(ctx context.Context, opts Options) error {
	_, err := tea.NewProgram(New(ctx, opts), tea.WithContext(ctx)).Run()
	return err
}

type tab int

const (
	tabDashboard tab = iota
	tabBackups
	tabQueue
	tabRemotes
	tabStorages
	tabDoctor
)

var tabNames = []string{"Dashboard", "Backups", "Queue", "Remotes", "Storages", "Diagnostics"}

// backupRow is a backup with its storage.
type backupRow struct {
	Storage string
	apiv1.Backup
}

func (b backupRow) volid() string { return b.Storage + ":" + b.Volname }

// Model is the TUI state.
type Model struct {
	ctx  context.Context
	c    *client.Client
	opts Options

	tab           tab
	width, height int
	tbl           table.Model
	tblTab        tab

	status   *apiv1.Status
	storages []apiv1.Storage
	backups  []backupRow
	jobs     []apiv1.Job
	remotes  []apiv1.Remote
	checks   []doctor.Check

	loadErr    error
	flash      string
	flashErr   bool
	busy       string
	refreshing bool
	overlay    overlay
	events     <-chan apiv1.Event
}

// New returns the initial model.
func New(ctx context.Context, opts Options) *Model {
	if opts.Refresh == 0 {
		opts.Refresh = 5 * time.Second
	}
	if opts.PVESH == nil {
		opts.PVESH = func(ctx context.Context, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, "pvesh", args...).CombinedOutput() //nolint:gosec // argv only, no shell
		}
	}
	m := &Model{ctx: ctx, c: opts.Client, opts: opts, width: 100, height: 30}
	m.tbl = table.New(table.WithFocused(true))
	m.tbl.SetStyles(tableStyles())
	return m
}

// Messages.
type (
	refreshMsg struct {
		status   *apiv1.Status
		storages []apiv1.Storage
		backups  []backupRow
		jobs     []apiv1.Job
		remotes  []apiv1.Remote
		err      error
	}
	checksMsg  []doctor.Check
	tickMsg    struct{}
	eventMsg   apiv1.Event
	eventsGone struct{}
	// doneMsg reports a finished action; next opens another overlay.
	doneMsg struct {
		text string
		err  error
		next overlay
		tab  *tab
	}
)

// Init implements tea.Model.
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.refresh(), m.tick()}
	if !m.opts.NoEvents {
		cmds = append(cmds, m.subscribe())
	}
	return tea.Batch(cmds...)
}

func (m *Model) tick() tea.Cmd {
	return tea.Tick(m.opts.Refresh, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *Model) subscribe() tea.Cmd {
	ch, _ := m.c.Events(m.ctx)
	m.events = ch
	return m.nextEvent()
}

func (m *Model) nextEvent() tea.Cmd {
	ch := m.events
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return eventsGone{}
		}
		return eventMsg(e)
	}
}

// refresh loads everything the views show.
func (m *Model) refresh() tea.Cmd {
	m.refreshing = true
	c, ctx := m.c, m.ctx
	return func() tea.Msg {
		var r refreshMsg
		st, err := c.Status(ctx)
		if err != nil {
			return refreshMsg{err: err}
		}
		r.status = st
		get := func(path string, out any) {
			if r.err == nil {
				r.err = c.Do(ctx, http.MethodGet, path, nil, out)
			}
		}
		get("/v1/storages", &r.storages)
		get("/v1/jobs?limit=300", &r.jobs)
		get("/v1/remotes", &r.remotes)
		for _, s := range r.storages {
			var list []apiv1.Backup
			get("/v1/storages/"+url.PathEscape(s.ID)+"/backups?state=all", &list)
			for _, b := range list {
				r.backups = append(r.backups, backupRow{s.ID, b})
			}
		}
		slices.SortStableFunc(r.backups, func(a, b backupRow) int { return int(b.CTime - a.CTime) })
		return r
	}
}

func (m *Model) loadChecks() tea.Cmd {
	c, ctx := m.c, m.ctx
	return func() tea.Msg { return checksMsg(doctor.Run(ctx, c)) }
}

// action runs fn in the background and reports its outcome.
func (m *Model) action(busy string, fn func(ctx context.Context) doneMsg) tea.Cmd {
	m.busy = busy
	ctx := m.ctx
	return func() tea.Msg { return fn(ctx) }
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.rebuildTable()
		return m, nil
	case refreshMsg:
		m.refreshing = false
		m.loadErr = msg.err
		if msg.err == nil {
			m.status, m.storages, m.backups, m.jobs, m.remotes = msg.status, msg.storages, msg.backups, msg.jobs, msg.remotes
		}
		m.rebuildTable()
		return m, nil
	case checksMsg:
		m.checks = msg
		m.rebuildTable()
		return m, nil
	case tickMsg:
		cmds := []tea.Cmd{m.tick()}
		if !m.refreshing {
			cmds = append(cmds, m.refresh())
		}
		return m, tea.Batch(cmds...)
	case eventMsg:
		cmds := []tea.Cmd{m.nextEvent()}
		if !m.refreshing {
			cmds = append(cmds, m.refresh())
		}
		return m, tea.Batch(cmds...)
	case eventsGone:
		// Reconnect later (the daemon may have restarted).
		return m, tea.Tick(5*time.Second, func(time.Time) tea.Msg { return resubscribe{} })
	case resubscribe:
		return m, m.subscribe()
	case doneMsg:
		m.busy = ""
		if msg.tab != nil {
			m.switchTab(*msg.tab)
		}
		m.flash, m.flashErr = msg.text, msg.err != nil
		if msg.err != nil {
			m.flash = "Error: " + cleanLine(msg.err.Error())
		}
		if msg.next != nil {
			m.overlay = msg.next
		}
		return m, m.refresh()
	}
	if m.overlay != nil {
		next, cmd := m.overlay.update(m, msg)
		m.overlay = next
		return m, cmd
	}
	if key, ok := msg.(tea.KeyPressMsg); ok {
		return m, m.handleKey(key)
	}
	return m, nil
}

type resubscribe struct{}

func (m *Model) switchTab(t tab) {
	m.tab = t
	m.flash = ""
	m.rebuildTable()
}

func (m *Model) handleKey(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "q", "ctrl+c":
		return tea.Quit
	case "tab", "right", "l":
		m.switchTab((m.tab + 1) % tab(len(tabNames)))
		return m.tabEntered()
	case "shift+tab", "left", "h":
		m.switchTab((m.tab + tab(len(tabNames)) - 1) % tab(len(tabNames)))
		return m.tabEntered()
	case "1", "2", "3", "4", "5", "6":
		m.switchTab(tab(k.String()[0] - '1'))
		return m.tabEntered()
	case "ctrl+r", "f5":
		return m.refresh()
	case "?":
		m.overlay = newInfo("Keys", helpText())
		return nil
	}
	if cmd, ok := m.tabKey(k.String()); ok {
		return cmd
	}
	var cmd tea.Cmd
	m.tbl, cmd = m.tbl.Update(k)
	return cmd
}

func (m *Model) tabEntered() tea.Cmd {
	if m.tab == tabDoctor && m.checks == nil {
		return m.loadChecks()
	}
	return nil
}

// Styles.
var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	activeTab     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("39")).Padding(0, 1)
	inactiveTab   = lipgloss.NewStyle().Foreground(lipgloss.Color("245")).Padding(0, 1)
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	warnStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	errStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	selectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("39"))
)

func tableStyles() table.Styles {
	s := table.DefaultStyles()
	s.Header = s.Header.Foreground(lipgloss.Color("39"))
	s.Selected = selectedStyle
	return s
}

// View implements tea.Model.
func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "pve-rclone-backup"
	return v
}

func (m *Model) render() string {
	var tabs []string
	for i, name := range tabNames {
		label := fmt.Sprintf("%d %s", i+1, name)
		if tab(i) == m.tab {
			tabs = append(tabs, activeTab.Render(label))
		} else {
			tabs = append(tabs, inactiveTab.Render(label))
		}
	}
	header := lipgloss.JoinHorizontal(lipgloss.Top, titleStyle.Render("pve-rclone-backup ")+" ", strings.Join(tabs, ""))
	var body string
	switch {
	case m.loadErr != nil:
		body = errStyle.Render("Cannot reach the daemon: "+cleanLine(m.loadErr.Error())) + "\n\n" +
			dimStyle.Render("Is pve-rclone-backupd running? (systemctl status pve-rclone-backupd)")
	case m.tab == tabDashboard:
		body = m.dashboard()
	default:
		body = m.tbl.View()
	}
	if m.overlay != nil {
		body = m.overlay.view(m.width)
	}
	footer := dimStyle.Render(m.keysHint())
	line := ""
	switch {
	case m.busy != "":
		line = warnStyle.Render(m.busy + "…")
	case m.flash != "" && m.flashErr:
		line = errStyle.Render(m.flash)
	case m.flash != "":
		line = okStyle.Render(m.flash)
	}
	return header + "\n\n" + body + "\n\n" + line + "\n" + footer
}

func (m *Model) keysHint() string {
	if m.overlay != nil {
		return m.overlay.hint()
	}
	common := "tab/1-6 switch · ctrl+r refresh · ? keys · q quit"
	if h := tabHints[m.tab]; h != "" {
		return h + " · " + common
	}
	return common
}

func helpText() string {
	var b strings.Builder
	b.WriteString("tab, shift+tab, 1-6   switch views\nctrl+r                refresh\nup/down               select\nq                     quit\n\n")
	for i, name := range tabNames {
		if h := tabHints[tab(i)]; h != "" {
			fmt.Fprintf(&b, "%-12s %s\n", name, h)
		}
	}
	return b.String()
}
