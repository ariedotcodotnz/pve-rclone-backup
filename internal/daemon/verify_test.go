// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"testing"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
)

func TestWithinBudget(t *testing.T) {
	b := func(size int64) *store.Backup { return &store.Backup{ArchiveSize: size} }
	cands := []*store.Backup{b(40), b(30), b(50), b(10)}
	for _, c := range []struct {
		used, budget int64
		want         int
	}{
		{0, 100, 2},  // 40+30, the next (50) would exceed
		{50, 100, 1}, // 50+40
		{90, 100, 0},
		{0, 10, 1}, // larger than the budget, nothing else today
		{5, 10, 0},
	} {
		if got := withinBudget(cands, c.used, c.budget); len(got) != c.want {
			t.Errorf("used %d budget %d: %d picked, want %d", c.used, c.budget, len(got), c.want)
		}
	}
}
