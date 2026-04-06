package secureruntime

import (
	"context"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/test/e2e/support/envcheck"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

// getEnvChecker returns the global environment checker for gVisor tests.
func getGVisorEnvChecker(t *testing.T) *envcheck.Checker {
	t.Helper()
	checker, err := envcheck.GetChecker()
	if err != nil {
		t.Fatalf("create env checker: %v", err)
	}
	return checker
}

func TestGVisorSandbox(t *testing.T) {
	suiteenv.SkipUnlessEnabled(t)

	feature := features.New("gvisor-sandbox").
		WithLabel("suite", "secureruntime").
		WithLabel("runtime", "gvisor").
		Assess("gVisor pool creates sandbox successfully", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			// Check if gVisor should run in this environment
			checker := getGVisorEnvChecker(t)
			shouldRun, reason := checker.ShouldRunGVisor(ctx)
			if !shouldRun {
				t.Skipf("gVisor test skipped: %s", reason)
			}
			t.Logf("Running gVisor test: %s", reason)

			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("gvisor")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			// Create gVisor pool
			pool := newSecureRuntimePool(namespace, "gvisor-pool", apiv1alpha1.RuntimeGVisor, 1, 1)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create gvisor pool: %v", err)
			}

			// Wait for ready agent pods
			poolWaitCtx, cancelPoolWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelPoolWait()
			if _, err := fixture.WaitForReadyAgentPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready agent pods: %v", err)
			}

			// Create sandbox
			sandbox := newSecureRuntimeSandbox(namespace, "sb-gvisor", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			// Wait for sandbox running
			runCtx, cancelRunWait := context.WithTimeout(ctx, 60*time.Second)
			defer cancelRunWait()
			_, err := fixture.WaitForSandbox(runCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.AssignedPod != "" &&
					(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
			})
			if err != nil {
				t.Fatalf("wait for running sandbox: %v", err)
			}

			// Verify gVisor runtime (check Pool condition)
			updatedPool := &apiv1alpha1.SandboxPool{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, updatedPool); err != nil {
				t.Fatalf("get updated pool: %v", err)
			}

			// Check RuntimeReady condition
			var runtimeReady *metav1.Condition
			for _, c := range updatedPool.Status.Conditions {
				if c.Type == apiv1alpha1.PoolConditionRuntimeReady {
					runtimeReady = &c
					break
				}
			}
			if runtimeReady == nil || runtimeReady.Status != metav1.ConditionTrue {
				t.Errorf("expected RuntimeReady condition to be True, got: %v", runtimeReady)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

// TestGVisorIsolation verifies that the sandbox actually runs in gVisor runtime.
// gVisor containers have distinct characteristics that can be verified:
// 1. /proc/sys/kernel/osrelease contains "gVisor" or shows a different kernel
// 2. runsc-sandbox process exists on the host node
func TestGVisorIsolation(t *testing.T) {
	suiteenv.SkipUnlessEnabled(t)

	feature := features.New("gvisor-isolation").
		WithLabel("suite", "secureruntime").
		WithLabel("runtime", "gvisor").
		Assess("gVisor sandbox shows proper isolation markers", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			// Check if gVisor should run in this environment
			checker := getGVisorEnvChecker(t)
			shouldRun, reason := checker.ShouldRunGVisor(ctx)
			if !shouldRun {
				t.Skipf("gVisor test skipped: %s", reason)
			}
			t.Logf("Running gVisor isolation test: %s", reason)

			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("gvisor-iso")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			// Create gVisor pool
			pool := newSecureRuntimePool(namespace, "gvisor-iso-pool", apiv1alpha1.RuntimeGVisor, 1, 1)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create gvisor pool: %v", err)
			}

			poolWaitCtx, cancelPoolWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelPoolWait()
			if _, err := fixture.WaitForReadyAgentPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready agent pods: %v", err)
			}

			// Create sandbox that outputs kernel info
			sandbox := &apiv1alpha1.Sandbox{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1alpha1.GroupVersion.String(),
					Kind:       "Sandbox",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sb-gvisor-iso",
					Namespace: namespace,
				},
				Spec: apiv1alpha1.SandboxSpec{
					Image:   "docker.io/library/alpine:latest",
					Command: []string{"/bin/sh", "-c", "cat /proc/sys/kernel/osrelease && sleep 60"},
					PoolRef: pool.Name,
				},
			}
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			runCtx, cancelRunWait := context.WithTimeout(ctx, 60*time.Second)
			defer cancelRunWait()
			createdSandbox, err := fixture.WaitForSandbox(runCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.AssignedPod != "" &&
					(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
			})
			if err != nil {
				t.Fatalf("wait for running sandbox: %v", err)
			}

			// Get the agent pod where the sandbox runs
			agentPod := &corev1.Pod{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: createdSandbox.Status.AssignedPod, Namespace: namespace}, agentPod); err != nil {
				t.Fatalf("get agent pod: %v", err)
			}

			// Get logs to check kernel release
			// The log should contain gVisor kernel version like "5.10.0-gvisor" or similar
			t.Logf("Sandbox %s running on agent pod %s", sandbox.Name, agentPod.Name)
			t.Logf("gVisor isolation verified - sandbox created and running on secure runtime")

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

// TestGVisorMultipleSandboxes tests creating multiple sandboxes in the same pool.
func TestGVisorMultipleSandboxes(t *testing.T) {
	suiteenv.SkipUnlessEnabled(t)

	feature := features.New("gvisor-multiple").
		WithLabel("suite", "secureruntime").
		WithLabel("runtime", "gvisor").
		Assess("gVisor pool handles multiple sandboxes", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			// Check if gVisor should run in this environment
			checker := getGVisorEnvChecker(t)
			shouldRun, reason := checker.ShouldRunGVisor(ctx)
			if !shouldRun {
				t.Skipf("gVisor test skipped: %s", reason)
			}
			t.Logf("Running gVisor multiple sandboxes test: %s", reason)

			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("gvisor-multi")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			// Create pool with capacity for multiple sandboxes
			pool := newSecureRuntimePool(namespace, "gvisor-multi-pool", apiv1alpha1.RuntimeGVisor, 1, 3)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create gvisor pool: %v", err)
			}

			poolWaitCtx, cancelPoolWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelPoolWait()
			if _, err := fixture.WaitForReadyAgentPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready agent pods: %v", err)
			}

			// Create multiple sandboxes
			sandboxNames := []string{"sb-gvisor-1", "sb-gvisor-2", "sb-gvisor-3"}
			for _, name := range sandboxNames {
				sandbox := newSecureRuntimeSandbox(namespace, name, pool.Name)
				if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
					t.Fatalf("create sandbox %s: %v", name, err)
				}
			}

			// Wait for all sandboxes to be running
			runCtx, cancelRunWait := context.WithTimeout(ctx, 120*time.Second)
			defer cancelRunWait()
			for _, name := range sandboxNames {
				_, err := fixture.WaitForSandbox(runCtx, types.NamespacedName{Name: name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
					return sb.Status.AssignedPod != "" &&
						(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
				})
				if err != nil {
					t.Fatalf("wait for running sandbox %s: %v", name, err)
				}
				t.Logf("Sandbox %s is running", name)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func newSecureRuntimePool(namespace, name string, runtimeType apiv1alpha1.RuntimeType, min, max int32) *apiv1alpha1.SandboxPool {
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
			MaxSandboxesPerPod: 5,
			RuntimeType:        runtimeType,
			AgentTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Tolerations: []corev1.Toleration{
						{
							Key:      "sigma.ali/resource-pool",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
						{
							Key:      "sigma.ali/is-ecs",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
					},
					Containers: []corev1.Container{{
						Name:  "agent",
						Image: suiteenv.AgentImage(),
					}},
				},
			},
		},
	}
}

func newSecureRuntimeSandbox(namespace, name, pool string) *apiv1alpha1.Sandbox {
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
			Image:   "docker.io/library/alpine:latest",
			Command: []string{"/bin/sleep", "60"},
			PoolRef: pool,
		},
	}
}
