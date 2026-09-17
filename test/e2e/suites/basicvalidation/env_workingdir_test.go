package basicvalidation

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	fastpathv2 "fast-sandbox/api/proto/v2"
	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/portforward"
	"fast-sandbox/test/e2e/support/suiteenv"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestSandboxEnvAndWorkingDir(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("sandbox-env-working-dir").
		WithLabel("suite", "basicvalidation").
		WithLabel("tier", "smoke").
		Assess("CRD-created sandboxes receive envs and working directory", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("env")
			createNamespace(ctx, t, k8sClient, namespace)
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := createValidationPool(namespace, "env-workingdir-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}
			waitForPoolReady(ctx, t, fixture, namespace, pool.Name)

			envSandbox := &apiv1alpha2.Sandbox{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1alpha2.GroupVersion.String(),
					Kind:       "Sandbox",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sb-env-test",
					Namespace: namespace,
				},
				Spec: apiv1alpha2.SandboxSpec{
					Image:   "docker.io/library/alpine:latest",
					Command: []string{"/bin/sh", "-c", `echo "TEST_VAR=$TEST_VAR"; echo "ANOTHER_VAR=$ANOTHER_VAR"; sleep 3600`},
					PoolRef: pool.Name,
					Envs: []corev1.EnvVar{
						{Name: "TEST_VAR", Value: "test_value_123"},
						{Name: "ANOTHER_VAR", Value: "another_value_456"},
					},
				},
			}
			if _, err := fixture.CreateSandbox(ctx, namespace, envSandbox); err != nil {
				t.Fatalf("create env sandbox: %v", err)
			}

			assignedEnvSandbox := waitForAssignedSandbox(ctx, t, fixture, namespace, envSandbox.Name)
			envLog := waitForSandboxLog(ctx, t, namespace, assignedEnvSandbox.Status.Placement.FastletName, sandboxIdentifier(assignedEnvSandbox),
				"TEST_VAR=test_value_123",
				"ANOTHER_VAR=another_value_456",
			)
			if !strings.Contains(envLog, "TEST_VAR=test_value_123") || !strings.Contains(envLog, "ANOTHER_VAR=another_value_456") {
				t.Fatalf("unexpected env sandbox log: %q", envLog)
			}

			workdirSandbox := &apiv1alpha2.Sandbox{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1alpha2.GroupVersion.String(),
					Kind:       "Sandbox",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sb-workdir-test",
					Namespace: namespace,
				},
				Spec: apiv1alpha2.SandboxSpec{
					Image:      "docker.io/library/alpine:latest",
					Command:    []string{"/bin/sh", "-c", `echo "PWD=$(pwd)"; sleep 3600`},
					WorkingDir: "/tmp",
					PoolRef:    pool.Name,
				},
			}
			if _, err := fixture.CreateSandbox(ctx, namespace, workdirSandbox); err != nil {
				t.Fatalf("create working-dir sandbox: %v", err)
			}

			assignedWorkdirSandbox := waitForAssignedSandbox(ctx, t, fixture, namespace, workdirSandbox.Name)
			workdirLog := waitForSandboxLog(ctx, t, namespace, assignedWorkdirSandbox.Status.Placement.FastletName, sandboxIdentifier(assignedWorkdirSandbox), "PWD=/tmp")
			if !strings.Contains(workdirLog, "PWD=/tmp") {
				t.Fatalf("unexpected working-dir sandbox log: %q", workdirLog)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestFastPathEnvAndWorkingDir(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("fastpath-env-working-dir").
		WithLabel("suite", "basicvalidation").
		WithLabel("tier", "smoke").
		Assess("FastPath-created sandboxes receive envs and working directory", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("fastpath")
			createNamespace(ctx, t, k8sClient, namespace)
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := createValidationPool(namespace, "fastpath-env-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}
			waitForPoolReady(ctx, t, fixture, namespace, pool.Name)

			// Fastlet control loop runs every 2s, give it time to register capacity
			t.Log("Waiting for fastlet capacity to sync...")
			time.Sleep(5 * time.Second)

			controllerNamespace := discoverFastPathNamespace(ctx, t, k8sClient)
			controllerPort := reserveLocalPort(t)
			pfCmd := exec.CommandContext(ctx, "kubectl", "port-forward", "service/fast-sandbox-fastpath", fmt.Sprintf("%d:9090", controllerPort), "-n", controllerNamespace)
			var pfStdout, pfStderr bytes.Buffer
			pfCmd.Stdout = &pfStdout
			pfCmd.Stderr = &pfStderr
			if err := pfCmd.Start(); err != nil {
				t.Fatalf("start controller port-forward: %v", err)
			}
			defer func() {
				if err := (portforward.ManagedProcess{Cmd: pfCmd}).Cleanup(); err != nil {
					t.Fatalf("cleanup controller port-forward: %v", err)
				}
			}()

			readyCtx, cancelReady := context.WithTimeout(ctx, 15*time.Second)
			defer cancelReady()
			if err := portforward.WaitForReady(readyCtx, fmt.Sprintf("127.0.0.1:%d", controllerPort), 100*time.Millisecond); err != nil {
				t.Fatalf("wait for controller port-forward: %v (stdout=%q stderr=%q)", err, pfStdout.String(), pfStderr.String())
			}

			dialCtx, cancelDial := context.WithTimeout(ctx, 10*time.Second)
			defer cancelDial()
			conn, err := grpc.DialContext(dialCtx,
				fmt.Sprintf("127.0.0.1:%d", controllerPort),
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithBlock(),
			)
			if err != nil {
				t.Fatalf("dial fast-path controller: %v", err)
			}
			defer conn.Close()

			client := fastpathv2.NewFastPathServiceClient(conn)
			createCtx, cancelCreate := context.WithTimeout(ctx, 30*time.Second)
			defer cancelCreate()
			requestID := namespace + "-fastpath-env"
			resp, err := client.CreateSandbox(createCtx, &fastpathv2.CreateSandboxRequest{
				Image:      "docker.io/library/alpine:latest",
				PoolRef:    pool.Name,
				Namespace:  namespace,
				RequestId:  requestID,
				Command:    []string{"/bin/sh", "-c", `echo "FASTPATH_VAR=$FASTPATH_VAR"; echo "PWD=$(pwd)"; sleep 3600`},
				WorkingDir: "/app",
				Envs: map[string]string{
					"FASTPATH_VAR": "hello_from_fastpath",
				},
			})
			if err != nil {
				t.Fatalf("create fast-path sandbox: %v", err)
			}
			if resp.GetSandbox().GetIdentity().GetUid() == "" {
				t.Fatalf("create fast-path sandbox returned empty sandbox UID")
			}

			waitCtx, cancelWait := context.WithTimeout(ctx, 30*time.Second)
			defer cancelWait()
			readySandbox, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Name: resp.GetSandbox().GetIdentity().GetName(), Namespace: namespace}, func(sb *apiv1alpha2.Sandbox) bool {
				return sb.Status.Placement.FastletName != "" && sb.Status.Runtime.State == apiv1alpha2.RuntimeReady
			})
			if err != nil {
				t.Fatalf("wait for fast-path sandbox CRD: %v", err)
			}

			fastpathLog := waitForSandboxLog(ctx, t, namespace, readySandbox.Status.Placement.FastletName, resp.GetSandbox().GetIdentity().GetUid(),
				"FASTPATH_VAR=hello_from_fastpath",
				"PWD=/app",
			)
			if !strings.Contains(fastpathLog, "FASTPATH_VAR=hello_from_fastpath") || !strings.Contains(fastpathLog, "PWD=/app") {
				t.Fatalf("unexpected fast-path sandbox log: %q", fastpathLog)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func createNamespace(ctx context.Context, t *testing.T, kubeClient ctrlclient.Client, namespace string) {
	t.Helper()
	if err := kubeClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatalf("create namespace %s: %v", namespace, err)
	}
}

func createValidationPool(namespace, name string) *apiv1alpha2.SandboxPool {
	return &apiv1alpha2.SandboxPool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1alpha2.GroupVersion.String(),
			Kind:       "SandboxPool",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: apiv1alpha2.SandboxPoolSpec{
			Capacity: apiv1alpha2.PoolCapacity{
				PoolMin: 1,
				PoolMax: 10,
			},
			MaxSandboxesPerPod: 20,
			Runtime:            apiv1alpha2.RuntimeContainer,
			SandboxResources: apiv1alpha2.SandboxResourceProfile{
				CPU: resource.MustParse("100m"), Memory: resource.MustParse("64Mi"), PIDs: 64,
			},
			FastletTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "fastlet",
						Image: suiteenv.FastletImage(),
					}},
				},
			},
		},
	}
}

