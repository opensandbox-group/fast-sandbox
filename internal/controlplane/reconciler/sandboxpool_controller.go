package reconciler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	infracatalog "fast-sandbox/internal/catalog/infra"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	orchestration "fast-sandbox/internal/controlplane/orchestrator"
	"fast-sandbox/internal/controlplane/placement"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
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
	DurableReader        client.Reader
	Scheme               *runtime.Scheme
	Registry             placement.FastletRegistry
	Catalog              *runtimecatalog.Catalog
	InfraCatalog         *infracatalog.Catalog
	FastletDrainer       FastletDrainer
	FastletProxyImage    string
	BoxLiteRuntimeImage  string
	RouteVerifyPublicKey string
	DrainTimeout         time.Duration
	Now                  func() time.Time
}

type FastletDrainer interface {
	SetDraining(context.Context, string, *fastletapi.SetDrainingRequest) (*fastletapi.SetDrainingResponse, error)
}

const (
	defaultDrainTimeout = 5 * time.Minute
	drainRequeue        = 2 * time.Second
)

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
	if profile.Capabilities.DefaultState == runtimecatalog.CapabilityDegraded {
		_ = r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type:    apiv1alpha1.PoolConditionRuntimeReady,
			Status:  metav1.ConditionFalse,
			Reason:  apiv1alpha1.ReasonRuntimeUnavailable,
			Message: profile.Capabilities.Reason,
		})
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if err := apiv1alpha1.ValidateSandboxResourceProfile(pool.Spec.SandboxResources); err != nil {
		_ = r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type:    apiv1alpha1.PoolConditionRuntimeReady,
			Status:  metav1.ConditionFalse,
			Reason:  apiv1alpha1.ReasonResourceProfileInvalid,
			Message: err.Error(),
		})
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	_, err = r.resolveInfraPlan(&pool, profile)
	if err != nil {
		_ = r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type: apiv1alpha1.PoolConditionInfraReady, Status: metav1.ConditionFalse,
			Reason: apiv1alpha1.ReasonInfraProfileInvalid, Message: err.Error(),
		})
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	_ = r.updatePoolCondition(ctx, &pool, metav1.Condition{
		Type: apiv1alpha1.PoolConditionInfraReady, Status: metav1.ConditionTrue,
		Reason: apiv1alpha1.ReasonInfraProfileAvailable, Message: "InfraProfile is compatible with the selected runtime",
	})

	var childPods corev1.PodList
	if err := r.durableReader().List(ctx, &childPods, client.InNamespace(req.Namespace), client.MatchingLabels(poolLabels(pool.Name))); err != nil {
		return ctrl.Result{}, err
	}
	runtimeCondition, readyPods := r.runtimeCapabilityCondition(&pool, childPods.Items)
	if err := r.updatePoolCondition(ctx, &pool, runtimeCondition); err != nil {
		return ctrl.Result{}, err
	}
	var allSandboxes apiv1alpha1.SandboxList
	if err := r.durableReader().List(ctx, &allSandboxes, client.InNamespace(req.Namespace)); err != nil {
		return ctrl.Result{}, err
	}

	var activeCount, pendingCount int32
	childIdentities := make(map[string]struct{}, len(childPods.Items))
	for index := range childPods.Items {
		childIdentities[podIdentity(&childPods.Items[index])] = struct{}{}
	}
	for _, sb := range allSandboxes.Items {
		if sb.Spec.PoolRef == pool.Name && sb.DeletionTimestamp == nil {
			if sb.Status.Assignment != nil {
				identity := sb.Status.Assignment.FastletName + "/" + sb.Status.Assignment.FastletPodUID
				if _, exists := childIdentities[identity]; exists {
					activeCount++
				}
			} else if sandboxNeedsPlacement(&sb) {
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
	desiredPod, err := r.constructPod(&pool, profile)
	if err != nil {
		return ctrl.Result{}, err
	}
	desiredPodHash := desiredPod.Annotations[placement.AnnotationPodTemplateHash]

	if currentCount < desiredPods {
		diff := desiredPods - currentCount
		logger.Info("Scaling up fastlet pool", "diff", diff)
		for i := int32(0); i < diff; i++ {
			pod := desiredPod.DeepCopy()
			if err := r.Create(ctx, pod); err != nil {
				logger.Error(err, "Failed to create fastlet pod")
				return ctrl.Result{}, err
			}
		}
	}
	if needsPlannedUpgradeSurge(childPods.Items, desiredPods, desiredPodHash) {
		logger.Info("Creating Fastlet surge Pod before planned upgrade drain", "desiredPods", desiredPods, "templateHash", desiredPodHash)
		if err := r.Create(ctx, desiredPod.DeepCopy()); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: drainRequeue}, nil
	}
	if pool.Status.CurrentPods != currentCount || pool.Status.TotalFastlets != currentCount || pool.Status.ReadyPods != readyPods {
		pool.Status.CurrentPods = currentCount
		pool.Status.TotalFastlets = currentCount
		pool.Status.ReadyPods = readyPods
		if err := r.Status().Update(ctx, &pool); err != nil {
			return ctrl.Result{}, err
		}
	}
	if result, handled, err := r.reconcileDraining(ctx, &pool, childPods.Items, allSandboxes.Items, desiredPods, desiredPodHash); err != nil {
		return ctrl.Result{}, err
	} else if handled {
		return result, nil
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *SandboxPoolReconciler) runtimeCapabilityCondition(pool *apiv1alpha1.SandboxPool, pods []corev1.Pod) (metav1.Condition, int32) {
	condition := metav1.Condition{
		Type:               apiv1alpha1.PoolConditionRuntimeReady,
		Status:             metav1.ConditionFalse,
		Reason:             apiv1alpha1.ReasonRuntimeCapabilityPending,
		Message:            "Waiting for a child Fastlet heartbeat with a ready runtime",
		ObservedGeneration: pool.Generation,
	}
	if r.Registry == nil {
		return condition, 0
	}

	children := make(map[string]struct{}, len(pods))
	for index := range pods {
		children[podIdentity(&pods[index])] = struct{}{}
	}
	var ready int32
	observedHeartbeat := false
	for _, info := range r.Registry.GetAllFastlets() {
		if info.Namespace != pool.Namespace || info.PoolName != pool.Name {
			continue
		}
		if _, exists := children[info.PodName+"/"+info.PodUID]; !exists {
			continue
		}
		if !info.LastHeartbeat.IsZero() {
			observedHeartbeat = true
		}
		if info.PodReady && info.RuntimeReady {
			ready++
		}
	}
	if ready > 0 {
		condition.Status = metav1.ConditionTrue
		condition.Reason = apiv1alpha1.ReasonRuntimeAvailable
		condition.Message = fmt.Sprintf("%d child Fastlet pod(s) report the runtime ready", ready)
	} else if observedHeartbeat {
		condition.Reason = apiv1alpha1.ReasonRuntimeUnavailable
		condition.Message = "Child Fastlet heartbeats report no ready runtime"
	}
	return condition, ready
}

func (r *SandboxPoolReconciler) reconcileDraining(
	ctx context.Context,
	pool *apiv1alpha1.SandboxPool,
	pods []corev1.Pod,
	sandboxes []apiv1alpha1.Sandbox,
	desiredPods int32,
	desiredPodHash string,
) (ctrl.Result, bool, error) {
	target := int(len(pods) - int(desiredPods))
	if target < 0 {
		target = 0
	}
	draining := make([]*corev1.Pod, 0, len(pods))
	available := make([]*corev1.Pod, 0, len(pods))
	for index := range pods {
		pod := &pods[index]
		if placement.PodDrainRequested(pod) {
			draining = append(draining, pod)
		} else {
			available = append(available, pod)
		}
	}

	// Demand may recover while a previous scale-down is in progress. A Pod is
	// made schedulable again only after Fastlet has accepted the inverse RPC.
	if len(draining) > target {
		sort.Slice(draining, func(i, j int) bool { return drainStartedAt(draining[i]).After(drainStartedAt(draining[j])) })
		for _, pod := range draining[:len(draining)-target] {
			if err := r.cancelDrain(ctx, pod); err != nil {
				return ctrl.Result{RequeueAfter: drainRequeue}, true, err
			}
		}
		return ctrl.Result{RequeueAfter: drainRequeue}, true, nil
	}

	active := activeAssignmentsByPod(sandboxes, pool.Name)
	if len(draining) < target {
		sort.Slice(available, func(i, j int) bool {
			leftCurrent := fastletPodTemplateCurrent(available[i], desiredPodHash)
			rightCurrent := fastletPodTemplateCurrent(available[j], desiredPodHash)
			if leftCurrent != rightCurrent {
				return !leftCurrent
			}
			left := active[podIdentity(available[i])]
			right := active[podIdentity(available[j])]
			if left != right {
				return left < right
			}
			return available[i].Name < available[j].Name
		})
		count := target - len(draining)
		if count > len(available) {
			count = len(available)
		}
		currentTemplateReady := currentTemplatePodsReady(pods, desiredPodHash, r.Registry)
		for _, pod := range available[:count] {
			reason := placement.DrainReasonScaleDown
			if !fastletPodTemplateCurrent(pod, desiredPodHash) && currentTemplateReady {
				reason = placement.DrainReasonPlannedUpgrade
			}
			if !fastletPodTemplateCurrent(pod, desiredPodHash) && hasCurrentTemplatePod(pods, desiredPodHash) && !currentTemplateReady {
				return ctrl.Result{RequeueAfter: drainRequeue}, true, nil
			}
			if err := r.startDrain(ctx, pod, reason); err != nil {
				return ctrl.Result{RequeueAfter: drainRequeue}, true, err
			}
		}
		return ctrl.Result{RequeueAfter: drainRequeue}, true, nil
	}

	if target == 0 {
		return ctrl.Result{}, false, nil
	}

	now := r.now()
	timeout := r.DrainTimeout
	if timeout <= 0 {
		timeout = defaultDrainTimeout
	}
	for _, pod := range draining {
		acked, err := r.requestDrain(ctx, pod, true, pod.Annotations[placement.AnnotationDrainReason])
		if err != nil {
			klog.FromContext(ctx).Error(err, "Retry Fastlet drain request", "pod", pod.Name)
		}
		empty := active[podIdentity(pod)] == 0
		timedOut := !drainStartedAt(pod).IsZero() && now.Sub(drainStartedAt(pod)) >= timeout
		previouslyAcked := pod.Annotations[placement.AnnotationDrainAckedAt] != ""
		if (empty && (acked || previouslyAcked)) || timedOut {
			if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, true, err
			}
			klog.FromContext(ctx).Info("Deleted drained Fastlet Pod", "pod", pod.Name, "empty", empty, "timedOut", timedOut)
		}
	}
	return ctrl.Result{RequeueAfter: drainRequeue}, true, nil
}

func (r *SandboxPoolReconciler) startDrain(ctx context.Context, pod *corev1.Pod, reason string) error {
	before := pod.DeepCopy()
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[placement.AnnotationDraining] = "true"
	pod.Annotations[placement.AnnotationDrainStartedAt] = r.now().UTC().Format(time.RFC3339Nano)
	pod.Annotations[placement.AnnotationDrainReason] = reason
	delete(pod.Annotations, placement.AnnotationDrainAckedAt)
	if err := r.Patch(ctx, pod, client.MergeFrom(before)); err != nil {
		return fmt.Errorf("persist drain intent for Fastlet Pod %s: %w", pod.Name, err)
	}
	_, err := r.requestDrain(ctx, pod, true, reason)
	return err
}

func (r *SandboxPoolReconciler) cancelDrain(ctx context.Context, pod *corev1.Pod) error {
	if _, err := r.requestDrain(ctx, pod, false, "scale-down-cancelled"); err != nil {
		return fmt.Errorf("cancel drain for Fastlet Pod %s: %w", pod.Name, err)
	}
	before := pod.DeepCopy()
	delete(pod.Annotations, placement.AnnotationDraining)
	delete(pod.Annotations, placement.AnnotationDrainStartedAt)
	delete(pod.Annotations, placement.AnnotationDrainReason)
	delete(pod.Annotations, placement.AnnotationDrainAckedAt)
	return r.Patch(ctx, pod, client.MergeFrom(before))
}

func (r *SandboxPoolReconciler) requestDrain(ctx context.Context, pod *corev1.Pod, draining bool, reason string) (bool, error) {
	if r.FastletDrainer == nil {
		return false, errors.New("Fastlet drain client is not configured")
	}
	if pod.Status.PodIP == "" {
		return false, fmt.Errorf("Fastlet Pod %s has no Pod IP", pod.Name)
	}
	response, err := r.FastletDrainer.SetDraining(ctx, pod.Status.PodIP, &fastletapi.SetDrainingRequest{Draining: draining, Reason: reason})
	if err != nil {
		return false, err
	}
	if response == nil || response.Draining != draining {
		return false, fmt.Errorf("Fastlet Pod %s returned an inconsistent drain state", pod.Name)
	}
	if draining && pod.Annotations[placement.AnnotationDrainAckedAt] == "" {
		before := pod.DeepCopy()
		if pod.Annotations == nil {
			pod.Annotations = make(map[string]string)
		}
		pod.Annotations[placement.AnnotationDrainAckedAt] = r.now().UTC().Format(time.RFC3339Nano)
		if err := r.Patch(ctx, pod, client.MergeFrom(before)); err != nil {
			return false, err
		}
	}
	return true, nil
}

func activeAssignmentsByPod(sandboxes []apiv1alpha1.Sandbox, poolName string) map[string]int {
	result := make(map[string]int)
	for index := range sandboxes {
		sandbox := &sandboxes[index]
		if sandbox.Spec.PoolRef != poolName || sandbox.Status.Assignment == nil {
			continue
		}
		assignment := sandbox.Status.Assignment
		result[assignment.FastletName+"/"+assignment.FastletPodUID]++
	}
	return result
}

func sandboxNeedsPlacement(sandbox *apiv1alpha1.Sandbox) bool {
	if sandbox == nil || sandbox.Status.Assignment != nil || sandbox.DeletionTimestamp != nil {
		return false
	}
	if sandbox.Status.HasCondition(orchestration.ConditionRuntimeReady, metav1.ConditionFalse, orchestration.ReasonExpired) ||
		sandbox.Status.HasCondition(orchestration.ConditionRuntimeReady, metav1.ConditionFalse, orchestration.ReasonFastletPodLost) ||
		sandbox.Status.RuntimeState == apiv1alpha1.ObservedStateDraining {
		return false
	}
	return true
}

func podIdentity(pod *corev1.Pod) string {
	return pod.Name + "/" + string(pod.UID)
}

func drainStartedAt(pod *corev1.Pod) time.Time {
	value, _ := time.Parse(time.RFC3339Nano, pod.Annotations[placement.AnnotationDrainStartedAt])
	return value
}

func (r *SandboxPoolReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *SandboxPoolReconciler) durableReader() client.Reader {
	if r.DurableReader != nil {
		return r.DurableReader
	}
	return r.Client
}

// constructPod builds a Fastlet Pod from the template and a platform-owned
// RuntimeProfile. RuntimeClass and backend handler overrides are never copied
// from the Pool into the Pod.
func (r *SandboxPoolReconciler) constructPod(pool *apiv1alpha1.SandboxPool, profile runtimecatalog.RuntimeProfile) (*corev1.Pod, error) {
	sandboxResources := pool.Spec.SandboxResources
	if err := apiv1alpha1.ValidateSandboxResourceProfile(sandboxResources); err != nil {
		return nil, err
	}
	infraPlan, err := r.resolveInfraPlan(pool, profile)
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
	labels["fast-sandbox.io/infra-profile"] = infraPlan.ProfileName
	annotations := make(map[string]string)
	for k, v := range pool.Spec.FastletTemplate.ObjectMeta.Annotations {
		annotations[k] = v
	}
	annotations["fast-sandbox.io/runtime-profile-hash"] = profile.ProfileHash
	annotations["fast-sandbox.io/resource-profile-hash"] = sandboxResources.Hash()
	annotations["fast-sandbox.io/infra-profile-hash"] = infraPlan.ProfileHash
	warmImagesJSON, err := json.Marshal(uniqueWarmImages(pool.Spec.WarmImages))
	if err != nil {
		return nil, fmt.Errorf("encode warmImages: %w", err)
	}

	podSpec := pool.Spec.FastletTemplate.Spec.DeepCopy()
	podSpec.HostNetwork = false
	podSpec.HostPID = false
	podSpec.RuntimeClassName = nil
	if len(podSpec.Containers) == 0 {
		return nil, errors.New("fastletTemplate.spec.containers must contain the fastlet container")
	}
	for _, container := range podSpec.Containers[1:] {
		if container.Name == "fastlet-proxy" || container.Name == "boxlite-runtime" {
			return nil, fmt.Errorf("%s is a platform-owned sidecar name", container.Name)
		}
	}
	if err := validatePlatformOwnedStorage(podSpec); err != nil {
		return nil, err
	}
	if err := mergeNodeSelector(podSpec, profile.Deployment.NodeSelector); err != nil {
		return nil, err
	}

	runtimeResourceOwner := podSpec.Containers[0].Name
	if profile.Deployment.ResourceOwner != "" {
		runtimeResourceOwner = profile.Deployment.ResourceOwner
	}
	if len(podSpec.Containers) > 0 {
		c := &podSpec.Containers[0]
		if c.SecurityContext == nil {
			c.SecurityContext = &corev1.SecurityContext{}
		}
		c.SecurityContext.Privileged = boolPtr(profile.Deployment.Privileged && profile.Deployment.Sidecar == "")
		c.Env = removeRuntimeOwnedEnv(c.Env)

		c.Env = append(c.Env,
			corev1.EnvVar{Name: "FASTLET_CONTROL_PORT", Value: ":5758"},
			corev1.EnvVar{Name: "FASTLET_PROXY_CONTROL_SOCKET", Value: "/run/fast-sandbox/proxy/control.sock"},
			corev1.EnvVar{Name: "FAST_SANDBOX_WARM_IMAGES", Value: string(warmImagesJSON)},
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
				ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{ContainerName: runtimeResourceOwner, Resource: "limits.cpu"}},
			},
			corev1.EnvVar{
				Name:      "MEMORY_LIMIT",
				ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{ContainerName: runtimeResourceOwner, Resource: "limits.memory"}},
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
			corev1.EnvVar{Name: "FAST_SANDBOX_INFRA_PROFILE", Value: infraPlan.ProfileName},
			corev1.EnvVar{Name: "FAST_SANDBOX_INFRA_PROFILE_HASH", Value: infraPlan.ProfileHash},
			corev1.EnvVar{Name: "RUNTIME_SOCKET", Value: "/run/containerd/containerd.sock"},
			corev1.EnvVar{Name: "INFRA_DIR_IN_POD", Value: "/opt/fast-sandbox/infra"},
		)
		if infraPlanUsesStaticArtifacts(infraPlan) {
			c.Env = append(c.Env, corev1.EnvVar{Name: "FAST_SANDBOX_INFRA_STATIC_ROOTS", Value: "/opt/fast-sandbox/components"})
		}

		c.VolumeMounts = append(c.VolumeMounts,
			corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"},
			corev1.VolumeMount{Name: "infra-tools", MountPath: "/opt/fast-sandbox/infra"},
			corev1.VolumeMount{Name: "proxy-control", MountPath: "/run/fast-sandbox/proxy"},
		)
		if runtimeResourceOwner == c.Name {
			if err := applyFastletResources(c, profile.Deployment.Overhead, sandboxResources, getFastletCapacity(pool)); err != nil {
				return nil, err
			}
		}
		c.ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/readyz", Port: intstr.FromInt32(5758), Scheme: corev1.URISchemeHTTP,
			}},
			InitialDelaySeconds: 0, PeriodSeconds: 2, TimeoutSeconds: 1, FailureThreshold: 1,
		}

	}
	proxyImage := r.FastletProxyImage
	if proxyImage == "" {
		proxyImage = "fast-sandbox/fastlet-proxy:dev"
	}
	podSpec.Containers = append(podSpec.Containers, corev1.Container{
		Name: "fastlet-proxy", Image: proxyImage, ImagePullPolicy: corev1.PullIfNotPresent,
		Env: []corev1.EnvVar{
			{Name: "FASTLET_PROXY_CONTROL_SOCKET", Value: "/run/fast-sandbox/proxy/control.sock"},
			{Name: "FASTLET_PROXY_DATA_ADDRESS", Value: ":5780"},
			{Name: "FASTLET_PROXY_METRICS_ADDRESS", Value: ":9093"},
			{Name: "FAST_SANDBOX_ROUTE_VERIFY_PUBLIC_KEY", Value: r.RouteVerifyPublicKey},
		},
		Ports: []corev1.ContainerPort{
			{Name: "data-proxy", ContainerPort: 5780, Protocol: corev1.ProtocolTCP},
			{Name: "proxy-metrics", ContainerPort: 9093, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: "proxy-control", MountPath: "/run/fast-sandbox/proxy"}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/readyz", Port: intstr.FromInt32(5780), Scheme: corev1.URISchemeHTTP,
			}},
			InitialDelaySeconds: 0, PeriodSeconds: 2, TimeoutSeconds: 1, FailureThreshold: 1,
		},
	})
	if profile.Deployment.Sidecar != "" {
		if profile.Deployment.Sidecar != "boxlite-runtime" || profile.BoxLite == nil {
			return nil, fmt.Errorf("runtime profile %q requests unknown platform sidecar %q", profile.Name, profile.Deployment.Sidecar)
		}
		podSpec.Containers[0].VolumeMounts = append(podSpec.Containers[0].VolumeMounts,
			corev1.VolumeMount{Name: "boxlite-control", MountPath: "/run/fast-sandbox/boxlite"},
		)
		podSpec.Containers = append(podSpec.Containers, r.boxLiteRuntimeContainer(*profile.BoxLite))
		if runtimeResourceOwner != "boxlite-runtime" {
			return nil, fmt.Errorf("BoxLite runtime resource owner must be boxlite-runtime, got %q", runtimeResourceOwner)
		}
		if err := applyFastletResources(&podSpec.Containers[len(podSpec.Containers)-1], profile.Deployment.Overhead, sandboxResources, getFastletCapacity(pool)); err != nil {
			return nil, err
		}
	}

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
		corev1.Volume{Name: "proxy-control", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	)
	runtimeContainer := &podSpec.Containers[0]
	if profile.Deployment.Sidecar != "" {
		podSpec.Volumes = append(podSpec.Volumes,
			corev1.Volume{Name: "boxlite-control", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		)
		runtimeContainer = &podSpec.Containers[len(podSpec.Containers)-1]
	}
	if err := mergeRuntimeHostPaths(podSpec, runtimeContainer, profile.Deployment.HostPaths); err != nil {
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
	if err := stampFastletPodTemplateHash(pod); err != nil {
		return nil, err
	}

	if err := ctrl.SetControllerReference(pool, pod, r.Scheme); err != nil {
		return nil, err
	}
	return pod, nil
}

func stampFastletPodTemplateHash(pod *corev1.Pod) error {
	annotations := make(map[string]string, len(pod.Annotations))
	for key, value := range pod.Annotations {
		if key != placement.AnnotationPodTemplateHash {
			annotations[key] = value
		}
	}
	payload := struct {
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		Spec        corev1.PodSpec    `json:"spec"`
	}{Labels: pod.Labels, Annotations: annotations, Spec: pod.Spec}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode Fastlet Pod template identity: %w", err)
	}
	digest := sha256.Sum256(encoded)
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[placement.AnnotationPodTemplateHash] = fmt.Sprintf("%x", digest)
	return nil
}

