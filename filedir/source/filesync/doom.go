package filesync

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// An item that is deleted while it is being replicated is not finished: the downloads under it stop, what has come of it is thrown away,
// the items under it that have not been started are skipped, and the deletion is applied.  Two things doom an item for the rest of the sync
// that is running: a deletion made on this node (Delete), and a deletion that the peer announces while the sync is under way (watchDeletes).

var errDoomed = errors.New("deleted while it was being replicated")

type doomEntry struct {
	ver Vector // a pull of a version this deletion dominates is dropped
	any bool   // the deletion was made here: everything pulled in this sync under it is dropped
}

type dooms struct {
	mu      sync.Mutex
	by      map[string]doomEntry
	syncing int
}

// doom marks name, and everything under it, as deleted for the sync that is running.  Without one there is nothing to stop.
func (f *Folder) doom(name string, e doomEntry) {
	f.dm.mu.Lock()
	defer f.dm.mu.Unlock()
	if f.dm.syncing == 0 {
		return
	}
	if f.dm.by == nil {
		f.dm.by = map[string]doomEntry{}
	}
	f.dm.by[name] = e
}

// doomedLocked says whether a pull of fi would bring back something that was deleted.  f.dm.mu must be held.
func (f *Folder) doomedLocked(fi FileInfo) bool {
	if len(f.dm.by) == 0 {
		return false
	}
	for n := fi.Name; ; {
		if e, ok := f.dm.by[n]; ok && (e.any || e.ver.Compare(fi.Version) == Greater) {
			return true
		}
		i := strings.LastIndexByte(n, '/')
		if i < 0 {
			return false
		}
		n = n[:i]
	}
}

func (f *Folder) doomed(fi FileInfo) bool {
	f.dm.mu.Lock()
	defer f.dm.mu.Unlock()
	return f.doomedLocked(fi)
}

func (f *Folder) syncBegin() {
	f.dm.mu.Lock()
	f.dm.syncing++
	f.dm.mu.Unlock()
}

func (f *Folder) syncEnd() {
	f.dm.mu.Lock()
	if f.dm.syncing--; f.dm.syncing == 0 {
		f.dm.by = nil
	}
	f.dm.mu.Unlock()
}

// watchDeletes asks the peer, while the sync runs, what it has deleted since the list was taken.  What the peer deleted is doomed, so the
// pulls under it stop; the deletions themselves are handed to late, to be applied before the sync ends.  stop ends it.
func (f *Folder) watchDeletes(src Source, max int64, every time.Duration, stop <-chan struct{}, late func([]FileInfo)) {
	seen := map[string]bool{}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		list, _, err := src.Index(f.cfg.ID, max)
		if err != nil {
			continue // the sync itself reports a peer that has gone
		}
		var dels []FileInfo
		for _, fi := range list {
			if !fi.Deleted || fi.Check(f.cfg.MaxFileSize) != nil || f.ignored(fi.Name, fi.Kind == KDir) {
				continue
			}
			key := fi.Name + "\x00" + fi.Version.String()
			if seen[key] {
				continue
			}
			seen[key] = true
			f.doom(fi.Name, doomEntry{ver: fi.Version})
			dels = append(dels, fi)
		}
		if len(dels) > 0 {
			late(dels)
		}
	}
}

// watchEvery is how often the peer is asked what it has deleted: often enough to stop a big download soon, rarely enough to be free.
func (f *Folder) watchEvery() time.Duration {
	if f.cfg.WatchEvery > 0 {
		return f.cfg.WatchEvery
	}
	return 2 * time.Second
}
