package filesync

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// What a person does in the file manager: read, write, make a folder, rename, delete.  These work on the folder's disk, the same way the
// scan sees it; the next scan gives the change a version and the other nodes take it.  A name goes through the same checks as one from
// a peer, and a link in the way is refused.

var (
	ErrExists   = errors.New("filesync: that name is already there")
	ErrNotFound = errors.New("filesync: no such file or folder")
	ErrTooBig   = errors.New("filesync: the file is larger than this folder allows")
	ErrIsDir    = errors.New("filesync: that is a folder")
)

// resolve gives the path of a name inside the folder ("" is the folder itself, which only a few operations accept).
func (f *Folder) resolve(name string) (string, error) {
	if err := f.marker(); err != nil {
		return "", err
	}
	if err := ValidName(name); err != nil {
		return "", err
	}
	if !filepath.IsLocal(name) {
		return "", fmt.Errorf("%w: %s is outside the folder", ErrBadName, name)
	}
	if f.ignored(name, false) || f.ignored(name, true) {
		return "", fmt.Errorf("%w: %s is left out of the sync", ErrBadName, name)
	}
	return f.full(name)
}

// OpenRead opens a regular file for reading.  A link is not followed.
func (f *Folder) OpenRead(name string) (*os.File, os.FileInfo, error) {
	p, err := f.resolve(name)
	if err != nil {
		return nil, nil, err
	}
	fh, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}
	st, err := fh.Stat()
	if err != nil {
		fh.Close()
		return nil, nil, err
	}
	if st.IsDir() {
		fh.Close()
		return nil, nil, ErrIsDir
	}
	if !st.Mode().IsRegular() {
		fh.Close()
		return nil, nil, fmt.Errorf("%w: %s is not a plain file", ErrBadName, name)
	}
	return fh, st, nil
}

// Mkdir makes a folder (and the ones above it that are missing).
func (f *Folder) Mkdir(name string) error {
	p, err := f.resolve(name)
	if err != nil {
		return err
	}
	if st, err := os.Lstat(p); err == nil {
		if st.IsDir() {
			return nil
		}
		return ErrExists
	}
	return os.MkdirAll(p, 0o755)
}

// WriteFile writes a file whole, from r: to a temporary file first, then moved into place, so no one sees a half written file.  An
// existing file is replaced (the old one goes to the trash); a folder of that name is an error.
func (f *Folder) WriteFile(name string, r io.Reader) (int64, error) { return f.writeFile(name, r, -1) }

// WriteFileHashed is WriteFile for a file whose size is known: its blocks are hashed as it is written and it goes into the index at once,
// so the next scan does not have to read it all again (minutes for a big file, with the file not showing until it is done).
func (f *Folder) WriteFileHashed(name string, r io.Reader, size int64) (int64, error) {
	return f.writeFile(name, r, size)
}

// writeFile writes the file; with size >= 0 it also records it in the index.
func (f *Folder) writeFile(name string, r io.Reader, size int64) (int64, error) {
	p, err := f.resolve(name)
	if err != nil {
		return 0, err
	}
	if st, err := os.Lstat(p); err == nil && st.IsDir() {
		return 0, ErrIsDir
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return 0, err
	}
	if err := f.room(0); err != nil {
		return 0, err
	}
	tdir := filepath.Join(f.root, MetaDir, "tmp")
	if err := os.MkdirAll(tdir, 0o700); err != nil {
		return 0, err
	}
	tf, err := os.CreateTemp(tdir, "up-*")
	if err != nil {
		return 0, err
	}
	tmp := tf.Name()
	defer os.Remove(tmp) // gone already once renamed
	src := io.Reader(&guard{r: r, f: f})
	if f.cfg.MaxFileSize > 0 {
		src = io.LimitReader(src, f.cfg.MaxFileSize+1)
	}
	var n int64
	var blocks []Block
	if size >= 0 {
		blocks, n, err = HashBlocks(io.TeeReader(src, tf), size)
	} else {
		n, err = io.Copy(tf, src)
	}
	if err == nil && f.cfg.MaxFileSize > 0 && n > f.cfg.MaxFileSize {
		err = ErrTooBig
	}
	if err == nil {
		err = tf.Sync()
	}
	if cerr := tf.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	return n, f.install(name, p, tmp, n, blocks, size >= 0)
}

