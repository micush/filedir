package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"filedir/filesync"
)

// The file manager's calls.  They act on this node's copy of the folder; the next scan gives the change a version and the other nodes
// take it.  Every call is a POST except reading, so the read-only account is refused the others by the same rule as everywhere else.

const fdTextMax = 1 << 20 // the largest file the editor opens

func (w *WebServer) fdFolderOf(rw http.ResponseWriter, id string) *fdFolder {
	if fdSvc == nil {
		writeJSON(rw, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "file sync is not running"})
		return nil
	}
	f := fdSvc.get(id)
	if f == nil || f.f == nil {
		writeJSON(rw, http.StatusNotFound, map[string]any{"ok": false, "error": "no such folder on this node"})
		return nil
	}
	return f
}

func fdFail(rw http.ResponseWriter, err error) {
	code, msg := http.StatusInternalServerError, "that did not work"
	switch {
	case errors.Is(err, filesync.ErrBadName):
		code, msg = http.StatusBadRequest, "that name is not allowed"
	case errors.Is(err, filesync.ErrNotFound):
		code, msg = http.StatusNotFound, "not found"
	case errors.Is(err, filesync.ErrExists):
		code, msg = http.StatusConflict, "that name is already there"
	case errors.Is(err, filesync.ErrIsDir):
		code, msg = http.StatusConflict, "that is a folder"
	case errors.Is(err, filesync.ErrTooBig):
		code, msg = http.StatusRequestEntityTooLarge, "the file is larger than this folder allows"
	case errors.Is(err, filesync.ErrNoMarker):
		code, msg = http.StatusServiceUnavailable, "the folder is not there (is its disk mounted?)"
	case errors.Is(err, filesync.ErrDiskFull):
		code, msg = http.StatusInsufficientStorage, "the disk is full up to the limit set for files (Configure > Folders)"
	default:
		warnf("files: %v", err)
	}
	writeJSON(rw, code, map[string]any{"ok": false, "error": msg})
}

func (w *WebServer) handleFilesDownload(rw http.ResponseWriter, r *http.Request, s *session) {
	q := r.URL.Query()
	f := w.fdFolderOf(rw, q.Get("folder"))
	if f == nil {
		return
	}
	fh, st, err := f.f.OpenRead(q.Get("path"))
	if err != nil {
		fdFail(rw, err)
		return
	}
	defer fh.Close()
	fdNoDeadlines(rw)
	rw = fdDownloadTo(rw, r)
	rw.Header().Set("Content-Type", "application/octet-stream")
	rw.Header().Set("X-Content-Type-Options", "nosniff")
	rw.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(st.Name()))
	http.ServeContent(rw, r, "", st.ModTime(), fh)
}

// fdNoDeadlines lifts the server's read and write time limits for one request: a file is as long in coming or going as its size and the
// person's line make it, and the limits (a minute to read, five to write) are there for requests that are small.
func fdNoDeadlines(rw http.ResponseWriter) {
	rc := http.NewResponseController(rw)
	rc.SetReadDeadline(time.Time{})
	rc.SetWriteDeadline(time.Time{})
}

// handleFilesZip sends files and folders as one zip, written as it is read.  The names are repeated path= parameters ("" or none for the
// whole folder).  It is a GET so the browser can save it as it arrives, which a fetch into memory could not do for a large folder.
func (w *WebServer) handleFilesZip(rw http.ResponseWriter, r *http.Request, s *session) {
	q := r.URL.Query()
	f := w.fdFolderOf(rw, q.Get("folder"))
	if f == nil {
		return
	}
	names := q["path"]
	if len(names) == 0 {
		names = []string{""}
	}
	out := "download"
	switch {
	case len(names) == 1 && names[0] == "":
		out = f.cfg.ID
		if out == fdRootID {
			out = "root"
		}
	case len(names) == 1:
		out = path.Base(names[0])
	default:
		out = strings.Trim(f.cfg.ID, "/") + "-selection"
		if f.cfg.ID == fdRootID {
			out = "root-selection"
		}
	}
	// refuse before sending anything: a bad name is an error the page can show, not half an archive
	for _, n := range names {
		if n == "" {
			continue
		}
		if fh, _, err := f.f.OpenRead(n); err == nil {
			fh.Close()
		} else if !errors.Is(err, filesync.ErrIsDir) {
			fdFail(rw, err)
			return
		}
	}
	fdNoDeadlines(rw)
	h := rw.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(out+".zip"))
	st, err := f.f.WriteZip(fdDownloadTo(rw, r), names, f.cfg.ID)
	if err != nil {
		warnf("files: %s: zip for %s stopped: %v", f.cfg.ID, s.user, err) // the status line is sent: all that is left is to cut the stream
		panic(http.ErrAbortHandler)
	}
	fdAudit(s, "files: %s: downloaded %d item(s) as a zip (%d files, %d bytes, %d left out)", f.cfg.ID, len(names), st.Files, st.Bytes, st.Skipped)
}

