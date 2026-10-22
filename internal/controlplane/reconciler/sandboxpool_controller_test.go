package reconciler

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	orchestration "fast-sandbox/internal/controlplane/orchestrator"
	"fast-sandbox/internal/controlplane/placement"
	"fast-sandbox/internal/fastlet/podcgroup"
	"fast-sandbox/internal/fastletsettings"
	"fast-sandbox/internal/nodecleanup"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/registryconfig"
	"fast-sandbox/internal/runtimeenv"
)

func (r *SandboxPoolReconciler) planResourceAwareScale(
	pool *apiv1alpha2.SandboxPool,
	pods []corev1.Pod,
	pendingSandboxes int32,
	plannedPod *corev1.Pod,
	observed map[string]placement.FastletInfo,
	now time.Time,
) resourceAwareScalePlan {
	requests, _ := pool.Spec.SandboxResources.RequestResources()
	pendingDemand := placement.ResourceDemand{CPU: requests.CPU.DeepCopy(), Memory: requests.Memory.DeepCopy()}
	pendingDemand.CPU.Mul(int64(pendingSandboxes))
	pendingDemand.Memory.Mul(int64(pendingSandboxes))
	return r.planResourceAwareScaleWithDemand(pool, pods, pendingSandboxes, pendingDemand, plannedPod, observed, now)
}

func TestRegistryConfigCompilesSameNamespaceSecretsAndPreservesLastValidRevision(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	pool := &apiv1alpha2.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "tenant-a", UID: types.UID("pool-a-uid")},
	}
	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: registryconfig.ConfigMapName, Namespace: "tenant-a"},
		Data: map[string]string{registryconfig.ConfigMapKey: `
registries:
  - host: registry.example.com
    repositoryPrefix: team-a
    secretRef:
      name: registry-team-a
`},
	}
	credential := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "registry-team-a", Namespace: "tenant-a"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(
			`{"auths":{"registry.example.com":{"username":"alice","password":"secret"}}}`,
		)},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool, source, credential).Build()
	reconciler := &SandboxPoolReconciler{Client: k8sClient, Scheme: scheme}

	compiled, err := reconciler.ensureRegistrySecret(context.Background(), pool)
	require.NoError(t, err)
	selected, found := compiled.Match("registry.example.com/team-a/runner:v1")
	require.True(t, found)
	require.Equal(t, "alice", selected.Username)
	var projected corev1.Secret
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{
		Namespace: "tenant-a", Name: registrySecretName("pool-a"),
	}, &projected))
	previous := append([]byte(nil), projected.Data[registryconfig.SecretKey]...)

	source.Data[registryconfig.ConfigMapKey] = "registries:\n  - unexpected: value\n"
	require.NoError(t, k8sClient.Update(context.Background(), source))
	_, err = reconciler.ensureRegistrySecret(context.Background(), pool)
	require.ErrorContains(t, err, "unexpected")
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{
		Namespace: "tenant-a", Name: registrySecretName("pool-a"),
	}, &projected))
	require.Equal(t, previous, projected.Data[registryconfig.SecretKey], "an invalid update must retain the last compiled Secret")
}

func TestPoolRegistryConditionConvergesWithoutRepeatedStatusWrites(t *testing.T) {
	for _, test := range []struct {
		name                 string
		fastlet              bool
		applied              bool
		infraInvalid         bool
		initialRegistryValid bool
	}{
		{name: "empty pool"},
		{name: "configuration propagating", fastlet: true},
		{name: "configuration applied", fastlet: true, applied: true},
		{name: "registry recovered with invalid infra", infraInvalid: true, fastlet: true},
		{name: "new pool with invalid infra", infraInvalid: true, initialRegistryValid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			require.NoError(t, apiv1alpha2.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			pool := &apiv1alpha2.SandboxPool{
				ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default", UID: "pool-a-uid", Generation: 1},
				Spec: apiv1alpha2.SandboxPoolSpec{
					Runtime: apiv1alpha2.RuntimeContainer, MaxSandboxesPerPod: 5,
					SandboxResources: testSandboxResources(),
					FastletTemplate:  corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fastlet", Image: "fastlet:test"}}}},
				},
				Status: apiv1alpha2.SandboxPoolStatus{Conditions: []metav1.Condition{{
					Type: apiv1alpha2.PoolConditionInfraReady, Status: metav1.ConditionFalse,
					Reason: apiv1alpha2.ReasonInfraComponentsInvalid, Message: "previous invalid configuration",
				}}},
			}
			if test.fastlet {
				pool.Spec.Capacity.PoolMin = 1
			}
			if test.infraInvalid {
				component := testInlineComponent()
				component.Artifact = nil
				component.Delivery = apiv1alpha2.InfraDeliveryHostProcess
				pool.Spec.InfraComponents = []apiv1alpha2.InfraComponent{component}
				// The declaration is valid, but the container runtime cannot
				// deliver host-process components.
				require.NoError(t, pool.Spec.ValidateInfraComponents())
			}
			source := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: registryconfig.ConfigMapName, Namespace: pool.Namespace},
				Data:       map[string]string{registryconfig.ConfigMapKey: "registries: ["},
			}
			if test.initialRegistryValid {
				source.Data[registryconfig.ConfigMapKey] = "registries: []"
			}
			statusWrites := 0
			k8sClient := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&apiv1alpha2.SandboxPool{}).
				WithObjects(pool, source).
				WithInterceptorFuncs(interceptor.Funcs{
					SubResourceUpdate: func(ctx context.Context, c client.Client, name string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
						err := c.SubResource(name).Update(ctx, obj, opts...)
						if _, ok := obj.(*apiv1alpha2.SandboxPool); err == nil && ok && name == "status" {
							statusWrites++
						}
						return err
					},
				}).Build()
			registry := placement.NewInMemoryRegistry()
			reconciler := &SandboxPoolReconciler{Client: k8sClient, Scheme: scheme, Catalog: runtimecatalog.Builtin(), Registry: registry}
			if test.fastlet {
				plan, err := runtimeenv.ResolveDefault(reconciler.Catalog, pool.Spec.Runtime)
				require.NoError(t, err)
				// Existing Fastlets can outlive a later invalid infra update.
				podPool := pool.DeepCopy()
				if test.infraInvalid {
					podPool.Spec.InfraComponents = nil
				}
				pod, err := reconciler.constructPodWithRuntimePlan(podPool, plan)
				require.NoError(t, err)
				pod.Name, pod.GenerateName, pod.UID = "fastlet-a", "", "pod-a"
				pod.Status.Phase = corev1.PodRunning
				pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
				require.NoError(t, k8sClient.Create(ctx, pod))
				revision := "previous-revision"
				if test.applied {
					compiled, err := registryconfig.NewCompiled(nil)
					require.NoError(t, err)
					revision = compiled.Revision
				}
				seedControllerRegistry(t, registry, placement.FastletInfo{
					ID: "default/fastlet-a", Namespace: pool.Namespace, PoolName: pool.Name,
					PodName: pod.Name, PodUID: string(pod.UID), PodReady: true, RuntimeReady: true,
					LastHeartbeat: time.Now(), RegistryRevision: revision,
				})
			}

			// Registry validation and rollout must reflect the current
			// configuration independently of infra validation.
			_, err := reconciler.Reconcile(ctx, poolRequest(pool))
			require.NoError(t, err)
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), pool))
			condition := apiMeta.FindStatusCondition(pool.Status.Conditions, apiv1alpha2.PoolConditionRegistryReady)
			require.NotNil(t, condition)
			if test.initialRegistryValid {
				require.Equal(t, metav1.ConditionTrue, condition.Status)
				require.Equal(t, apiv1alpha2.ReasonRegistryAvailable, condition.Reason)
			} else {
				require.Equal(t, metav1.ConditionFalse, condition.Status)
				require.Equal(t, apiv1alpha2.ReasonRegistryInvalid, condition.Reason)
			}
			// Removing the source selects the valid anonymous-registry default.
			require.NoError(t, k8sClient.Delete(ctx, source))
			_, err = reconciler.Reconcile(ctx, poolRequest(pool))
			require.NoError(t, err)
			require.NoError(t, k8sClient.Get(ctx, client.ObjectKeyFromObject(pool), pool))
			condition = apiMeta.FindStatusCondition(pool.Status.Conditions, apiv1alpha2.PoolConditionRegistryReady)
			require.NotNil(t, condition)
			if test.fastlet && !test.applied {
				require.Equal(t, metav1.ConditionFalse, condition.Status)
				require.Equal(t, "RegistryPropagating", condition.Reason)
			} else {
				require.Equal(t, metav1.ConditionTrue, condition.Status)
				require.Equal(t, apiv1alpha2.ReasonRegistryAvailable, condition.Reason)
			}
			infraCondition := apiMeta.FindStatusCondition(pool.Status.Conditions, apiv1alpha2.PoolConditionInfraReady)
			require.NotNil(t, infraCondition)
			if test.infraInvalid {
				require.Equal(t, metav1.ConditionFalse, infraCondition.Status)
				require.Equal(t, apiv1alpha2.ReasonInfraComponentsInvalid, infraCondition.Reason)
			} else {
				require.Equal(t, metav1.ConditionTrue, infraCondition.Status)
			}

			// Check actual writes: comparing only final status would miss a
			// condition that flips twice and ends each reconcile unchanged.
			statusWrites = 0
			for range 3 {
				_, err = reconciler.Reconcile(ctx, poolRequest(pool))
				require.NoError(t, err)
			}
			require.Zero(t, statusWrites, "stable reconciles must not publish intermediate conditions")
		})
	}
}

func TestRegistryWriteCredentialCompilesFromWriteSecretRef(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	pool := &apiv1alpha2.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "tenant-a", UID: types.UID("pool-a-uid")},
	}
	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: registryconfig.ConfigMapName, Namespace: "tenant-a"},
		Data: map[string]string{registryconfig.ConfigMapKey: `
registries:
  - host: registry.example.com
    secretRef:
      name: registry-team-a
    writeSecretRef:
      name: artifact-store-rw
`},
	}
	credential := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "registry-team-a", Namespace: "tenant-a"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(
			`{"auths":{"registry.example.com":{"username":"alice","password":"secret"}}}`,
		)},
	}
	writeSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "artifact-store-rw", Namespace: "tenant-a"},
		Data: map[string][]byte{
			"accessKeyId": []byte(" writer-key "), "secretAccessKey": []byte("writer-secret"),
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool, source, credential, writeSecret).Build()
	reconciler := &SandboxPoolReconciler{Client: k8sClient, Scheme: scheme}

	compiled, err := reconciler.ensureRegistrySecret(context.Background(), pool)
	require.NoError(t, err)
	selected, found := compiled.Match("registry.example.com/app:v1")
	require.True(t, found)
	require.Equal(t, "writer-key", selected.WriteUsername, "the write pair is trimmed and compiled in")
	require.Equal(t, "writer-secret", selected.WritePassword)

	// A missing write secret is rejected (the last compiled revision is
	// retained by the existing invalid-update path).
	require.NoError(t, k8sClient.Delete(context.Background(), writeSecret))
	_, err = reconciler.ensureRegistrySecret(context.Background(), pool)
	require.ErrorContains(t, err, "artifact-store-rw")
}

func TestRegistryWriteSecretRefRequiresName(t *testing.T) {
	_, err := registryconfig.NormalizeAndValidate(registryconfig.Config{Registries: []registryconfig.RegistryRule{{
		Host: "registry.example.com", SecretRef: registryconfig.SecretRef{Name: "ro"},
		WriteSecretRef: &registryconfig.SecretRef{},
	}}})
	require.ErrorContains(t, err, "writeSecretRef")
}

type recordingDrainer struct {
	mu       sync.Mutex
	requests []fastletapi.SetDrainingRequest
}

func (d *recordingDrainer) SetDraining(_ context.Context, _ string, request *fastletapi.SetDrainingRequest) (*fastletapi.SetDrainingResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests = append(d.requests, *request)
	return &fastletapi.SetDrainingResponse{Draining: request.Draining}, nil
}

