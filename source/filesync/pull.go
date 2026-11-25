package filesync

import (
	"crypto/sha256"
	"encoding/hex"
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
	d, _, err := f.block(name, off, size, hash, false)
	return d, err
}

// BlockPooled is Block for a caller that is done with the data once it has sent it: the buffer comes from the pool, and release (nil when
// the data is not pooled) gives it back.
func (f *Folder) BlockPooled(name string, off int64, size int, hash Hash) (data []byte, release func(), err error) {
	return f.block(name, off, size, hash, true)
}

func (f *Folder) block(name string, off int64, size int, hash Hash, pooled bool) ([]byte, func(), error) {
	if size <= 0 || size > maxBlock {
		return nil, nil, fmt.Errorf("%w: %s", ErrChanged, name)
	}
	l, ok := f.ix.get(name)
	if !ok || l.Deleted || l.Kind != KFile {
		if fd, ok := f.flightBlock(name, off, size, hash); ok { // a file that is new here and is being pulled
			return fd, nil, nil
		}
		return nil, nil, fmt.Errorf("%w: %s", ErrChanged, name)
	}
	p, err := f.full(name)
	if err != nil {
		return nil, nil, err
	}
	d, rel, err := readBlockBuf(p, off, size, hash, pooled)
	if err != nil { // not in the file that is in place: perhaps it has arrived of a newer one that is being pulled
		if fd, ok := f.flightBlock(name, off, size, hash); ok {
			return fd, nil, nil
		}
	}
	return d, rel, err
}

func readBlock(p string, off int64, size int, hash Hash) ([]byte, error) {
	d, _, err := readBlockBuf(p, off, size, hash, false)
	return d, err
}

func readBlockBuf(p string, off int64, size int, hash Hash, pooled bool) ([]byte, func(), error) {
	fh, err := os.Open(p)
	if err != nil {
		return nil, nil, err
	}
	defer fh.Close()
	var buf []byte
	var rel func()
	if pooled {
		buf = GetBuf(size)
		rel = func() { PutBuf(buf) }
	} else {
		buf = make([]byte, size)
	}
	if _, err := fh.ReadAt(buf, off); err != nil {
		if rel != nil {
			rel()
		}
		return nil, nil, fmt.Errorf("%w: %v", ErrChanged, err)
	}
	if sumBlock(buf) != hash {
		if rel != nil {
			rel()
		}
		return nil, nil, ErrChanged
	}
	return buf, rel, nil
}

func depth(name string) int { return strings.Count(name, "/") }

