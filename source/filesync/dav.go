package filesync

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Stat describes what is at a name on the disk right now ("" is the folder itself).  A link is described, not followed.  The index is not
// asked: it can be a scan behind a write that has just been made.
func (f *Folder) Stat(name string) (os.FileInfo, error) {
	p := f.root
	if name != "" {
		var err error
		if p, err = f.resolve(name); err != nil {
			return nil, err
		}
	} else if err := f.marker(); err != nil {
		return nil, err
	}
	st, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return nil, ErrNotFound
	}
	return st, err
}

// ReadDir lists a folder as it is on the disk, in name order, without what the folder keeps for itself and what the ignore patterns leave
// out.  "" is the folder itself.
func (f *Folder) ReadDir(name string) ([]os.FileInfo, error) {
	p := f.root
	if name != "" {
		var err error
		if p, err = f.resolve(name); err != nil {
			return nil, err
		}
	} else if err := f.marker(); err != nil {
		return nil, err
	}
	if st, err := os.Lstat(p); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	} else if !st.IsDir() {
		return nil, ErrNotFound
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	var out []os.FileInfo
	for _, e := range ents {
		n := e.Name()
		if name == "" && n == MetaDir {
			continue
		}
		rel := n
		if name != "" {
			rel = name + "/" + n
		}
		if f.ignored(rel, e.IsDir()) {
			continue
		}
		if st, err := os.Lstat(filepath.Join(p, n)); err == nil {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// TrashSize says how much the trash of this folder holds on this node: bytes and files.
func (f *Folder) TrashSize() (bytes int64, files int) {
	filepath.WalkDir(filepath.Join(f.root, MetaDir, "trash"), func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if st, err := d.Info(); err == nil {
			bytes += st.Size()
			files++
		}
		return nil
	})
	return
}

// EmptyTrash removes everything in the trash of this folder on this node, and says how many bytes and files that was.  The trash is not
// shared: the other nodes keep their own until its time is up or they are asked.
func (f *Folder) EmptyTrash() (bytes int64, files int, err error) {
	f.op.Lock()
	defer f.op.Unlock()
	bytes, files = f.TrashSize()
	td := filepath.Join(f.root, MetaDir, "trash")
	ents, err := os.ReadDir(td)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	for _, e := range ents {
		if err = os.RemoveAll(filepath.Join(td, e.Name())); err != nil {
			return
		}
	}
	return
}

// TrashEntry is one thing in the trash, shown like a file in a folder: a file or a folder that was deleted (or replaced), the moment it
// was put there (the name of its folder in the trash, "20060102-150405" in UTC), and its size (a folder: everything in it).
type TrashEntry struct {
	When  string `json:"when"`
	Name  string `json:"name"` // the last part
	Rel   string `json:"rel"`  // its path in the folder it was deleted from (and in the trash, under When)
	Kind  string `json:"kind"` // file or dir
	Size  int64  `json:"size"`
	Files int    `json:"files"` // a folder: how many files are in it
	Mod   int64  `json:"mod"`   // unix seconds: when it was put in the trash
}

func trashTime(when string) int64 {
	t, err := time.ParseInLocation("20060102-150405", when, time.UTC)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// TrashEntries lists the trash one level at a time.  With no when it is the top of every trash folder, the newest first (what was
// deleted, as it was named in the folder); with a when and a dir it is what is inside that folder of the trash.  At most limit.
func (f *Folder) TrashEntries(when, dir string, limit int) (out []TrashEntry, total int, err error) {
	td := filepath.Join(f.root, MetaDir, "trash")
	one := func(w, rel string) {
		ents, _ := os.ReadDir(filepath.Join(td, w, filepath.FromSlash(rel)))
		for _, e := range ents {
			total++
			if limit > 0 && len(out) >= limit {
				continue
			}
			te := TrashEntry{When: w, Name: e.Name(), Rel: path.Join(rel, e.Name()), Kind: "file", Mod: trashTime(w)}
			full := filepath.Join(td, w, filepath.FromSlash(te.Rel))
			if e.IsDir() {
				te.Kind = "dir"
				filepath.WalkDir(full, func(_ string, d os.DirEntry, err error) error {
					if err == nil && !d.IsDir() {
						te.Files++
						if st, err := d.Info(); err == nil {
							te.Size += st.Size()
						}
					}
					return nil
				})
			} else if st, err := e.Info(); err == nil {
				te.Size = st.Size()
			}
			out = append(out, te)
		}
	}
	if when == "" {
		whens, _ := os.ReadDir(td)
		for i := len(whens) - 1; i >= 0; i-- {
			if whens[i].IsDir() {
				one(whens[i].Name(), "")
			}
		}
		return out, total, nil
	}
	if !filepath.IsLocal(when) {
		return nil, 0, ErrBadName
	}
	rel := ""
	if dir != "" {
		if !filepath.IsLocal(dir) {
			return nil, 0, ErrBadName
		}
		rel = strings.Trim(path.Clean("/"+dir), "/")
	}
	if _, err := f.trashPath(when, dir); err != nil {
		return nil, 0, err
	}
	one(when, rel)
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Kind == "dir") != (out[j].Kind == "dir") {
			return out[i].Kind == "dir"
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, total, nil
}

// trashPath is the place in the trash of a name under a when, checked to be inside it.
func (f *Folder) trashPath(when, name string) (string, error) {
	if when == "" || strings.ContainsAny(when, "/\\") || when == "." || when == ".." {
		return "", ErrBadName
	}
	if !filepath.IsLocal(when) {
		return "", ErrBadName
	}
	base := filepath.Join(f.root, MetaDir, "trash", when)
	if name == "" {
		return base, nil
	}
	if !filepath.IsLocal(name) {
		return "", ErrBadName
	}
	p := filepath.Join(base, filepath.FromSlash(name))
	if rel, err := filepath.Rel(base, p); err != nil || strings.HasPrefix(rel, "..") {
		return "", ErrBadName
	}
	return p, nil
}

// RestoreTrash puts a file or a folder of the trash back where it was, file by file.  A name that is taken now gets "name restored"
// (then "restored 2", …) so nothing is replaced.  It returns how many files went back.  The next scan makes them new files of the
// folder, for every node.
func (f *Folder) RestoreTrash(when, name string) (int, error) {
	if !filepath.IsLocal(when) {
		return 0, ErrBadName
	}
	f.op.Lock()
	defer f.op.Unlock()
	src, err := f.trashPath(when, name)
	if err != nil {
		return 0, err
	}
	if src == filepath.Join(f.root, MetaDir, "trash", when) {
		return 0, ErrBadName
	}
	if _, err := os.Lstat(src); err != nil {
		return 0, ErrNotFound
	}
	var files []string
	filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(filepath.Join(f.root, MetaDir, "trash", when), p)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	n := 0
	for _, rel := range files {
		if err := f.restoreOne(when, rel); err != nil {
			return n, err
		}
		n++
	}
	f.pruneTrash(when, src)
	return n, nil
}

func (f *Folder) restoreOne(when, name string) error {
	if !filepath.IsLocal(when) || !filepath.IsLocal(name) {
		return ErrBadName
	}
	src := filepath.Join(f.root, MetaDir, "trash", when, filepath.FromSlash(name))
	dstName := name
	for i := 1; ; i++ {
		p, err := f.resolve(dstName)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(p); os.IsNotExist(err) {
			break
		}
		ext := path.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		if i == 1 {
			dstName = stem + " restored" + ext
		} else {
			dstName = fmt.Sprintf("%s restored %d%s", stem, i, ext)
		}
	}
	dst, err := f.resolve(dstName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

// pruneTrash removes the empty folders left at p and above it, up to and including the when folder.
func (f *Folder) pruneTrash(when, p string) {
	base := filepath.Join(f.root, MetaDir, "trash", when)
	// folders of the subtree first, deepest first
	var dirs []string
	filepath.WalkDir(p, func(q string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			dirs = append(dirs, q)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(dirs[i]) // only if empty
	}
	for q := filepath.Dir(p); strings.HasPrefix(q, base) && len(q) >= len(base); q = filepath.Dir(q) {
		if os.Remove(q) != nil {
			break
		}
	}
}

// DeleteTrash removes a file or a folder of the trash for good.
func (f *Folder) DeleteTrash(when, name string) (bytes int64, files int, err error) {
	if !filepath.IsLocal(when) {
		return 0, 0, ErrBadName
	}
	f.op.Lock()
	defer f.op.Unlock()
	src, err := f.trashPath(when, name)
	if err != nil {
		return 0, 0, err
	}
	if src == filepath.Join(f.root, MetaDir, "trash", when) {
		return 0, 0, ErrBadName
	}
	if _, err := os.Lstat(src); err != nil {
		return 0, 0, ErrNotFound
	}
	filepath.WalkDir(src, func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files++
			if st, err := d.Info(); err == nil {
				bytes += st.Size()
			}
		}
		return nil
	})
	if err := os.RemoveAll(src); err != nil {
		return 0, 0, err
	}
	f.pruneTrash(when, src)
	return bytes, files, nil
}