func needsPlannedUpgradeSurge(pods []corev1.Pod, desiredPods int32, desiredPodHash string) bool {
	if desiredPods <= 0 || len(pods) != int(desiredPods) {
		return false
	}
	for index := range pods {
		if !fastletPodTemplateCurrent(&pods[index], desiredPodHash) {
			return true
		}
	}
	return false
}

func fastletPodTemplateCurrent(pod *corev1.Pod, desiredPodHash string) bool {
	return pod != nil && desiredPodHash != "" && pod.Annotations[placement.AnnotationPodTemplateHash] == desiredPodHash
}

func hasCurrentTemplatePod(pods []corev1.Pod, desiredPodHash string) bool {
	for index := range pods {
		if fastletPodTemplateCurrent(&pods[index], desiredPodHash) {
			return true
		}
	}
	return false
}

func currentTemplatePodsReady(pods []corev1.Pod, desiredPodHash string, registry placement.FastletRegistry) bool {
	heartbeats := make(map[string]placement.FastletInfo)
	if registry != nil {
		for _, info := range registry.GetAllFastlets() {
			heartbeats[info.Namespace+"/"+info.PodName+"/"+info.PodUID] = info
		}
	}
	found := false
	for index := range pods {
		pod := &pods[index]
		if !fastletPodTemplateCurrent(pod, desiredPodHash) {
			continue
		}
		found = true
		if pod.Status.Phase != corev1.PodRunning || !podConditionTrue(pod.Status.Conditions, corev1.PodReady) {
			return false
		}
		if registry != nil {
			info, exists := heartbeats[pod.Namespace+"/"+pod.Name+"/"+string(pod.UID)]
			if !exists || !info.PodReady || !info.RuntimeReady || !info.InfraReady || info.LastHeartbeat.IsZero() || info.Draining {
				return false
			}
		}
	}
	return found
}

