// SPDX-License-Identifier: AGPL-3.0-or-later

package repo

import (
	"testing"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/manifest"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

func TestCheckPartsComparesHashesOfTheSameAlgorithm(t *testing.T) {
	m := &manifest.Manifest{Segments: manifest.Segments{List: []manifest.SegmentInfo{
		{Index: 0, Size: 10, StoredSize: 58, StoredHash: map[string]string{"quickxor": "qx-original"}},
	}}}
	listed := func(hashType, hash string) []transport.Entry {
		return []transport.Entry{{Name: "part.000000", Size: 10, StoredSize: 58, StoredHash: hash, StoredHashType: hashType}}
	}
	cases := []struct {
		name        string
		entries     []transport.Entry
		wantProblem bool
	}{
		{"same hash", listed("quickxor", "qx-original"), false},
		{"changed hash", listed("quickxor", "qx-changed"), true},
		// The repository was copied to a backend with another hash.
		{"other algorithm", listed("md5", "d41d8cd98f00b204e9800998ecf8427e"), false},
		{"no hash", listed("", ""), false},
	}
	for _, c := range cases {
		if got := checkParts(m, c.entries); (len(got) > 0) != c.wantProblem {
			t.Errorf("%s: problems %v", c.name, got)
		}
	}
}
