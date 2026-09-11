package main

import (
	"fmt"
	"os"
	"path/filepath"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/artifacts"
)

// stageManifest assembles manifest.json (content-addressed, design schema)
// and SHA256SUMS in the workdir. Checksums are computed once and shared
// between the two outputs via the cache. The serialization and checksum
// conventions live in internal/artifacts so every producer (builder, live
// snapshot driver, runtime-agent) emits byte-identical layouts.
func stageManifest(spec apiv1alpha2.SandboxTemplateSpec, sourceDigest, kernel, rootfs, vmstate, memory string, layers []string, workdir string) ([]byte, error) {
	cache := map[string]string{}
	rootfsGiB, err := sizeGiB(spec.Output.RootfsSize)
	if err != nil {
		return nil, err
	}
	manifest, err := buildManifest(spec, sourceDigest, kernel, rootfs, vmstate, memory, layers, cache, rootfsGiB)
	if err != nil {
		return nil, err
	}
	manifestBytes, err := artifacts.MarshalManifest(manifest)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(workdir, "manifest.json"), manifestBytes, 0o644); err != nil {
		return nil, err
	}
	if err := writeChecksums(workdir, cache); err != nil {
		return nil, err
	}
	return manifestBytes, nil
}

// buildManifest assembles the content-addressed manifest (design schema).
// rootfsGiB is the actual size passed to oci2rootfs (the declared
// rootfsSize rounded up to SI GiB), recorded so consumers can reconcile the
// declared minimum with the real artifact size.
func buildManifest(spec apiv1alpha2.SandboxTemplateSpec, sourceDigest, kernel, rootfs, vmstate, memory string, layers []string, cache map[string]string, rootfsGiB int) (map[string]any, error) {
	files := map[string]any{}
	staged := []struct{ name, path string }{
		{"rootfs.ext4", rootfs},
		{"vmstate.snap", vmstate},
		{"memory.snap", memory},
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
	return map[string]any{
		"schemaVersion":     1,
		"runtime":           "firecracker",
		"sourceImage":       spec.Image,
		"sourceImageDigest": sourceDigest,
		"execd":             spec.Execd,
		"kernel": map[string]any{
			"name":   filepath.Base(kernel),
			"digest": kernelDigest,
		},
		// Snapshot compatibility tuple (design): consumers match these
		// against node labels before restoring a snapshot.
		"compatibility": map[string]any{
			"firecrackerVersion": artifacts.FirecrackerVersion(firecrackerBin),
			"hostKernel":         artifacts.HostKernelRelease(),
			"cpuModel":           artifacts.HostCPUModel(),
		},
		"machine": map[string]any{
			"vcpu":   spec.Machine.VCPU,
			"memory": spec.Machine.Memory,
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
		"entrypoint": spec.Entrypoint,
		"init":       spec.Init,
		// Note: envs are published verbatim into the manifest — do not place
		// secrets here; use the publishSecretRef secret for credentials.
		"envs": spec.Envs,
		// The actual rootfs size (rounded up from the declared minimum to SI
		// GiB), matching files['rootfs.ext4'].sizeBytes.
		"rootfsSize": fmt.Sprintf("%dG", rootfsGiB),
		"format":     spec.Output.Format,
		"files":      files,
		"validation": map[string]any{
			"booted":   true,
			"restored": true,
		},
	}, nil
}

// writeChecksums writes SHA256SUMS covering only the published artifact set
// (rootfs/vmstate/memory/layers), reusing the checksums already computed for
// the manifest. Intermediate build files (OCI layout, console logs, etc.) are
// deliberately excluded.
func writeChecksums(workdir string, cache map[string]string) error {
	files := []string{"rootfs.ext4", "vmstate.snap", "memory.snap"}
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
