// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build e2e

// Package e2e drives the nested Proxmox VE node of test/e2e/vm.sh over SSH:
// the built package is installed and exercised through PVE's own tools and
// API, as an administrator would.
//
//	test/e2e/vm.sh up && eval "$(test/e2e/vm.sh env)" && \
//	    E2E_DEB=dist/pve-rclone-backup_..._amd64.deb go test -tags e2e -v ./test/e2e
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// node is the test VM.
type node struct {
	host, port, key string
	// name is the PVE node name.
	name string
	// token authorises requests to the PVE API over HTTPS, the way the
	// GUI's requests reach pvedaemon.
	token string
}

func newNode(t *testing.T) *node {
	target, key := os.Getenv("E2E_SSH"), os.Getenv("E2E_SSH_KEY")
	if target == "" || key == "" {
		t.Fatal("E2E_SSH and E2E_SSH_KEY are not set; run: eval \"$(test/e2e/vm.sh env)\"")
	}
	hostport := strings.TrimPrefix(target, "root@")
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		t.Fatalf("E2E_SSH=%s: %v", target, err)
	}
	n := &node{host: host, port: port, key: key}
	n.name = strings.TrimSpace(n.must(t, "hostname"))
	return n
}

func (n *node) sshArgs() []string {
	return []string{"-i", n.key, "-p", n.port, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "root@" + n.host}
}

// exitMarker ends the output of every command run on the node with its
// exit status, which ssh's own status cannot tell apart from a failed
// connection (both may be 255).
const exitMarker = "\n__e2e_exit="

// run executes a shell command on the node and returns its combined output
// and exit status.
func (n *node) run(t testing.TB, cmd string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	full := "(" + cmd + "\n); printf '" + strings.ReplaceAll(exitMarker, "\n", "\\n") + "%d\\n' $?"
	out, err := exec.CommandContext(ctx, "ssh", append(n.sshArgs(), full)...).CombinedOutput() //nolint:gosec // commands for the test VM
	i := strings.LastIndex(string(out), exitMarker)
	if i < 0 {
		t.Fatalf("ssh failed running %q: %v\n%s", cmd, err, out)
	}
	var code int
	if _, err := fmt.Sscanf(string(out[i+len(exitMarker):]), "%d", &code); err != nil {
		t.Fatalf("%s: no exit status in %q", cmd, out[i:])
	}
	return string(out[:i]), code
}

// must runs a command that has to succeed.
func (n *node) must(t testing.TB, format string, args ...any) string {
	t.Helper()
	cmd := fmt.Sprintf(format, args...)
	out, code := n.run(t, cmd)
	if code != 0 {
		t.Fatalf("%s: exit status %d\n%s", cmd, code, out)
	}
	return out
}

// put copies a local file to the node.
func (n *node) put(t testing.TB, local, remote string) {
	t.Helper()
	args := []string{"-q", "-i", n.key, "-P", n.port, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR", "-o", "BatchMode=yes", local, "root@" + n.host + ":" + remote}
	if out, err := exec.Command("scp", args...).CombinedOutput(); err != nil { //nolint:gosec // copies to the test VM
		t.Fatalf("scp %s: %v\n%s", local, err, out)
	}
}

// cli runs pve-rclone-backup with JSON output and decodes it into v.
func (n *node) cli(t testing.TB, v any, args ...string) {
	t.Helper()
	out := n.must(t, "pve-rclone-backup --output json %s", quoteAll(args))
	if v != nil {
		if err := json.Unmarshal([]byte(out), v); err != nil {
			t.Fatalf("pve-rclone-backup %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

// pvesh runs a pvesh command with JSON output and decodes it into v.
func (n *node) pvesh(t testing.TB, v any, args ...string) {
	t.Helper()
	out := n.must(t, "pvesh %s --output-format json", quoteAll(args))
	if v != nil {
		if err := json.Unmarshal([]byte(out), v); err != nil {
			t.Fatalf("pvesh %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
}

// apiResult is the envelope of a PVE API response.
type apiResult struct {
	Data   json.RawMessage   `json:"data"`
	Errors map[string]string `json:"errors"`
}

// api sends a request through pveproxy and pvedaemon (the GUI's path) and
// returns the HTTP status and the decoded envelope. params are form
// values.
func (n *node) api(t testing.TB, method, path string, params ...string) (int, apiResult) {
	t.Helper()
	if n.token == "" {
		t.Fatal("no API token")
	}
	cmd := fmt.Sprintf("curl -sk -o /tmp/api.json -w '%%{http_code}' -X %s -H %s", method,
		quote("Authorization: PVEAPIToken="+n.token))
	if method == "GET" || method == "DELETE" {
		cmd += " -G" // parameters in the query string
	}
	for _, p := range params {
		cmd += " --data-urlencode " + quote(p)
	}
	cmd += " " + quote("https://127.0.0.1:8006/api2/json"+path) + " && echo && cat /tmp/api.json"
	out := n.must(t, "%s", cmd)
	status, body, _ := strings.Cut(out, "\n")
	var code int
	if _, err := fmt.Sscan(status, &code); err != nil {
		t.Fatalf("%s %s: bad status %q", method, path, status)
	}
	var res apiResult
	if body != "" {
		if err := json.Unmarshal([]byte(body), &res); err != nil {
			t.Fatalf("%s %s: %v\n%s", method, path, err, body)
		}
	}
	return code, res
}

// apiOK is api for requests that have to succeed; it decodes data into v.
func (n *node) apiOK(t testing.TB, v any, method, path string, params ...string) {
	t.Helper()
	code, res := n.api(t, method, path, params...)
	if code != 200 {
		t.Fatalf("%s %s: HTTP %d %v", method, path, code, res.Errors)
	}
	if v != nil {
		if err := json.Unmarshal(res.Data, v); err != nil {
			t.Fatalf("%s %s: %v\n%s", method, path, err, res.Data)
		}
	}
}

// waitFor polls cond until it reports done.
func waitFor(t testing.TB, what string, timeout time.Duration, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		done, status := cond()
		if done {
			return
		}
		if status != last {
			t.Logf("waiting for %s: %s", what, status)
			last = status
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s (last: %s)", what, status)
		}
		time.Sleep(3 * time.Second)
	}
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func quoteAll(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = quote(a)
	}
	return strings.Join(q, " ")
}
