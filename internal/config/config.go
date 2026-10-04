// SPDX-License-Identifier: AGPL-3.0-or-later

// Package config holds the declarative configuration model: the
// rclone-backup storage.cfg properties, per-node settings and cluster-wide
// daemon settings. Types and decoders are generated from schema/*.yaml.
//
// Validation mirrors Proxmox VE's (anchored patterns, 0/1 booleans,
// split_list lists). Unlike PVE, which drops invalid values with a warning,
// every problem is returned: the daemon refuses to act on a storage whose
// configuration it does not fully understand.
package config

//go:generate go run ../../cmd/schemagen -root ../..

import (
	"embed"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// StorageType is the storage.cfg type of offsite repositories.
const StorageType = "rclone-backup"

// Schemas holds the generated JSON Schemas (storage, node, daemon).
//
//go:embed *.schema.json
var Schemas embed.FS

// DecodeStorage decodes the properties of an rclone-backup storage.cfg
// section (all keys except the type).
func DecodeStorage(id string, props map[string]Raw) (*Storage, error) {
	consumed := map[string]bool{}
	s, err := decodeStorage(props, consumed)
	s.ID = id
	base, baseErrs := decodeBaseOptions(props, consumed)
	s.Base = base
	errs := append([]error{err}, baseErrs...)
	errs = append(errs, unexpected(props, consumed)...)
	if s.Guests == "listed" && len(s.VMIDs) == 0 {
		errs = append(errs, &PropertyError{Key: "rclone-vmids", Err: errors.New(`required when rclone-guests is "listed"`)})
	}
	if err := errors.Join(errs...); err != nil {
		return s, fmt.Errorf("storage %q: %w", id, err)
	}
	return s, nil
}

func unexpected(props map[string]Raw, consumed map[string]bool) []error {
	var errs []error
	for _, k := range slices.Sorted(maps.Keys(props)) {
		if !consumed[k] {
			errs = append(errs, &PropertyError{Key: k, Err: errUnexpected})
		}
	}
	return errs
}

var kvLineRe = regexp.MustCompile(`^([a-z][a-z0-9_-]*):\s*(.*?)\s*$`)

// ParseKeyValue parses a PVE-style "key: value" file such as
// datacenter.cfg. Blank lines and lines starting with '#' are ignored.
func ParseKeyValue(raw []byte) (map[string]Raw, error) {
	out := map[string]Raw{}
	for i, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		m := kvLineRe.FindStringSubmatch(trimmed)
		if m == nil {
			return nil, fmt.Errorf("line %d: expected \"key: value\"", i+1)
		}
		if _, dup := out[m[1]]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", i+1, m[1])
		}
		out[m[1]] = Raw{Value: m[2], NoValue: m[2] == ""}
	}
	return out, nil
}

// DefaultNodeConfig returns the node settings with all defaults applied.
func DefaultNodeConfig() *NodeConfig { return defaultNodeConfig() }

// ParseNodeConfig parses /etc/pve/nodes/<node>/pve-rclone-backup.cfg.
func ParseNodeConfig(raw []byte) (*NodeConfig, error) {
	props, err := ParseKeyValue(raw)
	if err != nil {
		return nil, fmt.Errorf("node config: %w", err)
	}
	consumed := map[string]bool{}
	c, err := decodeNodeConfig(props, consumed)
	if err = errors.Join(append([]error{err}, unexpected(props, consumed)...)...); err != nil {
		return nil, fmt.Errorf("node config: %w", err)
	}
	return c, nil
}

// DefaultDaemonConfig returns the daemon settings with all defaults applied.
func DefaultDaemonConfig() *DaemonConfig { return defaultDaemonConfig() }

// ParseDaemonConfig parses /etc/pve/pve-rclone-backup/daemon.cfg.
func ParseDaemonConfig(raw []byte) (*DaemonConfig, error) {
	props, err := ParseKeyValue(raw)
	if err != nil {
		return nil, fmt.Errorf("daemon config: %w", err)
	}
	consumed := map[string]bool{}
	c, err := decodeDaemonConfig(props, consumed)
	if err = errors.Join(append([]error{err}, unexpected(props, consumed)...)...); err != nil {
		return nil, fmt.Errorf("daemon config: %w", err)
	}
	return c, nil
}
