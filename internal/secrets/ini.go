// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"bufio"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// sections is an rclone-style INI configuration: [section] key = value.
type sections map[string]map[string]string

func (s sections) clone() sections {
	out := make(sections, len(s))
	for name, kv := range s {
		out[name] = maps.Clone(kv)
	}
	return out
}

// parseINI reads the rclone config file format (as written by rclone and
// by encodeINI). Comment lines start with '#' or ';'.
func parseINI(data []byte) (sections, error) {
	out := sections{}
	var cur map[string]string
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		switch {
		case line == "" || line[0] == '#' || line[0] == ';':
		case line[0] == '[':
			if !strings.HasSuffix(line, "]") || len(line) < 3 {
				return nil, fmt.Errorf("line %d: malformed section header", n)
			}
			name := line[1 : len(line)-1]
			if _, dup := out[name]; dup {
				return nil, fmt.Errorf("line %d: duplicate section %q", n, name)
			}
			cur = map[string]string{}
			out[name] = cur
		default:
			k, v, ok := strings.Cut(line, "=")
			if !ok || cur == nil {
				return nil, fmt.Errorf("line %d: expected key = value inside a section", n)
			}
			cur[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out, sc.Err()
}

// encodeINI writes sections and keys in sorted order. The format has no
// escaping, so entries that would change the file's structure are refused.
func encodeINI(s sections) ([]byte, error) {
	var b strings.Builder
	b.WriteString("# Managed by pve-rclone-backupd. Contains credentials - do not share.\n")
	for _, name := range slices.Sorted(maps.Keys(s)) {
		if err := CheckEntry(name, "", ""); err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "\n[%s]\n", name)
		for _, k := range slices.Sorted(maps.Keys(s[name])) {
			if err := CheckEntry(name, k, s[name][k]); err != nil {
				return nil, err
			}
			fmt.Fprintf(&b, "%s = %s\n", k, s[name][k])
		}
	}
	return []byte(b.String()), nil
}

// CheckEntry reports whether a section name and, if key is not empty, a
// key and value can be stored in the remotes file and read back unchanged:
// no line breaks or other control characters, section names without
// brackets, keys without "=" that do not look like a comment or section.
func CheckEntry(section, key, value string) error {
	bad := func(s string) bool {
		return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 && r != '\t' || r == 0x7f })
	}
	switch {
	case section == "" || bad(section) || strings.ContainsAny(section, "[]") || strings.TrimSpace(section) != section:
		return fmt.Errorf("secrets: invalid remote name %q", section)
	case key == "":
		return nil
	case bad(key) || strings.Contains(key, "=") || strings.ContainsAny(key[:1], "#;[") || strings.TrimSpace(key) != key:
		return fmt.Errorf("secrets: invalid setting name %q in remote %q", key, section)
	case bad(value) || strings.TrimSpace(value) != value:
		return fmt.Errorf("secrets: setting %q of remote %q has a value that cannot be stored", key, section)
	}
	return nil
}
