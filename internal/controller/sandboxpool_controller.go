package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/controller/fastletpool"
	"fast-sandbox/internal/runtimecatalog"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

// SandboxPoolReconciler reconciles SandboxPool resources.
type SandboxPoolReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Registry fastletpool.FastletRegistry
	Catalog  *runtimecatalog.Catalog
}

// Reconcile manages the lifecycle of Fastlet Pods based on the demand from Sandboxes.
func (r *SandboxPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := klog.FromContext(ctx)

	var pool apiv1alpha1.SandboxPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	profile, err := r.resolveRuntimeProfile(&pool)
	if err != nil {
		logger.Error(err, "Runtime profile resolution failed")
		_ = r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type:    apiv1alpha1.PoolConditionRuntimeReady,
			Status:  metav1.ConditionFalse,
			Reason:  apiv1alpha1.ReasonRuntimeProfileInvalid,
			Message: err.Error(),
		})
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if profile.Capabilities.DefaultState == runtimecatalog.CapabilityUnsupported {
		_ = r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type:    apiv1alpha1.PoolConditionRuntimeReady,
			Status:  metav1.ConditionFalse,
			Reason:  apiv1alpha1.ReasonRuntimeUnsupported,
			Message: profile.Capabilities.Reason,
		})
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if _, err := pool.Spec.EffectiveSandboxResources(); err != nil {
		_ = r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type:    apiv1alpha1.PoolConditionRuntimeReady,
			Status:  metav1.ConditionFalse,
			Reason:  apiv1alpha1.ReasonResourceProfileInvalid,
			Message: err.Error(),
		})
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	var childPods corev1.PodList
	if err := r.List(ctx, &childPods, client.InNamespace(req.Namespace), client.MatchingLabels(poolLabels(pool.Name))); err != nil {
		return ctrl.Result{}, err
	}
	var allSandboxes apiv1alpha1.SandboxList
	if err := r.List(ctx, &allSandboxes, client.InNamespace(req.Namespace)); err != nil {
		return ctrl.Result{}, err
	}

	var activeCount, pendingCount int32
	for _, sb := range allSandboxes.Items {
		if sb.Spec.PoolRef == pool.Name {
			if sb.Status.AssignedFastlet != "" {
				activeCount++
			} else {
				pendingCount++
			}
		}
	}

	maxPerPod := getFastletCapacity(&pool)
	if maxPerPod <= 0 {
		maxPerPod = 1
	}

	totalNeededSlots := activeCount + pendingCount + pool.Spec.Capacity.BufferMin
	desiredPods := (totalNeededSlots + maxPerPod - 1) / maxPerPod

	if desiredPods < pool.Spec.Capacity.PoolMin {
		desiredPods = pool.Spec.Capacity.PoolMin
	}
	if pool.Spec.Capacity.PoolMax > 0 && desiredPods > pool.Spec.Capacity.PoolMax {
		desiredPods = pool.Spec.Capacity.PoolMax
	}

	currentCount := int32(len(childPods.Items))

	if currentCount < desiredPods {
		diff := desiredPods - currentCount
		logger.Info("Scaling up fastlet pool", "diff", diff)
		for i := int32(0); i < diff; i++ {
			pod, err := r.constructPod(&pool, profile)
			if err != nil {
				return ctrl.Result{}, err
			}
			if err := r.Create(ctx, pod); err != nil {
				logger.Error(err, "Failed to create fastlet pod")
				return ctrl.Result{}, err
			}
		}
	} else if currentCount > desiredPods {
		diff := currentCount - desiredPods
		logger.Info("Scaling down fastlet pool", "diff", diff)
		for i := int32(0); i < diff; i++ {
			pod := childPods.Items[i]
			if err := r.Delete(ctx, &pod); err != nil {
				logger.Error(err, "Failed to delete fastlet pod", "pod", pod.Name)
				return ctrl.Result{}, err
			}
		}
	}

	pool.Status.CurrentPods = currentCount
	pool.Status.TotalFastlets = currentCount
	if err := r.Status().Update(ctx, &pool); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// constructPod builds a Fastlet Pod from the template and a platform-owned
