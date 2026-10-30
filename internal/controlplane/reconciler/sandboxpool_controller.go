package reconciler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	infracatalog "fast-sandbox/internal/catalog/infra"
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

// SandboxPoolReconciler reconciles SandboxPool resources.
type SandboxPoolReconciler struct {
	client.Client
	DurableReader                       client.Reader
	Scheme                              *runtime.Scheme
	Registry                            placement.FastletRegistry
	Catalog                             *runtimecatalog.Catalog
	FastletDrainer                      FastletDrainer
	FastletProxyImage                   string
	RouteVerifyPublicKey                string
	RuntimeEnvironmentNamespace         string
	RuntimeEnvironmentConfigMap         string
	DrainTimeout                        time.Duration
	FastletStartupGrace                 time.Duration
	ResourceScaleDownStabilization      time.Duration
	DisableResourceAwareStaticAdmission bool
	DisableResourceAwareMemoryAdmission bool
	DisableResourceAwarePoolScaling     bool
	Now                                 func() time.Time
}

type FastletDrainer interface {
	SetDraining(context.Context, string, *fastletapi.SetDrainingRequest) (*fastletapi.SetDrainingResponse, error)
}

const (
	defaultDrainTimeout                   = 5 * time.Minute
	drainRequeue                          = 2 * time.Second
	defaultFastletStartupGrace            = 2 * time.Minute
	defaultResourceScaleDownStabilization = time.Minute
)

const (
	// poolLabelKey labels platform-owned Fastlet Pods and derived objects with their owning SandboxPool.
	poolLabelKey = "fast-sandbox.io/pool"
	// envFastletProxyControlSocket carries the fastlet-proxy control socket path into containers.
	envFastletProxyControlSocket = "FASTLET_PROXY_CONTROL_SOCKET"
	// envPodName projects the Fastlet Pod name into the container environment.
	envPodName = "POD_NAME"
	// envPodUID projects the Fastlet Pod UID into the container environment.
	envPodUID = "POD_UID"
	// envFastSandboxRegistryConfigPath carries the registry config file path into containers.
	envFastSandboxRegistryConfigPath = "FAST_SANDBOX_REGISTRY_CONFIG_PATH"
	// infraHostPath is the in-Pod directory where infra component tools are exposed.
	infraHostPath = "/opt/fast-sandbox/infra"
	// tmpDir is the node's /tmp shared into the Fastlet container.
	tmpDir = "/tmp"
	// infraToolsVolumeName is the reserved volume carrying infra component tools.
	infraToolsVolumeName = "infra-tools"
	// infraPlanVolumeName is the reserved volume mounting the compiled infra plan.
	infraPlanVolumeName = "infra-plan"
	// runtimePlanVolumeName is the reserved volume mounting the resolved runtime plan.
	runtimePlanVolumeName = "runtime-plan"
	// registryConfigDir is the in-Pod directory holding the compiled registry config.
	registryConfigDir = "/etc/fast-sandbox/registry"
	// proxyRunDir is the shared runtime directory for the fastlet-proxy control socket.
	proxyRunDir = "/run/fast-sandbox/proxy"
	// nodeCleanupVolumeName is the reserved volume exposing the node-cleanup socket directory.
	nodeCleanupVolumeName = "node-cleanup"
	// fastletProxyContainerName is the name of the Fastlet Proxy sidecar container.
	fastletProxyContainerName = "fastlet-proxy"
	// tmpVolumeName is the reserved scratch volume name.
	tmpVolumeName = "tmp"
	// registryConfigVolumeName is the reserved volume mounting the registry config.
	registryConfigVolumeName = "registry-config"
	// proxyControlVolumeName is the reserved volume sharing the proxy control socket.
	proxyControlVolumeName = "proxy-control"
)