// handleFilesText returns a small text file for the editor.
func (w *WebServer) handleFilesText(rw http.ResponseWriter, r *http.Request, s *session) {
	q := r.URL.Query()
	f := w.fdFolderOf(rw, q.Get("folder"))
	if f == nil {
		return
	}
	fh, st, err := f.f.OpenRead(q.Get("path"))
	if err != nil {
		fdFail(rw, err)
		return
	}
	defer fh.Close()
	if st.Size() > fdTextMax {
		writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"size": st.Size(), "too_big": true}})
		return
	}
	b, err := io.ReadAll(io.LimitReader(fh, fdTextMax+1))
	if err != nil {
		fdFail(rw, err)
		return
	}
	if !utf8.Valid(b) || strings.IndexByte(string(b), 0) >= 0 {
		writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"size": st.Size(), "binary": true}})
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"size": st.Size(), "text": string(b), "mod": st.ModTime().Unix()}})
}

// handleFilesUpload takes a file as the raw request body.
func (w *WebServer) handleFilesUpload(rw http.ResponseWriter, r *http.Request, s *session) {
	q := r.URL.Query()
	f := w.fdFolderOf(rw, q.Get("folder"))
	if f == nil {
		return
	}
	if f.f.Ignored(q.Get("path"), false) { // left out by the folder's ignore list: not an error, so a dropped folder goes on with the rest
		io.Copy(io.Discard, io.LimitReader(r.Body, maxUploadBytes+1<<20))
		writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"skipped": true}})
		return
	}
	fdNoDeadlines(rw)
	n, err := f.f.WriteFile(q.Get("path"), fdUploadFrom(r))
	if err != nil {
		fdFail(rw, err)
		return
	}
	f.rescan()
	fdAudit(s, "files: %s: wrote %s (%d bytes)", f.cfg.ID, q.Get("path"), n)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"size": n}})
}

// A big upload arrives in pieces, so that a connection that drops costs one piece, not the file: begin says how much is here (it finds the
// upload again by who, where, what and how big, so a page that was reloaded takes it up too), each piece starts where that ends, and
// finish puts the file in place.  The pieces wait on this node: if the next ask reaches another node, the upload starts again.
type fdUpload struct {
	id, user, folder, path string
	size                   int64
	last                   time.Time
	done                   bool // finished: asking again gets the same answer
	n                      int64
}

var (
	fdUpMu  sync.Mutex
	fdUps   = map[string]*fdUpload{} // by id
	fdUpKey = map[string]string{}    // user|folder|path|size -> id
)

func fdUpExpire() { // the caller holds fdUpMu
	for id, u := range fdUps {
		if time.Since(u.last) > 48*time.Hour || (u.done && time.Since(u.last) > 10*time.Minute) {
			delete(fdUps, id)
			delete(fdUpKey, u.user+"|"+u.folder+"|"+u.path+"|"+strconv.FormatInt(u.size, 10))
		}
	}
}

func (w *WebServer) handleFilesUploadBegin(rw http.ResponseWriter, r *http.Request, s *session) {
	var in struct {
		Folder, Path string
		Size         int64
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in) != nil || in.Size < 0 || in.Path == "" {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	f := w.fdFolderOf(rw, in.Folder)
	if f == nil {
		return
	}
	if f.f.Ignored(in.Path, false) {
		writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"skipped": true}})
		return
	}
	if err := f.f.CheckRoom(in.Size); err != nil {
		fdFail(rw, err)
		return
	}
	key := s.user + "|" + in.Folder + "|" + in.Path + "|" + strconv.FormatInt(in.Size, 10)
	fdUpMu.Lock()
	fdUpExpire()
	u := fdUps[fdUpKey[key]]
	if u == nil || u.done {
		b := make([]byte, 16)
		rand.Read(b)
		u = &fdUpload{id: hex.EncodeToString(b), user: s.user, folder: in.Folder, path: in.Path, size: in.Size}
		fdUps[u.id] = u
		fdUpKey[key] = u.id
	}
	u.last = time.Now()
	id := u.id
	fdUpMu.Unlock()
	off, err := f.f.ResumeSize(id)
	if err != nil {
		fdFail(rw, err)
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"id": id, "offset": off}})
}