func podConditionTrue(conditions []corev1.PodCondition, conditionType corev1.PodConditionType) bool {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (r *SandboxPoolReconciler) boxLiteRuntimeContainer(config runtimecatalog.BoxLiteConfig) corev1.Container {
	image := r.BoxLiteRuntimeImage
	if image == "" {
		image = "fast-sandbox/boxlite-runtime:dev"
	}
	return corev1.Container{
		Name:            "boxlite-runtime",
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args: []string{
			"--socket", config.ControlSocket,
			"--state-root", config.StateRoot,
		},
		Env: []corev1.EnvVar{
			{
				Name:      "POD_UID",
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}},
			},
			{Name: "FAST_SANDBOX_INFRA_STORE_ROOT", Value: "/opt/fast-sandbox/infra"},
		},
		SecurityContext: &corev1.SecurityContext{Privileged: boolPtr(true)},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "boxlite-control", MountPath: "/run/fast-sandbox/boxlite"},
			{Name: "infra-tools", MountPath: "/opt/fast-sandbox/infra", ReadOnly: true},
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{
				"/usr/local/bin/boxlite-runtime", "--probe-socket", config.ControlSocket,
			}}},
			InitialDelaySeconds: 0, PeriodSeconds: 2, TimeoutSeconds: 4, FailureThreshold: 1,
		},
	}
}

