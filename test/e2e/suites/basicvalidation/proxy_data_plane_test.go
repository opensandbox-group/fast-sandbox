package basicvalidation

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

func TestSandboxProxyDataPlane(t *testing.T) {
	suiteenv.RequireBasic(t)
	feature := features.New("sandbox-proxy-data-plane").
		WithLabel("suite", "basicvalidation").
		WithLabel("tier", "proxy").
		Assess("routes same and arbitrary ports only to the assigned Fastlet with generation fencing", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))
			namespace := testSuite.AllocateNamespace("proxy")
			createNamespace(ctx, t, k8sClient, namespace)
			defer suiteenv.DeleteNamespace(context.Background(), t, k8sClient, namespace)

			pool := proxyPool(namespace, "proxy-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create proxy pool: %v", err)
			}
			waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			if _, err := fixture.WaitForReadyFastletPods(waitCtx, types.NamespacedName{Namespace: namespace, Name: pool.Name}, 2); err != nil {
				t.Fatalf("wait for two proxy-enabled Fastlets: %v", err)
			}
			assertFastletProxySidecars(ctx, t, k8sClient, namespace, pool.Name)

			grpcEndpoint, fastPathForward, err := e2eenv.StartControllerPortForward(ctx, testSuite.ControllerNamespace())
			if err != nil {
				t.Fatalf("start FastPath port-forward: %v", err)
			}
			defer fastPathForward.Cleanup()
			dialContext, dialCancel := context.WithTimeout(ctx, 20*time.Second)
			defer dialCancel()
			connection, err := grpc.DialContext(dialContext, grpcEndpoint, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
			if err != nil {
				t.Fatalf("dial FastPath: %v", err)
			}
			defer connection.Close()
			fastPath := fastpathv1.NewFastPathServiceClient(connection)

			first := createProxySandbox(ctx, t, fastPath, namespace, pool.Name, "proxy-a", 8080)
			second := createProxySandbox(ctx, t, fastPath, namespace, pool.Name, "proxy-b", 18080)
			firstSandbox := waitForProxyReady(ctx, t, fixture, namespace, first.SandboxName)
			secondSandbox := waitForProxyReady(ctx, t, fixture, namespace, second.SandboxName)
			if firstSandbox.Status.Assignment.FastletPodUID == secondSandbox.Status.Assignment.FastletPodUID {
				t.Fatalf("capacity-one Pool did not place Sandboxes on distinct Fastlet Pods")
			}

			proxyBase, proxyForward, err := e2eenv.StartSandboxProxyPortForward(ctx, testSuite.ControllerNamespace())
			if err != nil {
				t.Fatalf("start Sandbox Proxy port-forward: %v", err)
			}
			defer proxyForward.Cleanup()
			firstAccess := resolveProxyAccess(ctx, t, fastPath, first.SandboxUid, 8080)
			secondAccess := resolveProxyAccess(ctx, t, fastPath, second.SandboxUid, 18080)
			assertEverySandboxProxyReplica(ctx, t, k8sClient, firstAccess, "proxy-a")
			assertProxyResponse(ctx, t, proxyBase, firstAccess, "proxy-a")
			assertProxyResponse(ctx, t, proxyBase, secondAccess, "proxy-b")
			restartFastletProxy(ctx, t, k8sClient, namespace, firstSandbox.Status.Assignment.FastletName)
			assertProxyResponse(ctx, t, proxyBase, firstAccess, "proxy-a")

			oldGeneration := firstSandbox.Status.RouteGeneration
			before := firstSandbox.DeepCopy()
			firstSandbox.Spec.ResetRevision = &metav1.Time{Time: time.Now().UTC()}
			if err := k8sClient.Patch(ctx, firstSandbox, client.MergeFrom(before)); err != nil {
				t.Fatalf("reset first Sandbox: %v", err)
			}
			reset := waitForProxyGeneration(ctx, t, fixture, namespace, firstSandbox.Name, oldGeneration)
			assertProxyRejected(ctx, t, proxyBase, firstAccess)
			newAccess := resolveProxyAccess(ctx, t, fastPath, string(reset.UID), 8080)
			assertProxyResponse(ctx, t, proxyBase, newAccess, "proxy-a")

			if err := k8sClient.Delete(ctx, reset); err != nil {
				t.Fatalf("delete reset Sandbox: %v", err)
			}
			deleteWait, deleteCancel := context.WithTimeout(ctx, 90*time.Second)
			defer deleteCancel()
			if err := fixture.WaitForSandboxDeleted(deleteWait, types.NamespacedName{Namespace: namespace, Name: reset.Name}); err != nil {
				t.Fatalf("wait for Sandbox deletion: %v", err)
			}
			assertProxyRejected(ctx, t, proxyBase, newAccess)
			return ctx
		}).Feature()
	testSuite.Env().Test(t, feature)
}

