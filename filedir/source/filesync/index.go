package filesync

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// index is the folder's list of entries, kept in memory and in a journal on disk: every change is appended as one line, and the
// file is rewritten when it holds much more than the live entries.  It lives outside the synced folder.
type index struct {
	mu      sync.RWMutex
	files   map[string]*FileInfo
	seq     int64
	cursors map[NodeID]int64 // for each peer, how far into its index we have read
	path    string
	jf      *os.File
	lines   int
	sg      syncGroup  // shares journal syncs between writers
	snap    []FileInfo // the sorted copy snapshot hands out, kept until the index changes (nil: not built)
	alg     string     // the hash of the blocks, as the journal's first line says ("": SHA-256, from before it was said)
}

type journalLine struct {
	Alg    string    `json:"alg,omitempty"` // the hash of the blocks in the entries (HashName): the first line of a journal; none: SHA-256
	File   *FileInfo `json:"f,omitempty"`
	Drop   string    `json:"drop,omitempty"` // an entry removed for good (an old tombstone)
	Cursor *cursorLn `json:"c,omitempty"`
}

type cursorLn struct {
	Node NodeID `json:"n"`
	Seq  int64  `json:"s"`
}

// ErrLegacyIndex is what Open says of an index made when the blocks were hashed with SHA-256: MigrateIndex makes it a current one.
var ErrLegacyIndex = errors.New("filesync: the index has SHA-256 block hashes and has to be converted")

func openIndex(dir string) (*index, error) { return loadIndex(dir, false) }

func loadIndex(dir string, allowLegacy bool) (*index, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ix := &index{files: map[string]*FileInfo{}, cursors: map[NodeID]int64{}, path: filepath.Join(dir, "index.jsonl")}
	if f, err := os.Open(ix.path); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 64<<20)
		for sc.Scan() {
			var ln journalLine
			if json.Unmarshal(sc.Bytes(), &ln) != nil {
				continue // a line cut short by a crash: the lines before it are kept
			}
			ix.lines++
			ix.apply(ln)
		}
		f.Close()
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if ix.lines > 0 && ix.alg != HashName && !allowLegacy {
		return nil, ErrLegacyIndex
	}
	var err error
	if ix.jf, err = os.OpenFile(ix.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600); err != nil {
		return nil, err
	}
	if ix.lines == 0 { // a new index says what its hashes are, in its first line
		b, _ := json.Marshal(journalLine{Alg: HashName})
		if _, err := ix.jf.Write(append(b, '\n')); err != nil {
			ix.jf.Close()
			return nil, err
		}
		ix.jf.Sync()
		ix.alg = HashName
		ix.lines = 1
	}
	return ix, nil
}

func (ix *index) apply(ln journalLine) {
	switch {
	case ln.Alg != "":
		ix.alg = ln.Alg
	case ln.File != nil:
		ix.snap = nil
		ix.files[ln.File.Name] = ln.File
		if ln.File.Seq > ix.seq {
			ix.seq = ln.File.Seq
		}
	case ln.Drop != "":
		ix.snap = nil
		delete(ix.files, ln.Drop)
	case ln.Cursor != nil:
		ix.cursors[ln.Cursor.Node] = ln.Cursor.Seq
	}
}

