// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	tf "github.com/ariedotcodotnz/pve-rclone-backup/internal/textfmt"
)

type jsonRaw = json.RawMessage

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// overlay is a dialog shown over the current view.
type overlay interface {
	// update handles a message; a nil overlay closes the dialog.
	update(m *Model, msg tea.Msg) (overlay, tea.Cmd)
	view(width int, height ...int) string
	hint() string
}

// info shows a scrollable text.
type info struct {
	title string
	text  string
	port  viewport.Model
}

const infoLines = 22

func newInfo(title, text string) *info {
	p := viewport.New(viewport.WithWidth(100), viewport.WithHeight(infoLines))
	p.MouseWheelEnabled = false
	p.SoftWrap = true
	return &info{title: cleanLine(title), text: strings.TrimRight(tf.CleanBlock(text), "\n"), port: p}
}

func (o *info) update(_ *Model, msg tea.Msg) (overlay, tea.Cmd) {
	k, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return o, nil
	}
	switch k.String() {
	case "esc", "enter", "q":
		return nil, nil
	case "home":
		o.port.GotoTop()
	case "end":
		o.port.GotoBottom()
	default:
		o.port, _ = o.port.Update(msg)
	}
	return o, nil
}

func (o *info) view(width int, height ...int) string {
	o.port.SetWidth(max(1, width))
	o.port.SetHeight(max(1, dialogHeight(height)-3))
	o.port.SetContent(o.text)
	return titleStyle.Render(o.title) + "\n\n" + o.port.View()
}

func (o *info) hint() string { return "esc close · up/down/pgup/pgdown scroll · home/end" }

func dialogHeight(height []int) int {
	if len(height) > 0 {
		return height[0]
	}
	return infoLines + 3
}

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

func (o *confirm) view(width int, _ ...int) string {
	return lipgloss.Wrap(warnStyle.Render(o.question), max(1, width), " ") + "\n\n" + "[y] yes   [n] no"
}

func (o *confirm) hint() string { return "y confirm · n cancel" }

