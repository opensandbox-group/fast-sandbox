package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
)

func TestManifestRecordsObservedFreePageReporting(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(map[bool]string{true: "supported guest", false: "unsupported guest"}[enabled], func(t *testing.T) {
			dir := t.TempDir()
			for _, file := range []string{"kernel", rootfsImageName, vmstateFileName, memoryFileName} {
				require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte(file), 0o600))
			}
			spec := apiv1alpha2.SandboxTemplateSpec{
				Machine: apiv1alpha2.MachineSpec{VCPU: "1", Memory: "2Gi"},
				Output:  apiv1alpha2.OutputSpec{RootfsSize: "1Gi"},
			}
			phases := snapshotPhaseTimings{CPUTemplate: "T2", FreePageReporting: enabled}
			payload, err := stageManifest(spec, "sha256:test", filepath.Join(dir, "kernel"),
				filepath.Join(dir, rootfsImageName), filepath.Join(dir, vmstateFileName),
				filepath.Join(dir, memoryFileName), nil, dir, phases)
			require.NoError(t, err)
			var manifest map[string]any
			require.NoError(t, json.Unmarshal(payload, &manifest))
			require.Equal(t, enabled, manifest["freePageReporting"])
			require.NotContains(t, manifest, "memoryReclaim")
			require.Equal(t, "T2", manifest["compatibility"].(map[string]any)["cpuTemplate"])
		})
	}
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
