// SPDX-License-Identifier: AGPL-3.0-or-later

package storages

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

var (
	// ErrInvalid marks a configuration that can never work as given.
	ErrInvalid = errors.New("invalid storage configuration")
	// ErrPrecondition marks a configuration that needs a setup step first.
	ErrPrecondition = errors.New("storage setup incomplete")
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// pveProps converts a section as parsed by PVE (the $scfg hash, encoded as
// JSON) back into raw storage.cfg properties.
func pveProps(section map[string]any) (map[string]config.Raw, error) {
	out := map[string]config.Raw{}
	for k, v := range section {
		if k == "type" || v == nil {
			continue
		}
		s, err := pveValue(k, v)
		if err != nil {
			return nil, err
		}
		out[k] = config.Raw{Value: s}
	}
	return out, nil
}

func pveValue(key string, v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case bool:
		if x {
			return "1", nil
		}
		return "0", nil
	case map[string]any:
		// content and nodes are sets ({backup => 1}); property strings
		// such as prune-backups are key/value maps.
		var parts []string
		for _, k := range slices.Sorted(maps.Keys(x)) {
			if key == "content" || key == "nodes" {
				if b, err := pveValue(key, x[k]); err == nil && b != "0" && b != "" {
					parts = append(parts, k)
				}
				continue
			}
			s, err := pveValue(key, x[k])
			if err != nil {
				return "", err
			}
			parts = append(parts, k+"="+s)
		}
		return strings.Join(parts, ","), nil
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			s, err := pveValue(key, e)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return strings.Join(parts, ","), nil
	}
	return "", invalidf("property %s has an unsupported value type %T", key, v)
}

// deletedKeys accepts PVE's delete parameter as a list or a comma/space
// separated string.
func deletedKeys(v any) []string {
	switch x := v.(type) {
	case string:
		return strings.FieldsFunc(x, func(r rune) bool { return r == ',' || r == ';' || r == ' ' })
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Validate checks a storage configuration for PVE's add and update hooks.
// It only uses local state: storage.cfg, remotes.conf, the key store and
// the repositories this node knows. It never contacts the remote, because
// the hooks run while PVE holds the storage configuration lock.
func (m *Manager) Validate(ctx context.Context, id string, req apiv1.ValidateRequest) (*apiv1.ValidateResponse, error) {
	if !config.ValidStorageID(id) {
		return nil, invalidf("invalid storage ID %q", id)
	}
	merged := maps.Clone(req.Config)
	if merged == nil {
		merged = map[string]any{}
	}
	maps.Copy(merged, req.Update)
	for _, k := range deletedKeys(req.Delete) {
		delete(merged, k)
	}
	if t, ok := merged["type"].(string); ok && t != config.StorageType {
		return nil, invalidf("storage type %q is not %s", t, config.StorageType)
	}
	props, err := pveProps(merged)
	if err != nil {
		return nil, err
	}
	sc, err := config.DecodeStorage(id, props)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	profile, err := checkRemote(sc)
	if err != nil {
		return nil, invalidf("%v; add it with 'pve-rclone-backup remote add %s'", err, sc.Remote)
	}
	if err := profile.CheckSegmentSize(sc.SegmentSize, sc.Encryption == "crypt"); err != nil {
		return nil, invalidf("rclone-segment-size: %v", err)
	}

	if err := m.Reload(ctx); err != nil {
		return nil, err
	}
	resp := &apiv1.ValidateResponse{}
	if err := m.checkSources(id, sc, resp); err != nil {
		return nil, err
	}

	// A storage may only be bound to a repository this node knows. An
	// update that keeps the binding needs no check (the node may have been
	// offline since the storage was added).
	m.mu.Lock()
	e := m.entries[id]
	unchanged := e != nil && e.binding == bindingHash(sc)
	m.mu.Unlock()
	if !unchanged {
		known, err := m.opts.Store.FindRepository(ctx, sc.Remote, sc.Path)
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: no repository is known at %s:%s; create it with 'pve-rclone-backup storage init %s' or import a recovery kit first",
				ErrPrecondition, sc.Remote, sc.Path, id)
		}
		if err != nil {
			return nil, err
		}
		if known.Encryption != sc.Encryption {
			return nil, invalidf("the repository at %s:%s uses encryption %q, not %q", sc.Remote, sc.Path, known.Encryption, sc.Encryption)
		}
		if sc.Encryption == "crypt" {
			if _, err := m.opts.Keys(known.UUID); err != nil {
				return nil, fmt.Errorf("%w: the keys of repository %s are not available: %w", ErrPrecondition, known.UUID, err)
			}
		}
	}

	if sc.Base.PruneBackups == nil {
		resp.Warnings = append(resp.Warnings, "prune-backups is not set: offsite backups are kept until deleted")
	}
	if len(sc.ReplicateFrom) == 0 {
		resp.Warnings = append(resp.Warnings, "rclone-replicate-from is empty: this storage only lists and restores offsite backups")
	}
	if sc.Immutable {
		resp.Warnings = append(resp.Warnings, "rclone-immutable is set: offsite backups cannot be deleted through this storage")
	}
	m.Wake()
	return resp, nil
}

// checkSources validates rclone-replicate-from against storage.cfg.
func (m *Manager) checkSources(id string, sc *config.Storage, resp *apiv1.ValidateResponse) error {
	m.mu.Lock()
	cfg := m.cfg
	m.mu.Unlock()
	for _, src := range sc.ReplicateFrom {
		if src == id {
			return invalidf("rclone-replicate-from: a storage cannot replicate from itself")
		}
		sec := cfg.Get(src)
		switch {
		case sec == nil:
			return invalidf("rclone-replicate-from: storage %q does not exist", src)
		case sec.Type == "pbs":
			return invalidf("rclone-replicate-from: %q is a Proxmox Backup Server storage; its backups are not archive files and cannot be replicated", src)
		case sec.Type == config.StorageType:
			return invalidf("rclone-replicate-from: %q is itself an offsite storage", src)
		}
		if _, ok := sec.Get("path"); !ok {
			return invalidf("rclone-replicate-from: %q (type %s) is not a file-based storage", src, sec.Type)
		}
		if content, ok := sec.Content(); ok && !content["backup"] {
			return invalidf("rclone-replicate-from: %q does not hold backups (content does not include 'backup')", src)
		} else if !ok {
			resp.Warnings = append(resp.Warnings, fmt.Sprintf("rclone-replicate-from: %q has no content setting; make sure it holds backups", src))
		}
	}
	return nil
}
