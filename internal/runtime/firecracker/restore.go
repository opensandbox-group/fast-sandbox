package firecracker

// restore.go implements the golden snapshot restore startup path: restore
// is the only startup path. EnsureSandbox
// validates the request memory
// against the manifest machine tuple, launches the Firecracker process via
// the jailer (which chroots it and pins it to the slot netns; the jail root
// holds the instance rootfs reflink copy and hard-linked snapshots), then
// loads the golden snapshot (vmstate + file-backed memory) as the FIRST API
// call and resumes the instance. Devices (root drive, NIC) are restored
// from the snapshot; only the NIC host tap is replaced per instance via
// network_overrides.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"

	"fast-sandbox/internal/artifacts"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"fast-sandbox/internal/ocitemplate"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"
)

// cachedManifestPath returns the commit-point manifest of a pulled image.
func cachedManifestPath(stateRoot, image string) string {
	return filepath.Join(stateRoot, imageCacheDir, imageKey(image), "manifest.json")
}

// restoreReference resolves the local cache reference a runtime restores
// from: the canonical checkpoint reference of a resume (spec.Restore), or
// the image reference of a fresh boot. Both the node cache and the driver
// restore paths key off this value, so a checkpoint restored on another host
// reuses the standard image cache layout unchanged.
func restoreReference(spec fastletapi.SandboxDesiredSpec) (string, error) {
	if spec.Restore == nil {
		return spec.Image, nil
	}
	if spec.Restore.ManifestRef == "" || spec.Restore.ArtifactDigest == "" {
		return "", fmt.Errorf("%w: restore requires manifestRef and artifactDigest", ErrInvalidConfig)
	}
	if strings.HasPrefix(spec.Restore.ManifestRef, ocitemplate.Prefix) {
		if err := ocitemplate.ValidateDigestReference(spec.Restore.ManifestRef, spec.Restore.ArtifactDigest); err != nil {
			return "", fmt.Errorf("%w: %w", ErrInvalidConfig, err)
		}
		return spec.Restore.ManifestRef, nil
	}
	return artifacts.CheckpointReference(spec.Restore.ArtifactDigest), nil
}

// manifestMachine uses the shared integer CPU/MiB contract.
type manifestMachine = artifacts.VMMachine

// readCachedManifestMachine loads the machine tuple from the cached
// manifest. Only an absent local manifest reports false; malformed metadata fails.
func readCachedManifestMachine(stateRoot, image string) (manifestMachine, bool, error) {
	payload, err := os.ReadFile(cachedManifestPath(stateRoot, image))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return manifestMachine{}, false, nil
		}
		return manifestMachine{}, false, err
	}
	document, err := artifacts.DecodeVMManifest(payload)
	if err != nil {
		return manifestMachine{}, false, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	return document.Machine, true, nil
}

// manifestGuestNetwork is the guest network baked into a template snapshot
// (clone networking model). MTU is 0 for manifests recorded before the field
// existed (the kernel default was baked implicitly).
type manifestGuestNetwork = artifacts.VMGuestNetwork

// readCachedManifestGuestNetwork loads the baked guest network from the
// cached manifest (builder records guestNetwork.ip, guestNetwork.mtu). It
// reports false when the manifest is absent or carries no guest network
// (hand-seeded local cache); the caller falls back to the BakedGuestIP
// convention.
func readCachedManifestGuestNetwork(stateRoot, image string) (manifestGuestNetwork, bool, error) {
	payload, err := os.ReadFile(cachedManifestPath(stateRoot, image))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return manifestGuestNetwork{}, false, nil
		}
		return manifestGuestNetwork{}, false, err
	}
	document, err := artifacts.DecodeVMManifest(payload)
	if err != nil {
		return manifestGuestNetwork{}, false, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	if err := requireStaticGuestNetwork(document.GuestNetwork); err != nil {
		return manifestGuestNetwork{}, false, err
	}
	return *document.GuestNetwork, true, nil
}

func requireStaticGuestNetwork(network *artifacts.VMGuestNetwork) error {
	if network == nil || network.IP == "" || network.Gateway == "" || network.Netmask == "" || network.MTU <= 0 {
		return fmt.Errorf("%w: Fastlet requires a complete guestNetwork", ErrInvalidConfig)
	}
	return nil
}

