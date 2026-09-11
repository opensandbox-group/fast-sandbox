package fastpath

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	fastpathv2 "fast-sandbox/api/proto/v2"
	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	orchestration "fast-sandbox/internal/controlplane/orchestrator"
	"fast-sandbox/internal/controlplane/placement"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newSnapshotServer builds a Server whose default namespace holds one
// Ready, assigned Sandbox on fastlet-a.
func newSnapshotServer(t *testing.T) (*Server, *countingUIDClient, *fastpathFastlet) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	baseClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&apiv1alpha2.Sandbox{}, &apiv1alpha2.SandboxSnapshot{}).
		WithObjects(assignedReadySandbox(t, "sandbox-a", "sandbox-uid-a"), assignedReadySandbox(t, "sandbox-b", "sandbox-uid-b")).
		Build()
	k8sClient := &countingUIDClient{Client: baseClient}
	candidate := testCandidate("fastlet-a", "pod-a", "10.0.0.1")
	registry := &fastpathRegistry{
		candidates: []placement.FastletInfo{candidate},
		fastlets:   map[placement.FastletID]placement.FastletInfo{candidate.ID: candidate},
	}
	fastlet := &fastpathFastlet{}
	orchestrator := &orchestration.Orchestrator{Client: k8sClient, Registry: registry, FastletClient: fastlet}
	return &Server{K8sClient: k8sClient, RouteCache: baseClient, Orchestrator: orchestrator}, k8sClient, fastlet
}

func assignedReadySandbox(t *testing.T, name, uid string) *apiv1alpha2.Sandbox {
	t.Helper()
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(uid)},
		Spec:       apiv1alpha2.SandboxSpec{Image: "alpine:latest", PoolRef: "pool-a"},
	}
	envelope := assignment.AssignmentEnvelope{
		Version: assignment.AssignmentEnvelopeVersion, FastletName: "fastlet-a", FastletPodUID: "pod-a", NodeName: "node-a",
		Attempt: 1, InstanceGeneration: 1, RouteGeneration: 1, RuntimeInstanceID: "runtime-" + name,
		RuntimeProfileHash: "runtime-hash", ResourceProfileHash: "resource-hash", InfraRevision: "infra-hash",
	}
	require.NoError(t, assignment.SetAssignmentAnnotation(sandbox, envelope))
	sandbox.Status = apiv1alpha2.SandboxStatus{
		Placement: apiv1alpha2.PlacementStatus{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1},
		Runtime:   apiv1alpha2.RuntimeStatus{State: apiv1alpha2.RuntimeReady, Generation: 1},
		DataPlane: apiv1alpha2.DataPlaneStatus{State: apiv1alpha2.DataPlaneReady, RouteGeneration: 1},
	}
	return sandbox
}

func snapshotCreateRequest(requestID, templateName string) *fastpathv2.CreateSandboxSnapshotRequest {
	return &fastpathv2.CreateSandboxSnapshotRequest{
		RequestId: requestID, TemplateName: templateName,
		Sandbox: &fastpathv2.SandboxReference{
			NamespacedName: &fastpathv2.NamespacedName{Name: "sandbox-a", Namespace: "default"},
			ExpectedUid:    "sandbox-uid-a",
		},
	}
}

func TestCreateSandboxSnapshotPersistsTriggersAndProjects(t *testing.T) {
	server, k8sClient, _ := newSnapshotServer(t)
	response, err := server.CreateSandboxSnapshot(context.Background(), snapshotCreateRequest("snap-a", "app-v2"))
	require.NoError(t, err)
	require.Equal(t, "snap-a", response.Snapshot.Identity.GetName())
	require.Equal(t, fastpathv2.SnapshotPhase_SNAPSHOT_PHASE_CREATING, response.Snapshot.Phase)
	require.Equal(t, "app-v2", response.Snapshot.TemplateName)
	require.Equal(t, "fastlet-a", response.Snapshot.FastletName)
	require.NotEmpty(t, response.Snapshot.Identity.GetUid())

	var persisted apiv1alpha2.SandboxSnapshot
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "snap-a"}, &persisted))
	require.Equal(t, "sandbox-a", persisted.Spec.SandboxRef.Name)
	require.Equal(t, types.UID("sandbox-uid-a"), persisted.Spec.SandboxRef.UID)
	require.Equal(t, "app-v2", persisted.Spec.TemplateName)
	require.NotEmpty(t, persisted.Annotations[assignment.AnnotationCreateSpecHash])
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseCreating, persisted.Status.Phase)
	require.Equal(t, "fastlet-a", persisted.Status.FastletName)
	require.NotEmpty(t, persisted.Status.SnapshotID)
}

