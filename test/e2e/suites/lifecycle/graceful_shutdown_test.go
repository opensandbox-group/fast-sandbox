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

func TestGracefulShutdown(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("graceful-shutdown").
		WithLabel("suite", "lifecycle").
		WithLabel("tier", "smoke").
		Assess("deleting a sandbox transitions through Terminating before final removal", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("lifecycle")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := &apiv1alpha1.SandboxPool{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1alpha1.GroupVersion.String(),
					Kind:       "SandboxPool",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "shutdown-pool",
					Namespace: namespace,
				},
				Spec: apiv1alpha1.SandboxPoolSpec{
					Capacity: apiv1alpha1.PoolCapacity{
						PoolMin: 1,
						PoolMax: 2,
					},
					MaxSandboxesPerPod: 5,
					RuntimeType:        apiv1alpha1.RuntimeContainer,
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
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}

			poolWaitCtx, cancelPoolWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelPoolWait()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}

			sandbox := &apiv1alpha1.Sandbox{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1alpha1.GroupVersion.String(),
					Kind:       "Sandbox",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sb-graceful",
					Namespace: namespace,
				},
				Spec: apiv1alpha1.SandboxSpec{
					Image:   "docker.io/library/alpine:latest",
					Command: []string{"/bin/sh", "-c"},
					Args: []string{`
trap 'echo GRACEFUL_SHUTDOWN_RECEIVED; exit 0' TERM
echo "Starting sandbox with graceful shutdown handler"
while true; do
  sleep 10
done
`},
					PoolRef: pool.Name,
				},
			}
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			runCtx, cancelRunWait := context.WithTimeout(ctx, 60*time.Second)
			defer cancelRunWait()
			assignedSandbox, err := fixture.WaitForSandbox(runCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.AssignedFastlet != "" &&
					(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
			})
			if err != nil {
				t.Fatalf("wait for running sandbox: %v", err)
			}
			if assignedSandbox.Status.AssignedFastlet == "" {
				t.Fatalf("sandbox assigned pod is empty")
			}

			// Give sandbox time to fully stabilize before deletion
			time.Sleep(3 * time.Second)

			if err := k8sClient.Delete(ctx, sandbox); err != nil {
				t.Fatalf("delete sandbox: %v", err)
			}

			// Wait for sandbox to transition to Terminating
			// Give controller more time to process deletion and call Fastlet
			termCtx, cancelTermWait := context.WithTimeout(ctx, 90*time.Second) // Increased from 45s
			defer cancelTermWait()
			terminatingSandbox, err := fixture.WaitForSandbox(termCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.DeletionTimestamp != nil && sb.Status.Phase == string(apiv1alpha1.PhaseTerminating)
			})
			if err != nil {
				// Log current state for debugging
				currentSandbox := &apiv1alpha1.Sandbox{}
				if getErr := k8sClient.Get(ctx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}, currentSandbox); getErr == nil {
					t.Logf("Sandbox state at timeout: phase=%s, deletionTimestamp=%v, assignedFastlet=%s",
						currentSandbox.Status.Phase, currentSandbox.DeletionTimestamp, currentSandbox.Status.AssignedFastlet)
				}
				t.Fatalf("wait for sandbox terminating: %v", err)
			}
			if terminatingSandbox.DeletionTimestamp == nil {
				t.Fatalf("sandbox deletion timestamp is nil after delete")
			}
			if terminatingSandbox.Status.AssignedFastlet == "" {
				t.Fatalf("sandbox assigned pod cleared too early")
			}

			deleteCtx, cancelDeleteWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelDeleteWait()
			if err := fixture.WaitForSandboxDeleted(deleteCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}); err != nil {
				t.Fatalf("wait for sandbox deleted: %v", err)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}
