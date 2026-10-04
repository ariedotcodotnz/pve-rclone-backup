// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"encoding/json"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	tf "github.com/ariedotcodotnz/pve-rclone-backup/internal/textfmt"
)

type jsonRaw = json.RawMessage

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// overlay is a dialog shown over the current view.
type overlay interface {
	// update handles a message; a nil overlay closes the dialog.
	update(m *Model, msg tea.Msg) (overlay, tea.Cmd)
	view(width int) string
	hint() string
}

// info shows a scrollable text.
type info struct {
	title  string
	lines  []string
	offset int
}

const infoLines = 22

func newInfo(title, text string) *info {
	return &info{title: title, lines: strings.Split(strings.TrimRight(tf.CleanBlock(text), "\n"), "\n")}
}

func (o *info) update(_ *Model, msg tea.Msg) (overlay, tea.Cmd) {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return o, nil
	}
	switch k.String() {
	case "esc", "enter", "q":
		return nil, nil
	case "down", "j":
		o.offset = min(o.offset+1, max(0, len(o.lines)-infoLines))
	case "up", "k":
		o.offset = max(0, o.offset-1)
	case "pgdown", "space":
		o.offset = min(o.offset+infoLines, max(0, len(o.lines)-infoLines))
	case "pgup":
		o.offset = max(0, o.offset-infoLines)
	}
	return o, nil
}

func (o *info) view(int) string {
	end := min(len(o.lines), o.offset+infoLines)
	body := strings.Join(o.lines[o.offset:end], "\n")
	if len(o.lines) > infoLines {
		body += "\n" + dimStyle.Render("(more with up/down)")
	}
	return titleStyle.Render(o.title) + "\n\n" + body
}

func (o *info) hint() string { return "esc close · up/down scroll" }

// confirm asks a yes/no question.
type confirm struct {
	question string
	yes      tea.Cmd
}

func newConfirm(question string, yes tea.Cmd) *confirm { return &confirm{question: question, yes: yes} }

func (o *confirm) update(_ *Model, msg tea.Msg) (overlay, tea.Cmd) {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return o, nil
	}
	switch k.String() {
	case "y", "Y":
		return nil, o.yes
	case "n", "N", "esc", "q":
		return nil, nil
	}
	return o, nil
}

func (o *confirm) view(int) string {
	return warnStyle.Render(o.question) + "\n\n" + "[y] yes   [n] no"
}

func (o *confirm) hint() string { return "y confirm · n cancel" }

// field is a form input.
type field struct {
	key, label string
	required   bool
	input      textinput.Model
}

func newField(key, label, value string, required, secret bool) *field {
	in := textinput.New()
	in.Prompt = "> "
	in.SetValue(value)
	in.SetWidth(60)
	st := in.Styles()
	st.Cursor.Blink = false // a steady cursor; also no timer per keystroke
	in.SetStyles(st)
	if secret {
		in.EchoMode = textinput.EchoPassword
	}
	return &field{key: key, label: label, required: required, input: in}
}

// form collects values.
type form struct {
	title, intro string
	fields       []*field
	focus        int
	err          string
	// submit returns the action to run, or a message to show instead.
	submit func(values map[string]string) (tea.Cmd, string)
	cancel tea.Cmd
}

func newForm(title, intro string, fields []*field, submit func(map[string]string) (tea.Cmd, string)) *form {
	f := &form{title: title, intro: intro, fields: fields, submit: submit}
	if len(fields) > 0 {
		fields[0].input.Focus()
	}
	return f
}

func (f *form) move(d int) tea.Cmd {
	f.fields[f.focus].input.Blur()
	f.focus = (f.focus + d + len(f.fields)) % len(f.fields)
	return f.fields[f.focus].input.Focus()
}

func (f *form) values() map[string]string {
	out := map[string]string{}
	for _, fl := range f.fields {
		out[fl.key] = strings.TrimSpace(fl.input.Value())
	}
	return out
}

func (f *form) update(_ *Model, msg tea.Msg) (overlay, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "esc":
			return nil, f.cancel
		case "tab", "down":
			return f, f.move(1)
		case "shift+tab", "up":
			return f, f.move(-1)
		case "enter":
			if f.focus < len(f.fields)-1 {
				return f, f.move(1)
			}
			vals := f.values()
			for _, fl := range f.fields {
				if fl.required && vals[fl.key] == "" {
					f.err = fl.label + " is required"
					return f, nil
				}
			}
			cmd, problem := f.submit(vals)
			if problem != "" {
				f.err = problem
				return f, nil
			}
			return nil, cmd
		}
	}
	if len(f.fields) == 0 {
		return f, nil
	}
	var cmd tea.Cmd
	f.fields[f.focus].input, cmd = f.fields[f.focus].input.Update(msg)
	return f, cmd
}

func (f *form) view(int) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(f.title) + "\n\n")
	if f.intro != "" {
		b.WriteString(f.intro + "\n\n")
	}
	for i, fl := range f.fields {
		label := fl.label
		if i == f.focus {
			label = selectedStyle.Render(label)
		}
		b.WriteString(label + "\n" + fl.input.View() + "\n")
	}
	if f.err != "" {
		b.WriteString("\n" + errStyle.Render(cleanLine(f.err)))
	}
	return strings.TrimRight(b.String(), "\n")
}

func (f *form) hint() string { return "enter next/submit · tab move · esc cancel" }
