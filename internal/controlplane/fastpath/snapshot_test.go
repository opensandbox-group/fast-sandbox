package fastpath

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fastpathv2 "fast-sandbox/api/proto/v2"
	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	orchestration "fast-sandbox/internal/controlplane/orchestrator"
	"fast-sandbox/internal/controlplane/placement"
	"fast-sandbox/internal/ocitemplate"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
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
	require.Contains(t, persisted.Finalizers, assignment.SnapshotCleanupFinalizer, "protect artifacts before the direct Fastlet trigger")
	require.Equal(t, "pool-a", persisted.Annotations[assignment.AnnotationSnapshotPoolRef])
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

func TestCreateOCISnapshotWithoutTemplateName(t *testing.T) {
	server, k8sClient, _ := newSnapshotServer(t)
	for _, name := range []string{"sandbox-a", "sandbox-b"} {
		var sandbox apiv1alpha2.Sandbox
		require.NoError(t, k8sClient.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: name}, &sandbox))
		sandbox.Spec.Image = "oci://r.example/ns/template:base"
		require.NoError(t, k8sClient.Update(t.Context(), &sandbox))
		request := snapshotCreateRequest("snapshot-"+name, "")
		request.Sandbox.NamespacedName.Name = name
		request.Sandbox.ExpectedUid = string(sandbox.UID)
		response, err := server.CreateSandboxSnapshot(t.Context(), request)
		require.NoError(t, err, "independent unnamed OCI snapshots must not share a template-name fence")
		require.NotEmpty(t, response.Snapshot.SnapshotId)
		require.Empty(t, response.Snapshot.TemplateName)
	}
}

func TestNativeSnapshotStillRequiresTemplateName(t *testing.T) {
	server, _, _ := newSnapshotServer(t)
	_, err := server.CreateSandboxSnapshot(t.Context(), snapshotCreateRequest("snapshot-a", ""))
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "template_name is required for a native snapshot")
}

func snapshotModeRequest(requestID, templateName string, mode fastpathv2.SnapshotMode) *fastpathv2.CreateSandboxSnapshotRequest {
	request := snapshotCreateRequest(requestID, templateName)
	request.Mode = mode
	return request
}

// setSandboxImage repoints one fixture Sandbox at a different image.
func setSandboxImage(t *testing.T, server *Server, name, image string) {
	t.Helper()
	var sandbox apiv1alpha2.Sandbox
	require.NoError(t, server.K8sClient.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: name}, &sandbox))
	sandbox.Spec.Image = image
	require.NoError(t, server.K8sClient.Update(t.Context(), &sandbox))
}

// grantRootfsOnlyCapability marks the assigned fastlet as rootfs-only
// capable with an observed heartbeat, mirroring a heartbeat that carries
// the capability.
func grantRootfsOnlyCapability(t *testing.T, server *Server) {
	t.Helper()
	registry, ok := server.Orchestrator.Registry.(*fastpathRegistry)
	require.True(t, ok)
	registry.mu.Lock()
	defer registry.mu.Unlock()
	id := placement.NewFastletID("default", "fastlet-a")
	candidate := registry.fastlets[id]
	candidate.Capabilities = []string{fastletapi.CapabilityRootfsOnlySnapshots}
	candidate.LastHeartbeat = time.Now()
	registry.fastlets[id] = candidate
}

func TestCreateSandboxSnapshotRejectsUnknownMode(t *testing.T) {
	server, _, _ := newSnapshotServer(t)
	_, err := server.CreateSandboxSnapshot(t.Context(), snapshotModeRequest("snapshot-a", "app-v2", fastpathv2.SnapshotMode(42)))
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "unknown snapshot mode")
}

func TestCreateSandboxSnapshotRejectsRootfsOnlyForNativeImage(t *testing.T) {
	server, k8sClient, fastlet := newSnapshotServer(t)
	// sandbox-a runs a native (non-OCI) image: the mode must be rejected
	// before the intent is persisted or any fastlet is contacted.
	_, err := server.CreateSandboxSnapshot(t.Context(), snapshotModeRequest("snapshot-a", "app-v2", fastpathv2.SnapshotMode_SNAPSHOT_MODE_ROOTFS_ONLY))
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "ROOT_FS_ONLY requires a Sandbox on the OCI artifact path")
	var list apiv1alpha2.SandboxSnapshotList
	require.NoError(t, k8sClient.Client.List(t.Context(), &list))
	require.Empty(t, list.Items, "the rejected intent must not be persisted")
	fastlet.mu.Lock()
	defer fastlet.mu.Unlock()
	require.Empty(t, fastlet.snapshotSpecs, "no sandbox side effects")
}

