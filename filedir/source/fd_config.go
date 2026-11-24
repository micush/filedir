package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// FilesConfig is the set of synced folders.  It is part of the shared settings, so a folder added on one node is on all of them.
type FilesConfig struct {
	Folders []FolderCfg `json:"folders,omitempty"`
	// MaxDiskPct: nothing is written or fetched into a folder that would take its disk past this percent full, so the files cannot fill the
	// host.  0 is the default, 80; 100 turns the limit off.
	MaxDiskPct int `json:"max_disk_pct,omitempty"`
	// Bandwidth limits in Mbit/s, per node, shared by all the transfers of the kind on it (0: none): what people upload, what is downloaded
	// from this node, and what this node sends to the other nodes to replicate.
	LimitUploadMbit   int `json:"limit_upload_mbit,omitempty"`
	LimitDownloadMbit int `json:"limit_download_mbit,omitempty"`
	LimitReplMbit     int `json:"limit_repl_mbit,omitempty"`
}

const fdMaxLimitMbit = 100000

const fdDefaultDiskPct = 80

// diskPct is the limit in force: the default when none is set, and 0 (none) when it is 100.
func (f FilesConfig) diskPct() int {
	switch {
	case f.MaxDiskPct <= 0:
		return fdDefaultDiskPct
	case f.MaxDiskPct >= 100:
		return 0
	}
	return f.MaxDiskPct
}

// FolderCfg is one synced folder.  The path is the same on every node.
type FolderCfg struct {
	ID          string   `json:"id"`                     // the name every node knows it by
	Path        string   `json:"path"`                   // where it lives on each node
	Ignore      []string `json:"ignore,omitempty"`       // patterns left out of the sync
	IgnorePerms bool     `json:"ignore_perms,omitempty"` // do not sync permission bits
	MaxFileMB   int      `json:"max_file_mb,omitempty"`  // larger files are left alone (0: no limit)
	TrashDays   int      `json:"trash_days,omitempty"`   // 0: 30
	Paused      bool     `json:"paused,omitempty"`
}

// fdRootID is the name of the root folder: the one that the Files page shows first, with every other synced folder listed inside it
// (wherever each is kept on the disk).  At most one, as every name is unique.
const fdRootID = "/"

// fdDBName is the name of the directory that holds a folder's index: its name, or a name no folder can have for the root.
func fdDBName(id string) string {
	if id == fdRootID {
		return "_root"
	}
	return id
}

var folderIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (f *FilesConfig) clone() *FilesConfig {
	if f == nil {
		return nil
	}
	c := &FilesConfig{MaxDiskPct: f.MaxDiskPct, LimitUploadMbit: f.LimitUploadMbit, LimitDownloadMbit: f.LimitDownloadMbit, LimitReplMbit: f.LimitReplMbit}
	for _, fc := range f.Folders {
		fc.Ignore = append([]string(nil), fc.Ignore...)
		if len(fc.Ignore) == 0 {
			fc.Ignore = nil
		}
		c.Folders = append(c.Folders, fc)
	}
	if len(c.Folders) == 0 && c.MaxDiskPct == 0 && c.LimitUploadMbit == 0 && c.LimitDownloadMbit == 0 && c.LimitReplMbit == 0 {
		return nil
	}
	return c
}

// Validate checks the folders.  A nil or empty set is fine: the node then syncs nothing.
func (f *FilesConfig) Validate() error {
	if f == nil {
		return nil
	}
	if f.MaxDiskPct < 0 || (f.MaxDiskPct > 0 && f.MaxDiskPct < 10) || f.MaxDiskPct > 100 {
		return fmt.Errorf("files: the disk limit must be from 10 to 100 percent (100: no limit)")
	}
	for _, v := range []int{f.LimitUploadMbit, f.LimitDownloadMbit, f.LimitReplMbit} {
		if v < 0 || v > fdMaxLimitMbit {
			return fmt.Errorf("files: a bandwidth limit is from 1 to %d Mbit/s (0: none)", fdMaxLimitMbit)
		}
	}
	ids, paths := map[string]bool{}, []string{}
	for i, fc := range f.Folders {
		if fc.ID != fdRootID && !folderIDRe.MatchString(fc.ID) {
			return fmt.Errorf("files: folder %d: the name %q must be letters, digits, . _ - (at most 64), or / for the root", i+1, fc.ID)
		}
		if ids[fc.ID] {
			return fmt.Errorf("files: the name %q is used twice", fc.ID)
		}
		ids[fc.ID] = true
		if strings.ContainsRune(fc.Path, 0) || !filepath.IsAbs(fc.Path) || filepath.Clean(fc.Path) == "/" {
			return fmt.Errorf("files: %s: the path %q must be an absolute path to a folder (not /)", fc.ID, fc.Path)
		}
		fc.Path = filepath.Clean(fc.Path)
		for _, p := range paths {
			if p == fc.Path || strings.HasPrefix(p+"/", fc.Path+"/") || strings.HasPrefix(fc.Path+"/", p+"/") {
				return fmt.Errorf("files: %s: the path %q overlaps another folder", fc.ID, fc.Path)
			}
		}
		paths = append(paths, fc.Path)
		if fc.MaxFileMB < 0 || fc.TrashDays < 0 || fc.TrashDays > 3650 {
			return fmt.Errorf("files: %s: a limit is out of range", fc.ID)
		}
		if len(fc.Ignore) > 200 {
			return fmt.Errorf("files: %s: at most 200 ignore patterns", fc.ID)
		}
	}
	return nil
}
