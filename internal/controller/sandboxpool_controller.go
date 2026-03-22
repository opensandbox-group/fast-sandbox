package controller

import (
	"context"
	"fmt"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	agentruntime "fast-sandbox/internal/agent/runtime"
	"fast-sandbox/internal/controller/agentpool"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

// SandboxPoolReconciler reconciles SandboxPool resources.
type SandboxPoolReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Registry agentpool.AgentRegistry
}

// Reconcile manages the lifecycle of Agent Pods based on the demand from Sandboxes.
func (r *SandboxPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := klog.FromContext(ctx)

	var pool apiv1alpha1.SandboxPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Validate RuntimeClass if specified
	if err := r.validateRuntimeClass(ctx, &pool); err != nil {
		logger.Error(err, "RuntimeClass validation failed")
		_ = r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type:    apiv1alpha1.PoolConditionRuntimeReady,
			Status:  metav1.ConditionFalse,
			Reason:  apiv1alpha1.ReasonRuntimeUnavailable,
			Message: err.Error(),
		})
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Update condition to ready if using secure runtime
	if getRuntimeClassName(&pool) != "" {
		_ = r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type:    apiv1alpha1.PoolConditionRuntimeReady,
			Status:  metav1.ConditionTrue,
			Reason:  apiv1alpha1.ReasonRuntimeAvailable,
			Message: fmt.Sprintf("RuntimeClass %s is available", getRuntimeClassName(&pool)),
		})
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
			if sb.Status.AssignedPod != "" {
				activeCount++
			} else {
				pendingCount++
			}
		}
	}

	maxPerPod := getAgentCapacity(&pool)
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
		logger.Info("Scaling up agent pool", "diff", diff)
		for i := int32(0); i < diff; i++ {
			pod := r.constructPod(&pool)
			if err := r.Create(ctx, pod); err != nil {
				logger.Error(err, "Failed to create agent pod")
				return ctrl.Result{}, err
			}
		}
	} else if currentCount > desiredPods {
		diff := currentCount - desiredPods
		logger.Info("Scaling down agent pool", "diff", diff)
		for i := int32(0); i < diff; i++ {
			pod := childPods.Items[i]
			if err := r.Delete(ctx, &pod); err != nil {
				logger.Error(err, "Failed to delete agent pod", "pod", pod.Name)
				return ctrl.Result{}, err
			}
		}
	}

	pool.Status.CurrentPods = currentCount
	pool.Status.TotalAgents = currentCount
	if err := r.Status().Update(ctx, &pool); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// constructPod builds an Agent Pod from the template with necessary runtime configurations injected.
func (r *SandboxPoolReconciler) constructPod(pool *apiv1alpha1.SandboxPool) *corev1.Pod {
	labels := make(map[string]string)
	for k, v := range pool.Spec.AgentTemplate.ObjectMeta.Labels {
		labels[k] = v
	}
	for k, v := range poolLabels(pool.Name) {
		labels[k] = v
	}

	podSpec := pool.Spec.AgentTemplate.Spec.DeepCopy()
	podSpec.HostNetwork = false
	podSpec.HostPID = false

	if len(podSpec.Containers) > 0 {
		c := &podSpec.Containers[0]
		if c.SecurityContext == nil {
			c.SecurityContext = &corev1.SecurityContext{}
		}
		c.SecurityContext.Privileged = boolPtr(true)

		c.Env = append(c.Env,
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
				ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{ContainerName: "agent", Resource: "limits.cpu"}},
			},
			corev1.EnvVar{
				Name:      "MEMORY_LIMIT",
				ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{ContainerName: "agent", Resource: "limits.memory"}},
			},
			corev1.EnvVar{
				Name:  "AGENT_CAPACITY",
				Value: fmt.Sprintf("%d", getAgentCapacity(pool)),
			},
			corev1.EnvVar{
				Name:  "RUNTIME_TYPE",
				Value: string(getRuntimeType(pool)),
			},
			corev1.EnvVar{
				Name:  "RUNTIME_HANDLER",
				Value: getContainerdRuntimeHandler(pool),
			},
			corev1.EnvVar{Name: "RUNTIME_SOCKET", Value: "/run/containerd/containerd.sock"},
			corev1.EnvVar{Name: "INFRA_DIR_IN_POD", Value: "/opt/fast-sandbox/infra"},
		)

		c.VolumeMounts = append(c.VolumeMounts,
			corev1.VolumeMount{Name: "containerd-run", MountPath: "/run/containerd"},
			corev1.VolumeMount{Name: "containerd-root", MountPath: "/var/lib/containerd"},
			corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"},
			corev1.VolumeMount{Name: "infra-tools", MountPath: "/opt/fast-sandbox/infra"},
		)

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
			Name:         "containerd-run",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/run/containerd", Type: &hostPathDirectory}},
		},
		corev1.Volume{
			Name:         "containerd-root",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/containerd", Type: &hostPathDirectory}},
		},
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

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: pool.Name + "-agent-",
			Namespace:    pool.Namespace,
			Labels:       labels,
		},
		Spec: *podSpec,
	}

	ctrl.SetControllerReference(pool, pod, r.Scheme)
	return pod
}

func poolLabels(poolName string) map[string]string {
	return map[string]string{
		"fast-sandbox.io/pool": poolName,
		"app":                  "sandbox-agent",
	}
}

func getAgentCapacity(pool *apiv1alpha1.SandboxPool) int32 {
	if pool.Spec.MaxSandboxesPerPod > 0 {
		return pool.Spec.MaxSandboxesPerPod
	}
	return 5
}

func getRuntimeType(pool *apiv1alpha1.SandboxPool) apiv1alpha1.RuntimeType {
	if pool.Spec.RuntimeType != "" {
		return pool.Spec.RuntimeType
	}
	return apiv1alpha1.RuntimeContainer
}

// getRuntimeClassName returns the RuntimeClassName for the pool.
// Returns empty string for default container runtime.
func getRuntimeClassName(pool *apiv1alpha1.SandboxPool) string {
	if pool.Spec.RuntimeType == "" || pool.Spec.RuntimeType == apiv1alpha1.RuntimeContainer {
		return ""
	}
	if pool.Spec.RuntimeClassName != "" {
		return pool.Spec.RuntimeClassName
	}
	return string(pool.Spec.RuntimeType)
}

// getContainerdRuntimeHandler returns the containerd runtime handler for the pool.
func getContainerdRuntimeHandler(pool *apiv1alpha1.SandboxPool) string {
	if pool.Spec.ContainerdRuntimeHandler != "" {
		return pool.Spec.ContainerdRuntimeHandler
	}
	return agentruntime.GetRuntimeHandler(agentruntime.RuntimeType(pool.Spec.RuntimeType))
}

// validateRuntimeClass checks if the specified RuntimeClass exists.
func (r *SandboxPoolReconciler) validateRuntimeClass(ctx context.Context, pool *apiv1alpha1.SandboxPool) error {
	runtimeClassName := getRuntimeClassName(pool)
	if runtimeClassName == "" {
		return nil // No validation needed for default runtime
	}

	runtimeClass := &nodev1.RuntimeClass{}
	if err := r.Get(ctx, client.ObjectKey{Name: runtimeClassName}, runtimeClass); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("RuntimeClass %q not found", runtimeClassName)
		}
		return fmt.Errorf("failed to get RuntimeClass %q: %w", runtimeClassName, err)
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