func TestCreateSandboxSnapshotRootfsOnlyForOCI(t *testing.T) {
	server, k8sClient, fastlet := newSnapshotServer(t)
	setSandboxImage(t, server, "sandbox-a", ocitemplate.Prefix+"r.example/ns/template:base")
	grantRootfsOnlyCapability(t, server)

	request := snapshotModeRequest("snap-rootfs", "", fastpathv2.SnapshotMode_SNAPSHOT_MODE_ROOTFS_ONLY)
	response, err := server.CreateSandboxSnapshot(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, fastpathv2.SnapshotMode_SNAPSHOT_MODE_ROOTFS_ONLY, response.Snapshot.Mode)

	fastlet.mu.Lock()
	dispatched := append([]fastletapi.SnapshotSpec(nil), fastlet.snapshotSpecs...)
	fastlet.mu.Unlock()
	require.Len(t, dispatched, 1)
	require.Equal(t, fastletapi.SnapshotModeRootfsOnly, dispatched[0].Mode)

	var persisted apiv1alpha2.SandboxSnapshot
	require.NoError(t, k8sClient.Client.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "snap-rootfs"}, &persisted))
	require.Equal(t, apiv1alpha2.SandboxSnapshotModeRootfsOnly, persisted.Spec.Mode)
	require.Equal(t, apiv1alpha2.SandboxSnapshotModeRootfsOnly, persisted.Status.Mode)

	// The identical replay is idempotent and reports the produced mode.
	response, err = server.CreateSandboxSnapshot(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, fastpathv2.SnapshotMode_SNAPSHOT_MODE_ROOTFS_ONLY, response.Snapshot.Mode)
}

// markFastletObserved stamps the registry candidate with an observed
// heartbeat whose capability list mirrors the argument.
func markFastletObserved(t *testing.T, server *Server, capabilities []string) {
	t.Helper()
	registry, ok := server.Orchestrator.Registry.(*fastpathRegistry)
	require.True(t, ok)
	registry.mu.Lock()
	defer registry.mu.Unlock()
	id := placement.NewFastletID("default", "fastlet-a")
	candidate := registry.fastlets[id]
	candidate.Capabilities = capabilities
	candidate.LastHeartbeat = time.Now()
	registry.fastlets[id] = candidate
}

func TestCreateSandboxSnapshotRootfsOnlyRequiresFastletCapability(t *testing.T) {
	// Version skew, confirmed: the assigned fastlet's heartbeat observed and
	// advertises no rootfs-only capability (an old build). The intent is
	// persisted, the dispatch is gated, and the one-shot object terminates
	// Failed — the fastlet can never silently produce a Full artifact from a
	// RootfsOnly request.
	server, k8sClient, fastlet := newSnapshotServer(t)
	setSandboxImage(t, server, "sandbox-a", ocitemplate.Prefix+"r.example/ns/template:base")
	markFastletObserved(t, server, nil)

	_, err := server.CreateSandboxSnapshot(t.Context(), snapshotModeRequest("snap-rootfs", "", fastpathv2.SnapshotMode_SNAPSHOT_MODE_ROOTFS_ONLY))
	require.Equal(t, codes.Unimplemented, status.Code(err))
	require.ErrorContains(t, err, "does not advertise the rootfs-only snapshot capability")

	fastlet.mu.Lock()
	defer fastlet.mu.Unlock()
	require.Empty(t, fastlet.snapshotSpecs, "the gated dispatch must never reach the fastlet")
	var persisted apiv1alpha2.SandboxSnapshot
	require.NoError(t, k8sClient.Client.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "snap-rootfs"}, &persisted))
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseFailed, persisted.Status.Phase)
	require.Equal(t, apiv1alpha2.SandboxSnapshotModeRootfsOnly, persisted.Spec.Mode)
}

func TestCreateSandboxSnapshotUnobservedCapabilityStaysPendingUntilHeartbeat(t *testing.T) {
	// After a control-plane restart the registry entry can exist without any
	// heartbeat: the capability is unobserved, not absent. The one-shot
	// intent must stay Pending (retryable), and the same request_id must
	// dispatch once the capable heartbeat lands.
	server, k8sClient, fastlet := newSnapshotServer(t)
	setSandboxImage(t, server, "sandbox-a", ocitemplate.Prefix+"r.example/ns/template:base")

	request := snapshotModeRequest("snap-rootfs", "", fastpathv2.SnapshotMode_SNAPSHOT_MODE_ROOTFS_ONLY)
	response, err := server.CreateSandboxSnapshot(t.Context(), request)
	require.NoError(t, err, "an unobserved capability must not fail the one-shot intent")
	require.Equal(t, fastpathv2.SnapshotPhase_SNAPSHOT_PHASE_PENDING, response.Snapshot.Phase)
	fastlet.mu.Lock()
	specs := len(fastlet.snapshotSpecs)
	fastlet.mu.Unlock()
	require.Zero(t, specs, "dispatch still fails closed until support is confirmed")

	// The capable heartbeat lands; the replay of the SAME request dispatches.
	grantRootfsOnlyCapability(t, server)
	response, err = server.CreateSandboxSnapshot(t.Context(), request)
	require.NoError(t, err, "the pending intent must recover once the capability is observed")
	require.Equal(t, fastpathv2.SnapshotPhase_SNAPSHOT_PHASE_CREATING, response.Snapshot.Phase)
	fastlet.mu.Lock()
	defer fastlet.mu.Unlock()
	require.Len(t, fastlet.snapshotSpecs, 1)
	require.Equal(t, fastletapi.SnapshotModeRootfsOnly, fastlet.snapshotSpecs[0].Mode)
	var persisted apiv1alpha2.SandboxSnapshot
	require.NoError(t, k8sClient.Client.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "snap-rootfs"}, &persisted))
	require.Equal(t, apiv1alpha2.SandboxSnapshotPhaseCreating, persisted.Status.Phase)
}

