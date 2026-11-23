package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"filedir/filesync"
)

// fdPeerView is what one node knows about syncing with another.
type fdPeerView struct {
	Node  string `json:"node"`
	Name  string `json:"name,omitempty"` // the host name of that node
	At    int64  `json:"at,omitempty"`   // when it last asked that node (unix seconds)
	Err   string `json:"error,omitempty"`
	Moved int    `json:"moved,omitempty"`
}

type fdFolderStatus struct {
	ID       string              `json:"id"`
	Path     string              `json:"path"`
	Paused   bool                `json:"paused,omitempty"`
	Files    int                 `json:"files"`
	Dirs     int                 `json:"dirs"`
	Links    int                 `json:"links"`
	Bytes    int64               `json:"bytes"`
	Deleted  int                 `json:"deleted"` // tombstones
	Conflict int                 `json:"conflicts"`
	Scanned  int64               `json:"scanned,omitempty"`
	Error    string              `json:"error,omitempty"`
	Peers    []fdPeerView        `json:"peers,omitempty"`
	Active   []filesync.Transfer `json:"active,omitempty"` // large files being pulled now
}

type fdNodeStatus struct {
	Node    string           `json:"node"`
	Name    string           `json:"name,omitempty"` // the host name of the node
	Addr    string           `json:"addr,omitempty"`
	Folders []fdFolderStatus `json:"folders"`
}

// Status describes this node's folders.
func (s *fdService) Status() fdNodeStatus {
	st := fdNodeStatus{Node: s.selfID(), Folders: []fdFolderStatus{}}
	st.Name = byName(s.nodeNames(), st.Node)
	if s.mg != nil && s.mg.cl != nil {
		st.Addr = s.mg.cl.node.Self().Addr
	}
	seen := map[string]bool{}
	names := s.nodeNames()
	for _, f := range s.list() {
		seen[f.cfg.ID] = true
		fs := fdFolderStatus{ID: f.cfg.ID, Path: f.cfg.Path}
		f.mu.Lock()
		if !f.scanned.IsZero() {
			fs.Scanned = f.scanned.Unix()
		}
		fs.Error = f.scanErr
		for id, p := range f.peers {
			pv := fdPeerView{Node: id, Name: byName(names, id), At: p.At.Unix(), Err: p.Err, Moved: p.Moved}
			fs.Peers = append(fs.Peers, pv)
		}
		f.mu.Unlock()
		sort.Slice(fs.Peers, func(i, j int) bool { return fs.Peers[i].Node < fs.Peers[j].Node })
		if f.f != nil {
			fs.Active = f.f.Transfers()
			for _, e := range f.f.Entries() {
				switch {
				case e.Deleted:
					fs.Deleted++
					continue
				case e.Kind == filesync.KDir:
					fs.Dirs++
				case e.Kind == filesync.KSymlink:
					fs.Links++
				default:
					fs.Files++
					fs.Bytes += e.Size
				}
				if strings.Contains(e.Name, ".sync-conflict-") {
					fs.Conflict++
				}
			}
		}
		st.Folders = append(st.Folders, fs)
	}
	for _, fc := range s.config().Folders {
		if !seen[fc.ID] {
			st.Folders = append(st.Folders, fdFolderStatus{ID: fc.ID, Path: fc.Path, Paused: fc.Paused})
		}
	}
	sort.Slice(st.Folders, func(i, j int) bool { return st.Folders[i].ID < st.Folders[j].ID })
	return st
}

