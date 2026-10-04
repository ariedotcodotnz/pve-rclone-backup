// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build integration

package transport

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	onedriveapi "github.com/rclone/rclone/backend/onedrive/api"
	"github.com/rclone/rclone/fs/fserrors"
	"github.com/rclone/rclone/lib/pacer"
	"golang.org/x/oauth2"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/secrets"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport/faultfs"
)

// faultyRepo configures remote "<name>" as a faulty wrapper over a local
// directory and returns a crypt target for generation 1 on top of it.
func faultyRepo(t *testing.T, ctl *faultfs.Controller) (*Target, *Target, string) {
	t.Helper()
	dir := t.TempDir()
	name := fmt.Sprintf("fx%d", time.Now().UnixNano())
	faultfs.Register(name, ctl)
	testStorage.SetSection(name, map[string]string{"type": "faulty", "id": name, "remote": dir})
	loc := RepoLocation{Remote: name, Path: "pve-backups"}
	g, err := secrets.NewGeneration(1, "base32768", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	crypted, err := OpenCrypt(t.Context(), loc, g)
	if err != nil {
		t.Fatal(err)
	}
	base, err := OpenBase(t.Context(), loc)
	if err != nil {
		t.Fatal(err)
	}
	return crypted, base, dir
}

func TestRepoTargetsAndDocuments(t *testing.T) {
	ctl := &faultfs.Controller{}
	crypted, base, dir := faultyRepo(t, ctl)

	if _, err := base.PutBytes(t.Context(), "pve-rclone-backup.repo.json", []byte(`{"format":"x"}`)); err != nil {
		t.Fatal(err)
	}
	doc := []byte(`{"format":"pve-rclone-backup.manifest","version":1}`)
	if _, err := crypted.PutBytes(t.Context(), "v1/homelab/qemu/100/ts/manifest.json", doc); err != nil {
		t.Fatal(err)
	}
	got, err := crypted.ReadAll(t.Context(), "v1/homelab/qemu/100/ts/manifest.json", 1<<20)
	if err != nil || !bytes.Equal(got, doc) {
		t.Fatalf("read back %q, %v", got, err)
	}
	if _, err := crypted.ReadAll(t.Context(), "v1/homelab/qemu/100/ts/manifest.json", 10); err == nil {
		t.Fatal("size limit not enforced")
	}

	entries, err := crypted.List(t.Context(), "v1/homelab/qemu/100/ts")
	if err != nil || len(entries) != 1 || entries[0].Name != "manifest.json" || entries[0].Size != int64(len(doc)) ||
		entries[0].StoredSize != CryptStoredSize(int64(len(doc))) || entries[0].StoredHash == "" {
		t.Fatalf("list = %+v, %v", entries, err)
	}
	if top, _ := crypted.List(t.Context(), ""); len(top) != 1 || !top[0].Dir || top[0].Name != "v1" {
		t.Fatalf("top listing = %+v", top)
	}
	// On disk: plaintext marker at the base, encrypted names below g1/.
	if _, err := os.Stat(filepath.Join(dir, "pve-backups", "pve-rclone-backup.repo.json")); err != nil {
		t.Fatal("marker not stored in plaintext at the repository base")
	}
	g1, _ := os.ReadDir(filepath.Join(dir, "pve-backups", "g1"))
	if len(g1) != 1 || g1[0].Name() == "v1" {
		t.Fatalf("crypt root content %v", g1)
	}

	res, err := crypted.Probe(t.Context())
	if err != nil || res.HashType == "" {
		t.Fatalf("probe: %+v %v", res, err)
	}
	if left, _ := crypted.List(t.Context(), ""); len(left) != 1 {
		t.Fatalf("probe left objects behind: %+v", left)
	}

	if err := crypted.Remove(t.Context(), "v1/homelab/qemu/100/ts/manifest.json"); err != nil {
		t.Fatal(err)
	}
	if err := crypted.Rmdir(t.Context(), "v1/homelab/qemu/100/ts"); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidLocationsRejected(t *testing.T) {
	crypted, _, _ := faultyRepo(t, &faultfs.Controller{})
	if _, err := crypted.PutBytes(t.Context(), "doc", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	other, err := secrets.NewGeneration(1, "base32768", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenCrypt(t.Context(), RepoLocation{Remote: "Bad Name", Path: "x"}, other); err == nil {
		t.Fatal("invalid remote name accepted")
	}
	if _, err := OpenCrypt(t.Context(), RepoLocation{Remote: "ok", Path: "../escape"}, other); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestInjectedFaultsAreClassified(t *testing.T) {
	qe := &onedriveapi.Error{}
	qe.ErrorInfo.Code = "quotaLimitReached"
	quota := fserrors.FatalError(qe)
	cases := []struct {
		name string
		ctl  *faultfs.Controller
		want Class
	}{
		{"quota", &faultfs.Controller{PutError: func(string, int) error { return quota }}, ClassQuota},
		{"auth", &faultfs.Controller{PutError: func(string, int) error {
			return fmt.Errorf("couldn't fetch token: %w", &oauth2.RetrieveError{ErrorCode: "invalid_grant"})
		}}, ClassAuth},
		{"corrupted upload", &faultfs.Controller{Corrupt: func(string) bool { return true }}, ClassIntegrity},
		{"wrong provider hash", &faultfs.Controller{WrongHash: func(string) bool { return true }}, ClassIntegrity},
		{"persistent network error", &faultfs.Controller{PutError: func(string, int) error {
			return fserrors.RetryErrorf("connection reset by peer")
		}}, ClassTransient},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			crypted, _, _ := faultyRepo(t, c.ctl)
			data := bytes.Repeat([]byte("x"), 100_000)
			_, err := crypted.PutSegment(t.Context(), "seg", Segment{Source: bytes.NewReader(data), Size: int64(len(data))}, nil)
			if err == nil {
				t.Fatal("fault not reported")
			}
			if got := Classify(err); got != c.want {
				t.Fatalf("class %q, want %q (%v)", got, c.want, err)
			}
			if _, err := crypted.Stat(t.Context(), "seg"); err == nil {
				t.Fatal("failed upload left an object behind")
			}
		})
	}
}

func TestThrottlingIsRetried(t *testing.T) {
	ctl := &faultfs.Controller{PutError: func(_ string, attempt int) error {
		if attempt == 1 {
			return pacer.RetryAfterError(errors.New("429 too many requests"), 100*time.Millisecond)
		}
		return nil
	}}
	crypted, _, _ := faultyRepo(t, ctl)
	data := []byte(strings.Repeat("y", 70_000))
	if _, err := crypted.PutSegment(t.Context(), "seg", Segment{Source: bytes.NewReader(data), Size: int64(len(data))}, nil); err != nil {
		t.Fatalf("throttled upload not retried: %v", err)
	}
	if ctl.TotalPuts() != 2 {
		t.Fatalf("puts = %d, want 2", ctl.TotalPuts())
	}
}

func TestHiddenObjectsAreMissing(t *testing.T) {
	ctl := &faultfs.Controller{}
	crypted, _, _ := faultyRepo(t, ctl)
	if _, err := crypted.PutBytes(t.Context(), "dir/a", []byte("a")); err != nil {
		t.Fatal(err)
	}
	ctl.Hide = func(string) bool { return true }
	if _, err := crypted.Stat(t.Context(), "dir/a"); Classify(err) != ClassNotFound {
		t.Fatalf("hidden object: %v", err)
	}
}
