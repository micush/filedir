package filesync

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type testNode struct {
	id NodeID
	f  *Folder
}

type local struct {
	mu   sync.Mutex
	f    *Folder
	down bool
	fail int // blocks served before failing; <0: no limit
}

func (l *local) Index(folder string, after int64) ([]FileInfo, int64, error) {
	if l.down {
		return nil, 0, errors.New("down")
	}
	e, m := l.f.Index(after)
	return e, m, nil
}

func (l *local) Block(folder, name string, off int64, size int, h Hash) ([]byte, error) {
	if l.down {
		return nil, errors.New("down")
	}
	l.mu.Lock()
	if l.fail == 0 {
		l.mu.Unlock()
		return nil, errors.New("cut off")
	}
	if l.fail > 0 {
		l.fail--
	}
	l.mu.Unlock()
	return l.f.Block(name, off, size, h)
}

var clock = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

var clockMu sync.Mutex

func tick() time.Time {
	clockMu.Lock()
	defer clockMu.Unlock()
	clock = clock.Add(time.Minute)
	return clock
}

func newNode(t *testing.T, id string, mut ...func(*Config)) *testNode {
	t.Helper()
	d := t.TempDir()
	cfg := Config{ID: "f", Root: filepath.Join(d, "root"), DBDir: filepath.Join(d, "db"), Node: NodeID(id), Create: true, Now: tick}
	for _, m := range mut {
		m(&cfg)
	}
	f, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return &testNode{NodeID(id), f}
}

func (n *testNode) path(name string) string { return filepath.Join(n.f.root, filepath.FromSlash(name)) }

