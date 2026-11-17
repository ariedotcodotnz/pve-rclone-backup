// SPDX-License-Identifier: AGPL-3.0-or-later

// Package textfmt formats values for terminal output.
package textfmt

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// Bytes formats a size with binary units.
func Bytes(n int64) string {
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

// Time formats a time in the local zone, "-" for none.
func Time(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// Unix returns a time for Unix seconds (nil for 0).
func Unix(sec int64) *time.Time {
	if sec == 0 {
		return nil
	}
	t := time.Unix(sec, 0)
	return &t
}

// Clean removes control characters (text from the daemon can come from
// guests and remotes) and folds whitespace runs into single spaces.
func Clean(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
	return s
}

// CleanBlock removes control characters but keeps line breaks.
func CleanBlock(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case r == '\t':
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
}

// Truncate shortens s to at most n runes.
func Truncate(s string, n int) string {
	r := []rune(s)
	if n <= 0 {
		return ""
	}
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

// Dash returns "-" for an empty string.
func Dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// YesNo formats a boolean.
func YesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// Account describes a cloud account's quota, such as "1.0 TiB of 1.0 TiB
// used, 2.0 GiB free". Unknown values are left out; "-" if none is known.
func Account(total, used, free, trashed *int64) string {
	var parts []string
	switch {
	case used != nil && total != nil && *total > 0:
		parts = append(parts, Bytes(*used)+" of "+Bytes(*total)+" used")
	case used != nil:
		parts = append(parts, Bytes(*used)+" used")
	}
	if free != nil {
		parts = append(parts, Bytes(max(*free, 0))+" free")
	}
	if trashed != nil && *trashed > 0 {
		parts = append(parts, Bytes(*trashed)+" in the recycle bin")
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ", ")
}

// Backups describes the space backups take, such as "12 backups, 1.2 GiB
// (2 marked for deletion, 200.0 MiB)".
func Backups(n int, bytes int64, tombstoned int, tombstonedBytes int64) string {
	s := fmt.Sprintf("%d backups, %s", n, Bytes(bytes))
	if n == 1 {
		s = "1 backup, " + Bytes(bytes)
	}
	if tombstoned > 0 {
		s += fmt.Sprintf(" (%d marked for deletion, %s)", tombstoned, Bytes(tombstonedBytes))
	}
	return s
}
