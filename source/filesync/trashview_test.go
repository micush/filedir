package filesync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTrashBrowseRestoreDelete(t *testing.T) {
	f := zipTestFolder(t)
	root := f.root
	os.MkdirAll(filepath.Join(root, "d", "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "d", "a.txt"), []byte("aaa"), 0o644)
	os.WriteFile(filepath.Join(root, "d", "sub", "c.txt"), []byte("cc"), 0o644)
	os.WriteFile(filepath.Join(root, "b.txt"), []byte("bb"), 0o644)
	if _, err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete("d"); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete("b.txt"); err != nil {
		t.Fatal(err)
	}
	top, total, err := f.TrashEntries("", "", 0)
	if err != nil || total != 2 || len(top) != 2 {
		t.Fatalf("top: %+v %d %v", top, total, err)
	}
	var d, b TrashEntry
	for _, e := range top {
		if e.Name == "d" {
			d = e
		} else {
			b = e
		}
	}
	if d.Kind != "dir" || d.Files != 2 || d.Size != 5 || d.Mod == 0 || b.Kind != "file" || b.Size != 2 {
		t.Fatalf("entries: %+v %+v", d, b)
	}
	in, _, err := f.TrashEntries(d.When, "d", 0)
	if err != nil || len(in) != 2 || in[0].Name != "sub" || in[0].Kind != "dir" || in[1].Name != "a.txt" {
		t.Fatalf("inside d: %+v %v", in, err)
	}
	// restore a file whose name is taken again, then the folder
	os.WriteFile(filepath.Join(root, "b.txt"), []byte("new"), 0o644)
	if n, err := f.RestoreTrash(b.When, "b.txt"); err != nil || n != 1 {
		t.Fatalf("restore b: %d %v", n, err)
	}
	if x, _ := os.ReadFile(filepath.Join(root, "b restored.txt")); string(x) != "bb" {
		t.Fatal("b restored.txt")
	}
	if x, _ := os.ReadFile(filepath.Join(root, "b.txt")); string(x) != "new" {
		t.Fatal("a file that was there was replaced")
	}
	if n, err := f.RestoreTrash(d.When, "d"); err != nil || n != 2 {
		t.Fatalf("restore d: %d %v", n, err)
	}
	if x, _ := os.ReadFile(filepath.Join(root, "d", "sub", "c.txt")); string(x) != "cc" {
		t.Fatal("restored content")
	}
	if _, total, _ := f.TrashEntries("", "", 0); total != 0 {
		t.Fatalf("left in the trash: %d", total)
	}
	if _, err := os.Stat(filepath.Join(root, MetaDir, "trash", d.When)); !os.IsNotExist(err) {
		t.Fatal("the empty trash folder was left")
	}
	// delete for good
	os.WriteFile(filepath.Join(root, "e.txt"), []byte("eee"), 0o644)
	f.Scan()
	f.Delete("e.txt")
	top, _, _ = f.TrashEntries("", "", 0)
	if by, n, err := f.DeleteTrash(top[0].When, "e.txt"); err != nil || n != 1 || by != 3 {
		t.Fatalf("delete: %d %d %v", by, n, err)
	}
	if _, total, _ := f.TrashEntries("", "", 0); total != 0 {
		t.Fatal("still there")
	}
	for _, bad := range [][2]string{{"..", "x"}, {"20200101-000000", "../../../etc/passwd"}, {"a/b", "x"}, {"20200101-000000", "nope"}, {"20200101-000000", ""}} {
		if _, err := f.RestoreTrash(bad[0], bad[1]); err == nil {
			t.Errorf("restore %v accepted", bad)
		}
		if _, _, err := f.DeleteTrash(bad[0], bad[1]); err == nil {
			t.Errorf("delete %v accepted", bad)
		}
	}
}
