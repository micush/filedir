package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filedir/filesync"
)

// A folder whose index is from before the block hash was BLAKE3 is converted by the service, shows that it is, and is opened when done.
func TestAFolderWithAnOldIndexIsConvertedAndThenOpened(t *testing.T) {
	e := newWebEnv(t)
	dir := t.TempDir()
	mk := func() *fdService {
		s := newFdService(nil, e.mg)
		s.cfgFn = func() FilesConfig { return FilesConfig{Folders: []FolderCfg{{ID: "docs", Path: dir}}} }
		return s
	}
	s := mk()
	s.reconcile()
	data := make([]byte, 700<<10)
	for i := range data {
		data[i] = byte(i * 13)
	}
	os.WriteFile(filepath.Join(dir, "a.bin"), data, 0o644)
	s.scanAll()
	if _, _, ok := s.List("docs", "", 10); !ok {
		t.Fatal("the folder did not open")
	}
	s.Stop()
	// make its index what version 80 wrote
	state := e.mg.stateDir
	if state == "" {
		state = "/var/lib/filedir"
	}
	p := filepath.Join(state, "files", fdDBName("docs"), "index.jsonl")
	in, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var ln struct {
			Alg  string          `json:"alg"`
			File *map[string]any `json:"f"`
		}
		var raw map[string]json.RawMessage
		json.Unmarshal(sc.Bytes(), &raw)
		json.Unmarshal(sc.Bytes(), &ln)
		if ln.Alg != "" {
			continue
		}
		if ln.File != nil {
			f := *ln.File
			if blks, ok := f["blk"].([]any); ok {
				for _, b := range blks {
					bm := b.(map[string]any)
					off, size := int64(bm["o"].(float64)), int64(bm["s"].(float64))
					sum := sha256.Sum256(data[off : off+size])
					bm["h"] = hexOf(sum[:])
				}
			}
			raw["f"], _ = json.Marshal(f)
		}
		b, _ := json.Marshal(raw)
		out = append(out, append(b, '\n')...)
	}
	in.Close()
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}

	s2 := mk()
	t.Cleanup(s2.Stop)
	s2.reconcile()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if f := s2.get("docs"); f != nil && f.f != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the folder was not opened after the conversion")
		}
		time.Sleep(20 * time.Millisecond)
		s2.reconcile()
	}
	s2.scanAll()
	es := s2.get("docs").f.Entries()
	if len(es) != 1 || es[0].Name != "a.bin" || len(es[0].Blocks) == 0 {
		t.Fatalf("entries after the conversion: %+v", es)
	}
	if es[0].Blocks[0].Hash != func() filesync.Hash { _, b, _ := hashOf(data[:es[0].Blocks[0].Size]); return b }() {
		t.Fatal("the first block does not have the current hash")
	}
}

// A node that asks for the index without saying which block hash it uses (version 80 or older) is told to update; one that says is served.
func TestIndexRequestsFromOldNodesAreRefusedWithAReason(t *testing.T) {
	s := newFdService(nil, nil)
	c := &Cluster{files: s}
	for _, tc := range []struct {
		body string
		code int
		msg  string
	}{
		{`{"folder":"x","after":0}`, http.StatusConflict, "blake3"},
		{`{"folder":"x","after":0,"alg":"blake3"}`, http.StatusNotFound, "no such folder"},
	} {
		rw := httptest.NewRecorder()
		c.handleFilesIndex(rw, httptest.NewRequest("POST", "/cluster/files/index", nil), ClusterPeer{}, []byte(tc.body))
		if rw.Code != tc.code || !strings.Contains(rw.Body.String(), tc.msg) {
			t.Errorf("%s: %d %s", tc.body, rw.Code, rw.Body.String())
		}
	}
}

func hexOf(b []byte) string {
	const d = "0123456789abcdef"
	o := make([]byte, 0, 2*len(b))
	for _, c := range b {
		o = append(o, d[c>>4], d[c&15])
	}
	return string(o)
}

// hashOf is the current hash of a block, for the test to compare with.
func hashOf(b []byte) (int, filesync.Hash, error) {
	hs, _, err := filesync.HashBlocks(strings.NewReader(string(b)), int64(len(b)))
	if err != nil || len(hs) != 1 {
		return 0, filesync.Hash{}, err
	}
	return len(b), hs[0].Hash, nil
}
