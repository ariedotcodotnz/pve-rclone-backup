// SPDX-License-Identifier: AGPL-3.0-or-later

package secrets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/rclone/rclone/fs/config"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/pve/cfs"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testDir(t *testing.T) (dir string, lock *cfs.Locker) {
	t.Helper()
	dir = t.TempDir()
	return dir, &cfs.Locker{Dir: filepath.Join(dir, "lock"), AcquireTimeout: 10 * time.Second}
}

func TestINIRoundTrip(t *testing.T) {
	in := sections{"od": {"type": "onedrive", "token": `{"access_token":"a=b"}`}, "b": {"type": "local"}}
	data, err := encodeINI(in)
	if err != nil {
		t.Fatal(err)
	}
	out, err := parseINI(data)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(out) != fmt.Sprint(in) {
		t.Fatalf("round trip: %v != %v", out, in)
	}
	rcloneStyle := "# comment\n[od]\ntype = onedrive\ndrive_type = personal\n; another\n\n[x]\nk=v\n"
	if s, err := parseINI([]byte(rcloneStyle)); err != nil || s["od"]["drive_type"] != "personal" || s["x"]["k"] != "v" {
		t.Fatalf("rclone style: %v %v", s, err)
	}
	for _, bad := range []string{"k = v\n", "[a\n", "[a]\n[a]\n", "[a]\nnovalue\n"} {
		if _, err := parseINI([]byte(bad)); err == nil {
			t.Errorf("parsed %q", bad)
		}
	}
}

