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
	"charm.land/bubbles/v2/viewport"
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
	// Commands and Execute expose the CLI's complete command tree in forms.
	Commands []Command
	Execute  ExecuteFunc
}

// Run shows the TUI until the user quits.
func Run(ctx context.Context, opts Options) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
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
	tabActions
)

var tabNames = []string{"Dashboard", "Backups", "Queue", "Remotes", "Storages", "Diagnostics", "Actions"}

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

	loadErr       error
	flash         string
	flashErr      bool
	busy          string
	refreshing    bool
	overlay       overlay
	events        <-chan apiv1.Event
	actions       []Command
	actionsQuery  string
	running       *commandRun
	dashboardPort viewport.Model
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
	m.dashboardPort = viewport.New()
	m.dashboardPort.SoftWrap = true
	m.dashboardPort.MouseWheelEnabled = false
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
	startActionMsg struct {
		busy string
		run  tea.Cmd
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
	ctx := m.ctx
	return func() tea.Msg { return startActionMsg{busy, func() tea.Msg { return fn(ctx) }} }
}

// Update implements tea.Model.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if handled, cmd := m.commandMessage(msg); handled {
		return m, cmd
	}
	if k, ok := msg.(tea.KeyPressMsg); ok && k.String() == "ctrl+c" {
		if m.running != nil {
			m.running.cancel()
			m.overlay = m.running.output
			return m, nil
		}
		return m, tea.Quit
	}
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.rebuildTable()
		return m, nil
	case refreshMsg:
		selection := m.selectionID()
		m.refreshing = false
		m.loadErr = msg.err
		if msg.err == nil {
			m.status, m.storages, m.backups, m.jobs, m.remotes = msg.status, msg.storages, msg.backups, msg.jobs, msg.remotes
		}
		m.rebuildTable()
		m.restoreSelection(selection)
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
	case startActionMsg:
		if m.busy != "" {
			return m, nil
		}
		m.busy = msg.busy
		return m, msg.run
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
	case "1", "2", "3", "4", "5", "6", "7":
		m.switchTab(tab(k.String()[0] - '1'))
		return m.tabEntered()
	case "ctrl+r", "f5":
		return m.refresh()
	case "?":
		m.overlay = newInfo("Keys", helpText())
		return nil
	}
	if m.busy != "" {
		return nil
	}
	if k.String() == ":" {
		m.switchTab(tabActions)
		return nil
	}
	if k.String() == "A" && m.opts.Execute != nil {
		m.overlay = m.contextActions()
		return nil
	}
	if cmd, ok := m.tabKey(k.String()); ok {
		return cmd
	}
	var cmd tea.Cmd
	if m.tab == tabDashboard {
		if k.String() == "home" {
			m.dashboardPort.GotoTop()
			return nil
		}
		if k.String() == "end" {
			m.dashboardPort.GotoBottom()
			return nil
		}
		m.dashboardPort, cmd = m.dashboardPort.Update(k)
		return cmd
	}
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
	v.MouseMode = tea.MouseModeNone
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
	header := m.header(tabs)
	var body string
	switch {
	case m.loadErr != nil && m.tab != tabActions:
		body = errStyle.Render("Cannot reach the daemon: "+cleanLine(m.loadErr.Error())) + "\n\n" +
			dimStyle.Render("Is pve-rclone-backupd running? (systemctl status pve-rclone-backupd)")
	case m.tab == tabDashboard:
		m.dashboardPort.SetWidth(max(1, m.width))
		m.dashboardPort.SetHeight(m.bodyHeight())
		m.dashboardPort.SetContent(m.dashboard())
		body = m.dashboardPort.View()
	case m.tab == tabActions && len(m.actions) == 0:
		body = "No matching actions. Press / to change the search."
	default:
		m.tbl.SetHeight(m.bodyHeight())
		body = m.tbl.View()
	}
	if m.overlay != nil {
		body = m.overlay.view(m.width, m.bodyHeight())
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
	if m.tab == tabActions && m.overlay == nil && m.actionsQuery != "" {
		line = dimStyle.Render("Search: " + cleanLine(m.actionsQuery))
	}
	if m.width < 20 || m.height < 10 {
		return "Terminal too small. Resize to at least 20 columns and 10 rows. Ctrl+c quits."
	}
	footer = lipgloss.Wrap(footer, max(1, m.width), " ")
	return lipgloss.NewStyle().MaxWidth(m.width).MaxHeight(m.height).Render(header + "\n\n" + body + "\n\n" + line + "\n" + footer)
}

func (m *Model) header(tabs []string) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("pve-rclone-backup"))
	lineWidth := lipgloss.Width("pve-rclone-backup") + 2
	if m.width < 130 {
		b.WriteByte('\n')
		lineWidth = 0
	} else {
		b.WriteString("  ")
	}
	for _, label := range tabs {
		w := lipgloss.Width(label)
		if lineWidth > 0 && lineWidth+w > m.width {
			b.WriteByte('\n')
			lineWidth = 0
		}
		b.WriteString(label)
		lineWidth += w
	}
	return b.String()
}

func (m *Model) bodyHeight() int {
	var tabs []string
	for i, name := range tabNames {
		tabs = append(tabs, fmt.Sprintf(" %d %s ", i+1, name))
	}
	footerHeight := lipgloss.Height(lipgloss.Wrap(m.keysHint(), max(1, m.width), " "))
	return max(1, m.height-lipgloss.Height(m.header(tabs))-5-footerHeight)
}

func (m *Model) keysHint() string {
	if m.overlay != nil {
		return m.overlay.hint()
	}
	common := "tab/1-7 views · A actions · : all actions · ctrl+r refresh · ? keys · q quit"
	if h := tabHints[m.tab]; h != "" {
		return h + "\n" + common
	}
	return common
}

func helpText() string {
	var b strings.Builder
	b.WriteString("tab, shift+tab, 1-7   switch views\nctrl+r / F5           refresh\nup/down, pgup/pgdown  select or scroll\nA                     actions for the selected row\n: / 7                 all CLI actions (including recovery and configuration)\nq / ctrl+c            quit (ctrl+c stops a running command first)\n\nForms: tab/shift+tab select field; ctrl+s submits; space toggles booleans.\nEdit an option to set it; ctrl+x explicitly sets an empty value or resets it.\nMultiline/repeated fields accept newlines; enter advances single-line fields.\nDuring a command: esc stops it or stops waiting for the daemon job.\n\n")
	for i, name := range tabNames {
		if h := tabHints[tab(i)]; h != "" {
			fmt.Fprintf(&b, "%-12s %s\n", name, h)
		}
	}
	return b.String()
}
