package controller

import (
	"testing"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/runtimecatalog"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

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

func TestConstructPodUsesRuntimeProfileAndFixedResources(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	reconciler := &SandboxPoolReconciler{Scheme: scheme, Catalog: runtimecatalog.Builtin()}
	runtimeClass := "must-not-leak"
	pool := &apiv1alpha1.SandboxPool{
		TypeMeta:   metav1.TypeMeta{APIVersion: apiv1alpha1.GroupVersion.String(), Kind: "SandboxPool"},
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default", UID: types.UID("pool-uid")},
		Spec: apiv1alpha1.SandboxPoolSpec{
			Runtime:            apiv1alpha1.RuntimeContainer,
			MaxSandboxesPerPod: 5,
			InfraProfile:       "opensandbox",
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
	require.Equal(t, "opensandbox", envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_INFRA_PROFILE"))
	require.Equal(t, ":5758", envValue(pod.Spec.Containers[0].Env, "FASTLET_CONTROL_PORT"))
	require.JSONEq(t, `["alpine:latest","ubuntu:24.04"]`, envValue(pod.Spec.Containers[0].Env, "FAST_SANDBOX_WARM_IMAGES"))
	require.NotNil(t, pod.Spec.Containers[0].ReadinessProbe)
	require.Equal(t, "/readyz", pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Path)
	require.Equal(t, int32(5758), pod.Spec.Containers[0].ReadinessProbe.HTTPGet.Port.IntVal)

	cpu := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	memory := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]
	require.Equal(t, "5100m", cpu.String())
	require.Equal(t, "5248Mi", memory.String())
	require.True(t, hasHostPath(pod, "/run/containerd"))
	require.True(t, hasHostPath(pod, "/var/lib/containerd"))
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
