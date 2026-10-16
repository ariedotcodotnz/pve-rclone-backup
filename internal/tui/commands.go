// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	tf "github.com/ariedotcodotnz/pve-rclone-backup/internal/textfmt"
)

// Command describes an executable CLI command, supplied by the CLI package so
// that the TUI does not duplicate the command tree or import it in a cycle.
type Command struct {
	Path          []string
	Summary, Help string
	Arguments     []Argument
	Flags         []Flag
}

type Argument struct {
	Name               string
	Required, Repeated bool
}

type Flag struct {
	Name, Help, Type, Default string
	Required                  bool
}

type AskFunc func(question string, secret bool) (string, error)
type ExecuteFunc func(ctx context.Context, args []string, output io.Writer, ask AskFunc) int

func (c Command) name() string { return strings.Join(c.Path, " ") }

func (m *Model) filterCommands(query string) []Command {
	var out []Command
	for _, c := range m.opts.Commands {
		haystack := strings.ToLower(c.name() + " " + c.Summary)
		match := true
		for _, word := range strings.Fields(strings.ToLower(query)) {
			if !strings.Contains(haystack, word) {
				match = false
				break
			}
		}
		if match {
			out = append(out, c)
		}
	}
	return out
}

func (m *Model) command(name string) (Command, bool) {
	for _, c := range m.opts.Commands {
		if c.name() == name {
			return c, true
		}
	}
	return Command{}, false
}

// contextValues pre-fills forms with the selected object. Context menus only
// show actions for that object; the Actions view always includes everything.
func (m *Model) contextValues() map[string]string {
	v := map[string]string{}
	if b, ok := m.selectedBackup(); ok {
		v["volid"], v["storage"], v["vmid"] = b.volid(), b.Storage, strconv.Itoa(b.VMID)
	}
	if j, ok := m.selectedJob(); ok {
		v["job"] = strconv.FormatInt(j.ID, 10)
		v["storage"] = j.Storage
	}
	if r, ok := m.selectedRemote(); ok {
		v["remote"] = r.Name
	}
	if s, ok := m.selectedStorage(); ok {
		v["storage"] = s.ID
	}
	return v
}

func (m *Model) openCommand(name string) bool {
	c, ok := m.command(name)
	if !ok || m.opts.Execute == nil {
		return false
	}
	m.overlay = m.commandForm(c, m.contextValues())
	return true
}

func (m *Model) contextActions() overlay {
	var names []string
	switch m.tab {
	case tabBackups:
		names = []string{"backup inspect", "backup notes", "backup verify", "backup fetch", "restore", "backup protect", "backup unprotect", "backup delete", "backup undelete", "verify history", "backup list"}
	case tabQueue:
		names = []string{"queue show", "queue retry", "queue cancel", "queue priority", "queue scan", "queue list"}
	case tabRemotes:
		names = []string{"remote show", "remote about", "remote test", "remote reconnect", "remote remove", "remote add", "remote list"}
	case tabStorages:
		names = []string{"storage show", "storage set", "storage init", "storage resync", "retention preview", "retention run", "verify history", "recovery-kit export", "recovery-kit status", "storage list"}
	default:
		return newCommandMenu(m.opts.Commands, nil)
	}
	var commands []Command
	for _, name := range names {
		if c, ok := m.command(name); ok {
			commands = append(commands, c)
		}
	}
	return newCommandMenu(commands, m.contextValues())
}

