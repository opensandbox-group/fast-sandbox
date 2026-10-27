package fastpath

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	fastpathv2 "fast-sandbox/api/proto/v2"
	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/controlplane/assignment"
	orchestration "fast-sandbox/internal/controlplane/orchestrator"
	"fast-sandbox/internal/controlplane/placement"
	routeauth "fast-sandbox/internal/dataplane/auth"
	dataplane "fast-sandbox/internal/dataplane/contract"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

func TestResolveEndpointIssuesInstanceFencedCredential(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := routeauth.NewIssuer(privateKey, time.Minute, func() time.Time { return now })
	require.NoError(t, err)
	verifier, err := routeauth.NewVerifier(publicKey, func() time.Time { return now })
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	envelope := assignment.AssignmentEnvelope{
		Version: assignment.AssignmentEnvelopeVersion, FastletName: "fastlet-a", FastletPodUID: "pod-a",
		NodeName: "node-a", Attempt: 3, InstanceGeneration: 1, RouteGeneration: 5,
		RuntimeInstanceID: "runtime-a", RuntimeProfileHash: "runtime-hash",
		ResourceProfileHash: "resource-hash", InfraRevision: "infra-hash",
	}
	projected := envelope.StatusPlacement()
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "tenant-a", UID: types.UID("uid-a")},
		Spec:       apiv1alpha2.SandboxSpec{PoolRef: "pool-a"},
		Status: apiv1alpha2.SandboxStatus{Placement: projected,
			Runtime:   apiv1alpha2.RuntimeStatus{State: apiv1alpha2.RuntimeReady, Generation: 1},
			DataPlane: apiv1alpha2.DataPlaneStatus{State: apiv1alpha2.DataPlaneReady, RouteGeneration: 5}},
	}
	require.NoError(t, assignment.SetAssignmentAnnotation(sandbox, envelope))
	pool := &apiv1alpha2.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "tenant-a"}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox, pool).Build()
	candidate := testCandidate("fastlet-a", "pod-a", "10.0.0.1")
	candidate.ID = placement.NewFastletID("tenant-a", candidate.PodName)
	registry := &fastpathRegistry{fastlets: map[placement.FastletID]placement.FastletInfo{
		candidate.ID: candidate,
	}}
	fastlet := &fastpathFastlet{inspectStatus: testObservedStatus("uid-a", "running", sandbox.Generation, nil)}
	server := &Server{K8sClient: k8sClient, RouteCache: k8sClient, CredentialIssuer: issuer,
		Orchestrator:        &orchestration.Orchestrator{Client: k8sClient, Registry: registry, FastletClient: fastlet},
		SandboxProxyBaseURL: "https://proxy.example.test"}
	response, err := server.ResolveEndpoint(context.Background(), &fastpathv2.ResolveEndpointRequest{
		Sandbox: sandboxNameReference("tenant-a", "sandbox-a"),
		Target:  &fastpathv2.EndpointTarget{Target: &fastpathv2.EndpointTarget_Port{Port: 8080}},
	})
	require.NoError(t, err)
	require.Equal(t, "https://proxy.example.test/v1/sandboxes/uid-a/ports/8080", response.ProxyEndpoint)
	require.Equal(t, int64(5), response.RouteGeneration)
	token := response.RequiredHeaders[dataplane.HeaderRouteCredential]
	claims, err := verifier.VerifyExpected(token, routeauth.Claims{
		Namespace: "tenant-a", SandboxUID: "uid-a", TargetKind: routeauth.TargetKindPort,
		Protocol: "HTTP", TargetPort: 8080, FastletPodUID: "pod-a",
		AssignmentAttempt: 3, RouteGeneration: 5,
	})
	require.NoError(t, err)
	require.Equal(t, now.Add(time.Minute).Unix(), claims.ExpiresAt)

	_, err = verifier.VerifyExpected(token, routeauth.Claims{
		Namespace: "tenant-a", SandboxUID: "uid-a", TargetKind: routeauth.TargetKindPort,
		Protocol: "HTTP", TargetPort: 8080, FastletPodUID: "pod-a",
		AssignmentAttempt: 4, RouteGeneration: 6,
	})
	require.ErrorIs(t, err, routeauth.ErrClaimMismatch)
}

