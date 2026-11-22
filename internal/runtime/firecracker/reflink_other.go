//go:build !linux

package firecracker

import "os/exec"

// reflinkOnly attempts a metadata-only CoW clone of source to target. On
// non-Linux platforms FICLONE is unavailable, so we retain the cp probe and
// let copyReflinkOrCopy fall back to a plain copy when unsupported.
func reflinkOnly(source, target string) error {
	return exec.Command("cp", "--reflink=always", source, target).Run()
}
