package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"filedir/filesync"
)

// Files in the cluster.
//
// Every node keeps its own copy of every synced folder.  A node asks each other node, over the signed and certificate-pinned peer
// channel of clusternet.go, for the entries that changed since it last asked (/cluster/files/index), and fetches the blocks it
// lacks (/cluster/files/block).  A file is fetched from every peer that holds that version, a few blocks from each at once, so the
// more nodes have a file the faster it reaches the next one.  The engine (filesync) does the deciding; this file only carries.

const (
	fdScanEvery   = 30 * time.Second
	fdSyncEvery   = 10 * time.Second
	fdIndexBatch  = 5000
	fdMaxBlock    = 16 << 20
	fdPeerTimeout = 60 * time.Second
)

type fdFolder struct {
	cfg     FolderCfg
	pct     int // the disk limit it was opened with
	f       *filesync.Folder
	mu      sync.Mutex
	scanned time.Time
	scanErr string
	peers   map[string]*fdPeerState

	rescanning atomic.Bool
	svc        *fdService
	told       bool            // a notice to the other nodes is going out or waiting (tellPeers)
	tellDue    bool            // a change came while it was
	pulling    map[string]bool // peers being asked now because they said they have something new (syncSoon)
	watch      *fdWatch        // inotify on the folder's directories (fd_watch.go); nil when it could not start
}

type fdPeerState struct {
	At    time.Time
	Err   string
	Moved int
}

type fdService struct {
	sup *Supervisor
	mg  *Mgmt

	mu      sync.Mutex
	cancel  context.CancelFunc
	folders map[string]*fdFolder
	srcs    map[string]*fdSource // by peer node id

	cfgFn  func() FilesConfig // a test's own configuration
	served atomic.Int64       // blocks this node has sent to peers

	vmu  sync.Mutex
	view map[string]map[string]map[string]filesync.Vector // peer → folder → name → version last seen in its index
}

var fdSvc *fdService

func newFdService(sup *Supervisor, mg *Mgmt) *fdService {
	s := &fdService{sup: sup, mg: mg, folders: map[string]*fdFolder{}, srcs: map[string]*fdSource{},
		view: map[string]map[string]map[string]filesync.Vector{}}
	if mg != nil && mg.cl != nil {
		mg.cl.files = s
	}
	return s
}

func (s *fdService) config() FilesConfig {
	if s.cfgFn != nil {
		return s.cfgFn()
	}
	if s.sup == nil {
		return FilesConfig{}
	}
	dc := s.sup.config()
	if dc == nil || dc.Settings.Files == nil {
		return FilesConfig{}
	}
	return *dc.Settings.Files
}

func (s *fdService) selfID() string {
	if s.mg == nil || s.mg.cl == nil {
		return "local"
	}
	return s.mg.cl.node.Self().NodeID
}

func (s *fdService) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()
	go s.loop(ctx, 3*time.Second, s.reconcile)
	go s.loop(ctx, fdScanEvery, s.scanAll)
	go s.loop(ctx, fdSyncEvery, s.syncAll)
	go s.loop(ctx, 6*time.Hour, s.maintain)
}

func (s *fdService) loop(ctx context.Context, every time.Duration, f func()) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f()
		}
	}
}

func (s *fdService) Stop() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	fs := s.folders
	s.folders = map[string]*fdFolder{}
	s.mu.Unlock()
	for _, f := range fs {
		f.close()
	}
}

// close stops the watch and closes the folder's index.  A folder that did not open has neither.
func (f *fdFolder) close() {
	if f.watch != nil {
		f.watch.Close()
	}
	if f.f != nil {
		f.f.Close()
	}
}

func (s *fdService) get(id string) *fdFolder {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.folders[id]
}

func (s *fdService) list() []*fdFolder {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*fdFolder, 0, len(s.folders))
	for _, f := range s.folders {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].cfg.ID < out[j].cfg.ID })
	return out
}

