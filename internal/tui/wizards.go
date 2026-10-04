// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/api/apiv1"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/client"
)

func yes(v string) bool { return strings.EqualFold(v, "y") || strings.EqualFold(v, "yes") }

// remoteAddForm starts configuring a OneDrive remote.
func (m *Model) remoteAddForm() overlay {
	return newForm("Add a OneDrive remote",
		"Leave the client ID empty to use rclone's shared app; your own app registration avoids throttling.",
		[]*field{
			newField("name", "Remote name", "onedrive", true, false),
			newField("client_id", "OAuth client ID (optional)", "", false, false),
			newField("client_secret", "OAuth client secret (optional)", "", false, true),
		}, func(v map[string]string) (tea.Cmd, string) {
			params := map[string]string{}
			if v["client_id"] != "" {
				params["client_id"] = v["client_id"]
			}
			if v["client_secret"] != "" {
				params["client_secret"] = v["client_secret"]
			}
			return m.startSetup(http.MethodPost, "/v1/remote-setup",
				apiv1.RemoteSetupRequest{Name: v["name"], Provider: "onedrive", Params: params}), ""
		})
}

func (m *Model) startSetup(method, path string, body any) tea.Cmd {
	c := m.c
	return m.action("Starting remote setup", func(ctx context.Context) doneMsg {
		var s apiv1.RemoteSetup
		if err := c.Do(ctx, method, path, body, &s); err != nil {
			return doneMsg{err: err}
		}
		return m.advanceSetup(ctx, c, s)
	})
}

// advanceSetup answers questions that need no user input (the relay
// method, replacing an old token) and returns the next dialog.
func (m *Model) advanceSetup(ctx context.Context, c *client.Client, s apiv1.RemoteSetup) doneMsg {
	finish := func() { _ = c.Do(context.WithoutCancel(ctx), http.MethodDelete, "/v1/remote-setup/"+s.ID, nil, nil) }
	deadline := time.Now().Add(30 * time.Second)
	for {
		switch {
		case s.Status == apiv1.SetupDone:
			finish()
			return doneMsg{text: "Remote " + cleanLine(s.Name) + " is ready; test it with t"}
		case s.Status == apiv1.SetupFailed:
			finish()
			return doneMsg{err: fmt.Errorf("setting up %s failed: %s", s.Name, s.Error)}
		case s.Status == apiv1.SetupQuestion && s.Option != nil &&
			(s.Option.Name == "config_is_local" || s.Option.Name == "config_refresh_token"):
			var next apiv1.RemoteSetup
			if err := c.Do(ctx, http.MethodPost, "/v1/remote-setup/"+s.ID+"/answer", apiv1.RemoteSetupAnswer{State: s.State, Result: "true"}, &next); err != nil {
				finish()
				return doneMsg{err: err}
			}
			s = next
		case s.Status == apiv1.SetupAuthorizing && s.AuthURL == "":
			if time.Now().After(deadline) {
				finish()
				return doneMsg{err: errors.New("the authorization did not start")}
			}
			time.Sleep(200 * time.Millisecond)
			if err := c.Do(ctx, http.MethodGet, "/v1/remote-setup/"+s.ID, nil, &s); err != nil {
				finish()
				return doneMsg{err: err}
			}
		default:
			return doneMsg{next: m.setupForm(s)}
		}
	}
}

func (m *Model) cancelSetup(id string) tea.Cmd {
	c := m.c
	return m.action("Cancelling", func(ctx context.Context) doneMsg {
		_ = c.Do(ctx, http.MethodDelete, "/v1/remote-setup/"+id, nil, nil)
		return doneMsg{text: "Remote setup cancelled"}
	})
}

