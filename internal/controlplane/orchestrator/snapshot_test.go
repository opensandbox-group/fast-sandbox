package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	"fast-sandbox/internal/controlplane/placement"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

// newSnapshotDispatchHarness builds an Orchestrator whose target Sandbox is
// durably assigned to one registry fastlet. The candidate's capabilities are
// the caller's version-skew scenario; the heartbeat is marked observed so a
// missing capability reads as confirmed, not unobserved.
func newSnapshotDispatchHarness(t *testing.T, capabilities []string) (*Orchestrator, *fakeFastletClient, *apiv1alpha2.Sandbox, *apiv1alpha2.SandboxSnapshot) {
	t.Helper()
	orchestrator, registry, fastletClient, sandbox := newHarness(t)
	sandbox.UID = "sandbox-uid-a"
	candidate := placement.FastletInfo{
		ID: "fastlet-a", PodName: "fastlet-a", PodUID: "pod-a", PodIP: "10.0.0.1",
		RuntimeName: apiv1alpha2.RuntimeContainer, RuntimeProfileHash: "runtime-a", ResourceProfileHash: "resources-a", InfraRevision: "infra-a",
		Capabilities: capabilities, LastHeartbeat: time.Now(),
	}
	registry.fastlets[candidate.ID] = candidate
	envelope, err := AssignmentForCandidate(candidate, 1, 1, 1, "runtime-a")
	require.NoError(t, err)
	require.NoError(t, assignment.SetAssignmentAnnotation(sandbox, envelope))
	snapshot := &apiv1alpha2.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-a", Namespace: "default", UID: "snapshot-uid-a"},
		Spec:       apiv1alpha2.SandboxSnapshotSpec{TemplateName: "app-v2"},
	}
	return orchestrator, fastletClient, sandbox, snapshot
}

func TestCreateSnapshotDispatchesSpecMode(t *testing.T) {
	orchestrator, fastletClient, sandbox, snapshot := newSnapshotDispatchHarness(t, []string{fastletapi.CapabilityRootfsOnlySnapshots})
	fastletClient.createSnapshot = func(_ string, request *fastletapi.CreateSnapshotRequest) (*fastletapi.CreateSnapshotResponse, error) {
		return &fastletapi.CreateSnapshotResponse{Disposition: fastletapi.CreateDispositionCreated,
			Snapshot: &fastletapi.SnapshotStatus{SnapshotID: "snap-1", Phase: fastletapi.SnapshotPhasePending}}, nil
	}

	// A legacy (pre-mode) intent resolves to Full; an explicit RootfsOnly
	// intent dispatches RootfsOnly.
	for _, tc := range []struct {
		specMode apiv1alpha2.SandboxSnapshotMode
		want     fastletapi.SnapshotMode
	}{
		{specMode: "", want: fastletapi.SnapshotModeFull},
		{specMode: apiv1alpha2.SandboxSnapshotModeFull, want: fastletapi.SnapshotModeFull},
		{specMode: apiv1alpha2.SandboxSnapshotModeRootfsOnly, want: fastletapi.SnapshotModeRootfsOnly},
	} {
		snapshot.Spec.Mode = tc.specMode
		observed, err := orchestrator.CreateSnapshot(context.Background(), snapshot, sandbox)
		require.NoError(t, err)
		require.NotNil(t, observed)
		require.Equal(t, tc.want, fastletClient.snapshotRequest.Snapshot.Mode)
		require.Equal(t, "app-v2", fastletClient.snapshotRequest.Snapshot.TemplateName)
	}
}

