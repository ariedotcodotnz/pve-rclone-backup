// SPDX-License-Identifier: AGPL-3.0-or-later

// Package storagecfg reads /etc/pve/storage.cfg with the same rules as
// PVE::SectionConfig and PVE::Storage::Plugin::parse_config. It is read-only:
// storage.cfg is changed exclusively through the PVE API.
package storagecfg

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
)

// DefaultPath is the location of the cluster-wide storage configuration.
const DefaultPath = "/etc/pve/storage.cfg"

// Section is one storage definition.
type Section struct {
	Type  string
	ID    string
	Line  int                   // line of the section header (1-based)
	Props map[string]config.Raw // properties other than the type
}

// Get returns a property value and whether it is set.
func (s *Section) Get(key string) (string, bool) {
	r, ok := s.Props[key]
	return r.Value, ok
}

// Bool interprets a property like PVE: a bare key or "1" is true.
func (s *Section) Bool(key string) bool {
	r, ok := s.Props[key]
	return ok && (r.NoValue || r.Value == "1")
}

// List splits a list property like PVE::Tools::split_list.
func (s *Section) List(key string) []string {
	return config.SplitList(s.Props[key].Value)
}

// Content returns the configured content types. ok is false when the
// property is absent and the plugin default applies.
func (s *Section) Content() (content map[string]bool, ok bool) {
	r, ok := s.Props["content"]
	if !ok {
		return nil, false
	}
	content = map[string]bool{}
	for _, c := range config.SplitList(r.Value) {
		content[c] = true
	}
	return content, true
}

// Config is a parsed storage.cfg.
type Config struct {
	Sections []*Section // in file order
}

// Get returns the section with the given storage ID, or nil.
func (c *Config) Get(id string) *Section {
	for _, s := range c.Sections {
		if s.ID == id {
			return s
		}
	}
	return nil
}

// OfType returns all sections of a storage type in file order.
func (c *Config) OfType(typ string) []*Section {
	var out []*Section
	for _, s := range c.Sections {
		if s.Type == typ {
			out = append(out, s)
		}
	}
	return out
}

// Warning is a problem PVE would also warn about while parsing.
type Warning struct {
	Line int
	Msg  string
}

func (w Warning) String() string { return fmt.Sprintf("storage.cfg line %d: %s", w.Line, w.Msg) }

var (
	commentRe  = regexp.MustCompile(`^\s*#`)
	headerRe   = regexp.MustCompile(`^(\S+):\s*(\S+)\s*$`)
	propertyRe = regexp.MustCompile(`^\s+(\S+)(?:\s+(.*\S))?\s*$`)
)

// Parse parses storage.cfg content. Malformed lines are skipped and
// reported as warnings, exactly where PVE would skip them. The implicit
// "local" directory storage PVE always provides is added (see ensureLocal).
func Parse(raw []byte) (*Config, []Warning) {
	lines := strings.Split(string(raw), "\n")
	var warnings []Warning
	cfg := &Config{}

	i := 0
	// next returns the next non-comment line; ok is false at end of input.
	next := func() (line string, lineno int, ok bool) {
		for i < len(lines) {
			line, lineno = lines[i], i+1
			i++
			if !commentRe.MatchString(line) {
				return line, lineno, true
			}
		}
		return "", 0, false
	}
	// PVE ends sections and skips lines that are false in Perl: "" and "0".
	falsy := func(s string) bool { return s == "" || s == "0" }

	for {
		line, lineno, ok := next()
		if !ok {
			break
		}
		if falsy(line) {
			continue
		}
		m := headerRe.FindStringSubmatch(line)
		if m == nil {
			warnings = append(warnings, Warning{lineno, fmt.Sprintf("ignore config line: %s", line)})
			continue
		}
		typ, id := strings.ToLower(m[1]), m[2]
		skip := false
		if !config.ValidStorageID(id) {
			warnings = append(warnings, Warning{lineno, fmt.Sprintf("skip section %q: invalid storage ID", id)})
			skip = true
		} else if cfg.Get(id) != nil {
			warnings = append(warnings, Warning{lineno, fmt.Sprintf("skip section %q: duplicate storage ID", id)})
			skip = true
		}
		sec := &Section{Type: typ, ID: id, Line: lineno, Props: map[string]config.Raw{}}

		for {
			line, lineno, ok := next()
			if !ok || falsy(line) {
				break
			}
			if skip {
				continue
			}
			pm := propertyRe.FindStringSubmatchIndex(line)
			if pm == nil {
				warnings = append(warnings, Warning{lineno, fmt.Sprintf("section %q: ignore config line: %s", id, line)})
				continue
			}
			key := line[pm[2]:pm[3]]
			raw := config.Raw{NoValue: pm[4] < 0}
			if !raw.NoValue {
				raw.Value = line[pm[4]:pm[5]]
			}
			if _, dup := sec.Props[key]; dup || key == "type" {
				warnings = append(warnings, Warning{lineno, fmt.Sprintf("section %q: duplicate attribute %q", id, key)})
				continue
			}
			sec.Props[key] = raw
		}
		if !skip {
			cfg.Sections = append(cfg.Sections, sec)
		}
	}

	ensureLocal(cfg)
	return cfg, warnings
}

// ensureLocal mirrors PVE::Storage::Plugin::parse_config: there is always a
// "local" directory storage at /var/lib/vz without node restrictions. A
// missing or conflicting definition is replaced by the built-in default,
// which includes backup content.
func ensureLocal(cfg *Config) {
	idx := slices.IndexFunc(cfg.Sections, func(s *Section) bool { return s.ID == "local" })
	if idx >= 0 {
		s := cfg.Sections[idx]
		if path, ok := s.Get("path"); s.Type == "dir" && (!ok || path == "" || path == "/var/lib/vz") {
			s.Props["path"] = config.Raw{Value: "/var/lib/vz"}
			delete(s.Props, "nodes")
			return
		}
		cfg.Sections = slices.Delete(cfg.Sections, idx, idx+1)
	}
	local := &Section{Type: "dir", ID: "local", Props: map[string]config.Raw{
		"path":          {Value: "/var/lib/vz"},
		"prune-backups": {Value: "keep-all=1"},
		"content":       {Value: "backup,images,iso,rootdir,snippets,vztmpl"},
	}}
	cfg.Sections = append([]*Section{local}, cfg.Sections...)
}
