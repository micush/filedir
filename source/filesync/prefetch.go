package filesync

import (
	"crypto/sha256"
	"sync"
)

// BlockReq names one block of one file.
type BlockReq struct {
	Name string
	Off  int64
	Size int
	Hash Hash
}

// BatchSource is a Source that can hand over many blocks in one request.  A nil slice in the answer is a block it could not give;
// the caller then asks for that one the usual way.  An error means the batch as a whole failed.
type BatchSource interface {
	Source
	Blocks(folder string, reqs []BlockReq) ([][]byte, error)
}

const (
	prefetchFiles = 64       // files in one batch
	prefetchBytes = 4 << 20  // bytes in one batch
	prefetchHold  = 16 << 20 // bytes the prefetcher holds ahead of the pulls
	smallFile     = minBlock // files of one block, up to this size, are batched
)

type pfKey struct {
	name string
	off  int64
	hash Hash
}

// prefetcher wraps a BatchSource.  A background goroutine fetches the small files of a sync a batch at a time, a request for many
// files instead of one for each; Block serves from what has arrived (waiting for a batch that is under way) and asks the peer the
// usual way for anything else.  Whatever the prefetcher gets wrong costs only the old way of asking: nothing from here is trusted
// without the hash check that every block gets in fetchOne.
type prefetcher struct {
	BatchSource
	mu     sync.Mutex
	cond   *sync.Cond
	cache  map[pfKey][]byte
	want   map[pfKey]bool // asked for, not arrived yet
	held   int
	stop   bool
	broken bool // the peer cannot batch
	done   chan struct{}
}

func newPrefetcher(src BatchSource, folder string, files []FileInfo) *prefetcher {
	p := &prefetcher{BatchSource: src, cache: map[pfKey][]byte{}, want: map[pfKey]bool{}, done: make(chan struct{})}
	p.cond = sync.NewCond(&p.mu)
	for _, fi := range files {
		p.want[pfKey{fi.Name, fi.Blocks[0].Off, fi.Blocks[0].Hash}] = true
	}
	go p.run(folder, files)
	return p
}

func (p *prefetcher) run(folder string, files []FileInfo) {
	defer close(p.done)
	defer func() { // whatever is still wanted will not come
		p.mu.Lock()
		p.want = map[pfKey]bool{}
		p.cond.Broadcast()
		p.mu.Unlock()
	}()
	for len(files) > 0 {
		var reqs []BlockReq
		bytes := 0
		for len(files) > 0 && len(reqs) < prefetchFiles && bytes < prefetchBytes {
			b := files[0].Blocks[0]
			reqs = append(reqs, BlockReq{files[0].Name, b.Off, int(b.Size), b.Hash})
			bytes += int(b.Size)
			files = files[1:]
		}
		p.mu.Lock()
		for p.held > prefetchHold && !p.stop {
			p.cond.Wait()
		}
		stop := p.stop
		p.mu.Unlock()
		if stop {
			return
		}
		out, err := p.BatchSource.Blocks(folder, reqs)
		p.mu.Lock()
		for i, r := range reqs {
			k := pfKey{r.Name, r.Off, r.Hash}
			delete(p.want, k)
			if err == nil && i < len(out) && out[i] != nil {
				p.cache[k] = out[i]
				p.held += len(out[i])
			}
		}
		if err != nil {
			p.broken = true
		}
		p.cond.Broadcast()
		p.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// Block serves a prefetched block once; anything else goes to the peer.
func (p *prefetcher) Block(folder, name string, off int64, size int, h Hash) ([]byte, error) {
	k := pfKey{name, off, h}
	p.mu.Lock()
	for p.want[k] {
		p.cond.Wait()
	}
	if d, ok := p.cache[k]; ok {
		delete(p.cache, k)
		p.held -= len(d)
		p.cond.Broadcast()
		p.mu.Unlock()
		if len(d) == size && sha256.Sum256(d) == h { // a bad batch is not trusted: ask the usual way
			return d, nil
		}
	} else {
		p.mu.Unlock()
	}
	return p.BatchSource.Block(folder, name, off, size, h)
}

func (p *prefetcher) close() {
	p.mu.Lock()
	p.stop = true
	p.cond.Broadcast()
	p.mu.Unlock()
	<-p.done
}

// batchable picks the entries of list that a sync will pull as whole small files: new here or newer than what is held.
func (f *Folder) batchable(list []FileInfo) []FileInfo {
	var out []FileInfo
	for _, fi := range list {
		if fi.Kind != KFile || fi.Deleted || len(fi.Blocks) != 1 || fi.Blocks[0].Size <= 0 || fi.Blocks[0].Size > smallFile {
			continue
		}
		if fi.Check(f.cfg.MaxFileSize) != nil || f.ignored(fi.Name, false) {
			continue
		}
		if l, have := f.ix.get(fi.Name); have && fi.Version.Compare(l.Version) != Greater {
			continue
		}
		out = append(out, fi)
	}
	return out
}
