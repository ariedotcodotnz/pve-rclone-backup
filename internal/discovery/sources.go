// SPDX-License-Identifier: AGPL-3.0-or-later

package discovery

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
	"github.com/ariedotcodotnz/pve-rclone-backup/internal/pve/storagecfg"
)

// Source is a local backup storage that offsite storages replicate from.
type Source struct {
	StoreID string
	Type    string
	Dir     string // dump directory
	Shared  bool
	Targets []*config.Storage
	// Problem explains why the source cannot be used right now (empty if
	// usable).
	Problem string
}

// networkTypes are storage types whose path must be a mount point.
var networkTypes = []string{"nfs", "cifs", "cephfs", "glusterfs"}

// sharedTypes are inherently shared between nodes.
var sharedTypes = []string{"nfs", "cifs", "cephfs", "glusterfs"}

// ResolveSources maps the replication sources of the given offsite
// storages to dump directories on this node. Sources configured for other
// nodes only are left out; unusable ones carry a Problem.
func ResolveSources(cfg *storagecfg.Config, targets []*config.Storage, node string, mounted func(string) bool) []*Source {
	byID := map[string]*Source{}
	var order []string
	for _, t := range targets {
		for _, id := range t.ReplicateFrom {
			src := byID[id]
			if src == nil {
				src = resolve(cfg, id, node, mounted)
				if src == nil {
					continue
				}
				byID[id] = src
				order = append(order, id)
			}
			src.Targets = append(src.Targets, t)
		}
	}
	out := make([]*Source, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}

func resolve(cfg *storagecfg.Config, id, node string, mounted func(string) bool) *Source {
	sec := cfg.Get(id)
	if sec == nil {
		return &Source{StoreID: id, Problem: "storage does not exist"}
	}
	if nodes := sec.List("nodes"); len(nodes) > 0 && !slices.Contains(nodes, node) {
		return nil
	}
	src := &Source{StoreID: id, Type: sec.Type, Shared: slices.Contains(sharedTypes, sec.Type) || sec.Bool("shared")}
	if sec.Bool("disable") {
		src.Problem = "storage is disabled"
		return src
	}
	base, ok := sec.Get("path")
	if !ok || !filepath.IsAbs(base) {
		src.Problem = "storage has no absolute path"
		return src
	}
	base = filepath.Clean(base)
	sub, err := backupSubdir(sec)
	if err != nil {
		src.Problem = err.Error()
		return src
	}
	src.Dir = filepath.Join(base, sub)

	mountPoint := ""
	if slices.Contains(networkTypes, sec.Type) {
		mountPoint = base
	} else if v, ok := sec.Get("is_mountpoint"); ok {
		switch strings.ToLower(v) {
		case "", "0", "no", "off", "false":
		case "1", "yes", "on", "true":
			mountPoint = base
		default:
			mountPoint = filepath.Clean(v)
		}
	}
	if mountPoint != "" && !mounted(mountPoint) {
		src.Problem = fmt.Sprintf("%s is not mounted", mountPoint)
		return src
	}
	if fi, err := os.Stat(src.Dir); err != nil || !fi.IsDir() { //nolint:gosec // storage paths come from root-owned storage.cfg
		src.Problem = fmt.Sprintf("dump directory %s does not exist", src.Dir)
	}
	return src
}

// backupSubdir returns the backup directory relative to the storage path:
// "dump", or the content-dirs override for backups.
func backupSubdir(sec *storagecfg.Section) (string, error) {
	sub := "dump"
	if v, ok := sec.Get("content-dirs"); ok {
		for _, part := range strings.Split(v, ",") {
			k, dir, found := strings.Cut(strings.TrimSpace(part), "=")
			if found && k == "backup" {
				sub = strings.TrimPrefix(dir, "/")
			}
		}
	}
	clean := filepath.Clean(sub)
	if sub == "" || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("invalid backup directory override %q", sub)
	}
	return clean, nil
}

// Mounted reports whether path is a mount point, from /proc/self/mountinfo.
func Mounted(path string) bool {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	path = filepath.Clean(path)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 && unescapeMount(fields[4]) == path {
			return true
		}
	}
	return false
}

// unescapeMount decodes the octal escapes (\040 for space) of mountinfo.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
