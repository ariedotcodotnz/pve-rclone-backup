// SPDX-License-Identifier: AGPL-3.0-or-later

// Command onedrive-smoke is a manual end-to-end check of the embedded rclone
// transport against a real OneDrive account (milestone M1):
//
//   - headless OAuth: the provider URL is printed, you log in on any device,
//     then paste the address of the "page can't be reached" localhost redirect;
//   - a crypt remote (base32768 names) is layered on a scratch folder;
//   - a random file is uploaded as segments, verified via the provider's
//     QuickXorHash without downloading, downloaded, compared and deleted.
//
// Nothing is written to disk except a temporary source file. Deleted test
// objects end up in the OneDrive recycle bin.
//
//	go run ./test/manual/onedrive-smoke -size 64M -segment 16M
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/fs/rc"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/transport"
)

func main() {
	size := fs.SizeSuffix(64 << 20)
	segment := fs.SizeSuffix(16 << 20)
	flag.Var(&size, "size", "test file size")
	flag.Var(&segment, "segment", "segment size")
	path := flag.String("path", "pve-rclone-backup-smoke", "scratch folder in OneDrive (deleted afterwards)")
	clientID := flag.String("client-id", "", "own Azure app client ID (optional)")
	clientSecret := flag.String("client-secret", "", "own Azure app client secret (optional)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, int64(size), int64(segment), *path, *clientID, *clientSecret); err != nil {
		fmt.Fprintln(os.Stderr, "FAILED:", err)
		os.Exit(1)
	}
	fmt.Println("OK: all checks passed")
}

var stdin = bufio.NewReader(os.Stdin)

