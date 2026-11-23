package filesync

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A source that, at the first block asked for, runs act (the deletion) and gives the watcher time to see it; the data it hands out is
// what was there before, so it is the doom that stops the pull and not a block that cannot be had.
type actSrc struct {
	*local
	once sync.Once
	act  func()
	data map[string][]byte
}

func (s *actSrc) Block(folder, name string, off int64, size int, h Hash) ([]byte, error) {
	s.once.Do(func() { s.act(); time.Sleep(400 * time.Millisecond) })
	if d, ok := s.data[name]; ok && int(off)+size <= len(d) {
		return d[off : int(off)+size], nil
	}
	return s.local.Block(folder, name, off, size, h)
}

func doomSetup(t *testing.T, mut func(*Config)) (a, b *testNode, src *actSrc) {
	a = newNode(t, "A")
	b = newNode(t, "B", func(c *Config) { c.WatchEvery = 20 * time.Millisecond })
	data := map[string][]byte{}
	for i := 0; i < 8; i++ {
		n := fmt.Sprintf("d/sub/f%d.txt", i)
		a.write(t, n, fmt.Sprintf("content of file %d", i))
		data[n] = []byte(fmt.Sprintf("content of file %d", i))
	}
	a.write(t, "keep.txt", "kept")
	a.scan(t)
	return a, b, &actSrc{local: &local{f: a.f, fail: -1}, data: data}
}

func noneUnder(t *testing.T, n *testNode, dir string) {
	t.Helper()
	for _, e := range n.f.Entries() {
		if !e.Deleted && (e.Name == dir || len(e.Name) > len(dir) && e.Name[:len(dir)+1] == dir+"/") {
			t.Errorf("%s still has %s", n.id, e.Name)
		}
	}
	if _, err := os.Stat(n.path(dir)); err == nil {
		t.Errorf("%s still has %s on disk", n.id, dir)
	}
	if ents, _ := os.ReadDir(filepath.Join(n.f.root, MetaDir, "tmp")); len(ents) != 0 {
		t.Errorf("%s left %d temporary files", n.id, len(ents))
	}
}

// The peer deletes a folder while this node is pulling it: the pulls stop, nothing of it is left, and the sync does not fail.
func TestPeerDeletesWhileReplicating(t *testing.T) {
	a, b, src := doomSetup(t, nil)
	src.act = func() {
		if err := a.f.Delete("d"); err != nil {
			t.Error(err)
		}
	}
	if _, err := b.f.SyncFrom(a.id, src); err != nil {
		t.Fatalf("the sync failed: %v", err)
	}
	noneUnder(t, b, "d")
	if got, ok := b.read("keep.txt"); !ok || got != "kept" {
		t.Errorf("the rest was not replicated: %q %v", got, ok)
	}
}

// The folder is deleted on this node while it is being pulled here.
func TestLocalDeleteWhileReplicating(t *testing.T) {
	_, b, src := doomSetup(t, nil)
	src.act = func() {
		// some of it has arrived or is arriving; Delete is the user's
		if err := b.f.Delete("d"); err != nil && err != ErrNotFound {
			t.Error(err)
		}
	}
	// the folder must exist here for Delete to find it: the first entry is the directory itself, which the pull makes first
	b.write(t, "d/placeholder", "x")
	b.scan(t)
	if _, err := b.f.SyncFrom("A", src); err != nil {
		t.Fatalf("the sync failed: %v", err)
	}
	noneUnder(t, b, "d")
}

// A big deletion arrives as one entry per file and is written to the index in batches: all of it must be there afterwards, and the position kept.
func TestBigDeletionIsApplied(t *testing.T) {
	a := newNode(t, "A")
	b := newNode(t, "B")
	for i := 0; i < 700; i++ {
		a.write(t, fmt.Sprintf("big/s%d/f%d.txt", i%7, i), "x")
	}
	a.write(t, "keep.txt", "k")
	b.sync(t, a)
	if _, ok := b.read("big/s3/f3.txt"); !ok {
		t.Fatal("not replicated")
	}
	if err := a.f.Delete("big"); err != nil {
		t.Fatal(err)
	}
	b.sync(t, a)
	noneUnder(t, b, "big")
	live := 0
	for _, e := range b.f.Entries() {
		if !e.Deleted {
			live++
		}
	}
	if live != 1 {
		t.Errorf("live entries %d, want 1", live)
	}
	n, err := b.f.SyncFrom(a.id, &local{f: a.f, fail: -1})
	if err != nil || n != 0 {
		t.Errorf("a second sync: %d %v", n, err)
	}
	// and the other node gets nothing back from it
	a.sync(t, b)
	if _, ok := a.read("big/s3/f3.txt"); ok {
		t.Error("the deleted file came back")
	}
}
