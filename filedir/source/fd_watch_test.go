package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// waitFor polls cond for up to d.
func eventually(d time.Duration, cond func() bool) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return cond()
}

func (n *fdTestNode) listed(path string) bool {
	f := n.svc.get("docs")
	if f == nil || f.f == nil {
		return false
	}
	e, ok := f.f.Entry(path)
	return ok && !e.Deleted
}

func (n *fdTestNode) gone(path string) bool {
	f := n.svc.get("docs")
	e, ok := f.f.Entry(path)
	return ok && e.Deleted
}

// A change made behind the daemon's back is in the index within a few seconds, without any scan being asked for: far sooner than the
// 30 s timer, which these tests never start.
func TestWatchFindsChangesAtOnce(t *testing.T) {
	n := newFdTestNode(t, "A")
	if n.svc.get("docs").watch == nil {
		t.Skip("inotify is not available here")
	}
	n.write(t, "new.txt", []byte("hello"))
	if !eventually(4*time.Second, func() bool { return n.listed("new.txt") }) {
		t.Fatal("a new file was not found")
	}
	// a directory made after the watch began, and a file made in it at once
	n.write(t, "later/deep/inside.txt", []byte("x"))
	if !eventually(4*time.Second, func() bool { return n.listed("later/deep/inside.txt") }) {
		t.Fatal("a file in a new directory was not found")
	}
	// a file added to that directory later, which only a watch on it can see
	time.Sleep(700 * time.Millisecond)
	n.write(t, "later/deep/second.txt", []byte("y"))
	if !eventually(4*time.Second, func() bool { return n.listed("later/deep/second.txt") }) {
		t.Fatal("a file in a directory made earlier was not found")
	}
	// a rename of a directory, then a file in it under its new name
	if err := os.Rename(filepath.Join(n.root, "later"), filepath.Join(n.root, "moved")); err != nil {
		t.Fatal(err)
	}
	if !eventually(4*time.Second, func() bool { return n.listed("moved/deep/inside.txt") && n.gone("later/deep/inside.txt") }) {
		t.Fatal("a moved directory was not found")
	}
	time.Sleep(700 * time.Millisecond)
	n.write(t, "moved/deep/third.txt", []byte("z"))
	if !eventually(4*time.Second, func() bool { return n.listed("moved/deep/third.txt") }) {
		t.Fatal("a file in a moved directory was not found")
	}
	// a removal
	os.Remove(filepath.Join(n.root, "new.txt"))
	if !eventually(4*time.Second, func() bool { return n.gone("new.txt") }) {
		t.Fatal("a removed file was not found")
	}
}

// Writes inside the metadata folder (the daemon's own temporary files) and ignored names start no scan.
func TestWatchLeavesOwnFilesAlone(t *testing.T) {
	n := newFdTestNode(t, "A")
	f := n.svc.get("docs")
	if f.watch == nil {
		t.Skip("inotify is not available here")
	}
	eventually(2*time.Second, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return !f.scanned.IsZero() })
	f.mu.Lock()
	before := f.scanned
	f.mu.Unlock()
	os.WriteFile(filepath.Join(n.root, ".filedir", "tmp-probe"), []byte("x"), 0o644)
	time.Sleep(1200 * time.Millisecond)
	f.mu.Lock()
	after := f.scanned
	f.mu.Unlock()
	if !after.Equal(before) {
		t.Fatal("a write in the metadata folder started a scan")
	}
}

// A change on one node reaches another in about a second, with neither node's sync timer running.
func TestChangeIsToldToThePeers(t *testing.T) {
	a, b := newFdTestNode(t, "A"), newFdTestNode(t, "B")
	b.join(a.tnode)
	a.sync()
	b.sync()
	if a.svc.get("docs").watch == nil {
		t.Skip("inotify is not available here")
	}
	a.write(t, "told.txt", []byte("news"))
	if !eventually(8*time.Second, func() bool { return b.has("told.txt", []byte("news")) }) {
		t.Fatal("B did not get the file without being asked to sync")
	}
}
