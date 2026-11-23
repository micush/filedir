package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Sessions of the files site are the same on every node.  The site is served on the gateways' addresses, so a person's next request may
// be answered by any node: the one that was serving them fails, the election moves the VIP, or an anycast route changes.  A session
// is therefore sent to the other members, over the signed and pinned cluster channel, when it is made, ended, or changes who may do
// what, and its activity is passed on at most every sessTouchEvery so the idle time holds everywhere.  A node that gets a cookie it
// does not know asks the others for it before it turns the person away, so a node that missed a message (it was down, it has just
// joined) still serves them.  What travels is the session's record: the SHA-256 of the cookie (never the cookie), the user, the CSRF
// token and the times.  The management GUI's sessions are not shared: an administrator works on one node's address.
//
// A session that is ended is remembered on each node for as long as it could have lived (dead), so a message that arrives late does
// not bring it back.  That memory is not kept over a restart: a node that was down when a person signed out still has the session,
// and passes it on again if the person's old cookie is used there before it runs out.

const (
	sessTouchEvery = 20 * time.Second
	missedFor      = 30 * time.Second
	fetchRate      = 20.0 // asks of the other nodes per second, for cookies this node does not know (so made-up cookies cannot make the cluster busy)
	fetchBurst     = 40.0
)

// sessionShare carries sessions to and from the other nodes.
type sessionShare interface {
	put(rec sessionRec)
	del(key string)
	get(key string) *sessionRec // the record of a session, from any node that has it; nil when none does
}

type sessMsg struct {
	Op  string      `json:"op"` // put, del or get
	Rec *sessionRec `json:"rec,omitempty"`
	Key string      `json:"key,omitempty"`
}

type sessReply struct {
	Rec *sessionRec `json:"rec,omitempty"`
}

// clusterShare is the real thing: the members of the management cluster.
type clusterShare struct{ cl *Cluster }

func (c *clusterShare) peers() []ClusterPeer {
	if c.cl == nil || !c.cl.Enabled() {
		return nil
	}
	return c.cl.Snapshot().Peers
}

func (c *clusterShare) tell(m sessMsg) {
	var wg sync.WaitGroup
	for _, p := range c.peers() {
		wg.Add(1)
		go func(p ClusterPeer) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := c.cl.call(ctx, p, "POST", "/cluster/websession", m, nil, 5*time.Second); err != nil {
				debugf("web: files: session %s to %s: %v", m.Op, p.Addr, err)
			}
		}(p)
	}
	wg.Wait()
}

func (c *clusterShare) put(rec sessionRec) { c.tell(sessMsg{Op: "put", Rec: &rec}) }
func (c *clusterShare) del(key string)     { c.tell(sessMsg{Op: "del", Key: key}) }

func (c *clusterShare) get(key string) *sessionRec {
	peers := c.peers()
	if len(peers) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch := make(chan *sessionRec, len(peers))
	for _, p := range peers {
		go func(p ClusterPeer) {
			var out sessReply
			if err := c.cl.call(ctx, p, "POST", "/cluster/websession", sessMsg{Op: "get", Key: key}, &out, 3*time.Second); err != nil {
				out.Rec = nil
			}
			ch <- out.Rec
		}(p)
	}
	for range peers {
		if r := <-ch; r != nil {
			return r // the first node that has it
		}
	}
	return nil
}

