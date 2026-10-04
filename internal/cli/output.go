// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"golang.org/x/term"
)

func (a *App) json(v any) error {
	enc := json.NewEncoder(a.Out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// table writes aligned columns. Cells are sanitized: values may come from
// guest configurations and notes.
func (a *App) table(header []string, rows [][]string) {
	tw := tabwriter.NewWriter(a.Out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, c := range r {
			cells[i] = clean(c)
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	_ = tw.Flush()
}

// clean removes control characters and tabs and shortens long values.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if r := []rune(s); len(r) > 60 {
		s = string(r[:59]) + "…"
	}
	return s
}

// humanBytes formats a size with binary units.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func humanTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func unixTime(sec int64) *time.Time {
	if sec == 0 {
		return nil
	}
	t := time.Unix(sec, 0)
	return &t
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// prompt asks a question and returns the trimmed answer.
func (a *App) prompt(question string) (string, error) {
	fmt.Fprint(a.Err, question)
	line, err := a.readLine()
	return strings.TrimSpace(line), err
}

// promptSecret asks for a secret without echo when stdin is a terminal.
func (a *App) promptSecret(question string) (string, error) {
	fmt.Fprint(a.Err, question)
	if f, ok := a.In.(interface{ Fd() uintptr }); ok && term.IsTerminal(int(f.Fd())) {
		b, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(a.Err)
		return string(b), err
	}
	line, err := a.readLine()
	return strings.TrimRight(line, "\r\n"), err
}

// readLine reads one line from the input without buffering beyond it.
func (a *App) readLine() (string, error) {
	if a.reader == nil {
		a.reader = bufio.NewReader(a.In)
	}
	line, err := a.reader.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("no answer on standard input: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// confirm asks a yes/no question; --yes answers yes.
func (a *App) confirm(question string) (bool, error) {
	if a.Yes {
		return true, nil
	}
	if !a.Interactive {
		return false, usagef("%s: pass --yes to confirm non-interactively", strings.TrimSuffix(question, "?"))
	}
	ans, err := a.prompt(question + " [y/N] ")
	if err != nil {
		return false, err
	}
	return strings.EqualFold(ans, "y") || strings.EqualFold(ans, "yes"), nil
}
