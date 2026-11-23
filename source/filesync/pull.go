package filesync

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

var errNotEmpty = errors.New("filesync: directory is not empty")

// Block serves a block of a file to a peer.  What is read is checked against the hash first, so a file that has changed since it was
// scanned is never sent as if it were the old one.
func (f *Folder) Block(name string, off int64, size int, hash Hash) ([]byte, error) {
	if size <= 0 || size > maxBlock {
		return nil, fmt.Errorf("%w: %s", ErrChanged, name)
	}
	l, ok := f.ix.get(name)
	if !ok || l.Deleted || l.Kind != KFile {
		if fd, ok := f.flightBlock(name, off, size, hash); ok { // a file that is new here and is being pulled
			return fd, nil
		}
		return nil, fmt.Errorf("%w: %s", ErrChanged, name)
	}
	p, err := f.full(name)
	if err != nil {
		return nil, err
	}
	d, err := readBlock(p, off, size, hash)
	if err != nil { // not in the file that is in place: perhaps it has arrived of a newer one that is being pulled
		if fd, ok := f.flightBlock(name, off, size, hash); ok {
			return fd, nil
		}
	}
	return d, err
}

func readBlock(p string, off int64, size int, hash Hash) ([]byte, error) {
	fh, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	buf := make([]byte, size)
	if _, err := fh.ReadAt(buf, off); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrChanged, err)
	}
	if sha256.Sum256(buf) != hash {
		return nil, ErrChanged
	}
	return buf, nil
}

func depth(name string) int { return strings.Count(name, "/") }

// SyncFrom brings this folder up to date with what a peer holds.  It scans first, so that what has changed here has a version to be
// compared with.  It returns how many paths changed on disk.  The peer's position is remembered only when every entry was dealt with, so
// an entry that failed (the peer went away half way) is asked for again.
func (f *Folder) SyncFrom(peer NodeID, src Source) (int, error) {
	f.op.Lock()
	defer f.op.Unlock()
	if _, err := f.scan(); err != nil {
		return 0, err
	}
	cur := f.ix.cursor(peer)
	list, max, err := src.Index(f.cfg.ID, cur)
	if err != nil {
		return 0, err
	}
	if max < cur { // the peer's index was rebuilt: start again
		cur = 0
		if list, max, err = src.Index(f.cfg.ID, 0); err != nil {
			return 0, err
		}
	}
	// deletions first and deepest first, so that a directory is empty by the time it is removed; then everything else, shallowest first
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.Deleted != b.Deleted {
			return a.Deleted
		}
		if a.Deleted {
			return depth(a.Name) > depth(b.Name)
		}
		return depth(a.Name) < depth(b.Name)
	})
	if bs, ok := src.(BatchSource); ok {
		if small := f.batchable(list); len(small) > 1 {
			pf := newPrefetcher(bs, f.cfg.ID, small)
			defer pf.close()
			src = pf
		}
	}
	n := 0
	var failed error
	var retry []FileInfo
	var mu sync.Mutex // guards n, failed, retry while files are pulled side by side
	record := func(fi FileInfo, ch bool, err error) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case errors.Is(err, errNotEmpty):
			retry = append(retry, fi)
		case err != nil:
			if failed == nil {
				failed = fmt.Errorf("%s: %w", fi.Name, err)
			}
		case ch:
			n++
		}
	}
	// Files that are only written (not deleted, not directories) are pulled several at a time: for many small files the wait for the
	// peer and for the disk, not the copying, is what takes the time.  Anything else waits until the ones in flight are done, so
	// directories and deletions keep their order.
	var wg sync.WaitGroup
	slots := make(chan struct{}, filePar)
	for _, fi := range list {
		if fi.Kind == KFile && !fi.Deleted {
			slots <- struct{}{}
			wg.Add(1)
			go func(fi FileInfo) {
				defer func() { <-slots; wg.Done() }()
				ch, err := f.handle(fi, src)
				record(fi, ch, err)
			}(fi)
			continue
		}
		wg.Wait()
		ch, err := f.handle(fi, src)
		record(fi, ch, err)
	}
	wg.Wait()
	for _, fi := range retry {
		ch, err := f.handle(fi, src)
		switch {
		case errors.Is(err, errNotEmpty):
			// something is still in the directory that the other nodes don't know of: it stays, and so does the directory
			if err := f.keepDir(fi); err != nil && failed == nil {
				failed = err
			}
		case err != nil:
			if failed == nil {
				failed = fmt.Errorf("%s: %w", fi.Name, err)
			}
		case ch:
			n++
		}
	}
	if failed != nil {
		return n, failed
	}
	return n, f.ix.setCursor(peer, max)
}