// reconcile opens the folders the configuration names and closes the ones it no longer does (or that are paused, or moved).
func (s *fdService) reconcile() {
	fdSetLimits(s.config())
	want := map[string]FolderCfg{}
	for _, fc := range s.config().Folders {
		if !fc.Paused {
			want[fc.ID] = fc
		}
	}
	s.mu.Lock()
	var drop []*fdFolder
	for id, f := range s.folders {
		if w, ok := want[id]; !ok || !fdSameCfg(w, f.cfg) || f.pct != s.config().diskPct() {
			drop = append(drop, f)
			delete(s.folders, id)
		}
	}
	s.mu.Unlock()
	for _, f := range drop {
		f.close()
	}
	for id, fc := range want {
		if s.get(id) != nil {
			continue
		}
		f, err := s.open(fc)
		s.mu.Lock()
		if err != nil {
			warnf("files: %s: %v", id, err)
			s.folders[id] = &fdFolder{cfg: fc, pct: s.config().diskPct(), scanErr: err.Error()}
		} else {
			s.folders[id] = f
		}
		s.mu.Unlock()
	}
}

func fdSameCfg(a, b FolderCfg) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func (s *fdService) open(fc FolderCfg) (*fdFolder, error) {
	dir := "/var/lib/filedir"
	if s.mg != nil && s.mg.stateDir != "" {
		dir = s.mg.stateDir
	}
	cfg := filesync.Config{
		ID: fc.ID, Root: fc.Path, DBDir: filepath.Join(dir, "files", fdDBName(fc.ID)), Node: filesync.NodeID(s.selfID()),
		Ignore: fc.Ignore, IgnorePerms: fc.IgnorePerms, MaxFileSize: int64(fc.MaxFileMB) << 20, MaxUsedPct: s.config().diskPct(), TrashDays: fc.TrashDays,
		Create: true, Peers: func(name string, v filesync.Vector) []filesync.Source { return s.holders(fc.ID, name, v) },
		Swarm: s.swarm,
	}
	f, err := filesync.Open(cfg)
	if err != nil {
		return nil, err
	}
	nf := &fdFolder{cfg: fc, pct: cfg.MaxUsedPct, f: f, peers: map[string]*fdPeerState{}, svc: s}
	nf.watch = startFdWatch(nf)
	go nf.scanNow() // what is there is known at once, not at the next round
	return nf, nil
}

func (s *fdService) scanAll() {
	for _, f := range s.list() {
		f.scanNow()
	}
}

// scanNow scans the folder and notes when and how it went.  A scan that found changes tells the other nodes, so they come and fetch
// them at once instead of at their next round.
func (f *fdFolder) scanNow() {
	if f.f == nil {
		return
	}
	n, err := f.f.Scan()
	f.mu.Lock()
	f.scanned = time.Now()
	f.scanErr = ""
	if err != nil {
		f.scanErr = err.Error()
	}
	f.mu.Unlock()
	if n > 0 && err == nil && f.svc != nil {
		f.svc.tellPeers(f.cfg.ID)
	}
}

func (s *fdService) maintain() {
	for _, f := range s.list() {
		if f.f != nil {
			f.f.Maintain()
		}
	}
}

func (s *fdService) otherPeers() []ClusterPeer {
	if s.mg == nil || s.mg.cl == nil || !s.mg.cl.Enabled() {
		return nil
	}
	return s.mg.cl.node.Snapshot().Peers
}

// source is the connection to one peer; the same value every time, so the engine can tell it from the others.
func (s *fdService) source(p ClusterPeer) *fdSource {
	s.mu.Lock()
	defer s.mu.Unlock()
	if x := s.srcs[p.NodeID]; x != nil {
		x.mu.Lock()
		x.peer = p
		x.mu.Unlock()
		return x
	}
	x := &fdSource{svc: s, peer: p}
	s.srcs[p.NodeID] = x
	return x
}

func (s *fdService) syncAll() {
	peers := s.otherPeers()
	folders := s.list()
	var wg sync.WaitGroup
	for _, f := range folders {
		if f.f == nil {
			continue
		}
		for _, p := range peers {
			wg.Add(1)
			go func(f *fdFolder, p ClusterPeer) {
				defer wg.Done()
				s.syncOne(f, p)
			}(f, p)
		}
	}
	wg.Wait()
}