func (w *WebServer) fdUploadOf(rw http.ResponseWriter, id string, s *session) (*fdUpload, *fdFolder) {
	fdUpMu.Lock()
	u := fdUps[id]
	if u != nil && u.user != s.user {
		u = nil
	}
	if u != nil {
		u.last = time.Now()
	}
	fdUpMu.Unlock()
	if u == nil {
		jsonError(rw, http.StatusNotFound, "no such upload on this node")
		return nil, nil
	}
	f := w.fdFolderOf(rw, u.folder)
	if f == nil {
		return nil, nil
	}
	return u, f
}

func (w *WebServer) handleFilesUploadChunk(rw http.ResponseWriter, r *http.Request, s *session) {
	q := r.URL.Query()
	u, f := w.fdUploadOf(rw, q.Get("id"), s)
	if u == nil {
		return
	}
	off, err := strconv.ParseInt(q.Get("offset"), 10, 64)
	if err != nil || off < 0 {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	fdNoDeadlines(rw)
	n, err := f.f.ResumeAppend(u.id, off, io.LimitReader(fdUploadFrom(r), u.size-off))
	if errors.Is(err, filesync.ErrOffset) {
		writeJSON(rw, http.StatusConflict, map[string]any{"ok": false, "error": "the upload is at another place", "offset": n})
		return
	}
	if err != nil && r.Context().Err() == nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		fdFail(rw, err)
		return
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"offset": n}})
}

func (w *WebServer) handleFilesUploadFinish(rw http.ResponseWriter, r *http.Request, s *session) {
	var in struct{ ID string }
	if json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&in) != nil {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	u, f := w.fdUploadOf(rw, in.ID, s)
	if u == nil {
		return
	}
	fdUpMu.Lock()
	done, n := u.done, u.n
	fdUpMu.Unlock()
	if done { // the answer was lost on the way: it is the same
		writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"size": n}})
		return
	}
	fdNoDeadlines(rw)
	n, err := f.f.ResumeFinish(u.path, u.id, u.size)
	if errors.Is(err, filesync.ErrOffset) {
		have, _ := f.f.ResumeSize(u.id)
		writeJSON(rw, http.StatusConflict, map[string]any{"ok": false, "error": "the upload is not all here", "offset": have})
		return
	}
	if err != nil {
		fdFail(rw, err)
		return
	}
	fdUpMu.Lock()
	u.done, u.n = true, n
	fdUpMu.Unlock()
	f.rescan()
	fdAudit(s, "files: %s: wrote %s (%d bytes)", f.cfg.ID, u.path, n)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"size": n}})
}

func (w *WebServer) handleFilesMkdir(rw http.ResponseWriter, r *http.Request, s *session) {
	var in struct{ Folder, Path string }
	if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in) != nil {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	f := w.fdFolderOf(rw, in.Folder)
	if f == nil {
		return
	}
	if f.f.Ignored(in.Path, true) {
		writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "skipped": true})
		return
	}
	if err := f.f.Mkdir(in.Path); err != nil {
		fdFail(rw, err)
		return
	}
	f.rescan()
	fdAudit(s, "files: %s: made folder %s", f.cfg.ID, in.Path)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

