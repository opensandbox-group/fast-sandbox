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

// getEnvChecker returns the global environment checker.
func getEnvChecker(t *testing.T) *envcheck.Checker {
	t.Helper()
	checker, err := envcheck.GetChecker()
	if err != nil {
		t.Fatalf("create env checker: %v", err)
	}
	return checker
}

func TestKataQemuSandbox(t *testing.T) {
	suiteenv.SkipUnlessEnabled(t)

	feature := features.New("kata-qemu-sandbox").
		WithLabel("suite", "secureruntime").
		WithLabel("runtime", "kata").
		Assess("Kata QEMU pool creates sandbox successfully", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			// Check if kata-qemu should run in this environment
			checker := getEnvChecker(t)
			shouldRun, reason := checker.ShouldRunKataQemu(ctx)
			if !shouldRun {
				t.Skipf("kata-qemu test skipped: %s", reason)
			}
			t.Logf("Running kata-qemu test: %s", reason)

			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("kata-qemu")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			// Create Kata QEMU pool
			pool := newSecureRuntimePool(namespace, "kata-qemu-pool", apiv1alpha1.RuntimeKataQemu, 1, 1)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create kata pool: %v", err)
			}

			// Wait for ready agent pods
			poolWaitCtx, cancelPoolWait := context.WithTimeout(ctx, 120*time.Second) // Kata needs more time
			defer cancelPoolWait()
			if _, err := fixture.WaitForReadyAgentPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready agent pods: %v", err)
			}

			// Create sandbox
			sandbox := newSecureRuntimeSandbox(namespace, "sb-kata-qemu", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			// Wait for sandbox running (Kata takes longer)
			runCtx, cancelRunWait := context.WithTimeout(ctx, 120*time.Second)
			defer cancelRunWait()
			_, err := fixture.WaitForSandbox(runCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.AssignedPod != "" &&
					(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
			})
			if err != nil {
				t.Fatalf("wait for running sandbox: %v", err)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestKataFcSandbox(t *testing.T) {
	suiteenv.SkipUnlessEnabled(t)

	feature := features.New("kata-fc-sandbox").
		WithLabel("suite", "secureruntime").
		WithLabel("runtime", "kata-fc").
		Assess("Kata Firecracker pool creates sandbox successfully", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			// Check if kata-fc should run in this environment
			checker := getEnvChecker(t)
			shouldRun, reason := checker.ShouldRunKataFc(ctx)
			if !shouldRun {
				t.Skipf("kata-fc test skipped: %s", reason)
			}
			t.Logf("Running kata-fc test: %s", reason)

			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("kata-fc")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := newSecureRuntimePool(namespace, "kata-fc-pool", apiv1alpha1.RuntimeKataFc, 1, 1)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create kata-fc pool: %v", err)
			}

			poolWaitCtx, cancelPoolWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelPoolWait()
			if _, err := fixture.WaitForReadyAgentPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready agent pods: %v", err)
			}

			sandbox := newSecureRuntimeSandbox(namespace, "sb-kata-fc", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			runCtx, cancelRunWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelRunWait()
			_, err := fixture.WaitForSandbox(runCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.AssignedPod != "" &&
					(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
			})
			if err != nil {
				t.Fatalf("wait for running sandbox: %v", err)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestKataClhSandbox(t *testing.T) {
	suiteenv.SkipUnlessEnabled(t)

	feature := features.New("kata-clh-sandbox").
		WithLabel("suite", "secureruntime").
		WithLabel("runtime", "kata-clh").
		Assess("Kata Cloud Hypervisor pool creates sandbox successfully", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			// Check if kata-clh should run in this environment
			checker := getEnvChecker(t)
			shouldRun, reason := checker.ShouldRunKataClh(ctx)
			if !shouldRun {
				t.Skipf("kata-clh test skipped: %s", reason)
			}
			t.Logf("Running kata-clh test: %s", reason)

			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("kata-clh")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := newSecureRuntimePool(namespace, "kata-clh-pool", apiv1alpha1.RuntimeKataClh, 1, 1)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create kata-clh pool: %v", err)
			}

			poolWaitCtx, cancelPoolWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelPoolWait()
			if _, err := fixture.WaitForReadyAgentPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready agent pods: %v", err)
			}

			sandbox := newSecureRuntimeSandbox(namespace, "sb-kata-clh", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			runCtx, cancelRunWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelRunWait()
			_, err := fixture.WaitForSandbox(runCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.AssignedPod != "" &&
					(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
			})
			if err != nil {
				t.Fatalf("wait for running sandbox: %v", err)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}
