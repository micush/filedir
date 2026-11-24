package filesync

import (
	"os"
	"testing"
)

func TestBufPoolGivesTheSizeAskedAndTakesOnlyItsOwn(t *testing.T) {
	for _, n := range []int{1, 1000, 1 << 16, 1<<17 + 1, 4 << 20, maxBlock} {
		b := GetBuf(n)
		if len(b) != n {
			t.Fatalf("GetBuf(%d) has length %d", n, len(b))
		}
		PutBuf(b)
		if c := GetBuf(n); len(c) != n {
			t.Fatalf("a reused buffer for %d has length %d", n, len(c))
		}
	}
	PutBuf(make([]byte, 100000)) // not a class size: left to the collector, and must not be handed out as one
	PutBuf(nil)
	if b := GetBuf(100000); len(b) != 100000 || cap(b) != 1<<17 {
		t.Fatalf("a buffer for 100000 bytes has length %d, capacity %d", len(b), cap(b))
	}
}

// A file pulled from a source that recycles its blocks arrives whole: the buffers handed back are not written over while they are used.
func TestPullFromARecyclingSourceKeepsTheFileWhole(t *testing.T) {
	a := newNode(t, "a")
	b := newNode(t, "b")
	data := writeBig(t, a, "big.bin", 200)
	if _, err := b.f.SyncFrom("a", &recycler{swarmPeer{f: a.f}}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(b.path("big.bin"))
	if err != nil || string(got) != string(data) {
		t.Fatalf("the pulled file differs (%v)", err)
	}
}

// recycler serves blocks from the pool, as the cluster's source does.
type recycler struct{ swarmPeer }

func (r *recycler) RecyclesBlocks() {}
func (r *recycler) Block(folder, name string, off int64, size int, h Hash) ([]byte, error) {
	d, err := r.swarmPeer.Block(folder, name, off, size, h)
	if err != nil {
		return nil, err
	}
	b := GetBuf(len(d))
	copy(b, d)
	return b, nil
}