// AllStatus asks every node.  A node that does not answer is named, not fatal.
func (s *fdService) AllStatus(ctx context.Context) ([]fdNodeStatus, []NodeError) {
	out := []fdNodeStatus{s.Status()}
	var errs []NodeError
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range s.otherPeers() {
		wg.Add(1)
		go func(p ClusterPeer) {
			defer wg.Done()
			var st fdNodeStatus
			err := s.mg.cl.callFiles(ctx, p, "/cluster/files/status", []byte("{}"), func(rd io.Reader) error { return json.NewDecoder(rd).Decode(&st) }, 15*time.Second)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, NodeError{p.Addr, err.Error()})
				return
			}
			out = append(out, st)
		}(p)
	}
	wg.Wait()
	names := s.nodeNames()
	for i := range out {
		if out[i].Name == "" {
			out[i].Name = byName(names, out[i].Node)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	sort.Slice(errs, func(i, j int) bool { return errs[i].Node < errs[j].Node })
	return out, errs
}

func (c *Cluster) handleFilesStatus(rw http.ResponseWriter, r *http.Request, caller ClusterPeer, body []byte) {
	if c.files == nil {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	b, _ := json.Marshal(c.files.Status())
	rw.Header().Set("Content-Type", "application/json")
	rw.Write(b)
}

// NodeError says which node could not answer.
type NodeError struct {
	Node  string `json:"node"`
	Error string `json:"error"`
}

// FileView is one entry as the API shows it.
type FileView struct {
	Name    string `json:"name"` // the last part
	Path    string `json:"path"` // from the folder's root
	Kind    string `json:"kind"` // file, dir or link
	Size    int64  `json:"size,omitempty"`
	Mod     int64  `json:"mod,omitempty"` // unix seconds
	Target  string `json:"target,omitempty"`
	By      string `json:"by,omitempty"`
	Conflct bool   `json:"conflict,omitempty"`
	Mount   string `json:"mount,omitempty"`    // a synced folder shown inside the root folder: its name (the entry opens that folder)
	Subdirs bool   `json:"has_dirs,omitempty"` // a folder that has folders in it (the tree shows an arrow for it only then)
}

// dirsWithDirs is the set of paths ("" is the folder itself) that have a folder directly in them.
func dirsWithDirs(f *fdFolder) map[string]bool {
	out := map[string]bool{}
	if f == nil || f.f == nil {
		return out
	}
	for _, e := range f.f.Entries() {
		if e.Deleted || e.Kind != filesync.KDir {
			continue
		}
		d := path.Dir(e.Name)
		if d == "." {
			d = ""
		}
		out[d] = true
	}
	return out
}

// List returns the entries directly under a path of a folder, folders first.  It answers from this node's index.
func (s *fdService) List(folder, dir string, limit int, dirsOnly ...bool) ([]FileView, int, bool) {
	onlyDirs := len(dirsOnly) > 0 && dirsOnly[0]
	f := s.get(folder)
	if f == nil || f.f == nil {
		return nil, 0, false
	}
	dir = strings.Trim(path.Clean("/"+dir), "/")
	prefix := ""
	if dir != "" {
		prefix = dir + "/"
	}
	var out []FileView
	mounts := map[string]bool{} // the root folder shows the other synced folders at its top; one of them hides a real entry of the same name
	if folder == fdRootID && dir == "" {
		for _, o := range s.list() {
			if o.cfg.ID != fdRootID && o.f != nil {
				mounts[o.cfg.ID] = true
				out = append(out, FileView{Name: o.cfg.ID, Path: o.cfg.ID, Kind: "dir", Mount: o.cfg.ID})
			}
		}
	}
	for _, e := range f.f.Entries() {
		if e.Deleted || !strings.HasPrefix(e.Name, prefix) || strings.Contains(e.Name[len(prefix):], "/") || e.Name == dir || mounts[e.Name] {
			continue
		}
		if onlyDirs && e.Kind != filesync.KDir {
			continue
		}
		v := FileView{Name: e.Name[len(prefix):], Path: e.Name, Size: e.Size, Target: e.Target, By: string(e.By), Conflct: strings.Contains(e.Name, ".sync-conflict-")}
		switch e.Kind {
		case filesync.KDir:
			v.Kind = "dir"
		case filesync.KSymlink:
			v.Kind = "link"
		default:
			v.Kind = "file"
		}
		if e.ModNs != 0 {
			v.Mod = e.ModNs / 1e9
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Kind == "dir") != (out[j].Kind == "dir") {
			return out[i].Kind == "dir"
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	var hasDirs map[string]bool
	for i := range out {
		if out[i].Kind != "dir" {
			continue
		}
		if out[i].Mount != "" {
			out[i].Subdirs = dirsWithDirs(s.get(out[i].Mount))[""]
			continue
		}
		if hasDirs == nil {
			hasDirs = dirsWithDirs(f)
		}
		out[i].Subdirs = hasDirs[out[i].Path]
	}
	total := len(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, total, true
}

func (w *WebServer) handleFilesStatus(rw http.ResponseWriter, r *http.Request, s *session) {
	if fdSvc == nil {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "file sync is not running"})
		return
	}
	if r.URL.Query().Get("local") == "1" {
		writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"nodes": []fdNodeStatus{fdSvc.Status()}, "link_host": filesLinkHost(w.status.sup.config())}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	st, errs := fdSvc.AllStatus(ctx)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"nodes": st, "node_errors": errs, "link_host": filesLinkHost(w.status.sup.config())}})
}

