// SPDX-License-Identifier: AGPL-3.0-or-later

package restore

import (
	"os/exec"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStartWithUmask(t *testing.T) {
	old := unix.Umask(0o077)
	defer unix.Umask(old)
	cmd := exec.Command("sh", "-c", "umask")
	var out strings.Builder
	cmd.Stdout = &out
	if err := startWithUmask(cmd, 0o022); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != "0022" {
		t.Errorf("child umask %s, want 0022", got)
	}
	if cur := unix.Umask(0o077); cur != 0o077 {
		t.Errorf("daemon umask changed to %#o", cur)
	}
}
