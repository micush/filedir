package main

import (
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The files site.  The gateways' addresses (the VIPs, the further addresses and the anycast addresses) answer on settings.mcp_port, 443 by
// default, with /mcp, and, when web.files_group or web.files_read_group names a group, with a web site for the people who use the files: a
// sign-in page and the Files page, and nothing of the cluster.  It is not the management GUI, which stays on the node's own address
// (web.listen) and shows Files as one of its pages.  What the files site serves is the list below and nothing else; every other path
// is a 404, so no setting, node, address or log can be asked for through it.
//
// Its sessions have a cookie of their own and cannot be used on the management GUI, nor can the GUI's on the files site; they are shared
// among the nodes (websessshare.go).  Sign-in is a PAM account in one of the two groups, or a directory (LDAP) or single sign-on user the management GUI lets in, at the same level.

// filesAccess says whether user may sign in to the files site, and whether only to look: the members of the full-access and read-only
// groups (the same people as on the management GUI) and of the files-only groups.  Full access wins.
func (w *WebServer) filesAccess(user string, pol *WebConfig) (ok, readOnly bool, err error) {
	asked, failed := 0, 0
	in := func(group string) bool {
		if group == "" {
			return false
		}
		asked++
		m, e := w.auth.InGroup(user, group)
		if e != nil {
			failed++
			err = e
		}
		return m
	}
	if in(pol.Group) || in(pol.FilesGroup) {
		return true, false, nil
	}
	if in(pol.ReadGroup) || in(pol.FilesReadGroup) {
		return true, true, nil
	}
	if failed < asked {
		err = nil // some answered: the plain "not a member" answer
	}
	return false, false, err
}

// filesSiteHandler is the routes of the files site, to be put beside /mcp.
func (w *WebServer) filesSiteHandler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "webui")
	static := http.FileServerFS(sub)
	on := func(h http.Handler) http.Handler { // the site is off until a group is set, and goes off when it is cleared
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(rw, r)
		})
	}
	authed := func(h func(http.ResponseWriter, *http.Request, *session)) http.Handler {
		return on(slowLog(w.authedSite(siteFiles, h, true)))
	}
	authedRaw := func(h func(http.ResponseWriter, *http.Request, *session)) http.Handler {
		return on(w.authedSite(siteFiles, h, false))
	}
	// A download link pasted into a browser that is not signed in goes to the sign-in page and on to the download afterwards, and not to
	// a bare "not logged in"; anything that is not a page navigation (a script, curl) still gets the 401.
	signInFirst := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			nav := r.Header.Get("Sec-Fetch-Mode") == "navigate" || (r.Header.Get("Sec-Fetch-Mode") == "" && strings.Contains(r.Header.Get("Accept"), "text/html"))
			if nav && w.lookupSite(r, siteFiles) == nil {
				http.Redirect(rw, r, "/?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			h.ServeHTTP(rw, r)
		})
	}
	mux.Handle("GET /{$}", on(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, err := fs.ReadFile(sub, "files.html")
		if err != nil {
			http.NotFound(rw, r)
			return
		}
		rw.Header().Set("Content-Type", "text/html; charset=utf-8")
		rw.Write(b)
	})))
	mux.Handle("GET /app.js", on(http.HandlerFunc(serveAppJS)))
	mux.Handle("GET /style.css", on(static))
	mux.Handle("GET /api/login/state", on(http.HandlerFunc(w.handleFilesLoginState)))
	mux.Handle("GET /api/sso/start", on(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { w.ssoStart(rw, r, siteFiles) })))
	mux.Handle("GET /api/sso/callback", on(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { w.ssoCallback(rw, r, siteFiles) })))
	mux.Handle("POST /api/login", on(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { w.login(rw, r, siteFiles) })))
	mux.Handle("POST /api/logout", authed(w.handleLogout))
	mux.Handle("GET /api/session", authed(w.handleSession))
	mux.Handle("GET /api/files/status", authed(w.handleFilesStatusSite))
	mux.Handle("GET /api/files/list", authed(w.handleFilesList))
	mux.Handle("GET /api/files/search", authed(w.handleFilesSearch))
	mux.Handle("GET /api/files/download", signInFirst(authed(w.handleFilesDownload)))
	mux.Handle("GET /api/files/zip", signInFirst(authed(w.handleFilesZip)))
	mux.Handle("GET /api/files/text", authed(w.handleFilesText))
	mux.Handle("POST /api/files/upload", authedRaw(w.handleFilesUpload))
	mux.Handle("POST /api/files/upload/begin", authed(w.handleFilesUploadBegin))
	mux.Handle("POST /api/files/upload/chunk", authedRaw(w.handleFilesUploadChunk))
	mux.Handle("POST /api/files/upload/finish", authed(w.handleFilesUploadFinish))
	mux.Handle("POST /api/files/mkdir", authed(w.handleFilesMkdir))
	mux.Handle("POST /api/files/share", authed(w.handleFilesShare))
	mux.Handle("POST /api/files/fetch", authed(w.handleFilesFetch))
	mux.Handle("GET /api/files/fetch", authed(w.handleFilesFetchStatus))
	mux.Handle("POST /api/files/fetch/cancel", authed(w.handleFilesFetchCancel))
	mux.Handle("GET /dl/{token}", on(http.HandlerFunc(w.handlePublicDownload)))
	mux.Handle("GET /dl/{token}/{name}", on(http.HandlerFunc(w.handlePublicDownload)))
	mux.Handle("PUT /up/{token}/{name}", on(http.HandlerFunc(w.handleLinkUpload)))
	mux.Handle("POST /up/{token}/{name}", on(http.HandlerFunc(w.handleLinkUpload)))
	mux.Handle("/dav/", on(http.HandlerFunc(w.handleDAV)))
	mux.Handle("/dav", on(http.HandlerFunc(w.handleDAV))) // answered as /dav/ is: a client that asks for /dav (Dolphin, davfs2) does not follow a redirect on PROPFIND
	mux.Handle("POST /api/files/copy", authed(w.handleFilesCopy))
	mux.Handle("GET /api/files/copy", authed(w.handleFilesCopyStatus))
	mux.Handle("GET /api/files/size", authed(w.handleFilesSize))
	mux.Handle("POST /api/files/copy/cancel", authed(w.handleFilesCopyCancel))
	mux.Handle("POST /api/files/rename", authed(w.handleFilesRename))
	mux.Handle("POST /api/files/delete", authed(w.handleFilesDelete))
	return secure(mux)
}

