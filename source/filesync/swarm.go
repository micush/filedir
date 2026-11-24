package filesync

import (
	"crypto/sha256"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"sync"
	"time"
)

// Sharing a download while it runs.  A node that is pulling a big file says which blocks of it it already holds, and serves them to the
// others that are pulling the same version.  Each node takes its blocks in a different order, so they have different ones to trade, and
// every node helps every other: the first copy of a file spreads over all the uplinks, not just the first one's.

// swarmEvery is how often a node that is pulling a file asks the others which blocks of it they have.
var swarmEvery = time.Second

// swarmMin is how many blocks a file needs before its download is shared: for fewer it is not worth the asking.
const swarmMin = 8

// Availability says which blocks of one version of a file a node holds right now.
type Availability struct {
	Complete bool   // the whole file, in place
	N        int    // the number of blocks (when Bits is set)
	Bits     []byte // bit i set: block i is held
}

// Has says whether block i is held.
func (a Availability) Has(i int) bool {
	if a.Complete {
		return true
	}
	return i >= 0 && i < a.N && i>>3 < len(a.Bits) && a.Bits[i>>3]&(1<<(uint(i)&7)) != 0
}

// Haver is a Source that can say which blocks it holds of a file that it may still be pulling.
type Haver interface {
	Have(folder, name string, v Vector) (Availability, error)
}

// flight is one download that is under way here.
type flight struct {
	fi   FileInfo
	path string // the temporary file
	mu   sync.Mutex
	have []bool
}

func (fl *flight) index(off int64) int {
	i := sort.Search(len(fl.fi.Blocks), func(i int) bool { return fl.fi.Blocks[i].Off >= off })
	if i < len(fl.fi.Blocks) && fl.fi.Blocks[i].Off == off {
		return i
	}
	return -1
}

func (fl *flight) mark(i int) {
	fl.mu.Lock()
	fl.have[i] = true
	fl.mu.Unlock()
}

func (f *Folder) flyStart(fi FileInfo, path string) *flight {
	if len(fi.Blocks) < swarmMin {
		return nil
	}
	fl := &flight{fi: fi, path: path, have: make([]bool, len(fi.Blocks))}
	f.flyMu.Lock()
	if f.fly == nil {
		f.fly = map[string]*flight{}
	}
	f.fly[fi.Name] = fl
	f.flyMu.Unlock()
	return fl
}

func (f *Folder) flyEnd(fl *flight) {
	if fl == nil {
		return
	}
	f.flyMu.Lock()
	if f.fly[fl.fi.Name] == fl {
		delete(f.fly, fl.fi.Name)
	}
	f.flyMu.Unlock()
}

func (f *Folder) flightOf(name string) *flight {
	f.flyMu.Lock()
	defer f.flyMu.Unlock()
	return f.fly[name]
}

// Have says what this node holds of a version of a file: all of it, or the blocks that have come so far of a pull that is under way.
func (f *Folder) Have(name string, v Vector) Availability {
	if l, ok := f.ix.get(name); ok && !l.Deleted && l.Kind == KFile && l.Version.Compare(v) == Equal {
		return Availability{Complete: true}
	}
	fl := f.flightOf(name)
	if fl == nil || fl.fi.Version.Compare(v) != Equal {
		return Availability{}
	}
	fl.mu.Lock()
	defer fl.mu.Unlock()
	a := Availability{N: len(fl.have), Bits: make([]byte, (len(fl.have)+7)/8)}
	for i, h := range fl.have {
		if h {
			a.Bits[i>>3] |= 1 << (uint(i) & 7)
		}
	}
	return a
}