func TestCreateSandboxSnapshotRecordsSourceActionBindings(t *testing.T) {
	server, k8sClient, _ := newSnapshotServer(t)
	var sandbox apiv1alpha2.Sandbox
	require.NoError(t, server.K8sClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "sandbox-a"}, &sandbox))
	sandbox.Spec.ActionBindings = []apiv1alpha2.ActionBinding{
		{Handler: "egress", Input: `{"egressPolicy":"deny-all"}`},
	}
	require.NoError(t, server.K8sClient.Update(context.Background(), &sandbox))

	_, err := server.CreateSandboxSnapshot(context.Background(), snapshotCreateRequest("snap-a", "app-v2"))
	require.NoError(t, err)
	var persisted apiv1alpha2.SandboxSnapshot
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "snap-a"}, &persisted))
	var recorded []apiv1alpha2.ActionBinding
	require.NoError(t, json.Unmarshal([]byte(persisted.Annotations[assignment.AnnotationSourceActionBindings]), &recorded))
	require.Equal(t, []apiv1alpha2.ActionBinding{{Handler: "egress", Input: `{"egressPolicy":"deny-all"}`}}, recorded)
}

func TestCreateSandboxSnapshotOmitsProvenanceWithoutBindings(t *testing.T) {
	server, k8sClient, _ := newSnapshotServer(t)
	_, err := server.CreateSandboxSnapshot(context.Background(), snapshotCreateRequest("snap-a", "app-v2"))
	require.NoError(t, err)
	var persisted apiv1alpha2.SandboxSnapshot
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "snap-a"}, &persisted))
	require.NotContains(t, persisted.Annotations, assignment.AnnotationSourceActionBindings)
}

func TestCreateSandboxSnapshotReplaysIdempotently(t *testing.T) {
	server, k8sClient, _ := newSnapshotServer(t)
	_, err := server.CreateSandboxSnapshot(context.Background(), snapshotCreateRequest("snap-a", "app-v2"))
	require.NoError(t, err)
	response, err := server.CreateSandboxSnapshot(context.Background(), snapshotCreateRequest("snap-a", "app-v2"))
	require.NoError(t, err)
	require.Equal(t, "snap-a", response.Snapshot.Identity.GetName())
	var list apiv1alpha2.SandboxSnapshotList
	require.NoError(t, k8sClient.Client.List(context.Background(), &list, client.InNamespace("default")))
	require.Len(t, list.Items, 1, "replay must not persist a second object")
}

func TestCreateSandboxSnapshotOmittedNamespaceReplayMatchesExplicit(t *testing.T) {
	server, k8sClient, _ := newSnapshotServer(t)
	server.DefaultNamespace = "tenant-default"
	request := snapshotCreateRequest("snap-a", "app-v2")
	request.Sandbox.NamespacedName.Namespace = ""
	request.Sandbox.NamespacedName.Name = "sandbox-b" // lives in tenant-default
	request.Sandbox.ExpectedUid = "sandbox-uid-b"
	tenantSandbox := assignedReadySandbox(t, "sandbox-b", "sandbox-uid-b")
	tenantSandbox.Namespace = "tenant-default"
	require.NoError(t, k8sClient.Client.Create(context.Background(), tenantSandbox))

	_, err := server.CreateSandboxSnapshot(context.Background(), request)
	require.NoError(t, err)
	// The explicit-namespace replay must resolve to the same spec hash.
	request.Sandbox.NamespacedName.Namespace = "tenant-default"
	_, err = server.CreateSandboxSnapshot(context.Background(), request)
	require.NoError(t, err, "omitted-namespace replay must hash identically to the explicit form")
	var list apiv1alpha2.SandboxSnapshotList
	require.NoError(t, k8sClient.Client.List(context.Background(), &list, client.InNamespace("tenant-default")))
	require.Len(t, list.Items, 1)
}

