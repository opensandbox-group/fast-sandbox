//go:build boxlite_native

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"fast-sandbox/internal/api"
	"fast-sandbox/internal/boxlitesidecar"
	"fast-sandbox/internal/boxlitestate"
	"fast-sandbox/internal/boxlitewire"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"github.com/stretchr/testify/require"
)

func TestNativeResourceOptionsAndCapabilityBoundary(t *testing.T) {
	cpus, memoryMiB, err := resourceOptions("250m", "256Mi")
	require.NoError(t, err)
	require.Equal(t, 1, cpus, "BoxLite VM vCPU is integer-valued; host cgroup must enforce the fractional Pool CPU")
	require.Equal(t, 256, memoryMiB)
	_, _, err = resourceOptions("0", "256Mi")
	require.Error(t, err)
	capabilities := (&nativeBackend{}).Capabilities(context.Background())
	require.False(t, capabilities.Capabilities[boxlitewire.CapabilityResourceLimit])
	require.True(t, capabilities.Capabilities[boxlitewire.CapabilityLocalForward])
	require.False(t, capabilities.Ready)
}

func TestNativeRuntimeOptionsValidateRegistryTransport(t *testing.T) {
	t.Setenv("FAST_SANDBOX_BOXLITE_REGISTRY_HOST", "127.0.0.1:15000")
	t.Setenv("FAST_SANDBOX_BOXLITE_REGISTRY_TRANSPORT", "http")
	options, err := nativeRuntimeOptions(t.TempDir())
	require.NoError(t, err)
	require.Len(t, options, 2)

	t.Setenv("FAST_SANDBOX_BOXLITE_REGISTRY_TRANSPORT", "ftp")
	_, err = nativeRuntimeOptions(t.TempDir())
	require.ErrorContains(t, err, "unsupported BoxLite registry transport")

	t.Setenv("FAST_SANDBOX_BOXLITE_REGISTRY_TRANSPORT", "https")
	t.Setenv("FAST_SANDBOX_BOXLITE_REGISTRY_SKIP_VERIFY", "not-a-boolean")
	_, err = nativeRuntimeOptions(t.TempDir())
	require.ErrorContains(t, err, "FAST_SANDBOX_BOXLITE_REGISTRY_SKIP_VERIFY")
}

func TestEnsureOwnerRecordFencesFastletPodUID(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, ensureOwnerRecord(home, "pod-a"))
	require.NoError(t, ensureOwnerRecord(home, "pod-a"))
	require.ErrorContains(t, ensureOwnerRecord(home, "pod-b"), "owner fence")
}

func TestNativeBundleCopiesOnlySharedInfraArtifacts(t *testing.T) {
	root := t.TempDir()
	artifactRoot := filepath.Join(root, "artifacts")
	require.NoError(t, os.MkdirAll(artifactRoot, 0700))
	source := filepath.Join(artifactRoot, "sandbox-tunnel")
	require.NoError(t, os.WriteFile(source, []byte("tunnel"), 0555))
	bundleRoot := filepath.Join(root, "bundle")
	backend := &nativeBackend{artifactRoot: artifactRoot}
	record := &nativeRecord{
		BundleRoot: bundleRoot,
		Request: boxlitewire.EnsureRequest{Artifacts: []boxlitewire.Artifact{{
			Source: source, Destination: fastletinfra.SandboxTunnelContainerPath,
		}}},
	}
	require.NoError(t, backend.prepareBundle(record))
	payload, err := os.ReadFile(filepath.Join(bundleRoot, "bin", "sandbox-tunnel"))
	require.NoError(t, err)
	require.Equal(t, "tunnel", string(payload))

	record.Request.Artifacts[0].Source = filepath.Join(root, "outside")
	require.NoError(t, os.WriteFile(record.Request.Artifacts[0].Source, []byte("outside"), 0555))
	require.Error(t, backend.prepareBundle(record))
}

func TestNativeEnsureHashFencesImmutableSpec(t *testing.T) {
	request := boxlitewire.EnsureRequest{
		Namespace: "ns", TunnelGuestPort: 19090,
		Sandbox: api.SandboxSpec{SandboxID: "uid-a", Image: "alpine:latest", FastletPodUID: "pod-a", InstanceGeneration: 1, AssignmentAttempt: 1},
	}
	first, err := ensureHash(request)
	require.NoError(t, err)
	request.Sandbox.Image = "busybox:latest"
	second, err := ensureHash(request)
	require.NoError(t, err)
	require.NotEqual(t, first, second)
}