// RuntimeProfile. RuntimeClass and backend handler overrides are never copied
// from the Pool into the Pod.
func (r *SandboxPoolReconciler) constructPod(pool *apiv1alpha1.SandboxPool, profile runtimecatalog.RuntimeProfile) (*corev1.Pod, error) {
	sandboxResources, err := pool.Spec.EffectiveSandboxResources()
	if err != nil {
		return nil, err
	}
	labels := make(map[string]string)
	for k, v := range pool.Spec.FastletTemplate.ObjectMeta.Labels {
		labels[k] = v
	}
	for k, v := range poolLabels(pool.Name) {
		labels[k] = v
	}
	labels["fast-sandbox.io/runtime"] = string(profile.Name)
	labels["fast-sandbox.io/runtime-profile"] = shortProfileIdentity(profile)
	annotations := make(map[string]string)
	for k, v := range pool.Spec.FastletTemplate.ObjectMeta.Annotations {
		annotations[k] = v
	}
	annotations["fast-sandbox.io/runtime-profile-hash"] = profile.ProfileHash

	podSpec := pool.Spec.FastletTemplate.Spec.DeepCopy()
	podSpec.HostNetwork = false
	podSpec.HostPID = false
	podSpec.RuntimeClassName = nil
	if len(podSpec.Containers) == 0 {
		return nil, errors.New("fastletTemplate.spec.containers must contain the fastlet container")
	}
	if err := mergeNodeSelector(podSpec, profile.Deployment.NodeSelector); err != nil {
		return nil, err
	}

	if len(podSpec.Containers) > 0 {
		c := &podSpec.Containers[0]
		if c.SecurityContext == nil {
			c.SecurityContext = &corev1.SecurityContext{}
		}
		c.SecurityContext.Privileged = boolPtr(profile.Deployment.Privileged)
		c.Env = removeRuntimeOwnedEnv(c.Env)

		c.Env = append(c.Env,
			corev1.EnvVar{Name: "FASTLET_CONTROL_PORT", Value: ":5758"},
			corev1.EnvVar{
				Name:      "NODE_NAME",
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}},
			},
			corev1.EnvVar{
				Name:      "POD_NAME",
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
			},
			corev1.EnvVar{
				Name:      "POD_IP",
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}},
			},
			corev1.EnvVar{
				Name:      "POD_UID",
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}},
			},
			corev1.EnvVar{
				Name:      "NAMESPACE",
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}},
			},
			corev1.EnvVar{
				Name:      "CPU_LIMIT",
				ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{ContainerName: "fastlet", Resource: "limits.cpu"}},
			},
			corev1.EnvVar{
				Name:      "MEMORY_LIMIT",
				ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{ContainerName: "fastlet", Resource: "limits.memory"}},
			},
			corev1.EnvVar{
				Name:  "FASTLET_CAPACITY",
				Value: fmt.Sprintf("%d", getFastletCapacity(pool)),
			},
			corev1.EnvVar{
				Name:  "FAST_SANDBOX_RUNTIME",
				Value: string(profile.Name),
			},
			corev1.EnvVar{
				Name:  "FAST_SANDBOX_RUNTIME_PROFILE_HASH",
				Value: profile.ProfileHash,
			},
			corev1.EnvVar{Name: "FAST_SANDBOX_RESOURCE_CPU", Value: sandboxResources.CPU.String()},
			corev1.EnvVar{Name: "FAST_SANDBOX_RESOURCE_MEMORY", Value: sandboxResources.Memory.String()},
			corev1.EnvVar{Name: "FAST_SANDBOX_RESOURCE_PIDS", Value: strconv.FormatInt(sandboxResources.PIDs, 10)},
			corev1.EnvVar{Name: "FAST_SANDBOX_INFRA_PROFILE", Value: pool.Spec.InfraProfile},
			corev1.EnvVar{Name: "RUNTIME_SOCKET", Value: "/run/containerd/containerd.sock"},
			corev1.EnvVar{Name: "INFRA_DIR_IN_POD", Value: "/opt/fast-sandbox/infra"},
		)

		c.VolumeMounts = append(c.VolumeMounts,
			corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"},
			corev1.VolumeMount{Name: "infra-tools", MountPath: "/opt/fast-sandbox/infra"},
		)
		if err := applyFastletResources(c, profile.Deployment.Overhead, sandboxResources, getFastletCapacity(pool)); err != nil {
			return nil, err
		}
		c.ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/readyz", Port: intstr.FromInt32(5758), Scheme: corev1.URISchemeHTTP,
			}},
			InitialDelaySeconds: 0, PeriodSeconds: 2, TimeoutSeconds: 1, FailureThreshold: 1,
		}

	}

	podSpec.InitContainers = append(podSpec.InitContainers, corev1.Container{
		Name:            "infra-init",
		Image:           "alpine:latest",
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"sh", "-c", "cat <<'EOF' > /opt/fast-sandbox/infra/fs-helper\n#!/bin/sh\necho [FS-INFRA] Helper Initiated\nexec \"$@\"\nEOF\nchmod +x /opt/fast-sandbox/infra/fs-helper"},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "infra-tools", MountPath: "/opt/fast-sandbox/infra"},
		},
	})

	hostPathDirectory := corev1.HostPathDirectory

	podSpec.Volumes = append(podSpec.Volumes,
		corev1.Volume{
			Name:         "tmp",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/tmp", Type: &hostPathDirectory}},
		},
		corev1.Volume{
			Name: "infra-tools",
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
	)
	if err := mergeRuntimeHostPaths(podSpec, &podSpec.Containers[0], profile.Deployment.HostPaths); err != nil {
		return nil, err
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: pool.Name + "-fastlet-",
			Namespace:    pool.Namespace,
			Labels:       labels,
			Annotations:  annotations,
		},
		Spec: *podSpec,
	}

	if err := ctrl.SetControllerReference(pool, pod, r.Scheme); err != nil {
		return nil, err
	}
	return pod, nil
}

