package filesync

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Scan walks the folder and updates the index to match the disk: new and changed files are hashed and get a new version, and
// paths that have gone get a tombstone.  It returns how many entries changed.
func (f *Folder) Scan() (int, error) {
	f.op.Lock()
	defer f.op.Unlock()
	return f.scan()
}

func (f *Folder) scan() (int, error) {
	if err := f.marker(); err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	var unreadable []string
	var changed []FileInfo
	err := filepath.WalkDir(f.root, func(p string, d fs.DirEntry, werr error) error {
		rel, _ := filepath.Rel(f.root, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return werr
		}
		if rel == MetaDir {
			return filepath.SkipDir
		}
		if werr != nil { // a directory or file that can't be read: what is under it is not taken for deleted
			unreadable = append(unreadable, rel)
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if f.ignored(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			unreadable = append(unreadable, rel)
			return nil
		}
		var kind Kind
		switch {
		case info.IsDir():
			kind = KDir
		case info.Mode()&fs.ModeSymlink != 0:
			kind = KSymlink
		case info.Mode().IsRegular():
			kind = KFile
		default:
			return nil // sockets, devices, pipes: not synced
		}
		seen[rel] = true
		l, have := f.ix.get(rel)
		if have && f.unchanged(&l, kind, info, p) {
			return nil
		}
		nf, ok := f.describe(rel, p, kind, info)
		if !ok {
			unreadable = append(unreadable, rel)
			return nil
		}
		if have && !l.Deleted && sameContent(&l, &nf, f.cfg.IgnorePerms) {
			l.ModNs = nf.ModNs // only the time changed: remembered, but no new version for the others to fetch
			changed = append(changed, l)
			return nil
		}
		nf.Version = l.Version.Bump(f.cfg.Node)
		nf.By = f.cfg.Node
		changed = append(changed, nf)
		return nil
	})
	if err != nil {
		return 0, err
	}
	// what has gone
	var gone []FileInfo
	live := 0
	for _, l := range f.ix.snapshot() {
		if l.Deleted {
			continue
		}
		live++
		if seen[l.Name] || f.ignored(l.Name, l.Kind == KDir) || underAny(l.Name, unreadable) {
			continue
		}
		d := FileInfo{Name: l.Name, Kind: l.Kind, Deleted: true, ModNs: f.now().UnixNano(), Version: l.Version.Bump(f.cfg.Node), By: f.cfg.Node}
		gone = append(gone, d)
	}
	if len(gone) > 100 && len(gone)*2 > live {
		return 0, fmt.Errorf("%w (%d of %d)", ErrMassDelete, len(gone), live)
	}
	changed = append(changed, gone...)
	if err := f.ix.put(changed...); err != nil {
		return 0, err
	}
	return len(changed), nil
}

func underAny(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if name == p || strings.HasPrefix(name, p+"/") {
			return true
		}
	}
	return false
}

// unchanged says whether the disk still shows what the index holds for a name, going by size, time and mode: no reading of the file.
func (f *Folder) unchanged(l *FileInfo, kind Kind, info fs.FileInfo, p string) bool {
	if l.Deleted || l.Kind != kind {
		return false
	}
	perm := uint32(info.Mode().Perm())
	if !f.cfg.IgnorePerms && l.Mode != perm && kind != KSymlink {
		return false
	}
	switch kind {
	case KDir:
		return true
	case KSymlink:
		t, err := os.Readlink(p)
		return err == nil && t == l.Target
	}
	return l.Size == info.Size() && l.ModNs == info.ModTime().UnixNano()
}

// describe reads a path into an entry (without version or seq).  ok is false when it could not be read, or changed while it was.
func (f *Folder) describe(rel, p string, kind Kind, info fs.FileInfo) (FileInfo, bool) {
	fi := FileInfo{Name: rel, Kind: kind, ModNs: info.ModTime().UnixNano()}
	if !f.cfg.IgnorePerms {
		fi.Mode = uint32(info.Mode().Perm())
	}
	switch kind {
	case KSymlink:
		t, err := os.Readlink(p)
		if err != nil || t == "" {
			return fi, false
		}
		fi.Target, fi.ModNs, fi.Mode = t, 0, 0
	case KFile:
		if f.cfg.MaxFileSize > 0 && info.Size() > f.cfg.MaxFileSize {
			return fi, false
		}
		fh, err := os.Open(p)
		if err != nil {
			return fi, false
		}
		defer fh.Close()
		blocks, n, err := HashBlocks(fh, info.Size())
		if err != nil {
			return fi, false
		}
		after, err := fh.Stat()
		if err != nil || after.Size() != n || after.ModTime().UnixNano() != fi.ModNs {
			return fi, false // written to while it was read: left for the next scan
		}
		fi.Size, fi.Blocks = n, blocks
	}
	return fi, true
}
