// SPDX-License-Identifier: AGPL-3.0-or-later

package config

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

type storageCases struct {
	Minimal    map[string]string `json:"minimal"`
	Accept     [][2]string       `json:"accept"`
	Reject     [][2]string       `json:"reject"`
	PVELenient [][2]string       `json:"pve_lenient"`
}

func loadCases(t *testing.T) storageCases {
	t.Helper()
	raw, err := os.ReadFile("../../schema/testdata/storage-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var c storageCases
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func props(kv map[string]string) map[string]Raw {
	out := map[string]Raw{}
	for k, v := range kv {
		out[k] = Raw{Value: v}
	}
	return out
}

func TestStorageDefaults(t *testing.T) {
	s, err := DecodeStorage("offsite", props(loadCases(t).Minimal))
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "offsite" || s.Remote != "od" || s.Source != "homelab" {
		t.Fatalf("unexpected identity %+v", s)
	}
	checks := []struct {
		name string
		ok   bool
	}{
		{"path", s.Path == "pve-backups"},
		{"encryption", s.Encryption == "crypt"},
		{"guests", s.Guests == "all"},
		{"tags", slices.Equal(s.Tags, []string{"offsite"})},
		{"backfill", s.Backfill == "latest"},
		{"supersede", s.Supersede},
		{"transfers", s.Transfers == 2},
		{"segment size", s.SegmentSize == 1<<30},
		{"verify interval", s.VerifyInterval == 24*time.Hour},
		{"verify content off", s.VerifyContent.Off},
		{"verify budget", s.VerifyBudget == 100<<30},
		{"min age", s.MinAge == 7*24*time.Hour},
		{"keep min", s.KeepMin == 1},
		{"delete grace", s.DeleteGrace == 7*24*time.Hour},
		{"max deletes", s.MaxDeletes == 25},
		{"immutable", !s.Immutable},
		{"no prune options", s.Base.PruneBackups == nil},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("default %s wrong: %+v", c.name, s)
		}
	}
}

// The same cases are checked against PVE itself in perl/t/05-config.t.
func TestStorageValidationMatchesPVE(t *testing.T) {
	cases := loadCases(t)
	run := func(kv [2]string) error {
		p := props(cases.Minimal)
		p[kv[0]] = Raw{Value: kv[1]}
		_, err := DecodeStorage("offsite", p)
		return err
	}
	for _, c := range cases.Accept {
		if err := run(c); err != nil {
			t.Errorf("accept %s=%q: %v", c[0], c[1], err)
		}
	}
	for _, c := range slices.Concat(cases.Reject, cases.PVELenient) {
		err := run(c)
		var pe *PropertyError
		if !errors.As(err, &pe) || pe.Key != c[0] {
			t.Errorf("reject %s=%q: got %v", c[0], c[1], err)
		}
	}
}

func TestStorageRequiredAndUnexpected(t *testing.T) {
	_, err := DecodeStorage("offsite", props(map[string]string{"rclone-source": "x1"}))
	if pe, ok := errors.AsType[*PropertyError](err); !ok || pe.Key != "rclone-remote" || !errors.Is(err, errMissing) {
		t.Fatalf("missing remote not reported: %v", err)
	}

	p := props(loadCases(t).Minimal)
	p["rclone-bogus"] = Raw{Value: "1"}
	if _, err := DecodeStorage("offsite", p); !errors.Is(err, errUnexpected) {
		t.Fatalf("unexpected property not reported: %v", err)
	}

	p = props(loadCases(t).Minimal)
	p["rclone-guests"] = Raw{Value: "listed"}
	if _, err := DecodeStorage("offsite", p); err == nil {
		t.Fatal("guests=listed without vmids accepted")
	}
	p["rclone-vmids"] = Raw{Value: "100 200"}
	s, err := DecodeStorage("offsite", p)
	if err != nil || !slices.Equal(s.VMIDs, []int{100, 200}) {
		t.Fatalf("vmids: %v %v", s, err)
	}
}

func TestStorageFlagsAndBase(t *testing.T) {
	p := props(loadCases(t).Minimal)
	p["rclone-immutable"] = Raw{NoValue: true} // bare key: true, like PVE
	p["shared"] = Raw{Value: "1"}
	p["nodes"] = Raw{Value: "pve1,pve2"}
	p["prune-backups"] = Raw{Value: "keep-daily=7,keep-monthly=6"}
	p["bwlimit"] = Raw{Value: "restore=51200,default=10240,move=0.5,clone=1e3"}
	s, err := DecodeStorage("offsite", p)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Immutable || !s.Base.Shared || !slices.Equal(s.Base.Nodes, []string{"pve1", "pve2"}) {
		t.Fatalf("flags/base not decoded: %+v", s)
	}
	if pb := s.Base.PruneBackups; pb == nil || pb.KeepDaily != 7 || pb.KeepMonthly != 6 || pb.IsKeepAll() {
		t.Fatalf("prune options: %+v", s.Base.PruneBackups)
	}
	// PVE's limits are numbers: fractions are kept, not truncated to 0.
	if !maps.Equal(s.Base.BwLimit, map[string]float64{"restore": 51200, "default": 10240, "move": 0.5, "clone": 1000}) {
		t.Fatalf("bwlimit: %v", s.Base.BwLimit)
	}

	p["rclone-remote"] = Raw{NoValue: true}
	if _, err := DecodeStorage("offsite", p); !errors.Is(err, errNoValue) {
		t.Fatalf("string without value accepted: %v", err)
	}
}

func TestParsePruneOptionsNormalizesLikePVE(t *testing.T) {
	for in, keepAll := range map[string]bool{
		"keep-all=1":              true,
		"keep-daily=0":            true, // no positive option means keep-all
		"keep-all=0,keep-last=0":  true,
		"keep-last=3":             false,
		"keep-all=0,keep-daily=7": false,
	} {
		p, err := ParsePruneOptions(in)
		if err != nil {
			t.Errorf("%s: %v", in, err)
			continue
		}
		if p.IsKeepAll() != keepAll {
			t.Errorf("%s: keep-all = %v, want %v", in, p.IsKeepAll(), keepAll)
		}
	}
	for _, bad := range []string{"keep-all=1,keep-last=1", "keep-last=-1", "keep-last=x", "keep-last", "keep-last=1,keep-last=2"} {
		if _, err := ParsePruneOptions(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestDurationsAndSizes(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"0": 0, "30s": 30 * time.Second, "15m": 15 * time.Minute, "6h": 6 * time.Hour,
		"7d": 7 * 24 * time.Hour, "2w": 14 * 24 * time.Hour,
	} {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "7", "1y", "-1d", "7 d", "99999999999999999w"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q) accepted", bad)
		}
	}
	for in, want := range map[string]int64{"0": 0, "512": 512, "64M": 64 << 20, "1G": 1 << 30, "2T": 2 << 40} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "1GiB", "1.5G", "-1", "99999999999T"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("ParseSize(%q) accepted", bad)
		}
	}
}

