package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"time"
)

// Public download links: "Copy public link" on the Files page makes a link that downloads one file with no sign-in, for a few days.
// The link carries what it opens (folder, path, expiry) and a signature made with a key derived from the cluster secret, so every node
// of the cluster (and the anycast address, whichever node answers) accepts the same link, and nothing is stored.  Anyone who has the
// link can download that one file until it runs out; it cannot be used for anything else.  A link cannot be taken back before it
// expires except by replacing the cluster secret, so the choices are short: 1, 7 or 30 days.

const (
	pubLinkMaxDays = 30
	pubLinkContext = "filedir files public link v1"
)

type pubLink struct {
	Folder string `json:"f"`
	Path   string `json:"p"`
	Exp    int64  `json:"e"`
}

func (w *WebServer) pubLinkKey() []byte { return w.linkKey(pubLinkContext) }

// linkKey is the signing key for one kind of link.  Download and upload links use different contexts, so a token of one kind is never
// valid as the other, and a node of an older version, which knows only the download context, refuses an upload link.
func (w *WebServer) linkKey(context string) []byte {
	secret := w.mg.cl.node.Secret()
	if len(secret) == 0 {
		return nil
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(context))
	return m.Sum(nil)
}

func pubLinkSign(key []byte, l pubLink) string {
	b, _ := json.Marshal(l)
	body := base64.RawURLEncoding.EncodeToString(b)
	m := hmac.New(sha256.New, key)
	m.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// pubLinkOpen checks a token's signature and expiry and returns what it opens.
func pubLinkOpen(key []byte, token string, now time.Time) (pubLink, bool) {
	var l pubLink
	i := -1
	for j := len(token) - 1; j >= 0; j-- {
		if token[j] == '.' {
			i = j
			break
		}
	}
	if i < 0 || len(key) == 0 {
		return l, false
	}
	body, sig := token[:i], token[i+1:]
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return l, false
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(body))
	if !hmac.Equal(got, m.Sum(nil)) {
		return l, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || json.Unmarshal(raw, &l) != nil || now.Unix() > l.Exp {
		return pubLink{}, false
	}
	return l, true
}

// handleFilesShare makes a public link to one file.  Only an account that may change files can: a read-only account is refused by the
// read-only rule for POST, as for every change.
func (w *WebServer) handleFilesShare(rw http.ResponseWriter, r *http.Request, s *session) {
	var in struct {
		Folder, Path string
		Days         int
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in) != nil {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	if in.Days < 1 || in.Days > pubLinkMaxDays {
		jsonError(rw, http.StatusBadRequest, "a public link lasts 1 to 30 days")
		return
	}
	key := w.pubLinkKey()
	if key == nil {
		jsonError(rw, http.StatusServiceUnavailable, "no key to sign a public link with on this node")
		return
	}
	f := w.fdFolderOf(rw, in.Folder)
	if f == nil {
		return
	}
	fh, st, err := f.f.OpenRead(in.Path)
	if err != nil {
		fdFail(rw, err)
		return
	}
	fh.Close()
	exp := time.Now().Add(time.Duration(in.Days) * 24 * time.Hour)
	tok := pubLinkSign(key, pubLink{Folder: in.Folder, Path: in.Path, Exp: exp.Unix()})
	fdAudit(s, "files: %s: made a public link to %s, valid %d day(s)", f.cfg.ID, in.Path, in.Days)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "url": "/dl/" + tok + "/" + url.PathEscape(st.Name()), "expires": exp.Unix()})
}

// handlePublicDownload serves the one file a valid link names, with no session.  Nothing about the cause of a refusal is given.
func (w *WebServer) handlePublicDownload(rw http.ResponseWriter, r *http.Request) {
	l, ok := pubLinkOpen(w.pubLinkKey(), r.PathValue("token"), time.Now())
	if !ok {
		http.Error(rw, "This link is not valid, or it has expired.", http.StatusNotFound)
		return
	}
	rw.Header().Set("Cache-Control", "no-store")
	rw.Header().Set("Referrer-Policy", "no-referrer")
	infof("files: public link used for %s:%s (from %s)", l.Folder, l.Path, clientIP(r))
	q := r.URL.Query()
	q.Set("folder", l.Folder)
	q.Set("path", l.Path)
	r2 := r.Clone(r.Context())
	r2.URL.RawQuery = q.Encode()
	w.handleFilesDownload(rw, r2, nil)
}

