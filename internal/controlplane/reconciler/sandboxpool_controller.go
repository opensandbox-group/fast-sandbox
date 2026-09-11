package reconciler

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	infracatalog "fast-sandbox/internal/catalog/infra"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	"fast-sandbox/internal/controlplane/placement"
	"fast-sandbox/internal/fastlet/podcgroup"
	"fast-sandbox/internal/nodecleanup"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/registryconfig"
	"fast-sandbox/internal/runtimeenv"

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
	"sigs.k8s.io/yaml"
)

// SandboxPoolReconciler reconciles SandboxPool resources.
type SandboxPoolReconciler struct {
	client.Client
	DurableReader               client.Reader
	Scheme                      *runtime.Scheme
	Registry                    placement.FastletRegistry
	Catalog                     *runtimecatalog.Catalog
	FastletDrainer              FastletDrainer
	FastletProxyImage           string
	BoxLiteRuntimeImage         string
	RouteVerifyPublicKey        string
	RuntimeEnvironmentNamespace string
	RuntimeEnvironmentConfigMap string
	DrainTimeout                time.Duration
	Now                         func() time.Time
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

	var pool apiv1alpha2.SandboxPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	runtimePlan, err := r.resolveRuntimePlan(ctx, &pool)
	if err != nil {
		logger.Error(err, "Runtime profile resolution failed")
		_ = r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type:    apiv1alpha2.PoolConditionRuntimeReady,
			Status:  metav1.ConditionFalse,
			Reason:  apiv1alpha2.ReasonRuntimeProfileInvalid,
			Message: err.Error(),
		})
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	profile := runtimePlan.Profile
	if profile.Capabilities.DefaultState == runtimecatalog.CapabilityUnsupported {
		_ = r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type:    apiv1alpha2.PoolConditionRuntimeReady,
			Status:  metav1.ConditionFalse,
			Reason:  apiv1alpha2.ReasonRuntimeUnsupported,
			Message: profile.Capabilities.Reason,
		})
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if profile.Capabilities.DefaultState == runtimecatalog.CapabilityDegraded {
		_ = r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type:    apiv1alpha2.PoolConditionRuntimeReady,
			Status:  metav1.ConditionFalse,
			Reason:  apiv1alpha2.ReasonRuntimeUnavailable,
			Message: profile.Capabilities.Reason,
		})
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if err := apiv1alpha2.ValidateSandboxResourceProfile(pool.Spec.SandboxResources); err != nil {
		_ = r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type:    apiv1alpha2.PoolConditionRuntimeReady,
			Status:  metav1.ConditionFalse,
			Reason:  apiv1alpha2.ReasonResourceProfileInvalid,
			Message: err.Error(),
		})
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	compiledRegistry, err := r.ensureRegistrySecret(ctx, &pool)
	if err != nil {
		_ = r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type: apiv1alpha2.PoolConditionRegistryReady, Status: metav1.ConditionFalse,
			Reason: apiv1alpha2.ReasonRegistryInvalid, Message: boundedStatusMessage(err.Error()),
		})
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	_ = r.updatePoolCondition(ctx, &pool, metav1.Condition{
		Type: apiv1alpha2.PoolConditionRegistryReady, Status: metav1.ConditionTrue,
		Reason: apiv1alpha2.ReasonRegistryAvailable, Message: "Registry configuration is valid and compiled",
	})
	infraPlan, err := r.resolveInfraPlan(&pool, profile)
	if err != nil {
		_ = r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type: apiv1alpha2.PoolConditionInfraReady, Status: metav1.ConditionFalse,
			Reason: apiv1alpha2.ReasonInfraComponentsInvalid, Message: err.Error(),
		})
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	_ = r.updatePoolCondition(ctx, &pool, metav1.Condition{
		Type: apiv1alpha2.PoolConditionInfraReady, Status: metav1.ConditionTrue,
		Reason: apiv1alpha2.ReasonInfraComponentsAvailable, Message: "Infra Components are valid for the selected runtime",
	})
	if err := r.ensureInfraPlanConfigMap(ctx, &pool, infraPlan); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureRuntimePlanConfigMap(ctx, &pool, runtimePlan); err != nil {
		return ctrl.Result{}, err
	}

	var childPods corev1.PodList
	if err := r.durableReader().List(ctx, &childPods, client.InNamespace(req.Namespace), client.MatchingLabels(poolLabels(pool.Name))); err != nil {
		return ctrl.Result{}, err
	}
	appliedRegistry, totalRegistry := r.registryRolloutCounts(&pool, compiledRegistry, childPods.Items)
	registryCondition := metav1.Condition{
		Type: apiv1alpha2.PoolConditionRegistryReady, Status: metav1.ConditionTrue,
		Reason: apiv1alpha2.ReasonRegistryAvailable, Message: "Registry configuration is applied to every current Fastlet",
	}
	if appliedRegistry != totalRegistry {
		registryCondition.Status = metav1.ConditionFalse
		registryCondition.Reason = "RegistryPropagating"
		registryCondition.Message = fmt.Sprintf("Registry configuration is applied to %d/%d current Fastlets", appliedRegistry, totalRegistry)
	}
	if err := r.updatePoolCondition(ctx, &pool, registryCondition); err != nil {
		return ctrl.Result{}, err
	}
	runtimeCondition, readyPods := r.runtimeCapabilityCondition(&pool, childPods.Items)
	if err := r.updatePoolCondition(ctx, &pool, runtimeCondition); err != nil {
		return ctrl.Result{}, err
	}
	var allSandboxes apiv1alpha2.SandboxList
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
			if sb.Status.Placement.FastletName != "" {
				identity := sb.Status.Placement.FastletName + "/" + string(sb.Status.Placement.FastletPodUID)
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
	desiredPod, err := r.constructPodWithRuntimePlan(&pool, runtimePlan)
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
	preparedFastlets := r.preparedFastletCount(&pool, infraPlan.Revision)
	warmImageStatuses := r.aggregateWarmImageStatus(&pool, childPods.Items)
	idleFastlets, busyFastlets := r.fastletUtilizationCounts(&pool, childPods.Items)
	if pool.Status.CurrentPods != currentCount ||
		pool.Status.ReadyPods != readyPods || pool.Status.IdleFastlets != idleFastlets ||
		pool.Status.BusyFastlets != busyFastlets ||
		pool.Status.RuntimeRevision != runtimePlan.Revision || pool.Status.InfraRevision != infraPlan.Revision ||
		pool.Status.FastletRevision != desiredPodHash || pool.Status.PreparedFastlets != preparedFastlets ||
		!reflect.DeepEqual(pool.Status.WarmImages, warmImageStatuses) {
		pool.Status.CurrentPods = currentCount
		pool.Status.ReadyPods = readyPods
		pool.Status.IdleFastlets = idleFastlets
		pool.Status.BusyFastlets = busyFastlets
		pool.Status.RuntimeRevision = runtimePlan.Revision
		pool.Status.InfraRevision = infraPlan.Revision
		pool.Status.FastletRevision = desiredPodHash
		pool.Status.PreparedFastlets = preparedFastlets
		pool.Status.WarmImages = warmImageStatuses
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

func (r *SandboxPoolReconciler) aggregateWarmImageStatus(
	pool *apiv1alpha2.SandboxPool,
	pods []corev1.Pod,
) []apiv1alpha2.WarmImageStatus {
	children := childPodIdentities(pods)
	totalFastlets := int32(len(children))
	desired := uniqueWarmImages(pool.Spec.WarmImages)
	result := make([]apiv1alpha2.WarmImageStatus, 0, len(desired))
	byImage := make(map[string]*apiv1alpha2.WarmImageStatus, len(desired))
	for _, image := range desired {
		result = append(result, apiv1alpha2.WarmImageStatus{
			Image: image, DesiredFastlets: totalFastlets, ObservedGeneration: pool.Generation,
		})
		byImage[image] = &result[len(result)-1]
	}
	if r.Registry == nil {
		return result
	}
	for _, info := range r.Registry.GetAllFastlets() {
		if info.Namespace != pool.Namespace || info.PoolName != pool.Name {
			continue
		}
		if _, exists := children[info.PodName+"/"+info.PodUID]; !exists {
			continue
		}
		for _, observed := range info.WarmImages {
			status := byImage[observed.Image]
			if status == nil {
				continue
			}
			switch observed.State {
			case "Cached":
				status.CachedFastlets++
			case "Failed":
				status.FailedFastlets++
				if observed.Message != "" {
					status.LastError = boundedStatusMessage(observed.Message)
				}
			default:
				status.PullingFastlets++
			}
		}
	}
	return result
}

func (r *SandboxPoolReconciler) fastletUtilizationCounts(
	pool *apiv1alpha2.SandboxPool,
	pods []corev1.Pod,
) (idle int32, busy int32) {
	if r.Registry == nil {
		return 0, 0
	}
	children := childPodIdentities(pods)
	for _, info := range r.Registry.GetAllFastlets() {
		if info.Namespace != pool.Namespace || info.PoolName != pool.Name {
			continue
		}
		if _, exists := children[info.PodName+"/"+info.PodUID]; !exists {
			continue
		}
		if !info.PodReady || !info.RuntimeReady || !info.InfraReady || info.Draining || info.DrainRequested {
			continue
		}
		if info.Used() == 0 {
			idle++
		} else {
			busy++
		}
	}
	return idle, busy
}

func childPodIdentities(pods []corev1.Pod) map[string]struct{} {
	children := make(map[string]struct{}, len(pods))
	for index := range pods {
		children[podIdentity(&pods[index])] = struct{}{}
	}
	return children
}

func boundedStatusMessage(message string) string {
	const maxStatusMessage = 512
	if len(message) > maxStatusMessage {
		return message[:maxStatusMessage]
	}
	return message
}

func (r *SandboxPoolReconciler) runtimeCapabilityCondition(pool *apiv1alpha2.SandboxPool, pods []corev1.Pod) (metav1.Condition, int32) {
	condition := metav1.Condition{
		Type:               apiv1alpha2.PoolConditionRuntimeReady,
		Status:             metav1.ConditionFalse,
		Reason:             apiv1alpha2.ReasonRuntimeCapabilityPending,
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
		condition.Reason = apiv1alpha2.ReasonRuntimeAvailable
		condition.Message = fmt.Sprintf("%d child Fastlet pod(s) report the runtime ready", ready)
	} else if observedHeartbeat {
		condition.Reason = apiv1alpha2.ReasonRuntimeUnavailable
		condition.Message = "Child Fastlet heartbeats report no ready runtime"
	}
	return condition, ready
}

func (r *SandboxPoolReconciler) reconcileDraining(
	ctx context.Context,
	pool *apiv1alpha2.SandboxPool,
	pods []corev1.Pod,
	sandboxes []apiv1alpha2.Sandbox,
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

func activeAssignmentsByPod(sandboxes []apiv1alpha2.Sandbox, poolName string) map[string]int {
	result := make(map[string]int)
	for index := range sandboxes {
		sandbox := &sandboxes[index]
		if sandbox.Spec.PoolRef != poolName || sandbox.Status.Placement.FastletName == "" {
			continue
		}
		placement := sandbox.Status.Placement
		result[placement.FastletName+"/"+string(placement.FastletPodUID)]++
	}
	return result
}

func sandboxNeedsPlacement(sandbox *apiv1alpha2.Sandbox) bool {
	if sandbox == nil || sandbox.Status.Placement.FastletName != "" || sandbox.DeletionTimestamp != nil {
		return false
	}
	if sandbox.Status.Runtime.State == apiv1alpha2.RuntimeStopping ||
		sandbox.Status.Runtime.State == apiv1alpha2.RuntimeStopped {
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
func (r *SandboxPoolReconciler) constructPod(pool *apiv1alpha2.SandboxPool, profile runtimecatalog.RuntimeProfile) (*corev1.Pod, error) {
	plan, err := runtimeenv.ResolveDefault(r.Catalog, profile.Name)
	if err != nil {
		return nil, err
	}
	return r.constructPodWithRuntimePlan(pool, plan)
}

func (r *SandboxPoolReconciler) constructPodWithRuntimePlan(pool *apiv1alpha2.SandboxPool, runtimePlan runtimeenv.ResolvedRuntimePlan) (*corev1.Pod, error) {
	if err := pool.Spec.ValidateActionHandlers(); err != nil {
		return nil, err
	}
	profile := runtimePlan.Profile
	sandboxResources := pool.Spec.SandboxResources
	if err := apiv1alpha2.ValidateSandboxResourceProfile(sandboxResources); err != nil {
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
	labels["fast-sandbox.io/infra-revision"] = shortRevision(infraPlan.Revision)
	annotations := make(map[string]string)
	for k, v := range pool.Spec.FastletTemplate.ObjectMeta.Annotations {
		annotations[k] = v
	}
	annotations["fast-sandbox.io/runtime-profile-hash"] = profile.ProfileHash
	annotations["fast-sandbox.io/runtime-plan-revision"] = runtimePlan.Revision
	annotations["fast-sandbox.io/resource-profile-hash"] = sandboxResources.Hash()
	annotations["fast-sandbox.io/infra-revision"] = infraPlan.Revision
	warmImagesJSON, err := json.Marshal(uniqueWarmImages(pool.Spec.WarmImages))
	if err != nil {
		return nil, fmt.Errorf("encode warmImages: %w", err)
	}
	actionHandlersJSON, err := json.Marshal(pool.Spec.ActionHandlers)
	if err != nil {
		return nil, fmt.Errorf("encode actionHandlers: %w", err)
	}

	podSpec := pool.Spec.FastletTemplate.Spec.DeepCopy()
	podSpec.HostNetwork = false
	podSpec.HostPID = false
	podSpec.RuntimeClassName = nil
	podSpec.AutomountServiceAccountToken = boolPtr(false)
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
			corev1.EnvVar{Name: "FAST_SANDBOX_ACTION_HANDLERS", Value: string(actionHandlersJSON)},
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
			corev1.EnvVar{Name: "FAST_SANDBOX_RUNTIME_PLAN_PATH", Value: runtimeenv.PlanMountPath + "/" + runtimeenv.PlanFileName},
			corev1.EnvVar{Name: "FAST_SANDBOX_RESOURCE_CPU", Value: sandboxResources.CPU.String()},
			corev1.EnvVar{Name: "FAST_SANDBOX_RESOURCE_MEMORY", Value: sandboxResources.Memory.String()},
			corev1.EnvVar{Name: "FAST_SANDBOX_RESOURCE_PIDS", Value: strconv.FormatInt(sandboxResources.PIDs, 10)},
			corev1.EnvVar{Name: "FAST_SANDBOX_INFRA_REVISION", Value: infraPlan.Revision},
			corev1.EnvVar{Name: "FAST_SANDBOX_INFRA_PLAN_PATH", Value: "/etc/fast-sandbox/infra/plan.json"},
			corev1.EnvVar{Name: "FAST_SANDBOX_REGISTRY_CONFIG_PATH", Value: registryconfig.MountPath},
			corev1.EnvVar{Name: "RUNTIME_SOCKET", Value: runtimePlan.Containerd.Socket},
			corev1.EnvVar{Name: "FAST_SANDBOX_SNAPSHOTTER", Value: runtimePlan.Containerd.Snapshotter},
			corev1.EnvVar{Name: "FAST_SANDBOX_KUBELET_ROOT", Value: runtimePlan.Kubelet.Root},
			corev1.EnvVar{Name: "INFRA_DIR_IN_POD", Value: "/opt/fast-sandbox/infra"},
		)
		if profile.ResidualProcess != runtimecatalog.ResidualProcessNone {
			c.Env = append(c.Env, corev1.EnvVar{Name: "FAST_SANDBOX_NODE_CLEANUP_SOCKET", Value: nodecleanup.DefaultSocketPath})
		}
		c.VolumeMounts = append(c.VolumeMounts,
			corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"},
			corev1.VolumeMount{Name: "infra-tools", MountPath: "/opt/fast-sandbox/infra"},
			corev1.VolumeMount{Name: "infra-plan", MountPath: "/etc/fast-sandbox/infra", ReadOnly: true},
			corev1.VolumeMount{Name: "runtime-plan", MountPath: runtimeenv.PlanMountPath, ReadOnly: true},
			corev1.VolumeMount{Name: "registry-config", MountPath: "/etc/fast-sandbox/registry", ReadOnly: true},
			corev1.VolumeMount{Name: "proxy-control", MountPath: "/run/fast-sandbox/proxy"},
		)
		if profile.Driver == runtimecatalog.DriverKindContainerd {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
				Name: podcgroup.VolumeName, MountPath: podcgroup.HostRoot,
			})
		}
		if profile.ResidualProcess != runtimecatalog.ResidualProcessNone {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: "node-cleanup", MountPath: filepath.Dir(nodecleanup.DefaultSocketPath)})
		}
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
	if err := ensureBoundedPodContainers(podSpec, runtimeResourceOwner); err != nil {
		return nil, err
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
		corev1.Volume{
			Name: "infra-plan",
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: infraPlanConfigMapName(pool.Name, infraPlan.Revision)},
			}},
		},
		corev1.Volume{
			Name: "runtime-plan",
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: runtimePlanConfigMapName(pool.Name, runtimePlan.Revision)},
			}},
		},
		corev1.Volume{
			Name: "registry-config",
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: registrySecretName(pool.Name),
			}},
		},
		corev1.Volume{Name: "proxy-control", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	)
	if profile.Driver == runtimecatalog.DriverKindContainerd {
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: podcgroup.VolumeName,
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
				Path: podcgroup.HostPath, Type: &hostPathDirectory,
			}},
		})
	}
	if profile.ResidualProcess != runtimecatalog.ResidualProcessNone {
		hostPathDirectoryOrCreate := corev1.HostPathDirectoryOrCreate
		podSpec.Volumes = append(podSpec.Volumes, corev1.Volume{
			Name: "node-cleanup",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
				Path: filepath.Dir(nodecleanup.DefaultSocketPath), Type: &hostPathDirectoryOrCreate,
			}},
		})
	}
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
			{Name: "FAST_SANDBOX_REGISTRY_CONFIG_PATH", Value: registryconfig.MountPath},
		},
		SecurityContext: &corev1.SecurityContext{Privileged: boolPtr(true)},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "boxlite-control", MountPath: "/run/fast-sandbox/boxlite"},
			{Name: "infra-tools", MountPath: "/opt/fast-sandbox/infra", ReadOnly: true},
			{Name: "registry-config", MountPath: "/etc/fast-sandbox/registry", ReadOnly: true},
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
		"tmp":                "/tmp",
		"infra-tools":        "/opt/fast-sandbox/infra",
		"infra-plan":         "/etc/fast-sandbox/infra",
		"runtime-plan":       runtimeenv.PlanMountPath,
		"registry-config":    "/etc/fast-sandbox/registry",
		"proxy-control":      "/run/fast-sandbox/proxy",
		"node-cleanup":       filepath.Dir(nodecleanup.DefaultSocketPath),
		"boxlite-control":    "/run/fast-sandbox/boxlite",
		podcgroup.VolumeName: podcgroup.HostRoot,
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

