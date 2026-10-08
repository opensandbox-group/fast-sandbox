package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// vmMetadata is the authoritative VM specification read from the snapfiles
// device (/metadata.json). Flags may override the machine sizing fields;
// hypervisor_type / cpu_arch / firecracker_version drive pre-flight
// validation (ADR-007).
type vmMetadata struct {
	VCPUCount          int             `json:"vcpu_count"`
	MemoryMB           int             `json:"memory_mb"`
	DiskSizeMB         int             `json:"disk_size_mb"`
	FirecrackerVersion string          `json:"firecracker_version"`
	KernelVersion      string          `json:"kernel_version"`
	HypervisorType     string          `json:"hypervisor_type"`
	Network            metadataNetwork `json:"network"`
	CPUArch            string          `json:"cpu_arch"`
	CPUModel           string          `json:"cpu_model"`
	HugePages          bool            `json:"huge_pages"`
	CreatedAt          string          `json:"created_at"`
	EnvdVersion        string          `json:"envd_version"`
	DefaultUser        string          `json:"default_user"`
	DefaultWorkdir     string          `json:"default_workdir"`
}

// metadataNetwork mirrors the network object; host_dev_name (cnid-*) belongs
// to an existing networking stack the CLI integrates with via NetworkProvider.
type metadataNetwork struct {
	HostDevName string `json:"host_dev_name"`
	Mac         string `json:"mac"`
}

const hypervisorFirecracker = "firecracker"

// loadMetadata reads metadata.json from the mounted snapfiles device.
func loadMetadata(path string) (vmMetadata, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return vmMetadata{}, fmt.Errorf("read metadata %s: %w", path, err)
	}
	var metadata vmMetadata
	if err := json.Unmarshal(payload, &metadata); err != nil {
		return vmMetadata{}, fmt.Errorf("parse metadata %s: %w", path, err)
	}
	return metadata, nil
}

// specOverrides carries the create-flag machine overrides. A zero value means
// "keep the metadata value".
type specOverrides struct {
	vcpu     int
	memoryMB int
}

// resolvedSpec is the effective machine spec after applying overrides.
type resolvedSpec struct {
	vcpu     int
	memoryMB int
}

// resolveSpec applies the flag overrides on top of the authoritative
// metadata values.
func (m vmMetadata) resolveSpec(overrides specOverrides) resolvedSpec {
	spec := resolvedSpec{vcpu: m.VCPUCount, memoryMB: m.MemoryMB}
	if overrides.vcpu > 0 {
		spec.vcpu = overrides.vcpu
	}
	if overrides.memoryMB > 0 {
		spec.memoryMB = overrides.memoryMB
	}
	return spec
}

// validate performs the pre-flight checks of ADR-007. firecrackerBin is the
// resolved binary; force downgrades the firecracker version mismatch from an
// error to a warning.
func (m vmMetadata) validate(firecrackerBin string, force bool) error {
	if hv := strings.TrimSpace(m.HypervisorType); hv != "" && hv != hypervisorFirecracker {
		return fmt.Errorf("unsupported hypervisor_type %q: only %q is implemented", hv, hypervisorFirecracker)
	}
	if arch := strings.TrimSpace(m.CPUArch); arch != "" && arch != hostCPUArch() {
		return fmt.Errorf("cpu_arch %q does not match host %q", arch, hostCPUArch())
	}
	want := strings.TrimSpace(m.FirecrackerVersion)
	if want == "" {
		return nil
	}
	have := firecrackerBinaryVersion(firecrackerBin)
	if have == "" || have == "unknown" {
		msg := fmt.Sprintf("cannot determine the local firecracker version (want %s)", want)
		if force {
			fmt.Fprintln(os.Stderr, "warning:", msg)
			return nil
		}
		return fmt.Errorf("%s; use --force to proceed", msg)
	}
	if have != want {
		msg := fmt.Sprintf("firecracker version mismatch: host %s, template %s", have, want)
		if force {
			fmt.Fprintln(os.Stderr, "warning:", msg)
			return nil
		}
		return fmt.Errorf("%s; use --force to proceed", msg)
	}
	return nil
}

// hostCPUArch maps the Go architecture to the metadata.json cpu_arch spelling
// (uname -m style, matching firecracker/kernel naming).
func hostCPUArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return runtime.GOARCH
	}
}

var versionPattern = regexp.MustCompile(`v?(\d+\.\d+\.\d+)`)

// firecrackerBinaryVersion runs `<bin> --version` and extracts the semantic
// version, returning "" when the binary is unavailable and "unknown" when the
// output cannot be parsed.
func firecrackerBinaryVersion(bin string) string {
	output, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	if match := versionPattern.FindStringSubmatch(string(output)); match != nil {
		return match[1]
	}
	return "unknown"
}

// ---- build-flow metadata authoring ----

// metadataSchemaVersion is the schema_version stamped into generated
// metadata.json files.
const metadataSchemaVersion = 1

// snapfilesMetadata is the metadata.json written into the snapfiles image by
// the build flow. It embeds the consumer-side vmMetadata schema (so the
// create flow parses it unchanged) and adds build provenance fields, which
// the consumer simply ignores.
type snapfilesMetadata struct {
	vmMetadata
	SchemaVersion int    `json:"schema_version"`
	SourceImage   string `json:"source_image"`
	RootfsDriveID string `json:"rootfs_drive_id"`
}

// kernelVersionFromPath derives kernel_version from a vmlinux-<version>
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
	return "", fmt.Errorf("cannot infer kernel_version from %q; pass --kernel-version", kernelPath)
}

// buildSnapfilesMetadata renders the metadata.json baked into the snapfiles
// image. createdAt is injected for testability.
func buildSnapfilesMetadata(options *buildOptions, sourceRef string, createdAt time.Time) ([]byte, error) {
	kernelVersion, err := kernelVersionFromPath(options.kernel, options.kernelVersion)
	if err != nil {
		return nil, err
	}
	diskSizeMB, err := diskSizeMiB(options.diskSizeGiB)
	if err != nil {
		return nil, err
	}
	firecrackerVersion := firecrackerBinaryVersion(global.firecrackerBin)
	metadata := snapfilesMetadata{
		vmMetadata: vmMetadata{
			VCPUCount:          options.vcpu,
			MemoryMB:           options.memoryMB,
			DiskSizeMB:         diskSizeMB,
			FirecrackerVersion: firecrackerVersion,
			KernelVersion:      kernelVersion,
			HypervisorType:     hypervisorFirecracker,
			CPUArch:            hostCPUArch(),
			CreatedAt:          createdAt.UTC().Format(time.RFC3339),
			DefaultWorkdir:     "/data",
		},
		SchemaVersion: metadataSchemaVersion,
		SourceImage:   sourceRef,
		RootfsDriveID: options.rootfsDriveID,
	}
	payload, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal snapfiles metadata: %w", err)
	}
	return payload, nil
}
