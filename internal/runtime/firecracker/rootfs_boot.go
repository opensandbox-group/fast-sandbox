package firecracker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"fast-sandbox/internal/artifacts"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"fast-sandbox/internal/firecrackerconfig"
)

// Cold boot uses only a resolved rootfs of the same architecture; it does not reuse the source host CPU template or register state.
func checkRootfsBootCompatibility(m artifacts.VMManifest, version string) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if !m.IsRootfsOnly() || m.Compatibility.CPUArch != artifacts.HostCPUArch() {
		return fmt.Errorf("%w: RootfsOnly architecture mismatch", ErrIncompatibleArtifact)
	}
	if (version != rootfsFirecrackerVersion && version != managedRootfsCaptureVersion) || m.Compatibility.FirecrackerVersion != version {
		return fmt.Errorf("%w: RootfsOnly requires a supported, matching Firecracker version", ErrIncompatibleArtifact)
	}
	return nil
}
func (d *Driver) rootfsKernel(m artifacts.VMManifest) (string, error) {
	return firecrackerconfig.ResolveKernel("/usr/local/share/firecracker", m.Kernel.Version, d.config.KernelPath)
}
func (d *Driver) prepareRootfsBootInstance(id, directory, rootfs, kernel string) (devicePath, jailPath, socketPath string, resultErr error) {
	if d.config.JailerPath == "" {
		return d.prepareOCIInstance(id, directory, rootfs, "", "")
	}
	root := jailerRoot(filepath.Join(d.config.StateRoot, jailerChrootBaseDir), filepath.Base(d.config.BinaryPath), truncatedSandboxID(id))
	if err := os.MkdirAll(root, 0750); err != nil {
		return "", "", "", err
	}
	// Boot has not started; roll back existing mounts if any bind step fails.
	defer func() {
		if resultErr != nil {
			d.removeJailRoot(id)
		}
	}()
	for _, entry := range []struct{ source, target string }{{rootfs, rootfsDeviceName}, {kernel, "rootfs-kernel"}} {
		target := filepath.Join(root, entry.target)
		f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR, 0640)
		if err != nil {
			return "", "", "", err
		}
		if err = f.Close(); err != nil {
			return "", "", "", err
		}
		if err = bindMount(entry.source, target); err != nil {
			return "", "", "", err
		}
	}
	return filepath.Join(root, rootfsDeviceName), root, filepath.Join(root, "api.sock"), nil
}
func configureRootfsVM(ctx context.Context, c *Client, slot *fastletnetwork.Slot, m artifacts.VMManifest, kernel string, jailed bool) error {
	if slot == nil || slot.GuestTap == "" {
		return ErrNetworkUnavailable
	}
	if err := requireStaticGuestNetwork(m.GuestNetwork); err != nil {
		return err
	}
	if jailed {
		kernel = "/rootfs-kernel"
	}
	ops, err := firecrackerconfig.RootfsBoot(m, kernel, rootfsDeviceName, slot.GuestTap, "guest-control.sock")
	if err != nil {
		return err
	}
	for _, op := range ops {
		if err := c.do(ctx, "PUT", op.Path, op.Body, nil); err != nil {
			return err
		}
	}
	return nil
}
