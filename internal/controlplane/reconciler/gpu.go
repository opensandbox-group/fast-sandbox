package reconciler

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	podresources "k8s.io/component-helpers/resource"
	scheduling "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"tags.cncf.io/container-device-interface/pkg/cdi"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	"fast-sandbox/internal/controlplane/placement"
	"fast-sandbox/internal/runtimeenv"
)

const nvidiaGPU corev1.ResourceName = "nvidia.com/gpu"

// Keep the serving GPU revision during a compatible rollout until a new Fastlet
// can accept work. A drain still stops admission through the normal Pod fence.
func admissionFastletRevision(pool *apiv1alpha2.SandboxPool, desired *corev1.Pod, pods []corev1.Pod, observed map[string]placement.FastletInfo, now time.Time) string {
	next := desired.Annotations[placement.AnnotationPodTemplateHash]
	current := pool.Status.FastletRevision
	if pool.Spec.SandboxResources.GPUs == 0 || current == "" || current == next {
		return next
	}
	keepCurrent := false
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
			continue
		}
		if fastletPodTemplateCurrent(pod, next) {
			info, ok := observed[podIdentity(pod)]
			if ok && pod.Status.Phase == corev1.PodRunning && podConditionTrue(pod.Status.Conditions, corev1.PodReady) &&
				!placement.PodDrainRequested(pod) && info.PodReady && info.RuntimeReady && info.InfraReady &&
				!info.Draining && !info.DrainRequested && !info.LastHeartbeat.IsZero() &&
				now.Sub(info.LastHeartbeat) <= placement.DefaultObservationStaleAfter {
				return next
			}
		}
		if fastletPodTemplateCurrent(pod, current) &&
			pod.Annotations["fast-sandbox.io/runtime-plan-revision"] == desired.Annotations["fast-sandbox.io/runtime-plan-revision"] &&
			pod.Annotations["fast-sandbox.io/resource-profile-hash"] == desired.Annotations["fast-sandbox.io/resource-profile-hash"] &&
			pod.Annotations["fast-sandbox.io/infra-revision"] == desired.Annotations["fast-sandbox.io/infra-revision"] &&
			pod.Annotations["fast-sandbox.io/settings-revision"] == desired.Annotations["fast-sandbox.io/settings-revision"] {
			keepCurrent = true
		}
	}
	if keepCurrent {
		return current
	}
	return next
}

func gpuBlockedSurge(pods []corev1.Pod, desiredPodHash string) *corev1.Pod {
	for i := range pods {
		pod := &pods[i]
		if !fastletPodTemplateCurrent(pod, desiredPodHash) || pod.Spec.NodeName != "" ||
			pod.Status.Phase != corev1.PodPending || pod.DeletionTimestamp != nil {
			continue
		}
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse &&
				condition.Reason == corev1.PodReasonUnschedulable && strings.Contains(condition.Message, "Insufficient "+string(nvidiaGPU)) {
				return pod
			}
		}
	}
	return nil
}

// The scheduler's shortage message aggregates all nodes. Check the old Pod's node
// before bypassing the readiness gate. Leave complex placement to the scheduler.
func gpuDrainCandidates(ctx context.Context, reader client.Reader, surge *corev1.Pod, available []*corev1.Pod, desiredHash string) ([]*corev1.Pod, error) {
	if !canCheckGPUPlacement(&surge.Spec) {
		return nil, nil
	}
	requests := podresources.PodRequests(surge, podresources.PodResourcesOptions{})
	requestedGPU := requests[nvidiaGPU]
	if requestedGPU.IsZero() {
		return nil, nil
	}
	affinity := nodeaffinity.GetRequiredNodeAffinity(surge)
	nodes := make(map[string]*corev1.Node)
	occupants := make(map[string][]corev1.Pod)
	var candidates []*corev1.Pod
	for _, old := range available {
		name := old.Spec.NodeName
		if name == "" || old.DeletionTimestamp != nil || fastletPodTemplateCurrent(old, desiredHash) {
			continue
		}
		node, checked := nodes[name]
		if !checked {
			nodes[name] = nil
			node = &corev1.Node{}
			if err := reader.Get(ctx, client.ObjectKey{Name: name}, node); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return nil, err
			}
			availableGPU := node.Status.Allocatable[nvidiaGPU]
			match, err := affinity.Match(node)
			if availableGPU.Cmp(requestedGPU) < 0 || err != nil || !match || !gpuDrainNodeReady(ctx, node, surge) {
				continue
			}
			var pods corev1.PodList
			if err := reader.List(ctx, &pods, client.MatchingFields{"spec.nodeName": name}); err != nil {
				return nil, err
			}
			nodes[name], occupants[name] = node, pods.Items
		}
		if node != nil && gpuFitsAfterDrain(node.Status.Allocatable, occupants[name], old, requests) {
			candidates = append(candidates, old)
		}
	}
	return candidates, nil
}