// syncOne asks one peer about one folder until it has nothing more (or a batch of rounds is done).
func (s *fdService) syncOne(f *fdFolder, p ClusterPeer) {
	src := s.source(p)
	st := &fdPeerState{}
	moved := 0
	var err error
	for round := 0; round < 20; round++ {
		var n int
		n, err = f.f.SyncFrom(filesync.NodeID(p.NodeID), src)
		moved += n
		if err != nil || n == 0 {
			break
		}
	}
	st.At, st.Moved = time.Now(), moved
	if err != nil {
		st.Err = err.Error()
	}
	f.mu.Lock()
	was := ""
	if old := f.peers[p.NodeID]; old != nil {
		was = old.Err
	}
	f.peers[p.NodeID] = st
	f.mu.Unlock()
	if st.Err != was { // said when it starts or changes, not at every round
		if st.Err != "" {
			warnf("files: %s: syncing with %s failed: %s", f.cfg.ID, p.Addr, st.Err)
		} else {
			infof("files: %s: syncing with %s works again", f.cfg.ID, p.Addr)
		}
	}
}

// holders are the sources of the peers that hold this version of a file, going by the indexes they have sent.
func (s *fdService) holders(folder, name string, v filesync.Vector) []filesync.Source {
	s.vmu.Lock()
	var ids []string
	for peer, fs := range s.view {
		if cur, ok := fs[folder][name]; ok && cur.Compare(v) == filesync.Equal {
			ids = append(ids, peer)
		}
	}
	s.vmu.Unlock()
	var out []filesync.Source
	snap := s.otherPeers()
	for _, id := range ids {
		for _, p := range snap {
			if p.NodeID == id {
				out = append(out, s.source(p))
			}
		}
	}
	return out
}

// swarm names every other node that is connected: each may be pulling the file that this one is, and can be asked which blocks of it it has.
func (s *fdService) swarm() []filesync.Source {
	var out []filesync.Source
	for _, p := range s.otherPeers() {
		out = append(out, s.source(p))
	}
	return out
}

func (s *fdService) note(peer, folder string, es []filesync.FileInfo) {
	s.vmu.Lock()
	defer s.vmu.Unlock()
	fs := s.view[peer]
	if fs == nil {
		fs = map[string]map[string]filesync.Vector{}
		s.view[peer] = fs
	}
	m := fs[folder]
	if m == nil {
		m = map[string]filesync.Vector{}
		fs[folder] = m
	}
	for _, e := range es {
		if e.Deleted {
			delete(m, e.Name)
		} else {
			m[e.Name] = e.Version
		}
	}
}

// fdSource is a peer, as the engine sees it.
type fdSource struct {
	svc  *fdService
	mu   sync.Mutex
	peer ClusterPeer // as last learned; read through get (it is refreshed while calls are going on)
}

func (x *fdSource) get() ClusterPeer {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.peer
}

type fdIndexReq struct {
	Folder string `json:"folder"`
	After  int64  `json:"after"`
}

type fdIndexReply struct {
	Entries []filesync.FileInfo `json:"entries"`
	Max     int64               `json:"max"`
}

type fdBlockReq struct {
	Folder string        `json:"folder"`
	Name   string        `json:"name"`
	Off    int64         `json:"off"`
	Size   int           `json:"size"`
	Hash   filesync.Hash `json:"hash"`
}

func (x *fdSource) Index(folder string, after int64) ([]filesync.FileInfo, int64, error) {
	cl := x.svc.mg.cl
	ctx, cancel := context.WithTimeout(context.Background(), fdPeerTimeout)
	defer cancel()
	var r fdIndexReply
	qb, _ := json.Marshal(fdIndexReq{folder, after})
	if err := cl.callFiles(ctx, x.get(), "/cluster/files/index", qb, func(rd io.Reader) error { return json.NewDecoder(rd).Decode(&r) }, fdPeerTimeout); err != nil {
		return nil, 0, err
	}
	x.svc.note(x.get().NodeID, folder, r.Entries)
	return r.Entries, r.Max, nil
}

