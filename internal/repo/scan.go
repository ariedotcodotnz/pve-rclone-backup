// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/manifest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

// Backup states found by a scan.
const (
	StateComplete   = "complete"   // manifest valid, all segments present
	StateTombstoned = "tombstoned" // complete, deletion pending
	StateDamaged    = "damaged"    // manifest valid, segments missing or altered
	StateIncomplete = "incomplete" // no manifest: upload in progress or interrupted
	StateDeleting   = "deleting"   // deletion started and must be finished
	StateInvalid    = "invalid"    // unreadable or inconsistent manifest
)

// ScannedBackup is one backup directory found in the repository.
type ScannedBackup struct {
	Generation int
	ID         layout.BackupID
	State      string
	Manifest   *manifest.Manifest // nil unless the manifest could be read
	Meta       *manifest.Meta
	Problems   []string
	Files      []transport.Entry
}

// Scan walks every generation of a source and classifies each backup
// directory. It doubles as listing-based verification (presence, sizes and
// provider hashes) of every backup.
func (r *Repo) Scan(ctx context.Context, source string) ([]ScannedBackup, error) {
	if !layout.ValidSource(source) {
		return nil, fmt.Errorf("repo: invalid source %q", source)
	}
	var out []ScannedBackup
	for _, mg := range r.Marker.Generations {
		t, err := r.Target(mg.ID)
		if err != nil {
			return nil, err
		}
		found, err := r.scanGeneration(ctx, t, mg.ID, source)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

func listDirs(ctx context.Context, t *transport.Target, dir string) ([]string, error) {
	entries, err := t.List(ctx, dir)
	if transport.Classify(err) == transport.ClassNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.Dir {
			out = append(out, e.Name)
		}
	}
	return out, nil
}

func (r *Repo) scanGeneration(ctx context.Context, t *transport.Target, gen int, source string) ([]ScannedBackup, error) {
	var out []ScannedBackup
	srcDir := layout.SourceDir(source)
	types, err := listDirs(ctx, t, srcDir)
	if err != nil {
		return nil, err
	}
	for _, vmtype := range types {
		if vmtype != "qemu" && vmtype != "lxc" {
			continue
		}
		vmids, err := listDirs(ctx, t, path.Join(srcDir, vmtype))
		if err != nil {
			return nil, err
		}
		for _, vmid := range vmids {
			if _, err := strconv.Atoi(vmid); err != nil {
				continue
			}
			tsDirs, err := listDirs(ctx, t, path.Join(srcDir, vmtype, vmid))
			if err != nil {
				return nil, err
			}
			for _, ts := range tsDirs {
				id, err := layout.ParseDir(path.Join(srcDir, vmtype, vmid, ts))
				if err != nil {
					continue
				}
				b, err := r.scanBackup(ctx, t, gen, id)
				if err != nil {
					return nil, err
				}
				out = append(out, b)
			}
		}
	}
	return out, nil
}

func (r *Repo) scanBackup(ctx context.Context, t *transport.Target, gen int, id layout.BackupID) (ScannedBackup, error) {
	b := ScannedBackup{Generation: gen, ID: id}
	files, err := t.List(ctx, id.Dir())
	if err != nil {
		return b, err
	}
	b.Files = files
	has := func(name string) bool {
		return slices.ContainsFunc(files, func(e transport.Entry) bool { return !e.Dir && e.Name == name })
	}
	if has(layout.MetaName) {
		// A misplaced or unreadable meta document is ignored: only a meta
		// document bound to this backup may mark it as being deleted.
		// Any other error says nothing about this backup: the scan fails
		// rather than classify it from incomplete information.
		meta, err := r.ReadMeta(ctx, gen, id)
		switch {
		case err == nil:
			b.Meta = meta
		case errors.Is(err, ErrInvalidDocument):
			b.Problems = append(b.Problems, "meta: "+err.Error())
		default:
			return b, err
		}
	}
	if b.Meta != nil && b.Meta.Tombstone != nil && b.Meta.Tombstone.State == "deleting" {
		b.State = StateDeleting
		return b, nil
	}
	if !has(layout.ManifestName) {
		b.State = StateIncomplete
		return b, nil
	}
	m, err := r.ReadManifest(ctx, gen, id)
	if transport.Classify(err) == transport.ClassNotFound {
		// Removed since the listing (a deletion in progress elsewhere).
		b.State = StateIncomplete
		return b, nil
	}
	if err != nil {
		if !errors.Is(err, ErrInvalidDocument) {
			return b, err
		}
		b.State = StateInvalid
		b.Problems = append(b.Problems, "manifest: "+err.Error())
		return b, nil
	}
	b.Manifest = m
	if problems := checkParts(m, files); len(problems) > 0 {
		b.State = StateDamaged
		b.Problems = append(b.Problems, problems...)
		return b, nil
	}
	b.State = StateComplete
	if b.Meta != nil && b.Meta.Tombstone != nil {
		b.State = StateTombstoned
	}
	return b, nil
}
