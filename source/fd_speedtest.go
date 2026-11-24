package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"filedir/filesync"
	"filedir/filesync/blake3"
)

// A speed test for the files, to tell where the time goes when a transfer is slower than the network.  It measures the parts of a
// replication apart: the connection to each other node with nothing else in the way (data over the same signed and encrypted channel,
// made up on the far side and thrown away on this one: no disk, no hash), how fast this node hashes a block, and how fast its disk takes
// a write.  Run on a node, it is that node as the receiver.

const (
	fdSpeedStreams = 16 // requests at once to a node, as many as a replication uses
	fdSpeedMaxMB   = 4096
)

var (
	fdSpeedOnce sync.Once
	fdSpeedBuf  []byte
)

// fdSpeedData is a megabyte of random bytes that the test sends over and over (compression would make a fool of a test of zeros).
func fdSpeedData() []byte {
	fdSpeedOnce.Do(func() {
		fdSpeedBuf = make([]byte, 1<<20)
		rand.Read(fdSpeedBuf)
	})
	return fdSpeedBuf
}

type fdSpeedReq struct {
	MB int `json:"mb"`
}

// handleFilesSpeed sends the number of megabytes the peer asks for.
func (c *Cluster) handleFilesSpeed(rw http.ResponseWriter, r *http.Request, caller ClusterPeer, body []byte) {
	var q fdSpeedReq
	if json.Unmarshal(body, &q) != nil || q.MB <= 0 || q.MB > fdSpeedMaxMB {
		jsonError(rw, http.StatusBadRequest, "bad request")
		return
	}
	rw.Header().Set("Content-Type", "application/octet-stream")
	d := fdSpeedData()
	for i := 0; i < q.MB; i++ {
		if _, err := rw.Write(d); err != nil {
			return
		}
	}
}

type fdSpeedPeer struct {
	Node  string  `json:"node"`
	Addr  string  `json:"addr"`
	Mbit  float64 `json:"mbit,omitempty"`
	Secs  float64 `json:"secs,omitempty"`
	Error string  `json:"error,omitempty"`
}

type fdSpeedResult struct {
	Peers []fdSpeedPeer `json:"peers"`
	Hash  struct {
		Blake3 float64 `json:"blake3_mbs"`
		SHA256 float64 `json:"sha256_mbs"`
	} `json:"hash"`
	Disk struct {
		Folder string  `json:"folder,omitempty"`
		Write  float64 `json:"write_mbs,omitempty"`
		Error  string  `json:"error,omitempty"`
	} `json:"disk"`
}

// speedTest runs the test on this node: mb megabytes from each other node, then the hash and the disk.
func (s *fdService) speedTest(ctx context.Context, mb int) fdSpeedResult {
	var res fdSpeedResult
	res.Peers = []fdSpeedPeer{}
	if mb < fdSpeedStreams {
		mb = fdSpeedStreams
	}
	for _, p := range s.otherPeers() {
		if ctx.Err() != nil {
			break
		}
		sp := fdSpeedPeer{Node: p.NodeID, Addr: p.Addr}
		per := mb / fdSpeedStreams
		var got atomic.Int64
		var wg sync.WaitGroup
		var mu sync.Mutex
		t0 := time.Now()
		for i := 0; i < fdSpeedStreams; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				body, _ := json.Marshal(fdSpeedReq{MB: per})
				err := s.mg.cl.callFiles(ctx, p, "/cluster/files/speed", body, func(rd io.Reader) error {
					n, err := io.Copy(io.Discard, rd)
					got.Add(n)
					return err
				}, 2*time.Minute)
				if err != nil {
					mu.Lock()
					if sp.Error == "" {
						sp.Error = err.Error()
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		sp.Secs = time.Since(t0).Seconds()
		if sp.Error == "" && sp.Secs > 0 {
			sp.Mbit = float64(got.Load()) * 8 / 1e6 / sp.Secs
		}
		res.Peers = append(res.Peers, sp)
	}
	res.Hash.Blake3, res.Hash.SHA256 = fdSpeedHash()
	s.speedDisk(&res, mb)
	return res
}

// fdSpeedHash is how fast one core hashes blocks of 4 MB, with the hash that is used (BLAKE3) and with SHA-256, in MB a second.
func fdSpeedHash() (b3, sha float64) {
	buf := make([]byte, 4<<20)
	rand.Read(buf)
	run := func(f func()) float64 {
		n, t0 := 0, time.Now()
		for time.Since(t0) < 400*time.Millisecond {
			f()
			n++
		}
		return float64(n) * 4 / time.Since(t0).Seconds()
	}
	return run(func() { blake3.Sum256(buf) }), run(func() { sha256.Sum256(buf) })
}

// speedDisk writes about mb megabytes (at most a quarter of the room there is) into the folder's own directory in the first folder that
// is here, makes sure they are on the disk, and removes them.
func (s *fdService) speedDisk(res *fdSpeedResult, mb int) {
	for _, f := range s.list() {
		if f.f == nil {
			continue
		}
		res.Disk.Folder = f.cfg.ID
		dir := filepath.Join(f.cfg.Path, filesync.MetaDir, "tmp")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			res.Disk.Error = err.Error()
			return
		}
		if _, avail, _, err := filesync.DiskSpace(dir); err == nil && uint64(mb)<<20 > avail/4 {
			mb = int(avail / 4 >> 20)
		}
		if mb < 16 {
			res.Disk.Error = "not enough room to test"
			return
		}
		fh, err := os.CreateTemp(dir, "speedtest-")
		if err != nil {
			res.Disk.Error = err.Error()
			return
		}
		defer os.Remove(fh.Name())
		defer fh.Close()
		d := fdSpeedData()
		t0 := time.Now()
		for i := 0; i < mb; i++ {
			if _, err := fh.Write(d); err != nil {
				res.Disk.Error = err.Error()
				return
			}
		}
		if err := fh.Sync(); err != nil {
			res.Disk.Error = err.Error()
			return
		}
		res.Disk.Write = float64(mb) / time.Since(t0).Seconds()
		return
	}
	res.Disk.Error = "no folder is open here"
}

// handleFilesSpeedTest runs the test on this node.  It sends and receives a lot, so it is for a person to start.
func (w *WebServer) handleFilesSpeedTest(rw http.ResponseWriter, r *http.Request, s *session) {
	var in struct {
		MB int `json:"mb"`
	}
	json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&in)
	if in.MB <= 0 {
		in.MB = 512
	}
	if in.MB > fdSpeedMaxMB {
		in.MB = fdSpeedMaxMB
	}
	if fdSvc == nil {
		jsonError(rw, http.StatusServiceUnavailable, "files are not running here")
		return
	}
	if !fdSpeedBusy.CompareAndSwap(false, true) {
		jsonError(rw, http.StatusConflict, "a speed test is running already")
		return
	}
	defer fdSpeedBusy.Store(false)
	fdAudit(s, "files: speed test (%d MB from each node)", in.MB)
	writeJSON(rw, http.StatusOK, map[string]any{"ok": true, "data": fdSvc.speedTest(r.Context(), in.MB)})
}

var fdSpeedBusy atomic.Bool
