package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A read-write caller can make, copy, move, rename and delete in a synced folder; a delete goes to the trash; a read-only caller can do none of it.
func TestMCPFileOps(t *testing.T) {
	_, call, root := mcpFilesEnv(t, "127.0.0.0/8 rw")
	must := func(tool, args string) string {
		t.Helper()
		txt, bad := call(tool, args)
		if bad {
			t.Fatalf("%s %s: %s", tool, args, txt)
		}
		return txt
	}
	refused := func(tool, args, want string) {
		t.Helper()
		if txt, bad := call(tool, args); !bad || !strings.Contains(txt, want) {
			t.Errorf("%s %s: want an error with %q, got bad=%v %q", tool, args, want, bad, txt)
		}
	}
	has := func(rel string) bool { _, err := os.Stat(filepath.Join(root, rel)); return err == nil }
	must("files_write", `{"folder":"docs","path":"a/one.txt","content":"one"}`)
	must("files_mkdir", `{"folder":"docs","path":"b/c"}`)
	if !has("b/c") {
		t.Fatal("mkdir")
	}
	must("files_copy", `{"folder":"docs","path":"a/one.txt","to":"b/copy.txt"}`)
	if !has("a/one.txt") || !has("b/copy.txt") {
		t.Fatal("copy")
	}
	refused("files_copy", `{"folder":"docs","path":"a/one.txt","to":"b/copy.txt"}`, "already there")
	must("files_copy", `{"folder":"docs","path":"a/one.txt","to":"b/copy.txt","unique":true}`)
	if !has("b/copy copy.txt") {
		t.Errorf("unique copy: %v", dirNames(root, "b"))
	}
	must("files_move", `{"folder":"docs","path":"b/copy.txt","to":"b/c/moved.txt"}`)
	if has("b/copy.txt") || !has("b/c/moved.txt") {
		t.Fatal("move")
	}
	must("files_move", `{"folder":"docs","path":"b/c/moved.txt","to":"b/c/renamed.txt"}`)
	if has("b/c/moved.txt") || !has("b/c/renamed.txt") {
		t.Fatal("rename")
	}
	must("files_move", `{"folder":"docs","path":"a","to":"z"}`) // a folder tree
	if has("a") || !has("z/one.txt") {
		t.Fatal("move a tree")
	}
	refused("files_move", `{"folder":"docs","path":"z","to":"z/inside"}`, "itself")
	refused("files_move", `{"folder":"docs","path":"z","to":"nothere/z"}`, "not there")
	refused("files_move", `{"folder":"docs","path":"nothing","to":"x"}`, "not found")
	refused("files_delete", `{"folder":"docs","path":""}`, "required")
	refused("files_delete", `{"folder":"docs","path":"../x"}`, "")
	refused("files_mkdir", `{"folder":"nofolder","path":"x"}`, "no such folder")
	must("files_delete", `{"folder":"docs","path":"b"}`)
	if has("b") {
		t.Fatal("delete")
	}
	if !has("z/one.txt") {
		t.Fatal("a delete took more than it was asked to")
	}
}

func dirNames(root, rel string) []string {
	var out []string
	es, _ := os.ReadDir(filepath.Join(root, rel))
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func TestMCPFileOpsNeedReadWrite(t *testing.T) {
	_, call, _ := mcpFilesEnv(t, "127.0.0.0/8 ro")
	for _, c := range [][2]string{{"files_mkdir", `{"folder":"docs","path":"x"}`}, {"files_delete", `{"folder":"docs","path":"x"}`},
		{"files_move", `{"folder":"docs","path":"x","to":"y"}`}, {"files_copy", `{"folder":"docs","path":"x","to":"y"}`}} {
		if txt, bad := call(c[0], c[1]); !bad || !strings.Contains(txt, "read-only") {
			t.Errorf("%s: a read-only caller got %q", c[0], txt)
		}
	}
}

// A delete goes to the trash; the trash can be listed, restored from and, with a confirmation, emptied for good.
func TestMCPTrash(t *testing.T) {
	_, call, root := mcpFilesEnv(t, "127.0.0.0/8 rw")
	must := func(tool, args string) string {
		t.Helper()
		txt, bad := call(tool, args)
		if bad {
			t.Fatalf("%s %s: %s", tool, args, txt)
		}
		return txt
	}
	must("files_write", `{"folder":"docs","path":"keep.txt","content":"k"}`)
	must("files_write", `{"folder":"docs","path":"gone.txt","content":"g"}`)
	must("files_delete", `{"folder":"docs","path":"gone.txt"}`)
	if txt := must("files_trash", `{"folder":"docs"}`); !strings.Contains(txt, `"files":1`) {
		t.Fatalf("trash: %s", txt)
	}
	if txt, bad := call("files_trash_empty", `{"folder":"docs"}`); !bad || !strings.Contains(txt, "confirm") {
		t.Errorf("emptied without confirm: %q", txt)
	}
	if txt, bad := call("files_trash_empty", `{"folder":"docs","confirm":false}`); !bad {
		t.Errorf("emptied with confirm false: %q", txt)
	}
	if txt := must("files_trash", `{"folder":"docs"}`); !strings.Contains(txt, `"files":1`) {
		t.Fatalf("a refused empty took something: %s", txt)
	}
	must("files_trash_empty", `{"folder":"docs","confirm":true}`)
	if txt := must("files_trash", `{"folder":"docs"}`); !strings.Contains(txt, `"files":0`) {
		t.Fatalf("trash after emptying: %s", txt)
	}
	if _, err := os.Stat(filepath.Join(root, "keep.txt")); err != nil {
		t.Error("emptying the trash took a live file")
	}
	// restore
	must("files_delete", `{"folder":"docs","path":"keep.txt"}`)
	var l struct {
		Entries []struct{ When, Rel string }
	}
	if err := json.Unmarshal([]byte(must("files_trash", `{"folder":"docs"}`)), &l); err != nil || len(l.Entries) != 1 {
		t.Fatalf("listing: %v %+v", err, l)
	}
	must("files_trash_restore", string(jb(map[string]any{"folder": "docs", "when": l.Entries[0].When, "name": l.Entries[0].Rel})))
	if b, err := os.ReadFile(filepath.Join(root, "keep.txt")); err != nil || string(b) != "k" {
		t.Errorf("restored: %q %v", b, err)
	}
}

func TestMCPTrashNeedsReadWrite(t *testing.T) {
	_, call, _ := mcpFilesEnv(t, "127.0.0.0/8 ro")
	if txt, bad := call("files_trash_empty", `{"folder":"docs","confirm":true}`); !bad || !strings.Contains(txt, "read-only") {
		t.Errorf("read-only emptied the trash: %q", txt)
	}
	if _, bad := call("files_trash", `{"folder":"docs"}`); bad {
		t.Error("read-only cannot look at the trash")
	}
}
