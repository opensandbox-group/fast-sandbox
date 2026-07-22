package secureruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"

	fastpathv1 "fast-sandbox/api/proto/v1"
	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	e2eenv "fast-sandbox/test/e2e/env"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestGVisorSandbox(t *testing.T) {
	suiteenv.RequireGVisor(t)

	feature := features.New("gvisor-sandbox").
		WithLabel("suite", "secureruntime").
		WithLabel("runtime", "gvisor").
		Assess("gVisor pool creates sandbox successfully", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
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

			// Wait for ready fastlet pods
			poolWaitCtx, cancelPoolWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelPoolWait()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}
			runtimeWaitCtx, cancelRuntimeWait := context.WithTimeout(ctx, 30*time.Second)
			defer cancelRuntimeWait()
			if _, err := fixture.WaitForPoolCondition(runtimeWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, apiv1alpha1.PoolConditionRuntimeReady, metav1.ConditionTrue); err != nil {
				t.Fatalf("wait for gVisor RuntimeReady: %v", err)
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
				return sb.Status.Assignment != nil && sb.Status.RuntimeState == apiv1alpha1.ObservedStateReady
			})
			if err != nil {
				t.Fatalf("wait for running sandbox: %v", err)
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
	suiteenv.RequireGVisor(t)

	feature := features.New("gvisor-isolation").
		WithLabel("suite", "secureruntime").
		WithLabel("runtime", "gvisor").
		Assess("gVisor sandbox shows proper isolation markers", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
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
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}
			runtimeWaitCtx, cancelRuntimeWait := context.WithTimeout(ctx, 30*time.Second)
			defer cancelRuntimeWait()
			if _, err := fixture.WaitForPoolCondition(runtimeWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, apiv1alpha1.PoolConditionRuntimeReady, metav1.ConditionTrue); err != nil {
				t.Fatalf("wait for gVisor RuntimeReady: %v", err)
			}

			// Keep the workload serving after emitting isolation and DNS markers so
			// the test can validate the Fastlet-owned netns before and after recovery.
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
					Command: []string{"/bin/sh", "-c", `if nslookup kubernetes.default.svc.cluster.local >/dev/null 2>&1; then echo DNS_OK; else echo DNS_FAIL; fi; echo KERNEL=$(uname -r); printf '%s\n' '#!/bin/sh' 'printf "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\ngvisor-ok\n"' > /serve.sh; chmod +x /serve.sh; exec nc -lk -p 18080 -e /serve.sh`},
					PoolRef: pool.Name,
				},
			}
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			runCtx, cancelRunWait := context.WithTimeout(ctx, 60*time.Second)
			defer cancelRunWait()
			createdSandbox, err := fixture.WaitForSandbox(runCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.Assignment != nil && sb.Status.RuntimeState == apiv1alpha1.ObservedStateReady &&
					sb.Status.DataPlaneState == apiv1alpha1.ObservedStateReady
			})
			if err != nil {
				t.Fatalf("wait for running sandbox: %v", err)
			}

			// Get the fastlet pod where the sandbox runs
			fastletPod := &corev1.Pod{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: createdSandbox.Status.Assignment.FastletName, Namespace: namespace}, fastletPod); err != nil {
				t.Fatalf("get fastlet pod: %v", err)
			}

			sandboxID := secureRuntimeSandboxIdentifier(createdSandbox)
			logOutput := waitForSecureRuntimeLog(ctx, t, namespace, fastletPod.Name, sandboxID, "DNS_OK", "KERNEL=")
			hostKernel := strings.TrimSpace(secureRuntimeKubectlOutput(ctx, t, "exec", "-n", namespace, fastletPod.Name, "-c", "fastlet", "--", "uname", "-r"))
			guestKernel := secureRuntimeLogValue(logOutput, "KERNEL=")
			if guestKernel == "" || guestKernel == hostKernel {
				t.Fatalf("gVisor guest kernel was not isolated from the Fastlet host: guest=%q host=%q log=%q", guestKernel, hostKernel, logOutput)
			}
			runtimeInfo := secureRuntimeDockerOutput(ctx, t, "exec", fastletPod.Spec.NodeName, "ctr", "-n", "k8s.io", "containers", "info", sandboxID)
			if !strings.Contains(runtimeInfo, "io.containerd.runsc.v1") {
				t.Fatalf("containerd metadata does not identify the gVisor handler: %s", runtimeInfo)
			}
			state := waitForSecureRuntimeNetworkState(ctx, t, namespace, fastletPod.Name, string(fastletPod.UID), sandboxID)
			waitForSecureRuntimeHTTP(ctx, t, namespace, fastletPod.Name, state.IP, 18080, "gvisor-ok")
			proxyBase, proxyForward, err := e2eenv.StartSandboxProxyPortForward(ctx, testSuite.ControllerNamespace())
			if err != nil {
				t.Fatalf("start Sandbox Proxy port-forward: %v", err)
			}
			defer proxyForward.Cleanup()
			grpcEndpoint, fastPathForward, err := e2eenv.StartControllerPortForward(ctx, testSuite.ControllerNamespace())
			if err != nil {
				t.Fatalf("start FastPath port-forward: %v", err)
			}
			defer fastPathForward.Cleanup()
			dialCtx, cancelDial := context.WithTimeout(ctx, 20*time.Second)
			defer cancelDial()
			connection, err := grpc.DialContext(dialCtx, grpcEndpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
			if err != nil {
				t.Fatalf("dial FastPath: %v", err)
			}
			defer connection.Close()
			access, err := fastpathv1.NewFastPathServiceClient(connection).ResolveEndpoint(ctx, &fastpathv1.ResolveEndpointRequest{
				SandboxUid: string(createdSandbox.UID), TargetPort: 18080, Protocol: "http",
			})
			if err != nil {
				t.Fatalf("resolve gVisor proxy endpoint: %v", err)
			}
			assertSecureRuntimeProxyResponse(ctx, t, proxyBase, access, "gvisor-ok")

			previousRestarts := fastletContainerRestartCount(fastletPod)
			_, _ = secureRuntimeKubectl(ctx, "exec", "-n", namespace, fastletPod.Name, "-c", "fastlet", "--", "kill", "1")
			waitForFastletContainerRestart(ctx, t, k8sClient, namespace, fastletPod.Name, string(fastletPod.UID), previousRestarts)
			waitForSecureRuntimeHTTP(ctx, t, namespace, fastletPod.Name, state.IP, 18080, "gvisor-ok")
			assertSecureRuntimeProxyResponse(ctx, t, proxyBase, access, "gvisor-ok")
			t.Logf("gVisor isolation and recovery verified: guest kernel=%s host kernel=%s private IP=%s", guestKernel, hostKernel, state.IP)

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

type secureRuntimeNetworkState struct {
	IP    string `json:"ip"`
	Owner struct {
		SandboxUID string `json:"sandboxUid"`
	} `json:"owner"`
}

func secureRuntimeSandboxIdentifier(sandbox *apiv1alpha1.Sandbox) string {
	return string(sandbox.UID)
}

func waitForSecureRuntimeLog(ctx context.Context, t *testing.T, namespace, pod, sandboxID string, values ...string) string {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var last string
	for {
		output, err := secureRuntimeKubectl(waitCtx, "exec", "-n", namespace, pod, "-c", "fastlet", "--", "cat", fmt.Sprintf("/var/log/fast-sandbox/%s.log", sandboxID))
		last = string(output)
		matched := err == nil
		for _, value := range values {
			matched = matched && strings.Contains(last, value)
		}
		if matched {
			return last
		}
		select {
		case <-waitCtx.Done():
			t.Fatalf("wait for secure runtime sandbox log containing %v: %v; last=%q", values, waitCtx.Err(), last)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func secureRuntimeLogValue(logOutput, prefix string) string {
	for _, line := range strings.Split(logOutput, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), prefix))
		}
	}
	return ""
}

