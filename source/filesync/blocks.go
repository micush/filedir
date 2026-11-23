package filesync

import (
	"crypto/sha256"
	"io"
)

const (
	minBlock  = 128 << 10
	maxBlock  = 16 << 20
	bigFile   = 32 << 20 // a file of this size or more is cut in bigBlock pieces at least
	bigBlock  = 1 << 20
	maxBlocks = 2000 // aim for at most this many blocks a file: the bigger the file, the bigger its blocks
)

// BlockSize is the block size for a file of the given size.  Two nodes that disagree about it still work: a file is cut by the size
// its entry names, and a block is only reused where the hashes match.
func BlockSize(size int64) int {
	bs := int64(minBlock)
	if size >= bigFile { // a block is one request to a peer: a big file in 128 KB pieces spent its time on requests, not on bytes
		bs = bigBlock
	}
	for size/bs > maxBlocks && bs < maxBlock {
		bs *= 2
	}
	return int(bs)
}

// HashBlocks reads r to the end and returns its blocks.  It returns the bytes read, which is the file's size.
func HashBlocks(r io.Reader, size int64) ([]Block, int64, error) {
	bs := BlockSize(size)
	buf := make([]byte, bs)
	var blocks []Block
	var off int64
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			blocks = append(blocks, Block{Off: off, Size: int32(n), Hash: sha256.Sum256(buf[:n])})
			off += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return blocks, off, nil
		}
		if err != nil {
			return nil, off, err
		}
	}
}
