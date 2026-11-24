package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// filesAuth: "pat" is in the files group, "rita" in the read-only one, "alice" is an administrator, "bob" has a password and no group.
type filesAuth struct{ fakeAuth }

func (f *filesAuth) InGroup(user, group string) (bool, error) {
	switch group {
	case "users":
		return user == "pat", nil
	case "viewers":
		return user == "rita", nil
	case "filedir,filedir-users":
		return user == "alice", nil
	}
	return false, errors.New("wrong group " + group)
}

func (f *filesAuth) Authenticate(service, user, pass string) error {
	if pass == "pw" && (user == "pat" || user == "rita" || user == "alice" || user == "bob") {
		return nil
	}
	return errors.New("bad password")
}

type siteEnv struct {
	t    *testing.T
	ws   *WebServer
	site *httptest.Server
	adm  *httptest.Server
	root string
}

type siteClient struct {
	e      *siteEnv
	cookie string
	csrf   string
}

func newSiteEnv(t *testing.T, groups bool) *siteEnv {
	t.Helper()
	failDelay = time.Millisecond
	dir := t.TempDir()
	dc := newDaemonConfig()
	dc.Cluster.Enabled = false
	mg, err := NewMgmt(filepath.Join(dir, "filedir.conf"), filepath.Join(dir, "state"), dc)
	if err != nil {
		t.Fatal(err)
	}
	sup := NewSupervisor(context.Background(), newDaemonConfig())
	st := NewStatusServer("", sup)
	st.mg = mg
	ws := NewWebServer(mg, st, &filesAuth{})
	pol := defaultWeb()
	pol.MaxFailedLogins = 1000
	if groups {
		pol.FilesGroup, pol.FilesReadGroup = "users", "viewers"
	} else {
		pol.Group, pol.ReadGroup = "", "" // no group at all: nobody could sign in
	}
	ws.policy.Store(&pol)
	e := &siteEnv{t: t, ws: ws, root: filepath.Join(dir, "data")}
	e.site = httptest.NewServer(ws.filesSiteHandler())
	e.adm = httptest.NewServer(ws.Handler())
	t.Cleanup(func() { e.site.Close(); e.adm.Close() })
	s := newFdService(nil, mg)
	s.cfgFn = func() FilesConfig {
		return FilesConfig{Folders: []FolderCfg{{ID: "docs", Path: e.root, Ignore: []string{"*.tmp", ".git/"}}}}
	}
	fdSvc = s
	t.Cleanup(s.Stop)
	s.reconcile()
	os.MkdirAll(filepath.Join(e.root, "sub"), 0o755)
	os.WriteFile(filepath.Join(e.root, "a.txt"), []byte("alpha"), 0o644)
	os.WriteFile(filepath.Join(e.root, "sub", "b.txt"), []byte("beta"), 0o644)
	s.scanAll()
	return e
}

func (c *siteClient) req(base, method, path string, body any, mods ...func(*http.Request)) (int, []byte, http.Header) {
	c.e.t.Helper()
	var rd io.Reader
	if raw, ok := body.([]byte); ok {
		rd = bytes.NewReader(raw)
	} else if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, base+path, rd)
	if body != nil {
		if _, raw := body.([]byte); !raw {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	if c.cookie != "" {
		req.Header.Set("Cookie", c.cookie)
	}
	if method != "GET" && c.csrf != "" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	for _, m := range mods {
		m(req)
	}
	res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, b, res.Header
}

func (c *siteClient) login(user, pass string) int {
	c.e.t.Helper()
	code, b, h := c.req(c.e.site.URL, "POST", "/api/login", map[string]string{"username": user, "password": pass})
	c.cookie, c.csrf = "", ""
	if code == 200 {
		for _, ck := range (&http.Response{Header: h}).Cookies() {
			if ck.Name == filesCookie {
				c.cookie = ck.Name + "=" + ck.Value
				if !ck.HttpOnly || !ck.Secure || ck.SameSite != http.SameSiteStrictMode || ck.Path != "/" {
					c.e.t.Fatalf("cookie flags: %+v", ck)
				}
			}
			if ck.Name == sessionCookie {
				c.e.t.Fatal("the files site set the management GUI's cookie")
			}
		}
		var m map[string]any
		json.Unmarshal(b, &m)
		c.csrf, _ = m["csrf"].(string)
		for _, k := range []string{"hostname", "config_path"} {
			if _, has := m[k]; has {
				c.e.t.Fatalf("an end user was told %s", k)
			}
		}
	}
	return code
}

// The files site is always there: with no group at all it still answers, and nobody can sign in.
func TestFilesSiteIsAlwaysOn(t *testing.T) {
	e := newSiteEnv(t, false)
	c := &siteClient{e: e}
	for _, p := range []string{"/", "/app.js", "/style.css", "/api/login/state"} {
		if code, _, _ := c.req(e.site.URL, "GET", p, nil); code != 200 {
			t.Errorf("GET %s with no group set: %d", p, code)
		}
	}
	if code, _, _ := c.req(e.site.URL, "GET", "/api/files/list?folder=docs", nil); code != http.StatusUnauthorized {
		t.Errorf("list without a session: %d", code)
	}
	if code := c.login("pat", "pw"); code == 200 || code == 404 {
		t.Errorf("login with no group set: %d", code)
	}
}

func TestFilesSiteSignInAndWhatItServes(t *testing.T) {
	e := newSiteEnv(t, true)
	c := &siteClient{e: e}
	// the page and its script
	code, b, _ := c.req(e.site.URL, "GET", "/", nil)
	if code != 200 || !strings.Contains(string(b), `data-site="files"`) || strings.Contains(string(b), "help.js") {
		t.Fatalf("page: %d %s", code, b)
	}
	if code, _, _ := c.req(e.site.URL, "GET", "/app.js", nil); code != 200 {
		t.Fatalf("app.js: %d", code)
	}
	// who may sign in
	if code := c.login("bob", "pw"); code != 401 {
		t.Fatalf("a password and no group: %d", code)
	}
	if code := c.login("pat", "wrong"); code != 401 {
		t.Fatalf("wrong password: %d", code)
	}
	if code := c.login("alice", "pw"); code != 200 { // a member of the full-access groups may use the files site as well
		t.Fatalf("an administrator could not sign in to the files site: %d", code)
	}
	if code := c.login("pat", "pw"); code != 200 {
		t.Fatalf("a member of the files group: %d", code)
	}
	// browsing
	code, b, _ = c.req(e.site.URL, "GET", "/api/files/list?folder=docs", nil)
	var l struct {
		Data struct{ Entries []FileView }
	}
	json.Unmarshal(b, &l)
	if code != 200 || len(l.Data.Entries) != 2 || l.Data.Entries[0].Name != "sub" || l.Data.Entries[0].By != "" {
		t.Fatalf("list: %d %s", code, b)
	}
	// the filter looks through every folder and subfolder
	code, b, _ = c.req(e.site.URL, "GET", "/api/files/search?q=SUB%20B.TXT", nil)
	var sr struct {
		Data struct {
			Entries   []FileHit
			Total     int
			Truncated bool
		}
	}
	json.Unmarshal(b, &sr)
	if code != 200 || sr.Data.Total != 1 || len(sr.Data.Entries) != 1 || sr.Data.Truncated || sr.Data.Entries[0].Path != "sub/b.txt" {
		t.Fatalf("search: %d %s", code, b)
	}
	for _, h := range sr.Data.Entries {
		if h.Folder != "docs" || h.By != "" || h.Name != "b.txt" {
			t.Fatalf("search hit: %+v", h)
		}
	}
	if code, b, _ = c.req(e.site.URL, "GET", "/api/files/search?q=b.txt%20zzz-nothing", nil); code != 200 || strings.Contains(string(b), `"path"`) {
		t.Fatalf("every word must match: %d %s", code, b)
	}
	// the folder status does not tell where the folder is, or about nodes
	code, b, _ = c.req(e.site.URL, "GET", "/api/files/status?local=1", nil)
	if code != 200 || strings.Contains(string(b), e.root) || strings.Contains(string(b), `"path"`) || strings.Contains(string(b), `"peers"`) || strings.Contains(string(b), `"addr"`) {
		t.Fatalf("status shows too much: %s", b)
	}
	// writing, with the CSRF token
	if code, _, _ = c.req(e.site.URL, "POST", "/api/files/mkdir", map[string]string{"folder": "docs", "path": "newdir"}); code != 200 {
		t.Fatalf("mkdir: %d", code)
	}
	if code, _, _ = c.req(e.site.URL, "POST", "/api/files/mkdir", map[string]string{"folder": "docs", "path": "x"}, func(r *http.Request) { r.Header.Del("X-CSRF-Token") }); code != 403 {
		t.Fatalf("a write without the CSRF token: %d", code)
	}
	if code, _, _ = c.req(e.site.URL, "POST", "/api/files/mkdir", map[string]string{"folder": "docs", "path": "x"}, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }); code != 403 {
		t.Fatalf("a cross-origin write: %d", code)
	}
	if code, _, _ = c.req(e.site.URL, "POST", "/api/files/upload?folder=docs&path=up/new.txt", []byte("hello")); code != 200 {
		t.Fatalf("upload: %d", code)
	}
	if got, _ := os.ReadFile(filepath.Join(e.root, "up", "new.txt")); string(got) != "hello" {
		t.Fatalf("uploaded file: %q", got)
	}
	// a file the ignore list covers is skipped, not an error: a dropped folder goes on with the rest
	for _, p := range []string{"up/x.tmp", "up/.git/objects/pack/p1"} {
		code, b, _ = c.req(e.site.URL, "POST", "/api/files/upload?folder=docs&path="+p, []byte("zz"))
		if code != 200 || !strings.Contains(string(b), `"skipped":true`) {
			t.Fatalf("an ignored upload %s: %d %s", p, code, b)
		}
		if _, err := os.Stat(filepath.Join(e.root, filepath.FromSlash(p))); err == nil {
			t.Fatalf("%s was written", p)
		}
	}
	if code, b, _ = c.req(e.site.URL, "POST", "/api/files/mkdir", map[string]string{"folder": "docs", "path": "up/.git"}); code != 200 || !strings.Contains(string(b), `"skipped":true`) {
		t.Fatalf("an ignored folder: %d %s", code, b)
	}
	// a zip of a selection
	code, b, h := c.req(e.site.URL, "GET", "/api/files/zip?folder=docs&path=sub&path=a.txt", nil)
	if code != 200 || h.Get("Content-Type") != "application/zip" || !bytes.HasPrefix(b, []byte("PK")) || !strings.Contains(h.Get("Content-Disposition"), "attachment") {
		t.Fatalf("zip: %d %v", code, h)
	}
	if code, _, _ = c.req(e.site.URL, "GET", "/api/files/zip?folder=docs&path=../etc", nil); code != 400 {
		t.Fatalf("zip of a bad name: %d", code)
	}
	// signing out ends it
	if code, _, _ = c.req(e.site.URL, "POST", "/api/logout", map[string]string{}); code != 200 {
		t.Fatalf("logout: %d", code)
	}
	if code, _, _ = c.req(e.site.URL, "GET", "/api/session", nil); code != 401 {
		t.Fatalf("after logout: %d", code)
	}
}

