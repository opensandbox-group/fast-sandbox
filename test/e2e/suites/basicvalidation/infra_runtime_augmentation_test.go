package basicvalidation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	fastpathv1 "fast-sandbox/api/proto/v1"
	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/pkg/sandboxclient"
	e2eenv "fast-sandbox/test/e2e/env"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestSDKAdapterDataPlane(t *testing.T) {
	suiteenv.RequireBasic(t)
	feature := features.New("sdk-adapter-data-plane").
		WithLabel("suite", "basicvalidation").
		WithLabel("tier", "infra").
		Assess("resolves an authenticated route and uses the injected component protocol", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))
			namespace := testSuite.AllocateNamespace("sdk")
			createNamespace(ctx, t, k8sClient, namespace)
			defer suiteenv.DeleteNamespace(context.Background(), t, k8sClient, namespace)

			pool := infraPool(namespace, "sdk-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create SDK adapter Pool: %v", err)
			}
			waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			if _, err := fixture.WaitForReadyFastletPods(waitCtx, types.NamespacedName{Namespace: namespace, Name: pool.Name}, 1); err != nil {
				t.Fatalf("wait for SDK adapter Fastlet Pod: %v", err)
			}

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
			created := createInfraSandbox(ctx, t, fastPath, namespace, pool.Name)
			_ = waitForProxyReady(ctx, t, fixture, namespace, created.SandboxName)

			proxyBase, proxyForward, err := e2eenv.StartSandboxProxyPortForward(ctx, testSuite.ControllerNamespace())
			if err != nil {
				t.Fatalf("start Sandbox Proxy port-forward: %v", err)
			}
			defer proxyForward.Cleanup()
			adapter := &sandboxclient.OpenSandboxExecd{
				Resolver: &sandboxclient.EndpointResolver{Control: fastPath, DefaultNamespace: namespace, ProxyBaseURL: proxyBase},
				Port:     18080,
			}
			execd, _, err := adapter.Client(ctx, sandboxclient.SandboxRef{Name: created.SandboxName, Namespace: namespace})
			if err != nil {
				t.Fatalf("resolve OpenSandbox Execd client: %v", err)
			}
			var stdout strings.Builder
			var exitCode *int
			err = execd.RunCommand(ctx, opensandbox.RunCommandRequest{Command: "printf sdk-exec"}, func(event opensandbox.StreamEvent) error {
				var payload struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					ExitCode *int   `json:"exit_code"`
				}
				if err := json.Unmarshal([]byte(event.Data), &payload); err != nil {
					return err
				}
				if payload.Type == "stdout" {
					stdout.WriteString(payload.Text)
				}
				if payload.Type == "execution_complete" {
					exitCode = payload.ExitCode
				}
				return nil
			})
			if err != nil {
				t.Fatalf("OpenSandbox SDK command: %v", err)
			}
			if stdout.String() != "sdk-exec\n" || exitCode == nil || *exitCode != 0 {
				t.Fatalf("unexpected OpenSandbox SDK execution: stdout=%q exit=%v", stdout.String(), exitCode)
			}
			var downloaded bytes.Buffer
			reader, err := execd.DownloadFile(ctx, "/tmp/value", "")
			if err != nil {
				t.Fatalf("OpenSandbox SDK download: %v", err)
			}
			defer reader.Close()
			if _, err := io.Copy(&downloaded, reader); err != nil {
				t.Fatalf("copy OpenSandbox SDK download: %v", err)
			}
			if downloaded.String() != "sdk-file" {
				t.Fatalf("download = %q, want sdk-file", downloaded.String())
			}
			return ctx
		}).Feature()
	testSuite.Env().Test(t, feature)
}

