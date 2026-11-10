package main

import (
	"debug/elf"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

// Guest control ships with every OverlayBD (OCI) template: the builder injects
// the statically linked helper, guest-init starts it before user workloads, and
// fsb-vm's build-time freeze/thaw probe validates the stack end to end before
// the guestControl declaration is recorded. There is no opt-out: the capability
// is inert unless a capture uses the control channel, and the probe, not
// operator configuration, is the validation gate. Native (object-store)
// templates do not carry the helper because rootfs-only capture is
// OverlayBD-only. Existing templates are unchanged; rebuilding is the migration
// path. Injection alone does not declare support.
func injectGuestControl(spec apiv1alpha2.SandboxTemplateSpec, rootfs string) (bool, error) {
	if spec.Output.Format != apiv1alpha2.ArtifactFormatOverlayBD {
		return false, nil
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
