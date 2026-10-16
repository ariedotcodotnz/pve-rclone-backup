// SPDX-License-Identifier: AGPL-3.0-or-later

package transport

import (
	"errors"
	"fmt"
	"testing"

	onedriveapi "github.com/rclone/rclone/backend/onedrive/api"
	"github.com/rclone/rclone/fs"
)

func onedriveError(code string) error {
	e := &onedriveapi.Error{}
	e.ErrorInfo.Code = code
	return e
}

// TestDeletionOptions: the options deletionSpecs sets exist in the
// backends, where an unknown name would silently be ignored.
func TestDeletionOptions(t *testing.T) {
	for name, prof := range profiles {
		if prof.PermanentDeleteOption == "" {
			continue
		}
		ri, err := fs.Find(prof.Backend)
		if err != nil {
			t.Fatal(err)
		}
		for _, opt := range append([]string{prof.PermanentDeleteOption}, prof.LocationOptions...) {
			if ri.Options.Get(opt) == nil {
				t.Errorf("profile %s: backend %s has no option %q", name, prof.Backend, opt)
			}
		}
	}
}

func TestConnectionValue(t *testing.T) {
	for in, want := range map[string]string{
		"":        "",
		"b!x-y_z": "b!x-y_z",
		"a,b":     `"a,b"`,
		"a:b":     `"a:b"`,
		`a"b`:     `"a""b"`,
		"'a'":     `"'a'"`,
	} {
		if got := connectionValue(in); got != want {
			t.Errorf("connectionValue(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestPermanentDeleteRefused(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{onedriveError("accessDenied"), true},
		{onedriveError("notSupported"), true},
		{onedriveError("invalidRequest"), true},
		{onedriveError("405 Method Not Allowed"), true},
		{fmt.Errorf("transport: remove x: %w", onedriveError("403 Forbidden")), true},
		// Answers a normal deletion would get as well.
		{onedriveError("itemNotFound"), false},
		{onedriveError("activityLimitReached"), false},
		{onedriveError("quotaLimitReached"), false},
		{onedriveError("500 Internal Server Error"), false},
		{fs.ErrorObjectNotFound, false},
		{errors.New("accessDenied"), false},
		{nil, false},
	} {
		if got := permanentDeleteRefused(c.err); got != c.want {
			t.Errorf("permanentDeleteRefused(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
