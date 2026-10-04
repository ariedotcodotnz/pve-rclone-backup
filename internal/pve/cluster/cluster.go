// SPDX-License-Identifier: AGPL-3.0-or-later

// Package cluster reads cluster state that pmxcfs exposes as files: the
// guest list (.vmlist), node membership (.members) and guest
// configurations.
package cluster

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/ariedotcodotnz/pve-rclone-backup/internal/config"
)

// Guest is an entry of .vmlist.
type Guest struct {
	VMID int
	Node string
	Type string // qemu or lxc
}

// VMList reads the cluster-wide guest list.
func VMList(pveDir string) (map[int]Guest, error) {
	raw, err := os.ReadFile(filepath.Join(pveDir, ".vmlist")) //nolint:gosec // fixed path below the PVE directory
	if err != nil {
		return nil, err
	}
	var doc struct {
		IDs map[string]struct {
			Node string `json:"node"`
			Type string `json:"type"`
		} `json:"ids"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("cluster: parse .vmlist: %w", err)
	}
	out := make(map[int]Guest, len(doc.IDs))
	for id, g := range doc.IDs {
		vmid, err := strconv.Atoi(id)
		if err != nil || !config.ValidVMID(id) || !config.ValidNodeName(g.Node) {
			continue
		}
		switch g.Type {
		case "qemu", "lxc":
			out[vmid] = Guest{VMID: vmid, Node: g.Node, Type: g.Type}
		}
	}
	return out, nil
}

// Members describes the cluster membership seen by this node.
type Members struct {
	Node    string   // this node
	Cluster string   // cluster name (empty when standalone)
	Online  []string // online nodes, sorted (just this node when standalone)
}

// ReadMembers reads .members. A standalone node lists only itself.
func ReadMembers(pveDir, node string) (Members, error) {
	m := Members{Node: node, Online: []string{node}}
	raw, err := os.ReadFile(filepath.Join(pveDir, ".members")) //nolint:gosec // fixed path below the PVE directory
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	var doc struct {
		NodeName string `json:"nodename"`
		Cluster  struct {
			Name string `json:"name"`
		} `json:"cluster"`
		NodeList map[string]struct {
			Online int `json:"online"`
		} `json:"nodelist"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return m, fmt.Errorf("cluster: parse .members: %w", err)
	}
	m.Cluster = doc.Cluster.Name
	if len(doc.NodeList) == 0 {
		return m, nil
	}
	m.Online = nil
	for name, n := range doc.NodeList {
		if n.Online == 1 || name == node {
			m.Online = append(m.Online, name)
		}
	}
	slices.Sort(m.Online)
	return m, nil
}

// Primary is the node that runs cluster-wide scheduled work: the
// lowest-named online node.
func (m Members) Primary() string { return m.Online[0] }

// GuestConfigPath returns the configuration file of a guest.
func GuestConfigPath(pveDir string, g Guest) string {
	dir := "qemu-server"
	if g.Type == "lxc" {
		dir = "lxc"
	}
	return filepath.Join(pveDir, "nodes", g.Node, dir, strconv.Itoa(g.VMID)+".conf")
}

// GuestTags returns the tags of a guest's current configuration (not of
// its snapshots).
func GuestTags(pveDir string, g Guest) ([]string, error) {
	raw, err := os.ReadFile(GuestConfigPath(pveDir, g))
	if err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "[") {
			break // snapshot or pending sections follow the current config
		}
		if v, ok := strings.CutPrefix(line, "tags:"); ok {
			return SplitTags(v), nil
		}
	}
	return nil, sc.Err()
}

// SplitTags splits a PVE tag list (separated by ';', ',' or spaces).
func SplitTags(v string) []string {
	var out []string
	for _, t := range strings.FieldsFunc(v, func(r rune) bool { return r == ';' || r == ',' || r == ' ' || r == '\t' }) {
		if config.ValidTag(t) {
			out = append(out, t)
		}
	}
	return out
}
