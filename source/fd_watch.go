package main

import (
	"encoding/binary"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Change detection.  A synced folder is scanned every 30 seconds (fdScanEvery), which is how a change made behind the daemon's back was
// found at all.  This watches the folder's directories with inotify, so a file that is created, closed after a write, moved, removed
// or changed in mode is found within about a second instead, and the scan that follows tells the other nodes.  The 30 second scan stays
// as the safety net: it finds what a watch cannot (a folder on a network file system sends no events, the kernel's queue overflows, the
// watch limit is reached).
//
// inotify is not recursive: every directory has a watch of its own, added as the directory is found, and a directory that appears later
// is watched as soon as its creation is seen (and scanned at once, because files may already be in it).  A directory the ignore patterns
// leave out, the metadata folder and a link to a directory are not watched.

const (
	fdWatchMask = syscall.IN_CREATE | syscall.IN_DELETE | syscall.IN_CLOSE_WRITE | syscall.IN_MOVED_FROM | syscall.IN_MOVED_TO |
		syscall.IN_DELETE_SELF | syscall.IN_MOVE_SELF | syscall.IN_ATTRIB | syscall.IN_ONLYDIR | syscall.IN_EXCL_UNLINK | inDontFollow
	inDontFollow = 0x02000000 // IN_DONT_FOLLOW: missing from package syscall

	fdWatchQuiet = 400 * time.Millisecond // a scan starts this long after the last event...
	fdWatchMax   = 3 * time.Second        // ...but never later than this after the first, however busy the folder is
)

type fdWatch struct {
	f    *fdFolder
	file *os.File
	fd   int

	mu     sync.Mutex
	dirs   map[int32]string // watch → directory, relative to the folder ("" is the top)
	closed bool
	first  time.Time // the first event of the burst waiting for its scan
	timer  *time.Timer
	warned bool
}

// startFdWatch starts watching f, or returns nil (and says why once) when it cannot: the folder is then only scanned on the timer.
func startFdWatch(f *fdFolder) *fdWatch {
	if f.f == nil {
		return nil
	}
	fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		warnf("files: %s: cannot watch for changes (%v): scanning every %s instead", f.cfg.ID, err, fdScanEvery)
		return nil
	}
	w := &fdWatch{f: f, fd: fd, file: os.NewFile(uintptr(fd), "inotify"), dirs: map[int32]string{}}
	w.addTree("")
	go w.loop()
	return w
}

// Close stops the watch.
func (w *fdWatch) Close() {
	w.mu.Lock()
	w.closed = true
	if w.timer != nil {
		w.timer.Stop()
	}
	w.mu.Unlock()
	w.file.Close() // ends the read in loop
}

// addTree watches the directory rel and every directory under it.
func (w *fdWatch) addTree(rel string) {
	root := w.f.cfg.Path
	start := filepath.Join(root, filepath.FromSlash(rel))
	filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil // a link or a file: nothing to watch (a link is not followed)
		}
		r, _ := filepath.Rel(root, p)
		r = filepath.ToSlash(r)
		if r == "." {
			r = ""
		}
		if r != "" && w.f.f.Ignored(r, true) {
			return filepath.SkipDir
		}
		w.add(p, r)
		return nil
	})
}

func (w *fdWatch) add(p, rel string) {
	wd, err := syscall.InotifyAddWatch(w.fd, p, fdWatchMask)
	if err != nil {
		if err == syscall.ENOSPC && !w.warned {
			w.warned = true
			warnf("files: %s: the limit of inotify watches is reached (fs.inotify.max_user_watches): changes in the folders without one are found by the scan every %s", w.f.cfg.ID, fdScanEvery)
		}
		return
	}
	w.mu.Lock()
	w.dirs[int32(wd)] = rel // the same directory gives the same watch number again
	w.mu.Unlock()
}

func (w *fdWatch) loop() {
	buf := make([]byte, 64<<10)
	for {
		n, err := w.file.Read(buf)
		if err != nil {
			return
		}
		hit := false
		for off := 0; off+syscall.SizeofInotifyEvent <= n; {
			wd := int32(binary.NativeEndian.Uint32(buf[off:]))
			mask := binary.NativeEndian.Uint32(buf[off+4:])
			nameLen := int(binary.NativeEndian.Uint32(buf[off+12:]))
			end := off + syscall.SizeofInotifyEvent + nameLen
			if end > n {
				break
			}
			name := string(trimNul(buf[off+syscall.SizeofInotifyEvent : end]))
			off = end
			if mask&syscall.IN_Q_OVERFLOW != 0 { // events were lost: look at everything again
				w.addTree("")
				hit = true
				continue
			}
			w.mu.Lock()
			dir, known := w.dirs[wd]
			if mask&syscall.IN_IGNORED != 0 { // the directory is gone
				delete(w.dirs, wd)
			}
			w.mu.Unlock()
			if !known || mask&syscall.IN_IGNORED != 0 {
				continue
			}
			rel := dir
			if name != "" {
				rel = strings.TrimPrefix(dir+"/"+name, "/")
			}
			if rel == ".filedir" || strings.HasPrefix(rel, ".filedir/") || w.f.f.Ignored(rel, mask&syscall.IN_ISDIR != 0) {
				continue
			}
			if mask&syscall.IN_ISDIR != 0 && mask&(syscall.IN_MOVED_FROM|syscall.IN_MOVED_TO) != 0 {
				w.addTree("") // a directory moved: the same watches now belong to other names, so every name is taken again
			} else if mask&syscall.IN_ISDIR != 0 && mask&syscall.IN_CREATE != 0 {
				w.addTree(rel) // a new directory: watched now, and what is already in it is found by the scan below
			}
			hit = true
		}
		if hit {
			w.kick()
		}
	}
}

// kick schedules a scan: soon after the events stop, and not later than fdWatchMax after they began.
func (w *fdWatch) kick() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	now := time.Now()
	if w.timer == nil || w.first.IsZero() {
		w.first = now
	}
	wait := fdWatchQuiet
	if left := fdWatchMax - now.Sub(w.first); left < wait {
		wait = max(left, 0)
	}
	if w.timer != nil {
		w.timer.Stop()
	}
	w.timer = time.AfterFunc(wait, func() {
		w.mu.Lock()
		w.first = time.Time{}
		closed := w.closed
		w.mu.Unlock()
		if !closed {
			w.f.scanNow()
		}
	})
}