func waitForSecureRuntimeNetworkState(ctx context.Context, t *testing.T, namespace, pod, podUID, sandboxID string) secureRuntimeNetworkState {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var last map[string]secureRuntimeNetworkState
	for {
		command := fmt.Sprintf(`for f in /run/fast-sandbox/network/%s/*.json; do [ -f "$f" ] && cat "$f" && echo; done`, podUID)
		output, err := secureRuntimeKubectl(waitCtx, "exec", "-n", namespace, pod, "-c", "fastlet", "--", "sh", "-c", command)
		if err == nil {
			last = make(map[string]secureRuntimeNetworkState)
			decoder := json.NewDecoder(bytes.NewReader(output))
			for {
				var state secureRuntimeNetworkState
				if err := decoder.Decode(&state); err != nil {
					break
				}
				last[state.Owner.SandboxUID] = state
			}
			if state, exists := last[sandboxID]; exists && state.IP != "" {
				return state
			}
		}
		select {
		case <-waitCtx.Done():
			t.Fatalf("wait for gVisor private network state: %v; last=%+v", waitCtx.Err(), last)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func waitForSecureRuntimeHTTP(ctx context.Context, t *testing.T, namespace, pod, ip string, port int, want string) string {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var last string
	for {
		output, err := secureRuntimeKubectl(waitCtx, "exec", "-n", namespace, pod, "-c", "fastlet", "--", "wget", "-q", "-O", "-", fmt.Sprintf("http://%s:%d/", ip, port))
		last = string(output)
		if err == nil && strings.Contains(last, want) {
			return last
		}
		select {
		case <-waitCtx.Done():
			t.Fatalf("wait for secure runtime private HTTP endpoint: %v; last=%q", waitCtx.Err(), last)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func assertSecureRuntimeProxyResponse(ctx context.Context, t *testing.T, proxyBase string, access *fastpathv1.ResolveEndpointResponse, want string) {
	t.Helper()
	parsed, err := url.Parse(access.ProxyEndpoint)
	if err != nil {
		t.Fatalf("parse gVisor proxy endpoint: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var lastStatus int
	var lastBody string
	for {
		request, _ := http.NewRequestWithContext(waitCtx, http.MethodGet, proxyBase+parsed.RequestURI(), nil)
		for name, value := range access.RequiredHeaders {
			request.Header.Set(name, value)
		}
		response, requestErr := http.DefaultClient.Do(request)
		if requestErr == nil {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			lastStatus, lastBody = response.StatusCode, string(body)
			if response.StatusCode == http.StatusOK && strings.Contains(lastBody, want) {
				return
			}
		}
		select {
		case <-waitCtx.Done():
			t.Fatalf("wait for gVisor transparent proxy: %v; status=%d body=%q", waitCtx.Err(), lastStatus, lastBody)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func verifySecureRuntimeProxy(ctx context.Context, t *testing.T, sandboxUID string, port uint32, want string) {
	t.Helper()
	proxyBase, proxyForward, err := e2eenv.StartSandboxProxyPortForward(ctx, testSuite.ControllerNamespace())
	if err != nil {
		t.Fatalf("start Sandbox Proxy port-forward: %v", err)
	}
	defer proxyForward.Cleanup()
	grpcEndpoint, fastPathForward, err := e2eenv.StartControllerPortForward(ctx, testSuite.ControllerNamespace())
	if err != nil {
		t.Fatalf("start FastPath port-forward: %v", err)
	}
	defer fastPathForward.Cleanup()
	dialCtx, cancelDial := context.WithTimeout(ctx, 20*time.Second)
	defer cancelDial()
	connection, err := grpc.DialContext(dialCtx, grpcEndpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatalf("dial FastPath: %v", err)
	}
	defer connection.Close()
	access, err := fastpathv1.NewFastPathServiceClient(connection).ResolveEndpoint(ctx, &fastpathv1.ResolveEndpointRequest{
		SandboxUid: sandboxUID, TargetPort: port, Protocol: "http",
	})
	if err != nil {
		t.Fatalf("resolve secure runtime proxy endpoint: %v", err)
	}
	assertSecureRuntimeProxyResponse(ctx, t, proxyBase, access, want)
}

func fastletContainerRestartCount(pod *corev1.Pod) int32 {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "fastlet" {
			return status.RestartCount
		}
	}
	return 0
}

func waitForFastletContainerRestart(ctx context.Context, t *testing.T, kubeClient interface {
	Get(context.Context, types.NamespacedName, client.Object, ...client.GetOption) error
}, namespace, podName, podUID string, previous int32) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for {
		pod := &corev1.Pod{}
		if err := kubeClient.Get(waitCtx, types.NamespacedName{Namespace: namespace, Name: podName}, pod); err == nil && string(pod.UID) == podUID {
			for _, status := range pod.Status.ContainerStatuses {
				if status.Name == "fastlet" && status.RestartCount > previous && status.Ready {
					return
				}
			}
		}
		select {
		case <-waitCtx.Done():
			t.Fatalf("wait for gVisor Fastlet recovery: %v", waitCtx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func secureRuntimeKubectlOutput(ctx context.Context, t *testing.T, args ...string) string {
	t.Helper()
	output, err := secureRuntimeKubectl(ctx, args...)
	if err != nil {
		t.Fatalf("kubectl %v: %v: %s", args, err, output)
	}
	return string(output)
}

func secureRuntimeKubectl(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "kubectl", args...).CombinedOutput()
}

func secureRuntimeDockerOutput(ctx context.Context, t *testing.T, args ...string) string {
	t.Helper()
	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %v: %v: %s", args, err, output)
	}
	return string(output)
}

// TestGVisorMultipleSandboxes tests creating multiple sandboxes in the same pool.
func TestGVisorMultipleSandboxes(t *testing.T) {
	suiteenv.RequireGVisor(t)

	feature := features.New("gvisor-multiple").
		WithLabel("suite", "secureruntime").
		WithLabel("runtime", "gvisor").
		Assess("gVisor pool handles multiple sandboxes", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
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
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
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
					return sb.Status.Assignment != nil && sb.Status.RuntimeState == apiv1alpha1.ObservedStateReady
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

func newSecureRuntimePool(namespace, name string, runtimeName apiv1alpha1.RuntimeName, min, max int32) *apiv1alpha1.SandboxPool {
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
			Runtime:            runtimeName,
			SandboxResources: apiv1alpha1.SandboxResourceProfile{
				CPU: resource.MustParse("250m"), Memory: resource.MustParse("256Mi"), PIDs: 128,
			},
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
