package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"filedir/filesync"
)

// WebDAV on the files site, at /dav/.  A drive in Windows, a folder in Finder, davfs2, rclone, Cyberduck and the Office programs reach the
// synced folders through it.  It is the same folders, the same people and the same rules as the Files page:
//   - the name and password are the sign-in page's (host accounts, then the directory), with its lockout, and the same groups decide who
//     may come and who may only look;
//   - the layout is the Files page's: if there is a root folder ("/"), /dav/ is that folder with the other folders listed in it,
//     otherwise /dav/ lists the folders;
//   - a write is made in this node's copy, and the next scan gives it a version for the other nodes.
// A person who signs in through SSO has no password to give, so cannot use it.  Locks are handed out and not enforced (the clients that
// ask for one before they write need only the answer).

const (
	davPrefix  = "/dav"
	davCredTTL = time.Minute // a sign-in that was good is not asked again for this long: a client sends its password with every request
)

type davCred struct {
	ro   bool
	exp  time.Time
	seen time.Time
}

type davCreds struct {
	mu   sync.Mutex
	salt [16]byte
	m    map[[32]byte]davCred
}

func (c *davCreds) key(user, pass string) [32]byte {
	if c.salt == ([16]byte{}) {
		rand.Read(c.salt[:])
	}
	h := sha256.New()
	h.Write(c.salt[:])
	h.Write([]byte(user + "\x00" + pass))
	var k [32]byte
	h.Sum(k[:0])
	return k
}

// get returns a good sign-in from the last minute, and whether the person has been seen at all lately (so one line in the log is
// enough for a client that signs in again every minute).
func (c *davCreds) get(user, pass string) (cr davCred, ok, recent bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cr, found := c.m[c.key(user, pass)]
	if !found {
		return cr, false, false
	}
	return cr, time.Now().Before(cr.exp), time.Since(cr.seen) < 10*time.Minute
}

func (c *davCreds) put(user, pass string, ro bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[[32]byte]davCred{}
	}
	now := time.Now()
	if len(c.m) > 2000 {
		for k, v := range c.m {
			if now.Sub(v.seen) > 10*time.Minute {
				delete(c.m, k)
			}
		}
	}
	c.m[c.key(user, pass)] = davCred{ro: ro, exp: now.Add(davCredTTL), seen: now}
}

// davAuth asks for and checks the Basic sign-in.  When it returns false the answer has been sent.
func (w *WebServer) davAuth(rw http.ResponseWriter, r *http.Request) (user string, ro, ok bool) {
	ask := func() {
		rw.Header().Set("WWW-Authenticate", `Basic realm="FileDirector", charset="UTF-8"`)
		http.Error(rw, "sign in", http.StatusUnauthorized)
	}
	user, pass, has := r.BasicAuth()
	if !has {
		ask()
		return "", false, false
	}
	if cr, good, _ := w.dav.get(user, pass); good {
		return user, cr.ro, true
	}
	ip := clientIP(r)
	keys := loginKeys(ip, user)
	select {
	case w.loginQ <- struct{}{}:
		defer func() { <-w.loginQ }()
	default:
		rw.Header().Set("Retry-After", "2")
		http.Error(rw, "too many sign-ins at once", http.StatusServiceUnavailable)
		return "", false, false
	}
	if wait, ok := w.reserve(keys...); !ok {
		w.logLockedOut(ip, user, wait)
		rw.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		http.Error(rw, "too many failed sign-ins; wait", http.StatusTooManyRequests)
		return "", false, false
	}
	readOnly, _, reason := w.verifyCreds(siteFiles, user, pass, w.policy.Load())
	if reason != "" {
		lockedFor := w.release(true, keys...)
		warnf("web: WebDAV sign-in failed for %q from %s: %s", user, ip, reason)
		time.Sleep(failDelay)
		if lockedFor > 0 {
			warnf("web: locking out %q from %s for %s after repeated failed logins", user, ip, lockedFor.Round(time.Second))
			rw.Header().Set("Retry-After", strconv.Itoa(int(lockedFor.Seconds())+1))
			http.Error(rw, "too many failed sign-ins; wait", http.StatusTooManyRequests)
			return "", false, false
		}
		ask()
		return "", false, false
	}
	w.release(false, keys...)
	w.clearFailures(keys...)
	if _, _, recent := w.dav.get(user, pass); !recent {
		infof("web: login: %q signed in to WebDAV from %s (%s)", user, ip, accessWord(readOnly))
	}
	w.dav.put(user, pass, readOnly)
	return user, readOnly, true
}