func TestRemoteStoreBasics(t *testing.T) {
	dir, lock := testDir(t)
	path := RemotesPath(dir)
	s := NewRemoteStore(path, lock, quiet())
	if err := s.Load(); !errors.Is(err, config.ErrorConfigFileNotFound) {
		t.Fatalf("first load of missing file: %v", err)
	}
	s.SetValue("od", "type", "onedrive")
	s.SetValue("od", "token", "t1")
	if v, ok := s.GetValue("od", "token"); !ok || v != "t1" || s.Pending() != 2 {
		t.Fatalf("value %q %v pending %d", v, ok, s.Pending())
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("remotes file mode %v", fi.Mode().Perm())
	}
	if s.Pending() != 0 {
		t.Fatal("changes still pending after save")
	}
	s.SetValue("od", "token", "t1") // unchanged value records nothing
	if s.Pending() != 0 {
		t.Fatal("no-op change recorded")
	}
	if !s.DeleteKey("od", "token") || s.DeleteKey("od", "token") {
		t.Fatal("DeleteKey")
	}
	s.DeleteSection("od")
	if s.HasSection("od") || len(s.GetSectionList()) != 0 {
		t.Fatal("section not deleted")
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	other := NewRemoteStore(path, lock, quiet())
	if err := other.Load(); err != nil || other.HasSection("od") {
		t.Fatalf("deletion not persisted: %v", err)
	}
}

// Two nodes changing the shared file must not lose each other's updates.
func TestConcurrentNodesMergeChanges(t *testing.T) {
	dir, lock := testDir(t)
	path := RemotesPath(dir)
	seed := NewRemoteStore(path, lock, quiet())
	_ = seed.Load()
	seed.SetValue("od", "type", "onedrive")
	seed.SetValue("od", "token", "initial")
	if err := seed.Save(); err != nil {
		t.Fatal(err)
	}

	a := NewRemoteStore(path, lock, quiet())
	b := NewRemoteStore(path, lock, quiet())
	_ = a.Load()
	_ = b.Load()

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			a.SetValue("od", "token", fmt.Sprintf("refreshed-%d", i))
			if err := a.Save(); err != nil {
				t.Error(err)
			}
		})
		wg.Go(func() {
			b.SetValue(fmt.Sprintf("remote%d", i), "type", "local")
			if err := b.Save(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()

	final := NewRemoteStore(path, lock, quiet())
	_ = final.Load()
	for i := range 20 {
		if !final.HasSection(fmt.Sprintf("remote%d", i)) {
			t.Errorf("remote%d lost", i)
		}
	}
	tok, _ := final.GetValue("od", "token")
	if tok == "initial" || tok == "" {
		t.Errorf("token refresh lost: %q", tok)
	}
	if typ, _ := final.GetValue("od", "type"); typ != "onedrive" {
		t.Error("existing key lost")
	}
	if err := a.Load(); err != nil || !a.HasSection("remote7") {
		t.Fatal("other node's changes not visible after reload")
	}
}

type failingLock struct{ err error }

func (f *failingLock) Do(context.Context, string, func(context.Context) error) error { return f.err }

func TestPendingSurvivesNoQuorum(t *testing.T) {
	dir, lock := testDir(t)
	fl := &failingLock{err: cfs.ErrNoQuorum}
	s := NewRemoteStore(RemotesPath(dir), fl, quiet())
	_ = s.Load()
	s.SetValue("od", "token", "fresh")
	if err := s.Save(); !errors.Is(err, cfs.ErrNoQuorum) {
		t.Fatalf("save without quorum: %v", err)
	}
	if v, _ := s.GetValue("od", "token"); v != "fresh" || s.Pending() != 1 {
		t.Fatal("in-memory token lost while the cluster filesystem is read-only")
	}
	s.lock = lock
	if err := s.Flush(t.Context()); err != nil || s.Pending() != 0 {
		t.Fatalf("flush after quorum returned: %v", err)
	}
}

func TestUnreadableFileKeepsLastGoodState(t *testing.T) {
	dir, lock := testDir(t)
	path := RemotesPath(dir)
	s := NewRemoteStore(path, lock, quiet())
	_ = s.Load()
	s.SetValue("od", "type", "onedrive")
	_ = s.Save()
	if err := os.WriteFile(path, []byte("[broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Load(); err != nil {
		t.Fatalf("Load must never fail (rclone would exit): %v", err)
	}
	if !s.HasSection("od") {
		t.Fatal("last good configuration dropped")
	}
}

func TestKeyStore(t *testing.T) {
	dir, lock := testDir(t)
	ks := NewKeyStore(dir, lock)
	const uuid = "6f0c2f1e-3a7b-4c2d-9e8f-0123456789ab"
	if _, err := ks.Load(uuid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing keys: %v", err)
	}
	k, err := NewRepoKeys(uuid, "base32768", time.Unix(1790000000, 0))
	if err != nil {
		t.Fatal(err)
	}
	g := k.Generations[0]
	if len(g.Password) < 40 || g.Password == g.Password2 || g.FilenameEncryption != "standard" || !g.DirectoryNameEncryption {
		t.Fatalf("generation = %+v", g)
	}
	if err := ks.Save(t.Context(), k); err != nil {
		t.Fatal(err)
	}
	got, err := ks.Load(uuid)
	if err != nil || got.Generations[0].Password != g.Password {
		t.Fatalf("load: %+v %v", got, err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "keys", uuid+".json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("keys file mode %v", fi.Mode().Perm())
	}

	// Rotation adds a generation; older ones become retired but stay.
	g2, _ := NewGeneration(2, "base32768", time.Now())
	got.Generations[0].State = "retired"
	got.Generations = append(got.Generations, g2)
	if err := ks.Save(t.Context(), got); err != nil {
		t.Fatal(err)
	}
	if a, _ := got.Active(); a.ID != 2 {
		t.Fatalf("active generation %d", a.ID)
	}

	dropped := *got
	dropped.Generations = dropped.Generations[1:]
	if err := ks.Save(t.Context(), &dropped); !errors.Is(err, ErrWouldDropKeys) {
		t.Fatalf("dropping a generation: %v", err)
	}
	changed, _ := ks.Load(uuid)
	changed.Generations[0].Password = "different"
	if err := ks.Save(t.Context(), changed); !errors.Is(err, ErrWouldDropKeys) {
		t.Fatalf("changing a generation's secret: %v", err)
	}
	if _, err := ks.Load("../../etc/passwd"); err == nil {
		t.Fatal("path traversal via UUID")
	}
	if _, err := NewGeneration(1, "rot13", time.Now()); err == nil {
		t.Fatal("unknown encoding accepted")
	}
}

func TestMergeKeys(t *testing.T) {
	now := time.Unix(1790000000, 0)
	local, err := NewRepoKeys("6f0c2f1e-3a7b-4c2d-9e8f-0123456789ab", "base32768", now)
	if err != nil {
		t.Fatal(err)
	}
	if got, changed, err := MergeKeys(local, local); err != nil || changed || got != local {
		t.Fatalf("merge with itself: %v %v", changed, err)
	}
	g2, _ := NewGeneration(2, "base32768", now)
	newer := *local
	newer.Generations = append(append([]Generation(nil), local.Generations...), g2)
	got, changed, err := MergeKeys(local, &newer)
	if err != nil || !changed || len(got.Generations) != 2 {
		t.Fatalf("merge newer: %+v %v %v", got, changed, err)
	}
	// An older kit (fewer generations) adds nothing.
	if got, changed, err := MergeKeys(&newer, local); err != nil || changed || len(got.Generations) != 2 {
		t.Fatalf("merge older: %v %v", changed, err)
	}
	other, _ := NewRepoKeys(local.RepoUUID, "base32768", now)
	if _, _, err := MergeKeys(local, other); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("conflicting keys: %v", err)
	}
	if got, changed, err := MergeKeys(nil, local); err != nil || !changed || got != local {
		t.Fatalf("merge into nothing: %v %v", changed, err)
	}
}

// TestRemoteStoreSeesOtherNodesChanges: a token another node refreshed is
// read from the file, without losing this node's unsaved changes.
func TestRemoteStoreSeesOtherNodesChanges(t *testing.T) {
	dir, lock := testDir(t)
	path := RemotesPath(dir)
	seed := NewRemoteStore(path, lock, quiet())
	_ = seed.Load()
	seed.SetValue("od", "type", "onedrive")
	seed.SetValue("od", "token", "t1")
	if err := seed.Save(); err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1790000000, 0)
	here := NewRemoteStore(path, lock, quiet())
	here.now = func() time.Time { return now }
	if err := here.Load(); err != nil {
		t.Fatal(err)
	}
	here.SetValue("od", "drive_id", "local-change") // not saved yet

	other := NewRemoteStore(path, lock, quiet())
	_ = other.Load()
	other.SetValue("od", "token", "t2-from-another-node")
	if err := other.Save(); err != nil {
		t.Fatal(err)
	}

	if v, _ := here.GetValue("od", "token"); v != "t1" {
		t.Fatalf("file rechecked before the interval: token %q", v) // within recheckInterval
	}
	now = now.Add(2 * recheckInterval)
	if v, _ := here.GetValue("od", "token"); v != "t2-from-another-node" {
		t.Fatalf("token refreshed by another node not seen: %q", v)
	}
	if v, _ := here.GetValue("od", "drive_id"); v != "local-change" || here.Pending() != 1 {
		t.Fatalf("pending local change lost: %q, %d pending", v, here.Pending())
	}
}

// TestRemoteStoreRefusesInjection: values with line breaks (e.g. from a
// crafted recovery kit) cannot add sections or settings to remotes.conf.
func TestRemoteStoreRefusesInjection(t *testing.T) {
	dir, lock := testDir(t)
	path := RemotesPath(dir)
	s := NewRemoteStore(path, lock, quiet())
	_ = s.Load()
	s.SetValue("od", "type", "onedrive")
	s.SetValue("od", "token", "{}\n\n[od2]\ntype = local")
	s.SetValue("od", "x\ny", "v")
	s.SetValue("od]\n[od3", "type", "local")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	other := NewRemoteStore(path, lock, quiet())
	if err := other.Load(); err != nil {
		t.Fatal(err)
	}
	if got := other.GetSectionList(); len(got) != 1 || got[0] != "od" || len(other.GetKeyList("od")) != 1 {
		t.Fatalf("sections %v, keys %v; file:\n%s", got, other.GetKeyList("od"), raw)
	}
	for _, c := range []struct{ section, key, value string }{
		{"od", "token", "a\nb"}, {"od", "k=v", "x"}, {"od", "#k", "x"}, {"o[d", "", ""}, {"od", "k", " padded "},
	} {
		if CheckEntry(c.section, c.key, c.value) == nil {
			t.Errorf("CheckEntry(%q, %q, %q) accepted", c.section, c.key, c.value)
		}
	}
	if _, err := encodeINI(sections{"od": {"token": "a\n[x]"}}); err == nil {
		t.Error("encodeINI wrote a value with a line break")
	}
}