func canCheckGPUPlacement(spec *corev1.PodSpec) bool {
	if (spec.SchedulerName != "" && spec.SchedulerName != corev1.DefaultSchedulerName) ||
		len(spec.SchedulingGates) > 0 || len(spec.ResourceClaims) > 0 || spec.OS != nil && spec.OS.Name != corev1.Linux {
		return false
	}
	if affinity := spec.Affinity; affinity != nil {
		if affinity.PodAffinity != nil || affinity.PodAntiAffinity != nil {
			return false
		}
	}
	for _, constraint := range spec.TopologySpreadConstraints {
		if constraint.WhenUnsatisfiable == corev1.DoNotSchedule {
			return false
		}
	}
	for _, volume := range spec.Volumes {
		if volume.PersistentVolumeClaim != nil || volume.Ephemeral != nil || volume.CSI != nil {
			return false
		}
	}
	for _, containers := range [][]corev1.Container{spec.Containers, spec.InitContainers} {
		for _, container := range containers {
			for _, port := range container.Ports {
				if port.HostPort != 0 || spec.HostNetwork {
					return false
				}
			}
		}
	}
	return true
}

func gpuDrainNodeReady(ctx context.Context, node *corev1.Node, surge *corev1.Pod) bool {
	if node.Spec.Unschedulable || node.DeletionTimestamp != nil {
		return false
	}
	_, blocked := scheduling.FindMatchingUntoleratedTaint(klog.FromContext(ctx), node.Spec.Taints, surge.Spec.Tolerations, func(taint *corev1.Taint) bool {
		return taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute
	}, false)
	if blocked {
		return false
	}
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func gpuFitsAfterDrain(allocatable corev1.ResourceList, pods []corev1.Pod, old *corev1.Pod, requests corev1.ResourceList) bool {
	remaining := allocatable.DeepCopy()
	var releasedGPU resource.Quantity
	for i := range pods {
		pod := &pods[i]
		used := podresources.PodRequests(pod, podresources.PodResourcesOptions{UseStatusResources: true})
		if pod.UID == old.UID && pod.Name == old.Name && pod.Namespace == old.Namespace {
			if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
				return false
			}
			releasedGPU = used[nvidiaGPU]
			continue
		}
		used[corev1.ResourcePods] = *resource.NewQuantity(1, resource.DecimalSI)
		for name, quantity := range used {
			free := remaining[name]
			free.Sub(quantity)
			remaining[name] = free
		}
	}
	gpuRequest, gpuFree := requests[nvidiaGPU], remaining[nvidiaGPU]
	gpuFree = gpuFree.DeepCopy()
	gpuFree.Sub(releasedGPU)
	if releasedGPU.IsZero() || gpuFree.Cmp(gpuRequest) >= 0 {
		return false
	}
	for name, quantity := range requests {
		free := remaining[name]
		if !quantity.IsZero() && free.Cmp(quantity) < 0 {
			return false
		}
	}
	freePodSlots := remaining[corev1.ResourcePods]
	return freePodSlots.Value() >= 1
}

// A GPU Pool has a fixed device cost per slot; both existing slot scheduling and
// Pool scaling therefore use the same Kubernetes-reserved device capacity.
func applyGPUResources(pool *apiv1alpha2.SandboxPool, plan runtimeenv.ResolvedRuntimePlan, pod *corev1.PodSpec) error {
	count := pool.Spec.SandboxResources.GPUs
	if count == 0 {
		return nil
	}
	if err := pool.Spec.ValidateGPUContract(); err != nil {
		return err
	}
	if requirements := pool.Spec.SandboxResources.GPURequirements; requirements != nil && requirements.Model != "" {
		if err := mergeNodeSelector(pod, map[string]string{"nvidia.com/gpu.product": requirements.Model}); err != nil {
			return fmt.Errorf("GPU model node selection: %w", err)
		}
	}
	owner := &pod.Containers[0]
	total := int64(count) * int64(pool.Spec.MaxSandboxesPerPod)
	quantity := *resource.NewQuantity(total, resource.DecimalSI)
	if owner.Resources.Requests == nil {
		owner.Resources.Requests = corev1.ResourceList{}
	}
	if owner.Resources.Limits == nil {
		owner.Resources.Limits = corev1.ResourceList{}
	}
	owner.Resources.Requests[nvidiaGPU] = quantity
	owner.Resources.Limits[nvidiaGPU] = quantity
	owner.Env = append(owner.Env, corev1.EnvVar{Name: "FAST_SANDBOX_GPU_CONTAINER_NAME", Value: owner.Name}, corev1.EnvVar{Name: "FAST_SANDBOX_GPU_EXPECTED_DEVICES", Value: strconv.FormatInt(total, 10)})
	directory := corev1.HostPathDirectory
	create := corev1.HostPathDirectoryOrCreate
	requirements := []runtimecatalog.HostPathRequirement{
		{Name: "gpu-pod-resources", HostPath: filepath.Join(plan.Kubelet.Root, "pod-resources"), MountPath: runtimecatalog.GPUPodResourcesMountPath, Type: directory, ReadOnly: true},
		{Name: "gpu-cdi-etc", HostPath: cdi.DefaultStaticDir, MountPath: cdi.DefaultStaticDir, Type: create, ReadOnly: true},
		{Name: "gpu-cdi-run", HostPath: cdi.DefaultDynamicDir, MountPath: cdi.DefaultDynamicDir, Type: create, ReadOnly: true},
		{Name: "gpu-leases", HostPath: runtimecatalog.GPUStateRoot, MountPath: runtimecatalog.GPUStateRoot, Type: create},
	}
	if plan.Kubelet.Root == "" {
		return fmt.Errorf("GPU Pool requires a kubelet root in the runtime environment")
	}
	return mergeRuntimeHostPaths(pod, owner, requirements)
}
