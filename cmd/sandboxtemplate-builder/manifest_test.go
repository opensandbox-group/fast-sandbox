package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/artifacts"
	"fast-sandbox/internal/guestcontrol"
)

func TestNativeManifestExcludesStaleOverlayBDLayers(t *testing.T) {
	spec, kernel, rootfs, vmstate, memory := buildManifestFixture(t)
	workdir := filepath.Dir(rootfs)
	for _, kind := range []string{"rootfs", "memory"} {
		dir := filepath.Join(workdir, "overlaybd", kind)
		require.NoError(t, os.MkdirAll(dir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "layer.lsmt"), []byte("stale-layer"), 0600))
	}
	raw, err := stageManifest(spec, "sha256:source", kernel, rootfs, vmstate, memory, workdir, nativeManifestPhases(t, kernel), nil)
	require.NoError(t, err)
	var manifest struct {
		Format string         `json:"format"`
		Files  map[string]any `json:"files"`
	}
	require.NoError(t, json.Unmarshal(raw, &manifest))
	require.Empty(t, manifest.Format)
	require.Len(t, manifest.Files, 3)
	for _, name := range []string{rootfsImageName, vmstateFileName, memoryFileName} {
		require.Contains(t, manifest.Files, name)
	}
	sums, err := os.ReadFile(filepath.Join(workdir, "SHA256SUMS"))
	require.NoError(t, err)
	require.NotContains(t, string(sums), "overlaybd")
}

// TestCompatibilityCPUTemplate: the manifest records the template the
// snapshot stage actually booted with; an empty effective template (the raw
// host CPUID fallback) is recorded as "none" instead of an empty field.
func TestCompatibilityCPUTemplate(t *testing.T) {
	if got, want := compatibilityCPUTemplate("T2"), "T2"; got != want {
		t.Fatalf("compatibilityCPUTemplate = %q, want %q", got, want)
	}
	if got, want := compatibilityCPUTemplate("T2A"), "T2A"; got != want {
		t.Fatalf("compatibilityCPUTemplate = %q, want %q", got, want)
	}
	if got, want := compatibilityCPUTemplate(""), "none"; got != want {
		t.Fatalf("compatibilityCPUTemplate = %q, want %q", got, want)
	}
}

func TestManifestRecordsFreePageReporting(t *testing.T) {
	for _, test := range []struct {
		name, log string
		want      bool
	}{
		{name: "supported guest", log: freePageReportingReadyMarker + "\n", want: true},
		{name: "unsupported guest", log: "SANDBOX_FREE_PAGE_REPORTING_UNAVAILABLE missing_guest_kernel_support\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec, kernel, rootfs, vmstate, memory := buildManifestFixture(t)
			workdir := filepath.Dir(rootfs)
			bootLog := filepath.Join(workdir, "boot.console.log")
			require.NoError(t, os.WriteFile(bootLog, []byte(test.log), 0o600))
			supported, err := readFreePageReportingSupport(bootLog)
			require.NoError(t, err)
			report, err := json.Marshal(func() snapshotPhaseTimings {
				p := nativeManifestPhases(t, kernel)
				p.FreePageReporting = supported
				return p
			}())
			require.NoError(t, err)
			var fields map[string]any
			require.NoError(t, json.Unmarshal(report, &fields))
			require.Equal(t, test.want, fields["freePageReporting"], "the stage report must include false explicitly")
			var phases snapshotPhaseTimings
			require.NoError(t, json.Unmarshal(report, &phases))
			raw, err := stageManifest(spec, "sha256:source", kernel, rootfs, vmstate, memory, workdir, phases, nil)
			require.NoError(t, err)
			manifest, err := artifacts.DecodeVMManifest(raw)
			require.NoError(t, err)
			require.NotNil(t, manifest.FreePageReporting)
			require.Equal(t, test.want, *manifest.FreePageReporting)
		})
	}
}

// buildManifestFixture stages the files buildManifest checksums.
func buildManifestFixture(t *testing.T) (apiv1alpha2.SandboxTemplateSpec, string, string, string, string) {
	t.Helper()
	workdir := t.TempDir()
	for _, name := range []string{rootfsImageName, vmstateFileName, memoryFileName} {
		if err := os.WriteFile(filepath.Join(workdir, name), []byte(name), 0o600); err != nil {
			t.Fatalf("stage %s: %v", name, err)
		}
	}
	spec := apiv1alpha2.SandboxTemplateSpec{
		Image:   "registry.example.com/sandbox:v1",
		Machine: apiv1alpha2.MachineSpec{VCPU: "2", Memory: "1Gi"},
		Output: apiv1alpha2.OutputSpec{
			Format:     apiv1alpha2.ArtifactFormatNative,
			RootfsSize: "1Gi",
		},
	}
	require.NoError(t, os.WriteFile(filepath.Join(workdir, "firecracker"), []byte("#!/bin/sh\necho Firecracker v1.16.1\n"), 0755))
	t.Setenv("PATH", workdir+string(os.PathListSeparator)+os.Getenv("PATH"))
	kernel := filepath.Join(workdir, "vmlinux-6.1.177")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o600); err != nil {
		t.Fatalf("stage kernel: %v", err)
	}
	return spec, kernel,
		filepath.Join(workdir, rootfsImageName),
		filepath.Join(workdir, vmstateFileName),
		filepath.Join(workdir, memoryFileName)
}

// TestBuildManifestMitmCALineage: platform.egressMitmCA is present exactly
// when the build baked a CA.
func TestBuildManifestMitmCALineage(t *testing.T) {
	spec, kernel, rootfs, vmstate, memory := buildManifestFixture(t)

	manifest, err := buildManifest(spec, "sha256:source", kernel, rootfs, vmstate, memory,
		map[string]string{}, 1, "T2", false, nil)
	if err != nil {
		t.Fatalf("buildManifest: %v", err)
	}
	require.Empty(t, manifest.Platform.EgressMitmCA)
	require.Equal(t, artifacts.VMManifestVersion, manifest.SchemaVersion)
	wantEntry := mitmCALineage{
		CertSHA256: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
		SPKISHA256: "6a21c9f0e0d2e0d0f76b4b1e5f0e8e5e7e9d0c1b2a3f4b5c6d7e8f9a0b1c2d3e",
		NotAfter:   "2036-01-01T00:00:00Z",
	}
	manifest, err = buildManifest(spec, "sha256:source", kernel, rootfs, vmstate, memory,
		map[string]string{}, 1, "T2", false, []mitmCALineage{wantEntry})
	if err != nil {
		t.Fatalf("buildManifest: %v", err)
	}
	require.NotNil(t, manifest.Platform.EgressMitmCA)
	require.Equal(t, []mitmCALineage{wantEntry}, manifest.Platform.EgressMitmCA.Certificates)
}

func nativeManifestPhases(t *testing.T, kernel string) snapshotPhaseTimings {
	t.Helper()
	entry, err := artifacts.FileEntry(kernel, map[string]string{})
	require.NoError(t, err)
	return snapshotPhaseTimings{CPUTemplate: "T2", KernelSHA256: entry.SHA256, KernelCmdline: "root=/dev/vda rw", GuestControl: &artifacts.VMGuestControl{Protocol: guestcontrol.Protocol, Transport: "vsock", Port: 1234, HelperVersion: "sha256:" + entry.SHA256}}
}
