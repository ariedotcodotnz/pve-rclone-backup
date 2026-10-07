// SPDX-License-Identifier: AGPL-3.0-or-later

package restore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/jobs"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/store"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

// Verification levels run by jobs (levels 1 and 2 are listing checks done
// by catalogue resyncs).
const (
	LevelContent = 3 // download, check every digest and the archive structure
	LevelRestore = 4 // restore into a scratch guest and remove it again
)

// VerifyParams are the parameters of a verify job.
type VerifyParams struct {
	Level          int    `json:"level"`
	ScratchVMID    int    `json:"scratch_vmid,omitzero"`
	ScratchStorage string `json:"scratch_storage,omitempty"`
}

// verifyDetails is stored with each verification run.
type verifyDetails struct {
	Bytes      int64  `json:"bytes"`
	Structural string `json:"structural,omitempty"` // tar, vma or skipped
	VMID       int    `json:"vmid,omitzero"`
	Error      string `json:"error,omitempty"`
}

// structureTool returns the command that checks an archive's structure
// from its decompressed stream, or nil when the tools are not installed.
func structureTool(format, compression string) []string {
	var tool []string
	switch format {
	case "tar":
		tool = []string{"tar", "-tf", "-"}
	case "vma":
		tool = []string{"vma", "verify", "-"}
	default:
		return nil
	}
	if _, err := exec.LookPath(tool[0]); err != nil {
		return nil
	}
	if d, err := decompressor(compression); err != nil || d != nil && !installed(d[0]) {
		return nil
	}
	return tool
}

func installed(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

// verify runs a content or restore verification and records it.
func (r *Runner) verify(ctx context.Context, t *jobs.Task, s *source, p VerifyParams) error {
	vid, err := r.opts.Store.AddVerification(ctx, &store.Verification{StoreID: s.b.StoreID, Volname: s.b.Volname,
		Level: p.Level, StartedAt: r.opts.Now().Unix()})
	if err != nil {
		return err
	}
	det := verifyDetails{}
	var runErr error
	switch p.Level {
	case LevelContent:
		runErr = r.verifyContent(ctx, t, s, &det)
	case LevelRestore:
		runErr = r.verifyRestore(ctx, t, s, p, &det)
	default:
		runErr = jobs.Permanent(fmt.Errorf("unsupported verification level %d", p.Level))
	}
	if errors.Is(runErr, context.Canceled) {
		_ = r.opts.Store.FinishVerification(context.WithoutCancel(ctx), vid, "error", `{"error":"cancelled"}`)
		return runErr
	}
	result, damaged := "ok", false
	if runErr != nil {
		det.Error = runErr.Error()
		result = "failed"
		// A digest mismatch, and also stored ciphertext that no longer
		// decrypts or authenticates, is damage of the offsite copy.
		damaged = transport.Classify(runErr) == transport.ClassIntegrity
		if damaged {
			result = "damaged"
		}
	}
	dj, _ := json.Marshal(det)
	bg := context.WithoutCancel(ctx)
	if err := r.opts.Store.FinishVerification(bg, vid, result, string(dj)); err != nil {
		return err
	}
	switch {
	case damaged:
		_ = r.opts.Store.SetVerification(bg, s.b.StoreID, s.b.Volname, p.Level, "damaged", true)
		if r.opts.OnDamaged != nil {
			r.opts.OnDamaged(s.b.StoreID, s.b.Volname, runErr.Error())
		}
	case runErr == nil:
		if err := r.opts.Store.SetVerification(bg, s.b.StoreID, s.b.Volname, p.Level, "ok", false); err != nil {
			return err
		}
		r.log.Info("offsite backup verified", "storage", s.b.StoreID, "volname", s.b.Volname, "level", p.Level, "structure", det.Structural)
		if r.opts.OnVerified != nil {
			r.opts.OnVerified(s.b.StoreID, s.b.Volname)
		}
	}
	if runErr != nil {
		return jobs.Permanent(runErr)
	}
	return nil
}

// verifyContent downloads the archive, checking every segment digest and
// the archive digest (and, for encrypted repositories, every 64 KiB
// block's authenticator), and checks the archive's structure when the
// tools are available.
func (r *Runner) verifyContent(ctx context.Context, t *jobs.Task, s *source, det *verifyDetails) error {
	if err := t.Advance(ctx, jobs.StateTransferring, "reading the archive", nil); err != nil {
		return err
	}
	rd := NewReader(ctx, s.tgt, s.id, s.m)
	defer func() { _ = rd.Close() }()
	pr, pw := io.Pipe()
	copyErr := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.MultiWriter(pw, &progressWriter{ctx: ctx, t: t}), rd)
		_ = pw.CloseWithError(err)
		copyErr <- err
	}()
	var structErr error
	if tool := structureTool(s.m.Archive.Format, s.m.Archive.Compression); tool != nil {
		det.Structural = tool[0]
		structErr = r.quietPipeline(ctx, pr, s.m.Archive.Compression, tool...)
		// Drain what the tool did not read so every digest is checked.
		_, _ = io.Copy(io.Discard, pr)
	} else {
		det.Structural = "skipped"
		_, structErr = io.Copy(io.Discard, pr)
	}
	cerr := <-copyErr
	det.Bytes = rd.BytesRead()
	if cerr != nil {
		return cerr
	}
	if structErr != nil {
		return fmt.Errorf("the archive's structure is invalid: %w", structErr)
	}
	return t.Advance(ctx, jobs.StateVerifying, fmt.Sprintf("all digests match; structure check: %s", det.Structural), nil)
}