func validatePlatformOwnedStorage(podSpec *corev1.PodSpec) error {
	reservedVolumes := map[string]string{
		"tmp":             "/tmp",
		"infra-tools":     "/opt/fast-sandbox/infra",
		"proxy-control":   "/run/fast-sandbox/proxy",
		"boxlite-control": "/run/fast-sandbox/boxlite",
	}
	for _, volume := range podSpec.Volumes {
		if _, reserved := reservedVolumes[volume.Name]; reserved {
			return fmt.Errorf("%s is a platform-owned volume name", volume.Name)
		}
	}
	for _, container := range append(append([]corev1.Container(nil), podSpec.InitContainers...), podSpec.Containers...) {
		for _, mount := range container.VolumeMounts {
			for name, path := range reservedVolumes {
				if mount.Name == name || mount.MountPath == path {
					return fmt.Errorf("container %q uses volume name %s or mount path %s reserved by the platform", container.Name, name, path)
				}
			}
		}
	}
	return nil
}

func shortProfileIdentity(profile runtimecatalog.RuntimeProfile) string {
	hash := profile.ProfileHash
	if len(hash) > 12 {
		hash = hash[:12]
	}
	return profile.Version + "-" + hash
}

func uniqueWarmImages(images []string) []string {
	seen := make(map[string]struct{}, len(images))
	result := make([]string, 0, len(images))
	for _, image := range images {
		if image == "" {
			continue
		}
		if _, exists := seen[image]; exists {
			continue
		}
		seen[image] = struct{}{}
		result = append(result, image)
	}
	return result
}

