//go:build !amd64

package blake3

const haveHash4, haveHash8 = false, false

func hash4(in []byte, counter uint64) (out [4][8]uint32) { panic("blake3: no vector code here") }
func hash8(in []byte, counter uint64) (out [8][8]uint32) { panic("blake3: no vector code here") }

func parents4(cvs [][8]uint32) { panic("blake3: no vector code here") }
func parents8(cvs [][8]uint32) { panic("blake3: no vector code here") }
