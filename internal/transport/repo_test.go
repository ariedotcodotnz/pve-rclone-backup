// SPDX-License-Identifier: AGPL-3.0-or-later

package transport

import "testing"

func TestRepoLocationValidate(t *testing.T) {
	for _, path := range []string{"pve-backups", "proxmox", "Drive 1", "Drive 1/PVE  backups.v2", "a/b/c"} {
		if err := (RepoLocation{Remote: "od", Path: path}).Validate(); err != nil {
			t.Errorf("%q rejected: %v", path, err)
		}
	}
	for _, path := range []string{"", " Drive 1", "Drive 1 ", "Drive 1 /x", "/abs", "a//b", "a/../b", "..", "Drive\t1", "a/"} {
		if err := (RepoLocation{Remote: "od", Path: path}).Validate(); err == nil {
			t.Errorf("%q accepted", path)
		}
	}
}