func (n *testNode) write(t *testing.T, name, data string) {
	t.Helper()
	p := n.path(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	// distinct, ordered modification times
	mt := tick()
	os.Chtimes(p, mt, mt)
}

func (n *testNode) read(name string) (string, bool) {
	b, err := os.ReadFile(n.path(name))
	return string(b), err == nil
}

func (n *testNode) sync(t *testing.T, from *testNode) {
	t.Helper()
	from.f.Scan()
	if _, err := n.f.SyncFrom(from.id, &local{f: from.f, fail: -1}); err != nil {
		t.Fatalf("%s from %s: %v", n.id, from.id, err)
	}
}

func (n *testNode) scan(t *testing.T) {
	t.Helper()
	if _, err := n.f.Scan(); err != nil {
		t.Fatal(err)
	}
}

// tree lists every path under the folder (not its own directory) with its content.
func (n *testNode) tree(t *testing.T) map[string]string {
	if t != nil {
		t.Helper()
	}
	m := map[string]string{}
	filepath.Walk(n.f.root, func(p string, st os.FileInfo, err error) error {
		rel, _ := filepath.Rel(n.f.root, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if rel == MetaDir {
			return filepath.SkipDir
		}
		switch {
		case st.IsDir():
			m[rel+"/"] = ""
		case st.Mode()&os.ModeSymlink != 0:
			tg, _ := os.Readlink(p)
			m[rel] = "-> " + tg
		default:
			b, _ := os.ReadFile(p)
			m[rel] = string(b)
		}
		return nil
	})
	return m
}

func same(t *testing.T, a, b *testNode) {
	t.Helper()
	ta, tb := a.tree(t), b.tree(t)
	if fmt.Sprint(ta) != fmt.Sprint(tb) {
		t.Fatalf("%s and %s differ:\n%s: %v\n%s: %v", a.id, b.id, a.id, ta, b.id, tb)
	}
}

func TestPropagatesFilesDirsAndLinks(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.write(t, "x/y/z.txt", "hello")
	a.write(t, "empty", "")
	os.Symlink("x/y/z.txt", a.path("ln"))
	a.sync(t, b) // nothing yet: scans a
	b.sync(t, a)
	same(t, a, b)
	if s, _ := b.read("x/y/z.txt"); s != "hello" {
		t.Fatal(s)
	}
	if tg, _ := os.Readlink(b.path("ln")); tg != "x/y/z.txt" {
		t.Fatal(tg)
	}
}

func TestEditThenDeletePropagate(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.write(t, "d/f", "one")
	b.sync(t, a)
	b.write(t, "d/f", "two")
	a.sync(t, b)
	if s, _ := a.read("d/f"); s != "two" {
		t.Fatal(s)
	}
	os.Remove(a.path("d/f"))
	os.Remove(a.path("d"))
	b.sync(t, a)
	same(t, a, b)
	if len(b.tree(t)) != 0 {
		t.Fatal(b.tree(t))
	}
	// the old content is in the trash, not gone
	var found bool
	filepath.Walk(filepath.Join(b.f.root, MetaDir, "trash"), func(p string, st os.FileInfo, err error) error {
		if err == nil && !st.IsDir() {
			found = true
		}
		return nil
	})
	if !found {
		t.Fatal("the deleted file is not in the trash")
	}
}

func TestConcurrentEditKeepsBoth(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.write(t, "doc.txt", "base")
	b.sync(t, a)
	a.write(t, "doc.txt", "from a")
	b.write(t, "doc.txt", "from b") // later
	a.sync(t, b)
	b.sync(t, a)
	a.sync(t, b)
	same(t, a, b)
	tr := a.tree(t)
	if tr["doc.txt"] != "from b" {
		t.Fatalf("the later edit should win: %v", tr)
	}
	var cp string
	for k, v := range tr {
		if strings.HasPrefix(k, "doc.sync-conflict-") && strings.HasSuffix(k, ".txt") {
			cp = v
		}
	}
	if cp != "from a" {
		t.Fatalf("no conflict copy of a's edit: %v", tr)
	}
}

func TestEditBeatsDelete(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.write(t, "f", "base")
	b.sync(t, a)
	os.Remove(a.path("f"))
	b.write(t, "f", "edited")
	a.sync(t, b)
	b.sync(t, a)
	a.sync(t, b)
	same(t, a, b)
	if s, ok := a.read("f"); !ok || s != "edited" {
		t.Fatalf("the edit must survive: %q %v", s, ok)
	}
}

func TestSameContentConcurrentNoConflictCopy(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.write(t, "f", "same")
	b.write(t, "f", "same")
	b.f.cfg.IgnorePerms, a.f.cfg.IgnorePerms = true, true
	a.sync(t, b)
	b.sync(t, a)
	a.sync(t, b)
	same(t, a, b)
	if len(a.tree(t)) != 1 {
		t.Fatal(a.tree(t))
	}
}

func TestFileDirectoryFlip(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.write(t, "p/q", "file in dir")
	b.sync(t, a)
	os.RemoveAll(a.path("p"))
	a.write(t, "p", "now a file")
	b.sync(t, a)
	same(t, a, b)
	if s, _ := b.read("p"); s != "now a file" {
		t.Fatal(b.tree(t))
	}
	os.Remove(a.path("p"))
	os.MkdirAll(a.path("p/r"), 0o755)
	b.sync(t, a)
	same(t, a, b)
}

func TestRejectsHostileEntries(t *testing.T) {
	b := newNode(t, "b")
	outside := filepath.Join(filepath.Dir(b.f.root), "outside")
	bad := []string{"../outside", "/etc/passwd", "a/../../outside", MetaDir + "/x", "a//b", "", ".", "a/./b", "a\x00b"}
	var es []FileInfo
	for i, n := range bad {
		es = append(es, FileInfo{Name: n, Kind: KFile, Size: 1, Blocks: []Block{{0, 1, Hash{1}}}, Version: Vector{"evil": uint64(i + 1)}, By: "evil"})
	}
	// a link, then a file written through it
	es = append(es,
		FileInfo{Name: "ln", Kind: KSymlink, Target: outside, Version: Vector{"evil": 1}, By: "evil"},
		FileInfo{Name: "ln/pwn", Kind: KFile, Size: 0, Version: Vector{"evil": 1}, By: "evil"})
	src := &fake{es: es}
	b.f.SyncFrom("evil", src) // errors about the link are fine
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("something was written outside the folder")
	}
	if _, err := os.Stat(filepath.Join(outside, "pwn")); err == nil {
		t.Fatal("written through a link")
	}
}

type fake struct{ es []FileInfo }

func (s *fake) Index(string, int64) ([]FileInfo, int64, error) { return s.es, int64(len(s.es)), nil }
func (s *fake) Block(string, string, int64, int, Hash) ([]byte, error) {
	return nil, errors.New("no")
}

func TestBadBlockRejected(t *testing.T) {
	b := newNode(t, "b")
	fi := FileInfo{Name: "f", Kind: KFile, Size: 3, Blocks: []Block{{0, 3, Hash{9}}}, Version: Vector{"x": 1}, By: "x"}
	src := &lying{fake{es: []FileInfo{fi}}}
	if _, err := b.f.SyncFrom("x", src); err == nil {
		t.Fatal("a block with the wrong hash was accepted")
	}
	if _, ok := b.read("f"); ok {
		t.Fatal("a file with wrong data was written")
	}
	if e, _ := os.ReadDir(filepath.Join(b.f.root, MetaDir, "tmp")); len(e) != 0 {
		t.Fatal("temp file left")
	}
}

type lying struct{ fake }

func (s *lying) Block(string, string, int64, int, Hash) ([]byte, error) { return []byte("abc"), nil }

func TestMissingMarkerAndMassDelete(t *testing.T) {
	a := newNode(t, "a")
	for i := 0; i < 120; i++ {
		a.write(t, fmt.Sprintf("f%03d", i), "x")
	}
	a.scan(t)
	// an unmounted disk: the marker is gone
	os.RemoveAll(filepath.Join(a.f.root, MetaDir))
	for i := 0; i < 120; i++ {
		os.Remove(a.path(fmt.Sprintf("f%03d", i)))
	}
	if _, err := a.f.Scan(); !errors.Is(err, ErrNoMarker) {
		t.Fatalf("want no marker, got %v", err)
	}
	os.MkdirAll(filepath.Join(a.f.root, MetaDir), 0o700)
	if _, err := a.f.Scan(); !errors.Is(err, ErrMassDelete) {
		t.Fatalf("want mass delete guard, got %v", err)
	}
	for _, e := range a.f.Entries() {
		if e.Deleted {
			t.Fatal("nothing should have been marked deleted")
		}
	}
}

func TestOnlyChangedBlocksAreFetched(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	big := bytes.Repeat([]byte("0123456789abcdef"), 64<<10) // 1 MiB: 8 blocks
	a.write(t, "big", string(big))
	b.sync(t, a)
	big2 := append([]byte(nil), big...)
	big2[3*128<<10+5] = 'X' // one block
	a.write(t, "big", string(big2))
	src := &local{f: a.f, fail: -1}
	cnt := &counting{Source: src}
	a.scan(t)
	if _, err := b.f.SyncFrom("a", cnt); err != nil {
		t.Fatal(err)
	}
	if cnt.n != 1 {
		t.Fatalf("fetched %d blocks, want 1", cnt.n)
	}
	same(t, a, b)
}

type counting struct {
	Source
	n int
}

func (c *counting) Block(f, n string, o int64, s int, h Hash) ([]byte, error) {
	c.n++
	return c.Source.Block(f, n, o, s, h)
}

func TestInterruptedPullLeavesOldFileAndResumes(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	big := bytes.Repeat([]byte("z"), 1<<20)
	a.write(t, "f", "old")
	b.sync(t, a)
	a.write(t, "f", string(big))
	a.scan(t)
	if _, err := b.f.SyncFrom("a", &local{f: a.f, fail: 2}); err == nil {
		// all blocks are identical, so one fetch may serve... fall through only if it really finished
		if s, _ := b.read("f"); s != string(big) {
			t.Fatal("no error but not synced")
		}
	} else if s, _ := b.read("f"); s != "old" {
		t.Fatalf("the old file must stay while a pull is unfinished, got %d bytes", len(s))
	}
	b.sync(t, a)
	same(t, a, b)
}

func TestCrashBeforeSwapThenRecover(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.write(t, "f", "new content")
	a.scan(t)
	b.f.hook = func(s string) error {
		if s == "before-swap" {
			return errors.New("crash")
		}
		return nil
	}
	if _, err := b.f.SyncFrom("a", &local{f: a.f, fail: -1}); err == nil {
		t.Fatal("expected the crash")
	}
	if _, ok := b.read("f"); ok {
		t.Fatal("a partial file is visible")
	}
	b.f.hook = nil
	b.f.Close()
	nf, err := Open(b.f.cfg) // a restart
	if err != nil {
		t.Fatal(err)
	}
	b.f = nf
	b.sync(t, a)
	same(t, a, b)
}

func TestLocalWriteDuringPullAborts(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.write(t, "f", "v1")
	b.sync(t, a)
	a.write(t, "f", "v2 from a")
	a.scan(t)
	b.f.hook = func(s string) error {
		if s == "before-swap" {
			b.write(t, "f", "v2 typed on b, longer")
		}
		return nil
	}
	if _, err := b.f.SyncFrom("a", &local{f: a.f, fail: -1}); !errors.Is(err, ErrLocalChange) {
		t.Fatalf("want ErrLocalChange, got %v", err)
	}
	if s, _ := b.read("f"); s != "v2 typed on b, longer" {
		t.Fatal("the local edit was lost: " + s)
	}
	b.f.hook = nil
	// next round: both edits are kept, one as a conflict copy
	b.sync(t, a)
	a.sync(t, b)
	b.sync(t, a)
	same(t, a, b)
	n := 0
	for _, v := range a.tree(t) {
		if v == "v2 from a" || v == "v2 typed on b, longer" {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("both edits must survive: %v", a.tree(t))
	}
}

func TestDirDeleteKeepsUnknownLocalFile(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	a.write(t, "d/x", "x")
	b.sync(t, a)
	os.RemoveAll(a.path("d"))
	b.write(t, "d/new", "added on b")
	a.scan(t)
	b.sync(t, a)
	a.sync(t, b)
	b.sync(t, a)
	same(t, a, b)
	if s, _ := a.read("d/new"); s != "added on b" {
		t.Fatalf("lost a file: %v", a.tree(t))
	}
}

func TestIgnore(t *testing.T) {
	a := newNode(t, "a", func(c *Config) { c.Ignore = []string{"*.tmp", "cache/"} })
	b := newNode(t, "b")
	a.write(t, "k.tmp", "t")
	a.write(t, "cache/z", "z")
	a.write(t, "keep", "k")
	b.sync(t, a)
	if got := fmt.Sprint(b.tree(t)); got != "map[keep:k]" {
		t.Fatal(got)
	}
}

func TestMaintain(t *testing.T) {
	a, b := newNode(t, "a", func(c *Config) { c.TombDays = 1; c.TrashDays = 1 }), newNode(t, "b")
	a.write(t, "f", "x")
	b.sync(t, a)
	a.sync(t, b)
	os.Remove(b.path("f"))
	a.sync(t, b) // scans b? no: a pulls b's index after b scans
	b.scan(t)
	a.sync(t, b)
	clock = clock.Add(48 * time.Hour)
	if err := a.f.Maintain(); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.f.Entry("f"); ok {
		t.Fatal("old tombstone kept")
	}
	if e, _ := os.ReadDir(filepath.Join(a.f.root, MetaDir, "trash")); len(e) != 0 {
		t.Fatal("old trash kept")
	}
}

// A random soak: several nodes edit, delete and sync in random pairs, some syncs are cut off, some nodes restart; at the end a full
// round of syncs must leave every node with the same tree, and nothing written (and not deleted) may be lost.
func TestSoakConverges(t *testing.T) {
	for seed := int64(1); seed <= 12; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { soak(t, seed) })
	}
}

