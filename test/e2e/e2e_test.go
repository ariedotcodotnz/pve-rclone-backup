// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
)

// The guests created by provision.sh.
const (
	vmID       = 100 // no OS; one 512 MiB disk with 160 MiB of random data
	ctID       = 200 // unprivileged Alpine container with /root/payload
	privCTID   = 201 // privileged Alpine container with /root/payload
	storageID  = "offsite"
	remoteName = "sim"
)

// backupRow is a line of 'backup list --output json'.
type backupRow struct {
	Storage string `json:"storage"`
	apiv1.Backup
}

// content is an entry of PVE's storage content API.
type content struct {
	Volid     string `json:"volid"`
	Format    string `json:"format"`
	Subtype   string `json:"subtype"`
	Size      int64  `json:"size"`
	CTime     int64  `json:"ctime"`
	VMID      int    `json:"vmid"`
	Notes     string `json:"notes"`
	Protected int    `json:"protected"`
}

// suite carries state from one step to the next.
type suite struct {
	n *node
	// volids of the first offsite backups by guest.
	vm, ct, privCT string
}

// TestPVE runs the scenarios in order on one node; each step builds on the
// previous ones, so the first failure stops the run.
func TestPVE(t *testing.T) {
	deb := os.Getenv("E2E_DEB")
	if deb == "" {
		t.Fatal("E2E_DEB (the package to test) is not set")
	}
	s := &suite{n: newNode(t)}
	steps := []struct {
		name string
		fn   func(t *testing.T)
	}{
		{"install", func(t *testing.T) { s.install(t, deb) }},
		{"setup", s.setup},
		{"replicate", s.replicate},
		{"pve-listing", s.listing},
		{"show-configuration", s.showConfig},
		{"notes-and-protection", s.notesAndProtection},
		{"gui-delete-is-a-tombstone", s.guiDelete},
		{"direct-backup-refused", s.directBackupRefused},
		{"prune", s.prune},
		{"fetch-and-native-restore", s.fetchAndRestore},
		{"vm-stream-restore", s.streamRestore},
		{"container-restores", s.containerRestores},
		{"content-verification", s.verifyContent},
		{"resume-after-restart", s.resumeAfterRestart},
		{"daemon-down", s.daemonDown},
		{"rebuild-state", s.rebuildState},
		{"remove-package", s.remove},
	}
	// E2E_STOP_AFTER=<step> leaves the node in that step's state, to look
	// around (with make e2e E2E_KEEP=1).
	stop := os.Getenv("E2E_STOP_AFTER")
	for _, step := range steps {
		if !t.Run(step.name, step.fn) {
			t.FailNow()
		}
		if step.name == stop {
			t.Logf("stopping after %s (E2E_STOP_AFTER)", stop)
			return
		}
	}
}

func (s *suite) install(t *testing.T, deb string) {
	n := s.n
	n.put(t, deb, "/root/pve-rclone-backup.deb")
	n.must(t, "DEBIAN_FRONTEND=noninteractive apt-get install -y /root/pve-rclone-backup.deb")
	n.must(t, "systemctl is-active pve-rclone-backupd")
	n.must(t, "pve-rclone-backup status")
	// The GUI's requests reach the plugin through pveproxy and pvedaemon,
	// which the package's trigger reloads; an API token exercises that path.
	var tok struct {
		Value string `json:"value"`
	}
	n.pvesh(t, &tok, "create", "/access/users/root@pam/token/e2e", "--privsep", "0")
	n.token = "root@pam!e2e=" + tok.Value
}

