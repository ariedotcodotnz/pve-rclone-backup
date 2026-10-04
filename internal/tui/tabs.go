// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/doctor"
	tf "github.com/ariedotcodotnz/pve-rclone-backup/internal/textfmt"
)

var tabHints = map[tab]string{
	tabBackups:  "enter details · v verify · f fetch · R restore · p protect · d delete · u undelete",
	tabQueue:    "enter details · r retry · c cancel",
	tabRemotes:  "a add · t test · c reconnect · x remove",
	tabStorages: "enter details · i init · k recovery kit · s resync",
	tabDoctor:   "enter run checks again",
}

func cleanLine(s string) string { return tf.Clean(s) }

func usageText(u *apiv1.Usage) string {
	if u == nil || u.Used == nil {
		return "-"
	}
	s := tf.Bytes(*u.Used)
	if u.Total != nil && *u.Total > 0 {
		s += " / " + tf.Bytes(*u.Total)
	}
	if u.Trashed != nil && *u.Trashed > 0 {
		s += " (" + tf.Bytes(*u.Trashed) + " in recycle bin)"
	}
	return s
}

func kitText(s apiv1.Storage) string {
	switch {
	case s.Encryption != "crypt":
		return "n/a"
	case s.KitConfirmed:
		return "confirmed"
	}
	return "MISSING"
}

func healthStyled(h string) string {
	switch h {
	case apiv1.HealthOK:
		return okStyle.Render(h)
	case apiv1.HealthDegraded, apiv1.HealthOpening, apiv1.HealthDisabled:
		return warnStyle.Render(h)
	}
	return errStyle.Render(h)
}

func progressText(j apiv1.Job) string {
	if j.TotalBytes == nil || *j.TotalBytes == 0 {
		return "-"
	}
	return fmt.Sprintf("%d%% of %s", j.ProgressBytes*100 / *j.TotalBytes, tf.Bytes(*j.TotalBytes))
}

