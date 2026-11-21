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

func TestRegistryCredentialRotationIsProjectedWithoutPodReplacement(t *testing.T) {
	suiteenv.RequireBasic(t)
	feature := features.New("registry-credential-rotation").
		WithLabel("suite", "basic-validation").
		WithLabel("tier", "smoke").
		Assess("namespace Registry Secret rotation updates the mounted credentials without replacing Fastlets", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
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
			waitForRegistryProjected(t, waitCtx, k8sClient, namespace, pool.Name, "first")

			var credential corev1.Secret
			if err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "registry-reader"}, &credential); err != nil {
				t.Fatalf("get Registry credential Secret: %v", err)
			}
			credential.Data[corev1.DockerConfigJsonKey] = registryCredentialPayload("rotated")
			if err := k8sClient.Update(ctx, &credential); err != nil {
				t.Fatalf("rotate Registry credential Secret: %v", err)
			}
			waitForRegistryProjected(t, waitCtx, k8sClient, namespace, pool.Name, "rotated")
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

func waitForRegistryProjected(
	t *testing.T,
	ctx context.Context,
	k8sClient client.Client,
	namespace string,
	poolName string,
	password string,
) {
	t.Helper()
	for {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"fast-sandbox.io/pool": poolName}); err == nil && len(pods.Items) == 1 {
			content, err := kubectl(ctx, "exec", "-n", namespace, pods.Items[0].Name, "-c", "fastlet", "--", "cat", registryconfig.MountPath)
			if err == nil {
				compiled, parseErr := registryconfig.ParseCompiled(content)
				if parseErr == nil {
					credential, found := compiled.Match("registry.example.com/team-a/image:v1")
					if found && credential.Username == "reader" && credential.Password == password {
						return
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for Registry credentials to be projected into Fastlet: %v", ctx.Err())
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