func (x *fdSource) Block(folder, name string, off int64, size int, h filesync.Hash) ([]byte, error) {
	if size <= 0 || size > fdMaxBlock {
		return nil, fmt.Errorf("a block of %d bytes", size)
	}
	cl := x.svc.mg.cl
	// a big block from a node that is held to a few Mbit/s takes its time: a second more for every 128 KB
	tmo := fdPeerTimeout + time.Duration(size>>17)*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), tmo)
	defer cancel()
	body, _ := json.Marshal(fdBlockReq{folder, name, off, size, h})
	var data []byte
	err := cl.callFiles(ctx, x.get(), "/cluster/files/block", body, func(rd io.Reader) error {
		// the size is known: one buffer, not the copies and doublings that ReadAll makes of a big block
		b := make([]byte, size)
		n, err := io.ReadFull(rd, b)
		if err == io.ErrUnexpectedEOF || err == io.EOF {
			err = nil // a short block: the caller says it does not match
		}
		data = b[:n]
		var one [1]byte
		if m, _ := rd.Read(one[:]); m > 0 && err == nil {
			data = append(data, one[0]) // longer than asked: it does not match either
		}
		return err
	}, tmo)
	return data, err
}

type fdBlocksReq struct {
	Folder string `json:"folder"`
	Items  []struct {
		Name string        `json:"name"`
		Off  int64         `json:"off"`
		Size int           `json:"size"`
		Hash filesync.Hash `json:"hash"`
	} `json:"items"`
}

const fdMaxBatch = 256 // blocks in one batched request

// Blocks fetches many small blocks in one request.  The answer is a frame for each: one byte (1: here, 0: not), four bytes of length,
// the data.  A block that is not here comes back nil, and is asked for singly.
func (x *fdSource) Blocks(folder string, reqs []filesync.BlockReq) ([][]byte, error) {
	if len(reqs) == 0 || len(reqs) > fdMaxBatch {
		return nil, fmt.Errorf("a batch of %d blocks", len(reqs))
	}
	var q fdBlocksReq
	q.Folder = folder
	total := 0
	for _, r := range reqs {
		if r.Size <= 0 || r.Size > fdMaxBlock {
			return nil, fmt.Errorf("a block of %d bytes", r.Size)
		}
		total += r.Size
		q.Items = append(q.Items, struct {
			Name string        `json:"name"`
			Off  int64         `json:"off"`
			Size int           `json:"size"`
			Hash filesync.Hash `json:"hash"`
		}{r.Name, r.Off, r.Size, r.Hash})
	}
	if total > fdMaxBlock {
		return nil, fmt.Errorf("a batch of %d bytes", total)
	}
	tmo := fdPeerTimeout + time.Duration(total>>17)*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), tmo)
	defer cancel()
	body, _ := json.Marshal(q)
	out := make([][]byte, len(reqs))
	err := x.svc.mg.cl.callFiles(ctx, x.get(), "/cluster/files/blocks", body, func(rd io.Reader) error {
		for i, r := range reqs {
			var h [5]byte
			if _, err := io.ReadFull(rd, h[:]); err != nil {
				return err
			}
			n := int(binary.BigEndian.Uint32(h[1:]))
			if n < 0 || n > r.Size {
				return fmt.Errorf("a frame of %d bytes for a block of %d", n, r.Size)
			}
			if h[0] == 0 {
				continue
			}
			b := make([]byte, n)
			if _, err := io.ReadFull(rd, b); err != nil {
				return err
			}
			out[i] = b
		}
		return nil
	}, tmo)
	if err != nil {
		return nil, err
	}
	return out, nil
}

type fdHaveReq struct {
	Folder  string          `json:"folder"`
	Name    string          `json:"name"`
	Version filesync.Vector `json:"version"`
}

type fdHaveResp struct {
	Complete bool   `json:"complete"`
	N        int    `json:"n"`
	Bits     []byte `json:"bits,omitempty"`
}

