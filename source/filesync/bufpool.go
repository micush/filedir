package filesync

import (
	"math/bits"
	"sync"
)

// Blocks of a big file are megabytes, and moving a file at several Gbit/s allocates and clears hundreds of megabytes a second (and has
// the collector sweep them) if every block gets a fresh buffer.  The buffers that are handed back are kept in pools by size: a block's
// size is a power of two (see blockSize), so a class is one size.

const bufMinClass = 16 // blocks under 64 KB are not worth pooling

var bufPools [25]sync.Pool // class k: buffers of 1<<k bytes, up to maxBlock

// GetBuf returns a buffer of n bytes.  Its contents are not cleared.  Hand it back with PutBuf when nothing refers to it any more.
func GetBuf(n int) []byte {
	if n <= 0 || n > maxBlock {
		return make([]byte, n)
	}
	k := bits.Len(uint(n - 1))
	if k < bufMinClass {
		return make([]byte, n)
	}
	if p, ok := bufPools[k].Get().(*[]byte); ok {
		return (*p)[:n]
	}
	return make([]byte, n, 1<<k)
}

// PutBuf hands back a buffer that GetBuf gave out.  The caller must not touch it afterwards, and must give back only what GetBuf
// returned: a buffer that is not of a class's size is left to the collector.
func PutBuf(b []byte) {
	c := cap(b)
	if c < 1<<bufMinClass || c > maxBlock || c&(c-1) != 0 {
		return
	}
	b = b[:c]
	bufPools[bits.Len(uint(c))-1].Put(&b)
}

// BlockRecycler marks a Source whose Block gives buffers that came from GetBuf and that nothing else keeps: the one that fetched the
// block hands it back with PutBuf when it has written it.
type BlockRecycler interface{ RecyclesBlocks() }
