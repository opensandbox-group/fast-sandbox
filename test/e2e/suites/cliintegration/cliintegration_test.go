package cliintegration

import (
	"context"
	"strings"
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

func TestUpdateReset(t *testing.T) {
	manager := e2eenv.Require(t, e2eenv.ProfileBasic)
	cliBinaryPath := buildFSBCtl(t, manager)

	feature := features.New("cli-update-reset").
		WithLabel("suite", "cliintegration").
		WithLabel("tier", "smoke").
		Assess("fsb-ctl update and reset commands work correctly", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("cliupdate")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			// Create pool
			pool := createCLIPool(namespace, "update-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if _, err := fixture.WaitForReadyAgentPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready agent pods: %v", err)
			}

			// Wait for agent capacity to sync to controller registry
			// Agent control loop runs every 2s, give it time to register capacity
			t.Log("Waiting for agent capacity to sync...")
			time.Sleep(8 * time.Second)

			// Start port-forward to controller
			ctrlNS := testSuite.ControllerNamespace()
			endpoint, pf, err := e2eenv.StartControllerPortForward(ctx, ctrlNS)
			if err != nil {
				t.Fatalf("start controller port-forward: %v", err)
			}
			defer pf.Cleanup()
			t.Logf("Controller port-forward established on %s", endpoint)

			ctl := e2eenv.NewFSBCtl(
				e2eenv.WithFSBCtlBinary(cliBinaryPath),
				e2eenv.WithFSBCtlEndpoint(endpoint),
				e2eenv.WithFSBCtlNamespace(namespace),
			)

			t.Log("Creating sandbox through fsb-ctl run...")
			if output, err := ctl.Run(ctx, "sb-update-test", e2eenv.FSBCtlConfig{
				Image:           "docker.io/library/alpine:latest",
				PoolRef:         pool.Name,
				ConsistencyMode: "strong",
				Command:         []string{"/bin/sleep"},
				Args:            []string{"3600"},
			}); err != nil {
				t.Fatalf("fsb-ctl run failed: %v\noutput: %s", err, output)
			}

			waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			if _, err := ctl.WaitRunning(waitCtx, "sb-update-test"); err != nil {
				t.Fatalf("wait for sandbox running via fsb-ctl: %v", err)
			}

			// Test 1: fsb-ctl get command
			t.Log("Testing fsb-ctl get command...")
			info, err := ctl.GetJSON(ctx, "sb-update-test")
			if err != nil {
				t.Fatalf("fsb-ctl get failed: %v", err)
			}
			if info.Phase == "" {
				t.Fatalf("fsb-ctl get output missing phase: %+v", info)
			}
			t.Log("✓ fsb-ctl get command works")

			// Test 2: fsb-ctl update --labels
			t.Log("Testing fsb-ctl update --labels...")
			output, err := ctl.UpdateLabels(ctx, "sb-update-test", "test=e2e", "env=cli")
			if err != nil || !strings.Contains(string(output), "updated successfully") {
				t.Fatalf("fsb-ctl update labels failed: %v\noutput: %s", err, output)
			}
			t.Log("✓ fsb-ctl update --labels works")

			// Test 3: fsb-ctl reset command
			t.Log("Testing fsb-ctl reset command...")
			output, err = ctl.Reset(ctx, "sb-update-test")
			if err != nil || !strings.Contains(string(output), "reset triggered") {
				t.Fatalf("fsb-ctl reset failed: %v\noutput: %s", err, output)
			}
			t.Log("✓ fsb-ctl reset command works")

			resetWaitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if _, err := fixture.WaitForSandbox(resetWaitCtx, types.NamespacedName{Name: "sb-update-test", Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Spec.ResetRevision != nil
			}); err != nil {
				t.Fatalf("wait for reset revision: %v", err)
			}
			t.Log("✓ ResetRevision was set correctly")

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestCLILogs(t *testing.T) {
	manager := e2eenv.Require(t, e2eenv.ProfileBasic)
	cliBinaryPath := buildFSBCtl(t, manager)

	feature := features.New("cli-logs").
		WithLabel("suite", "cliintegration").
		WithLabel("tier", "smoke").
		Assess("fsb-ctl logs command retrieves sandbox logs", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("clilogs")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			// Create pool
			pool := createCLIPool(namespace, "logs-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if _, err := fixture.WaitForReadyAgentPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready agent pods: %v", err)
			}

			// Wait for agent capacity to sync to controller registry
			// Agent control loop runs every 2s, give it time to register capacity
			t.Log("Waiting for agent capacity to sync...")
			time.Sleep(8 * time.Second)

			// Start port-forward to controller
			ctrlNS := testSuite.ControllerNamespace()
			endpoint, pf, err := e2eenv.StartControllerPortForward(ctx, ctrlNS)
			if err != nil {
				t.Fatalf("start controller port-forward: %v", err)
			}
			defer pf.Cleanup()
			t.Logf("Controller port-forward established on %s", endpoint)

			ctl := e2eenv.NewFSBCtl(
				e2eenv.WithFSBCtlBinary(cliBinaryPath),
				e2eenv.WithFSBCtlEndpoint(endpoint),
				e2eenv.WithFSBCtlNamespace(namespace),
			)

			t.Log("Creating sandbox through fsb-ctl run...")
			if output, err := ctl.Run(ctx, "sb-logs-test", e2eenv.FSBCtlConfig{
				Image:           "docker.io/library/alpine:latest",
				PoolRef:         pool.Name,
				ConsistencyMode: "strong",
				Command:         []string{"/bin/sh"},
				Args:            []string{"-c", "echo 'Log-Test-Line-1' && sleep 1 && echo 'Log-Test-Line-2' && sleep 3600"},
			}); err != nil {
				t.Fatalf("fsb-ctl run failed: %v\noutput: %s", err, output)
			}

			waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			if _, err := ctl.WaitRunning(waitCtx, "sb-logs-test"); err != nil {
				t.Fatalf("wait for sandbox running via fsb-ctl: %v", err)
			}

			// Wait for logs to be produced
			time.Sleep(3 * time.Second)

			logsCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			logs, err := ctl.Logs(logsCtx, "sb-logs-test")
			if err != nil {
				t.Fatalf("fsb-ctl logs failed: %v\nlogs: %s", err, logs)
			}
			if !strings.Contains(logs, "Log-Test-Line-1") || !strings.Contains(logs, "Log-Test-Line-2") {
				t.Fatalf("fsb-ctl logs output missing expected lines:\n%s", logs)
			}
			t.Log("✓ fsb-ctl logs command works")

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestCLIRun(t *testing.T) {
	manager := e2eenv.Require(t, e2eenv.ProfileBasic)
	cliBinaryPath := buildFSBCtl(t, manager)

	feature := features.New("cli-run").
		WithLabel("suite", "cliintegration").
		WithLabel("tier", "smoke").
		Assess("fsb-ctl run command creates sandbox with config file", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("clirun")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			// Create pool
			pool := createCLIPool(namespace, "run-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if _, err := fixture.WaitForReadyAgentPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready agent pods: %v", err)
			}

			// Wait for agent capacity to sync to controller registry
			// Agent control loop runs every 2s, give it time to register capacity
			t.Log("Waiting for agent capacity to sync...")
			time.Sleep(8 * time.Second)

			// Start port-forward to controller
			ctrlNS := testSuite.ControllerNamespace()
			endpoint, pf, err := e2eenv.StartControllerPortForward(ctx, ctrlNS)
			if err != nil {
				t.Fatalf("start controller port-forward: %v", err)
			}
			defer pf.Cleanup()
			t.Logf("Controller port-forward established on %s", endpoint)

			ctl := e2eenv.NewFSBCtl(
				e2eenv.WithFSBCtlBinary(cliBinaryPath),
				e2eenv.WithFSBCtlEndpoint(endpoint),
				e2eenv.WithFSBCtlNamespace(namespace),
			)

			t.Log("Testing fsb-ctl run command...")
			output, err := ctl.Run(ctx, "sb-run-test", e2eenv.FSBCtlConfig{
				Image:           "docker.io/library/alpine:latest",
				PoolRef:         pool.Name,
				ConsistencyMode: "strong",
				Command:         []string{"/bin/sh"},
				Args:            []string{"-c", "echo 'Hello from fsb-ctl' && sleep 30"},
			})
			if err != nil {
				t.Fatalf("fsb-ctl run failed: %v\noutput: %s", err, output)
			}

			if strings.Contains(string(output), "created successfully") || strings.Contains(string(output), "ID:") {
				t.Log("✓ fsb-ctl run command works")

				waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
				defer cancel()
				if _, err := ctl.WaitRunning(waitCtx, "sb-run-test"); err != nil {
					t.Fatalf("wait for sandbox running via fsb-ctl: %v", err)
				}
			} else {
				t.Fatalf("fsb-ctl run unexpected output: %s", output)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func buildFSBCtl(t *testing.T, manager *e2eenv.Manager) string {
	t.Helper()

	cliBinaryPath, err := manager.BuildFSBCtl(context.Background())
	if err != nil {
		t.Fatalf("build fsb-ctl binary: %v", err)
	}
	t.Logf("Built fsb-ctl at %s", cliBinaryPath)
	return cliBinaryPath
}

func createCLIPool(namespace, name string) *apiv1alpha1.SandboxPool {
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
				PoolMax: 10, // Increased for CLI tests
			},
			MaxSandboxesPerPod: 20, // Increased capacity
			RuntimeType:        apiv1alpha1.RuntimeContainer,
			AgentTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "agent",
						Image: suiteenv.AgentImage(),
					}},
				},
			},
		},
	}
}
