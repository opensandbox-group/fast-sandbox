package secureruntime

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	e2eenv "fast-sandbox/test/e2e/env"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"

	"github.com/opencontainers/runtime-spec/specs-go"
	corev1 "k8s.io/api/core/v1"
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

			pool := newSecureRuntimePool(namespace, "kata-qemu-pool", apiv1alpha2.RuntimeKataQemu, 1, 1)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create kata pool: %v", err)
			}

			poolWaitCtx, cancelPoolWait := context.WithTimeout(ctx, 120*time.Second) // Kata needs more time
			defer cancelPoolWait()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}
			waitForKataRuntimeReady(ctx, t, fixture, namespace, pool.Name)

			ctl := newFastctlForNamespace(ctx, t, cliBinaryPath, namespace)
			runKataSandboxWhenCapacityAvailable(ctx, t, ctl, "sb-kata-qemu", secureRuntimeFastctlConfig(pool.Name, "kata-qemu-ok"))

			runCtx, cancelRunWait := context.WithTimeout(ctx, 120*time.Second)
			defer cancelRunWait()
			if _, err := ctl.WaitRunning(runCtx, "sb-kata-qemu"); err != nil {
				t.Fatalf("wait for kata-qemu sandbox running via fastctl: %v", err)
			}
			verifyKataRuntime(ctx, t, k8sClient, fixture, namespace, "sb-kata-qemu", apiv1alpha2.RuntimeKataQemu, "kata-qemu-ok")

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestKataDragonballSandbox(t *testing.T) {
	manager := suiteenv.RequireKataDragonball(t)
	cliBinaryPath := buildFastctl(t, manager)

	feature := features.New("kata-dragonball-sandbox").
		WithLabel("suite", "secureruntime").
		WithLabel("runtime", "kata-dragonball").
		Assess("Kata Dragonball pool creates sandbox successfully", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("kata-dragonball")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := newSecureRuntimePool(namespace, "kata-dragonball-pool", apiv1alpha2.RuntimeKataDragonball, 1, 1)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create kata-dragonball pool: %v", err)
			}

			poolWaitCtx, cancelPoolWait := context.WithTimeout(ctx, 120*time.Second)
			defer cancelPoolWait()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready kata-dragonball Fastlet: %v", err)
			}
			waitForKataRuntimeReady(ctx, t, fixture, namespace, pool.Name)

			ctl := newFastctlForNamespace(ctx, t, cliBinaryPath, namespace)
			runKataSandboxWhenCapacityAvailable(ctx, t, ctl, "sb-kata-dragonball", secureRuntimeFastctlConfig(pool.Name, "kata-dragonball-ok"))

			runCtx, cancelRunWait := context.WithTimeout(ctx, 120*time.Second)
			defer cancelRunWait()
			if _, err := ctl.WaitRunning(runCtx, "sb-kata-dragonball"); err != nil {
				t.Fatalf("wait for kata-dragonball sandbox: %v", err)
			}
			verifyKataRuntime(ctx, t, k8sClient, fixture, namespace, "sb-kata-dragonball", apiv1alpha2.RuntimeKataDragonball, "kata-dragonball-ok")

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestKataFcSandbox(t *testing.T) {
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

			pool := newSecureRuntimePool(namespace, "kata-fc-pool", apiv1alpha2.RuntimeKataFc, 1, 1)
			pool.Spec.InfraComponents = []apiv1alpha2.InfraComponent{fixtures.OpenSandboxExecdComponent()}
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create kata-fc pool: %v", err)
			}

			poolWaitCtx, cancelPoolWait := context.WithTimeout(ctx, 180*time.Second)
			defer cancelPoolWait()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready kata-fc Fastlet: %v", err)
			}
			waitForKataRuntimeReady(ctx, t, fixture, namespace, pool.Name)

			ctl := newFastctlForNamespace(ctx, t, cliBinaryPath, namespace)
			runKataSandboxWhenCapacityAvailable(ctx, t, ctl, "sb-kata-fc", secureRuntimeFastctlConfig(pool.Name, "kata-fc-ok"))

			runCtx, cancelRunWait := context.WithTimeout(ctx, 180*time.Second)
			defer cancelRunWait()
			if _, err := ctl.WaitRunning(runCtx, "sb-kata-fc"); err != nil {
				t.Fatalf("wait for kata-fc sandbox running via fastctl: %v", err)
			}
			verifyKataRuntime(ctx, t, k8sClient, fixture, namespace, "sb-kata-fc", apiv1alpha2.RuntimeKataFc, "kata-fc-ok")
			output, err := ctl.Command(ctx, "opensandbox", "exec", "sb-kata-fc", "--", "sh", "-lc", "printf kata-fc-execd-ok")
			if err != nil {
				t.Fatalf("execute through Firecracker Execd after Fastlet recovery: %v\n%s", err, output)
			}
			if !strings.Contains(string(output), "kata-fc-execd-ok") {
				t.Fatalf("unexpected Firecracker Execd output: %s", output)
			}

			deleteKataFCSandboxAndVerifyNoVMM(ctx, t, k8sClient, fixture, ctl, namespace, "sb-kata-fc")

			// A second create/delete on the same one-slot Fastlet proves that the
			// first deletion released capacity without accumulating an orphan VMM.
			runKataSandboxWhenCapacityAvailable(ctx, t, ctl, "sb-kata-fc-reuse", secureRuntimeFastctlConfig(pool.Name, "kata-fc-reuse-ok"))
			reuseCtx, cancelReuseWait := context.WithTimeout(ctx, 180*time.Second)
			defer cancelReuseWait()
			if _, err := ctl.WaitRunning(reuseCtx, "sb-kata-fc-reuse"); err != nil {
				t.Fatalf("wait for replacement kata-fc sandbox: %v", err)
			}
			deleteKataFCSandboxAndVerifyNoVMM(ctx, t, k8sClient, fixture, ctl, namespace, "sb-kata-fc-reuse")

			verifyKataFCLostFastletCleanup(ctx, t, k8sClient, fixture, ctl, namespace, pool.Name)

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func verifyKataFCLostFastletCleanup(
	ctx context.Context,
	t *testing.T,
	k8sClient client.Client,
	fixture *fixtures.FixtureClient,
	ctl *e2eenv.Fastctl,
	namespace string,
	poolName string,
) {
	t.Helper()
	const sandboxName = "sb-kata-fc-lost-fastlet"
	runKataSandboxWhenCapacityAvailable(ctx, t, ctl, sandboxName, secureRuntimeFastctlConfig(poolName, "kata-fc-lost-fastlet-ok"))
	waitCtx, cancelWait := context.WithTimeout(ctx, 180*time.Second)
	defer cancelWait()
	sandbox, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Name: sandboxName, Namespace: namespace}, func(item *apiv1alpha2.Sandbox) bool {
		return item.Status.Placement.FastletName != "" && item.Status.Runtime.State == apiv1alpha2.RuntimeReady
	})
	if err != nil {
		t.Fatalf("wait for lost-Fastlet cleanup Sandbox: %v", err)
	}
	fastlet := &corev1.Pod{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: sandbox.Status.Placement.FastletName, Namespace: namespace}, fastlet); err != nil {
		t.Fatalf("get Firecracker Fastlet before Pod loss: %v", err)
	}
	sandboxID := string(sandbox.UID)
	if err := k8sClient.Delete(ctx, fastlet); err != nil {
		t.Fatalf("delete Firecracker Fastlet Pod: %v", err)
	}
	cleanupCtx, cancelCleanup := context.WithTimeout(ctx, 90*time.Second)
	defer cancelCleanup()
	assertNoFirecrackerProcessWithin(cleanupCtx, t, fastlet.Spec.NodeName, sandboxID, 85*time.Second)
	t.Log("NodeJanitor removed the Firecracker process after the owning Fastlet Pod was lost")
}