func (m *Model) commandForm(c Command, defaults map[string]string) *form {
	var fields []*field
	for i, arg := range c.Arguments {
		label := arg.Name
		if arg.Required {
			label += " (required)"
		}
		value := defaults[arg.Name]
		if c.name() == "storage init" || c.name() == "remote add" {
			value = ""
		}
		key := fmt.Sprintf("arg%d", i)
		var fl *field
		if arg.Repeated {
			fl = multilineField(key, label+" — one argument per line", value, arg.Required)
		} else {
			fl = newField(key, label, value, arg.Required, false)
		}
		fields = append(fields, fl)
	}
	for _, flag := range c.Flags {
		label := "--" + flag.Name
		if kind := map[string]string{"bool": "true/false", "int": "number", "stringSlice": "comma separated"}[flag.Type]; kind != "" {
			label += " (" + kind + ")"
		}
		value := flag.Default
		// Context only supplies filters, never restore targets or creation IDs.
		if flag.Name == "storage" || flag.Name == "volid" {
			if v := defaults[flag.Name]; v != "" {
				value = v
			}
		}
		var fl *field
		if flag.Type == "stringArray" || flag.Name == "set" && c.name() == "backup notes" {
			fl = multilineField("flag:"+flag.Name, label+" — "+map[bool]string{true: "one value per line", false: "multiline text"}[flag.Type == "stringArray"], value, flag.Required)
		} else {
			fl = newField("flag:"+flag.Name, label, value, flag.Required,
				strings.Contains(flag.Name, "secret") && !strings.HasSuffix(flag.Name, "-file"))
		}
		fl.option, fl.included, fl.boolean = true, flag.Required || value != flag.Default, flag.Type == "bool"
		fl.help = flag.Help
		fl.defaultValue = flag.Default
		fields = append(fields, fl)
	}
	intro := c.Help
	if intro == "" {
		intro = c.Summary
	}
	intro += "\n\nEdit an option to set it; ctrl+x explicitly sets an empty value or restores its default.\nCtrl+s runs the action. Confirmation questions appear here when required."
	f := newForm(c.name(), intro, fields, nil)
	f.submit = func(values map[string]string) (tea.Cmd, string) {
		args := append([]string(nil), c.Path...)
		// Flags precede a '--' separator so literal positional values can start
		// with a dash. Values stay individual argv entries; no shell is involved.
		for i, flag := range c.Flags {
			fl := fields[len(c.Arguments)+i]
			value := values[fl.key]
			if !fl.included && value == flag.Default {
				continue
			}
			switch flag.Type {
			case "bool":
				b, err := strconv.ParseBool(value)
				if err != nil {
					return nil, "--" + flag.Name + " must be true or false (space toggles it)"
				}
				args = append(args, "--"+flag.Name+"="+strconv.FormatBool(b))
			case "stringArray":
				lines := strings.Split(value, "\n")
				for len(lines) > 1 && lines[len(lines)-1] == "" {
					lines = lines[:len(lines)-1]
				}
				for _, line := range lines {
					args = append(args, "--"+flag.Name+"="+line)
				}
			default:
				args = append(args, "--"+flag.Name+"="+value)
			}
		}
		args = append(args, "--")
		for i, arg := range c.Arguments {
			value := values[fmt.Sprintf("arg%d", i)]
			if arg.Repeated {
				for _, line := range strings.Split(value, "\n") {
					if line != "" {
						args = append(args, line)
					}
				}
			} else if value != "" {
				args = append(args, value)
			}
		}
		return m.executeCommand(c.name(), args), ""
	}
	return f
}

// commandMenu is a keyboard list with live search and selection context.
type commandMenu struct {
	commands  []Command
	defaults  map[string]string
	query     string
	cursor    int
	searching bool
}

func newCommandMenu(commands []Command, defaults map[string]string) *commandMenu {
	return &commandMenu{commands: commands, defaults: defaults}
}

func (o *commandMenu) matches() []Command {
	var out []Command
	for _, c := range o.commands {
		match := true
		for _, word := range strings.Fields(strings.ToLower(o.query)) {
			if !strings.Contains(strings.ToLower(c.name()+" "+c.Summary), word) {
				match = false
				break
			}
		}
		if match {
			out = append(out, c)
		}
	}
	return out
}