func TestNativeEnsureReturnsTypedImmutableSpecConflict(t *testing.T) {
	request := boxlitewire.EnsureRequest{
		Namespace: "default", TunnelGuestPort: 19090,
		Sandbox: api.SandboxSpec{
			SandboxID: "sandbox-a", Image: "alpine:latest", CPU: "1", Memory: "256Mi",
			FastletPodUID: "pod-a", InstanceGeneration: 1, AssignmentAttempt: 1,
		},
		Artifacts: []boxlitewire.Artifact{{Destination: fastletinfra.SandboxTunnelContainerPath}},
	}
	hash, err := ensureHash(request)
	require.NoError(t, err)
	backend := &nativeBackend{records: map[string]*nativeRecord{
		request.Sandbox.SandboxID: {SpecHash: hash},
	}}
	request.Sandbox.Image = "busybox:latest"
	_, err = backend.Ensure(context.Background(), request)
	var typed *boxlitesidecar.Error
	require.ErrorAs(t, err, &typed)
	require.Equal(t, boxlitewire.ErrorImmutableSpecConflict, typed.Code)
}

func TestNativeLoadRecordsValidatesOwnerFilenameAndBundleFences(t *testing.T) {
	root := t.TempDir()
	metadataRoot := filepath.Join(root, boxlitestate.MetadataDirectoryName)
	bundleRoot := filepath.Join(root, boxlitestate.BundleDirectoryName)
	require.NoError(t, os.MkdirAll(metadataRoot, 0700))
	require.NoError(t, os.MkdirAll(bundleRoot, 0700))
	request := boxlitewire.EnsureRequest{
		Namespace: "default", TunnelGuestPort: 19090,
		Sandbox: api.SandboxSpec{
			SandboxID: "sandbox-a", Image: "alpine:latest", CPU: "250m", Memory: "256Mi",
			FastletPodUID: "pod-a", InstanceGeneration: 1, AssignmentAttempt: 1,
		},
		Artifacts: []boxlitewire.Artifact{{
			Source: "/opt/fast-sandbox/infra/tunnel", Destination: fastletinfra.SandboxTunnelContainerPath,
		}},
	}
	hash, err := ensureHash(request)
	require.NoError(t, err)
	credential, err := fastletnetwork.GenerateLocalForwardCredential()
	require.NoError(t, err)
	record := nativeRecord{
		Version: boxlitestate.Version, Namespace: request.Namespace, SpecHash: hash, Request: request,
		HostPort: 21000, TunnelCredential: credential, CreatedAt: 1,
		BundleRoot: filepath.Join(bundleRoot, boxlitestate.SafeSegment(request.Sandbox.SandboxID), hash),
	}
	recordPath := filepath.Join(metadataRoot, boxlitestate.RecordFileName(request.Sandbox.SandboxID))
	writeNativeRecord(t, recordPath, record)
	backend := &nativeBackend{podUID: "pod-a", metadataRoot: metadataRoot, bundleRoot: bundleRoot, records: map[string]*nativeRecord{}}
	require.NoError(t, backend.loadRecords())

	record.Request.Sandbox.FastletPodUID = "pod-b"
	record.SpecHash, err = ensureHash(record.Request)
	require.NoError(t, err)
	record.BundleRoot = filepath.Join(bundleRoot, boxlitestate.SafeSegment(record.Request.Sandbox.SandboxID), record.SpecHash)
	writeNativeRecord(t, recordPath, record)
	require.ErrorContains(t, backend.loadRecords(), "owner fence mismatch")

	record.Request.Sandbox.FastletPodUID = "pod-a"
	record.SpecHash, err = ensureHash(record.Request)
	require.NoError(t, err)
	record.BundleRoot = "/"
	writeNativeRecord(t, recordPath, record)
	require.ErrorContains(t, backend.loadRecords(), "bundle fence mismatch")
}

func writeNativeRecord(t *testing.T, path string, record nativeRecord) {
	t.Helper()
	payload, err := json.Marshal(record)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, payload, 0600))
}
