package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRootFolderName(t *testing.T) {
	ok := FilesConfig{Folders: []FolderCfg{{ID: "/", Path: "/var/lib/filedir/root"}, {ID: "docs", Path: "/srv/docs"}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"//", "/a", "a/b", ""} {
		c := FilesConfig{Folders: []FolderCfg{{ID: bad, Path: "/srv/x"}}}
		if c.Validate() == nil {
			t.Errorf("%q was accepted as a folder name", bad)
		}
	}
	if fdDBName("/") == "/" || folderIDRe.MatchString(fdDBName("/")) {
		t.Fatal("the root's index directory could clash with a folder's")
	}
}

// The root folder lists the other synced folders at its top, wherever they are kept; a real entry of the same name is hidden by them.
func TestRootFolderListsTheOthers(t *testing.T) {
	e := newWebEnv(t)
	base := t.TempDir()
	rootDir, docsDir, filesDir := filepath.Join(base, "root"), filepath.Join(base, "elsewhere", "d"), filepath.Join(base, "f")
	s := newFdService(nil, e.mg)
	s.cfgFn = func() FilesConfig {
		return FilesConfig{Folders: []FolderCfg{{ID: "/", Path: rootDir}, {ID: "docs", Path: docsDir}, {ID: "files", Path: filesDir}}}
	}
	old := fdSvc
	fdSvc = s
	t.Cleanup(func() { fdSvc = old; s.Stop() })
	s.reconcile()
	os.MkdirAll(filepath.Join(rootDir, "docs"), 0o755) // hidden by the synced folder docs
	os.MkdirAll(filepath.Join(rootDir, "photos"), 0o755)
	os.WriteFile(filepath.Join(rootDir, "readme.txt"), []byte("r"), 0o644)
	os.WriteFile(filepath.Join(docsDir, "a.txt"), []byte("a"), 0o644)
	s.scanAll()
	list, total, ok := s.List("/", "", 100)
	if !ok || total != 4 {
		t.Fatalf("root list: %v %d %+v", ok, total, list)
	}
	var mounts, names []string
	for _, v := range list {
		names = append(names, v.Name)
		if v.Mount != "" {
			mounts = append(mounts, v.Mount)
		}
	}
	if len(mounts) != 2 || mounts[0] != "docs" || mounts[1] != "files" || names[2] != "photos" || names[3] != "readme.txt" {
		t.Fatalf("names %v mounts %v", names, mounts)
	}
	// only the top of the root shows them; a folder under it does not
	if sub, _, _ := s.List("/", "photos", 100); len(sub) != 0 {
		t.Fatalf("photos: %+v", sub)
	}
	// and a synced folder lists its own files, with no root in it
	if d, _, _ := s.List("docs", "", 100); len(d) != 1 || d[0].Name != "a.txt" || d[0].Mount != "" {
		t.Fatalf("docs: %+v", d)
	}
	if h, n := s.Search("a.txt", 10); n != 1 || h[0].Folder != "docs" {
		t.Fatalf("search: %+v", h)
	}
	// a search can be kept to one folder and what is under a path in it, and matches the part of the path below that
	os.MkdirAll(filepath.Join(rootDir, "photos", "trip"), 0o755)
	os.WriteFile(filepath.Join(rootDir, "photos", "trip", "txt-notes.txt"), []byte("n"), 0o644)
	s.scanAll()
	if _, n := s.Search("txt", 10); n != 3 { // readme.txt, a.txt, photos/trip/txt-notes.txt
		t.Fatalf("everywhere: %d", n)
	}
	if h, n := s.Search("txt", 10, "docs"); n != 1 || h[0].Folder != "docs" {
		t.Fatalf("one folder: %+v", h)
	}
	if h, n := s.Search("txt", 10, "/", "photos"); n != 1 || h[0].Path != "photos/trip/txt-notes.txt" {
		t.Fatalf("under a path: %+v", h)
	}
	if _, n := s.Search("photos", 10, "/", "photos"); n != 0 {
		t.Fatalf("the path itself is not matched: %d", n)
	}
}

// A file written into the root folder (through the API, as the Files page does) is a file in the root's folder on the disk.
func TestRootFolderWritesToItsPathOnDisk(t *testing.T) {
	e := newWebEnv(t)
	base := t.TempDir()
	rootDir := filepath.Join(base, "folders", "root")
	s := newFdService(nil, e.mg)
	s.cfgFn = func() FilesConfig { return FilesConfig{Folders: []FolderCfg{{ID: "/", Path: rootDir}}} }
	old := fdSvc
	fdSvc = s
	t.Cleanup(func() { fdSvc = old; s.Stop() })
	s.reconcile()
	f := s.get("/")
	if f == nil || f.f == nil {
		t.Fatal("the root folder is not open")
	}
	if n, err := f.f.WriteFile("test/test2/test3/a.bin", strings.NewReader("hello")); err != nil || n != 5 {
		t.Fatalf("write: %d %v", n, err)
	}
	if b, err := os.ReadFile(filepath.Join(rootDir, "test", "test2", "test3", "a.bin")); err != nil || string(b) != "hello" {
		t.Fatalf("on disk: %q %v", b, err)
	}
}
