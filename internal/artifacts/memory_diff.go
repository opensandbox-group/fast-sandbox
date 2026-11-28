package artifacts

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// ApplyMemoryDiff overlays Firecracker's written extents on a disposable
// copy of its parent. A hole leaves the parent intact. Written zeros are data.
func ApplyMemoryDiff(basePath, diffPath string) error {
	diff, err := os.Open(diffPath)
	if err != nil {
		return err
	}
	defer diff.Close()
	base, err := os.OpenFile(basePath, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer base.Close()
	info, err := diff.Stat()
	if err != nil {
		return err
	}
	baseInfo, err := base.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size != baseInfo.Size() || os.SameFile(info, baseInfo) {
		return fmt.Errorf("memory diff and base must be distinct files of equal size")
	}
	for offset := int64(0); offset < size; {
		start, err := seekData(diff, offset)
		if errors.Is(err, syscall.ENXIO) {
			return nil
		}
		if err != nil {
			return err
		}
		end, err := seekHole(diff, start)
		if err != nil {
			return err
		}
		if start < offset || end <= start || end > size {
			return fmt.Errorf("invalid memory diff extent [%d, %d)", start, end)
		}
		section := io.NewSectionReader(diff, start, end-start)
		if n, err := io.Copy(io.NewOffsetWriter(base, start), section); err != nil {
			return err
		} else if n != end-start {
			return io.ErrUnexpectedEOF
		}
		offset = end
	}
	return nil
}

// SupportsMemoryDiffExtents checks the dump filesystem before pausing the
// VM. Filesystems that report holes as data cannot safely carry a Diff.
func SupportsMemoryDiffExtents(directory string) bool {
	page := int64(os.Getpagesize())
	probe, err := os.CreateTemp(directory, ".memory-extents-")
	if err != nil {
		return false
	}
	defer os.Remove(probe.Name())
	defer probe.Close()
	if err := probe.Truncate(3 * page); err != nil {
		return false
	}
	if _, err := probe.WriteAt(make([]byte, page), page); err != nil {
		return false
	}
	data, dataErr := seekData(probe, 0)
	hole, holeErr := seekHole(probe, page)
	return dataErr == nil && holeErr == nil && data == page && hole == 2*page
}
