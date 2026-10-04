// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// BaseOptions are properties defined by PVE's base storage plugin that the
// rclone-backup type accepts.
type BaseOptions struct {
	Content             map[string]bool // nil: the plugin default (backup)
	Nodes               []string        // nil: all nodes
	Disable             bool
	Shared              bool
	PruneBackups        *PruneOptions // nil: unset (keep everything)
	MaxProtectedBackups *int64
	BwLimit             map[string]int64 // KiB/s per operation (clone, default, migration, move, restore)
}

// PruneOptions is PVE's prune-backups property string.
type PruneOptions struct {
	KeepAll     bool
	KeepLast    int
	KeepHourly  int
	KeepDaily   int
	KeepWeekly  int
	KeepMonthly int
	KeepYearly  int
}

// IsKeepAll reports whether the options keep every backup.
func (p *PruneOptions) IsKeepAll() bool {
	return p == nil || p.KeepAll ||
		p.KeepLast+p.KeepHourly+p.KeepDaily+p.KeepWeekly+p.KeepMonthly+p.KeepYearly == 0
}

// ParsePropertyString parses a PVE property string "k1=v1,k2=v2".
func ParsePropertyString(s string) (map[string]string, error) {
	out := map[string]string{}
	for part := range strings.SplitSeq(s, ",") {
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid property string element %q", part)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("duplicate key %q in property string", k)
		}
		out[k] = v
	}
	return out, nil
}

// ParsePruneOptions parses and normalizes a prune-backups value like PVE's
// validate_prune_backups: no positive keep-* option means keep-all, and
// keep-all may not be combined with other options.
func ParsePruneOptions(s string) (*PruneOptions, error) {
	kv, err := ParsePropertyString(s)
	if err != nil {
		return nil, err
	}
	p := &PruneOptions{}
	fields := map[string]*int{
		"keep-last": &p.KeepLast, "keep-hourly": &p.KeepHourly, "keep-daily": &p.KeepDaily,
		"keep-weekly": &p.KeepWeekly, "keep-monthly": &p.KeepMonthly, "keep-yearly": &p.KeepYearly,
	}
	for k, v := range kv {
		if k == "keep-all" {
			b, err := decodeBool(Raw{Value: v})
			if err != nil {
				return nil, fmt.Errorf("keep-all: %w", err)
			}
			p.KeepAll = b
			continue
		}
		dst, ok := fields[k]
		if !ok {
			return nil, fmt.Errorf("unknown prune option %q", k)
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || !integerRe.MatchString(v) {
			return nil, fmt.Errorf("%s: expected a non-negative integer, got %q", k, v)
		}
		*dst = n
	}
	others := p.KeepLast + p.KeepHourly + p.KeepDaily + p.KeepWeekly + p.KeepMonthly + p.KeepYearly
	switch {
	case others == 0:
		return &PruneOptions{KeepAll: true}, nil
	case p.KeepAll:
		return nil, errors.New("keep-all cannot be set together with other options")
	}
	return p, nil
}

var contentTypes = []string{"backup", "none"}

var bwlimitKeys = []string{"clone", "default", "migration", "move", "restore"}

var numberRe = regexp.MustCompile(`^\d+(?:\.\d+)?$`)

func decodeBaseOptions(props map[string]Raw, consumed map[string]bool) (BaseOptions, []error) {
	var b BaseOptions
	var errs []error
	fail := func(key string, err error) { errs = append(errs, &PropertyError{Key: key, Err: err}) }

	take := func(key string) (Raw, bool) {
		r, ok := props[key]
		if ok {
			consumed[key] = true
		}
		return r, ok
	}

	if r, ok := take("content"); ok {
		items, err := decodeStringList(r, func(s string) bool { return slices.Contains(contentTypes, s) }, "content type")
		if err != nil {
			fail("content", err)
		} else {
			b.Content = map[string]bool{}
			for _, c := range items {
				b.Content[c] = true
			}
			if b.Content["none"] && len(b.Content) > 1 {
				fail("content", errors.New("unable to combine 'none' with other content types"))
			}
		}
	}
	if r, ok := take("nodes"); ok {
		items, err := decodeStringList(r, ValidNodeName, "node name")
		if err != nil {
			fail("nodes", err)
		} else {
			b.Nodes = items
		}
	}
	if r, ok := take("disable"); ok {
		v, err := decodeBool(r)
		if err != nil {
			fail("disable", err)
		}
		b.Disable = v
	}
	if r, ok := take("shared"); ok {
		v, err := decodeBool(r)
		if err != nil {
			fail("shared", err)
		}
		b.Shared = v
	}
	if r, ok := take("prune-backups"); ok {
		if err := checkRaw(r); err != nil {
			fail("prune-backups", err)
		} else if p, err := ParsePruneOptions(r.Value); err != nil {
			fail("prune-backups", err)
		} else {
			b.PruneBackups = p
		}
	}
	if r, ok := take("max-protected-backups"); ok {
		minimum := int64(-1)
		v, err := decodeInt(r, &minimum, nil)
		if err != nil {
			fail("max-protected-backups", err)
		} else {
			b.MaxProtectedBackups = &v
		}
	}
	if r, ok := take("bwlimit"); ok {
		b.BwLimit, errs = decodeBwLimit(r, errs)
	}
	return b, errs
}

func decodeBwLimit(r Raw, errs []error) (map[string]int64, []error) {
	fail := func(err error) []error { return append(errs, &PropertyError{Key: "bwlimit", Err: err}) }
	if err := checkRaw(r); err != nil {
		return nil, fail(err)
	}
	kv, err := ParsePropertyString(r.Value)
	if err != nil {
		return nil, fail(err)
	}
	out := map[string]int64{}
	for k, v := range kv {
		if !slices.Contains(bwlimitKeys, k) {
			return nil, fail(fmt.Errorf("unknown bandwidth limit key %q", k))
		}
		if !numberRe.MatchString(v) {
			return nil, fail(fmt.Errorf("%s: invalid limit %q", k, v))
		}
		f, _ := strconv.ParseFloat(v, 64)
		out[k] = int64(f)
	}
	return out, errs
}
