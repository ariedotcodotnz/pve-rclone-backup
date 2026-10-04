// SPDX-License-Identifier: AGPL-3.0-or-later

package replicate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/layout"
)

// GuestConfig is the configuration recorded in a backup's manifest so it
// can be shown and restored without downloading the archive.
type GuestConfig struct {
	Config        string
	Firewall      *string
	FirewallKnown bool
}

// Extractor reads the guest configuration from an archive.
type Extractor interface {
	Extract(ctx context.Context, a layout.Archive, archive io.ReaderAt, size int64) (*GuestConfig, error)
}

// PVETools extracts configurations with the tools PVE itself uses: vma
// for VM archives and tar for container archives, behind the matching
// decompressor. Output and time are bounded: archives are guest data.
type PVETools struct {
	Timeout time.Duration // per command (default 2m)
	Limit   int64         // output limit (default 1 MiB)
}

const (
	vzdumpConfCT = "./etc/vzdump/pct.conf"
	vzdumpFwCT   = "./etc/vzdump/pct.fw"
	vmaConfVM    = "qemu-server.conf"
	vmaFwVM      = "qemu-server.fw"
)

// errMissing marks a member that does not exist in the archive.
var errMissing = errors.New("not in archive")

func decompressor(compression string) []string {
	switch compression {
	case "zst":
		return []string{"zstd", "-q", "-d", "-c"}
	case "gz":
		return []string{"gzip", "-d", "-c"}
	case "lzo":
		return []string{"lzop", "-d", "-c"}
	case "bz2":
		return []string{"bzip2", "-d", "-c"}
	}
	return nil
}

// Extract implements Extractor.
func (p PVETools) Extract(ctx context.Context, a layout.Archive, archive io.ReaderAt, size int64) (*GuestConfig, error) {
	var confMember, fwMember string
	var tool []string
	switch a.Format {
	case "tar":
		confMember, fwMember = vzdumpConfCT, vzdumpFwCT
		tool = []string{"tar", "--extract", "--to-stdout", "--occurrence", "--file", "-"}
	case "vma":
		confMember, fwMember = vmaConfVM, vmaFwVM
		tool = []string{"vma", "config", "-", "-c"}
	default:
		return nil, fmt.Errorf("replicate: unsupported archive format %q", a.Format)
	}
	conf, err := p.member(ctx, a, archive, size, tool, confMember)
	if err != nil {
		return nil, fmt.Errorf("replicate: read guest configuration: %w", err)
	}
	gc := &GuestConfig{Config: string(conf), FirewallKnown: true}
	fw, err := p.member(ctx, a, archive, size, tool, fwMember)
	switch {
	case err == nil:
		s := string(fw)
		gc.Firewall = &s
	case errors.Is(err, errMissing):
	default:
		gc.FirewallKnown = false
	}
	return gc, nil
}

// member streams the archive through the decompressor into the tool and
// returns the tool's output for one member.
func (p PVETools) member(ctx context.Context, a layout.Archive, archive io.ReaderAt, size int64, tool []string, member string) ([]byte, error) {
	timeout, limit := p.Timeout, p.Limit
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	if limit == 0 {
		limit = 1 << 20
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := append(append([]string{}, tool...), member)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // fixed tool names and validated member names
	var stderr bytes.Buffer
	out := &limitedBuffer{limit: limit}
	cmd.Stdout, cmd.Stderr = out, &stderr
	cmd.WaitDelay = 5 * time.Second

	in := io.Reader(io.NewSectionReader(archive, 0, size))
	var decomp *exec.Cmd
	if d := decompressor(a.Compression); d != nil {
		decomp = exec.CommandContext(ctx, d[0], d[1:]...) //nolint:gosec // fixed decompressor commands
		decomp.Stdin = in
		decomp.WaitDelay = 5 * time.Second
		pipe, err := decomp.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := decomp.Start(); err != nil {
			return nil, fmt.Errorf("%s: %w", d[0], err)
		}
		in = pipe
	}
	cmd.Stdin = in
	runErr := cmd.Run()
	if decomp != nil {
		// The tool stops reading early (it only needs the header or the
		// first members), so the decompressor may die from a broken pipe.
		_ = decomp.Process.Kill()
		_ = decomp.Wait()
	}
	if out.overflow {
		return nil, fmt.Errorf("%s output exceeds %d bytes", member, limit)
	}
	if runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "Not found in archive") || strings.Contains(msg, "unable to find configuration") ||
			strings.Contains(msg, "not found") && strings.Contains(msg, member) {
			return nil, errMissing
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s: timed out", args[0])
		}
		return nil, fmt.Errorf("%s: %w: %s", args[0], runErr, firstLine(msg))
	}
	return out.Bytes(), nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// limitedBuffer collects command output up to a limit. It must not embed
// bytes.Buffer: the promoted ReadFrom would let io.Copy bypass Write.
type limitedBuffer struct {
	buf      bytes.Buffer
	limit    int64
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if int64(b.buf.Len()+len(p)) > b.limit {
		b.overflow = true
		return 0, errors.New("output limit exceeded")
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }

// guestName returns the guest's name from its configuration (name for
// VMs, hostname for containers), ignoring snapshot sections.
func guestName(vmtype, config string) string {
	key := "name:"
	if vmtype == "lxc" {
		key = "hostname:"
	}
	for line := range strings.Lines(config) {
		if strings.HasPrefix(line, "[") {
			break
		}
		if v, ok := strings.CutPrefix(line, key); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