// Have asks which blocks of a version of a file the peer holds, including those that have arrived of a pull it has under way.
func (x *fdSource) Have(folder, name string, v filesync.Vector) (filesync.Availability, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fdPeerTimeout)
	defer cancel()
	body, _ := json.Marshal(fdHaveReq{folder, name, v})
	var out fdHaveResp
	err := x.svc.mg.cl.callFiles(ctx, x.get(), "/cluster/files/have", body, func(rd io.Reader) error {
		return json.NewDecoder(io.LimitReader(rd, 1<<16)).Decode(&out)
	}, fdPeerTimeout)
	if err != nil {
		return filesync.Availability{}, err
	}
	if out.N < 0 || out.N > 1<<20 || len(out.Bits) > (out.N+7)/8 {
		return filesync.Availability{}, fmt.Errorf("a list of %d blocks", out.N)
	}
	return filesync.Availability{Complete: out.Complete, N: out.N, Bits: out.Bits}, nil
}

func (c *Cluster) handleFilesHave(rw http.ResponseWriter, r *http.Request, caller ClusterPeer, body []byte) {
	var q fdHaveReq
	if json.Unmarshal(body, &q) != nil || c.files == nil || len(q.Name) > 4096 {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	f := c.files.get(q.Folder)
	if f == nil || f.f == nil {
		jsonError(rw, http.StatusNotFound, "no such folder here")
		return
	}
	a := f.f.Have(q.Name, q.Version)
	rw.Header().Set("Content-Type", "application/json")
	json.NewEncoder(rw).Encode(fdHaveResp{a.Complete, a.N, a.Bits})
}

func (c *Cluster) handleFilesBlocks(rw http.ResponseWriter, r *http.Request, caller ClusterPeer, body []byte) {
	var q fdBlocksReq
	if json.Unmarshal(body, &q) != nil || c.files == nil || len(q.Items) == 0 || len(q.Items) > fdMaxBatch {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	total := 0
	for _, it := range q.Items {
		if it.Size <= 0 || it.Size > fdMaxBlock {
			jsonError(rw, http.StatusBadRequest, "bad request")
			return
		}
		total += it.Size
	}
	if total > fdMaxBlock {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	f := c.files.get(q.Folder)
	if f == nil || f.f == nil {
		jsonError(rw, http.StatusNotFound, "no such folder here")
		return
	}
	var buf bytes.Buffer
	for _, it := range q.Items {
		data, err := f.f.Block(it.Name, it.Off, it.Size, it.Hash)
		var h [5]byte
		if err == nil {
			h[0] = 1
			binary.BigEndian.PutUint32(h[1:], uint32(len(data)))
			buf.Write(h[:])
			buf.Write(data)
			c.files.served.Add(1)
		} else {
			buf.Write(h[:])
		}
	}
	rw.Header().Set("Content-Type", "application/octet-stream")
	fdThrottleWriter{rw, &fdLimRepl, r.Context()}.Write(buf.Bytes())
}

// applyFileListener starts the file transfer listener (c.mu is held).  It has its own port so that bulk transfers do not share the
// cluster's control channel; it uses the same node identity and the same signed requests.
func (c *Cluster) applyFileListener(cert tls.Certificate) {
	if c.fsrv != nil && c.flisten == c.cfg.FileListen {
		return
	}
	c.stopFileListener()
	ln, err := net.Listen("tcp", c.cfg.FileListen)
	if err != nil {
		errorf("cluster: cannot listen for file transfer on %s: %v", c.cfg.FileListen, err)
		return
	}
	mux := http.NewServeMux()
	c.registerFileRoutes(mux)
	srv := &http.Server{
		ErrorLog: log.New(io.Discard, "", 0), Handler: mux, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 4 * time.Minute, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, ClientAuth: tls.RequestClientCert},
	}
	c.fsrv, c.flisten = srv, c.cfg.FileListen
	go func() {
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errorf("cluster: file transfer listener stopped: %v", err)
		}
	}()
	infof("cluster: file transfer listening on %s", c.cfg.FileListen)
}

// stopFileListener closes it (c.mu is held, or the cluster is stopping).
func (c *Cluster) stopFileListener() {
	if c.fsrv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	c.fsrv.Shutdown(ctx)
	cancel()
	c.fsrv, c.flisten = nil, ""
}

// fileAddr is the file transfer address of a peer's cluster address: the same host, the file port.
func (c *Cluster) fileAddr(peer ClusterPeer, addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if peer.FilePort > 0 {
		return net.JoinHostPort(h, strconv.Itoa(peer.FilePort))
	}
	c.mu.Lock()
	fl := c.cfg.FileListen
	c.mu.Unlock()
	_, p, err := net.SplitHostPort(fl)
	if err != nil {
		p = strconv.Itoa(defaultFilePort)
	}
	return net.JoinHostPort(h, p)
}

// callFiles talks to a peer's file transfer port, trying the addresses it is known by like callRaw does.
func (c *Cluster) callFiles(ctx context.Context, peer ClusterPeer, path string, body []byte, onBody func(io.Reader) error, timeout time.Duration) error {
	var errs []string
	addrs := c.addrsFor(peer)
	for _, a := range addrs {
		err := c.callAddrWith(true, ctx, c.fileAddr(peer, a), peer, "POST", path, body, onBody, timeout, nil)
		if err == nil {
			c.noteGoodAddr(peer, a)
			return nil
		}
		var pe *peerError
		if errors.As(err, &pe) || ctx.Err() != nil || len(addrs) == 1 {
			return err
		}
		errs = append(errs, a+": "+err.Error())
	}
	return errors.New(strings.Join(errs, "; "))
}

func (c *Cluster) registerFileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /cluster/files/index", c.peerAuth(c.handleFilesIndex))
	mux.HandleFunc("POST /cluster/files/block", c.peerAuth(c.handleFilesBlock))
	mux.HandleFunc("POST /cluster/files/blocks", c.peerAuth(c.handleFilesBlocks))
	mux.HandleFunc("POST /cluster/files/have", c.peerAuth(c.handleFilesHave))
	mux.HandleFunc("POST /cluster/files/status", c.peerAuth(c.handleFilesStatus))
	mux.HandleFunc("POST /cluster/files/notify", c.peerAuth(c.handleFilesNotify))
}