// setupForm asks the session's current question.
func (m *Model) setupForm(s apiv1.RemoteSetup) overlay {
	c := m.c
	then := func(fn func(ctx context.Context) (apiv1.RemoteSetup, error)) tea.Cmd {
		return m.action("Working", func(ctx context.Context) doneMsg {
			next, err := fn(ctx)
			if err != nil {
				_ = c.Do(context.WithoutCancel(ctx), http.MethodDelete, "/v1/remote-setup/"+s.ID, nil, nil)
				return doneMsg{err: err}
			}
			return m.advanceSetup(ctx, c, next)
		})
	}
	var f *form
	if s.Status == apiv1.SetupAuthorizing {
		intro := "1. Open this address in a browser on any device and sign in:\n\n" + cleanLine(s.AuthURL) +
			"\n\n2. After you approve access, the browser opens http://localhost:53682/... and shows an error page.\n" +
			"   That is expected.\n3. Copy the complete address from the browser's address bar and paste it below."
		f = newForm("Authorize "+cleanLine(s.Name), intro, []*field{newField("redirect", "Redirect URL", "", true, false)},
			func(v map[string]string) (tea.Cmd, string) {
				return then(func(ctx context.Context) (apiv1.RemoteSetup, error) {
					var next apiv1.RemoteSetup
					err := c.Do(ctx, http.MethodPost, "/v1/remote-setup/"+s.ID+"/oauth-redirect", apiv1.OAuthRedirect{URL: v["redirect"]}, &next)
					return next, err
				}), ""
			})
	} else {
		o := s.Option
		var intro strings.Builder
		intro.WriteString(cleanLine(o.Help))
		for i, e := range o.Examples {
			fmt.Fprintf(&intro, "\n  %d) %s  %s", i+1, cleanLine(e.Value), cleanLine(firstLine(e.Help)))
		}
		if s.Error != "" {
			intro.WriteString("\n\n" + errStyle.Render(cleanLine(s.Error)))
		}
		f = newForm("Set up "+cleanLine(s.Name), intro.String(), []*field{newField("answer", o.Name, o.Default, o.Required, o.IsPassword)},
			func(v map[string]string) (tea.Cmd, string) {
				ans := v["answer"]
				if n, err := strconv.Atoi(ans); err == nil && n >= 1 && n <= len(o.Examples) {
					ans = o.Examples[n-1].Value
				}
				return then(func(ctx context.Context) (apiv1.RemoteSetup, error) {
					var next apiv1.RemoteSetup
					err := c.Do(ctx, http.MethodPost, "/v1/remote-setup/"+s.ID+"/answer", apiv1.RemoteSetupAnswer{State: s.State, Result: ans}, &next)
					return next, err
				}), ""
			})
	}
	f.cancel = m.cancelSetup(s.ID)
	return f
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// writeKit stores a recovery kit in a new file readable only by root.
func writeKit(path, text string) error {
	f, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%s exists; choose another file", path)
	}
	if err != nil {
		return err
	}
	if _, err := f.WriteString(text); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// exportKit exports a kit to a file and returns the checksum dialog;
// after confirmation, then runs (it may be nil).
func (m *Model) exportKit(ctx context.Context, req apiv1.KitExportRequest, file string, then func(ctx context.Context) doneMsg) doneMsg {
	var kit apiv1.KitExportResponse
	if err := m.c.Do(ctx, http.MethodPost, "/v1/recovery-kit/export", req, &kit); err != nil {
		return doneMsg{err: err}
	}
	if err := writeKit(file, kit.Kit); err != nil {
		return doneMsg{err: err}
	}
	intro := fmt.Sprintf("The recovery kit was written to %s.\n\nIt holds the encryption keys: without it, encrypted offsite backups cannot be\n"+
		"restored if this host is lost. Store it offline now (password manager, printed copy).\n\nChecksum: %s",
		cleanLine(file), kit.Checksum)
	if !kit.Encrypted {
		intro += "\n\nThe kit is not passphrase-protected: keep the file secret."
	}
	c := m.c
	f := newForm("Confirm the recovery kit", intro, []*field{newField("checksum", "Type the checksum to confirm the kit is stored safely", "", true, false)},
		func(v map[string]string) (tea.Cmd, string) {
			if !strings.EqualFold(v["checksum"], kit.Checksum) {
				return nil, "the checksum does not match; check the kit file"
			}
			return m.action("Confirming", func(ctx context.Context) doneMsg {
				if err := c.Do(ctx, http.MethodPost, "/v1/recovery-kit/confirm", apiv1.KitConfirmRequest{Checksum: kit.Checksum}, nil); err != nil {
					return doneMsg{err: err}
				}
				if then != nil {
					return then(ctx)
				}
				return doneMsg{text: "Recovery kit confirmed"}
			}), ""
		})
	return doneMsg{next: f}
}

// kitForm exports a recovery kit for storages.
func (m *Model) kitForm(targets []apiv1.KitTarget, then func(ctx context.Context) doneMsg) overlay {
	name := "pve-rclone-backup"
	if len(targets) == 1 {
		name = targets[0].Storage
	}
	return newForm("Export a recovery kit", "", []*field{
		newField("file", "Kit file", "/root/"+name+"-recovery-kit.txt", true, false),
		newField("passphrase", "Passphrase (optional, encrypts the kit)", "", false, true),
		newField("token", "Include the remote's credentials? (y/N)", "n", false, false),
	}, func(v map[string]string) (tea.Cmd, string) {
		req := apiv1.KitExportRequest{Targets: targets, IncludeToken: yes(v["token"]), Passphrase: v["passphrase"]}
		return m.action("Exporting the recovery kit", func(ctx context.Context) doneMsg { return m.exportKit(ctx, req, v["file"], then) }), ""
	})
}

// storageInitForm creates a repository and adds the storage to PVE.
func (m *Model) storageInitForm() overlay {
	remote := ""
	if len(m.remotes) > 0 {
		remote = m.remotes[0].Name
	}
	source := ""
	if m.status != nil {
		source = strings.ToLower(m.status.Node)
	}
	return newForm("Initialize an offsite storage",
		"Creates the encrypted repository, exports its recovery kit and adds the storage to /etc/pve/storage.cfg.",
		[]*field{
			newField("storage", "Storage ID", "offsite", true, false),
			newField("remote", "Remote", remote, true, false),
			newField("path", "Repository path in the remote", "pve-backups", true, false),
			newField("source", "Name of this installation in the repository", source, true, false),
			newField("replicate", "Replicate from local storages (comma separated, empty for none)", "local", false, false),
			newField("encryption", "Encryption (crypt or none)", "crypt", true, false),
		}, func(v map[string]string) (tea.Cmd, string) {
			if v["encryption"] != "crypt" && v["encryption"] != "none" {
				return nil, "encryption must be crypt or none"
			}
			return m.action("Creating the repository", func(ctx context.Context) doneMsg { return m.initStorage(ctx, v) }), ""
		})
}

func (m *Model) initStorage(ctx context.Context, v map[string]string) doneMsg {
	id := v["storage"]
	var res apiv1.StorageInitResponse
	if err := m.c.Do(ctx, http.MethodPost, "/v1/storages/"+url.PathEscape(id)+"/init", apiv1.StorageInitRequest{
		Remote: v["remote"], Path: v["path"], Source: v["source"], Encryption: v["encryption"]}, &res); err != nil {
		return doneMsg{err: err}
	}
	add := func(ctx context.Context) doneMsg {
		args := []string{"create", "/storage", "--storage", id, "--type", "rclone-backup", "--rclone-remote", v["remote"],
			"--rclone-path", v["path"], "--rclone-source", v["source"], "--rclone-encryption", res.Encryption, "--content", "backup"}
		if v["replicate"] != "" {
			args = append(args, "--rclone-replicate-from", v["replicate"])
		}
		if out, err := m.opts.PVESH(ctx, args...); err != nil {
			return doneMsg{err: fmt.Errorf("pvesh: %w: %s", err, strings.TrimSpace(string(out)))}
		}
		t := tabStorages
		return doneMsg{text: "Storage " + cleanLine(id) + " added", tab: &t}
	}
	if !res.KitRequired {
		return add(ctx)
	}
	return doneMsg{next: m.kitForm([]apiv1.KitTarget{{Storage: id, Remote: v["remote"], Path: v["path"], Source: v["source"]}}, add)}
}

func (m *Model) fetchForm(b backupRow) overlay {
	return newForm("Fetch "+cleanLine(b.Volname),
		"Downloads and verifies the backup into a local backup storage, where PVE can restore it.",
		[]*field{
			newField("target", "Local backup storage", "local", true, false),
			newField("protect", "Protect it from the next local prune? (Y/n)", "y", false, false),
		}, func(v map[string]string) (tea.Cmd, string) {
			protect := v["protect"] == "" || yes(v["protect"])
			c := m.c
			return m.action("Starting the fetch", func(ctx context.Context) doneMsg {
				var j apiv1.Job
				if err := c.Do(ctx, http.MethodPost, backupPath(b)+"/fetch", apiv1.FetchRequest{TargetStorage: v["target"], Protect: &protect}, &j); err != nil {
					return doneMsg{err: err}
				}
				t := tabQueue
				return doneMsg{text: fmt.Sprintf("Fetch job %d started", j.ID), tab: &t}
			}), ""
		})
}

func (m *Model) restoreForm(b backupRow) overlay {
	mode := "stage"
	if b.VMType == "qemu" {
		mode = "stream"
	}
	return newForm("Restore "+cleanLine(b.Volname),
		"VMs stream into qmrestore without staging; containers are staged and restored with pct restore.",
		[]*field{
			newField("vmid", "VMID of the restored guest", "", true, false),
			newField("storage", "Storage for its disks (empty: as in the backup)", "", false, false),
			newField("mode", "Mode (stream or stage)", mode, true, false),
			newField("unique", "Assign new MAC addresses? (y/N)", "n", false, false),
		}, func(v map[string]string) (tea.Cmd, string) {
			vmid, err := strconv.Atoi(v["vmid"])
			if err != nil || vmid < 100 {
				return nil, "VMID must be a number of at least 100"
			}
			req := apiv1.RestoreRequest{Storage: b.Storage, Volname: b.Volname, Mode: v["mode"], TargetVMID: vmid,
				TargetStorage: v["storage"], Unique: yes(v["unique"])}
			c := m.c
			return m.action("Starting the restore", func(ctx context.Context) doneMsg {
				var j apiv1.Job
				if err := c.Do(ctx, http.MethodPost, "/v1/restores", req, &j); err != nil {
					return doneMsg{err: err}
				}
				t := tabQueue
				return doneMsg{text: fmt.Sprintf("Restore job %d started", j.ID), tab: &t}
			}), ""
		})
}
