package main

import (
	"sync"
	"sync/atomic"
	"time"
)

// What replication moves between the nodes, for the graph on the Files status page: the bytes of file data this node has sent to the
// others and has received from them, counted as they go, and sampled every few seconds into a short history.  The history is kept in
// memory only: a restart starts it again.

const (
	fdTrafficEvery = 5 * time.Second
	fdTrafficKeep  = 360 // samples: half an hour
)

// fdTrafficPoint is one sample: the time (unix seconds) and the rates over the interval before it, in bytes a second.
type fdTrafficPoint struct {
	T  int64 `json:"t"`
	Rx int64 `json:"rx"`
	Tx int64 `json:"tx"`
}

type fdTraffic struct {
	rx, tx atomic.Int64 // bytes received from peers, and sent to them, since the start

	mu       sync.Mutex
	pts      []fdTrafficPoint
	lastRx   int64
	lastTx   int64
	lastTime time.Time
}

// sample takes one point from the counters.  now is a parameter so that a test can drive it.
func (t *fdTraffic) sample(now time.Time) {
	rx, tx := t.rx.Load(), t.tx.Load()
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.lastTime.IsZero() {
		secs := now.Sub(t.lastTime).Seconds()
		if secs > 0 {
			t.pts = append(t.pts, fdTrafficPoint{T: now.Unix(), Rx: int64(float64(rx-t.lastRx) / secs), Tx: int64(float64(tx-t.lastTx) / secs)})
			if len(t.pts) > fdTrafficKeep {
				t.pts = append(t.pts[:0], t.pts[len(t.pts)-fdTrafficKeep:]...)
			}
		}
	}
	t.lastRx, t.lastTx, t.lastTime = rx, tx, now
}

// history is a copy of the samples, oldest first.
func (t *fdTraffic) history() []fdTrafficPoint {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]fdTrafficPoint(nil), t.pts...)
}
