// SPDX-License-Identifier: AGPL-3.0-or-later

package discovery

import (
	"fmt"
	"slices"
	"strings"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
)

// guestSelected applies an offsite storage's guest selection to an
// archive. tags are the guest's current tags; known is false when the
// guest no longer exists.
func guestSelected(t *config.Storage, vmid int, tags []string, known bool) (bool, string) {
	if slices.Contains(t.ExcludeVMIDs, vmid) {
		return false, "guest is excluded by rclone-exclude-vmids"
	}
	switch t.Guests {
	case "listed":
		if !slices.Contains(t.VMIDs, vmid) {
			return false, "guest is not listed in rclone-vmids"
		}
	case "tagged":
		if !known {
			return false, "guest no longer exists, so its tags are unknown"
		}
		for _, tag := range tags {
			if slices.ContainsFunc(t.Tags, func(want string) bool { return strings.EqualFold(want, tag) }) {
				return true, ""
			}
		}
		return false, fmt.Sprintf("guest has none of the tags %s", strings.Join(t.Tags, ", "))
	}
	return true, ""
}