func proxyPool(namespace, name string) *apiv1alpha1.SandboxPool {
	return &apiv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: apiv1alpha1.SandboxPoolSpec{
			Capacity: apiv1alpha1.PoolCapacity{PoolMin: 2, PoolMax: 2}, MaxSandboxesPerPod: 1,
			Runtime:          apiv1alpha1.RuntimeContainer,
			SandboxResources: apiv1alpha1.SandboxResourceProfile{CPU: resource.MustParse("50m"), Memory: resource.MustParse("64Mi"), PIDs: 64},
			FastletTemplate:  corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fastlet", Image: suiteenv.FastletImage()}}}},
		},
	}
}

func createProxySandbox(ctx context.Context, t *testing.T, fastPath fastpathv1.FastPathServiceClient, namespace, pool, name string, port int) *fastpathv1.CreateResponse {
	t.Helper()
	command := fmt.Sprintf(`printf '%%s\n' '#!/bin/sh' 'printf "HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n%s\n"' > /serve.sh; chmod +x /serve.sh; exec nc -lk -p %d -e /serve.sh`, name, port)
	requestContext, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	response, err := fastPath.CreateSandbox(requestContext, &fastpathv1.CreateRequest{
		Namespace: namespace, PoolRef: pool, Name: name, Image: "docker.io/library/alpine:latest",
		Command: []string{"/bin/sh", "-c", command}, RequestId: namespace + "-" + name,
	})
	if err != nil {
		t.Fatalf("CreateSandbox %s: %v", name, err)
	}
	return response
}

func waitForProxyReady(ctx context.Context, t *testing.T, fixture *fixtures.FixtureClient, namespace, name string) *apiv1alpha1.Sandbox {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	sandbox, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Namespace: namespace, Name: name}, func(item *apiv1alpha1.Sandbox) bool {
		return item.Status.Assignment != nil && item.Status.RouteGeneration > 0 &&
			item.Status.RuntimeState == apiv1alpha1.ObservedStateReady && item.Status.DataPlaneState == apiv1alpha1.ObservedStateReady
	})
	if err != nil {
		t.Fatalf("wait for proxy-ready Sandbox %s: %v", name, err)
	}
	return sandbox
}

func waitForProxyGeneration(ctx context.Context, t *testing.T, fixture *fixtures.FixtureClient, namespace, name string, previous int64) *apiv1alpha1.Sandbox {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	sandbox, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Namespace: namespace, Name: name}, func(item *apiv1alpha1.Sandbox) bool {
		return item.Status.RouteGeneration > previous && item.Status.DataPlaneState == apiv1alpha1.ObservedStateReady
	})
	if err != nil {
		t.Fatalf("wait for new route generation: %v", err)
	}
	return sandbox
}

func resolveProxyAccess(ctx context.Context, t *testing.T, fastPath fastpathv1.FastPathServiceClient, sandboxUID string, port uint32) *fastpathv1.ResolveEndpointResponse {
	t.Helper()
	response, err := fastPath.ResolveEndpoint(ctx, &fastpathv1.ResolveEndpointRequest{SandboxUid: sandboxUID, TargetPort: port, Protocol: "http"})
	if err != nil {
		t.Fatalf("ResolveEndpoint %s:%d: %v", sandboxUID, port, err)
	}
	return response
}

