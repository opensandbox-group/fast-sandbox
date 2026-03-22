package fixtures

import (
	"context"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCreateSandboxPoolSetsNamespace(t *testing.T) {
	fixture := newFixtureHarness(t)
	pool := &apiv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a"},
	}

	created, err := fixture.CreateSandboxPool(context.Background(), "tenant-a", pool)
	if err != nil {
		t.Fatalf("expected create to succeed, got error: %v", err)
	}
	if created.Namespace != "tenant-a" {
		t.Fatalf("expected namespace to be set, got %q", created.Namespace)
	}

	stored := &apiv1alpha1.SandboxPool{}
	if err := fixture.client.Get(context.Background(), types.NamespacedName{Name: "pool-a", Namespace: "tenant-a"}, stored); err != nil {
		t.Fatalf("expected created pool to be persisted, got error: %v", err)
	}
}

func TestCreateSandboxSetsNamespace(t *testing.T) {
	fixture := newFixtureHarness(t)
	sb := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-a"},
		Spec: apiv1alpha1.SandboxSpec{
			Image:   "busybox:latest",
			PoolRef: "pool-a",
		},
	}

	created, err := fixture.CreateSandbox(context.Background(), "tenant-a", sb)
	if err != nil {
		t.Fatalf("expected create to succeed, got error: %v", err)
	}
	if created.Namespace != "tenant-a" {
		t.Fatalf("expected namespace to be set, got %q", created.Namespace)
	}
}

func TestWaitForSandboxPhaseReturnsUpdatedSandbox(t *testing.T) {
	fixture := newFixtureHarness(t)
	sb := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-running", Namespace: "tenant-a"},
		Spec: apiv1alpha1.SandboxSpec{
			Image:   "busybox:latest",
			PoolRef: "pool-a",
		},
		Status: apiv1alpha1.SandboxStatus{
			Phase: string(apiv1alpha1.PhasePending),
		},
	}
	if err := fixture.client.Create(context.Background(), sb); err != nil {
		t.Fatalf("expected sandbox seed create to succeed, got error: %v", err)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		current := &apiv1alpha1.Sandbox{}
		if err := fixture.client.Get(context.Background(), types.NamespacedName{Name: "sb-running", Namespace: "tenant-a"}, current); err != nil {
			return
		}
		current.Status.Phase = string(apiv1alpha1.PhaseRunning)
		_ = fixture.client.Update(context.Background(), current)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	got, err := fixture.WaitForSandboxPhase(ctx, types.NamespacedName{Name: "sb-running", Namespace: "tenant-a"}, apiv1alpha1.PhaseRunning)
	if err != nil {
		t.Fatalf("expected wait to succeed, got error: %v", err)
	}
	if got.Status.Phase != string(apiv1alpha1.PhaseRunning) {
		t.Fatalf("expected running phase, got %q", got.Status.Phase)
	}
}

func TestWaitForSandboxUsesPredicate(t *testing.T) {
	fixture := newFixtureHarness(t)
	sb := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-predicate", Namespace: "tenant-a"},
		Spec: apiv1alpha1.SandboxSpec{
			Image:   "busybox:latest",
			PoolRef: "pool-a",
		},
		Status: apiv1alpha1.SandboxStatus{
			Phase: string(apiv1alpha1.PhasePending),
		},
	}
	if err := fixture.client.Create(context.Background(), sb); err != nil {
		t.Fatalf("expected sandbox seed create to succeed, got error: %v", err)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		current := &apiv1alpha1.Sandbox{}
		if err := fixture.client.Get(context.Background(), types.NamespacedName{Name: "sb-predicate", Namespace: "tenant-a"}, current); err != nil {
			return
		}
		current.Status.Phase = string(apiv1alpha1.PhaseRunning)
		current.Status.AssignedPod = "agent-a"
		_ = fixture.client.Update(context.Background(), current)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	got, err := fixture.WaitForSandbox(ctx, types.NamespacedName{Name: "sb-predicate", Namespace: "tenant-a"}, func(sb *apiv1alpha1.Sandbox) bool {
		return sb.Status.AssignedPod != "" && sb.Status.Phase == string(apiv1alpha1.PhaseRunning)
	})
	if err != nil {
		t.Fatalf("expected predicate wait to succeed, got error: %v", err)
	}
	if got.Status.AssignedPod != "agent-a" {
		t.Fatalf("expected assigned pod to be observed, got %q", got.Status.AssignedPod)
	}
}

