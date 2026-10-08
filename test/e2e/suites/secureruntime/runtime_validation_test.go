package secureruntime

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"
)

func TestFirecrackerPoolWaitsForRuntimeHeartbeat(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("firecracker-runtime-heartbeat").
		WithLabel("suite", "secureruntime").
		WithLabel("tier", "validation").
		Assess("a configured Firecracker pool cannot assign sandboxes without a ready Fastlet heartbeat", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("firecracker-heartbeat")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			// Withhold the child heartbeat deterministically, regardless of whether
			// this host has Firecracker/KVM assets. This is a readiness test, not a
			// driver lifecycle test or an assertion that Firecracker is unsupported.
			pool := newSecureRuntimePool(namespace, "firecracker-heartbeat-pool", apiv1alpha2.RuntimeFirecracker, 1, 1)
			pool.Spec.FastletTemplate.Spec.NodeSelector = map[string]string{
				"fast-sandbox.io/e2e-runtime-heartbeat": namespace,
			}
			var matchingNodes corev1.NodeList
			if err := k8sClient.List(ctx, &matchingNodes, client.MatchingLabels(pool.Spec.FastletTemplate.Spec.NodeSelector)); err != nil {
				t.Fatalf("check test node selector: %v", err)
			}
			if len(matchingNodes.Items) != 0 {
				t.Fatal("test node selector must not match any node")
			}
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create Firecracker pool: %v", err)
			}

			poolKey := types.NamespacedName{Name: pool.Name, Namespace: namespace}
			if err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, 30*time.Second, true, func(ctx context.Context) (bool, error) {
				var updatedPool apiv1alpha2.SandboxPool
				if err := k8sClient.Get(ctx, poolKey, &updatedPool); err != nil {
					return false, err
				}
				condition := apiMeta.FindStatusCondition(updatedPool.Status.Conditions, apiv1alpha2.PoolConditionRuntimeReady)
				if condition == nil || condition.ObservedGeneration != updatedPool.Generation {
					return false, nil
				}
				if condition.Status != metav1.ConditionFalse || condition.Reason != apiv1alpha2.ReasonRuntimeCapabilityPending {
					return false, fmt.Errorf("expected RuntimeReady=False/RuntimeCapabilityPending, got %s/%s: %s", condition.Status, condition.Reason, condition.Message)
				}
				return true, nil
			}); err != nil {
				t.Fatalf("wait for Firecracker heartbeat condition: %v", err)
			}

			sandbox := newSecureRuntimeSandbox(namespace, "sb-firecracker-pending", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}
			sandboxKey := types.NamespacedName{Name: sandbox.Name, Namespace: namespace}
			pendingCtx, cancelPending := context.WithTimeout(ctx, 30*time.Second)
			defer cancelPending()
			if _, err := fixture.WaitForSandbox(pendingCtx, sandboxKey, func(sb *apiv1alpha2.Sandbox) bool {
				condition := apiMeta.FindStatusCondition(sb.Status.Conditions, apiv1alpha2.SandboxConditionReady)
				return sb.Status.Placement.FastletName == "" && sb.Status.Runtime.State == apiv1alpha2.RuntimePending &&
					condition != nil && condition.Status == metav1.ConditionFalse && condition.Reason == "NoCandidate"
			}); err != nil {
				t.Fatalf("wait for unassigned Pending sandbox: %v", err)
			}
			if err := fixture.EnsureSandboxRemainsUnassigned(ctx, sandboxKey, 5*time.Second); err != nil {
				t.Fatalf("ensure sandbox stays unassigned without a ready runtime heartbeat: %v", err)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestRuntimeValidationContainerDefault(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("container-runtime-default").
		WithLabel("suite", "secureruntime").
		WithLabel("tier", "validation").
		Assess("container runtime type works without RuntimeClass validation", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("container-default")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			// Create pool with container runtime (no RuntimeClass needed)
			pool := newSecureRuntimePool(namespace, "container-pool", apiv1alpha2.RuntimeContainer, 1, 1)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create container pool: %v", err)
			}

			poolWaitCtx, cancelPoolWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelPoolWait()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}

			var fastlets corev1.PodList
			if err := k8sClient.List(ctx, &fastlets, client.InNamespace(namespace), client.MatchingLabels{"fast-sandbox.io/pool": pool.Name}); err != nil {
				t.Fatalf("list fastlet pods: %v", err)
			}
			if len(fastlets.Items) != 1 {
				t.Fatalf("expected one fastlet pod, got %d", len(fastlets.Items))
			}
			fastlet := fastlets.Items[0]
			if fastlet.Spec.RuntimeClassName != nil {
				t.Fatalf("fastlet pod must not use Sandbox RuntimeClass, got %q", *fastlet.Spec.RuntimeClassName)
			}
			if got := podEnvValue(fastlet.Spec.Containers[0].Env, "FAST_SANDBOX_RUNTIME"); got != "container" {
				t.Fatalf("FAST_SANDBOX_RUNTIME = %q, want container", got)
			}
			if got := fastlet.Spec.Containers[0].Resources.Requests.Cpu().String(); got != "1350m" {
				t.Fatalf("fastlet CPU request = %q, want overhead + 5 slots = 1350m", got)
			}
			if got := fastlet.Spec.Containers[0].Resources.Requests.Memory().String(); got != "1408Mi" {
				t.Fatalf("fastlet memory request = %q, want overhead + 5 slots = 1408Mi", got)
			}

			sandbox := newSecureRuntimeSandbox(namespace, "sb-container", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			runCtx, cancelRunWait := context.WithTimeout(ctx, 60*time.Second)
			defer cancelRunWait()
			running, err := fixture.WaitForSandbox(runCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}, func(sb *apiv1alpha2.Sandbox) bool {
				return sb.Status.Placement.FastletName != "" && sb.Status.Runtime.State == apiv1alpha2.RuntimeReady
			})
			if err != nil {
				t.Fatalf("wait for running sandbox: %v", err)
			}
			sandboxID := string(running.UID)
			assertSandboxCgroupLimits(ctx, t, fastlet.Spec.NodeName, sandboxID)

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func podEnvValue(env []corev1.EnvVar, name string) string {
	for _, item := range env {
		if item.Name == name {
			return item.Value
		}
	}
	return ""
}

func assertSandboxCgroupLimits(ctx context.Context, t *testing.T, nodeName, sandboxID string) {
	t.Helper()
	output := runDockerExec(ctx, t, nodeName, "ctr", "-n", "k8s.io", "tasks", "list")
	pid := ""
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == sandboxID && fields[2] == "RUNNING" {
			pid = fields[1]
			break
		}
	}
	if pid == "" {
		t.Fatalf("sandbox task %q is not running in containerd task list:\n%s", sandboxID, output)
	}

	cgroupOutput := runDockerExec(ctx, t, nodeName, "cat", fmt.Sprintf("/proc/%s/cgroup", pid))
	cgroupPath := ""
	for _, line := range strings.Split(cgroupOutput, "\n") {
		if strings.HasPrefix(line, "0::") {
			cgroupPath = strings.TrimPrefix(line, "0::")
			break
		}
	}
	if cgroupPath == "" {
		t.Fatalf("cgroup v2 path not found for sandbox task %s: %s", sandboxID, cgroupOutput)
	}

	base := "/sys/fs/cgroup" + cgroupPath
	if got := strings.TrimSpace(runDockerExec(ctx, t, nodeName, "cat", base+"/cpu.max")); got != "25000 100000" {
		t.Fatalf("sandbox cpu.max = %q, want 25000 100000", got)
	}
	if got := strings.TrimSpace(runDockerExec(ctx, t, nodeName, "cat", base+"/memory.max")); got != "268435456" {
		t.Fatalf("sandbox memory.max = %q, want 268435456", got)
	}
	if got := strings.TrimSpace(runDockerExec(ctx, t, nodeName, "cat", base+"/pids.max")); got != "128" {
		t.Fatalf("sandbox pids.max = %q, want 128", got)
	}
}

func runDockerExec(ctx context.Context, t *testing.T, nodeName string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"exec", nodeName}, args...)
	output, err := exec.CommandContext(ctx, "docker", commandArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker exec %s %v failed: %v\n%s", nodeName, args, err, output)
	}
	return string(output)
}