var dbgHook func([]*testNode)

var soakHook func([]*testNode)

func soak(t *testing.T, seed int64) {
	rng := rand.New(rand.NewSource(seed))
	const N = 5
	var ns []*testNode
	for i := 0; i < N; i++ {
		ns = append(ns, newNode(t, fmt.Sprintf("n%d", i)))
	}
	if soakHook != nil {
		soakHook(ns)
	}
	names := []string{"a", "b", "c", "d/e", "d/f", "d/g/h", "i", "j/k"}
	wrote := map[string]bool{} // contents ever written
	op := ""
	for step := 0; step < 300; step++ {
		if step > 0 {
			checkNoClash(t, ns, step, op)
		}
		n := ns[rng.Intn(N)]
		switch r := rng.Intn(10); {
		case r < 3:
			nm := names[rng.Intn(len(names))]
			val := fmt.Sprintf("v%d-%d", seed, step)
			// don't write under a path that is a file here
			if err := os.MkdirAll(filepath.Dir(n.path(nm)), 0o755); err != nil {
				continue
			}
			if st, err := os.Lstat(n.path(nm)); err == nil && st.IsDir() {
				continue
			}
			if os.WriteFile(n.path(nm), []byte(val), 0o644) == nil {
				mt := tick()
				os.Chtimes(n.path(nm), mt, mt)
				wrote[val] = true
			}
		case r < 4:
			os.Remove(n.path(names[rng.Intn(len(names))]))
		default:
			m := ns[rng.Intn(N)]
			if m == n {
				continue
			}
			fail := -1
			if rng.Intn(5) == 0 {
				fail = rng.Intn(2)
			}
			op = fmt.Sprintf("%s from %s", n.id, m.id)
			m.f.Scan()
			n.f.SyncFrom(m.id, &local{f: m.f, fail: fail, down: rng.Intn(8) == 0})
		}
	}
	// settle: everyone syncs with everyone, a few rounds
	for round := 0; round < 4; round++ {
		for _, n := range ns {
			for _, m := range ns {
				if n != m {
					m.f.Scan()
					if _, err := n.f.SyncFrom(m.id, &local{f: m.f, fail: -1}); err != nil {
						t.Fatalf("settle: %v", err)
					}
				}
			}
		}
	}
	if dbgHook != nil {
		dbgHook(ns)
	}
	for i := 1; i < N; i++ {
		same(t, ns[0], ns[i])
	}
	// every value that is still on some node's disk at the end of the random phase was kept or replaced by a newer one; here we only
	// check that nothing was invented
	for _, v := range ns[0].tree(t) {
		if v != "" && !wrote[v] {
			t.Fatalf("content nobody wrote: %q", v)
		}
	}
	var names2 []string
	for k := range ns[0].tree(t) {
		names2 = append(names2, k)
	}
	sort.Strings(names2)
}