func TestResolveRuntimeProfileUsesCanonicalRuntime(t *testing.T) {
	reconciler := &SandboxPoolReconciler{Catalog: runtimecatalog.Builtin()}
	canonical, err := reconciler.resolveRuntimeProfile(&apiv1alpha2.SandboxPool{
		Spec: apiv1alpha2.SandboxPoolSpec{Runtime: apiv1alpha2.RuntimeKataFc},
	})
	require.NoError(t, err)
	require.Equal(t, apiv1alpha2.RuntimeKataFc, canonical.Name)
	_, err = reconciler.resolveRuntimeProfile(&apiv1alpha2.SandboxPool{})
	require.Error(t, err)
}

func TestResolveRuntimePlanUsesPlatformConfigMap(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: runtimeenv.ConfigMapName, Namespace: runtimeenv.SystemNamespace},
		Data: map[string]string{runtimeenv.ConfigMapKey: `
version: v1alpha2
environments:
  custom:
    nodeSelector:
      example.com/runtime: custom
    containerd:
      namespace: custom
      root: /srv/containerd
    kubelet:
      root: /srv/kubelet
    runtimes:
      container:
        handler: io.containerd.custom.v2
`},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source).Build()
	reconciler := &SandboxPoolReconciler{Client: k8sClient, Scheme: scheme, Catalog: runtimecatalog.Builtin()}
	pool := &apiv1alpha2.SandboxPool{Spec: apiv1alpha2.SandboxPoolSpec{Runtime: apiv1alpha2.RuntimeContainer}}

	plan, err := reconciler.resolveRuntimePlan(context.Background(), pool)
	require.NoError(t, err)
	require.Equal(t, "custom", plan.Environment)
	require.Equal(t, "custom", plan.Profile.Containerd.Namespace)
	require.Equal(t, "io.containerd.custom.v2", plan.Profile.Containerd.Handler)
	require.Equal(t, "/srv/kubelet", plan.Kubelet.Root)
	require.Equal(t, "custom", plan.Profile.Deployment.NodeSelector["example.com/runtime"])
}

func seedControllerRegistry(t *testing.T, registry *placement.InMemoryRegistry, info placement.FastletInfo) {
	t.Helper()
	registry.UpsertPod(info)
	statuses := make([]fastletapi.SandboxStatus, 0, len(info.SandboxStatuses))
	for _, status := range info.SandboxStatuses {
		statuses = append(statuses, status)
	}
	require.NoError(t, registry.ApplyHeartbeat(info.ID, info.PodUID, &fastletapi.HeartbeatResponse{
		FastletStatus: fastletapi.FastletStatus{
			FastletPodUID: info.PodUID, RuntimeReady: info.RuntimeReady, InfraReady: info.InfraReady,
			RuntimeProfileHash: info.RuntimeProfileHash, Admission: info.Admission, SandboxStatuses: statuses,
			InfraRevision: info.InfraRevision, RegistryRevision: info.RegistryRevision,
			PreparedArtifacts: info.PreparedArtifacts, WarmImages: info.WarmImages,
		},
		Sequence: 1, Cache: fastletapi.CacheSnapshot{Epoch: "test", Revision: 1, Full: true, Complete: true},
	}, info.LastHeartbeat))
}

func TestRuntimeCapabilityConditionAggregatesExactChildHeartbeat(t *testing.T) {
	registry := placement.NewInMemoryRegistry()
	now := time.Now()
	seedControllerRegistry(t, registry, placement.FastletInfo{
		ID: "default/fastlet-ready", Namespace: "default", PoolName: "pool-a",
		PodName: "fastlet-ready", PodUID: "uid-ready", PodReady: true, RuntimeReady: true, LastHeartbeat: now,
	})
	seedControllerRegistry(t, registry, placement.FastletInfo{
		ID: "default/fastlet-unready", Namespace: "default", PoolName: "pool-a",
		PodName: "fastlet-unready", PodUID: "uid-unready", PodReady: true, RuntimeReady: false, LastHeartbeat: now,
	})
	seedControllerRegistry(t, registry, placement.FastletInfo{
		ID: "default/stale-identity", Namespace: "default", PoolName: "pool-a",
		PodName: "fastlet-ready", PodUID: "old-uid", PodReady: true, RuntimeReady: true, LastHeartbeat: now,
	})
	reconciler := &SandboxPoolReconciler{Registry: registry}
	pool := &apiv1alpha2.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default", Generation: 7}}
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "fastlet-ready", Namespace: "default", UID: types.UID("uid-ready")}},
		{ObjectMeta: metav1.ObjectMeta{Name: "fastlet-unready", Namespace: "default", UID: types.UID("uid-unready")}},
	}

	condition, ready := reconciler.runtimeCapabilityCondition(pool, pods)
	require.Equal(t, int32(1), ready)
	require.Equal(t, metav1.ConditionTrue, condition.Status)
	require.Equal(t, apiv1alpha2.ReasonRuntimeAvailable, condition.Reason)
	require.Equal(t, int64(7), condition.ObservedGeneration)

	registry.RemoveIfPodUID("default/fastlet-ready", "uid-ready")
	condition, ready = reconciler.runtimeCapabilityCondition(pool, pods)
	require.Zero(t, ready)
	require.Equal(t, metav1.ConditionFalse, condition.Status)
	require.Equal(t, apiv1alpha2.ReasonRuntimeUnavailable, condition.Reason)
}

func TestRuntimeCapabilityConditionWaitsForHeartbeat(t *testing.T) {
	registry := placement.NewInMemoryRegistry()
	registry.UpsertPod(placement.FastletInfo{
		ID: "default/fastlet-a", Namespace: "default", PoolName: "pool-a",
		PodName: "fastlet-a", PodUID: "uid-a", PodReady: true,
	})
	reconciler := &SandboxPoolReconciler{Registry: registry}
	pool := &apiv1alpha2.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"}}
	pods := []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "fastlet-a", Namespace: "default", UID: types.UID("uid-a")}}}

	condition, ready := reconciler.runtimeCapabilityCondition(pool, pods)
	require.Zero(t, ready)
	require.Equal(t, metav1.ConditionFalse, condition.Status)
	require.Equal(t, apiv1alpha2.ReasonRuntimeCapabilityPending, condition.Reason)
}

func TestResourceAwareDesiredPodsUsesEffectiveFreeCapacityAndInFlightOnce(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	pool := resourceAwareScalingPool()
	planned := resourceAwarePlannedPod()
	pods := []corev1.Pod{
		*fastletPod("fastlet-open", "uid-open", "10.0.0.1"),
		*fastletPod("fastlet-blocked", "uid-blocked", "10.0.0.2"),
	}
	registry := placement.NewInMemoryRegistry()
	seedControllerRegistry(t, registry, resourceAwareControllerFastlet("fastlet-open", "uid-open", 2, fastletapi.AdmissionStateOpen, now))
	seedControllerRegistry(t, registry, resourceAwareControllerFastlet("fastlet-blocked", "uid-blocked", 1, fastletapi.AdmissionStateBlocked, now))
	reconciler := &SandboxPoolReconciler{Registry: registry, Now: func() time.Time { return now }}

	observed := resourceFastletsForPoolChildren(registry, pool, pods)
	require.Equal(t, int32(3), reconciler.planResourceAwareScale(pool, pods, 2, planned, observed, now).desiredPods,
		"two pending Sandboxes plus buffer require one replacement Fastlet when a peer is blocked")

	inFlight := fastletPod("fastlet-starting", "uid-starting", "")
	inFlight.Status.Phase = corev1.PodPending
	inFlight.CreationTimestamp = metav1.NewTime(now.Add(-time.Minute))
	pods = append(pods, *inFlight)
	observed = resourceFastletsForPoolChildren(registry, pool, pods)
	require.Equal(t, int32(3), reconciler.planResourceAwareScale(pool, pods, 2, planned, observed, now).desiredPods,
		"the starting Pod contributes nominal capacity exactly once")
}

func TestResourceAwareDesiredPodsUsesBufferMaxForEmptyScaleDown(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	pool := resourceAwareScalingPool()
	pool.Spec.Capacity.BufferMin = 2
	pool.Spec.Capacity.BufferMax = 4
	planned := resourceAwarePlannedPod()
	pods := []corev1.Pod{
		*fastletPod("fastlet-a", "uid-a", "10.0.0.1"),
		*fastletPod("fastlet-b", "uid-b", "10.0.0.2"),
	}
	registry := placement.NewInMemoryRegistry()
	seedControllerRegistry(t, registry, resourceAwareControllerFastlet("fastlet-a", "uid-a", 0, fastletapi.AdmissionStateOpen, now))
	seedControllerRegistry(t, registry, resourceAwareControllerFastlet("fastlet-b", "uid-b", 0, fastletapi.AdmissionStateOpen, now))
	reconciler := &SandboxPoolReconciler{Registry: registry, Now: func() time.Time { return now }}

	observed := resourceFastletsForPoolChildren(registry, pool, pods)
	require.Equal(t, int32(1), reconciler.planResourceAwareScale(pool, pods, 0, planned, observed, now).desiredPods)
}

func TestResourceAwareScalePlanKeepsSelectedFastletIdentity(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	pool := resourceAwareScalingPool()
	pool.Spec.Capacity.BufferMin = 0
	pool.Spec.Capacity.BufferMax = 0
	planned := resourceAwarePlannedPod()
	pods := []corev1.Pod{
		*fastletPod("fastlet-healthy", "uid-healthy", "10.0.0.1"),
		*fastletPod("fastlet-blocked", "uid-blocked", "10.0.0.2"),
	}
	registry := placement.NewInMemoryRegistry()
	seedControllerRegistry(t, registry, resourceAwareControllerFastlet("fastlet-healthy", "uid-healthy", 0, fastletapi.AdmissionStateOpen, now))
	seedControllerRegistry(t, registry, resourceAwareControllerFastlet("fastlet-blocked", "uid-blocked", 0, fastletapi.AdmissionStateBlocked, now))
	reconciler := &SandboxPoolReconciler{Registry: registry, Now: func() time.Time { return now }}

	observed := resourceFastletsForPoolChildren(registry, pool, pods)
	plan := reconciler.planResourceAwareScale(pool, pods, 0, planned, observed, now)
	require.Equal(t, int32(1), plan.desiredPods)
	require.Equal(t, []string{"fastlet-blocked/uid-blocked"}, plan.drainCandidates,
		"the Pod that contributed zero effective capacity must remain the drain target")
}

func TestResourceAwareScalePlanPreservesActiveDrainTarget(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	pool := resourceAwareScalingPool()
	planned := resourceAwarePlannedPod()
	draining := fastletPod("fastlet-draining", "uid-draining", "10.0.0.1")
	draining.Annotations = map[string]string{
		placement.AnnotationDraining:    "true",
		placement.AnnotationDrainReason: placement.DrainReasonScaleDown,
	}
	open := fastletPod("fastlet-open", "uid-open", "10.0.0.2")
	registry := placement.NewInMemoryRegistry()
	drainingInfo := resourceAwareControllerFastlet("fastlet-draining", "uid-draining", 0, fastletapi.AdmissionStateOpen, now)
	seedControllerRegistry(t, registry, drainingInfo)
	seedControllerRegistry(t, registry, resourceAwareControllerFastlet("fastlet-open", "uid-open", 2, fastletapi.AdmissionStateOpen, now))
	reconciler := &SandboxPoolReconciler{Registry: registry, Now: func() time.Time { return now }}

	pods := []corev1.Pod{*draining, *open}
	observed := resourceFastletsForPoolChildren(registry, pool, pods)
	plan := reconciler.planResourceAwareScale(pool, pods, 0, planned, observed, now)
	require.Equal(t, int32(1), plan.desiredPods)
	require.Equal(t, int32(1), plan.activeScaleDownDrains)

	desired, condition := reconciler.stabilizeResourceAwareScaleDown(pool, int32(2)-plan.activeScaleDownDrains, plan.desiredPods)
	require.Equal(t, int32(1), desired, "an active drain must not be canceled by a fresh stabilization window")
	require.Equal(t, metav1.ConditionFalse, condition.Status)
}

