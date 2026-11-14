//go:build !linux

package firecracker

import "errors"

// assertVMMCapabilities requires the Linux procfs; non-Linux builds exist
// for compilation and unit tests only (the Firecracker runtime needs KVM).
func assertVMMCapabilities(int) error {
	return errors.New("the VMM capability assertion requires Linux procfs")
}
