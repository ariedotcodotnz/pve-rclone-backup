// SPDX-License-Identifier: AGPL-3.0-or-later

package restore

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/jobs"
)

// Tools names the PVE commands restores run.
type Tools struct {
	QMRestore string // default qmrestore
	PCT       string // default pct
	QM        string // default qm
	// IOnice, if set, prefixes restore commands (e.g. ionice -c2 -n4).
	IOnice []string
}

func (t Tools) withDefaults() Tools {
	if t.QMRestore == "" {
		t.QMRestore = "qmrestore"
	}
	if t.PCT == "" {
		t.PCT = "pct"
	}
	if t.QM == "" {
		t.QM = "qm"
	}
	return t
}

// decompressor returns the command that decompresses an archive.
func decompressor(compression string) ([]string, error) {
	switch compression {
	case "":
		return nil, nil
	case "zst":
		return []string{"zstd", "-q", "-d", "-c"}, nil
	case "gz":
		return []string{"gzip", "-d", "-c"}, nil
	case "lzo":
		return []string{"lzop", "-d", "-c"}, nil
	case "bz2":
		return []string{"bzip2", "-d", "-c"}, nil
	}
	return nil, fmt.Errorf("restore: unsupported compression %q", compression)
}

// logLines records a command's output in the job history.
type logLines struct {
	t     *jobs.Task
	name  string
	mu    sync.Mutex
	tail  []string
	count int
}

const maxLoggedLines = 500

func (l *logLines) consume(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		l.mu.Lock()
		l.count++
		l.tail = append(l.tail, line)
		if len(l.tail) > 20 {
			l.tail = l.tail[1:]
		}
		n := l.count
		l.mu.Unlock()
		if n <= maxLoggedLines {
			_ = l.t.Event(context.Background(), "info", l.name+": "+line)
		}
	}
}

func (l *logLines) last() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.tail) == 0 {
		return ""
	}
	return l.tail[len(l.tail)-1]
}

// command runs argv with stdin and records its output.
func (r *Runner) command(ctx context.Context, t *jobs.Task, stdin io.Reader, argv ...string) error {
	argv = append(append([]string{}, r.opts.Tools.IOnice...), argv...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // fixed PVE tools with validated arguments
	cmd.Stdin = stdin
	cmd.WaitDelay = 30 * time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	logs := &logLines{t: t, name: argv[len(r.opts.Tools.IOnice)]}
	_ = t.Event(ctx, "info", "running "+strings.Join(argv, " "))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("restore: start %s: %w", argv[0], err)
	}
	done := make(chan struct{})
	go func() { logs.consume(out); close(done) }()
	<-done
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if last := logs.last(); last != "" {
			return fmt.Errorf("%s failed: %w: %s", logs.name, err, last)
		}
		return fmt.Errorf("%s failed: %w", logs.name, err)
	}
	return nil
}

// pipeline feeds src through the decompressor into argv.
func (r *Runner) pipeline(ctx context.Context, t *jobs.Task, src io.Reader, compression string, argv ...string) error {
	d, err := decompressor(compression)
	if err != nil {
		return err
	}
	if d == nil {
		return r.command(ctx, t, src, argv...)
	}
	dec := exec.CommandContext(ctx, d[0], d[1:]...) //nolint:gosec // fixed decompressor commands
	dec.Stdin = src
	dec.WaitDelay = 30 * time.Second
	var decErr strings.Builder
	dec.Stderr = &limitWriter{w: &decErr, n: 4096}
	pipe, err := dec.StdoutPipe()
	if err != nil {
		return err
	}
	if err := dec.Start(); err != nil {
		return fmt.Errorf("restore: start %s: %w", d[0], err)
	}
	runErr := r.command(ctx, t, pipe, argv...)
	if runErr != nil {
		_ = dec.Process.Kill()
	}
	decWait := dec.Wait()
	if runErr != nil {
		return runErr
	}
	if decWait != nil && ctx.Err() == nil {
		return fmt.Errorf("%s failed: %w: %s", d[0], decWait, strings.TrimSpace(decErr.String()))
	}
	return decWait
}

type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p
	if len(q) > l.n {
		q = q[:l.n]
	}
	l.n -= len(q)
	if _, err := l.w.Write(q); err != nil {
		return 0, err
	}
	return len(p), nil
}

// errIsCancel reports a cancelled or stopped job.
func errIsCancel(err error) bool { return errors.Is(err, context.Canceled) }