// handleWebSession is what a member does with another's message.
func (c *Cluster) handleWebSession(rw http.ResponseWriter, r *http.Request, caller ClusterPeer, body []byte) {
	var m sessMsg
	if json.Unmarshal(body, &m) != nil || m.Op == "" {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	w := c.mg.web.Load()
	if w == nil {
		jsonError(rw, http.StatusServiceUnavailable, "the web server is not running")
		return
	}
	b, _ := json.Marshal(w.sessionFromPeer(m))
	rw.Header().Set("Content-Type", "application/json")
	rw.Write(b)
}

// ── this node's side ──

// recOfLocked is the record of a session (w.mu held).
func (w *WebServer) recOfLocked(key string, s *session) sessionRec {
	return sessionRec{Key: key, User: s.user, CSRF: s.csrf, Created: s.created.Unix(), Last: s.last.Unix(), RO: s.readOnly, IP: s.ip, SSO: s.sso, SSOProv: s.ssoProv, LDAP: s.ldap, Site: s.site}
}

// shareOut runs fn against the sharing, if there is one.
func (w *WebServer) shareOut(fn func(sessionShare)) {
	if w.share != nil {
		fn(w.share)
	}
}

// pushSession sends one session to the other nodes and waits for them (briefly: a node that does not answer is given up on).
func (w *WebServer) pushSession(key string) {
	w.mu.Lock()
	s := w.sessions[key]
	if s == nil || s.site != siteFiles {
		w.mu.Unlock()
		return
	}
	rec := w.recOfLocked(key, s)
	w.mu.Unlock()
	w.shareOut(func(sh sessionShare) { sh.put(rec) })
}

func validSessKey(k string) bool {
	if len(k) != 64 {
		return false
	}
	for _, c := range k {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// validToken says whether a cookie value has the shape randToken makes.
func validToken(t string) bool {
	if len(t) != 43 {
		return false
	}
	for _, c := range t {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// fetchAllow is the limit on asking the other nodes for a cookie this one does not know.
func (w *WebServer) fetchAllow() bool {
	w.fetchMu.Lock()
	defer w.fetchMu.Unlock()
	now := time.Now()
	if w.fetchAt.IsZero() {
		w.fetchTk = fetchBurst
	} else {
		w.fetchTk = min(fetchBurst, w.fetchTk+now.Sub(w.fetchAt).Seconds()*fetchRate)
	}
	w.fetchAt = now
	if w.fetchTk < 1 {
		return false
	}
	w.fetchTk--
	return true
}

// pullSession asks the other nodes for the session a cookie belongs to, and keeps it if one has it.
func (w *WebServer) pullSession(tok, key string) *session {
	if w.share == nil || !validToken(tok) {
		return nil
	}
	w.mu.Lock()
	if t, ok := w.missed[key]; ok && time.Since(t) < missedFor {
		w.mu.Unlock()
		return nil
	}
	if _, gone := w.dead[key]; gone {
		w.mu.Unlock()
		return nil
	}
	w.mu.Unlock()
	if !w.fetchAllow() {
		return nil
	}
	rec := w.share.get(key)
	if rec == nil || rec.Key != key || !w.installSession(*rec) {
		w.mu.Lock()
		if len(w.missed) > 4096 {
			w.missed = map[string]time.Time{}
		}
		w.missed[key] = time.Now()
		w.mu.Unlock()
		return nil
	}
	w.mu.Lock()
	s := w.sessions[key]
	w.mu.Unlock()
	if s != nil {
		infof("web: files: the session of %q was taken over from another node", s.user)
		w.saveSessions()
	}
	return s
}

// installSession takes a session another node sent.  It is checked as if a stranger had sent it, and what is already here is only brought
// up to date: the later activity wins, and a session this node ended is not brought back.  The group is looked at again on first use.
func (w *WebServer) installSession(r sessionRec) bool {
	if r.Site != siteFiles || !validSessKey(r.Key) || !validUsername(r.User) || r.CSRF == "" || len(r.CSRF) > 128 || len(r.SSOProv) > 128 {
		return false
	}
	now := time.Now()
	created, last := time.Unix(r.Created, 0), time.Unix(r.Last, 0)
	if created.After(now.Add(clockSkew)) || now.Sub(created) > sessionMaxAge || now.Sub(last) > w.policy.Load().sessionIdle() {
		return false
	}
	if last.After(now) {
		last = now
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, gone := w.dead[r.Key]; gone {
		return false
	}
	if s := w.sessions[r.Key]; s != nil {
		if s.site != siteFiles || s.user != r.User {
			return false
		}
		if last.After(s.last) {
			s.last, s.readOnly = last, r.RO
		}
		return true
	}
	// how the person signed in travels with the session: the node that takes it over rechecks the group the same way (a directory or
	// single sign-on user has no account here to look up, and would otherwise be ended as "no longer in the group")
	w.sessions[r.Key] = &session{user: r.User, csrf: r.CSRF, created: created, last: last, readOnly: r.RO, ip: r.IP, site: siteFiles, key: r.Key, pushed: now,
		ldap: r.LDAP, sso: r.SSO, ssoProv: r.SSOProv}
	return true
}

// sessionFromPeer answers another node's message.
func (w *WebServer) sessionFromPeer(m sessMsg) sessReply {
	switch m.Op {
	case "put":
		if m.Rec != nil && w.installSession(*m.Rec) {
			w.saveSessions()
		}
	case "del":
		if validSessKey(m.Key) {
			w.mu.Lock()
			if s := w.sessions[m.Key]; s == nil || s.site == siteFiles {
				delete(w.sessions, m.Key)
				w.dead[m.Key] = time.Now()
			}
			w.mu.Unlock()
			w.saveSessions()
		}
	case "get":
		if !validSessKey(m.Key) {
			return sessReply{}
		}
		now := time.Now()
		w.mu.Lock()
		defer w.mu.Unlock()
		if s := w.sessions[m.Key]; s != nil && s.site == siteFiles && now.Sub(s.created) <= sessionMaxAge && now.Sub(s.last) <= w.policy.Load().sessionIdle() {
			r := w.recOfLocked(m.Key, s)
			return sessReply{Rec: &r}
		}
	}
	return sessReply{}
}
