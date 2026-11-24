package filesync

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrDiskFull is the answer when a write would take the disk that holds the folder past the limit that was set for it.
var ErrDiskFull = errors.New("filesync: the disk is as full as this folder is allowed to make it")

// DiskUse says how full the filesystem under a path is, the way df does (what is used, over what is used and what an ordinary user may
// still use), and how many bytes an ordinary user may still use.
func DiskUse(path string) (pct float64, avail uint64, err error) {
	var st syscall.Statfs_t
	if err = syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	used := (uint64(st.Blocks) - uint64(st.Bfree)) * bs
	avail = uint64(st.Bavail) * bs
	if used+avail == 0 {
		return 0, 0, nil
	}
	return float64(used) * 100 / float64(used+avail), avail, nil
}

// DiskSpace says how much of the filesystem under a path is used and how much an ordinary user may still use (the way df does), and an
// identity for the filesystem so that folders on one disk are told apart from folders on another.
func DiskSpace(path string) (used, avail uint64, id string, err error) {
	var st syscall.Statfs_t
	if err = syscall.Statfs(path, &st); err != nil {
		return 0, 0, "", err
	}
	bs := uint64(st.Bsize)
	return (uint64(st.Blocks) - uint64(st.Bfree)) * bs, uint64(st.Bavail) * bs, fmt.Sprint(st.Fsid), nil
}

// room refuses when taking in "incoming" more bytes would leave the disk fuller than the limit (MaxUsedPct, 0: no limit).  Before it
// refuses, the folder's trash is emptied (it is a copy kept for days, and the room is worth more) and the disk is measured again.
func (f *Folder) room(incoming int64) error {
	lim := f.cfg.MaxUsedPct
	if lim <= 0 || lim >= 100 {
		return nil
	}
	if f.over(incoming, lim) {
		f.freeTrash()
		if f.over(incoming, lim) {
			return fmt.Errorf("%w (%d%%)", ErrDiskFull, lim)
		}
	}
	return nil
}

// over says whether taking in "incoming" more bytes would pass the limit.
func (f *Folder) over(incoming int64, lim int) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(f.root, &st); err != nil {
		return false // what cannot be measured does not stop the work
	}
	bs := uint64(st.Bsize)
	used := (uint64(st.Blocks) - uint64(st.Bfree)) * bs
	avail := uint64(st.Bavail) * bs
	total := used + avail
	if total == 0 {
		return false
	}
	if incoming < 0 {
		incoming = 0
	}
	return float64(used+uint64(incoming))*100 > float64(total)*float64(lim)
}

// freeTrash empties the trash of this folder, except what was put there in the last few seconds (a delete may be in the middle of
// moving into it), and says how many bytes that freed.
func (f *Folder) freeTrash() int64 {
	td := filepath.Join(f.root, MetaDir, "trash")
	ents, err := os.ReadDir(td)
	if err != nil {
		return 0
	}
	keep := f.now().Add(-10 * time.Second).UTC().Format("20060102-150405")
	var freed int64
	for _, e := range ents {
		if e.Name() >= keep {
			continue
		}
		filepath.WalkDir(filepath.Join(td, e.Name()), func(_ string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				if st, err := d.Info(); err == nil {
					freed += st.Size()
				}
			}
			return nil
		})
		os.RemoveAll(filepath.Join(td, e.Name()))
	}
	return freed
}

// guard passes on what is read and asks again whether there is room each few megabytes, so a file of unknown length stops where the
// limit is, not after it.
type guard struct {
	r    io.Reader
	f    *Folder
	n    int64
	last int64
}

const guardStep = 4 << 20

func (g *guard) Read(p []byte) (int, error) {
	n, err := g.r.Read(p)
	g.n += int64(n)
	if err == nil && g.n-g.last >= guardStep {
		g.last = g.n
		if e := g.f.room(guardStep); e != nil {
			return n, e
		}
	}
	return n, err
}
