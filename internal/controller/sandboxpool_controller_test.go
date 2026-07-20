package controller

import (
	"context"
	"sync"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	"fast-sandbox/internal/controller/fastletpool"
	"fast-sandbox/internal/runtimecatalog"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type recordingDrainer struct {
	mu       sync.Mutex
	requests []api.SetDrainingRequest
}

func (d *recordingDrainer) SetDraining(_ context.Context, _ string, request *api.SetDrainingRequest) (*api.SetDrainingResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests = append(d.requests, *request)
	return &api.SetDrainingResponse{Draining: request.Draining}, nil
}

func TestResolveRuntimeProfileUsesCanonicalAndLegacyFields(t *testing.T) {
	reconciler := &SandboxPoolReconciler{Catalog: runtimecatalog.Builtin()}
	canonical, err := reconciler.resolveRuntimeProfile(&apiv1alpha1.SandboxPool{
		Spec: apiv1alpha1.SandboxPoolSpec{Runtime: apiv1alpha1.RuntimeKataFc},
	})
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeKataFc, canonical.Name)

	legacy, err := reconciler.resolveRuntimeProfile(&apiv1alpha1.SandboxPool{
		Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: apiv1alpha1.RuntimeGVisor},
	})
	require.NoError(t, err)
	require.Equal(t, apiv1alpha1.RuntimeGVisor, legacy.Name)

	_, err = reconciler.resolveRuntimeProfile(&apiv1alpha1.SandboxPool{
		Spec: apiv1alpha1.SandboxPoolSpec{
			RuntimeType: apiv1alpha1.RuntimeGVisor, ContainerdRuntimeHandler: "custom-handler",
		},
	})
	require.ErrorIs(t, err, apiv1alpha1.ErrLegacyRuntimeOverride)
}

func TestRuntimeCapabilityConditionAggregatesExactChildHeartbeat(t *testing.T) {
	registry := fastletpool.NewInMemoryRegistry()
	now := time.Now()
	registry.RegisterOrUpdate(fastletpool.FastletInfo{
		ID: "default/fastlet-ready", Namespace: "default", PoolName: "pool-a",
		PodName: "fastlet-ready", PodUID: "uid-ready", PodReady: true, RuntimeReady: true, LastHeartbeat: now,
	})
	registry.RegisterOrUpdate(fastletpool.FastletInfo{
		ID: "default/fastlet-unready", Namespace: "default", PoolName: "pool-a",
		PodName: "fastlet-unready", PodUID: "uid-unready", PodReady: true, RuntimeReady: false, LastHeartbeat: now,
	})
	registry.RegisterOrUpdate(fastletpool.FastletInfo{
		ID: "default/stale-identity", Namespace: "default", PoolName: "pool-a",
		PodName: "fastlet-ready", PodUID: "old-uid", PodReady: true, RuntimeReady: true, LastHeartbeat: now,
	})
	reconciler := &SandboxPoolReconciler{Registry: registry}
	pool := &apiv1alpha1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default", Generation: 7}}
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "fastlet-ready", Namespace: "default", UID: types.UID("uid-ready")}},
		{ObjectMeta: metav1.ObjectMeta{Name: "fastlet-unready", Namespace: "default", UID: types.UID("uid-unready")}},
	}

	condition, ready := reconciler.runtimeCapabilityCondition(pool, pods)
	require.Equal(t, int32(1), ready)
	require.Equal(t, metav1.ConditionTrue, condition.Status)
	require.Equal(t, apiv1alpha1.ReasonRuntimeAvailable, condition.Reason)
	require.Equal(t, int64(7), condition.ObservedGeneration)

	registry.Remove("default/fastlet-ready")
	condition, ready = reconciler.runtimeCapabilityCondition(pool, pods)
	require.Zero(t, ready)
	require.Equal(t, metav1.ConditionFalse, condition.Status)
	require.Equal(t, apiv1alpha1.ReasonRuntimeUnavailable, condition.Reason)
}

