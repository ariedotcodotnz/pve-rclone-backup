// SPDX-License-Identifier: AGPL-3.0-or-later

package restore

import (
	"fmt"
	"os/exec"
	"runtime"

	"golang.org/x/sys/unix"
)

// toolUmask is the umask PVE's tools expect (PVE's daemons run with it).
// Under the daemon's own 077, pct restore creates /var/lib/lxc/<vmid> so
// that an unprivileged container's mapped root cannot enter its rootfs.
const toolUmask = 0o022

// startWithUmask starts cmd with the given umask while the daemon keeps
// its own. The umask is shared by all threads, so the child is forked from
// a locked thread that first gets its own filesystem attributes (unshare
// CLONE_FS), which a forked child inherits.
func startWithUmask(cmd *exec.Cmd, mask int) error {
	errc := make(chan error, 1)
	go func() {
		// Never unlocked: with its own umask the thread must not run other
		// goroutines, so it ends with this one.
		runtime.LockOSThread()
		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			errc <- fmt.Errorf("restore: unshare filesystem attributes: %w", err)
			return
		}
		unix.Umask(mask)
		errc <- cmd.Start()
	}()
	return <-errc
}
