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
		Assess("resource commitment scales before the slot limit", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
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
			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func createResourceAwareSchedulingPool(namespace, name string) *apiv1alpha2.SandboxPool {
	pool := createSchedulingPool(namespace, name, 1, 2, 4)
	pool.Spec.SandboxResources = apiv1alpha2.SandboxResourceProfile{
		Requests: &apiv1alpha2.SandboxComputeResources{CPU: resource.MustParse("100m"), Memory: resource.MustParse("128Mi")},
		Limits:   &apiv1alpha2.SandboxComputeResources{CPU: resource.MustParse("200m"), Memory: resource.MustParse("256Mi")},
		PIDs:     64,
	}
	pool.Spec.FastletTemplate.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
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