func ask(prompt string) (string, error) {
	fmt.Print(prompt)
	line, err := stdin.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || line == "") {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func run(ctx context.Context, size, segSize int64, path, clientID, clientSecret string) error {
	tmp, err := os.MkdirTemp("", "onedrive-smoke-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	store := transport.NewMemoryStorage()
	if err := transport.Init(transport.Options{ConfigPath: filepath.Join(tmp, "remotes.conf"), Storage: store}); err != nil {
		return err
	}
	fmt.Println("rclone engine", transport.RcloneVersion())

	params := rc.Params{}
	if clientID != "" {
		params["client_id"] = clientID
		params["client_secret"] = clientSecret
	}
	if err := configureRemote(ctx, "od", "onedrive", params); err != nil {
		return fmt.Errorf("configure OneDrive: %w", err)
	}

	store.SetSection("od-crypt", map[string]string{
		"type":                      "crypt",
		"remote":                    "od:" + path + "/g1",
		"password":                  obscure.MustObscure(rand.Text()),
		"password2":                 obscure.MustObscure(rand.Text()),
		"filename_encryption":       "standard",
		"directory_name_encryption": "true",
		"filename_encoding":         "base32768",
	})
	tgt, err := transport.NewTarget(ctx, "od-crypt:")
	if err != nil {
		return err
	}
	base, err := transport.NewTarget(ctx, "od:")
	if err != nil {
		return err
	}
	if u, err := base.About(ctx); err == nil {
		fmt.Printf("quota: total=%s used=%s free=%s trashed=%s\n", sz(u.Total), sz(u.Used), sz(u.Free), sz(u.Trashed))
	} else {
		fmt.Println("quota:", err)
	}
	fmt.Println("provider hash:", tgt.ProviderHashType())

	src, digest, err := makeSource(tmp, size)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	state := transport.NewWholeHashState()
	count := max((size+segSize-1)/segSize, 1)
	var remotes []string
	var segs []transport.Segment
	// Remove whatever was uploaded, also when a later step fails.
	defer func() {
		for _, r := range remotes {
			if err := tgt.Remove(context.Background(), r); err != nil {
				fmt.Println("cleanup:", err)
			}
		}
		fmt.Printf("cleanup done; remove the empty folder %q (and recycle bin items) in OneDrive if desired\n", path)
	}()
	start := time.Now()
	for i := range count {
		off := i * segSize
		seg := transport.Segment{Source: src, Offset: off, Size: min(segSize, size-off), ModTime: time.Now()}
		remote := fmt.Sprintf("v1/smoke/qemu/100/%s/part.%06d", start.UTC().Format("2006_01_02-15_04_05"), i)
		t0 := time.Now()
		res, err := tgt.PutSegment(ctx, remote, seg, state)
		if err != nil {
			return fmt.Errorf("upload segment %d: %w", i, err)
		}
		fmt.Printf("segment %d: %s in %s (%.1f MiB/s) stored=%d %s=%s\n", i, fs.SizeSuffix(seg.Size), time.Since(t0).Round(time.Millisecond),
			float64(seg.Size)/(1<<20)/time.Since(t0).Seconds(), res.StoredSize, res.StoredHashType, res.StoredHash)
		state = res.WholeState
		remotes = append(remotes, remote)
		segs = append(segs, seg)
	}
	whole, err := transport.WholeHashSum(state)
	if err != nil {
		return err
	}
	if whole != digest {
		return fmt.Errorf("whole-archive hash %s != %s", whole, digest)
	}
	fmt.Printf("uploaded %s in %s\n", fs.SizeSuffix(size), time.Since(start).Round(time.Second))

	h := sha256.New()
	for i, r := range remotes {
		ok, err := tgt.VerifySegment(ctx, r, segs[i])
		if err != nil {
			return fmt.Errorf("verify segment %d without download: %w", i, err)
		}
		if !ok {
			return fmt.Errorf("segment %d: provider hash does not match the local data", i)
		}
		rc, err := tgt.Open(ctx, r)
		if err != nil {
			return err
		}
		_, err = io.Copy(h, rc)
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("download segment %d: %w", i, err)
		}
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != digest {
		return fmt.Errorf("downloaded data hash %s != %s", got, digest)
	}
	fmt.Println("verified provider hashes and downloaded content")
	return nil
}

// configureRemote drives rclone's configuration state machine on the
// terminal, using the OAuth relay for the browser step.
func configureRemote(ctx context.Context, name, provider string, params rc.Params) error {
	out, err := config.CreateRemote(ctx, name, provider, params, config.UpdateRemoteOpt{NonInteractive: true})
	for err == nil && out != nil && out.State != "" {
		if out.Error != "" {
			fmt.Println("error:", out.Error)
		}
		var result string
		if out.Option != nil && out.Option.Name == "config_is_local" {
			out, err = oauthRelay(ctx, name, out.State)
			continue
		}
		if out.Option != nil {
			result, err = askOption(out.Option)
			if err != nil {
				return err
			}
		}
		out, err = config.UpdateRemote(ctx, name, rc.Params{}, config.UpdateRemoteOpt{Continue: true, State: out.State, Result: result})
	}
	return err
}

func askOption(o *fs.Option) (string, error) {
	fmt.Printf("\n%s\n", o.Help)
	for _, e := range o.Examples {
		fmt.Printf("  %-12s %s\n", e.Value, strings.ReplaceAll(e.Help, "\n", " "))
	}
	def := fmt.Sprint(o.Default)
	answer, err := ask(fmt.Sprintf("%s [%s]> ", o.Name, def))
	if answer == "" {
		answer = def
	}
	return answer, err
}

func oauthRelay(ctx context.Context, name, state string) (*fs.ConfigOut, error) {
	type result struct {
		out *fs.ConfigOut
		err error
	}
	done := make(chan result, 1)
	go func() {
		o, err := config.UpdateRemote(ctx, name, rc.Params{config.ConfigAuthNoBrowser: "true"},
			config.UpdateRemoteOpt{Continue: true, State: state, Result: "true"})
		done <- result{o, err}
	}()

	var url string
	for deadline := time.Now().Add(15 * time.Second); ; {
		var err error
		if url, err = transport.ProviderAuthURL(ctx); err == nil {
			break
		} else if !errors.Is(err, transport.ErrOAuthNotRunning) || time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Printf("\nOpen this URL on any device and log in:\n\n%s\n\n", url)
	fmt.Println("Your browser will then fail to load a http://localhost:53682/... page.")
	for {
		pasted, err := ask("Paste that complete address here: ")
		if err != nil {
			return nil, err
		}
		if err := transport.RelayRedirect(ctx, pasted); err != nil {
			fmt.Println(err)
			continue
		}
		r := <-done
		return r.out, r.err
	}
}

func makeSource(dir string, size int64) (*os.File, string, error) {
	f, err := os.Create(filepath.Join(dir, "source.bin")) //nolint:gosec // dir is our own temp dir
	if err != nil {
		return nil, "", err
	}
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for written := int64(0); written < size; {
		chunk := buf[:min(int64(len(buf)), size-written)]
		clear(chunk)
		if written%(4<<20) == 0 { // random islands, zero holes
			_, _ = rand.Read(chunk)
		}
		if _, err := f.Write(chunk); err != nil {
			_ = f.Close()
			return nil, "", err
		}
		h.Write(chunk)
		written += int64(len(chunk))
	}
	return f, hex.EncodeToString(h.Sum(nil)), nil
}

func sz(v *int64) string {
	if v == nil {
		return "?"
	}
	return fs.SizeSuffix(*v).String()
}
