// SPDX-License-Identifier: AGPL-3.0-or-later

package textfmt

import "testing"

func TestFormatting(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 1536: "1.5 KiB", 5 << 30: "5.0 GiB"} {
		if got := Bytes(n); got != want {
			t.Errorf("Bytes(%d) = %q", n, got)
		}
	}
	if got := Clean("a\tb\x1b[31mc\nd\u009b"); got != "a b[31mc d" {
		t.Errorf("Clean = %q", got)
	}
	if got := CleanBlock("a\nb\x07"); got != "a\nb" {
		t.Errorf("CleanBlock = %q", got)
	}
	if Truncate("abcdef", 4) != "abc…" || Truncate("abc", 4) != "abc" || Truncate("abc", 0) != "" {
		t.Error("Truncate")
	}
	if Time(nil) != "-" || Unix(0) != nil || Dash("") != "-" || YesNo(true) != "yes" {
		t.Error("helpers")
	}
}