// fdNotify says a folder has changed on the node that sent it.
type fdNotify struct {
	Folder string `json:"folder"`
}

// fdNoTell switches the notices off (tests that count which node served what need the nodes to ask for themselves).
var fdNoTell atomic.Bool

// fdTellEvery is the least time between two notices of one folder to the other nodes; a change that comes sooner is told when it is up.
const fdTellEvery = time.Second

// tellPeers lets the other nodes know a folder has changed here, so they ask for it now and not at their next round.  It is only a nudge:
// a node that does not answer, or is of a version without the call, finds the change at its next round (fdSyncEvery) all the same.
func (s *fdService) tellPeers(folder string) {
	if fdNoTell.Load() {
		return
	}
	if s.mg == nil || s.mg.cl == nil || !s.mg.cl.Enabled() {
		return
	}
	f := s.get(folder)
	if f == nil {
		return
	}
	f.mu.Lock()
	if f.told {
		f.tellDue = true // one is on its way: it will see this change too
		f.mu.Unlock()
		return
	}
	f.told = true
	f.mu.Unlock()
	go func() {
		for {
			s.tellNow(folder)
			time.Sleep(fdTellEvery)
			f.mu.Lock()
			again := f.tellDue
			f.tellDue = false
			if !again {
				f.told = false
			}
			f.mu.Unlock()
			if !again {
				return
			}
		}
	}()
}

func (s *fdService) tellNow(folder string) {
	body, _ := json.Marshal(fdNotify{Folder: folder})
	var wg sync.WaitGroup
	for _, p := range s.otherPeers() {
		wg.Add(1)
		go func(p ClusterPeer) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := s.mg.cl.callFiles(ctx, p, "/cluster/files/notify", body, func(io.Reader) error { return nil }, 10*time.Second)
			if err != nil {
				debugf("files: %s: could not tell %s of a change: %v", folder, p.Addr, err)
			}
		}(p)
	}
	wg.Wait()
}