func TestCreateSandboxSnapshotRejectsConflictingReplay(t *testing.T) {
	server, _, _ := newSnapshotServer(t)
	_, err := server.CreateSandboxSnapshot(context.Background(), snapshotCreateRequest("snap-a", "app-v2"))
	require.NoError(t, err)
	_, err = server.CreateSandboxSnapshot(context.Background(), snapshotCreateRequest("snap-a", "app-v3"))
	require.Equal(t, codes.AlreadyExists, status.Code(err))
}

func TestCreateSandboxSnapshotRejectsNotReadySandbox(t *testing.T) {
	server, _, fastlet := newSnapshotServer(t)
	// Both the lagging CR projection AND the live fastlet observation report
	// a non-Ready runtime: the request must be rejected before any CR write.
	fastlet.mu.Lock()
	fastlet.inspectStatus = testObservedStatus("sandbox-uid-a", "creating", 1, nil)
	fastlet.mu.Unlock()
	var sandbox apiv1alpha2.Sandbox
	require.NoError(t, server.K8sClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "sandbox-a"}, &sandbox))
	sandbox.Status.Runtime.State = apiv1alpha2.RuntimeCreating
	require.NoError(t, server.K8sClient.Status().Update(context.Background(), &sandbox))

	_, err := server.CreateSandboxSnapshot(context.Background(), snapshotCreateRequest("snap-a", "app-v2"))
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	var list apiv1alpha2.SandboxSnapshotList
	require.NoError(t, server.K8sClient.List(context.Background(), &list))
	require.Empty(t, list.Items, "rejected request must not persist an object")
}

func TestCreateSandboxSnapshotAcceptsReadyBehindLaggingProjection(t *testing.T) {
	server, _, _ := newSnapshotServer(t)
	// The fastlet reports the runtime Running (the default fixture) while
	// the CR projection still says Creating: the snapshot must proceed.
	var sandbox apiv1alpha2.Sandbox
	require.NoError(t, server.K8sClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "sandbox-a"}, &sandbox))
	sandbox.Status.Runtime.State = apiv1alpha2.RuntimeCreating
	require.NoError(t, server.K8sClient.Status().Update(context.Background(), &sandbox))

	response, err := server.CreateSandboxSnapshot(context.Background(), snapshotCreateRequest("snap-a", "app-v2"))
	require.NoError(t, err)
	require.Equal(t, "snap-a", response.Snapshot.Identity.GetName())
}

func TestCreateSandboxSnapshotRejectsNonTerminalPredecessor(t *testing.T) {
	server, _, _ := newSnapshotServer(t)
	_, err := server.CreateSandboxSnapshot(context.Background(), snapshotCreateRequest("snap-first", "app-v2"))
	require.NoError(t, err)

	_, err = server.CreateSandboxSnapshot(context.Background(), snapshotCreateRequest("snap-second", "app-v3"))
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "snap-first")

	// The template-name fence is cluster-wide: a non-terminal holder in
	// another namespace on a different Sandbox is rejected too.
	other := snapshotCreateRequest("snap-second", "app-v9")
	other.Sandbox.NamespacedName.Name = "sandbox-b"
	other.Sandbox.ExpectedUid = "sandbox-uid-b"
	var foreign apiv1alpha2.SandboxSnapshot
	foreign.Namespace = "other-ns"
	foreign.Name = "snap-foreign"
	foreign.Spec = apiv1alpha2.SandboxSnapshotSpec{
		SandboxRef:   apiv1alpha2.SandboxRef{Name: "sandbox-x", Namespace: "other-ns", UID: "uid-x"},
		TemplateName: "app-v9",
	}
	foreign.Status.Phase = apiv1alpha2.SandboxSnapshotPhaseCreating
	require.NoError(t, server.K8sClient.Create(context.Background(), &foreign))
	require.NoError(t, server.K8sClient.Status().Update(context.Background(), &foreign))

	_, err = server.CreateSandboxSnapshot(context.Background(), other)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "template name")
	require.Contains(t, err.Error(), "other-ns/snap-foreign")
}

