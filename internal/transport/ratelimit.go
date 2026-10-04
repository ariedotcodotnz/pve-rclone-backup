// SPDX-License-Identifier: AGPL-3.0-or-later

package transport

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"golang.org/x/time/rate"
)

// Limiter limits upload bandwidth of one storage according to an rclone
// bandwidth timetable such as "1M" or "08:00,512k 23:00,off". The limit is
// applied when reading source data, so it covers retries and does not
// depend on rclone's process-wide limiter.
type Limiter struct {
	table fs.BwTimetable
	now   func() time.Time

	mu      sync.Mutex
	limiter *rate.Limiter
	current fs.SizeSuffix
}

const limiterBurst = 256 << 10

// NewLimiter parses spec; an empty spec means unlimited.
func NewLimiter(spec string) (*Limiter, error) {
	l := &Limiter{now: time.Now}
	if spec != "" {
		if err := l.table.Set(spec); err != nil {
			return nil, fmt.Errorf("transport: invalid bandwidth limit %q: %w", spec, err)
		}
	}
	return l, nil
}

// Rate returns the current limit in bytes per second, or -1 for unlimited.
func (l *Limiter) Rate() int64 {
	if l == nil {
		return -1
	}
	return int64(l.table.LimitAt(l.now()).Bandwidth.Tx)
}

func (l *Limiter) currentLimiter() *rate.Limiter {
	tx := l.table.LimitAt(l.now()).Bandwidth.Tx
	l.mu.Lock()
	defer l.mu.Unlock()
	if tx <= 0 {
		l.limiter, l.current = nil, tx
		return nil
	}
	if l.limiter == nil || tx != l.current {
		l.limiter = rate.NewLimiter(rate.Limit(tx), limiterBurst)
		l.current = tx
	}
	return l.limiter
}

// wait blocks until n bytes may be read.
func (l *Limiter) wait(ctx context.Context, n int) error {
	if l == nil {
		return nil
	}
	for n > 0 {
		lim := l.currentLimiter()
		if lim == nil {
			return nil
		}
		chunk := min(n, limiterBurst)
		if err := lim.WaitN(ctx, chunk); err != nil {
			return err
		}
		n -= chunk
	}
	return nil
}

type limitedReader struct {
	ctx context.Context
	r   io.Reader
	l   *Limiter
}

func (lr *limitedReader) Read(p []byte) (int, error) {
	if len(p) > limiterBurst {
		p = p[:limiterBurst]
	}
	n, err := lr.r.Read(p)
	if n > 0 {
		if werr := lr.l.wait(lr.ctx, n); werr != nil {
			return n, werr
		}
	}
	return n, err
}
