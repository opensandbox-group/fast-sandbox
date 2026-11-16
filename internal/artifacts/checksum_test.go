package artifacts

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestSha256FileSparse verifies the sparse-aware hasher produces the same
// digest as a plain full read on a file with holes (regression: the SEEK_DATA
// path previously hashed from the wrong offset).
func TestSha256FileSparse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sparse.bin")
	handle, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	payload := []byte("hello-sparse-world-0123456789")
	for _, offset := range []int64{0, 4 << 20, 16 << 20, 40 << 20, 63 << 20} {
		if _, err := handle.Seek(offset, 0); err != nil {
			t.Fatalf("seek: %v", err)
		}
		if _, err := handle.Write(payload); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if err := handle.Truncate(64 << 20); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reference: hash the file as a plain byte stream (holes read as zeros).
	handle, err = os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	reference := sha256.New()
	if _, err := io.Copy(reference, handle); err != nil {
		t.Fatalf("reference hash: %v", err)
	}
	_ = handle.Close()
	want := hex.EncodeToString(reference.Sum(nil))

	got, err := SHA256File(path)
	if err != nil {
		t.Fatalf("SHA256File: %v", err)
	}
	if got != want {
		t.Fatalf("sparse hash mismatch: got %s want %s", got, want)
	}
}

// TestSha256FileConcurrent hashes sparse files of different content
// concurrently: every digest must equal its sequential one (regression: the
// SEEK_DATA path read through a package-global buffer shared by concurrent
// calls, e.g. the rootfs hashes of two snapshots assembled at once).
func TestSha256FileConcurrent(t *testing.T) {
	dir := t.TempDir()
	var paths, want []string
	for i := range 6 {
		path := filepath.Join(dir, "sparse-"+string(rune('a'+i)))
		size := int64(32 << 20)
		handle, err := os.Create(path)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := handle.Truncate(size); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		full := make([]byte, size)
		// Two data extents, well under half the file: SHA256File takes the
		// SEEK_DATA path (holes fed as zeros, extents read through the buffer).
		for _, offset := range []int64{4 << 20, 20 << 20} {
			block := make([]byte, 5<<20)
			for j := range block {
				block[j] = byte(i*31 + j + int(offset>>20))
			}
			if _, err := handle.WriteAt(block, offset); err != nil {
				t.Fatalf("write: %v", err)
			}
			copy(full[offset:], block)
		}
		if err := handle.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		sum := sha256.Sum256(full)
		paths = append(paths, path)
		want = append(want, hex.EncodeToString(sum[:]))
	}
	got := make([]string, len(paths))
	errs := make([]error, len(paths))
	for round := 0; round < 4; round++ {
		var wg sync.WaitGroup
		for i, path := range paths {
			wg.Add(1)
			go func() {
				defer wg.Done()
				got[i], errs[i] = SHA256File(path)
			}()
		}
		wg.Wait()
		for i := range paths {
			if errs[i] != nil || got[i] != want[i] {
				t.Fatalf("round %d file %d: got %q (%v), want %q", round, i, got[i], errs[i], want[i])
			}
		}
	}
}