func TestSnapshotSpecHashNormalizesFullMode(t *testing.T) {
	legacy := snapshotCreateRequest("snap-a", "app-v2")
	explicitFull := snapshotModeRequest("snap-a", "app-v2", fastpathv2.SnapshotMode_SNAPSHOT_MODE_FULL)
	rootfsOnly := snapshotModeRequest("snap-a", "app-v2", fastpathv2.SnapshotMode_SNAPSHOT_MODE_ROOTFS_ONLY)

	legacyHash, err := SnapshotSpecHash(legacy)
	require.NoError(t, err)
	explicitHash, err := SnapshotSpecHash(explicitFull)
	require.NoError(t, err)
	rootfsHash, err := SnapshotSpecHash(rootfsOnly)
	require.NoError(t, err)
	require.Equal(t, legacyHash, explicitHash, "explicit FULL must hash identically to an absent mode (legacy replay compatibility)")
	require.NotEqual(t, legacyHash, rootfsHash, "ROOT_FS_ONLY is a distinct intent")
}

func TestCreateSandboxSnapshotModeChangingReplayRejected(t *testing.T) {
	server, k8sClient, _ := newSnapshotServer(t)
	setSandboxImage(t, server, "sandbox-a", ocitemplate.Prefix+"r.example/ns/template:base")
	grantRootfsOnlyCapability(t, server)

	// A legacy (absent-mode) request accepted first.
	_, err := server.CreateSandboxSnapshot(t.Context(), snapshotCreateRequest("snap-a", ""))
	require.NoError(t, err)
	// An explicit FULL replay keeps the historical hash: still idempotent.
	_, err = server.CreateSandboxSnapshot(t.Context(), snapshotModeRequest("snap-a", "", fastpathv2.SnapshotMode_SNAPSHOT_MODE_FULL))
	require.NoError(t, err, "explicit FULL must replay a legacy Full intent")
	// Changing the effective mode on the same request_id is spec drift.
	_, err = server.CreateSandboxSnapshot(t.Context(), snapshotModeRequest("snap-a", "", fastpathv2.SnapshotMode_SNAPSHOT_MODE_ROOTFS_ONLY))
	require.Equal(t, codes.AlreadyExists, status.Code(err))
	require.ErrorContains(t, err, "belongs to another snapshot intent")

	// The drift fence also holds in the other direction.
	setSandboxImage(t, server, "sandbox-b", ocitemplate.Prefix+"r.example/ns/template:base")
	request := snapshotModeRequest("snap-b", "", fastpathv2.SnapshotMode_SNAPSHOT_MODE_ROOTFS_ONLY)
	request.Sandbox.NamespacedName.Name = "sandbox-b"
	request.Sandbox.ExpectedUid = "sandbox-uid-b"
	_, err = server.CreateSandboxSnapshot(t.Context(), request)
	require.NoError(t, err)
	request.Mode = fastpathv2.SnapshotMode_SNAPSHOT_MODE_FULL
	_, err = server.CreateSandboxSnapshot(t.Context(), request)
	require.Equal(t, codes.AlreadyExists, status.Code(err))

	var list apiv1alpha2.SandboxSnapshotList
	require.NoError(t, k8sClient.Client.List(t.Context(), &list))
	require.Len(t, list.Items, 2, "replays must not persist additional objects")
}

func TestSnapshotInfoReportsLegacyFullMode(t *testing.T) {
	// A pre-mode SandboxSnapshot (no status.mode) reports FULL, never
	// UNSPECIFIED: clients branch on the artifact class.
	info := snapshotInfo(&apiv1alpha2.SandboxSnapshot{})
	require.Equal(t, fastpathv2.SnapshotMode_SNAPSHOT_MODE_FULL, info.Mode)
	info = snapshotInfo(&apiv1alpha2.SandboxSnapshot{Status: apiv1alpha2.SandboxSnapshotStatus{Mode: apiv1alpha2.SandboxSnapshotModeRootfsOnly}})
	require.Equal(t, fastpathv2.SnapshotMode_SNAPSHOT_MODE_ROOTFS_ONLY, info.Mode)
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
	require.Len(t, list.Items, 1)
	require.NotNil(t, list.Items[0].DeletionTimestamp, "finalizer must wait for cleanup")
	_, err = server.CreateSandboxSnapshot(t.Context(), snapshotCreateRequest("snap-a", "app-v2"))
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "a deleting snapshot cannot restart publication")

	_, err = server.DeleteSandboxSnapshot(context.Background(), &fastpathv2.DeleteSandboxSnapshotRequest{
		Snapshot: &fastpathv2.SandboxReference{NamespacedName: &fastpathv2.NamespacedName{Name: "snap-a", Namespace: "default"}},
	})
	require.NoError(t, err, "delete is idempotent")
}