func deleteKataFCSandboxAndVerifyNoVMM(
	ctx context.Context,
	t *testing.T,
	k8sClient client.Client,
	fixture *fixtures.FixtureClient,
	ctl *e2eenv.Fastctl,
	namespace string,
	sandboxName string,
) {
	t.Helper()
	sandbox := &apiv1alpha2.Sandbox{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: sandboxName, Namespace: namespace}, sandbox); err != nil {
		t.Fatalf("get kata-fc sandbox before deletion: %v", err)
	}
	if sandbox.Status.Placement.FastletName == "" {
		t.Fatalf("kata-fc sandbox %s has no assignment before deletion", sandboxName)
	}
	fastlet := &corev1.Pod{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: sandbox.Status.Placement.FastletName, Namespace: namespace}, fastlet); err != nil {
		t.Fatalf("get kata-fc Fastlet before deletion: %v", err)
	}
	sandboxID := string(sandbox.UID)
	if err := ctl.Delete(ctx, sandboxName); err != nil {
		t.Fatalf("delete kata-fc sandbox %s: %v", sandboxName, err)
	}
	deleteCtx, cancelDelete := context.WithTimeout(ctx, 30*time.Second)
	defer cancelDelete()
	if err := fixture.WaitForSandboxDeleted(deleteCtx, types.NamespacedName{Name: sandboxName, Namespace: namespace}); err != nil {
		t.Fatalf("wait for kata-fc sandbox %s deletion: %v", sandboxName, err)
	}
	assertNoFirecrackerProcess(deleteCtx, t, fastlet.Spec.NodeName, sandboxID)
}