// install puts a finished temporary file in place of name: what was there is kept in the trash, and the file is recorded in the index when
// its blocks are known.
func (f *Folder) install(name, p, tmp string, n int64, blocks []Block, record bool) error {
	mode := os.FileMode(0o644)
	if st, err := os.Lstat(p); err == nil && st.Mode().IsRegular() {
		mode = st.Mode().Perm()
		if err := f.toTrashCopy(p, name); err != nil {
			return err
		}
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	syncDir(filepath.Dir(p))
	if record {
		f.recordWritten(name, p, n, blocks)
	}
	return nil
}

// ── uploads that can be taken up again ──
// A big upload from a browser arrives in pieces, into a temporary file that is kept when the connection drops: the browser asks how much
// is there and sends the rest.  The file goes in place only when all of it is here.

// ErrOffset is returned when a piece does not start where the file here ends.
var ErrOffset = errors.New("filesync: the piece does not start where the upload ends")

func (f *Folder) resumePath(id string) (string, error) {
	if len(id) < 8 || len(id) > 64 || strings.Trim(id, "0123456789abcdef") != "" {
		return "", ErrBadName
	}
	d := filepath.Join(f.root, MetaDir, "tmp")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(d, "resume-"+id), nil
}

// ResumeSize says how much of an upload is here (0 if it has not started).
func (f *Folder) ResumeSize(id string) (int64, error) {
	p, err := f.resumePath(id)
	if err != nil {
		return 0, err
	}
	st, err := os.Stat(p)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// ResumeAppend adds the next piece of an upload.  It must start at off, the size here; if it does not, ErrOffset comes back with the size
// that is here.  A piece that breaks off half way is kept as far as it came: the next ask says how far.
func (f *Folder) ResumeAppend(id string, off int64, r io.Reader) (int64, error) {
	p, err := f.resumePath(id)
	if err != nil {
		return 0, err
	}
	fh, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return 0, err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return 0, err
	}
	if st.Size() != off {
		return st.Size(), ErrOffset
	}
	if err := f.room(0); err != nil {
		return off, err
	}
	if _, err := fh.Seek(off, io.SeekStart); err != nil {
		return off, err
	}
	src := io.Reader(&guard{r: r, f: f})
	if f.cfg.MaxFileSize > 0 {
		src = io.LimitReader(src, f.cfg.MaxFileSize+1-off)
	}
	n, err := io.Copy(fh, src)
	if err == nil && f.cfg.MaxFileSize > 0 && off+n > f.cfg.MaxFileSize {
		err = ErrTooBig
	}
	return off + n, err
}

// ResumeFinish puts a complete upload in place as name, and records it in the index.  size is what the browser says the file is.
func (f *Folder) ResumeFinish(name, id string, size int64) (int64, error) {
	tmp, err := f.resumePath(id)
	if err != nil {
		return 0, err
	}
	p, err := f.resolve(name)
	if err != nil {
		return 0, err
	}
	if st, err := os.Lstat(p); err == nil && st.IsDir() {
		return 0, ErrIsDir
	}
	fh, err := os.Open(tmp)
	if err != nil {
		return 0, ErrNotFound
	}
	st, err := fh.Stat()
	if err != nil || st.Size() != size {
		fh.Close()
		return 0, ErrOffset
	}
	blocks, n, err := HashBlocks(fh, size)
	fh.Close()
	if err == nil && f.cfg.MaxFileSize > 0 && n > f.cfg.MaxFileSize {
		err = ErrTooBig
	}
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return 0, err
	}
	if sf, err := os.OpenFile(tmp, os.O_RDWR, 0); err == nil { // on disk before it takes the place of the old one
		sf.Sync()
		sf.Close()
	}
	return n, f.install(name, p, tmp, n, blocks, true)
}

// ResumeDrop forgets an upload.
func (f *Folder) ResumeDrop(id string) {
	if p, err := f.resumePath(id); err == nil {
		os.Remove(p)
	}
}

// CheckRoom says whether size more bytes fit under the disk limit.
func (f *Folder) CheckRoom(size int64) error { return f.room(size) }

// recordWritten puts a file this node has just written into the index, as a scan would have found it.  If anything is off (it changed
// again already, or the entry is not well formed) it does nothing, and the next scan reads the file as before.
func (f *Folder) recordWritten(name, p string, n int64, blocks []Block) {
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() != n {
		return
	}
	nf := FileInfo{Name: name, Kind: KFile, ModNs: st.ModTime().UnixNano(), Size: n, Blocks: blocks}
	if !f.cfg.IgnorePerms {
		nf.Mode = uint32(st.Mode().Perm())
	}
	l, have := f.ix.get(name)
	if have && !l.Deleted && sameContent(&l, &nf, f.cfg.IgnorePerms) {
		l.ModNs = nf.ModNs
		f.ix.put(l)
		return
	}
	nf.Version = l.Version.Bump(f.cfg.Node)
	nf.By = f.cfg.Node
	if nf.Check(f.cfg.MaxFileSize) != nil {
		return
	}
	f.ix.put(nf)
}