func TestReconcileDrainingUsesResourceScalePlanIdentity(t *testing.T) {
	reconciler, k8sClient, _, pool := newDrainHarness(t, nil)
	const desiredHash = "desired-template"
	for _, name := range []string{"fastlet-a", "fastlet-b"} {
		pod := getFastletPod(t, k8sClient, name)
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[placement.AnnotationPodTemplateHash] = desiredHash
		require.NoError(t, k8sClient.Update(context.Background(), pod))
	}
	fastletA := getFastletPod(t, k8sClient, "fastlet-a")
	fastletB := getFastletPod(t, k8sClient, "fastlet-b")

	_, handled, err := reconciler.reconcileDraining(
		context.Background(), pool, []corev1.Pod{*fastletA, *fastletB}, nil, 1, desiredHash, []string{"fastlet-b/pod-b"},
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.False(t, placement.PodDrainRequested(getFastletPod(t, k8sClient, "fastlet-a")))
	require.True(t, placement.PodDrainRequested(getFastletPod(t, k8sClient, "fastlet-b")),
		"draining must consume the exact candidate selected by the resource calculation")
}

func TestResourceAwareDesiredPodsExpiresNominalCapacityWithoutHeartbeat(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	pool := resourceAwareScalingPool()
	planned := resourceAwarePlannedPod()
	pod := fastletPod("fastlet-starting", "uid-starting", "")
	pod.Status.Phase = corev1.PodPending
	reconciler := &SandboxPoolReconciler{Now: func() time.Time { return now }, FastletStartupGrace: 2 * time.Minute}

	pod.CreationTimestamp = metav1.NewTime(now.Add(-time.Minute))
	pods := []corev1.Pod{*pod}
	require.Equal(t, int32(1), reconciler.planResourceAwareScale(pool, pods, 2, planned, nil, now).desiredPods,
		"a newly created Pod contributes its nominal capacity once")

	pod.CreationTimestamp = metav1.NewTime(now.Add(-3 * time.Minute))
	pods[0] = *pod
	require.Equal(t, int32(2), reconciler.planResourceAwareScale(pool, pods, 2, planned, nil, now).desiredPods,
		"a Pod without a heartbeat stops hiding replacement demand after startup grace")
}

func TestResourceAwareScaleDownRequiresContinuousStabilization(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 0, 0, 0, time.UTC)
	pool := resourceAwareScalingPool()
	reconciler := &SandboxPoolReconciler{
		Now: func() time.Time { return now }, ResourceScaleDownStabilization: time.Minute,
	}

	desired, condition := reconciler.stabilizeResourceAwareScaleDown(pool, 2, 1)
	require.Equal(t, int32(2), desired)
	require.Equal(t, now, condition.LastTransitionTime.Time)
	apiMeta.SetStatusCondition(&pool.Status.Conditions, *condition)

	now = now.Add(30 * time.Second)
	desired, condition = reconciler.stabilizeResourceAwareScaleDown(pool, 2, 1)
	require.Equal(t, int32(2), desired)
	require.Equal(t, metav1.ConditionTrue, condition.Status)

	now = now.Add(31 * time.Second)
	desired, condition = reconciler.stabilizeResourceAwareScaleDown(pool, 2, 1)
	require.Equal(t, int32(1), desired)
	require.Equal(t, metav1.ConditionTrue, condition.Status)

	desired, condition = reconciler.stabilizeResourceAwareScaleDown(pool, 2, 2)
	require.Equal(t, int32(2), desired)
	require.Equal(t, metav1.ConditionFalse, condition.Status, "recovered demand resets the stabilization window")
}

func TestSandboxNeedsResourceCapacityKeepsRejectedAssignmentPending(t *testing.T) {
	pending := &apiv1alpha2.Sandbox{
		Status: apiv1alpha2.SandboxStatus{
			Placement: apiv1alpha2.PlacementStatus{FastletName: "fastlet-full", FastletPodUID: "uid-full"},
			Runtime:   apiv1alpha2.RuntimeStatus{State: apiv1alpha2.RuntimePending},
		},
	}
	require.True(t, sandboxNeedsResourceCapacity(pending, nil, time.Time{}),
		"a rejected Sandbox still needs capacity even though its durable assignment is present")

	creating := pending.DeepCopy()
	creating.Status.Runtime.State = apiv1alpha2.RuntimeCreating
	require.False(t, sandboxNeedsResourceCapacity(creating, nil, time.Time{}),
		"creation already holds a Fastlet commitment reservation")

	ready := pending.DeepCopy()
	ready.Status.Runtime.State = apiv1alpha2.RuntimeReady
	require.False(t, sandboxNeedsResourceCapacity(ready, nil, time.Time{}))

	paused := pending.DeepCopy()
	paused.Status.Runtime.State = apiv1alpha2.RuntimePaused
	require.False(t, sandboxNeedsResourceCapacity(paused, nil, time.Time{}))
}

func TestSandboxNeedsResourceCapacityUsesExactResumeReservation(t *testing.T) {
	now := time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC)
	resume := &apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("sandbox-a"), Generation: 7},
		Status: apiv1alpha2.SandboxStatus{
			Placement: apiv1alpha2.PlacementStatus{FastletName: "fastlet-a", FastletPodUID: types.UID("uid-a"), Attempt: 3},
			Runtime:   apiv1alpha2.RuntimeStatus{State: apiv1alpha2.RuntimeResuming, Generation: 2},
		},
	}
	fastlet := resourceAwareControllerFastlet("fastlet-a", "uid-a", 1, fastletapi.AdmissionStateOpen, now)
	fastlet.ResourceReceivedAt = now
	fastlet.SandboxStatuses = map[string]fastletapi.SandboxStatus{
		"sandbox-a": {
			SandboxID: "sandbox-a", InstanceGeneration: 2, AssignmentAttempt: 3, AcceptedGeneration: 7,
		},
	}
	observed := map[string]placement.FastletInfo{"fastlet-a/uid-a": fastlet}

	require.True(t, sandboxNeedsResourceCapacity(resume, nil, now),
		"a resume without Fastlet reservation evidence still needs capacity")
	require.False(t, sandboxNeedsResourceCapacity(resume, observed, now),
		"an accepted resume is already represented in Admission.Used")

	newSpecGeneration := resume.DeepCopy()
	newSpecGeneration.Generation++
	require.False(t, sandboxNeedsResourceCapacity(newSpecGeneration, observed, now),
		"an unrelated Spec update does not release the existing Fastlet reservation")

	oldGeneration := fastlet
	oldGeneration.SandboxStatuses = map[string]fastletapi.SandboxStatus{
		"sandbox-a": {
			SandboxID: "sandbox-a", InstanceGeneration: 1, AssignmentAttempt: 3, AcceptedGeneration: 7,
		},
	}
	require.True(t, sandboxNeedsResourceCapacity(resume, map[string]placement.FastletInfo{"fastlet-a/uid-a": oldGeneration}, now),
		"an old runtime generation must not suppress pending demand")

	oldAttempt := fastlet
	oldAttempt.SandboxStatuses = map[string]fastletapi.SandboxStatus{
		"sandbox-a": {
			SandboxID: "sandbox-a", InstanceGeneration: 2, AssignmentAttempt: 2, AcceptedGeneration: 7,
		},
	}
	require.True(t, sandboxNeedsResourceCapacity(resume, map[string]placement.FastletInfo{"fastlet-a/uid-a": oldAttempt}, now),
		"an old assignment attempt must not suppress pending demand")

	stale := fastlet
	stale.LastHeartbeat = now.Add(-placement.DefaultObservationStaleAfter - time.Second)
	require.True(t, sandboxNeedsResourceCapacity(resume, map[string]placement.FastletInfo{"fastlet-a/uid-a": stale}, now),
		"a stale reservation must return to conservative pending demand")
}

func TestAcceptedResumeDoesNotCreateReplacementFastlet(t *testing.T) {
	now := time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC)
	pool := resourceAwareScalingPool()
	pool.Spec.Capacity.BufferMin = 3
	pod := fastletPod("fastlet-a", "uid-a", "10.0.0.1")
	fastlet := resourceAwareControllerFastlet("fastlet-a", "uid-a", 1, fastletapi.AdmissionStateOpen, now)
	fastlet.SandboxStatuses = map[string]fastletapi.SandboxStatus{
		"sandbox-a": {
			SandboxID: "sandbox-a", InstanceGeneration: 2, AssignmentAttempt: 3, AcceptedGeneration: 7,
		},
	}
	registry := placement.NewInMemoryRegistry()
	seedControllerRegistry(t, registry, fastlet)
	reconciler := &SandboxPoolReconciler{Registry: registry, Now: func() time.Time { return now }}
	observed := resourceFastletsForPoolChildren(registry, pool, []corev1.Pod{*pod})
	sandbox := apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("sandbox-a"), Generation: 7},
		Spec:       apiv1alpha2.SandboxSpec{PoolRef: "pool-a"},
		Status: apiv1alpha2.SandboxStatus{
			Placement: apiv1alpha2.PlacementStatus{FastletName: "fastlet-a", FastletPodUID: types.UID("uid-a"), Attempt: 3},
			Runtime:   apiv1alpha2.RuntimeStatus{State: apiv1alpha2.RuntimeResuming, Generation: 2},
		},
	}
	children := map[string]struct{}{"fastlet-a/uid-a": {}}

	_, pending := poolDemandCounts([]apiv1alpha2.Sandbox{sandbox}, "pool-a", children, true, observed, now)
	require.Zero(t, pending, "the accepted resume is already charged to the Fastlet ledger")
	plan := reconciler.planResourceAwareScale(
		pool, []corev1.Pod{*pod}, pending, resourceAwarePlannedPod(), observed, now,
	)
	require.Equal(t, int32(1), plan.desiredPods,
		"the accepted resume must not create a replacement Fastlet")
	require.Equal(t, int32(2), reconciler.planResourceAwareScale(
		pool, []corev1.Pod{*pod}, 1, resourceAwarePlannedPod(), observed, now,
	).desiredPods, "recounting the accepted resume would create an unnecessary Fastlet")
}

func TestPoolDemandCountsFallsBackToSlotDemandWhenResourceScalingIsDisabled(t *testing.T) {
	pending := apiv1alpha2.Sandbox{
		Spec: apiv1alpha2.SandboxSpec{PoolRef: "pool-a"},
		Status: apiv1alpha2.SandboxStatus{
			Runtime: apiv1alpha2.RuntimeStatus{State: apiv1alpha2.RuntimePending},
		},
	}

	active, pendingCount := poolDemandCounts([]apiv1alpha2.Sandbox{pending}, "pool-a", nil, false, nil, time.Time{})
	require.Zero(t, active)
	require.Equal(t, int32(1), pendingCount,
		"disabling ResourceAware scaling must retain the existing slot-based demand path")
}