// FileHit is one match of a search: a FileView and the folder it is in.
type FileHit struct {
	FileView
	Folder string `json:"folder"`
}

// Search finds the files and folders of every synced folder whose path (inside its folder) holds every word of q, ignoring case.  It
// answers from this node's index, like List.  Folders come in the order of their names, matches within one by path.  total is how many
// matched, out holds at most limit of them.  With a scope (a folder id, and a path in it) only that folder is searched, and only what is
// under that path, and the words are matched against the part of the path below it.
func (s *fdService) Search(q string, limit int, scope ...string) (out []FileHit, total int) {
	words := strings.Fields(strings.ToLower(q))
	if len(words) == 0 {
		return nil, 0
	}
	only, under := "", ""
	if len(scope) > 0 && scope[0] != "" {
		only = scope[0]
		if len(scope) > 1 {
			if d := strings.Trim(path.Clean("/"+scope[1]), "/"); d != "" {
				under = d + "/"
			}
		}
	}
	for _, f := range s.list() {
		if f.f == nil || only != "" && f.cfg.ID != only {
			continue
		}
		var part []FileHit
		for _, e := range f.f.Entries() {
			if e.Deleted || e.Name == "" || !strings.HasPrefix(e.Name, under) || e.Name == strings.TrimSuffix(under, "/") {
				continue
			}
			low := strings.ToLower(e.Name[len(under):])
			ok := true
			for _, w := range words {
				if !strings.Contains(low, w) {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			v := FileView{Name: e.Name[strings.LastIndexByte(e.Name, '/')+1:], Path: e.Name, Size: e.Size, Target: e.Target, By: string(e.By), Conflct: strings.Contains(e.Name, ".sync-conflict-")}
			switch e.Kind {
			case filesync.KDir:
				v.Kind = "dir"
			case filesync.KSymlink:
				v.Kind = "link"
			default:
				v.Kind = "file"
			}
			if e.ModNs != 0 {
				v.Mod = e.ModNs / 1e9
			}
			part = append(part, FileHit{v, f.cfg.ID})
		}
		sort.Slice(part, func(i, j int) bool { return strings.ToLower(part[i].Path) < strings.ToLower(part[j].Path) })
		total += len(part)
		for _, h := range part {
			if limit <= 0 || len(out) < limit {
				out = append(out, h)
			}
		}
	}
	return out, total
}

func (w *WebServer) handleFilesSearch(rw http.ResponseWriter, r *http.Request, s *session) {
	if fdSvc == nil {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "file sync is not running"})
		return
	}
	q := r.URL.Query()
	if len(q.Get("q")) > 200 {
		writeJSON(rw, http.StatusBadRequest, map[string]any{"ok": false, "error": "the filter is too long"})
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	list, total := fdSvc.Search(q.Get("q"), limit, q.Get("folder"), q.Get("path"))
	if s.site == siteFiles { // which node changed a file is the cluster's business
		for i := range list {
			list[i].By = ""
		}
	} else {
		names := fdSvc.nodeNames()
		for i := range list {
			list[i].By = byName(names, list[i].By)
		}
	}
	if list == nil {
		list = []FileHit{}
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"entries": list, "total": total, "truncated": total > len(list)}})
}

