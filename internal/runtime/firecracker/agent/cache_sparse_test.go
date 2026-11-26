package agent

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"testing/iotest"
)

// sparseFixture builds an input whose middle is a large all-zero hole and
// returns it together with its logical length.
func sparseFixture(head, hole, tail int) []byte {
	out := make([]byte, 0, head+hole+tail)
	out = append(out, bytes.Repeat([]byte("A"), head)...)
	out = append(out, make([]byte, hole)...)
	out = append(out, bytes.Repeat([]byte("B"), tail)...)
	return out
}

// allocatedBytes reports the physical bytes the file occupies on disk.
func allocatedBytes(t *testing.T, path string) int64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		t.Fatalf("fstat %s: %v", path, err)
	}
	return st.Blocks * 512
}

func runCopySparse(t *testing.T, input []byte) (string, int64, int64) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	written, err := copySparse(f, bytes.NewReader(input))
	if err != nil {
		t.Fatalf("copySparse: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path, written, allocatedBytes(t, path)
}

func assertMatchesInput(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("content mismatch: got %d bytes, want %d", len(got), len(want))
	}
	if sha256.Sum256(got) != sha256.Sum256(want) {
		t.Fatal("digest mismatch")
	}
}

func TestCopySparseKeepsContentAndShrinksAllocation(t *testing.T) {
	const head, hole, tail = 4096, 1 << 20, 8192
	input := sparseFixture(head, hole, tail)
	path, written, allocated := runCopySparse(t, input)
	if written != int64(len(input)) {
		t.Fatalf("written = %d, want %d", written, len(input))
	}
	assertMatchesInput(t, path, input)
	if allocated >= int64(len(input))/2 {
		t.Fatalf("allocation = %d bytes, want well under the %d-byte logical size (holes not preserved)", allocated, len(input))
	}
}

func TestCopySparseAllZeroFile(t *testing.T) {
	input := make([]byte, 1<<20)
	path, written, allocated := runCopySparse(t, input)
	if written != int64(len(input)) {
		t.Fatalf("written = %d, want %d", written, len(input))
	}
	assertMatchesInput(t, path, input)
	if allocated >= int64(len(input))/4 {
		t.Fatalf("all-zero file allocated %d bytes, want near zero", allocated)
	}
}

func TestCopySparseTrailingZeros(t *testing.T) {
	const head, tail = 64 << 10, 256 << 10
	input := make([]byte, 0, head+tail)
	input = append(input, bytes.Repeat([]byte("A"), head)...)
	input = append(input, make([]byte, tail)...)
	path, written, allocated := runCopySparse(t, input)
	if written != int64(len(input)) {
		t.Fatalf("written = %d, want %d (trailing hole must keep the logical length)", written, len(input))
	}
	assertMatchesInput(t, path, input)
	if allocated >= int64(len(input))/2 {
		t.Fatalf("trailing hole allocated %d bytes, want well under %d", allocated, len(input))
	}
}

func TestCopySparseEmptyInput(t *testing.T) {
	path, written, _ := runCopySparse(t, nil)
	if written != 0 {
		t.Fatalf("written = %d, want 0", written)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("size = %d, want 0", info.Size())
	}
}

func TestCopySparseDenseDataStillCorrect(t *testing.T) {
	input := bytes.Repeat([]byte{0x5a}, sparseChunkSize*3+17)
	path, written, _ := runCopySparse(t, input)
	if written != int64(len(input)) {
		t.Fatalf("written = %d, want %d", written, len(input))
	}
	assertMatchesInput(t, path, input)
}

func TestCopySparseShortReadsSameResult(t *testing.T) {
	input := sparseFixture(8192, 96<<10, 4096)
	dir := t.TempDir()
	path := filepath.Join(dir, "short.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// A reader that hands out one byte at a time exercises the short-read
	// path of the chunk reader.
	slow := io.MultiReader(bytes.NewReader(input[:1]), iotest.OneByteReader(bytes.NewReader(input[1:])))
	if _, err := copySparse(f, iotest.OneByteReader(slow)); err != nil {
		t.Fatalf("copySparse: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	assertMatchesInput(t, path, input)
}

func TestCopySparseReadError(t *testing.T) {
	sentinel := errors.New("transient read failure")
	input := bytes.Repeat([]byte{0x41}, 4096)
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	_, err = copySparse(f, io.MultiReader(bytes.NewReader(input), errReader{sentinel}))
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
