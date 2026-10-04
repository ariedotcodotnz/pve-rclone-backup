// SPDX-License-Identifier: AGPL-3.0-or-later

package storagecfg

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
)

func TestParsePVEFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../perl/t/data/storage.cfg")
	if err != nil {
		t.Fatal(err)
	}
	cfg, warnings := Parse(raw)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	ids := make([]string, 0, len(cfg.Sections))
	for _, s := range cfg.Sections {
		ids = append(ids, s.ID)
	}
	if !slices.Equal(ids, []string{"local", "pbs1", "offsite"}) {
		t.Fatalf("sections = %v", ids)
	}
	off := cfg.Get("offsite")
	if off.Type != config.StorageType || !off.Bool("shared") {
		t.Fatalf("offsite = %+v", off)
	}
	s, err := config.DecodeStorage(off.ID, off.Props)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.ReplicateFrom, []string{"local"}) || !slices.Equal(s.ExcludeVMIDs, []int{105, 106}) {
		t.Fatalf("decoded = %+v", s)
	}
	if len(cfg.OfType(config.StorageType)) != 1 {
		t.Fatal("OfType")
	}
}

func TestParseEdgeCases(t *testing.T) {
	raw := strings.Join([]string{
		"# leading comment",
		"",
		"dir: backups",
		"\tpath /mnt/backups",
		"# comment inside a section is skipped",
		"\tcontent backup",
		"\tshared",          // bare flag
		"\tpath /mnt/other", // duplicate attribute: first value wins
		"not a property",    // ignored with warning
		"\tnodes pve1 \r",   // CRLF tolerated like Perl's \s
		"",
		"nfs: x", // invalid ID (too short): whole section skipped
		"\tpath /mnt/pve/x",
		"",
		"garbage line",
		"Dir: Upper-Case-ID", // type is lower-cased
		"\tpath /srv/u",
		"0",             // Perl-false line ends the section
		"\tcontent iso", // ignored: not inside a section
	}, "\n")
	cfg, warnings := Parse([]byte(raw))

	b := cfg.Get("backups")
	if b == nil {
		t.Fatal("section backups missing")
	}
	if p, _ := b.Get("path"); p != "/mnt/backups" {
		t.Errorf("duplicate attribute overrode first value: %q", p)
	}
	if !b.Bool("shared") {
		t.Error("bare flag not true")
	}
	if n := b.List("nodes"); !slices.Equal(n, []string{"pve1"}) {
		t.Errorf("nodes = %q", n)
	}
	if c, ok := b.Content(); !ok || !c["backup"] {
		t.Errorf("content = %v", c)
	}
	if cfg.Get("x") != nil {
		t.Error("section with invalid ID kept")
	}
	if u := cfg.Get("Upper-Case-ID"); u == nil || u.Type != "dir" || len(u.Props) != 1 {
		t.Errorf("upper-case section = %+v", u)
	}
	if len(warnings) < 4 {
		t.Errorf("expected warnings for duplicate, bad line, bad ID, garbage, stray property; got %v", warnings)
	}
}

func TestEnsureLocal(t *testing.T) {
	cases := map[string]struct {
		raw      string
		wantPath string
		synth    bool
	}{
		"missing":    {"dir: other\n\tpath /x\n", "/var/lib/vz", true},
		"kept":       {"dir: local\n\tpath /var/lib/vz\n\tcontent iso\n\tnodes pve1\n", "/var/lib/vz", false},
		"no path":    {"dir: local\n\tcontent iso\n", "/var/lib/vz", false},
		"wrong path": {"dir: local\n\tpath /data\n", "/var/lib/vz", true},
		"wrong type": {"zfspool: local\n\tpool rpool\n", "/var/lib/vz", true},
	}
	for name, tc := range cases {
		cfg, _ := Parse([]byte(tc.raw))
		local := cfg.Get("local")
		if local == nil || local.Type != "dir" {
			t.Errorf("%s: local = %+v", name, local)
			continue
		}
		if p, _ := local.Get("path"); p != tc.wantPath {
			t.Errorf("%s: path = %q", name, p)
		}
		if _, ok := local.Get("nodes"); ok {
			t.Errorf("%s: node restriction kept on local", name)
		}
		c, _ := local.Content()
		if tc.synth != c["backup"] && tc.synth {
			t.Errorf("%s: synthesized local lacks backup content", name)
		}
		if n := len(cfg.OfType("dir")); name == "missing" && n != 2 {
			t.Errorf("%s: dir sections = %d", name, n)
		}
	}
}
