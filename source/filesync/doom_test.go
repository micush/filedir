package filesync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// Upload a tree (with empty folders in it), let it replicate, delete it on one node, and let the nodes talk (scans and syncs both ways) for
// a while: nothing of it may come back on either node.
func TestDeletedTreeStaysDeleted(t *testing.T) {
	a := newNode(t, "A")
	b := newNode(t, "B")
	for i := 0; i < 60; i++ {
		a.write(t, fmt.Sprintf("proj/p%d/.git/objects/%02x/o%d", i%5, i%16, i), "obj")
		a.write(t, fmt.Sprintf("proj/p%d/src/a/b/f%d.go", i%5, i), "code")
	}
	for i := 0; i < 5; i++ {
		os.MkdirAll(a.path(fmt.Sprintf("proj/p%d/empty/deeper", i)), 0o755)
		os.MkdirAll(a.path(fmt.Sprintf("proj/p%d/.git/refs/heads", i)), 0o755)
	}
	a.write(t, "keep.txt", "k")
	a.scan(t)
	b.sync(t, a)
	a.sync(t, b)
	b.scan(t)
	if _, ok := b.read("proj/p1/src/a/b/f1.go"); !ok {
		t.Fatal("not replicated")
	}
	if err := a.f.Delete("proj"); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 6; round++ {
		a.scan(t)
		b.scan(t)
		b.sync(t, a)
		a.sync(t, b)
	}
	noneUnder(t, a, "proj")
	noneUnder(t, b, "proj")
}

// The same, deleted on the node that was synced to, with the other node scanning in between.
func TestDeletedTreeStaysDeletedOtherWay(t *testing.T) {
	a := newNode(t, "A")
	b := newNode(t, "B")
	for i := 0; i < 40; i++ {
		a.write(t, fmt.Sprintf("proj/p%d/d%d/f%d", i%4, i%3, i), "x")
	}
	a.scan(t)
	b.sync(t, a)
	b.scan(t)
	if err := b.f.Delete("proj"); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 6; round++ {
		a.scan(t)
		b.scan(t)
		a.sync(t, b)
		b.sync(t, a)
	}
	noneUnder(t, a, "proj")
	noneUnder(t, b, "proj")
}

// A folder that holds files the sync leaves out (*.tmp) on the node that receives the deletion: the folder was kept, and came back on the
// other node as empty folders.  What the sync leaves out goes to the trash with the folder.
func TestDeletedTreeWithIgnoredFilesStaysDeleted(t *testing.T) {
	ign := func(c *Config) { c.Ignore = []string{"*.tmp"} }
	a := newNode(t, "A", ign)
	b := newNode(t, "B", ign)
	for i := 0; i < 30; i++ {
		a.write(t, fmt.Sprintf("proj/p%d/src/f%d.go", i%3, i), "code")
	}
	a.write(t, "keep.txt", "k")
	a.scan(t)
	b.sync(t, a)
	// the receiving node has left-out files in some of the folders, and one folder that is left out as a whole
	b.write(t, "proj/p0/src/cache.tmp", "t")
	b.write(t, "proj/p1/deep/er/x.tmp", "t")
	os.MkdirAll(b.path("proj/p2/src/junk"), 0o755)
	b.write(t, "proj/p2/src/junk/y.tmp", "t")
	b.scan(t)
	if err := a.f.Delete("proj"); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 6; round++ {
		a.scan(t)
		b.scan(t)
		b.sync(t, a)
		a.sync(t, b)
	}
	noneUnder(t, a, "proj")
	noneUnder(t, b, "proj")
	// a file the sync does not leave out, and that no other node knows of, still keeps its folder
	a2 := newNode(t, "A2", ign)
	b2 := newNode(t, "B2", ign)
	a2.write(t, "d/one.txt", "1")
	a2.scan(t)
	b2.sync(t, a2)
	b2.write(t, "d/new.txt", "mine")
	if err := a2.f.Delete("d"); err != nil {
		t.Fatal(err)
	}
	b2.sync(t, a2)
	if got, ok := b2.read("d/new.txt"); !ok || got != "mine" {
		t.Errorf("a file that was never synced was removed: %q %v", got, ok)
	}
}