func TestResourceAwareScaleUsesHeterogeneousPendingCommitments(t *testing.T) {
	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	pool := resourceAwareScalingPool()
	pool.Spec.MaxSandboxesPerPod = 4
	pool.Spec.Capacity.BufferMin = 0
	pool.Spec.Capacity.BufferMax = 0
	planned := resourceAwarePlannedPod()
	pod := fastletPod("fastlet-a", "uid-a", "10.0.0.1")
	registry := placement.NewInMemoryRegistry()
	seedControllerRegistry(t, registry, resourceAwareControllerFastlet("fastlet-a", "uid-a", 0, fastletapi.AdmissionStateOpen, now))
	reconciler := &SandboxPoolReconciler{Registry: registry, Now: func() time.Time { return now }}
	observed := resourceFastletsForPoolChildren(registry, pool, []corev1.Pod{*pod})

	sandboxes := make([]apiv1alpha2.Sandbox, 3)
	for index := range sandboxes {
		sandboxes[index] = apiv1alpha2.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("large-%d", index)},
			Spec: apiv1alpha2.SandboxSpec{
				PoolRef: "pool-a",
				Resources: &apiv1alpha2.SandboxResourceRequirements{Limits: apiv1alpha2.SandboxComputeResources{
					CPU: resource.MustParse("4"), Memory: resource.MustParse("4Gi"),
				}},
			},
			Status: apiv1alpha2.SandboxStatus{Runtime: apiv1alpha2.RuntimeStatus{State: apiv1alpha2.RuntimePending}},
		}
	}
	pending, err := poolPendingResourceDemand(pool, sandboxes, observed, now)
	require.NoError(t, err)
	require.Equal(t, int64(6000), pending.CPU.MilliValue())
	expectedMemory := resource.MustParse("6Gi")
	require.Equal(t, expectedMemory.Value(), pending.Memory.Value())

	plan := reconciler.planResourceAwareScaleWithDemand(
		pool, []corev1.Pod{*pod}, int32(len(sandboxes)), pending, planned, observed, now,
	)
	require.Equal(t, int32(2), plan.desiredPods,
		"three Sandboxes fit the four-slot count but their six-CPU commitment needs two Fastlets")
}

func TestPoolSummarySeparatesFastletsResourcesAndSandboxes(t *testing.T) {
	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	pool := resourceAwareScalingPool()
	pods := []corev1.Pod{
		*fastletPod("fastlet-open", "uid-open", "10.0.0.1"),
		*fastletPod("fastlet-blocked", "uid-blocked", "10.0.0.2"),
	}
	for index := range pods {
		if pods[index].Annotations == nil {
			pods[index].Annotations = make(map[string]string)
		}
		pods[index].Annotations[fastletsettings.CommitmentCapacityAnnotation] = `{"cpu":"4","memory":"8Gi"}`
	}
	registry := placement.NewInMemoryRegistry()
	seedControllerRegistry(t, registry, resourceAwareControllerFastlet("fastlet-open", "uid-open", 1, fastletapi.AdmissionStateOpen, now))
	seedControllerRegistry(t, registry, resourceAwareControllerFastlet("fastlet-blocked", "uid-blocked", 2, fastletapi.AdmissionStateBlocked, now))
	observed := resourceFastletsForPoolChildren(registry, pool, pods)

	fastlets, resources := summarizePoolFastlets(pool, pods, 2, 2, 0, 2, observed, now)
	require.Equal(t, &apiv1alpha2.SandboxPoolFastletSummary{Desired: 2, Current: 2, Ready: 2, Busy: 2}, fastlets)
	require.Equal(t, "8", resources.Capacity.CPU.String())
	require.Equal(t, "16Gi", resources.Capacity.Memory.String())
	require.Equal(t, "3", resources.Committed.CPU.String())
	require.Equal(t, "6Gi", resources.Committed.Memory.String())
	require.Equal(t, "3", resources.Free.CPU.String())
	require.Equal(t, "6Gi", resources.Free.Memory.String())

	terminatingAt := metav1.NewTime(now)
	sandboxes := []apiv1alpha2.Sandbox{
		{Spec: apiv1alpha2.SandboxSpec{PoolRef: "pool-a"}, Status: apiv1alpha2.SandboxStatus{Runtime: apiv1alpha2.RuntimeStatus{State: apiv1alpha2.RuntimeReady}}},
		{Spec: apiv1alpha2.SandboxSpec{PoolRef: "pool-a"}, Status: apiv1alpha2.SandboxStatus{Runtime: apiv1alpha2.RuntimeStatus{State: apiv1alpha2.RuntimePaused}}},
		{Spec: apiv1alpha2.SandboxSpec{PoolRef: "pool-a"}, Status: apiv1alpha2.SandboxStatus{Runtime: apiv1alpha2.RuntimeStatus{State: apiv1alpha2.RuntimePending}}},
		{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &terminatingAt}, Spec: apiv1alpha2.SandboxSpec{PoolRef: "pool-a"}},
		{Spec: apiv1alpha2.SandboxSpec{PoolRef: "pool-a"}, Status: apiv1alpha2.SandboxStatus{Runtime: apiv1alpha2.RuntimeStatus{State: apiv1alpha2.RuntimeFailed}}},
	}
	summary := summarizePoolSandboxes(sandboxes, "pool-a")
	require.Equal(t, &apiv1alpha2.SandboxPoolSandboxSummary{Total: 5, Pending: 1, Running: 1, Paused: 1, Terminating: 1, Failed: 1}, summary)
}

func TestPoolStructuredSummaryUpdatesAtLowFrequency(t *testing.T) {
	observedAt := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	pool := &apiv1alpha2.SandboxPool{}
	first := poolObservation{
		currentPods: 1, observedAt: observedAt,
		fastlets:  &apiv1alpha2.SandboxPoolFastletSummary{Current: 1},
		sandboxes: &apiv1alpha2.SandboxPoolSandboxSummary{Total: 1},
	}
	require.True(t, first.apply(pool))
	require.Equal(t, int32(1), pool.Status.Sandboxes.Total)

	withinWindow := first
	withinWindow.observedAt = observedAt.Add(30 * time.Second)
	withinWindow.sandboxes = &apiv1alpha2.SandboxPoolSandboxSummary{Total: 2}
	require.False(t, withinWindow.apply(pool), "summary-only changes are intentionally rate limited")
	require.Equal(t, int32(1), pool.Status.Sandboxes.Total)

	afterWindow := withinWindow
	afterWindow.observedAt = observedAt.Add(time.Minute + time.Second)
	require.True(t, afterWindow.apply(pool))
	require.Equal(t, int32(2), pool.Status.Sandboxes.Total)
}

func resourceAwareScalingPool() *apiv1alpha2.SandboxPool {
	return &apiv1alpha2.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
		Spec: apiv1alpha2.SandboxPoolSpec{
			MaxSandboxesPerPod: 10,
			Capacity:           apiv1alpha2.PoolCapacity{PoolMin: 1, PoolMax: 10, BufferMin: 2, BufferMax: 4},
			SandboxResources: apiv1alpha2.NewResourceAwareSandboxResourceProfile(
				resource.MustParse("2"), resource.MustParse("4Gi"), resource.MustParse("2"), resource.MustParse("2"), 256,
			),
		},
	}
}

func resourceAwarePlannedPod() *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		fastletsettings.CommitmentCapacityAnnotation: `{"cpu":"4","memory":"8Gi"}`,
	}}, Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "fastlet", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("4"), corev1.ResourceMemory: resource.MustParse("8Gi"),
		}},
	}}}}
}

func resourceAwareControllerFastlet(name, uid string, used int, state fastletapi.AdmissionState, now time.Time) placement.FastletInfo {
	return placement.FastletInfo{
		ID: placement.FastletID(name), Namespace: "default", PoolName: "pool-a", PodName: name, PodUID: uid,
		PodReady: true, RuntimeReady: true, InfraReady: true, LastHeartbeat: now,
		Admission: fastletapi.AdmissionStatus{Capacity: 10, Used: used, Running: used, Resources: &fastletapi.ResourceAdmissionStatus{
			Version: "v2", Epoch: "epoch-" + name, Revision: 1,
			Capacity:  fastletapi.SandboxResourceVector{CPU: "4", Memory: "8Gi"},
			Committed: fastletapi.SandboxResourceVector{CPU: fmt.Sprintf("%d", used), Memory: fmt.Sprintf("%dGi", used*2)},
			State:     state,
		}},
		ResourceCapacity: placement.ResourceDemand{CPU: resource.MustParse("4"), Memory: resource.MustParse("8Gi")},
	}
}

