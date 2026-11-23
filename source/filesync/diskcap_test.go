package filesync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiskCapRefusesWritesPastTheLimit(t *testing.T) {
	dir := t.TempDir()
	pct, _, err := DiskUse(dir)
	if err != nil || pct < 2 {
		t.Skipf("the disk here is %.1f%% full (%v): too empty to be past a 1%% limit", pct, err)
	}
	open := func(lim int) *Folder {
		f, err := Open(Config{ID: "d", Root: dir, Node: "n1", DBDir: filepath.Join(t.TempDir(), "db"), Create: true, MaxUsedPct: lim})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	if _, err := open(1).WriteFile("a.txt", strings.NewReader("x")); !errors.Is(err, ErrDiskFull) {
		t.Fatalf("past the limit: %v", err)
	}
	if _, err := open(1).StageAppend("b.bin", 0, []byte("x")); !errors.Is(err, ErrDiskFull) {
		t.Fatalf("stage past the limit: %v", err)
	}
	if n, err := open(100).WriteFile("a.txt", strings.NewReader("x")); err != nil || n != 1 {
		t.Fatalf("no limit: %d %v", n, err)
	}
	if n, err := open(0).WriteFile("c.txt", strings.NewReader("x")); err != nil || n != 1 {
		t.Fatalf("0 is no limit here (the daemon turns its default into a number): %d %v", n, err)
	}
	// a stream of unknown length stops partway
	big := strings.NewReader(strings.Repeat("y", guardStep*2+10))
	g := &guard{r: big, f: open(1)}
	buf := make([]byte, 1<<20)
	var gerr error
	for gerr == nil {
		_, gerr = g.Read(buf)
	}
	if !errors.Is(gerr, ErrDiskFull) {
		t.Fatalf("a long stream was not stopped: %v", gerr)
	}
}

// Before refusing for lack of room, the folder's trash is emptied; what was put there in the last seconds is left (a delete may be moving in).
func TestRoomEmptiesTheTrashFirst(t *testing.T) {
	dir := t.TempDir()
	if pct, _, err := DiskUse(dir); err != nil || pct < 2 {
		t.Skipf("the disk here is %.1f%% full", pct)
	}
	f, err := Open(Config{ID: "d", Root: dir, Node: "n1", DBDir: filepath.Join(t.TempDir(), "db"), Create: true, MaxUsedPct: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	old := filepath.Join(dir, MetaDir, "trash", "20200101-000000")
	fresh := filepath.Join(dir, MetaDir, "trash", f.now().UTC().Format("20060102-150405"))
	for _, d := range []string{old, fresh} {
		os.MkdirAll(d, 0o700)
		os.WriteFile(filepath.Join(d, "big.bin"), make([]byte, 5000), 0o644)
	}
	if err := f.room(1); !errors.Is(err, ErrDiskFull) {
		t.Fatalf("the disk is past a 1%% limit whatever the trash: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("the old trash was kept")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("what was just deleted was removed")
	}
	if n := f.freeTrash(); n != 0 {
		t.Fatalf("freed %d more", n)
	}
}
