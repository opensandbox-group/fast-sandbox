//go:build linux

package agent

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// dataChunkOffsets returns the chunk-aligned offsets of the chunks that
// overlap a data extent (SEEK_DATA/SEEK_HOLE). Chunks lying entirely in a
// hole are known zero without reading them; ok=false means the filesystem
// cannot answer and every chunk must be read.
func dataChunkOffsets(file *os.File, size, chunkSize int64) ([]int64, bool) {
	fd := int(file.Fd())
	var offsets []int64
	next := int64(0) // first chunk offset not yet emitted
	for position := int64(0); position < size; {
		dataStart, err := unix.Seek(fd, position, unix.SEEK_DATA)
		if err != nil {
			if errors.Is(err, unix.ENXIO) {
				break // only a hole remains
			}
			return nil, false
		}
		holeStart, err := unix.Seek(fd, dataStart, unix.SEEK_HOLE)
		if err != nil {
			return nil, false
		}
		first := max(dataStart/chunkSize*chunkSize, next)
		for offset := first; offset < holeStart && offset < size; offset += chunkSize {
			offsets = append(offsets, offset)
			next = offset + chunkSize
		}
		position = holeStart
	}
	return offsets, true
}