func (s *suite) setup(t *testing.T) {
	n := s.n
	// A local-filesystem remote stands in for the cloud; the daemon reads
	// remotes at start-up. Its path is relative to the daemon's working
	// directory (/).
	n.must(t, "mkdir -p /etc/pve/priv/pve-rclone-backup && printf '[%s]\\ntype = local\\n' >/etc/pve/priv/pve-rclone-backup/remotes.conf", remoteName)
	n.must(t, "systemctl restart pve-rclone-backupd")
	var remotes []apiv1.Remote
	n.cli(t, &remotes, "remote", "list")
	if len(remotes) != 1 || remotes[0].Name != remoteName {
		t.Fatalf("remotes = %+v", remotes)
	}

	init := []string{"storage", "init", storageID, "--remote", remoteName, "--path", "srv/remote-sim/repo",
		"--source", "e2e", "--replicate-from", "local"}
	out, code := n.run(t, "pve-rclone-backup "+quoteAll(append(init, "--kit-file", "/root/offsite-kit.txt")))
	sum := regexp.MustCompile(`--confirm-checksum ([0-9a-f-]+)`).FindStringSubmatch(out)
	if code != 2 || sum == nil {
		t.Fatalf("init without a confirmed kit: exit %d\n%s", code, out)
	}
	n.must(t, "pve-rclone-backup recovery-kit confirm %s", sum[1])
	n.must(t, "pve-rclone-backup %s", quoteAll(init))

	// Several segments per VM archive; prune tests need no minimum age.
	n.must(t, "pvesh set /storage/%s --rclone-segment-size 64M --rclone-min-age 0", storageID)
	// The test node's disk is small: keep 1 GiB free instead of 10.
	n.must(t, "printf 'staging-reserve: 1G\\n' >/etc/pve/nodes/%s/pve-rclone-backup.cfg", n.name)
	n.must(t, "systemctl restart pve-rclone-backupd")
	waitFor(t, "the storage to be active", time.Minute, func() (bool, string) {
		out := n.must(t, "pvesm status --storage %s", storageID)
		return strings.Contains(out, "active"), out
	})
}

// vzdump backs a guest up to local storage and returns the archive name.
func (s *suite) vzdump(t *testing.T, vmid int, mode string) string {
	t.Helper()
	before := s.localArchives(t, vmid)
	s.n.must(t, "vzdump %d --storage local --mode %s --compress zstd", vmid, mode)
	for _, a := range s.localArchives(t, vmid) {
		if !slices.Contains(before, a) {
			return a
		}
	}
	t.Fatalf("vzdump %d produced no new archive", vmid)
	return ""
}

func (s *suite) localArchives(t *testing.T, vmid int) []string {
	out, _ := s.n.run(t, fmt.Sprintf("ls /var/lib/vz/dump | grep -E '^vzdump-(qemu|lxc)-%d-.*\\.(vma|tar)\\.zst$'", vmid))
	return strings.Fields(out)
}

// waitReplicated waits until the archive is a complete offsite backup.
func (s *suite) waitReplicated(t *testing.T, archive string) apiv1.Backup {
	t.Helper()
	var got apiv1.Backup
	waitFor(t, "replication of "+archive, 10*time.Minute, func() (bool, string) {
		var jobs []apiv1.Job
		s.n.cli(t, &jobs, "queue", "list", "--storage", storageID)
		status := "not discovered"
		for _, j := range jobs {
			if j.Kind == "replicate" && path.Base(j.SourcePath) == archive {
				status = fmt.Sprintf("job %d %s %d/%d bytes %s", j.ID, j.State, j.ProgressBytes, derefOr(j.TotalBytes), j.LastError)
				if j.State == "failed" || j.State == "source_lost" {
					t.Fatalf("replication of %s: %s", archive, status)
				}
			}
		}
		for _, b := range s.backups(t, "") {
			if b.Volname == "backup/"+archive && b.State == "complete" {
				got = b.Backup
				return true, ""
			}
		}
		return false, status
	})
	return got
}

