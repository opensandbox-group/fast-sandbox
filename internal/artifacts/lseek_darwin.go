//go:build darwin

package artifacts

// lseek(2) SEEK_DATA/SEEK_HOLE constants — macOS: DATA=4, HOLE=3.
// (Linux uses the swapped values; see lseek_linux.go.)
const (
	seekDataConst = 4
	seekHoleConst = 3
)
