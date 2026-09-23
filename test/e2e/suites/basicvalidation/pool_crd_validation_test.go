package basicvalidation

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/test/e2e/support/suiteenv"
)

func TestSandboxPoolCRDValidation(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("sandbox-pool-crd-validation").
		WithLabel("suite", "basicvalidation").
		WithLabel("tier", "smoke").
		Assess("enforce canonical runtime schema and convention-only immutability", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			namespace := testSuite.AllocateNamespace("pool-validation")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			missingRuntime := sandboxPoolObject(namespace, "missing-runtime", map[string]any{
				"maxSandboxesPerPod": int64(1),
				"sandboxResources":   requiredPoolResources(),
				"capacity":           zeroPoolCapacity(),
				"fastletTemplate":    map[string]any{},
			})
			if err := k8sClient.Create(ctx, missingRuntime); err == nil || !apierrors.IsInvalid(err) {
				t.Fatalf("create Pool without runtime error = %v, want Invalid", err)
			}

			invalidRuntime := sandboxPoolObject(namespace, "invalid-runtime", map[string]any{
				"runtime":            "not-a-runtime",
				"maxSandboxesPerPod": int64(1),
				"sandboxResources":   requiredPoolResources(),
				"capacity":           zeroPoolCapacity(),
				"fastletTemplate":    map[string]any{},
			})
			if err := k8sClient.Create(ctx, invalidRuntime); err == nil || !apierrors.IsInvalid(err) {
				t.Fatalf("create Pool with invalid runtime error = %v, want Invalid", err)
			}

			pool := &apiv1alpha2.SandboxPool{
				ObjectMeta: metav1.ObjectMeta{Name: "valid-runtime-pool", Namespace: namespace},
				Spec: apiv1alpha2.SandboxPoolSpec{
					Runtime:            apiv1alpha2.RuntimeContainer,
					MaxSandboxesPerPod: 1,
					SandboxResources: apiv1alpha2.SandboxResourceProfile{
						CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi"), PIDs: 256,
					},
					Capacity:        apiv1alpha2.PoolCapacity{},
					FastletTemplate: corev1.PodTemplateSpec{},
				},
			}
			if err := k8sClient.Create(ctx, pool); err != nil {
				t.Fatalf("create valid Pool: %v", err)
			}

			// Runtime and sandboxResources immutability is convention-only:
			// these CRDs ship without CEL rules so legacy (< 1.25) API
			// servers can load them, so the API server accepts these edits.
			// This asserts the weakened contract: updates succeed.
			err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
				if err := k8sClient.Get(ctx, clientObjectKey(pool), pool); err != nil {
					return err
				}
				pool.Spec.Runtime = apiv1alpha2.RuntimeGVisor
				return k8sClient.Update(ctx, pool)
			})
			if err != nil {
				t.Fatalf("update runtime (immutability is convention-only) error = %v, want success", err)
			}
			err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
				if err := k8sClient.Get(ctx, clientObjectKey(pool), pool); err != nil {
					return err
				}
				pool.Spec.SandboxResources.Memory = resource.MustParse("2Gi")
				return k8sClient.Update(ctx, pool)
			})
			if err != nil {
				t.Fatalf("update sandboxResources (immutability is convention-only) error = %v, want success", err)
			}
			return ctx
		}).Feature()

	testSuite.Env().Test(t, feature)
}

func sandboxPoolObject(namespace, name string, spec map[string]any) *unstructured.Unstructured {
	pool := &unstructured.Unstructured{}
	pool.SetGroupVersionKind(schema.GroupVersionKind{
		Group: apiv1alpha2.GroupVersion.Group, Version: apiv1alpha2.GroupVersion.Version, Kind: "SandboxPool",
	})
	pool.SetNamespace(namespace)
	pool.SetName(name)
	pool.Object["spec"] = spec
	return pool
}

func zeroPoolCapacity() map[string]any {
	return map[string]any{"poolMin": int64(0), "poolMax": int64(0), "bufferMin": int64(0), "bufferMax": int64(0)}
}

func requiredPoolResources() map[string]any {
	return map[string]any{"cpu": "1", "memory": "1Gi", "pids": int64(256)}
}

func clientObjectKey(object metav1.Object) client.ObjectKey {
	return client.ObjectKey{Name: object.GetName(), Namespace: object.GetNamespace()}
}