func derefOr(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func (s *suite) backups(t *testing.T, state string) []backupRow {
	var rows []backupRow
	args := []string{"backup", "list", "--storage", storageID}
	if state != "" {
		args = append(args, "--state", state)
	}
	s.n.cli(t, &rows, args...)
	return rows
}

func volid(archive string) string { return storageID + ":backup/" + archive }

func (s *suite) replicate(t *testing.T) {
	n := s.n
	n.must(t, "qm start %d", vmID)
	vm := s.vzdump(t, vmID, "snapshot")
	n.must(t, "qm stop %d", vmID)
	// Container file systems hold setuid programs, which restores must keep.
	for _, id := range []int{ctID, privCTID} {
		n.must(t, "pct mount %[1]d >/dev/null && cd /var/lib/lxc/%[1]d/rootfs/root && cp ../bin/busybox suid && "+
			"chown --reference=. suid && chmod 4755 suid; c=$?; cd /; pct unmount %[1]d; exit $c", id)
	}
	ct := s.vzdump(t, ctID, "stop")
	priv := s.vzdump(t, privCTID, "suspend")
	for _, a := range []string{vm, ct, priv} {
		b := s.waitReplicated(t, a)
		if b.VerifyLevel != 2 {
			t.Errorf("%s: verify level %d after upload, want 2", a, b.VerifyLevel)
		}
		local := strings.TrimSpace(n.must(t, "stat -c %%s /var/lib/vz/dump/%s", a))
		if fmt.Sprint(b.Size) != local {
			t.Errorf("%s: offsite size %d, local %s", a, b.Size, local)
		}
	}
	s.vm, s.ct, s.privCT = volid(vm), volid(ct), volid(priv)

	var detail struct {
		Manifest struct {
			Segments struct {
				Count int `json:"count"`
			} `json:"segments"`
		} `json:"manifest"`
	}
	n.cli(t, &detail, "backup", "inspect", s.vm)
	if detail.Manifest.Segments.Count < 2 {
		t.Errorf("VM archive stored as %d segments, want several", detail.Manifest.Segments.Count)
	}
}

func (s *suite) contentList(t *testing.T) map[string]content {
	var list []content
	s.n.apiOK(t, &list, "GET", fmt.Sprintf("/nodes/%s/storage/%s/content", s.n.name, storageID))
	out := map[string]content{}
	for _, c := range list {
		out[c.Volid] = c
	}
	return out
}

func (s *suite) listing(t *testing.T) {
	n := s.n
	list := s.contentList(t)
	for volid, subtype := range map[string]string{s.vm: "qemu", s.ct: "lxc", s.privCT: "lxc"} {
		c, ok := list[volid]
		if !ok {
			t.Errorf("%s missing from the GUI's content list: %v", volid, list)
			continue
		}
		if c.Subtype != subtype || c.Size == 0 || c.CTime == 0 || c.VMID == 0 {
			t.Errorf("%s: %+v", volid, c)
		}
	}
	if out := n.must(t, "pvesm list %s", storageID); !strings.Contains(out, s.vm) {
		t.Errorf("pvesm list:\n%s", out)
	}
	var st struct {
		Active int   `json:"active"`
		Total  int64 `json:"total"`
	}
	n.apiOK(t, &st, "GET", fmt.Sprintf("/nodes/%s/storage/%s/status", n.name, storageID))
	if st.Active != 1 {
		t.Errorf("storage status = %+v", st)
	}
}

func (s *suite) showConfig(t *testing.T) {
	n := s.n
	if out := n.must(t, "pvesm extractconfig %s", s.vm); !strings.Contains(out, "name: e2e-vm") {
		t.Errorf("pvesm extractconfig:\n%s", out)
	}
	var cfg string
	n.apiOK(t, &cfg, "GET", fmt.Sprintf("/nodes/%s/vzdump/extractconfig", n.name), "volume="+s.ct)
	if !strings.Contains(cfg, "hostname: e2e-ct") {
		t.Errorf("GUI Show Configuration:\n%s", cfg)
	}
}

func (s *suite) setAttrs(t *testing.T, vol string, params ...string) {
	s.n.apiOK(t, nil, "PUT", fmt.Sprintf("/nodes/%s/storage/%s/content/%s", s.n.name, storageID, vol), params...)
}

func (s *suite) notesAndProtection(t *testing.T) {
	s.setAttrs(t, s.ct, "notes=offsite e2e notes", "protected=1")
	c := s.contentList(t)[s.ct]
	if c.Notes != "offsite e2e notes" || c.Protected != 1 {
		t.Fatalf("after update: %+v", c)
	}
	var d apiv1.BackupDetail
	s.n.cli(t, &d, "backup", "inspect", s.ct)
	if d.Notes != "offsite e2e notes" || !d.Protected {
		t.Errorf("catalogue: notes %q protected %v", d.Notes, d.Protected)
	}
}

// waitTask waits for a PVE task and returns its exit status.
func (s *suite) waitTask(t *testing.T, upid string) string {
	t.Helper()
	var exit string
	waitFor(t, "task "+upid, 5*time.Minute, func() (bool, string) {
		var st struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		s.n.apiOK(t, &st, "GET", fmt.Sprintf("/nodes/%s/tasks/%s/status", s.n.name, upid))
		exit = st.ExitStatus
		return st.Status == "stopped", st.Status
	})
	return exit
}

// apiTask starts a task through the API and returns its exit status.
func (s *suite) apiTask(t *testing.T, method, ep string, params ...string) string {
	t.Helper()
	code, res := s.n.api(t, method, ep, params...)
	if code != 200 {
		return fmt.Sprintf("HTTP %d %s", code, res.Data)
	}
	var upid string
	if err := json.Unmarshal(res.Data, &upid); err != nil {
		t.Fatalf("%s %s: %v", method, ep, err)
	}
	return s.waitTask(t, upid)
}

func (s *suite) guiDelete(t *testing.T) {
	ep := fmt.Sprintf("/nodes/%s/storage/%s/content/%s", s.n.name, storageID, s.ct)
	if exit := s.apiTask(t, "DELETE", ep); exit == "OK" {
		t.Fatal("deleting a protected backup succeeded")
	}
	s.setAttrs(t, s.ct, "protected=0")
	if exit := s.apiTask(t, "DELETE", ep); exit != "OK" {
		t.Fatalf("delete: %s", exit)
	}
	if _, ok := s.contentList(t)[s.ct]; ok {
		t.Error("a deleted backup is still listed")
	}
	var tombstoned bool
	for _, b := range s.backups(t, "all") {
		if volid(path2name(b.Volname)) == s.ct {
			tombstoned = b.State == "tombstoned" && b.DeleteAfter != nil
		}
	}
	if !tombstoned {
		t.Fatal("GUI delete did not leave a tombstone")
	}
	// Nothing is deleted from the remote during the grace period.
	s.n.must(t, "pve-rclone-backup backup undelete %s", s.ct)
	if _, ok := s.contentList(t)[s.ct]; !ok {
		t.Fatal("undeleted backup is not listed")
	}
}

func path2name(volname string) string { return strings.TrimPrefix(volname, "backup/") }

func (s *suite) directBackupRefused(t *testing.T) {
	out, code := s.n.run(t, fmt.Sprintf("vzdump %d --storage %s", ctID, storageID))
	if code == 0 || !strings.Contains(out, "direct backups to an rclone-backup storage are not supported") {
		t.Fatalf("vzdump to the offsite storage: exit %d\n%s", code, out)
	}
}

func (s *suite) prune(t *testing.T) {
	// A second backup of the container, so keep-last=1 removes the first.
	second := volid(s.vzdump(t, ctID, "stop"))
	s.waitReplicated(t, path.Base(second))
	ep := fmt.Sprintf("/nodes/%s/storage/%s/prunebackups", s.n.name, storageID)
	params := []string{"prune-backups=keep-last=1", "type=lxc", fmt.Sprintf("vmid=%d", ctID)}
	var marks []struct {
		Volid string `json:"volid"`
		Mark  string `json:"mark"`
	}
	s.n.apiOK(t, &marks, "GET", ep, params...)
	got := map[string]string{}
	for _, m := range marks {
		got[m.Volid] = m.Mark
	}
	if got[s.ct] != "remove" || got[second] != "keep" {
		t.Fatalf("prune preview = %v", got)
	}
	if exit := s.apiTask(t, "DELETE", ep, params...); exit != "OK" {
		t.Fatalf("prune: %s", exit)
	}
	list := s.contentList(t)
	if _, ok := list[s.ct]; ok {
		t.Error("pruned backup is still listed")
	}
	if _, ok := list[second]; !ok {
		t.Error("kept backup is gone")
	}
	s.n.must(t, "pve-rclone-backup backup undelete %s", s.ct)
}

// diskSHA256 hashes a guest's first disk.
func (s *suite) diskSHA256(t *testing.T, vmid int) string {
	return strings.Fields(s.n.must(t, "sha256sum <$(pvesm path local-lvm:vm-%d-disk-0)", vmid))[0]
}

func (s *suite) fetchAndRestore(t *testing.T) {
	n := s.n
	name := path2name(strings.TrimPrefix(s.vm, storageID+":"))
	want := strings.TrimSpace(n.must(t, "cat /root/vm100.sha256"))
	// The local copy is lost; fetch it back.
	n.must(t, "pvesm free local:backup/%s", name)
	n.must(t, "pve-rclone-backup backup fetch %s --to-storage local", s.vm)
	n.must(t, "test -f /var/lib/vz/dump/%[1]s && test -f /var/lib/vz/dump/%[1]s.protected", name)
	n.must(t, "qmrestore /var/lib/vz/dump/%s 900 --storage local-lvm", name)
	if got := s.diskSHA256(t, 900); got != want {
		t.Errorf("restored disk %s, want %s", got, want)
	}
	n.must(t, "qm destroy 900 --purge")

	// The fetched archive is never uploaded again.
	n.must(t, "pve-rclone-backup queue scan --storage local")
	var jobs []apiv1.Job
	n.cli(t, &jobs, "queue", "list", "--storage", storageID)
	var uploads []string
	for _, j := range jobs {
		if path.Base(j.SourcePath) == name && j.State != "skipped" {
			uploads = append(uploads, fmt.Sprintf("job %d %s", j.ID, j.State))
		}
	}
	if len(uploads) != 1 {
		t.Errorf("uploads of %s (the original only, not the fetched copy): %v", name, uploads)
	}
}

func (s *suite) streamRestore(t *testing.T) {
	n := s.n
	n.must(t, "pve-rclone-backup restore %s --vmid 901 --target-storage local-lvm --mode stream", s.vm)
	if got, want := s.diskSHA256(t, 901), strings.TrimSpace(n.must(t, "cat /root/vm100.sha256")); got != want {
		t.Errorf("restored disk %s, want %s", got, want)
	}
	n.must(t, "qm destroy 901 --purge")
}

func (s *suite) containerRestores(t *testing.T) {
	n := s.n
	for vol, vmid := range map[string]int{s.ct: 902, s.privCT: 903} {
		n.must(t, "pve-rclone-backup restore %s --vmid %d --target-storage local-lvm", vol, vmid)
		// The payload's checksum, recorded inside the original container.
		out := n.must(t, "pct mount %[1]d >/dev/null && cd /var/lib/lxc/%[1]d/rootfs && "+
			"sha256sum <root/payload && cat root/payload.sha256 && stat -c %%a root/suid; c=$?; cd /; pct unmount %[1]d; exit $c", vmid)
		if f := strings.Fields(out); len(f) != 5 || f[0] != f[2] || f[4] != "4755" {
			t.Errorf("container %d payload checksum and setuid file mode:\n%s", vmid, out)
		}
		unpriv := strings.Contains(n.must(t, "pct config %d", vmid), "unprivileged: 1")
		if unpriv != (vmid == 902) {
			t.Errorf("container %d unprivileged = %v", vmid, unpriv)
		}
		n.must(t, "pct destroy %d --purge", vmid)
	}
}

func (s *suite) verifyContent(t *testing.T) {
	s.n.must(t, "pve-rclone-backup backup verify %s --level 3", s.vm)
	var d apiv1.BackupDetail
	s.n.cli(t, &d, "backup", "inspect", s.vm)
	if d.VerifyLevel != 3 {
		t.Errorf("verify level %d after content verification", d.VerifyLevel)
	}
}

// remoteFiles lists the remote's objects with their modification times.
func (s *suite) remoteFiles(t *testing.T) map[string]string {
	out := s.n.must(t, "find /srv/remote-sim -type f -printf '%%T@ %%p\\n'")
	files := map[string]string{}
	for line := range strings.Lines(out) {
		if mt, p, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			files[p] = mt
		}
	}
	return files
}

