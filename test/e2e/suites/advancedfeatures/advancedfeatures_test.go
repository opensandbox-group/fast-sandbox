package advancedfeatures

import (
	"context"
	"strings"
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

func TestInfraProfileWiring(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("infra-profile-wiring").
		WithLabel("suite", "advancedfeatures").
		WithLabel("tier", "smoke").
		Assess("platform-owned InfraProfile is wired into the fastlet pod", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("infra")
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
					Name:      "injection-pool",
					Namespace: namespace,
				},
				Spec: apiv1alpha1.SandboxPoolSpec{
					Capacity: apiv1alpha1.PoolCapacity{
						PoolMin: 1,
						PoolMax: 1,
					},
					MaxSandboxesPerPod: 5,
					Runtime:            apiv1alpha1.RuntimeContainer,
					SandboxResources:   suiteenv.SmallSandboxResourceProfile(),
					InfraProfile:       "test-infra",
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

			poolWaitCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()
			if _, err := fixture.WaitForReadyFastletPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready fastlet pods: %v", err)
			}

			// Get the fastlet pod
			podList := &corev1.PodList{}
			if err := k8sClient.List(ctx, podList); err != nil {
				t.Fatalf("list pods: %v", err)
			}

			var fastletPod *corev1.Pod
			for _, pod := range podList.Items {
				if pod.Namespace == namespace && strings.Contains(pod.Name, "injection-pool") {
					fastletPod = &pod
					break
				}
			}

			if fastletPod == nil {
				t.Fatalf("fastlet pod not found")
			}

			if got := fastletPod.Labels["fast-sandbox.io/infra-profile"]; got != "test-infra" {
				t.Fatalf("infra profile label = %q, want test-infra", got)
			}
			if fastletPod.Annotations["fast-sandbox.io/infra-profile-hash"] == "" {
				t.Fatalf("infra profile hash annotation is empty")
			}
			var foundProfileEnv, foundInfraMount, foundInfraVolume bool
			for _, container := range fastletPod.Spec.Containers {
				if container.Name != "fastlet" {
					continue
				}
				for _, env := range container.Env {
					if env.Name == "FAST_SANDBOX_INFRA_PROFILE" && env.Value == "test-infra" {
						foundProfileEnv = true
					}
				}
				for _, mount := range container.VolumeMounts {
					if mount.Name == "infra-tools" && mount.MountPath == "/opt/fast-sandbox/infra" {
						foundInfraMount = true
					}
				}
			}
			for _, volume := range fastletPod.Spec.Volumes {
				if volume.Name == "infra-tools" && volume.EmptyDir != nil {
					foundInfraVolume = true
				}
			}
			if !foundProfileEnv || !foundInfraMount || !foundInfraVolume {
				t.Fatalf("InfraProfile wiring incomplete: env=%t mount=%t volume=%t", foundProfileEnv, foundInfraMount, foundInfraVolume)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}
