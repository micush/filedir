package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var davClient = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func dav(t *testing.T, e *siteEnv, user, method, p string, body string, hdr ...string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(method, e.site.URL+p, strings.NewReader(body))
	if user != "" {
		req.SetBasicAuth(user, "pw")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := davClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return res.StatusCode, b.String(), res.Header
}

func TestWebDAV(t *testing.T) {
	e := newSiteEnv(t, true)
	if code, _, h := dav(t, e, "", "PROPFIND", "/dav/", ""); code != 401 || !strings.Contains(h.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("no sign-in: %d %v", code, h)
	}
	if code, _, _ := dav(t, e, "bob", "PROPFIND", "/dav/", ""); code != 401 {
		t.Fatalf("a person with no group: %d", code)
	}
	if code, _, h := dav(t, e, "pat", "OPTIONS", "/dav/", ""); code != 200 || !strings.Contains(h.Get("DAV"), "2") || !strings.Contains(h.Get("Allow"), "MKCOL") {
		t.Fatalf("options: %d %v", code, h)
	}
	// no root folder: /dav/ lists the folders
	code, b, _ := dav(t, e, "pat", "PROPFIND", "/dav/", "", "Depth", "1")
	if code != 207 || !strings.Contains(b, "<D:href>/dav/docs/</D:href>") {
		t.Fatalf("top: %d %s", code, b)
	}
	code, b, _ = dav(t, e, "pat", "PROPFIND", "/dav/docs/", "", "Depth", "1")
	if code != 207 || !strings.Contains(b, "/dav/docs/a.txt") || !strings.Contains(b, "/dav/docs/sub/") || !strings.Contains(b, "<D:getcontentlength>5</D:getcontentlength>") || strings.Contains(b, ".filedir") {
		t.Fatalf("folder: %d %s", code, b)
	}
	if code, b, _ := dav(t, e, "pat", "PROPFIND", "/dav/docs/", "", "Depth", "infinity"); code != 403 || !strings.Contains(b, "propfind-finite-depth") {
		t.Fatalf("infinity: %d %s", code, b)
	}
	code, b, _ = dav(t, e, "pat", "PROPFIND", "/dav/docs/a.txt", `<?xml version="1.0"?><propfind xmlns="DAV:"><prop><getcontentlength/><x:nope xmlns:x="urn:x"/></prop></propfind>`, "Depth", "0")
	if code != 207 || !strings.Contains(b, "<D:getcontentlength>5<") || !strings.Contains(b, "404 Not Found") || !strings.Contains(b, `<nope xmlns="urn:x"/>`) {
		t.Fatalf("named props: %d %s", code, b)
	}
	if code, _, _ := dav(t, e, "pat", "PROPFIND", "/dav/docs/none.txt", "", "Depth", "0"); code != 404 {
		t.Fatalf("missing: %d", code)
	}
	if code, b, h := dav(t, e, "pat", "GET", "/dav/docs/sub/b.txt", ""); code != 200 || b != "beta" || h.Get("ETag") == "" {
		t.Fatalf("get: %d %q", code, b)
	}
	if code, b, _ := dav(t, e, "pat", "GET", "/dav/docs/a.txt", "", "Range", "bytes=1-2"); code != 206 || b != "lp" {
		t.Fatalf("range: %d %q", code, b)
	}
	if code, _, h := dav(t, e, "pat", "GET", "/dav/docs/sub/", ""); code != 303 || h.Get("Location") != "/" {
		t.Fatalf("a folder in a browser: %d %v", code, h)
	}

	// writes land on the disk
	if code, _, _ := dav(t, e, "pat", "PUT", "/dav/docs/new.txt", "hello"); code != 201 {
		t.Fatalf("put: %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(e.root, "new.txt")); string(b) != "hello" {
		t.Fatalf("on disk: %q", b)
	}
	if code, _, _ := dav(t, e, "pat", "PUT", "/dav/docs/new.txt", "hello2"); code != 204 {
		t.Fatalf("replace: %d", code)
	}
	if code, _, _ := dav(t, e, "pat", "PUT", "/dav/docs/nodir/x.txt", "x"); code != 409 {
		t.Fatalf("no parent: %d", code)
	}
	if code, _, _ := dav(t, e, "pat", "PUT", "/dav/docs/sub", "x"); code != 405 {
		t.Fatalf("put on a folder: %d", code)
	}
	if code, _, _ := dav(t, e, "pat", "MKCOL", "/dav/docs/made", ""); code != 201 {
		t.Fatalf("mkcol: %d", code)
	}
	if code, _, _ := dav(t, e, "pat", "MKCOL", "/dav/docs/made", ""); code != 405 {
		t.Fatalf("mkcol again: %d", code)
	}
	if st, err := os.Stat(filepath.Join(e.root, "made")); err != nil || !st.IsDir() {
		t.Fatalf("folder on disk: %v", err)
	}
	if code, _, h := dav(t, e, "pat", "LOCK", "/dav/docs/new.txt", ""); code != 200 || !strings.HasPrefix(h.Get("Lock-Token"), "<opaquelocktoken:") {
		t.Fatalf("lock: %d %v", code, h)
	}
	if code, _, _ := dav(t, e, "pat", "UNLOCK", "/dav/docs/new.txt", ""); code != 204 {
		t.Fatalf("unlock: %d", code)
	}
	if code, b, _ := dav(t, e, "pat", "PROPPATCH", "/dav/docs/new.txt", `<propertyupdate xmlns="DAV:"><set><prop><x:t xmlns:x="urn:x">1</x:t></prop></set></propertyupdate>`); code != 207 || !strings.Contains(b, "200 OK") {
		t.Fatalf("proppatch: %d %s", code, b)
	}

	// copy and move, in a folder and into a copy of a folder
	dest := "http://" + strings.TrimPrefix(e.site.URL, "http://") + "/dav/docs/"
	if code, _, _ := dav(t, e, "pat", "COPY", "/dav/docs/sub", "", "Destination", dest+"sub2"); code != 201 {
		t.Fatalf("copy folder: %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(e.root, "sub2", "b.txt")); string(b) != "beta" {
		t.Fatalf("copied: %q", b)
	}
	if code, _, _ := dav(t, e, "pat", "COPY", "/dav/docs/a.txt", "", "Destination", dest+"sub2/b.txt", "Overwrite", "F"); code != 412 {
		t.Fatalf("overwrite F: %d", code)
	}
	if code, _, _ := dav(t, e, "pat", "MOVE", "/dav/docs/a.txt", "", "Destination", dest+"sub2/b.txt"); code != 204 {
		t.Fatalf("move over: %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(e.root, "sub2", "b.txt")); string(b) != "alpha" {
		t.Fatalf("moved: %q", b)
	}
	if _, err := os.Stat(filepath.Join(e.root, "a.txt")); err == nil {
		t.Fatal("the moved file is still there")
	}
	if code, _, _ := dav(t, e, "pat", "MOVE", "/dav/docs/sub", "", "Destination", dest+"sub/in"); code != 403 {
		t.Fatalf("into itself: %d", code)
	}
	if code, _, _ := dav(t, e, "pat", "MOVE", "/dav/docs/sub", "", "Destination", "http://elsewhere.example/dav/docs/x"); code != 502 {
		t.Fatalf("another server: %d", code)
	}
	if code, _, _ := dav(t, e, "pat", "DELETE", "/dav/docs/sub2", ""); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	if _, err := os.Stat(filepath.Join(e.root, "sub2")); err == nil {
		t.Fatal("deleted folder is still there")
	}
	if code, _, _ := dav(t, e, "pat", "DELETE", "/dav/docs", ""); code != 403 {
		t.Fatalf("delete the folder itself: %d", code)
	}
	if code, _, _ := dav(t, e, "pat", "PUT", "/dav/docs/x.tmp", "x", "Origin", "https://evil.example"); code != 403 {
		t.Fatalf("cross-origin write: %d", code)
	}

	// a person who may only look
	if code, _, _ := dav(t, e, "rita", "PROPFIND", "/dav/docs/", "", "Depth", "1"); code != 207 {
		t.Fatalf("read-only lists: %d", code)
	}
	if code, b, _ := dav(t, e, "rita", "GET", "/dav/docs/sub/b.txt", ""); code != 200 || b != "beta" {
		t.Fatalf("read-only reads: %d", code)
	}
	for _, m := range []string{"PUT", "DELETE", "MKCOL", "MOVE", "COPY", "LOCK", "PROPPATCH"} {
		if code, _, _ := dav(t, e, "rita", m, "/dav/docs/ro.txt", "x", "Destination", dest+"ro2"); code != 403 {
			t.Fatalf("read-only %s: %d", m, code)
		}
	}
	if _, err := os.Stat(filepath.Join(e.root, "ro.txt")); err == nil {
		t.Fatal("a read-only person wrote a file")
	}
}

// With a root folder, /dav/ is that folder and the others are listed in it, as on the Files page.
func TestWebDAVRootFolder(t *testing.T) {
	e := newSiteEnv(t, true)
	rootDir := filepath.Join(filepath.Dir(e.root), "rootdir")
	s := fdSvc
	s.cfgFn = func() FilesConfig {
		return FilesConfig{Folders: []FolderCfg{{ID: "/", Path: rootDir}, {ID: "docs", Path: e.root}}}
	}
	s.reconcile()
	os.WriteFile(filepath.Join(rootDir, "top.txt"), []byte("top"), 0o644)
	code, b, _ := dav(t, e, "pat", "PROPFIND", "/dav/", "", "Depth", "1")
	if code != 207 || !strings.Contains(b, "/dav/docs/") || !strings.Contains(b, "/dav/top.txt") {
		t.Fatalf("root: %d %s", code, b)
	}
	if code, _, _ := dav(t, e, "pat", "PUT", "/dav/deep.txt", "d"); code != 201 {
		t.Fatalf("put in root: %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(rootDir, "deep.txt")); string(b) != "d" {
		t.Fatalf("root file on disk: %q", b)
	}
	if code, b, _ := dav(t, e, "pat", "GET", "/dav/docs/sub/b.txt", ""); code != 200 || b != "beta" {
		t.Fatalf("a folder in the root: %d %q", code, b)
	}
}

func TestWebDAVLockoutAndLimit(t *testing.T) {
	e := newSiteEnv(t, true)
	pol := *e.ws.policy.Load()
	pol.MaxFailedLogins = 2
	e.ws.policy.Store(&pol)
	bad := func() int {
		req, _ := http.NewRequest("PROPFIND", e.site.URL+"/dav/", nil)
		req.SetBasicAuth("pat", "wrong")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	bad()
	bad()
	if code := bad(); code != http.StatusTooManyRequests {
		t.Fatalf("not locked out: %d", code)
	}
	if code, _, _ := dav(t, e, "pat", "PROPFIND", "/dav/", ""); code != http.StatusTooManyRequests {
		t.Fatalf("the right password during the lockout: %d", code)
	}
}

func TestFilesDiskLimitSetting(t *testing.T) {
	if (FilesConfig{}).diskPct() != 80 || (FilesConfig{MaxDiskPct: 100}).diskPct() != 0 || (FilesConfig{MaxDiskPct: 65}).diskPct() != 65 {
		t.Fatal("the limit is 80 unless set, and 100 is none")
	}
	for _, v := range []int{-1, 5, 101} {
		if (&FilesConfig{MaxDiskPct: v}).Validate() == nil {
			t.Fatalf("%d was accepted", v)
		}
	}
	if (&FilesConfig{MaxDiskPct: 90}).Validate() != nil || (&FilesConfig{}).Validate() != nil {
		t.Fatal("a good limit was refused")
	}
	if c := (&FilesConfig{MaxDiskPct: 70}).clone(); c == nil || c.MaxDiskPct != 70 {
		t.Fatalf("clone: %+v", c)
	}
}
