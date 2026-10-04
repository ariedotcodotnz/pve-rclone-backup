// SPDX-License-Identifier: AGPL-3.0-or-later

// Package logging configures structured logging with secret redaction.
//
// Every log line, including rclone's, passes through Redactor: attributes
// with sensitive keys are masked and string values are scrubbed of OAuth
// tokens, bearer credentials and password fields. Logs go to stderr, which
// systemd forwards to the journal.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
)

// Mask replaces redacted content.
const Mask = "[REDACTED]"

var sensitiveKeys = []string{
	"token", "access_token", "refresh_token", "id_token", "password", "password2", "pass",
	"passphrase", "secret", "client_secret", "authorization", "cookie", "api_key", "private_key",
}

func sensitiveKey(k string) bool {
	k = strings.ToLower(k)
	for _, s := range sensitiveKeys {
		if k == s || strings.HasSuffix(k, "_"+s) || strings.HasSuffix(k, "-"+s) {
			return true
		}
	}
	return false
}

var scrubbers = []struct {
	re   *regexp.Regexp
	repl string
}{
	// JSON fields: "access_token":"..."
	{regexp.MustCompile(`(?i)("(?:access_token|refresh_token|id_token|token|password2?|pass|client_secret|secret)"\s*:\s*")(?:[^"\\]|\\.)*(")`), "${1}" + Mask + "${2}"},
	// key=value / key: value pairs
	{regexp.MustCompile(`(?i)\b(access_token|refresh_token|id_token|client_secret|password2?|passphrase|code)(\s*[=:]\s*)[^\s&,;"']+`), "${1}${2}" + Mask},
	// HTTP authorization
	{regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9\-._~+/]+=*`), "${1} " + Mask},
}

// Redact scrubs credentials from free text.
func Redact(s string) string {
	for _, sc := range scrubbers {
		s = sc.re.ReplaceAllString(s, sc.repl)
	}
	return s
}

// Redactor is a slog.Handler that masks sensitive attributes and scrubs
// credentials from messages and string values before passing records on.
type Redactor struct{ next slog.Handler }

// NewRedactor wraps next.
func NewRedactor(next slog.Handler) *Redactor { return &Redactor{next: next} }

func (h *Redactor) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *Redactor) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *Redactor) WithAttrs(attrs []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		red[i] = redactAttr(a)
	}
	return &Redactor{next: h.next.WithAttrs(red)}
}

func (h *Redactor) WithGroup(name string) slog.Handler {
	return &Redactor{next: h.next.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	if sensitiveKey(a.Key) {
		return slog.String(a.Key, Mask)
	}
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, Redact(v.String()))
	case slog.KindGroup:
		group := v.Group()
		red := make([]any, len(group))
		for i, g := range group {
			red[i] = redactAttr(g)
		}
		return slog.Group(a.Key, red...)
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return slog.String(a.Key, Redact(err.Error()))
		}
		if s, ok := v.Any().(fmt.Stringer); ok {
			return slog.String(a.Key, Redact(s.String()))
		}
		return slog.String(a.Key, Redact(fmt.Sprint(v.Any())))
	}
	return slog.Attr{Key: a.Key, Value: v}
}

// ParseLevel maps the daemon's log-level setting to a slog level.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q", s)
}

// New returns a redacting logger writing to w in the given format ("json"
// or "text"). The level can be changed at runtime through lv.
func New(w io.Writer, format string, lv *slog.LevelVar) (*slog.Logger, error) {
	opts := &slog.HandlerOptions{Level: lv}
	var h slog.Handler
	switch format {
	case "json":
		h = slog.NewJSONHandler(w, opts)
	case "text", "":
		h = slog.NewTextHandler(w, opts)
	default:
		return nil, fmt.Errorf("unknown log format %q", format)
	}
	return slog.New(NewRedactor(h)), nil
}