func TestCreateSandboxSnapshotPublishingHolderReleasesSandboxFenceOnly(t *testing.T) {
	server, _, _ := newSnapshotServer(t)
	_, err := server.CreateSandboxSnapshot(context.Background(), snapshotCreateRequest("snap-first", "app-v2"))
	require.NoError(t, err)
	var holder apiv1alpha2.SandboxSnapshot
	require.NoError(t, server.K8sClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "snap-first"}, &holder))
	holder.Status.Phase = apiv1alpha2.SandboxSnapshotPhasePublishing
	require.NoError(t, server.K8sClient.Status().Update(context.Background(), &holder))

	// Same Sandbox, different template: the pause window is over, admitted.
	_, err = server.CreateSandboxSnapshot(context.Background(), snapshotCreateRequest("snap-second", "app-v9"))
	require.NoError(t, err)

	// Same template name (even though the sandbox fence is released): the
	// index key stays exclusive until the holder terminates.
	_, err = server.CreateSandboxSnapshot(context.Background(), snapshotCreateRequest("snap-third", "app-v2"))
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "template name")
}

func TestCreateSandboxSnapshotAllowsRequestAfterTerminalPredecessor(t *testing.T) {
	server, _, _ := newSnapshotServer(t)
	_, err := server.CreateSandboxSnapshot(context.Background(), snapshotCreateRequest("snap-first", "app-v2"))
	require.NoError(t, err)
	var first apiv1alpha2.SandboxSnapshot
	require.NoError(t, server.K8sClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "snap-first"}, &first))
	first.Status.Phase = apiv1alpha2.SandboxSnapshotPhaseFailed
	require.NoError(t, server.K8sClient.Status().Update(context.Background(), &first))

	_, err = server.CreateSandboxSnapshot(context.Background(), snapshotCreateRequest("snap-second", "app-v2"))
	require.NoError(t, err)
}

func TestCreateSandboxSnapshotDeterministicRejectionMarksFailed(t *testing.T) {
	server, k8sClient, fastlet := newSnapshotServer(t)
	fastlet.mu.Lock()
	fastlet.snapshotErr = &fastletapi.FastletError{Code: fastletapi.ErrorSnapshotInProgress, Message: "busy"}
	fastlet.mu.Unlock()

	_, err := server.CreateSandboxSnapshot(context.Background(), snapshotCreateRequest("snap-a", "app-v2"))
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	var persisted apiv1alpha2.SandboxSnapshot
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "snap-a"}, &persisted))
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseFailed, persisted.Status.Phase)
	require.NotNil(t, persisted.Status.CompletedAt)
}

func TestCreateSandboxSnapshotUnreachableFastletKeepsPending(t *testing.T) {
	server, k8sClient, fastlet := newSnapshotServer(t)
	fastlet.mu.Lock()
	fastlet.snapshotErr = errors.New("connection refused")
	fastlet.mu.Unlock()

	response, err := server.CreateSandboxSnapshot(context.Background(), snapshotCreateRequest("snap-a", "app-v2"))
	require.NoError(t, err, "intent is persisted; the Controller retries")
	require.Equal(t, fastpathv2.SnapshotPhase_SNAPSHOT_PHASE_PENDING, response.Snapshot.Phase)
	var persisted apiv1alpha2.SandboxSnapshot
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "snap-a"}, &persisted))
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhasePending, persisted.Status.Phase)
}