func TestFilesSiteReadOnlyGroup(t *testing.T) {
	e := newSiteEnv(t, true)
	c := &siteClient{e: e}
	if code := c.login("rita", "pw"); code != 200 {
		t.Fatalf("read-only member: %d", code)
	}
	if code, _, _ := c.req(e.site.URL, "GET", "/api/files/list?folder=docs", nil); code != 200 {
		t.Fatalf("a read-only user cannot browse: %d", code)
	}
	if code, _, _ := c.req(e.site.URL, "GET", "/api/files/zip?folder=docs", nil); code != 200 {
		t.Fatalf("a read-only user cannot download: %d", code)
	}
	for _, p := range []string{"/api/files/mkdir", "/api/files/delete", "/api/files/rename", "/api/files/upload?folder=docs&path=z"} {
		if code, _, _ := c.req(e.site.URL, "POST", p, map[string]string{"folder": "docs", "path": "z"}); code != 403 {
			t.Errorf("POST %s as read-only: %d", p, code)
		}
	}
	if _, err := os.Stat(filepath.Join(e.root, "z")); err == nil {
		t.Fatal("a read-only user wrote a file")
	}
}

// What the files site does not serve, however signed in, and the two kinds of session apart.
func TestFilesSiteServesNothingOfTheCluster(t *testing.T) {
	e := newSiteEnv(t, true)
	c := &siteClient{e: e}
	c.login("pat", "pw")
	for _, p := range []string{"/api/config", "/api/gateways", "/api/neighbors", "/api/canvas", "/api/users", "/api/cluster", "/api/proxy?node=x&path=/api/config", "/api/update",
		"/api/files/status", "/metrics", "/api/metrics/dashboard.json", "/help.js", "/api/sso/start", "/api/versions", "/api/power", "/api/log", "/api/mcpusage", "/api/backup/files"} {
		code, _, _ := c.req(e.site.URL, "GET", p, nil)
		if p == "/api/files/status" { // the folder list is there, without the cluster
			continue
		}
		if code != 404 {
			t.Errorf("GET %s on the files site: %d", p, code)
		}
	}
	for _, p := range []string{"/api/config", "/api/users", "/api/power", "/api/cluster/join", "/api/assert-agc", "/api/login/ldap", "/api/ldap/test"} {
		if code, _, _ := c.req(e.site.URL, "POST", p, map[string]string{}); code != 404 && code != 405 {
			t.Errorf("POST %s on the files site: %d", p, code)
		}
		if code, _, _ := c.req(e.site.URL, "PUT", p, map[string]string{}); code != 404 && code != 405 {
			t.Errorf("PUT %s on the files site: %d", p, code)
		}
	}
	// a files session is nothing on the management GUI, even under the GUI's own cookie name
	tok := strings.TrimPrefix(c.cookie, filesCookie+"=")
	for _, ck := range []string{c.cookie, sessionCookie + "=" + tok} {
		cc := &siteClient{e: e, cookie: ck}
		if code, _, _ := cc.req(e.adm.URL, "GET", "/api/session", nil); code != 401 {
			t.Errorf("a files session on the management GUI (%s...): %d", ck[:12], code)
		}
		if code, _, _ := cc.req(e.adm.URL, "GET", "/api/config", nil); code != 401 {
			t.Errorf("a files session read the configuration: %d", code)
		}
	}
	// and the other way: an administrator's session is nothing on the files site, under either cookie name
	admin := &siteClient{e: e}
	code, _, h := admin.req(e.adm.URL, "POST", "/api/login", map[string]string{"username": "alice", "password": "pw"})
	if code != 200 {
		t.Fatalf("admin login: %d", code)
	}
	var atok string
	for _, ck := range (&http.Response{Header: h}).Cookies() {
		if ck.Name == sessionCookie {
			atok = ck.Value
		}
	}
	for _, name := range []string{sessionCookie, filesCookie} {
		cc := &siteClient{e: e, cookie: name + "=" + atok}
		if code, _, _ := cc.req(e.site.URL, "GET", "/api/session", nil); code != 401 {
			t.Errorf("an administrator's session on the files site under %s: %d", name, code)
		}
		if code, _, _ := cc.req(e.site.URL, "GET", "/api/files/list?folder=docs", nil); code != 401 {
			t.Errorf("an administrator's session listed files on the site under %s: %d", name, code)
		}
	}
	// the management GUI does not take the files site's login
	if code, _, _ := (&siteClient{e: e}).req(e.adm.URL, "POST", "/api/login", map[string]string{"username": "pat", "password": "pw"}); code != 401 {
		t.Errorf("a files user signed in to the management GUI: %d", code)
	}
}