func getFastletCapacity(pool *apiv1alpha2.SandboxPool) int32 {
	return pool.Spec.MaxSandboxesPerPod
}

func (r *SandboxPoolReconciler) resolveRuntimeProfile(pool *apiv1alpha2.SandboxPool) (runtimecatalog.RuntimeProfile, error) {
	if err := pool.Spec.ValidateRuntime(); err != nil {
		return runtimecatalog.RuntimeProfile{}, err
	}
	catalog := r.Catalog
	if catalog == nil {
		catalog = runtimecatalog.Builtin()
	}
	return catalog.Resolve(pool.Spec.Runtime)
}

func (r *SandboxPoolReconciler) resolveRuntimePlan(ctx context.Context, pool *apiv1alpha2.SandboxPool) (runtimeenv.ResolvedRuntimePlan, error) {
	if err := pool.Spec.ValidateRuntime(); err != nil {
		return runtimeenv.ResolvedRuntimePlan{}, err
	}
	config, err := r.loadRuntimeEnvironmentConfig(ctx)
	if err != nil {
		return runtimeenv.ResolvedRuntimePlan{}, err
	}
	catalog := r.Catalog
	if catalog == nil {
		catalog = runtimecatalog.Builtin()
	}
	return runtimeenv.Resolve(catalog, config, pool.Spec.Runtime)
}

