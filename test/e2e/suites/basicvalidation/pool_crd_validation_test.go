package basicvalidation

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"
)

func TestSandboxPoolCRDValidation(t *testing.T) {
	suiteenv.RequireBasic(t)

	feature := features.New("sandbox-pool-crd-validation").
		WithLabel("suite", "basicvalidation").
		WithLabel("tier", "smoke").
		Assess("enforce canonical runtime schema and convention-only immutability", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))
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
					SandboxResources: apiv1alpha2.NewLegacySandboxResourceProfile(
						resource.MustParse("1"), resource.MustParse("1Gi"), 256,
					),
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
				memory := resource.MustParse("2Gi")
				pool.Spec.SandboxResources.Memory = &memory
				return k8sClient.Update(ctx, pool)
			})
			if err != nil {
				t.Fatalf("update sandboxResources (immutability is convention-only) error = %v, want success", err)
			}

			legacyResources := corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi"),
				},
			}
			legacy := &apiv1alpha2.SandboxPool{
				ObjectMeta: metav1.ObjectMeta{Name: "legacy-explicit-owner-resources", Namespace: namespace},
				Spec: apiv1alpha2.SandboxPoolSpec{
					Runtime: apiv1alpha2.RuntimeContainer, MaxSandboxesPerPod: 1,
					SandboxResources: apiv1alpha2.NewLegacySandboxResourceProfile(resource.MustParse("1"), resource.MustParse("1Gi"), 256),
					Capacity:         apiv1alpha2.PoolCapacity{PoolMin: 1, PoolMax: 1},
					FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: "fastlet", Image: suiteenv.FastletImage(), Resources: legacyResources,
					}}}},
				},
			}
			if _, err := fixture.CreateSandboxPool(ctx, namespace, legacy); err != nil {
				t.Fatalf("create LegacySlot Pool with explicit runtime-owner resources: %v", err)
			}
			legacyCtx, cancelLegacy := context.WithTimeout(ctx, 90*time.Second)
			defer cancelLegacy()
			if _, err := fixture.WaitForReadyFastletPods(legacyCtx, clientObjectKey(legacy), 1); err != nil {
				t.Fatalf("wait for LegacySlot Fastlet: %v", err)
			}
			var legacyPods corev1.PodList
			if err := k8sClient.List(ctx, &legacyPods, client.InNamespace(namespace), client.MatchingLabels{"fast-sandbox.io/pool": legacy.Name}); err != nil {
				t.Fatalf("list LegacySlot Fastlets: %v", err)
			}
			if len(legacyPods.Items) != 1 || !resourceRequirementsEqual(legacyPods.Items[0].Spec.Containers[0].Resources, legacyResources) {
				t.Fatalf("LegacySlot runtime-owner resources were not preserved: %+v", legacyPods.Items)
			}

			resourceAware := &apiv1alpha2.SandboxPool{
				ObjectMeta: metav1.ObjectMeta{Name: "resource-aware-explicit-owner-resources", Namespace: namespace},
				Spec: apiv1alpha2.SandboxPoolSpec{
					Runtime: apiv1alpha2.RuntimeContainer, MaxSandboxesPerPod: 2,
					SandboxResources: apiv1alpha2.NewResourceAwareSandboxResourceProfile(
						resource.MustParse("500m"), resource.MustParse("256Mi"), resource.MustParse("2"), resource.MustParse("1.25"), 64,
					),
					Capacity: apiv1alpha2.PoolCapacity{PoolMin: 1, PoolMax: 1},
					FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
						Name: "fastlet", Image: suiteenv.FastletImage(), Resources: legacyResources,
					}}}},
				},
			}
			if _, err := fixture.CreateSandboxPool(ctx, namespace, resourceAware); err != nil {
				t.Fatalf("create ResourceAware Pool with invalid runtime-owner resources: %v", err)
			}
			invalidCtx, cancelInvalid := context.WithTimeout(ctx, 60*time.Second)
			defer cancelInvalid()
			invalidPool, err := fixture.WaitForPoolCondition(invalidCtx, clientObjectKey(resourceAware), apiv1alpha2.PoolConditionRuntimeReady, metav1.ConditionFalse)
			if err != nil {
				t.Fatalf("wait for ResourceAware resource contract rejection: %v", err)
			}
			condition := apiMeta.FindStatusCondition(invalidPool.Status.Conditions, apiv1alpha2.PoolConditionRuntimeReady)
			if condition == nil || condition.Reason != apiv1alpha2.ReasonResourceProfileInvalid {
				t.Fatalf("RuntimeReady condition=%+v, want ResourceProfileInvalid", condition)
			}
			var invalidPods corev1.PodList
			if err := k8sClient.List(ctx, &invalidPods, client.InNamespace(namespace), client.MatchingLabels{"fast-sandbox.io/pool": resourceAware.Name}); err != nil {
				t.Fatalf("list invalid ResourceAware Fastlets: %v", err)
			}
			if len(invalidPods.Items) != 0 {
				t.Fatalf("invalid ResourceAware Pool created %d Fastlet Pods, want 0", len(invalidPods.Items))
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

func resourceRequirementsEqual(got, want corev1.ResourceRequirements) bool {
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		gotRequest, wantRequest := got.Requests[name], want.Requests[name]
		gotLimit, wantLimit := got.Limits[name], want.Limits[name]
		if gotRequest.Cmp(wantRequest) != 0 || gotLimit.Cmp(wantLimit) != 0 {
			return false
		}
	}
	return true
}