func (w *WebServer) handleFilesRename(rw http.ResponseWriter, r *http.Request, s *session) {
	var in struct{ Folder, From, To string }
	if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in) != nil {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	f := w.fdFolderOf(rw, in.Folder)
	if f == nil {
		return
	}
	if err := f.f.Rename(in.From, in.To); err != nil {
		fdFail(rw, err)
		return
	}
	f.rescan()
	fdAudit(s, "files: %s: renamed %s to %s", f.cfg.ID, in.From, in.To)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

func (w *WebServer) handleFilesDelete(rw http.ResponseWriter, r *http.Request, s *session) {
	var in struct{ Folder, Path string }
	if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in) != nil {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	f := w.fdFolderOf(rw, in.Folder)
	if f == nil {
		return
	}
	if err := f.f.Delete(in.Path); err != nil {
		fdFail(rw, err)
		return
	}
	f.rescan()
	fdAudit(s, "files: %s: deleted %s (kept in the trash)", f.cfg.ID, in.Path)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

// rescan asks for a scan soon, without waiting for it, and without piling them up.
func (f *fdFolder) rescan() {
	f.rsMu.Lock()
	defer f.rsMu.Unlock()
	if f.rsRunning {
		f.rsDirty = true // one is on its way or waiting: it will see this change too
		return
	}
	f.rsRunning = true
	go func() {
		for {
			t0 := time.Now()
			f.scanNow()
			took := time.Since(t0)
			f.rsMu.Lock()
			if !f.rsDirty {
				f.rsRunning = false
				f.rsMu.Unlock()
				return
			}
			f.rsDirty = false
			f.rsMu.Unlock()
			// Changes came while it ran (an upload of thousands of files makes one every few milliseconds): the next scan waits twice as long
			// as this one took, so scanning never takes more than a third of the time, and there is never more than one.
			time.Sleep(max(time.Second, 2*took))
		}
	}()
}

func fdAudit(s *session, format string, a ...any) {
	infof("web: "+s.user+": "+format, a...)
}

// fdFreeName gives a name in the folder that is not taken: "name copy.ext", then "name copy 2.ext", and so on.
func fdFreeName(f *fdFolder, rel string) (string, error) {
	dir, base := path.Split(rel)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if base == stem+ext && strings.HasPrefix(base, ".") && ext == base {
		stem, ext = base, ""
	}
	for i := 1; i < 1000; i++ {
		n := stem + " copy"
		if i > 1 {
			n += " " + strconv.Itoa(i)
		}
		cand := dir + n + ext
		if _, err := f.f.Stat(cand); errors.Is(err, filesync.ErrNotFound) {
			return cand, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", filesync.ErrExists
}

// handleFilesCopy copies or moves a file or folder to another place, which may be in another synced folder (drag and drop, paste, and
// duplicate on the Files page).  A name that is taken is an error, unless "unique" asks for a free one ("name copy").
func (w *WebServer) handleFilesCopy(rw http.ResponseWriter, r *http.Request, s *session) {
	var in struct {
		Folder, Path, ToFolder, To string
		Move, Unique, Async        bool
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in) != nil {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	src, dst := w.fdFolderOf(rw, in.Folder), (*fdFolder)(nil)
	if src == nil {
		return
	}
	if dst = w.fdFolderOf(rw, in.ToFolder); dst == nil {
		return
	}
	from, to := strings.Trim(path.Clean("/"+in.Path), "/"), strings.Trim(path.Clean("/"+in.To), "/")
	if from == "" || to == "" {
		jsonError(rw, http.StatusBadRequest, "that cannot be copied or moved")
		return
	}
	if src == dst && (strings.HasPrefix(to, from+"/") || (to == from && (in.Move || !in.Unique))) {
		jsonError(rw, http.StatusConflict, "that cannot be put into itself")
		return
	}
	if _, err := src.f.Stat(from); err != nil {
		fdFail(rw, err)
		return
	}
	if p := path.Dir(to); p != "." {
		if st, err := dst.f.Stat(p); err != nil || !st.IsDir() {
			jsonError(rw, http.StatusConflict, "the folder it goes into is not there")
			return
		}
	}
	if _, err := dst.f.Stat(to); err == nil {
		if !in.Unique {
			jsonError(rw, http.StatusConflict, "that name is already there")
			return
		}
		n, err := fdFreeName(dst, to)
		if err != nil {
			fdFail(rw, err)
			return
		}
		to = n
	}
	if in.Async && !(in.Move && src == dst) { // the copy goes on in the background, and the page asks how far it is
		a, b := davTarget{f: src, id: src.cfg.ID, rel: from}, davTarget{f: dst, id: dst.cfg.ID, rel: to}
		job := &fdCopyJob{id: randToken(), user: s.user, dest: to, total: davTreeSize(a), state: "running"}
		var ctx context.Context
		ctx, job.cancel = context.WithCancel(context.Background())
		fdCopyMu.Lock()
		running := 0
		for id, j := range fdCopyJobs {
			j.mu.Lock()
			if j.state == "running" {
				running++
			} else if time.Since(j.ended) > 10*time.Minute {
				delete(fdCopyJobs, id)
			}
			j.mu.Unlock()
		}
		if running >= fdFetchMaxRunning {
			fdCopyMu.Unlock()
			jsonError(rw, http.StatusTooManyRequests, "this node is already copying several things; wait for one to finish")
			return
		}
		fdCopyJobs[job.id] = job
		fdCopyMu.Unlock()
		go func() {
			defer job.cancel()
			err := davCopyTreeN(ctx, a, b, false, &job.done)
			if err == nil && in.Move {
				err = src.f.Delete(from)
			}
			cancelled := ctx.Err() != nil && err != nil
			if cancelled {
				dst.f.Delete(to) // what was copied so far is not kept; the source is untouched
			}
			src.rescan()
			dst.rescan()
			job.mu.Lock()
			job.ended = time.Now()
			if cancelled {
				job.state, job.err = "cancelled", "cancelled"
			} else if err != nil {
				job.state, job.err = "failed", err.Error()
			} else {
				job.state = "done"
			}
			job.mu.Unlock()
			if cancelled {
				infof("files: %s: copying %s/%s to %s/%s was cancelled", dst.cfg.ID, src.cfg.ID, from, dst.cfg.ID, to)
				return
			}
			if err != nil {
				warnf("files: %s: copying %s/%s to %s/%s did not finish: %v", dst.cfg.ID, src.cfg.ID, from, dst.cfg.ID, to, err)
				return
			}
			fdAudit(s, "files: %s: %s %s/%s to %s/%s", dst.cfg.ID, map[bool]string{true: "moved", false: "copied"}[in.Move], src.cfg.ID, from, dst.cfg.ID, to)
		}()
		writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"path": to, "id": job.id, "total": job.total}})
		return
	}
	var err error
	if in.Move && src == dst {
		err = src.f.Rename(from, to)
	} else {
		a, b := davTarget{f: src, id: src.cfg.ID, rel: from}, davTarget{f: dst, id: dst.cfg.ID, rel: to}
		if err = davCopyTree(a, b, false); err == nil && in.Move {
			err = src.f.Delete(from)
		}
	}
	if err != nil {
		fdFail(rw, err)
		return
	}
	src.rescan()
	dst.rescan()
	fdAudit(s, "files: %s: %s %s/%s to %s/%s", dst.cfg.ID, map[bool]string{true: "moved", false: "copied"}[in.Move], src.cfg.ID, from, dst.cfg.ID, to)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"path": to}})
}