func assertNoFirecrackerProcess(ctx context.Context, t *testing.T, nodeName, sandboxID string) {
	t.Helper()
	assertNoFirecrackerProcessWithin(ctx, t, nodeName, sandboxID, 5*time.Second)
}

func assertNoFirecrackerProcessWithin(ctx context.Context, t *testing.T, nodeName, sandboxID string, timeout time.Duration) {
	t.Helper()
	const script = `expected=$(printf '%.32s' "$1")
for file in /proc/[0-9]*/cmdline; do
  [ -r "$file" ] || continue
  args=$(tr '\000' '\n' < "$file")
  executable=$(printf '%s\n' "$args" | sed -n '1p')
  process_id=$(printf '%s\n' "$args" | awk 'previous == "--id" { print; exit } { previous = $0 }')
  if [ "${executable##*/}" = firecracker ] && [ "$process_id" = "$expected" ]; then
    echo "residual Firecracker process ${file#/proc/} sandbox-id=$process_id"
  fi
done`
	deadline := time.Now().Add(timeout)
	var last string
	for {
		output, err := exec.CommandContext(ctx, "docker", "exec", nodeName, "sh", "-c", script, "sh", sandboxID).CombinedOutput()
		last = strings.TrimSpace(string(output))
		if err == nil && last == "" {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("Firecracker process for deleted sandbox %s still exists on node %s: commandErr=%v output=%s", sandboxID, nodeName, err, last)
		}
		time.Sleep(100 * time.Millisecond)
	}
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

			pool := newSecureRuntimePool(namespace, "kata-clh-pool", apiv1alpha2.RuntimeKataClh, 1, 1)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create kata-clh pool: %v", err)
			}

			poolWaitCtx, cancelPoolWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelPoolWait()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}
			waitForKataRuntimeReady(ctx, t, fixture, namespace, pool.Name)

			ctl := newFastctlForNamespace(ctx, t, cliBinaryPath, namespace)
			runKataSandboxWhenCapacityAvailable(ctx, t, ctl, "sb-kata-clh", secureRuntimeFastctlConfig(pool.Name, "kata-clh-ok"))

			runCtx, cancelRunWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelRunWait()
			if _, err := ctl.WaitRunning(runCtx, "sb-kata-clh"); err != nil {
				t.Fatalf("wait for kata-clh sandbox running via fastctl: %v", err)
			}
			verifyKataRuntime(ctx, t, k8sClient, fixture, namespace, "sb-kata-clh", apiv1alpha2.RuntimeKataClh, "kata-clh-ok")

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
	proxyEndpoint, proxyForward, err := e2eenv.StartSandboxProxyPortForward(ctx, testSuite.ControllerNamespace())
	if err != nil {
		t.Fatalf("start Sandbox Proxy port-forward: %v", err)
	}
	t.Cleanup(func() {
		if err := proxyForward.Cleanup(); err != nil {
			t.Logf("cleanup Sandbox Proxy port-forward: %v", err)
		}
	})

	return e2eenv.NewFastctl(
		e2eenv.WithFastctlBinary(cliBinaryPath),
		e2eenv.WithFastctlEndpoint(endpoint),
		e2eenv.WithFastctlProxyEndpoint(proxyEndpoint),
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
		Image:   "docker.io/library/alpine:latest",
		PoolRef: poolName,
		Command: []string{"/bin/sh"},
		Args:    []string{"-c", script},
	}
}