// quietPipeline is pipeline without recording the tool's output (tar -t
// lists every file).
func (r *Runner) quietPipeline(ctx context.Context, src io.Reader, compression string, argv ...string) error {
	d, err := decompressor(compression)
	if err != nil {
		return err
	}
	fed, err := feed(src)
	if err != nil {
		return err
	}
	defer func() { _ = fed.Close() }()
	var in io.Reader = fed
	var dec *exec.Cmd
	if d != nil {
		dec = exec.CommandContext(ctx, d[0], d[1:]...) //nolint:gosec // fixed decompressor commands
		dec.Stdin = fed
		pipe, err := dec.StdoutPipe()
		if err != nil {
			return err
		}
		if err := dec.Start(); err != nil {
			return err
		}
		in = pipe
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // fixed tool names
	cmd.Stdin = in
	errText := new(stringsBuilder)
	cmd.Stderr = &limitWriter{w: errText, n: 4096}
	runErr := cmd.Run()
	if dec != nil {
		if runErr != nil {
			_ = dec.Process.Kill()
		}
		if derr := dec.Wait(); runErr == nil && derr != nil {
			runErr = fmt.Errorf("%s: %w", d[0], derr)
		}
	}
	if runErr != nil && errText.Len() > 0 {
		return fmt.Errorf("%w: %s", runErr, firstLine(errText.String()))
	}
	return runErr
}

// verifyRestore restores the backup into a scratch guest and removes it.
func (r *Runner) verifyRestore(ctx context.Context, t *jobs.Task, s *source, p VerifyParams, det *verifyDetails) error {
	det.VMID = p.ScratchVMID
	rp := RestoreParams{Mode: Modes(s.m.Backup.VMType)[0], TargetVMID: p.ScratchVMID, TargetStorage: p.ScratchStorage, Unique: true}
	if err := r.restore(ctx, t, s, rp); err != nil {
		return err
	}
	det.Bytes = s.m.Archive.Size
	tool, kind := r.opts.Tools.QM, "VM"
	if s.m.Backup.VMType == "lxc" {
		tool, kind = r.opts.Tools.PCT, "container"
	}
	_ = t.Event(ctx, "info", fmt.Sprintf("restore test passed; removing scratch %s %d", kind, p.ScratchVMID))
	if err := r.command(ctx, t, nil, tool, "destroy", strconv.Itoa(p.ScratchVMID), "--purge", "1"); err != nil {
		return fmt.Errorf("restore test passed, but removing the scratch guest failed: %w", err)
	}
	return nil
}