// fdCopyJob is one copy in progress: it goes on after the request that started it was answered.
type fdCopyJob struct {
	id, user, dest string
	total          int64
	done           atomic.Int64
	mu             sync.Mutex
	state          string // running, done or failed
	err            string
	ended          time.Time
	cancel         context.CancelFunc
}

var (
	fdCopyMu   sync.Mutex
	fdCopyJobs = map[string]*fdCopyJob{}
)

// handleFilesSize adds up what is under a folder (or all of a synced folder when the path is empty), from the index: no reading of the disk.
func (w *WebServer) handleFilesSize(rw http.ResponseWriter, r *http.Request, s *session) {
	f := w.fdFolderOf(rw, r.URL.Query().Get("folder"))
	if f == nil {
		return
	}
	p := strings.Trim(r.URL.Query().Get("path"), "/")
	var files, dirs, bytes int64
	for _, e := range f.f.Entries() {
		if e.Deleted || (p != "" && e.Name != p && !strings.HasPrefix(e.Name, p+"/")) || e.Name == p {
			continue
		}
		switch e.Kind {
		case filesync.KFile:
			files++
			bytes += e.Size
		case filesync.KDir:
			dirs++
		}
	}
	out := map[string]any{"files": files, "dirs": dirs, "bytes": bytes}
	if self, ok := f.f.Entry(p); ok && p != "" && !self.Deleted { // the folder's own time and who changed it last
		out["mod"], out["by"] = self.ModNs/1e9, self.By
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": out})
}

// handleFilesCopyStatus says how far a copy is.
func (w *WebServer) handleFilesCopyStatus(rw http.ResponseWriter, r *http.Request, s *session) {
	fdCopyMu.Lock()
	j := fdCopyJobs[r.URL.Query().Get("id")]
	fdCopyMu.Unlock()
	if j == nil || j.user != s.user {
		jsonError(rw, http.StatusNotFound, "no such copy on this node")
		return
	}
	j.mu.Lock()
	out := map[string]any{"state": j.state, "done": j.done.Load(), "total": j.total, "path": j.dest, "error": j.err}
	j.mu.Unlock()
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": out})
}

// handleFilesCopyCancel stops a copy; what was copied so far is removed again.
func (w *WebServer) handleFilesCopyCancel(rw http.ResponseWriter, r *http.Request, s *session) {
	var in struct{ ID string }
	json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&in)
	fdCopyMu.Lock()
	j := fdCopyJobs[in.ID]
	fdCopyMu.Unlock()
	if j == nil || j.user != s.user {
		jsonError(rw, http.StatusNotFound, "no such copy on this node")
		return
	}
	j.cancel()
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

// handleFilesTrash says what the trash of a folder holds on this node.
func (w *WebServer) handleFilesTrash(rw http.ResponseWriter, r *http.Request, s *session) {
	f := w.fdFolderOf(rw, r.URL.Query().Get("folder"))
	if f == nil {
		return
	}
	b, n := f.f.TrashSize()
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"bytes": b, "files": n}})
}