func TestFilesSiteGroupLeftEndsTheSession(t *testing.T) {
	e := newSiteEnv(t, true)
	c := &siteClient{e: e}
	c.login("pat", "pw")
	e.ws.mu.Lock()
	for _, s := range e.ws.sessions {
		s.checked = time.Now().Add(-time.Hour) // due for the group check
	}
	e.ws.mu.Unlock()
	pol := defaultWeb()
	pol.FilesGroup = "someone-else" // pat is no longer in a files group
	pol.FilesReadGroup = "viewers"
	e.ws.policy.Store(&pol)
	// InGroup answers an error for an unknown group: the session must end, not stay
	if code, _, _ := c.req(e.site.URL, "GET", "/api/session", nil); code != 401 {
		t.Fatalf("a user who left the group kept the session: %d", code)
	}
}

// ── sessions among the nodes ──

// memShare joins WebServers in one process the way the cluster joins nodes.
type memShare struct {
	mu    sync.Mutex
	self  *WebServer
	peers []*WebServer
	down  bool
	calls map[string]int
}

func (m *memShare) others() []*WebServer {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return nil
	}
	return m.peers
}
func (m *memShare) count(op string) {
	m.mu.Lock()
	if m.calls == nil {
		m.calls = map[string]int{}
	}
	m.calls[op]++
	m.mu.Unlock()
}
func (m *memShare) put(r sessionRec) {
	m.count("put")
	for _, p := range m.others() {
		p.sessionFromPeer(sessMsg{Op: "put", Rec: &r})
	}
}
func (m *memShare) del(k string) {
	m.count("del")
	for _, p := range m.others() {
		p.sessionFromPeer(sessMsg{Op: "del", Key: k})
	}
}
func (m *memShare) get(k string) *sessionRec {
	m.count("get")
	for _, p := range m.others() {
		if r := p.sessionFromPeer(sessMsg{Op: "get", Key: k}).Rec; r != nil {
			return r
		}
	}
	return nil
}

func twoSites(t *testing.T) (a, b *siteEnv, sa, sb *memShare) {
	a, b = newSiteEnv(t, true), newSiteEnv(t, true)
	sa, sb = &memShare{self: a.ws, peers: []*WebServer{b.ws}}, &memShare{self: b.ws, peers: []*WebServer{a.ws}}
	a.ws.share, b.ws.share = sa, sb
	return
}

func (e *siteEnv) has(key string) bool {
	e.ws.mu.Lock()
	defer e.ws.mu.Unlock()
	return e.ws.sessions[key] != nil
}

func TestFilesSessionFollowsThePersonToAnotherNode(t *testing.T) {
	a, b, _, sb := twoSites(t)
	ca := &siteClient{e: a}
	if ca.login("pat", "pw") != 200 {
		t.Fatal("login on A")
	}
	key := sessKey(strings.TrimPrefix(ca.cookie, filesCookie+"="))
	if !b.has(key) {
		t.Fatal("B was not told of the session when it was made")
	}
	// the same cookie and CSRF token work on B
	cb := &siteClient{e: b, cookie: ca.cookie, csrf: ca.csrf}
	if code, _, _ := cb.req(b.site.URL, "GET", "/api/files/list?folder=docs", nil); code != 200 {
		t.Fatalf("the session on B: %d", code)
	}
	if code, _, _ := cb.req(b.site.URL, "POST", "/api/files/mkdir", map[string]string{"folder": "docs", "path": "fromB"}); code != 200 {
		t.Fatalf("a write on B with A's CSRF token: %d", code)
	}
	// B missed the message (it was down): it asks the others when it meets the cookie
	b.ws.mu.Lock()
	delete(b.ws.sessions, key)
	b.ws.mu.Unlock()
	if code, _, _ := cb.req(b.site.URL, "GET", "/api/session", nil); code != 200 {
		t.Fatalf("B did not take the session over from A: %d", code)
	}
	if sb.calls["get"] != 1 {
		t.Fatalf("B asked %d times", sb.calls["get"])
	}
	// signing out on B ends it on A, and a late copy does not bring it back
	if code, _, _ := cb.req(b.site.URL, "POST", "/api/logout", map[string]string{}); code != 200 {
		t.Fatal("logout on B")
	}
	waitUntil(t, func() bool { return !a.has(key) }, "the logout reached A")
	ca2 := &siteClient{e: a, cookie: ca.cookie}
	if code, _, _ := ca2.req(a.site.URL, "GET", "/api/session", nil); code != 401 {
		t.Fatalf("the session still works on A after sign-out: %d", code)
	}
	rec := sessionRec{Key: key, User: "pat", CSRF: ca.csrf, Created: time.Now().Unix(), Last: time.Now().Unix(), Site: siteFiles}
	if a.ws.sessionFromPeer(sessMsg{Op: "put", Rec: &rec}); a.has(key) {
		t.Fatal("a late copy brought a signed-out session back")
	}
}

func waitUntil(t *testing.T, cond func() bool, what string) {
	t.Helper()
	if !eventually(3*time.Second, cond) {
		t.Fatal("timed out: " + what)
	}
}

func TestFilesSessionActivityHoldsTheIdleTimeEverywhere(t *testing.T) {
	a, b, _, _ := twoSites(t)
	ca := &siteClient{e: a}
	ca.login("pat", "pw")
	key := sessKey(strings.TrimPrefix(ca.cookie, filesCookie+"="))
	idle := a.ws.policy.Load().sessionIdle()
	// on B the session was last used nearly an idle time ago; the person has been busy on A
	b.ws.mu.Lock()
	b.ws.sessions[key].last = time.Now().Add(-idle + time.Minute)
	b.ws.mu.Unlock()
	a.ws.mu.Lock()
	a.ws.sessions[key].pushed = time.Now().Add(-time.Minute)
	a.ws.mu.Unlock()
	ca.req(a.site.URL, "GET", "/api/session", nil) // activity on A, which tells B
	waitUntil(t, func() bool {
		b.ws.mu.Lock()
		defer b.ws.mu.Unlock()
		return time.Since(b.ws.sessions[key].last) < 30*time.Second
	}, "B learned of the activity")
}

func TestFilesSessionRefusesWhatAStrangerWouldSend(t *testing.T) {
	a := newSiteEnv(t, true)
	now := time.Now().Unix()
	good := sessionRec{Key: strings.Repeat("a", 64), User: "pat", CSRF: "c", Created: now, Last: now, Site: siteFiles}
	bad := []sessionRec{}
	for _, f := range []func(r *sessionRec){
		func(r *sessionRec) { r.Site = "" },                     // not a files session: never taken
		func(r *sessionRec) { r.Site = "admin" },                //
		func(r *sessionRec) { r.Key = "short" },                 // not a key
		func(r *sessionRec) { r.Key = strings.Repeat("Z", 64) }, //
		func(r *sessionRec) { r.User = "" },
		func(r *sessionRec) { r.User = "a/b" },
		func(r *sessionRec) { r.CSRF = "" },
		func(r *sessionRec) { r.Created = now - int64(sessionMaxAge.Seconds()) - 10 },
		func(r *sessionRec) { r.Last = now - int64(24*time.Hour.Seconds()) },
		func(r *sessionRec) { r.Created = now + 3600 },
	} {
		r := good
		f(&r)
		bad = append(bad, r)
	}
	for i, r := range bad {
		a.ws.sessionFromPeer(sessMsg{Op: "put", Rec: &r})
		if len(a.ws.sessions) != 0 {
			t.Fatalf("bad record %d was taken: %+v", i, r)
		}
	}
	a.ws.sessionFromPeer(sessMsg{Op: "put", Rec: &good})
	if !a.has(good.Key) {
		t.Fatal("a good record was refused")
	}
	// a management GUI session cannot be overwritten or removed through the files messages
	adminKey := strings.Repeat("b", 64)
	a.ws.sessions[adminKey] = &session{user: "alice", csrf: "x", created: time.Now(), last: time.Now()}
	a.ws.sessionFromPeer(sessMsg{Op: "del", Key: adminKey})
	if !a.has(adminKey) {
		t.Fatal("a peer ended a management session")
	}
	if r := a.ws.sessionFromPeer(sessMsg{Op: "get", Key: adminKey}).Rec; r != nil {
		t.Fatal("a peer was given a management session")
	}
	over := sessionRec{Key: adminKey, User: "mallory", CSRF: "m", Created: now, Last: now, Site: siteFiles}
	a.ws.sessionFromPeer(sessMsg{Op: "put", Rec: &over})
	if a.ws.sessions[adminKey].user != "alice" {
		t.Fatal("a peer replaced a management session")
	}
}

