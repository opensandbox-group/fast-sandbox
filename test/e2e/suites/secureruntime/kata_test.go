package secureruntime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	e2eenv "fast-sandbox/test/e2e/env"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"

	corev1 "k8s.io/api/core/v1"
	"github.com/opencontainers/runtime-spec/specs-go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
			waitForKataRuntimeReady(ctx, t, fixture, namespace, pool.Name)
			waitForFastletRegistrySync(t)

			ctl := newFastctlForNamespace(ctx, t, cliBinaryPath, namespace)
			if output, err := ctl.Run(ctx, "sb-kata-qemu", secureRuntimeFastctlConfig(pool.Name, "kata-qemu-ok")); err != nil {
				t.Fatalf("fastctl run kata-qemu sandbox: %v\noutput: %s", err, output)
			}

			runCtx, cancelRunWait := context.WithTimeout(ctx, 120*time.Second)
			defer cancelRunWait()
			if _, err := ctl.WaitRunning(runCtx, "sb-kata-qemu"); err != nil {
				t.Fatalf("wait for kata-qemu sandbox running via fastctl: %v", err)
			}
			verifyKataRuntime(ctx, t, k8sClient, fixture, namespace, "sb-kata-qemu", apiv1alpha1.RuntimeKataQemu, "kata-qemu-ok")

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestKataFcSandbox(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("kata-fc-capability-gate").
		WithLabel("suite", "secureruntime").
		WithLabel("runtime", "kata-fc").
		Assess("Kata Firecracker fails closed until the runtime is validated", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
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

			conditionCtx, cancelCondition := context.WithTimeout(ctx, 30*time.Second)
			defer cancelCondition()
			updated, err := fixture.WaitForPoolCondition(conditionCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, apiv1alpha1.PoolConditionRuntimeReady, metav1.ConditionFalse)
			if err != nil {
				t.Fatalf("wait for kata-fc capability gate: %v", err)
			}
			var runtimeCondition *metav1.Condition
			for index := range updated.Status.Conditions {
				if updated.Status.Conditions[index].Type == apiv1alpha1.PoolConditionRuntimeReady {
					runtimeCondition = &updated.Status.Conditions[index]
					break
				}
			}
			if runtimeCondition == nil || runtimeCondition.Reason != apiv1alpha1.ReasonRuntimeUnavailable || runtimeCondition.Message != "KataFirecrackerNotValidated" {
				t.Fatalf("unexpected kata-fc capability condition: %+v", runtimeCondition)
			}
			var fastlets corev1.PodList
			if err := k8sClient.List(ctx, &fastlets, client.InNamespace(namespace), client.MatchingLabels{"fast-sandbox.io/pool": pool.Name}); err != nil {
				t.Fatalf("list kata-fc Fastlets: %v", err)
			}
			if len(fastlets.Items) != 0 {
				t.Fatalf("degraded kata-fc profile created %d Fastlet Pod(s)", len(fastlets.Items))
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
			waitForKataRuntimeReady(ctx, t, fixture, namespace, pool.Name)
			waitForFastletRegistrySync(t)

			ctl := newFastctlForNamespace(ctx, t, cliBinaryPath, namespace)
			if output, err := ctl.Run(ctx, "sb-kata-clh", secureRuntimeFastctlConfig(pool.Name, "kata-clh-ok")); err != nil {
				t.Fatalf("fastctl run kata-clh sandbox: %v\noutput: %s", err, output)
			}

			runCtx, cancelRunWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelRunWait()
			if _, err := ctl.WaitRunning(runCtx, "sb-kata-clh"); err != nil {
				t.Fatalf("wait for kata-clh sandbox running via fastctl: %v", err)
			}
			verifyKataRuntime(ctx, t, k8sClient, fixture, namespace, "sb-kata-clh", apiv1alpha1.RuntimeKataClh, "kata-clh-ok")

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

func secureRuntimeFastctlConfig(poolName, marker string) e2eenv.FastctlConfig {
	script := `dns=DNS_FAIL
if nslookup kubernetes.default.svc.cluster.local >/dev/null 2>&1; then dns=DNS_OK; fi
kernel=$(uname -r)
cat > /serve.sh <<EOF
#!/bin/sh
printf 'HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n` + marker + `\nDNS=$dns\nKERNEL=$kernel\n'
EOF
chmod +x /serve.sh
exec nc -lk -p 18080 -e /serve.sh`
	return e2eenv.FastctlConfig{
		Image:           "docker.io/library/alpine:latest",
		PoolRef:         poolName,
		ConsistencyMode: "strong",
		Command:         []string{"/bin/sh"},
		Args:            []string{"-c", script},
	}
}

func waitForKataRuntimeReady(ctx context.Context, t *testing.T, fixture *fixtures.FixtureClient, namespace, poolName string) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := fixture.WaitForPoolCondition(waitCtx, types.NamespacedName{Name: poolName, Namespace: namespace}, apiv1alpha1.PoolConditionRuntimeReady, metav1.ConditionTrue); err != nil {
		t.Fatalf("wait for Kata RuntimeReady: %v", err)
	}
}

func verifyKataRuntime(ctx context.Context, t *testing.T, kubeClient client.Client, fixture *fixtures.FixtureClient, namespace, sandboxName string, runtimeName apiv1alpha1.RuntimeName, marker string) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	sandbox, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Name: sandboxName, Namespace: namespace}, func(item *apiv1alpha1.Sandbox) bool {
		return item.Status.Assignment != nil && item.Status.RuntimeState == apiv1alpha1.ObservedStateReady && item.Status.DataPlaneState == apiv1alpha1.ObservedStateReady
	})
	if err != nil {
		t.Fatalf("wait for %s Sandbox readiness: %v", runtimeName, err)
	}
	fastlet := &corev1.Pod{}
	if err := kubeClient.Get(ctx, types.NamespacedName{Name: sandbox.Status.Assignment.FastletName, Namespace: namespace}, fastlet); err != nil {
		t.Fatalf("get %s Fastlet: %v", runtimeName, err)
	}
	sandboxID := secureRuntimeSandboxIdentifier(sandbox)
	runtimeInfo := secureRuntimeDockerOutput(ctx, t, "exec", fastlet.Spec.NodeName, "ctr", "-n", "k8s.io", "containers", "info", sandboxID)
	assertKataOCISpecResources(t, runtimeName, runtimeInfo)
	state := waitForSecureRuntimeNetworkState(ctx, t, namespace, fastlet.Name, string(fastlet.UID), sandboxID)
	guestOutput := waitForSecureRuntimeHTTP(ctx, t, namespace, fastlet.Name, state.IP, 18080, marker)
	hostKernel := strings.TrimSpace(secureRuntimeKubectlOutput(ctx, t, "exec", "-n", namespace, fastlet.Name, "-c", "fastlet", "--", "uname", "-r"))
	guestKernel := secureRuntimeLogValue(guestOutput, "KERNEL=")
	if guestKernel == "" || guestKernel == hostKernel {
		t.Fatalf("%s guest kernel was not isolated: guest=%q host=%q output=%q", runtimeName, guestKernel, hostKernel, guestOutput)
	}
	if got := secureRuntimeLogValue(guestOutput, "DNS="); got != "DNS_OK" {
		t.Fatalf("%s DNS result = %q, want DNS_OK", runtimeName, got)
	}
	verifySecureRuntimeProxy(ctx, t, string(sandbox.UID), 18080, marker)

	previousRestarts := fastletContainerRestartCount(fastlet)
	_, _ = secureRuntimeKubectl(ctx, "exec", "-n", namespace, fastlet.Name, "-c", "fastlet", "--", "kill", "1")
	waitForFastletContainerRestart(ctx, t, kubeClient, namespace, fastlet.Name, string(fastlet.UID), previousRestarts)
	waitForSecureRuntimeHTTP(ctx, t, namespace, fastlet.Name, state.IP, 18080, marker)
	verifySecureRuntimeProxy(ctx, t, string(sandbox.UID), 18080, marker)
	t.Logf("%s isolation, resource limits, private network, proxy and recovery verified: guest kernel=%s private IP=%s", runtimeName, guestKernel, state.IP)
}

