//go:build !linux

package firecracker

import "fmt"

func stateRootFilesystemType(string) (string, error) {
	return "", fmt.Errorf("Firecracker StateRoot requires Linux and XFS with reflink=1")
}