// two entries with the same version must describe the same content
func checkNoClash(t *testing.T, ns []*testNode, step int, op string) {
	for _, a := range ns {
		for _, b := range ns {
			for _, ea := range a.f.Entries() {
				eb, ok := b.f.Entry(ea.Name)
				if ok && !ea.Deleted && !eb.Deleted && ea.Version.Compare(eb.Version) == Equal && !sameContent(&ea, &eb, false) {
					t.Fatalf("step %d (%s): %s and %s hold %s at the same version %v with different content", step, op, a.id, b.id, ea.Name, ea.Version)
				}
			}
		}
	}
}

func TestFileManagerOps(t *testing.T) {
	a, b := newNode(t, "a"), newNode(t, "b")
	f := a.f
	if _, err := f.WriteFile("docs/one.txt", strings.NewReader("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteFile("docs/one.txt", strings.NewReader("second")); err != nil {
		t.Fatal(err)
	}
	if s, _ := a.read("docs/one.txt"); s != "second" {
		t.Fatal(s)
	}
	if err := f.Mkdir("docs/sub/deep"); err != nil {
		t.Fatal(err)
	}
	if err := f.Rename("docs/one.txt", "docs/sub/two.txt"); err != nil {
		t.Fatal(err)
	}
	if err := f.Rename("docs/sub/two.txt", "docs/sub"); !errors.Is(err, ErrExists) {
		t.Fatalf("rename onto a folder: %v", err)
	}
	if _, err := f.WriteFile("docs/sub", strings.NewReader("x")); !errors.Is(err, ErrIsDir) {
		t.Fatalf("write over a folder: %v", err)
	}
	fh, _, err := f.OpenRead("docs/sub/two.txt")
	if err != nil {
		t.Fatal(err)
	}
	bs, _ := io.ReadAll(fh)
	fh.Close()
	if string(bs) != "second" {
		t.Fatal(string(bs))
	}
	// the same changes reach another node
	b.sync(t, a)
	same(t, a, b)
	if err := f.Delete("docs/sub"); err != nil {
		t.Fatal(err)
	}
	b.sync(t, a)
	same(t, a, b)
	if _, ok := b.read("docs/sub/two.txt"); ok {
		t.Fatal("the deletion did not arrive")
	}
	// the first and the replaced versions are in the trash
	var kept int
	filepath.Walk(filepath.Join(a.f.root, MetaDir, "trash"), func(p string, st os.FileInfo, err error) error {
		if err == nil && !st.IsDir() {
			kept++
		}
		return nil
	})
	if kept != 2 {
		t.Fatalf("trash holds %d files, want 2", kept)
	}
}

func TestFileManagerRefusesEscapes(t *testing.T) {
	a := newNode(t, "a")
	outside := filepath.Join(filepath.Dir(a.f.root), "secret")
	os.WriteFile(outside, []byte("s"), 0o644)
	os.Symlink(filepath.Dir(a.f.root), a.path("up"))
	os.Symlink(outside, a.path("lnk"))
	for _, n := range []string{"../secret", "/etc/passwd", "up/secret", MetaDir + "/x", "a/../../secret", ""} {
		if _, _, err := a.f.OpenRead(n); err == nil {
			t.Errorf("OpenRead(%q) worked", n)
		}
		if _, err := a.f.WriteFile(n, strings.NewReader("x")); err == nil {
			t.Errorf("WriteFile(%q) worked", n)
		}
		if err := a.f.Delete(n); err == nil {
			t.Errorf("Delete(%q) worked", n)
		}
		if err := a.f.Mkdir(n); err == nil {
			t.Errorf("Mkdir(%q) worked", n)
		}
	}
	if _, _, err := a.f.OpenRead("lnk"); err == nil {
		t.Error("a link was followed")
	}
	if b, _ := os.ReadFile(outside); string(b) != "s" {
		t.Error("the outside file changed")
	}
}

func TestWriteFileRespectsMaxSize(t *testing.T) {
	a := newNode(t, "a", func(c *Config) { c.MaxFileSize = 10 })
	if _, err := a.f.WriteFile("big", strings.NewReader("0123456789ab")); !errors.Is(err, ErrTooBig) {
		t.Fatalf("got %v", err)
	}
	if _, ok := a.read("big"); ok {
		t.Fatal("a too large file was kept")
	}
}
