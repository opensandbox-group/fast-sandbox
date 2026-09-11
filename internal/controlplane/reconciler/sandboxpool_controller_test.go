package reconciler

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	orchestration "fast-sandbox/internal/controlplane/orchestrator"
	"fast-sandbox/internal/controlplane/placement"
	"fast-sandbox/internal/fastlet/podcgroup"
	"fast-sandbox/internal/nodecleanup"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/registryconfig"
	"fast-sandbox/internal/runtimeenv"

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
			SandboxResources: apiv1alpha2.SandboxResourceProfile{
				CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi"), PIDs: 256,
			},
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
	require.Equal(t, "1", envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_RESOURCE_CPU"))
	require.Equal(t, "1Gi", envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_RESOURCE_MEMORY"))
	require.NotEmpty(t, envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_INFRA_REVISION"))
	require.Equal(t, envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_INFRA_REVISION"), pod.Annotations["fast-sandbox.io/infra-revision"])
	require.Equal(t, "/etc/fast-sandbox/infra/plan.json", envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_INFRA_PLAN_PATH"))
	require.Equal(t, "/etc/fast-sandbox/runtime/plan.json", envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_RUNTIME_PLAN_PATH"))
	require.Equal(t, "/run/containerd/containerd.sock", envValue(pod.Spec.Containers[0].Env, "RUNTIME_SOCKET"))
	require.Equal(t, "overlayfs", envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_SNAPSHOTTER"))
	require.Equal(t, "/var/lib/kubelet", envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_KUBELET_ROOT"))
	require.Equal(t, runtimePlan.Revision, pod.Annotations["fast-sandbox.io/runtime-plan-revision"])
	require.NotEmpty(t, pod.Annotations[placement.AnnotationPodTemplateHash])
	require.Equal(t, shortRevision(pod.Annotations["fast-sandbox.io/infra-revision"]), pod.Labels["fast-sandbox.io/infra-revision"])
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
	sandbox := apiv1alpha2.SandboxResourceProfile{
		CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi"), PIDs: 128,
	}

	require.NoError(t, applyFastletResources(&container, overhead, sandbox, 10))
	require.Equal(t, "5", container.Resources.Limits.Cpu().String())
	require.Equal(t, "5Gi", container.Resources.Limits.Memory().String())
	require.Equal(t, "5", container.Resources.Requests.Cpu().String())
	require.Equal(t, "5Gi", container.Resources.Requests.Memory().String())
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
	sandbox := apiv1alpha2.SandboxResourceProfile{
		CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi"), PIDs: 128,
	}

	err := applyFastletResources(&container, overhead, sandbox, 10)
	require.ErrorContains(t, err, "below runtime overhead")
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

func TestLoadedFastletWaitsUntilDrainTimeout(t *testing.T) {
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
	var deleted corev1.Pod
	err = k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "fastlet-a"}, &deleted)
	require.True(t, client.IgnoreNotFound(err) == nil && err != nil)
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
	}, 1, desiredHash)
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
	}, 1, desiredHash)
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

func TestConstructPodInjectsBoxLiteRuntimeSidecarAsResourceOwner(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	reconciler := &SandboxPoolReconciler{
		Scheme: scheme, Catalog: runtimecatalog.Builtin(),
		FastletProxyImage: "fastlet-proxy:test", BoxLiteRuntimeImage: "boxlite-runtime:test",
	}
	pool := &apiv1alpha2.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "boxlite-pool", Namespace: "default", UID: types.UID("pool-uid")},
		Spec: apiv1alpha2.SandboxPoolSpec{
			Runtime: apiv1alpha2.RuntimeBoxLite, MaxSandboxesPerPod: 3,
			SandboxResources: apiv1alpha2.SandboxResourceProfile{
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
	require.Equal(t, "50m", fastlet.Resources.Requests.Cpu().String())
	require.Equal(t, "64Mi", fastlet.Resources.Requests.Memory().String())
	require.Equal(t, "250m", fastlet.Resources.Limits.Cpu().String())
	require.Equal(t, "256Mi", fastlet.Resources.Limits.Memory().String())
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
	require.True(t, volumeMountForNamedContainer(t, pod, "boxlite-runtime", "registry-config").ReadOnly)
	require.Equal(t, registryconfig.MountPath, envValue(boxLite.Env, "FAST_SANDBOX_REGISTRY_CONFIG_PATH"))
	require.NotNil(t, volumeMountForNamedContainer(t, pod, "boxlite-runtime", "dev-kvm"))
	require.NotNil(t, volumeMountForNamedContainer(t, pod, "boxlite-runtime", "boxlite-state"))
	require.Nil(t, volumeMountForNamedContainer(t, pod, "fastlet", "dev-kvm"))
}

func TestConstructPodRejectsPlatformBoxLiteSidecarOverride(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha2.AddToScheme(scheme))
	reconciler := &SandboxPoolReconciler{Scheme: scheme, Catalog: runtimecatalog.Builtin()}
	pool := &apiv1alpha2.SandboxPool{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default", UID: types.UID("pool-uid")},
		Spec: apiv1alpha2.SandboxPoolSpec{
			Runtime: apiv1alpha2.RuntimeContainer, MaxSandboxesPerPod: 3, SandboxResources: testSandboxResources(),
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
		Name: "user-init", Image: "user:test", VolumeMounts: []corev1.VolumeMount{{Name: "user", MountPath: "/run/fast-sandbox/boxlite"}},
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

func testSandboxResources() apiv1alpha2.SandboxResourceProfile {
	return apiv1alpha2.SandboxResourceProfile{
		CPU: resource.MustParse("1"), Memory: resource.MustParse("512Mi"), PIDs: 256,
	}
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