func TestRuntimeCapabilityConditionWaitsForHeartbeat(t *testing.T) {
	registry := fastletpool.NewInMemoryRegistry()
	registry.RegisterOrUpdate(fastletpool.FastletInfo{
		ID: "default/fastlet-a", Namespace: "default", PoolName: "pool-a",
		PodName: "fastlet-a", PodUID: "uid-a", PodReady: true,
	})
	reconciler := &SandboxPoolReconciler{Registry: registry}
	pool := &apiv1alpha1.SandboxPool{ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"}}
	pods := []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "fastlet-a", Namespace: "default", UID: types.UID("uid-a")}}}

	condition, ready := reconciler.runtimeCapabilityCondition(pool, pods)
	require.Zero(t, ready)
	require.Equal(t, metav1.ConditionFalse, condition.Status)
	require.Equal(t, apiv1alpha1.ReasonRuntimeCapabilityPending, condition.Reason)
}

func TestConstructPodUsesRuntimeProfileAndFixedResources(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	reconciler := &SandboxPoolReconciler{
		Scheme: scheme, Catalog: runtimecatalog.Builtin(),
		FastletProxyImage: "fastlet-proxy:test", RouteVerifyPublicKey: "test-public-key",
	}
	runtimeClass := "must-not-leak"
	pool := &apiv1alpha1.SandboxPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiv1alpha1.GroupVersion.String(), Kind: "SandboxPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default", UID: types.UID("pool-uid")},
		Spec: apiv1alpha1.SandboxPoolSpec{
			Runtime:            apiv1alpha1.RuntimeContainer,
			MaxSandboxesPerPod: 5,
			InfraProfile:       "test-infra",
			WarmImages:         []string{"alpine:latest", "ubuntu:24.04"},
			SandboxResources: apiv1alpha1.SandboxResourceProfile{
				CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi"), PIDs: 256,
			},
			FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RuntimeClassName: &runtimeClass,
				Containers: []corev1.Container{{
					Name: "fastlet", Image: "fastlet:test",
					Env: []corev1.EnvVar{
						{Name: "RUNTIME_HANDLER", Value: "attacker-handler"},
						{Name: "FASTLET_CAPACITY", Value: "999"},
						{Name: "FASTLET_CONTROL_PORT", Value: ":9999"},
					},
					ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}},
				}},
			}},
		},
	}
	profile, err := reconciler.resolveRuntimeProfile(pool)
	require.NoError(t, err)
	pod, err := reconciler.constructPod(pool, profile)
	require.NoError(t, err)
	require.Nil(t, pod.Spec.RuntimeClassName)
	require.Equal(t, "container", envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_RUNTIME"))
	require.Equal(t, profile.ProfileHash, envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_RUNTIME_PROFILE_HASH"))
	require.Equal(t, profile.ProfileHash, pod.Annotations["fast-sandbox.io/runtime-profile-hash"])
	require.Equal(t, pool.Spec.SandboxResources.Hash(), pod.Annotations["fast-sandbox.io/resource-profile-hash"])
	require.Equal(t, shortProfileIdentity(profile), pod.Labels["fast-sandbox.io/runtime-profile"])
	require.Empty(t, envValue(pod.Spec.Containers[0].Env, "RUNTIME_HANDLER"))
	require.Empty(t, envValue(pod.Spec.Containers[0].Env, "RUNTIME_TYPE"))
	require.Equal(t, "5", envValue(pod.Spec.Containers[0].Env, "FASTLET_CAPACITY"))
	require.Equal(t, "1", envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_RESOURCE_CPU"))
	require.Equal(t, "1Gi", envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_RESOURCE_MEMORY"))
	require.Equal(t, "test-infra", envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_INFRA_PROFILE"))
	require.NotEmpty(t, envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_INFRA_PROFILE_HASH"))
	require.Equal(t, envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_INFRA_PROFILE_HASH"), pod.Annotations["fast-sandbox.io/infra-profile-hash"])
	require.NotEmpty(t, pod.Annotations[fastletpool.AnnotationPodTemplateHash])
	require.Equal(t, "test-infra", pod.Labels["fast-sandbox.io/infra-profile"])
	require.Equal(t, ":5758", envValue(pod.Spec.Containers[0].Env, "FASTLET_CONTROL_PORT"))
	require.JSONEq(t, `["alpine:latest","ubuntu:24.04"]`, envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_WARM_IMAGES"))
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
	require.Equal(t, "/run/fast-sandbox/proxy/control.sock", envValue(pod.Spec.Containers[0].Env, "FASTLET_PROXY_CONTROL_SOCKET"))
	require.NotNil(t, volumeMountForContainer(pod, 0, "proxy-control"))
	require.NotNil(t, volumeMountForContainer(pod, 1, "proxy-control"))

	cpu := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	memory := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]
	require.Equal(t, "5100m", cpu.String())
	require.Equal(t, "5248Mi", memory.String())
	require.True(t, hasHostPath(pod, "/run/containerd"))
	require.True(t, hasHostPath(pod, "/var/lib/containerd"))
	require.True(t, hasHostPath(pod, "/run/fast-sandbox/netns"))
	propagation := volumeMount(pod, "fast-sandbox-netns")
	require.NotNil(t, propagation)
	require.Equal(t, corev1.MountPropagationBidirectional, *propagation)

	samePod, err := reconciler.constructPod(pool, profile)
	require.NoError(t, err)
	require.Equal(t, pod.Annotations[fastletpool.AnnotationPodTemplateHash], samePod.Annotations[fastletpool.AnnotationPodTemplateHash])
	pool.Spec.WarmImages = append(pool.Spec.WarmImages, "busybox:latest")
	changedPod, err := reconciler.constructPod(pool, profile)
	require.NoError(t, err)
	require.NotEqual(t, pod.Annotations[fastletpool.AnnotationPodTemplateHash], changedPod.Annotations[fastletpool.AnnotationPodTemplateHash])
}

func TestScaleDownDrainsEmptyFastletBeforeDeletion(t *testing.T) {
	reconciler, k8sClient, drainer, pool := newDrainHarness(t, []apiv1alpha1.Sandbox{
		assignedSandbox("sandbox-a", "fastlet-a", "pod-a"),
	})

	result, err := reconciler.Reconcile(context.Background(), poolRequest(pool))
	require.NoError(t, err)
	require.Equal(t, drainRequeue, result.RequeueAfter)
	fastletA := getFastletPod(t, k8sClient, "fastlet-a")
	fastletB := getFastletPod(t, k8sClient, "fastlet-b")
	require.False(t, fastletpool.PodDrainRequested(fastletA))
	require.True(t, fastletpool.PodDrainRequested(fastletB), "the empty Fastlet must be selected before a loaded peer")
	require.NotEmpty(t, fastletB.Annotations[fastletpool.AnnotationDrainAckedAt])

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

func TestLoadedFastletWaitsUntilDrainTimeout(t *testing.T) {
	now := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)
	reconciler, k8sClient, _, pool := newDrainHarness(t, []apiv1alpha1.Sandbox{
		assignedSandbox("sandbox-a", "fastlet-a", "pod-a"),
		assignedSandbox("sandbox-b", "fastlet-b", "pod-b"),
	})
	reconciler.Now = func() time.Time { return now }
	reconciler.DrainTimeout = 5 * time.Minute
	_, err := reconciler.Reconcile(context.Background(), poolRequest(pool))
	require.NoError(t, err)
	draining := getFastletPod(t, k8sClient, "fastlet-a")
	require.True(t, fastletpool.PodDrainRequested(draining))

	_, err = reconciler.Reconcile(context.Background(), poolRequest(pool))
	require.NoError(t, err)
	_ = getFastletPod(t, k8sClient, "fastlet-a")

	now = now.Add(6 * time.Minute)
	_, err = reconciler.Reconcile(context.Background(), poolRequest(pool))
	require.NoError(t, err)
	var deleted corev1.Pod
	err = k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "fastlet-a"}, &deleted)
	require.True(t, client.IgnoreNotFound(err) == nil && err != nil)
}

func TestPlannedUpgradeWaitsForReadySurgeThenDrainsOldTemplate(t *testing.T) {
	reconciler, k8sClient, _, pool := newDrainHarness(t, []apiv1alpha1.Sandbox{
		assignedSandbox("sandbox-a", "fastlet-a", "pod-a"),
	})
	const desiredHash = "desired-template"

	oldPod := getFastletPod(t, k8sClient, "fastlet-a")
	oldPod.Annotations = map[string]string{fastletpool.AnnotationPodTemplateHash: "old-template"}
	require.NoError(t, k8sClient.Update(context.Background(), oldPod))
	newPod := getFastletPod(t, k8sClient, "fastlet-b")
	newPod.Annotations = map[string]string{fastletpool.AnnotationPodTemplateHash: desiredHash}
	newPod.Status.Phase = corev1.PodRunning
	require.NoError(t, k8sClient.Update(context.Background(), newPod))
	registry := fastletpool.NewInMemoryRegistry()
	reconciler.Registry = registry

	oldPod = getFastletPod(t, k8sClient, "fastlet-a")
	newPod = getFastletPod(t, k8sClient, "fastlet-b")
	result, handled, err := reconciler.reconcileDraining(context.Background(), pool, []corev1.Pod{*oldPod, *newPod}, []apiv1alpha1.Sandbox{
		assignedSandbox("sandbox-a", "fastlet-a", "pod-a"),
	}, 1, desiredHash)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, drainRequeue, result.RequeueAfter)
	require.False(t, fastletpool.PodDrainRequested(getFastletPod(t, k8sClient, "fastlet-a")), "old Pod must remain schedulable until the surge Pod is Ready")

	newPod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	registry.RegisterOrUpdate(fastletpool.FastletInfo{
		ID: fastletpool.FastletID(newPod.Name), Namespace: newPod.Namespace, PodName: newPod.Name, PodUID: string(newPod.UID),
		PodReady: true, RuntimeReady: true, InfraReady: true, LastHeartbeat: time.Now(),
	})
	_, handled, err = reconciler.reconcileDraining(context.Background(), pool, []corev1.Pod{*oldPod, *newPod}, []apiv1alpha1.Sandbox{
		assignedSandbox("sandbox-a", "fastlet-a", "pod-a"),
	}, 1, desiredHash)
	require.NoError(t, err)
	require.True(t, handled)
	draining := getFastletPod(t, k8sClient, "fastlet-a")
	require.True(t, fastletpool.PodDrainRequested(draining))
	require.Equal(t, fastletpool.DrainReasonPlannedUpgrade, draining.Annotations[fastletpool.AnnotationDrainReason])
	require.NotEmpty(t, draining.Annotations[fastletpool.AnnotationDrainAckedAt])

	require.True(t, needsPlannedUpgradeSurge([]corev1.Pod{*oldPod}, 1, desiredHash))
	require.False(t, needsPlannedUpgradeSurge([]corev1.Pod{*oldPod, *newPod}, 1, desiredHash))
}

func TestSandboxNeedsPlacementExcludesTerminalAndAssignedStates(t *testing.T) {
	require.True(t, sandboxNeedsPlacement(&apiv1alpha1.Sandbox{}))
	for _, phase := range []apiv1alpha1.SandboxPhase{apiv1alpha1.PhaseExpired, apiv1alpha1.PhaseLost, apiv1alpha1.PhaseTerminating} {
		require.False(t, sandboxNeedsPlacement(&apiv1alpha1.Sandbox{Status: apiv1alpha1.SandboxStatus{Phase: string(phase)}}))
	}
	assignment := apiv1alpha1.SandboxAssignment{FastletName: "fastlet-a", FastletPodUID: "pod-a", Attempt: 1}
	require.False(t, sandboxNeedsPlacement(&apiv1alpha1.Sandbox{Status: apiv1alpha1.SandboxStatus{Assignment: &assignment}}))
}

func newDrainHarness(t *testing.T, sandboxes []apiv1alpha1.Sandbox) (*SandboxPoolReconciler, client.Client, *recordingDrainer, *apiv1alpha1.SandboxPool) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	pool := &apiv1alpha1.SandboxPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiv1alpha1.GroupVersion.String(), Kind: "SandboxPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default", UID: types.UID("pool-a-uid")},
		Spec: apiv1alpha1.SandboxPoolSpec{
			Runtime: apiv1alpha1.RuntimeContainer, MaxSandboxesPerPod: 5,
			Capacity:        apiv1alpha1.PoolCapacity{PoolMin: 1, PoolMax: 10},
			FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "fastlet", Image: "fastlet:test"}}}},
		},
	}
	objects := []client.Object{pool, fastletPod("fastlet-a", "pod-a", "10.0.0.1"), fastletPod("fastlet-b", "pod-b", "10.0.0.2")}
	for index := range sandboxes {
		objects = append(objects, &sandboxes[index])
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&apiv1alpha1.SandboxPool{}, &apiv1alpha1.Sandbox{}).
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