func TestConstructPodUsesRuntimeProfileAndFixedResources(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	reconciler := &SandboxPoolReconciler{
		Scheme: scheme, Catalog: runtimecatalog.Builtin(),
		FastletProxyImage: "fastlet-proxy:test", RouteVerifyPublicKey: "test-public-key",
	}
	runtimeClass := "must-not-leak"
	automount := true
	pool := &apiv1alpha2.SandboxPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiv1alpha2.GroupVersion.String(), Kind: "SandboxPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default", UID: types.UID("pool-uid")},
		Spec: apiv1alpha2.SandboxPoolSpec{
			Runtime:            apiv1alpha2.RuntimeContainer,
			MaxSandboxesPerPod: 5,
			InfraComponents:    []apiv1alpha2.InfraComponent{testInlineComponent()},
			WarmImages:         []string{"alpine:latest", "ubuntu:24.04"},
			SandboxResources: apiv1alpha2.NewLegacySandboxResourceProfile(
				resource.MustParse("1"), resource.MustParse("1Gi"), 256,
			),
			FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RuntimeClassName:             &runtimeClass,
				AutomountServiceAccountToken: &automount,
				Containers: []corev1.Container{{
					Name: "fastlet", Image: "fastlet:test",
					Env: []corev1.EnvVar{
						{Name: "FAST_SANDBOX_RUNTIME", Value: "attacker-runtime"},
						{Name: "FASTLET_CAPACITY", Value: "999"},
						{Name: "FASTLET_CONTROL_PORT", Value: ":9999"},
					},
					ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}},
				}},
			}},
		},
	}
	runtimePlan, err := runtimeenv.ResolveDefault(reconciler.Catalog, pool.Spec.Runtime)
	require.NoError(t, err)
	profile := runtimePlan.Profile
	pod, err := reconciler.constructPodWithRuntimePlan(pool, runtimePlan)
	require.NoError(t, err)
	require.Nil(t, pod.Spec.RuntimeClassName)
	require.NotNil(t, pod.Spec.AutomountServiceAccountToken)
	require.False(t, *pod.Spec.AutomountServiceAccountToken)
	require.Equal(t, "container", envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_RUNTIME"))
	require.Equal(t, profile.ProfileHash, envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_RUNTIME_PROFILE_HASH"))
	require.Equal(t, profile.ProfileHash, pod.Annotations["fast-sandbox.io/runtime-profile-hash"])
	require.Equal(t, pool.Spec.SandboxResources.Hash(), pod.Annotations["fast-sandbox.io/resource-profile-hash"])
	require.Equal(t, shortProfileIdentity(profile), pod.Labels["fast-sandbox.io/runtime-profile"])
	require.Equal(t, "5", envValue(pod.Spec.Containers[0].Env, "FASTLET_CAPACITY"))
	require.NotEmpty(t, envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_INFRA_REVISION"))
	require.Equal(t, envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_INFRA_REVISION"), pod.Annotations["fast-sandbox.io/infra-revision"])
	require.Equal(t, "/etc/fast-sandbox/infra/plan.json", envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_INFRA_PLAN_PATH"))
	require.Equal(t, "/etc/fast-sandbox/runtime/plan.json", envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_RUNTIME_PLAN_PATH"))
	require.Empty(t, envValue(pod.Spec.Containers[0].Env, "RUNTIME_SOCKET"))
	require.Empty(t, envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_SNAPSHOTTER"))
	require.Empty(t, envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_KUBELET_ROOT"))
	require.Empty(t, envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_RESOURCE_CPU"))
	require.Empty(t, envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_RESOURCE_MEMORY"))
	require.Empty(t, envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_RESOURCE_PIDS"))
	require.Empty(t, envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_WARM_IMAGES"))
	require.Equal(t, fastletsettings.MountPath, envValue(pod.Spec.Containers[0].Env, fastletsettings.EnvDir))
	require.NotNil(t, pod.Annotations["fast-sandbox.io/settings-revision"])
	require.NotNil(t, volumeMountForContainer(pod, 0, fastletsettings.VolumeName))
	require.Equal(t, runtimePlan.Revision, pod.Annotations["fast-sandbox.io/runtime-plan-revision"])
	require.NotEmpty(t, pod.Annotations[placement.AnnotationPodTemplateHash])
	require.Equal(t, shortRevision(pod.Annotations["fast-sandbox.io/infra-revision"]), pod.Labels["fast-sandbox.io/infra-revision"])
	require.Equal(t, ":5758", envValue(pod.Spec.Containers[0].Env, "FASTLET_CONTROL_PORT"))
	require.NotNil(t, pod.Spec.Containers[0].ReadinessProbe)
	require.Equal(t, "/readyz", pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Path)
	require.Equal(t, int32(5758), pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Port.IntVal)
	require.Len(t, pod.Spec.Containers, 2)
	require.Equal(t, "fastlet-proxy", pod.Spec.Containers[1].Name)
	require.Equal(t, "fastlet-proxy:test", pod.Spec.Containers[1].Image)
	require.Equal(t, "test-public-key", envValue(pod.Spec.Containers[1].Env, "FAST_SANDBOX_ROUTE_VERIFY_PUBLIC_KEY"))
	require.Equal(t, ":9093", envValue(pod.Spec.Containers[1].Env, "FASTLET_PROXY_METRICS_ADDRESS"))
	require.Equal(t, int32(9093), containerPortForName(t, &pod.Spec.Containers[1], "proxy-metrics"))
	require.Equal(t, int32(5780), pod.Spec.Containers[1].ReadinessProbe.HTTPGet.Port.IntVal)
	require.Equal(t, "250m", pod.Spec.Containers[1].Resources.Limits.Cpu().String())
	require.Equal(t, "256Mi", pod.Spec.Containers[1].Resources.Limits.Memory().String())
	require.Equal(t, "/run/fast-sandbox/proxy/control.sock", envValue(pod.Spec.Containers[0].Env, "FASTLET_PROXY_CONTROL_SOCKET"))
	require.NotNil(t, volumeMountForContainer(pod, 0, "proxy-control"))
	require.NotNil(t, volumeMountForContainer(pod, 1, "proxy-control"))

	cpu := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	memory := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]
	require.Equal(t, "5100m", cpu.String())
	require.Equal(t, "5248Mi", memory.String())
	cpuLimit := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
	memoryLimit := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory]
	require.Equal(t, "5100m", cpuLimit.String())
	require.Equal(t, "5248Mi", memoryLimit.String())
	require.True(t, hasHostPath(pod, "/run/containerd"))
	require.True(t, hasHostPath(pod, "/var/lib/containerd"))
	require.True(t, hasHostPath(pod, podcgroup.HostPath))
	require.False(t, volumeMountForNamedContainer(t, pod, "fastlet", podcgroup.VolumeName).ReadOnly)
	require.True(t, hasHostPath(pod, "/run/fast-sandbox/netns"))
	propagation := volumeMount(pod, "fast-sandbox-netns")
	require.NotNil(t, propagation)
	require.Equal(t, corev1.MountPropagationBidirectional, *propagation)

	samePod, err := reconciler.constructPod(pool, profile)
	require.NoError(t, err)
	require.Equal(t, pod.Annotations[placement.AnnotationPodTemplateHash], samePod.Annotations[placement.AnnotationPodTemplateHash])
	pool.Spec.WarmImages = append(pool.Spec.WarmImages, "busybox:latest")
	changedPod, err := reconciler.constructPod(pool, profile)
	require.NoError(t, err)
	require.NotEqual(t, pod.Annotations[placement.AnnotationPodTemplateHash], changedPod.Annotations[placement.AnnotationPodTemplateHash])
}

func TestApplyFastletResourcesAllowsExplicitAggregateOvercommit(t *testing.T) {
	container := corev1.Container{
		Name: "fastlet",
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("5"),
			corev1.ResourceMemory: resource.MustParse("5Gi"),
		}},
	}
	overhead := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("128Mi"),
	}
	sandbox := apiv1alpha2.NewLegacySandboxResourceProfile(
		resource.MustParse("1"), resource.MustParse("1Gi"), 128,
	)

	require.NoError(t, applyFastletResources(&container, overhead, sandbox, 10))
	require.Equal(t, "5", container.Resources.Limits.Cpu().String())
	require.Equal(t, "5Gi", container.Resources.Limits.Memory().String())
	require.Equal(t, "5", container.Resources.Requests.Cpu().String())
	require.Equal(t, "5Gi", container.Resources.Requests.Memory().String())
}

func TestApplyFastletResourcesPreservesLegacyLimitBelowOneSandbox(t *testing.T) {
	container := corev1.Container{
		Name: "fastlet",
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		}},
	}
	overhead := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("128Mi"),
	}
	sandbox := apiv1alpha2.NewLegacySandboxResourceProfile(
		resource.MustParse("1"), resource.MustParse("1Gi"), 128,
	)

	require.NoError(t, applyFastletResources(&container, overhead, sandbox, 10))
	require.Equal(t, "500m", container.Resources.Limits.Cpu().String())
	require.Equal(t, "512Mi", container.Resources.Limits.Memory().String())
	require.Equal(t, "500m", container.Resources.Requests.Cpu().String())
	require.Equal(t, "512Mi", container.Resources.Requests.Memory().String())
}

func TestApplyFastletResourcesRejectsLimitBelowRuntimeOverhead(t *testing.T) {
	container := corev1.Container{
		Name: "fastlet",
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		}},
	}
	overhead := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("128Mi"),
	}
	sandbox := apiv1alpha2.NewLegacySandboxResourceProfile(
		resource.MustParse("1"), resource.MustParse("1Gi"), 128,
	)

	err := applyFastletResources(&container, overhead, sandbox, 10)
	require.ErrorContains(t, err, "below runtime overhead")
}

func TestCompileFastletResourcesDerivesCommitmentAndEnvelope(t *testing.T) {
	pool := &apiv1alpha2.SandboxPool{Spec: apiv1alpha2.SandboxPoolSpec{
		MaxSandboxesPerPod: 10,
		SandboxResources: apiv1alpha2.NewResourceAwareSandboxResourceProfile(
			resource.MustParse("2"), resource.MustParse("10Gi"), resource.MustParse("2"), resource.MustParse("2"), 256,
		),
		FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fastlet"}}}},
	}}
	overhead := corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("128Mi"),
	}

	resolved, err := compileFastletResources(pool, overhead)
	require.NoError(t, err)
	require.Equal(t, "10", resolved.CommitmentCapacity.CPU.String())
	require.Equal(t, "50Gi", resolved.CommitmentCapacity.Memory.String())
	require.Equal(t, "10", resolved.RuntimeEnvelope.CPU.String())
	require.Equal(t, "50Gi", resolved.RuntimeEnvelope.Memory.String())
}

func TestCompileFastletResourcesEnvelopeFitsOneSandboxLimit(t *testing.T) {
	pool := &apiv1alpha2.SandboxPool{Spec: apiv1alpha2.SandboxPoolSpec{
		MaxSandboxesPerPod: 1,
		SandboxResources: apiv1alpha2.NewResourceAwareSandboxResourceProfile(
			resource.MustParse("2"), resource.MustParse("10Gi"), resource.MustParse("2"), resource.MustParse("2"), 256,
		),
		FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fastlet"}}}},
	}}

	resolved, err := compileFastletResources(pool, nil)
	require.NoError(t, err)
	require.Equal(t, "1", resolved.CommitmentCapacity.CPU.String())
	require.Equal(t, "5Gi", resolved.CommitmentCapacity.Memory.String())
	require.Equal(t, "2", resolved.RuntimeEnvelope.CPU.String())
	require.Equal(t, "10Gi", resolved.RuntimeEnvelope.Memory.String())
}

func TestCompileFastletResourcesRejectsResourceAwareRuntimeOwnerOverrides(t *testing.T) {
	pool := &apiv1alpha2.SandboxPool{Spec: apiv1alpha2.SandboxPoolSpec{
		MaxSandboxesPerPod: 10,
		SandboxResources: apiv1alpha2.NewResourceAwareSandboxResourceProfile(
			resource.MustParse("2"), resource.MustParse("10Gi"), resource.MustParse("2"), resource.MustParse("2"), 256,
		),
		FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fastlet", Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("500m"),
			},
		}}}}},
	}}

	_, err := compileFastletResources(pool, nil)
	require.ErrorContains(t, err, "must not set resources.requests.cpu")
}

func TestConstructResourceAwarePodProjectsOneResolvedCapacity(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	reconciler := &SandboxPoolReconciler{
		Scheme: scheme, Catalog: runtimecatalog.Builtin(), FastletProxyImage: "fastlet-proxy:test",
	}
	pool := &apiv1alpha2.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default", UID: types.UID("pool-uid")},
		Spec: apiv1alpha2.SandboxPoolSpec{
			Runtime: apiv1alpha2.RuntimeContainer, MaxSandboxesPerPod: 10,
			SandboxResources: apiv1alpha2.NewResourceAwareSandboxResourceProfile(
				resource.MustParse("2"), resource.MustParse("5Gi"), resource.MustParse("8"), resource.MustParse("1.25"), 256,
			),
			FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "fastlet", Image: "fastlet:test"},
				{Name: "metrics", Image: "metrics:test", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("20m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
				}},
			}}},
		},
	}

	pod, err := reconciler.constructPod(pool, runtimecatalog.RuntimeProfile{Name: apiv1alpha2.RuntimeContainer})
	require.NoError(t, err)
	capacity, err := fastletsettings.DecodeCommitmentCapacity(pod.Annotations[fastletsettings.CommitmentCapacityAnnotation])
	require.NoError(t, err)
	require.Equal(t, "2500m", capacity.CPU.String())
	require.Equal(t, "40Gi", capacity.Memory.String())
	require.Equal(t, "2500m", pod.Spec.Containers[0].Resources.Requests.Cpu().String())
	require.Equal(t, "40Gi", pod.Spec.Containers[0].Resources.Requests.Memory().String())
	require.Equal(t, pod.Spec.Containers[0].Resources.Requests, pod.Spec.Containers[0].Resources.Limits)
	require.Equal(t, "20m", pod.Spec.Containers[1].Resources.Limits.Cpu().String(), "sidecar resources remain independent")
}

func TestEnsureBoundedPodContainersRejectsUnboundedUserSidecar(t *testing.T) {
	podSpec := &corev1.PodSpec{Containers: []corev1.Container{
		{Name: "fastlet"},
		{Name: "metrics-extension"},
		{Name: "fastlet-proxy"},
	}}

	err := ensureBoundedPodContainers(podSpec, "fastlet")
	require.ErrorContains(t, err, `container "metrics-extension" must define a non-zero cpu limit`)
}

func testInlineComponent() apiv1alpha2.InfraComponent {
	return apiv1alpha2.InfraComponent{
		Name: "execd",
		Artifact: &apiv1alpha2.InfraArtifact{
			Source: apiv1alpha2.InfraArtifactSource{Image: &apiv1alpha2.InfraArtifactImage{
				Reference: "registry.example/execd@sha256:0000000000000000000000000000000000000000000000000000000000000000",
			}},
			Mappings: []apiv1alpha2.InfraArtifactMapping{{
				SourcePath: "/execd", TargetPath: "/.fast/components/execd/execd",
			}},
		},
		Process: apiv1alpha2.InfraProcess{
			Command: []string{"/.fast/components/execd/execd", "--port", "44772"},
			HealthCheck: apiv1alpha2.InfraHealthCheck{
				HTTPGet: &apiv1alpha2.InfraHTTPGet{Path: "/ping"},
			},
		},
		Endpoint: apiv1alpha2.InfraEndpoint{Protocol: "HTTP", Port: 44772},
	}
}

func TestScaleDownDrainsEmptyFastletBeforeDeletion(t *testing.T) {
	reconciler, k8sClient, drainer, pool := newDrainHarness(t, []apiv1alpha2.Sandbox{
		assignedSandbox("sandbox-a", "fastlet-a", "pod-a"),
	})

	result, err := reconciler.Reconcile(context.Background(), poolRequest(pool))
	require.NoError(t, err)
	require.Equal(t, drainRequeue, result.RequeueAfter)
	fastletA := getFastletPod(t, k8sClient, "fastlet-a")
	fastletB := getFastletPod(t, k8sClient, "fastlet-b")
	require.False(t, placement.PodDrainRequested(fastletA))
	require.True(t, placement.PodDrainRequested(fastletB), "the empty Fastlet must be selected before a loaded peer")
	require.NotEmpty(t, fastletB.Annotations[placement.AnnotationDrainAckedAt])

	// A fresh reconciler instance resumes from the durable Pod annotation and
	// removes the already-empty Fastlet without relying on process memory.
	replacement := *reconciler
	_, err = replacement.Reconcile(context.Background(), poolRequest(pool))
	require.NoError(t, err)
	var deleted corev1.Pod
	err = k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "fastlet-b"}, &deleted)
	require.True(t, client.IgnoreNotFound(err) == nil && err != nil)
	require.NotEmpty(t, drainer.requests)
}