func poolLabels(poolName string) map[string]string {
	return map[string]string{
		"fast-sandbox.io/pool": poolName,
		"app":                  "sandbox-fastlet",
	}
}

func getFastletCapacity(pool *apiv1alpha1.SandboxPool) int32 {
	return pool.Spec.MaxSandboxesPerPod
}

func (r *SandboxPoolReconciler) resolveRuntimeProfile(pool *apiv1alpha1.SandboxPool) (runtimecatalog.RuntimeProfile, error) {
	if err := pool.Spec.ValidateRuntime(); err != nil {
		return runtimecatalog.RuntimeProfile{}, err
	}
	catalog := r.Catalog
	if catalog == nil {
		catalog = runtimecatalog.Builtin()
	}
	return catalog.Resolve(pool.Spec.Runtime)
}

func (r *SandboxPoolReconciler) resolveInfraPlan(pool *apiv1alpha1.SandboxPool, runtimeProfile runtimecatalog.RuntimeProfile) (infracatalog.Plan, error) {
	catalog := r.InfraCatalog
	if catalog == nil {
		catalog = infracatalog.Builtin()
	}
	return catalog.Compile(pool.Spec.InfraProfile, runtimeProfile)
}

func infraPlanUsesStaticArtifacts(plan infracatalog.Plan) bool {
	for _, component := range plan.Components {
		if component.Component.Artifact.SourceType == infracatalog.SourceStatic {
			return true
		}
	}
	return false
}