// Emptying the trash must not wait for the files to be removed.
func TestEmptyTrashReturnsAtOnce(t *testing.T) {
	a := newNode(t, "A")
	for i := 0; i < 200; i++ {
		a.write(t, fmt.Sprintf("d/f%d.txt", i), "data")
	}
	a.scan(t)
	if err := a.f.Delete("d"); err != nil {
		t.Fatal(err)
	}
	if _, n := a.f.TrashSize(); n != 200 {
		t.Fatalf("trash holds %d files, want 200", n)
	}
	b, n, err := a.f.EmptyTrash()
	if err != nil || n != 200 || b != 200*4 {
		t.Fatalf("EmptyTrash = %d bytes, %d files, %v", b, n, err)
	}
	if _, n := a.f.TrashSize(); n != 0 {
		t.Errorf("trash still holds %d files", n)
	}
	// and it is usable again at once
	a.write(t, "e.txt", "x")
	a.scan(t)
	a.f.Delete("e.txt")
	if _, n := a.f.TrashSize(); n != 1 {
		t.Errorf("new trash holds %d files, want 1", n)
	}
	if b, n, err := a.f.EmptyTrash(); err != nil || n != 1 || b != 1 {
		t.Errorf("second EmptyTrash = %d, %d, %v", b, n, err)
	}
}

// slow hands blocks out a little at a time, so that a sync is still going when the deletion comes.
type slowSrc struct{ *local }

func (s slowSrc) Block(folder, name string, off int64, size int, h Hash) ([]byte, error) {
	time.Sleep(300 * time.Microsecond)
	return s.local.Block(folder, name, off, size, h)
}

// What a cluster does: both nodes scan and sync from each other all the time, and a folder is deleted on one of them while the other has
// not finished replicating it.  Nothing of it may come back.
func TestDeleteDuringReplicationStaysDeleted(t *testing.T) {
	for iter := 0; iter < 12; iter++ {
		a := newNode(t, "A", func(c *Config) { c.WatchEvery = 10 * time.Millisecond })
		b := newNode(t, "B", func(c *Config) { c.WatchEvery = 10 * time.Millisecond })
		for i := 0; i < 400; i++ {
			a.write(t, fmt.Sprintf("proj/r%d/.git/objects/%02x/o%d", i%4, i%64, i), fmt.Sprintf("obj %d", i))
			a.write(t, fmt.Sprintf("proj/r%d/src/m%d/f%d.go", i%4, i%9, i), fmt.Sprintf("code %d", i))
		}
		a.write(t, "keep.txt", "k")
		a.scan(t)
		stop := make(chan struct{})
		var wg sync.WaitGroup
		loop := func(me, other *testNode) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				me.f.Scan()
				me.f.SyncFrom(other.id, slowSrc{&local{f: other.f, fail: -1}})
				time.Sleep(time.Millisecond)
			}
		}
		wg.Add(2)
		go loop(b, a)
		go loop(a, b)
		time.Sleep(time.Duration(5+iter*25) * time.Millisecond)
		if err := a.f.Delete("proj"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1500 * time.Millisecond)
		close(stop)
		wg.Wait()
		for round := 0; round < 4; round++ {
			b.sync(t, a)
			a.sync(t, b)
		}
		noneUnder(t, a, "proj")
		noneUnder(t, b, "proj")
		if t.Failed() {
			t.Fatalf("iteration %d", iter)
		}
	}
}

// A folder that is kept because something is in it says so in the log, and says what.
func TestKeptFolderIsLogged(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	logf := func(format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	a := newNode(t, "A")
	b := newNode(t, "B", func(c *Config) { c.Logf = logf })
	a.write(t, "d/e/one.txt", "1")
	a.scan(t)
	b.sync(t, a)
	b.write(t, "d/e/mine.txt", "not known to anyone")
	if err := a.f.Delete("d"); err != nil {
		t.Fatal(err)
	}
	b.sync(t, a)
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, l := range lines {
		if strings.Contains(l, "kept the folder d/e") && strings.Contains(l, "mine.txt (in the index, live)") {
			found = true
		}
	}
	if !found {
		t.Errorf("the log does not say why d/e was kept: %q", lines)
	}
}
