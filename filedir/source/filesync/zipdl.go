package filesync

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// A download of folders and files as one zip, written to w while it is read: nothing is built up in memory or on disk, so a folder of
// any size can go to a slow reader.  It reads this node's disk the same way the file manager does: names go through ValidName, a link
// in the way is refused, links are not followed (they are left out), the metadata folder and anything the ignore patterns leave out are
// not in the archive, and neither is anything that is not a plain file or folder.

// MaxZipItems is the most names one archive may be asked for.
const MaxZipItems = 500

// Ignored says whether a name is left out of the folder by its ignore patterns (the metadata folder is always left out).
func (f *Folder) Ignored(name string, isDir bool) bool {
	if name == MetaDir || strings.HasPrefix(name, MetaDir+"/") {
		return true
	}
	return f.ignored(name, isDir)
}

// ZipStats says what went into an archive.
type ZipStats struct {
	Files   int
	Folders int
	Bytes   int64
	Skipped int // links, special files and files that could not be read
}

// WriteZip writes the named files and folders (a folder with everything under it) to w as a zip.  A name of "" is the whole folder, which
// goes in under rootName.  Each name goes in under its last part; two with the same last part are told apart by a number.
func (f *Folder) WriteZip(w io.Writer, names []string, rootName string) (ZipStats, error) {
	var st ZipStats
	if len(names) == 0 || len(names) > MaxZipItems {
		return st, fmt.Errorf("%w: between 1 and %d names", ErrBadName, MaxZipItems)
	}
	if err := f.marker(); err != nil {
		return st, err
	}
	type item struct{ name, p, top string }
	var items []item
	used := map[string]bool{}
	for _, n := range names {
		var p, top string
		if n == "" {
			p, top = f.root, cleanTop(rootName)
		} else {
			var err error
			if p, err = f.resolve(n); err != nil {
				return st, err
			}
			top = path.Base(n)
		}
		base, ext := top, ""
		if e := path.Ext(top); e != "" && e != top {
			base, ext = strings.TrimSuffix(top, e), e
		}
		for i := 2; used[strings.ToLower(top)]; i++ {
			top = fmt.Sprintf("%s (%d)%s", base, i, ext)
		}
		used[strings.ToLower(top)] = true
		items = append(items, item{n, p, top})
	}
	zw := zip.NewWriter(w)
	for _, it := range items {
		li, err := os.Lstat(it.p)
		if err != nil {
			if os.IsNotExist(err) {
				return st, ErrNotFound
			}
			return st, err
		}
		if li.Mode()&os.ModeSymlink != 0 {
			st.Skipped++
			continue
		}
		if !li.IsDir() {
			if err := f.zipFile(zw, it.p, it.top, &st); err != nil {
				return st, err
			}
			continue
		}
		err = filepath.WalkDir(it.p, func(p string, d fs.DirEntry, werr error) error {
			rel, _ := filepath.Rel(f.root, p)
			rel = filepath.ToSlash(rel)
			if werr != nil { // not readable: left out, the rest still goes in
				st.Skipped++
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			inner, _ := filepath.Rel(it.p, p)
			name := it.top
			if inner != "." {
				name = it.top + "/" + filepath.ToSlash(inner)
			}
			if strings.ContainsRune(inner, '\\') || strings.ContainsRune(inner, 0) { // an extractor on another system could read it as a path
				st.Skipped++
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if rel != "." && f.Ignored(rel, d.IsDir()) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			switch {
			case d.IsDir():
				info, err := d.Info()
				if err != nil {
					st.Skipped++
					return filepath.SkipDir
				}
				h := &zip.FileHeader{Name: name + "/", Method: zip.Store}
				h.Modified = info.ModTime()
				if _, err := zw.CreateHeader(h); err != nil {
					return err
				}
				st.Folders++
			case d.Type().IsRegular():
				return f.zipFile(zw, p, name, &st)
			default: // a link, a socket, a device
				st.Skipped++
			}
			return nil
		})
		if err != nil {
			return st, err
		}
	}
	return st, zw.Close()
}

func cleanTop(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 {
			return '_'
		}
		return r
	}, s)
	if s == "" || s == "." || s == ".." {
		return "files"
	}
	return s
}

// zipFile adds one plain file.  A file that cannot be opened (or has become a link) is counted and left out; a failure to write to the
// archive ends it.
func (f *Folder) zipFile(zw *zip.Writer, p, name string, st *ZipStats) error {
	fh, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		st.Skipped++
		return nil
	}
	defer fh.Close()
	info, err := fh.Stat()
	if err != nil || !info.Mode().IsRegular() {
		st.Skipped++
		return nil
	}
	h, err := zip.FileInfoHeader(info)
	if err != nil {
		st.Skipped++
		return nil
	}
	h.Name, h.Method = name, zip.Deflate
	if packed(name) {
		h.Method = zip.Store
	}
	zf, err := zw.CreateHeader(h)
	if err != nil {
		return err
	}
	// what was there when the file was opened: a file that grows while it is read is cut at that size, so the archive agrees with itself
	n, err := io.Copy(zf, io.LimitReader(fh, info.Size()))
	st.Bytes += n
	if err != nil {
		return err
	}
	st.Files++
	return nil
}

// packed says whether a name is of a kind that is compressed already, so deflating it again only costs time.
func packed(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".zip", ".gz", ".tgz", ".bz2", ".xz", ".zst", ".7z", ".rar", ".jpg", ".jpeg", ".png", ".gif", ".webp", ".mp3", ".mp4", ".m4a", ".mkv", ".mov", ".avi", ".webm", ".ogg", ".flac", ".pdf", ".docx", ".xlsx", ".pptx", ".jar", ".apk":
		return true
	}
	return false
}