// Made-up cookies must not make the cluster busy.
func TestFilesSessionUnknownCookiesAreNotPassedAround(t *testing.T) {
	a, _, sa, _ := twoSites(t)
	c := &siteClient{e: a}
	// not the shape of a token: nobody is asked
	c.cookie = filesCookie + "=nonsense"
	c.req(a.site.URL, "GET", "/api/session", nil)
	if sa.calls["get"] != 0 {
		t.Fatalf("a cookie that is not a token was asked about: %d", sa.calls["get"])
	}
	// the same unknown token twice: asked once
	c.cookie = filesCookie + "=" + randToken()
	c.req(a.site.URL, "GET", "/api/session", nil)
	c.req(a.site.URL, "GET", "/api/session", nil)
	if sa.calls["get"] != 1 {
		t.Fatalf("one unknown token was asked about %d times", sa.calls["get"])
	}
	// many different ones: the number of questions is bounded
	for i := 0; i < 500; i++ {
		c.cookie = filesCookie + "=" + randToken()
		c.req(a.site.URL, "GET", "/api/session", nil)
	}
	if sa.calls["get"] > int(fetchBurst)+20 {
		t.Fatalf("500 made-up cookies caused %d questions to the other nodes", sa.calls["get"])
	}
}

func TestFilesSessionsSurviveARestartAndStaySeparate(t *testing.T) {
	e := newSiteEnv(t, true)
	c := &siteClient{e: e}
	c.login("pat", "pw")
	e.ws.saveSessions()
	ws2 := NewWebServer(e.ws.mg, nil, &filesAuth{})
	pol := defaultWeb()
	pol.FilesGroup = "users"
	ws2.policy.Store(&pol)
	site2 := httptest.NewServer(ws2.filesSiteHandler())
	defer site2.Close()
	adm2 := httptest.NewServer(ws2.Handler())
	defer adm2.Close()
	c2 := &siteClient{e: e, cookie: c.cookie}
	if code, _, _ := c2.req(site2.URL, "GET", "/api/session", nil); code != 200 {
		t.Fatalf("a files session after a restart: %d", code)
	}
	if code, _, _ := c2.req(adm2.URL, "GET", "/api/session", nil); code != 401 {
		t.Fatalf("a restored files session on the management GUI: %d", code)
	}
}

// ── through two real cluster nodes ──

func TestFilesSessionsOverTheClusterChannel(t *testing.T) {
	a, b := twoNodeCluster(t)
	mk := func(n *tnode) *siteEnv {
		sup := NewSupervisor(context.Background(), newDaemonConfig())
		st := NewStatusServer("", sup)
		st.mg = n.mg
		ws := NewWebServer(n.mg, st, &filesAuth{})
		pol := defaultWeb()
		pol.FilesGroup = "users"
		ws.policy.Store(&pol)
		e := &siteEnv{t: t, ws: ws}
		e.site = httptest.NewServer(ws.filesSiteHandler())
		t.Cleanup(e.site.Close)
		return e
	}
	ea, eb := mk(a), mk(b)
	ca := &siteClient{e: ea}
	if ca.login("pat", "pw") != 200 {
		t.Fatal("login on A")
	}
	key := sessKey(strings.TrimPrefix(ca.cookie, filesCookie+"="))
	waitUntil(t, func() bool { return eb.has(key) }, "the session reached B over the cluster channel")
	cb := &siteClient{e: eb, cookie: ca.cookie, csrf: ca.csrf}
	if code, _, _ := cb.req(eb.site.URL, "GET", "/api/session", nil); code != 200 {
		t.Fatalf("the session on B: %d", code)
	}
	// B forgets it; it fetches it from A over the channel
	eb.ws.mu.Lock()
	delete(eb.ws.sessions, key)
	eb.ws.mu.Unlock()
	if code, _, _ := cb.req(eb.site.URL, "GET", "/api/session", nil); code != 200 {
		t.Fatalf("B did not fetch the session from A: %d", code)
	}
	// sign out on B: A drops it
	if code, _, _ := cb.req(eb.site.URL, "POST", "/api/logout", map[string]string{}); code != 200 {
		t.Fatal("logout on B")
	}
	waitUntil(t, func() bool { return !ea.has(key) }, "the logout reached A over the cluster channel")
	if r := ea.ws.sessionFromPeer(sessMsg{Op: "get", Key: key}).Rec; r != nil {
		t.Fatal("A still hands out the session")
	}
}

// A download link pasted into a browser that is not signed in goes to the sign-in page and back; a script still gets the 401; while the
// site is off it says so.
func TestFilesSiteLinkSignInAndOffMessage(t *testing.T) {
	e := newSiteEnv(t, true)
	c := &siteClient{e: e}
	nav := func(r *http.Request) { r.Header.Set("Sec-Fetch-Mode", "navigate"); r.Header.Set("Accept", "text/html") }
	path := "/api/files/download?folder=docs&path=sub%2Fb.txt"
	code, _, h := c.req(e.site.URL, "GET", path, nil, nav)
	if code != http.StatusSeeOther || h.Get("Location") != "/?next="+url.QueryEscape(path) {
		t.Fatalf("a browser without a session: %d %q", code, h.Get("Location"))
	}
	if code, _, _ := c.req(e.site.URL, "GET", path, nil); code != http.StatusUnauthorized {
		t.Fatalf("a script without a session: %d", code)
	}
	if c.login("pat", "pw") != 200 {
		t.Fatal("login")
	}
	if code, b, _ := c.req(e.site.URL, "GET", path, nil, nav); code != 200 || string(b) != "beta" {
		t.Fatalf("signed in: %d %q", code, b)
	}
}

// With no group set for the files site, the people who may sign in to the management GUI may sign in to it: the site is always on.
func TestFilesSiteFallsBackToTheLoginGroups(t *testing.T) {
	e := newSiteEnv(t, true)
	pol := *e.ws.policy.Load()
	pol.FilesGroup, pol.FilesReadGroup = "", ""
	pol.Group, pol.ReadGroup = "filedir,filedir-users", ""
	e.ws.policy.Store(&pol)
	c := &siteClient{e: e}
	if code := c.login("alice", "pw"); code != 200 {
		t.Fatalf("a login-group member on the files site: %d", code)
	}
	if code, _, _ := c.req(e.site.URL, "GET", "/api/files/list?folder=docs", nil); code != 200 {
		t.Fatalf("list: %d", code)
	}
	if code := (&siteClient{e: e}).login("bob", "pw"); code == 200 {
		t.Fatal("someone in no group signed in")
	}
}

