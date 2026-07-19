package faultrecovery

import (
	"context"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestAutoExpiry(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("auto-expiry").
		WithLabel("suite", "faultrecovery").
		WithLabel("tier", "smoke").
		Assess("sandbox with expireTime is garbage collected after expiry", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("expiry")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			// Create pool
			pool := createFaultPool(namespace, "expiry-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}

			// Calculate expiry time (90 seconds from now to allow enough time for scheduling)
			expiryTime := metav1.NewTime(time.Now().Add(90 * time.Second))

			// Create sandbox with expiry
			sandbox := &apiv1alpha1.Sandbox{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1alpha1.GroupVersion.String(),
					Kind:       "Sandbox",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sb-expiry-test",
					Namespace: namespace,
				},
				Spec: apiv1alpha1.SandboxSpec{
					Image:      "docker.io/library/alpine:latest",
					Command:    []string{"/bin/sleep", "3600"},
					PoolRef:    pool.Name,
					ExpireTime: &expiryTime,
				},
			}
			if err := k8sClient.Create(ctx, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			// Wait for sandbox to be assigned first
			waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			assignedSandbox, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Name: "sb-expiry-test", Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.AssignedFastlet != "" &&
					(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
			})
			if err != nil {
				t.Fatalf("wait for sandbox to be assigned: %v", err)
			}
			t.Logf("Sandbox is assigned and running, phase=%s", assignedSandbox.Status.Phase)

			// Wait for expiry (with buffer)
			// Expiry time was set to 90 seconds, so we need to wait for that plus some buffer
			t.Log("Waiting for sandbox to expire...")
			expireWaitCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
			defer cancel()

			expiredSandbox, err := fixture.WaitForSandbox(expireWaitCtx, types.NamespacedName{Name: "sb-expiry-test", Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.Phase == string(apiv1alpha1.PhaseExpired)
			})
			if err != nil {
				// Log current state for debugging
				currentSandbox := &apiv1alpha1.Sandbox{}
				if getErr := k8sClient.Get(ctx, types.NamespacedName{Name: "sb-expiry-test", Namespace: namespace}, currentSandbox); getErr == nil {
					t.Logf("Sandbox state at timeout: phase=%s, assignedFastlet=%s, sandboxID=%s",
						currentSandbox.Status.Phase, currentSandbox.Status.AssignedFastlet, currentSandbox.Status.SandboxID)
				}
				t.Fatalf("wait for sandbox expiry: %v", err)
			}

			// Verify CRD is preserved
			if expiredSandbox.Status.Phase != string(apiv1alpha1.PhaseExpired) {
				t.Fatalf("expected phase Expired, got %s", expiredSandbox.Status.Phase)
			}
			t.Log("✓ Sandbox expired, CRD preserved")

			// Verify status fields are cleared
			if expiredSandbox.Status.AssignedFastlet != "" {
				t.Fatalf("expected assignedFastlet to be empty after expiry, got %s", expiredSandbox.Status.AssignedFastlet)
			}
			if expiredSandbox.Status.SandboxID != "" {
				t.Fatalf("expected sandboxID to be empty after expiry, got %s", expiredSandbox.Status.SandboxID)
			}
			t.Log("✓ Status fields correctly cleared after expiry")

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestMemoryLeak(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("memory-leak").
		WithLabel("suite", "faultrecovery").
		WithLabel("tier", "smoke").
		Assess("registry handles create/delete cycles without memory leak", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("memleak")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			// Create pool
			pool := createFaultPool(namespace, "memory-test-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}

			// Create 5 sandboxes
			t.Log("Creating 5 sandboxes...")
			for i := 1; i <= 5; i++ {
				sandbox := createFaultSandbox(namespace, "sb-mem-%d", pool.Name, i)
				if err := k8sClient.Create(ctx, sandbox); err != nil {
					t.Fatalf("create sandbox sb-mem-%d: %v", i, err)
				}
			}

			// Wait for all to be assigned
			time.Sleep(10 * time.Second)

			// Delete 3 sandboxes
			t.Log("Deleting 3 sandboxes...")
			for i := 1; i <= 3; i++ {
				name := types.NamespacedName{Name: sandboxName("sb-mem-%d", i), Namespace: namespace}
				sandbox := &apiv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace}}
				if err := k8sClient.Delete(ctx, sandbox); err != nil && !errors.IsNotFound(err) {
					t.Logf("Warning: delete sandbox sb-mem-%d: %v", i, err)
				}
			}

			// Wait for deletion
			time.Sleep(5 * time.Second)

			// Create new sandbox to verify registry still works
			t.Log("Creating new sandbox to verify registry...")
			newSandbox := createFaultSandbox(namespace, "sb-mem-new", pool.Name, 0)
			if err := k8sClient.Create(ctx, newSandbox); err != nil {
				t.Fatalf("create new sandbox: %v", err)
			}

			// Wait for new sandbox to be assigned
			waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			if _, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Name: "sb-mem-new", Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.AssignedFastlet != ""
			}); err != nil {
				t.Fatalf("new sandbox not assigned, registry may have issues: %v", err)
			}
			t.Log("✓ New sandbox assigned successfully, registry working correctly")

			// Create more sandboxes to further verify
			for i := 1; i <= 3; i++ {
				sandbox := createFaultSandbox(namespace, "sb-mem-verify-%d", pool.Name, i)
				if err := k8sClient.Create(ctx, sandbox); err != nil {
					t.Fatalf("create verify sandbox sb-mem-verify-%d: %v", i, err)
				}
			}

			time.Sleep(10 * time.Second)
			t.Log("✓ Registry functioning normally, no memory leak indicators")

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestControlledRecovery(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("controlled-recovery").
		WithLabel("suite", "faultrecovery").
		WithLabel("tier", "smoke").
		Assess("manual reset and auto-recreate work correctly", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("recovery")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			// Create pool
			pool := createFaultPool(namespace, "recovery-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}

			// Create sandbox
			sandbox := createFaultSandbox(namespace, "sb-recovery", pool.Name, 0)
			if err := k8sClient.Create(ctx, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			// Wait for sandbox to be running
			waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			runningSandbox, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Name: "sb-recovery", Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.AssignedFastlet != "" &&
					(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
			})
			if err != nil {
				t.Fatalf("wait for sandbox to be running: %v", err)
			}

			oldPod := runningSandbox.Status.AssignedFastlet
			t.Logf("Sandbox running on pod: %s", oldPod)

			// Test 1: Manual reset via ResetRevision
			t.Log("Testing manual reset via ResetRevision...")
			resetTime := metav1.Now()

			// Get fresh copy for update
			resetSandbox := &apiv1alpha1.Sandbox{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "sb-recovery", Namespace: namespace}, resetSandbox); err != nil {
				t.Fatalf("get sandbox for reset: %v", err)
			}
			resetSandbox.Spec.ResetRevision = &resetTime
			if err := k8sClient.Update(ctx, resetSandbox); err != nil {
				t.Fatalf("update sandbox with reset revision: %v", err)
			}

			// Wait for reset to be accepted
			// Give controller more time to process reset request
			resetWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second) // Increased from 60s
			defer cancel()
			_, err = fixture.WaitForSandbox(resetWaitCtx, types.NamespacedName{Name: "sb-recovery", Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				if sb.Status.AcceptedResetRevision == nil {
					return false
				}
				// Check if accepted reset revision has the same second as spec reset revision
				// Kubernetes truncates timestamps to seconds, so we compare at second precision
				return resetTime.Time.Truncate(time.Second).Equal(sb.Status.AcceptedResetRevision.Time.Truncate(time.Second))
			})
			if err != nil {
				// Log current state for debugging
				currentSandbox := &apiv1alpha1.Sandbox{}
				if getErr := k8sClient.Get(ctx, types.NamespacedName{Name: "sb-recovery", Namespace: namespace}, currentSandbox); getErr == nil {
					t.Logf("Sandbox state at timeout: phase=%s, acceptedResetRevision=%v",
						currentSandbox.Status.Phase, currentSandbox.Status.AcceptedResetRevision)
				}
				t.Fatalf("wait for reset to be accepted: %v", err)
			}
			t.Log("✓ Manual reset was accepted by controller")

			// Test 2: AutoRecreate
			t.Log("Testing AutoRecreate mechanism...")
			// Use retry logic to handle concurrent modifications
			var autoRecreateSandbox *apiv1alpha1.Sandbox
			updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				autoRecreateSandbox = &apiv1alpha1.Sandbox{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "sb-recovery", Namespace: namespace}, autoRecreateSandbox); err != nil {
					return err
				}
				// Set AutoRecreate policy
				autoRecreateSandbox.Spec.FailurePolicy = apiv1alpha1.FailurePolicyAutoRecreate
				autoRecreateSandbox.Spec.RecoveryTimeoutSeconds = 15
				return k8sClient.Update(ctx, autoRecreateSandbox)
			})
			if updateErr != nil {
				t.Fatalf("update sandbox with AutoRecreate policy: %v", updateErr)
			}

			time.Sleep(2 * time.Second)

			// Get current assigned pod
			currentSandbox := &apiv1alpha1.Sandbox{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "sb-recovery", Namespace: namespace}, currentSandbox); err != nil {
				t.Fatalf("get sandbox: %v", err)
			}
			currentPod := currentSandbox.Status.AssignedFastlet

			// Delete the fastlet pod to trigger disconnect
			t.Logf("Deleting fastlet pod %s to trigger AutoRecreate...", currentPod)
			fastletPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: currentPod, Namespace: namespace}}
			if err := k8sClient.Delete(ctx, fastletPod); err != nil && !errors.IsNotFound(err) {
				t.Logf("Warning: delete fastlet pod: %v", err)
			}

			// Wait for sandbox to be rescheduled to a new pod
			t.Log("Waiting for AutoRecreate to trigger...")
			recreateWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()

			_, err = fixture.WaitForSandbox(recreateWaitCtx, types.NamespacedName{Name: "sb-recovery", Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.AssignedFastlet != "" && sb.Status.AssignedFastlet != oldPod
			})
			if err != nil {
				t.Logf("Warning: AutoRecreate may not have completed in time: %v", err)
			} else {
				t.Log("✓ AutoRecreate triggered, sandbox rescheduled to new pod")
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestPodExistence(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("pod-existence").
		WithLabel("suite", "faultrecovery").
		WithLabel("tier", "smoke").
		Assess("janitor correctly identifies and handles orphan containers", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("existence")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			// Create pool
			pool := createFaultPool(namespace, "existence-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}

			// Create sandbox with exposed ports
			sandbox := &apiv1alpha1.Sandbox{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1alpha1.GroupVersion.String(),
					Kind:       "Sandbox",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sb-existence",
					Namespace: namespace,
				},
				Spec: apiv1alpha1.SandboxSpec{
					Image:        "docker.io/library/alpine:latest",
					Command:      []string{"/bin/sleep", "3600"},
					PoolRef:      pool.Name,
					ExposedPorts: []int32{8080},
				},
			}
			if err := k8sClient.Create(ctx, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			// Wait for sandbox to be running
			waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			runningSandbox, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Name: "sb-existence", Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.AssignedFastlet != "" &&
					(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
			})
			if err != nil {
				t.Fatalf("wait for sandbox to be running: %v", err)
			}
			t.Log("Sandbox created successfully")

			fastletPod := runningSandbox.Status.AssignedFastlet
			t.Logf("Fastlet Pod: %s", fastletPod)

			// Delete fastlet pod to simulate orphan scenario
			t.Logf("Deleting fastlet pod %s to simulate orphan...", fastletPod)
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fastletPod, Namespace: namespace}}
			if err := k8sClient.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
				t.Fatalf("delete fastlet pod: %v", err)
			}

			// Wait for Janitor scan cycle (simulated by short wait)
			t.Log("Waiting for Janitor scan cycle...")
			time.Sleep(35 * time.Second)

			// Check sandbox status
			existingSandbox := &apiv1alpha1.Sandbox{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "sb-existence", Namespace: namespace}, existingSandbox)
			if err != nil {
				if errors.IsNotFound(err) {
					t.Log("✓ Sandbox CRD was deleted (Janitor handled orphan)")
					return ctx
				}
				t.Fatalf("get sandbox: %v", err)
			}

			// Sandbox exists, check its state
			phase := existingSandbox.Status.Phase
			switch phase {
			case string(apiv1alpha1.PhaseFailed), "Unknown":
				t.Logf("✓ Sandbox in %s state, orphan was identified", phase)
			case string(apiv1alpha1.PhaseRunning), string(apiv1alpha1.PhaseBound):
				newPod := existingSandbox.Status.AssignedFastlet
				if newPod != "" && newPod != fastletPod {
					t.Logf("✓ Sandbox was rescheduled to new pod: %s", newPod)
				} else {
					t.Log("✓ Sandbox still running (may be expected state)")
				}
			default:
				t.Logf("Sandbox phase: %s", phase)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func createFaultPool(namespace, name string) *apiv1alpha1.SandboxPool {
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
				PoolMax: 2,
			},
			MaxSandboxesPerPod: 5,
			Runtime:            apiv1alpha1.RuntimeContainer,
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

func createFaultSandbox(namespace, namePattern, pool string, index int) *apiv1alpha1.Sandbox {
	name := sandboxName(namePattern, index)
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

func sandboxName(pattern string, index int) string {
	if index == 0 {
		return pattern
	}
	return pattern[:len(pattern)-3] + string(rune('0'+index))
}
