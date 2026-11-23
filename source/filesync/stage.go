package filesync

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrBadOffset is a chunk that does not begin where the file so far ends.
var ErrBadOffset = errors.New("filesync: the chunk does not start where the file so far ends")

const stageMaxAge = time.Hour

func (f *Folder) stagePath(name string) string {
	h := sha1.Sum([]byte(name))
	return filepath.Join(f.root, MetaDir, "tmp", "stage-"+hex.EncodeToString(h[:8]))
}

// StageAppend adds one chunk of a file that is sent in parts.  Offset 0 begins (and discards any earlier attempt); any other offset must be
// the size so far.  The file is not in the folder until StageCommit: nobody sees half of it, and it is one change (one entry in the
// trash for what it replaces).  It returns the size so far.  A staged file nobody finishes is removed an hour after its last chunk.
func (f *Folder) StageAppend(name string, off int64, data []byte) (int64, error) {
	p, err := f.resolve(name)
	if err != nil {
		return 0, err
	}
	if st, err := os.Lstat(p); err == nil && st.IsDir() {
		return 0, ErrIsDir
	}
	tdir := filepath.Join(f.root, MetaDir, "tmp")
	if err := os.MkdirAll(tdir, 0o700); err != nil {
		return 0, err
	}
	if off == 0 {
		if es, err := os.ReadDir(tdir); err == nil { // other people's abandoned uploads
			for _, e := range es {
				if i, err := e.Info(); err == nil && strings.HasPrefix(e.Name(), "stage-") && time.Since(i.ModTime()) > stageMaxAge {
					os.Remove(filepath.Join(tdir, e.Name()))
				}
			}
		}
	}
	sp := f.stagePath(name)
	flag := os.O_WRONLY | os.O_CREATE | os.O_APPEND
	if off == 0 {
		flag = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	fh, err := os.OpenFile(sp, flag, 0o600)
	if err != nil {
		return 0, err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return 0, err
	}
	if st.Size() != off {
		return 0, ErrBadOffset
	}
	if err := f.room(int64(len(data))); err != nil {
		return 0, err
	}
	if _, err := fh.Write(data); err != nil {
		return 0, err
	}
	size := off + int64(len(data))
	if f.cfg.MaxFileSize > 0 && size > f.cfg.MaxFileSize {
		os.Remove(sp)
		return 0, ErrTooBig
	}
	return size, nil
}

// StageCommit puts the staged file in the folder under name, as WriteFile does, and removes the staged copy.
func (f *Folder) StageCommit(name string) (int64, error) {
	sp := f.stagePath(name)
	fh, err := os.Open(sp)
	if err != nil {
		return 0, ErrNotFound
	}
	defer os.Remove(sp)
	defer fh.Close()
	return f.WriteFile(name, io.Reader(fh))
}