func TestLoadedScaleDownFastletCancelsAtDrainTimeout(t *testing.T) {
	now := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)
	reconciler, k8sClient, _, pool := newDrainHarness(t, []apiv1alpha2.Sandbox{
		assignedSandbox("sandbox-a", "fastlet-a", "pod-a"),
		assignedSandbox("sandbox-b", "fastlet-b", "pod-b"),
	})
	reconciler.Now = func() time.Time { return now }
	reconciler.DrainTimeout = 5 * time.Minute
	_, err := reconciler.Reconcile(context.Background(), poolRequest(pool))
	require.NoError(t, err)
	draining := getFastletPod(t, k8sClient, "fastlet-a")
	require.True(t, placement.PodDrainRequested(draining))

	_, err = reconciler.Reconcile(context.Background(), poolRequest(pool))
	require.NoError(t, err)
	_ = getFastletPod(t, k8sClient, "fastlet-a")

	now = now.Add(6 * time.Minute)
	_, err = reconciler.Reconcile(context.Background(), poolRequest(pool))
	require.NoError(t, err)
	remaining := getFastletPod(t, k8sClient, "fastlet-a")
	require.False(t, placement.PodDrainRequested(remaining), "ordinary scale-down must not force-delete a loaded Fastlet")
}

func TestPlannedUpgradeWaitsForReadySurgeThenDrainsOldTemplate(t *testing.T) {
	reconciler, k8sClient, _, pool := newDrainHarness(t, []apiv1alpha2.Sandbox{
		assignedSandbox("sandbox-a", "fastlet-a", "pod-a"),
	})
	const desiredHash = "desired-template"

	oldPod := getFastletPod(t, k8sClient, "fastlet-a")
	oldPod.Annotations = map[string]string{placement.AnnotationPodTemplateHash: "old-template"}
	require.NoError(t, k8sClient.Update(context.Background(), oldPod))
	newPod := getFastletPod(t, k8sClient, "fastlet-b")
	newPod.Annotations = map[string]string{placement.AnnotationPodTemplateHash: desiredHash}
	newPod.Status.Phase = corev1.PodRunning
	require.NoError(t, k8sClient.Update(context.Background(), newPod))
	registry := placement.NewInMemoryRegistry()
	reconciler.Registry = registry

	oldPod = getFastletPod(t, k8sClient, "fastlet-a")
	newPod = getFastletPod(t, k8sClient, "fastlet-b")
	result, handled, err := reconciler.reconcileDraining(context.Background(), pool, []corev1.Pod{*oldPod, *newPod}, []apiv1alpha2.Sandbox{
		assignedSandbox("sandbox-a", "fastlet-a", "pod-a"),
	}, 1, desiredHash, nil)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, drainRequeue, result.RequeueAfter)
	require.False(t, placement.PodDrainRequested(getFastletPod(t, k8sClient, "fastlet-a")), "old Pod must remain schedulable until the surge Pod is Ready")

	newPod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	seedControllerRegistry(t, registry, placement.FastletInfo{
		ID: placement.FastletID(newPod.Name), Namespace: newPod.Namespace, PodName: newPod.Name, PodUID: string(newPod.UID),
		PodReady: true, RuntimeReady: true, InfraReady: true, LastHeartbeat: time.Now(),
		Admission: fastletapi.AdmissionStatus{Capacity: 1},
	})
	_, handled, err = reconciler.reconcileDraining(context.Background(), pool, []corev1.Pod{*oldPod, *newPod}, []apiv1alpha2.Sandbox{
		assignedSandbox("sandbox-a", "fastlet-a", "pod-a"),
	}, 1, desiredHash, nil)
	require.NoError(t, err)
	require.True(t, handled)
	draining := getFastletPod(t, k8sClient, "fastlet-a")
	require.True(t, placement.PodDrainRequested(draining))
	require.Equal(t, placement.DrainReasonPlannedUpgrade, draining.Annotations[placement.AnnotationDrainReason])
	require.NotEmpty(t, draining.Annotations[placement.AnnotationDrainAckedAt])

	require.True(t, needsPlannedUpgradeSurge([]corev1.Pod{*oldPod}, 1, desiredHash))
	require.False(t, needsPlannedUpgradeSurge([]corev1.Pod{*oldPod, *newPod}, 1, desiredHash))
}

func TestSandboxNeedsPlacementExcludesTerminalAndAssignedStates(t *testing.T) {
	require.True(t, sandboxNeedsPlacement(&apiv1alpha2.Sandbox{}))
	expired := &apiv1alpha2.Sandbox{Status: apiv1alpha2.SandboxStatus{
		Runtime: apiv1alpha2.RuntimeStatus{State: apiv1alpha2.RuntimeStopped},
		Conditions: []metav1.Condition{{
			Type: orchestration.ConditionReady, Status: metav1.ConditionFalse, Reason: "arbitrary-diagnostic",
		}},
	}}
	require.False(t, sandboxNeedsPlacement(expired))
	diagnosticOnly := &apiv1alpha2.Sandbox{Status: apiv1alpha2.SandboxStatus{Conditions: []metav1.Condition{{
		Type: orchestration.ConditionReady, Status: metav1.ConditionFalse, Reason: orchestration.ReasonFastletPodLost,
	}}}}
	require.True(t, sandboxNeedsPlacement(diagnosticOnly), "Condition Reason is not state-machine input")
	require.False(t, sandboxNeedsPlacement(&apiv1alpha2.Sandbox{Status: apiv1alpha2.SandboxStatus{Runtime: apiv1alpha2.RuntimeStatus{State: apiv1alpha2.RuntimeStopping}}}))
	placementStatus := apiv1alpha2.PlacementStatus{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1}
	require.False(t, sandboxNeedsPlacement(&apiv1alpha2.Sandbox{Status: apiv1alpha2.SandboxStatus{Placement: placementStatus}}))
}

func newDrainHarness(t *testing.T, sandboxes []apiv1alpha2.Sandbox) (*SandboxPoolReconciler, client.Client, *recordingDrainer, *apiv1alpha2.SandboxPool) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	pool := &apiv1alpha2.SandboxPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiv1alpha2.GroupVersion.String(), Kind: "SandboxPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default", UID: types.UID("pool-a-uid")},
		Spec: apiv1alpha2.SandboxPoolSpec{
			Runtime: apiv1alpha2.RuntimeContainer, MaxSandboxesPerPod: 5,
			Capacity:         apiv1alpha2.PoolCapacity{PoolMin: 1, PoolMax: 10},
			SandboxResources: testSandboxResources(),
			FastletTemplate:  corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fastlet", Image: "fastlet:test"}}}},
		},
	}
	objects := []client.Object{pool, fastletPod("fastlet-a", "pod-a", "10.0.0.1"), fastletPod("fastlet-b", "pod-b", "10.0.0.2")}
	for index := range sandboxes {
		objects = append(objects, &sandboxes[index])
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&apiv1alpha2.SandboxPool{}, &apiv1alpha2.Sandbox{}).
		WithObjects(objects...).Build()
	drainer := &recordingDrainer{}
	reconciler := &SandboxPoolReconciler{
		Client: k8sClient, Scheme: scheme, Catalog: runtimecatalog.Builtin(), FastletDrainer: drainer,
	}
	return reconciler, k8sClient, drainer, pool
}

func fastletPod(name, uid, ip string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "default", UID: types.UID(uid),
			Labels: map[string]string{"app": "sandbox-fastlet", "fast-sandbox.io/pool": "pool-a"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip},
	}
}

func assignedSandbox(name, fastletName, podUID string) apiv1alpha2.Sandbox {
	placementStatus := apiv1alpha2.PlacementStatus{FastletName: fastletName, FastletPodUID: types.UID(podUID), Attempt: 1}
	return apiv1alpha2.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name + "-uid")},
		Spec:       apiv1alpha2.SandboxSpec{Image: "alpine:latest", PoolRef: "pool-a"},
		Status: apiv1alpha2.SandboxStatus{Placement: placementStatus,
			Runtime: apiv1alpha2.RuntimeStatus{Generation: 1}},
	}
}

func getFastletPod(t *testing.T, k8sClient client.Client, name string) *corev1.Pod {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &pod))
	return &pod
}

func poolRequest(pool *apiv1alpha2.SandboxPool) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}}
}

func TestConstructPodSharesNodeOCIToolsAndSocket(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	reconciler := &SandboxPoolReconciler{Scheme: scheme, Catalog: runtimecatalog.Builtin()}
	for _, runtimeName := range []apiv1alpha2.RuntimeName{apiv1alpha2.RuntimeContainer, apiv1alpha2.RuntimeFirecracker} {
		t.Run(string(runtimeName), func(t *testing.T) {
			pool := &apiv1alpha2.SandboxPool{
				ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default", UID: types.UID("pool-uid")},
				Spec: apiv1alpha2.SandboxPoolSpec{Runtime: runtimeName, MaxSandboxesPerPod: 1, SandboxResources: testSandboxResources(),
					FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fastlet", Image: "fastlet:test"}}}},
				},
			}
			plan, err := runtimeenv.ResolveDefault(reconciler.Catalog, runtimeName)
			require.NoError(t, err)
			pod, err := reconciler.constructPodWithRuntimePlan(pool, plan)
			require.NoError(t, err)
			require.Nil(t, volumeMountForContainer(pod, 0, "firecracker-overlaybd-cache"))
			for name, path := range map[string]string{"oci-tools": runtimecatalog.OCIToolsDir, "oci-socket": runtimecatalog.OCISocketDir} {
				mount := volumeMountForContainer(pod, 0, name)
				if runtimeName != apiv1alpha2.RuntimeFirecracker {
					require.Nil(t, mount)
					continue
				}
				require.NotNil(t, mount)
				require.Equal(t, path, mount.MountPath)
				require.Equal(t, name == "oci-tools", mount.ReadOnly)
				found := false
				for _, volume := range pod.Spec.Volumes {
					if volume.Name == name {
						require.NotNil(t, volume.HostPath)
						require.Equal(t, path, volume.HostPath.Path)
						found = true
					}
				}
				require.True(t, found)
			}
		})
	}
}

func TestConstructPodAddsKVMWithoutRuntimeClass(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	reconciler := &SandboxPoolReconciler{Scheme: scheme, Catalog: runtimecatalog.Builtin()}
	pool := &apiv1alpha2.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "kata-pool", Namespace: "default", UID: types.UID("pool-uid")},
		Spec: apiv1alpha2.SandboxPoolSpec{
			Runtime: apiv1alpha2.RuntimeKataClh, MaxSandboxesPerPod: 3, SandboxResources: testSandboxResources(),
			FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "fastlet", Image: "fastlet:test"}},
			}},
		},
	}
	profile, err := reconciler.resolveRuntimeProfile(pool)
	require.NoError(t, err)
	pod, err := reconciler.constructPod(pool, profile)
	require.NoError(t, err)
	require.Nil(t, pod.Spec.RuntimeClassName)
	require.True(t, hasHostPath(pod, "/dev/kvm"))
	require.True(t, hasHostPath(pod, "/opt/kata"))
}