// toTrashCopy keeps the file that is about to be replaced.  A hard link is enough: the new file is a different one.
func (f *Folder) toTrashCopy(p, name string) error {
	if !filepath.IsLocal(name) {
		return ErrBadName
	}
	dst := filepath.Join(f.root, MetaDir, "trash", f.now().UTC().Format("20060102-150405"), filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	for i := 1; ; i++ {
		if _, err := os.Lstat(dst); os.IsNotExist(err) {
			break
		}
		dst = fmt.Sprintf("%s.%d", dst, i)
	}
	if err := os.Link(p, dst); err != nil {
		return err
	}
	return nil
}

// Delete moves a file, link or whole folder to the trash.  The deletion reaches the other nodes at the next scan.
func (f *Folder) Delete(name string) error {
	p, err := f.resolve(name)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(p); err != nil {
		if os.IsNotExist(err) {
			// gone from the disk but still in the index (a scan refused to record it): deleting it again records it
			if n, terr := f.tombstoneN(name); terr != nil || n > 0 {
				return terr
			}
			return ErrNotFound
		}
		return err
	}
	f.doom(name, doomEntry{any: true})                                                    // a sync that is pulling it stops, and what has come of it goes with it
	if err := f.toTrashAt(p, name, f.now().UTC().Format("20060102-150405")); err != nil { // a deletion made here has its own stamp
		return err
	}
	return f.tombstone(name)
}

// tombstone records at once that a name, and everything under it, is gone.  A person deleted it on purpose, so the scan's refusal to
// delete most of a folder (which guards against a disk that came back empty) must not apply: without this, deleting a folder with
// hundreds of files in it was refused at the next scan, and the folder stayed in the list on every node.
func (f *Folder) tombstone(name string) error {
	_, err := f.tombstoneN(name)
	return err
}

func (f *Folder) tombstoneN(name string) (int, error) {
	var gone []FileInfo
	for _, l := range f.ix.snapshot() {
		if l.Deleted || !(l.Name == name || strings.HasPrefix(l.Name, name+"/")) {
			continue
		}
		gone = append(gone, FileInfo{Name: l.Name, Kind: l.Kind, Deleted: true, ModNs: f.now().UnixNano(), Version: l.Version.Bump(f.cfg.Node), By: f.cfg.Node})
	}
	return len(gone), f.ix.put(gone...)
}

// Rename moves a file or folder.  It will not replace anything.
func (f *Folder) Rename(from, to string) error {
	a, err := f.resolve(from)
	if err != nil {
		return err
	}
	b, err := f.resolve(to)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(a); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	if _, err := os.Lstat(b); err == nil {
		return ErrExists
	}
	if err := os.MkdirAll(filepath.Dir(b), 0o755); err != nil {
		return err
	}
	if err := os.Rename(a, b); err != nil {
		return err
	}
	return f.recordMoved(from, to)
}

// recordMoved records a rename in the index at once: the old names are deleted and the new ones take over the old entries' content (a rename
// keeps size and time, so nothing has to be read).  Left to the scan, a big file or folder would vanish from the list while the scan
// hashed it all again, and be back minutes later.  An entry that does not match the disk is left to the scan.
func (f *Folder) recordMoved(from, to string) error {
	var out []FileInfo
	for _, l := range f.ix.snapshot() {
		if l.Deleted || !(l.Name == from || strings.HasPrefix(l.Name, from+"/")) {
			continue
		}
		out = append(out, FileInfo{Name: l.Name, Kind: l.Kind, Deleted: true, ModNs: f.now().UnixNano(), Version: l.Version.Bump(f.cfg.Node), By: f.cfg.Node})
		nn := to + strings.TrimPrefix(l.Name, from)
		p, err := f.resolve(nn)
		if err != nil {
			continue
		}
		st, err := os.Lstat(p)
		if err != nil {
			continue
		}
		nf := l
		nf.Name, nf.Seq = nn, 0
		switch {
		case l.Kind == KFile && st.Mode().IsRegular() && st.Size() == l.Size && st.ModTime().UnixNano() == l.ModNs:
		case l.Kind == KDir && st.IsDir():
		case l.Kind == KSymlink && st.Mode()&os.ModeSymlink != 0:
		default:
			continue
		}
		old, have := f.ix.get(nn)
		if have {
			nf.Version = old.Version.Bump(f.cfg.Node)
		} else {
			nf.Version = Vector{}.Bump(f.cfg.Node)
		}
		nf.By = f.cfg.Node
		out = append(out, nf)
	}
	if len(out) == 0 {
		return nil
	}
	return f.ix.put(out...)
}