func (w *WebServer) handleFilesList(rw http.ResponseWriter, r *http.Request, s *session) {
	if fdSvc == nil {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "file sync is not running"})
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	list, total, ok := fdSvc.List(q.Get("folder"), q.Get("path"), limit, q.Get("dirs") == "1")
	if s.site == siteFiles { // which node changed a file is the cluster's business
		for i := range list {
			list[i].By = ""
		}
	} else {
		names := fdSvc.nodeNames()
		for i := range list {
			list[i].By = byName(names, list[i].By)
		}
	}
	if !ok {
		writeJSON(rw, http.StatusNotFound, map[string]any{"ok": false, "error": "no such folder on this node"})
		return
	}
	if list == nil {
		list = []FileView{}
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"entries": list, "total": total, "truncated": total > len(list)}})
}

// filesMCPTools are the file tools, added to the list in mcp.go's init.
// mcpFileChunk is how much of a file files_read sends by default (raw bytes; base64 makes it a third larger).
const mcpFileChunk = 512 << 10

// fdErrText says what went wrong in words an MCP client can act on.
func fdErrText(err error) string {
	switch {
	case errors.Is(err, filesync.ErrBadName):
		return "that name is not allowed"
	case errors.Is(err, filesync.ErrNotFound):
		return "not found"
	case errors.Is(err, filesync.ErrIsDir):
		return "that is a folder"
	case errors.Is(err, filesync.ErrExists):
		return "that name is already there"
	case errors.Is(err, filesync.ErrTooBig):
		return "the file is larger than this folder allows"
	case errors.Is(err, filesync.ErrBadOffset):
		return "offset is not where the file so far ends (it must be the number of bytes already sent; start again with offset 0)"
	case errors.Is(err, filesync.ErrNoMarker):
		return "the folder is not there (is its disk mounted?)"
	}
	warnf("files: %v", err)
	return "that did not work"
}

func mcpFilesRead(c *mcpCtx) (any, error) {
	if fdSvc == nil {
		return nil, errors.New("file sync is not running")
	}
	folder, p := c.str("folder"), c.str("path")
	f := fdSvc.get(folder)
	if f == nil || f.f == nil {
		return nil, errors.New("no such folder on this node")
	}
	fh, st, err := f.f.OpenRead(p)
	if err != nil {
		return nil, errors.New(fdErrText(err))
	}
	defer fh.Close()
	size := st.Size()
	off := int64(c.num("offset", 0))
	if off < 0 || off > size {
		return nil, errors.New("offset is beyond the end of the file")
	}
	max := int64(c.w.status.sup.mcpResultLimit()/2) - 4096 // base64 and the JSON around it must fit the result limit
	n := int64(mcpFileChunk)
	if l := int64(c.num("length", 0)); l > 0 {
		n = l
	}
	n = min(n, max, size-off)
	buf := make([]byte, n)
	if got, err := fh.ReadAt(buf, off); err != nil && !(err == io.EOF && int64(got) == n) {
		return nil, errors.New("the file could not be read")
	}
	enc := c.str("encoding")
	if enc != "text" && enc != "base64" { // auto: text only when this is all of a text file
		enc = "base64"
		if off == 0 && n == size && utf8.Valid(buf) && bytes.IndexByte(buf, 0) < 0 {
			enc = "text"
		}
	}
	out := map[string]any{"folder": folder, "path": p, "size": size, "mod": st.ModTime().Unix(), "offset": off, "length": n, "eof": off+n >= size, "encoding": enc}
	if enc == "text" {
		if !utf8.Valid(buf) {
			return nil, errors.New("this part of the file is not valid UTF-8 text: use encoding base64")
		}
		out["content"] = string(buf)
	} else {
		out["content"] = base64.StdEncoding.EncodeToString(buf)
	}
	return out, nil
}

