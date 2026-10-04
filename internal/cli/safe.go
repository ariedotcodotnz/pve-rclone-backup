// SPDX-License-Identifier: AGPL-3.0-or-later

package cli

import (
	"io"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// safeWriter strips terminal control characters (C0 except tab and
// newline, DEL and C1) from everything the CLI prints. Text from the
// daemon can originate from guest configurations, file names and remote
// error messages, and must not be able to inject escape sequences.
type safeWriter struct {
	w       io.Writer
	partial []byte // an incomplete UTF-8 sequence from the previous write
}

func newSafeWriter(w io.Writer) io.Writer {
	if _, ok := w.(*safeWriter); ok {
		return w
	}
	return &safeWriter{w: w}
}

func (s *safeWriter) Write(p []byte) (int, error) {
	buf := append(s.partial, p...)
	s.partial = nil
	out := make([]byte, 0, len(buf))
	for len(buf) > 0 {
		r, size := utf8.DecodeRune(buf)
		if r == utf8.RuneError && size <= 1 {
			if !utf8.FullRune(buf) {
				s.partial = append([]byte(nil), buf...)
				break
			}
			buf = buf[1:] // invalid byte
			continue
		}
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			out = append(out, buf[:size]...)
		}
		buf = buf[size:]
	}
	if _, err := s.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// secretValue resolves "@path" to the contents of a file, so that
// secrets need not appear on the command line (and in the process list
// or shell history).
func secretValue(v string) (string, error) {
	path, ok := strings.CutPrefix(v, "@")
	if !ok {
		return v, nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // the operator names the file
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