func TestConstructKataFCPodDelegatesHostProcessCleanupWithoutHostPID(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	reconciler := &SandboxPoolReconciler{Scheme: scheme, Catalog: runtimecatalog.Builtin()}
	pool := &apiv1alpha2.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "kata-fc-pool", Namespace: "default", UID: types.UID("pool-uid")},
		Spec: apiv1alpha2.SandboxPoolSpec{
			Runtime: apiv1alpha2.RuntimeKataFc, MaxSandboxesPerPod: 1, SandboxResources: testSandboxResources(),
			FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "fastlet", Image: "fastlet:test"}},
			}},
		},
	}
	profile, err := reconciler.resolveRuntimeProfile(pool)
	require.NoError(t, err)
	pod, err := reconciler.constructPod(pool, profile)
	require.NoError(t, err)

	require.False(t, pod.Spec.HostPID, "host PID visibility belongs only to NodeJanitor")
	fastlet := containerForName(t, pod, "fastlet")
	require.Equal(t, nodecleanup.DefaultSocketPath, envValue(fastlet.Env, "FAST_SANDBOX_NODE_CLEANUP_SOCKET"))
	require.Equal(t, filepath.Dir(nodecleanup.DefaultSocketPath), volumeMountForNamedContainer(t, pod, "fastlet", "node-cleanup").MountPath)
	require.Nil(t, volumeMountForNamedContainer(t, pod, "fastlet-proxy", "node-cleanup"))
}

func TestConstructPodRejectsReservedControlMountFromUserSidecarOrInitContainer(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	reconciler := &SandboxPoolReconciler{Scheme: scheme, Catalog: runtimecatalog.Builtin()}
	base := &apiv1alpha2.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default", UID: types.UID("pool-uid")},
		Spec: apiv1alpha2.SandboxPoolSpec{
			Runtime: apiv1alpha2.RuntimeContainer, MaxSandboxesPerPod: 3, SandboxResources: testSandboxResources(),
			FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "fastlet", Image: "fastlet:test"},
				{Name: "user-sidecar", Image: "user:test", VolumeMounts: []corev1.VolumeMount{{Name: "proxy-control", MountPath: "/user"}}},
			}}},
		},
	}
	profile, err := reconciler.resolveRuntimeProfile(base)
	require.NoError(t, err)
	_, err = reconciler.constructPod(base, profile)
	require.ErrorContains(t, err, "reserved by the platform")

	base.Spec.FastletTemplate.Spec.Containers = base.Spec.FastletTemplate.Spec.Containers[:1]
	base.Spec.FastletTemplate.Spec.InitContainers = []corev1.Container{{
		Name: "user-init", Image: "user:test", VolumeMounts: []corev1.VolumeMount{{Name: "user", MountPath: "/run/fast-sandbox/proxy"}},
	}}
	_, err = reconciler.constructPod(base, profile)
	require.ErrorContains(t, err, "reserved by the platform")
}

func TestConstructPodRejectsInfraArtifactStorageOverride(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	reconciler := &SandboxPoolReconciler{Scheme: scheme, Catalog: runtimecatalog.Builtin()}
	pool := &apiv1alpha2.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default", UID: types.UID("pool-uid")},
		Spec: apiv1alpha2.SandboxPoolSpec{
			Runtime: apiv1alpha2.RuntimeContainer, MaxSandboxesPerPod: 3, SandboxResources: testSandboxResources(),
			FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name: "fastlet", Image: "fastlet:test",
					VolumeMounts: []corev1.VolumeMount{{Name: "user-data", MountPath: "/opt/fast-sandbox/infra"}},
				}},
			}},
		},
	}
	profile, err := reconciler.resolveRuntimeProfile(pool)
	require.NoError(t, err)
	_, err = reconciler.constructPod(pool, profile)
	require.ErrorContains(t, err, "reserved by the platform")
}

func TestUniqueWarmImagesPreservesFirstOccurrence(t *testing.T) {
	require.Equal(t, []string{"alpine:latest", "ubuntu:24.04"}, uniqueWarmImages([]string{
		"alpine:latest", "", "ubuntu:24.04", "alpine:latest",
	}))
}

type warmImageStatusRegistry struct {
	placement.FastletRegistry
	fastlets []placement.FastletInfo
}

func (r *warmImageStatusRegistry) GetAllFastlets() []placement.FastletInfo {
	return r.fastlets
}

func TestPoolWarmImageFailuresConvergeRegardlessOfFastletOrder(t *testing.T) {
	pool := &apiv1alpha2.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default", Generation: 1},
		Spec:       apiv1alpha2.SandboxPoolSpec{WarmImages: []string{"alpine:latest"}},
	}
	registry := &warmImageStatusRegistry{}
	var pods []corev1.Pod
	for _, report := range []struct{ name, message string }{
		{"fastlet-a", "pull denied"}, {"fastlet-b", "connection refused"},
	} {
		uid := report.name + "-uid"
		pods = append(pods, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: report.name, UID: types.UID(uid)}})
		registry.fastlets = append(registry.fastlets, placement.FastletInfo{
			Namespace: pool.Namespace, PoolName: pool.Name, PodName: report.name, PodUID: uid,
			WarmImages: []fastletapi.WarmImageState{{Image: "alpine:latest", State: "Failed", Message: report.message}},
		})
	}
	reconciler := &SandboxPoolReconciler{Registry: registry}
	warm := reconciler.aggregateWarmImageStatus(pool, pods)
	require.Len(t, warm, 1)
	require.Equal(t, int32(2), warm[0].FailedFastlets)
	require.Equal(t, "connection refused", warm[0].LastError)
	require.True(t, (poolObservation{warmImages: warm}).apply(pool))

	// The same reports in reverse order must not require another status write.
	slices.Reverse(registry.fastlets)
	warm = reconciler.aggregateWarmImageStatus(pool, pods)
	require.False(t, (poolObservation{warmImages: warm}).apply(pool))
}

func TestPoolObservabilityAggregatesOnlyCurrentPodIdentities(t *testing.T) {
	registry := placement.NewInMemoryRegistry()
	compiled, err := registryconfig.NewCompiled([]registryconfig.Credential{{
		Host: "registry.example.com", Username: "reader", Password: "secret",
	}})
	require.NoError(t, err)
	now := time.Now()
	pool := &apiv1alpha2.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "tenant-a", Generation: 9},
		Spec: apiv1alpha2.SandboxPoolSpec{
			WarmImages: []string{"alpine:latest", "ubuntu:24.04"},
		},
	}
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "fastlet-a", Namespace: "tenant-a", UID: types.UID("uid-a")}},
		{ObjectMeta: metav1.ObjectMeta{Name: "fastlet-b", Namespace: "tenant-a", UID: types.UID("uid-b")}},
	}
	seedControllerRegistry(t, registry, placement.FastletInfo{
		ID: "tenant-a/fastlet-a", Namespace: "tenant-a", PoolName: "pool-a",
		PodName: "fastlet-a", PodUID: "uid-a", PodReady: true, RuntimeReady: true, InfraReady: true,
		Admission:        fastletapi.AdmissionStatus{Capacity: 4},
		RegistryRevision: compiled.Revision, LastHeartbeat: now,
		WarmImages: []fastletapi.WarmImageState{
			{Image: "alpine:latest", State: "Cached"},
			{Image: "ubuntu:24.04", State: "Pulling"},
		},
	})
	seedControllerRegistry(t, registry, placement.FastletInfo{
		ID: "tenant-a/fastlet-b", Namespace: "tenant-a", PoolName: "pool-a",
		PodName: "fastlet-b", PodUID: "uid-b", PodReady: true, RuntimeReady: true, InfraReady: true,
		Admission:        fastletapi.AdmissionStatus{Capacity: 4, Used: 1, Running: 1},
		RegistryRevision: "stale-revision", LastHeartbeat: now,
		WarmImages: []fastletapi.WarmImageState{{
			Image: "alpine:latest", State: "Failed", Message: "pull denied",
		}},
	})
	seedControllerRegistry(t, registry, placement.FastletInfo{
		ID: "tenant-a/stale-fastlet-a", Namespace: "tenant-a", PoolName: "pool-a",
		PodName: "fastlet-a", PodUID: "old-uid", PodReady: true, RuntimeReady: true, InfraReady: true,
		RegistryRevision: compiled.Revision, LastHeartbeat: now,
		WarmImages: []fastletapi.WarmImageState{{Image: "alpine:latest", State: "Cached"}},
	})

	reconciler := &SandboxPoolReconciler{Registry: registry}
	warm := reconciler.aggregateWarmImageStatus(pool, pods)
	require.Equal(t, []apiv1alpha2.WarmImageStatus{
		{
			Image: "alpine:latest", DesiredFastlets: 2, CachedFastlets: 1, FailedFastlets: 1,
			ObservedGeneration: 9, LastError: "pull denied",
		},
		{
			Image: "ubuntu:24.04", DesiredFastlets: 2, PullingFastlets: 1,
			ObservedGeneration: 9,
		},
	}, warm)
	idle, busy := reconciler.fastletUtilizationCounts(pool, pods)
	require.Equal(t, int32(1), idle)
	require.Equal(t, int32(1), busy)
}

func TestRegistryCredentialRotationDoesNotChangeFastletTemplate(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	pool := &apiv1alpha2.SandboxPool{
		TypeMeta: metav1.TypeMeta{APIVersion: apiv1alpha2.GroupVersion.String(), Kind: "SandboxPool"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "pool-a", Namespace: "tenant-a", UID: types.UID("pool-a-uid"),
		},
		Spec: apiv1alpha2.SandboxPoolSpec{
			Runtime: apiv1alpha2.RuntimeContainer, MaxSandboxesPerPod: 2,
			SandboxResources: testSandboxResources(),
			FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "fastlet", Image: "fastlet:test",
			}}}},
		},
	}
	source := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: registryconfig.ConfigMapName, Namespace: "tenant-a"},
		Data: map[string]string{registryconfig.ConfigMapKey: `
registries:
  - host: registry.example.com
    secretRef:
      name: registry-reader
`},
	}
	credential := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "registry-reader", Namespace: "tenant-a"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(
			`{"auths":{"registry.example.com":{"username":"reader","password":"first"}}}`,
		)},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool, source, credential).Build()
	reconciler := &SandboxPoolReconciler{
		Client: k8sClient, Scheme: scheme, Catalog: runtimecatalog.Builtin(),
		FastletProxyImage: "fastlet-proxy:test",
	}
	first, err := reconciler.ensureRegistrySecret(context.Background(), pool)
	require.NoError(t, err)
	profile, err := reconciler.resolveRuntimeProfile(pool)
	require.NoError(t, err)
	firstPod, err := reconciler.constructPod(pool, profile)
	require.NoError(t, err)

	credential.Data[corev1.DockerConfigJsonKey] = []byte(
		`{"auths":{"registry.example.com":{"username":"reader","password":"rotated"}}}`,
	)
	require.NoError(t, k8sClient.Update(context.Background(), credential))
	second, err := reconciler.ensureRegistrySecret(context.Background(), pool)
	require.NoError(t, err)
	secondPod, err := reconciler.constructPod(pool, profile)
	require.NoError(t, err)

	require.NotEqual(t, first.Revision, second.Revision)
	require.Equal(t,
		firstPod.Annotations[placement.AnnotationPodTemplateHash],
		secondPod.Annotations[placement.AnnotationPodTemplateHash],
		"projected registry Secret rotation must not roll Fastlet Pods",
	)
	var projected corev1.Secret
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{
		Namespace: "tenant-a", Name: registrySecretName(pool.Name),
	}, &projected))
	compiled, err := registryconfig.ParseCompiled(projected.Data[registryconfig.SecretKey])
	require.NoError(t, err)
	require.Equal(t, second.Revision, compiled.Revision)
}

