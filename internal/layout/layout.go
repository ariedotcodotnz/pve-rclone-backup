// SPDX-License-Identifier: AGPL-3.0-or-later

// Package layout defines where things live inside a repository and how
// offsite backups are named in Proxmox VE. It is the only place that
// builds repository paths; every component is validated, so no path is
// ever derived from untrusted text.
//
// Below a generation root (gN/, encrypted with rclone crypt):
//
//	v1/<source>/source.json
//	v1/<source>/<qemu|lxc>/<vmid>/<YYYY_MM_DD-HH_MM_SS>[.<n>]/
//	    part.000000 … part.NNNNNN   archive bytes in fixed-size segments
//	    vzdump.log                  task log, if any
//	    meta.json                   mutable: notes, protected, tombstone
//	    manifest.json               immutable commit record, written last
package layout

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Fixed names.
const (
	Version      = "v1"
	ManifestName = "manifest.json"
	MetaName     = "meta.json"
	LogName      = "vzdump.log"
	SourceDoc    = "source.json"
	MarkerName   = "pve-rclone-backup.repo.json" // at the repository base, plaintext
	KeyCheckName = "keycheck"                    // at each generation root
	partPrefix   = "part."
)

var (
	sourceRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	tsRe     = regexp.MustCompile(`^[0-9]{4}_[0-9]{2}_[0-9]{2}-[0-9]{2}_[0-9]{2}_[0-9]{2}$`)
	partRe   = regexp.MustCompile(`^part\.([0-9]{6,})$`)
	// vzdump archive names (PVE::Storage::archive_info), optionally with
	// our collision suffix before the extension.
	archiveRe = regexp.MustCompile(`^vzdump-(qemu|lxc|openvz)-([1-9][0-9]{2,8})-` +
		`([0-9]{4}_[0-9]{2}_[0-9]{2}-[0-9]{2}_[0-9]{2}_[0-9]{2})(?:\.([1-9][0-9]{0,3}))?` +
		`\.(tgz|(?:vma|tar)(?:\.(?:gz|lzo|zst|bz2))?)$`)
)

// ValidSource reports whether s is a valid source name.
func ValidSource(s string) bool { return sourceRe.MatchString(s) }

// BackupID identifies a backup within a repository.
type BackupID struct {
	Source    string
	VMType    string // qemu or lxc
	VMID      int
	TSLabel   string // vzdump timestamp, local time: YYYY_MM_DD-HH_MM_SS
	Collision int    // 0, or n for the n-th different archive with the same name
}

// Validate checks every component.
func (id BackupID) Validate() error {
	switch {
	case !sourceRe.MatchString(id.Source):
		return fmt.Errorf("layout: invalid source %q", id.Source)
	case id.VMType != "qemu" && id.VMType != "lxc":
		return fmt.Errorf("layout: invalid guest type %q", id.VMType)
	case id.VMID < 100 || id.VMID > 999999999:
		return fmt.Errorf("layout: invalid guest ID %d", id.VMID)
	case !tsRe.MatchString(id.TSLabel):
		return fmt.Errorf("layout: invalid timestamp %q", id.TSLabel)
	case id.Collision < 0 || id.Collision > 9999:
		return fmt.Errorf("layout: invalid collision index %d", id.Collision)
	}
	return nil
}

func (id BackupID) tsDir() string {
	if id.Collision > 0 {
		return id.TSLabel + "." + strconv.Itoa(id.Collision)
	}
	return id.TSLabel
}

// Dir is the backup directory relative to a generation root.
func (id BackupID) Dir() string {
	return path.Join(SourceDir(id.Source), id.VMType, strconv.Itoa(id.VMID), id.tsDir())
}

// Path returns a file inside the backup directory.
func (id BackupID) Path(name string) string { return path.Join(id.Dir(), name) }

// Volname is the PVE volume name: backup/<original archive name>, with the
// collision suffix inserted before the extension when needed.
func (id BackupID) Volname(ext string) string {
	return "backup/vzdump-" + id.VMType + "-" + strconv.Itoa(id.VMID) + "-" + id.tsDir() + "." + ext
}

func (id BackupID) String() string { return id.Dir() }

// SourceDir is a source's directory relative to a generation root.
func SourceDir(source string) string { return path.Join(Version, source) }

// PartName returns the name of segment i.
func PartName(i int) string { return fmt.Sprintf("part.%06d", i) }

// ParsePart returns the index of a segment name.
func ParsePart(name string) (int, bool) {
	m := partRe.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

// ParseDir parses a backup directory path (relative to a generation root).
func ParseDir(dir string) (BackupID, error) {
	parts := strings.Split(dir, "/")
	if len(parts) != 5 || parts[0] != Version {
		return BackupID{}, fmt.Errorf("layout: %q is not a backup directory", dir)
	}
	vmid, err := strconv.Atoi(parts[3])
	if err != nil || strconv.Itoa(vmid) != parts[3] {
		return BackupID{}, fmt.Errorf("layout: invalid guest ID in %q", dir)
	}
	id := BackupID{Source: parts[1], VMType: parts[2], VMID: vmid}
	ts, n, found := strings.Cut(parts[4], ".")
	id.TSLabel = ts
	if found {
		c, err := strconv.Atoi(n)
		if err != nil || c < 1 || strconv.Itoa(c) != n {
			return BackupID{}, fmt.Errorf("layout: invalid collision suffix in %q", dir)
		}
		id.Collision = c
	}
	return id, id.Validate()
}

// Archive is a parsed vzdump archive name.
type Archive struct {
	VMType      string // qemu, lxc (openvz archives are treated as lxc)
	VMID        int
	TSLabel     string
	Collision   int
	Ext         string // e.g. vma.zst, tar.zst, tgz
	Format      string // vma or tar
	Compression string // zst, gz, lzo, bz2 or empty
}

// ParseArchiveName parses a vzdump archive file name.
func ParseArchiveName(name string) (Archive, error) {
	m := archiveRe.FindStringSubmatch(name)
	if m == nil {
		return Archive{}, fmt.Errorf("layout: %q is not a vzdump archive name", name)
	}
	vmid, _ := strconv.Atoi(m[2])
	a := Archive{VMType: m[1], VMID: vmid, TSLabel: m[3], Ext: m[5]}
	if a.VMType == "openvz" {
		a.VMType = "lxc"
	}
	if m[4] != "" {
		a.Collision, _ = strconv.Atoi(m[4])
	}
	if a.Ext == "tgz" {
		a.Format, a.Compression = "tar", "gz"
	} else {
		a.Format, a.Compression, _ = strings.Cut(a.Ext, ".")
	}
	return a, nil
}

// ID returns the backup identity of an archive within a source.
func (a Archive) ID(source string) BackupID {
	return BackupID{Source: source, VMType: a.VMType, VMID: a.VMID, TSLabel: a.TSLabel, Collision: a.Collision}
}

// ParseVolname parses a PVE volume name of an offsite backup.
func ParseVolname(volname string) (Archive, error) {
	name, ok := strings.CutPrefix(volname, "backup/")
	if !ok {
		return Archive{}, fmt.Errorf("layout: %q is not a backup volume", volname)
	}
	return ParseArchiveName(name)
}