func (r *SandboxPoolReconciler) loadRuntimeEnvironmentConfig(ctx context.Context) (runtimeenv.Config, error) {
	namespace := r.RuntimeEnvironmentNamespace
	if namespace == "" {
		namespace = runtimeenv.SystemNamespace
	}
	name := r.RuntimeEnvironmentConfigMap
	if name == "" {
		name = runtimeenv.ConfigMapName
	}
	var source corev1.ConfigMap
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &source)
	if apierrors.IsNotFound(err) {
		return runtimeenv.DefaultConfig(), nil
	}
	if err != nil {
		return runtimeenv.Config{}, fmt.Errorf("read runtime environment ConfigMap: %w", err)
	}
	raw, found := source.Data[runtimeenv.ConfigMapKey]
	if !found {
		return runtimeenv.Config{}, fmt.Errorf("ConfigMap %s/%s must contain %s", namespace, name, runtimeenv.ConfigMapKey)
	}
	return runtimeenv.Parse([]byte(raw))
}

func (r *SandboxPoolReconciler) resolveInfraPlan(pool *apiv1alpha2.SandboxPool, runtimeProfile runtimecatalog.RuntimeProfile) (infracatalog.Plan, error) {
	return infracatalog.Compile(pool.Spec.InfraComponents, runtimeProfile)
}

