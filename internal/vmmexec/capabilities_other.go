//go:build !linux

package vmmexec

import "errors"

// The capability transitions are Linux prctl/capset operations; non-Linux
// builds exist for compilation and platform-neutral unit tests only.

// DropCapabilities is not supported off Linux.
func DropCapabilities() error {
	return errors.New("capability transitions require Linux")
}

// VerifyDropped is not supported off Linux.
func VerifyDropped() error {
	return errors.New("capability transitions require Linux")
}
