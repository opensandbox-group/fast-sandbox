package sandboxproxy

import (
	"context"
	"testing"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestKubernetesResolverUsesAuthoritativeFallbackAndWarmsIndex(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	sandbox := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "tenant-a", UID: types.UID("uid-a")},
		Status: apiv1alpha1.SandboxStatus{
			DataPlaneState: apiv1alpha1.ObservedStateReady, RouteGeneration: 4,
			Assignment: &apiv1alpha1.SandboxAssignment{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 3, NodeName: "node-a"},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "fastlet-a", Namespace: "tenant-a", UID: types.UID("pod-a")},
		Status:     corev1.PodStatus{PodIP: "10.0.0.8"},
	}
	index := NewIndex()
	resolver := &KubernetesResolver{Index: index, Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox, pod).Build()}

	route, err := resolver.Resolve(context.Background(), "uid-a")
	require.NoError(t, err)
	require.Equal(t, "10.0.0.8", route.FastletPodIP)
	require.Equal(t, int64(4), route.RouteGeneration)

	route, err = index.Resolve("uid-a")
	require.NoError(t, err)
	require.Equal(t, "pod-a", route.FastletPodUID)
}