// Transfer links for MCP.  files_download_url and files_upload_url hand an assistant a link that moves one file with plain HTTP, so the
// bytes never pass through the conversation: whatever has a shell and can reach the gateway runs curl on it.  A download link is the
// public link above, just short (minutes); an upload link names the one file it may write, and is signed with a key of its own.  Both
// are stateless like the public link, so whichever node the gateway address reaches accepts them, and both run out; neither can be
// used for anything else.  An upload link can be used again until it runs out, and each use replaces that one file (the old one goes
// to the trash, as with files_write).
const (
	pubUpContext   = "filedir files upload link v1"
	linkMaxMinutes = 24 * 60
	linkDefMinutes = 15
)

type mcpHostKey struct{}

// handleLinkUpload takes the body of a PUT or POST as the file an upload link names.
func (w *WebServer) handleLinkUpload(rw http.ResponseWriter, r *http.Request) {
	l, ok := pubLinkOpen(w.linkKey(pubUpContext), r.PathValue("token"), time.Now())
	if !ok {
		http.Error(rw, "This link is not valid, or it has expired.", http.StatusNotFound)
		return
	}
	f := w.fdFolderOf(rw, l.Folder)
	if f == nil {
		return
	}
	fdNoDeadlines(rw)
	n, err := f.f.WriteFile(l.Path, fdUploadFrom(r))
	if err != nil {
		fdFail(rw, err)
		return
	}
	f.rescan()
	infof("files: %s: wrote %s (%d bytes) through an upload link (from %s)", f.cfg.ID, l.Path, n, clientIP(r))
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"size": n}})
}

// mcpFilesLink makes the link for files_download_url (upload false) or files_upload_url (upload true).
func mcpFilesLink(c *mcpCtx, upload bool) (any, error) {
	if fdSvc == nil {
		return nil, errors.New("file sync is not running")
	}
	folder, p := c.str("folder"), c.str("path")
	f := fdSvc.get(folder)
	if f == nil || f.f == nil {
		return nil, errors.New("no such folder on this node")
	}
	if p == "" {
		return nil, errors.New("path is required")
	}
	mins := c.num("minutes", linkDefMinutes)
	if mins < 1 || mins > linkMaxMinutes {
		return nil, errors.New("minutes is 1 to 1440")
	}
	host, _ := c.ctx.Value(mcpHostKey{}).(string)
	if host == "" {
		return nil, errors.New("cannot tell which address this call came to")
	}
	exp := time.Now().Add(time.Duration(mins) * time.Minute)
	l := pubLink{Folder: folder, Path: p, Exp: exp.Unix()}
	var key []byte
	var prefix string
	if upload {
		key, prefix = c.w.linkKey(pubUpContext), "/up/"
	} else {
		fh, _, err := f.f.OpenRead(p)
		if err != nil {
			return nil, errors.New(fdErrText(err))
		}
		fh.Close()
		key, prefix = c.w.pubLinkKey(), "/dl/"
	}
	if key == nil {
		return nil, errors.New("no key to sign a link with on this node")
	}
	u := "https://" + host + prefix + pubLinkSign(key, l) + "/" + url.PathEscape(path.Base(p))
	fdAudit(c.s, "files: %s: made a %s link for %s, valid %d minute(s)", f.cfg.ID, map[bool]string{true: "upload", false: "download"}[upload], p, mins)
	out := map[string]any{"folder": folder, "path": p, "url": u, "expires": exp.Unix(), "minutes": mins}
	if upload {
		out["how"] = "Send the file's bytes as the body of a PUT (or POST) to url, from a machine that can reach this address, for example: curl -T LOCALFILE URL. The answer is JSON with the size. The link writes this one file, replacing what is there, and stops working at expires."
	} else {
		out["how"] = "GET url from a machine that can reach this address, for example: curl -o LOCALFILE URL. The link opens this one file and stops working at expires."
	}
	return out, nil
}
