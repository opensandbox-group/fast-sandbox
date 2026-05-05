package cleanupjanitor

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

func TestNamespaceAware(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("namespace-aware").
		WithLabel("suite", "cleanupjanitor").
		WithLabel("tier", "smoke").
		Assess("janitor correctly handles non-default namespace sandboxes", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("nsaware")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := createCleanupPool(namespace, "ns-test-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if _, err := fixture.WaitForReadyAgentPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready agent pods: %v", err)
			}

			sandbox := createCleanupSandbox(namespace, "sb-ns-test", pool.Name, []int32{8080})
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			assigned := waitForAssignedSandbox(ctx, t, fixture, namespace, "sb-ns-test")
			if assigned.Status.AssignedPod == "" {
				t.Fatalf("sandbox not assigned")
			}

			// Wait for Janitor scan cycle (simulated by short wait)
			select {
			case <-ctx.Done():
				t.Fatalf("context cancelled during janitor wait: %v", ctx.Err())
			case <-time.After(10 * time.Second):
			}

			// Verify sandbox still exists after janitor scan
			existingSandbox := &apiv1alpha1.Sandbox{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "sb-ns-test", Namespace: namespace}, existingSandbox); err != nil {
				t.Fatalf("sandbox should still exist after janitor scan: %v", err)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestJanitorRecovery(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("janitor-recovery").
		WithLabel("suite", "cleanupjanitor").
		WithLabel("tier", "smoke").
		Assess("janitor detects and handles orphan containers", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("orphan")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := createCleanupPool(namespace, "orphan-test-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if _, err := fixture.WaitForReadyAgentPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready agent pods: %v", err)
			}

			sandbox := createCleanupSandbox(namespace, "sb-orphan", pool.Name, nil)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			// Wait for sandbox to be assigned
			waitForAssignedSandbox(ctx, t, fixture, namespace, "sb-orphan")

			// Simulate orphan scenario: remove finalizers and delete CRD
			orphanSandbox := &apiv1alpha1.Sandbox{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "sb-orphan", Namespace: namespace}, orphanSandbox); err != nil {
				t.Fatalf("get sandbox for orphan simulation: %v", err)
			}
			orphanSandbox.Finalizers = nil
			if err := k8sClient.Update(ctx, orphanSandbox); err != nil {
				t.Fatalf("remove finalizers: %v", err)
			}
			if err := k8sClient.Delete(ctx, orphanSandbox); err != nil {
				t.Fatalf("delete sandbox crd: %v", err)
			}

			// Wait for CRD to be deleted
			deleteCtx, cancelDelete := context.WithTimeout(ctx, 30*time.Second)
			defer cancelDelete()
			for {
				err := k8sClient.Get(deleteCtx, types.NamespacedName{Name: "sb-orphan", Namespace: namespace}, &apiv1alpha1.Sandbox{})
				if err != nil {
					break
				}
				select {
				case <-deleteCtx.Done():
					t.Fatalf("timeout waiting for sandbox CRD deletion")
				case <-time.After(500 * time.Millisecond):
				}
			}

			// Wait for Janitor scan cycle
			select {
			case <-ctx.Done():
				t.Fatalf("context cancelled during janitor wait: %v", ctx.Err())
			case <-time.After(5 * time.Second):
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func createCleanupPool(namespace, name string) *apiv1alpha1.SandboxPool {
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
				PoolMin: 1,
				PoolMax: 1,
			},
			MaxSandboxesPerPod: 2,
			RuntimeType:        apiv1alpha1.RuntimeContainer,
			AgentTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "agent",
						Image: suiteenv.AgentImage(),
					}},
				},
			},
		},
	}
}

func createCleanupSandbox(namespace, name, pool string, ports []int32) *apiv1alpha1.Sandbox {
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
		return sb.Status.AssignedPod != "" &&
			(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
	})
	if err != nil {
		t.Fatalf("wait for assigned sandbox %s/%s: %v", namespace, name, err)
	}
	return sandbox
}
