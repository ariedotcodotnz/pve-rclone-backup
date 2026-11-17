// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package retention_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/jobs"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo/repotest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/retention"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

func TestMain(m *testing.M) {
	cleanup, err := repotest.InitTransport()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// runDeletes runs delete jobs of storage "offsite" on r like the daemon
// does, and returns the store and a function queueing a deletion.
func runDeletes(t *testing.T, r *repo.Repo) (*store.Store, func(dir, volname string) int64) {
	st, queue := runDeleter(t, r, nil)
	return st, func(dir, volname string) int64 {
		return queue(retention.DeleteParams{RemoteDir: dir, Reason: "retention"}, volname)
	}
}

// runDeleter runs delete jobs with a Deleter whose InUse is inUse.
func runDeleter(t *testing.T, r *repo.Repo, inUse func(layout.BackupID) bool) (*store.Store, func(retention.DeleteParams, string) int64) {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.PutStorage(t.Context(), &store.StorageRow{StoreID: "offsite", Remote: r.Loc.Remote, BasePath: r.Loc.Path,
		Source: "homelab", ConfigHash: "x"}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Storage{ID: "offsite", Remote: r.Loc.Remote, Transfers: 1}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := &retention.Deleter{Log: log, Store: st, Repos: func(string) (*repo.Repo, *config.Storage, error) { return r, cfg, nil }}
	if inUse != nil {
		d.InUse = func(_ context.Context, _ string, id layout.BackupID) bool { return inUse(id) }
	}
	s := jobs.New(jobs.Options{Log: log, Store: st, Node: "pve1", Kind: "delete", Workers: 1, FreesSpace: true,
		Targets: func() []*config.Storage { return []*config.Storage{cfg} }, Poll: 20 * time.Millisecond, Runner: d})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	queue := func(p retention.DeleteParams, volname string) int64 {
		pj, _ := json.Marshal(p)
		id, _, err := st.InsertJob(t.Context(), &store.Job{Kind: "delete", StoreID: "offsite", State: "queued",
			DedupeKey: "delete:offsite:" + p.RemoteDir, OwnerNode: "pve1", BackupVolname: volname, ParamsJSON: string(pj)})
		if err != nil {
			t.Fatal(err)
		}
		s.Wake()
		return id
	}
	return st, queue
}

func waitJob(t *testing.T, st *store.Store, id int64, states ...string) *store.Job {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		j, err := st.GetJob(t.Context(), id)
		if err == nil && strings.Contains(" "+strings.Join(states, " ")+" ", " "+j.State+" ") {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d is %+v, %v; want %v", id, j, err, states)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A deletion that ran unmarked on a full remote and stopped half-way
// leaves a directory without a manifest, which a resync drops from the
// catalogue. The delete job still finishes it, but it never deletes a
// committed backup the catalogue does not know.
func TestDeleterFinishesUnmarkedDeletion(t *testing.T) {
	ctx := t.Context()
	r, _ := repotest.Init(t)
	half := repotest.Backup{VMID: 100, Size: 200 << 10}
	repotest.Write(t, r, half)
	tgt, err := r.Target(1)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{layout.ManifestName, layout.PartName(0)} {
		if err := tgt.Remove(ctx, half.ID().Path(name)); err != nil {
			t.Fatal(err)
		}
	}
	committed := repotest.Backup{VMID: 101, Size: 10 << 10}
	repotest.Write(t, r, committed)

	st, queue := runDeletes(t, r)
	dir := func(b repotest.Backup) string { return transport.CryptRoot(1) + "/" + b.ID().Dir() }
	finished := queue(dir(half), "backup/vzdump-qemu-100-x.vma.zst")
	refused := queue(dir(committed), "backup/vzdump-qemu-101-x.vma.zst")
	waitJob(t, st, finished, jobs.StateComplete)
	if j := waitJob(t, st, refused, jobs.StateFailed); !strings.Contains(j.LastError, "is not being deleted") {
		t.Fatalf("deletion of an uncatalogued committed backup = %+v", j)
	}
	scanned, err := r.Scan(ctx, "homelab")
	if err != nil || len(scanned) != 1 || scanned[0].ID != committed.ID() || scanned[0].State != repo.StateComplete {
		t.Fatalf("scan = %+v, %v", scanned, err)
	}

	// A backup already deleted elsewhere leaves nothing to do.
	gone := repotest.Backup{VMID: 102}
	waitJob(t, st, queue(dir(gone), "backup/vzdump-qemu-102-x.vma.zst"), jobs.StateComplete)
}

// Removing an incomplete upload: nothing is removed while an upload job
// works on it again, nor once it was committed meanwhile.
func TestDeleterPurgesIncompleteUploads(t *testing.T) {
	r, _ := repotest.Init(t)
	abandoned := repotest.Backup{VMID: 100, Size: 150 << 10}
	resumed := repotest.Backup{VMID: 101, Size: 150 << 10}
	committed := repotest.Backup{VMID: 102, Size: 10 << 10}
	repotest.Upload(t, r, abandoned)
	repotest.Upload(t, r, resumed)
	repotest.Write(t, r, committed)
	st, queue := runDeleter(t, r, func(id layout.BackupID) bool { return id.VMID == 101 })
	purge := func(b repotest.Backup) int64 {
		return queue(retention.DeleteParams{RemoteDir: transport.CryptRoot(1) + "/" + b.ID().Dir(), Reason: "test", Incomplete: true}, "")
	}
	waitJob(t, st, purge(abandoned), jobs.StateComplete)
	waitJob(t, st, purge(resumed), jobs.StateComplete)
	id := purge(committed)
	waitJob(t, st, id, jobs.StateComplete)
	events, err := st.JobEvents(t.Context(), id)
	if err != nil || !slices.ContainsFunc(events, func(e store.JobEvent) bool { return strings.Contains(e.Message, "holds a committed backup") }) {
		t.Fatalf("events of the purge of a committed backup = %+v, %v", events, err)
	}
	scanned, err := r.Scan(t.Context(), "homelab")
	if err != nil || len(scanned) != 2 || scanned[0].ID != resumed.ID() || scanned[0].State != repo.StateIncomplete ||
		scanned[1].ID != committed.ID() || scanned[1].State != repo.StateComplete {
		t.Fatalf("scan = %+v, %v", scanned, err)
	}
}
