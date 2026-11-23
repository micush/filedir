package main

import "testing"

// A folder reports whether it has folders in it, so the tree shows an arrow only where there is something to open.
func TestListSaysWhichFoldersHaveFolders(t *testing.T) {
	n := newFdTestNode(t, "a")
	n.write(t, "plain/f.txt", []byte("x"))
	n.write(t, "nest/inner/f.txt", []byte("x"))
	n.svc.scanAll()
	list, _, ok := fdSvc.List("docs", "", 100)
	if !ok {
		t.Fatal("no list")
	}
	got := map[string]bool{}
	for _, e := range list {
		if e.Kind == "dir" {
			got[e.Name] = e.Subdirs
		}
	}
	if len(got) != 2 || got["plain"] || !got["nest"] {
		t.Fatalf("has_dirs: %v", got)
	}
	inner, _, _ := fdSvc.List("docs", "nest", 100)
	if len(inner) != 1 || inner[0].Subdirs {
		t.Fatalf("inner: %+v", inner)
	}
}