func assignedSandbox(name, fastletName, podUID string) apiv1alpha1.Sandbox {
	assignment := apiv1alpha1.SandboxAssignment{FastletName: fastletName, FastletPodUID: podUID, Attempt: 1}
	return apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name + "-uid")},
		Spec:       apiv1alpha1.SandboxSpec{Image: "alpine:latest", PoolRef: "pool-a"},
		Status:     apiv1alpha1.SandboxStatus{Assignment: &assignment, AssignmentAttempt: 1, InstanceGeneration: 1},
	}
}

func getFastletPod(t *testing.T, k8sClient client.Client, name string) *corev1.Pod {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &pod))
	return &pod
}

func poolRequest(pool *apiv1alpha1.SandboxPool) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}}
}

func TestConstructPodAddsKVMWithoutRuntimeClass(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	reconciler := &SandboxPoolReconciler{Scheme: scheme, Catalog: runtimecatalog.Builtin()}
	pool := &apiv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "kata-pool", Namespace: "default", UID: types.UID("pool-uid")},
		Spec: apiv1alpha1.SandboxPoolSpec{
			Runtime: apiv1alpha1.RuntimeKataClh,
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

func TestConstructPodInjectsBoxLiteRuntimeSidecarAsResourceOwner(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	reconciler := &SandboxPoolReconciler{
		Scheme: scheme, Catalog: runtimecatalog.Builtin(),
		FastletProxyImage: "fastlet-proxy:test", BoxLiteRuntimeImage: "boxlite-runtime:test",
	}
	pool := &apiv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "boxlite-pool", Namespace: "default", UID: types.UID("pool-uid")},
		Spec: apiv1alpha1.SandboxPoolSpec{
			Runtime: apiv1alpha1.RuntimeBoxLite, MaxSandboxesPerPod: 3,
			SandboxResources: apiv1alpha1.SandboxResourceProfile{
				CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi"), PIDs: 128,
			},
			FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "fastlet", Image: "fastlet:test"}},
			}},
		},
	}
	profile, err := reconciler.resolveRuntimeProfile(pool)
	require.NoError(t, err)
	pod, err := reconciler.constructPod(pool, profile)
	require.NoError(t, err)
	require.Len(t, pod.Spec.Containers, 3)

	fastlet := containerForName(t, pod, "fastlet")
	boxLite := containerForName(t, pod, "boxlite-runtime")
	require.Equal(t, "boxlite-runtime:test", boxLite.Image)
	require.False(t, *fastlet.SecurityContext.Privileged)
	require.True(t, *boxLite.SecurityContext.Privileged)
	require.Empty(t, fastlet.Resources.Requests)
	cpu := boxLite.Resources.Requests[corev1.ResourceCPU]
	memory := boxLite.Resources.Requests[corev1.ResourceMemory]
	require.Equal(t, "3200m", cpu.String())
	require.Equal(t, "3328Mi", memory.String())
	require.Equal(t, "boxlite-runtime", resourceFieldContainer(fastlet.Env, "CPU_LIMIT"))
	require.Equal(t, "boxlite-runtime", resourceFieldContainer(fastlet.Env, "MEMORY_LIMIT"))
	require.Equal(t, "/run/fast-sandbox/boxlite/runtime.sock", envValueFromArgs(boxLite.Args, "--socket"))
	require.Equal(t, "/var/lib/fast-sandbox/boxlite", envValueFromArgs(boxLite.Args, "--state-root"))
	require.Equal(t, []string{
		"/usr/local/bin/boxlite-runtime", "--probe-socket", "/run/fast-sandbox/boxlite/runtime.sock",
	}, boxLite.ReadinessProbe.Exec.Command)
	require.NotNil(t, volumeMountForNamedContainer(t, pod, "fastlet", "boxlite-control"))
	require.NotNil(t, volumeMountForNamedContainer(t, pod, "boxlite-runtime", "boxlite-control"))
	require.True(t, volumeMountForNamedContainer(t, pod, "boxlite-runtime", "infra-tools").ReadOnly)
	require.NotNil(t, volumeMountForNamedContainer(t, pod, "boxlite-runtime", "dev-kvm"))
	require.NotNil(t, volumeMountForNamedContainer(t, pod, "boxlite-runtime", "boxlite-state"))
	require.Nil(t, volumeMountForNamedContainer(t, pod, "fastlet", "dev-kvm"))
}

