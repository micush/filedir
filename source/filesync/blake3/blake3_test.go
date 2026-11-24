package blake3

import (
	"encoding/hex"
	"os/exec"
	"strings"
	"testing"
)

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*251 + i>>8)
	}
	return b
}

// The reference values come from the reference implementation (the Python package "blake3"), made by the test when it is installed.
func reference(t *testing.T, sizes []int) map[int]string {
	t.Helper()
	var sb strings.Builder
	for _, n := range sizes {
		sb.WriteString(itoa(n) + " ")
	}
	script := "import blake3,sys\nfor n in map(int,sys.argv[1:]):\n b=bytes(((i*251+(i>>8))&255) for i in range(n))\n print(n,blake3.blake3(b).hexdigest())\n"
	cmd := exec.Command("python3", append([]string{"-c", script}, strings.Fields(sb.String())...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Skip("no reference implementation (python3 with the blake3 package):", err)
	}
	m := map[int]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(l)
		m[atoi(f[0])] = f[1]
	}
	return m
}

func itoa(n int) string { return fmtInt(n) }
func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}
func atoi(s string) (n int) {
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return
}

var testSizes = func() []int {
	sz := []int{0, 1, 63, 64, 65, 127, 128, 100000, 131072, 131073, 1<<20 + 77, 4 << 20, 16 << 20}
	for c := 1; c <= 70; c++ { // every number of chunks, with the edges of the last
		sz = append(sz, c*ChunkSize-1, c*ChunkSize, c*ChunkSize+1)
	}
	for _, c := range []int{127, 128, 129, 255, 256, 257, 511, 512, 513, 1023, 1024, 1025} {
		sz = append(sz, c*ChunkSize, c*ChunkSize+500)
	}
	return sz
}()

// modes: plain Go; SSSE3 four chunks at a time; AVX2 eight at a time (each only where the processor has it)
func withModes(f func(mode string)) {
	o4, o8 := haveHash4, haveHash8
	defer func() { haveHash4, haveHash8 = o4, o8 }()
	haveHash4, haveHash8 = false, false
	f("plain")
	if o4 {
		haveHash4 = true
		f("ssse3")
	}
	if o8 {
		haveHash8 = true
		f("avx2")
	}
}

func TestSumMatchesTheReference(t *testing.T) {
	want := reference(t, testSizes)
	withModes(func(mode string) {
		for _, n := range testSizes {
			got := Sum256(pattern(n))
			if hex.EncodeToString(got[:]) != want[n] {
				t.Errorf("%s, size %d: got %x want %s", mode, n, got, want[n])
			}
		}
	})
}

func BenchmarkSum256(b *testing.B) {
	data := pattern(1 << 20)
	withModes(func(mode string) {
		b.Run(mode, func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			for i := 0; i < b.N; i++ {
				Sum256(data)
			}
		})
	})
}
