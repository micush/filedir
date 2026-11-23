package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
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

func (w *WebServer) pubLinkKey() []byte {
	secret := w.mg.cl.node.Secret()
	if len(secret) == 0 {
		return nil
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(pubLinkContext))
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