func TestDecodeInt(t *testing.T) {
	zero, eight := int64(0), int64(8)
	for in, want := range map[string]int{"0": 0, "8": 8, "+3": 3} {
		if got, err := decodeInt(Raw{Value: in}, &zero, &eight); err != nil || got != want {
			t.Errorf("decodeInt(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "9", "-1", "1.5", "0x8", "99999999999999999999"} {
		if _, err := decodeInt(Raw{Value: bad}, &zero, &eight); err == nil {
			t.Errorf("decodeInt(%q) accepted", bad)
		}
	}
	// Values beyond int are refused, not wrapped around.
	if _, err := decodeInt(Raw{Value: "99999999999999999999"}, &zero, nil); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Errorf("huge value: %v", err)
	}
}

func TestSplitListMatchesPVE(t *testing.T) {
	for in, want := range map[string][]string{
		"a,b;c d":   {"a", "b", "c", "d"},
		"  a ,, b ": {"a", "b"},
		"":          nil,
		"a\x00b c":  {"a", "b c"},
	} {
		if got := SplitList(in); !slices.Equal(got, want) {
			t.Errorf("SplitList(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNodeAndDaemonConfig(t *testing.T) {
	n, err := ParseNodeConfig([]byte("# comment\nstaging-dir: /srv/staging\nupload-workers: 4\ninotify: 0\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if n.StagingDir != "/srv/staging" || n.UploadWorkers != 4 || n.Inotify || n.ScanInterval != 15*time.Minute {
		t.Fatalf("node config: %+v", n)
	}
	if d := DefaultNodeConfig(); d.StagingReserve != 10<<30 || d.RestoreIOnice != "best-effort:4" {
		t.Fatalf("node defaults: %+v", d)
	}
	for _, bad := range []string{"upload-workers: 0\n", "bogus: 1\n", "staging-dir relative\n", "staging-dir: relative\n", "inotify: 1\ninotify: 0\n"} {
		if _, err := ParseNodeConfig([]byte(bad)); err == nil {
			t.Errorf("node config %q accepted", bad)
		}
	}

	d, err := ParseDaemonConfig([]byte("log-level: debug\nretention-time: 23:30\n"))
	if err != nil {
		t.Fatal(err)
	}
	if d.LogLevel != "debug" || d.RetentionTime != "23:30" || d.CatalogResyncInterval != 6*time.Hour {
		t.Fatalf("daemon config: %+v", d)
	}
	if _, err := ParseDaemonConfig([]byte("retention-time: 24:00\n")); err == nil {
		t.Fatal("invalid retention time accepted")
	}
}

func TestEmbeddedSchemas(t *testing.T) {
	for _, name := range []string{"storage", "node", "daemon"} {
		raw, err := Schemas.ReadFile(name + ".schema.json")
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Properties map[string]map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Properties) == 0 {
			t.Fatalf("%s schema: %v", name, err)
		}
	}
	raw, _ := Schemas.ReadFile("storage.schema.json")
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	if req, _ := doc["required"].([]any); len(req) != 2 {
		t.Fatalf("storage schema required = %v", doc["required"])
	}
	if len(storageKeys) != len(doc["properties"].(map[string]any)) {
		t.Fatal("storage keys and schema disagree")
	}
}
