package cmd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestMigratePoolManifestConvertsLegacyRuntimeAndMaterializesDefaults(t *testing.T) {
	input := []byte(`apiVersion: sandbox.fast.io/v1alpha1
kind: SandboxPool
metadata:
  name: legacy
  labels:
    keep: me
spec:
  capacity: {poolMin: 1, poolMax: 2, bufferMin: 0, bufferMax: 1}
  runtimeType: gvisor
  runtimeClassName: gvisor
  containerdRuntimeHandler: io.containerd.runsc.v1
  fastletTemplate:
    spec:
      containers:
      - {name: fastlet, image: fastlet:test}
`)

	output, changed, err := migratePoolManifest(input)
	require.NoError(t, err)
	require.True(t, changed)
	var document map[string]any
	require.NoError(t, yaml.Unmarshal(output, &document))
	spec := document["spec"].(map[string]any)
	require.Equal(t, "gvisor", spec["runtime"])
	require.NotContains(t, spec, "runtimeType")
	require.NotContains(t, spec, "runtimeClassName")
	require.NotContains(t, spec, "containerdRuntimeHandler")
	require.EqualValues(t, 5, spec["maxSandboxesPerPod"])
	require.Equal(t, "minimal", spec["infraProfile"])
	require.Equal(t, map[string]any{"cpu": "1", "memory": "512Mi", "pids": 256}, spec["sandboxResources"])
	require.Equal(t, "me", document["metadata"].(map[string]any)["labels"].(map[string]any)["keep"])
}

func TestMigratePoolManifestRejectsConflictingRuntimeAndUnsafeOverride(t *testing.T) {
	base := `apiVersion: sandbox.fast.io/v1alpha1
kind: SandboxPool
metadata: {name: legacy}
spec:
  capacity: {poolMin: 1, poolMax: 1, bufferMin: 0, bufferMax: 0}
  fastletTemplate: {spec: {containers: [{name: fastlet, image: fastlet:test}]}}
`
	_, _, err := migratePoolManifest([]byte(base + "  runtime: container\n  runtimeType: container\n"))
	require.ErrorContains(t, err, "cannot be combined")

	_, _, err = migratePoolManifest([]byte(base + "  runtimeType: gvisor\n  containerdRuntimeHandler: custom\n"))
	require.ErrorContains(t, err, "does not match")
}

func TestMigratePoolManifestCheckIsStableAfterConversion(t *testing.T) {
	input := []byte(`apiVersion: sandbox.fast.io/v1alpha1
kind: SandboxPool
metadata: {name: current}
spec:
  capacity: {poolMin: 1, poolMax: 1, bufferMin: 0, bufferMax: 0}
  maxSandboxesPerPod: 3
  runtime: container
  sandboxResources: {cpu: 500m, memory: 256Mi, pids: 128}
  infraProfile: minimal
  fastletTemplate: {spec: {containers: [{name: fastlet, image: fastlet:test}]}}
`)

	output, changed, err := migratePoolManifest(input)
	require.NoError(t, err)
	require.False(t, changed)
	require.Contains(t, string(output), "runtime: container")
	require.False(t, strings.Contains(string(output), "runtimeType"))
}

func TestMigratePoolManifestConvertsEveryYAMLDocument(t *testing.T) {
	document := `apiVersion: sandbox.fast.io/v1alpha1
kind: SandboxPool
metadata: {name: NAME}
spec:
  capacity: {poolMin: 1, poolMax: 1, bufferMin: 0, bufferMax: 0}
  runtimeType: container
  fastletTemplate: {spec: {containers: [{name: fastlet, image: fastlet:test}]}}
`
	input := []byte(strings.ReplaceAll(document, "NAME", "first") + "---\n" + strings.ReplaceAll(document, "NAME", "second"))
	output, changed, err := migratePoolManifest(input)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, 2, strings.Count(string(output), "kind: SandboxPool"))
	require.Equal(t, 2, strings.Count(string(output), "runtime: container"))
	require.NotContains(t, string(output), "runtimeType")
}
