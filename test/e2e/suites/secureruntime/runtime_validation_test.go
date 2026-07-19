package secureruntime

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestRuntimeValidationUnsupportedBoxLite(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("boxlite-sidecar-capability-gate").
		WithLabel("suite", "secureruntime").
		WithLabel("tier", "validation").
		Assess("BoxLite remains fail closed until required resource limits are enforced", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("unsupported-runtime")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			// BoxLite has an independent pure-Go RuntimeDriver client, but remains
			// unsupported until the Sidecar can enforce the full resource contract.
			pool := &apiv1alpha1.SandboxPool{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1alpha1.GroupVersion.String(),
					Kind:       "SandboxPool",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "unsupported-boxlite-pool",
					Namespace: namespace,
				},
				Spec: apiv1alpha1.SandboxPoolSpec{
					Capacity: apiv1alpha1.PoolCapacity{
						PoolMin: 1,
						PoolMax: 1,
					},
					MaxSandboxesPerPod: 5,
					Runtime:            apiv1alpha1.RuntimeBoxLite,
					FastletTemplate: corev1.PodTemplateSpec{
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
								Name:  "fastlet",
								Image: suiteenv.FastletImage(),
							}},
						},
					},
				},
			}

			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create pool: %v", err)
			}

			// Wait for condition to be updated
			conditionCtx, cancelCondition := context.WithTimeout(ctx, 30*time.Second)
			defer cancelCondition()

			var runtimeReady *metav1.Condition
			for {
				updatedPool := &apiv1alpha1.SandboxPool{}
				if err := k8sClient.Get(conditionCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, updatedPool); err != nil {
					t.Fatalf("get pool: %v", err)
				}

				for _, c := range updatedPool.Status.Conditions {
					if c.Type == apiv1alpha1.PoolConditionRuntimeReady {
						runtimeReady = &c
						break
					}
				}

				if runtimeReady != nil || conditionCtx.Err() != nil {
					break
				}

				time.Sleep(500 * time.Millisecond)
			}

			if runtimeReady == nil {
				t.Fatal("expected RuntimeReady condition to be set")
			}
			if runtimeReady.Status != metav1.ConditionFalse {
				t.Errorf("expected RuntimeReady condition to be False, got: %v", runtimeReady.Status)
			}
			if runtimeReady.Reason != apiv1alpha1.ReasonRuntimeUnsupported {
				t.Errorf("expected Reason to be RuntimeUnsupported, got: %v", runtimeReady.Reason)
			}
			if runtimeReady.Message != "BoxLiteResourceEnforcementIncomplete" {
				t.Errorf("unexpected BoxLite capability message: %q", runtimeReady.Message)
			}

			t.Logf("Pool condition correctly shows error: %s", runtimeReady.Message)

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
			pool := newSecureRuntimePool(namespace, "container-pool", apiv1alpha1.RuntimeContainer, 1, 1)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create container pool: %v", err)
			}

			// Wait for ready fastlet pods
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

			// Create sandbox
			sandbox := newSecureRuntimeSandbox(namespace, "sb-container", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			// Wait for sandbox running
			runCtx, cancelRunWait := context.WithTimeout(ctx, 60*time.Second)
			defer cancelRunWait()
			running, err := fixture.WaitForSandbox(runCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.AssignedFastlet != "" &&
					(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
			})
			if err != nil {
				t.Fatalf("wait for running sandbox: %v", err)
			}
			sandboxID := running.Status.SandboxID
			if sandboxID == "" {
				sandboxID = string(running.UID)
			}
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