func TestInfraRuntimeAugmentation(t *testing.T) {
	suiteenv.RequireBasic(t)
	feature := features.New("infra-runtime-augmentation").
		WithLabel("suite", "basicvalidation").
		WithLabel("tier", "infra").
		Assess("injects a protected component and publishes its route only after readiness", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))
			namespace := testSuite.AllocateNamespace("infra")
			createNamespace(ctx, t, k8sClient, namespace)
			defer suiteenv.DeleteNamespace(context.Background(), t, k8sClient, namespace)

			pool := infraPool(namespace, "infra-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create Infra Pool: %v", err)
			}
			waitCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			if _, err := fixture.WaitForReadyFastletPods(waitCtx, types.NamespacedName{Namespace: namespace, Name: pool.Name}, 1); err != nil {
				t.Fatalf("wait for Infra Fastlet Pod: %v", err)
			}

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

			created := createInfraSandbox(ctx, t, fastPath, namespace, pool.Name)
			ready := waitForProxyReady(ctx, t, fixture, namespace, created.SandboxName)
			if ready.Status.DataPlaneState != apiv1alpha1.ObservedStateReady {
				t.Fatalf("Create returned before DataPlaneReady: %s", ready.Status.DataPlaneState)
			}

			proxyBase, proxyForward, err := e2eenv.StartSandboxProxyPortForward(ctx, testSuite.ControllerNamespace())
			if err != nil {
				t.Fatalf("start Sandbox Proxy port-forward: %v", err)
			}
			defer proxyForward.Cleanup()
			infraAccess := resolveProxyAccess(ctx, t, fastPath, created.SandboxUid, 18080)
			assertProxyPathResponse(ctx, t, proxyBase, infraAccess, "/value", "test-infra")
			userAccess := resolveProxyAccess(ctx, t, fastPath, created.SandboxUid, 18081)
			assertProxyPathResponse(ctx, t, proxyBase, userAccess, "/user", "user-started")
			return ctx
		}).Feature()
	testSuite.Env().Test(t, feature)
}

func infraPool(namespace, name string) *apiv1alpha1.SandboxPool {
	return &apiv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: apiv1alpha1.SandboxPoolSpec{
			Capacity: apiv1alpha1.PoolCapacity{PoolMin: 1, PoolMax: 1}, MaxSandboxesPerPod: 1,
			Runtime: apiv1alpha1.RuntimeContainer, InfraProfile: "test-infra",
			SandboxResources: suiteenv.SmallSandboxResourceProfile(),
			FastletTemplate:  corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fastlet", Image: suiteenv.FastletImage()}}}},
		},
	}
}

func createInfraSandbox(ctx context.Context, t *testing.T, fastPath fastpathv1.FastPathServiceClient, namespace, pool string) *fastpathv1.CreateResponse {
	t.Helper()
	command := `cat > /tmp/user-serve <<'EOF'
#!/bin/sh
set -eu
body=user-started
while IFS= read -r line; do
  line="$(printf '%s' "$line" | tr -d '\r')"
  [ -n "$line" ] || break
  case "$line" in
    X-Fast-Sandbox-Infra-Token:\ *) body=credential-leaked ;;
  esac
done
printf 'HTTP/1.1 200 OK\r\nContent-Length: %s\r\nConnection: close\r\n\r\n%s' "${#body}" "$body"
EOF
chmod 0700 /tmp/user-serve
exec nc -lk -p 18081 -e /tmp/user-serve`
	requestID := namespace + "-infra-sandbox"
	request := &fastpathv1.CreateRequest{
		Namespace: namespace, PoolRef: pool, Name: requestID, Image: "docker.io/library/alpine:latest",
		Command: []string{"/bin/sh", "-c", command}, RequestId: requestID,
	}
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		requestContext, cancel := context.WithTimeout(ctx, 30*time.Second)
		response, err := fastPath.CreateSandbox(requestContext, request)
		cancel()
		if err == nil {
			return response
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("CreateSandbox never observed an Infra-ready Fastlet: %v", lastErr)
	return nil
}

func assertProxyPathResponse(ctx context.Context, t *testing.T, proxyBase string, access *fastpathv1.ResolveEndpointResponse, path, want string) {
	t.Helper()
	parsed, err := url.Parse(access.ProxyEndpoint)
	if err != nil {
		t.Fatalf("parse proxy endpoint: %v", err)
	}
	requestURI := strings.TrimSuffix(parsed.RequestURI(), "/") + "/" + strings.TrimPrefix(path, "/")
	deadline := time.Now().Add(20 * time.Second)
	var lastStatus int
	var lastBody string
	for time.Now().Before(deadline) {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, proxyBase+requestURI, nil)
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
	t.Fatalf("proxy path never became ready: status=%d body=%q want=%q endpoint=%s", lastStatus, lastBody, want, fmt.Sprintf("%s%s", proxyBase, requestURI))
}