func (m *Model) dashboard() string {
	var b strings.Builder
	st := m.status
	if st == nil {
		return dimStyle.Render("Loading…")
	}
	health := okStyle.Render("healthy")
	if !st.Healthy {
		health = warnStyle.Render("attention needed")
	}
	fmt.Fprintf(&b, "Daemon %s on %s, up %s, %s\n", cleanLine(st.Version), cleanLine(st.Node),
		(time.Duration(st.UptimeSec) * time.Second).Round(time.Second), health)
	fmt.Fprintf(&b, "Queue  %d queued · %d active · %d waiting to retry · %d failed\n", st.Jobs.Queued, st.Jobs.Active,
		st.Jobs.RetryWait, st.Jobs.Failed)
	for _, p := range st.Problems {
		b.WriteString(warnStyle.Render("Problem: "+cleanLine(p)) + "\n")
	}
	for _, a := range st.Alerts {
		style := warnStyle
		if a.Severity == "error" {
			style = errStyle
		}
		b.WriteString(style.Render("Alert: "+cleanLine(a.Message)) + "\n")
	}
	b.WriteString("\n" + titleStyle.Render("Storages") + "\n")
	if len(m.storages) == 0 {
		b.WriteString(dimStyle.Render("No rclone-backup storages yet: add a remote (Remotes, a), then initialize a storage (Storages, i).") + "\n")
	}
	for _, s := range m.storages {
		newest := ""
		for _, bk := range m.backups {
			if bk.Storage == s.ID && bk.State == "complete" {
				newest = tf.Time(tf.Unix(bk.CTime))
				break
			}
		}
		fmt.Fprintf(&b, "  %-14s %s  %s:%s  usage %s  kit %s  newest offsite backup %s\n", cleanLine(s.ID), healthStyled(s.Health),
			cleanLine(s.Remote), cleanLine(s.Path), usageText(s.Usage), kitText(s), tf.Dash(newest))
	}
	var active []string
	for _, j := range m.jobs {
		switch j.State {
		case "preparing", "uploading", "verifying", "committing", "transferring", "applying":
			active = append(active, fmt.Sprintf("  #%d %-8s %-12s %s %s", j.ID, j.Kind, j.State, cleanLine(j.Volname), progressText(j)))
		}
	}
	b.WriteString("\n" + titleStyle.Render("Running") + "\n")
	if len(active) == 0 {
		b.WriteString(dimStyle.Render("  nothing") + "\n")
	}
	for _, line := range active {
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// columns sizes columns to the terminal; the last one takes the rest.
func columns(width int, names []string, widths []int) []table.Column {
	used := 0
	for _, w := range widths {
		used += w + 2
	}
	cols := make([]table.Column, len(names))
	for i, n := range names {
		w := 0
		if i < len(widths) {
			w = widths[i]
		} else {
			w = max(10, width-used-2)
		}
		cols[i] = table.Column{Title: n, Width: w}
	}
	return cols
}

func cell(s string, w int) string { return tf.Truncate(cleanLine(s), w) }

func (m *Model) rebuildTable() {
	cursor := m.tbl.Cursor()
	if m.tblTab != m.tab {
		cursor = 0
	}
	m.tblTab = m.tab
	var cols []table.Column
	var rows []table.Row
	switch m.tab {
	case tabBackups:
		cols = columns(m.width, []string{"BACKUP", "STATE", "SIZE", "TIME", "VERIFIED", "PROT", "NOTES"}, []int{46, 10, 10, 16, 8, 4})
		for _, b := range m.backups {
			rows = append(rows, table.Row{cell(b.volid(), 46), b.State, tf.Bytes(b.Size), tf.Time(tf.Unix(b.CTime)),
				"L" + strconv.Itoa(b.VerifyLevel), map[bool]string{true: "yes", false: ""}[b.Protected], cell(b.Notes, 60)})
		}
	case tabQueue:
		cols = columns(m.width, []string{"ID", "KIND", "STATE", "STORAGE", "GUEST", "PROGRESS", "NOTE"}, []int{6, 9, 12, 12, 10, 16})
		for _, j := range m.jobs {
			guest := "-"
			if j.VMID != 0 {
				guest = fmt.Sprintf("%s/%d", j.VMType, j.VMID)
			}
			rows = append(rows, table.Row{strconv.FormatInt(j.ID, 10), j.Kind, j.State, cell(j.Storage, 12), guest, progressText(j),
				cell(j.LastError, 80)})
		}
	case tabRemotes:
		cols = columns(m.width, []string{"REMOTE", "TYPE", "AUTHORIZED", "TOKEN UNTIL", "STORAGES"}, []int{18, 10, 10, 16})
		for _, r := range m.remotes {
			rows = append(rows, table.Row{cell(r.Name, 18), cell(r.Type, 10), tf.YesNo(r.Authorized), tf.Time(r.TokenExpiry),
				cell(strings.Join(r.Storages, ","), 60)})
		}
	case tabStorages:
		cols = columns(m.width, []string{"STORAGE", "HEALTH", "ACTIVE", "LOCATION", "SOURCE", "KIT", "USAGE"}, []int{14, 14, 6, 26, 12, 9})
		for _, s := range m.storages {
			rows = append(rows, table.Row{cell(s.ID, 14), s.Health, tf.YesNo(s.Active), cell(s.Remote+":"+s.Path, 26), cell(s.Source, 12),
				kitText(s), usageText(s.Usage)})
		}
	case tabDoctor:
		cols = columns(m.width, []string{"", "CHECK", "DETAIL"}, []int{5, 26})
		for _, c := range m.checks {
			rows = append(rows, table.Row{map[string]string{doctor.OK: "OK", doctor.Warn: "WARN", doctor.Fail: "FAIL"}[c.Level],
				cell(c.Subject, 26), cell(c.Message, 200)})
		}
	default:
		return
	}
	m.tbl.SetRows(nil)
	m.tbl.SetColumns(cols)
	m.tbl.SetRows(rows)
	m.tbl.SetWidth(m.width)
	m.tbl.SetHeight(max(5, m.height-8))
	if cursor >= len(rows) {
		cursor = len(rows) - 1
	}
	m.tbl.SetCursor(max(cursor, 0))
}

func (m *Model) selectedBackup() (backupRow, bool) {
	i := m.tbl.Cursor()
	if m.tab != tabBackups || i < 0 || i >= len(m.backups) {
		return backupRow{}, false
	}
	return m.backups[i], true
}

func (m *Model) selectedJob() (apiv1.Job, bool) {
	i := m.tbl.Cursor()
	if m.tab != tabQueue || i < 0 || i >= len(m.jobs) {
		return apiv1.Job{}, false
	}
	return m.jobs[i], true
}

func (m *Model) selectedRemote() (apiv1.Remote, bool) {
	i := m.tbl.Cursor()
	if m.tab != tabRemotes || i < 0 || i >= len(m.remotes) {
		return apiv1.Remote{}, false
	}
	return m.remotes[i], true
}

func (m *Model) selectedStorage() (apiv1.Storage, bool) {
	i := m.tbl.Cursor()
	if m.tab != tabStorages || i < 0 || i >= len(m.storages) {
		return apiv1.Storage{}, false
	}
	return m.storages[i], true
}

func backupPath(b backupRow) string {
	return "/v1/storages/" + url.PathEscape(b.Storage) + "/backups/" + url.PathEscape(strings.TrimPrefix(b.Volname, "backup/"))
}

// call is an action that performs one API request.
func (m *Model) call(busy, method, path string, in any, ok func(out []byte) string) tea.Cmd {
	c := m.c
	return m.action(busy, func(ctx context.Context) doneMsg {
		var out jsonRaw
		if err := c.Do(ctx, method, path, in, &out); err != nil {
			return doneMsg{err: err}
		}
		return doneMsg{text: ok(out)}
	})
}

// tabKey handles a tab's own keys.
func (m *Model) tabKey(key string) (tea.Cmd, bool) {
	switch m.tab {
	case tabBackups:
		b, ok := m.selectedBackup()
		if !ok {
			return nil, false
		}
		switch key {
		case "enter":
			return m.showBackup(b), true
		case "v":
			return m.call("Starting verification", http.MethodPost, backupPath(b)+"/verify", apiv1.VerifyRequest{Level: 3},
				func([]byte) string { return "Content verification of " + cleanLine(b.Volname) + " queued (see Queue)" }), true
		case "f":
			m.overlay = m.fetchForm(b)
			return nil, true
		case "R":
			m.overlay = m.restoreForm(b)
			return nil, true
		case "p":
			prot := !b.Protected
			return m.call("Updating", http.MethodPatch, backupPath(b), apiv1.BackupUpdate{Protected: &prot},
				func([]byte) string {
					return map[bool]string{true: "Protected ", false: "Unprotected "}[prot] + cleanLine(b.Volname)
				}), true
		case "d":
			m.overlay = newConfirm(fmt.Sprintf("Delete %s? It is kept until the storage's grace period ends and can be restored with u.", cleanLine(b.volid())),
				m.call("Deleting", http.MethodDelete, backupPath(b), nil, func([]byte) string { return "Marked " + cleanLine(b.Volname) + " for deletion" }))
			return nil, true
		case "u":
			return m.call("Restoring", http.MethodPost, backupPath(b)+"/undelete", nil,
				func([]byte) string { return cleanLine(b.Volname) + " is kept" }), true
		}
	case tabQueue:
		j, ok := m.selectedJob()
		if !ok {
			return nil, false
		}
		switch key {
		case "enter":
			return m.showJob(j.ID), true
		case "r":
			return m.call("Retrying", http.MethodPost, fmt.Sprintf("/v1/jobs/%d/retry", j.ID), nil,
				func([]byte) string { return fmt.Sprintf("Job %d queued again", j.ID) }), true
		case "c":
			m.overlay = newConfirm(fmt.Sprintf("Cancel job %d?", j.ID), m.call("Cancelling", http.MethodPost, fmt.Sprintf("/v1/jobs/%d/cancel", j.ID), nil,
				func([]byte) string { return fmt.Sprintf("Job %d cancelled", j.ID) }))
			return nil, true
		}
	case tabRemotes:
		if key == "a" {
			m.overlay = m.remoteAddForm()
			return nil, true
		}
		r, ok := m.selectedRemote()
		if !ok {
			return nil, false
		}
		switch key {
		case "t":
			c := m.c
			return m.action("Testing "+r.Name, func(ctx context.Context) doneMsg {
				var res apiv1.ProbeResult
				if err := c.Do(ctx, http.MethodPost, "/v1/remotes/"+url.PathEscape(r.Name)+"/test", nil, &res); err != nil {
					return doneMsg{err: err}
				}
				return doneMsg{text: fmt.Sprintf("%s works: round trip %d ms, usage %s", cleanLine(r.Name), res.LatencyMS, usageText(res.Usage))}
			}), true
		case "c":
			return m.startSetup(http.MethodPost, "/v1/remotes/"+url.PathEscape(r.Name)+"/reconnect", nil), true
		case "x":
			m.overlay = newConfirm("Remove remote "+cleanLine(r.Name)+" and its credentials?",
				m.call("Removing", http.MethodDelete, "/v1/remotes/"+url.PathEscape(r.Name), nil, func([]byte) string { return "Removed " + cleanLine(r.Name) }))
			return nil, true
		}
	case tabStorages:
		if key == "i" {
			m.overlay = m.storageInitForm()
			return nil, true
		}
		s, ok := m.selectedStorage()
		if !ok {
			return nil, false
		}
		switch key {
		case "enter":
			m.overlay = newInfo("Storage "+cleanLine(s.ID), storageDetail(s))
			return nil, true
		case "s":
			return m.call("Scheduling resync", http.MethodPost, "/v1/storages/"+url.PathEscape(s.ID)+"/resync", nil,
				func([]byte) string { return "Catalogue resync of " + cleanLine(s.ID) + " scheduled" }), true
		case "k":
			m.overlay = m.kitForm([]apiv1.KitTarget{{Storage: s.ID}}, nil)
			return nil, true
		}
	case tabDoctor:
		if key == "enter" {
			return m.loadChecks(), true
		}
	}
	return nil, false
}

func storageDetail(s apiv1.Storage) string {
	return fmt.Sprintf("Health:        %s %s\nActive:        %s\nLocation:      %s:%s\nSource:        %s\nEncryption:    %s\nRepository:    %s (generation %d)\nReplicates:    %s\nResynced:      %s\nUsage:         %s\nRecovery kit:  %s",
		s.Health, cleanLine(s.HealthDetail), tf.YesNo(s.Active), cleanLine(s.Remote), cleanLine(s.Path), cleanLine(s.Source), s.Encryption,
		tf.Dash(s.RepoUUID), s.Generation, tf.Dash(cleanLine(strings.Join(s.ReplicateFrom, ", "))), tf.Time(s.LastResyncAt),
		usageText(s.Usage), kitText(s))
}

func (m *Model) showBackup(b backupRow) tea.Cmd {
	c := m.c
	return m.action("Loading", func(ctx context.Context) doneMsg {
		var d apiv1.BackupDetail
		if err := c.Do(ctx, http.MethodGet, backupPath(b), nil, &d); err != nil {
			return doneMsg{err: err}
		}
		var m struct {
			Archive struct {
				Filename   string `json:"filename"`
				SHA256     string `json:"sha256"`
				SourceNode string `json:"source_node"`
			} `json:"archive"`
			Segments struct {
				Count int   `json:"count"`
				Size  int64 `json:"size"`
			} `json:"segments"`
			RepoUUID string `json:"repo_uuid"`
		}
		_ = jsonUnmarshal(d.Manifest, &m)
		text := fmt.Sprintf("State:        %s\nGuest:        %s %d %s\nTime:         %s\nArchive:      %s, %s\nSHA-256:      %s\nSegments:     %d of %s\nFrom node:    %s\nVerified:     level %d %s %s\nProtected:    %s\nDeletion:     %s\nNotes:\n%s",
			d.State, d.VMType, d.VMID, cleanLine(d.GuestName), tf.Time(tf.Unix(d.CTime)), cleanLine(m.Archive.Filename), tf.Bytes(d.Size),
			m.Archive.SHA256, m.Segments.Count, tf.Bytes(m.Segments.Size), cleanLine(m.Archive.SourceNode), d.VerifyLevel,
			tf.Time(d.VerifiedAt), d.VerifyResult, tf.YesNo(d.Protected), tf.Time(d.DeleteAfter), tf.CleanBlock(d.Notes))
		return doneMsg{next: newInfo(cleanLine(b.volid()), text)}
	})
}

func (m *Model) showJob(id int64) tea.Cmd {
	c := m.c
	return m.action("Loading", func(ctx context.Context) doneMsg {
		var d apiv1.JobDetail
		if err := c.Do(ctx, http.MethodGet, fmt.Sprintf("/v1/jobs/%d", id), nil, &d); err != nil {
			return doneMsg{err: err}
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Kind:      %s\nState:     %s\nStorage:   %s\nBackup:    %s\nSource:    %s\nProgress:  %s\nAttempts:  %d\n",
			d.Kind, d.State, cleanLine(d.Storage), tf.Dash(cleanLine(d.Volname)), tf.Dash(cleanLine(d.SourcePath)), progressText(d.Job), d.Attempts)
		if d.LastError != "" {
			fmt.Fprintf(&b, "Error:     %s\n", cleanLine(d.LastError))
		}
		b.WriteString("\nHistory:\n")
		for _, e := range d.Events {
			fmt.Fprintf(&b, "  %s %s\n", tf.Time(&e.Time), cleanLine(e.Message))
		}
		return doneMsg{next: newInfo(fmt.Sprintf("Job %d", id), b.String())}
	})
}
