package cliintegration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	e2eenv "fast-sandbox/test/e2e/env"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestQuickStartOpenSandboxExecd(t *testing.T) {
	manager := e2eenv.Require(t, e2eenv.ProfileBasic)
	cliBinaryPath := buildFastctl(t, manager)

	feature := features.New("quickstart-opensandbox-execd").
		WithLabel("suite", "cliintegration").
		WithLabel("tier", "smoke").
		Assess("runs the printed lifecycle, diagnostics, exec, and file commands against real Execd", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))
			namespace := testSuite.AllocateNamespace("quickstart")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(context.Background(), t, k8sClient, namespace)

			pool := createCLIPool(namespace, "quickstart-execd-pool")
			pool.Spec.InfraProfile = "opensandbox-execd-quickstart"
			pool.Spec.Capacity = apiv1alpha1.PoolCapacity{PoolMin: 1, PoolMax: 1}
			pool.Spec.MaxSandboxesPerPod = 1
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create quickstart Pool: %v", err)
			}
			poolWaitCtx, poolCancel := context.WithTimeout(ctx, 2*time.Minute)
			defer poolCancel()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for Execd-ready Fastlet Pod: %v", err)
			}

			controlEndpoint, controlForward, err := e2eenv.StartControllerPortForward(ctx, testSuite.ControllerNamespace())
			if err != nil {
				t.Fatalf("start Fast-Path port-forward: %v", err)
			}
			defer controlForward.Cleanup()
			proxyEndpoint, proxyForward, err := e2eenv.StartSandboxProxyPortForward(ctx, testSuite.ControllerNamespace())
			if err != nil {
				t.Fatalf("start Sandbox Proxy port-forward: %v", err)
			}
			defer proxyForward.Cleanup()

			ctl := e2eenv.NewFastctl(
				e2eenv.WithFastctlBinary(cliBinaryPath),
				e2eenv.WithFastctlEndpoint(controlEndpoint),
				e2eenv.WithFastctlProxyEndpoint(proxyEndpoint),
				e2eenv.WithFastctlNamespace(namespace),
			)
			const sandboxName = "quickstart-execd-sandbox"
			if output, err := ctl.Run(ctx, sandboxName, e2eenv.FastctlConfig{
				Image: "docker.io/library/alpine:latest", PoolRef: pool.Name,
				Command: []string{"/bin/sleep"}, Args: []string{"3600"},
			}); err != nil {
				t.Fatalf("fastctl run failed: %v\n%s", err, output)
			}
			defer ctl.Delete(context.Background(), sandboxName)

			readyCtx, readyCancel := context.WithTimeout(ctx, 90*time.Second)
			defer readyCancel()
			if _, err := ctl.WaitRunning(readyCtx, sandboxName); err != nil {
				t.Fatalf("wait for quickstart Sandbox: %v", err)
			}
			if output, err := ctl.Command(ctx, "diagnostics", "sandbox", sandboxName); err != nil {
				t.Fatalf("fastctl diagnostics failed: %v\n%s", err, output)
			} else if !strings.Contains(string(output), "Fastlet diagnostics: reachable") {
				t.Fatalf("unexpected diagnostics output: %s", output)
			}

			output, err := ctl.Command(ctx, "opensandbox", "exec", sandboxName, "--", "sh", "-lc", "printf 'hello from execd\\n' > /tmp/execd.txt && cat /tmp/execd.txt")
			if err != nil {
				t.Fatalf("fastctl opensandbox exec failed: %v\n%s", err, output)
			}
			if !strings.Contains(string(output), "hello from execd") {
				t.Fatalf("unexpected exec output: %s", output)
			}

			localRoot := t.TempDir()
			uploadPath := filepath.Join(localRoot, "from-host.txt")
			if err := os.WriteFile(uploadPath, []byte("hello from host\n"), 0600); err != nil {
				t.Fatalf("write upload fixture: %v", err)
			}
			if output, err = ctl.Command(ctx, "opensandbox", "cp", uploadPath, sandboxName+":/tmp/from-host.txt"); err != nil {
				t.Fatalf("fastctl opensandbox cp upload failed: %v\n%s", err, output)
			}
			if output, err = ctl.Command(ctx, "opensandbox", "files", "stat", sandboxName, "/tmp/from-host.txt"); err != nil {
				t.Fatalf("fastctl opensandbox files stat failed: %v\n%s", err, output)
			} else if !strings.Contains(string(output), "/tmp/from-host.txt") {
				t.Fatalf("unexpected stat output: %s", output)
			}
			if output, err = ctl.Command(ctx, "opensandbox", "files", "read", sandboxName, "/tmp/from-host.txt"); err != nil {
				t.Fatalf("fastctl opensandbox files read failed: %v\n%s", err, output)
			} else if !strings.Contains(string(output), "hello from host") {
				t.Fatalf("unexpected file output: %s", output)
			}
			downloadPath := filepath.Join(localRoot, "downloaded.txt")
			if output, err = ctl.Command(ctx, "opensandbox", "cp", sandboxName+":/tmp/execd.txt", downloadPath); err != nil {
				t.Fatalf("fastctl opensandbox cp download failed: %v\n%s", err, output)
			}
			if downloaded, err := os.ReadFile(downloadPath); err != nil {
				t.Fatalf("read downloaded fixture: %v", err)
			} else if string(downloaded) != "hello from execd\n" {
				t.Fatalf("downloaded content = %q", downloaded)
			}

			if err := ctl.Delete(ctx, sandboxName); err != nil {
				t.Fatalf("fastctl delete failed: %v", err)
			}
			deleteDeadline := time.Now().Add(60 * time.Second)
			for {
				var sandbox apiv1alpha1.Sandbox
				err := k8sClient.Get(ctx, types.NamespacedName{Name: sandboxName, Namespace: namespace}, &sandbox)
				if apierrors.IsNotFound(err) {
					break
				}
				if err != nil {
					t.Fatalf("get deleting Sandbox: %v", err)
				}
				if time.Now().After(deleteDeadline) {
					t.Fatalf("Sandbox %s was not deleted", sandboxName)
				}
				time.Sleep(250 * time.Millisecond)
			}
			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestUpdateReset(t *testing.T) {
	manager := e2eenv.Require(t, e2eenv.ProfileBasic)
	cliBinaryPath := buildFastctl(t, manager)

	feature := features.New("cli-update-reset").
		WithLabel("suite", "cliintegration").
		WithLabel("tier", "smoke").
		Assess("fastctl update and reset commands work correctly", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
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
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}

			// Wait for fastlet capacity to sync to controller registry
			// Fastlet control loop runs every 2s, give it time to register capacity
			t.Log("Waiting for fastlet capacity to sync...")
			time.Sleep(8 * time.Second)

			// Start port-forward to controller
			ctrlNS := testSuite.ControllerNamespace()
			endpoint, pf, err := e2eenv.StartControllerPortForward(ctx, ctrlNS)
			if err != nil {
				t.Fatalf("start controller port-forward: %v", err)
			}
			defer pf.Cleanup()
			t.Logf("Controller port-forward established on %s", endpoint)

			ctl := e2eenv.NewFastctl(
				e2eenv.WithFastctlBinary(cliBinaryPath),
				e2eenv.WithFastctlEndpoint(endpoint),
				e2eenv.WithFastctlNamespace(namespace),
			)

			t.Log("Creating sandbox through fastctl run...")
			if output, err := ctl.Run(ctx, "sb-update-test", e2eenv.FastctlConfig{
				Image:   "docker.io/library/alpine:latest",
				PoolRef: pool.Name,
				Command: []string{"/bin/sleep"},
				Args:    []string{"3600"},
			}); err != nil {
				t.Fatalf("fastctl run failed: %v\noutput: %s", err, output)
			}

			waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			if _, err := ctl.WaitRunning(waitCtx, "sb-update-test"); err != nil {
				t.Fatalf("wait for sandbox running via fastctl: %v", err)
			}

			// Test 1: fastctl get command
			t.Log("Testing fastctl get command...")
			info, err := ctl.GetJSON(ctx, "sb-update-test")
			if err != nil {
				t.Fatalf("fastctl get failed: %v", err)
			}
			if info.RuntimeState == "" {
				t.Fatalf("fastctl get output missing runtime state: %+v", info)
			}
			t.Log("✓ fastctl get command works")

			// Test 2: fastctl update --labels
			t.Log("Testing fastctl update --labels...")
			output, err := ctl.UpdateLabels(ctx, "sb-update-test", "test=e2e", "env=cli")
			if err != nil || !strings.Contains(string(output), "updated successfully") {
				t.Fatalf("fastctl update labels failed: %v\noutput: %s", err, output)
			}
			t.Log("✓ fastctl update --labels works")

			// Test 3: fastctl reset command
			t.Log("Testing fastctl reset command...")
			output, err = ctl.Reset(ctx, "sb-update-test")
			if err != nil || !strings.Contains(string(output), "reset triggered") {
				t.Fatalf("fastctl reset failed: %v\noutput: %s", err, output)
			}
			t.Log("✓ fastctl reset command works")

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

func TestCLIRun(t *testing.T) {
	manager := e2eenv.Require(t, e2eenv.ProfileBasic)
	cliBinaryPath := buildFastctl(t, manager)

	feature := features.New("cli-run").
		WithLabel("suite", "cliintegration").
		WithLabel("tier", "smoke").
		Assess("fastctl run command creates sandbox with config file", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
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
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}

			// Wait for fastlet capacity to sync to controller registry
			// Fastlet control loop runs every 2s, give it time to register capacity
			t.Log("Waiting for fastlet capacity to sync...")
			time.Sleep(8 * time.Second)

			// Start port-forward to controller
			ctrlNS := testSuite.ControllerNamespace()
			endpoint, pf, err := e2eenv.StartControllerPortForward(ctx, ctrlNS)
			if err != nil {
				t.Fatalf("start controller port-forward: %v", err)
			}
			defer pf.Cleanup()
			t.Logf("Controller port-forward established on %s", endpoint)

			ctl := e2eenv.NewFastctl(
				e2eenv.WithFastctlBinary(cliBinaryPath),
				e2eenv.WithFastctlEndpoint(endpoint),
				e2eenv.WithFastctlNamespace(namespace),
			)

			t.Log("Testing fastctl run command...")
			output, err := ctl.Run(ctx, "sb-run-test", e2eenv.FastctlConfig{
				Image:   "docker.io/library/alpine:latest",
				PoolRef: pool.Name,
				Command: []string{"/bin/sh"},
				Args:    []string{"-c", "echo 'Hello from fastctl' && sleep 30"},
			})
			if err != nil {
				t.Fatalf("fastctl run failed: %v\noutput: %s", err, output)
			}

			if strings.Contains(string(output), "created successfully") || strings.Contains(string(output), "ID:") {
				t.Log("✓ fastctl run command works")

				waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
				defer cancel()
				if _, err := ctl.WaitRunning(waitCtx, "sb-run-test"); err != nil {
					t.Fatalf("wait for sandbox running via fastctl: %v", err)
				}
			} else {
				t.Fatalf("fastctl run unexpected output: %s", output)
			}

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
	t.Logf("Built fastctl at %s", cliBinaryPath)
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
			Runtime:            apiv1alpha1.RuntimeContainer,
			SandboxResources:   suiteenv.SmallSandboxResourceProfile(),
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