func TestCreateSnapshotRequiresRootfsOnlyCapability(t *testing.T) {
	// An observed Fastlet reporting no capabilities would silently ignore
	// the mode field: the dispatch gate must fail closed before the call.
	orchestrator, fastletClient, sandbox, snapshot := newSnapshotDispatchHarness(t, nil)
	dispatched := false
	fastletClient.createSnapshot = func(string, *fastletapi.CreateSnapshotRequest) (*fastletapi.CreateSnapshotResponse, error) {
		dispatched = true
		return &fastletapi.CreateSnapshotResponse{Disposition: fastletapi.CreateDispositionCreated,
			Snapshot: &fastletapi.SnapshotStatus{SnapshotID: "snap-1", Phase: fastletapi.SnapshotPhasePending}}, nil
	}
	snapshot.Spec.Mode = apiv1alpha2.SandboxSnapshotModeRootfsOnly

	_, err := orchestrator.CreateSnapshot(context.Background(), snapshot, sandbox)
	require.Error(t, err)
	require.False(t, dispatched, "the gated request must never reach the Fastlet")
	var failure *fastletapi.FastletError
	require.ErrorAs(t, err, &failure)
	require.Equal(t, fastletapi.ErrorSnapshotUnsupported, failure.Code)

	// Full snapshots stay dispatchable without any capability.
	snapshot.Spec.Mode = apiv1alpha2.SandboxSnapshotModeFull
	_, err = orchestrator.CreateSnapshot(context.Background(), snapshot, sandbox)
	require.NoError(t, err)
	require.True(t, dispatched)
}

// An unobserved capability is unknown, not absent: after a control-plane
// restart the Pod informer can populate the registry before the first
// heartbeat arrives, and permanently failing the one-shot snapshot in that
// window would burn a valid request_id. The gate refreshes once and
// otherwise stays retryable; a refreshed heartbeat can satisfy it in the
// same call.
func TestCreateSnapshotUnobservedCapabilityStaysRetryable(t *testing.T) {
	_, _, fastletClient, _ := newHarness(t)
	candidate := placement.FastletInfo{
		ID: "fastlet-a", PodName: "fastlet-a", PodUID: "pod-a", PodIP: "10.0.0.1",
		RuntimeName: apiv1alpha2.RuntimeContainer, RuntimeProfileHash: "runtime-a", ResourceProfileHash: "resources-a", InfraRevision: "infra-a",
		// No capabilities and no heartbeat yet: the informer-only view a
		// restarted control plane sees before the first heartbeat lands.
	}
	registry := placement.NewInMemoryRegistry()
	registry.UpsertPod(candidate)
	orchestrator := &Orchestrator{Registry: registry, FastletClient: fastletClient}
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default", UID: "sandbox-uid-a"},
		Spec:       apiv1alpha2.SandboxSpec{Image: "oci://r.example/ns/template:base", PoolRef: "pool-a"},
	}
	envelope, err := AssignmentForCandidate(candidate, 1, 1, 1, "runtime-a")
	require.NoError(t, err)
	require.NoError(t, assignment.SetAssignmentAnnotation(sandbox, envelope))
	snapshot := &apiv1alpha2.SandboxSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-a", Namespace: "default", UID: "snapshot-uid-a"},
		Spec:       apiv1alpha2.SandboxSnapshotSpec{Mode: apiv1alpha2.SandboxSnapshotModeRootfsOnly},
	}

	// The refresh cannot reach a heartbeat: the capability stays unobserved
	// and the rejection must stay retryable (ErrAssignedFastletUnavailable),
	// never the deterministic ErrorSnapshotUnsupported that permanently
	// fails the one-shot object.
	fastletClient.heartbeat = func(string, *fastletapi.HeartbeatRequest) (*fastletapi.HeartbeatResponse, error) {
		return nil, errors.New("fastlet heartbeat unreachable")
	}
	dispatched := false
	fastletClient.createSnapshot = func(string, *fastletapi.CreateSnapshotRequest) (*fastletapi.CreateSnapshotResponse, error) {
		dispatched = true
		return &fastletapi.CreateSnapshotResponse{Disposition: fastletapi.CreateDispositionCreated,
			Snapshot: &fastletapi.SnapshotStatus{SnapshotID: "snap-1", Phase: fastletapi.SnapshotPhasePending}}, nil
	}
	_, err = orchestrator.CreateSnapshot(context.Background(), snapshot, sandbox)
	require.ErrorIs(t, err, ErrAssignedFastletUnavailable)
	require.False(t, dispatched)

	// The next attempt finds the capable heartbeat via the synchronous
	// refresh and dispatches with the mode intact.
	fastletClient.heartbeat = func(string, *fastletapi.HeartbeatRequest) (*fastletapi.HeartbeatResponse, error) {
		return &fastletapi.HeartbeatResponse{
			FastletStatus: fastletapi.FastletStatus{
				FastletPodUID: "pod-a", RuntimeReady: true,
				RuntimeProfileHash: "runtime-a", ResourceProfileHash: "resources-a", InfraRevision: "infra-a",
				Capabilities: []string{fastletapi.CapabilityRootfsOnlySnapshots},
			},
			Sequence: 1, Cache: fastletapi.CacheSnapshot{Epoch: "boot-a", Revision: 1},
		}, nil
	}
	observed, err := orchestrator.CreateSnapshot(context.Background(), snapshot, sandbox)
	require.NoError(t, err)
	require.NotNil(t, observed)
	require.True(t, dispatched)
	require.Equal(t, fastletapi.SnapshotModeRootfsOnly, fastletClient.snapshotRequest.Snapshot.Mode)
}

