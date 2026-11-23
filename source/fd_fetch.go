package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Upload from URL: the node fetches a file from a web address into the folder the person is in.  Only http and https; the address the
// connection really goes to (after the name is resolved, and after every redirect) must not be this host or a link-local address, so a
// pasted URL cannot be used to reach what only the node itself can (its own services, a cloud metadata address).  Private network
// addresses are allowed: fetching from a server on the same network is the point.

const fdFetchMaxRedirects = 5

// fdFetchBlocked says whether the node refuses to connect to ip.  A variable so that a test can reach a server on loopback.
var fdFetchBlocked = func(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast()
}

func fdFetchClient() *http.Client {
	d := &net.Dialer{
		Timeout: 20 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || fdFetchBlocked(ip) {
				return errors.New("that address is not one the node may fetch from")
			}
			return nil
		},
	}
	return &http.Client{
		Transport: &http.Transport{DialContext: d.DialContext, TLSHandshakeTimeout: 20 * time.Second, ResponseHeaderTimeout: 60 * time.Second, DisableCompression: true},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > fdFetchMaxRedirects {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("redirected to something that is not http or https")
			}
			return nil
		},
	}
}

// fdFetchName is the file name a fetched file is given: the one the server names, else the last part of the address.
func fdFetchName(u *url.URL, disp string) string {
	if _, p, err := mime.ParseMediaType(disp); err == nil {
		if n := path.Base(strings.ReplaceAll(p["filename"], "\\", "/")); n != "" && n != "." && n != "/" {
			return n
		}
	}
	if n := path.Base(u.Path); n != "" && n != "." && n != "/" {
		if un, err := url.PathUnescape(n); err == nil {
			return un
		}
		return n
	}
	return "download"
}

// fdFetchJob is one fetch in progress: it goes on after the request that started it has been answered, and the page asks how far it is.
type fdFetchJob struct {
	id, user, dest, host string
	total                int64 // from Content-Length; -1 when the server did not say
	received             atomic.Int64
	mu                   sync.Mutex
	state                string // running, done, failed or cancelled
	err                  string
	size                 int64
	ended                time.Time
	cancel               context.CancelFunc
}

var (
	fdFetchMu   sync.Mutex
	fdFetchJobs = map[string]*fdFetchJob{}
)

const fdFetchMaxRunning = 4

// fdFetchGC forgets the jobs that ended more than ten minutes ago; the caller holds fdFetchMu.
func fdFetchGC() {
	for id, j := range fdFetchJobs {
		j.mu.Lock()
		old := j.state != "running" && time.Since(j.ended) > 10*time.Minute
		j.mu.Unlock()
		if old {
			delete(fdFetchJobs, id)
		}
	}
}

type fdCountReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c fdCountReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// handleFilesFetch starts a fetch.  The connection and the server's answer are dealt with here, so an address that is refused or a server
// that says no is answered at once; the file itself then comes in the background (handleFilesFetchStatus says how far).
func (w *WebServer) handleFilesFetch(rw http.ResponseWriter, r *http.Request, s *session) {
	var in struct{ Folder, Dir, URL, Name string }
	if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in) != nil {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	u, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		jsonError(rw, http.StatusBadRequest, "give a web address that starts with http:// or https://")
		return
	}
	f := w.fdFolderOf(rw, in.Folder)
	if f == nil {
		return
	}
	fdFetchMu.Lock()
	fdFetchGC()
	running := 0
	for _, j := range fdFetchJobs {
		j.mu.Lock()
		if j.state == "running" {
			running++
		}
		j.mu.Unlock()
	}
	fdFetchMu.Unlock()
	if running >= fdFetchMaxRunning {
		jsonError(rw, http.StatusTooManyRequests, "this node is already fetching several files; wait for one to finish")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop := context.AfterFunc(r.Context(), cancel) // the person gave up before the server answered
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	req.Header.Set("User-Agent", "filedir/"+version())
	res, err := fdFetchClient().Do(req)
	stop()
	if err != nil {
		cancel()
		jsonError(rw, http.StatusBadGateway, "could not fetch it: "+fdFetchErr(err))
		return
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		cancel()
		jsonError(rw, http.StatusBadGateway, fmt.Sprintf("the server answered %s", res.Status))
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = fdFetchName(res.Request.URL, res.Header.Get("Content-Disposition"))
	}
	if strings.ContainsAny(name, "/\\") {
		res.Body.Close()
		cancel()
		jsonError(rw, http.StatusBadRequest, "that name is not allowed")
		return
	}
	dest := path.Join(in.Dir, name)
	job := &fdFetchJob{id: randToken(), user: s.user, dest: dest, host: u.Host, total: res.ContentLength, state: "running", cancel: cancel}
	fdFetchMu.Lock()
	fdFetchJobs[job.id] = job
	fdFetchMu.Unlock()
	fdAudit(s, "files: %s: fetching %s from %s", f.cfg.ID, dest, u.Host)
	go func() {
		defer cancel()
		defer res.Body.Close()
		n, err := f.f.WriteFile(dest, fdThrottleReader{fdCountReader{res.Body, &job.received}, &fdLimUp, ctx})
		job.mu.Lock()
		job.ended, job.size = time.Now(), n
		switch {
		case ctx.Err() != nil:
			job.state, job.err = "cancelled", "cancelled"
		case err != nil:
			job.state, job.err = "failed", fdFetchErr(err)
			if res.ContentLength >= 0 && job.received.Load() < res.ContentLength {
				job.err = "the connection ended before the whole file came: " + job.err
			}
		default:
			job.state = "done"
		}
		state, jerr := job.state, job.err
		job.mu.Unlock()
		if state == "done" {
			f.rescan()
			infof("files: %s: fetched %s from %s (%d bytes)", f.cfg.ID, dest, u.Host, n)
		} else {
			warnf("files: %s: fetching %s from %s did not finish: %s", f.cfg.ID, dest, u.Host, jerr)
		}
	}()
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"id": job.id, "path": dest, "name": name, "total": job.total}})
}

func fdFetchJobOf(id, user string) *fdFetchJob {
	fdFetchMu.Lock()
	defer fdFetchMu.Unlock()
	if j := fdFetchJobs[id]; j != nil && j.user == user {
		return j
	}
	return nil
}

// handleFilesFetchStatus says how far a fetch is.
func (w *WebServer) handleFilesFetchStatus(rw http.ResponseWriter, r *http.Request, s *session) {
	j := fdFetchJobOf(r.URL.Query().Get("id"), s.user)
	if j == nil {
		jsonError(rw, http.StatusNotFound, "no such fetch on this node (it may have finished long ago, or be on another node)")
		return
	}
	j.mu.Lock()
	out := map[string]any{"state": j.state, "received": j.received.Load(), "total": j.total, "path": j.dest, "error": j.err}
	j.mu.Unlock()
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": out})
}

// handleFilesFetchCancel stops a fetch; what was received so far is not kept.
func (w *WebServer) handleFilesFetchCancel(rw http.ResponseWriter, r *http.Request, s *session) {
	var in struct{ ID string }
	json.NewDecoder(io.LimitReader(r.Body, 1<<12)).Decode(&in)
	j := fdFetchJobOf(in.ID, s.user)
	if j == nil {
		jsonError(rw, http.StatusNotFound, "no such fetch on this node")
		return
	}
	j.cancel()
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true})
}

// fdFetchErr is the reason a fetch failed, without the address the node tried to reach.
func fdFetchErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return err.Error()
}
