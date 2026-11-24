// Package blake3 is BLAKE3 (the 256-bit hash of a whole message, nothing else of it) for the block hashes of the sync engine.  It is
// written here, with the standard library only: a plain Go version of the compression function that works everywhere, and on amd64 a
// version in assembly that hashes four chunks of a message at a time with SSSE3 vector instructions, which a CPU without the SHA
// extensions (that SHA-256 can use) gets several times the speed of SHA-256 from.  The result is the same either way and is the one of
// the reference implementation; the tests check it against that on many sizes.
package blake3

import "math/bits"

const (
	// ChunkSize is the size of a chunk of the message, the unit the tree is made of.
	ChunkSize = 1024
	blockSize = 64
	// Size is the length of the hash.
	Size = 32

	flagChunkStart = 1 << 0
	flagChunkEnd   = 1 << 1
	flagParent     = 1 << 2
	flagRoot       = 1 << 3
)

var iv = [8]uint32{0x6A09E667, 0xBB67AE85, 0x3C6EF372, 0xA54FF53A, 0x510E527F, 0x9B05688C, 0x1F83D9AB, 0x5BE0CD19}

var msgPerm = [16]int{2, 6, 3, 10, 7, 0, 4, 13, 1, 11, 12, 5, 9, 14, 15, 8}

// schedule[r] is the order the message words are used in round r.
var schedule = func() (s [7][16]int) {
	for i := range s[0] {
		s[0][i] = i
	}
	for r := 1; r < 7; r++ {
		for i := range s[r] {
			s[r][i] = s[r-1][msgPerm[i]]
		}
	}
	return
}()

func g(v *[16]uint32, a, b, c, d int, mx, my uint32) {
	v[a] = v[a] + v[b] + mx
	v[d] = bits.RotateLeft32(v[d]^v[a], -16)
	v[c] = v[c] + v[d]
	v[b] = bits.RotateLeft32(v[b]^v[c], -12)
	v[a] = v[a] + v[b] + my
	v[d] = bits.RotateLeft32(v[d]^v[a], -8)
	v[c] = v[c] + v[d]
	v[b] = bits.RotateLeft32(v[b]^v[c], -7)
}

// compress is the compression function; it returns the eight words of the chaining value that follows.
func compress(cv *[8]uint32, m *[16]uint32, counter uint64, blen, flags uint32) [8]uint32 {
	v := [16]uint32{cv[0], cv[1], cv[2], cv[3], cv[4], cv[5], cv[6], cv[7], iv[0], iv[1], iv[2], iv[3], uint32(counter), uint32(counter >> 32), blen, flags}
	for r := 0; r < 7; r++ {
		s := &schedule[r]
		g(&v, 0, 4, 8, 12, m[s[0]], m[s[1]])
		g(&v, 1, 5, 9, 13, m[s[2]], m[s[3]])
		g(&v, 2, 6, 10, 14, m[s[4]], m[s[5]])
		g(&v, 3, 7, 11, 15, m[s[6]], m[s[7]])
		g(&v, 0, 5, 10, 15, m[s[8]], m[s[9]])
		g(&v, 1, 6, 11, 12, m[s[10]], m[s[11]])
		g(&v, 2, 7, 8, 13, m[s[12]], m[s[13]])
		g(&v, 3, 4, 9, 14, m[s[14]], m[s[15]])
	}
	var out [8]uint32
	for i := range out {
		out[i] = v[i] ^ v[i+8]
	}
	return out
}

func words(b []byte) (m [16]uint32) {
	var tmp [blockSize]byte
	copy(tmp[:], b)
	for i := range m {
		m[i] = uint32(tmp[4*i]) | uint32(tmp[4*i+1])<<8 | uint32(tmp[4*i+2])<<16 | uint32(tmp[4*i+3])<<24
	}
	return
}

// chunkCV is the chaining value of one chunk (up to 1024 bytes) as chunk number counter; extra is flagRoot when the chunk is the whole
// message, which marks the last block.
func chunkCV(chunk []byte, counter uint64, extra uint32) [8]uint32 {
	cv := iv
	n := (len(chunk) + blockSize - 1) / blockSize
	if n == 0 {
		n = 1
	}
	for i := 0; i < n; i++ {
		blk := chunk[i*blockSize:]
		if len(blk) > blockSize {
			blk = blk[:blockSize]
		}
		var fl uint32
		if i == 0 {
			fl |= flagChunkStart
		}
		if i == n-1 {
			fl |= flagChunkEnd | extra
		}
		m := words(blk)
		cv = compress(&cv, &m, counter, uint32(len(blk)), fl)
	}
	return cv
}

func parentCV(l, r *[8]uint32, extra uint32) [8]uint32 {
	var m [16]uint32
	copy(m[:8], l[:])
	copy(m[8:], r[:])
	return compress(&iv, &m, 0, blockSize, flagParent|extra)
}

func toBytes(cv [8]uint32) (out [Size]byte) {
	for i, w := range cv {
		out[4*i], out[4*i+1], out[4*i+2], out[4*i+3] = byte(w), byte(w>>8), byte(w>>16), byte(w>>24)
	}
	return
}

// Sum256 returns the BLAKE3 hash of b.
func Sum256(b []byte) [Size]byte {
	if len(b) <= ChunkSize {
		return toBytes(chunkCV(b, 0, flagRoot))
	}
	chunks := (len(b) + ChunkSize - 1) / ChunkSize
	var small [64][8]uint32
	cvs := small[:0]
	if chunks > len(small) {
		cvs = make([][8]uint32, 0, chunks)
	}
	cvs = cvs[:chunks]
	full := chunks - 1 // the last chunk may be short
	i := 0
	for ; i+8 <= full && haveHash8; i += 8 {
		out := hash8(b[i*ChunkSize:(i+8)*ChunkSize], uint64(i))
		copy(cvs[i:], out[:])
	}
	for ; i+4 <= full && haveHash4; i += 4 {
		out := hash4(b[i*ChunkSize:(i+4)*ChunkSize], uint64(i))
		copy(cvs[i:], out[:])
	}
	for ; i < full; i++ {
		cvs[i] = chunkCV(b[i*ChunkSize:(i+1)*ChunkSize], uint64(i), 0)
	}
	cvs[full] = chunkCV(b[full*ChunkSize:], uint64(full), 0)
	// The tree is made a level at a time: neighbours are joined, and an odd one at the end goes up as it is.  This is the tree of the
	// definition (the left side of each node is the largest power of two of chunks that leaves something for the right).
	for n := chunks; n > 2; n = (n + 1) / 2 {
		pairs := n / 2
		j := 0
		for ; j+8 <= pairs && haveHash8; j += 8 {
			parents8(cvs[2*j : 2*j+16])
			// the results are in the first 8 places of this window; move them to j.. (j <= 2j, and the window is done with)
			copy(cvs[j:j+8], cvs[2*j:2*j+8])
		}
		for ; j+4 <= pairs && haveHash4; j += 4 {
			parents4(cvs[2*j : 2*j+8])
			copy(cvs[j:j+4], cvs[2*j:2*j+4])
		}
		for ; j < pairs; j++ {
			cvs[j] = parentCV(&cvs[2*j], &cvs[2*j+1], 0)
		}
		if n&1 == 1 {
			cvs[pairs] = cvs[n-1]
		}
	}
	return toBytes(parentCV(&cvs[0], &cvs[1], flagRoot))
}
