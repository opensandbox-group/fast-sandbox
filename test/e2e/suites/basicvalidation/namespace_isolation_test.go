package basicvalidation

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

func TestNamespaceIsolation(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("namespace-isolation").
		WithLabel("suite", "basicvalidation").
		WithLabel("tier", "smoke").
		Assess("pool only schedules sandboxes from the same namespace", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespaceA := testSuite.AllocateNamespace("ns-a")
			namespaceB := testSuite.AllocateNamespace("ns-b")
			for _, ns := range []string{namespaceA, namespaceB} {
				if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
					t.Fatalf("create namespace %s: %v", ns, err)
				}
				defer suiteenv.DeleteNamespace(ctx, t, k8sClient, ns)
			}

			pool := &apiv1alpha1.SandboxPool{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1alpha1.GroupVersion.String(),
					Kind:       "SandboxPool",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: namespaceA,
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
			if _, err := fixture.CreateSandboxPool(ctx, namespaceA, pool); err != nil {
				t.Fatalf("create sandbox pool: %v", err)
			}
			if _, err := fixture.WaitForReadyFastletPods(ctx, types.NamespacedName{Name: pool.Name, Namespace: namespaceA}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}

			sameNamespaceSandbox := &apiv1alpha1.Sandbox{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1alpha1.GroupVersion.String(),
					Kind:       "Sandbox",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sb-same-ns",
					Namespace: namespaceA,
				},
				Spec: apiv1alpha1.SandboxSpec{
					Image:   "docker.io/library/alpine:latest",
					Command: []string{"/bin/sleep", "60"},
					PoolRef: pool.Name,
				},
			}
			if _, err := fixture.CreateSandbox(ctx, namespaceA, sameNamespaceSandbox); err != nil {
				t.Fatalf("create same-namespace sandbox: %v", err)
			}
			if _, err := fixture.WaitForSandbox(ctx, types.NamespacedName{Name: sameNamespaceSandbox.Name, Namespace: namespaceA}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.AssignedFastlet != "" &&
					(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
			}); err != nil {
				t.Fatalf("wait for same-namespace sandbox assignment: %v", err)
			}

			crossNamespaceSandbox := &apiv1alpha1.Sandbox{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1alpha1.GroupVersion.String(),
					Kind:       "Sandbox",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sb-cross-ns",
					Namespace: namespaceB,
				},
				Spec: apiv1alpha1.SandboxSpec{
					Image:   "docker.io/library/alpine:latest",
					Command: []string{"/bin/sleep", "60"},
					PoolRef: pool.Name,
				},
			}
			if _, err := fixture.CreateSandbox(ctx, namespaceB, crossNamespaceSandbox); err != nil {
				t.Fatalf("create cross-namespace sandbox: %v", err)
			}
			if err := fixture.EnsureSandboxRemainsUnassigned(ctx, types.NamespacedName{Name: crossNamespaceSandbox.Name, Namespace: namespaceB}, 15*time.Second); err != nil {
				t.Fatalf("ensure cross-namespace sandbox remains unassigned: %v", err)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}
