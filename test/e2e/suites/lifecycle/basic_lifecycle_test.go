package lifecycle

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

func TestBasicLifecycleRecreateSameName(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("basic-lifecycle-recreate-same-name").
		WithLabel("suite", "lifecycle").
		WithLabel("tier", "smoke").
		Assess("same-name recreation succeeds immediately after deletion", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("recreate")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := newLifecyclePool(namespace, "lifecycle-test-pool", 1, 1)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancelPoolWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelPoolWait()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}

			var previousSandboxID string
			for attempt := 1; attempt <= 4; attempt++ {
				sandbox := newSleepSandbox(namespace, "sb-basic-lifecycle", pool.Name)
				if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
					t.Fatalf("create sandbox on attempt %d: %v", attempt, err)
				}

				runCtx, cancelRunWait := context.WithTimeout(ctx, 60*time.Second)
				assignedSandbox, err := fixture.WaitForSandbox(runCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
					return sb.Status.Assignment != nil && sb.Status.RuntimeState == apiv1alpha1.ObservedStateReady
				})
				cancelRunWait()
				if err != nil {
					t.Fatalf("wait for running sandbox on attempt %d: %v", attempt, err)
				}

				currentSandboxID := string(assignedSandbox.UID)
				if previousSandboxID != "" && currentSandboxID == previousSandboxID {
					t.Fatalf("sandbox recreation attempt %d reused sandbox ID %q", attempt, currentSandboxID)
				}

				if err := k8sClient.Delete(ctx, sandbox); err != nil {
					t.Fatalf("delete sandbox on attempt %d: %v", attempt, err)
				}

				deleteCtx, cancelDeleteWait := context.WithTimeout(ctx, 90*time.Second)
				if err := fixture.WaitForSandboxDeleted(deleteCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}); err != nil {
					cancelDeleteWait()
					t.Fatalf("wait for sandbox deletion on attempt %d: %v", attempt, err)
				}
				cancelDeleteWait()

				previousSandboxID = currentSandboxID
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestDeleteSelfExitedWorkloadIsIdempotent(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("delete-self-exited-workload-is-idempotent").
		WithLabel("suite", "lifecycle").
		WithLabel("tier", "smoke").
		Assess("deletion converges after the runtime task exits by itself", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("self-exit-delete")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := newLifecyclePool(namespace, "self-exit-delete-pool", 1, 1)
			pool.Spec.MaxSandboxesPerPod = 1
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancelPoolWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelPoolWait()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}

			sandbox := newSleepSandbox(namespace, "self-exiting", pool.Name)
			sandbox.Spec.Command = []string{"/bin/sh", "-lc", "sleep 2"}
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create self-exiting sandbox: %v", err)
			}

			runCtx, cancelRunWait := context.WithTimeout(ctx, 60*time.Second)
			_, err := fixture.WaitForSandbox(runCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.Assignment != nil && sb.Status.RuntimeState == apiv1alpha1.ObservedStateReady
			})
			cancelRunWait()
			if err != nil {
				t.Fatalf("wait for self-exiting sandbox to start: %v", err)
			}

			// RuntimeReady is reported at task start. Wait beyond the command's own
			// lifetime so deletion takes the already-stopped task path.
			select {
			case <-ctx.Done():
				t.Fatalf("wait for workload self-exit: %v", ctx.Err())
			case <-time.After(3 * time.Second):
			}

			if err := k8sClient.Delete(ctx, sandbox); err != nil {
				t.Fatalf("delete self-exited sandbox: %v", err)
			}
			deleteCtx, cancelDeleteWait := context.WithTimeout(ctx, 90*time.Second)
			if err := fixture.WaitForSandboxDeleted(deleteCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}); err != nil {
				cancelDeleteWait()
				t.Fatalf("wait for self-exited sandbox deletion: %v", err)
			}
			cancelDeleteWait()

			// MaxSandboxesPerPod=1 makes successful replacement proof that Fastlet
			// released the admission slot after the idempotent runtime deletion.
			replacement := newSleepSandbox(namespace, "replacement", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, replacement); err != nil {
				t.Fatalf("create replacement sandbox: %v", err)
			}
			replacementCtx, cancelReplacementWait := context.WithTimeout(ctx, 60*time.Second)
			_, err = fixture.WaitForSandbox(replacementCtx, types.NamespacedName{Name: replacement.Name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.Assignment != nil && sb.Status.RuntimeState == apiv1alpha1.ObservedStateReady
			})
			cancelReplacementWait()
			if err != nil {
				t.Fatalf("wait for replacement sandbox: %v", err)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func newLifecyclePool(namespace, name string, min, max int32) *apiv1alpha1.SandboxPool {
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

func newSleepSandbox(namespace, name, pool string) *apiv1alpha1.Sandbox {
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
			Command: []string{"/bin/sleep", "3600"},
			PoolRef: pool,
		},
	}
}
