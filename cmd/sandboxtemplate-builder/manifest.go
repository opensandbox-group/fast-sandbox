package main

import (
	"fmt"
	"os"
	"path/filepath"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/artifacts"
)

// Published artifact and execd asset names shared by the build stages.
const (
	// rootfsDirName is the rootfs drive id and the OverlayBD layer directory.
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
// snapshot driver, runtime-agent) emits byte-identical layouts. cpuTemplate
// is what the snapshot stage reported (see compatibilityCPUTemplate).
func stageManifest(spec apiv1alpha2.SandboxTemplateSpec, sourceDigest, kernel, rootfs, vmstate, memory string, layers []string, workdir, cpuTemplate string) ([]byte, error) {
	cache := map[string]string{}
	rootfsGiB, err := sizeGiB(spec.Output.RootfsSize)
	if err != nil {
		return nil, err
	}
	manifest, err := buildManifest(spec, sourceDigest, kernel, rootfs, vmstate, memory, layers, cache, rootfsGiB, cpuTemplate)
	if err != nil {
		return nil, err
	}
	manifestBytes, err := artifacts.MarshalManifest(manifest)
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
func buildManifest(spec apiv1alpha2.SandboxTemplateSpec, sourceDigest, kernel, rootfs, vmstate, memory string, layers []string, cache map[string]string, rootfsGiB int, cpuTemplate string) (map[string]any, error) {
	files := map[string]any{}
	staged := []struct{ name, path string }{
		{rootfsImageName, rootfs},
		{vmstateFileName, vmstate},
		{memoryFileName, memory},
	}
	if len(layers) > 0 {
		staged = append(staged,
			struct{ name, path string }{"overlaybd/rootfs/layer.lsmt", layers[0]})
		if len(layers) > 1 {
			staged = append(staged,
				struct{ name, path string }{"overlaybd/memory/layer.lsmt", layers[1]})
		}
	}
	for _, artifact := range staged {
		entry, err := artifacts.FileEntry(artifact.path, cache)
		if err != nil {
			return nil, fmt.Errorf("checksum %s: %w", artifact.name, err)
		}
		files[artifact.name] = entry
	}
	kernelDigest, err := artifacts.SHA256FileCached(kernel, cache)
	if err != nil {
		return nil, fmt.Errorf("checksum kernel: %w", err)
	}
	// Snapshot compatibility: vendor/family/model is the CPUID identity
	// consumers match before restoring (8163 and 8269CY share identity 6/85);
	// cpuModelName is display-only. cpuTemplate records how the snapshot was
	// masked; field semantics live in docs/guides/artifact-manifest-reference.md.
	identity := artifacts.HostCPUIdentity()
	compatibility := map[string]any{
		"vendor":             identity.Vendor,
		"cpuFamily":          identity.Family,
		"cpuModel":           identity.Model,
		"cpuModelName":       identity.ModelName,
		"cpuTemplate":        compatibilityCPUTemplate(cpuTemplate),
		"firecrackerVersion": artifacts.FirecrackerVersion(firecrackerBin),
		"hostKernel":         artifacts.HostKernelRelease(),
	}
	return map[string]any{
		"schemaVersion": 1,
		"runtime":       "firecracker",
		"lineage": map[string]any{
			"image":        spec.Image,
			"imageDigest":  sourceDigest,
			execdAssetName: spec.Execd,
			"kernel": map[string]any{
				"name":   filepath.Base(kernel),
				"digest": kernelDigest,
			},
			"entrypoint": spec.Entrypoint,
			"init":       spec.Init,
			// Note: envs are published verbatim into the manifest — do not
			// place secrets here; use the publishSecretRef secret for
			// credentials.
			"envs": spec.Envs,
		},
		"compatibility": compatibility,
		"machine": map[string]any{
			"vcpu":   spec.Machine.VCPU,
			"memory": spec.Machine.Memory,
			// The actual rootfs size (rounded up from the declared minimum
			// to whole GiB), matching files['rootfs.ext4'].sizeBytes.
			rootfsDirName: fmt.Sprintf("%dGi", rootfsGiB),
		},
		// The guest network baked into the snapshot (clone networking
		// model): the restored guest owns a static eth0 address/MAC; the
		// consumer replaces only the host tap via network_overrides and
		// validates the MTU against its slot data plane.
		"guestNetwork": map[string]any{
			"iface":   "eth0",
			"mac":     bakedGuestMAC,
			"ip":      bakedGuestIP,
			"gateway": bakedGuestGateway,
			"netmask": bakedGuestNetmask,
			"mtu":     bakedGuestMTU,
		},
		"format": spec.Output.Format,
		"files":  files,
		"validation": map[string]any{
			"booted":   true,
			"restored": true,
		},
	}, nil
}

// compatibilityCPUTemplate normalizes the snapshot-stage CPU template for
// the manifest: an empty template means the raw host CPUID fallback ran and
// is recorded as "none".
func compatibilityCPUTemplate(cpuTemplate string) string {
	if cpuTemplate == "" {
		return "none"
	}
	return cpuTemplate
}

// writeChecksums writes SHA256SUMS covering only the published artifact set
// (rootfs/vmstate/memory/layers), reusing the checksums already computed for
// the manifest. Intermediate build files (OCI layout, console logs, etc.) are
// deliberately excluded.
func writeChecksums(workdir string, cache map[string]string) error {
	files := []string{rootfsImageName, vmstateFileName, memoryFileName}
	layers, err := filepath.Glob(filepath.Join(workdir, "overlaybd", "*", "layer.lsmt"))
	if err != nil {
		return fmt.Errorf("glob overlaybd layers: %w", err)
	}
	for _, layer := range layers {
		relative, err := filepath.Rel(workdir, layer)
		if err != nil {
			return err
		}
		files = append(files, relative)
	}
	return artifacts.WriteSHA256SUMS(workdir, files, cache)
}
