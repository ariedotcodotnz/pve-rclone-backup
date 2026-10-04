// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"net/http"
	"path/filepath"
	"strings"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
)

func (d *Daemon) discoveryRoutes() {
	d.api.Handle("POST /v1/discovery/scan", func(w http.ResponseWriter, r *http.Request) error {
		var req struct {
			Storage string `json:"storage"`
		}
		if r.ContentLength != 0 {
			if err := api.DecodeJSON(r, &req); err != nil {
				return err
			}
		}
		sum, err := d.discovery.Scan(r.Context(), req.Storage)
		if err != nil {
			return err
		}
		return api.WriteJSON(w, http.StatusOK, sum)
	})
	// The optional vzdump hook reports finished archives. It must answer
	// immediately: the hook runs inside the backup job.
	d.api.Handle("POST /v1/discovery/notify", func(w http.ResponseWriter, r *http.Request) error {
		var req apiv1.DiscoveryNotify
		if err := api.DecodeJSON(r, &req); err != nil {
			return err
		}
		if !filepath.IsAbs(req.Path) || strings.ContainsRune(req.Path, 0) {
			return api.Invalid("path must be absolute")
		}
		d.discovery.Notify(req.Path)
		return api.WriteJSON(w, http.StatusAccepted, struct{}{})
	})
}
