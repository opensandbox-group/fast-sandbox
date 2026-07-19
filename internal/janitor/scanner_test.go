package janitor

import (
	"context"
	"errors"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCleanupDecisionRequiresPodAndAssignmentFences(t *testing.T) {
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	resource := ResourceIdentity{
		Backend: BackendContainerd, ResourceID: "container-a", CreatedAt: now.Add(-time.Hour),
		FastletPodUID: "pod-a", FastletPodName: "fastlet-a", FastletPodNamespace: "default",
		SandboxUID: "sandbox-a-uid", SandboxName: "sandbox-a", SandboxNamespace: "default",
		InstanceGeneration: 1, AssignmentAttempt: 1,
	}
	assignment := apiv1alpha1.SandboxAssignment{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1}
	running := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default", UID: types.UID("sandbox-a-uid")},
		Status:     apiv1alpha1.SandboxStatus{Assignment: &assignment, InstanceGeneration: 1, AssignmentAttempt: 1, Phase: string(apiv1alpha1.PhaseRunning)},
	}
	exactPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "fastlet-a", Namespace: "default", UID: types.UID("pod-a")}}

	t.Run("exact Pod still exists", func(t *testing.T) {
		janitor := newAuthorityJanitor(t, now, []*corev1.Pod{exactPod}, []*apiv1alpha1.Sandbox{running})
		decision, err := janitor.cleanupDecision(context.Background(), resource)
		require.NoError(t, err)
		require.False(t, decision.Eligible)
		require.Equal(t, "FastletPodStillExists", decision.Reason)
	})

	t.Run("control plane has not observed loss", func(t *testing.T) {
		janitor := newAuthorityJanitor(t, now, nil, []*apiv1alpha1.Sandbox{running})
		decision, err := janitor.cleanupDecision(context.Background(), resource)
		require.NoError(t, err)
		require.False(t, decision.Eligible)
		require.Equal(t, "AssignmentStillAuthoritative", decision.Reason)
	})

	t.Run("Manual policy Sandbox is durably Lost", func(t *testing.T) {
		lost := running.DeepCopy()
		lost.Status.Phase = string(apiv1alpha1.PhaseLost)
		janitor := newAuthorityJanitor(t, now, nil, []*apiv1alpha1.Sandbox{lost})
		decision, err := janitor.cleanupDecision(context.Background(), resource)
		require.NoError(t, err)
		require.True(t, decision.Eligible)
		require.Equal(t, "SandboxMarkedLost", decision.Reason)
	})

	t.Run("AutoRecreate advanced the assignment fence", func(t *testing.T) {
		advanced := running.DeepCopy()
		advanced.Status.Assignment.Attempt = 2
		advanced.Status.AssignmentAttempt = 2
		advanced.Status.InstanceGeneration = 2
		janitor := newAuthorityJanitor(t, now, nil, []*apiv1alpha1.Sandbox{advanced})
		decision, err := janitor.cleanupDecision(context.Background(), resource)
		require.NoError(t, err)
		require.True(t, decision.Eligible)
		require.Equal(t, "ResourceFenceSuperseded", decision.Reason)
	})

	t.Run("Sandbox CRD disappeared", func(t *testing.T) {
		janitor := newAuthorityJanitor(t, now, nil, nil)
		decision, err := janitor.cleanupDecision(context.Background(), resource)
		require.NoError(t, err)
		require.True(t, decision.Eligible)
		require.Equal(t, "SandboxIdentityMissing", decision.Reason)
	})
}

func TestCleanupDecisionTreatsSameNameReplacementAsDifferentPod(t *testing.T) {
	now := time.Now()
	replacement := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "fastlet-a", Namespace: "default", UID: types.UID("pod-b")}}
	janitor := newAuthorityJanitor(t, now, []*corev1.Pod{replacement}, nil)
	decision, err := janitor.cleanupDecision(context.Background(), ResourceIdentity{
		Backend: BackendLinuxNetwork, ResourceID: "pod-a/slot-a", CreatedAt: now.Add(-time.Hour),
		FastletPodUID: "pod-a", FastletPodName: "fastlet-a", FastletPodNamespace: "default",
	})
	require.NoError(t, err)
	require.True(t, decision.Eligible)
	require.Equal(t, "UnboundResourceFromLostPod", decision.Reason)
}

func TestCleanupDecisionHonorsGraceAndFailsClosed(t *testing.T) {
	now := time.Now()
	resource := ResourceIdentity{Backend: BackendContainerd, ResourceID: "container-a", FastletPodUID: "pod-a", CreatedAt: now.Add(-time.Second)}
	janitor := newAuthorityJanitor(t, now, nil, nil)
	decision, err := janitor.cleanupDecision(context.Background(), resource)
	require.NoError(t, err)
	require.False(t, decision.Eligible)
	require.Equal(t, "OrphanGracePeriod", decision.Reason)

	resource.CreatedAt = now.Add(-time.Hour)
	janitor.kubeClient = nil
	decision, err = janitor.cleanupDecision(context.Background(), resource)
	require.Error(t, err)
	require.False(t, decision.Eligible)
}

func TestCleanupDecisionFailsClosedOnPodAPIError(t *testing.T) {
	now := time.Now()
	janitor := newAuthorityJanitor(t, now, nil, nil)
	janitor.kubeClient.(*fake.Clientset).PrependReactor("get", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	decision, err := janitor.cleanupDecision(context.Background(), ResourceIdentity{
		Backend: BackendContainerd, ResourceID: "container-a", FastletPodUID: "pod-a",
		FastletPodName: "fastlet-a", FastletPodNamespace: "default", CreatedAt: now.Add(-time.Hour),
	})
	require.Error(t, err)
	require.False(t, decision.Eligible)
}

func newAuthorityJanitor(t *testing.T, now time.Time, pods []*corev1.Pod, sandboxes []*apiv1alpha1.Sandbox) *Janitor {
	t.Helper()
	podObjects := make([]runtime.Object, 0, len(pods))
	for _, pod := range pods {
		podObjects = append(podObjects, pod)
	}
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	objects := make([]runtime.Object, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		objects = append(objects, sandbox)
	}
	k8sClient := ctrlfake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()
	return &Janitor{
		kubeClient: fake.NewSimpleClientset(podObjects...), K8sClient: k8sClient,
		OrphanTimeout: 30 * time.Second, Now: func() time.Time { return now },
	}
}