func (o *commandMenu) update(m *Model, msg tea.Msg) (overlay, tea.Cmd) {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return o, nil
	}
	if o.searching {
		switch k.String() {
		case "esc", "enter":
			o.searching = false
		case "backspace":
			r := []rune(o.query)
			if len(r) > 0 {
				o.query = string(r[:len(r)-1])
			}
			o.cursor = 0
		default:
			if k.Text != "" {
				o.query += k.Text
				o.cursor = 0
			}
		}
		return o, nil
	}
	list := o.matches()
	switch k.String() {
	case "esc", "q":
		return nil, nil
	case "/":
		o.searching = true
	case "down", "j":
		o.cursor = min(o.cursor+1, max(0, len(list)-1))
	case "up", "k":
		o.cursor = max(0, o.cursor-1)
	case "pgdown":
		o.cursor = min(o.cursor+max(1, m.bodyHeight()-4), max(0, len(list)-1))
	case "pgup":
		o.cursor = max(0, o.cursor-max(1, m.bodyHeight()-4))
	case "home":
		o.cursor = 0
	case "end":
		o.cursor = max(0, len(list)-1)
	case "enter":
		if len(list) > 0 {
			return m.commandForm(list[o.cursor], o.defaults), nil
		}
	}
	return o, nil
}