// A public link downloads one file with no sign-in, until it expires, and nothing else.
func TestFilesPublicLink(t *testing.T) {
	e := newSiteEnv(t, true)
	pat := &siteClient{e: e}
	if pat.login("pat", "pw") != 200 {
		t.Fatal("login")
	}
	code, b, _ := pat.req(e.site.URL, "POST", "/api/files/share", map[string]any{"folder": "docs", "path": "sub/b.txt", "days": 7})
	var out struct {
		URL     string `json:"url"`
		Expires int64  `json:"expires"`
	}
	json.Unmarshal(b, &out)
	if code != 200 || !strings.HasPrefix(out.URL, "/dl/") || !strings.HasSuffix(out.URL, "/b.txt") {
		t.Fatalf("share: %d %s", code, b)
	}
	anon := &siteClient{e: e}
	if code, b, h := anon.req(e.site.URL, "GET", out.URL, nil); code != 200 || string(b) != "beta" || !strings.Contains(h.Get("Content-Disposition"), "b.txt") {
		t.Fatalf("anonymous download: %d %q %v", code, b, h)
	}
	if code, _, _ := anon.req(e.site.URL, "GET", strings.Replace(out.URL, "/dl/", "/dl/x", 1), nil); code != 404 {
		t.Fatalf("a changed token was accepted: %d", code)
	}
	if code, _, _ := anon.req(e.site.URL, "GET", "/api/files/download?folder=docs&path=a.txt", nil); code != http.StatusUnauthorized {
		t.Fatalf("a link opened the rest: %d", code)
	}
	key := e.ws.pubLinkKey()
	old := "/dl/" + pubLinkSign(key, pubLink{Folder: "docs", Path: "a.txt", Exp: time.Now().Add(-time.Minute).Unix()})
	if code, _, _ := anon.req(e.site.URL, "GET", old, nil); code != 404 {
		t.Fatalf("an expired link worked: %d", code)
	}
	for _, in := range []map[string]any{{"folder": "docs", "path": "sub/b.txt", "days": 0}, {"folder": "docs", "path": "sub/b.txt", "days": 31}, {"folder": "docs", "path": "../x", "days": 1}, {"folder": "docs", "path": "nope.txt", "days": 1}} {
		if code, _, _ := pat.req(e.site.URL, "POST", "/api/files/share", in); code < 400 {
			t.Fatalf("%v was accepted: %d", in, code)
		}
	}
	rita := &siteClient{e: e}
	if rita.login("rita", "pw") != 200 {
		t.Fatal("login rita")
	}
	if code, _, _ := rita.req(e.site.URL, "POST", "/api/files/share", map[string]any{"folder": "docs", "path": "a.txt", "days": 1}); code != http.StatusForbidden {
		t.Fatalf("a read-only account made a public link: %d", code)
	}
}