func assertKataOCISpecResources(t *testing.T, runtimeName apiv1alpha1.RuntimeName, runtimeInfo string) {
	t.Helper()
	var info struct {
		Runtime struct {
			Name string `json:"Name"`
		} `json:"Runtime"`
		Spec specs.Spec `json:"Spec"`
	}
	if err := json.Unmarshal([]byte(runtimeInfo), &info); err != nil {
		t.Fatalf("decode %s containerd info: %v: %s", runtimeName, err, runtimeInfo)
	}
	if info.Runtime.Name != "io.containerd.kata.v2" {
		t.Fatalf("%s containerd runtime = %q, want io.containerd.kata.v2", runtimeName, info.Runtime.Name)
	}
	if info.Spec.Linux == nil || info.Spec.Linux.Resources == nil {
		t.Fatalf("%s OCI spec has no Linux resources: %s", runtimeName, runtimeInfo)
	}
	resources := info.Spec.Linux.Resources
	if resources.Memory == nil || resources.Memory.Limit == nil || *resources.Memory.Limit != 268435456 {
		t.Fatalf("%s OCI memory limit = %+v, want 268435456", runtimeName, resources.Memory)
	}
	if resources.CPU == nil || resources.CPU.Quota == nil || resources.CPU.Period == nil || *resources.CPU.Quota != 25000 || *resources.CPU.Period != 100000 {
		t.Fatalf("%s OCI CPU limit = %+v, want quota=25000 period=100000", runtimeName, resources.CPU)
	}
	if resources.Pids == nil || resources.Pids.Limit == nil || *resources.Pids.Limit != 128 {
		t.Fatalf("%s OCI PIDs limit = %+v, want 128", runtimeName, resources.Pids)
	}
}

func waitForFastletRegistrySync(t *testing.T) {
	t.Helper()
	t.Log("waiting for fastlet capacity to sync to controller registry")
	time.Sleep(8 * time.Second)
}
