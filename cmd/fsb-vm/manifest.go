package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"fast-sandbox/internal/artifacts"
)

func loadManifest(path string) (artifacts.VMManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return artifacts.VMManifest{}, fmt.Errorf("read VM manifest %s: %w", path, err)
	}
	return artifacts.DecodeVMManifest(raw)
}

// --force only downgrades VMM version mismatches, never machine, architecture,
// drive-path or CPU compatibility errors.
func validateManifest(m artifacts.VMManifest, binary string, force bool) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if m.Machine.RootfsDrivePath != rootfsDriveLink {
		return fmt.Errorf("fsb-vm requires rootfsDrivePath %q", rootfsDriveLink)
	}
	if m.Compatibility.CPUArch != artifacts.HostCPUArch() {
		return fmt.Errorf("cpuArch %q does not match host %q", m.Compatibility.CPUArch, artifacts.HostCPUArch())
	}
	have, want := artifacts.FirecrackerVersion(binary), m.Compatibility.FirecrackerVersion
	if have != want {
		err := fmt.Errorf("firecracker version mismatch: host %q, template %q", have, want)
		if !force {
			return fmt.Errorf("%w; use --force to proceed", err)
		}
		fmt.Fprintln(os.Stderr, "warning:", err)
	}
	if m.IsRootfsOnly() {
		return nil
	}
	return artifacts.MatchRestoreCompatibility(m.Compatibility, artifacts.HostCPUIdentity(), want)
}

// kernelVersionFromPath derives kernel.version from a vmlinux-<version>
// filename; the explicit override wins and a bare "vmlinux" name is an
// error (the metadata field is authoritative for create-side kernel
// resolution, ADR-004).
func kernelVersionFromPath(kernelPath, override string) (string, error) {
	if strings.TrimSpace(override) != "" {
		return strings.TrimSpace(override), nil
	}
	base := filepath.Base(kernelPath)
	if version, ok := strings.CutPrefix(base, "vmlinux-"); ok && version != "" {
		return version, nil
	}
	return "", fmt.Errorf("cannot infer kernel.version from %q; pass --kernel-version", kernelPath)
}

// buildSnapfilesManifest owns the VM contract. Extras can only supplement
// guest networking, provenance and platform state; core VM fields are rejected.
func buildSnapfilesManifest(options *buildOptions, createdAt time.Time) ([]byte, error) {
	version, err := kernelVersionFromPath(options.kernel, options.kernelVersion)
	if err != nil {
		return nil, err
	}
	size, err := diskSizeMiB(options.diskSizeGiB)
	if err != nil {
		return nil, err
	}
	manifest := artifacts.NewVMManifest(artifacts.VMMachine{VCPUCount: options.vcpu, MemoryMiB: int64(options.memoryMB), RootfsSizeMiB: int64(size), RootfsDriveID: options.rootfsDriveID, RootfsDrivePath: rootfsDriveLink}, version, global.firecrackerBin, artifacts.CPUTemplateNone)
	return completeBuildManifest(options, createdAt, manifest)
}

// Complete metadata from a manifest whose compatibility identifies the build
// environment. Production supplies the real host; metadata tests use fixtures.
func completeBuildManifest(options *buildOptions, createdAt time.Time, manifest artifacts.VMManifest) ([]byte, error) {
	if options.guestControlPort != 0 {
		if options.guestControlVersion == "" {
			return nil, fmt.Errorf("guest control capability requires successful freeze/thaw probe")
		}
		manifest.GuestControl = &artifacts.VMGuestControl{Protocol: "fsb-freeze-v1", Transport: "vsock", Port: options.guestControlPort, HelperVersion: options.guestControlVersion}
	}
	if options.kernelSHA256 == "" {
		return nil, fmt.Errorf("boot kernel digest is missing")
	}
	manifest.Kernel.SHA256 = options.kernelSHA256
	manifest.Kernel.Cmdline = options.bootArgs
	if strings.TrimSpace(manifest.Kernel.Cmdline) == "" {
		manifest.Kernel.Cmdline = defaultBootArgs + " init=" + options.initPath
	}
	manifest.Lineage = &artifacts.VMLineage{Image: options.source, CreatedAt: createdAt.UTC().Format(time.RFC3339)}
	if ref, err := parseRegistryReference(options.source); err == nil && strings.HasPrefix(ref.Reference, "sha256:") {
		manifest.Lineage.ImageDigest = ref.Reference
	}
	if options.tapName != "" {
		manifest.GuestNetwork = &artifacts.VMGuestNetwork{Iface: "eth0", MAC: options.guestMAC}
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if options.manifestExtraFile != "" {
		extra, err := os.ReadFile(options.manifestExtraFile)
		if err != nil {
			return nil, err
		}
		raw, err = supplementManifest(raw, extra)
		if err != nil {
			return nil, err
		}
	}
	document, err := artifacts.DecodeVMManifest(raw)
	if err != nil {
		return nil, err
	}
	if document.IsRootfsOnly() {
		return nil, fmt.Errorf("build produces Full snapshots; use pack --rootfs-only for captured rootfsOnly bundles")
	}
	return raw, nil
}

func supplementManifest(raw, extra []byte) ([]byte, error) {
	var document, additions map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(extra, &additions); err != nil {
		return nil, fmt.Errorf("decode manifest extras: %w", err)
	}
	if additions == nil {
		return nil, fmt.Errorf("manifest extras must be a JSON object")
	}
	for name, value := range additions {
		switch name {
		case "guestNetwork", "lineage":
			merged, err := supplementManifestSection(name, document[name], value)
			if err != nil {
				return nil, err
			}
			document[name] = merged
		case "platform":
			document[name] = value
		default:
			return nil, fmt.Errorf("manifest extras cannot set core field %q", name)
		}
	}
	return json.MarshalIndent(document, "", "  ")
}

func supplementManifestSection(name string, original, extra []byte) (json.RawMessage, error) {
	var base, fields map[string]json.RawMessage
	if err := json.Unmarshal(original, &base); err != nil || base == nil {
		return nil, fmt.Errorf("manifest extras cannot introduce %s", name)
	}
	if err := json.Unmarshal(extra, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("invalid manifest extras %s", name)
	}
	for field, data := range fields {
		// Image is optional provenance (the builder may retain a source tag).
		// Its digest, timestamp and the actual NIC identity remain fsb-vm owned.
		if old, exists := base[field]; exists && !(name == "lineage" && field == "image") {
			var a, b any
			if err := json.Unmarshal(old, &a); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(data, &b); err != nil {
				return nil, err
			}
			if !reflect.DeepEqual(a, b) {
				return nil, fmt.Errorf("manifest extras conflict with %s.%s", name, field)
			}
		}
		base[field] = data
	}
	merged, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	return merged, nil
}