func shortRevision(revision string) string {
	value := strings.TrimPrefix(revision, "sha256:")
	if len(value) > 12 {
		value = value[:12]
	}
	return value
}

func infraPlanConfigMapName(poolName, revision string) string {
	suffix := "-infra-" + shortRevision(revision)
	maxPoolLength := 253 - len(suffix)
	if len(poolName) > maxPoolLength {
		poolName = strings.TrimRight(poolName[:maxPoolLength], "-.")
	}
	return poolName + suffix
}

func runtimePlanConfigMapName(poolName, revision string) string {
	suffix := "-runtime-" + shortRevision(revision)
	maxPoolLength := 253 - len(suffix)
	if len(poolName) > maxPoolLength {
		poolName = strings.TrimRight(poolName[:maxPoolLength], "-.")
	}
	return poolName + suffix
}

func registrySecretName(poolName string) string {
	const suffix = "-registry"
	maxPoolLength := 253 - len(suffix)
	if len(poolName) > maxPoolLength {
		poolName = strings.TrimRight(poolName[:maxPoolLength], "-.")
	}
	return poolName + suffix
}

type dockerConfigJSON struct {
	Auths map[string]dockerAuthConfig `json:"auths"`
}

type dockerAuthConfig struct {
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	Auth          string `json:"auth,omitempty"`
	IdentityToken string `json:"identitytoken,omitempty"`
}

