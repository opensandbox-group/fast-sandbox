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
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"

	"fast-sandbox/internal/artifacts"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
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
func restoreReference(spec fastletapi.SandboxSpec) (string, error) {
	if spec.Restore == nil {
		return spec.Image, nil
	}
	if spec.Restore.ManifestRef == "" || spec.Restore.ArtifactDigest == "" {
		return "", fmt.Errorf("%w: restore requires manifestRef and artifactDigest", ErrInvalidConfig)
	}
	return artifacts.CheckpointReference(spec.Restore.ArtifactDigest), nil
}

// manifestMachine is the machine tuple recorded in the published manifest
// (builder records {vcpu, memory} as resource quantities).
type manifestMachine struct {
	VCPU   string `json:"vcpu"`
	Memory string `json:"memory"`
}

// readCachedManifestMachine loads the machine tuple from the cached
// manifest. It reports false when the manifest is absent or carries no
// machine tuple (hand-seeded local cache).
func readCachedManifestMachine(stateRoot, image string) (manifestMachine, bool, error) {
	payload, err := os.ReadFile(cachedManifestPath(stateRoot, image))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return manifestMachine{}, false, nil
		}
		return manifestMachine{}, false, err
	}
	var document struct {
		Machine manifestMachine `json:"machine"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		return manifestMachine{}, false, fmt.Errorf("decode cached manifest: %w", err)
	}
	if document.Machine.VCPU == "" || document.Machine.Memory == "" {
		return manifestMachine{}, false, nil
	}
	return document.Machine, true, nil
}

// manifestGuestNetwork is the guest network baked into a template snapshot
// (clone networking model). MTU is 0 for manifests recorded before the field
// existed (the kernel default was baked implicitly).
type manifestGuestNetwork struct {
	IP      string `json:"ip"`
	Gateway string `json:"gateway"`
	Netmask string `json:"netmask"`
	MTU     int    `json:"mtu"`
}

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
	var document struct {
		GuestNetwork manifestGuestNetwork `json:"guestNetwork"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		return manifestGuestNetwork{}, false, fmt.Errorf("decode cached manifest: %w", err)
	}
	if document.GuestNetwork.IP == "" {
		return manifestGuestNetwork{}, false, nil
	}
	return document.GuestNetwork, true, nil
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
// profile is validated as the fallback.
func validateRestoreMachineConfig(spec fastletapi.SandboxSpec, config runtimecatalog.FirecrackerConfig, stateRoot, image string) error {
	machine, ok, err := readCachedManifestMachine(stateRoot, image)
	if err != nil {
		return err
	}
	if !ok {
		_, err := resolveMachineConfig(spec, config)
		return err
	}
	if _, err := machineVCPUs(machine.VCPU); err != nil {
		return err
	}
	snapshotMiB, err := parseMemMiB(machine.Memory)
	if err != nil {
		return err
	}
	if spec.Memory != "" {
		requestMiB, err := parseMemMiB(spec.Memory)
		if err != nil {
			return err
		}
		if requestMiB < snapshotMiB {
			return fmt.Errorf("%w: requested memory %d MiB is below the template snapshot memory %d MiB", ErrInvalidConfig, requestMiB, snapshotMiB)
		}
	}
	return nil
}

// readCachedManifestCompatibility loads the compatibility block from the
// cached manifest. It reports false for manifests without the structured
// fields (published before the builder recorded CPU provenance, or
// hand-seeded caches), and also treats an undecodable block as absent: pre-
// structured manifests carry a string cpuModel that cannot decode into the
// structured shape.
func readCachedManifestCompatibility(stateRoot, image string) (artifacts.SnapshotCompatibility, bool, error) {
	payload, err := os.ReadFile(cachedManifestPath(stateRoot, image))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return artifacts.SnapshotCompatibility{}, false, nil
		}
		return artifacts.SnapshotCompatibility{}, false, err
	}
	var document struct {
		Compatibility artifacts.SnapshotCompatibility `json:"compatibility"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		return artifacts.SnapshotCompatibility{}, false, nil
	}
	compat := document.Compatibility
	if compat.Vendor == "" && compat.CPUTemplate == "" {
		return artifacts.SnapshotCompatibility{}, false, nil
	}
	return compat, true, nil
}

// checkRestoreCompatibility is the restore-admission core: fail fast on an
// incompatible artifact before any snapshot file is staged. A legacy
// manifest is admitted with a warning (OSEP-0024 Phase 1 backward
// compatibility); everything else defers to the tiered matcher.
func checkRestoreCompatibility(compat artifacts.SnapshotCompatibility, hasCompat bool, local artifacts.CPUIdentity, localFirecrackerVersion, image string) error {
	if !hasCompat {
		klog.InfoS("cached manifest carries no structured compatibility; admitting restore without CPU checks",
			"image", image)
		return nil
	}
	if err := artifacts.MatchRestoreCompatibility(compat, local, localFirecrackerVersion); err != nil {
		if errors.Is(err, artifacts.ErrLegacyCompatibility) {
			klog.InfoS("cached manifest compatibility is legacy; admitting restore without CPU checks", "image", image)
			return nil
		}
		return fmt.Errorf("%w: %w", ErrIncompatibleArtifact, err)
	}
	return nil
}

// firecrackerVersion resolves the local VMM binary version once; admission
// needs it on every Create and the exec is not free.
func (d *Driver) firecrackerVersion() string {
	d.versionOnce.Do(func() { d.fcVersion = artifacts.FirecrackerVersion(d.config.BinaryPath) })
	return d.fcVersion
}

// validateRestoreCompatibility admits a restore only if the cached
// manifest's compatibility block matches this node (tiered match: template
// allowlist for masked snapshots, identity equality for unmasked ones).
func (d *Driver) validateRestoreCompatibility(stateRoot, image string) error {
	compat, ok, err := readCachedManifestCompatibility(stateRoot, image)
	if err != nil {
		return err
	}
	return checkRestoreCompatibility(compat, ok, artifacts.HostCPUIdentity(), d.firecrackerVersion(), image)
}

// machineVCPUs parses the manifest vcpu quantity into a vCPU count.
func machineVCPUs(value string) (int, error) {
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return 0, fmt.Errorf("%w: invalid manifest vcpu %q", ErrInvalidConfig, value)
	}
	vcpus := int(math.Ceil(float64(quantity.MilliValue()) / 1000.0))
	if vcpus < 1 {
		return 0, fmt.Errorf("%w: manifest vcpu %q yields no vCPU", ErrInvalidConfig, value)
	}
	return vcpus, nil
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
