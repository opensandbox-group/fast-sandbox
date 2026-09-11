// Package artifacts holds the artifact-set conventions shared by every
// producer and consumer of SandboxTemplate-style snapshot artifacts: the
// golden-image builder (cmd/sandboxtemplate-builder), the live-snapshot
// firecracker driver, and the node runtime-agent. Anything here is a wire
// format or a byte-exact layout invariant — changing it desynchronizes
// already-published artifact sets.
package artifacts

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"syscall"
)

// SHA256File hashes a file, skipping sparse holes by feeding zero bytes for
// them (holes are semantically zero), so multi-GiB sparse roots are not read
// in full. Dense files fall back to a plain full read.
func SHA256File(path string) (string, error) {
	handle, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer handle.Close()
	size, err := handle.Seek(0, io.SeekEnd)
	if err != nil {
		return "", err
	}
	if _, err := handle.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	digest := sha256.New()
	// Fragmented sparse files (e.g. Firecracker memory snapshots with
	// 4K-granular holes) are faster hashed with a plain full read: the
	// per-segment lseek overhead dominates. Decide by the data ratio
	// instead of segment count: coarsely sparse files (small data ratio)
	// keep the SEEK_DATA path, dense files fall back to a full read.
	dataBytes, _, err := scanSparse(handle, size)
	if err != nil || dataBytes*2 >= size {
		if _, err := handle.Seek(0, io.SeekStart); err != nil {
			return "", err
		}
		if _, err := io.CopyBuffer(digest, handle, hashBuffer); err != nil {
			return "", err
		}
		return hex.EncodeToString(digest.Sum(nil)), nil
	}
	if _, err := handle.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	zeros := make([]byte, 256*1024)
	consumed := int64(0)
	for consumed < size {
		dataStart, dataErr := seekData(handle, consumed)
		if dataErr != nil {
			// ENXIO: no data until EOF — hash the rest as zeros.
			if dataErr == syscall.ENXIO {
				if err := feedZeros(digest, size-consumed, zeros); err != nil {
					return "", err
				}
				consumed = size
				break
			}
			// Filesystem without SEEK_DATA support (or a state change
			// between the scan and this pass): fall back to a full read with
			// a FRESH digest — hashing into the partially-filled one would
			// silently corrupt the checksum.
			if _, err := handle.Seek(0, io.SeekStart); err != nil {
				return "", err
			}
			digest = sha256.New()
			if _, err := io.CopyBuffer(digest, handle, hashBuffer); err != nil {
				return "", err
			}
			break
		}
		if dataStart > consumed {
			if err := feedZeros(digest, dataStart-consumed, zeros); err != nil {
				return "", err
			}
			consumed = dataStart
		}
		// Locate the end of this data extent, then read it from the start:
		// seekHole moves the file offset, so seek back before hashing.
		holeStart, holeErr := seekHole(handle, dataStart)
		if holeErr != nil {
			holeStart = size
		}
		if _, err := handle.Seek(dataStart, io.SeekStart); err != nil {
			return "", err
		}
		if _, err := io.CopyBuffer(digest, io.LimitReader(handle, holeStart-dataStart), hashBuffer); err != nil {
			return "", err
		}
		consumed = holeStart
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// hashBuffer keeps a large read buffer for hashing so high-latency storage
// does not dominate the phase with syscall round trips.
var hashBuffer = make([]byte, 4*1024*1024)

// scanSparse walks the SEEK_DATA/SEEK_HOLE map of a file without reading
// payload and returns the total data bytes and the segment count. ENXIO is
// the normal end-of-file condition, not an error.
func scanSparse(file *os.File, size int64) (dataBytes int64, segments int, err error) {
	offset := int64(0)
	for offset < size {
		dataStart, err := seekData(file, offset)
		if err != nil {
			if err == syscall.ENXIO {
				break
			}
			return dataBytes, segments, err
		}
		holeStart, err := seekHole(file, dataStart)
		if err != nil {
			holeStart = size
		}
		dataBytes += holeStart - dataStart
		segments++
		// Already dense enough to decide: stop scanning early.
		if dataBytes*2 >= size {
			return dataBytes, segments, nil
		}
		offset = holeStart
	}
	return dataBytes, segments, nil
}

// feedZeros feeds n zero bytes into the digest without allocating n bytes.
func feedZeros(digest io.Writer, n int64, chunk []byte) error {
	for n > 0 {
		k := int64(len(chunk))
		if n < k {
			k = n
		}
		if _, err := digest.Write(chunk[:k]); err != nil {
			return err
		}
		n -= k
	}
	return nil
}

// seekData and seekHole wrap lseek(2) SEEK_DATA/SEEK_HOLE; the constants
// differ per OS (Linux DATA=3/HOLE=4, macOS DATA=4/HOLE=3), see
// lseek_linux.go / lseek_darwin.go.
func seekData(file *os.File, offset int64) (int64, error) {
	position, _, errno := syscall.Syscall(syscall.SYS_LSEEK, file.Fd(), uintptr(offset), seekDataConst)
	if errno != 0 {
		return 0, errno
	}
	return int64(position), nil
}

// seekHole wraps lseek(2) SEEK_HOLE (see lseek_linux.go / lseek_darwin.go).
func seekHole(file *os.File, offset int64) (int64, error) {
	position, _, errno := syscall.Syscall(syscall.SYS_LSEEK, file.Fd(), uintptr(offset), seekHoleConst)
	if errno != 0 {
		return 0, errno
	}
	return int64(position), nil
}
