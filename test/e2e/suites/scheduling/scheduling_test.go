package scheduling

import (
	"context"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestIdenticalPrivatePortsDoNotAffectScheduling(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("identical-private-ports-do-not-affect-scheduling").
		WithLabel("suite", "scheduling").
		WithLabel("tier", "smoke").
		Assess("same-port sandboxes are both admitted without host-port endpoints", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("portmutex")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := createSchedulingPool(namespace, "port-mutex-pool", 2, 2, 5)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 2); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}

			sandboxA := createSandboxWithPorts(namespace, "sb-port-a", pool.Name, []int32{8080})
			if _, err := fixture.CreateSandbox(ctx, namespace, sandboxA); err != nil {
				t.Fatalf("create sandbox A: %v", err)
			}

			assignedA := waitForAssignedSandbox(ctx, t, fixture, namespace, "sb-port-a")
			if assignedA.Status.AssignedFastlet == "" {
				t.Fatalf("sandbox A not assigned")
			}

			sandboxB := createSandboxWithPorts(namespace, "sb-port-b", pool.Name, []int32{8080})
			if _, err := fixture.CreateSandbox(ctx, namespace, sandboxB); err != nil {
				t.Fatalf("create sandbox B: %v", err)
			}

			assignedB := waitForAssignedSandbox(ctx, t, fixture, namespace, "sb-port-b")
			if assignedB.Status.AssignedFastlet == "" {
				t.Fatalf("sandbox B not assigned")
			}

			if len(assignedA.Status.Endpoints) != 0 || len(assignedB.Status.Endpoints) != 0 {
				t.Fatalf("deprecated host-port endpoints must stay empty: A=%v B=%v", assignedA.Status.Endpoints, assignedB.Status.Endpoints)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

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

			sandbox1 := createSandboxWithPorts(namespace, "sb-slot-1", pool.Name, nil)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox1); err != nil {
				t.Fatalf("create sandbox 1: %v", err)
			}
			waitForAssignedSandbox(ctx, t, fixture, namespace, "sb-slot-1")

			sandbox2 := createSandboxWithPorts(namespace, "sb-slot-2", pool.Name, nil)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox2); err != nil {
				t.Fatalf("create sandbox 2: %v", err)
			}
			waitForAssignedSandbox(ctx, t, fixture, namespace, "sb-slot-2")

			sandbox3 := createSandboxWithPorts(namespace, "sb-slot-3", pool.Name, nil)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox3); err != nil {
				t.Fatalf("create sandbox 3: %v", err)
			}

			// Verify sandbox 3 remains unassigned due to capacity limit
			ensureCtx, cancelEnsure := context.WithTimeout(ctx, 30*time.Second)
			defer cancelEnsure()
			if err := fixture.EnsureSandboxRemainsUnassigned(ensureCtx, types.NamespacedName{Name: "sb-slot-3", Namespace: namespace}, 10*time.Second); err != nil {
				t.Fatalf("sandbox 3 should remain unassigned due to capacity limit: %v", err)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
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

			sandbox1 := createSandboxWithPorts(namespace, "sb-scale-1", pool.Name, nil)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox1); err != nil {
				t.Fatalf("create sandbox 1: %v", err)
			}

			sandbox2 := createSandboxWithPorts(namespace, "sb-scale-2", pool.Name, nil)
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

			if assigned1.Status.AssignedFastlet == assigned2.Status.AssignedFastlet {
				t.Fatalf("both sandboxes on same pod %s, expected different pods", assigned1.Status.AssignedFastlet)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func createSchedulingPool(namespace, name string, min, max, maxPerPod int32) *apiv1alpha1.SandboxPool {
	return &apiv1alpha1.SandboxPool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1alpha1.GroupVersion.String(),
			Kind:       "SandboxPool",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: apiv1alpha1.SandboxPoolSpec{
			Capacity: apiv1alpha1.PoolCapacity{
				PoolMin: min,
				PoolMax: max,
			},
			MaxSandboxesPerPod: maxPerPod,
			Runtime:            apiv1alpha1.RuntimeContainer,
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

func createSandboxWithPorts(namespace, name, pool string, ports []int32) *apiv1alpha1.Sandbox {
	return &apiv1alpha1.Sandbox{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1alpha1.GroupVersion.String(),
			Kind:       "Sandbox",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: apiv1alpha1.SandboxSpec{
			Image:        "docker.io/library/alpine:latest",
			Command:      []string{"/bin/sleep", "3600"},
			PoolRef:      pool,
			ExposedPorts: ports,
		},
	}
}

func waitForAssignedSandbox(ctx context.Context, t *testing.T, fixture *fixtures.FixtureClient, namespace, name string) *apiv1alpha1.Sandbox {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	sandbox, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Name: name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
		return sb.Status.AssignedFastlet != "" &&
			(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
	})
	if err != nil {
		t.Fatalf("wait for assigned sandbox %s/%s: %v", namespace, name, err)
	}
	return sandbox
}