// Upload from URL: the node fetches a web address into the folder; it will not go to itself or a link-local address.
func TestFilesFetchFromURL(t *testing.T) {
	e := newSiteEnv(t, true)
	up := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pub/report.txt":
			rw.Write([]byte("gamma"))
		case "/named":
			rw.Header().Set("Content-Disposition", `attachment; filename="given.bin"`)
			rw.Write([]byte("delta"))
		case "/jump":
			http.Redirect(rw, r, "/pub/report.txt", http.StatusFound)
		default:
			http.NotFound(rw, r)
		}
	}))
	defer up.Close()
	pat := &siteClient{e: e}
	if pat.login("pat", "pw") != 200 {
		t.Fatal("login")
	}
	fetch := func(in map[string]any) (int, string) {
		code, b, _ := pat.req(e.site.URL, "POST", "/api/files/fetch", in)
		if code == 200 { // the file comes in the background: wait for it
			var st struct{ Data struct{ ID string } }
			json.Unmarshal(b, &st)
			for i := 0; i < 200; i++ {
				_, sb, _ := pat.req(e.site.URL, "GET", "/api/files/fetch?id="+st.Data.ID, nil)
				if strings.Contains(string(sb), `"state":"done"`) {
					break
				}
				if strings.Contains(string(sb), `"state":"failed"`) {
					return 500, string(sb)
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		return code, string(b)
	}
	in := func(u string) map[string]any { return map[string]any{"folder": "docs", "dir": "sub", "url": u} }
	// the node refuses its own address by default
	if code, body := fetch(in(up.URL + "/pub/report.txt")); code < 400 || !strings.Contains(body, "may fetch") {
		t.Fatalf("loopback was fetched: %d %s", code, body)
	}
	old := fdFetchBlocked
	fdFetchBlocked = func(netip.Addr) bool { return false }
	defer func() { fdFetchBlocked = old }()
	for u, want := range map[string]string{"/pub/report.txt": "sub/report.txt", "/named": "sub/given.bin", "/jump": "sub/report.txt"} {
		if code, body := fetch(in(up.URL + u)); code != 200 || !strings.Contains(body, want) {
			t.Fatalf("fetch %s: %d %s", u, code, body)
		}
	}
	if b, err := os.ReadFile(filepath.Join(e.root, "sub", "given.bin")); err != nil || string(b) != "delta" {
		t.Fatalf("file: %q %v", b, err)
	}
	if code, _ := fetch(in(up.URL + "/missing")); code != http.StatusBadGateway {
		t.Fatalf("a 404 upstream: %d", code)
	}
	for _, bad := range []string{"ftp://example.test/x", "file:///etc/passwd", "not a url", ""} {
		if code, _ := fetch(in(bad)); code != http.StatusBadRequest {
			t.Fatalf("%q: %d", bad, code)
		}
	}
	if code, _ := fetch(map[string]any{"folder": "docs", "dir": "sub", "url": up.URL + "/pub/report.txt", "name": "../x"}); code != http.StatusBadRequest {
		t.Fatalf("a name with a slash: %d", code)
	}
	rita := &siteClient{e: e}
	rita.login("rita", "pw")
	if code, _, _ := rita.req(e.site.URL, "POST", "/api/files/fetch", in(up.URL+"/pub/report.txt")); code != http.StatusForbidden {
		t.Fatalf("a read-only account fetched: %d", code)
	}
}

// Directory (LDAP) users the management GUI lets in may sign in to the files site, at the same level.
func TestFilesSiteLDAPSignIn(t *testing.T) {
	e := newSiteEnv(t, true)
	f := newFakeLDAP(t, false)
	defer f.stop()
	pol := *e.ws.policy.Load()
	pol.LDAPConfig = f.config()
	e.ws.policy.Store(&pol)
	c := &siteClient{e: e}
	if code := c.login("alice", "alicepw"); code != 200 {
		t.Fatalf("a directory user on the files site: %d", code)
	}
	code, b, _ := c.req(e.site.URL, "GET", "/api/session", nil)
	var m map[string]any
	json.Unmarshal(b, &m)
	if code != 200 || m["read_only"] != false || m["site"] != "files" {
		t.Fatalf("session: %d %s", code, b)
	}
	if code, _, _ := c.req(e.site.URL, "GET", "/api/files/list?folder=docs", nil); code != 200 {
		t.Fatalf("list: %d", code)
	}
	if code := (&siteClient{e: e}).login("alice", "wrong"); code == 200 {
		t.Fatal("a wrong directory password signed in")
	}
	// the session is not one of the management GUI's
	if code, _, _ := c.req(e.adm.URL, "GET", "/api/session", nil); code != http.StatusUnauthorized {
		t.Fatalf("a files session on the management GUI: %d", code)
	}
}

// Single sign-on users on the GUI's list may sign in to the files site; the redirect address is the files site's own host.
func TestFilesSiteSSOSignIn(t *testing.T) {
	e := newSiteEnv(t, true)
	idp := newFakeIdP(t)
	ssoInsecure, ssoGoogleIssuer = true, idp.srv.URL
	defer func() { ssoInsecure, ssoGoogleIssuer = false, "https://accounts.google.com" }()
	pol := *e.ws.policy.Load()
	pol.GoogleClientID, pol.GoogleClientSecret = "gid", idp.secret
	pol.Users = []string{"alice@example.com rw", "bob@example.com"}
	if err := pol.Validate(); err != nil {
		t.Fatal(err)
	}
	e.ws.policy.Store(&pol)
	c := &siteClient{e: e}
	code, b, _ := c.req(e.site.URL, "GET", "/api/login/state", nil)
	if code != 200 || !strings.Contains(string(b), `"google"`) {
		t.Fatalf("login state: %d %s", code, b)
	}
	site := &webEnv{t: t, ts: e.site, ws: e.ws, client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	back, ck := idp.signIn(site, "google", nil)
	if back.StatusCode != 302 || ck == "" || !strings.HasPrefix(ck, filesCookie+"=") {
		t.Fatalf("sign-in: %d %q", back.StatusCode, ck)
	}
	if !strings.HasPrefix(idp.redirect, "https://"+strings.TrimPrefix(e.site.URL, "http://")+"/") {
		t.Fatalf("redirect %q is not the files site's own address", idp.redirect)
	}
	cc := &siteClient{e: e, cookie: ck}
	if code, b, _ := cc.req(e.site.URL, "GET", "/api/session", nil); code != 200 || !strings.Contains(string(b), `"alice@example.com"`) {
		t.Fatalf("session: %d %s", code, b)
	}
	if code, _, _ := cc.req(e.site.URL, "GET", "/api/files/list?folder=docs", nil); code != 200 {
		t.Fatalf("list: %d", code)
	}
	_, ck = idp.signIn(site, "google", func(c, h map[string]any) { c["email"] = "mallory@example.com" })
	if ck != "" {
		t.Fatal("someone not on the list signed in to the files site")
	}
}

// One list of groups: the directory looks for the same names as the host, a name it does not have is skipped, and the files-only groups
// open the files site and not the management GUI.
func TestDirectoryUsesTheCommonGroups(t *testing.T) {
	e := newSiteEnv(t, true)
	f := newFakeLDAP(t, false)
	defer f.stop()
	pol := *e.ws.policy.Load()
	pol.LDAPConfig = f.config()
	pol.LDAPRWGroup, pol.LDAPROGroup = "", "" // no groups of the directory's own
	pol.Group, pol.ReadGroup = "filedir,nosuchgroup,dns-admins", "dns-readers"
	pol.FilesGroup, pol.FilesReadGroup = "users", "viewers"
	e.ws.policy.Store(&pol)
	if !pol.ldapFor(siteAdmin).enabled() {
		t.Fatal("the directory is off though the groups are set")
	}
	site := &siteClient{e: e}
	if code := site.login("alice", "alicepw"); code != 200 {
		t.Fatalf("a member of a full-access group on the files site: %d", code)
	}
	if code := (&siteClient{e: e}).login("carol", "carolpw"); code == 200 {
		t.Fatal("someone in no group signed in")
	}
	// the files-only groups open the files site and not the management GUI
	pat := &siteClient{e: e}
	if pat.login("pat", "pw") != 200 {
		t.Fatal("a files-only user could not sign in to the files site")
	}
	if code, _, _ := pat.req(e.adm.URL, "POST", "/api/login", map[string]string{"username": "pat", "password": "pw"}); code != http.StatusUnauthorized {
		t.Fatalf("a files-only user on the management GUI: %d", code)
	}
}

// A fetch says how far it is, and can be stopped; what was fetched until then is not kept.
func TestFilesFetchProgressAndCancel(t *testing.T) {
	e := newSiteEnv(t, true)
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Length", "10")
		rw.Write([]byte("01234"))
		rw.(http.Flusher).Flush()
		select {
		case <-release:
			rw.Write([]byte("56789"))
		case <-r.Context().Done():
		}
	}))
	defer up.Close()
	old := fdFetchBlocked
	fdFetchBlocked = func(netip.Addr) bool { return false }
	defer func() { fdFetchBlocked = old }()
	pat := &siteClient{e: e}
	if pat.login("pat", "pw") != 200 {
		t.Fatal("login")
	}
	start := func(name string) string {
		code, b, _ := pat.req(e.site.URL, "POST", "/api/files/fetch", map[string]any{"folder": "docs", "dir": "sub", "url": up.URL + "/x", "name": name})
		var m struct{ Data struct{ ID string } }
		json.Unmarshal(b, &m)
		if code != 200 || m.Data.ID == "" {
			t.Fatalf("start: %d %s", code, b)
		}
		return m.Data.ID
	}
	status := func(id string) (state string, received, total float64) {
		_, b, _ := pat.req(e.site.URL, "GET", "/api/files/fetch?id="+id, nil)
		var m struct {
			Data struct {
				State           string
				Received, Total float64
			}
		}
		json.Unmarshal(b, &m)
		return m.Data.State, m.Data.Received, m.Data.Total
	}
	wait := func(id string, want func(string, float64) bool) (string, float64) {
		for i := 0; i < 300; i++ {
			st, rc, _ := status(id)
			if want(st, rc) {
				return st, rc
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("timed out waiting")
		return "", 0
	}
	id := start("whole.bin")
	if _, rc := wait(id, func(s string, rc float64) bool { return rc == 5 }); rc != 5 {
		t.Fatalf("received %v", rc)
	}
	if st, rc, total := status(id); st != "running" || rc != 5 || total != 10 {
		t.Fatalf("midway: %s %v of %v", st, rc, total)
	}
	close(release)
	wait(id, func(s string, _ float64) bool { return s == "done" })
	if b, err := os.ReadFile(filepath.Join(e.root, "sub", "whole.bin")); err != nil || string(b) != "0123456789" {
		t.Fatalf("file: %q %v", b, err)
	}
	// cancel: nothing is kept
	release = make(chan struct{})
	id = start("cut.bin")
	wait(id, func(s string, rc float64) bool { return rc == 5 })
	if code, _, _ := pat.req(e.site.URL, "POST", "/api/files/fetch/cancel", map[string]string{"id": id}); code != 200 {
		t.Fatalf("cancel: %d", code)
	}
	if st, _ := wait(id, func(s string, _ float64) bool { return s != "running" }); st != "cancelled" {
		t.Fatalf("state after cancel: %s", st)
	}
	if _, err := os.Stat(filepath.Join(e.root, "sub", "cut.bin")); err == nil {
		t.Fatal("a cancelled fetch left a file")
	}
	// somebody else's fetch is not theirs to see
	rita := &siteClient{e: e}
	rita.login("rita", "pw")
	if code, _, _ := rita.req(e.site.URL, "GET", "/api/files/fetch?id="+id, nil); code != http.StatusNotFound {
		t.Fatalf("another user's fetch: %d", code)
	}
}

func TestFilesCopyMoveDuplicate(t *testing.T) {
	e := newSiteEnv(t, true)
	pat := &siteClient{e: e}
	if pat.login("pat", "pw") != 200 {
		t.Fatal("login")
	}
	cp := func(in map[string]any) (int, string) {
		code, b, _ := pat.req(e.site.URL, "POST", "/api/files/copy", in)
		return code, string(b)
	}
	read := func(p string) string { b, _ := os.ReadFile(filepath.Join(e.root, p)); return string(b) }
	// duplicate: a free name beside it, again and again
	if code, b := cp(map[string]any{"folder": "docs", "path": "a.txt", "tofolder": "docs", "to": "a.txt", "unique": true}); code != 200 || !strings.Contains(b, "a copy.txt") || read("a copy.txt") != "alpha" {
		t.Fatalf("duplicate: %d %s", code, b)
	}
	if code, b := cp(map[string]any{"folder": "docs", "path": "a.txt", "tofolder": "docs", "to": "a.txt", "unique": true}); code != 200 || !strings.Contains(b, "a copy 2.txt") {
		t.Fatalf("duplicate again: %d %s", code, b)
	}
	// copy a folder into another, and paste a file into a folder
	if code, b := cp(map[string]any{"folder": "docs", "path": "sub", "tofolder": "docs", "to": "other/sub"}); code != 409 {
		t.Fatalf("into a folder that is not there: %d %s", code, b)
	}
	os.MkdirAll(filepath.Join(e.root, "dest"), 0o755)
	if code, b := cp(map[string]any{"folder": "docs", "path": "sub", "tofolder": "docs", "to": "dest/sub", "unique": true}); code != 200 || read("dest/sub/b.txt") != "beta" || read("sub/b.txt") != "beta" {
		t.Fatalf("copy folder: %d %s", code, b)
	}
	// a taken name is refused unless a free one is asked for
	if code, _ := cp(map[string]any{"folder": "docs", "path": "a.txt", "tofolder": "docs", "to": "dest/sub/b.txt"}); code != 409 {
		t.Fatalf("taken: %d", code)
	}
	// move: the source is gone, the content is there
	if code, b := cp(map[string]any{"folder": "docs", "path": "a.txt", "tofolder": "docs", "to": "dest/a.txt", "move": true}); code != 200 || read("dest/a.txt") != "alpha" {
		t.Fatalf("move: %d %s", code, b)
	}
	if _, err := os.Stat(filepath.Join(e.root, "a.txt")); err == nil {
		t.Fatal("the moved file is still there")
	}
	// not into itself, and not to the same place
	if code, _ := cp(map[string]any{"folder": "docs", "path": "dest", "tofolder": "docs", "to": "dest/in/dest", "move": true}); code != 409 {
		t.Fatalf("into itself: %d", code)
	}
	if code, _ := cp(map[string]any{"folder": "docs", "path": "dest/a.txt", "tofolder": "docs", "to": "dest/a.txt", "move": true}); code != 409 {
		t.Fatalf("to the same place: %d", code)
	}
	// between synced folders
	other := t.TempDir()
	s := fdSvc
	s.cfgFn = func() FilesConfig {
		return FilesConfig{Folders: []FolderCfg{{ID: "docs", Path: e.root}, {ID: "more", Path: other}}}
	}
	s.reconcile()
	if code, b := cp(map[string]any{"folder": "docs", "path": "dest/a.txt", "tofolder": "more", "to": "a.txt", "move": true}); code != 200 {
		t.Fatalf("across folders: %d %s", code, b)
	}
	if b, _ := os.ReadFile(filepath.Join(other, "a.txt")); string(b) != "alpha" {
		t.Fatalf("in the other folder: %q", b)
	}
	if _, err := os.Stat(filepath.Join(e.root, "dest", "a.txt")); err == nil {
		t.Fatal("not removed from the first folder")
	}
	// a person who may only look cannot
	rita := &siteClient{e: e}
	if rita.login("rita", "pw") != 200 {
		t.Fatal("login rita")
	}
	if code, _, _ := rita.req(e.site.URL, "POST", "/api/files/copy", map[string]any{"folder": "docs", "path": "sub", "tofolder": "docs", "to": "sub2"}); code != 403 {
		t.Fatalf("read-only copy: %d", code)
	}
}

func TestFilesEmptyTrash(t *testing.T) {
	e := newSiteEnv(t, true)
	f := fdSvc.get("docs")
	if err := f.f.Delete("a.txt"); err != nil { // to the trash
		t.Fatal(err)
	}
	f.f.WriteFile("sub/b.txt", strings.NewReader("replaced")) // the old one goes to the trash too
	call := func(h http.HandlerFunc, method, target, body string) map[string]any {
		rw := httptest.NewRecorder()
		h(rw, httptest.NewRequest(method, target, strings.NewReader(body)))
		var out struct{ Data map[string]any }
		json.Unmarshal(rw.Body.Bytes(), &out)
		return out.Data
	}
	s := &session{user: "root"}
	size := call(func(rw http.ResponseWriter, r *http.Request) { e.ws.handleFilesTrash(rw, r, s) }, "GET", "/api/files/trash?folder=docs", "")
	if size["files"] != float64(2) || size["bytes"] != float64(9) { // alpha (5) + beta (4)
		t.Fatalf("size: %v", size)
	}
	got := call(func(rw http.ResponseWriter, r *http.Request) { e.ws.handleFilesTrashEmpty(rw, r, s) }, "POST", "/api/files/trash/empty", `{"folder":"docs"}`)
	if got["files"] != float64(2) || got["bytes"] != float64(9) {
		t.Fatalf("emptied: %v", got)
	}
	if b, n := f.f.TrashSize(); b != 0 || n != 0 {
		t.Fatalf("still there: %d %d", b, n)
	}
	if b, err := os.ReadFile(filepath.Join(e.root, "sub", "b.txt")); err != nil || string(b) != "replaced" {
		t.Fatalf("the live file was touched: %q %v", b, err)
	}
	if again := call(func(rw http.ResponseWriter, r *http.Request) { e.ws.handleFilesTrashEmpty(rw, r, s) }, "POST", "/api/files/trash/empty", `{"folder":"docs"}`); again["files"] != float64(0) {
		t.Fatalf("empty again: %v", again)
	}
}

// A directory or single sign-on session keeps how it was made when another node takes it over: the node rechecks it the same way, and
// does not look for a local account the person does not have.
func TestFilesSessionKeepsHowThePersonSignedIn(t *testing.T) {
	a, _, _, _ := twoSites(t)
	now := time.Now().Unix()
	k1, k2 := sessKey("one"), sessKey("two")
	a.ws.sessionFromPeer(sessMsg{Op: "put", Rec: &sessionRec{Key: k1, User: "lee", CSRF: "c", Created: now, Last: now, Site: siteFiles, LDAP: true}})
	a.ws.sessionFromPeer(sessMsg{Op: "put", Rec: &sessionRec{Key: k2, User: "kim", CSRF: "c", Created: now, Last: now, Site: siteFiles, SSO: true, SSOProv: "corp"}})
	a.ws.mu.Lock()
	defer a.ws.mu.Unlock()
	if s := a.ws.sessions[k1]; s == nil || !s.ldap {
		t.Fatal("the directory session lost its ldap mark")
	}
	if s := a.ws.sessions[k2]; s == nil || !s.sso || s.ssoProv != "corp" {
		t.Fatal("the single sign-on session lost its mark")
	}
}

// A copy asked for in the background answers at once, says how far it is, and ends with the content in place (and, for a move, the
// source gone).  Somebody else cannot see it.
func TestFilesCopyInTheBackgroundReportsProgress(t *testing.T) {
	e := newSiteEnv(t, true)
	pat := &siteClient{e: e}
	if pat.login("pat", "pw") != 200 {
		t.Fatal("login")
	}
	big := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
	os.WriteFile(filepath.Join(e.root, "big.bin"), big, 0o644)
	wait := func(id string) map[string]any {
		var d map[string]any
		for i := 0; i < 100; i++ {
			code, b, _ := pat.req(e.site.URL, "GET", "/api/files/copy?id="+id, nil)
			if code != 200 {
				t.Fatalf("status: %d %s", code, b)
			}
			var r struct{ Data map[string]any }
			json.Unmarshal(b, &r)
			if d = r.Data; d["state"] != "running" {
				return d
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("the copy did not end")
		return nil
	}
	code, b, _ := pat.req(e.site.URL, "POST", "/api/files/copy", map[string]any{"folder": "docs", "path": "big.bin", "tofolder": "docs", "to": "big.bin", "unique": true, "async": true})
	var r struct {
		Data struct {
			ID, Path string
			Total    int64
		}
	}
	json.Unmarshal(b, &r)
	if code != 200 || r.Data.ID == "" || r.Data.Total != int64(len(big)) {
		t.Fatalf("start: %d %s", code, b)
	}
	if d := wait(r.Data.ID); d["state"] != "done" || int64(d["done"].(float64)) != int64(len(big)) {
		t.Fatalf("end: %v", d)
	}
	if got, _ := os.ReadFile(filepath.Join(e.root, r.Data.Path)); !bytes.Equal(got, big) {
		t.Fatal("the copy differs")
	}
	rita := &siteClient{e: e}
	rita.login("rita", "pw")
	if code, _, _ := rita.req(e.site.URL, "GET", "/api/files/copy?id="+r.Data.ID, nil); code != http.StatusNotFound {
		t.Fatalf("another user's copy: %d", code)
	}
	// a move that goes on in the background removes the source
	code, b, _ = pat.req(e.site.URL, "POST", "/api/files/copy", map[string]any{"folder": "docs", "path": "big.bin", "tofolder": "docs", "to": "moved.bin", "move": true, "async": true})
	json.Unmarshal(b, &r)
	if code != 200 || wait(r.Data.ID)["state"] != "done" {
		t.Fatalf("move: %d %s", code, b)
	}
	if _, err := os.Stat(filepath.Join(e.root, "big.bin")); err == nil {
		t.Fatal("the moved file is still there")
	}
}

// Cancelling a background copy stops it, keeps nothing of it, and leaves the source alone (also for a move).
func TestFilesCopyCanBeCancelled(t *testing.T) {
	e := newSiteEnv(t, true)
	pat := &siteClient{e: e}
	if pat.login("pat", "pw") != 200 {
		t.Fatal("login")
	}
	os.MkdirAll(filepath.Join(e.root, "tree"), 0o755)
	for i := 0; i < 400; i++ {
		os.WriteFile(filepath.Join(e.root, "tree", fmt.Sprintf("f%d", i)), bytes.Repeat([]byte("x"), 64<<10), 0o644)
	}
	code, b, _ := pat.req(e.site.URL, "POST", "/api/files/copy", map[string]any{"folder": "docs", "path": "tree", "tofolder": "docs", "to": "tree2", "async": true})
	var r struct{ Data struct{ ID string } }
	json.Unmarshal(b, &r)
	if code != 200 || r.Data.ID == "" {
		t.Fatalf("start: %d %s", code, b)
	}
	rita := &siteClient{e: e}
	rita.login("rita", "pw")
	if code, _, _ := rita.req(e.site.URL, "POST", "/api/files/copy/cancel", map[string]string{"id": r.Data.ID}); code != http.StatusNotFound && code != http.StatusForbidden {
		t.Fatalf("another user cancelling: %d", code)
	}
	pat.req(e.site.URL, "POST", "/api/files/copy/cancel", map[string]string{"id": r.Data.ID})
	var state string
	for i := 0; i < 200 && state != "cancelled" && state != "done"; i++ {
		_, b, _ := pat.req(e.site.URL, "GET", "/api/files/copy?id="+r.Data.ID, nil)
		var st struct{ Data struct{ State string } }
		json.Unmarshal(b, &st)
		if state = st.Data.State; state == "running" {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if state == "done" {
		t.Skip("the copy ended before the cancel arrived")
	}
	if state != "cancelled" {
		t.Fatalf("state: %q", state)
	}
	if n, _ := filepath.Glob(filepath.Join(e.root, "tree", "f*")); len(n) != 400 {
		t.Fatalf("the source lost files: %d left", len(n))
	}
	if _, err := os.Stat(filepath.Join(e.root, "tree2")); err == nil {
		t.Fatal("a cancelled copy left its folder")
	}
}

// The size of a folder is everything under it, all the way down; a path that is empty gives the whole synced folder.
func TestFilesSizeAddsUpEverythingUnderAFolder(t *testing.T) {
	e := newSiteEnv(t, true)
	pat := &siteClient{e: e}
	if pat.login("pat", "pw") != 200 {
		t.Fatal("login")
	}
	os.MkdirAll(filepath.Join(e.root, "top/mid/low"), 0o755)
	os.WriteFile(filepath.Join(e.root, "top/a.bin"), make([]byte, 1000), 0o644)
	os.WriteFile(filepath.Join(e.root, "top/mid/b.bin"), make([]byte, 20000), 0o644)
	os.WriteFile(filepath.Join(e.root, "top/mid/low/c.bin"), make([]byte, 300000), 0o644)
	os.WriteFile(filepath.Join(e.root, "other.bin"), make([]byte, 5), 0o644)
	pat.req(e.site.URL, "POST", "/api/files/mkdir", map[string]any{"folder": "docs", "path": "scan-trigger"})
	for i := 0; i < 50; i++ {
		_, b, _ := pat.req(e.site.URL, "GET", "/api/files/size?folder=docs&path=top", nil)
		var r struct {
			Data struct{ Files, Dirs, Bytes int64 }
		}
		json.Unmarshal(b, &r)
		if r.Data.Bytes == 321000 {
			if r.Data.Files != 3 || r.Data.Dirs != 2 {
				t.Fatalf("counts: %s", b)
			}
			_, b, _ = pat.req(e.site.URL, "GET", "/api/files/size?folder=docs&path=", nil)
			json.Unmarshal(b, &r)
			if r.Data.Bytes < 321005 {
				t.Fatalf("whole folder: %s", b)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the size never added up to 321000")
}

// A big upload that is cut off is taken up again where it ended: begin says how much is here, the rest is sent, finish puts it in place.
// Somebody else cannot use the upload, a piece that starts at the wrong place is refused with where to start, and finishing twice gives
// the same answer.
func TestFilesUploadResumesWhereItEnded(t *testing.T) {
	e := newSiteEnv(t, true)
	pat := &siteClient{e: e}
	if pat.login("pat", "pw") != 200 {
		t.Fatal("login")
	}
	data := make([]byte, 3<<20+12345)
	for i := range data {
		data[i] = byte(i * 7)
	}
	begin := func() (string, int64) {
		code, b, _ := pat.req(e.site.URL, "POST", "/api/files/upload/begin", map[string]any{"folder": "docs", "path": "up/big.bin", "size": len(data)})
		var r struct {
			Data struct {
				ID     string
				Offset int64
			}
		}
		json.Unmarshal(b, &r)
		if code != 200 || r.Data.ID == "" {
			t.Fatalf("begin: %d %s", code, b)
		}
		return r.Data.ID, r.Data.Offset
	}
	id, off := begin()
	if off != 0 {
		t.Fatalf("a new upload starts at %d", off)
	}
	chunk := func(id string, from, to int64) (int, map[string]any) {
		code, b, _ := pat.req(e.site.URL, "POST", fmt.Sprintf("/api/files/upload/chunk?id=%s&offset=%d", id, from), data[from:to])
		var r map[string]any
		json.Unmarshal(b, &r)
		return code, r
	}
	if code, r := chunk(id, 0, 1<<20); code != 200 || r["data"].(map[string]any)["offset"].(float64) != 1<<20 {
		t.Fatalf("first piece: %d %v", code, r)
	}
	// the connection drops; the browser asks again and the same upload is found, with what came
	id2, off := begin()
	if id2 != id || off != 1<<20 {
		t.Fatalf("not taken up again: %s at %d (was %s)", id2, off, id)
	}
	if code, r := chunk(id, 5, 100); code != http.StatusConflict || r["offset"].(float64) != 1<<20 {
		t.Fatalf("a piece at the wrong place: %d %v", code, r)
	}
	rita := &siteClient{e: e}
	rita.login("rita", "pw")
	if code, _, _ := rita.req(e.site.URL, "POST", "/api/files/upload/chunk?id="+id+"&offset=0", []byte("x")); code == 200 {
		t.Fatal("another user wrote to the upload")
	}
	if code, _ := chunk(id, 1<<20, int64(len(data))); code != 200 {
		t.Fatalf("the rest: %d", code)
	}
	for i := 0; i < 2; i++ { // the second asks again after a lost answer
		code, b, _ := pat.req(e.site.URL, "POST", "/api/files/upload/finish", map[string]any{"id": id})
		if code != 200 {
			t.Fatalf("finish %d: %d %s", i, code, b)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(e.root, "up/big.bin")); !bytes.Equal(got, data) {
		t.Fatal("the file differs")
	}
	if left, _ := filepath.Glob(filepath.Join(e.root, ".filedir/tmp/resume-*")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
	// an upload that is not all here is not finished
	_, b, _ := pat.req(e.site.URL, "POST", "/api/files/upload/begin", map[string]any{"folder": "docs", "path": "short.bin", "size": 100})
	var r2 struct{ Data struct{ ID string } }
	json.Unmarshal(b, &r2)
	pat.req(e.site.URL, "POST", "/api/files/upload/chunk?id="+r2.Data.ID+"&offset=0", data[:40])
	if code, _, _ := pat.req(e.site.URL, "POST", "/api/files/upload/finish", map[string]any{"id": r2.Data.ID}); code != http.StatusConflict {
		t.Fatalf("finishing 40 of 100 bytes: %d", code)
	}
	if _, err := os.Stat(filepath.Join(e.root, "short.bin")); err == nil {
		t.Fatal("a part of a file was put in place")
	}
}

// The Files page is served by two sites, the management GUI and the files site, each with its own route table.  Every file call of the
// files site must be in the management GUI's table too, or the same page gets a 404 there (the zip download once did).
func TestBothSitesServeTheSameFileCalls(t *testing.T) {
	routes := func(file string) map[string]bool {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, m := range regexp.MustCompile(`"(GET|POST|PUT|DELETE) (/api/files/[a-z/]*)"`).FindAllStringSubmatch(string(b), -1) {
			out[m[1]+" "+m[2]] = true
		}
		return out
	}
	admin, files := routes("web.go"), routes("webfiles.go")
	if len(files) < 10 || len(admin) < 10 {
		t.Fatalf("found %d and %d routes: the pattern no longer matches how they are written", len(files), len(admin))
	}
	for r := range files {
		if !admin[r] {
			t.Errorf("%s is served by the files site but not by the management GUI", r)
		}
	}
}
