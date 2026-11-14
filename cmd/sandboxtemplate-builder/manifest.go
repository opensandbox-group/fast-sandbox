package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/artifacts"
)

// Published artifact and execd asset names shared by the build stages.
const (
	// rootfsDirName is the rootfs drive id.
	rootfsDirName = "rootfs"
	// rootfsImageName is the rootfs image file staged in the workdir.
	rootfsImageName = "rootfs.ext4"
	// vmstateFileName is the Firecracker VM state file of the snapshot.
	vmstateFileName = "vmstate.snap"
	// memoryFileName is the guest memory image of the snapshot.
	memoryFileName = "memory.snap"
	// execdAssetName is the execd binary carried by the execd image.
	execdAssetName = "execd"
	// bootstrapScriptName is the runtime bootstrap script beside execd.
	bootstrapScriptName = "bootstrap.sh"
	// prepareScriptName is the runtime prepare script beside execd.
	prepareScriptName = "prepare.sh"
)

// stageManifest assembles manifest.json (content-addressed, design schema)
// and SHA256SUMS in the workdir. Checksums are computed once and shared
// between the two outputs via the cache. The serialization and checksum
// conventions live in internal/artifacts so every producer (builder, live
// snapshot driver, runtime-agent) emits byte-identical layouts. phases records
// the verified boot inputs and detected Guest capabilities; mitmCA carries
// the baked egress MITM CA lineage.
func stageManifest(spec apiv1alpha2.SandboxTemplateSpec, sourceDigest, kernel, rootfs, vmstate, memory, workdir string, phases snapshotPhaseTimings, mitmCA []mitmCALineage) ([]byte, error) {
	cache := map[string]string{}
	rootfsGiB, err := sizeGiB(spec.Output.RootfsSize)
	if err != nil {
		return nil, err
	}
	manifest, err := buildManifest(spec, sourceDigest, kernel, rootfs, vmstate, memory, cache, rootfsGiB, phases.CPUTemplate, phases.FreePageReporting, mitmCA)
	if err != nil {
		return nil, err
	}
	if err := applyNativeBootProvenance(&manifest, phases, kernel); err != nil {
		return nil, err
	}
	manifestBytes, err := artifacts.MarshalVMManifest(manifest)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(workdir, "manifest.json"), manifestBytes, 0o644); err != nil { //nolint:gosec // published artifact manifest, world-readable by design
		return nil, err
	}
	if err := writeChecksums(workdir, cache); err != nil {
		return nil, err
	}
	return manifestBytes, nil
}

// buildManifest assembles the content-addressed manifest (design schema).
// rootfsGiB is the actual size passed to oci2rootfs (the declared
// rootfsSize rounded up to whole GiB), recorded so consumers can reconcile the
// declared minimum with the real artifact size. The lineage object records
// where the set came from and what was baked in at build time; snapshot
// and checkpoint producers carry it forward verbatim across generations.
func buildManifest(spec apiv1alpha2.SandboxTemplateSpec, sourceDigest, kernel, rootfs, vmstate, memory string, cache map[string]string, rootfsGiB int, cpuTemplate string, freePageReporting bool, mitmCA []mitmCALineage) (artifacts.VMManifest, error) {
	var empty artifacts.VMManifest
	cpu, err := resource.ParseQuantity(spec.Machine.VCPU)
	if err != nil {
		return empty, err
	}
	mem, err := resource.ParseQuantity(spec.Machine.Memory)
	if err != nil {
		return empty, err
	}
	document := artifacts.NewVMManifest(artifacts.VMMachine{VCPUCount: int(cpu.Value()), MemoryMiB: (mem.Value() + (1 << 20) - 1) >> 20, RootfsSizeMiB: int64(rootfsGiB) << 10, RootfsDriveID: rootfsDirName, RootfsDrivePath: snapshotDriveName}, strings.TrimPrefix(filepath.Base(kernel), "vmlinux-"), firecrackerBin, compatibilityCPUTemplate(cpuTemplate))
	document.FreePageReporting = &freePageReporting
	document.Lineage = &artifacts.VMLineage{Image: spec.Image, ImageDigest: sourceDigest, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	document.Platform = templatePlatform(spec, mitmCA)
	document.GuestNetwork = &artifacts.VMGuestNetwork{Iface: "eth0", MAC: bakedGuestMAC, IP: bakedGuestIP, Gateway: bakedGuestGateway, Netmask: bakedGuestNetmask, MTU: bakedGuestMTU}
	document.Files = map[string]artifacts.ManifestFile{}
	for _, file := range []struct{ name, path string }{{rootfsImageName, rootfs}, {vmstateFileName, vmstate}, {memoryFileName, memory}} {
		entry, err := artifacts.FileEntry(file.path, cache)
		if err != nil {
			return empty, err
		}
		document.Files[file.name] = entry
	}
	return document, document.Validate()
}

func templatePlatform(spec apiv1alpha2.SandboxTemplateSpec, ca []mitmCALineage) *artifacts.VMPlatform {
	platform := &artifacts.VMPlatform{Execd: spec.Execd, Entrypoint: spec.Entrypoint, Init: spec.Init}
	if len(spec.Envs) > 0 {
		platform.Envs = map[string]string{}
		for _, env := range spec.Envs {
			platform.Envs[env.Name] = env.Value
		}
	}
	if len(ca) > 0 {
		platform.EgressMitmCA = &artifacts.MITMCALineage{Certificates: ca}
	}
	return platform
}

// compatibilityCPUTemplate normalizes the snapshot-stage CPU template for
// the manifest: an empty template means the raw host CPUID fallback ran and
// is recorded as "none".
func compatibilityCPUTemplate(cpuTemplate string) string {
	if cpuTemplate == "" {
		return artifacts.CPUTemplateNone
	}
	return cpuTemplate
}

// writeChecksums writes SHA256SUMS covering only the published artifact set
// (rootfs/vmstate/memory), reusing the checksums already computed for
// the manifest. Intermediate build files (OCI layout, console logs, etc.) are
// deliberately excluded.
func writeChecksums(workdir string, cache map[string]string) error {
	files := []string{rootfsImageName, vmstateFileName, memoryFileName}
	return artifacts.WriteSHA256SUMS(workdir, files, cache)
}

// Bind the published capability to the successful boot/restore report and the
// same kernel bytes. Enabling injection without a probe report fails closed.
func applyNativeBootProvenance(manifest *artifacts.VMManifest, phases snapshotPhaseTimings, kernel string) error {
	if phases.GuestControl == nil {
		return fmt.Errorf("native guest control requires a successful freeze/thaw probe")
	}
	entry, err := artifacts.FileEntry(kernel, map[string]string{})
	if err != nil {
		return err
	}
	if phases.KernelSHA256 != entry.SHA256 || strings.TrimSpace(phases.KernelCmdline) == "" {
		return fmt.Errorf("native snapshot kernel provenance is missing or changed")
	}
	manifest.Kernel.SHA256 = phases.KernelSHA256
	manifest.Kernel.Cmdline = phases.KernelCmdline
	manifest.GuestControl = phases.GuestControl
	return nil
}