// handleFilesLoginState says only whether the caller's address is locked out.
func (w *WebServer) handleFilesLoginState(rw http.ResponseWriter, r *http.Request) {
	wait, locked := w.throttled("ip:" + clientIP(r))
	out := map[string]any{"ok": true, "locked": locked}
	var sso []map[string]string
	for _, p := range w.ssoCfg().providers() {
		sso = append(sso, map[string]string{"id": p.id, "title": p.title})
	}
	if len(sso) > 0 {
		out["sso"] = sso
	}
	if locked {
		out["retry_after"] = int(wait.Seconds()) + 1
	}
	writeJSON(rw, http.StatusOK, out)
}

// handleFilesStatusSite lists the folders with what each holds.  An end user is not shown where a folder is on the disk, which nodes there
// are, or how syncing is going.
func (w *WebServer) handleFilesStatusSite(rw http.ResponseWriter, r *http.Request, s *session) {
	if fdSvc == nil {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "file sync is not running"})
		return
	}
	type folder struct {
		ID        string `json:"id"`
		Files     int    `json:"files"`
		Dirs      int    `json:"dirs"`
		Bytes     int64  `json:"bytes"`
		Conflicts int    `json:"conflicts"`
		Scanned   int64  `json:"scanned,omitempty"`
	}
	list := []folder{}
	for _, f := range fdSvc.Status().Folders {
		list = append(list, folder{f.ID, f.Files, f.Dirs, f.Bytes, f.Conflict, f.Scanned})
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"nodes": []map[string]any{{"folders": list}}, "link_host": filesLinkHost(w.status.sup.config())}})
}

// filesLinkHost is the address a copied download link points at: an anycast address of a running gateway first, else the shared address
// of the first running gateway (IPv4 before IPv6), written the way a URL wants it, with the port when it is not 443.  Empty when no
// gateway is running.  The gateway's FQDN (or its anycast FQDN) stands in for the address when one is set.
func filesLinkHost(dc *DaemonConfig) string {
	if dc == nil {
		return ""
	}
	pick := func(s string) string {
		if p, err := netip.ParsePrefix(s); err == nil {
			s = p.Addr().String()
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return ""
		}
		return a.Unmap().String()
	}
	var anycast, vip string
	name := func(n string, fallback string) string {
		if n = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(n)), "."); n != "" {
			return n
		}
		return fallback
	}
	for i := range dc.Groups {
		g := &dc.Groups[i]
		if g.Paused || g.PausedAll {
			continue
		}
		if anycast == "" {
			paused := g.pausedAnycast()
			for _, a := range g.ExtraVIPs {
				if _, p := paused[a]; !p {
					if s := pick(a); s != "" {
						anycast = name(g.AnycastFQDN, s)
						break
					}
				}
			}
		}
		if vip == "" {
			if vip = pick(g.VIP4); vip == "" {
				vip = pick(g.VIP6)
			}
			if vip != "" {
				vip = name(g.FQDN, vip)
			}
		}
	}
	h := anycast
	if h == "" {
		h = vip
	}
	if h == "" {
		return ""
	}
	port := strconv.Itoa(dc.Settings.mcpPort())
	if port == "443" {
		if strings.Contains(h, ":") {
			return "[" + h + "]"
		}
		return h
	}
	return net.JoinHostPort(h, port)
}

// slowLog notes a request of the files site that took over a second (transfers, which take as long as the file, are left out), so a
// slow site can be told from a slow network: the line says which call, and how long this node spent on it.
func slowLog(h http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		t0 := time.Now()
		h.ServeHTTP(rw, r)
		if d := time.Since(t0); d > time.Second && !strings.Contains(r.URL.Path, "/download") && !strings.Contains(r.URL.Path, "/zip") {
			warnf("web: files: %s %s took %s on this node", r.Method, r.URL.Path, d.Round(time.Millisecond))
		}
	})
}