func assertProxyResponse(ctx context.Context, t *testing.T, proxyBase string, access *fastpathv1.ResolveEndpointResponse, want string) {
	t.Helper()
	parsed, err := url.Parse(access.ProxyEndpoint)
	if err != nil {
		t.Fatalf("parse proxy endpoint: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	var lastStatus int
	var lastBody string
	for time.Now().Before(deadline) {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, proxyBase+parsed.RequestURI(), nil)
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
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("proxy response never became ready: status=%d body=%q want=%q", lastStatus, lastBody, want)
}

func assertProxyRejected(ctx context.Context, t *testing.T, proxyBase string, access *fastpathv1.ResolveEndpointResponse) {
	t.Helper()
	parsed, _ := url.Parse(access.ProxyEndpoint)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, proxyBase+parsed.RequestURI(), nil)
		for name, value := range access.RequiredHeaders {
			request.Header.Set(name, value)
		}
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusServiceUnavailable {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("stale route credential continued to access Sandbox")
}

func assertFastletProxySidecars(ctx context.Context, t *testing.T, k8sClient client.Client, namespace, pool string) {
	t.Helper()
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"fast-sandbox.io/pool": pool}); err != nil {
		t.Fatalf("list Fastlet Pods: %v", err)
	}
	if len(pods.Items) != 2 {
		t.Fatalf("expected two Fastlet Pods, got %d", len(pods.Items))
	}
	for _, pod := range pods.Items {
		found := false
		for _, container := range pod.Spec.Containers {
			found = found || container.Name == "fastlet-proxy"
		}
		if !found {
			t.Fatalf("Fastlet Pod %s has no platform-owned fastlet-proxy sidecar", pod.Name)
		}
	}
}

func assertEverySandboxProxyReplica(ctx context.Context, t *testing.T, k8sClient client.Client, access *fastpathv1.ResolveEndpointResponse, want string) {
	t.Helper()
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, client.InNamespace(testSuite.ControllerNamespace()), client.MatchingLabels{"app": "fast-sandbox-proxy"}); err != nil {
		t.Fatalf("list Sandbox Proxy replicas: %v", err)
	}
	if len(pods.Items) < 2 {
		t.Fatalf("expected at least two Sandbox Proxy replicas, got %d", len(pods.Items))
	}
	for _, pod := range pods.Items {
		base, forward, err := e2eenv.StartPodPortForward(ctx, pod.Namespace, pod.Name, 8080)
		if err != nil {
			t.Fatalf("port-forward Sandbox Proxy %s: %v", pod.Name, err)
		}
		assertProxyResponse(ctx, t, base, access, want)
		if err := forward.Cleanup(); err != nil {
			t.Errorf("cleanup Sandbox Proxy %s port-forward: %v", pod.Name, err)
		}
	}
}

func restartFastletProxy(ctx context.Context, t *testing.T, k8sClient client.Client, namespace, podName string) {
	t.Helper()
	previous := containerRestartCount(ctx, t, k8sClient, namespace, podName, "fastlet-proxy")
	if output, err := kubectl(ctx, "exec", "-n", namespace, podName, "-c", "fastlet-proxy", "--", "kill", "1"); err != nil {
		t.Fatalf("restart Fastlet Proxy: %v: %s", err, output)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for {
		var pod corev1.Pod
		if err := k8sClient.Get(waitCtx, types.NamespacedName{Namespace: namespace, Name: podName}, &pod); err == nil {
			for _, status := range pod.Status.ContainerStatuses {
				if status.Name == "fastlet-proxy" && status.RestartCount > previous && status.Ready {
					return
				}
			}
		}
		select {
		case <-waitCtx.Done():
			t.Fatalf("wait for Fastlet Proxy restart: %v", waitCtx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func containerRestartCount(ctx context.Context, t *testing.T, k8sClient client.Client, namespace, podName, containerName string) int32 {
	t.Helper()
	var pod corev1.Pod
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: podName}, &pod); err != nil {
		t.Fatalf("get Fastlet Pod: %v", err)
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == containerName {
			return status.RestartCount
		}
	}
	t.Fatalf("container %s not found in Pod %s", containerName, podName)
	return 0
}