func (r *SandboxPoolReconciler) ensureRegistrySecret(ctx context.Context, pool *apiv1alpha2.SandboxPool) (registryconfig.Compiled, error) {
	var source corev1.ConfigMap
	err := r.Get(ctx, client.ObjectKey{Namespace: pool.Namespace, Name: registryconfig.ConfigMapName}, &source)
	if apierrors.IsNotFound(err) {
		compiled, compileErr := registryconfig.NewCompiled(nil)
		if compileErr != nil {
			return registryconfig.Compiled{}, compileErr
		}
		return compiled, r.persistRegistrySecret(ctx, pool, compiled)
	}
	if err != nil {
		return registryconfig.Compiled{}, fmt.Errorf("read Registry ConfigMap: %w", err)
	}
	raw, found := source.Data[registryconfig.ConfigMapKey]
	if !found {
		return registryconfig.Compiled{}, fmt.Errorf("ConfigMap %s must contain %s", registryconfig.ConfigMapName, registryconfig.ConfigMapKey)
	}
	var config registryconfig.Config
	if err := yaml.UnmarshalStrict([]byte(raw), &config); err != nil {
		return registryconfig.Compiled{}, fmt.Errorf("decode Registry configuration: %w", err)
	}
	config, err = registryconfig.NormalizeAndValidate(config)
	if err != nil {
		return registryconfig.Compiled{}, err
	}
	credentials := make([]registryconfig.Credential, 0, len(config.Registries))
	for _, rule := range config.Registries {
		credential, err := r.registryCredentialFromSecret(ctx, pool.Namespace, rule)
		if err != nil {
			return registryconfig.Compiled{}, err
		}
		credentials = append(credentials, credential)
	}
	compiled, err := registryconfig.NewCompiled(credentials)
	if err != nil {
		return registryconfig.Compiled{}, err
	}
	return compiled, r.persistRegistrySecret(ctx, pool, compiled)
}