// keepDir answers a deletion of a directory that still holds files here: the directory is kept, and its version moves past the
// deletion, so the other nodes take it back.
func (f *Folder) keepDir(del FileInfo) error {
	l, _ := f.ix.get(del.Name)
	nf := FileInfo{Name: del.Name, Kind: KDir, Mode: 0o755, ModNs: f.now().UnixNano(), By: f.cfg.Node,
		Version: del.Version.Merge(l.Version).Bump(f.cfg.Node)}
	if p, err := f.full(del.Name); err == nil {
		if st, err := os.Lstat(p); err == nil && !f.cfg.IgnorePerms {
			nf.Mode = uint32(st.Mode().Perm())
		}
	}
	return f.ix.put(nf)
}

// handle decides what one entry from a peer means here, and does it.  An entry that is not well formed is dropped for good.
func (f *Folder) handle(fi FileInfo, src Source) (bool, error) {
	if err := fi.Check(f.cfg.MaxFileSize); err != nil {
		return false, nil
	}
	if f.ignored(fi.Name, fi.Kind == KDir) {
		return false, nil
	}
	l, have := f.ix.get(fi.Name)
	if !have {
		return f.apply(fi, FileInfo{Name: fi.Name, Deleted: true}, false, src)
	}
	switch fi.Version.Compare(l.Version) {
	case Equal, Lesser:
		return false, nil
	case Greater:
		return f.apply(fi, l, false, src)
	}
	// concurrent: each side has a change the other has not seen
	merged := fi.Version.Merge(l.Version)
	switch {
	case fi.Deleted && l.Deleted:
		l.Version = merged
		return false, f.ix.put(l)
	case fi.Deleted:
		return false, f.ix.put(l) // a change beats a deletion: this one stays; it is announced again, so the peer looks at it again
	case l.Deleted:
		fi.Version = merged.Bump(f.cfg.Node) // the winner's content under a version of its own: two nodes that settle two different pairs must not end up with the same vector
		return f.apply(fi, l, false, src)
	case sameContent(&fi, &l, f.cfg.IgnorePerms):
		l.Version = merged
		return false, f.ix.put(l)
	}
	remoteWins := fi.ModNs > l.ModNs || (fi.ModNs == l.ModNs && fi.By > l.By)
	if !remoteWins {
		// this one stays; it is announced again (a new position in the index, the same version), because the peer may have looked at it
		// before, when it had something else, and moved on; it loses, and keeps a conflict copy
		return false, f.ix.put(l)
	}
	fi.Version = merged.Bump(f.cfg.Node)
	return f.apply(fi, l, true, src)
}

// apply makes the disk match fi.  l is what the index held (a tombstone if nothing).  With conflict set, a file in the way is kept
// under a conflict name, otherwise it goes to the trash.
func (f *Folder) apply(fi, l FileInfo, conflict bool, src Source) (bool, error) {
	p, err := f.full(fi.Name)
	if err != nil {
		return false, err
	}
	if err := f.checkLocal(p, &l); err != nil {
		return false, err
	}
	switch {
	case fi.Deleted:
		if st, err := os.Lstat(p); err == nil {
			if err := f.displace(p, st, &l, false); err != nil {
				return false, err
			}
		}
		return true, f.ix.put(fi)
	case fi.Kind == KDir:
		if st, err := os.Lstat(p); err == nil && !st.IsDir() {
			if err := f.displace(p, st, &l, conflict); err != nil {
				return false, err
			}
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			return false, err
		}
		if !f.cfg.IgnorePerms && fi.Mode != 0 {
			if err := os.Chmod(p, os.FileMode(fi.Mode)); err != nil {
				return false, err
			}
		}
		return true, f.ix.put(fi)
	case fi.Kind == KSymlink:
		if st, err := os.Lstat(p); err == nil {
			if err := f.displace(p, st, &l, conflict); err != nil {
				return false, err
			}
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return false, err
		}
		if err := os.Symlink(fi.Target, p); err != nil {
			return false, err
		}
		return true, f.ix.put(fi)
	}
	return f.pullFile(fi, l, p, conflict, src)
}

