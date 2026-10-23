package fastlet

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCreateRequestSerializesIdentityDesiredSpecAndRequestMetadataOnce(t *testing.T) {
	request := CreateSandboxRequest{
		RequestID: "request-a",
		Identity: SandboxIdentity{
			SandboxUID: "uid-a", Namespace: "tenant-a", Name: "sandbox-a",
			InstanceGeneration: 2, RuntimeInstanceID: "runtime-a", AssignmentAttempt: 3,
			RouteGeneration: 4, FastletPodUID: "pod-a",
		},
		Sandbox:        SandboxDesiredSpec{Image: "alpine:latest"},
		ActionBindings: []ActionBindingInput{{Handler: "egress", Input: "allow: example.com"}},
	}
	encoded, err := json.Marshal(request)
	require.NoError(t, err)
	var object map[string]any
	require.NoError(t, json.Unmarshal(encoded, &object))
	require.Equal(t, "request-a", object["requestId"])

	identity := object["identity"].(map[string]any)
	require.Equal(t, "uid-a", identity["sandboxUid"])
	require.Equal(t, "tenant-a", identity["namespace"])
	require.Equal(t, "sandbox-a", identity["name"])
	require.NotContains(t, identity, "requestId")
	require.NotContains(t, identity, "image")

	desired := object["sandbox"].(map[string]any)
	require.Equal(t, "alpine:latest", desired["image"])
	require.NotContains(t, desired, "cpu")
	require.NotContains(t, desired, "memory")
	require.NotContains(t, desired, "pids")
	for _, key := range []string{"sandboxUid", "sandboxId", "namespace", "name", "requestId", "instanceGeneration", "runtimeInstanceId", "assignmentAttempt", "routeGeneration", "fastletPodUid"} {
		require.NotContains(t, desired, key)
	}

	binding := object["actionBindings"].([]any)[0].(map[string]any)
	require.Equal(t, map[string]any{"handler": "egress", "input": "allow: example.com"}, binding)
}

func TestEnsureSandboxInputKeepsInvocationConfigAndAllocationSeparate(t *testing.T) {
	input := EnsureSandboxInput{
		RequestID: "invocation-a",
		Sandbox: RuntimeSandboxConfig{
			Identity: SandboxIdentity{SandboxUID: "uid-a", Namespace: "tenant-a", Name: "sandbox-a"},
			Desired:  SandboxDesiredSpec{Image: "alpine:latest"},
			Limits:   SandboxRuntimeLimits{CPU: "500m", Memory: "256Mi", PIDs: 128},
		},
	}
	encoded, err := json.Marshal(input)
	require.NoError(t, err)
	var object map[string]any
	require.NoError(t, json.Unmarshal(encoded, &object))
	require.Equal(t, "invocation-a", object["requestId"])
	sandbox := object["sandbox"].(map[string]any)
	require.Equal(t, "uid-a", sandbox["identity"].(map[string]any)["sandboxUid"])
	require.Equal(t, "alpine:latest", sandbox["desired"].(map[string]any)["image"])
	require.Equal(t, "500m", sandbox["limits"].(map[string]any)["cpu"])
	for _, key := range []string{"sandboxId", "claimNamespace", "claimName", "requestId", "network", "networkIp", "networkSlotId"} {
		require.NotContains(t, sandbox, key)
	}

	allocation := RuntimeAllocation{Network: NetworkAllocation{SlotID: "slot-a", IP: "10.42.0.2"}}
	allocationJSON, err := json.Marshal(allocation)
	require.NoError(t, err)
	require.NotContains(t, string(allocationJSON), "invocation-a")
	require.Contains(t, string(allocationJSON), `"slotId":"slot-a"`)
}

func TestRuntimeSandboxConfigReadsLegacyDurableSpec(t *testing.T) {
	legacy := []byte(`{
		"identity":{"sandboxUid":"uid-a"},
		"spec":{"image":"alpine:latest","resourceProfileHash":"profile-a","cpu":"500m","memory":"256Mi","pids":128}
	}`)
	var config RuntimeSandboxConfig
	require.NoError(t, json.Unmarshal(legacy, &config))
	require.Equal(t, "uid-a", config.Identity.SandboxUID)
	require.Equal(t, "alpine:latest", config.Desired.Image)
	require.Equal(t, "profile-a", config.Desired.ResourceProfileHash)
	require.Equal(t, SandboxRuntimeLimits{CPU: "500m", Memory: "256Mi", PIDs: 128}, config.Limits)

	encoded, err := json.Marshal(config)
	require.NoError(t, err)
	require.Contains(t, string(encoded), `"desired"`)
	require.Contains(t, string(encoded), `"limits"`)
	require.NotContains(t, string(encoded), `"spec"`)
}

func TestRuntimeSandboxConfigRejectsMixedCurrentAndLegacyShapes(t *testing.T) {
	var config RuntimeSandboxConfig
	err := json.Unmarshal([]byte(`{"desired":{"image":"current"},"spec":{"image":"legacy"}}`), &config)
	require.ErrorContains(t, err, "cannot contain both desired and legacy spec")
}

func TestHeartbeatUsesAdmissionAndCacheAsSingleAuthorities(t *testing.T) {
	encoded, err := json.Marshal(HeartbeatResponse{
		FastletStatus: FastletStatus{RuntimeProfileHash: "runtime-a", Admission: AdmissionStatus{Capacity: 8, Running: 2, Used: 2}},
		Sequence:      7,
		Cache:         CacheSnapshot{Epoch: "epoch-a", Revision: 9, Full: true, Complete: true, Images: []string{"alpine:latest"}},
	})
	require.NoError(t, err)
	var object map[string]any
	require.NoError(t, json.Unmarshal(encoded, &object))
	require.NotContains(t, object, "capacity")
	require.NotContains(t, object, "observedAt")
	require.NotContains(t, object, "diagnostics")
	require.NotContains(t, object, "images")
	require.Equal(t, float64(8), object["admission"].(map[string]any)["capacity"])
	require.Equal(t, []any{"alpine:latest"}, object["cache"].(map[string]any)["images"])
}
