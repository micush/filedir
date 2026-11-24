package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mcpFilesEnv(t *testing.T, acl string) (*webEnv, func(string, string) (string, bool), string) {
	e, post := mcpEnv(t, acl)
	root := t.TempDir()
	s := newFdService(nil, e.mg)
	s.cfgFn = func() FilesConfig { return FilesConfig{Folders: []FolderCfg{{ID: "docs", Path: root}}} }
	old := fdSvc
	fdSvc = s
	t.Cleanup(func() { fdSvc = old; s.Stop() })
	s.reconcile()
	call := func(name, args string) (string, bool) {
		_, m := post(mcpCall(name, args))
		if m["error"] != nil {
			return "protocol error: " + string(jb(m["error"])), true
		}
		return toolResult(t, m)
	}
	return e, call, root
}

func TestMCPFilesWriteAndRead(t *testing.T) {
	_, call, root := mcpFilesEnv(t, "127.0.0.0/8 rw")
	// text, exactly as given (a leading space and a trailing newline are kept)
	body := "  hello world\nsecond line\n"
	if txt, bad := call("files_write", string(jb(map[string]any{"folder": "docs", "path": "notes/a.txt", "content": body}))); bad || !strings.Contains(txt, `"complete":true`) {
		t.Fatalf("write: %s", txt)
	}
	if b, err := os.ReadFile(filepath.Join(root, "notes", "a.txt")); err != nil || string(b) != body {
		t.Fatalf("on disk: %q %v", b, err)
	}
	var r struct {
		Content, Encoding string
		Size, Length      int64
		EOF               bool
	}
	txt, bad := call("files_read", `{"folder":"docs","path":"notes/a.txt"}`)
	if bad || json.Unmarshal([]byte(txt), &r) != nil || r.Encoding != "text" || r.Content != body || !r.EOF || r.Size != int64(len(body)) {
		t.Fatalf("read: %s", txt)
	}
	// binary, as base64, in parts
	bin := make([]byte, 2500)
	for i := range bin {
		bin[i] = byte(i * 7)
	}
	bin[10] = 0
	if txt, bad := call("files_write", string(jb(map[string]any{"folder": "docs", "path": "b.bin", "encoding": "base64", "content": base64.StdEncoding.EncodeToString(bin)}))); bad {
		t.Fatalf("write bin: %s", txt)
	}
	var got []byte
	for off := 0; ; {
		txt, bad := call("files_read", string(jb(map[string]any{"folder": "docs", "path": "b.bin", "offset": off, "length": 1000})))
		if bad || json.Unmarshal([]byte(txt), &r) != nil || r.Encoding != "base64" {
			t.Fatalf("read part at %d: %s", off, txt)
		}
		b, err := base64.StdEncoding.DecodeString(r.Content)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, b...)
		off += int(r.Length)
		if r.EOF {
			break
		}
		if r.Length != 1000 {
			t.Fatalf("a part of %d bytes, not 1000", r.Length)
		}
	}
	if !bytes.Equal(got, bin) {
		t.Fatal("the binary file did not come back as it went")
	}
}

func TestMCPFilesWriteInParts(t *testing.T) {
	_, call, root := mcpFilesEnv(t, "127.0.0.0/8 rw")
	part := func(off, n int, more bool, data string) (string, bool) {
		return call("files_write", string(jb(map[string]any{"folder": "docs", "path": "big/c.txt", "offset": off, "more": more, "content": data})))
	}
	if txt, bad := part(0, 0, true, "aaaa"); bad || !strings.Contains(txt, `"complete":false`) {
		t.Fatalf("first: %s", txt)
	}
	if _, err := os.Stat(filepath.Join(root, "big", "c.txt")); err == nil {
		t.Fatal("half a file is in the folder")
	}
	if txt, bad := part(9, 0, true, "xx"); !bad || !strings.Contains(txt, "offset") {
		t.Fatalf("a wrong offset was taken: %s", txt)
	}
	if txt, bad := part(4, 0, true, "bbbb"); bad {
		t.Fatalf("second: %s", txt)
	}
	if txt, bad := part(8, 0, false, "cc"); bad || !strings.Contains(txt, `"complete":true`) || !strings.Contains(txt, `"size":10`) {
		t.Fatalf("last: %s", txt)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "big", "c.txt")); string(b) != "aaaabbbbcc" {
		t.Fatalf("assembled: %q", b)
	}
	// a start over discards the earlier try
	part(0, 0, true, "zz")
	part(0, 0, true, "yy")
	if txt, bad := part(2, 0, false, "!"); bad || !strings.Contains(txt, `"size":3`) {
		t.Fatalf("restart: %s", txt)
	}
}

func TestMCPFilesRefusals(t *testing.T) {
	e, call, root := mcpFilesEnv(t, "127.0.0.0/8 rw")
	_ = e
	for _, p := range []string{"../x.txt", ".filedir/x", "/etc/x"} {
		if txt, bad := call("files_write", string(jb(map[string]any{"folder": "docs", "path": p, "content": "x"}))); !bad {
			t.Errorf("%q was written: %s", p, txt)
		}
	}
	if _, bad := call("files_write", `{"folder":"nope","path":"a","content":"x"}`); !bad {
		t.Error("an unknown folder took a file")
	}
	if _, bad := call("files_write", `{"folder":"docs","path":"a","content":"!!","encoding":"base64"}`); !bad {
		t.Error("bad base64 was taken")
	}
	os.MkdirAll(filepath.Join(root, "d"), 0o755)
	if _, bad := call("files_read", `{"folder":"docs","path":"d"}`); !bad {
		t.Error("a folder was read as a file")
	}
	if _, bad := call("files_read", `{"folder":"docs","path":"missing"}`); !bad {
		t.Error("a missing file was read")
	}
}

func TestMCPFilesWriteNeedsReadWrite(t *testing.T) {
	_, post := mcpEnv(t, "127.0.0.0/8")
	_, m := post(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	names := strings.Join(toolNames(t, m), " ")
	if !strings.Contains(names, "files_read") || strings.Contains(names, "files_write") {
		t.Fatalf("a read-only caller's tools: %s", names)
	}
	_, m = post(mcpCall("files_write", `{"folder":"docs","path":"a","content":"x"}`))
	if txt, bad := toolResult(t, m); !bad || !strings.Contains(txt, "read-only") {
		t.Fatalf("refusal: %s", txt)
	}
}

func jb(v any) []byte { b, _ := json.Marshal(v); return b }
