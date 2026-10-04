// SPDX-License-Identifier: AGPL-3.0-or-later

package transport

// Backends compiled into the daemon. Additional backends must not be added
// without a capability profile and tests (see ADR 0002).
import (
	_ "github.com/rclone/rclone/backend/crypt"
	_ "github.com/rclone/rclone/backend/local"
	_ "github.com/rclone/rclone/backend/onedrive"
)
