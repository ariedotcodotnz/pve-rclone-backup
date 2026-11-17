// SPDX-License-Identifier: AGPL-3.0-or-later

package doctor

import (
	"strings"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
)

func TestSpace(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	at := time.Date(2026, 11, 17, 20, 0, 0, 0, time.Local)
	sp := &apiv1.StorageSpace{Backups: 64, BackupBytes: 456 << 30}
	if level, msg := space(apiv1.Storage{}); level != "" || msg != "" {
		t.Fatalf("nothing known: %s %q", level, msg)
	}
	level, msg := space(apiv1.Storage{Space: sp, Usage: &apiv1.Usage{Total: n(1 << 40), Used: n(726 << 30), Free: n(298 << 30), UpdatedAt: at}})
	if level != OK || msg != "backups 64 backups, 456.0 GiB; account 726.0 GiB of 1.0 TiB used, 298.0 GiB free (the whole cloud account, as of 2026-11-17 20:00)" {
		t.Fatalf("space = %s %q", level, msg)
	}
	// The case reported: the account is full, the backups take less than half of it.
	level, msg = space(apiv1.Storage{Space: sp, Usage: &apiv1.Usage{Total: n(1 << 40), Used: n(1 << 40), Free: n(0), UpdatedAt: at}})
	if level != Warn || !strings.HasPrefix(msg, "the cloud account is full: backups 64 backups, 456.0 GiB; account 1.0 TiB of 1.0 TiB used, 0 B free") {
		t.Fatalf("full account = %s %q", level, msg)
	}
	if level, msg := space(apiv1.Storage{Space: sp}); level != OK || msg != "backups 64 backups, 456.0 GiB" {
		t.Fatalf("without quota = %s %q", level, msg)
	}
	fits := &apiv1.StorageSpace{Backups: 2, BackupBytes: 2 << 30, Incomplete: 1, IncompleteBytes: 1 << 30,
		Retention: &apiv1.RetentionFit{PerGuest: 4, NeedBytes: 4 << 30, TotalNeedBytes: 4 << 30, AvailableBytes: 10 << 30, Fits: true}}
	if level, msg := space(apiv1.Storage{Space: fits}); level != OK ||
		msg != "backups 2 backups, 2.0 GiB; incomplete uploads 1.0 GiB, of which 0 B in progress; retention stores up to 4.0 GiB of the 10.0 GiB the account has for backups" {
		t.Fatalf("with retention = %s %q", level, msg)
	}
}