// Reconcile manages the lifecycle of Fastlet Pods based on the demand from Sandboxes.
func (r *SandboxPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) { //nolint:gocognit,gocyclo,maintidx // pre-existing reconcile state machine; refactor tracked separately
	logger := klog.FromContext(ctx)

	var pool apiv1alpha2.SandboxPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// The Identity struct has no pool field, so attach the pool key directly:
	// every log in this reconcile (drain retries, scale-up, condition writes)
	// must stay attributable when multiple pools share the controller.
	logger = logger.WithValues("pool", req.NamespacedName.String())
	ctx = klog.NewContext(ctx, logger)

	runtimePlan, err := r.resolveRuntimePlan(ctx, &pool)
	if err != nil {
		logger.Error(err, "Runtime profile resolution failed")
		if condErr := r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type:    apiv1alpha2.PoolConditionRuntimeReady,
			Status:  metav1.ConditionFalse,
			Reason:  apiv1alpha2.ReasonRuntimeProfileInvalid,
			Message: err.Error(),
		}); condErr != nil {
			logger.Error(condErr, "Failed to update Pool condition", "condition", apiv1alpha2.PoolConditionRuntimeReady)
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	profile := runtimePlan.Profile
	if profile.Capabilities.DefaultState == runtimecatalog.CapabilityUnsupported {
		logger.V(1).Info("pool runtime unsupported by this node", "reason", profile.Capabilities.Reason)
		if condErr := r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type:    apiv1alpha2.PoolConditionRuntimeReady,
			Status:  metav1.ConditionFalse,
			Reason:  apiv1alpha2.ReasonRuntimeUnsupported,
			Message: profile.Capabilities.Reason,
		}); condErr != nil {
			logger.Error(condErr, "Failed to update Pool condition", "condition", apiv1alpha2.PoolConditionRuntimeReady)
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if profile.Capabilities.DefaultState == runtimecatalog.CapabilityDegraded {
		logger.V(1).Info("pool runtime degraded on this node", "reason", profile.Capabilities.Reason)
		if condErr := r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type:    apiv1alpha2.PoolConditionRuntimeReady,
			Status:  metav1.ConditionFalse,
			Reason:  apiv1alpha2.ReasonRuntimeUnavailable,
			Message: profile.Capabilities.Reason,
		}); condErr != nil {
			logger.Error(condErr, "Failed to update Pool condition", "condition", apiv1alpha2.PoolConditionRuntimeReady)
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if err := apiv1alpha2.ValidateSandboxResourceProfile(pool.Spec.SandboxResources); err != nil {
		logger.V(1).Info("pool sandbox resource profile is invalid", "reason", err.Error())
		if condErr := r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type:    apiv1alpha2.PoolConditionRuntimeReady,
			Status:  metav1.ConditionFalse,
			Reason:  apiv1alpha2.ReasonResourceProfileInvalid,
			Message: err.Error(),
		}); condErr != nil {
			logger.Error(condErr, "Failed to update Pool condition", "condition", apiv1alpha2.PoolConditionRuntimeReady)
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	resolvedResources, err := compileFastletResources(&pool, profile.Deployment.Overhead)
	if err != nil {
		logger.V(1).Info("pool Fastlet resource contract is invalid", "reason", err.Error())
		if condErr := r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type:    apiv1alpha2.PoolConditionRuntimeReady,
			Status:  metav1.ConditionFalse,
			Reason:  apiv1alpha2.ReasonResourceProfileInvalid,
			Message: err.Error(),
		}); condErr != nil {
			logger.Error(condErr, "Failed to update Pool condition", "condition", apiv1alpha2.PoolConditionRuntimeReady)
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	r.applyResourceAwareFeatureGates(&resolvedResources)
	compiledRegistry, err := r.ensureRegistrySecret(ctx, &pool)
	if err != nil {
		logger.V(1).Info("pool registry configuration is invalid", "reason", boundedStatusMessage(err.Error()))
		if condErr := r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type: apiv1alpha2.PoolConditionRegistryReady, Status: metav1.ConditionFalse,
			Reason: apiv1alpha2.ReasonRegistryInvalid, Message: boundedStatusMessage(err.Error()),
		}); condErr != nil {
			logger.Error(condErr, "Failed to update Pool condition", "condition", apiv1alpha2.PoolConditionRegistryReady)
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	logger.V(4).Info("pool registry configuration compiled", "revision", compiledRegistry.Revision)
	infraPlan, infraErr := r.resolveInfraPlan(&pool, profile)
	infraCondition := metav1.Condition{
		Type: apiv1alpha2.PoolConditionInfraReady, Status: metav1.ConditionTrue,
		Reason: apiv1alpha2.ReasonInfraComponentsAvailable, Message: "Infra Components are valid for the selected runtime",
	}
	if infraErr != nil {
		logger.V(1).Info("pool Infra Components are invalid", "reason", infraErr.Error())
		infraCondition.Status = metav1.ConditionFalse
		infraCondition.Reason = apiv1alpha2.ReasonInfraComponentsInvalid
		infraCondition.Message = infraErr.Error()
	}
	if err := r.updatePoolCondition(ctx, &pool, infraCondition); err != nil {
		return ctrl.Result{}, err
	}

	// Report registry rollout even when infra validation failed, but do not
	// provision Fastlets until both configurations are valid.
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
	if infraErr != nil {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil //nolint:nilerr // invalid configuration is reported through InfraReady and retried at a fixed interval
	}
	if err := r.ensureInfraPlanConfigMap(ctx, &pool, infraPlan); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureRuntimePlanConfigMap(ctx, &pool, runtimePlan); err != nil {
		return ctrl.Result{}, err
	}
	settings, err := buildFastletSettingsFromResolved(&pool, resolvedResources)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureFastletSettingsConfigMap(ctx, &pool, settings); err != nil {
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

	resourceAware := pool.Spec.SandboxResources.ResourceMode() == apiv1alpha2.SandboxResourceModeResourceAware
	resourceAwareScaling := resourceAware && !r.DisableResourceAwarePoolScaling
	childIdentities := make(map[string]struct{}, len(childPods.Items))
	for index := range childPods.Items {
		childIdentities[podIdentity(&childPods.Items[index])] = struct{}{}
	}
	now := r.now()
	observedFastlets := resourceFastletsForPoolChildren(r.Registry, &pool, childPods.Items)
	blockedNodes := make(map[string]struct{})
	if r.Registry != nil {
		blockedNodes = placement.FreshHostMemoryPressureNodes(
			r.Registry.GetAllFastlets(), now, placement.DefaultObservationStaleAfter,
		)
	}
	var activeCount, pendingCount int32
	var pendingDemands []placement.ResourceDemand
	if resourceAwareScaling {
		resolution, resolveErr := resolvePoolPendingResources(&pool, allSandboxes.Items, observedFastlets, now)
		err = resolveErr
		if err != nil {
			return ctrl.Result{}, err
		}
		pendingDemands = resolution.admissible
		pendingCount = int32(len(pendingDemands))
		for _, rejected := range resolution.unadmittable {
			logger.V(2).Info("excluding unadmittable Sandbox from Pool demand",
				"sandbox", rejected.key.String(), "reason", rejected.reason, "message", rejected.message)
		}
	} else {
		activeCount, pendingCount = poolSlotDemandCounts(allSandboxes.Items, pool.Name, childIdentities)
	}

	maxPerPod := getFastletCapacity(&pool)
	if maxPerPod <= 0 {
		maxPerPod = 1
	}

	desiredPod, err := r.constructPodWithResolvedResources(&pool, runtimePlan, resolvedResources, settings)
	if err != nil {
		return ctrl.Result{}, err
	}
	desiredPods := int32(0)
	resourceScalePlan := resourceAwareScalePlan{}
	if resourceAwareScaling {
		resourceScalePlan = r.planResourceAwareScaleWithDemand(
			&pool, childPods.Items, allSandboxes.Items, pendingDemands, resolvedResources.CommitmentCapacity, observedFastlets, blockedNodes, now,
		)
		desiredPods = resourceScalePlan.desiredPods
	} else {
		totalNeededSlots := activeCount + pendingCount + pool.Spec.Capacity.BufferMin
		desiredPods = (totalNeededSlots + maxPerPod - 1) / maxPerPod
	}

	currentCount := int32(len(childPods.Items))
	if desiredPods < pool.Spec.Capacity.PoolMin {
		desiredPods = pool.Spec.Capacity.PoolMin
	}
	requestedDesiredPods := desiredPods
	capacityLimited := pool.Spec.Capacity.PoolMax > 0 && requestedDesiredPods > pool.Spec.Capacity.PoolMax &&
		(!resourceAwareScaling || requestedDesiredPods > currentCount)
	if pool.Spec.Capacity.PoolMax > 0 && desiredPods > pool.Spec.Capacity.PoolMax {
		desiredPods = pool.Spec.Capacity.PoolMax
	}
	if err := r.updatePoolCondition(ctx, &pool, poolCapacityCondition(&pool, requestedDesiredPods, capacityLimited)); err != nil {
		return ctrl.Result{}, err
	}

	var scaleDownCandidate *metav1.Condition
	if resourceAwareScaling {
		// Every persisted drain is already a committed removal, including a
		// planned upgrade. Stabilize only additional shrink so a later reconcile
		// does not erase the durable target before the draining Pod is deleted.
		stabilizationBaseline := currentCount - resourceScalePlan.activeDrains
		desiredPods, scaleDownCandidate = r.stabilizeResourceAwareScaleDown(&pool, stabilizationBaseline, desiredPods)
	}
	desiredPodHash := desiredPod.Annotations[placement.AnnotationPodTemplateHash]

	if currentCount < desiredPods {
		diff := desiredPods - currentCount
		logger.Info("scaling up fastlet pool", "diff", diff)
		for range diff {
			pod := desiredPod.DeepCopy()
			if err := r.Create(ctx, pod); err != nil {
				logger.Error(err, "Failed to create fastlet pod")
				return ctrl.Result{}, err
			}
		}
	}
	if needsPlannedUpgradeSurge(childPods.Items, desiredPods, desiredPodHash) {
		logger.Info("creating Fastlet surge Pod before planned upgrade drain", "desiredPods", desiredPods, "templateHash", desiredPodHash)
		if err := r.Create(ctx, desiredPod.DeepCopy()); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: drainRequeue}, nil
	}
	preparedFastlets := r.preparedFastletCount(&pool, infraPlan.Revision)
	warmImageStatuses := r.aggregateWarmImageStatus(&pool, childPods.Items)
	idleFastlets, busyFastlets := r.fastletUtilizationCounts(&pool, childPods.Items)
	fastletSummary, resourceSummary := summarizePoolFastlets(
		&pool, childPods.Items, desiredPods, readyPods, idleFastlets, busyFastlets, observedFastlets, blockedNodes, now,
	)
	sandboxSummary := summarizePoolSandboxes(allSandboxes.Items, pool.Name)
	observation := poolObservation{
		currentPods:        currentCount,
		readyPods:          readyPods,
		idleFastlets:       idleFastlets,
		busyFastlets:       busyFastlets,
		runtimeRevision:    runtimePlan.Revision,
		infraRevision:      infraPlan.Revision,
		fastletRevision:    desiredPodHash,
		preparedFastlets:   preparedFastlets,
		scaleDownCandidate: scaleDownCandidate,
		warmImages:         warmImageStatuses,
		fastlets:           fastletSummary,
		resources:          resourceSummary,
		sandboxes:          sandboxSummary,
		observedAt:         now,
	}
	if err := r.updatePoolStatus(ctx, &pool, observation.apply); err != nil {
		return ctrl.Result{}, err
	}
	if result, handled, err := r.reconcileDraining(
		ctx, &pool, childPods.Items, allSandboxes.Items, desiredPods, desiredPodHash,
		resourceAwareScaling, resourceScalePlan.drainCandidates, observedFastlets,
	); err != nil {
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
				// Registry iteration order is unspecified; select a stable error
				// so unchanged reports cannot trigger another status write.
				if msg := boundedStatusMessage(observed.Message); msg != "" &&
					(status.LastError == "" || msg < status.LastError) {
					status.LastError = msg
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

type resourceAwareDrainCandidate struct {
	identity string
	free     int32
}

type resourceAwareScalePlan struct {
	desiredPods           int32
	drainCandidates       []string
	activeDrains          int32
	activeScaleDownDrains int32
}

type resourceAwareCapacityView struct {
	freeSlots             int32
	freeBins              []resourceAwareFreeBin
	removable             []resourceAwareDrainCandidate
	activeDrains          int32
	activeScaleDownDrains int32
}

type resourceAwareFreeBin struct {
	slots     int32
	resources placement.ResourceDemand
}

type unadmittableSandboxResource struct {
	key     types.NamespacedName
	reason  string
	message string
}

type poolPendingResourceResolution struct {
	admissible   []placement.ResourceDemand
	unadmittable []unadmittableSandboxResource
}

func (r *SandboxPoolReconciler) planResourceAwareScaleWithDemand(
	pool *apiv1alpha2.SandboxPool,
	pods []corev1.Pod,
	sandboxes []apiv1alpha2.Sandbox,
	pendingDemands []placement.ResourceDemand,
	commitmentCapacity apiv1alpha2.SandboxComputeResources,
	observed map[string]placement.FastletInfo,
	blockedNodes map[string]struct{},
	now time.Time,
) resourceAwareScalePlan {
	if blockedNodes == nil {
		infos := make([]placement.FastletInfo, 0, len(observed))
		for _, info := range observed {
			infos = append(infos, info)
		}
		blockedNodes = placement.FreshHostMemoryPressureNodes(infos, now, placement.DefaultObservationStaleAfter)
	}
	pendingSandboxes := int32(len(pendingDemands))
	current := int32(len(pods))
	plan := resourceAwareScalePlan{desiredPods: current}
	nominal := getFastletCapacity(pool)
	if nominal <= 0 {
		nominal = 1
	}
	requests, ok := pool.Spec.SandboxResources.RequestResources()
	if !ok {
		return plan
	}
	demand := placement.ResourceDemand{CPU: requests.CPU, Memory: requests.Memory}
	view := r.observeResourceAwareCapacity(pool, pods, sandboxes, commitmentCapacity, observed, blockedNodes, now, demand, nominal)
	plan.activeDrains = view.activeDrains
	plan.activeScaleDownDrains = view.activeScaleDownDrains
	availableFree := view.freeSlots
	plan.desiredPods = current - plan.activeDrains
	podsNeeded, packable := additionalFastletsForDemands(
		pendingDemands, pool.Spec.Capacity.BufferMin, demand, view.freeBins,
		resourceAwareFreeBin{slots: nominal, resources: placement.ResourceDemand{
			CPU: commitmentCapacity.CPU.DeepCopy(), Memory: commitmentCapacity.Memory.DeepCopy(),
		}},
	)
	if !packable {
		// A demand that cannot fit an empty Fastlet cannot be repaired by
		// adding more identical Pods. Request validation owns that error.
		return plan
	}
	if podsNeeded > 0 {
		// Canceling an in-progress resource scale-down can recover at most
		// one nominal Fastlet per drain. Create only the remaining Pods.
		podsNeeded -= min(podsNeeded, plan.activeScaleDownDrains)
		plan.desiredPods = current + podsNeeded
		return plan
	}
	if pendingSandboxes > 0 {
		return plan
	}

	retainedFree := max(pool.Spec.Capacity.BufferMax, pool.Spec.Capacity.BufferMin)
	sort.Slice(view.removable, func(i, j int) bool {
		if view.removable[i].free != view.removable[j].free {
			return view.removable[i].free < view.removable[j].free
		}
		return view.removable[i].identity < view.removable[j].identity
	})
	for _, candidate := range view.removable {
		if plan.desiredPods <= pool.Spec.Capacity.PoolMin || availableFree-candidate.free < retainedFree {
			break
		}
		availableFree -= candidate.free
		plan.desiredPods--
		plan.drainCandidates = append(plan.drainCandidates, candidate.identity)
	}
	return plan
}

func (r *SandboxPoolReconciler) observeResourceAwareCapacity(
	pool *apiv1alpha2.SandboxPool,
	pods []corev1.Pod,
	sandboxes []apiv1alpha2.Sandbox,
	commitmentCapacity apiv1alpha2.SandboxComputeResources,
	observed map[string]placement.FastletInfo,
	blockedNodes map[string]struct{},
	now time.Time,
	demand placement.ResourceDemand,
	nominal int32,
) resourceAwareCapacityView {
	view := resourceAwareCapacityView{
		freeBins:  make([]resourceAwareFreeBin, 0, len(pods)),
		removable: make([]resourceAwareDrainCandidate, 0, len(pods)),
	}
	awaitingAdmission := awaitingAdmissionByFastlet(sandboxes, observed, now)
	for index := range pods {
		pod := &pods[index]
		identity := podIdentity(pod)
		drainRequested := placement.PodDrainRequested(pod)
		if drainRequested {
			view.activeDrains++
			if pod.Annotations[placement.AnnotationDrainReason] == placement.DrainReasonScaleDown {
				view.activeScaleDownDrains++
			}
		}
		// The Pod annotation is the durable drain intent. Do not wait for the
		// next heartbeat to remove that Fastlet from available capacity.
		if drainRequested {
			continue
		}
		// Node memory pressure applies before a Fastlet has emitted its first
		// heartbeat too. Once the scheduler has bound the Pod, its node name is
		// enough to keep startup-grace capacity aligned with placement's
		// node-wide pressure filter.
		if _, blocked := blockedNodes[pod.Spec.NodeName]; pod.Spec.NodeName != "" && blocked {
			continue
		}
		info, exists := observed[identity]
		if !exists || info.LastHeartbeat.IsZero() {
			if r.podWithinStartupGrace(pod, now) {
				view.freeSlots += nominal
				view.freeBins = append(view.freeBins, resourceAwareFreeBin{
					slots: nominal,
					resources: placement.ResourceDemand{
						CPU: commitmentCapacity.CPU.DeepCopy(), Memory: commitmentCapacity.Memory.DeepCopy(),
					},
				})
			}
			continue
		}
		if _, blocked := blockedNodes[info.NodeName]; blocked {
			continue
		}
		schedulability := placement.ResourceSchedulability(info, now, placement.DefaultObservationStaleAfter)
		free := placement.EffectiveFreeCapacity(info, demand, now, placement.DefaultObservationStaleAfter)
		view.freeSlots += free
		freeResources := schedulability.FreeResources
		slotFree := schedulability.FreeSlots
		if slotFree > 0 && freeResources.CPU.Sign() > 0 && freeResources.Memory.Sign() > 0 {
			view.freeBins = append(view.freeBins, resourceAwareFreeBin{
				slots: slotFree,
				resources: placement.ResourceDemand{
					CPU: freeResources.CPU.DeepCopy(), Memory: freeResources.Memory.DeepCopy(),
				},
			})
		}
		if fastletResourceObservationFresh(info, now) && info.SchedulingUsed() == 0 && awaitingAdmission[identity] == 0 &&
			!info.Draining && !info.DrainRequested {
			view.removable = append(view.removable, resourceAwareDrainCandidate{identity: identity, free: free})
		}
	}
	return view
}

// additionalFastletsForDemands performs a deterministic best-fit-decreasing
// simulation. Aggregate free vectors are a lower bound, but only a concrete
// Fastlet bin can admit a Sandbox; this closes the fragmentation gap between
// Pool scaling and placement without turning the reconciler into an exact
// (NP-hard) bin-packing solver.
func additionalFastletsForDemands(
	pending []placement.ResourceDemand,
	bufferMin int32,
	defaultDemand placement.ResourceDemand,
	existing []resourceAwareFreeBin,
	fresh resourceAwareFreeBin,
) (int32, bool) {
	demands := make([]placement.ResourceDemand, 0, len(pending)+int(bufferMin))
	for _, demand := range pending {
		demands = append(demands, placement.ResourceDemand{CPU: demand.CPU.DeepCopy(), Memory: demand.Memory.DeepCopy()})
	}
	for range bufferMin {
		demands = append(demands, placement.ResourceDemand{CPU: defaultDemand.CPU.DeepCopy(), Memory: defaultDemand.Memory.DeepCopy()})
	}
	sort.SliceStable(demands, func(i, j int) bool {
		left := dominantDemandShare(demands[i], fresh.resources)
		right := dominantDemandShare(demands[j], fresh.resources)
		if left != right {
			return left > right
		}
		if demands[i].Memory.Cmp(demands[j].Memory) != 0 {
			return demands[i].Memory.Cmp(demands[j].Memory) > 0
		}
		return demands[i].CPU.Cmp(demands[j].CPU) > 0
	})

	bins := make([]resourceAwareFreeBin, 0, len(existing)+len(demands))
	for index := range existing {
		bins = append(bins, resourceAwareFreeBin{
			slots: existing[index].slots,
			resources: placement.ResourceDemand{
				CPU: existing[index].resources.CPU.DeepCopy(), Memory: existing[index].resources.Memory.DeepCopy(),
			},
		})
	}
	var added int32
	for _, demand := range demands {
		best := -1
		bestScore := 2.0
		for index := range bins {
			if !resourceDemandFits(demand, bins[index]) {
				continue
			}
			remaining := placement.ResourceDemand{CPU: bins[index].resources.CPU.DeepCopy(), Memory: bins[index].resources.Memory.DeepCopy()}
			remaining.CPU.Sub(demand.CPU)
			remaining.Memory.Sub(demand.Memory)
			score := dominantDemandShare(remaining, fresh.resources)
			if best < 0 || score < bestScore {
				best, bestScore = index, score
			}
		}
		if best < 0 {
			if !resourceDemandFits(demand, fresh) {
				return 0, false
			}
			bins = append(bins, resourceAwareFreeBin{
				slots: fresh.slots,
				resources: placement.ResourceDemand{
					CPU: fresh.resources.CPU.DeepCopy(), Memory: fresh.resources.Memory.DeepCopy(),
				},
			})
			best = len(bins) - 1
			added++
		}
		bins[best].slots--
		bins[best].resources.CPU.Sub(demand.CPU)
		bins[best].resources.Memory.Sub(demand.Memory)
	}
	return added, true
}

func resourceDemandFits(demand placement.ResourceDemand, bin resourceAwareFreeBin) bool {
	return bin.slots > 0 && demand.CPU.Sign() > 0 && demand.Memory.Sign() > 0 &&
		demand.CPU.Cmp(bin.resources.CPU) <= 0 && demand.Memory.Cmp(bin.resources.Memory) <= 0
}

func dominantDemandShare(demand, capacity placement.ResourceDemand) float64 {
	if capacity.CPU.MilliValue() <= 0 || capacity.Memory.Value() <= 0 {
		return 2
	}
	return max(
		float64(demand.CPU.MilliValue())/float64(capacity.CPU.MilliValue()),
		float64(demand.Memory.Value())/float64(capacity.Memory.Value()),
	)
}

func resourceFastletsForPoolChildren(
	registry placement.FastletRegistry,
	pool *apiv1alpha2.SandboxPool,
	pods []corev1.Pod,
) map[string]placement.FastletInfo {
	observed := make(map[string]placement.FastletInfo, len(pods))
	if registry == nil {
		return observed
	}
	children := childPodIdentities(pods)
	for _, info := range registry.GetAllFastlets() {
		if info.Namespace != pool.Namespace || info.PoolName != pool.Name {
			continue
		}
		identity := info.PodName + "/" + info.PodUID
		if _, exists := children[identity]; exists {
			observed[identity] = info
		}
	}
	return observed
}

func (r *SandboxPoolReconciler) podWithinStartupGrace(pod *corev1.Pod, now time.Time) bool {
	if pod == nil || pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded || pod.CreationTimestamp.IsZero() {
		return false
	}
	grace := r.FastletStartupGrace
	if grace <= 0 {
		grace = defaultFastletStartupGrace
	}
	age := now.Sub(pod.CreationTimestamp.Time)
	return age < 0 || age <= grace
}

func (r *SandboxPoolReconciler) stabilizeResourceAwareScaleDown(
	pool *apiv1alpha2.SandboxPool,
	current int32,
	desired int32,
) (int32, *metav1.Condition) {
	now := r.now()
	if desired >= current {
		return desired, &metav1.Condition{
			Type: apiv1alpha2.PoolConditionScaleDownCandidate, Status: metav1.ConditionFalse,
			Reason: apiv1alpha2.ReasonDemandReturned, Message: "Pool capacity is within the configured buffer",
			ObservedGeneration: pool.Generation, LastTransitionTime: metav1.NewTime(now),
		}
	}
	condition := metav1.Condition{
		Type: apiv1alpha2.PoolConditionScaleDownCandidate, Status: metav1.ConditionTrue,
		Reason: apiv1alpha2.ReasonExcessCapacity, Message: "Pool has excess capacity and is waiting for the scale-down stabilization window",
		ObservedGeneration: pool.Generation, LastTransitionTime: metav1.NewTime(now),
	}
	existing := apiMeta.FindStatusCondition(pool.Status.Conditions, apiv1alpha2.PoolConditionScaleDownCandidate)
	if existing == nil || existing.Status != metav1.ConditionTrue {
		return current, &condition
	}
	condition.LastTransitionTime = existing.LastTransitionTime
	window := r.ResourceScaleDownStabilization
	if window <= 0 {
		window = defaultResourceScaleDownStabilization
	}
	if now.Sub(existing.LastTransitionTime.Time) < window {
		return current, &condition
	}
	condition.Message = "Pool capacity has remained above the configured buffer for the stabilization window"
	return desired, &condition
}

func poolCapacityCondition(pool *apiv1alpha2.SandboxPool, requestedDesiredPods int32, capacityLimited bool) metav1.Condition {
	condition := metav1.Condition{
		Type:    apiv1alpha2.PoolConditionCapacityAvailable,
		Status:  metav1.ConditionTrue,
		Reason:  apiv1alpha2.ReasonCapacityAvailable,
		Message: "Pool replica limits can satisfy the current capacity target",
	}
	if capacityLimited && pool != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = apiv1alpha2.ReasonPoolMaxReached
		condition.Message = fmt.Sprintf(
			"Current demand requires %d Fastlet Pods, but poolMax limits the Pool to %d",
			requestedDesiredPods, pool.Spec.Capacity.PoolMax,
		)
	}
	return condition
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

func (r *SandboxPoolReconciler) reconcileDraining( //nolint:gocognit // pre-existing drain state machine; refactor tracked separately
	ctx context.Context,
	pool *apiv1alpha2.SandboxPool,
	pods []corev1.Pod,
	sandboxes []apiv1alpha2.Sandbox,
	desiredPods int32,
	desiredPodHash string,
	resourceAware bool,
	preferredScaleDown []string,
	observedFastlets map[string]placement.FastletInfo,
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
		count := min(target-len(draining), len(available))
		currentTemplateReady := currentTemplatePodsReady(pods, desiredPodHash, r.Registry)
		if hasCurrentTemplatePod(pods, desiredPodHash) && !currentTemplateReady {
			for _, pod := range available {
				if !fastletPodTemplateCurrent(pod, desiredPodHash) {
					return ctrl.Result{RequeueAfter: drainRequeue}, true, nil
				}
			}
		}
		selections := selectDrainTargets(available, count, preferredScaleDown, desiredPodHash, currentTemplateReady, resourceAware, active)
		for _, selection := range selections {
			if err := r.startDrain(ctx, selection.pod, selection.reason); err != nil {
				return ctrl.Result{RequeueAfter: drainRequeue}, true, err
			}
		}
		if len(selections) < count {
			return ctrl.Result{RequeueAfter: drainRequeue}, true, nil
		}
		return ctrl.Result{RequeueAfter: drainRequeue}, true, nil
	}

	if target == 0 {
		return ctrl.Result{}, false, nil
	}

	now := r.now()
	awaitingAdmission := awaitingAdmissionByFastlet(sandboxes, observedFastlets, now)
	timeout := r.DrainTimeout
	if timeout <= 0 {
		timeout = defaultDrainTimeout
	}
	for _, pod := range draining {
		acked, err := r.requestDrain(ctx, pod, true, pod.Annotations[placement.AnnotationDrainReason])
		if err != nil {
			// Re-fires every drain requeue until acked; the attempt counter
			// keeps the repeats attributable without suppressing them.
			attempt := 1
			if started := drainStartedAt(pod); !started.IsZero() && drainRequeue > 0 {
				attempt = int(now.Sub(started)/drainRequeue) + 1
			}
			if attempt <= 1 {
				klog.FromContext(ctx).Error(err, "Retry Fastlet drain request", "pod", pod.Name)
			} else {
				klog.FromContext(ctx).V(1).Error(err, "Retry Fastlet drain request still failing", "pod", pod.Name, "attempt", attempt)
			}
		}
		empty := active[podIdentity(pod)] == 0
		if resourceAware {
			identity := podIdentity(pod)
			info, exists := observedFastlets[identity]
			empty = exists && fastletResourceObservationFresh(info, now) && info.SchedulingUsed() == 0 &&
				awaitingAdmission[identity] == 0
		}
		timedOut := !drainStartedAt(pod).IsZero() && now.Sub(drainStartedAt(pod)) >= timeout
		previouslyAcked := pod.Annotations[placement.AnnotationDrainAckedAt] != ""
		if timedOut && !empty && pod.Annotations[placement.AnnotationDrainReason] == placement.DrainReasonScaleDown {
			if err := r.cancelDrain(ctx, pod); err != nil {
				return ctrl.Result{}, true, err
			}
			klog.FromContext(ctx).Info("cancelled loaded scale-down drain after timeout", "pod", pod.Name)
			continue
		}
		if (empty && (acked || previouslyAcked)) || timedOut {
			if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, true, err
			}
			klog.FromContext(ctx).Info("deleted drained Fastlet Pod", "pod", pod.Name, "empty", empty, "timedOut", timedOut)
		}
	}
	return ctrl.Result{RequeueAfter: drainRequeue}, true, nil
}

type drainTarget struct {
	pod    *corev1.Pod
	reason string
}

func selectDrainTargets(
	available []*corev1.Pod,
	count int,
	preferredScaleDown []string,
	desiredPodHash string,
	currentTemplateReady bool,
	resourceAware bool,
	active map[string]int,
) []drainTarget {
	if count <= 0 {
		return nil
	}
	selected := make(map[string]struct{}, count)
	result := make([]drainTarget, 0, count)
	appendTarget := func(pod *corev1.Pod, reason string) {
		identity := podIdentity(pod)
		if len(result) >= count {
			return
		}
		if _, exists := selected[identity]; exists {
			return
		}
		selected[identity] = struct{}{}
		result = append(result, drainTarget{pod: pod, reason: reason})
	}

	if currentTemplateReady {
		upgrade := make([]*corev1.Pod, 0, len(available))
		for _, pod := range available {
			if !fastletPodTemplateCurrent(pod, desiredPodHash) {
				upgrade = append(upgrade, pod)
			}
		}
		sort.Slice(upgrade, func(i, j int) bool {
			left, right := active[podIdentity(upgrade[i])], active[podIdentity(upgrade[j])]
			if left != right {
				return left < right
			}
			return upgrade[i].Name < upgrade[j].Name
		})
		for _, pod := range upgrade {
			appendTarget(pod, placement.DrainReasonPlannedUpgrade)
		}
	}

	availableByIdentity := make(map[string]*corev1.Pod, len(available))
	for _, pod := range available {
		availableByIdentity[podIdentity(pod)] = pod
	}
	if resourceAware {
		for _, identity := range preferredScaleDown {
			if pod := availableByIdentity[identity]; pod != nil {
				appendTarget(pod, placement.DrainReasonScaleDown)
			}
		}
		return result
	}

	remaining := append([]*corev1.Pod(nil), available...)
	sort.Slice(remaining, func(i, j int) bool {
		left, right := active[podIdentity(remaining[i])], active[podIdentity(remaining[j])]
		if left != right {
			return left < right
		}
		return remaining[i].Name < remaining[j].Name
	})
	for _, pod := range remaining {
		appendTarget(pod, placement.DrainReasonScaleDown)
	}
	return result
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
		return false, fmt.Errorf("fastlet Pod %s has no Pod IP", pod.Name)
	}
	response, err := r.FastletDrainer.SetDraining(ctx, pod.Status.PodIP, &fastletapi.SetDrainingRequest{Draining: draining, Reason: reason})
	if err != nil {
		return false, err
	}
	if response == nil || response.Draining != draining {
		return false, fmt.Errorf("fastlet Pod %s returned an inconsistent drain state", pod.Name)
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

func awaitingAdmissionByFastlet(
	sandboxes []apiv1alpha2.Sandbox,
	observedFastlets map[string]placement.FastletInfo,
	now time.Time,
) map[string]int {
	result := make(map[string]int)
	for index := range sandboxes {
		sandbox := &sandboxes[index]
		if sandbox.DeletionTimestamp != nil || sandbox.Status.Placement.FastletName == "" || sandbox.Status.Placement.FastletPodUID == "" {
			continue
		}
		identity := sandbox.Status.Placement.FastletName + "/" + string(sandbox.Status.Placement.FastletPodUID)
		if !fastletReportsCurrentReservation(sandbox, observedFastlets, now) {
			result[identity]++
		}
	}
	return result
}

func fastletResourceObservationFresh(info placement.FastletInfo, now time.Time) bool {
	return !info.LastHeartbeat.IsZero() && !info.ResourceReceivedAt.IsZero() &&
		now.Sub(info.LastHeartbeat) <= placement.DefaultObservationStaleAfter &&
		now.Sub(info.ResourceReceivedAt) <= placement.DefaultObservationStaleAfter
}

func sandboxNeedsPlacement(sandbox *apiv1alpha2.Sandbox) bool {
	if sandbox == nil || sandbox.Status.Placement.FastletName != "" || sandbox.DeletionTimestamp != nil {
		return false
	}
	if sandbox.Status.Runtime.State == apiv1alpha2.RuntimeStopping ||
		sandbox.Status.Runtime.State == apiv1alpha2.RuntimeStopped ||
		sandbox.Status.Runtime.State == apiv1alpha2.RuntimePausing ||
		sandbox.Status.Runtime.State == apiv1alpha2.RuntimePaused ||
		sandbox.Status.Runtime.State == apiv1alpha2.RuntimeResuming {
		return false
	}
	return true
}

func sandboxNeedsResourceCapacity(
	sandbox *apiv1alpha2.Sandbox,
	observedFastlets map[string]placement.FastletInfo,
	now time.Time,
) bool {
	if sandbox == nil || sandbox.DeletionTimestamp != nil {
		return false
	}
	switch sandbox.Status.Runtime.State {
	case apiv1alpha2.RuntimeReady,
		apiv1alpha2.RuntimeCreating,
		apiv1alpha2.RuntimeFailed,
		apiv1alpha2.RuntimeStopping,
		apiv1alpha2.RuntimeStopped,
		apiv1alpha2.RuntimePausing,
		apiv1alpha2.RuntimePaused:
		return false
	case apiv1alpha2.RuntimeResuming:
		return !fastletReportsCurrentReservation(sandbox, observedFastlets, now)
	default:
		return true
	}
}

// fastletReportsCurrentReservation distinguishes a resume that is still
// waiting for admission from one whose commitment is already included in the
// assigned Fastlet's Admission.Used ledger. Every identity fence must match so
// a stale observation from an earlier runtime or assignment cannot suppress
// real pending demand.
func fastletReportsCurrentReservation(
	sandbox *apiv1alpha2.Sandbox,
	observedFastlets map[string]placement.FastletInfo,
	now time.Time,
) bool {
	if sandbox == nil || sandbox.UID == "" || sandbox.Status.Placement.FastletName == "" ||
		sandbox.Status.Placement.FastletPodUID == "" || sandbox.Status.Placement.Attempt <= 0 ||
		sandbox.Status.Runtime.Generation <= 0 || sandbox.Generation <= 0 {
		return false
	}
	identity := sandbox.Status.Placement.FastletName + "/" + string(sandbox.Status.Placement.FastletPodUID)
	fastlet, exists := observedFastlets[identity]
	if !exists || fastlet.PodName != sandbox.Status.Placement.FastletName ||
		fastlet.PodUID != string(sandbox.Status.Placement.FastletPodUID) ||
		fastlet.LastHeartbeat.IsZero() || fastlet.ResourceReceivedAt.IsZero() ||
		now.Sub(fastlet.LastHeartbeat) > placement.DefaultObservationStaleAfter ||
		now.Sub(fastlet.ResourceReceivedAt) > placement.DefaultObservationStaleAfter {
		return false
	}
	status, exists := fastlet.SandboxStatuses[string(sandbox.UID)]
	return exists &&
		status.InstanceGeneration == sandbox.Status.Runtime.Generation &&
		status.AssignmentAttempt == sandbox.Status.Placement.Attempt
}

func poolSlotDemandCounts(
	sandboxes []apiv1alpha2.Sandbox,
	poolName string,
	childIdentities map[string]struct{},
) (active int32, pending int32) {
	for index := range sandboxes {
		sandbox := &sandboxes[index]
		if sandbox.Spec.PoolRef != poolName || sandbox.DeletionTimestamp != nil {
			continue
		}
		if sandbox.Status.Placement.FastletName != "" {
			identity := sandbox.Status.Placement.FastletName + "/" + string(sandbox.Status.Placement.FastletPodUID)
			if _, exists := childIdentities[identity]; exists {
				active++
			}
		}
		if sandboxNeedsPlacement(sandbox) {
			pending++
		}
	}
	return active, pending
}

func resolvePoolPendingResources(
	pool *apiv1alpha2.SandboxPool,
	sandboxes []apiv1alpha2.Sandbox,
	observedFastlets map[string]placement.FastletInfo,
	now time.Time,
) (poolPendingResourceResolution, error) {
	result := poolPendingResourceResolution{admissible: make([]placement.ResourceDemand, 0)}
	for index := range sandboxes {
		sandbox := &sandboxes[index]
		if sandbox.Spec.PoolRef != pool.Name || !sandboxNeedsResourceCapacity(sandbox, observedFastlets, now) {
			continue
		}
		_, demand, err := orchestration.ResolveSandboxResources(pool, sandbox)
		if err != nil {
			var resolutionErr *orchestration.SandboxResourceResolutionError
			if errors.As(err, &resolutionErr) {
				result.unadmittable = append(result.unadmittable, unadmittableSandboxResource{
					key:    types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Name},
					reason: resolutionErr.Reason, message: resolutionErr.Error(),
				})
				continue
			}
			return poolPendingResourceResolution{}, fmt.Errorf("resolve pending Sandbox %s resources: %w", sandbox.Name, err)
		}
		result.admissible = append(result.admissible, demand)
	}
	return result, nil
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
func (r *SandboxPoolReconciler) constructPod(pool *apiv1alpha2.SandboxPool, profile runtimecatalog.RuntimeProfile) (*corev1.Pod, error) { //nolint:unused // used by reconciler tests
	plan, err := runtimeenv.ResolveDefault(r.Catalog, profile.Name)
	if err != nil {
		return nil, err
	}
	return r.constructPodWithRuntimePlan(pool, plan)
}

func (r *SandboxPoolReconciler) constructPodWithRuntimePlan(pool *apiv1alpha2.SandboxPool, runtimePlan runtimeenv.ResolvedRuntimePlan) (*corev1.Pod, error) { //nolint:unused // used by reconciler tests
	if err := pool.Spec.ValidateActionHandlers(); err != nil {
		return nil, err
	}
	profile := runtimePlan.Profile
	sandboxResources := pool.Spec.SandboxResources
	if err := apiv1alpha2.ValidateSandboxResourceProfile(sandboxResources); err != nil {
		return nil, err
	}
	resolvedResources, err := compileFastletResources(pool, profile.Deployment.Overhead)
	if err != nil {
		return nil, err
	}
	r.applyResourceAwareFeatureGates(&resolvedResources)
	settings, err := buildFastletSettingsFromResolved(pool, resolvedResources)
	if err != nil {
		return nil, err
	}
	return r.constructPodWithResolvedResources(pool, runtimePlan, resolvedResources, settings)
}

//nolint:gocognit,maintidx // Pod assembly covers all runtime variants.
func (r *SandboxPoolReconciler) constructPodWithResolvedResources(
	pool *apiv1alpha2.SandboxPool,
	runtimePlan runtimeenv.ResolvedRuntimePlan,
	resolvedResources resolvedFastletResources,
	settings fastletsettings.Bundle,
) (*corev1.Pod, error) {
	profile := runtimePlan.Profile
	sandboxResources := pool.Spec.SandboxResources
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
	if resolvedResources.Mode == apiv1alpha2.SandboxResourceModeResourceAware {
		capacity, encodeErr := fastletsettings.EncodeCommitmentCapacity(resolvedResources.CommitmentCapacity)
		if encodeErr != nil {
			return nil, encodeErr
		}
		annotations[fastletsettings.CommitmentCapacityAnnotation] = capacity
	}
	settingsRevision, err := settings.Revision()
	if err != nil {
		return nil, fmt.Errorf("compute Fastlet settings revision: %w", err)
	}
	annotations["fast-sandbox.io/settings-revision"] = settingsRevision

	podSpec := pool.Spec.FastletTemplate.Spec.DeepCopy()
	podSpec.HostNetwork = false
	podSpec.HostPID = false
	podSpec.RuntimeClassName = nil
	podSpec.AutomountServiceAccountToken = boolPtr(false)
	if len(podSpec.Containers) == 0 {
		return nil, errors.New("fastletTemplate.spec.containers must contain the fastlet container")
	}
	for _, container := range podSpec.Containers[1:] {
		if container.Name == fastletProxyContainerName {
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
	if len(podSpec.Containers) > 0 {
		c := &podSpec.Containers[0]
		if c.SecurityContext == nil {
			c.SecurityContext = &corev1.SecurityContext{}
		}
		c.SecurityContext.Privileged = boolPtr(profile.Deployment.Privileged)
		c.Env = removeRuntimeOwnedEnv(c.Env)

		c.Env = append(c.Env,
			corev1.EnvVar{Name: "FASTLET_CONTROL_PORT", Value: ":5758"},
			corev1.EnvVar{Name: envFastletProxyControlSocket, Value: "/run/fast-sandbox/proxy/control.sock"},
			corev1.EnvVar{Name: "FAST_SANDBOX_SETTINGS_DIR", Value: fastletsettings.MountPath},
			corev1.EnvVar{
				Name:      "NODE_NAME",
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}},
			},
			corev1.EnvVar{
				Name:      envPodName,
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
			},
			corev1.EnvVar{
				Name:      "POD_IP",
				ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}},
			},
			corev1.EnvVar{
				Name:      envPodUID,
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
				Value: strconv.Itoa(int(getFastletCapacity(pool))),
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
			corev1.EnvVar{Name: "FAST_SANDBOX_INFRA_REVISION", Value: infraPlan.Revision},
			corev1.EnvVar{Name: "FAST_SANDBOX_INFRA_PLAN_PATH", Value: "/etc/fast-sandbox/infra/plan.json"},
			corev1.EnvVar{Name: envFastSandboxRegistryConfigPath, Value: registryconfig.MountPath},
			corev1.EnvVar{Name: "INFRA_DIR_IN_POD", Value: infraHostPath},
		)
		if profile.ResidualProcess != runtimecatalog.ResidualProcessNone {
			c.Env = append(c.Env, corev1.EnvVar{Name: "FAST_SANDBOX_NODE_CLEANUP_SOCKET", Value: nodecleanup.DefaultSocketPath})
		}
		c.VolumeMounts = append(c.VolumeMounts,
			corev1.VolumeMount{Name: tmpVolumeName, MountPath: tmpDir},
			corev1.VolumeMount{Name: infraToolsVolumeName, MountPath: infraHostPath},
			corev1.VolumeMount{Name: infraPlanVolumeName, MountPath: "/etc/fast-sandbox/infra", ReadOnly: true},
			corev1.VolumeMount{Name: runtimePlanVolumeName, MountPath: runtimeenv.PlanMountPath, ReadOnly: true},
			corev1.VolumeMount{Name: registryConfigVolumeName, MountPath: registryConfigDir, ReadOnly: true},
			corev1.VolumeMount{Name: fastletsettings.VolumeName, MountPath: fastletsettings.MountPath, ReadOnly: true},
			corev1.VolumeMount{Name: proxyControlVolumeName, MountPath: proxyRunDir},
		)
		if profile.Driver == runtimecatalog.DriverKindContainerd {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
				Name: podcgroup.VolumeName, MountPath: podcgroup.HostRoot,
			})
		}
		if profile.ResidualProcess != runtimecatalog.ResidualProcessNone {
			c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{Name: nodeCleanupVolumeName, MountPath: filepath.Dir(nodecleanup.DefaultSocketPath)})
		}
		if runtimeResourceOwner == c.Name {
			if resolvedResources.Mode == apiv1alpha2.SandboxResourceModeResourceAware {
				applyResolvedRuntimeOwnerResources(c, resolvedResources)
			} else if err := applyFastletResources(c, profile.Deployment.Overhead, sandboxResources, getFastletCapacity(pool)); err != nil {
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
		Name: fastletProxyContainerName, Image: proxyImage, ImagePullPolicy: corev1.PullIfNotPresent,
		Env: []corev1.EnvVar{
			{Name: envFastletProxyControlSocket, Value: "/run/fast-sandbox/proxy/control.sock"},
			{Name: "FASTLET_PROXY_DATA_ADDRESS", Value: ":5780"},
			{Name: "FASTLET_PROXY_METRICS_ADDRESS", Value: ":9093"},
			{Name: "FAST_SANDBOX_ROUTE_VERIFY_PUBLIC_KEY", Value: r.RouteVerifyPublicKey},
		},
		Ports: []corev1.ContainerPort{
			{Name: "data-proxy", ContainerPort: 5780, Protocol: corev1.ProtocolTCP},
			{Name: "proxy-metrics", ContainerPort: 9093, Protocol: corev1.ProtocolTCP},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: proxyControlVolumeName, MountPath: proxyRunDir}},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/readyz", Port: intstr.FromInt32(5780), Scheme: corev1.URISchemeHTTP,
			}},
			InitialDelaySeconds: 0, PeriodSeconds: 2, TimeoutSeconds: 1, FailureThreshold: 1,
		},
	})
	if err := ensureBoundedPodContainers(podSpec, runtimeResourceOwner); err != nil {
		return nil, err
	}

	hostPathDirectory := corev1.HostPathDirectory

	podSpec.Volumes = append(podSpec.Volumes,
		corev1.Volume{
			Name:         tmpVolumeName,
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: tmpDir, Type: &hostPathDirectory}},
		},
		corev1.Volume{
			Name: infraToolsVolumeName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		},
		corev1.Volume{
			Name: infraPlanVolumeName,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: infraPlanConfigMapName(pool.Name, infraPlan.Revision)},
			}},
		},
		corev1.Volume{
			Name: runtimePlanVolumeName,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: runtimePlanConfigMapName(pool.Name, runtimePlan.Revision)},
			}},
		},
		corev1.Volume{
			Name: registryConfigVolumeName,
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: registrySecretName(pool.Name),
			}},
		},
		corev1.Volume{
			Name: fastletsettings.VolumeName,
			VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: settingsConfigMapName(pool.Name, settingsRevision)},
			}},
		},
		corev1.Volume{Name: proxyControlVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
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
			Name: nodeCleanupVolumeName,
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
				Path: filepath.Dir(nodecleanup.DefaultSocketPath), Type: &hostPathDirectoryOrCreate,
			}},
		})
	}
	runtimeContainer := &podSpec.Containers[0]
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
	pod.Annotations[placement.AnnotationPodTemplateHash] = hex.EncodeToString(digest[:])
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