func TestEnsureFastletSettingsConfigMapIsContentAddressed(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	pool := &apiv1alpha2.SandboxPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiv1alpha2.GroupVersion.String(), Kind: "SandboxPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "tenant-a", UID: types.UID("pool-uid")},
		Spec: apiv1alpha2.SandboxPoolSpec{
			Runtime:            apiv1alpha2.RuntimeContainer,
			MaxSandboxesPerPod: 5,
			WarmImages:         []string{"alpine:latest"},
			SandboxResources:   testSandboxResources(),
			FastletTemplate:    corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fastlet", Image: "fastlet:test"}}}},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).Build()
	reconciler := &SandboxPoolReconciler{
		Client: k8sClient, Scheme: scheme, Catalog: runtimecatalog.Builtin(),
		FastletProxyImage: "fastlet-proxy:test",
	}
	profile, err := reconciler.Catalog.Resolve(pool.Spec.Runtime)
	require.NoError(t, err)

	settings, err := buildFastletSettings(pool, profile)
	require.NoError(t, err)
	require.NoError(t, reconciler.ensureFastletSettingsConfigMap(context.Background(), pool, settings))
	require.NoError(t, reconciler.ensureFastletSettingsConfigMap(context.Background(), pool, settings))

	revision, err := settings.Revision()
	require.NoError(t, err)
	var projected corev1.ConfigMap
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{
		Namespace: "tenant-a", Name: settingsConfigMapName(pool.Name, revision),
	}, &projected))
	require.Equal(t, `["alpine:latest"]`, projected.Data[fastletsettings.WarmImagesKey])
	require.JSONEq(t, `[]`, projected.Data[fastletsettings.ActionHandlersKey])
	require.JSONEq(t, `{"cpu":"1","memory":"512Mi","pids":256}`, projected.Data[fastletsettings.ResourceProfileKey])
	require.NotContains(t, projected.Data, fastletsettings.ResourceAdmissionKey)
	require.Equal(t, revision, projected.Annotations["fast-sandbox.io/settings-revision"])
	require.NotNil(t, projected.Immutable)
	require.True(t, *projected.Immutable)

	pool.Spec.WarmImages = append(pool.Spec.WarmImages, "busybox:latest")
	updated, err := buildFastletSettings(pool, profile)
	require.NoError(t, err)
	updatedRevision, err := updated.Revision()
	require.NoError(t, err)
	require.NotEqual(t, revision, updatedRevision)
	require.NoError(t, reconciler.ensureFastletSettingsConfigMap(context.Background(), pool, updated))
	var rolled corev1.ConfigMap
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{
		Namespace: "tenant-a", Name: settingsConfigMapName(pool.Name, updatedRevision),
	}, &rolled))
	require.Equal(t, `["alpine:latest","busybox:latest"]`, rolled.Data[fastletsettings.WarmImagesKey])
}

func TestBuildFastletSettingsProjectsResourceAwareCapacity(t *testing.T) {
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeContainer)
	require.NoError(t, err)
	pool := &apiv1alpha2.SandboxPool{Spec: apiv1alpha2.SandboxPoolSpec{
		Runtime:            apiv1alpha2.RuntimeContainer,
		MaxSandboxesPerPod: 2,
		SandboxResources: apiv1alpha2.NewResourceAwareSandboxResourceProfile(
			resource.MustParse("100m"), resource.MustParse("128Mi"), resource.MustParse("2"), resource.MustParse("2"), 64,
		),
		FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fastlet"}}}},
	}}

	settings, err := buildFastletSettings(pool, profile)
	require.NoError(t, err)
	require.NotNil(t, settings.ResourceAdmission)
	require.Equal(t, "v2", settings.ResourceAdmission.Version)
	require.Equal(t, apiv1alpha2.SandboxResourceModeResourceAware, settings.ResourceAdmission.Mode)
	require.Equal(t, pool.Spec.SandboxResources.Hash(), settings.ResourceAdmission.ResourceProfileHash)

	require.Equal(t, "100m", settings.ResourceAdmission.Capacity.CPU.String())
	require.Equal(t, "128Mi", settings.ResourceAdmission.Capacity.Memory.String())
	require.Equal(t, "100m", settings.ResourceAdmission.RuntimeLimit.CPU.String())
	require.Equal(t, "128Mi", settings.ResourceAdmission.RuntimeLimit.Memory.String())
	require.True(t, settings.ResourceAdmission.StaticAdmission)
	require.True(t, settings.ResourceAdmission.MemoryAdmission)

	files, err := settings.Files()
	require.NoError(t, err)
	require.Contains(t, files, fastletsettings.ResourceAdmissionKey)
	require.JSONEq(t, `{"version":"v2","mode":"ResourceAware","staticAdmission":true,"memoryAdmission":true,"capacity":{"cpu":"100m","memory":"128Mi"},"runtimeLimit":{"cpu":"100m","memory":"128Mi"},"resourceProfileHash":"`+pool.Spec.SandboxResources.Hash()+`"}`, files[fastletsettings.ResourceAdmissionKey])
}

func testSandboxResources() apiv1alpha2.SandboxResourceProfile {
	return apiv1alpha2.NewLegacySandboxResourceProfile(
		resource.MustParse("1"), resource.MustParse("512Mi"), 256,
	)
}

func envValue(env []corev1.EnvVar, name string) string {
	for _, item := range env {
		if item.Name == name {
			return item.Value
		}
	}
	return ""
}

func hasHostPath(pod *corev1.Pod, path string) bool {
	for _, volume := range pod.Spec.Volumes {
		if volume.HostPath != nil && volume.HostPath.Path == path {
			return true
		}
	}
	return false
}

func volumeMount(pod *corev1.Pod, name string) *corev1.MountPropagationMode {
	for _, mount := range pod.Spec.Containers[0].VolumeMounts {
		if mount.Name == name {
			return mount.MountPropagation
		}
	}
	return nil
}

func volumeMountForContainer(pod *corev1.Pod, container int, name string) *corev1.VolumeMount {
	for index := range pod.Spec.Containers[container].VolumeMounts {
		if pod.Spec.Containers[container].VolumeMounts[index].Name == name {
			return &pod.Spec.Containers[container].VolumeMounts[index]
		}
	}
	return nil
}

func containerForName(t *testing.T, pod *corev1.Pod, name string) *corev1.Container {
	t.Helper()
	for index := range pod.Spec.Containers {
		if pod.Spec.Containers[index].Name == name {
			return &pod.Spec.Containers[index]
		}
	}
	t.Fatalf("container %q was not found", name)
	return nil
}

func containerPortForName(t *testing.T, container *corev1.Container, name string) int32 {
	t.Helper()
	for _, port := range container.Ports {
		if port.Name == name {
			return port.ContainerPort
		}
	}
	t.Fatalf("container port %q was not found", name)
	return 0
}

func volumeMountForNamedContainer(t *testing.T, pod *corev1.Pod, containerName, volumeName string) *corev1.VolumeMount {
	t.Helper()
	container := containerForName(t, pod, containerName)
	for index := range container.VolumeMounts {
		if container.VolumeMounts[index].Name == volumeName {
			return &container.VolumeMounts[index]
		}
	}
	return nil
}

func TestUpdatePoolConditionRetriesConflictByRefetchingLatest(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	pool := &apiv1alpha2.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "tenant-a"},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&apiv1alpha2.SandboxPool{}).
		WithObjects(pool).Build()
	reconciler := &SandboxPoolReconciler{Client: k8sClient, Scheme: scheme}
	key := client.ObjectKey{Namespace: "tenant-a", Name: "pool-a"}

	stale := &apiv1alpha2.SandboxPool{}
	require.NoError(t, k8sClient.Get(context.Background(), key, stale))

	// A concurrent writer (for example the OpenSandbox server pool service)
	// records its observation and bumps the resourceVersion after our stale
	// read; the condition write must retry instead of surfacing a Conflict.
	external := &apiv1alpha2.SandboxPool{}
	require.NoError(t, k8sClient.Get(context.Background(), key, external))
	external.Status.CurrentPods = 9
	require.NoError(t, k8sClient.Status().Update(context.Background(), external))

	require.NoError(t, reconciler.updatePoolCondition(context.Background(), stale, metav1.Condition{
		Type: apiv1alpha2.PoolConditionRegistryReady, Status: metav1.ConditionTrue,
		Reason: apiv1alpha2.ReasonRegistryAvailable, Message: "ok",
	}))

	var persisted apiv1alpha2.SandboxPool
	require.NoError(t, k8sClient.Get(context.Background(), key, &persisted))
	require.Equal(t, int32(9), persisted.Status.CurrentPods, "the concurrent writer's field must survive")
	require.NotNil(t, apiMeta.FindStatusCondition(persisted.Status.Conditions, apiv1alpha2.PoolConditionRegistryReady))
	require.Equal(t, stale.ResourceVersion, persisted.ResourceVersion, "the caller must adopt the persisted resourceVersion")
}

func TestUpdatePoolConditionIsNoopWhenConflictRefetchAlreadyApplied(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	pool := &apiv1alpha2.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "tenant-a", Generation: 3},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&apiv1alpha2.SandboxPool{}).
		WithObjects(pool).Build()
	reconciler := &SandboxPoolReconciler{Client: k8sClient, Scheme: scheme}
	key := client.ObjectKey{Namespace: "tenant-a", Name: "pool-a"}
	condition := metav1.Condition{
		Type: apiv1alpha2.PoolConditionRegistryReady, Status: metav1.ConditionTrue,
		Reason: apiv1alpha2.ReasonRegistryAvailable, Message: "ok", ObservedGeneration: 3,
	}

	stale := &apiv1alpha2.SandboxPool{}
	require.NoError(t, k8sClient.Get(context.Background(), key, stale))
	// Another writer already applied the identical condition; the retry must
	// detect the no-op instead of rewriting the same status forever.
	external := &apiv1alpha2.SandboxPool{}
	require.NoError(t, k8sClient.Get(context.Background(), key, external))
	apiMeta.SetStatusCondition(&external.Status.Conditions, condition)
	require.NoError(t, k8sClient.Status().Update(context.Background(), external))
	var afterExternal apiv1alpha2.SandboxPool
	require.NoError(t, k8sClient.Get(context.Background(), key, &afterExternal))

	require.NoError(t, reconciler.updatePoolCondition(context.Background(), stale, condition))

	var persisted apiv1alpha2.SandboxPool
	require.NoError(t, k8sClient.Get(context.Background(), key, &persisted))
	require.Equal(t, afterExternal.ResourceVersion, persisted.ResourceVersion, "an already-applied condition must not trigger another status write")
}

func TestUpdatePoolObservationRetriesConflictByRefetchingLatest(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	pool := &apiv1alpha2.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "tenant-a"},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&apiv1alpha2.SandboxPool{}).
		WithObjects(pool).Build()
	reconciler := &SandboxPoolReconciler{Client: k8sClient, Scheme: scheme}
	key := client.ObjectKey{Namespace: "tenant-a", Name: "pool-a"}

	stale := &apiv1alpha2.SandboxPool{}
	require.NoError(t, k8sClient.Get(context.Background(), key, stale))
	external := &apiv1alpha2.SandboxPool{}
	require.NoError(t, k8sClient.Get(context.Background(), key, external))
	apiMeta.SetStatusCondition(&external.Status.Conditions, metav1.Condition{
		Type: apiv1alpha2.PoolConditionRuntimeReady, Status: metav1.ConditionTrue, Reason: "external", Message: "external",
	})
	require.NoError(t, k8sClient.Status().Update(context.Background(), external))

	observation := poolObservation{
		currentPods: 2, readyPods: 2, idleFastlets: 1, busyFastlets: 1,
		runtimeRevision: "runtime-1", infraRevision: "infra-1", fastletRevision: "hash-1",
		preparedFastlets: 2, warmImages: []apiv1alpha2.WarmImageStatus{{Image: "alpine:latest", DesiredFastlets: 2}},
	}
	require.NoError(t, reconciler.updatePoolStatus(context.Background(), stale, observation.apply))

	var persisted apiv1alpha2.SandboxPool
	require.NoError(t, k8sClient.Get(context.Background(), key, &persisted))
	require.Equal(t, int32(2), persisted.Status.CurrentPods)
	require.Equal(t, "runtime-1", persisted.Status.RuntimeRevision)
	require.NotNil(t, apiMeta.FindStatusCondition(persisted.Status.Conditions, apiv1alpha2.PoolConditionRuntimeReady), "conditions written by another writer must be preserved")
	require.Equal(t, observation.warmImages, stale.Status.WarmImages, "the caller must adopt the persisted status")
}