// handleFilesTrashEmpty empties the trash of a folder on this node.  What is in it cannot be got back.
func (w *WebServer) handleFilesTrashEmpty(rw http.ResponseWriter, r *http.Request, s *session) {
	var in struct{ Folder string }
	if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in) != nil {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	f := w.fdFolderOf(rw, in.Folder)
	if f == nil {
		return
	}
	b, n, err := f.f.EmptyTrash()
	if err != nil {
		fdFail(rw, err)
		return
	}
	f.trashStale()
	fdAudit(s, "files: %s: emptied the trash (%d files, %d bytes)", f.cfg.ID, n, b)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"bytes": b, "files": n}})
}

// handleFilesTrashList lists one level of the trash of a folder on this node, like a folder's listing.
func (w *WebServer) handleFilesTrashList(rw http.ResponseWriter, r *http.Request, s *session) {
	f := w.fdFolderOf(rw, r.URL.Query().Get("folder"))
	if f == nil {
		return
	}
	items, total, err := f.f.TrashEntries(r.URL.Query().Get("when"), r.URL.Query().Get("path"), 5000)
	if err != nil {
		fdFail(rw, err)
		return
	}
	if items == nil {
		items = []filesync.TrashEntry{}
	}
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"entries": items, "total": total, "truncated": total > len(items)}})
}

// handleFilesTrashRestore puts a file or folder of the trash back in its folder.
func (w *WebServer) handleFilesTrashRestore(rw http.ResponseWriter, r *http.Request, s *session) {
	var in struct{ Folder, When, Name string }
	if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in) != nil {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	f := w.fdFolderOf(rw, in.Folder)
	if f == nil {
		return
	}
	n, err := f.f.RestoreTrash(in.When, in.Name)
	if err != nil {
		fdFail(rw, err)
		return
	}
	f.trashStale()
	f.rescan()
	fdAudit(s, "files: %s: restored %s from the trash (%d files)", f.cfg.ID, in.Name, n)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"files": n}})
}

// handleFilesTrashDelete removes a file or folder of the trash for good.
func (w *WebServer) handleFilesTrashDelete(rw http.ResponseWriter, r *http.Request, s *session) {
	var in struct{ Folder, When, Name string }
	if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in) != nil {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	f := w.fdFolderOf(rw, in.Folder)
	if f == nil {
		return
	}
	b, n, err := f.f.DeleteTrash(in.When, in.Name)
	if err != nil {
		fdFail(rw, err)
		return
	}
	f.trashStale()
	fdAudit(s, "files: %s: deleted %s from the trash for good (%d files, %d bytes)", f.cfg.ID, in.Name, n, b)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"bytes": b, "files": n}})
}
