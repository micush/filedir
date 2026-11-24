package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type fdTestNode struct {
	*tnode
	svc  *fdService
	root string
}

func newFdTestNode(t *testing.T, name string) *fdTestNode {
	n := newTNode(t, name)
	root := filepath.Join(n.dir, "data")
	s := newFdService(nil, n.mg)
	s.cfgFn = func() FilesConfig { return FilesConfig{Folders: []FolderCfg{{ID: "docs", Path: root}}} }
	fdSvc = s
	t.Cleanup(s.Stop)
	s.reconcile()
	return &fdTestNode{n, s, root}
}

func (n *fdTestNode) write(t *testing.T, name string, b []byte) {
	t.Helper()
	p := filepath.Join(n.root, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (n *fdTestNode) has(name string, want []byte) bool {
	b, err := os.ReadFile(filepath.Join(n.root, name))
	return err == nil && bytes.Equal(b, want)
}

func TestFilesSpreadAcrossTheCluster(t *testing.T) {
	fdNoTell.Store(true) // the count of who served what needs each node to ask when the test says so, not when told
	t.Cleanup(func() { fdNoTell.Store(false) })
	a, b, c := newFdTestNode(t, "A"), newFdTestNode(t, "B"), newFdTestNode(t, "C")
	b.join(a.tnode)
	c.join(a.tnode)
	for _, n := range []*fdTestNode{a, b, c} {
		n.sync()
	}
	big := bytes.Repeat([]byte("0123456789abcdef"), 128<<10) // 2 MiB: 16 blocks
	a.write(t, "dir/big.bin", big)
	a.write(t, "small.txt", []byte("hello"))
	a.svc.scanAll()

	// B takes it from A
	b.svc.syncAll()
	if !b.has("dir/big.bin", big) || !b.has("small.txt", []byte("hello")) {
		t.Fatal("B did not get the files")
	}
	// C learns what B holds, then pulls: the blocks come from both A and B
	b.svc.scanAll()
	for _, p := range c.svc.otherPeers() {
		if _, _, err := c.svc.source(p).Index("docs", 0); err != nil {
			t.Fatal(err)
		}
	}
	a0, b0 := a.svc.served.Load(), b.svc.served.Load()
	c.svc.syncAll()
	if !c.has("dir/big.bin", big) {
		t.Fatal("C did not get the file")
	}
	fromA, fromB := a.svc.served.Load()-a0, b.svc.served.Load()-b0
	if fromA == 0 || fromB == 0 {
		t.Fatalf("both A and B should have served blocks to C: A %d, B %d", fromA, fromB)
	}

	// a deletion and an edit reach everyone
	os.Remove(filepath.Join(c.root, "small.txt"))
	a.write(t, "dir/big.bin", []byte("now small"))
	time.Sleep(10 * time.Millisecond)
	for _, n := range []*fdTestNode{a, b, c} {
		n.svc.scanAll()
	}
	for i := 0; i < 3; i++ {
		for _, n := range []*fdTestNode{a, b, c} {
			n.svc.syncAll()
			n.svc.scanAll()
		}
	}
	for _, n := range []*fdTestNode{a, b, c} {
		if _, err := os.Stat(filepath.Join(n.root, "small.txt")); err == nil {
			t.Fatalf("%s still has small.txt", n.name)
		}
		if !n.has("dir/big.bin", []byte("now small")) {
			t.Fatalf("%s has the old big.bin", n.name)
		}
	}
}

func TestFilesStatusAndList(t *testing.T) {
	a, b := newFdTestNode(t, "A"), newFdTestNode(t, "B")
	b.join(a.tnode)
	a.sync()
	b.sync()
	a.write(t, "top.txt", []byte("1234"))
	a.write(t, "d/x.txt", []byte("12"))
	a.write(t, "d/sub/y.txt", []byte("1"))
	a.svc.scanAll()
	b.svc.syncAll()

	l, total, ok := b.svc.List("docs", "", 100)
	if !ok || total != 2 || l[0].Kind != "dir" || l[0].Name != "d" || l[1].Name != "top.txt" || l[1].Size != 4 {
		t.Fatalf("top level: %v %d %v", l, total, ok)
	}
	l, _, _ = b.svc.List("docs", "d", 100)
	if len(l) != 2 || l[0].Name != "sub" || l[1].Path != "d/x.txt" {
		t.Fatalf("d: %v", l)
	}
	if _, _, ok := b.svc.List("nope", "", 10); ok {
		t.Fatal("an unknown folder was listed")
	}
	st, errs := b.svc.AllStatus(context.Background())
	if len(errs) != 0 || len(st) != 2 {
		t.Fatalf("status: %v %v", st, errs)
	}
	for _, n := range st {
		f := n.Folders[0]
		if f.Files != 3 || f.Dirs != 2 || f.Bytes != 7 {
			t.Fatalf("%s: %+v", n.Node, f)
		}
	}
	if len(st[0].Folders[0].Peers)+len(st[1].Folders[0].Peers) == 0 {
		t.Fatal("no sync state reported")
	}
}
