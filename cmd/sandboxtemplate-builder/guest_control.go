package main

import (
	"context"
	"debug/elf"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/artifacts"
	"fast-sandbox/internal/guestcontrol"
)

// Guest control ships with every native and OverlayBD template. Injection is
// followed by a freeze/thaw probe before the builder declares the capability.
// Existing templates require a rebuild to acquire verified helper metadata.
func injectGuestControl(spec apiv1alpha2.SandboxTemplateSpec, rootfs string) (bool, error) {
	if spec.Output.Format != apiv1alpha2.ArtifactFormatOverlayBD && spec.Output.Format != apiv1alpha2.ArtifactFormatNative {
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

func probeNativeGuestControl(workdir string) (*artifacts.VMGuestControl, error) {
	version, err := guestcontrol.ProbeRootFilesystem(context.Background(), filepath.Join(workdir, "guest-control.sock"), 1234)
	if err != nil {
		return nil, fmt.Errorf("native guest helper probe: %w", err)
	}
	return &artifacts.VMGuestControl{Protocol: guestcontrol.Protocol, Transport: "vsock", Port: 1234, HelperVersion: version}, nil
}
