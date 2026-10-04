// SPDX-License-Identifier: AGPL-3.0-or-later

package storages

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/rclone/rclone/fs/fserrors"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/repo"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/secrets"
)

func TestPVEProps(t *testing.T) {
	// The shape PVE passes to storage hooks after check_config.
	props, err := pveProps(map[string]any{
		"type":                  "rclone-backup",
		"rclone-remote":         "od",
		"rclone-source":         "homelab",
		"rclone-transfers":      float64(3),
		"rclone-immutable":      true,
		"content":               map[string]any{"backup": float64(1)},
		"nodes":                 map[string]any{"pve2": float64(1), "pve1": float64(1), "gone": float64(0)},
		"prune-backups":         map[string]any{"keep-last": float64(3), "keep-daily": "7"},
		"rclone-replicate-from": []any{"local", "nfs"},
		"digest":                nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"rclone-remote": "od", "rclone-source": "homelab", "rclone-transfers": "3", "rclone-immutable": "1", "content": "backup",
		"nodes": "pve1,pve2", "prune-backups": "keep-daily=7,keep-last=3", "rclone-replicate-from": "local,nfs",
	}
	if len(props) != len(want) {
		t.Fatalf("props = %v", props)
	}
	for k, v := range want {
		if props[k].Value != v {
			t.Errorf("%s = %q, want %q", k, props[k].Value, v)
		}
	}
	sc, err := config.DecodeStorage("offsite", props)
	if err != nil || sc.Transfers != 3 || !sc.Immutable || sc.Base.PruneBackups.KeepLast != 3 || len(sc.Base.Nodes) != 2 {
		t.Fatalf("decoded = %+v, %v", sc, err)
	}
	if _, err := pveProps(map[string]any{"x": struct{}{}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unsupported value: %v", err)
	}
}

func TestDeletedKeys(t *testing.T) {
	for _, c := range []struct {
		in   any
		want int
	}{{"rclone-tags,rclone-vmids", 2}, {"a b;c", 3}, {[]any{"a", "b"}, 2}, {nil, 0}, {float64(1), 0}} {
		if got := deletedKeys(c.in); len(got) != c.want {
			t.Errorf("deletedKeys(%v) = %v", c.in, got)
		}
	}
}

func TestBackoff(t *testing.T) {
	if backoff(1) != 30*time.Second || backoff(2) != time.Minute || backoff(20) != 15*time.Minute {
		t.Fatalf("backoff = %v %v %v", backoff(1), backoff(2), backoff(20))
	}
}

func TestHealthOf(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{repo.ErrNotInitialized, apiv1.HealthUninitialized},
		{fmt.Errorf("x: %w", secrets.ErrNotFound), apiv1.HealthMisconfigured},
		{repo.ErrWrongKeys, apiv1.HealthMisconfigured},
		{repo.ErrEncryptionMismatch, apiv1.HealthMisconfigured},
		{fmt.Errorf("%w: remote", errRemote), apiv1.HealthMisconfigured},
		{errors.New("couldn't fetch token: invalid_grant"), apiv1.HealthAuthRequired},
		{errors.New("quotaLimitReached"), apiv1.HealthQuotaExceeded},
		{errors.New("HTTP error 429 (429 Too Many Requests)"), apiv1.HealthDegraded},
		{fserrors.NoRetryError(errors.New("bad")), apiv1.HealthMisconfigured},
		{errors.New("connection refused"), apiv1.HealthUnreachable},
	} {
		if got, _ := healthOf(c.err); got != c.want {
			t.Errorf("healthOf(%v) = %s, want %s", c.err, got, c.want)
		}
	}
}
