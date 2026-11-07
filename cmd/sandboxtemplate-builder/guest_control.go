package main

import (
	"debug/elf"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

// Explicitly enable the new capability; existing templates retain their startup scripts. Injection alone does not declare support.
func injectGuestControl(spec apiv1alpha2.SandboxTemplateSpec, rootfs string) (bool, error) {
	enabled := os.Getenv("FSB_GUEST_CONTROL_ENABLED")
	if enabled == "" || enabled == "0" {
		return false, nil
	}
	if enabled != "1" {
		return false, fmt.Errorf("FSB_GUEST_CONTROL_ENABLED must be 0 or 1")
	}
	if spec.Output.Format != apiv1alpha2.ArtifactFormatOverlayBD {
		return false, fmt.Errorf("guest control currently requires the OCI builder validation path")
	}
	source := os.Getenv("FSB_GUEST_CONTROL_HELPER")
	if source == "" {
		source = "/usr/local/lib/fsb-guest-control"
	}
	binary, err := elf.Open(source)
	if err != nil {
		return false, err
	}
	defer binary.Close()
	want := elf.EM_X86_64
	if runtime.GOARCH == "arm64" {
		want = elf.EM_AARCH64
	}
	if binary.Machine != want {
		return false, fmt.Errorf("helper architecture %s does not match builder", binary.Machine)
	}
	for _, program := range binary.Progs {
		if program.Type == elf.PT_INTERP {
			return false, fmt.Errorf("guest helper must be statically linked")
		}
	}
	payload, err := os.ReadFile(source)
	if err != nil {
		return false, err
	}
	root, err := os.OpenRoot(rootfs)
	if err != nil {
		return false, err
	}
	defer root.Close()
	name := "usr/local/sbin/fsb-guest-control"
	if err = root.MkdirAll(filepath.Dir(name), 0755); err != nil {
		return false, err
	}
	// Replace the old inode so image symlinks, hard links, or device nodes cannot redirect writes to other files.
	if err = root.Remove(name); err != nil && !os.IsNotExist(err) {
		return false, err
	}
	f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		return false, err
	}
	if _, err = f.Write(payload); err != nil {
		f.Close()
		return false, err
	}
	if err = f.Close(); err != nil {
		return false, err
	}
	if err = root.Chmod(name, 0755); err != nil {
		return false, err
	}
	return true, nil
}
