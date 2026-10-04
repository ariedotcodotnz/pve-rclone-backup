// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build e2e

package e2e

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// event is a line written by test/e2e/evlog.
type event struct {
	MS     int64  `json:"ms"`
	Op     string `json:"op"`
	Name   string `json:"name"`
	Cookie uint32 `json:"cookie,omitempty"`
	Count  int    `json:"count,omitempty"`
	Size   *int64 `json:"size,omitempty"`
}

// archiveRe matches finished archive names, as PVE::Storage::archive_info.
var archiveRe = regexp.MustCompile(`^vzdump-(qemu|lxc)-\d+-\d{4}_\d\d_\d\d-\d\d_\d\d_\d\d\.(vma|tar)(\.(zst|gz|lzo))?$`)

// archiveBaseRe matches the part of an archive name that its log shares.
var archiveBaseRe = regexp.MustCompile(`^vzdump-(qemu|lxc)-\d+-\d{4}_\d\d_\d\d-\d\d_\d\d_\d\d`)

// TestVzdumpEvents records what the discovery relies on: how vzdump writes,
// renames and removes files in a dump directory, for successful, failed
// and aborted backups (milestone M3). With E2E_RECORD=<dir> the event logs
// are written there as fixtures for internal/discovery's replay test.
func TestVzdumpEvents(t *testing.T) {
	n := newNode(t)
	n.put(t, buildEvlog(t), "/usr/local/bin/evlog")
	n.must(t, "chmod 755 /usr/local/bin/evlog")
	// A tiny storage that runs out of space mid-backup.
	n.must(t, "mkdir -p /mnt/tiny && (mountpoint -q /mnt/tiny || mount -t tmpfs -o size=24m tmpfs /mnt/tiny) && mkdir -p /mnt/tiny/dump")
	if _, code := n.run(t, "pvesm status --storage tiny"); code != 0 {
		n.must(t, "pvesm add dir tiny --path /mnt/tiny --content backup --is_mountpoint yes")
	}
	const local = "/var/lib/vz/dump"
	abort := `vzdump 100 --storage local --mode stop --compress zstd >/root/abort.log 2>&1 & pid=$!
until ls /var/lib/vz/dump/*.dat >/dev/null 2>&1; do sleep 0.2; done
sleep 1; kill -TERM $pid; wait $pid; exit 0`

	scenarios := []struct {
		name, dir, setup, cmd string
		ok                    bool
	}{
		{"qemu-snapshot", local, "qm start 100", "vzdump 100 --storage local --mode snapshot --compress zstd", true},
		{"qemu-stop", local, "qm stop 100", "vzdump 100 --storage local --mode stop --compress zstd", true},
		{"lxc-snapshot", local, "pct start 200", "vzdump 200 --storage local --mode snapshot --compress zstd", true},
		{"lxc-suspend", local, "", "vzdump 200 --storage local --mode suspend --compress zstd", true},
		{"lxc-stop-uncompressed", local, "pct stop 200", "vzdump 200 --storage local --mode stop --compress 0", true},
		{"lxc-notes-protected", local, "",
			"vzdump 200 --storage local --mode stop --compress zstd --notes-template '{{guestname}} offsite' --protected 1", true},
		{"lxc-prune", local, "", "vzdump 200 --storage local --mode stop --compress zstd --prune-backups keep-last=1", true},
		{"qemu-enospc", "/mnt/tiny/dump", "", "vzdump 100 --storage tiny --mode stop --compress zstd", false},
		{"qemu-abort", local, "", abort, false},
	}
	record := os.Getenv("E2E_RECORD")
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			if sc.setup != "" {
				n.must(t, "%s", sc.setup)
			}
			n.must(t, "rm -f /root/events.jsonl; nohup evlog %s >/root/events.jsonl 2>/dev/null </dev/null & echo $! >/root/evlog.pid; sleep 0.5", sc.dir)
			out, code := n.run(t, sc.cmd)
			n.must(t, "sleep 1; kill -TERM $(cat /root/evlog.pid); while kill -0 $(cat /root/evlog.pid) 2>/dev/null; do sleep 0.1; done")
			if (code == 0) != sc.ok && sc.name != "qemu-abort" {
				t.Fatalf("%s: exit status %d\n%s", sc.cmd, code, out)
			}
			raw := n.must(t, "cat /root/events.jsonl")
			events := parseEvents(t, raw)
			if record != "" {
				if err := os.WriteFile(filepath.Join(record, sc.name+".jsonl"), []byte(raw), 0o644); err != nil { //nolint:gosec // fixtures are public
					t.Fatal(err)
				}
			}
			checkEvents(t, events, sc.ok)
		})
	}
	n.must(t, "pvesm remove tiny && umount /mnt/tiny")
}

func parseEvents(t *testing.T, raw string) []event {
	var out []event
	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		var e event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("event %q: %v", sc.Text(), err)
		}
		out = append(out, e)
	}
	return out
}

// checkEvents asserts the properties discovery depends on.
func checkEvents(t *testing.T, events []event, ok bool) {
	t.Helper()
	var finals []string
	for i, e := range events {
		if !archiveRe.MatchString(e.Name) {
			continue
		}
		switch e.Op {
		case "create", "modify", "close_write":
			t.Errorf("event %d: %s on a finished archive name %s; vzdump should only rename into it", i, e.Op, e.Name)
		case "moved_to":
			finals = append(finals, e.Name)
		}
	}
	if !ok {
		if len(finals) > 0 {
			t.Errorf("a failed backup produced archives %v", finals)
		}
		return
	}
	if len(finals) != 1 {
		t.Fatalf("archives renamed into place: %v, want exactly one", finals)
	}
	// The task log is copied next to the archive after the rename.
	base := archiveBaseRe.FindString(finals[0])
	renamed, logged := -1, -1
	for i, e := range events {
		if e.Op == "moved_to" && e.Name == finals[0] {
			renamed = i
		}
		if e.Name == base+".log" && (e.Op == "close_write" || e.Op == "moved_to") {
			logged = i
		}
	}
	if logged < renamed {
		t.Errorf("task log %s.log written at event %d, archive renamed at %d", base, logged, renamed)
	}
}

// buildEvlog compiles the event recorder for the test node.
func buildEvlog(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "evlog")
	cmd := exec.Command("go", "build", "-trimpath", "-o", out, "./evlog") //nolint:gosec // fixed command; out is a temporary path
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH=amd64")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build evlog: %v\n%s", err, b)
	}
	return out
}