func (r *SandboxPoolReconciler) registryCredentialFromSecret(
	ctx context.Context,
	namespace string,
	rule registryconfig.RegistryRule,
) (registryconfig.Credential, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: rule.SecretRef.Name}, &secret); err != nil {
		return registryconfig.Credential{}, fmt.Errorf("read Registry Secret %s: %w", rule.SecretRef.Name, err)
	}
	if secret.Type != corev1.SecretTypeDockerConfigJson {
		return registryconfig.Credential{}, fmt.Errorf("Registry Secret %s must have type %s", secret.Name, corev1.SecretTypeDockerConfigJson)
	}
	content := secret.Data[corev1.DockerConfigJsonKey]
	if len(content) == 0 {
		return registryconfig.Credential{}, fmt.Errorf("Registry Secret %s has no %s data", secret.Name, corev1.DockerConfigJsonKey)
	}
	var dockerConfig dockerConfigJSON
	if err := json.Unmarshal(content, &dockerConfig); err != nil {
		return registryconfig.Credential{}, fmt.Errorf("decode Registry Secret %s: %w", secret.Name, err)
	}
	var auth dockerAuthConfig
	found := false
	for host, candidate := range dockerConfig.Auths {
		if registryconfig.NormalizeHost(host) == rule.Host {
			auth = candidate
			found = true
			break
		}
	}
	if !found {
		return registryconfig.Credential{}, fmt.Errorf("Registry Secret %s has no credentials for host %s", secret.Name, rule.Host)
	}
	if auth.Username == "" && auth.Password == "" && auth.Auth != "" {
		decoded, err := base64.StdEncoding.DecodeString(auth.Auth)
		if err != nil {
			return registryconfig.Credential{}, fmt.Errorf("decode auth for host %s in Secret %s: %w", rule.Host, secret.Name, err)
		}
		username, password, found := strings.Cut(string(decoded), ":")
		if !found {
			return registryconfig.Credential{}, fmt.Errorf("auth for host %s in Secret %s is invalid", rule.Host, secret.Name)
		}
		auth.Username, auth.Password = username, password
	}
	if auth.Username == "" && auth.Password == "" && auth.IdentityToken == "" {
		return registryconfig.Credential{}, fmt.Errorf("Registry Secret %s has empty credentials for host %s", secret.Name, rule.Host)
	}
	credential := registryconfig.Credential{
		Host: rule.Host, RepositoryPrefix: rule.RepositoryPrefix,
		Username: auth.Username, Password: auth.Password, IdentityToken: auth.IdentityToken,
	}
	if rule.WriteSecretRef != nil {
		writeUsername, writePassword, err := r.registryWriteCredential(ctx, namespace, rule.Host, *rule.WriteSecretRef)
		if err != nil {
			return registryconfig.Credential{}, err
		}
		credential.WriteUsername, credential.WritePassword = writeUsername, writePassword
	}
	return credential, nil
}

// registryWriteCredentialFromSecret keys of the write (publish) secret; they
// mirror the SandboxTemplate PublishSecretRef convention.
const (
	writeSecretAccessKeyID = "accessKeyId"
	writeSecretAccessKey   = "secretAccessKey"
)

// registryWriteCredential reads the Opaque write secret of an artifact-store
// rule (accessKeyId/secretAccessKey keys) and validates the pair is complete.
func (r *SandboxPoolReconciler) registryWriteCredential(ctx context.Context, namespace, host string, ref registryconfig.SecretRef) (string, string, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
		return "", "", fmt.Errorf("read write Secret %s for host %s: %w", ref.Name, host, err)
	}
	username := strings.TrimSpace(string(secret.Data[writeSecretAccessKeyID]))
	password := strings.TrimSpace(string(secret.Data[writeSecretAccessKey]))
	if username == "" || password == "" {
		return "", "", fmt.Errorf("write Secret %s for host %s must contain non-empty %s and %s", secret.Name, host, writeSecretAccessKeyID, writeSecretAccessKey)
	}
	return username, password, nil
}

func (r *SandboxPoolReconciler) persistRegistrySecret(
	ctx context.Context,
	pool *apiv1alpha2.SandboxPool,
	compiled registryconfig.Compiled,
) error {
	content, err := compiled.Marshal()
	if err != nil {
		return err
	}
	key := client.ObjectKey{Namespace: pool.Namespace, Name: registrySecretName(pool.Name)}
	var secret corev1.Secret
	err = r.Get(ctx, key, &secret)
	if apierrors.IsNotFound(err) {
		secret = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: key.Name, Namespace: key.Namespace,
				Labels: map[string]string{"fast-sandbox.io/pool": pool.Name, "fast-sandbox.io/registry-config": "compiled"},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{registryconfig.SecretKey: content},
		}
		if err := ctrl.SetControllerReference(pool, &secret, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, &secret)
	}
	if err != nil {
		return err
	}
	if string(secret.Data[registryconfig.SecretKey]) == string(content) && secret.Type == corev1.SecretTypeOpaque {
		return nil
	}
	before := secret.DeepCopy()
	secret.Type = corev1.SecretTypeOpaque
	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	secret.Data[registryconfig.SecretKey] = content
	return r.Patch(ctx, &secret, client.MergeFrom(before))
}

func (r *SandboxPoolReconciler) registryRolloutCounts(
	pool *apiv1alpha2.SandboxPool,
	compiled registryconfig.Compiled,
	pods []corev1.Pod,
) (int32, int32) {
	children := childPodIdentities(pods)
	total := int32(len(children))
	if r.Registry == nil {
		return 0, total
	}
	var applied int32
	for _, info := range r.Registry.GetAllFastlets() {
		if info.Namespace != pool.Namespace || info.PoolName != pool.Name || info.RegistryRevision != compiled.Revision {
			continue
		}
		if _, exists := children[info.PodName+"/"+info.PodUID]; exists {
			applied++
		}
	}
	return applied, total
}