func shortProfileIdentity(profile runtimecatalog.RuntimeProfile) string {
	hash := profile.ProfileHash
	if len(hash) > 12 {
		hash = hash[:12]
	}
	return profile.Version + "-" + hash
}

func poolLabels(poolName string) map[string]string {
	return map[string]string{
		"fast-sandbox.io/pool": poolName,
		"app":                  "sandbox-fastlet",
	}
}

func getFastletCapacity(pool *apiv1alpha1.SandboxPool) int32 {
	if pool.Spec.MaxSandboxesPerPod > 0 {
		return pool.Spec.MaxSandboxesPerPod
	}
	return 5
}

func (r *SandboxPoolReconciler) resolveRuntimeProfile(pool *apiv1alpha1.SandboxPool) (runtimecatalog.RuntimeProfile, error) {
	name, err := pool.Spec.EffectiveRuntime()
	if err != nil {
		return runtimecatalog.RuntimeProfile{}, err
	}
	catalog := r.Catalog
	if catalog == nil {
		catalog = runtimecatalog.Builtin()
	}
	return catalog.Resolve(name)
}

var runtimeOwnedEnv = map[string]struct{}{
	"RUNTIME_TYPE": {}, "RUNTIME_HANDLER": {},
	"FAST_SANDBOX_RUNTIME": {}, "FAST_SANDBOX_RUNTIME_PROFILE_HASH": {},
	"FAST_SANDBOX_RESOURCE_CPU": {}, "FAST_SANDBOX_RESOURCE_MEMORY": {}, "FAST_SANDBOX_RESOURCE_PIDS": {},
	"FAST_SANDBOX_INFRA_PROFILE": {}, "FASTLET_CAPACITY": {},
	"RUNTIME_SOCKET": {}, "INFRA_DIR_IN_POD": {},
	"FASTLET_CONTROL_PORT": {}, "AGENT_PORT": {},
	"NODE_NAME": {}, "POD_NAME": {}, "POD_IP": {}, "POD_UID": {}, "NAMESPACE": {},
}

func removeRuntimeOwnedEnv(env []corev1.EnvVar) []corev1.EnvVar {
	result := env[:0]
	for _, item := range env {
		if _, owned := runtimeOwnedEnv[item.Name]; owned {
			continue
		}
		result = append(result, item)
	}
	return result
}

func mergeNodeSelector(podSpec *corev1.PodSpec, required map[string]string) error {
	if podSpec.NodeSelector == nil && len(required) > 0 {
		podSpec.NodeSelector = make(map[string]string, len(required))
	}
	for key, value := range required {
		if existing, ok := podSpec.NodeSelector[key]; ok && existing != value {
			return fmt.Errorf("fastletTemplate nodeSelector %q=%q conflicts with runtime requirement %q", key, existing, value)
		}
		podSpec.NodeSelector[key] = value
	}
	return nil
}

