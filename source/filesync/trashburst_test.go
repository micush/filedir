package filesync

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A folder deleted on another node arrives file by file over many seconds; it must land in the trash as one entry, not one per second.
func TestPulledDeletesShareTrashFolder(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	f := &Folder{root: t.TempDir()}
	f.cfg.Now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		src := filepath.Join(f.root, "f"+string(rune('a'+i)))
		if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := f.toTrash(src, "projects/d/"+filepath.Base(src)); err != nil {
			t.Fatal(err)
		}
		now = now.Add(7 * time.Second) // a deletion that takes half a minute to arrive
	}
	ents, total, err := f.TrashEntries("", "", 0)
	if err != nil || total != 1 || len(ents) != 1 || ents[0].Name != "projects" || ents[0].Files != 5 {
		t.Fatalf("one deletion, got %d entries %+v (%v)", total, ents, err)
	}
	// a long pause starts a new trash folder
	now = now.Add(2 * time.Minute)
	src := filepath.Join(f.root, "z")
	_ = os.WriteFile(src, []byte("x"), 0o600)
	if err := f.toTrash(src, "projects/d/z"); err != nil {
		t.Fatal(err)
	}
	if _, total, _ := f.TrashEntries("", "", 0); total != 2 {
		t.Errorf("after a pause: %d entries, want 2", total)
	}
}