// checkLocal refuses to touch a path that has changed here since the last scan: the change has no version yet, and overwriting it
// would lose it.  The next scan gives it one, and the two are then compared properly.
func (f *Folder) checkLocal(p string, l *FileInfo) error {
	st, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if l.Deleted {
		return fmt.Errorf("%w: %s", ErrLocalChange, l.Name)
	}
	kind := KFile
	switch {
	case st.IsDir():
		kind = KDir
	case st.Mode()&os.ModeSymlink != 0:
		kind = KSymlink
	}
	if !f.unchanged(l, kind, st, p) {
		return fmt.Errorf("%w: %s", ErrLocalChange, l.Name)
	}
	return nil
}

// displace gets whatever is at p out of the way: a directory must be empty and is removed; anything else goes to the trash, or is
// renamed beside itself as a conflict copy.
func (f *Folder) displace(p string, st os.FileInfo, l *FileInfo, conflict bool) error {
	if st.IsDir() {
		if err := os.Remove(p); err != nil {
			if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
				return errNotEmpty
			}
			return err
		}
		return nil
	}
	if conflict {
		by := l.By
		if by == "" {
			by = f.cfg.Node
		}
		to, err := f.full(f.conflictName(l.Name, by))
		if err != nil {
			return err
		}
		return os.Rename(p, to)
	}
	return f.toTrash(p, l.Name)
}

// pullStamp is the trash folder a deletion that arrives from another node goes into.  A folder deleted elsewhere arrives one file after
// another, over many seconds: each used to get the stamp of its own second, so one deletion showed up in the trash as dozens of entries of
// the same name, each holding a slice of it.  While deletions keep arriving (a pause of less than a minute) they share the stamp of the first.
func (f *Folder) pullStamp() string {
	f.tmu.Lock()
	defer f.tmu.Unlock()
	now := f.now()
	if f.tStamp == "" || now.Sub(f.tLast) > time.Minute || now.Before(f.tLast) {
		f.tStamp = now.UTC().Format("20060102-150405")
	}
	f.tLast = now
	return f.tStamp
}

func (f *Folder) toTrash(p, name string) error {
	return f.toTrashAt(p, name, f.pullStamp())
}