func TestEnsureSandboxRemainsUnassignedFailsOnAssignment(t *testing.T) {
	fixture := newFixtureHarness(t)
	sb := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sb-assigned", Namespace: "tenant-a"},
		Spec: apiv1alpha1.SandboxSpec{
			Image:   "busybox:latest",
			PoolRef: "pool-a",
		},
	}
	if err := fixture.client.Create(context.Background(), sb); err != nil {
		t.Fatalf("expected sandbox seed create to succeed, got error: %v", err)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		current := &apiv1alpha1.Sandbox{}
		if err := fixture.client.Get(context.Background(), types.NamespacedName{Name: "sb-assigned", Namespace: "tenant-a"}, current); err != nil {
			return
		}
		current.Status.AssignedPod = "agent-a"
		current.Status.SandboxID = "sandbox-a"
		_ = fixture.client.Update(context.Background(), current)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if err := fixture.EnsureSandboxRemainsUnassigned(ctx, types.NamespacedName{Name: "sb-assigned", Namespace: "tenant-a"}, 200*time.Millisecond); err == nil {
		t.Fatal("expected unassigned check to fail once sandbox is assigned")
	}
}

func TestWaitForReadyAgentPodsReturnsReadyPods(t *testing.T) {
	fixture := newFixtureHarness(t)
	pool := &apiv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-ready", Namespace: "tenant-a"},
		Status: apiv1alpha1.SandboxPoolStatus{
			ReadyPods: 0,
		},
	}
	if err := fixture.client.Create(context.Background(), pool); err != nil {
		t.Fatalf("expected pool seed create to succeed, got error: %v", err)
	}

	go func() {
		time.Sleep(20 * time.Millisecond)
		current := &apiv1alpha1.SandboxPool{}
		if err := fixture.client.Get(context.Background(), types.NamespacedName{Name: "pool-ready", Namespace: "tenant-a"}, current); err != nil {
			return
		}
		current.Status.ReadyPods = 2
		_ = fixture.client.Update(context.Background(), current)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	got, err := fixture.WaitForReadyAgentPods(ctx, types.NamespacedName{Name: "pool-ready", Namespace: "tenant-a"}, 1)
	if err != nil {
		t.Fatalf("expected ready pod wait to succeed, got error: %v", err)
	}
	if got.Status.ReadyPods != 2 {
		t.Fatalf("expected ready pods to be observed, got %d", got.Status.ReadyPods)
	}
}

func TestWaitForReadyAgentPodsFallsBackToReadyAgentPods(t *testing.T) {
	fixture := newFixtureHarness(t)
	pool := &apiv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-fallback", Namespace: "tenant-a"},
		Status: apiv1alpha1.SandboxPoolStatus{
			ReadyPods: 0,
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pool-fallback-agent",
			Namespace: "tenant-a",
			Labels: map[string]string{
				"fast-sandbox.io/pool": "pool-fallback",
			},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
	if err := fixture.client.Create(context.Background(), pool); err != nil {
		t.Fatalf("expected pool seed create to succeed, got error: %v", err)
	}
	if err := fixture.client.Create(context.Background(), pod); err != nil {
		t.Fatalf("expected pod seed create to succeed, got error: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	got, err := fixture.WaitForReadyAgentPods(ctx, types.NamespacedName{Name: "pool-fallback", Namespace: "tenant-a"}, 1)
	if err != nil {
		t.Fatalf("expected ready pod wait to succeed via pod fallback, got error: %v", err)
	}
	if got.Name != "pool-fallback" {
		t.Fatalf("expected fallback wait to return the pool object, got %q", got.Name)
	}
}

func newFixtureHarness(t *testing.T) *FixtureClient {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add client-go scheme: %v", err)
	}
	if err := apiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add fast-sandbox scheme: %v", err)
	}

	return New(
		fake.NewClientBuilder().WithScheme(scheme).Build(),
		WithPollInterval(10*time.Millisecond),
	)
}
