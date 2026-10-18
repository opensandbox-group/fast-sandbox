package scheduling

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/fastletsettings"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"
)

func TestResourceSlotCapacity(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("resource-slot-capacity").
		WithLabel("suite", "scheduling").
		WithLabel("tier", "smoke").
		Assess("maxSandboxesPerPod limit enforced correctly", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("slot")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := createSchedulingPool(namespace, "slot-pool", 1, 1, 2)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}

			sandbox1 := createSchedulingSandbox(namespace, "sb-slot-1", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox1); err != nil {
				t.Fatalf("create sandbox 1: %v", err)
			}
			waitForAssignedSandbox(ctx, t, fixture, namespace, "sb-slot-1")

			sandbox2 := createSchedulingSandbox(namespace, "sb-slot-2", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox2); err != nil {
				t.Fatalf("create sandbox 2: %v", err)
			}
			waitForAssignedSandbox(ctx, t, fixture, namespace, "sb-slot-2")

			sandbox3 := createSchedulingSandbox(namespace, "sb-slot-3", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox3); err != nil {
				t.Fatalf("create sandbox 3: %v", err)
			}

			// Depending on heartbeat timing, placement either filters the full
			// Fastlet immediately or Fastlet rejects a durable assignment. Both
			// paths must leave the third Sandbox Pending without a third runtime.
			pendingCtx, cancelPending := context.WithTimeout(ctx, 30*time.Second)
			defer cancelPending()
			if _, err := fixture.WaitForSandbox(pendingCtx, types.NamespacedName{Name: "sb-slot-3", Namespace: namespace}, func(sb *apiv1alpha2.Sandbox) bool {
				return sb.Status.Runtime.State == apiv1alpha2.RuntimePending
			}); err != nil {
				t.Fatalf("wait for capacity-blocked Sandbox to remain Pending: %v", err)
			}
			ensureCtx, cancelEnsure := context.WithTimeout(ctx, 30*time.Second)
			defer cancelEnsure()
			if err := ensureSandboxRemainsCapacityBlocked(ensureCtx, k8sClient, types.NamespacedName{Name: "sb-slot-3", Namespace: namespace}, 10*time.Second); err != nil {
				t.Fatalf("sandbox 3 should remain Pending without consuming a third runtime slot: %v", err)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func ensureSandboxRemainsCapacityBlocked(ctx context.Context, k8sClient client.Client, name types.NamespacedName, duration time.Duration) error {
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		var sandbox apiv1alpha2.Sandbox
		if err := k8sClient.Get(ctx, name, &sandbox); err != nil {
			return err
		}
		if sandbox.Status.Runtime.State == apiv1alpha2.RuntimeReady {
			return fmt.Errorf("runtime became Ready despite exhausted capacity")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return nil
}

func TestAutoScaling(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("auto-scaling").
		WithLabel("suite", "scheduling").
		WithLabel("tier", "smoke").
		Assess("pool scales from 1 to 2 pods on demand", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("autoscale")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := createSchedulingPool(namespace, "scale-pool", 1, 2, 1)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for initial fastlet pod: %v", err)
			}

			sandbox1 := createSchedulingSandbox(namespace, "sb-scale-1", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox1); err != nil {
				t.Fatalf("create sandbox 1: %v", err)
			}

			sandbox2 := createSchedulingSandbox(namespace, "sb-scale-2", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox2); err != nil {
				t.Fatalf("create sandbox 2: %v", err)
			}

			scaleCtx, cancelScale := context.WithTimeout(ctx, 120*time.Second)
			defer cancelScale()
			if _, err := fixture.WaitForReadyFastletPods(scaleCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 2); err != nil {
				t.Fatalf("wait for pool to scale to 2 pods: %v", err)
			}

			assigned1 := waitForAssignedSandbox(ctx, t, fixture, namespace, "sb-scale-1")
			assigned2 := waitForAssignedSandbox(ctx, t, fixture, namespace, "sb-scale-2")

			if assigned1.Status.Placement.FastletName == assigned2.Status.Placement.FastletName {
				t.Fatalf("both sandboxes on same pod %s, expected different pods", assigned1.Status.Placement.FastletName)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestResourceAwareOvercommitScaling(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("resource-aware-overcommit-scaling").
		WithLabel("suite", "scheduling").
		WithLabel("tier", "smoke").
		Assess("derived resource commitment scales out and stabilizes scale-down", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("resource-aware")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := createResourceAwareSchedulingPool(namespace, "resource-aware-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create resource-aware sandbox pool: %v", err)
			}
			initialCtx, cancelInitial := context.WithTimeout(ctx, 90*time.Second)
			defer cancelInitial()
			if _, err := fixture.WaitForReadyFastletPods(initialCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for initial resource-aware fastlet: %v", err)
			}
			assertResourceAwareFastletContract(ctx, t, k8sClient, namespace, pool.Name)

			for index := 1; index <= 3; index++ {
				sandbox := createSchedulingSandbox(namespace, fmt.Sprintf("sb-resource-%d", index), pool.Name)
				if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
					t.Fatalf("create resource-aware sandbox %d: %v", index, err)
				}
			}

			scaleCtx, cancelScale := context.WithTimeout(ctx, 120*time.Second)
			defer cancelScale()
			if _, err := fixture.WaitForReadyFastletPods(scaleCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 2); err != nil {
				t.Fatalf("wait for resource commitment scale-out: %v", err)
			}

			assignments := make(map[string]int)
			for index := 1; index <= 3; index++ {
				assigned := waitForAssignedSandbox(ctx, t, fixture, namespace, fmt.Sprintf("sb-resource-%d", index))
				assignments[assigned.Status.Placement.FastletName]++
			}
			if len(assignments) != 2 {
				t.Fatalf("resource-aware placement used %d Fastlets, want 2: %v", len(assignments), assignments)
			}
			for fastlet, count := range assignments {
				if count > 2 {
					t.Fatalf("Fastlet %s admitted %d Sandboxes above its resource commitment capacity 2", fastlet, count)
				}
			}

			for index := 1; index <= 3; index++ {
				sandbox := &apiv1alpha2.Sandbox{ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      fmt.Sprintf("sb-resource-%d", index),
				}}
				if err := k8sClient.Delete(ctx, sandbox); err != nil {
					t.Fatalf("delete resource-aware sandbox %d: %v", index, err)
				}
				deleteCtx, cancelDelete := context.WithTimeout(ctx, 60*time.Second)
				if err := fixture.WaitForSandboxDeleted(deleteCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}); err != nil {
					cancelDelete()
					t.Fatalf("wait for resource-aware sandbox %d deletion: %v", index, err)
				}
				cancelDelete()
			}

			downCtx, cancelDown := context.WithTimeout(ctx, 150*time.Second)
			defer cancelDown()
			if err := waitForFastletPodCount(downCtx, k8sClient, namespace, pool.Name, 1); err != nil {
				t.Fatalf("wait for stabilized resource-aware scale-down: %v", err)
			}
			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestResourceAwareMemoryPressureScaling(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("resource-aware-memory-pressure-scaling").
		WithLabel("suite", "scheduling").
		WithLabel("tier", "smoke").
		Assess("blocked heartbeat removes effective capacity and rejects new placement", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("resource-pressure")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := createResourceAwareSchedulingPool(namespace, "resource-pressure-pool")
			pool.Spec.Capacity.BufferMin = 1
			pool.Spec.Capacity.BufferMax = 1
			pool.Spec.FastletTemplate.Spec.Containers[0].Env = []corev1.EnvVar{
				{Name: "FAST_SANDBOX_MEMORY_OBSERVER_INTERVAL", Value: "250ms"},
				{Name: "FAST_SANDBOX_MEMORY_HIGH_WATERMARK", Value: "0.01"},
				{Name: "FAST_SANDBOX_MEMORY_LOW_WATERMARK", Value: "0.005"},
				{Name: "FAST_SANDBOX_MEMORY_HIGH_SAMPLES", Value: "1"},
				{Name: "FAST_SANDBOX_MEMORY_RECOVERY_SAMPLES", Value: "1"},
				{Name: "FAST_SANDBOX_MEMORY_PSI_AVG10_THRESHOLD", Value: "0"},
				{Name: "FAST_SANDBOX_NODE_MEMORY_LOW_WATERMARK", Value: "0"},
				{Name: "FAST_SANDBOX_NODE_MEMORY_RECOVERY_WATERMARK", Value: "0"},
			}
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create pressured resource-aware pool: %v", err)
			}

			scaleCtx, cancelScale := context.WithTimeout(ctx, 120*time.Second)
			defer cancelScale()
			if _, err := fixture.WaitForReadyFastletPods(scaleCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 2); err != nil {
				t.Fatalf("wait for pressure-driven replacement capacity: %v", err)
			}

			// Allow the second Fastlet's blocked admission state to cross the
			// heartbeat boundary before testing placement.
			select {
			case <-ctx.Done():
				t.Fatalf("wait for blocked Fastlet heartbeats: %v", ctx.Err())
			case <-time.After(8 * time.Second):
			}

			sandbox := createSchedulingSandbox(namespace, "sb-pressure-blocked", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create Sandbox under memory pressure: %v", err)
			}
			if err := fixture.EnsureSandboxRemainsUnassigned(ctx,
				types.NamespacedName{Name: sandbox.Name, Namespace: namespace}, 10*time.Second,
			); err != nil {
				t.Fatalf("Sandbox should remain unassigned while all Fastlets are memory-blocked: %v", err)
			}
			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func createResourceAwareSchedulingPool(namespace, name string) *apiv1alpha2.SandboxPool {
	pool := createSchedulingPool(namespace, name, 1, 2, 2)
	pool.Spec.SandboxResources = apiv1alpha2.SandboxResourceProfile{
		Requests: &apiv1alpha2.SandboxComputeResources{CPU: resource.MustParse("100m"), Memory: resource.MustParse("128Mi")},
		Limits:   &apiv1alpha2.SandboxComputeResources{CPU: resource.MustParse("500m"), Memory: resource.MustParse("192Mi")},
		PIDs:     64,
	}
	pool.Spec.FastletTemplate.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: "FAST_SANDBOX_MEMORY_HIGH_WATERMARK", Value: "0.99"},
		{Name: "FAST_SANDBOX_MEMORY_LOW_WATERMARK", Value: "0.95"},
		{Name: "FAST_SANDBOX_MEMORY_PSI_AVG10_THRESHOLD", Value: "0"},
		{Name: "FAST_SANDBOX_NODE_MEMORY_LOW_WATERMARK", Value: "0"},
		{Name: "FAST_SANDBOX_NODE_MEMORY_RECOVERY_WATERMARK", Value: "0"},
	}
	return pool
}

func assertResourceAwareFastletContract(ctx context.Context, t *testing.T, k8sClient client.Client, namespace, pool string) {
	t.Helper()
	pods := &corev1.PodList{}
	if err := k8sClient.List(ctx, pods,
		client.InNamespace(namespace),
		client.MatchingLabels{"fast-sandbox.io/pool": pool},
	); err != nil {
		t.Fatalf("list resource-aware Fastlets: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("initial resource-aware Fastlet count=%d, want 1", len(pods.Items))
	}

	fastlet := pods.Items[0].Spec.Containers[0]
	wantCPU := resource.MustParse("600m")
	wantMemory := resource.MustParse("384Mi")
	for name, resources := range map[string]corev1.ResourceList{
		"requests": fastlet.Resources.Requests,
		"limits":   fastlet.Resources.Limits,
	} {
		if got := resources[corev1.ResourceCPU]; got.Cmp(wantCPU) != 0 {
			t.Fatalf("Fastlet %s CPU=%s, want %s", name, got.String(), wantCPU.String())
		}
		if got := resources[corev1.ResourceMemory]; got.Cmp(wantMemory) != 0 {
			t.Fatalf("Fastlet %s memory=%s, want %s", name, got.String(), wantMemory.String())
		}
	}

	capacity, err := fastletsettings.DecodeCommitmentCapacity(pods.Items[0].Annotations[fastletsettings.CommitmentCapacityAnnotation])
	if err != nil {
		t.Fatalf("decode Fastlet commitment capacity: %v", err)
	}
	wantCapacityCPU := resource.MustParse("200m")
	wantCapacityMemory := resource.MustParse("256Mi")
	if capacity.CPU.Cmp(wantCapacityCPU) != 0 || capacity.Memory.Cmp(wantCapacityMemory) != 0 {
		t.Fatalf("Fastlet commitment capacity=(%s,%s), want (%s,%s)",
			capacity.CPU.String(), capacity.Memory.String(), wantCapacityCPU.String(), wantCapacityMemory.String())
	}
}

func waitForFastletPodCount(ctx context.Context, k8sClient client.Client, namespace, pool string, want int) error {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		pods := &corev1.PodList{}
		if err := k8sClient.List(ctx, pods,
			client.InNamespace(namespace),
			client.MatchingLabels{"fast-sandbox.io/pool": pool},
		); err == nil && len(pods.Items) == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func createSchedulingPool(namespace, name string, min, max, maxPerPod int32) *apiv1alpha2.SandboxPool {
	return &apiv1alpha2.SandboxPool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1alpha2.GroupVersion.String(),
			Kind:       "SandboxPool",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: apiv1alpha2.SandboxPoolSpec{
			Capacity: apiv1alpha2.PoolCapacity{
				PoolMin: min,
				PoolMax: max,
			},
			MaxSandboxesPerPod: maxPerPod,
			Runtime:            apiv1alpha2.RuntimeContainer,
			SandboxResources:   suiteenv.SmallSandboxResourceProfile(),
			FastletTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "fastlet",
						Image: suiteenv.FastletImage(),
					}},
				},
			},
		},
	}
}

func createSchedulingSandbox(namespace, name, pool string) *apiv1alpha2.Sandbox {
	return &apiv1alpha2.Sandbox{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1alpha2.GroupVersion.String(),
			Kind:       "Sandbox",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: apiv1alpha2.SandboxSpec{
			Image:   "docker.io/library/alpine:latest",
			Command: []string{"/bin/sleep", "3600"},
			PoolRef: pool,
		},
	}
}

func waitForAssignedSandbox(ctx context.Context, t *testing.T, fixture *fixtures.FixtureClient, namespace, name string) *apiv1alpha2.Sandbox {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	sandbox, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Name: name, Namespace: namespace}, func(sb *apiv1alpha2.Sandbox) bool {
		return sb.Status.Placement.FastletName != "" && sb.Status.Runtime.State == apiv1alpha2.RuntimeReady
	})
	if err != nil {
		t.Fatalf("wait for assigned sandbox %s/%s: %v", namespace, name, err)
	}
	return sandbox
}