func validatePlatformOwnedStorage(podSpec *corev1.PodSpec) error {
	reservedVolumes := map[string]string{
		tmpVolumeName:              tmpDir,
		infraToolsVolumeName:       infraHostPath,
		infraPlanVolumeName:        "/etc/fast-sandbox/infra",
		runtimePlanVolumeName:      runtimeenv.PlanMountPath,
		registryConfigVolumeName:   registryConfigDir,
		fastletsettings.VolumeName: fastletsettings.MountPath,
		proxyControlVolumeName:     proxyRunDir,
		nodeCleanupVolumeName:      filepath.Dir(nodecleanup.DefaultSocketPath),
		podcgroup.VolumeName:       podcgroup.HostRoot,
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
		poolLabelKey: poolName,
		"app":        "sandbox-fastlet",
	}
}

func getFastletCapacity(pool *apiv1alpha2.SandboxPool) int32 {
	return pool.Spec.MaxSandboxesPerPod
}

func (r *SandboxPoolReconciler) resolveRuntimeProfile(pool *apiv1alpha2.SandboxPool) (runtimecatalog.RuntimeProfile, error) { //nolint:unused // used by reconciler tests
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
		return runtimeenv.Config{}, fmt.Errorf("configMap %s/%s must contain %s", namespace, name, runtimeenv.ConfigMapKey)
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

func settingsConfigMapName(poolName, revision string) string {
	suffix := "-settings-" + shortRevision(revision)
	maxPoolLength := 253 - len(suffix)
	if len(poolName) > maxPoolLength {
		poolName = strings.TrimRight(poolName[:maxPoolLength], "-.")
	}
	return poolName + suffix
}

// buildFastletSettings projects the pool-declared declarative configuration
// and, for ResourceAware Pools, the Controller-resolved resource contract.
func buildFastletSettings(pool *apiv1alpha2.SandboxPool, profile runtimecatalog.RuntimeProfile) (fastletsettings.Bundle, error) { //nolint:unused // used by reconciler tests
	resolved, err := compileFastletResources(pool, profile.Deployment.Overhead)
	if err != nil {
		return fastletsettings.Bundle{}, err
	}
	return buildFastletSettingsFromResolved(pool, resolved)
}

func buildFastletSettingsFromResolved(pool *apiv1alpha2.SandboxPool, resolved resolvedFastletResources) (fastletsettings.Bundle, error) {
	bundle := fastletsettings.Bundle{
		WarmImages:      uniqueWarmImages(pool.Spec.WarmImages),
		ActionHandlers:  pool.Spec.ActionHandlers,
		ResourceProfile: pool.Spec.SandboxResources,
	}
	if resolved.Mode != apiv1alpha2.SandboxResourceModeResourceAware {
		return bundle, nil
	}
	podMemoryBudget := resolved.RuntimeEnvelope.Memory.DeepCopy()
	for index := 1; index < len(pool.Spec.FastletTemplate.Spec.Containers); index++ {
		if limit, found := pool.Spec.FastletTemplate.Spec.Containers[index].Resources.Limits[corev1.ResourceMemory]; found && limit.Sign() > 0 {
			podMemoryBudget.Add(limit)
		}
	}
	for index := range pool.Spec.FastletTemplate.Spec.InitContainers {
		container := &pool.Spec.FastletTemplate.Spec.InitContainers[index]
		if container.RestartPolicy == nil || *container.RestartPolicy != corev1.ContainerRestartPolicyAlways {
			continue
		}
		if limit, found := container.Resources.Limits[corev1.ResourceMemory]; found && limit.Sign() > 0 {
			podMemoryBudget.Add(limit)
		}
	}
	// The platform-owned proxy is always injected and runs in the same Pod
	// cgroup. Its default memory limit must therefore be part of that scope's
	// observer budget, while remaining outside Sandbox commitment capacity.
	podMemoryBudget.Add(defaultPlatformResourceLimit(corev1.ResourceMemory))
	bundle.ResourceAdmission = &fastletsettings.ResourceAdmissionSettings{
		Version: "v2", Mode: apiv1alpha2.SandboxResourceModeResourceAware,
		StaticAdmission: resolved.StaticAdmission, MemoryAdmission: resolved.MemoryAdmission,
		Capacity: apiv1alpha2.SandboxComputeResources{
			CPU: resolved.CommitmentCapacity.CPU.DeepCopy(), Memory: resolved.CommitmentCapacity.Memory.DeepCopy(),
		},
		SandboxRuntimeBudget: apiv1alpha2.SandboxComputeResources{
			CPU: resolved.SandboxRuntimeBudget.CPU.DeepCopy(), Memory: resolved.SandboxRuntimeBudget.Memory.DeepCopy(),
		},
		RuntimeEnvelope: apiv1alpha2.SandboxComputeResources{
			CPU: resolved.RuntimeEnvelope.CPU.DeepCopy(), Memory: resolved.RuntimeEnvelope.Memory.DeepCopy(),
		},
		PodMemoryBudget:     podMemoryBudget,
		ResourceProfileHash: pool.Spec.SandboxResources.Hash(),
	}
	return bundle, nil
}

func (r *SandboxPoolReconciler) applyResourceAwareFeatureGates(resolved *resolvedFastletResources) {
	if resolved == nil || resolved.Mode != apiv1alpha2.SandboxResourceModeResourceAware {
		return
	}
	resolved.StaticAdmission = !r.DisableResourceAwareStaticAdmission
	resolved.MemoryAdmission = !r.DisableResourceAwareMemoryAdmission
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
	compiled, err := compileRegistryConfig(ctx, r.Client, pool.Namespace)
	if err != nil {
		return registryconfig.Compiled{}, err
	}
	return compiled, r.persistRegistrySecret(ctx, pool, compiled)
}

// Keys of the Opaque write (publish) secret; they mirror the SandboxTemplate
// PublishSecretRef convention. Consumed by registryWriteCredential in
// registrycompile.go.
const (
	writeSecretAccessKeyID = "accessKeyId"
	writeSecretAccessKey   = "secretAccessKey"
)

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
				Labels: map[string]string{poolLabelKey: pool.Name, "fast-sandbox.io/registry-config": "compiled"},
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
				poolLabelKey:                     pool.Name,
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
				poolLabelKey:                       pool.Name,
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

// ensureFastletSettingsConfigMap materializes the pool-declared Fastlet
// settings (warmImages, actionHandlers, sandboxResources) as an immutable,
// content-addressed ConfigMap mounted into every Fastlet Pod.
func (r *SandboxPoolReconciler) ensureFastletSettingsConfigMap(
	ctx context.Context,
	pool *apiv1alpha2.SandboxPool,
	bundle fastletsettings.Bundle,
) error {
	files, err := bundle.Files()
	if err != nil {
		return fmt.Errorf("encode Fastlet settings: %w", err)
	}
	revision, err := bundle.Revision()
	if err != nil {
		return fmt.Errorf("compute Fastlet settings revision: %w", err)
	}
	key := client.ObjectKey{Namespace: pool.Namespace, Name: settingsConfigMapName(pool.Name, revision)}
	var existing corev1.ConfigMap
	err = r.Get(ctx, key, &existing)
	if err == nil {
		if len(existing.Data) != len(files) {
			return fmt.Errorf("immutable Fastlet settings ConfigMap %s contains different settings", key.Name)
		}
		for name, value := range files {
			if existing.Data[name] != value {
				return fmt.Errorf("immutable Fastlet settings ConfigMap %s contains different settings", key.Name)
			}
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
				poolLabelKey:                        pool.Name,
				"fast-sandbox.io/settings-revision": shortRevision(revision),
			},
			Annotations: map[string]string{"fast-sandbox.io/settings-revision": revision},
		},
		Immutable: &immutable,
		Data:      files,
	}
	if err := ctrl.SetControllerReference(pool, configMap, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, configMap); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create immutable Fastlet settings ConfigMap: %w", err)
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
	envFastSandboxRegistryConfigPath: {},
	"RUNTIME_SOCKET":                 {}, "INFRA_DIR_IN_POD": {},
	"FASTLET_CONTROL_PORT":         {},
	envFastletProxyControlSocket:   {},
	"FAST_SANDBOX_WARM_IMAGES":     {},
	"FAST_SANDBOX_ACTION_HANDLERS": {},
	"FAST_SANDBOX_ACTIONS":         {},
	"FAST_SANDBOX_SETTINGS_DIR":    {},
	"NODE_NAME":                    {}, envPodName: {}, "POD_IP": {}, envPodUID: {}, "NAMESPACE": {},
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

func applyFastletResources(container *corev1.Container, overhead corev1.ResourceList, sandbox apiv1alpha2.SandboxResourceProfile, capacity int32) error { //nolint:gocognit // pre-existing resource-merge logic; refactor tracked separately
	if sandbox.ResourceMode() != apiv1alpha2.SandboxResourceModeLegacySlot {
		return fmt.Errorf("applyFastletResources only accepts LegacySlot profiles")
	}
	limits, ok := sandbox.LimitResources()
	if !ok {
		return apiv1alpha2.ErrInvalidSandboxResourceProfile
	}
	requests := limits
	defaultAggregate := overhead.DeepCopy()
	if defaultAggregate == nil {
		defaultAggregate = corev1.ResourceList{}
	}
	addScaledQuantity(defaultAggregate, corev1.ResourceCPU, requests.CPU, int64(capacity))
	addScaledQuantity(defaultAggregate, corev1.ResourceMemory, requests.Memory, int64(capacity))
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
		if index == 0 || container.Name == fastletProxyContainerName {
			applyDefaultPlatformResources(container)
			continue
		}
		if err := validateContainerHasLimits(container); err != nil {
			return fmt.Errorf("fastlet sidecar resources: %w", err)
		}
	}
	for index := range podSpec.InitContainers {
		if err := validateContainerHasLimits(&podSpec.InitContainers[index]); err != nil {
			return fmt.Errorf("fastlet init container resources: %w", err)
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
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		defaultRequest := defaultPlatformResourceRequest(name)
		defaultLimit := defaultPlatformResourceLimit(name)
		request, hasRequest := container.Resources.Requests[name]
		if !hasRequest || request.IsZero() {
			request = defaultRequest
			container.Resources.Requests[name] = request
		}
		limit, hasLimit := container.Resources.Limits[name]
		if !hasLimit || limit.IsZero() {
			limit = defaultLimit
			if request.Cmp(limit) > 0 {
				limit = request.DeepCopy()
			}
			container.Resources.Limits[name] = limit
		}
	}
}

func defaultPlatformResourceRequest(name corev1.ResourceName) resource.Quantity {
	if name == corev1.ResourceCPU {
		return resource.MustParse("50m")
	}
	return resource.MustParse("64Mi")
}

func defaultPlatformResourceLimit(name corev1.ResourceName) resource.Quantity {
	if name == corev1.ResourceCPU {
		return resource.MustParse("250m")
	}
	return resource.MustParse("256Mi")
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

// updatePoolCondition stamps ObservedGeneration and persists the condition;
// an unchanged condition is a no-op, avoiding a pointless status write per
// reconcile. Conflicts are retried against the latest object, see
// updatePoolStatus.
func (r *SandboxPoolReconciler) updatePoolCondition(ctx context.Context, pool *apiv1alpha2.SandboxPool, condition metav1.Condition) error {
	return r.updatePoolStatus(ctx, pool, func(current *apiv1alpha2.SandboxPool) bool {
		condition.ObservedGeneration = current.Generation
		existing := apiMeta.FindStatusCondition(current.Status.Conditions, condition.Type)
		if existing != nil && existing.Status == condition.Status && existing.Reason == condition.Reason &&
			existing.Message == condition.Message && existing.ObservedGeneration == condition.ObservedGeneration {
			return false
		}
		apiMeta.SetStatusCondition(&current.Status.Conditions, condition)
		return true
	})
}

// poolObservation carries the per-reconcile counter/revision projection into
// SandboxPool.status.
type poolObservation struct {
	currentPods        int32
	readyPods          int32
	idleFastlets       int32
	busyFastlets       int32
	runtimeRevision    string
	infraRevision      string
	fastletRevision    string
	preparedFastlets   int32
	scaleDownCandidate *metav1.Condition
	warmImages         []apiv1alpha2.WarmImageStatus
	fastlets           *apiv1alpha2.SandboxPoolFastletSummary
	resources          *apiv1alpha2.SandboxPoolResourceSummary
	sandboxes          *apiv1alpha2.SandboxPoolSandboxSummary
	observedAt         time.Time
}

func (o poolObservation) apply(pool *apiv1alpha2.SandboxPool) bool {
	status := &pool.Status
	summaryDue := status.SummaryObservedAt == nil || o.observedAt.Sub(status.SummaryObservedAt.Time) >= time.Minute
	summaryMatches := !summaryDue || reflect.DeepEqual(status.Fastlets, o.fastlets) && reflect.DeepEqual(status.Resources, o.resources) && reflect.DeepEqual(status.Sandboxes, o.sandboxes)
	// omitempty turns empty WarmImages into nil on API reads; treat both as equal
	// to avoid repeated status writes and watch-triggered reconciles.
	if status.CurrentPods == o.currentPods &&
		status.ReadyPods == o.readyPods && status.IdleFastlets == o.idleFastlets &&
		status.BusyFastlets == o.busyFastlets &&
		status.RuntimeRevision == o.runtimeRevision && status.InfraRevision == o.infraRevision &&
		status.FastletRevision == o.fastletRevision && status.PreparedFastlets == o.preparedFastlets &&
		slices.Equal(status.WarmImages, o.warmImages) && conditionMatches(status.Conditions, o.scaleDownCandidate) && summaryMatches {
		return false
	}
	status.CurrentPods = o.currentPods
	status.ReadyPods = o.readyPods
	status.IdleFastlets = o.idleFastlets
	status.BusyFastlets = o.busyFastlets
	status.RuntimeRevision = o.runtimeRevision
	status.InfraRevision = o.infraRevision
	status.FastletRevision = o.fastletRevision
	status.PreparedFastlets = o.preparedFastlets
	status.WarmImages = o.warmImages
	if o.scaleDownCandidate != nil {
		apiMeta.SetStatusCondition(&status.Conditions, *o.scaleDownCandidate)
	}
	if summaryDue {
		status.Fastlets, status.Resources, status.Sandboxes = o.fastlets, o.resources, o.sandboxes
		observedAt := metav1.NewTime(o.observedAt)
		status.SummaryObservedAt = &observedAt
	}
	return true
}

func summarizePoolFastlets(
	pool *apiv1alpha2.SandboxPool,
	pods []corev1.Pod,
	desired, ready, idle, busy int32,
	observed map[string]placement.FastletInfo,
	blockedNodes map[string]struct{},
	now time.Time,
) (*apiv1alpha2.SandboxPoolFastletSummary, *apiv1alpha2.SandboxPoolResourceSummary) {
	fastlets := &apiv1alpha2.SandboxPoolFastletSummary{Desired: desired, Current: int32(len(pods)), Ready: ready, Idle: idle, Busy: busy}
	resources := &apiv1alpha2.SandboxPoolResourceSummary{
		Capacity: zeroComputeResources(), Committed: zeroComputeResources(), Free: zeroComputeResources(),
	}
	if pool.Spec.SandboxResources.ResourceMode() != apiv1alpha2.SandboxResourceModeResourceAware {
		return fastlets, nil
	}
	for index := range pods {
		pod := &pods[index]
		if placement.PodDrainRequested(pod) {
			fastlets.Draining++
		}
		capacity, err := fastletsettings.DecodeCommitmentCapacity(pod.Annotations[fastletsettings.CommitmentCapacityAnnotation])
		if err == nil {
			addComputeResources(&resources.Capacity, capacity)
		}
		info, ok := observed[podIdentity(pod)]
		if !ok || info.Admission.Resources == nil || info.Admission.Resources.Version != "v2" {
			continue
		}
		addDemand(&resources.Committed, info.ResourceCommitted)
		if _, blocked := blockedNodes[info.NodeName]; blocked {
			continue
		}
		view := placement.ResourceSchedulability(info, now, placement.DefaultObservationStaleAfter)
		addDemand(&resources.Free, view.FreeResources)
	}
	return fastlets, resources
}

func summarizePoolSandboxes(sandboxes []apiv1alpha2.Sandbox, poolName string) *apiv1alpha2.SandboxPoolSandboxSummary {
	summary := &apiv1alpha2.SandboxPoolSandboxSummary{}
	for index := range sandboxes {
		sandbox := &sandboxes[index]
		if sandbox.Spec.PoolRef != poolName {
			continue
		}
		summary.Total++
		if sandbox.Status.Placement.FastletName != "" {
			summary.Assigned++
		}
		if sandbox.DeletionTimestamp != nil || sandbox.Status.Runtime.State == apiv1alpha2.RuntimeStopping {
			summary.Terminating++
			continue
		}
		switch sandbox.Status.Runtime.State {
		case apiv1alpha2.RuntimeReady:
			summary.Running++
		case apiv1alpha2.RuntimePaused, apiv1alpha2.RuntimePausing:
			summary.Paused++
		case apiv1alpha2.RuntimeFailed, apiv1alpha2.RuntimeUnavailable:
			summary.Failed++
		default:
			summary.Pending++
		}
	}
	return summary
}

func zeroComputeResources() apiv1alpha2.SandboxComputeResources {
	return apiv1alpha2.SandboxComputeResources{CPU: resource.MustParse("0"), Memory: resource.MustParse("0")}
}

func addComputeResources(target *apiv1alpha2.SandboxComputeResources, value apiv1alpha2.SandboxComputeResources) {
	target.CPU.Add(value.CPU)
	target.Memory.Add(value.Memory)
}

func addDemand(target *apiv1alpha2.SandboxComputeResources, value placement.ResourceDemand) {
	target.CPU.Add(value.CPU)
	target.Memory.Add(value.Memory)
}

func conditionMatches(conditions []metav1.Condition, expected *metav1.Condition) bool {
	if expected == nil {
		return true
	}
	current := apiMeta.FindStatusCondition(conditions, expected.Type)
	return current != nil && current.Status == expected.Status && current.Reason == expected.Reason &&
		current.Message == expected.Message && current.ObservedGeneration == expected.ObservedGeneration
}

// updatePoolStatus persists a SandboxPool status mutation conflict-safely.
// Concurrent writers (for example the OpenSandbox server pool service in
// all-in-one deployments) and the controller's own informer-cache lag used to
// surface raw Conflict errors dozens of times per minute and flood the log;
// see issue #88. apply mutates current in place and reports whether anything
// changed, so re-applying on conflict stays idempotent and an already-applied
// change stays a no-op. On conflict the latest object is re-read through the
// durable (API-server) reader, never the informer cache: a stale cached
// resourceVersion would turn every retry into another conflict and keep the
// storm alive. On success the caller's pool adopts the persisted
// resourceVersion and status so later writes within the same reconcile do not
// race their own predecessor.
func (r *SandboxPoolReconciler) updatePoolStatus(ctx context.Context, pool *apiv1alpha2.SandboxPool, apply func(*apiv1alpha2.SandboxPool) bool) error {
	key := client.ObjectKeyFromObject(pool)
	var persisted *apiv1alpha2.SandboxPool
	firstAttempt := true
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := pool.DeepCopy()
		if !firstAttempt {
			var latest apiv1alpha2.SandboxPool
			if err := r.durableReader().Get(ctx, key, &latest); err != nil {
				return err
			}
			current = &latest
		}
		firstAttempt = false
		if !apply(current) {
			persisted = current
			return nil
		}
		if err := r.Status().Update(ctx, current); err != nil {
			return err
		}
		persisted = current
		return nil
	})
	if err != nil || persisted == nil {
		return err
	}
	pool.ResourceVersion = persisted.ResourceVersion
	pool.Generation = persisted.Generation
	pool.Status = persisted.Status
	return nil
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
		klog.FromContext(ctx).Error(err, "Failed to list Pools for ConfigMap event; pool reconciles are skipped")
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
		klog.FromContext(ctx).Error(err, "Failed to list Pools for namespace event; pool reconciles are skipped", "poolNamespace", namespace)
		return nil
	}
	result := make([]ctrl.Request, 0, len(pools.Items))
	for index := range pools.Items {
		result = append(result, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&pools.Items[index])})
	}
	return result
}
