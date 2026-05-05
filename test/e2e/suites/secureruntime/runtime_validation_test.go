package secureruntime

import (
	"context"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestInvalidRuntimeClass(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("invalid-runtime-class").
		WithLabel("suite", "secureruntime").
		WithLabel("tier", "validation").
		Assess("pool with invalid RuntimeClass shows error condition", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("invalid-runtime")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			// Create pool with non-existent RuntimeClass
			pool := &apiv1alpha1.SandboxPool{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1alpha1.GroupVersion.String(),
					Kind:       "SandboxPool",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "invalid-runtime-pool",
					Namespace: namespace,
				},
				Spec: apiv1alpha1.SandboxPoolSpec{
					Capacity: apiv1alpha1.PoolCapacity{
						PoolMin: 1,
						PoolMax: 1,
					},
					MaxSandboxesPerPod: 5,
					RuntimeType:        apiv1alpha1.RuntimeGVisor,
					RuntimeClassName:   "non-existent-runtime",
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
			if runtimeReady.Reason != apiv1alpha1.ReasonRuntimeUnavailable {
				t.Errorf("expected Reason to be RuntimeUnavailable, got: %v", runtimeReady.Reason)
			}

			t.Logf("Pool condition correctly shows error: %s", runtimeReady.Message)

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestContainerRuntimeDefault(t *testing.T) {
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

			// Create sandbox
			sandbox := newSecureRuntimeSandbox(namespace, "sb-container", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			// Wait for sandbox running
			runCtx, cancelRunWait := context.WithTimeout(ctx, 60*time.Second)
			defer cancelRunWait()
			_, err := fixture.WaitForSandbox(runCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.AssignedFastlet != "" &&
					(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
			})
			if err != nil {
				t.Fatalf("wait for running sandbox: %v", err)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}