func TestConstructPodRejectsPlatformBoxLiteSidecarOverride(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	reconciler := &SandboxPoolReconciler{Scheme: scheme, Catalog: runtimecatalog.Builtin()}
	pool := &apiv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default", UID: types.UID("pool-uid")},
		Spec: apiv1alpha1.SandboxPoolSpec{
			Runtime: apiv1alpha1.RuntimeContainer,
			FastletTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Name: "fastlet", Image: "fastlet:test"},
				{Name: "boxlite-runtime", Image: "user-controlled:test"},
			}}},
		},
	}
	profile, err := reconciler.resolveRuntimeProfile(pool)
	require.NoError(t, err)
	_, err = reconciler.constructPod(pool, profile)
	require.ErrorContains(t, err, "platform-owned sidecar name")
}

func TestConstructPodRejectsReservedControlMountFromUserSidecarOrInitContainer(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	reconciler := &SandboxPoolReconciler{Scheme: scheme, Catalog: runtimecatalog.Builtin()}
	base := &apiv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default", UID: types.UID("pool-uid")},
		Spec: apiv1alpha1.SandboxPoolSpec{
			Runtime: apiv1alpha1.RuntimeContainer,
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
		Name: "user-init", Image: "user:test", VolumeMounts: []corev1.VolumeMount{{Name: "user", MountPath: "/run/fast-sandbox/boxlite"}},
	}}
	_, err = reconciler.constructPod(base, profile)
	require.ErrorContains(t, err, "reserved by the platform")
}

func TestConstructPodRejectsInfraArtifactStorageOverride(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	reconciler := &SandboxPoolReconciler{Scheme: scheme, Catalog: runtimecatalog.Builtin()}
	pool := &apiv1alpha1.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default", UID: types.UID("pool-uid")},
		Spec: apiv1alpha1.SandboxPoolSpec{
			Runtime: apiv1alpha1.RuntimeContainer,
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

func resourceFieldContainer(env []corev1.EnvVar, name string) string {
	for _, item := range env {
		if item.Name == name && item.ValueFrom != nil && item.ValueFrom.ResourceFieldRef != nil {
			return item.ValueFrom.ResourceFieldRef.ContainerName
		}
	}
	return ""
}

func envValueFromArgs(args []string, name string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == name {
			return args[index+1]
		}
	}
	return ""
}
