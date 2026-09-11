package artifacts

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
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