// SyncFrom brings this folder up to date with what a peer holds.  It scans first, so that what has changed here has a version to be
// compared with.  It returns how many paths changed on disk.  The peer's position is remembered only when every entry was dealt with, so
// an entry that failed (the peer went away half way) is asked for again.
func (f *Folder) SyncFrom(peer NodeID, src Source) (int, error) {
	f.op.Lock()
	defer f.op.Unlock()
	f.syncBegin()
	defer f.syncEnd()
	f.pq.begin()
	defer f.pq.end()
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
	// what the peer deletes while this runs: the pulls under it stop, and the deletions are applied before the end (doom.go)
	var late []FileInfo
	stopWatch := make(chan struct{})
	var ww sync.WaitGroup
	if len(list) > 0 {
		ww.Add(1)
		go func() {
			defer ww.Done()
			f.watchDeletes(src, max, f.watchEvery(), stopWatch, func(d []FileInfo) { mu.Lock(); late = append(late, d...); mu.Unlock() })
		}()
	}
	record := func(fi FileInfo, ch bool, err error) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case errors.Is(err, errNotEmpty):
			retry = append(retry, fi)
		case errors.Is(err, errDoomed):
			// deleted while it was being replicated: nothing to report, the deletion is applied below
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
	close(stopWatch)
	ww.Wait()
	// the deletions that came while the pulls were under way, deepest first: what had come of the items is removed with them
	mu.Lock()
	dels := late
	mu.Unlock()
	sort.SliceStable(dels, func(i, j int) bool { return depth(dels[i].Name) > depth(dels[j].Name) })
	for _, fi := range dels {
		ch, err := f.handle(fi, src)
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
	f.nt.flush(f.cfg.Logf)
	if err := f.flushPuts(); err != nil && failed == nil { // the position is kept only when every entry is in the index
		failed = err
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
	f.nt.add("kept the folder "+del.Name+" (the peer deleted it; it is not empty here): "+f.describeDir(del.Name), f.cfg.Logf)
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
	if !fi.Deleted && f.doomed(fi) { // deleted since the list was taken: not started
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
	case fi.Deleted && l.Kind == KDir && f.emptyHere(l.Name):
		// a folder with nothing in it has nothing that could beat a deletion: its version moved on only because it was kept or announced
		// again before, and that must not bring it back
		fi.Version = merged
		return f.apply(fi, l, false, src)
	case fi.Deleted:
		f.nt.add("kept "+l.Name+" (a change here beats the peer's deletion): here "+l.Version.String()+" by "+string(l.By)+", the deletion "+fi.Version.String()+" by "+string(fi.By), f.cfg.Logf)
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
	if fi.Deleted {
		f.dropParts(fi.Name) // what an earlier try had pulled of a file that is gone
	}
	switch {
	case fi.Deleted:
		if st, err := os.Lstat(p); err == nil {
			if fi.Purge && !st.IsDir() {
				if err := os.Remove(p); err != nil {
					return false, err
				}
			} else if err := f.displace(p, st, &l, false); err != nil {
				return false, err
			}
		}
		return true, f.putDeleted(fi)
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
				// what the sync leaves out (the ignore patterns) is not in the index and is never deleted by a deletion that arrives: left
				// alone it keeps the folder, and the folder then comes back on every node as an empty one.  A folder that holds nothing
				// else goes, with what the sync leaves out, to the trash.
				if f.onlyIgnored(p, l.Name) && f.sweepIgnored(p, l.Name) == nil {
					if err := os.Remove(p); err == nil {
						return nil
					}
				}
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
	if !filepath.IsLocal(name) || strings.ContainsAny(when, "/\\") {
		return ErrBadName
	}
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
	// The file is pulled into a partial file whose name is the file and its version, and it is kept when the pull fails (a network that
	// is down for hours, a restart): the next try checks which blocks are in it and fetches the rest.  It is dropped only when the file
	// was deleted meanwhile, and Maintain drops one that nobody came back to.
	tmp := partPath(tdir, fi)
	tf, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return false, err
	}
	var psize int64
	if st, err := tf.Stat(); err == nil {
		psize = st.Size()
	}
	now := time.Now()
	_ = os.Chtimes(tmp, now, now)
	done, keep := false, true
	defer func() {
		if !done {
			if st, err := tf.Stat(); err == nil && st.Size() == 0 {
				keep = false // nothing came of it
			}
			tf.Close()
			if !keep {
				os.Remove(tmp)
			}
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
	resumed := 0
	for bi, b := range fi.Blocks {
		var data []byte
		if lb, ok := have[b.Hash]; ok && lb.Size == b.Size {
			buf := make([]byte, b.Size)
			if _, err := old.ReadAt(buf, lb.Off); err == nil && sumBlock(buf) == b.Hash {
				data = buf
			}
		}
		if data == nil && b.Off+int64(b.Size) <= psize { // a block that an earlier try left in the partial file, if it is whole
			buf := make([]byte, b.Size)
			if _, err := tf.ReadAt(buf, b.Off); err == nil && sumBlock(buf) == b.Hash {
				resumed++
				if fl != nil {
					fl.mark(bi)
				}
				continue
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
	if resumed > 0 && f.cfg.Logf != nil {
		f.cfg.Logf("filesync: %s: %d of %d blocks were here from an earlier try; fetching the other %d", fi.Name, resumed, len(fi.Blocks), len(want))
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
	// deleted while it was coming: the temporary file is dropped.  The check and the swap are one step with respect to Delete, so a file
	// is never put back into a folder that is being removed.
	f.dm.mu.Lock()
	if f.doomedLocked(fi) {
		f.dm.mu.Unlock()
		keep = false
		return false, errDoomed
	}
	if st, err := os.Lstat(p); err == nil {
		if err := f.displace(p, st, &l, conflict); err != nil {
			f.dm.mu.Unlock()
			return false, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.dm.mu.Unlock()
		return false, err
	}
	if err := os.Rename(tmp, p); err != nil {
		f.dm.mu.Unlock()
		return false, err
	}
	f.dm.mu.Unlock()
	done = true
	f.dropParts(fi.Name) // the partial files of other versions of it
	syncDirShared(filepath.Dir(p))
	if err := f.stage("after-swap"); err != nil {
		return false, err
	}
	return true, f.ix.put(fi)
}

// partKeep is how long a partial file of a pull that did not finish is kept after the last try.
const partKeep = 72 * time.Hour

// partPath is the partial file of one version of a file.  Whatever is in it is checked block by block against the hashes before it is
// used, so a name that two versions happened to share would cost only a refetch.
func partPath(tdir string, fi FileInfo) string {
	h := sha256.Sum256([]byte(fi.Version.String() + "\x00" + fmt.Sprint(fi.Size)))
	return filepath.Join(tdir, partPrefix(fi.Name)+hex.EncodeToString(h[:10]))
}

// partPrefix starts the names of all the partial files of one file, whatever their version.
func partPrefix(name string) string {
	h := sha256.Sum256([]byte(name))
	return "part-" + hex.EncodeToString(h[:10]) + "-"
}

// dropParts removes the partial files of a file: it arrived, or it is gone.
func (f *Folder) dropParts(name string) {
	old, _ := filepath.Glob(filepath.Join(f.root, MetaDir, "tmp", partPrefix(name)+"*"))
	for _, p := range old {
		os.Remove(p)
	}
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
			if strings.HasPrefix(e.Name(), "part-") || strings.HasPrefix(e.Name(), "pull-") { // pulls that did not finish, and were not tried again
				if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > partKeep {
					os.Remove(filepath.Join(f.root, MetaDir, "tmp", e.Name()))
				}
				continue
			}
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

// onlyIgnored says whether everything under the folder p (named rel) is left out of the sync: files and folders that the ignore patterns
// match, and folders that hold only such things.  A link, or a file the sync does not leave out, makes it false.
func (f *Folder) onlyIgnored(p, rel string) bool {
	ents, err := os.ReadDir(p)
	if err != nil {
		return false
	}
	for _, e := range ents {
		n := rel + "/" + e.Name()
		switch {
		case e.Type()&os.ModeSymlink != 0:
			return false
		case e.IsDir():
			if !f.ignored(n, true) && !f.onlyIgnored(filepath.Join(p, e.Name()), n) {
				return false
			}
		case !f.ignored(n, false):
			return false
		}
	}
	return true
}

// sweepIgnored moves what onlyIgnored found into the trash, and removes the folders that are left empty.
func (f *Folder) sweepIgnored(p, rel string) error {
	ents, err := os.ReadDir(p)
	if err != nil {
		return err
	}
	for _, e := range ents {
		n, cp := rel+"/"+e.Name(), filepath.Join(p, e.Name())
		if e.IsDir() && !f.ignored(n, true) {
			if err := f.sweepIgnored(cp, n); err != nil {
				return err
			}
			if err := os.Remove(cp); err != nil {
				return err
			}
			continue
		}
		if err := f.toTrash(cp, n); err != nil {
			return err
		}
	}
	return nil
}

// emptyHere says whether the folder name has nothing in it on this disk (or is not there).
func (f *Folder) emptyHere(name string) bool {
	p, err := f.full(name)
	if err != nil {
		return false
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		return os.IsNotExist(err)
	}
	return len(ents) == 0
}
