package filesync

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// The sorted snapshot is built once and rebuilt only after the index changes.
func TestSnapshotCached(t *testing.T) {
	ix, err := openIndex(filepath.Join(t.TempDir(), "index"))
	if err != nil {
		t.Fatal(err)
	}
	defer ix.close()
	var fis []FileInfo
	for i := 0; i < 100000; i++ {
		fis = append(fis, FileInfo{Name: fmt.Sprintf("d%d/f%d", i%100, i), Size: int64(i)})
	}
	if err := ix.put(fis...); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	a := ix.snapshot()
	first := time.Since(t0)
	t0 = time.Now()
	b := ix.snapshot()
	again := time.Since(t0)
	if len(a) != 100000 || &a[0] != &b[0] {
		t.Fatalf("the second snapshot was rebuilt (%d entries)", len(a))
	}
	t.Logf("first %v, again %v", first, again)
	if again > first/20 && again > time.Millisecond {
		t.Errorf("a cached snapshot took %v (first %v)", again, first)
	}
	if err := ix.put(FileInfo{Name: "new"}); err != nil {
		t.Fatal(err)
	}
	if c := ix.snapshot(); len(c) != 100001 {
		t.Fatalf("a change was not seen: %d", len(c))
	}
	if err := ix.drop([]string{"new"}); err != nil {
		t.Fatal(err)
	}
	if c := ix.snapshot(); len(c) != 100000 {
		t.Fatalf("a drop was not seen: %d", len(c))
	}
}
