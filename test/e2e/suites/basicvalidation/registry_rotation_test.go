package basicvalidation

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/registryconfig"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"
)

func TestRegistryCredentialRotationReachesFastletWithoutPodReplacement(t *testing.T) {
	suiteenv.RequireBasic(t)
	feature := features.New("registry-credential-rotation").
		WithLabel("suite", "basic-validation").
		WithLabel("tier", "smoke").
		Assess("namespace Registry Secret rotation is projected and acknowledged without replacing Fastlets", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))
			namespace := testSuite.AllocateNamespace("registry-rotation")
			requireCreate(t, ctx, k8sClient, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			requireCreate(t, ctx, k8sClient, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: registryconfig.ConfigMapName, Namespace: namespace},
				Data: map[string]string{registryconfig.ConfigMapKey: `
registries:
  - host: registry.example.com
    repositoryPrefix: team-a
    secretRef:
      name: registry-reader
`},
			})
			requireCreate(t, ctx, k8sClient, registryCredentialSecret(namespace, "first"))
			pool := registryRotationPool(namespace)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create Registry rotation Pool: %v", err)
			}

			waitCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
			defer cancel()
			if _, err := fixture.WaitForReadyFastletPods(waitCtx, types.NamespacedName{
				Namespace: namespace, Name: pool.Name,
			}, 1); err != nil {
				t.Fatalf("wait for Registry rotation Fastlet: %v", err)
			}
			podUID := onlyPoolPodUID(t, ctx, k8sClient, namespace, pool.Name)
			initialRevision := waitForRegistryApplied(t, waitCtx, k8sClient, namespace, pool.Name, "", time.Time{})

			var credential corev1.Secret
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "registry-reader"}, &credential); err != nil {
				t.Fatalf("get Registry credential Secret: %v", err)
			}
			credential.Data[corev1.DockerConfigJsonKey] = registryCredentialPayload("rotated")
			rotatedAt := time.Now()
			if err := k8sClient.Update(ctx, &credential); err != nil {
				t.Fatalf("rotate Registry credential Secret: %v", err)
			}
			rotatedRevision := waitForRegistryApplied(t, waitCtx, k8sClient, namespace, pool.Name, initialRevision, rotatedAt)
			if rotatedRevision == initialRevision {
				t.Fatalf("Registry revision did not change after credential rotation")
			}
			if current := onlyPoolPodUID(t, ctx, k8sClient, namespace, pool.Name); current != podUID {
				t.Fatalf("Registry rotation replaced Fastlet Pod: before=%s after=%s", podUID, current)
			}
			return ctx
		}).
		Feature()
	testSuite.Env().Test(t, feature)
}

func registryRotationPool(namespace string) *apiv1alpha2.SandboxPool {
	return &apiv1alpha2.SandboxPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiv1alpha2.GroupVersion.String(), Kind: "SandboxPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "registry-rotation-pool", Namespace: namespace},
		Spec: apiv1alpha2.SandboxPoolSpec{
			Capacity:           apiv1alpha2.PoolCapacity{PoolMin: 1, PoolMax: 1},
			MaxSandboxesPerPod: 1,
			Runtime:            apiv1alpha2.RuntimeContainer,
			SandboxResources: apiv1alpha2.NewLegacySandboxResourceProfile(
				resource.MustParse("100m"), resource.MustParse("128Mi"), 64,
			),
			FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "fastlet", Image: suiteenv.FastletImage(), ImagePullPolicy: corev1.PullIfNotPresent,
			}}}},
		},
	}
}

func registryCredentialSecret(namespace, password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "registry-reader", Namespace: namespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: registryCredentialPayload(password)},
	}
}

func registryCredentialPayload(password string) []byte {
	return []byte(fmt.Sprintf(
		`{"auths":{"registry.example.com":{"username":"reader","password":%q}}}`,
		password,
	))
}

func waitForRegistryApplied(
	t *testing.T,
	ctx context.Context,
	k8sClient client.Client,
	namespace string,
	poolName string,
	previousRevision string,
	transitionedAfter time.Time,
) string {
	t.Helper()
	for {
		var compiledSecret corev1.Secret
		if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: poolName + "-registry"}, &compiledSecret); err == nil {
			compiled, parseErr := registryconfig.ParseCompiled(compiledSecret.Data[registryconfig.SecretKey])
			if parseErr == nil && compiled.Revision != "" && compiled.Revision != previousRevision {
				var pool apiv1alpha2.SandboxPool
				if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: poolName}, &pool); err == nil {
					for _, condition := range pool.Status.Conditions {
						transitioned := transitionedAfter.IsZero() || condition.LastTransitionTime.Time.After(transitionedAfter)
						if condition.Type == apiv1alpha2.PoolConditionRegistryReady && condition.Status == metav1.ConditionTrue &&
							condition.Reason == apiv1alpha2.ReasonRegistryAvailable && transitioned {
							return compiled.Revision
						}
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for Registry revision after %q: %v", previousRevision, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func onlyPoolPodUID(t *testing.T, ctx context.Context, k8sClient client.Client, namespace, poolName string) types.UID {
	t.Helper()
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods,
		client.InNamespace(namespace),
		client.MatchingLabels{"fast-sandbox.io/pool": poolName},
	); err != nil {
		t.Fatalf("list Pool Fastlets: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("Pool Fastlet count=%d, want 1", len(pods.Items))
	}
	return pods.Items[0].UID
}

func requireCreate(t *testing.T, ctx context.Context, k8sClient client.Client, object client.Object) {
	t.Helper()
	if err := k8sClient.Create(ctx, object); err != nil {
		t.Fatalf("create %T %s/%s: %v", object, object.GetNamespace(), object.GetName(), err)
	}
}