func waitForKataRuntimeReady(ctx context.Context, t *testing.T, fixture *fixtures.FixtureClient, namespace, poolName string) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if _, err := fixture.WaitForPoolCondition(waitCtx, types.NamespacedName{Name: poolName, Namespace: namespace}, apiv1alpha2.PoolConditionRuntimeReady, metav1.ConditionTrue); err != nil {
		t.Fatalf("wait for Kata RuntimeReady: %v", err)
	}
	if _, err := fixture.WaitForPoolCondition(waitCtx, types.NamespacedName{Name: poolName, Namespace: namespace}, apiv1alpha2.PoolConditionInfraReady, metav1.ConditionTrue); err != nil {
		t.Fatalf("wait for Kata InfraReady: %v", err)
	}
}

func verifyKataRuntime(ctx context.Context, t *testing.T, kubeClient client.Client, fixture *fixtures.FixtureClient, namespace, sandboxName string, runtimeName apiv1alpha2.RuntimeName, marker string) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	sandbox, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Name: sandboxName, Namespace: namespace}, func(item *apiv1alpha2.Sandbox) bool {
		return item.Status.Placement.FastletName != "" && item.Status.Runtime.State == apiv1alpha2.RuntimeReady && item.Status.DataPlane.State == apiv1alpha2.DataPlaneReady
	})
	if err != nil {
		t.Fatalf("wait for %s Sandbox readiness: %v", runtimeName, err)
	}
	fastlet := &corev1.Pod{}
	if err := kubeClient.Get(ctx, types.NamespacedName{Name: sandbox.Status.Placement.FastletName, Namespace: namespace}, fastlet); err != nil {
		t.Fatalf("get %s Fastlet: %v", runtimeName, err)
	}
	sandboxID := secureRuntimeSandboxIdentifier(sandbox)
	runtimeInfo := secureRuntimeDockerOutput(ctx, t, "exec", fastlet.Spec.NodeName, "ctr", "-n", "k8s.io", "containers", "info", sandboxID)
	assertKataOCISpecResources(t, runtimeName, runtimeInfo)
	state := waitForSecureRuntimeNetworkState(ctx, t, namespace, fastlet.Name, string(fastlet.UID), sandboxID)
	guestOutput := waitForSecureRuntimeHTTP(ctx, t, namespace, fastlet.Name, state.IP, 18080, marker, sandboxID)
	hostKernel := strings.TrimSpace(secureRuntimeKubectlOutput(ctx, t, "exec", "-n", namespace, fastlet.Name, "-c", "fastlet", "--", "uname", "-r"))
	guestKernel := secureRuntimeLogValue(guestOutput, "KERNEL=")
	if guestKernel == "" || guestKernel == hostKernel {
		t.Fatalf("%s guest kernel was not isolated: guest=%q host=%q output=%q", runtimeName, guestKernel, hostKernel, guestOutput)
	}
	if got := secureRuntimeLogValue(guestOutput, "DNS="); got != "DNS_OK" {
		t.Fatalf("%s DNS result = %q, want DNS_OK", runtimeName, got)
	}
	verifySecureRuntimeProxy(ctx, t, sandbox.Namespace, sandbox.Name, string(sandbox.UID), 18080, marker)

	previousRestarts := fastletContainerRestartCount(fastlet)
	_, _ = secureRuntimeKubectl(ctx, "exec", "-n", namespace, fastlet.Name, "-c", "fastlet", "--", "kill", "1")
	waitForFastletContainerRestart(ctx, t, kubeClient, namespace, fastlet.Name, string(fastlet.UID), previousRestarts)
	waitForSecureRuntimeHTTP(ctx, t, namespace, fastlet.Name, state.IP, 18080, marker, sandboxID)
	verifySecureRuntimeProxy(ctx, t, sandbox.Namespace, sandbox.Name, string(sandbox.UID), 18080, marker)
	t.Logf("%s isolation, resource limits, private network, proxy and recovery verified: guest kernel=%s private IP=%s", runtimeName, guestKernel, state.IP)
}

func assertKataOCISpecResources(t *testing.T, runtimeName apiv1alpha2.RuntimeName, runtimeInfo string) {
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

func runKataSandboxWhenCapacityAvailable(ctx context.Context, t *testing.T, ctl *e2eenv.Fastctl, name string, config e2eenv.FastctlConfig) []byte {
	t.Helper()
	createCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	output, err := ctl.RunWhenCapacityAvailable(createCtx, name, config)
	if err != nil {
		t.Fatalf("fastctl run %s when FastPath capacity is available: %v\noutput: %s", name, err, output)
	}
	return output
}
