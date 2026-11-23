package filesync

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Source is a peer, as the engine sees it.  The engine does no networking: the caller makes a Source from whatever connects the nodes.
type Source interface {
	// Index returns the peer's entries with a sequence number above after, and the highest sequence number the peer holds.
	Index(folder string, after int64) ([]FileInfo, int64, error)
	// Block returns size bytes at off of the file name, which must hash to hash.
	Block(folder, name string, off int64, size int, hash Hash) ([]byte, error)
}

// Config describes one folder on this node.
type Config struct {
	ID          string   // the folder's name, the same on every node
	Root        string   // where it lives on this node
	Node        NodeID   // this node
	DBDir       string   // where the index is kept: outside Root
	Ignore      []string // patterns (path.Match) for a name or a whole path; a trailing / matches directories only
	IgnorePerms bool     // do not sync or apply permission bits
	MaxFileSize int64    // larger files are neither scanned nor accepted (0: no limit)
	MaxUsedPct  int      // no file is written or fetched that would take the disk past this percent full (0, 100: no limit)
	TrashDays   int      // how long replaced and deleted files stay in the trash (0: 30)
	TombDays    int      // how long a deletion is remembered (0: 180)
	// Peers names the other nodes known to hold a version of a file, so a pull can take blocks from all of them at once: the more nodes
	// have a file, the faster it spreads.  Nil: only the peer that announced it.
	Peers func(name string, v Vector) []Source
	// Swarm names the other nodes that may be pulling the same file right now, which can be asked which blocks of it they have: a big
	// download is shared among them while it runs.  Their sources should also be Havers.
	Swarm      func() []Source
	Create     bool // make Root and its marker if missing: for a folder that is new on this node
	Now        func() time.Time
	WatchEvery time.Duration // how often a running sync asks the peer what it has deleted (default 2 s; tests make it short)
}

// Folder is one synced folder on this node.
type Folder struct {
	cfg    Config
	root   string
	ix     *index
	op     sync.Mutex // one scan, sync or maintenance at a time
	hook   func(stage string) error
	hookMu sync.Mutex
	flyMu  sync.Mutex
	fly    map[string]*flight // the downloads under way here, by name: what has arrived of them is served to the others
	dm     dooms              // what was deleted while a sync was running (doom.go)
	pq     pendingPuts
	tmu    sync.Mutex
	tStamp string    // the trash folder that deletions arriving from other nodes go into (see pullStamp)
	tLast  time.Time // when the last of them arrived
}

// Open opens a folder.  The folder must already hold its marker (MetaDir) unless Create is set: a folder that has lost it (an
// unmounted disk, a deleted directory) is not scanned, so its files are not taken for deleted.
func Open(cfg Config) (*Folder, error) {
	if cfg.Node == "" || cfg.Root == "" || cfg.DBDir == "" || cfg.ID == "" {
		return nil, fmt.Errorf("filesync: a folder needs an id, a root, a node and a database directory")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.TrashDays <= 0 {
		cfg.TrashDays = 30
	}
	if cfg.TombDays <= 0 {
		cfg.TombDays = 180
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, err
	}
	if cfg.Create {
		if err := os.MkdirAll(filepath.Join(root, MetaDir), 0o700); err != nil {
			return nil, err
		}
	}
	ix, err := openIndex(cfg.DBDir)
	if err != nil {
		return nil, err
	}
	f := &Folder{cfg: cfg, root: root, ix: ix}
	os.RemoveAll(filepath.Join(root, MetaDir, "tmp")) // partial files from a crash: never used, always fetched again
	return f, nil
}

func (f *Folder) Close() error { return f.ix.close() }

// Entries returns the index, sorted by name.  The slice is shared: read it, do not change it.
func (f *Folder) Entries() []FileInfo { return f.ix.snapshot() }

// Entry returns one entry.
func (f *Folder) Entry(name string) (FileInfo, bool) { return f.ix.get(name) }

// Index serves this node's entries above a sequence number to a peer.
func (f *Folder) Index(after int64) ([]FileInfo, int64) { return f.ix.after(after) }

func (f *Folder) now() time.Time { return f.cfg.Now() }

func (f *Folder) marker() error {
	st, err := os.Lstat(filepath.Join(f.root, MetaDir))
	if err != nil || !st.IsDir() {
		return ErrNoMarker
	}
	return nil
}

// ignored says whether a name is left out of the folder by the ignore patterns.
func (f *Folder) ignored(name string, isDir bool) bool {
	for _, p := range f.cfg.Ignore {
		dirOnly := strings.HasSuffix(p, "/")
		p = strings.TrimSuffix(p, "/")
		if dirOnly && !isDir {
			continue
		}
		if ok, _ := path.Match(p, name); ok {
			return true
		}
		if ok, _ := path.Match(p, path.Base(name)); ok {
			return true
		}
	}
	return false
}

// full is the path on disk of a name, after checking that no directory on the way is a link that leads out of the folder.  A name
// from a peer has already been through ValidName.  (This is a check, not a lock: another process that swaps a directory for a link
// between the check and the use could still get a write outside the folder.  Run the daemon as a user that owns only what it syncs
// where that matters.)
func (f *Folder) full(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	// ValidName refuses every way out already; this says it again on the name itself, before any path is made from it
	if !filepath.IsLocal(name) {
		return "", fmt.Errorf("%w: %s is outside the folder", ErrBadName, name)
	}
	p := f.root
	parts := strings.Split(name, "/")
	for _, c := range parts[:len(parts)-1] {
		p = filepath.Join(p, c)
		st, err := os.Lstat(p)
		if err != nil {
			if os.IsNotExist(err) {
				break // not there yet: made by the caller, inside what has been checked
			}
			return "", err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: %s goes through a link", ErrBadName, name)
		}
	}
	return filepath.Join(f.root, filepath.FromSlash(name)), nil
}

func (f *Folder) stage(s string) error {
	if f.hook != nil {
		return f.hook(s)
	}
	return nil
}

// conflictName is the name a losing file is kept under: name.sync-conflict-DATE-TIME-NODE.ext.
func (f *Folder) conflictName(name string, by NodeID) string {
	ext := path.Ext(name)
	base := strings.TrimSuffix(name, ext)
	if ext == "" || strings.HasSuffix(name, "/") {
		base, ext = name, ""
	}
	return fmt.Sprintf("%s.sync-conflict-%s-%s%s", base, f.now().UTC().Format("20060102-150405"), by, ext)
}