// ── where a URL points ──

type davTarget struct {
	f       *fdFolder // nil: the list of folders (there is no root folder)
	id, rel string
}

func (t davTarget) top() bool { return t.rel == "" }

// davResolve says which folder and which name in it a URL path is.  The first part names a synced folder when there is one of that name;
// otherwise it is a name in the root folder.
func davResolve(urlPath string) (davTarget, bool) {
	p := strings.TrimPrefix(urlPath, davPrefix)
	p = strings.Trim(path.Clean("/"+p), "/")
	if fdSvc == nil {
		return davTarget{}, false
	}
	root := fdSvc.get(fdRootID)
	if root != nil && root.f == nil {
		root = nil
	}
	if p == "" {
		if root != nil {
			return davTarget{f: root, id: fdRootID}, true
		}
		return davTarget{}, true
	}
	first, rest, _ := strings.Cut(p, "/")
	if first != fdRootID {
		if f := fdSvc.get(first); f != nil && f.f != nil {
			return davTarget{f: f, id: first, rel: rest}, true
		}
	}
	if root != nil {
		return davTarget{f: root, id: fdRootID, rel: p}, true
	}
	return davTarget{}, false
}

// href is the URL of an item: folders end in a slash.
func davHref(segs []string, dir bool) string {
	var b strings.Builder
	b.WriteString(davPrefix)
	for _, s := range segs {
		if s != "" {
			b.WriteString("/" + (&url.URL{Path: s}).EscapedPath())
		}
	}
	if dir || len(segs) == 0 {
		b.WriteString("/")
	}
	return b.String()
}

