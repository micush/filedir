package filesync

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// swarmPeer is a node as another node sees it: it answers for blocks and for which blocks it has.
type swarmPeer struct {
	f      *Folder
	delay  time.Duration
	served atomic.Int64
	mode   string // "": honest; "liar": says it has all and gives nothing; "corrupt": gives wrong bytes
}

func (p *swarmPeer) Index(folder string, after int64) ([]FileInfo, int64, error) {
	e, m := p.f.Index(after)
	return e, m, nil
}

func (p *swarmPeer) Block(folder, name string, off int64, size int, h Hash) ([]byte, error) {
	time.Sleep(p.delay)
	if p.mode == "liar" {
		return nil, errors.New("not here")
	}
	d, err := p.f.Block(name, off, size, h)
	if err != nil {
		return nil, err
	}
	p.served.Add(1)
	if p.mode == "corrupt" {
		d = append([]byte(nil), d...)
		d[0] ^= 0xff
	}
	return d, nil
}

func (p *swarmPeer) Have(folder, name string, v Vector) (Availability, error) {
	if p.mode != "" {
		return Availability{Complete: true}, nil
	}
	return p.f.Have(name, v), nil
}

func writeBig(t *testing.T, n *testNode, name string, blocks int) []byte {
	t.Helper()
	data := make([]byte, blocks*minBlock)
	rand.Read(data)
	if err := os.WriteFile(n.path(name), data, 0o644); err != nil {
		t.Fatal(err)
	}
	n.scan(t)
	return data
}

// Two nodes pulling the same big file from the one that has it hand each other blocks while they pull: what the origin served is less
// than twice the file, and both end with the file.
func TestNodesShareABigDownloadWhileItRuns(t *testing.T) {
	old := swarmEvery
	swarmEvery = 10 * time.Millisecond
	defer func() { swarmEvery = old }()
	var pb, pc *swarmPeer
	b := newNode(t, "b", func(c *Config) { c.Swarm = func() []Source { return []Source{pc} } })
	c := newNode(t, "c", func(c *Config) { c.Swarm = func() []Source { return []Source{pb} } })
	a := newNode(t, "a")
	pb, pc = &swarmPeer{f: b.f}, &swarmPeer{f: c.f}
	data := writeBig(t, a, "big.bin", 160)
	origin := &swarmPeer{f: a.f, delay: 20 * time.Millisecond}
	var wg sync.WaitGroup
	for _, n := range []*testNode{b, c} {
		wg.Add(1)
		go func(n *testNode) {
			defer wg.Done()
			if _, err := n.f.SyncFrom("a", origin); err != nil {
				t.Error(err)
			}
		}(n)
	}
	wg.Wait()
	for _, n := range []*testNode{b, c} {
		got, err := os.ReadFile(n.path("big.bin"))
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%s did not end with the file (%v)", n.id, err)
		}
	}
	shared := pb.served.Load() + pc.served.Load()
	t.Logf("origin served %d blocks, the two pullers %d between them (of 160 each)", origin.served.Load(), shared)
	if shared == 0 {
		t.Fatal("the two nodes did not give each other a block")
	}
	if origin.served.Load() >= 320 {
		t.Fatalf("the origin served %d blocks: nothing was saved", origin.served.Load())
	}
}

// A node that says it has everything and gives nothing, or gives wrong bytes, only costs time: the file arrives from the origin.
func TestSwarmPeersThatLieOrCorruptAreOnlySlow(t *testing.T) {
	for _, mode := range []string{"liar", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			old := swarmEvery
			swarmEvery = 5 * time.Millisecond
			defer func() { swarmEvery = old }()
			x := newNode(t, "x")
			liar := &swarmPeer{f: x.f, mode: mode}
			b := newNode(t, "b", func(c *Config) { c.Swarm = func() []Source { return []Source{liar} } })
			a := newNode(t, "a")
			data := writeBig(t, a, "big.bin", 40)
			if _, err := b.f.SyncFrom("a", &swarmPeer{f: a.f, delay: time.Millisecond}); err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(b.path("big.bin"))
			if !bytes.Equal(got, data) {
				t.Fatal("the file differs")
			}
		})
	}
}

// What has arrived of a download that is under way is served, nothing else is, and Have says which.
func TestADownloadUnderWayServesWhatHasArrived(t *testing.T) {
	a := newNode(t, "a")
	data := writeBig(t, a, "src.bin", 12)
	fi, _ := a.f.ix.get("src.bin")
	b := newNode(t, "b")
	tmp := b.path("part")
	if err := os.WriteFile(tmp, make([]byte, len(data)), 0o644); err != nil {
		t.Fatal(err)
	}
	fl := b.f.flyStart(fi, tmp)
	if fl == nil {
		t.Fatal("no flight for a file of 12 blocks")
	}
	defer b.f.flyEnd(fl)
	bl := fi.Blocks[3]
	fh, _ := os.OpenFile(tmp, os.O_WRONLY, 0)
	fh.WriteAt(data[bl.Off:bl.Off+int64(bl.Size)], bl.Off)
	fh.Close()
	if _, err := b.f.Block("src.bin", bl.Off, int(bl.Size), bl.Hash); err == nil {
		t.Fatal("a block that was not marked as arrived was served")
	}
	fl.mark(3)
	d, err := b.f.Block("src.bin", bl.Off, int(bl.Size), bl.Hash)
	if err != nil || !bytes.Equal(d, data[bl.Off:bl.Off+int64(bl.Size)]) {
		t.Fatalf("an arrived block was not served: %v", err)
	}
	if o := fi.Blocks[4]; true {
		if _, err := b.f.Block("src.bin", o.Off, int(o.Size), o.Hash); err == nil {
			t.Fatal("block 4 has not arrived")
		}
	}
	av := b.f.Have("src.bin", fi.Version)
	if av.Complete || !av.Has(3) || av.Has(4) || av.N != 12 {
		t.Fatalf("Have: %+v", av)
	}
	if av := b.f.Have("src.bin", Vector{"zz": 9}); av.N != 0 || av.Complete {
		t.Fatal("another version is not held")
	}
	// once the file is in place, the answer is: all of it
	b.f.flyEnd(fl)
	if err := os.WriteFile(b.path("src.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.f.ix.put(fi); err != nil {
		t.Fatal(err)
	}
	if !b.f.Have("src.bin", fi.Version).Complete {
		t.Fatal("a file in place is held whole")
	}
}
