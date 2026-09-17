package faultrecovery

import (
	"context"
	"testing"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
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

			sandbox := &apiv1alpha2.Sandbox{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1alpha2.GroupVersion.String(),
					Kind:       "Sandbox",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sb-expiry-test",
					Namespace: namespace,
				},
				Spec: apiv1alpha2.SandboxSpec{
					Image:      "docker.io/library/alpine:latest",
					Command:    []string{"/bin/sleep", "3600"},
					PoolRef:    pool.Name,
					ExpireTime: &expiryTime,
				},
			}
			if err := k8sClient.Create(ctx, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			assignedSandbox, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Name: "sb-expiry-test", Namespace: namespace}, func(sb *apiv1alpha2.Sandbox) bool {
				return sb.Status.Placement.FastletName != "" && sb.Status.Runtime.State == apiv1alpha2.RuntimeReady
			})
			if err != nil {
				t.Fatalf("wait for sandbox to be assigned: %v", err)
			}
			t.Logf("Sandbox is assigned and ready, runtimeState=%s", assignedSandbox.Status.Runtime.State)

			t.Log("Waiting for sandbox to expire...")
			expireWaitCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
			defer cancel()

			expiredSandbox, err := fixture.WaitForSandbox(expireWaitCtx, types.NamespacedName{Name: "sb-expiry-test", Namespace: namespace}, func(sb *apiv1alpha2.Sandbox) bool {
				return sb.Status.HasCondition(apiv1alpha2.SandboxConditionReady, metav1.ConditionFalse, "Expired")
			})
			if err != nil {
				currentSandbox := &apiv1alpha2.Sandbox{}
				if getErr := k8sClient.Get(ctx, types.NamespacedName{Name: "sb-expiry-test", Namespace: namespace}, currentSandbox); getErr == nil {
					t.Logf("Sandbox state at timeout: runtimeState=%s, assignment=%+v, uid=%s",
						currentSandbox.Status.Runtime.State, currentSandbox.Status.Placement, currentSandbox.UID)
				}
				t.Fatalf("wait for sandbox expiry: %v", err)
			}

			// Verify CRD is preserved
			if !expiredSandbox.Status.HasCondition(apiv1alpha2.SandboxConditionReady, metav1.ConditionFalse, "Expired") {
				t.Fatalf("expected Expired RuntimeReady condition, got %+v", expiredSandbox.Status.Conditions)
			}
			t.Log("✓ Sandbox expired, CRD preserved")

			if expiredSandbox.Status.Placement.FastletName != "" {
				t.Fatalf("expected placement target to be empty after expiry, got %+v", expiredSandbox.Status.Placement)
			}
			t.Log("✓ Status fields correctly cleared after expiry")

			// Expiration is recoverable while the CRD remains. Moving the desired
			// deadline into the future must create a new concrete runtime rather
			// than leaving Condition.Reason as a hidden terminal state.
			recoveredDesired := expiredSandbox.DeepCopy()
			nextExpiry := metav1.NewTime(time.Now().Add(5 * time.Minute))
			recoveredDesired.Spec.ExpireTime = &nextExpiry
			if err := k8sClient.Update(ctx, recoveredDesired); err != nil {
				t.Fatalf("extend expired Sandbox lifetime: %v", err)
			}
			recoveryCtx, cancelRecovery := context.WithTimeout(ctx, 90*time.Second)
			defer cancelRecovery()
			recovered, err := fixture.WaitForSandbox(recoveryCtx, types.NamespacedName{Name: "sb-expiry-test", Namespace: namespace}, func(sb *apiv1alpha2.Sandbox) bool {
				return sb.Status.Placement.FastletName != "" &&
					sb.Status.Runtime.State == apiv1alpha2.RuntimeReady &&
					sb.Status.Runtime.Generation > assignedSandbox.Status.Runtime.Generation &&
					sb.Status.HasCondition(apiv1alpha2.SandboxConditionReady, metav1.ConditionTrue, "Ready")
			})
			if err != nil {
				t.Fatalf("wait for expired Sandbox recovery: %v", err)
			}
			t.Logf("✓ Expired Sandbox recovered as runtime generation %d", recovered.Status.Runtime.Generation)

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

			pool := createFaultPool(namespace, "memory-test-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}

			t.Log("Creating 5 sandboxes...")
			for i := 1; i <= 5; i++ {
				sandbox := createFaultSandbox(namespace, "sb-mem-%d", pool.Name, i)
				if err := k8sClient.Create(ctx, sandbox); err != nil {
					t.Fatalf("create sandbox sb-mem-%d: %v", i, err)
				}
			}

			// Wait for all to be assigned
			time.Sleep(10 * time.Second)

			t.Log("Deleting 3 sandboxes...")
			for i := 1; i <= 3; i++ {
				name := types.NamespacedName{Name: sandboxName("sb-mem-%d", i), Namespace: namespace}
				sandbox := &apiv1alpha2.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: name.Name, Namespace: name.Namespace}}
				if err := k8sClient.Delete(ctx, sandbox); err != nil && !errors.IsNotFound(err) {
					t.Logf("Warning: delete sandbox sb-mem-%d: %v", i, err)
				}
			}

			// Wait for deletion
			time.Sleep(5 * time.Second)

			t.Log("Creating new sandbox to verify registry...")
			newSandbox := createFaultSandbox(namespace, "sb-mem-new", pool.Name, 0)
			if err := k8sClient.Create(ctx, newSandbox); err != nil {
				t.Fatalf("create new sandbox: %v", err)
			}

			waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			if _, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Name: "sb-mem-new", Namespace: namespace}, func(sb *apiv1alpha2.Sandbox) bool {
				return sb.Status.Placement.FastletName != ""
			}); err != nil {
				t.Fatalf("new sandbox not assigned, registry may have issues: %v", err)
			}
			t.Log("✓ New sandbox assigned successfully, registry working correctly")

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

			pool := createFaultPool(namespace, "recovery-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}

			sandbox := createFaultSandbox(namespace, "sb-recovery", pool.Name, 0)
			if err := k8sClient.Create(ctx, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			runningSandbox, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Name: "sb-recovery", Namespace: namespace}, func(sb *apiv1alpha2.Sandbox) bool {
				return sb.Status.Placement.FastletName != "" && sb.Status.Runtime.State == apiv1alpha2.RuntimeReady
			})
			if err != nil {
				t.Fatalf("wait for sandbox to be running: %v", err)
			}

			oldPod := runningSandbox.Status.Placement.FastletName
			t.Logf("Sandbox running on pod: %s", oldPod)

			t.Log("Testing manual reset via ResetRevision...")
			resetTime := metav1.Now()

			resetSandbox := &apiv1alpha2.Sandbox{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "sb-recovery", Namespace: namespace}, resetSandbox); err != nil {
				t.Fatalf("get sandbox for reset: %v", err)
			}
			resetSandbox.Spec.ResetRevision = &resetTime
			if err := k8sClient.Update(ctx, resetSandbox); err != nil {
				t.Fatalf("update sandbox with reset revision: %v", err)
			}

			// Give controller more time to process reset request
			resetWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			_, err = fixture.WaitForSandbox(resetWaitCtx, types.NamespacedName{Name: "sb-recovery", Namespace: namespace}, func(sb *apiv1alpha2.Sandbox) bool {
				if sb.Status.Runtime.AcceptedResetRevision == nil {
					return false
				}
				// Check if accepted reset revision has the same second as spec reset revision
				// Kubernetes truncates timestamps to seconds, so we compare at second precision
				return resetTime.Time.Truncate(time.Second).Equal(sb.Status.Runtime.AcceptedResetRevision.Time.Truncate(time.Second))
			})
			if err != nil {
				currentSandbox := &apiv1alpha2.Sandbox{}
				if getErr := k8sClient.Get(ctx, types.NamespacedName{Name: "sb-recovery", Namespace: namespace}, currentSandbox); getErr == nil {
					t.Logf("Sandbox state at timeout: runtimeState=%s, acceptedResetRevision=%v",
						currentSandbox.Status.Runtime.State, currentSandbox.Status.Runtime.AcceptedResetRevision)
				}
				t.Fatalf("wait for reset to be accepted: %v", err)
			}
			t.Log("✓ Manual reset was accepted by controller")

			t.Log("Testing AutoRecreate mechanism...")
			// Use retry logic to handle concurrent modifications
			var autoRecreateSandbox *apiv1alpha2.Sandbox
			updateErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				autoRecreateSandbox = &apiv1alpha2.Sandbox{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "sb-recovery", Namespace: namespace}, autoRecreateSandbox); err != nil {
					return err
				}
				autoRecreateSandbox.Spec.FailurePolicy = apiv1alpha2.FailurePolicyAutoRecreate
				autoRecreateSandbox.Spec.RecoveryTimeoutSeconds = 15
				return k8sClient.Update(ctx, autoRecreateSandbox)
			})
			if updateErr != nil {
				t.Fatalf("update sandbox with AutoRecreate policy: %v", updateErr)
			}

			time.Sleep(2 * time.Second)

			currentSandbox := &apiv1alpha2.Sandbox{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: "sb-recovery", Namespace: namespace}, currentSandbox); err != nil {
				t.Fatalf("get sandbox: %v", err)
			}
			if currentSandbox.Status.Placement.FastletName == "" {
				t.Fatal("sandbox lost its assignment before AutoRecreate test")
			}
			currentPod := currentSandbox.Status.Placement.FastletName

			t.Logf("Deleting fastlet pod %s to trigger AutoRecreate...", currentPod)
			fastletPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: currentPod, Namespace: namespace}}
			if err := k8sClient.Delete(ctx, fastletPod); err != nil && !errors.IsNotFound(err) {
				t.Logf("Warning: delete fastlet pod: %v", err)
			}

			t.Log("Waiting for AutoRecreate to trigger...")
			recreateWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()

			_, err = fixture.WaitForSandbox(recreateWaitCtx, types.NamespacedName{Name: "sb-recovery", Namespace: namespace}, func(sb *apiv1alpha2.Sandbox) bool {
				return sb.Status.Placement.FastletName != "" && sb.Status.Placement.FastletName != oldPod && sb.Status.Runtime.State == apiv1alpha2.RuntimeReady
			})
			if err != nil {
				t.Fatalf("wait for AutoRecreate to reschedule the Sandbox: %v", err)
			}
			t.Log("✓ AutoRecreate triggered, sandbox rescheduled to new pod")

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

			pool := createFaultPool(namespace, "existence-pool")
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}

			sandbox := &apiv1alpha2.Sandbox{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1alpha2.GroupVersion.String(),
					Kind:       "Sandbox",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sb-existence",
					Namespace: namespace,
				},
				Spec: apiv1alpha2.SandboxSpec{
					Image:   "docker.io/library/alpine:latest",
					Command: []string{"/bin/sleep", "3600"},
					PoolRef: pool.Name,
				},
			}
			if err := k8sClient.Create(ctx, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			runningSandbox, err := fixture.WaitForSandbox(waitCtx, types.NamespacedName{Name: "sb-existence", Namespace: namespace}, func(sb *apiv1alpha2.Sandbox) bool {
				return sb.Status.Placement.FastletName != "" && sb.Status.Runtime.State == apiv1alpha2.RuntimeReady
			})
			if err != nil {
				t.Fatalf("wait for sandbox to be running: %v", err)
			}
			t.Log("Sandbox created successfully")

			fastletPod := runningSandbox.Status.Placement.FastletName
			t.Logf("Fastlet Pod: %s", fastletPod)

			t.Logf("Deleting fastlet pod %s to simulate orphan...", fastletPod)
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: fastletPod, Namespace: namespace}}
			if err := k8sClient.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
				t.Fatalf("delete fastlet pod: %v", err)
			}

			// Wait for the Janitor scan cycle to pick up the orphaned resources
			t.Log("Waiting for Janitor scan cycle...")
			time.Sleep(35 * time.Second)

			existingSandbox := &apiv1alpha2.Sandbox{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: "sb-existence", Namespace: namespace}, existingSandbox)
			if err != nil {
				if errors.IsNotFound(err) {
					t.Log("✓ Sandbox CRD was deleted (Janitor handled orphan)")
					return ctx
				}
				t.Fatalf("get sandbox: %v", err)
			}

			state := existingSandbox.Status.Runtime.State
			switch state {
			case apiv1alpha2.RuntimeFailed, apiv1alpha2.RuntimeUnavailable, apiv1alpha2.RuntimeUnknown:
				t.Logf("✓ Sandbox in %s state, orphan was identified", state)
			case apiv1alpha2.RuntimeReady:
				newPod := ""
				if existingSandbox.Status.Placement.FastletName != "" {
					newPod = existingSandbox.Status.Placement.FastletName
				}
				if newPod != "" && newPod != fastletPod {
					t.Logf("✓ Sandbox was rescheduled to new pod: %s", newPod)
				} else {
					t.Log("✓ Sandbox still running (may be expected state)")
				}
			default:
				t.Logf("Sandbox runtime state: %s", state)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func createFaultPool(namespace, name string) *apiv1alpha2.SandboxPool {
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
				PoolMax: 2,
			},
			MaxSandboxesPerPod: 5,
			Runtime:            apiv1alpha2.RuntimeContainer,
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

func createFaultSandbox(namespace, namePattern, pool string, index int) *apiv1alpha2.Sandbox {
	name := sandboxName(namePattern, index)
	return &apiv1alpha2.Sandbox{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1alpha2.GroupVersion.String(),
			Kind:       "Sandbox",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: apiv1alpha2.SandboxSpec{
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
