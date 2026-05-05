package secureruntime

import (
	"context"
	"os"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	e2eenv "fast-sandbox/test/e2e/env"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestKataQemuSandbox(t *testing.T) {
	manager := suiteenv.RequireKataQemu(t)
	cliBinaryPath := buildFastctl(t, manager)

	feature := features.New("kata-qemu-sandbox").
		WithLabel("suite", "secureruntime").
		WithLabel("runtime", "kata").
		Assess("Kata QEMU pool creates sandbox successfully", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
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

			// Wait for ready fastlet pods
			poolWaitCtx, cancelPoolWait := context.WithTimeout(ctx, 120*time.Second) // Kata needs more time
			defer cancelPoolWait()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}
			waitForFastletRegistrySync(t)

			ctl := newFastctlForNamespace(ctx, t, cliBinaryPath, namespace)
			if output, err := ctl.Run(ctx, "sb-kata-qemu", secureRuntimeFastctlConfig(pool.Name)); err != nil {
				t.Fatalf("fastctl run kata-qemu sandbox: %v\noutput: %s", err, output)
			}

			runCtx, cancelRunWait := context.WithTimeout(ctx, 120*time.Second)
			defer cancelRunWait()
			if _, err := ctl.WaitRunning(runCtx, "sb-kata-qemu"); err != nil {
				t.Fatalf("wait for kata-qemu sandbox running via fastctl: %v", err)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestKataFcSandbox(t *testing.T) {
	// TODO(kata-fc): Enable this by default after the remote kind/Firecracker
	// environment can boot a plain RuntimeClass=kata-fc pod. On 2026-05-04 the
	// minimal pod failed before fast-sandbox with FailedCreatePodSandBox:
	// "timed out connecting to hybrid vsocket .../root/kata.hvsock". Real
	// kernel/image paths, disabling jailer, and dial_timeout=120 did not fix it.
	if os.Getenv("FAST_SANDBOX_E2E_KATA_FC") != "1" {
		t.Skip("TODO(kata-fc): Firecracker profile is currently opt-in; set FAST_SANDBOX_E2E_KATA_FC=1 to investigate the hvsock boot issue")
	}

	manager := suiteenv.RequireKataFc(t)
	cliBinaryPath := buildFastctl(t, manager)

	feature := features.New("kata-fc-sandbox").
		WithLabel("suite", "secureruntime").
		WithLabel("runtime", "kata-fc").
		Assess("Kata Firecracker pool creates sandbox successfully", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
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
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}
			waitForFastletRegistrySync(t)

			ctl := newFastctlForNamespace(ctx, t, cliBinaryPath, namespace)
			if output, err := ctl.Run(ctx, "sb-kata-fc", secureRuntimeFastctlConfig(pool.Name)); err != nil {
				t.Fatalf("fastctl run kata-fc sandbox: %v\noutput: %s", err, output)
			}

			runCtx, cancelRunWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelRunWait()
			if _, err := ctl.WaitRunning(runCtx, "sb-kata-fc"); err != nil {
				t.Fatalf("wait for kata-fc sandbox running via fastctl: %v", err)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestKataClhSandbox(t *testing.T) {
	manager := suiteenv.RequireKataClh(t)
	cliBinaryPath := buildFastctl(t, manager)

	feature := features.New("kata-clh-sandbox").
		WithLabel("suite", "secureruntime").
		WithLabel("runtime", "kata-clh").
		Assess("Kata Cloud Hypervisor pool creates sandbox successfully", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
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
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}
			waitForFastletRegistrySync(t)

			ctl := newFastctlForNamespace(ctx, t, cliBinaryPath, namespace)
			if output, err := ctl.Run(ctx, "sb-kata-clh", secureRuntimeFastctlConfig(pool.Name)); err != nil {
				t.Fatalf("fastctl run kata-clh sandbox: %v\noutput: %s", err, output)
			}

			runCtx, cancelRunWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelRunWait()
			if _, err := ctl.WaitRunning(runCtx, "sb-kata-clh"); err != nil {
				t.Fatalf("wait for kata-clh sandbox running via fastctl: %v", err)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func buildFastctl(t *testing.T, manager *e2eenv.Manager) string {
	t.Helper()

	cliBinaryPath, err := manager.BuildFastctl(context.Background())
	if err != nil {
		t.Fatalf("build fastctl binary: %v", err)
	}
	return cliBinaryPath
}

func newFastctlForNamespace(ctx context.Context, t *testing.T, cliBinaryPath, namespace string) *e2eenv.Fastctl {
	t.Helper()

	endpoint, pf, err := e2eenv.StartControllerPortForward(ctx, testSuite.ControllerNamespace())
	if err != nil {
		t.Fatalf("start controller port-forward: %v", err)
	}
	t.Cleanup(func() {
		if err := pf.Cleanup(); err != nil {
			t.Logf("cleanup controller port-forward: %v", err)
		}
	})

	return e2eenv.NewFastctl(
		e2eenv.WithFastctlBinary(cliBinaryPath),
		e2eenv.WithFastctlEndpoint(endpoint),
		e2eenv.WithFastctlNamespace(namespace),
	)
}

func secureRuntimeFastctlConfig(poolName string) e2eenv.FastctlConfig {
	return e2eenv.FastctlConfig{
		Image:           "docker.io/library/alpine:latest",
		PoolRef:         poolName,
		ConsistencyMode: "strong",
		Command:         []string{"/bin/sleep"},
		Args:            []string{"60"},
	}
}

func waitForFastletRegistrySync(t *testing.T) {
	t.Helper()
	t.Log("waiting for fastlet capacity to sync to controller registry")
	time.Sleep(8 * time.Second)
}
