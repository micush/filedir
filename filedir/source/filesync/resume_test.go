package filesync

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// flakyPeer serves some blocks and then the network is gone.
type flakyPeer struct {
	swarmPeer
	okFor int64 // blocks it serves before it fails; -1: it never fails
	asked atomic.Int64
}

func (p *flakyPeer) Block(folder, name string, off int64, size int, h Hash) ([]byte, error) {
	if p.okFor >= 0 && p.asked.Add(1) > p.okFor {
		return nil, errors.New("network is down")
	}
	return p.swarmPeer.Block(folder, name, off, size, h)
}

// A pull that fails half way (the network is gone for hours) keeps what arrived: the next try fetches only the blocks that are missing.
func TestPullResumesAfterTheNetworkWasDown(t *testing.T) {
	a := newNode(t, "a")
	data := writeBig(t, a, "big.bin", 40)
	b := newNode(t, "b")
	flaky := &flakyPeer{swarmPeer: swarmPeer{f: a.f}, okFor: 25}
	b.f.SyncFrom("a", flaky) // fails: the file is not there, and not partly there either
	if _, err := os.Stat(b.path("big.bin")); err == nil {
		t.Fatal("a file that did not arrive is in the folder")
	}
	parts, _ := filepath.Glob(filepath.Join(b.f.root, MetaDir, "tmp", "part-*"))
	if len(parts) != 1 {
		t.Fatalf("partial files after the failed pull: %v", parts)
	}
	honest := &swarmPeer{f: a.f}
	if _, err := b.f.SyncFrom("a", honest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(b.path("big.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the file did not arrive (%v)", err)
	}
	if n := honest.served.Load(); n > 20 {
		t.Fatalf("the second try fetched %d blocks of 40: what was here was not used", n)
	}
	if parts, _ := filepath.Glob(filepath.Join(b.f.root, MetaDir, "tmp", "part-*")); len(parts) != 0 {
		t.Fatalf("partial file left after the file arrived: %v", parts)
	}
}

// What is in a partial file is checked, not trusted: a block that is wrong in it is fetched again and the file is right.
func TestPullDoesNotTrustAPartialFile(t *testing.T) {
	a := newNode(t, "a")
	data := writeBig(t, a, "big.bin", 20)
	b := newNode(t, "b")
	b.f.SyncFrom("a", &flakyPeer{swarmPeer: swarmPeer{f: a.f}, okFor: 12})
	parts, _ := filepath.Glob(filepath.Join(b.f.root, MetaDir, "tmp", "part-*"))
	if len(parts) != 1 {
		t.Fatalf("partial files: %v", parts)
	}
	fh, err := os.OpenFile(parts[0], os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	fh.WriteAt([]byte("garbage"), 0)
	fh.WriteAt([]byte("garbage"), 5*minBlock+3)
	fh.Close()
	if _, err := b.f.SyncFrom("a", &swarmPeer{f: a.f}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(b.path("big.bin"))
	if !bytes.Equal(got, data) {
		t.Fatal("the file differs: a bad block of the partial file was used")
	}
}

// A partial file that nobody came back to is removed by Maintain once it is old enough, and not before.
func TestMaintainDropsOldPartialFiles(t *testing.T) {
	b := newNode(t, "b")
	tdir := filepath.Join(b.f.root, MetaDir, "tmp")
	os.MkdirAll(tdir, 0o700)
	fresh, old := filepath.Join(tdir, "part-fresh"), filepath.Join(tdir, "part-old")
	os.WriteFile(fresh, []byte("x"), 0o600)
	os.WriteFile(old, []byte("x"), 0o600)
	when := time.Now().Add(-partKeep - time.Hour)
	os.Chtimes(old, when, when)
	if err := b.f.Maintain(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("a fresh partial file was removed")
	}
	if _, err := os.Stat(old); err == nil {
		t.Fatal("an old partial file was kept")
	}
}
