package filesync

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func zipTestFolder(t *testing.T, ignore ...string) *Folder {
	t.Helper()
	root := t.TempDir()
	f, err := Open(Config{ID: "docs", Root: root, DBDir: t.TempDir(), Node: "A", Create: true, Ignore: ignore})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func zipPut(t *testing.T, f *Folder, name, body string) {
	t.Helper()
	p := filepath.Join(f.root, filepath.FromSlash(name))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func zipRead(t *testing.T, b []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range zr.File {
		if strings.HasSuffix(e.Name, "/") {
			out[e.Name] = ""
			continue
		}
		rc, _ := e.Open()
		c, _ := io.ReadAll(rc)
		rc.Close()
		out[e.Name] = string(c)
	}
	return out
}

func keys(m map[string]string) []string {
	var k []string
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

func TestZipFolderAndSelection(t *testing.T) {
	f := zipTestFolder(t, "*.tmp")
	zipPut(t, f, "a.txt", "alpha")
	zipPut(t, f, "d/b.txt", "beta")
	zipPut(t, f, "d/sub/c.txt", "gamma")
	zipPut(t, f, "d/skip.tmp", "ignored")
	zipPut(t, f, "x/same.txt", "one")
	zipPut(t, f, "y/same.txt", "two")
	os.MkdirAll(filepath.Join(f.root, "empty"), 0o755)
	os.Symlink("/etc/passwd", filepath.Join(f.root, "d", "link"))

	var buf bytes.Buffer
	st, err := f.WriteZip(&buf, []string{"d", "a.txt", "x/same.txt", "y/same.txt", "empty"}, "docs")
	if err != nil {
		t.Fatal(err)
	}
	got := zipRead(t, buf.Bytes())
	want := []string{"a.txt", "d/", "d/b.txt", "d/sub/", "d/sub/c.txt", "empty/", "same (2).txt", "same.txt"}
	if strings.Join(keys(got), ",") != strings.Join(want, ",") {
		t.Fatalf("archive holds %v, want %v", keys(got), want)
	}
	if got["d/sub/c.txt"] != "gamma" || got["same.txt"] != "one" || got["same (2).txt"] != "two" {
		t.Fatalf("contents: %v", got)
	}
	if st.Files != 5 || st.Skipped != 1 {
		t.Fatalf("stats %+v: the link should be the one thing left out", st)
	}

	// the whole folder goes in under its name; the metadata folder never does
	buf.Reset()
	if _, err := f.WriteZip(&buf, []string{""}, "docs"); err != nil {
		t.Fatal(err)
	}
	got = zipRead(t, buf.Bytes())
	for n := range got {
		if !strings.HasPrefix(n, "docs/") && n != "docs/" {
			t.Fatalf("%q is not under the folder's name", n)
		}
		if strings.Contains(n, MetaDir) {
			t.Fatalf("the metadata folder is in the archive: %q", n)
		}
	}
	if got["docs/d/b.txt"] != "beta" {
		t.Fatalf("whole folder: %v", keys(got))
	}
}

func TestZipRefusals(t *testing.T) {
	f := zipTestFolder(t)
	zipPut(t, f, "ok.txt", "fine")
	os.MkdirAll(filepath.Join(f.root, "real"), 0o755)
	zipPut(t, f, "real/inner.txt", "inside")
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("no"), 0o644)
	os.Symlink(outside, filepath.Join(f.root, "out"))
	os.Symlink(filepath.Join(outside, "secret"), filepath.Join(f.root, "lnk"))

	for _, n := range []string{"../x", "/etc/passwd", "a/../../b", ".filedir", ".filedir/index", "a\\b", "out/secret"} {
		if _, err := f.WriteZip(io.Discard, []string{n}, "docs"); err == nil {
			t.Errorf("%q was accepted", n)
		}
	}
	if _, err := f.WriteZip(io.Discard, []string{"missing"}, "docs"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing name: %v", err)
	}
	if _, err := f.WriteZip(io.Discard, nil, "docs"); err == nil {
		t.Error("no names was accepted")
	}
	if _, err := f.WriteZip(io.Discard, make([]string, MaxZipItems+1), "docs"); err == nil {
		t.Error("too many names were accepted")
	}
	// a link asked for by name, or met on the way, is left out and nothing it points at is read
	var buf bytes.Buffer
	st, err := f.WriteZip(&buf, []string{"lnk", "ok.txt"}, "docs")
	if err != nil {
		t.Fatal(err)
	}
	got := zipRead(t, buf.Bytes())
	if len(got) != 1 || got["ok.txt"] != "fine" || st.Skipped != 1 {
		t.Fatalf("link by name: %v %+v", keys(got), st)
	}
	for n, c := range got {
		if strings.Contains(c, "no") && n != "ok.txt" {
			t.Fatal("a file behind a link was read")
		}
	}
}