// commit appends the lines to the journal (the caller then calls syncJournal, outside the lock, so that writers that come together
// share one sync).  The caller holds ix.mu.
func (ix *index) commit(lns []journalLine) error {
	w := bufio.NewWriter(ix.jf)
	for _, ln := range lns {
		b, err := json.Marshal(ln)
		if err != nil {
			return err
		}
		w.Write(b)
		w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil {
		return err
	}
	ix.lines += len(lns)
	if ix.lines > 1000 && ix.lines > 3*(len(ix.files)+len(ix.cursors)) {
		return ix.compact()
	}
	return nil
}

// compact rewrites the journal with one line per live entry, then swaps it in.
func (ix *index) compact() error {
	tmp := ix.path + ".new"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	n := 0
	names := make([]string, 0, len(ix.files))
	for k := range ix.files {
		names = append(names, k)
	}
	sort.Strings(names)
	write := func(ln journalLine) {
		b, _ := json.Marshal(ln)
		w.Write(b)
		w.WriteByte('\n')
		n++
	}
	write(journalLine{Alg: HashName}) // the hash of every block in the entries that follow: the caller has them in it
	for _, k := range names {
		write(journalLine{File: ix.files[k]})
	}
	for node, s := range ix.cursors {
		write(journalLine{Cursor: &cursorLn{node, s}})
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if err := os.Rename(tmp, ix.path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(ix.path)); err == nil {
		d.Sync()
		d.Close()
	}
	ix.jf.Close()
	if ix.jf, err = os.OpenFile(ix.path, os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
		return err
	}
	ix.lines = n
	return nil
}

func (ix *index) close() error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.jf.Close()
}

// get returns a copy of an entry.
func (ix *index) get(name string) (FileInfo, bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	fi, ok := ix.files[name]
	if !ok {
		return FileInfo{}, false
	}
	return *fi, true
}

// put stores entries, giving each the next sequence number, and writes them to the journal.  The entries it keeps are copies.
func (ix *index) put(fis ...FileInfo) error {
	if len(fis) == 0 {
		return nil
	}
	// The peer reads the index in pages, in the order of the positions given here.  What was deleted is therefore given positions with
	// what is under a folder before the folder: a page that ends in the middle must not hand over a folder's deletion before the deletions
	// of what is in it, because the folder cannot be removed yet and was kept, and came back on every node as an empty folder.
	fis = deletionsLast(fis)
	ix.mu.Lock()
	ix.snap = nil
	lns := make([]journalLine, 0, len(fis))
	for _, fi := range fis {
		ix.seq++
		fi.Seq = ix.seq
		fi.Version = fi.Version.Copy()
		c := fi
		ix.files[fi.Name] = &c
		lns = append(lns, journalLine{File: &c})
	}
	err := ix.commit(lns)
	ix.mu.Unlock()
	if err != nil {
		return err
	}
	return ix.syncJournal()
}

func (ix *index) drop(names []string) error {
	if len(names) == 0 {
		return nil
	}
	ix.mu.Lock()
	ix.snap = nil
	lns := make([]journalLine, 0, len(names))
	for _, n := range names {
		delete(ix.files, n)
		lns = append(lns, journalLine{Drop: n})
	}
	err := ix.commit(lns)
	ix.mu.Unlock()
	if err != nil {
		return err
	}
	return ix.syncJournal()
}

func (ix *index) setCursor(n NodeID, s int64) error {
	ix.mu.Lock()
	ix.cursors[n] = s
	err := ix.commit([]journalLine{{Cursor: &cursorLn{n, s}}})
	ix.mu.Unlock()
	if err != nil {
		return err
	}
	return ix.syncJournal()
}

// syncJournal makes what commit wrote durable.  Writers that arrive while a sync runs share the next one, so a hundred files
// recorded together cost a few syncs, not a hundred.  Each caller returns only after a sync that began after its own write.
func (ix *index) syncJournal() error {
	return ix.sg.do(func() error {
		ix.mu.Lock()
		jf := ix.jf
		ix.mu.Unlock()
		if err := jf.Sync(); err != nil && !errors.Is(err, os.ErrClosed) { // closed by a compaction, which synced the new file
			return err
		}
		return nil
	})
}

func (ix *index) cursor(n NodeID) int64 {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.cursors[n]
}

// snapshot returns every entry, by name.  The slice is shared by every caller until the index next changes (copying and sorting a
// large index on every listing took seconds): it must be read, never changed.
func (ix *index) snapshot() []FileInfo {
	ix.mu.RLock()
	if s := ix.snap; s != nil {
		ix.mu.RUnlock()
		return s
	}
	ix.mu.RUnlock()
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.snap != nil {
		return ix.snap
	}
	out := make([]FileInfo, 0, len(ix.files))
	for _, fi := range ix.files {
		out = append(out, *fi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	ix.snap = out
	return out
}

// after returns the entries with a sequence number above s, oldest first, and the highest sequence number in the index.
func (ix *index) after(s int64) ([]FileInfo, int64) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	var out []FileInfo
	for _, fi := range ix.files {
		if fi.Seq > s {
			out = append(out, *fi)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, ix.seq
}

// deletionsLast puts the entries that are not deletions first, as they were given, and then the deletions, the deepest first.
func deletionsLast(fis []FileInfo) []FileInfo {
	n := 0
	for _, fi := range fis {
		if fi.Deleted {
			n++
		}
	}
	if n == 0 {
		return fis
	}
	out := make([]FileInfo, 0, len(fis))
	dels := make([]FileInfo, 0, n)
	for _, fi := range fis {
		if fi.Deleted {
			dels = append(dels, fi)
		} else {
			out = append(out, fi)
		}
	}
	sort.SliceStable(dels, func(i, j int) bool {
		return strings.Count(dels[i].Name, "/") > strings.Count(dels[j].Name, "/")
	})
	return append(out, dels...)
}