// field is a form input.
type field struct {
	key, label                string
	required                  bool
	input                     textinput.Model
	area                      *textarea.Model
	option, included, boolean bool
	help                      string
	defaultValue              string
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

func multilineField(key, label, value string, required bool) *field {
	f := newField(key, label, value, required, false)
	a := textarea.New()
	a.ShowLineNumbers = false
	a.SetHeight(3)
	a.SetValue(value)
	st := a.Styles()
	st.Cursor.Blink = false
	a.SetStyles(st)
	f.area = &a
	return f
}

func (f *field) value() string {
	if f.area != nil {
		return f.area.Value()
	}
	return f.input.Value()
}

func (f *field) focus() tea.Cmd {
	if f.area != nil {
		return f.area.Focus()
	}
	return f.input.Focus()
}

func (f *field) blur() {
	if f.area != nil {
		f.area.Blur()
	} else {
		f.input.Blur()
	}
}

// form collects values.
type form struct {
	title, intro string
	fields       []*field
	focus        int
	err          string
	// submit returns the action to run, or a message to show instead.
	submit    func(values map[string]string) (tea.Cmd, string)
	cancel    tea.Cmd
	port      viewport.Model
	lastFocus int
}

func newForm(title, intro string, fields []*field, submit func(map[string]string) (tea.Cmd, string)) *form {
	f := &form{title: title, intro: intro, fields: fields, submit: submit, lastFocus: -1,
		port: viewport.New(viewport.WithWidth(100), viewport.WithHeight(infoLines))}
	f.port.MouseWheelEnabled = false
	if len(fields) > 0 {
		fields[0].focus()
	}
	return f
}

func (f *form) move(d int) tea.Cmd {
	if len(f.fields) == 0 {
		return nil
	}
	f.fields[f.focus].blur()
	f.focus = (f.focus + d + len(f.fields)) % len(f.fields)
	return f.fields[f.focus].focus()
}

func (f *form) values() map[string]string {
	out := map[string]string{}
	for _, fl := range f.fields {
		out[fl.key] = fl.value()
		if fl.input.EchoMode != textinput.EchoPassword && fl.area == nil {
			out[fl.key] = strings.TrimSpace(out[fl.key])
		}
	}
	return out
}

func (f *form) update(_ *Model, msg tea.Msg) (overlay, tea.Cmd) {
	if k, ok := msg.(tea.KeyPressMsg); ok {
		switch k.String() {
		case "esc":
			return nil, f.cancel
		case "tab":
			return f, f.move(1)
		case "shift+tab":
			return f, f.move(-1)
		case "down", "up":
			if len(f.fields) > 0 && f.fields[f.focus].area == nil {
				d := 1
				if k.String() == "up" {
					d = -1
				}
				return f, f.move(d)
			}
		case "pgup", "pgdown":
			f.port, _ = f.port.Update(msg)
			return f, nil
		case "ctrl+x":
			if len(f.fields) > 0 && f.fields[f.focus].option {
				fl := f.fields[f.focus]
				fl.included = !fl.included
				if !fl.included {
					if fl.area != nil {
						fl.area.SetValue(fl.defaultValue)
					} else {
						fl.input.SetValue(fl.defaultValue)
					}
				}
				return f, nil
			}
		case "space":
			if len(f.fields) > 0 && f.fields[f.focus].boolean {
				fl := f.fields[f.focus]
				fl.input.SetValue(map[bool]string{true: "false", false: "true"}[fl.value() == "true"])
				fl.included = true
				return f, nil
			}
		case "enter":
			if len(f.fields) > 0 && f.fields[f.focus].area != nil {
				break
			}
			if f.focus < len(f.fields)-1 {
				return f, f.move(1)
			}
			return f.finish()
		case "ctrl+s":
			return f.finish()
		}
	}
	if len(f.fields) == 0 {
		return f, nil
	}
	var cmd tea.Cmd
	fl := f.fields[f.focus]
	before := fl.value()
	if fl.area != nil {
		*fl.area, cmd = fl.area.Update(msg)
	} else {
		fl.input, cmd = fl.input.Update(msg)
	}
	if fl.value() != before {
		fl.included = true
	}
	return f, cmd
}

func (f *form) finish() (overlay, tea.Cmd) {
	vals := f.values()
	for i, fl := range f.fields {
		if fl.required && strings.TrimSpace(vals[fl.key]) == "" {
			f.err = fl.label + " is required"
			if len(f.fields) > 0 {
				f.fields[f.focus].blur()
				f.focus = i
				fl.focus()
			}
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

func (f *form) view(width int, height ...int) string {
	width = max(1, width)
	h := max(1, dialogHeight(height)-2)
	f.port.SetWidth(width)
	f.port.SetHeight(h)
	var b strings.Builder
	if f.intro != "" {
		b.WriteString(lipgloss.Wrap(f.intro, width, " ") + "\n\n")
	}
	focusLine, focusEnd := 0, 0
	for i, fl := range f.fields {
		label := fl.label
		if fl.option {
			label = map[bool]string{true: "[set] ", false: "[default] "}[fl.included] + label
		}
		if i == f.focus {
			focusLine = strings.Count(b.String(), "\n")
			label = selectedStyle.Render(label)
		}
		b.WriteString(lipgloss.Wrap(label, width, " ") + "\n")
		if fl.help != "" {
			b.WriteString(lipgloss.Wrap(dimStyle.Render(fl.help), width, " ") + "\n")
		}
		if fl.area != nil {
			fl.area.SetWidth(width)
			b.WriteString(fl.area.View() + "\n")
		} else {
			fl.input.SetWidth(max(1, width-2))
			b.WriteString(fl.input.View() + "\n")
		}
		if i == f.focus {
			focusEnd = strings.Count(b.String(), "\n")
		}
	}
	f.port.SetContent(strings.TrimRight(b.String(), "\n"))
	if f.lastFocus != f.focus {
		if focusLine < f.port.YOffset() {
			f.port.SetYOffset(focusLine)
		}
		if focusEnd > f.port.YOffset()+h {
			f.port.SetYOffset(max(0, focusEnd-h))
		}
		f.lastFocus = f.focus
	}
	body := f.port.View()
	if f.err != "" {
		body += "\n" + lipgloss.Wrap(errStyle.Render(cleanLine(f.err)), width, " ")
	}
	return titleStyle.Render(fmt.Sprintf("%s (%d/%d)", f.title, min(f.focus+1, len(f.fields)), len(f.fields))) + "\n" + body
}

func (f *form) hint() string {
	return "tab move · ctrl+s submit · ctrl+x set/default · space bool · pgup/down · esc cancel"
}