func mcpFilesWrite(c *mcpCtx) (any, error) {
	if fdSvc == nil {
		return nil, errors.New("file sync is not running")
	}
	folder, p := c.str("folder"), c.str("path")
	f := fdSvc.get(folder)
	if f == nil || f.f == nil {
		return nil, errors.New("no such folder on this node")
	}
	content, _ := c.args["content"].(string) // as given: not trimmed
	var data []byte
	if c.str("encoding") == "base64" {
		b, err := base64.StdEncoding.DecodeString(strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' || r == ' ' {
				return -1
			}
			return r
		}, content))
		if err != nil {
			return nil, errors.New("content is not valid base64")
		}
		data = b
	} else {
		data = []byte(content)
	}
	off := int64(c.num("offset", 0))
	more, _ := c.flag("more")
	if off < 0 {
		return nil, errors.New("offset cannot be negative")
	}
	var size int64
	var err error
	if off == 0 && !more {
		size, err = f.f.WriteFile(p, bytes.NewReader(data))
	} else {
		if size, err = f.f.StageAppend(p, off, data); err == nil && !more {
			size, err = f.f.StageCommit(p)
		}
	}
	if err != nil {
		return nil, errors.New(fdErrText(err))
	}
	if !more {
		f.rescan()
		fdAudit(c.s, "files: %s: wrote %s (%d bytes) over MCP", f.cfg.ID, p, size)
	}
	return map[string]any{"folder": folder, "path": p, "size": size, "complete": !more}, nil
}

