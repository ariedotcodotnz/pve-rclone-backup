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

// encodeINI writes sections and keys in sorted order.
func encodeINI(s sections) []byte {
	var b strings.Builder
	b.WriteString("# Managed by pve-rclone-backupd. Contains credentials - do not share.\n")
	for _, name := range slices.Sorted(maps.Keys(s)) {
		fmt.Fprintf(&b, "\n[%s]\n", name)
		for _, k := range slices.Sorted(maps.Keys(s[name])) {
			fmt.Fprintf(&b, "%s = %s\n", k, s[name][k])
		}
	}
	return []byte(b.String())
}