func applyFastletResources(container *corev1.Container, overhead corev1.ResourceList, sandbox apiv1alpha1.SandboxResourceProfile, capacity int32) error {
	required := overhead.DeepCopy()
	if required == nil {
		required = corev1.ResourceList{}
	}
	addScaledQuantity(required, corev1.ResourceCPU, sandbox.CPU, int64(capacity))
	addScaledQuantity(required, corev1.ResourceMemory, sandbox.Memory, int64(capacity))
	if container.Resources.Requests == nil {
		container.Resources.Requests = corev1.ResourceList{}
	}
	for name, quantity := range required {
		current := container.Resources.Requests[name]
		if current.IsZero() || current.Cmp(quantity) < 0 {
			container.Resources.Requests[name] = quantity.DeepCopy()
		}
		if limit, ok := container.Resources.Limits[name]; ok && !limit.IsZero() && limit.Cmp(quantity) < 0 {
			return fmt.Errorf("fastlet container limit %s=%s is below runtime requirement %s", name, limit.String(), quantity.String())
		}
	}
	return nil
}

func addScaledQuantity(resources corev1.ResourceList, name corev1.ResourceName, quantity resource.Quantity, multiplier int64) {
	if quantity.IsZero() || multiplier <= 0 {
		return
	}
	scaled := quantity.DeepCopy()
	scaled.Mul(multiplier)
	current := resources[name]
	current.Add(scaled)
	resources[name] = current
}

func mergeRuntimeHostPaths(podSpec *corev1.PodSpec, container *corev1.Container, requirements []runtimecatalog.HostPathRequirement) error {
	for _, requirement := range requirements {
		volumeFound := false
		for _, volume := range podSpec.Volumes {
			if volume.Name != requirement.Name {
				continue
			}
			volumeFound = true
			if volume.HostPath == nil || volume.HostPath.Path != requirement.HostPath {
				return fmt.Errorf("fastletTemplate volume %q conflicts with runtime host path %q", requirement.Name, requirement.HostPath)
			}
		}
		if !volumeFound {
			hostPathType := requirement.Type
			podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
				Name: requirement.Name,
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
					Path: requirement.HostPath, Type: &hostPathType,
				}},
			})
		}

		mountFound := false
		for _, mount := range container.VolumeMounts {
			if mount.Name == requirement.Name {
				mountFound = true
				if mount.MountPath != requirement.MountPath || mount.ReadOnly != requirement.ReadOnly {
					return fmt.Errorf("fastletTemplate mount %q conflicts with runtime mount %q", requirement.Name, requirement.MountPath)
				}
			} else if mount.MountPath == requirement.MountPath {
				return fmt.Errorf("fastletTemplate mount path %q is reserved by runtime volume %q", requirement.MountPath, requirement.Name)
			}
		}
		if !mountFound {
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
				Name: requirement.Name, MountPath: requirement.MountPath, ReadOnly: requirement.ReadOnly,
			})
		}
	}
	return nil
}

// updatePoolCondition updates a condition on the pool status.
func (r *SandboxPoolReconciler) updatePoolCondition(ctx context.Context, pool *apiv1alpha1.SandboxPool, condition metav1.Condition) error {
	condition.LastTransitionTime = metav1.Now()

	found := false
	for i, c := range pool.Status.Conditions {
		if c.Type == condition.Type {
			pool.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		pool.Status.Conditions = append(pool.Status.Conditions, condition)
	}

	return r.Status().Update(ctx, pool)
}

func boolPtr(b bool) *bool {
	return &b
}

func (r *SandboxPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&apiv1alpha1.SandboxPool{}).
		Owns(&corev1.Pod{}).
		Watches(&apiv1alpha1.Sandbox{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
			sandbox, ok := obj.(*apiv1alpha1.Sandbox)
			if !ok {
				return nil
			}
			if sandbox.Spec.PoolRef != "" {
				return []ctrl.Request{
					{NamespacedName: client.ObjectKey{Name: sandbox.Spec.PoolRef, Namespace: sandbox.Namespace}},
				}
			}
			return nil
		})).
		Complete(r)
}
