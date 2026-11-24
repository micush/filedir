package filesync

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
)

// A folder whose index was made with SHA-256 block hashes (before version 81) is converted once, before it is opened: every file the
// index holds is read, and the hash of each of its blocks is made again, with the blocks cut where the entry cut them.  A file is
// taken to be what the index says if its size and time are, which is all that a scan ever goes by; one that is not is left with its
// old hashes, and the first scan finds it changed and makes a new entry of it.  Versions, sequence numbers and what has been read
// of the other nodes' indexes are kept as they are.

var migrating sync.Map // index directories being converted

// Migrating says whether the index in dir is being converted now.
func Migrating(dir string) bool { _, ok := migrating.Load(filepath.Clean(dir)); return ok }

// MigrateIndex converts the index of the folder in cfg to the current hash.  progress is called as it goes, with the files done and the
// files to do.  It does nothing for an index that is current.
func MigrateIndex(cfg Config, progress func(done, total int)) error {
	dir := filepath.Clean(cfg.DBDir)
	if _, busy := migrating.LoadOrStore(dir, true); busy {
		return fmt.Errorf("filesync: %s is being converted already", dir)
	}
	defer migrating.Delete(dir)
	ix, err := loadIndex(dir, true)
	if err != nil {
		return err
	}
	defer ix.close()
	if ix.alg == HashName {
		return nil
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return err
	}
	var todo []*FileInfo
	ix.mu.Lock()
	for _, fi := range ix.files {
		if fi.Kind == KFile && !fi.Deleted && len(fi.Blocks) > 0 {
			todo = append(todo, fi)
		}
	}
	ix.mu.Unlock()
	if progress != nil {
		progress(0, len(todo))
	}
	var next, done atomic.Int64
	var wg sync.WaitGroup
	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(todo) {
					return
				}
				rehash(root, todo[i])
				d := int(done.Add(1))
				if progress != nil && (d%16 == 0 || d == len(todo)) {
					progress(d, len(todo))
				}
			}
		}()
	}
	wg.Wait()
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.snap = nil
	return ix.compact() // writes the journal again, in the current hash
}

// rehash makes the new hashes of a file's blocks, if the file is what the entry says.  Where it is not, or cannot be read, the entry is
// left alone.
func rehash(root string, fi *FileInfo) {
	if !filepath.IsLocal(filepath.FromSlash(fi.Name)) {
		return
	}
	p := filepath.Join(root, filepath.FromSlash(fi.Name))
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() != fi.Size || st.ModTime().UnixNano() != fi.ModNs {
		return
	}
	fh, err := os.Open(p)
	if err != nil {
		return
	}
	defer fh.Close()
	hs := make([]Hash, len(fi.Blocks))
	var buf []byte
	for i, b := range fi.Blocks {
		if b.Size <= 0 || b.Size > maxBlock {
			return
		}
		if cap(buf) < int(b.Size) {
			buf = make([]byte, b.Size)
		}
		d := buf[:b.Size]
		if _, err := io.ReadFull(io.NewSectionReader(fh, b.Off, int64(b.Size)), d); err != nil {
			return
		}
		hs[i] = sumBlock(d)
	}
	if after, err := os.Lstat(p); err != nil || after.Size() != fi.Size || after.ModTime().UnixNano() != fi.ModNs {
		return // written to while it was read
	}
	for i := range fi.Blocks {
		fi.Blocks[i].Hash = hs[i]
	}
}
