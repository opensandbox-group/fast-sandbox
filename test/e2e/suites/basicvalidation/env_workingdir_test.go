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

	fastpathv1 "fast-sandbox/api/proto/v1"
	apiv1alpha1 "fast-sandbox/api/v1alpha1"
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

			envSandbox := &apiv1alpha1.Sandbox{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1alpha1.GroupVersion.String(),
					Kind:       "Sandbox",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sb-env-test",
					Namespace: namespace,
				},
				Spec: apiv1alpha1.SandboxSpec{
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
			envLog := waitForSandboxLog(ctx, t, namespace, assignedEnvSandbox.Status.AssignedFastlet, sandboxIdentifier(assignedEnvSandbox),
				"TEST_VAR=test_value_123",
				"ANOTHER_VAR=another_value_456",
			)
			if !strings.Contains(envLog, "TEST_VAR=test_value_123") || !strings.Contains(envLog, "ANOTHER_VAR=another_value_456") {
				t.Fatalf("unexpected env sandbox log: %q", envLog)
			}

			workdirSandbox := &apiv1alpha1.Sandbox{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1alpha1.GroupVersion.String(),
					Kind:       "Sandbox",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sb-workdir-test",
					Namespace: namespace,
				},
				Spec: apiv1alpha1.SandboxSpec{
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
			workdirLog := waitForSandboxLog(ctx, t, namespace, assignedWorkdirSandbox.Status.AssignedFastlet, sandboxIdentifier(assignedWorkdirSandbox), "PWD=/tmp")
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

			// Wait for fastlet capacity to sync to controller registry
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

			client := fastpathv1.NewFastPathServiceClient(conn)
			createCtx, cancelCreate := context.WithTimeout(ctx, 30*time.Second)
			defer cancelCreate()
			resp, err := client.CreateSandbox(createCtx, &fastpathv1.CreateRequest{
				Name:            "sb-fastpath-env",
				Image:           "docker.io/library/alpine:latest",
				PoolRef:         pool.Name,
				Namespace:       namespace,
				ConsistencyMode: fastpathv1.ConsistencyMode_FAST,
				Command:         []string{"/bin/sh", "-c", `echo "FASTPATH_VAR=$FASTPATH_VAR"; echo "PWD=$(pwd)"; sleep 3600`},
				WorkingDir:      "/app",
				Envs: map[string]string{
					"FASTPATH_VAR": "hello_from_fastpath",
				},
			})
			if err != nil {
				t.Fatalf("create fast-path sandbox: %v", err)
			}
			if resp.FastletPod == "" {
				t.Fatalf("create fast-path sandbox returned empty fastlet pod")
			}
			if resp.SandboxId == "" {
				t.Fatalf("create fast-path sandbox returned empty sandbox ID")
			}

			waitCtx, cancelWait := context.WithTimeout(ctx, 30*time.Second)
			defer cancelWait()
			if _, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Name: resp.SandboxName, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.AssignedFastlet != "" || string(sb.UID) != ""
			}); err != nil {
				t.Fatalf("wait for fast-path sandbox CRD: %v", err)
			}

			fastpathLog := waitForSandboxLog(ctx, t, namespace, resp.FastletPod, resp.SandboxId,
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

func createValidationPool(namespace, name string) *apiv1alpha1.SandboxPool {
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
				PoolMin: 1,
				PoolMax: 10, // Increased for parallel tests
			},
			MaxSandboxesPerPod: 20, // Increased capacity
			Runtime:            apiv1alpha1.RuntimeContainer,
			SandboxResources: apiv1alpha1.SandboxResourceProfile{
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

func waitForAssignedSandbox(ctx context.Context, t *testing.T, fixture *fixtures.FixtureClient, namespace, name string) *apiv1alpha1.Sandbox {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 90*time.Second) // Increased from 60s to 90s
	defer cancel()

	sandbox, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Name: name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
		return sb.Status.AssignedFastlet != "" &&
			(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
	})
	if err != nil {
		t.Fatalf("wait for assigned sandbox %s/%s: %v", namespace, name, err)
	}
	return sandbox
}

func sandboxIdentifier(sandbox *apiv1alpha1.Sandbox) string {
	if sandbox == nil {
		return ""
	}
	if sandbox.Status.SandboxID != "" {
		return sandbox.Status.SandboxID
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
