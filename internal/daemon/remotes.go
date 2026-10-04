// SPDX-License-Identifier: AGPL-3.0-or-later

package daemon

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/remotes"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

// remoteError maps remote management and transport errors to API errors.
func remoteError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, remotes.ErrNotFound), errors.Is(err, remotes.ErrNoSession):
		return api.Errorf(http.StatusNotFound, apiv1.CodeNotFound, "%v", err)
	case errors.Is(err, remotes.ErrInUse), errors.Is(err, remotes.ErrExists):
		return api.Errorf(http.StatusConflict, apiv1.CodeConflict, "%v", err)
	case errors.Is(err, remotes.ErrBusy):
		return api.Errorf(http.StatusConflict, apiv1.CodeBusy, "%v", err)
	case errors.Is(err, remotes.ErrInvalid):
		return api.Errorf(http.StatusBadRequest, apiv1.CodeInvalidArgument, "%v", err)
	}
	return transportError(err)
}

// transportError maps an error from a remote to an API error.
func transportError(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	e := api.Errorf(http.StatusBadGateway, apiv1.CodeRemoteUnreachable, "%v", err)
	switch transport.Classify(err) {
	case transport.ClassAuth:
		e.Body.Code = apiv1.CodeRemoteAuthRequired
	case transport.ClassQuota:
		e.Body.Code = apiv1.CodeQuotaExceeded
	case transport.ClassTransient, transport.ClassThrottled:
		e.Body.Retryable = true
	}
	return e
}

func (d *Daemon) remoteRoutes() {
	d.api.Handle("GET /v1/providers", func(w http.ResponseWriter, r *http.Request) error {
		return api.WriteJSON(w, http.StatusOK, remotes.Providers())
	})
	d.api.Handle("GET /v1/remotes", func(w http.ResponseWriter, r *http.Request) error {
		return api.WriteJSON(w, http.StatusOK, d.remotes.List())
	})
	d.api.Handle("GET /v1/remotes/{remote}", func(w http.ResponseWriter, r *http.Request) error {
		rem, err := d.remotes.Get(r.PathValue("remote"))
		if err != nil {
			return remoteError(err)
		}
		return api.WriteJSON(w, http.StatusOK, rem)
	})
	d.api.Handle("DELETE /v1/remotes/{remote}", func(w http.ResponseWriter, r *http.Request) error {
		if err := d.remotes.Delete(r.PathValue("remote")); err != nil {
			return remoteError(err)
		}
		return api.WriteJSON(w, http.StatusOK, struct{}{})
	})
	d.api.Handle("POST /v1/remotes/{remote}/test", func(w http.ResponseWriter, r *http.Request) error {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		res, err := d.remotes.Test(ctx, r.PathValue("remote"))
		if err != nil {
			return remoteError(err)
		}
		return api.WriteJSON(w, http.StatusOK, res)
	})
	d.api.Handle("GET /v1/remotes/{remote}/about", func(w http.ResponseWriter, r *http.Request) error {
		ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
		defer cancel()
		u, err := d.remotes.About(ctx, r.PathValue("remote"))
		if err != nil {
			return remoteError(err)
		}
		return api.WriteJSON(w, http.StatusOK, u)
	})
	d.api.Handle("POST /v1/remotes/{remote}/reconnect", func(w http.ResponseWriter, r *http.Request) error {
		s, err := d.remotes.Start(r.Context(), apiv1.RemoteSetupRequest{Name: r.PathValue("remote")}, true)
		if err != nil {
			return remoteError(err)
		}
		return api.WriteJSON(w, http.StatusOK, s)
	})
	d.api.Handle("POST /v1/remote-setup", func(w http.ResponseWriter, r *http.Request) error {
		var req apiv1.RemoteSetupRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return err
		}
		s, err := d.remotes.Start(r.Context(), req, false)
		if err != nil {
			return remoteError(err)
		}
		return api.WriteJSON(w, http.StatusOK, s)
	})
	d.api.Handle("GET /v1/remote-setup/{id}", func(w http.ResponseWriter, r *http.Request) error {
		s, err := d.remotes.Status(r.PathValue("id"))
		if err != nil {
			return remoteError(err)
		}
		return api.WriteJSON(w, http.StatusOK, s)
	})
	d.api.Handle("POST /v1/remote-setup/{id}/answer", func(w http.ResponseWriter, r *http.Request) error {
		var a apiv1.RemoteSetupAnswer
		if err := api.DecodeJSON(r, &a); err != nil {
			return err
		}
		s, err := d.remotes.Answer(r.Context(), r.PathValue("id"), a)
		if err != nil {
			return remoteError(err)
		}
		return api.WriteJSON(w, http.StatusOK, s)
	})
	d.api.Handle("POST /v1/remote-setup/{id}/oauth-redirect", func(w http.ResponseWriter, r *http.Request) error {
		var req apiv1.OAuthRedirect
		if err := api.DecodeJSON(r, &req); err != nil {
			return err
		}
		s, err := d.remotes.Redirect(r.Context(), r.PathValue("id"), req.URL)
		if err != nil {
			return remoteError(err)
		}
		return api.WriteJSON(w, http.StatusOK, s)
	})
	d.api.Handle("DELETE /v1/remote-setup/{id}", func(w http.ResponseWriter, r *http.Request) error {
		if err := d.remotes.Finish(r.PathValue("id")); err != nil {
			return remoteError(err)
		}
		return api.WriteJSON(w, http.StatusOK, struct{}{})
	})
}