var runtimeOwnedEnv = map[string]struct{}{
	"FAST_SANDBOX_RUNTIME": {}, "FAST_SANDBOX_RUNTIME_PROFILE_HASH": {},
	"FAST_SANDBOX_RESOURCE_CPU": {}, "FAST_SANDBOX_RESOURCE_MEMORY": {}, "FAST_SANDBOX_RESOURCE_PIDS": {},
	"FAST_SANDBOX_INFRA_PROFILE": {}, "FAST_SANDBOX_INFRA_PROFILE_HASH": {}, "FASTLET_CAPACITY": {},
	"FAST_SANDBOX_INFRA_STATIC_ROOTS": {},
	"RUNTIME_SOCKET":                  {}, "INFRA_DIR_IN_POD": {},
	"FASTLET_CONTROL_PORT":         {},
	"FASTLET_PROXY_CONTROL_SOCKET": {},
	"FAST_SANDBOX_WARM_IMAGES":     {},
	"NODE_NAME":                    {}, "POD_NAME": {}, "POD_IP": {}, "POD_UID": {}, "NAMESPACE": {},
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
			return fmt.Errorf("runtime owner container %q limit %s=%s is below runtime requirement %s", container.Name, name, limit.String(), quantity.String())
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
				if mount.MountPath != requirement.MountPath || mount.ReadOnly != requirement.ReadOnly ||
					mountPropagation(mount.MountPropagation) != requirement.MountPropagation {
					return fmt.Errorf("fastletTemplate mount %q conflicts with runtime mount %q", requirement.Name, requirement.MountPath)
				}
			} else if mount.MountPath == requirement.MountPath {
				return fmt.Errorf("fastletTemplate mount path %q is reserved by runtime volume %q", requirement.MountPath, requirement.Name)
			}
		}
		if !mountFound {
			mount := corev1.VolumeMount{Name: requirement.Name, MountPath: requirement.MountPath, ReadOnly: requirement.ReadOnly}
			if requirement.MountPropagation != "" {
				propagation := requirement.MountPropagation
				mount.MountPropagation = &propagation
			}
			container.VolumeMounts = append(container.VolumeMounts, mount)
		}
	}
	return nil
}

func mountPropagation(value *corev1.MountPropagationMode) corev1.MountPropagationMode {
	if value == nil {
		return ""
	}
	return *value
}

// updatePoolCondition updates a condition on the pool status.
func (r *SandboxPoolReconciler) updatePoolCondition(ctx context.Context, pool *apiv1alpha1.SandboxPool, condition metav1.Condition) error {
	condition.ObservedGeneration = pool.Generation
	existing := apiMeta.FindStatusCondition(pool.Status.Conditions, condition.Type)
	if existing != nil && existing.Status == condition.Status && existing.Reason == condition.Reason &&
		existing.Message == condition.Message && existing.ObservedGeneration == condition.ObservedGeneration {
		return nil
	}
	apiMeta.SetStatusCondition(&pool.Status.Conditions, condition)
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