func (s *suite) resumeAfterRestart(t *testing.T) {
	n := s.n
	n.must(t, "pvesh set /storage/%s --rclone-bwlimit 4M", storageID)
	n.must(t, "systemctl restart pve-rclone-backupd")
	before := s.remoteFiles(t)
	archive := s.vzdump(t, vmID, "stop")
	var job apiv1.Job
	waitFor(t, "the first segment of "+archive, 10*time.Minute, func() (bool, string) {
		var jobs []apiv1.Job
		n.cli(t, &jobs, "queue", "list", "--storage", storageID)
		for _, j := range jobs {
			if path.Base(j.SourcePath) == archive {
				job = j
				return j.State == "uploading" && j.NextSegment >= 1, fmt.Sprintf("%s segment %d", j.State, j.NextSegment)
			}
		}
		return false, "not discovered"
	})
	uploaded := map[string]string{}
	for p, mt := range s.remoteFiles(t) {
		if _, ok := before[p]; !ok {
			uploaded[p] = mt
		}
	}
	n.must(t, "systemctl restart pve-rclone-backupd")
	s.waitReplicated(t, archive)
	after := s.remoteFiles(t)
	for p, mt := range uploaded {
		// A file that vanished was an unfinished upload.
		if amt, ok := after[p]; ok && amt != mt {
			t.Errorf("%s was uploaded again after the restart", p)
		}
	}
	var d apiv1.JobDetail
	n.cli(t, &d, "queue", "show", fmt.Sprint(job.ID))
	t.Logf("job %d: %d segments, %d attempts", d.ID, len(d.Segments), d.Attempts)
	n.must(t, "pvesh set /storage/%s --delete rclone-bwlimit", storageID)
}

