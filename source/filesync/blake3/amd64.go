//go:build amd64

package blake3

// cpuid is the CPUID instruction, and xgetbv XGETBV (cpuid_amd64.s).
func cpuid(eaxArg, ecxArg uint32) (eax, ebx, ecx, edx uint32)
func xgetbv() (eax, edx uint32)

// hash4ssse3 and hash8avx2 are in hash4_amd64.s and hash8_amd64.s, made by gen_amd64.py.
//
//go:noescape
func hash4ssse3(in *byte, ctr *[8]uint32, out *[8][4]uint32)

//go:noescape
func hash8avx2(in *byte, ctr *[16]uint32, out *[8][8]uint32)

//go:noescape
func parents4ssse3(in *uint32, ctr *[8]uint32, out *[8][4]uint32)

//go:noescape
func parents8avx2(in *uint32, ctr *[16]uint32, out *[8][8]uint32)

var (
	// haveHash4: SSSE3 is on every x86-64 processor since 2006.
	haveHash4 = func() bool {
		_, _, ecx, _ := cpuid(1, 0)
		return ecx&(1<<9) != 0
	}()
	// haveHash8: AVX2, and the system saves the vector registers.
	haveHash8 = func() bool {
		_, _, ecx, _ := cpuid(1, 0)
		if ecx&(1<<27) == 0 || ecx&(1<<28) == 0 { // OSXSAVE, AVX
			return false
		}
		if x, _ := xgetbv(); x&6 != 6 {
			return false
		}
		_, ebx, _, _ := cpuid(7, 0)
		return ebx&(1<<5) != 0
	}()
)

// hash4 gives the chaining values of four chunks in a row (in is 4096 bytes); counter is the number of the first.
func hash4(in []byte, counter uint64) (out [4][8]uint32) {
	var ctr [8]uint32
	for k := 0; k < 4; k++ {
		c := counter + uint64(k)
		ctr[k], ctr[4+k] = uint32(c), uint32(c>>32)
	}
	var v [8][4]uint32
	hash4ssse3(&in[0], &ctr, &v)
	for w := 0; w < 8; w++ {
		for k := 0; k < 4; k++ {
			out[k][w] = v[w][k]
		}
	}
	return
}

// hash8 is hash4 for eight chunks (8192 bytes).
func hash8(in []byte, counter uint64) (out [8][8]uint32) {
	var ctr [16]uint32
	for k := 0; k < 8; k++ {
		c := counter + uint64(k)
		ctr[k], ctr[8+k] = uint32(c), uint32(c>>32)
	}
	var v [8][8]uint32
	hash8avx2(&in[0], &ctr, &v)
	for w := 0; w < 8; w++ {
		for k := 0; k < 8; k++ {
			out[k][w] = v[w][k]
		}
	}
	return
}

// parents4 and parents8 make the parent nodes of pairs of chaining values: cvs holds 2*lanes of them in a row, and the results are put
// in the first lanes places.
func parents4(cvs [][8]uint32) {
	var ctr [8]uint32
	var v [8][4]uint32
	parents4ssse3(&cvs[0][0], &ctr, &v)
	for w := 0; w < 8; w++ {
		for k := 0; k < 4; k++ {
			cvs[k][w] = v[w][k]
		}
	}
}

func parents8(cvs [][8]uint32) {
	var ctr [16]uint32
	var v [8][8]uint32
	parents8avx2(&cvs[0][0], &ctr, &v)
	for w := 0; w < 8; w++ {
		for k := 0; k < 8; k++ {
			cvs[k][w] = v[w][k]
		}
	}
}