func TestCreateSandboxSnapshotUnassignedSandboxReportsPending(t *testing.T) {
	server, k8sClient, _ := newSnapshotServer(t)
	unassigned := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-free", Namespace: "default", UID: types.UID("sandbox-uid-free")},
		Spec:       apiv1alpha2.SandboxSpec{Image: "alpine:latest", PoolRef: "pool-a"},
		Status: apiv1alpha2.SandboxStatus{
			Runtime:   apiv1alpha2.RuntimeStatus{State: apiv1alpha2.RuntimeReady, Generation: 1},
			DataPlane: apiv1alpha2.DataPlaneStatus{State: apiv1alpha2.DataPlaneReady, RouteGeneration: 1},
		},
	}
	require.NoError(t, k8sClient.Client.Create(context.Background(), unassigned))
	require.NoError(t, k8sClient.Client.Status().Update(context.Background(), unassigned))

	request := snapshotCreateRequest("snap-free", "app-free")
	request.Sandbox.NamespacedName.Name = "sandbox-free"
	request.Sandbox.ExpectedUid = "sandbox-uid-free"
	response, err := server.CreateSandboxSnapshot(context.Background(), request)
	require.NoError(t, err, "the intent is persisted; the Controller converges once the Sandbox is placed")
	require.Equal(t, fastpathv2.SnapshotPhase_SNAPSHOT_PHASE_PENDING, response.Snapshot.Phase)

	var persisted apiv1alpha2.SandboxSnapshot
	require.NoError(t, k8sClient.Client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "snap-free"}, &persisted))
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhasePending, persisted.Status.Phase)
}

func TestGetAndDeleteSandboxSnapshot(t *testing.T) {
	server, _, _ := newSnapshotServer(t)
	created, err := server.CreateSandboxSnapshot(context.Background(), snapshotCreateRequest("snap-a", "app-v2"))
	require.NoError(t, err)

	fetched, err := server.GetSandboxSnapshot(context.Background(), &fastpathv2.GetSandboxSnapshotRequest{
		Snapshot: &fastpathv2.NamespacedName{Name: "snap-a", Namespace: "default"}, ExpectedUid: created.Snapshot.Identity.GetUid(),
	})
	require.NoError(t, err)
	require.Equal(t, fastpathv2.SnapshotPhase_SNAPSHOT_PHASE_CREATING, fetched.Snapshot.Phase)

	_, err = server.GetSandboxSnapshot(context.Background(), &fastpathv2.GetSandboxSnapshotRequest{
		Snapshot: &fastpathv2.NamespacedName{Name: "snap-a", Namespace: "default"}, ExpectedUid: "wrong-uid",
	})
	require.Equal(t, codes.Aborted, status.Code(err))

	_, err = server.DeleteSandboxSnapshot(context.Background(), &fastpathv2.DeleteSandboxSnapshotRequest{
		Snapshot: &fastpathv2.SandboxReference{
			NamespacedName: &fastpathv2.NamespacedName{Name: "snap-a", Namespace: "default"}, ExpectedUid: "wrong-uid",
		},
	})
	require.Equal(t, codes.Aborted, status.Code(err), "delete must honor expected_uid like get")
	var survivors apiv1alpha2.SandboxSnapshotList
	require.NoError(t, server.K8sClient.List(context.Background(), &survivors, client.InNamespace("default")))
	require.Len(t, survivors.Items, 1, "a UID-mismatched delete must not remove the object")

	_, err = server.DeleteSandboxSnapshot(context.Background(), &fastpathv2.DeleteSandboxSnapshotRequest{
		Snapshot: &fastpathv2.SandboxReference{NamespacedName: &fastpathv2.NamespacedName{Name: "snap-a", Namespace: "default"}},
	})
	require.NoError(t, err)
	var list apiv1alpha2.SandboxSnapshotList
	require.NoError(t, server.K8sClient.List(context.Background(), &list, client.InNamespace("default")))
	require.Empty(t, list.Items)

	_, err = server.DeleteSandboxSnapshot(context.Background(), &fastpathv2.DeleteSandboxSnapshotRequest{
		Snapshot: &fastpathv2.SandboxReference{NamespacedName: &fastpathv2.NamespacedName{Name: "snap-a", Namespace: "default"}},
	})
	require.NoError(t, err, "delete is idempotent")
}
