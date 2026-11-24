package filesync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// What the sync did that a person would not expect (a folder that was deleted elsewhere and is kept here, a change that beat a deletion) is
// noted while it runs and said once when it is done: the first few, and how many more there were.  Without it a folder that comes back
// can only be explained by guessing.

const notesShown = 12

type notes struct {
	mu   sync.Mutex
	list []string
	more int
}

func (n *notes) add(s string, logf func(string, ...any)) {
	if logf == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.list) < notesShown {
		n.list = append(n.list, s)
	} else {
		n.more++
	}
}

func (n *notes) flush(logf func(string, ...any)) {
	n.mu.Lock()
	list, more := n.list, n.more
	n.list, n.more = nil, 0
	n.mu.Unlock()
	if logf == nil {
		return
	}
	for _, s := range list {
		logf("sync: %s", s)
	}
	if more > 0 {
		logf("sync: and %d more like these", more)
	}
}

// describeDir says what is in a folder on this disk, as far as the first few entries tell: each with whether the index knows it.
func (f *Folder) describeDir(rel string) string {
	p, err := f.full(rel)
	if err != nil {
		return err.Error()
	}
	ents, err := os.ReadDir(p)
	if err != nil {
		return err.Error()
	}
	var out []string
	for i, e := range ents {
		if i == 5 {
			out = append(out, fmt.Sprintf("and %d more", len(ents)-5))
			break
		}
		n := rel + "/" + e.Name()
		state := "not in the index"
		if l, ok := f.ix.get(n); ok {
			state = "in the index, live"
			if l.Deleted {
				state = "in the index, deleted"
			}
		}
		kind := "file"
		switch {
		case e.IsDir():
			kind = "folder"
		case e.Type()&os.ModeSymlink != 0:
			kind = "link"
		}
		if f.ignored(n, e.IsDir()) {
			state = "left out of the sync"
		}
		out = append(out, fmt.Sprintf("%s %s (%s)", kind, filepath.Base(n), state))
	}
	if len(out) == 0 {
		return "it holds nothing"
	}
	return "it holds " + strings.Join(out, ", ")
}