func TestResolveEndpointRequiresDataPlaneReady(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := routeauth.NewIssuer(privateKey, time.Minute, time.Now)
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default", UID: types.UID("uid-a")},
		Spec:       apiv1alpha2.SandboxSpec{PoolRef: "pool-a"},
		Status:     apiv1alpha2.SandboxStatus{Placement: apiv1alpha2.PlacementStatus{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1}},
	}
	pool := &apiv1alpha2.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"}}
	server := &Server{K8sClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox, pool).Build(), CredentialIssuer: issuer, SandboxProxyBaseURL: "http://proxy"}
	_, err = server.ResolveEndpoint(context.Background(), &fastpathv2.ResolveEndpointRequest{
		Sandbox: sandboxNameReference("default", "sandbox-a"),
		Target:  &fastpathv2.EndpointTarget{Target: &fastpathv2.EndpointTarget_Port{Port: 80}},
	})
	require.Equal(t, codes.Unavailable, status.Code(err))
}

func TestResolveEndpointRequiresAggregateReadyWhenActionIsPending(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	issuer, err := routeauth.NewIssuer(privateKey, time.Minute, time.Now)
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	envelope := assignment.AssignmentEnvelope{
		Version: assignment.AssignmentEnvelopeVersion, FastletName: "fastlet-a", FastletPodUID: "pod-a",
		Attempt: 1, InstanceGeneration: 1, RouteGeneration: 1, RuntimeInstanceID: "runtime-a",
		RuntimeProfileHash: "runtime-hash", ResourceProfileHash: "resource-hash", InfraRevision: "infra-hash",
	}
	sandbox := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default", UID: types.UID("uid-a"), Generation: 1},
		Spec:       apiv1alpha2.SandboxSpec{PoolRef: "pool-a", ActionBindings: []apiv1alpha2.ActionBinding{{Handler: "egress"}}},
		Status: apiv1alpha2.SandboxStatus{
			Placement: envelope.StatusPlacement(),
			Runtime:   apiv1alpha2.RuntimeStatus{State: apiv1alpha2.RuntimeReady, Generation: 1},
			DataPlane: apiv1alpha2.DataPlaneStatus{State: apiv1alpha2.DataPlaneReady, RouteGeneration: 1},
		},
	}
	require.NoError(t, assignment.SetAssignmentAnnotation(sandbox, envelope))
	pool := &apiv1alpha2.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox, pool).Build()
	registry := &fastpathRegistry{fastlets: map[placement.FastletID]placement.FastletInfo{
		"fastlet-a": testCandidate("fastlet-a", "pod-a", "10.0.0.1"),
	}}
	fastlet := &fastpathFastlet{inspectStatus: &fastletapi.SandboxStatus{
		SandboxID: "uid-a", Runtime: fastletapi.RuntimeObservation{State: fastletapi.RuntimeStateReady},
		DataPlane: fastletapi.DataPlaneObservation{State: fastletapi.DataPlaneStateReady}, AppliedGeneration: 0,
		ActionBindings: []fastletapi.ActionBindingStatus{{Handler: "egress", State: "Pending"}},
	}}
	server := &Server{
		K8sClient: k8sClient, RouteCache: k8sClient, CredentialIssuer: issuer, SandboxProxyBaseURL: "http://proxy",
		Orchestrator: &orchestration.Orchestrator{Client: k8sClient, Registry: registry, FastletClient: fastlet},
	}
	_, err = server.ResolveEndpoint(context.Background(), &fastpathv2.ResolveEndpointRequest{
		Sandbox: sandboxNameReference("default", "sandbox-a"),
		Target:  &fastpathv2.EndpointTarget{Target: &fastpathv2.EndpointTarget_Port{Port: 80}},
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func sandboxNameReference(namespace, name string) *fastpathv2.SandboxReference {
	return &fastpathv2.SandboxReference{NamespacedName: &fastpathv2.NamespacedName{Namespace: namespace, Name: name}}
}
