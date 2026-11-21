package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"fast-sandbox/internal/artifacts"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"fast-sandbox/internal/firecrackerconfig"
	"fast-sandbox/internal/ocitemplate"
)

var errIncompleteRootfsStart = errors.New("cannot adopt interrupted rootfs boot")

// verifyBootMachine reads the live VMM through the same API socket used for
// configuration. Full restores and ordinary boots have no override contract.
func verifyBootMachine(ctx context.Context, c *Client, state *SandboxState) error {
	if state.Config.Desired.RootfsBootDigest == "" {
		return nil
	}
	want := state.BootMachine
	if want == nil || want.VCPUCount <= 0 || want.MemoryMiB <= 0 {
		return fmt.Errorf("%w: missing rootfs boot shape for %s", ErrInvalidConfig, state.Config.Identity.SandboxUID)
	}
	got, err := c.DescribeMachine(ctx)
	if err != nil {
		return fmt.Errorf("read back rootfs boot shape: %w", err)
	}
	if got.VCPUs != want.VCPUCount || int64(got.MemSizeMiB) != want.MemoryMiB {
		return fmt.Errorf("%w: expected %d vCPUs and %d MiB, VMM reports %d vCPUs and %d MiB", ErrInvalidConfig, want.VCPUCount, want.MemoryMiB, got.VCPUs, got.MemSizeMiB)
	}
	return nil
}

// Called with the lifecycle lock after VMM shape and cgroup ownership checks.
func (d *Driver) finishVerifiedRootfsAdoption(ctx context.Context, directory string, state *SandboxState) error {
	if state.Phase != PhaseStarting || state.Config.Desired.RootfsBootDigest == "" {
		return nil
	}
	spec, id := state.Config.Desired, state.Config.Identity
	bundle := filepath.Dir(cachedManifestPath(d.config.StateRoot, spec.Image))
	if strings.HasPrefix(spec.Image, ocitemplate.Prefix) {
		bundle = filepath.Join(d.ociRoot(), "sandboxes", id.SandboxUID, "mnt")
	}
	d.importTemplateCarry(ctx, spec, spec.Image, bundle, id.SandboxUID)
	state.Phase = PhaseRunning
	if err := saveState(directory, state); err != nil {
		return err
	}
	d.publishVMMetadata(filepath.Dir(state.APIAddress), id)
	return nil
}

func (d *Driver) verifyExistingBoot(ctx context.Context, state *SandboxState) error {
	if state.Config.Desired.RootfsBootDigest == "" {
		return nil
	}
	client := d.newClient(state.APIAddress)
	defer client.Close()
	if state.Phase == PhaseStarting {
		current, err := client.VMState(ctx)
		if err != nil {
			return err
		}
		if current == "Not started" || current == "NotStarted" {
			return fmt.Errorf("%w: VMM state is %q", errIncompleteRootfsStart, current)
		}
		if current != vmRunning {
			return fmt.Errorf("cannot adopt interrupted rootfs boot: VMM state is %q", current)
		}
	}
	return verifyBootMachine(ctx, client, state)
}

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
func (d *Driver) prepareRootfsBootInstance(id, directory, rootfs, kernel, driveName string) (devicePath, jailPath, socketPath string, resultErr error) {
	if d.config.JailerPath == "" {
		if driveName == rootfsImageName {
			// Native boot already owns directory/rootfs.img. Keep the same
			// relative drive name that native Full snapshots restore later.
			return rootfs, "", filepath.Join(directory, "api.sock"), nil
		}
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
	for _, entry := range []struct{ source, target string }{{rootfs, driveName}, {kernel, "rootfs-kernel"}} {
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
	return filepath.Join(root, driveName), root, filepath.Join(root, "api.sock"), nil
}
func configureRootfsVM(ctx context.Context, c *Client, slot *fastletnetwork.Slot, m artifacts.VMManifest, kernel, driveName string, jailed bool) error {
	if slot == nil || slot.GuestTap == "" {
		return ErrNetworkUnavailable
	}
	if err := requireStaticGuestNetwork(m.GuestNetwork); err != nil {
		return err
	}
	if jailed {
		kernel = "/rootfs-kernel"
	}
	ops, err := firecrackerconfig.RootfsBoot(m, kernel, driveName, slot.GuestTap, "guest-control.sock")
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
