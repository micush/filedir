package filesync

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// legacy rewrites the index in dir as an older version made it: SHA-256 block hashes, and no first line that names the hash.
func legacy(t *testing.T, dir, root string) {
	t.Helper()
	p := filepath.Join(dir, "index.jsonl")
	in, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var ln journalLine
		if err := json.Unmarshal(sc.Bytes(), &ln); err != nil {
			t.Fatal(err)
		}
		if ln.Alg != "" {
			continue
		}
		if ln.File != nil && len(ln.File.Blocks) > 0 {
			data, err := os.ReadFile(filepath.Join(root, ln.File.Name))
			if err != nil {
				t.Fatal(err)
			}
			for i, b := range ln.File.Blocks {
				ln.File.Blocks[i].Hash = sha256.Sum256(data[b.Off : b.Off+int64(b.Size)])
			}
		}
		b, _ := json.Marshal(ln)
		out = append(out, append(b, '\n')...)
	}
	in.Close()
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAnIndexOfSHA256HashesIsConvertedOnceAndKeepsItsVersions(t *testing.T) {
	root, db := t.TempDir(), t.TempDir()
	cfg := Config{ID: "x", Root: root, DBDir: db, Node: "n1", Create: true}
	f, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 3<<20+12345)
	for i := range big {
		big[i] = byte(i*7 + i>>9)
	}
	os.WriteFile(filepath.Join(root, "big.bin"), big, 0o644)
	os.WriteFile(filepath.Join(root, "small.txt"), []byte("hello"), 0o644)
	os.WriteFile(filepath.Join(root, "empty"), nil, 0o644)
	os.MkdirAll(filepath.Join(root, "d"), 0o755)
	os.WriteFile(filepath.Join(root, "d", "in.txt"), []byte("in a folder"), 0o644)
	if _, err := f.Scan(); err != nil {
		t.Fatal(err)
	}
	before := f.Entries()
	f.Close()
	legacy(t, db, root)

	if _, err := Open(cfg); !errors.Is(err, ErrLegacyIndex) {
		t.Fatalf("Open of an old index: %v", err)
	}
	var last int
	if err := MigrateIndex(cfg, func(done, total int) { last = done }); err != nil {
		t.Fatal(err)
	}
	if last != 3 {
		t.Fatalf("progress ended at %d files, want 3", last)
	}
	g, err := Open(cfg)
	if err != nil {
		t.Fatalf("Open after the conversion: %v", err)
	}
	defer g.Close()
	after := g.Entries()
	if len(after) != len(before) {
		t.Fatalf("%d entries, were %d", len(after), len(before))
	}
	for i := range before {
		a, b := before[i], after[i]
		if a.Name != b.Name || a.Version.String() != b.Version.String() || a.Seq != b.Seq {
			t.Errorf("%s: version or sequence changed: %v %d -> %v %d", a.Name, a.Version, a.Seq, b.Version, b.Seq)
		}
		if len(a.Blocks) != len(b.Blocks) {
			t.Fatalf("%s: %d blocks, were %d", a.Name, len(b.Blocks), len(a.Blocks))
		}
		for k := range a.Blocks {
			if a.Blocks[k] != b.Blocks[k] {
				t.Errorf("%s block %d: %x, want %x", a.Name, k, b.Blocks[k], a.Blocks[k])
			}
		}
	}
	if n, err := g.Scan(); err != nil || n != 0 {
		t.Fatalf("a scan after the conversion found %d changes (%v): there should be none", n, err)
	}
	if err := MigrateIndex(cfg, nil); err != nil { // a current index is left alone
		t.Fatal(err)
	}
}

func TestAFileThatChangedBeforeTheConversionIsAChangeAfterIt(t *testing.T) {
	root, db := t.TempDir(), t.TempDir()
	cfg := Config{ID: "x", Root: root, DBDir: db, Node: "n1", Create: true}
	f, _ := Open(cfg)
	p := filepath.Join(root, "a.txt")
	os.WriteFile(p, []byte("one"), 0o644)
	f.Scan()
	v1, _ := f.Entry("a.txt")
	f.Close()
	legacy(t, db, root)
	os.WriteFile(p, []byte("two!"), 0o644) // changed while the node was off
	if err := MigrateIndex(cfg, nil); err != nil {
		t.Fatal(err)
	}
	g, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if n, _ := g.Scan(); n != 1 {
		t.Fatalf("the scan found %d changes, want 1", n)
	}
	v2, _ := g.Entry("a.txt")
	if v2.Version.String() == v1.Version.String() || v2.Size != 4 {
		t.Fatalf("the change was not made a new version: %v -> %v, size %d", v1.Version, v2.Version, v2.Size)
	}
}