func waitForPoolReady(ctx context.Context, t *testing.T, fixture *fixtures.FixtureClient, namespace, name string) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	if _, err := fixture.WaitForReadyFastletPods(waitCtx, types.NamespacedName{Name: name, Namespace: namespace}, 1); err != nil {
		t.Fatalf("wait for ready fastlet pods for pool %s/%s: %v", namespace, name, err)
	}
}

func waitForAssignedSandbox(ctx context.Context, t *testing.T, fixture *fixtures.FixtureClient, namespace, name string) *apiv1alpha2.Sandbox {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	sandbox, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Name: name, Namespace: namespace}, func(sb *apiv1alpha2.Sandbox) bool {
		return sb.Status.Placement.FastletName != "" && sb.Status.Runtime.State == apiv1alpha2.RuntimeReady
	})
	if err != nil {
		t.Fatalf("wait for assigned sandbox %s/%s: %v", namespace, name, err)
	}
	return sandbox
}

func sandboxIdentifier(sandbox *apiv1alpha2.Sandbox) string {
	if sandbox == nil {
		return ""
	}
	return string(sandbox.UID)
}

func waitForSandboxLog(ctx context.Context, t *testing.T, namespace, fastletPod, sandboxID string, want ...string) string {
	t.Helper()

	waitCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	var lastLog string
	for {
		logOutput, err := readSandboxLog(waitCtx, namespace, fastletPod, sandboxID)
		if err == nil {
			lastLog = logOutput
			matched := true
			for _, item := range want {
				if !strings.Contains(logOutput, item) {
					matched = false
					break
				}
			}
			if matched {
				return logOutput
			}
		}

		select {
		case <-waitCtx.Done():
			t.Fatalf("wait for sandbox log %s/%s id=%s containing %v: %v; last log=%q", namespace, fastletPod, sandboxID, want, waitCtx.Err(), lastLog)
		case <-ticker.C:
		}
	}
}

func readSandboxLog(ctx context.Context, namespace, fastletPod, sandboxID string) (string, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "exec", "-n", namespace, fastletPod, "--", "cat", fmt.Sprintf("/var/log/fast-sandbox/%s.log", sandboxID))
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func discoverFastPathNamespace(ctx context.Context, t *testing.T, kubeClient ctrlclient.Client) string {
	t.Helper()

	deployments := &appsv1.DeploymentList{}
	if err := kubeClient.List(ctx, deployments); err != nil {
		t.Fatalf("list deployments: %v", err)
	}
	for _, deployment := range deployments.Items {
		if deployment.Name == "fast-sandbox-fastpath" {
			return deployment.Namespace
		}
	}
	t.Fatalf("could not find deployment fast-sandbox-fastpath")
	return ""
}

func reserveLocalPort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve local port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}