// toTrashAt moves p into the trash folder when, as name.
func (f *Folder) toTrashAt(p, name, when string) error {
	dst := filepath.Join(f.root, MetaDir, "trash", when, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	for i := 1; ; i++ {
		if _, err := os.Lstat(dst); os.IsNotExist(err) {
			break
		}
		dst = fmt.Sprintf("%s.%d", dst, i)
	}
	return os.Rename(p, dst)
}

// pullFile builds the new file beside the folder's own directory, then swaps it in.  Blocks already on disk are reused; the rest come
// from the peer and are checked against their hashes.  The file is never half written under its own name.
func (f *Folder) pullFile(fi, l FileInfo, p string, conflict bool, src Source) (bool, error) {
	if !l.Deleted && l.Kind == KFile && sameContent(&fi, &l, f.cfg.IgnorePerms) {
		// the same bytes: only the version differs
		l.Version = fi.Version
		l.By, l.ModNs = fi.By, fi.ModNs
		return false, f.ix.put(l)
	}
	if err := f.room(fi.Size); err != nil {
		return false, err
	}
	tdir := filepath.Join(f.root, MetaDir, "tmp")
	if err := os.MkdirAll(tdir, 0o700); err != nil {
		return false, err
	}
	tf, err := os.CreateTemp(tdir, "pull-*")
	if err != nil {
		return false, err
	}
	tmp := tf.Name()
	done := false
	defer func() {
		if !done {
			tf.Close()
			os.Remove(tmp)
		}
	}()
	// the blocks the old file has, by hash
	have := map[Hash]Block{}
	var old *os.File
	if !l.Deleted && l.Kind == KFile {
		if of, err := os.Open(p); err == nil {
			old = of
			defer old.Close()
			for _, b := range l.Blocks {
				have[b.Hash] = b
			}
		}
	}
	// blocks the old file already has are copied; the rest are fetched, a few at a time, each from a different peer where there are
	// several that hold this version: the more nodes have the file, the more of it arrives at once
	fl := f.flyStart(fi, tmp)
	defer f.flyEnd(fl)
	var want []Block
	for bi, b := range fi.Blocks {
		var data []byte
		if lb, ok := have[b.Hash]; ok && lb.Size == b.Size {
			buf := make([]byte, b.Size)
			if _, err := old.ReadAt(buf, lb.Off); err == nil && sha256.Sum256(buf) == b.Hash {
				data = buf
			}
		}
		if data == nil {
			want = append(want, b)
			continue
		}
		if _, err := tf.WriteAt(data, b.Off); err != nil {
			return false, err
		}
		if fl != nil {
			fl.mark(bi)
		}
	}
	if err := f.fetch(tf, fi, want, src, fl); err != nil {
		return false, err
	}
	if err := tf.Truncate(fi.Size); err != nil {
		return false, err
	}
	if err := tf.Sync(); err != nil {
		return false, err
	}
	if err := tf.Close(); err != nil {
		return false, err
	}
	mode := os.FileMode(0o644)
	if !f.cfg.IgnorePerms && fi.Mode != 0 {
		mode = os.FileMode(fi.Mode)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return false, err
	}
	if err := os.Chtimes(tmp, time.Time{}, time.Unix(0, fi.ModNs)); err != nil {
		return false, err
	}
	if err := f.stage("before-swap"); err != nil {
		return false, err
	}
	// the file may have been written to while the blocks were coming: look again, just before the swap
	if err := f.checkLocal(p, &l); err != nil {
		return false, err
	}
	if st, err := os.Lstat(p); err == nil {
		if err := f.displace(p, st, &l, conflict); err != nil {
			return false, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, p); err != nil {
		return false, err
	}
	done = true
	syncDirShared(filepath.Dir(p))
	if err := f.stage("after-swap"); err != nil {
		return false, err
	}
	return true, f.ix.put(fi)
}

func syncDir(d string) {
	if fh, err := os.Open(d); err == nil {
		fh.Sync()
		fh.Close()
	}
}

// Maintain empties what is past its time: the trash, and the memory of deletions that every node has had long enough to see.
func (f *Folder) Maintain() error {
	f.op.Lock()
	defer f.op.Unlock()
	cut := f.now().Add(-time.Duration(f.cfg.TrashDays) * 24 * time.Hour).UTC().Format("20060102-150405")
	td := filepath.Join(f.root, MetaDir, "trash")
	if ents, err := os.ReadDir(td); err == nil {
		for _, e := range ents {
			if e.Name() < cut {
				os.RemoveAll(filepath.Join(td, e.Name()))
			}
		}
	}
	if ents, err := os.ReadDir(filepath.Join(f.root, MetaDir, "tmp")); err == nil { // uploads nobody came back to
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), "resume-") {
				if st, err := e.Info(); err == nil && f.now().Sub(st.ModTime()) > 48*time.Hour {
					os.Remove(filepath.Join(f.root, MetaDir, "tmp", e.Name()))
				}
			}
		}
	}
	tcut := f.now().Add(-time.Duration(f.cfg.TombDays) * 24 * time.Hour).UnixNano()
	var old []string
	for _, fi := range f.ix.snapshot() {
		if fi.Deleted && fi.ModNs < tcut {
			old = append(old, fi.Name)
		}
	}
	if len(old) > 0 {
		return f.ix.drop(old)
	}
	return nil
}

// perSource is how many blocks are asked for at once from each peer: a block is one request, so a peer that answers in a few
// milliseconds feeds a transfer at (blocks in flight x block size / that time), which a handful of requests keeps under a few Gbit/s.
const perSource = 16

// maxWorkers is how many blocks of one file are asked for at once.
const maxWorkers = 64

// filePar is how many files one SyncFrom pulls at once.
const filePar = 12
