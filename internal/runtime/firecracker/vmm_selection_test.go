package firecracker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/artifacts"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	"fast-sandbox/internal/firecrackervmm"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

func vmmTestBundle(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "catalog.json"), firecrackervmm.Releases, 0644))
	for _, version := range []string{"1.16.1", "1.17.0"} {
		dir := filepath.Join(root, "versions", version)
		require.NoError(t, os.MkdirAll(dir, 0755))
		for name, label := range map[string]string{"firecracker": "Firecracker", "jailer": "Jailer"} {
			require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("#!/bin/sh\necho '%s v%s'\n", label, version)), 0755))
		}
	}
	return root
}

func TestManagedVMMSelectionsRemainPerInstance(t *testing.T) {
	root := vmmTestBundle(t)
	d := &Driver{config: runtimecatalog.FirecrackerConfig{VMMDir: root, BinaryPath: "/legacy/firecracker", JailerPath: "/legacy/jailer"}, fcVersion: "1.16.1"}
	for _, version := range []string{"1.16.1", "1.17.0"} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			selected, err := d.selectVMM(t.Context(), version)
			require.NoError(t, err)
			require.Equal(t, version, d.selectionVersion(selected))
			require.Equal(t, filepath.Join(root, "versions", version, "jailer"), selected.JailerPath)
			directory := t.TempDir()
			require.NoError(t, saveState(directory, &SandboxState{VMM: selected, Phase: PhaseRunning, Config: fastletapi.RuntimeSandboxConfig{Identity: fastletapi.SandboxIdentity{SandboxUID: version}}}))
			state, err := loadState(directory)
			require.NoError(t, err)
			require.Equal(t, selected, state.VMM, "restart must retain the original selection")
			require.Equal(t, "/legacy/firecracker", d.config.BinaryPath)
		})
	}
}

func TestPreparedVMMRequiresMatchingCLISelection(t *testing.T) {
	root := vmmTestBundle(t)
	d := &Driver{config: runtimecatalog.FirecrackerConfig{VMMDir: root, StateRoot: t.TempDir()}}
	manifest := artifacts.VMManifest{Compatibility: artifacts.SnapshotCompatibility{FirecrackerVersion: "1.17.0"}}
	selected, err := d.selectVMM(t.Context(), "1.17.0")
	require.NoError(t, err)
	dir := filepath.Join(d.ociRoot(), "sandboxes", "vm")
	require.NoError(t, os.MkdirAll(dir, 0755))
	for _, version := range []string{"1.16.1", "1.17.0"} {
		selected.Version = version
		raw, err := json.Marshal(map[string]any{"vmm": selected})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "state.json"), raw, 0600))
		_, err = d.preparedVMM(t.Context(), "vm", manifest)
		if version == "1.17.0" {
			require.NoError(t, err)
		} else {
			require.ErrorContains(t, err, "disagrees")
		}
	}
	_, err = d.selectVMM(t.Context(), "")
	require.ErrorContains(t, err, "no Firecracker version")
	_, err = d.selectVMM(t.Context(), "1.16.2")
	require.ErrorContains(t, err, "unavailable")
	args := d.fsbArgs()
	require.Contains(t, args, "--vmm-dir")
	require.NotContains(t, args, "--firecracker")
}

func TestManagedRootfsCaptureVersionGate(t *testing.T) {
	for _, version := range []string{"1.16.1", "1.16.2", "1.17.0"} {
		err := checkRootfsCaptureVersion(&firecrackervmm.Selection{Version: version}, version)
		if version == "1.17.0" {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
	require.Error(t, checkRootfsCaptureVersion(&firecrackervmm.Selection{Version: "1.17.0"}, "1.16.1"))
	require.Error(t, checkRootfsCaptureVersion(nil, "1.16.1"), "an explicit digest cannot bypass the lifecycle gate")
	require.NoError(t, checkRootfsCaptureVersion(nil, "1.17.0"))
	require.ErrorContains(t, qualifyVMMExecutable(&SandboxState{}, ""), "custom VMM requires")
}

func TestManagedVMMUnavailableIsArtifactMismatch(t *testing.T) {
	root := vmmTestBundle(t)
	d := &Driver{config: runtimecatalog.FirecrackerConfig{VMMDir: root, StateRoot: t.TempDir()}}
	_, err := d.selectVMM(t.Context(), "1.15.0")
	require.ErrorIs(t, err, ErrIncompatibleArtifact)
	require.ErrorIs(t, err, firecrackervmm.ErrVersionUnavailable)
	// A broken installation is a runtime fault, not an artifact mismatch.
	require.NoError(t, os.Remove(filepath.Join(root, "versions", "1.17.0", "firecracker")))
	_, err = d.selectVMM(t.Context(), "1.17.0")
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrIncompatibleArtifact)
}
