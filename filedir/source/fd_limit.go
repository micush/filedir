package main

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// Bandwidth limits for the files: uploads (what people send in), downloads (what is sent out to people) and replication (what this node
// sends to the other nodes).  Each is per node and shared by every transfer of its kind on it, in Mbit/s, 0 for none.  A limiter spaces
// the bytes out: a transfer asks for the time its bytes take at the rate and waits for it, so a few short bursts go through at once and
// a long one runs at the rate.

type fdLimiter struct {
	mu   sync.Mutex
	rate float64   // bytes a second; 0: no limit
	next time.Time // when the bytes already promised are all gone
}

// fdLimBurst is how far behind the clock the schedule may fall, so a connection that was idle is not made to wait for time it did not use
// up, and a small file goes at once.
const fdLimBurst = 100 * time.Millisecond

// fdLimChunk is the most that is sent between two waits, so a limit of a few Mbit/s is smooth and not a burst and a long pause.
const fdLimChunk = 32 << 10

func (l *fdLimiter) set(mbit int) {
	l.mu.Lock()
	if mbit <= 0 {
		l.rate = 0
	} else {
		l.rate = float64(mbit) * 1e6 / 8
	}
	l.next = time.Time{}
	l.mu.Unlock()
}

func (l *fdLimiter) limited() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rate > 0
}

// wait reserves n bytes and sleeps until their time has come (or the context ends).
func (l *fdLimiter) wait(ctx context.Context, n int) error {
	l.mu.Lock()
	if l.rate <= 0 || n <= 0 {
		l.mu.Unlock()
		return nil
	}
	now := time.Now()
	if l.next.Before(now.Add(-fdLimBurst)) {
		l.next = now.Add(-fdLimBurst)
	}
	d := l.next.Sub(now)
	l.next = l.next.Add(time.Duration(float64(n) / l.rate * float64(time.Second)))
	l.mu.Unlock()
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var fdLimUp, fdLimDown, fdLimRepl fdLimiter

// fdSetLimits applies the configured limits.
func fdSetLimits(c FilesConfig) {
	fdLimUp.set(c.LimitUploadMbit)
	fdLimDown.set(c.LimitDownloadMbit)
	fdLimRepl.set(c.LimitReplMbit)
}

// fdThrottleReader slows what is read from it (an upload coming in, a file being fetched).
type fdThrottleReader struct {
	r   io.Reader
	l   *fdLimiter
	ctx context.Context
}

func (t fdThrottleReader) Read(p []byte) (int, error) {
	if len(p) > fdLimChunk && t.l.limited() {
		p = p[:fdLimChunk]
	}
	n, err := t.r.Read(p)
	if n > 0 {
		if werr := t.l.wait(t.ctx, n); werr != nil && err == nil {
			err = werr
		}
	}
	return n, err
}

// fdThrottleWriter slows what is written to it (a download going out).
type fdThrottleWriter struct {
	http.ResponseWriter
	l   *fdLimiter
	ctx context.Context
}

func (t fdThrottleWriter) Write(p []byte) (int, error) {
	if !t.l.limited() {
		return t.ResponseWriter.Write(p)
	}
	done := 0
	for done < len(p) {
		end := done + fdLimChunk
		if end > len(p) {
			end = len(p)
		}
		if err := t.l.wait(t.ctx, end-done); err != nil {
			return done, err
		}
		n, err := t.ResponseWriter.Write(p[done:end])
		done += n
		if err != nil {
			return done, err
		}
	}
	return done, nil
}

// Unwrap lets http.ResponseController reach the real writer (to lift the deadlines, to flush).
func (t fdThrottleWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }

func (t fdThrottleWriter) Flush() { http.NewResponseController(t.ResponseWriter).Flush() }

func fdDownloadTo(rw http.ResponseWriter, r *http.Request) http.ResponseWriter {
	return fdThrottleWriter{rw, &fdLimDown, r.Context()}
}

func fdUploadFrom(r *http.Request) io.Reader {
	return fdThrottleReader{r.Body, &fdLimUp, r.Context()}
}
