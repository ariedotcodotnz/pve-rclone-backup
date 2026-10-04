// SPDX-License-Identifier: AGPL-3.0-or-later

package version

import (
	"strings"
	"testing"
)

func TestGet(t *testing.T) {
	info := Get()
	if info.Version == "" {
		t.Fatal("empty version")
	}
	if info.APIMajor != APIMajor {
		t.Fatalf("APIMajor = %d, want %d", info.APIMajor, APIMajor)
	}
	if !strings.HasPrefix(info.GoVersion, "go") {
		t.Fatalf("unexpected GoVersion %q", info.GoVersion)
	}
}

func TestStringTruncatesCommit(t *testing.T) {
	s := Info{Version: "1.2.3", Commit: "0123456789abcdef", GoVersion: "go1.26", APIMajor: 1}.String()
	if !strings.Contains(s, "commit 0123456789ab,") {
		t.Fatalf("commit not truncated: %q", s)
	}
	if strings.Contains(s, "cdef") {
		t.Fatalf("commit not truncated: %q", s)
	}
}