func TestProjectSnapshotStatusReportsProducedMode(t *testing.T) {
	status := &apiv1alpha2.SandboxSnapshotStatus{}
	ProjectSnapshotStatus(status, apiv1alpha2.SandboxSnapshotModeRootfsOnly, &fastletapi.SnapshotStatus{
		Phase: fastletapi.SnapshotPhaseSucceeded, Mode: fastletapi.SnapshotModeRootfsOnly,
		ManifestRef: "oci://r.example/ns/snap@sha256:abc",
	})
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseSucceeded, status.Phase)
	require.Equal(t, apiv1alpha2.SandboxSnapshotModeRootfsOnly, status.Mode)
}

func TestProjectSnapshotStatusLegacyFullObservation(t *testing.T) {
	// A pre-mode Fastlet never echoes a mode: a Full request still succeeds
	// and normalizes to Full.
	status := &apiv1alpha2.SandboxSnapshotStatus{}
	ProjectSnapshotStatus(status, "", &fastletapi.SnapshotStatus{Phase: fastletapi.SnapshotPhaseSucceeded})
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseSucceeded, status.Phase)
	require.Equal(t, apiv1alpha2.SandboxSnapshotModeFull, status.Mode)
}

func TestProjectSnapshotStatusFailsClosedOnModeDrift(t *testing.T) {
	// Defense in depth behind the dispatch gate: a success observation whose
	// produced artifact class does not match the request never lands as
	// Succeeded — an old Fastlet that ignored the mode reports no mode (Full)
	// from a RootfsOnly request.
	status := &apiv1alpha2.SandboxSnapshotStatus{}
	ProjectSnapshotStatus(status, apiv1alpha2.SandboxSnapshotModeRootfsOnly, &fastletapi.SnapshotStatus{
		Phase: fastletapi.SnapshotPhaseSucceeded, SnapshotID: "snap-1",
		ManifestRef: "oci://r.example/ns/snap@sha256:abc", ArtifactDigest: "abc", SizeBytes: 42,
	})
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseFailed, status.Phase)
	require.Equal(t, apiv1alpha2.SandboxSnapshotModeFull, status.Mode)
	require.NotNil(t, status.CompletedAt)
	condition := status.Conditions[len(status.Conditions)-1]
	require.Equal(t, apiv1alpha2.SandboxSnapshotConditionCompleted, condition.Type)
	require.Equal(t, metav1.ConditionFalse, condition.Status)
	require.Equal(t, "SnapshotModeDrift", condition.Reason)
	// The drift rejection still published: the artifact facts stay on the
	// status so deletion retains remote cleanup ownership.
	require.Equal(t, "snap-1", status.SnapshotID)
	require.Equal(t, "oci://r.example/ns/snap@sha256:abc", status.ManifestRef)
	require.Equal(t, "abc", status.ArtifactDigest)
	require.Equal(t, int64(42), status.SizeBytes)

	// Terminal monotonicity: a later drift projection cannot roll a
	// Succeeded snapshot back.
	succeeded := &apiv1alpha2.SandboxSnapshotStatus{Phase: apiv1alpha2.SandboxSnapshotPhaseSucceeded}
	ProjectSnapshotStatus(succeeded, apiv1alpha2.SandboxSnapshotModeRootfsOnly, &fastletapi.SnapshotStatus{Phase: fastletapi.SnapshotPhaseSucceeded})
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseSucceeded, succeeded.Phase)
}
