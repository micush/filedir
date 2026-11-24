package main

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLimiterSpacesBytes(t *testing.T) {
	var l fdLimiter
	t0 := time.Now()
	l.wait(context.Background(), 10<<20) // no limit: at once
	if time.Since(t0) > 50*time.Millisecond {
		t.Fatal("waited with no limit")
	}
	l.set(16) // 2 MB/s
	t0 = time.Now()
	for i := 0; i < 32; i++ {
		l.wait(context.Background(), fdLimChunk) // 1 MiB in all
	}
	if d := time.Since(t0); d < 350*time.Millisecond || d > 900*time.Millisecond {
		t.Fatalf("1 MiB at 16 Mbit/s took %s (about half a second, less the burst)", d)
	}
	// the context ends a wait
	l.set(1)
	l.wait(context.Background(), 1<<20) // the schedule is now far ahead
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := l.wait(ctx, 1); err == nil {
		t.Fatal("a cancelled request kept waiting")
	}
}

func TestThrottleReaderAndWriter(t *testing.T) {
	var l fdLimiter
	l.set(16)
	data := bytes.Repeat([]byte("x"), 512<<10) // 0.25 s at 2 MB/s, less the burst
	t0 := time.Now()
	got, err := io.ReadAll(fdThrottleReader{bytes.NewReader(data), &l, context.Background()})
	if err != nil || len(got) != len(data) {
		t.Fatalf("read: %d %v", len(got), err)
	}
	if d := time.Since(t0); d < 120*time.Millisecond {
		t.Fatalf("512 KiB read in %s: not held to the rate", d)
	}
	rec := httptest.NewRecorder()
	l.set(16)
	t0 = time.Now()
	n, err := fdThrottleWriter{rec, &l, context.Background()}.Write(data)
	if err != nil || n != len(data) || rec.Body.Len() != len(data) {
		t.Fatalf("write: %d %v", n, err)
	}
	if d := time.Since(t0); d < 120*time.Millisecond {
		t.Fatalf("512 KiB written in %s: not held to the rate", d)
	}
}

func TestFilesLimitsHoldUploadsAndDownloads(t *testing.T) {
	e := newSiteEnv(t, true)
	pat := &siteClient{e: e}
	if pat.login("pat", "pw") != 200 {
		t.Fatal("login")
	}
	body := []byte(strings.Repeat("y", 768<<10))
	fdSetLimits(FilesConfig{LimitUploadMbit: 16, LimitDownloadMbit: 16})
	t.Cleanup(func() { fdSetLimits(FilesConfig{}) })
	t0 := time.Now()
	if code, b, _ := pat.req(e.site.URL, "POST", "/api/files/upload?folder=docs&path=big.bin", body); code != 200 {
		t.Fatalf("upload: %d %s", code, b)
	}
	if d := time.Since(t0); d < 200*time.Millisecond {
		t.Fatalf("768 KiB uploaded in %s at a limit of 2 MB/s", d)
	}
	t0 = time.Now()
	if code, b, _ := pat.req(e.site.URL, "GET", "/api/files/download?folder=docs&path=big.bin", nil); code != 200 || len(b) != len(body) {
		t.Fatalf("download: %d %d", code, len(b))
	}
	if d := time.Since(t0); d < 200*time.Millisecond {
		t.Fatalf("768 KiB downloaded in %s at a limit of 2 MB/s", d)
	}
	fdSetLimits(FilesConfig{})
	t0 = time.Now()
	pat.req(e.site.URL, "GET", "/api/files/download?folder=docs&path=big.bin", nil)
	if time.Since(t0) > 150*time.Millisecond {
		t.Fatal("still slow with no limit")
	}
}

func TestFilesLimitSettings(t *testing.T) {
	for _, c := range []FilesConfig{{LimitUploadMbit: -1}, {LimitDownloadMbit: 100001}, {LimitReplMbit: -5}} {
		if c.Validate() == nil {
			t.Fatalf("%+v was accepted", c)
		}
	}
	c := (&FilesConfig{LimitUploadMbit: 10, LimitDownloadMbit: 20, LimitReplMbit: 30}).clone()
	if c == nil || c.LimitUploadMbit != 10 || c.LimitDownloadMbit != 20 || c.LimitReplMbit != 30 {
		t.Fatalf("clone: %+v", c)
	}
	if (&FilesConfig{LimitReplMbit: 100}).Validate() != nil {
		t.Fatal("a good limit was refused")
	}
}

// What a node sends to the others is held to the replication limit of that node.
func TestFilesReplicationLimit(t *testing.T) {
	fdNoTell.Store(true)
	t.Cleanup(func() { fdNoTell.Store(false); fdSetLimits(FilesConfig{}) })
	a, b := newFdTestNode(t, "A"), newFdTestNode(t, "B")
	b.join(a.tnode)
	a.sync()
	b.sync()
	big := bytes.Repeat([]byte("0123456789abcdef"), 192<<10) // 3 MiB
	a.write(t, "big.bin", big)
	a.svc.scanAll()
	fdSetLimits(FilesConfig{LimitReplMbit: 16}) // 2 MB/s: a second and a half
	t0 := time.Now()
	b.svc.syncAll()
	d := time.Since(t0)
	if !b.has("big.bin", big) {
		t.Fatal("B did not get the file")
	}
	if d < 900*time.Millisecond {
		t.Fatalf("3 MiB crossed in %s at a limit of 2 MB/s", d)
	}
}