// flightBlock serves a block that has arrived of a download that is under way here.
func (f *Folder) flightBlock(name string, off int64, size int, hash Hash) ([]byte, bool) {
	fl := f.flightOf(name)
	if fl == nil {
		return nil, false
	}
	i := fl.index(off)
	if i < 0 || int(fl.fi.Blocks[i].Size) != size || fl.fi.Blocks[i].Hash != hash {
		return nil, false
	}
	fl.mu.Lock()
	ok := fl.have[i]
	fl.mu.Unlock()
	if !ok {
		return nil, false
	}
	d, err := readBlock(fl.path, off, size, hash)
	return d, err == nil
}

// swarmSource is what the scheduler knows of one peer.
type swarmSource struct {
	src      Source
	complete bool // holds the whole file
	bits     Availability
	busy     int
	fails    int // probes that failed in a row: a peer that cannot say is given up on
	probe    bool
}

func (x *swarmSource) has(i int) bool { return x.complete || x.bits.Has(i) }

// fetch gets the blocks in want into tf.  The sources are the node that sent the entry and every node known to hold the same version
// (Config.Peers), which have all of it, and, for a big file, the other nodes (Config.Swarm), which may have some of it: they are asked
// every second which blocks they have.  A block is asked for from the source with the fewest requests running that holds it, up to
// perSource at a time; one that fails is asked for from another, and the one that sent the entry is the last resort.  Every block is
// checked against its hash, so a peer that lies or sends rubbish only costs time.
func (f *Folder) fetch(tf *os.File, fi FileInfo, want []Block, first Source, fl *flight) error {
	if len(want) == 0 {
		return nil
	}
	srcs := []*swarmSource{{src: first, complete: true}}
	seen := map[Source]bool{first: true}
	if f.cfg.Peers != nil {
		for _, s := range f.cfg.Peers(fi.Name, fi.Version) {
			if s != nil && !seen[s] {
				seen[s] = true
				srcs = append(srcs, &swarmSource{src: s, complete: true})
			}
		}
	}
	if fl != nil && f.cfg.Swarm != nil {
		for _, s := range f.cfg.Swarm() {
			if _, ok := s.(Haver); ok && s != nil && !seen[s] {
				seen[s] = true
				srcs = append(srcs, &swarmSource{src: s, probe: true})
			}
		}
	}
	// the index of each wanted block in the file, and the order they are taken in: a shared download takes them in a random order, so
	// that the nodes have different blocks to give each other
	fidx := make([]int, len(want))
	for k, b := range want {
		fidx[k] = sort.Search(len(fi.Blocks), func(i int) bool { return fi.Blocks[i].Off >= b.Off })
	}
	todo := make([]int, len(want))
	for k := range todo {
		todo[k] = k
	}
	if fl != nil && len(srcs) > 1 {
		rand.Shuffle(len(todo), func(i, j int) { todo[i], todo[j] = todo[j], todo[i] })
	}
	workers := len(srcs) * perSource
	if workers > maxWorkers {
		workers = maxWorkers
	}
	if workers > len(want) {
		workers = len(want)
	}
	var (
		mu      sync.Mutex
		cond    = sync.NewCond(&mu)
		running int
		failed  error
		tried   = make([][]bool, len(want)) // tried[k][s]: block k was asked of source s and did not come
		stop    = make(chan struct{})
	)
	for k := range tried {
		tried[k] = make([]bool, len(srcs))
	}
	// say which blocks the sources that may have some have, once a second, and wake the workers when it changes
	var pw sync.WaitGroup
	for si, x := range srcs {
		if !x.probe {
			continue
		}
		pw.Add(1)
		go func(si int, x *swarmSource) {
			defer pw.Done()
			h := x.src.(Haver)
			for {
				a, err := h.Have(f.cfg.ID, fi.Name, fi.Version)
				mu.Lock()
				if err != nil {
					x.fails++
				} else {
					x.fails, x.complete, x.bits = 0, a.Complete, a
				}
				cond.Broadcast()
				give := x.fails >= 3
				mu.Unlock()
				if give {
					return
				}
				select {
				case <-stop:
					return
				case <-time.After(swarmEvery):
				}
			}
		}(si, x)
	}
	// the workers also wake by themselves, in case a change was missed
	go func() {
		t := time.NewTicker(300 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				mu.Lock()
				cond.Broadcast()
				mu.Unlock()
			}
		}
	}()
	// a block can still be had if a source that holds all of it has not been tried for it
	canStill := func(k int) bool {
		for si, x := range srcs {
			if x.complete && !tried[k][si] {
				return true
			}
		}
		return false
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			defer mu.Unlock()
			for {
				if failed == nil && f.doomed(fi) { // deleted while it was coming: stop
					failed = errDoomed
				}
				if failed != nil || (len(todo) == 0 && running == 0) {
					cond.Broadcast()
					return
				}
				pick, from := -1, -1
				for ti, k := range todo {
					best := -1
					for si, x := range srcs {
						if x.busy >= perSource || tried[k][si] || !x.has(fidx[k]) {
							continue
						}
						if best < 0 || x.busy < srcs[best].busy {
							best = si
						}
					}
					if best >= 0 {
						pick, from = ti, best
						break
					}
				}
				if pick < 0 {
					cond.Wait()
					continue
				}
				k := todo[pick]
				todo = append(todo[:pick], todo[pick+1:]...)
				x := srcs[from]
				x.busy++
				running++
				mu.Unlock()
				err := f.fetchOne(tf, fi, want[k], x.src)
				if err == nil && fl != nil && fidx[k] < len(fi.Blocks) {
					fl.mark(fidx[k])
				}
				mu.Lock()
				x.busy--
				running--
				if err != nil {
					tried[k][from] = true
					if x.probe { // it said it had this one and it did not: take it off its list
						x.bits = Availability{}
					}
					if !canStill(k) {
						if failed == nil {
							failed = err
						}
					} else {
						todo = append([]int{k}, todo...)
					}
				}
				cond.Broadcast()
			}
		}()
	}
	wg.Wait()
	close(stop)
	pw.Wait()
	mu.Lock()
	err := failed
	mu.Unlock()
	return err
}

