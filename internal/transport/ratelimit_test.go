// SPDX-License-Identifier: AGPL-3.0-or-later

package transport

import (
	"bytes"
	"io"
	"testing"
	"time"
)

func TestLimiterTimetable(t *testing.T) {
	l, err := NewLimiter("08:00,1M 23:00,off")
	if err != nil {
		t.Fatal(err)
	}
	at := func(h int) int64 {
		l.now = func() time.Time { return time.Date(2026, 10, 5, h, 30, 0, 0, time.Local) }
		return l.Rate()
	}
	if at(12) != 1<<20 || at(23) != -1 || at(3) != -1 {
		t.Fatalf("rates: 12h=%d 23h=%d 3h=%d", at(12), at(23), at(3))
	}
	if _, err := NewLimiter("fast"); err == nil {
		t.Fatal("invalid limit accepted")
	}
	if none, _ := NewLimiter(""); none.Rate() != -1 {
		t.Fatal("empty limit is not unlimited")
	}
	var nilLimiter *Limiter
	if nilLimiter.Rate() != -1 {
		t.Fatal("nil limiter is not unlimited")
	}
}

func TestLimiterThrottlesReads(t *testing.T) {
	l, _ := NewLimiter("2M")
	data := make([]byte, 3<<20)
	start := time.Now()
	n, err := io.Copy(io.Discard, &limitedReader{ctx: t.Context(), r: bytes.NewReader(data), l: l})
	if err != nil || n != int64(len(data)) {
		t.Fatal(n, err)
	}
	// 3 MiB at 2 MiB/s with a 256 KiB burst needs well over a second.
	if d := time.Since(start); d < time.Second {
		t.Fatalf("read 3 MiB at 2 MiB/s in %v", d)
	}
}