func davSegs(p string) []string {
	p = strings.Trim(strings.TrimPrefix(path.Clean("/"+strings.TrimPrefix(p, davPrefix)), "/"), "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// ── the answer to PROPFIND ──

type davItem struct {
	href string
	name string
	dir  bool
	size int64
	mod  time.Time
}

func davEtag(it davItem) string {
	return `"` + strconv.FormatInt(it.size, 16) + "-" + strconv.FormatInt(it.mod.UnixNano(), 16) + `"`
}

func davXML(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

type davPropReq struct {
	allprop, propname bool
	props             []xml.Name
}

// davParseProps reads what a PROPFIND or PROPPATCH names: the children of every <prop>, or allprop / propname.
func davParseProps(body []byte) davPropReq {
	var out davPropReq
	d := xml.NewDecoder(strings.NewReader(string(body)))
	depth, propAt := 0, -1
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			switch {
			case t.Name.Local == "allprop":
				out.allprop = true
			case t.Name.Local == "propname":
				out.propname = true
			case t.Name.Local == "prop" && propAt < 0:
				propAt = depth
			case propAt >= 0 && depth == propAt+1:
				out.props = append(out.props, t.Name)
			}
		case xml.EndElement:
			if depth == propAt {
				propAt = -1
			}
			depth--
		}
	}
	return out
}

var davKnown = []string{"resourcetype", "displayname", "getcontentlength", "getlastmodified", "creationdate", "getcontenttype", "getetag", "supportedlock", "lockdiscovery"}

func davPropValue(it davItem, name string) (string, bool) {
	switch name {
	case "resourcetype":
		if it.dir {
			return "<D:collection/>", true
		}
		return "", true
	case "displayname":
		return davXML(it.name), true
	case "getcontentlength":
		return strconv.FormatInt(it.size, 10), !it.dir
	case "getlastmodified":
		return it.mod.UTC().Format(http.TimeFormat), true
	case "creationdate":
		return it.mod.UTC().Format(time.RFC3339), true
	case "getcontenttype":
		if it.dir {
			return "", false
		}
		return davXML(davType(it.name)), true
	case "getetag":
		return davXML(davEtag(it)), !it.dir
	case "supportedlock":
		return "<D:lockentry><D:lockscope><D:exclusive/></D:lockscope><D:locktype><D:write/></D:locktype></D:lockentry>", true
	case "lockdiscovery":
		return "", true
	}
	return "", false
}

func davType(name string) string {
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

func davWriteItem(b *strings.Builder, it davItem, rq davPropReq) {
	b.WriteString("<D:response><D:href>" + davXML(it.href) + "</D:href>")
	var ok, missing strings.Builder
	switch {
	case rq.propname:
		for _, n := range davKnown {
			if _, has := davPropValue(it, n); has {
				ok.WriteString("<D:" + n + "/>")
			}
		}
	case len(rq.props) > 0:
		for _, n := range rq.props {
			if n.Space == "DAV:" {
				if v, has := davPropValue(it, n.Local); has {
					ok.WriteString("<D:" + n.Local)
					if v == "" {
						ok.WriteString("/>")
					} else {
						ok.WriteString(">" + v + "</D:" + n.Local + ">")
					}
					continue
				}
			}
			missing.WriteString("<" + davXML(n.Local) + ` xmlns="` + davXML(n.Space) + `"/>`)
		}
	default:
		for _, n := range davKnown {
			if v, has := davPropValue(it, n); has {
				if v == "" {
					ok.WriteString("<D:" + n + "/>")
				} else {
					ok.WriteString("<D:" + n + ">" + v + "</D:" + n + ">")
				}
			}
		}
	}
	if ok.Len() > 0 || missing.Len() == 0 {
		b.WriteString("<D:propstat><D:prop>" + ok.String() + "</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat>")
	}
	if missing.Len() > 0 {
		b.WriteString("<D:propstat><D:prop>" + missing.String() + "</D:prop><D:status>HTTP/1.1 404 Not Found</D:status></D:propstat>")
	}
	b.WriteString("</D:response>")
}

func davMulti(rw http.ResponseWriter, body string) {
	rw.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	rw.WriteHeader(http.StatusMultiStatus)
	io.WriteString(rw, `<?xml version="1.0" encoding="utf-8"?><D:multistatus xmlns:D="DAV:">`+body+`</D:multistatus>`)
}

func davItemOf(href, name string, st os.FileInfo) davItem {
	return davItem{href: href, name: name, dir: st.IsDir(), size: st.Size(), mod: st.ModTime()}
}

// davStat describes what a target is.  The list of folders and a folder's top are collections.
func davStat(t davTarget, segs []string) (davItem, error) {
	name := ""
	if len(segs) > 0 {
		name = segs[len(segs)-1]
	}
	if t.f == nil {
		return davItem{href: davHref(nil, true), dir: true, mod: time.Now()}, nil
	}
	st, err := t.f.f.Stat(t.rel)
	if err != nil {
		return davItem{}, err
	}
	if !st.IsDir() && !st.Mode().IsRegular() {
		return davItem{}, filesync.ErrNotFound // a link or a device is not shown
	}
	return davItemOf(davHref(segs, st.IsDir()), name, st), nil
}

func davChildren(t davTarget, segs []string) ([]davItem, error) {
	var out []davItem
	if t.f == nil || (t.id == fdRootID && t.rel == "") {
		for _, f := range fdSvc.list() {
			if f.cfg.ID == fdRootID || f.f == nil {
				continue
			}
			out = append(out, davItem{href: davHref(append(append([]string{}, segs...), f.cfg.ID), true), name: f.cfg.ID, dir: true, mod: time.Now()})
		}
		if t.f == nil {
			return out, nil
		}
	}
	mounts := map[string]bool{}
	for _, it := range out {
		mounts[it.name] = true
	}
	ents, err := t.f.f.ReadDir(t.rel)
	if err != nil {
		return nil, err
	}
	for _, st := range ents {
		if (!st.IsDir() && !st.Mode().IsRegular()) || (t.rel == "" && mounts[st.Name()]) {
			continue
		}
		out = append(out, davItemOf(davHref(append(append([]string{}, segs...), st.Name()), st.IsDir()), st.Name(), st))
	}
	return out, nil
}

// ── the handler ──

func davFail(rw http.ResponseWriter, err error) {
	code, msg := http.StatusInternalServerError, "that did not work"
	switch {
	case errors.Is(err, filesync.ErrBadName):
		code, msg = http.StatusForbidden, "that name is not allowed"
	case errors.Is(err, filesync.ErrNotFound):
		code, msg = http.StatusNotFound, "not found"
	case errors.Is(err, filesync.ErrExists):
		code, msg = http.StatusPreconditionFailed, "that name is already there"
	case errors.Is(err, filesync.ErrIsDir):
		code, msg = http.StatusMethodNotAllowed, "that is a folder"
	case errors.Is(err, filesync.ErrTooBig):
		code, msg = http.StatusRequestEntityTooLarge, "the file is larger than this folder allows"
	case errors.Is(err, filesync.ErrNoMarker):
		code, msg = http.StatusServiceUnavailable, "the folder is not there (is its disk mounted?)"
	case errors.Is(err, filesync.ErrDiskFull):
		code, msg = http.StatusInsufficientStorage, "the disk is full up to the limit set for files"
	default:
		warnf("webdav: %v", err)
	}
	http.Error(rw, msg, code)
}

const davAllow = "OPTIONS, GET, HEAD, PROPFIND, PROPPATCH, PUT, DELETE, MKCOL, MOVE, COPY, LOCK, UNLOCK"

func (w *WebServer) handleDAV(rw http.ResponseWriter, r *http.Request) {
	user, ro, ok := w.davAuth(rw, r)
	if !ok {
		return
	}
	h := rw.Header()
	h.Set("DAV", "1, 2")
	h.Set("MS-Author-Via", "DAV")
	switch r.Method {
	case http.MethodOptions:
		h.Set("Allow", davAllow)
		rw.WriteHeader(http.StatusOK)
		return
	case http.MethodGet, http.MethodHead, "PROPFIND":
	case "PUT", "DELETE", "MKCOL", "MOVE", "COPY", "PROPPATCH", "LOCK", "UNLOCK":
		if !sameOrigin(r) {
			http.Error(rw, "cross-origin request refused", http.StatusForbidden)
			return
		}
		if ro {
			http.Error(rw, readOnlyMsg, http.StatusForbidden)
			return
		}
	default:
		h.Set("Allow", davAllow)
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	t, found := davResolve(r.URL.Path)
	segs := davSegs(r.URL.Path)
	if !found {
		http.Error(rw, "not found", http.StatusNotFound)
		return
	}
	aud := &session{user: user}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		w.davGet(rw, r, t, segs)
	case "PROPFIND":
		w.davPropfind(rw, r, t, segs)
	case "PROPPATCH":
		it, err := davStat(t, segs)
		if err != nil {
			davFail(rw, err)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var b strings.Builder
		b.WriteString("<D:response><D:href>" + davXML(it.href) + "</D:href><D:propstat><D:prop>")
		for _, n := range davParseProps(body).props { // the times and flags a client sets are not kept: they are the file's own
			b.WriteString("<" + davXML(n.Local) + ` xmlns="` + davXML(n.Space) + `"/>`)
		}
		b.WriteString("</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>")
		davMulti(rw, b.String())
	case "LOCK":
		io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		var tok [16]byte
		rand.Read(tok[:])
		u := "opaquelocktoken:" + hex.EncodeToString(tok[:])
		h.Set("Lock-Token", "<"+u+">")
		h.Set("Content-Type", `application/xml; charset="utf-8"`)
		io.WriteString(rw, `<?xml version="1.0" encoding="utf-8"?><D:prop xmlns:D="DAV:"><D:lockdiscovery><D:activelock><D:locktype><D:write/></D:locktype>`+
			`<D:lockscope><D:exclusive/></D:lockscope><D:depth>infinity</D:depth><D:timeout>Second-600</D:timeout>`+
			`<D:locktoken><D:href>`+u+`</D:href></D:locktoken><D:lockroot><D:href>`+davXML(r.URL.Path)+`</D:href></D:lockroot></D:activelock></D:lockdiscovery></D:prop>`)
	case "UNLOCK":
		rw.WriteHeader(http.StatusNoContent)
	case "PUT":
		w.davPut(rw, r, t, aud)
	case "DELETE":
		if t.f == nil || t.top() {
			http.Error(rw, "a folder is removed under Synced Folders", http.StatusForbidden)
			return
		}
		if err := t.f.f.Delete(t.rel); err != nil {
			davFail(rw, err)
			return
		}
		t.f.rescan()
		fdAudit(aud, "webdav: %s: deleted %s (kept in the trash)", t.id, t.rel)
		rw.WriteHeader(http.StatusNoContent)
	case "MKCOL":
		if n, _ := io.Copy(io.Discard, io.LimitReader(r.Body, 1)); n > 0 {
			http.Error(rw, "a folder has no body", http.StatusUnsupportedMediaType)
			return
		}
		if t.f == nil || t.top() {
			http.Error(rw, "that already exists", http.StatusMethodNotAllowed)
			return
		}
		if _, err := t.f.f.Stat(t.rel); err == nil {
			http.Error(rw, "that already exists", http.StatusMethodNotAllowed)
			return
		}
		if !davParentThere(t) {
			http.Error(rw, "the folder above is not there", http.StatusConflict)
			return
		}
		if err := t.f.f.Mkdir(t.rel); err != nil {
			davFail(rw, err)
			return
		}
		t.f.rescan()
		fdAudit(aud, "webdav: %s: made folder %s", t.id, t.rel)
		rw.WriteHeader(http.StatusCreated)
	case "MOVE", "COPY":
		w.davMoveCopy(rw, r, t, aud)
	}
}

// davParentThere says whether the folder above a name exists.
func davParentThere(t davTarget) bool {
	p := path.Dir(t.rel)
	if p == "." {
		return true
	}
	st, err := t.f.f.Stat(p)
	return err == nil && st.IsDir()
}

func (w *WebServer) davGet(rw http.ResponseWriter, r *http.Request, t davTarget, segs []string) {
	if t.f == nil || t.top() {
		http.Redirect(rw, r, "/", http.StatusSeeOther) // a browser pointed here is sent to the Files page
		return
	}
	fh, st, err := t.f.f.OpenRead(t.rel)
	if errors.Is(err, filesync.ErrIsDir) {
		http.Redirect(rw, r, "/", http.StatusSeeOther)
		return
	}
	if err != nil {
		davFail(rw, err)
		return
	}
	defer fh.Close()
	fdNoDeadlines(rw)
	rw = fdDownloadTo(rw, r)
	it := davItemOf("", path.Base(t.rel), st)
	rw.Header().Set("Content-Type", davType(it.name))
	rw.Header().Set("ETag", davEtag(it))
	http.ServeContent(rw, r, "", st.ModTime(), fh)
}

func (w *WebServer) davPropfind(rw http.ResponseWriter, r *http.Request, t davTarget, segs []string) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	rq := davParseProps(body)
	depth := r.Header.Get("Depth")
	if depth == "infinity" {
		rw.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
		rw.WriteHeader(http.StatusForbidden)
		io.WriteString(rw, `<?xml version="1.0" encoding="utf-8"?><D:error xmlns:D="DAV:"><D:propfind-finite-depth/></D:error>`)
		return
	}
	it, err := davStat(t, segs)
	if err != nil {
		davFail(rw, err)
		return
	}
	var b strings.Builder
	davWriteItem(&b, it, rq)
	if it.dir && depth != "0" {
		kids, err := davChildren(t, segs)
		if err != nil {
			davFail(rw, err)
			return
		}
		for _, k := range kids {
			davWriteItem(&b, k, rq)
		}
	}
	davMulti(rw, b.String())
}

func (w *WebServer) davPut(rw http.ResponseWriter, r *http.Request, t davTarget, aud *session) {
	if t.f == nil || t.top() {
		http.Error(rw, "that is a folder", http.StatusMethodNotAllowed)
		return
	}
	existed := false
	if st, err := t.f.f.Stat(t.rel); err == nil {
		if st.IsDir() {
			http.Error(rw, "that is a folder", http.StatusMethodNotAllowed)
			return
		}
		existed = true
	}
	if !davParentThere(t) {
		http.Error(rw, "the folder above is not there", http.StatusConflict)
		return
	}
	fdNoDeadlines(rw)
	n, err := t.f.f.WriteFile(t.rel, fdUploadFrom(r))
	if err != nil {
		davFail(rw, err)
		return
	}
	t.f.rescan()
	fdAudit(aud, "webdav: %s: wrote %s (%d bytes)", t.id, t.rel, n)
	if existed {
		rw.WriteHeader(http.StatusNoContent)
	} else {
		rw.WriteHeader(http.StatusCreated)
	}
}

// davCopyTree copies a file, or a folder and what is in it, from one place to another (which may be in another synced folder).
func davCopyTree(src, dst davTarget, shallow bool) error {
	return davCopyTreeN(context.Background(), src, dst, shallow, nil)
}

// davTreeSize is how many bytes of files a tree holds.
func davTreeSize(t davTarget) int64 {
	st, err := t.f.f.Stat(t.rel)
	if err != nil {
		return 0
	}
	if !st.IsDir() {
		return st.Size()
	}
	var n int64
	kids, _ := t.f.f.ReadDir(t.rel)
	for _, k := range kids {
		if k.IsDir() || k.Mode().IsRegular() {
			c := t
			c.rel = path.Join(t.rel, k.Name())
			n += davTreeSize(c)
		}
	}
	return n
}

// davCopyTreeN copies like davCopyTree and counts the bytes of files copied into done (when it is not nil).
func davCopyTreeN(ctx context.Context, src, dst davTarget, shallow bool, done *atomic.Int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	st, err := src.f.f.Stat(src.rel)
	if err != nil {
		return err
	}
	if st.IsDir() {
		if err := dst.f.f.Mkdir(dst.rel); err != nil {
			return err
		}
		if shallow {
			return nil
		}
		kids, err := src.f.f.ReadDir(src.rel)
		if err != nil {
			return err
		}
		for _, k := range kids {
			if !k.IsDir() && !k.Mode().IsRegular() {
				continue
			}
			s2, d2 := src, dst
			s2.rel, d2.rel = path.Join(src.rel, k.Name()), path.Join(dst.rel, k.Name())
			if err := davCopyTreeN(ctx, s2, d2, false, done); err != nil {
				return err
			}
		}
		return nil
	}
	fh, _, err := src.f.f.OpenRead(src.rel)
	if err != nil {
		return err
	}
	defer fh.Close()
	var rd io.Reader = fdCtxReader{ctx, fh}
	if done != nil {
		rd = fdCountReader{rd, done}
	}
	_, err = dst.f.f.WriteFileHashed(dst.rel, rd, st.Size())
	return err
}

func (w *WebServer) davMoveCopy(rw http.ResponseWriter, r *http.Request, src davTarget, aud *session) {
	move := r.Method == "MOVE"
	if src.f == nil || src.top() {
		http.Error(rw, "a folder under Synced Folders cannot be moved or copied here", http.StatusForbidden)
		return
	}
	du, err := url.Parse(r.Header.Get("Destination"))
	if err != nil || du.Path == "" || (du.Host != "" && du.Host != r.Host) || !strings.HasPrefix(du.Path, davPrefix+"/") {
		http.Error(rw, "the destination is not here", http.StatusBadGateway)
		return
	}
	dst, found := davResolve(du.Path)
	if !found || dst.f == nil || dst.top() {
		http.Error(rw, "that destination is not allowed", http.StatusForbidden)
		return
	}
	if _, err := src.f.f.Stat(src.rel); err != nil {
		davFail(rw, err)
		return
	}
	if src.id == dst.id && (src.rel == dst.rel || strings.HasPrefix(dst.rel, src.rel+"/")) {
		http.Error(rw, "that cannot be put into itself", http.StatusForbidden)
		return
	}
	if !davParentThere(dst) {
		http.Error(rw, "the folder above the destination is not there", http.StatusConflict)
		return
	}
	existed := false
	if _, err := dst.f.f.Stat(dst.rel); err == nil {
		existed = true
		if strings.EqualFold(r.Header.Get("Overwrite"), "F") {
			http.Error(rw, "the destination is there", http.StatusPreconditionFailed)
			return
		}
		if err := dst.f.f.Delete(dst.rel); err != nil { // to the trash, like any other delete
			davFail(rw, err)
			return
		}
	}
	switch {
	case move && src.id == dst.id:
		err = src.f.f.Rename(src.rel, dst.rel)
	default:
		err = davCopyTree(src, dst, r.Header.Get("Depth") == "0")
		if err == nil && move {
			err = src.f.f.Delete(src.rel)
		}
	}
	if err != nil {
		davFail(rw, err)
		return
	}
	src.f.rescan()
	dst.f.rescan()
	fdAudit(aud, "webdav: %s: %s %s/%s to %s/%s", dst.id, map[bool]string{true: "moved", false: "copied"}[move], src.id, src.rel, dst.id, dst.rel)
	if existed {
		rw.WriteHeader(http.StatusNoContent)
	} else {
		rw.WriteHeader(http.StatusCreated)
	}
}

// fdCtxReader stops reading when its context ends, so a copy that is cancelled stops at the next read.
type fdCtxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c fdCtxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