// validateRestoreMachineConfig validates the Sandbox request against the
// machine tuple of the golden snapshot, it does not produce a machine
// configuration. Restore requires the machine tuple of the golden snapshot
// (Firecracker rejects a mem_size_mib different from the one the vmstate
// was created with), so the manifest machine is authoritative AND the
// machine-config API must not be called before snapshot/load (v1.16
// rejects it). The request cpu/mem only validate: a request memory below
// the snapshot memory is rejected with an explicit error. When the cached
// manifest carries no machine tuple (hand-seeded local cache), the request
// profile is validated as the fallback. An explicit CPU limit must also be
// large enough for the immutable snapshot vCPU tuple.
func validateRestoreMachineConfig(spec fastletapi.SandboxRuntimeLimits, config runtimecatalog.FirecrackerConfig, stateRoot, image string) error {
	machine, ok, err := readCachedManifestMachine(stateRoot, image)
	if err != nil {
		return err
	}
	return validateRestoreMachine(spec, config, machine, ok)
}

func validateRestoreMachine(spec fastletapi.SandboxRuntimeLimits, config runtimecatalog.FirecrackerConfig, machine manifestMachine, ok bool) error {
	if !ok {
		_, err := resolveMachineConfig(spec, config)
		return err
	}
	if machine.VCPUCount < 1 || machine.MemoryMiB < 1 {
		return fmt.Errorf("%w: invalid manifest machine", ErrInvalidConfig)
	}
	if spec.CPU != "" {
		quantity, err := resource.ParseQuantity(spec.CPU)
		if err != nil {
			return fmt.Errorf("%w: invalid CPU %q", ErrInvalidConfig, spec.CPU)
		}
		requestedVCPUs := int(math.Ceil(float64(quantity.MilliValue()) / 1000.0))
		if requestedVCPUs < machine.VCPUCount {
			return fmt.Errorf("%w: requested CPU %s permits %d vCPU(s), below the template snapshot vCPU count %d", ErrInvalidConfig, spec.CPU, requestedVCPUs, machine.VCPUCount)
		}
	}
	snapshotMiB := machine.MemoryMiB
	if spec.Memory != "" {
		requestMiB, err := parseMemMiB(spec.Memory)
		if err != nil {
			return err
		}
		if int64(requestMiB) < snapshotMiB {
			return fmt.Errorf("%w: requested memory %d MiB is below the template snapshot memory %d MiB", ErrInvalidConfig, requestMiB, snapshotMiB)
		}
	}
	return nil
}

// checkRestoreCompatibility rejects snapshots incompatible with this node.
func checkRestoreCompatibility(compat artifacts.SnapshotCompatibility, local artifacts.CPUIdentity, localFirecrackerVersion, image string) error {
	if compat.CPUArch != "" && compat.CPUArch != artifacts.HostCPUArch() {
		return fmt.Errorf("%w: snapshot cpuArch %q does not match host %q", ErrIncompatibleArtifact, compat.CPUArch, artifacts.HostCPUArch())
	}
	if err := artifacts.MatchRestoreCompatibility(compat, local, localFirecrackerVersion); err != nil {

		klog.ErrorS(err, "restore compatibility admission rejected", "image", image)
		return fmt.Errorf("%w: %w", ErrIncompatibleArtifact, err)
	}
	return nil
}

// firecrackerVersion resolves the local VMM binary version, caching only
// successful lookups: a transient failure ("unknown" — binary mid-upgrade,
// exec error) must not poison the restore admission for the process
// lifetime.
func (d *Driver) firecrackerVersion() string {
	d.mu.RLock()
	cached := d.fcVersion
	d.mu.RUnlock()
	if artifacts.KnownFirecrackerVersion(cached) {
		return cached
	}
	resolved := artifacts.FirecrackerVersion(d.config.BinaryPath)
	if artifacts.KnownFirecrackerVersion(resolved) {
		d.mu.Lock()
		d.fcVersion = resolved
		d.mu.Unlock()
	}
	return resolved
}

