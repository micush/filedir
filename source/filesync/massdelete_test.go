package filesync

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// A person who deletes (or renames) a folder with hundreds of files in it is not a disk that came back empty: the scan must not refuse.
func TestDeletingABigFolderIsNotAMassDeleteAccident(t *testing.T) {
	f := zipTestFolder(t)
	for i := 0; i < 300; i++ {
		p := filepath.Join(f.root, "test", fmt.Sprintf("f%d.bin", i))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	os.WriteFile(filepath.Join(f.root, "keep.txt"), []byte("k"), 0o644)
	if _, err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete("test"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Scan(); err != nil {
		t.Fatalf("scan after a deliberate delete: %v", err)
	}
	for _, e := range f.Entries() {
		if !e.Deleted && e.Name != "keep.txt" {
			t.Fatalf("still listed: %s", e.Name)
		}
	}
	// a rename of a big folder
	for i := 0; i < 300; i++ {
		p := filepath.Join(f.root, "src", fmt.Sprintf("g%d.bin", i))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("y"), 0o644)
	}
	if _, err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	if err := f.Rename("src", "dst"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Scan(); err != nil {
		t.Fatalf("scan after a rename: %v", err)
	}
	live := 0
	for _, e := range f.Entries() {
		if !e.Deleted && (e.Name == "dst" || len(e.Name) > 4 && e.Name[:4] == "dst/") {
			live++
		}
		if !e.Deleted && len(e.Name) >= 3 && e.Name[:3] == "src" {
			t.Fatalf("old name still listed: %s", e.Name)
		}
	}
	if live != 301 {
		t.Fatalf("under the new name: %d", live)
	}
	// the state an older version left: gone from the disk, still in the index; deleting again records it
	for i := 0; i < 900; i++ {
		p := filepath.Join(f.root, "old", fmt.Sprintf("h%d.bin", i))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("z"), 0o644)
	}
	if _, err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(f.root, "old"))
	if _, err := f.Scan(); err == nil {
		t.Fatal("the accident guard did not hold for a scan")
	}
	if err := f.Delete("old"); err != nil {
		t.Fatalf("delete of what the index still lists: %v", err)
	}
	if _, err := f.Scan(); err != nil {
		t.Fatalf("scan after that: %v", err)
	}
	if err := f.Delete("old"); err != ErrNotFound {
		t.Fatalf("a second delete: %v", err)
	}
}