func (r *SandboxPoolReconciler) ensureInfraPlanConfigMap(
	ctx context.Context,
	pool *apiv1alpha2.SandboxPool,
	plan infracatalog.Plan,
) error {
	payload, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("encode Infra plan: %w", err)
	}
	key := client.ObjectKey{Namespace: pool.Namespace, Name: infraPlanConfigMapName(pool.Name, plan.Revision)}
	var existing corev1.ConfigMap
	err = r.Get(ctx, key, &existing)
	if err == nil {
		if existing.Data["plan.json"] != string(payload) {
			return fmt.Errorf("immutable Infra plan ConfigMap %s contains a different plan", key.Name)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	immutable := true
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: key.Name, Namespace: key.Namespace,
			Labels: map[string]string{
				"fast-sandbox.io/pool":           pool.Name,
				"fast-sandbox.io/infra-revision": shortRevision(plan.Revision),
			},
			Annotations: map[string]string{"fast-sandbox.io/infra-revision": plan.Revision},
		},
		Immutable: &immutable,
		Data:      map[string]string{"plan.json": string(payload)},
	}
	if err := ctrl.SetControllerReference(pool, configMap, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, configMap); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create immutable Infra plan ConfigMap: %w", err)
	}
	return nil
}

func (r *SandboxPoolReconciler) ensureRuntimePlanConfigMap(
	ctx context.Context,
	pool *apiv1alpha2.SandboxPool,
	plan runtimeenv.ResolvedRuntimePlan,
) error {
	payload, err := plan.Marshal()
	if err != nil {
		return fmt.Errorf("encode runtime plan: %w", err)
	}
	key := client.ObjectKey{Namespace: pool.Namespace, Name: runtimePlanConfigMapName(pool.Name, plan.Revision)}
	var existing corev1.ConfigMap
	err = r.Get(ctx, key, &existing)
	if err == nil {
		if existing.Data[runtimeenv.PlanFileName] != string(payload) {
			return fmt.Errorf("immutable runtime plan ConfigMap %s contains a different plan", key.Name)
		}
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	immutable := true
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name: key.Name, Namespace: key.Namespace,
			Labels: map[string]string{
				"fast-sandbox.io/pool":             pool.Name,
				"fast-sandbox.io/runtime-revision": shortRevision(plan.Revision),
			},
			Annotations: map[string]string{"fast-sandbox.io/runtime-revision": plan.Revision},
		},
		Immutable: &immutable,
		Data:      map[string]string{runtimeenv.PlanFileName: string(payload)},
	}
	if err := ctrl.SetControllerReference(pool, configMap, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, configMap); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create immutable runtime plan ConfigMap: %w", err)
	}
	return nil
}

func (r *SandboxPoolReconciler) preparedFastletCount(pool *apiv1alpha2.SandboxPool, revision string) int32 {
	if r.Registry == nil {
		return 0
	}
	var count int32
	for _, info := range r.Registry.GetAllFastlets() {
		if info.Namespace == pool.Namespace && info.PoolName == pool.Name &&
			info.InfraRevision == revision && info.InfraReady {
			count++
		}
	}
	return count
}

var runtimeOwnedEnv = map[string]struct{}{
	"FAST_SANDBOX_RUNTIME": {}, "FAST_SANDBOX_RUNTIME_PROFILE_HASH": {},
	"FAST_SANDBOX_RUNTIME_PLAN_PATH": {}, "FAST_SANDBOX_SNAPSHOTTER": {}, "FAST_SANDBOX_KUBELET_ROOT": {},
	"FAST_SANDBOX_RESOURCE_CPU": {}, "FAST_SANDBOX_RESOURCE_MEMORY": {}, "FAST_SANDBOX_RESOURCE_PIDS": {},
	"FAST_SANDBOX_INFRA_REVISION": {}, "FAST_SANDBOX_INFRA_PLAN_PATH": {}, "FASTLET_CAPACITY": {},
	"FAST_SANDBOX_REGISTRY_CONFIG_PATH": {},
	"RUNTIME_SOCKET":                    {}, "INFRA_DIR_IN_POD": {},
	"FASTLET_CONTROL_PORT":         {},
	"FASTLET_PROXY_CONTROL_SOCKET": {},
	"FAST_SANDBOX_WARM_IMAGES":     {},
	"FAST_SANDBOX_ACTION_HANDLERS": {},
	"FAST_SANDBOX_ACTIONS":         {},
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

func applyFastletResources(container *corev1.Container, overhead corev1.ResourceList, sandbox apiv1alpha2.SandboxResourceProfile, capacity int32) error {
	defaultAggregate := overhead.DeepCopy()
	if defaultAggregate == nil {
		defaultAggregate = corev1.ResourceList{}
	}
	addScaledQuantity(defaultAggregate, corev1.ResourceCPU, sandbox.CPU, int64(capacity))
	addScaledQuantity(defaultAggregate, corev1.ResourceMemory, sandbox.Memory, int64(capacity))
	if container.Resources.Requests == nil {
		container.Resources.Requests = corev1.ResourceList{}
	}
	if container.Resources.Limits == nil {
		container.Resources.Limits = corev1.ResourceList{}
	}
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		defaultQuantity, configured := defaultAggregate[name]
		if !configured || defaultQuantity.IsZero() {
			continue
		}

		request, hasRequest := container.Resources.Requests[name]
		if hasRequest && request.IsZero() {
			hasRequest = false
		}
		limit, hasLimit := container.Resources.Limits[name]
		if hasLimit && limit.IsZero() {
			hasLimit = false
		}

		if hasLimit {
			minimum := overhead[name]
			if !minimum.IsZero() && limit.Cmp(minimum) < 0 {
				return fmt.Errorf("runtime owner container %q limit %s=%s is below runtime overhead %s", container.Name, name, limit.String(), minimum.String())
			}
			if hasRequest && request.Cmp(limit) > 0 {
				return fmt.Errorf("runtime owner container %q request %s=%s exceeds aggregate limit %s", container.Name, name, request.String(), limit.String())
			}
			if !hasRequest {
				request = defaultQuantity.DeepCopy()
				if request.Cmp(limit) > 0 {
					request = limit.DeepCopy()
				}
				container.Resources.Requests[name] = request
			}
			continue
		}

		// Without an explicit aggregate limit, retain the safe non-overcommitted
		// default. Operators opt into overcommit by setting a lower Fastlet
		// runtime-owner limit in fastletTemplate.
		limit = defaultQuantity.DeepCopy()
		if hasRequest && request.Cmp(limit) > 0 {
			limit = request.DeepCopy()
		}
		container.Resources.Limits[name] = limit
		if !hasRequest {
			container.Resources.Requests[name] = defaultQuantity.DeepCopy()
		}
	}
	return nil
}

