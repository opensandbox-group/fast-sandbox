//go:build !linux

package firecracker

import (
	"context"
	"errors"
	"os"
)

func captureProcessIdentity(context.Context, string, int) (string, error) {
	return "", errors.New("RootfsOnly capture requires Linux")
}

func captureSocketPID(context.Context, string) (int, error) { return 0, errors.New("requires Linux") }

func capturePIDAbsent(int) (bool, error) { return false, errors.New("requires Linux") }

func captureRootfsUpper(context.Context, rootfsCapture, *os.File, string, string, string) error {
	return errors.New("rootfs capture requires Linux")
}