// fetchOne asks one source for one block, checks it and writes it.
func (f *Folder) fetchOne(tf *os.File, fi FileInfo, b Block, s Source) error {
	d, err := s.Block(f.cfg.ID, fi.Name, b.Off, int(b.Size), b.Hash)
	if err != nil {
		return err
	}
	_, recycle := s.(BlockRecycler)
	if len(d) != int(b.Size) || sha256.Sum256(d) != b.Hash {
		if recycle {
			PutBuf(d)
		}
		return fmt.Errorf("%w: a block of %s does not match its hash", ErrBadInfo, fi.Name)
	}
	_, err = tf.WriteAt(d, b.Off)
	if recycle {
		PutBuf(d)
	}
	if err != nil {
		return err
	}
	f.hookMu.Lock()
	err = f.stage("fetched")
	f.hookMu.Unlock()
	return err
}

// Transfer is a file this node is pulling now, as far as it has come.
type Transfer struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Blocks int    `json:"blocks"`
	Done   int    `json:"done"`
}

// Transfers lists the large files being pulled (small ones are over before they could be shown), largest first.
func (f *Folder) Transfers() []Transfer {
	f.flyMu.Lock()
	fls := make([]*flight, 0, len(f.fly))
	for _, fl := range f.fly {
		fls = append(fls, fl)
	}
	f.flyMu.Unlock()
	out := make([]Transfer, 0, len(fls))
	for _, fl := range fls {
		fl.mu.Lock()
		n := 0
		for _, h := range fl.have {
			if h {
				n++
			}
		}
		fl.mu.Unlock()
		out = append(out, Transfer{Name: fl.fi.Name, Size: fl.fi.Size, Blocks: len(fl.have), Done: n})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Size > out[j].Size || out[i].Size == out[j].Size && out[i].Name < out[j].Name
	})
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}