func ensureBoundedPodContainers(podSpec *corev1.PodSpec, runtimeResourceOwner string) error {
	for index := range podSpec.Containers {
		container := &podSpec.Containers[index]
		if container.Name == runtimeResourceOwner {
			continue
		}
		if index == 0 || container.Name == "fastlet-proxy" {
			applyDefaultPlatformResources(container)
			continue
		}
		if err := validateContainerHasLimits(container); err != nil {
			return fmt.Errorf("Fastlet sidecar resources: %w", err)
		}
	}
	for index := range podSpec.InitContainers {
		if err := validateContainerHasLimits(&podSpec.InitContainers[index]); err != nil {
			return fmt.Errorf("Fastlet init container resources: %w", err)
		}
	}
	return nil
}

func applyDefaultPlatformResources(container *corev1.Container) {
	if container.Resources.Requests == nil {
		container.Resources.Requests = corev1.ResourceList{}
	}
	if container.Resources.Limits == nil {
		container.Resources.Limits = corev1.ResourceList{}
	}
	defaults := map[corev1.ResourceName]struct {
		request resource.Quantity
		limit   resource.Quantity
	}{
		corev1.ResourceCPU: {
			request: resource.MustParse("50m"),
			limit:   resource.MustParse("250m"),
		},
		corev1.ResourceMemory: {
			request: resource.MustParse("64Mi"),
			limit:   resource.MustParse("256Mi"),
		},
	}
	for name, defaultValue := range defaults {
		request, hasRequest := container.Resources.Requests[name]
		if !hasRequest || request.IsZero() {
			request = defaultValue.request.DeepCopy()
			container.Resources.Requests[name] = request
		}
		limit, hasLimit := container.Resources.Limits[name]
		if !hasLimit || limit.IsZero() {
			limit = defaultValue.limit.DeepCopy()
			if request.Cmp(limit) > 0 {
				limit = request.DeepCopy()
			}
			container.Resources.Limits[name] = limit
		}
	}
}

func validateContainerHasLimits(container *corev1.Container) error {
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		limit, ok := container.Resources.Limits[name]
		if !ok || limit.IsZero() {
			return fmt.Errorf("container %q must define a non-zero %s limit so the Kubernetes Pod cgroup remains bounded", container.Name, name)
		}
		if request, exists := container.Resources.Requests[name]; exists && request.Cmp(limit) > 0 {
			return fmt.Errorf("container %q request %s=%s exceeds limit %s", container.Name, name, request.String(), limit.String())
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
func (r *SandboxPoolReconciler) updatePoolCondition(ctx context.Context, pool *apiv1alpha2.SandboxPool, condition metav1.Condition) error {
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
		For(&apiv1alpha2.SandboxPool{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.ConfigMap{}).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
			runtimeNamespace := r.RuntimeEnvironmentNamespace
			if runtimeNamespace == "" {
				runtimeNamespace = runtimeenv.SystemNamespace
			}
			runtimeConfigMap := r.RuntimeEnvironmentConfigMap
			if runtimeConfigMap == "" {
				runtimeConfigMap = runtimeenv.ConfigMapName
			}
			if obj.GetNamespace() == runtimeNamespace && obj.GetName() == runtimeConfigMap {
				return r.mapAllPools(ctx)
			}
			if obj.GetName() != registryconfig.ConfigMapName {
				return nil
			}
			return r.mapNamespaceToPools(ctx, obj.GetNamespace())
		})).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
			return r.mapNamespaceToPools(ctx, obj.GetNamespace())
		})).
		Watches(&apiv1alpha2.Sandbox{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
			sandbox, ok := obj.(*apiv1alpha2.Sandbox)
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

func (r *SandboxPoolReconciler) mapAllPools(ctx context.Context) []ctrl.Request {
	var pools apiv1alpha2.SandboxPoolList
	if err := r.List(ctx, &pools); err != nil {
		return nil
	}
	result := make([]ctrl.Request, 0, len(pools.Items))
	for index := range pools.Items {
		result = append(result, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&pools.Items[index])})
	}
	return result
}

func (r *SandboxPoolReconciler) mapNamespaceToPools(ctx context.Context, namespace string) []ctrl.Request {
	var pools apiv1alpha2.SandboxPoolList
	if err := r.List(ctx, &pools, client.InNamespace(namespace)); err != nil {
		return nil
	}
	result := make([]ctrl.Request, 0, len(pools.Items))
	for index := range pools.Items {
		result = append(result, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&pools.Items[index])})
	}
	return result
}
