// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"net"
	"os"
	"strconv"
	"time"
)

// sdNotify sends a state update to systemd (sd_notify(3)). It is a no-op
// when the daemon was not started by systemd with Type=notify.
func sdNotify(state string) error {
	path := os.Getenv("NOTIFY_SOCKET")
	if path == "" {
		return nil
	}
	if path[0] == '@' {
		path = "\x00" + path[1:] // abstract socket
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_, err = conn.Write([]byte(state))
	return err
}

// watchdogInterval returns how often to ping systemd's watchdog, or 0 if
// the watchdog is not enabled for this process.
func watchdogInterval() time.Duration {
	usec, err := strconv.ParseInt(os.Getenv("WATCHDOG_USEC"), 10, 64)
	if err != nil || usec <= 0 {
		return 0
	}
	if pid := os.Getenv("WATCHDOG_PID"); pid != "" && pid != strconv.Itoa(os.Getpid()) {
		return 0
	}
	return time.Duration(usec) * time.Microsecond / 2
}
