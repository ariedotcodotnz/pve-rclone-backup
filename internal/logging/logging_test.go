// SPDX-License-Identifier: AGPL-3.0-or-later

package logging

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// Secrets planted in every shape they can reach a log line must never
// appear in the output.
func TestSecretsNeverReachTheLog(t *testing.T) {
	const (
		access  = "EwBwA8l6BAAUAOyDv0l6PcCVu89kmzvqZmkWABkAAQ"
		refresh = "M.C512_BAY.0.U.-Cg9xFakeRefreshToken!xyz"
		secret  = "c2VjcmV0LWNsaWVudC1zZWNyZXQ"
		pass    = "hunter2-correct-horse"
	)
	var buf bytes.Buffer
	lv := new(slog.LevelVar)
	lv.Set(slog.LevelDebug)
	log, err := New(&buf, "json", lv)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("token refreshed", "token", `{"access_token":"`+access+`","refresh_token":"`+refresh+`"}`)
	log.Info(`got {"access_token":"` + access + `"} from provider`)
	log.Debug("request", "authorization", "Bearer "+access)
	log.Debug("request header Authorization: Bearer " + access)
	log.Warn("oauth", "url", "http://localhost:53682/?code="+refresh+"&state=abc")
	log.Error("exchange failed", "err", errors.New("client_secret="+secret+" rejected"))
	log.Info("crypt", slog.Group("remote", "password", pass, "password2", pass, "name", "od-crypt"))
	log.With("client_secret", secret).Info("configured")
	log.Info("config", "remote_password", pass)
	// rclone's fs.NewFs logs connection strings with %q at debug level.
	log.Debug(fmt.Sprintf("Creating backend with remote %q",
		":crypt,remote='od:pve/g1',password='"+pass+"''x',password2=\""+secret+"\",suffix='.bin':"))

	out := buf.String()
	for _, s := range []string{access, refresh, secret, pass} {
		if strings.Contains(out, s) {
			t.Errorf("secret %q leaked:\n%s", s, out)
		}
	}
	for _, keep := range []string{"token refreshed", "od-crypt", "state=abc", "localhost:53682", "remote='od:pve/g1'", "suffix='.bin'"} {
		if !strings.Contains(out, keep) {
			t.Errorf("non-secret %q was removed:\n%s", keep, out)
		}
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"debug": slog.LevelDebug, "": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError} {
		if got, err := ParseLevel(in); err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v %v", in, got, err)
		}
	}
	if _, err := ParseLevel("loud"); err == nil {
		t.Error("unknown level accepted")
	}
	if _, err := New(&bytes.Buffer{}, "xml", nil); err == nil {
		t.Error("unknown format accepted")
	}
}