// handleFilesNotify: the caller changed a folder; ask it for what is new, now.
func (c *Cluster) handleFilesNotify(rw http.ResponseWriter, r *http.Request, caller ClusterPeer, body []byte) {
	var q fdNotify
	if json.Unmarshal(body, &q) != nil || c.files == nil {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	if f := c.files.get(q.Folder); f != nil && f.f != nil {
		c.files.syncSoon(f, caller)
	}
	rw.Header().Set("Content-Type", "application/json")
	rw.Write([]byte(`{}`))
}

// syncSoon asks one peer about one folder in the background, unless that is already going on.
func (s *fdService) syncSoon(f *fdFolder, caller ClusterPeer) {
	p := caller
	for _, q := range s.otherPeers() { // the member as this node knows it (its addresses), not as it described itself
		if q.NodeID == caller.NodeID {
			p = q
		}
	}
	key := p.NodeID
	f.mu.Lock()
	if f.pulling[key] {
		f.mu.Unlock()
		return
	}
	if f.pulling == nil {
		f.pulling = map[string]bool{}
	}
	f.pulling[key] = true
	f.mu.Unlock()
	go func() {
		s.syncOne(f, p)
		f.mu.Lock()
		delete(f.pulling, key)
		f.mu.Unlock()
	}()
}

func (c *Cluster) handleFilesIndex(rw http.ResponseWriter, r *http.Request, caller ClusterPeer, body []byte) {
	var q fdIndexReq
	if json.Unmarshal(body, &q) != nil || c.files == nil {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	f := c.files.get(q.Folder)
	if f == nil || f.f == nil {
		jsonError(rw, http.StatusNotFound, "no such folder here")
		return
	}
	es, max := f.f.Index(q.After)
	if len(es) > fdIndexBatch { // the peer asks again from where this ends
		es = es[:fdIndexBatch]
		max = es[len(es)-1].Seq
	}
	b, _ := json.Marshal(fdIndexReply{Entries: es, Max: max})
	rw.Header().Set("Content-Type", "application/json")
	rw.Write(b)
}

func (c *Cluster) handleFilesBlock(rw http.ResponseWriter, r *http.Request, caller ClusterPeer, body []byte) {
	var q fdBlockReq
	if json.Unmarshal(body, &q) != nil || c.files == nil || q.Size <= 0 || q.Size > fdMaxBlock {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	f := c.files.get(q.Folder)
	if f == nil || f.f == nil {
		jsonError(rw, http.StatusNotFound, "no such folder here")
		return
	}
	data, err := f.f.Block(q.Name, q.Off, q.Size, q.Hash)
	if err != nil {
		jsonError(rw, http.StatusConflict, "block not available")
		return
	}
	c.files.served.Add(1)
	rw.Header().Set("Content-Type", "application/octet-stream")
	fdThrottleWriter{rw, &fdLimRepl, r.Context()}.Write(data) // what this node sends the others, within its replication limit
}

// emptyTrashOn empties the trash of every synced folder that is on the filesystem mounted at mount (a "Disk nearly full" alert is open
// for it): the trash is a copy kept for 30 days, and the disk's room matters more.  A folder whose trash is empty is left alone.
func (s *fdService) emptyTrashOn(mount string) {
	var md syscall.Stat_t
	if syscall.Stat(mount, &md) != nil {
		return
	}
	for _, f := range s.list() {
		if f.f == nil {
			continue
		}
		var fd syscall.Stat_t
		if syscall.Stat(f.cfg.Path, &fd) != nil || fd.Dev != md.Dev {
			continue
		}
		if _, n := f.f.TrashSize(); n == 0 {
			continue
		}
		b, n, err := f.f.EmptyTrash()
		if err != nil {
			warnf("files: %s: emptying the trash on %s (disk nearly full) failed: %v", f.cfg.ID, mount, err)
			continue
		}
		warnf("files: %s: the disk %s is nearly full: emptied the trash (%d files, %d bytes)", f.cfg.ID, mount, n, b)
	}
}