func filesMCPTools() []mcpTool {
	return []mcpTool{
		{name: "files_read", title: "Read a file",
			desc:   "Download a file of a synced folder. The answer has size, mod (unix seconds), offset, length, eof, encoding and content. A text file that fits in one answer comes back as text; anything else as base64 (encoding \"base64\"), in parts: ask again with offset set to offset+length until eof is true. length is at most about half the result limit; the default is 512 KB. Where the privacy level hides things (addresses, e-mail addresses), they are hidden in text content too: use encoding base64 to get the bytes exactly.",
			noNode: true,
			props: []mcpProp{
				{name: "folder", typ: "string", desc: "The folder's name (see files_status).", required: true},
				{name: "path", typ: "string", desc: "The file's path inside it, no leading slash (see files_list).", required: true},
				{name: "offset", typ: "integer", desc: "Where to start, in bytes. Default 0."},
				{name: "length", typ: "integer", desc: "How many bytes at most. Default 512 KB."},
				{name: "encoding", typ: "string", enum: []string{"auto", "text", "base64"}, desc: "auto (default): text for a whole text file, else base64."},
			},
			run: mcpFilesRead},
		{name: "files_download_url", title: "Link to download a file",
			desc:   "Make a short-lived link that downloads one file of a synced folder with a plain HTTP GET, so a large file does not have to pass through this conversation as base64 text. Run curl on the link from a machine that can reach the gateway address. The answer has url, expires (unix seconds) and how. The link opens only that file and stops working after the given minutes (default 15, at most 1440).",
			noNode: true,
			props: []mcpProp{
				{name: "folder", typ: "string", desc: "The folder's name (see files_status).", required: true},
				{name: "path", typ: "string", desc: "The file's path inside it, no leading slash (see files_list).", required: true},
				{name: "minutes", typ: "integer", desc: "How long the link works. Default 15, at most 1440."},
			},
			run: func(c *mcpCtx) (any, error) { return mcpFilesLink(c, false) }},
		{name: "files_upload_url", title: "Link to upload a file",
			desc:   "Make a short-lived link that uploads one file into a synced folder with a plain HTTP PUT, so a large file does not have to pass through this conversation as base64 text. Run curl -T LOCALFILE on the link from a machine that can reach the gateway address; the folders on the way are made and a file that is there is replaced (the old one goes to the folder's trash for 30 days). The answer has url, expires (unix seconds) and how. The link writes only that path and stops working after the given minutes (default 15, at most 1440). Treat the link as a password until then.",
			noNode: true, write: true, destructive: true,
			props: []mcpProp{
				{name: "folder", typ: "string", desc: "The folder's name (see files_status).", required: true},
				{name: "path", typ: "string", desc: "The file's path inside it, no leading slash.", required: true},
				{name: "minutes", typ: "integer", desc: "How long the link works. Default 15, at most 1440."},
			},
			run: func(c *mcpCtx) (any, error) { return mcpFilesLink(c, true) }},
		{name: "files_write", title: "Write a file",
			desc:   "Upload a file into a synced folder, making the folders on the way. A file that is there is replaced (the old one goes to the folder's trash for 30 days); the other nodes get it within seconds. One call carries up to about 700 KB of data (the request is limited to 1 MB). For a larger file send it in parts: the first with offset 0 and more true, each next with offset set to the bytes sent so far and more true, the last with more false; nothing appears in the folder until the last part arrives. content is text, or base64 with encoding base64 (use it for anything that is not plain text).",
			noNode: true, write: true, destructive: true,
			props: []mcpProp{
				{name: "folder", typ: "string", desc: "The folder's name (see files_status).", required: true},
				{name: "path", typ: "string", desc: "The file's path inside it, no leading slash.", required: true},
				{name: "content", typ: "string", desc: "The file's contents (or this part of them).", required: true},
				{name: "encoding", typ: "string", enum: []string{"text", "base64"}, desc: "text (default) or base64."},
				{name: "offset", typ: "integer", desc: "For a file sent in parts: the bytes sent so far. Default 0."},
				{name: "more", typ: "boolean", desc: "True when more parts follow. Default false."},
			},
			run: mcpFilesWrite},
		{name: "files_mkdir", title: "Make a folder",
			desc:   "Make a folder inside a synced folder (the folders on the way too). Nothing happens if it is already there.",
			noNode: true, write: true,
			props: []mcpProp{
				{name: "folder", typ: "string", desc: "The synced folder's name (see files_status).", required: true},
				{name: "path", typ: "string", desc: "The new folder's path inside it, no leading slash.", required: true},
			},
			run: mcpFilesMkdir},
		{name: "files_delete", title: "Delete a file or folder",
			desc:   "Delete a file, or a folder with everything in it. It is not gone for good: it goes to the synced folder's trash on the node that answers (30 days by default) and the other nodes drop it within seconds.",
			noNode: true, write: true, destructive: true,
			props: []mcpProp{
				{name: "folder", typ: "string", desc: "The synced folder's name (see files_status).", required: true},
				{name: "path", typ: "string", desc: "The file's or folder's path inside it, no leading slash.", required: true},
			},
			run: mcpFilesDelete},
		{name: "files_move", title: "Move or rename a file or folder",
			desc:   "Move or rename a file or a folder tree, inside a synced folder or into another one (to_folder). The folder it goes into must exist (files_mkdir) and the name must be free, unless unique is true, which picks a free name (\"name copy.ext\").",
			noNode: true, write: true, destructive: true,
			props: []mcpProp{
				{name: "folder", typ: "string", desc: "The synced folder it is in (see files_status).", required: true},
				{name: "path", typ: "string", desc: "What to move, inside it.", required: true},
				{name: "to", typ: "string", desc: "Where it goes: the whole new path, including its name.", required: true},
				{name: "to_folder", typ: "string", desc: "Another synced folder to move it into. Default: the same one."},
				{name: "unique", typ: "boolean", desc: "Pick a free name when the new one is taken. Default false."},
			},
			run: func(c *mcpCtx) (any, error) { return mcpFilesCopyMove(c, true) }},
		{name: "files_copy", title: "Copy a file or folder",
			desc:   "Copy a file or a folder tree, inside a synced folder or into another one (to_folder). The folder it goes into must exist (files_mkdir) and the name must be free, unless unique is true, which picks a free name (\"name copy.ext\").",
			noNode: true, write: true,
			props: []mcpProp{
				{name: "folder", typ: "string", desc: "The synced folder it is in (see files_status).", required: true},
				{name: "path", typ: "string", desc: "What to copy, inside it.", required: true},
				{name: "to", typ: "string", desc: "Where the copy goes: the whole new path, including its name.", required: true},
				{name: "to_folder", typ: "string", desc: "Another synced folder to copy it into. Default: the same one."},
				{name: "unique", typ: "boolean", desc: "Pick a free name when the new one is taken. Default false."},
			},
			run: func(c *mcpCtx) (any, error) { return mcpFilesCopyMove(c, false) }},
		{name: "files_trash", title: "List the trash",
			desc:   "What the trash of a synced folder holds on the node that answers (each node keeps its own): the size, and one level of entries. Without when the answer lists the deletions by time; give when (and path to go into a folder) to list what is in one.",
			noNode: true,
			props: []mcpProp{
				{name: "folder", typ: "string", desc: "The synced folder's name (see files_status).", required: true},
				{name: "when", typ: "string", desc: "A deletion's time, as the listing gave it."},
				{name: "path", typ: "string", desc: "A folder inside that deletion."},
			},
			run: mcpFilesTrash},
		{name: "files_trash_restore", title: "Restore from the trash",
			desc:   "Put a file or folder of the trash back where it was deleted from. when and name are as files_trash lists them.",
			noNode: true, write: true,
			props: []mcpProp{
				{name: "folder", typ: "string", desc: "The synced folder's name.", required: true},
				{name: "when", typ: "string", desc: "The deletion's time, from files_trash.", required: true},
				{name: "name", typ: "string", desc: "The entry's name, from files_trash.", required: true},
			},
			run: mcpFilesTrashRestore},
		{name: "files_trash_empty", title: "Empty the trash",
			desc:   "Remove everything in a synced folder's trash FOR GOOD on the node that answers; it cannot be got back. Each node keeps its own trash. Needs confirm: true.",
			noNode: true, write: true, destructive: true,
			props: []mcpProp{
				{name: "folder", typ: "string", desc: "The synced folder's name.", required: true},
				{name: "confirm", typ: "boolean", desc: "Must be true.", required: true},
			},
			run: mcpFilesTrashEmpty},
		{name: "files_status", title: "File sync status",
			desc:   "The state of file sync on every node: each folder's path, how many files, folders, links and bytes it holds, how many deletions are remembered, how many conflict copies exist, when it was last scanned, any error, and when and how much it last synced with each other node.",
			noNode: true,
			run: func(c *mcpCtx) (any, error) {
				b, err := c.get("/api/files/status")
				return asAny(b), err
			}},
		{name: "files_list", title: "List files",
			desc:   "The files and folders directly under a path of a synced folder, from the node that answers (every node holds the same files once synced; files_status shows whether they are caught up). Folders first, then by name. Each entry has name, path, kind (file, dir, link), size, modification time (unix seconds), the node that last changed it, and whether it is a conflict copy.",
			noNode: true,
			props: []mcpProp{
				{name: "folder", typ: "string", desc: "The folder's name (see files_status).", required: true},
				{name: "path", typ: "string", desc: "A path inside it, no leading slash. Absent: the top."},
				{name: "limit", typ: "integer", desc: "How many entries, default 1000, at most 5000."},
			},
			run: func(c *mcpCtx) (any, error) {
				v := url.Values{}
				for _, k := range []string{"folder", "path", "limit"} {
					if s := c.str(k); s != "" {
						v.Set(k, s)
					}
				}
				b, err := c.get("/api/files/list?" + v.Encode())
				return asAny(b), err
			}},
	}
}

// nodeNames maps the ids of the cluster's nodes to their host names (what a person knows them by).
func (s *fdService) nodeNames() map[string]string {
	m := map[string]string{}
	if s.mg != nil && s.mg.cl != nil && s.mg.cl.Enabled() {
		for _, p := range s.mg.cl.View().Peers {
			if p.NodeID != "" && p.Hostname != "" {
				short, _, _ := strings.Cut(p.Hostname, ".")
				m[p.NodeID] = short
			}
		}
	}
	if id := s.selfID(); id != "" && m[id] == "" {
		short, _, _ := strings.Cut(selfHost(), ".")
		m[id] = short
	}
	return m
}

// byName is the name of the node an entry was last changed by, or its id when that node is not known any more.
func byName(names map[string]string, id string) string {
	if n := names[id]; n != "" {
		return n
	}
	return id
}
