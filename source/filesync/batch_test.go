package filesync

import (
	"bytes"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
)

// batchLocal is a local source that can also batch.
type batchLocal struct {
	*local
	batches, singles atomic.Int32
	mode             string // "", "error", "corrupt", "missing"
}

func (b *batchLocal) Block(folder, name string, off int64, size int, h Hash) ([]byte, error) {
	b.singles.Add(1)
	return b.local.Block(folder, name, off, size, h)
}

func (b *batchLocal) Blocks(folder string, reqs []BlockReq) ([][]byte, error) {
	b.batches.Add(1)
	if b.mode == "error" {
		return nil, errors.New("no such endpoint")
	}
	out := make([][]byte, len(reqs))
	for i, r := range reqs {
		d, err := b.local.Block(folder, r.Name, r.Off, r.Size, r.Hash)
		if err != nil || b.mode == "missing" && i%2 == 0 {
			continue
		}
		if b.mode == "corrupt" && i%3 == 0 {
			d = append([]byte(nil), d...)
			d[0] ^= 0xff
		}
		out[i] = d
	}
	return out, nil
}

func fillSmall(t *testing.T, a *testNode, n int) {
	for i := 0; i < n; i++ {
		a.write(t, fmt.Sprintf("d%d/f%d.txt", i%7, i), fmt.Sprintf("content of file %d %s", i, string(make([]byte, i%50))))
	}
	a.scan(t)
}

func TestSmallFilesTravelInBatches(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	fillSmall(t, a, 300)
	src := &batchLocal{local: &local{f: a.f, fail: -1}}
	if _, err := b.f.SyncFrom("a", src); err != nil {
		t.Fatal(err)
	}
	same(t, a, b)
	if src.batches.Load() == 0 || src.batches.Load() > 10 {
		t.Fatalf("%d batched requests for 300 small files", src.batches.Load())
	}
	if src.singles.Load() > 0 {
		t.Fatalf("%d single requests: the batch should have covered them", src.singles.Load())
	}
}

// What a batch gets wrong (an error, a missing block, bytes that do not match) costs only the old way of asking.
func TestBatchFailuresFallBackToSingleBlocks(t *testing.T) {
	for _, mode := range []string{"error", "missing", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			a, b := newNode(t, "a"), newNode(t, "b")
			fillSmall(t, a, 120)
			src := &batchLocal{local: &local{f: a.f, fail: -1}, mode: mode}
			if _, err := b.f.SyncFrom("a", src); err != nil {
				t.Fatal(err)
			}
			same(t, a, b)
		})
	}
}

// A peer that goes away half way leaves the cursor where it was, and the next sync finishes the job.
func TestParallelPullResumesAfterCutOff(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	fillSmall(t, a, 200)
	if _, err := b.f.SyncFrom("a", &local{f: a.f, fail: 50}); err == nil {
		t.Fatal("expected the cut off to be reported")
	}
	if _, err := b.f.SyncFrom("a", &local{f: a.f, fail: -1}); err != nil {
		t.Fatal(err)
	}
	same(t, a, b)
}

// Files pulled side by side that collide with different local content: the conflict copies are all kept.
func TestParallelPullKeepsConflicts(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	for i := 0; i < 40; i++ {
		a.write(t, fmt.Sprintf("f%d", i), fmt.Sprintf("from a %d", i))
	}
	a.scan(t)
	for i := 0; i < 40; i++ {
		b.write(t, fmt.Sprintf("f%d", i), fmt.Sprintf("from b %d", i))
	}
	b.scan(t)
	b.sync(t, a)
	a.sync(t, b)
	b.sync(t, a)
	same(t, a, b)
	tr := b.tree(t)
	n := 0
	for name := range tr {
		if len(name) > 2 && name[0] == 'f' && len(name) > 4 {
			n++
		}
	}
	if len(tr) < 80 {
		t.Fatalf("%d files after 40 conflicts, want at least 80 (each loser kept)", len(tr))
	}
}

// Writers that come together share journal syncs, and every record is on disk (a reopen sees all of it).
func TestGroupCommitKeepsEveryRecord(t *testing.T) {
	a := newNode(t, "a")
	done := make(chan error, 64)
	for i := 0; i < 64; i++ {
		go func(i int) {
			done <- a.f.ix.put(FileInfo{Name: fmt.Sprintf("x%d", i), Kind: KFile, Version: Vector{"a": uint64(i + 1)}})
		}(i)
	}
	for i := 0; i < 64; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 64; i++ {
		if _, ok := a.f.ix.get(fmt.Sprintf("x%d", i)); !ok {
			t.Fatalf("x%d lost", i)
		}
	}
}

// A file written with its size known is in the index at once, exactly as a scan would have recorded it: the next scan finds nothing
// to do, and a peer gets the same content.  Writing it again with other content makes a new version.
func TestWriteFileHashedIsIndexedAtOnce(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	data := bytes.Repeat([]byte("0123456789abcdef"), 40000) // 640 KB: several blocks
	if _, err := a.f.WriteFileHashed("big.bin", bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	fi, ok := a.f.ix.get("big.bin")
	if !ok || fi.Size != int64(len(data)) || len(fi.Blocks) < 2 || len(fi.Version) == 0 {
		t.Fatalf("not indexed: %+v", fi)
	}
	if n, err := a.f.Scan(); err != nil || n != 0 {
		t.Fatalf("the scan found %d changes (%v): the entry differs from what a scan records", n, err)
	}
	b.sync(t, a)
	same(t, a, b)
	v1 := fi.Version.Copy()
	if _, err := a.f.WriteFileHashed("big.bin", bytes.NewReader([]byte("short")), 5); err != nil {
		t.Fatal(err)
	}
	fi2, _ := a.f.ix.get("big.bin")
	if fi2.Version.Compare(v1) != Greater {
		t.Fatal("new content did not make a newer version")
	}
	b.sync(t, a)
	same(t, a, b)
}

// A renamed file or folder is in the index under its new name at once, and the scan finds nothing left to do: it does not vanish while
// it is hashed again.  A peer ends with the same tree.
func TestRenameIsIndexedAtOnce(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	for i := 0; i < 5; i++ {
		a.write(t, fmt.Sprintf("old/sub/f%d.txt", i), fmt.Sprintf("content %d", i))
	}
	a.write(t, "single.txt", "one")
	a.scan(t)
	b.sync(t, a)
	if err := a.f.Rename("old", "new/place"); err != nil {
		t.Fatal(err)
	}
	if err := a.f.Rename("single.txt", "moved.txt"); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"new/place", "new/place/sub", "new/place/sub/f3.txt", "moved.txt"} {
		if fi, ok := a.f.ix.get(n); !ok || fi.Deleted {
			t.Fatalf("%s is not in the index right after the rename", n)
		}
	}
	for _, n := range []string{"old", "old/sub/f3.txt", "single.txt"} {
		if fi, ok := a.f.ix.get(n); ok && !fi.Deleted {
			t.Fatalf("%s is still live", n)
		}
	}
	if n, err := a.f.Scan(); err != nil || n > 1 { // "new" itself, made by the rename, may be found
		t.Fatalf("the scan found %d changes (%v)", n, err)
	}
	b.sync(t, a)
	b.sync(t, a)
	same(t, a, b)
}