func (o *commandMenu) view(width int, height ...int) string {
	list := o.matches()
	var b strings.Builder
	b.WriteString(titleStyle.Render("Actions") + "\n")
	fmt.Fprintf(&b, "Search: %s  (%d actions)\n\n", cleanLine(o.query), len(list))
	n := max(1, dialogHeight(height)-4)
	start := max(0, min(o.cursor-n+1, len(list)-n))
	for i := start; i < min(len(list), start+n); i++ {
		line := cell(fmt.Sprintf("%-24s %s", list[i].name(), list[i].Summary), max(1, width-2))
		if i == o.cursor {
			line = selectedStyle.Render("> " + line)
		} else {
			line = "  " + line
		}
		b.WriteString(line + "\n")
	}
	if len(list) == 0 {
		b.WriteString("No matching actions. Press / to edit the search.")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (o *commandMenu) hint() string {
	if o.searching {
		return "type to search · enter/esc finish search · backspace edit"
	}
	return "up/down select · enter open form · / search · pgup/pgdown · esc close"
}

type commandRun struct {
	ctx    context.Context
	cancel context.CancelFunc
	events chan tea.Msg
	done   <-chan struct{}
	output *commandOutput
}

type commandText struct {
	run  *commandRun
	text string
}
type commandFinished struct {
	run  *commandRun
	code int
}
type commandQuestion struct {
	run      *commandRun
	question string
	secret   bool
	answer   chan string
}
type commandAnswer struct {
	run    *commandRun
	answer chan string
	value  string
}

type commandWriter struct{ run *commandRun }

func (w commandWriter) Write(p []byte) (int, error) {
	select {
	case w.run.events <- commandText{w.run, string(p)}:
		return len(p), nil
	case <-w.run.ctx.Done():
		return 0, w.run.ctx.Err()
	}
}

func (r *commandRun) next() tea.Cmd {
	return func() tea.Msg {
		select {
		case msg, ok := <-r.events:
			if !ok {
				return nil
			}
			return msg
		case <-r.done:
			return nil
		}
	}
}

func (m *Model) executeCommand(name string, args []string) tea.Cmd {
	// Start on the update loop, after the submitting form has been closed.
	return func() tea.Msg { return startCommand{name, args} }
}

type startCommand struct {
	name string
	args []string
}

func (m *Model) startCommand(msg startCommand) tea.Cmd {
	if m.busy != "" || m.running != nil || m.opts.Execute == nil {
		return nil
	}
	ctx, cancel := context.WithCancel(m.ctx)
	r := &commandRun{ctx: ctx, cancel: cancel, events: make(chan tea.Msg, 64), done: m.ctx.Done()}
	r.output = &commandOutput{info: newInfo(msg.name, ""), run: r}
	m.running, m.overlay, m.busy = r, r.output, msg.name
	m.flash, m.flashErr = "", false
	execute := m.opts.Execute
	return tea.Batch(r.next(), func() tea.Msg {
		defer close(r.events)
		defer cancel()
		ask := func(question string, secret bool) (string, error) {
			answer := make(chan string, 1)
			select {
			case r.events <- commandQuestion{r, question, secret, answer}:
			case <-ctx.Done():
				return "", ctx.Err()
			}
			select {
			case value := <-answer:
				return value, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		code := execute(ctx, msg.args, commandWriter{r}, ask)
		select {
		case r.events <- commandFinished{r, code}:
		case <-m.ctx.Done():
		}
		return nil
	})
}

const maxTranscript = 1 << 20

func (m *Model) commandMessage(msg tea.Msg) (bool, tea.Cmd) {
	switch msg := msg.(type) {
	case startCommand:
		return true, m.startCommand(msg)
	case commandText:
		o := msg.run.output
		follow := o.info.port.AtBottom()
		o.info.text += tf.CleanBlock(msg.text)
		if len(o.info.text) > maxTranscript {
			tail := o.info.text[len(o.info.text)-maxTranscript:]
			for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
				tail = tail[1:]
			}
			if _, after, found := strings.Cut(tail, "\n"); found {
				tail = after
			}
			o.info.text = "[Earlier output omitted]\n" + tail
		}
		o.info.port.SetContent(o.info.text)
		if follow {
			o.info.port.GotoBottom()
		}
		return true, msg.run.next()
	case commandQuestion:
		if msg.run.ctx.Err() != nil {
			return true, msg.run.next()
		}
		intro := msg.run.output.info.text
		// Keep prompts usable even when a command has produced a long history.
		lines := strings.Split(intro, "\n")
		intro = strings.Join(lines[max(0, len(lines)-12):], "\n") + "\n\n" + cleanLine(msg.question)
		f := newForm("Answer: "+msg.run.output.info.title, intro,
			[]*field{newField("answer", "Answer", "", false, msg.secret)}, func(v map[string]string) (tea.Cmd, string) {
				return func() tea.Msg { return commandAnswer{msg.run, msg.answer, v["answer"]} }, ""
			})
		f.cancel = func() tea.Msg { msg.run.cancel(); return resumeCommand{msg.run} }
		m.overlay = f
		return true, msg.run.next()
	case commandAnswer:
		if m.running == msg.run {
			// Restore the output before releasing the waiting CLI. Its next
			// prompt can then arrive without a delayed resume hiding it.
			m.overlay = msg.run.output
			msg.answer <- msg.value
		}
		return true, nil
	case resumeCommand:
		if m.running == msg.run {
			m.overlay = msg.run.output
		}
		return true, nil
	case commandFinished:
		msg.run.output.finished, msg.run.output.code = true, msg.code
		if m.running == msg.run {
			m.running, m.busy, m.overlay = nil, "", msg.run.output
		}
		return true, m.refresh()
	}
	return false, nil
}

type resumeCommand struct{ run *commandRun }

type commandOutput struct {
	info     *info
	run      *commandRun
	finished bool
	code     int
}

func (o *commandOutput) update(m *Model, msg tea.Msg) (overlay, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "s":
			if o.finished {
				f := newForm("Save output", "Writes a new file readable only by its owner. Existing files are kept.",
					[]*field{newField("file", "Output file", "pve-rclone-backup-output.txt", true, false)}, func(v map[string]string) (tea.Cmd, string) {
						return m.action("Saving output", func(context.Context) doneMsg {
							if err := writeKit(v["file"], o.info.text+"\n"); err != nil {
								return doneMsg{err: err, next: o}
							}
							return doneMsg{text: "Saved output to " + cleanLine(v["file"]), next: o}
						}), ""
					})
				f.cancel = func() tea.Msg { return doneMsg{next: o} }
				return f, nil
			}
			return o, nil
		case "esc", "q", "enter":
			if o.finished {
				return nil, nil
			}
			if k.String() == "esc" {
				o.run.cancel()
			}
			return o, nil
		}
	}
	o.info.port, _ = o.info.port.Update(msg)
	if k, ok := msg.(tea.KeyPressMsg); ok {
		if k.String() == "home" {
			o.info.port.GotoTop()
		}
		if k.String() == "end" {
			o.info.port.GotoBottom()
		}
	}
	return o, nil
}

func (o *commandOutput) view(width int, height ...int) string {
	return o.info.view(width, height...)
}

func (o *commandOutput) hint() string {
	if o.finished {
		return fmt.Sprintf("Exit %d · esc/enter close · s save output · up/down/pgup/pgdown · home/end", o.code)
	}
	return "esc/ctrl+c stop command/wait · pgup/down scroll · home/end · jobs continue"
}
