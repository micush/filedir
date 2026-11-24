package filesync

import (
	"os"
	"sync"
)

// syncGroup lets callers that each need "a sync that starts after my write" share one.  A call joins the batch that has not started yet;
// one caller at a time runs the batches in turn.  A batch starts after every call in it has written, so its sync covers them all.
type syncGroup struct {
	mu      sync.Mutex
	pending *syncBatch
	active  bool
}

type syncBatch struct {
	done chan struct{}
	err  error
}

func (g *syncGroup) do(fn func() error) error {
	g.mu.Lock()
	b := g.pending
	if b == nil {
		b = &syncBatch{done: make(chan struct{})}
		g.pending = b
	}
	lead := !g.active
	if lead {
		g.active = true
	}
	g.mu.Unlock()
	if !lead {
		<-b.done
		return b.err
	}
	for {
		g.mu.Lock()
		cur := g.pending
		g.pending = nil
		if cur == nil {
			g.active = false
			g.mu.Unlock()
			break
		}
		g.mu.Unlock()
		cur.err = fn()
		close(cur.done)
	}
	<-b.done
	return b.err
}

// dirSyncs shares the sync of one directory between the files that were just moved into it.
var dirSyncs sync.Map // path -> *syncGroup

func syncDirShared(d string) {
	g, _ := dirSyncs.LoadOrStore(d, &syncGroup{})
	g.(*syncGroup).do(func() error {
		fh, err := os.Open(d)
		if err != nil {
			return err
		}
		defer fh.Close()
		return fh.Sync()
	})
}
