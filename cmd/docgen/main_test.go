// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestEveryCommandIsDocumented(t *testing.T) {
	root := commandTree()
	pages := genMarkdown(root)
	var walk func(top, c *cobra.Command)
	walk = func(top, c *cobra.Command) {
		for _, s := range subcommands(c) {
			if top == nil {
				page, ok := pages[s.Name()+".md"]
				if !ok {
					t.Errorf("no page for %s", s.CommandPath())
					continue
				}
				if !bytes.Contains(pages["index.md"], []byte("("+s.Name()+".md)")) {
					t.Errorf("index does not link %s", s.Name())
				}
				if !bytes.Contains(page, []byte("# "+s.CommandPath()+"\n")) {
					t.Errorf("%s.md has no title", s.Name())
				}
				walk(s, s)
				continue
			}
			if !bytes.Contains(pages[top.Name()+".md"], []byte("\n## "+relativePath(s)+"\n")) {
				t.Errorf("%s.md does not document %s", top.Name(), s.CommandPath())
			}
			walk(top, s)
		}
	}
	walk(nil, root)
	if _, ok := pages["completion.md"]; !ok {
		t.Error("the completion command cobra adds at run time is not documented")
	}
}

func TestHiddenCommandsAndHelpAreLeftOut(t *testing.T) {
	for name, page := range genMarkdown(commandTree()) {
		for _, unwanted := range []string{"## hook notify", "`--help`", "`-h, --help`"} {
			if bytes.Contains(page, []byte(unwanted)) {
				t.Errorf("%s contains %q", name, unwanted)
			}
		}
	}
}

func TestGenerationIsDeterministic(t *testing.T) {
	a, b := genMarkdown(commandTree()), genMarkdown(commandTree())
	for name := range a {
		if !bytes.Equal(a[name], b[name]) {
			t.Errorf("%s: output not deterministic", name)
		}
	}
}

func TestEscape(t *testing.T) {
	if got := escape("restore <volid> & keep *all*"); got != `restore &lt;volid&gt; &amp; keep \*all\*` {
		t.Fatalf("escape = %s", got)
	}
	if got := cell("a | b"); got != `a \| b` {
		t.Fatalf("cell = %s", got)
	}
}

func TestManPages(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SOURCE_DATE_EPOCH", "1790000000")
	if err := genMan(commandTree(), dir); err != nil {
		t.Fatal(err)
	}
	page, err := os.ReadFile(filepath.Join(dir, "pve-rclone-backup-remote-add.1"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "--client-secret-file") || !strings.Contains(string(page), "Sep 2026") {
		t.Fatalf("unexpected manual page:\n%s", page)
	}
	// Placeholders survive (md2man drops anything that looks like an HTML
	// tag), and --help is left out.
	if !strings.Contains(string(page), "remote add <remote>") || strings.Contains(string(page), "help for add") {
		t.Fatalf("manual page lost a placeholder or lists --help:\n%s", page)
	}
	if _, err := os.Stat(filepath.Join(dir, "pve-rclone-backup-hook-notify.1")); err == nil {
		t.Error("hidden command has a manual page")
	}
}
