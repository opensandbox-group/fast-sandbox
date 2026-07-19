package fastpath

import (
	"context"
	"crypto/ed25519"
	"testing"
	"time"

	fastpathv1 "fast-sandbox/api/proto/v1"
	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/routeauth"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	sandbox := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "tenant-a", UID: types.UID("uid-a")},
		Status: apiv1alpha1.SandboxStatus{
			RuntimeState: apiv1alpha1.ObservedStateReady, DataPlaneState: apiv1alpha1.ObservedStateReady, RouteGeneration: 5,
			Assignment: &apiv1alpha1.SandboxAssignment{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 3, NodeName: "node-a"},
		},
	}
	server := &Server{
		K8sClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build(), CredentialIssuer: issuer,
		SandboxProxyBaseURL: "https://proxy.example.test",
	}
	response, err := server.ResolveEndpoint(context.Background(), &fastpathv1.ResolveEndpointRequest{SandboxUid: "uid-a", TargetPort: 8080, Protocol: "http"})
	require.NoError(t, err)
	require.Equal(t, "https://proxy.example.test/v1/sandboxes/uid-a/ports/8080", response.ProxyEndpoint)
	require.Equal(t, int64(5), response.RouteGeneration)
	token := response.RequiredHeaders["Authorization"][len("Bearer "):]
	claims, err := verifier.VerifyExpected(token, routeauth.Claims{
		Namespace: "tenant-a", SandboxUID: "uid-a", TargetPort: 8080, FastletPodUID: "pod-a",
		AssignmentAttempt: 3, RouteGeneration: 5,
	})
	require.NoError(t, err)
	require.Equal(t, now.Add(time.Minute).Unix(), claims.ExpiresAt)

	_, err = verifier.VerifyExpected(token, routeauth.Claims{
		Namespace: "tenant-a", SandboxUID: "uid-a", TargetPort: 8080, FastletPodUID: "pod-a",
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
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	sandbox := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default", UID: types.UID("uid-a")},
		Status:     apiv1alpha1.SandboxStatus{Assignment: &apiv1alpha1.SandboxAssignment{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1, NodeName: "node-a"}},
	}
	server := &Server{K8sClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build(), CredentialIssuer: issuer, SandboxProxyBaseURL: "http://proxy"}
	_, err = server.ResolveEndpoint(context.Background(), &fastpathv1.ResolveEndpointRequest{SandboxUid: "uid-a", TargetPort: 80})
	require.Equal(t, codes.Unavailable, status.Code(err))
}
