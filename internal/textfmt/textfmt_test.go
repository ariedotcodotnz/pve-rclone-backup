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

func TestAccountAndBackups(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	for _, c := range []struct {
		total, used, free, trashed *int64
		want                       string
	}{
		{n(1 << 40), n(1 << 40), n(0), n(0), "1.0 TiB of 1.0 TiB used, 0 B free"},
		{n(1 << 40), n(750 << 30), n(274 << 30), n(2 << 30), "750.0 GiB of 1.0 TiB used, 274.0 GiB free, 2.0 GiB in the recycle bin"},
		{nil, n(5 << 30), nil, nil, "5.0 GiB used"},
		{nil, nil, nil, nil, "-"},
	} {
		if got := Account(c.total, c.used, c.free, c.trashed); got != c.want {
			t.Errorf("Account = %q, want %q", got, c.want)
		}
	}
	if got := Backups(64, 456<<30, 2, 30<<30); got != "64 backups, 456.0 GiB (2 marked for deletion, 30.0 GiB)" {
		t.Errorf("Backups = %q", got)
	}
	if got := Backups(1, 1<<20, 0, 0); got != "1 backup, 1.0 MiB" {
		t.Errorf("Backups = %q", got)
	}
}