func (s *suite) daemonDown(t *testing.T) {
	n := s.n
	n.must(t, "systemctl stop pve-rclone-backupd")
	start := time.Now()
	out, _ := n.run(t, "pvesm status")
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("pvesm status took %s with the daemon down", d)
	}
	if !regexp.MustCompile(storageID + `\s+rclone-backup\s+inactive`).MatchString(out) {
		t.Errorf("pvesm status with the daemon down:\n%s", out)
	}
	// Local backups are unaffected; the archive is picked up on start.
	archive := s.vzdump(t, ctID, "stop")
	n.must(t, "systemctl start pve-rclone-backupd")
	s.waitReplicated(t, archive)
}

func (s *suite) rebuildState(t *testing.T) {
	n := s.n
	names := func() []string {
		var out []string
		for _, b := range s.backups(t, "all") {
			out = append(out, b.Volname+" "+b.State)
		}
		slices.Sort(out)
		return out
	}
	want := names()
	n.must(t, "systemctl stop pve-rclone-backupd && rm -f /var/lib/pve-rclone-backup/state.db* && systemctl start pve-rclone-backupd")
	waitFor(t, "the catalogue to be rebuilt", 5*time.Minute, func() (bool, string) {
		got := names()
		return slices.Equal(got, want), fmt.Sprintf("%d of %d backups", len(got), len(want))
	})
}

func (s *suite) remove(t *testing.T) {
	n := s.n
	n.must(t, "pvesh delete /storage/%s", storageID)
	n.must(t, "DEBIAN_FRONTEND=noninteractive apt-get purge -y pve-rclone-backup")
	if _, code := n.run(t, "systemctl is-active pve-rclone-backupd"); code == 0 {
		t.Error("daemon still running after purge")
	}
	n.must(t, "pvesm status")
	n.must(t, "test ! -e /var/lib/pve-rclone-backup")
	// Cluster-wide configuration and secrets are kept.
	n.must(t, "test -e /etc/pve/priv/pve-rclone-backup/remotes.conf")
}
