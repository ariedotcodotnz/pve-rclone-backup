// SPDX-License-Identifier: AGPL-3.0-or-later

// Package catalog rebuilds the local catalogue (a cache in SQLite) from the
// remote repository, which is the authority on what exists offsite.
package catalog

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/manifest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

// Report summarizes a resync.
type Report struct {
	Complete   int
	Tombstoned int
	Damaged    int
	Incomplete int
	Deleting   int
	Invalid    int
	// Pending lists directories that need follow-up work: interrupted
	// uploads (incomplete) and interrupted deletions (deleting).
	Pending  []repo.ScannedBackup
	Problems []string
}

// Entry converts a scanned complete, tombstoned or damaged backup into a
// catalogue row.
func Entry(storeID string, b repo.ScannedBackup, now time.Time) (*store.Backup, error) {
	m := b.Manifest
	if m == nil {
		return nil, fmt.Errorf("catalog: %s has no manifest", b.ID)
	}
	data, err := manifest.Encode(m)
	if err != nil {
		return nil, err
	}
	e := &store.Backup{
		StoreID: storeID, Volname: m.Backup.Volname, VMType: m.Backup.VMType, VMID: m.Backup.VMID,
		BackupTime: m.Backup.BackupTime, TSLabel: m.Backup.TSLabel, CollisionIndex: m.Backup.CollisionIndex,
		RemoteDir: transport.CryptRoot(b.Generation) + "/" + b.ID.Dir(), Generation: b.Generation,
		ArchiveSize: m.Archive.Size, ArchiveSHA256: m.Archive.SHA256, ArchiveFormat: m.Archive.Format,
		Compression: m.Archive.Compression, SegmentSize: m.Segments.Size, SegmentCount: m.Segments.Count,
		GuestName: m.Guest.Name, Notes: m.Sidecars.NotesAtUpload, Protected: m.Sidecars.ProtectedAtUpload,
		UploadedAt: m.UploadedAt.Unix(), ManifestJSON: string(data),
	}
	if b.Meta != nil {
		e.Notes, e.Protected = b.Meta.Notes, b.Meta.Protected
		if t := b.Meta.Tombstone; t != nil {
			after, at := t.DeleteAfter.Unix(), t.RequestedAt.Unix()
			e.DeleteAfter, e.TombstoneAt, e.TombstoneReason, e.TombstoneBy = &after, &at, t.Reason, t.By
		}
	}
	// The remote meta document is unusable, so notes, protection and
	// tombstone are unknown: store.ReplaceCatalog keeps the catalogue's own
	// values, and a backup the catalogue does not know yet is treated as
	// protected, so nothing deletes it until the document is readable.
	// Nothing is written back: the document may be a newer daemon's.
	if b.MetaInvalid {
		e.MetaUnknown, e.Protected = true, true
	}
	switch b.State {
	case repo.StateComplete:
		// The scan checked presence, sizes and provider hashes (level 2).
		e.State, e.VerifyLevel, e.VerifyResult = "complete", 2, "ok"
		at := now.Unix()
		e.VerifiedAt = &at
	case repo.StateTombstoned:
		e.State = "tombstoned"
	case repo.StateDamaged:
		e.State, e.VerifyResult = "damaged", "damaged"
	default:
		return nil, fmt.Errorf("catalog: %s is %s, not catalogued", b.ID, b.State)
	}
	return e, nil
}

// Resync scans a source in the repository and atomically replaces the
// storage's catalogue.
func Resync(ctx context.Context, st *store.Store, storeID string, r *repo.Repo, source string) (*Report, error) {
	scanned, err := r.Scan(ctx, source)
	if err != nil {
		return nil, fmt.Errorf("catalog: scan %s: %w", source, err)
	}
	now := time.Now()
	rep := &Report{}
	var entries []*store.Backup
	seen := map[string]bool{}
	for _, b := range scanned {
		switch b.State {
		case repo.StateComplete, repo.StateTombstoned, repo.StateDamaged:
			e, err := Entry(storeID, b, now)
			if err != nil {
				return nil, err
			}
			if seen[e.Volname] {
				rep.Problems = append(rep.Problems, fmt.Sprintf("%s exists in several generations; keeping generation %d", e.Volname, b.Generation))
				continue
			}
			seen[e.Volname] = true
			entries = append(entries, e)
			switch b.State {
			case repo.StateComplete:
				rep.Complete++
			case repo.StateTombstoned:
				rep.Tombstoned++
			default:
				rep.Damaged++
			}
			for _, p := range b.Problems {
				rep.Problems = append(rep.Problems, b.ID.String()+": "+p)
			}
		case repo.StateIncomplete:
			rep.Incomplete++
			rep.Pending = append(rep.Pending, b)
		case repo.StateDeleting:
			rep.Deleting++
			rep.Pending = append(rep.Pending, b)
		case repo.StateInvalid:
			rep.Invalid++
			for _, p := range b.Problems {
				rep.Problems = append(rep.Problems, b.ID.String()+": "+p)
			}
		}
	}
	if err := st.ReplaceCatalog(ctx, storeID, entries); err != nil {
		return nil, err
	}
	return rep, nil
}

// RemoteID returns the generation and identity of a catalogue entry from
// its remote directory ("g<N>/v1/<source>/...").
func RemoteID(b *store.Backup) (int, layout.BackupID, error) {
	root, dir, ok := strings.Cut(b.RemoteDir, "/")
	gen, err := strconv.Atoi(strings.TrimPrefix(root, "g"))
	if !ok || err != nil || !strings.HasPrefix(root, "g") || gen < 1 {
		return 0, layout.BackupID{}, fmt.Errorf("catalog: invalid remote directory %q", b.RemoteDir)
	}
	id, err := layout.ParseDir(dir)
	return gen, id, err
}
