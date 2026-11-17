// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package daemon

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/client"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/jobs"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo/repotest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

// incompleteSetup returns a repository holding a committed backup of
// guest 100 and uploads that never finished of guests 101 (no job knows
// it), 102 (its job ended source_lost) and 103 (its job waits to retry),
// and an environment whose state database holds those jobs.
func incompleteSetup(t *testing.T, daemonCfg string) (*repo.Repo, env, map[int]string) {
	t.Helper()
	r, _ := repotest.Init(t)
	at := time.Date(2026, 10, 28, 21, 0, 6, 0, time.Local)
	repotest.Write(t, r, repotest.Backup{VMID: 100, Time: at, Size: 10 << 10})
	dirs := map[int]string{}
	for _, vmid := range []int{101, 102, 103} {
		b := repotest.Backup{VMID: vmid, Time: at, Size: 150 << 10}
		repotest.Upload(t, r, b)
		dirs[vmid] = "g1/" + b.ID().Dir()
	}
	e := newEnv(t)
	e.keys = repotest.Loader(r.Keys)
	if err := os.MkdirAll(e.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), filepath.Join(e.stateDir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(24 * time.Hour).Unix()
	for vmid, state := range map[int]string{102: jobs.StateSourceLost, 103: jobs.StateRetryWait} {
		if _, _, err := st.InsertJob(t.Context(), &store.Job{Kind: "replicate", StoreID: "offsite", State: state,
			DedupeKey: fmt.Sprint("replicate:", vmid), OwnerNode: "pve-test", VMType: "qemu", VMID: vmid, BackupTime: at.Unix(),
			NextAttemptAt: &later}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.pveDir, "pve-rclone-backup"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.pveDir, "pve-rclone-backup", "daemon.cfg"), []byte(daemonCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	writeStorageCfg(t, e, localSection+strings.TrimSuffix(offsiteSection("offsite", r.Loc.Remote, "homelab"), "\n")+
		"\trclone-replicate-from local\n\n")
	return r, e, dirs
}

// remoteGuests returns the guests with a backup directory on the remote.
func remoteGuests(t *testing.T, r *repo.Repo) []int {
	t.Helper()
	scanned, err := r.Scan(t.Context(), "homelab")
	if err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, b := range scanned {
		out = append(out, b.ID.VMID)
	}
	slices.Sort(out)
	return out
}

func waitGuests(t *testing.T, r *repo.Repo, want ...int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !slices.Equal(remoteGuests(t, r), want) {
		if time.Now().After(deadline) {
			t.Fatalf("guests on the remote = %v, want %v", remoteGuests(t, r), want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func incompleteByGuest(t *testing.T, c *client.Client) map[int]apiv1.IncompleteUpload {
	t.Helper()
	var list []apiv1.IncompleteUpload
	if err := c.Do(t.Context(), http.MethodGet, "/v1/storages/offsite/incomplete", nil, &list); err != nil {
		t.Fatal(err)
	}
	out := map[int]apiv1.IncompleteUpload{}
	for _, u := range list {
		out[u.VMID] = u
	}
	return out
}

// TestIncompleteUploadsAreRemoved: uploads that never finished take quota
// but are never listed. They are removed automatically: at once when
// their archive is gone, after incomplete-upload-grace when nothing works
// on them, never while an upload job of this node does.
func TestIncompleteUploadsAreRemoved(t *testing.T) {
	ctx := t.Context()
	r, e, dirs := incompleteSetup(t, "incomplete-upload-grace: 2s\n")
	stop := start(t, e)
	defer func() { _ = stop() }()
	c := client.New(e.socket)
	waitStorage(t, c, "offsite", resynced)

	// The upload of an archive that is gone is removed at once.
	waitGuests(t, r, 100, 101, 103)
	got := incompleteByGuest(t, c)
	if u := got[101]; len(got) != 2 || u.Dir != dirs[101] || u.InUse || u.Job != 0 || u.Files != 3 || u.RemoveAfter == nil ||
		u.RemoveAfter.Sub(u.UnchangedSince) != 2*time.Second {
		t.Fatalf("abandoned upload = %+v (of %d)", u, len(got))
	}
	if u := got[103]; !u.InUse || u.JobState != jobs.StateRetryWait || u.RemoveAfter != nil {
		t.Fatalf("upload in use = %+v", u)
	}
	var s apiv1.Storage
	if err := c.Do(ctx, http.MethodGet, "/v1/storages/offsite", nil, &s); err != nil {
		t.Fatal(err)
	}
	if sp := s.Space; sp == nil || sp.Incomplete != 1 || sp.IncompleteBytes != got[101].Bytes || sp.InProgress != 1 ||
		sp.InProgressBytes != got[103].Bytes || len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "an upload that never finished takes") {
		t.Fatalf("space = %+v, warnings %q", s.Space, s.Warnings)
	}

	// Unchanged for the grace period, the upload nothing works on is
	// removed at the next resync; the one in use stays.
	time.Sleep(2100 * time.Millisecond)
	if err := c.Do(ctx, http.MethodPost, "/v1/storages/offsite/resync", nil, nil); err != nil {
		t.Fatal(err)
	}
	waitGuests(t, r, 100, 103)
	deadline := time.Now().Add(10 * time.Second)
	for len(incompleteByGuest(t, c)) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("removed uploads still listed: %+v", incompleteByGuest(t, c))
		}
		time.Sleep(50 * time.Millisecond)
	}
	var deletes []apiv1.Job
	if err := c.Do(ctx, http.MethodGet, "/v1/jobs?kind=delete", nil, &deletes); err != nil || len(deletes) != 2 {
		t.Fatalf("delete jobs = %+v, %v", deletes, err)
	}
	for _, j := range deletes {
		if j.State != jobs.StateComplete || j.VMType != "qemu" {
			t.Fatalf("delete job = %+v", j)
		}
	}
}

// TestPurgeIncompleteUploads: in a cluster, an upload no job of this node
// knows may be another node's, so it is only removed on request.
func TestPurgeIncompleteUploads(t *testing.T) {
	ctx := t.Context()
	r, e, dirs := incompleteSetup(t, "")
	if err := os.WriteFile(filepath.Join(e.pveDir, ".members"), []byte(`{"nodename":"pve-test","cluster":{"name":"lab"},
		"nodelist":{"pve-test":{"online":1},"pve-other":{"online":0}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := start(t, e)
	defer func() { _ = stop() }()
	c := client.New(e.socket)
	waitStorage(t, c, "offsite", resynced)
	waitGuests(t, r, 100, 101, 103) // 102's archive is gone
	if u := incompleteByGuest(t, c)[101]; u.RemoveAfter != nil {
		t.Fatalf("an upload another node may be working on is removed automatically: %+v", u)
	}

	path := "/v1/storages/offsite/incomplete/purge"
	if err := c.Do(ctx, http.MethodPost, path, apiv1.PurgeIncompleteRequest{Dirs: []string{dirs[103]}}, nil); !client.IsCode(err, apiv1.CodeConflict) {
		t.Fatalf("purge of an upload in use: %v", err)
	}
	if err := c.Do(ctx, http.MethodPost, path, apiv1.PurgeIncompleteRequest{Dirs: []string{"g1/v1/homelab/qemu/100/x"}}, nil); !client.IsCode(err, apiv1.CodeNotFound) {
		t.Fatalf("purge of an unknown directory: %v", err)
	}
	var out []apiv1.PurgedUpload
	if err := c.Do(ctx, http.MethodPost, path, apiv1.PurgeIncompleteRequest{DryRun: true}, &out); err != nil ||
		len(out) != 1 || out[0].Dir != dirs[101] || out[0].PurgeJob != 0 {
		t.Fatalf("dry run = %+v, %v", out, err)
	}
	waitGuests(t, r, 100, 101, 103)
	if err := c.Do(ctx, http.MethodPost, path, apiv1.PurgeIncompleteRequest{}, &out); err != nil || len(out) != 1 || out[0].PurgeJob == 0 {
		t.Fatalf("purge = %+v, %v", out, err)
	}
	waitGuests(t, r, 100, 103)
	if j := waitJob(t, c, out[0].PurgeJob); j.State != jobs.StateComplete {
		t.Fatalf("purge job = %+v", j)
	}

	// The same files left behind again are removed again, by the same job.
	repotest.Upload(t, r, repotest.Backup{VMID: 101, Time: time.Date(2026, 10, 28, 21, 0, 6, 0, time.Local), Size: 150 << 10})
	if err := c.Do(ctx, http.MethodPost, "/v1/storages/offsite/resync", nil, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for incompleteByGuest(t, c)[101].Dir == "" {
		if time.Now().After(deadline) {
			t.Fatal("the upload left behind again was not found")
		}
		time.Sleep(50 * time.Millisecond)
	}
	var again []apiv1.PurgedUpload
	if err := c.Do(ctx, http.MethodPost, path, apiv1.PurgeIncompleteRequest{Dirs: []string{dirs[101]}}, &again); err != nil ||
		len(again) != 1 || again[0].PurgeJob != out[0].PurgeJob {
		t.Fatalf("second purge = %+v, %v", again, err)
	}
	waitGuests(t, r, 100, 103)
	if j := waitJob(t, c, out[0].PurgeJob); j.State != jobs.StateComplete {
		t.Fatalf("purge job = %+v", j)
	}
}
