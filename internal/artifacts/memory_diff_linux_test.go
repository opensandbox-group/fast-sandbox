package artifacts

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyMemoryDiffPreservesHolesAndOverwritesZeroPages(t *testing.T) {
	const page = 4096
	dir := t.TempDir()
	base := filepath.Join(dir, "base")
	diff := filepath.Join(dir, "diff")
	original := bytes.Repeat([]byte{0xa5}, 4*page)
	if err := os.WriteFile(base, original, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(diff)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(int64(len(original))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(make([]byte, page), page); err != nil {
		t.Fatal(err)
	}
	changed := bytes.Repeat([]byte{0x5a}, page)
	if _, err := f.WriteAt(changed, 3*page); err != nil {
		t.Fatal(err)
	}
	if err := ApplyMemoryDiff(base, diff); err != nil {
		t.Fatal(err)
	}
	clear(original[page : 2*page])
	copy(original[3*page:], changed)
	got, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("merged memory does not preserve holes and overwrite written pages")
	}
}
