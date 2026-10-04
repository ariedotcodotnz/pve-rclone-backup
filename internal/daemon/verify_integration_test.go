// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package daemon

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/client"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo/repotest"
)

func waitJob(t *testing.T, c *client.Client, id int64) apiv1.Job {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var j apiv1.Job
		if err := c.Do(t.Context(), http.MethodGet, fmt.Sprintf("/v1/jobs/%d", id), nil, &j); err != nil {
			t.Fatal(err)
		}
		switch j.State {
		case "complete", "failed", "cancelled":
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d stuck in %s", id, j.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestVerificationEndpoints(t *testing.T) {
	ctx := t.Context()
	r, _ := repotest.Init(t)
	b := repotest.Backup{VMID: 100, Size: 100 << 10, SegmentSize: 32 << 10}
	repotest.Write(t, r, b)
	e := newEnv(t)
	e.keys = repotest.Loader(r.Keys)
	writeStorageCfg(t, e, localSection+offsiteSection("offsite", r.Loc.Remote, "homelab"))
	stop := start(t, e)
	defer func() { _ = stop() }()
	c := client.New(e.socket)
	waitStorage(t, c, "offsite", resynced)
	path := "/v1/storages/offsite/backups/" + strings.TrimPrefix(b.ID().Volname("vma.zst"), "backup/") + "/verify"

	var j apiv1.Job
	if err := c.Do(ctx, http.MethodPost, path, apiv1.VerifyRequest{Level: 3}, &j); err != nil {
		t.Fatal(err)
	}
	if j = waitJob(t, c, j.ID); j.State != "complete" || j.Kind != "verify" {
		t.Fatalf("verification = %+v", j)
	}
	var hist []apiv1.Verification
	if err := c.Do(ctx, http.MethodGet, "/v1/verifications?storage=offsite", nil, &hist); err != nil || len(hist) != 1 || hist[0].Result != "ok" || hist[0].Level != 3 {
		t.Fatalf("history = %+v, %v", hist, err)
	}
	var list []apiv1.Backup
	if err := c.Do(ctx, http.MethodGet, "/v1/storages/offsite/backups", nil, &list); err != nil || list[0].VerifyLevel != 3 {
		t.Fatalf("catalogue = %+v, %v", list, err)
	}
	for _, bad := range []apiv1.VerifyRequest{{Level: 4}, {Level: 9}} {
		if err := c.Do(ctx, http.MethodPost, path, bad, nil); !client.IsCode(err, apiv1.CodeInvalidArgument) {
			t.Errorf("level %d: %v", bad.Level, err)
		}
	}

	// A segment goes missing: the next listing check raises an alert.
	tgt, _ := r.Target(1)
	if err := tgt.Remove(ctx, b.ID().Path(layout.PartName(1))); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(ctx, http.MethodPost, path, apiv1.VerifyRequest{Level: 2}, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		st, err := c.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Alerts) == 1 && st.Alerts[0].ID == "damaged:offsite" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no damage alert: %+v", st.Alerts)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := c.Do(ctx, http.MethodGet, "/v1/storages/offsite/backups", nil, &list); err != nil || list[0].State != "damaged" || list[0].VerifyLevel != 0 {
		t.Fatalf("catalogue after damage = %+v, %v", list, err)
	}

	// Re-uploading the same bytes does not repair it: the ciphertext (and
	// its provider hash) differs from what was verified at upload.
	// Removing the damaged backup clears the alert.
	if err := r.Delete(ctx, 1, b.ID(), "test"); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(ctx, http.MethodPost, "/v1/storages/offsite/resync", nil, nil); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(20 * time.Second)
	for {
		st, _ := c.Status(ctx)
		if len(st.Alerts) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("damage alert not cleared: %+v", st.Alerts)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
