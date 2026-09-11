//go:build linux

package artifacts

// lseek(2) SEEK_DATA/SEEK_HOLE constants — Linux: DATA=3, HOLE=4.
// (macOS uses the swapped values; see lseek_darwin.go.)
const (
	seekDataConst = 3
	seekHoleConst = 4
)
