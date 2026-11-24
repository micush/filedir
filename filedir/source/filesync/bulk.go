package filesync

import "sync"

// A folder that was deleted elsewhere arrives as one deletion for each file in it, tens of thousands of them.  Each one used to be written to
// the index with a sync of its own, one after another, which is what made a big deletion take minutes.  While a sync runs the deletions are
// collected and written a few hundred at a time, one sync for each; the position in the peer's index is kept only after the last of them is
// in.  A node that stops in between has taken the files off the disk without recording it: the next scan records the deletion, as it does
// for a file a person removed.

const putBatch = 256

type pendingPuts struct {
	mu   sync.Mutex
	on   bool
	list []FileInfo
}

func (q *pendingPuts) begin() { q.mu.Lock(); q.on = true; q.mu.Unlock() }
func (q *pendingPuts) end()   { q.mu.Lock(); q.on = false; q.list = nil; q.mu.Unlock() }

// putDeleted records a deletion that was applied to the disk: collected while a sync runs, otherwise written at once.
func (f *Folder) putDeleted(fi FileInfo) error {
	f.pq.mu.Lock()
	if !f.pq.on {
		f.pq.mu.Unlock()
		return f.ix.put(fi)
	}
	f.pq.list = append(f.pq.list, fi)
	var out []FileInfo
	if len(f.pq.list) >= putBatch {
		out, f.pq.list = f.pq.list, nil
	}
	f.pq.mu.Unlock()
	return f.ix.put(out...)
}

// flushPuts writes what has been collected.
func (f *Folder) flushPuts() error {
	f.pq.mu.Lock()
	out := f.pq.list
	f.pq.list = nil
	f.pq.mu.Unlock()
	return f.ix.put(out...)
}
