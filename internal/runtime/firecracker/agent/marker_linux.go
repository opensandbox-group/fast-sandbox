//go:build linux

package agent

import (
	"errors"

	"golang.org/x/sys/unix"
)

func setXattr(path, name string, value []byte) error {
	return unix.Setxattr(path, name, value, 0)
}

func getXattr(path, name string) ([]byte, error) {
	buffer := make([]byte, 512)
	for {
		size, err := unix.Getxattr(path, name, buffer)
		if errors.Is(err, unix.ERANGE) {
			buffer = make([]byte, len(buffer)*4)
			continue
		}
		if err != nil {
			return nil, err
		}
		return buffer[:size], nil
	}
}
