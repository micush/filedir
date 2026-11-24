package main

import (
	"bytes"
	"fmt"
	"testing"

	"filedir/filesync"
	"time"
)

// How fast a file crosses between two nodes on this machine.  It asserts only that it arrives; the rate is logged.
func TestFilesTransferRate(t *testing.T) {
	fdNoTell.Store(true)
	t.Cleanup(func() { fdNoTell.Store(false) })
	a, b := newFdTestNode(t, "A"), newFdTestNode(t, "B")
	b.join(a.tnode)
	a.sync()
	b.sync()
	big := bytes.Repeat([]byte("0123456789abcdef"), 8<<20) // 128 MiB
	a.write(t, "big.bin", big)
	a.svc.scanAll()
	t0 := time.Now()
	b.svc.syncAll()
	d := time.Since(t0)
	if !b.has("big.bin", big) {
		t.Fatal("B did not get the file")
	}
	t.Logf("128 MiB in %s: %.2f Gbit/s", d.Round(time.Millisecond), float64(len(big))*8/d.Seconds()/1e9)
}

func TestPooledClientIsReused(t *testing.T) {
	if pooledClientAs("127.0.0.1:1", "aa", nil) != pooledClientAs("127.0.0.1:1", "aa", nil) {
		t.Fatal("a new client for the same peer")
	}
	if pooledClientAs("127.0.0.1:1", "aa", nil) == pooledClientAs("127.0.0.1:2", "aa", nil) {
		t.Fatal("one client for two peers")
	}
}

// Many small files between two nodes, through the real transport: they arrive, and the time is logged.
func TestSmallFilesTransferRate(t *testing.T) {
	fdNoTell.Store(true)
	t.Cleanup(func() { fdNoTell.Store(false) })
	a, b := newFdTestNode(t, "A"), newFdTestNode(t, "B")
	b.join(a.tnode)
	a.sync()
	b.sync()
	const n = 2000
	for i := 0; i < n; i++ {
		a.write(t, fmt.Sprintf("d%d/f%d.txt", i%20, i), []byte(fmt.Sprintf("small file number %d, a few bytes of content", i)))
	}
	a.svc.scanAll()
	t0 := time.Now()
	for r := 0; r < 5; r++ {
		b.svc.syncAll()
	}
	d := time.Since(t0)
	for i := 0; i < n; i++ {
		if !b.has(fmt.Sprintf("d%d/f%d.txt", i%20, i), []byte(fmt.Sprintf("small file number %d, a few bytes of content", i))) {
			t.Fatalf("file %d missing on B", i)
		}
	}
	t.Logf("%d small files in %s: %.0f files/s", n, d.Round(time.Millisecond), float64(n)/d.Seconds())
}

// A node can be asked, over the cluster channel, which blocks of a file it holds: all of a file in place, none of another version.
func TestFilesHaveOverTheClusterChannel(t *testing.T) {
	fdNoTell.Store(true)
	t.Cleanup(func() { fdNoTell.Store(false) })
	a, b := newFdTestNode(t, "A"), newFdTestNode(t, "B")
	b.join(a.tnode)
	a.sync()
	b.sync()
	a.write(t, "big.bin", bytes.Repeat([]byte("0123456789abcdef"), 1<<18)) // 4 MiB
	a.svc.scanAll()
	fi, ok := a.svc.get("docs").f.Entry("big.bin")
	if !ok {
		t.Fatal("A does not hold the file")
	}
	peers := b.svc.otherPeers()
	if len(peers) != 1 {
		t.Fatalf("%d peers", len(peers))
	}
	src := b.svc.source(peers[0])
	av, err := src.Have("docs", "big.bin", fi.Version)
	if err != nil || !av.Complete {
		t.Fatalf("a file in place is held whole: %+v %v", av, err)
	}
	av, err = src.Have("docs", "big.bin", filesync.Vector{"nobody": 7})
	if err != nil || av.Complete || av.N != 0 {
		t.Fatalf("another version is not held: %+v %v", av, err)
	}
	if _, err := src.Have("nofolder", "big.bin", fi.Version); err == nil {
		t.Fatal("a folder that is not there should be an error")
	}
}