// validateRestoreCompatibility admits a restore only if the cached
// manifest's compatibility matches this node (see
// artifacts.MatchRestoreCompatibility). A missing manifest is tiered: an
// agent-delivered set commits manifest.json last, so its absence is a
// delivery in flight (or an interrupted pull) and the create retries
// (ErrImageNotReady) rather than fail-opening as legacy; only a local-mode
// hand-seeded cache is admitted without checks.
func (d *Driver) validateRestoreCompatibility(stateRoot, image string) error {
	payload, err := os.ReadFile(cachedManifestPath(stateRoot, image))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && d.agentSocket != "" {
			return fmt.Errorf("%w: %q has no committed manifest yet", runtimecontract.ErrImageNotReady, image)
		}
		klog.InfoS("cached manifest absent; admitting restore without CPU checks", "image", image)
		return nil
	}
	document, err := artifacts.DecodeVMManifest(payload)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	return checkRestoreCompatibility(document.Compatibility, artifacts.HostCPUIdentity(), d.firecrackerVersion(), image)
}

// parseMemMiB parses a resource quantity into whole MiB.
func parseMemMiB(value string) (int, error) {
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid memory %q", ErrInvalidConfig, value)
	}
	mib := int(math.Ceil(float64(quantity.Value()) / (1024.0 * 1024.0)))
	if mib < 1 {
		return 0, fmt.Errorf("%w: memory %q yields no MiB", ErrInvalidConfig, value)
	}
	return mib, nil
}

// restoreSnapshotPath returns the cache path of a golden snapshot artifact.
func restoreSnapshotPath(stateRoot, image, name string) (string, error) {
	path := filepath.Join(stateRoot, imageCacheDir, imageKey(image), name)
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return "", fmt.Errorf("%w: %q", ErrImageNotReady, image)
	}
	return path, nil
}

// vmstateSnapshotName and memorySnapshotName are the golden snapshot files
// the pull layer commits into the image cache.
const (
	vmstateSnapshotName = "vmstate.snap"
	memorySnapshotName  = "memory.snap"
)

// resolveRestoreSnapshotFiles returns the cached vmstate and memory snapshot
// paths of an image, both of which restore requires.
func resolveRestoreSnapshotFiles(stateRoot, image string) (vmstate, memory string, err error) {
	vmstate, err = restoreSnapshotPath(stateRoot, image, vmstateSnapshotName)
	if err != nil {
		return "", "", err
	}
	memory, err = restoreSnapshotPath(stateRoot, image, memorySnapshotName)
	if err != nil {
		return "", "", err
	}
	return vmstate, memory, nil
}

// configureRestoreVM drives the Firecracker API for a snapshot restore.
// v1.16 requires LoadSnapshot to be the FIRST configuration call: any
// machine/drive/network API call before it sets the boot path and is
// rejected ("Loading a microVM snapshot not allowed after configuring
// boot-specific resources"). All devices (root drive, NIC) are restored
// from the vmstate; only the NIC host tap can be replaced via
// network_overrides. The boot source is deliberately absent: the snapshot
// already contains the guest state, no kernel is booted.
//
// The root drive path is baked in the vmstate as a relative path
// ("rootfs.img"); in direct mode each instance resolves it to its own
// reflink copy via the process cwd (the Firecracker process working
// directory). In jailer mode the chroot fixes the working directory to the
// jail root, so the snapshot files prepared under snapshots/ are addressed
// with their chroot-relative paths.
func configureRestoreVM(ctx context.Context, client *Client, slot *fastletnetwork.Slot, vmstatePath, memoryPath string, jailed bool) error {
	tapDevice := slot.GuestTap
	if tapDevice == "" {
		return fmt.Errorf("%w: slot %s has no pre-provisioned guest tap", ErrNetworkUnavailable, slot.ID)
	}
	snapshotPath, memPath := vmstatePath, memoryPath
	if jailed {
		snapshotPath = filepath.ToSlash(filepath.Join("/", jailerChrootSnapshotsDir, vmstateSnapshotName))
		memPath = filepath.ToSlash(filepath.Join("/", jailerChrootSnapshotsDir, memorySnapshotName))
	}
	if err := client.LoadSnapshot(ctx, SnapshotLoadRequest{
		SnapshotPath: snapshotPath,
		MemBackend: SnapshotMemBackend{
			BackendType: "File", BackendPath: memPath,
		},
		ResumeVM: false,
		NetworkOverrides: []SnapshotNetworkOverride{{
			IfaceID: "eth0", HostDevName: tapDevice,
		}},
	}); err != nil {
		return fmt.Errorf("load Firecracker snapshot: %w", err)
	}
	return nil
}
