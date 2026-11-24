package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// An assistant gets a link from the MCP server and moves a file with plain HTTP: the bytes never pass through the conversation.
func TestMCPTransferLinks(t *testing.T) {
	e, call, root := mcpFilesEnv(t, "127.0.0.0/8 rw")
	link := func(tool, path string, minutes int) (string, bool) {
		txt, bad := call(tool, string(jb(map[string]any{"folder": "docs", "path": path, "minutes": minutes})))
		if bad {
			return txt, true
		}
		var r struct{ URL string }
		if json.Unmarshal([]byte(txt), &r) != nil || r.URL == "" {
			t.Fatalf("%s: %s", tool, txt)
		}
		return strings.Replace(r.URL, "https://", "http://", 1), false
	}
	do := func(method, u string, body []byte) (int, []byte) {
		req, _ := http.NewRequest(method, u, bytes.NewReader(body))
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}
	data := make([]byte, 3<<20) // far more than a call could carry as base64
	for i := range data {
		data[i] = byte(i*31 + i>>8)
	}
	up, bad := link("files_upload_url", "in/big.bin", 5)
	if bad || !strings.HasPrefix(up, e.ts.URL+"/up/") || !strings.HasSuffix(up, "/big.bin") {
		t.Fatalf("upload link: %q %v (want a link on %s)", up, bad, e.ts.URL)
	}
	if code, b := do("PUT", up, data); code != 200 {
		t.Fatalf("put: %d %s", code, b)
	}
	if got, err := os.ReadFile(filepath.Join(root, "in", "big.bin")); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("on disk: %d bytes %v", len(got), err)
	}
	// the same link again replaces the file; POST works too
	if code, _ := do("POST", up, []byte("second")); code != 200 {
		t.Fatalf("post: %d", code)
	}
	down, bad := link("files_download_url", "in/big.bin", 5)
	if bad {
		t.Fatalf("download link: %s", down)
	}
	if code, b := do("GET", down, nil); code != 200 || string(b) != "second" {
		t.Fatalf("get: %d %q", code, b)
	}
	// a download link is no upload link and the other way round; a changed token, an expired one and a link to nothing are refused
	if code, _ := do("PUT", strings.Replace(down, "/dl/", "/up/", 1), []byte("x")); code != 404 {
		t.Errorf("a download link wrote a file: %d", code)
	}
	if code, _ := do("GET", strings.Replace(up, "/up/", "/dl/", 1), nil); code != 404 {
		t.Errorf("an upload link read a file: %d", code)
	}
	if code, _ := do("PUT", strings.Replace(up, "/up/", "/up/x", 1), []byte("x")); code != 404 {
		t.Errorf("a changed token was taken: %d", code)
	}
	old := "/up/" + pubLinkSign(e.ws.linkKey(pubUpContext), pubLink{Folder: "docs", Path: "in/big.bin", Exp: time.Now().Add(-time.Minute).Unix()}) + "/big.bin"
	if code, _ := do("PUT", e.ts.URL+old, []byte("x")); code != 404 {
		t.Errorf("an expired link was taken: %d", code)
	}
	if txt, bad := link("files_download_url", "in/nothing.bin", 5); !bad || !strings.Contains(txt, "not found") {
		t.Errorf("a link to nothing: %q", txt)
	}
	if txt, bad := link("files_upload_url", "in/x", 5000); !bad || !strings.Contains(txt, "1440") {
		t.Errorf("a link for days: %q", txt)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "in", "big.bin")); string(got) != "second" {
		t.Errorf("a refused link changed the file: %q", got)
	}
}

// A read-only caller can ask for a download link but not an upload link.
func TestMCPUploadLinkNeedsReadWrite(t *testing.T) {
	_, call, _ := mcpFilesEnv(t, "127.0.0.0/8 ro")
	if txt, bad := call("files_upload_url", `{"folder":"docs","path":"a"}`); !bad || !strings.Contains(txt, "read-only") {
		t.Errorf("read-only got an upload link: %q", txt)
	}
}
