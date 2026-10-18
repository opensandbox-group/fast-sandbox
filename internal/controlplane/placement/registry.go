package placement

import (
	"errors"
	"hash/fnv"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	fastletcache "fast-sandbox/internal/fastlet/cache"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

type FastletID string

const DefaultObservationStaleAfter = 45 * time.Second

type ResourceDemand struct {
	CPU    resource.Quantity
	Memory resource.Quantity
}

type resourceRevision struct {
	Epoch    string
	Revision uint64
}

var (
	ErrFastletNotFound  = errors.New("Fastlet is not present in the Pod watch store")
	ErrStalePodIdentity = errors.New("Fastlet Pod UID does not match the watched Pod")
	ErrStaleHeartbeat   = errors.New("Fastlet heartbeat sequence is stale")
	ErrProfileMismatch  = errors.New("Fastlet heartbeat profile does not match the watched Pod")
)

// FastletInfo is a local, eventually-consistent scheduling view. Capacity and
// cache fields are hints learned from Heartbeat; Fastlet admission remains the
// only authority that can consume a slot.
type FastletInfo struct {
	ID        FastletID
	Namespace string
	PodName   string
	PodUID    string
	PodIP     string
	NodeName  string
	PoolName  string

	RuntimeName         apiv1alpha2.RuntimeName
	RuntimeProfileHash  string
	ResourceProfileHash string
	InfraRevision       string
	FastletRevision     string
	InfraReady          bool
	PreparedArtifacts   []string
	RegistryRevision    string
	WarmImages          []fastletapi.WarmImageState
	PodReady            bool
	RuntimeReady        bool
	DrainRequested      bool
	Draining            bool

	Admission fastletapi.AdmissionStatus
	// ResourceCapacity is the Controller-resolved Sandbox commitment capacity
	// projected on the watched Pod. ResourceReceivedAt is local receive time
	// for the versioned Fastlet resource-admission snapshot.
	ResourceCapacity   ResourceDemand
	ResourceReceivedAt time.Time
	resourceFence      resourceRevision

	Images          []string
	CacheEpoch      string
	CacheRevision   uint64
	CacheComplete   bool
	SandboxStatuses map[string]fastletapi.SandboxStatus

	HeartbeatSequence uint64
	LastHeartbeat     time.Time
	PodObservedAt     time.Time
	RejectedUntil     time.Time
}

func (i FastletInfo) Used() int {
	return i.Admission.Used
}

func (i FastletInfo) Capacity() int {
	return i.Admission.Capacity
}

// FastletRegistry is the scheduling view consumed by the Pool controller.
type FastletRegistry interface {
	GetAllFastlets() []FastletInfo
	GetFastletByID(id FastletID) (FastletInfo, bool)
}

type CandidateRequest struct {
	Namespace           string
	PoolName            string
	RuntimeName         apiv1alpha2.RuntimeName
	RuntimeProfileHash  string
	ResourceProfileHash string
	InfraRevision       string
	FastletRevision     string
	ResourceMode        apiv1alpha2.SandboxResourceMode
	Demand              ResourceDemand
	Image               string
	StableKey           string
	Now                 time.Time
	// ExcludedFastlets is request-local; selection never reserves capacity.
	ExcludedFastlets map[FastletID]struct{}
}

type LocalFeedback struct {
	Code       fastletapi.FastletErrorCode
	Reason     fastletapi.AdmissionReason
	Admission  *fastletapi.AdmissionStatus
	ObservedAt time.Time
	RetryAfter time.Duration
}

type fastletSlot struct {
	mu   sync.RWMutex
	info FastletInfo
}

type rankedFastlet struct {
	slot         *fastletSlot
	id           FastletID
	capacity     int
	imageHit     bool
	used         int
	stateRank    int
	dominantLoad float64
	stableOrder  uint64
}

type InMemoryRegistry struct {
	mu         sync.RWMutex
	fastlets   map[FastletID]*fastletSlot
	staleAfter atomic.Int64
	clock      func() time.Time
}

func NewInMemoryRegistry() *InMemoryRegistry {
	registry := &InMemoryRegistry{fastlets: make(map[FastletID]*fastletSlot), clock: time.Now}
	registry.staleAfter.Store(int64(DefaultObservationStaleAfter))
	return registry
}

func (r *InMemoryRegistry) SetStaleAfter(duration time.Duration) { r.staleAfter.Store(int64(duration)) }

// UpsertPod is fed only by the Kubernetes Pod informer. Replacing a Pod UID
// clears all heartbeat state so a reused endpoint can never inherit readiness,
// capacity, or cache facts from the previous Pod instance.
func (r *InMemoryRegistry) UpsertPod(info FastletInfo) {
	if info.ID == "" {
		return
	}
	r.mu.Lock()
	slot := r.fastlets[info.ID]
	if slot == nil {
		slot = &fastletSlot{}
		r.fastlets[info.ID] = slot
	}
	r.mu.Unlock()

	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.info.PodUID != "" && slot.info.PodUID == info.PodUID {
		preserveHeartbeat(&info, slot.info)
	}
	if info.SandboxStatuses == nil {
		info.SandboxStatuses = make(map[string]fastletapi.SandboxStatus)
	}
	slot.info = cloneInfo(info)
}

func preserveHeartbeat(target *FastletInfo, previous FastletInfo) {
	target.RuntimeReady = previous.RuntimeReady
	target.InfraReady = previous.InfraReady
	target.Draining = target.DrainRequested || previous.Draining
	target.Admission = previous.Admission
	target.ResourceReceivedAt = previous.ResourceReceivedAt
	target.resourceFence = previous.resourceFence
	target.Images = append([]string(nil), previous.Images...)
	target.PreparedArtifacts = append([]string(nil), previous.PreparedArtifacts...)
	target.RegistryRevision = previous.RegistryRevision
	target.WarmImages = cloneWarmImageStates(previous.WarmImages)
	target.CacheEpoch = previous.CacheEpoch
	target.CacheRevision = previous.CacheRevision
	target.CacheComplete = previous.CacheComplete
	target.SandboxStatuses = cloneStatuses(previous.SandboxStatuses)
	target.HeartbeatSequence = previous.HeartbeatSequence
	target.LastHeartbeat = previous.LastHeartbeat
	target.RejectedUntil = previous.RejectedUntil
}

func (r *InMemoryRegistry) ApplyHeartbeat(id FastletID, expectedPodUID string, heartbeat *fastletapi.HeartbeatResponse, observedAt time.Time) error {
	if heartbeat == nil {
		return errors.New("heartbeat is nil")
	}
	r.mu.RLock()
	slot := r.fastlets[id]
	r.mu.RUnlock()
	if slot == nil {
		return ErrFastletNotFound
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.info.PodUID == "" || slot.info.PodUID != expectedPodUID || heartbeat.FastletPodUID != expectedPodUID {
		return ErrStalePodIdentity
	}
	if heartbeat.Cache.Epoch == slot.info.CacheEpoch && heartbeat.Sequence <= slot.info.HeartbeatSequence {
		return ErrStaleHeartbeat
	}
	if (slot.info.RuntimeProfileHash != "" && heartbeat.RuntimeProfileHash != slot.info.RuntimeProfileHash) ||
		(slot.info.ResourceProfileHash != "" && heartbeat.ResourceProfileHash != slot.info.ResourceProfileHash) ||
		(slot.info.InfraRevision != "" && heartbeat.InfraRevision != slot.info.InfraRevision) {
		slot.info.RuntimeReady = false
		slot.info.LastHeartbeat = observedAt
		return ErrProfileMismatch
	}

	slot.info.RuntimeReady = heartbeat.RuntimeReady
	slot.info.InfraReady = heartbeat.InfraReady
	slot.info.Draining = slot.info.DrainRequested || heartbeat.Draining
	slot.info.PreparedArtifacts = append([]string(nil), heartbeat.PreparedArtifacts...)
	slot.info.RegistryRevision = heartbeat.RegistryRevision
	slot.info.WarmImages = cloneWarmImageStates(heartbeat.WarmImages)
	applyHeartbeatAdmission(&slot.info, heartbeat.Admission, observedAt)
	if slot.info.RuntimeProfileHash == "" {
		slot.info.RuntimeProfileHash = heartbeat.RuntimeProfileHash
	}
	if slot.info.ResourceProfileHash == "" {
		slot.info.ResourceProfileHash = heartbeat.ResourceProfileHash
	}
	if slot.info.InfraRevision == "" {
		slot.info.InfraRevision = heartbeat.InfraRevision
	}
	slot.info.SandboxStatuses = make(map[string]fastletapi.SandboxStatus, len(heartbeat.SandboxStatuses))
	for _, status := range heartbeat.SandboxStatuses {
		slot.info.SandboxStatuses[status.SandboxID] = status
	}
	if heartbeat.Cache.Full {
		slot.info.CacheComplete = heartbeat.Cache.Complete
		if heartbeat.Cache.Complete {
			slot.info.Images = fastletcache.NormalizeInventory(heartbeat.Cache.Images)
		} else {
			slot.info.Images = nil
		}
	} else if heartbeat.Cache.Epoch != slot.info.CacheEpoch || heartbeat.Cache.Revision != slot.info.CacheRevision {
		// A revision gap without a full snapshot must never be used as an
		// authoritative cache hit.
		slot.info.CacheComplete = false
		slot.info.Images = nil
	}
	slot.info.CacheEpoch = heartbeat.Cache.Epoch
	slot.info.CacheRevision = heartbeat.Cache.Revision
	slot.info.HeartbeatSequence = heartbeat.Sequence
	slot.info.LastHeartbeat = observedAt
	if resourceRejectionCleared(slot.info) {
		slot.info.RejectedUntil = time.Time{}
	}
	return nil
}

func applyHeartbeatAdmission(info *FastletInfo, incoming fastletapi.AdmissionStatus, observedAt time.Time) {
	resourceStatus := incoming.Resources
	if resourceStatus == nil {
		if info.Admission.Resources == nil {
			info.Admission = cloneAdmission(incoming)
		}
		return
	}
	current := info.Admission.Resources
	accept := current == nil || resourceStatus.Epoch != current.Epoch || resourceStatus.Revision >= current.Revision
	if resourceStatus.Epoch == info.resourceFence.Epoch && resourceStatus.Revision <= info.resourceFence.Revision {
		accept = current != nil && resourceStatus.Epoch == current.Epoch && resourceStatus.Revision == current.Revision &&
			current.State == resourceStatus.State && current.Reason == resourceStatus.Reason
	}
	if accept {
		info.Admission = cloneAdmission(incoming)
	}
	if accepted := info.Admission.Resources; accepted != nil && accepted.Epoch == resourceStatus.Epoch && accepted.Revision == resourceStatus.Revision {
		info.ResourceReceivedAt = observedAt
	}
}

func (r *InMemoryRegistry) RecordFeedback(id FastletID, feedback LocalFeedback) {
	r.mu.RLock()
	slot := r.fastlets[id]
	r.mu.RUnlock()
	if slot == nil {
		return
	}
	if feedback.ObservedAt.IsZero() {
		feedback.ObservedAt = r.clock()
	}
	if feedback.RetryAfter <= 0 {
		feedback.RetryAfter = 5 * time.Second
	}
	if feedback.Admission != nil && feedback.Admission.Resources != nil {
		slot.mu.Lock()
		applyFeedbackAdmission(&slot.info, *feedback.Admission, feedback.ObservedAt, feedback.Code == fastletapi.ErrorCapacityRejected)
		slot.mu.Unlock()
	}
	switch feedback.Code {
	case fastletapi.ErrorCapacityRejected, fastletapi.ErrorDraining, fastletapi.ErrorRuntimeUnavailable, fastletapi.ErrorNetworkUnavailable, fastletapi.ErrorInfraUnavailable:
		slot.mu.Lock()
		slot.info.RejectedUntil = feedback.ObservedAt.Add(feedback.RetryAfter)
		slot.mu.Unlock()
	}
}

func applyFeedbackAdmission(info *FastletInfo, incoming fastletapi.AdmissionStatus, observedAt time.Time, fence bool) {
	resourceStatus := incoming.Resources
	if resourceStatus == nil {
		return
	}
	current := info.Admission.Resources
	if current != nil && (resourceStatus.Epoch != current.Epoch || resourceStatus.Revision < current.Revision) {
		return
	}
	info.Admission = cloneAdmission(incoming)
	info.ResourceReceivedAt = observedAt
	if fence || resourceStatus.State == fastletapi.AdmissionStateBlocked || resourceStatus.State == fastletapi.AdmissionStateUnknown {
		info.resourceFence = resourceRevision{Epoch: resourceStatus.Epoch, Revision: resourceStatus.Revision}
	}
}

func resourceRejectionCleared(info FastletInfo) bool {
	status := info.Admission.Resources
	if status == nil {
		return true
	}
	if status.State != fastletapi.AdmissionStateOpen && status.State != fastletapi.AdmissionStateDegraded {
		return false
	}
	return info.resourceFence.Epoch == "" || status.Epoch != info.resourceFence.Epoch || status.Revision > info.resourceFence.Revision
}

func (r *InMemoryRegistry) TopK(request CandidateRequest, k int) []FastletInfo {
	if request.Now.IsZero() {
		request.Now = r.clock()
	}
	request.Image = fastletcache.NormalizeReference(request.Image)
	staleAfter := time.Duration(r.staleAfter.Load())
	r.mu.RLock()
	slots := make([]*fastletSlot, 0, len(r.fastlets))
	for _, slot := range r.fastlets {
		slots = append(slots, slot)
	}
	r.mu.RUnlock()

	heartbeats := heartbeatAgeSummary{}
	blockedNodes := resourceBlockedNodes(slots, request.Now, staleAfter)
	ranked := make([]rankedFastlet, 0, len(slots))
	for _, slot := range slots {
		slot.mu.RLock()
		info := slot.info
		heartbeats.observe(info.LastHeartbeat, request.Now, staleAfter)
		if !hardFilter(info, request, staleAfter, blockedNodes) {
			slot.mu.RUnlock()
			continue
		}
		ranked = append(ranked, rankedFastlet{
			slot:         slot,
			id:           info.ID,
			capacity:     info.Capacity(),
			imageHit:     imageHit(info, request.Image),
			used:         info.Used(),
			stateRank:    admissionStateRank(info),
			dominantLoad: dominantResourceLoad(info, request),
			stableOrder:  stableOrder(request.StableKey, info.ID),
		})
		slot.mu.RUnlock()
	}
	recordHeartbeatAgeSummary(heartbeats)
	sort.Slice(ranked, func(i, j int) bool {
		left, right := ranked[i], ranked[j]
		if left.stateRank != right.stateRank {
			return left.stateRank < right.stateRank
		}
		if left.imageHit != right.imageHit {
			return left.imageHit
		}
		if left.dominantLoad != right.dominantLoad {
			return left.dominantLoad < right.dominantLoad
		}
		if left.stableOrder != right.stableOrder {
			return left.stableOrder < right.stableOrder
		}
		return left.id < right.id
	})
	eligibleCount := len(ranked)
	selectedImageHit := eligibleCount > 0 && ranked[0].imageHit
	if k > 0 && len(ranked) > k {
		ranked = ranked[:k]
	}
	selected := make([]FastletInfo, len(ranked))
	for index := range ranked {
		// Ranking is advisory; re-read and deep-clone only the bounded result.
		// A concurrent heartbeat may make this view newer than the rank, while
		// Fastlet atomic Create remains authoritative for actual admission.
		ranked[index].slot.mu.RLock()
		selected[index] = cloneInfo(ranked[index].slot.info)
		ranked[index].slot.mu.RUnlock()
	}
	recordTopK(len(slots), eligibleCount, request.Image, selectedImageHit)
	return selected
}

func hardFilter(info FastletInfo, request CandidateRequest, staleAfter time.Duration, blockedNodes map[string]struct{}) bool {
	if _, excluded := request.ExcludedFastlets[info.ID]; excluded {
		return false
	}
	if info.Namespace != request.Namespace || info.PoolName != request.PoolName {
		return false
	}
	if !info.PodReady || !info.RuntimeReady || !info.InfraReady || info.Draining || info.LastHeartbeat.IsZero() {
		return false
	}
	if request.Now.Sub(info.LastHeartbeat) > staleAfter || request.Now.Before(info.RejectedUntil) {
		return false
	}
	if info.Capacity() <= 0 || info.Used() >= info.Capacity() {
		return false
	}
	if request.RuntimeName != "" && info.RuntimeName != request.RuntimeName {
		return false
	}
	if request.RuntimeProfileHash != "" && info.RuntimeProfileHash != request.RuntimeProfileHash {
		return false
	}
	if request.ResourceProfileHash != "" && info.ResourceProfileHash != request.ResourceProfileHash {
		return false
	}
	if request.InfraRevision != "" && info.InfraRevision != request.InfraRevision {
		return false
	}
	if request.FastletRevision != "" && info.FastletRevision != request.FastletRevision {
		return false
	}
	if request.ResourceMode == apiv1alpha2.SandboxResourceModeResourceAware {
		if _, blocked := blockedNodes[info.NodeName]; blocked {
			return false
		}
		if !resourceEligible(info, request, staleAfter) {
			return false
		}
	}
	return true
}

func resourceBlockedNodes(slots []*fastletSlot, now time.Time, staleAfter time.Duration) map[string]struct{} {
	blocked := make(map[string]struct{})
	for _, slot := range slots {
		slot.mu.RLock()
		info := slot.info
		status := info.Admission.Resources
		if info.NodeName != "" && status != nil && status.Version == "v1" &&
			status.State == fastletapi.AdmissionStateBlocked && status.Reason == fastletapi.AdmissionReasonNodeMemoryPressure &&
			!info.ResourceReceivedAt.IsZero() && now.Sub(info.ResourceReceivedAt) <= staleAfter &&
			!info.LastHeartbeat.IsZero() && now.Sub(info.LastHeartbeat) <= staleAfter {
			blocked[info.NodeName] = struct{}{}
		}
		slot.mu.RUnlock()
	}
	return blocked
}

func resourceEligible(info FastletInfo, request CandidateRequest, staleAfter time.Duration) bool {
	status := info.Admission.Resources
	if status == nil || status.Version != "v1" || info.ResourceReceivedAt.IsZero() || request.Now.Sub(info.ResourceReceivedAt) > staleAfter {
		return false
	}
	if status.State != fastletapi.AdmissionStateOpen && status.State != fastletapi.AdmissionStateDegraded {
		return false
	}
	if request.Demand.CPU.Sign() <= 0 || request.Demand.Memory.Sign() <= 0 ||
		info.ResourceCapacity.CPU.Sign() <= 0 || info.ResourceCapacity.Memory.Sign() <= 0 {
		return false
	}
	next := int64(info.Used() + 1)
	return next*request.Demand.CPU.MilliValue() <= info.ResourceCapacity.CPU.MilliValue() &&
		next*request.Demand.Memory.Value() <= info.ResourceCapacity.Memory.Value()
}

// EffectiveFreeCapacity returns the immediately schedulable Sandbox count for
// a resource-aware Fastlet. It combines the slot ledger, static commitment,
// dynamic pressure state, and local receive-time freshness.
func EffectiveFreeCapacity(info FastletInfo, demand ResourceDemand, now time.Time, staleAfter time.Duration) int32 {
	request := CandidateRequest{Now: now, ResourceMode: apiv1alpha2.SandboxResourceModeResourceAware, Demand: demand}
	if !info.PodReady || !info.RuntimeReady || !info.InfraReady || info.Draining || info.DrainRequested || info.LastHeartbeat.IsZero() ||
		now.Sub(info.LastHeartbeat) > staleAfter || now.Before(info.RejectedUntil) || !resourceEligible(info, request, staleAfter) {
		return 0
	}
	slotFree := int64(info.Capacity() - info.Used())
	cpuFree := (info.ResourceCapacity.CPU.MilliValue() - int64(info.Used())*demand.CPU.MilliValue()) / demand.CPU.MilliValue()
	memoryFree := (info.ResourceCapacity.Memory.Value() - int64(info.Used())*demand.Memory.Value()) / demand.Memory.Value()
	free := min(slotFree, cpuFree, memoryFree)
	if free <= 0 {
		return 0
	}
	return int32(free)
}

func admissionStateRank(info FastletInfo) int {
	if info.Admission.Resources != nil && info.Admission.Resources.State == fastletapi.AdmissionStateDegraded {
		return 1
	}
	return 0
}

func dominantResourceLoad(info FastletInfo, request CandidateRequest) float64 {
	slotLoad := float64(info.Used()) / float64(info.Capacity())
	if request.ResourceMode != apiv1alpha2.SandboxResourceModeResourceAware {
		return slotLoad
	}
	used := int64(info.Used())
	cpuLoad := float64(used*request.Demand.CPU.MilliValue()) / float64(info.ResourceCapacity.CPU.MilliValue())
	memoryLoad := float64(used*request.Demand.Memory.Value()) / float64(info.ResourceCapacity.Memory.Value())
	return max(slotLoad, cpuLoad, memoryLoad)
}

func imageHit(info FastletInfo, image string) bool {
	if image == "" || !info.CacheComplete {
		return false
	}
	index := sort.SearchStrings(info.Images, image)
	return index < len(info.Images) && info.Images[index] == image
}

func stableOrder(key string, id FastletID) uint64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(key))
	_, _ = hash.Write([]byte{'|'})
	_, _ = hash.Write([]byte(id))
	return hash.Sum64()
}

func (r *InMemoryRegistry) GetAllFastlets() []FastletInfo {
	r.mu.RLock()
	slots := make([]*fastletSlot, 0, len(r.fastlets))
	for _, slot := range r.fastlets {
		slots = append(slots, slot)
	}
	r.mu.RUnlock()
	result := make([]FastletInfo, 0, len(slots))
	for _, slot := range slots {
		slot.mu.RLock()
		result = append(result, cloneInfo(slot.info))
		slot.mu.RUnlock()
	}
	return result
}

func (r *InMemoryRegistry) GetFastletByID(id FastletID) (FastletInfo, bool) {
	r.mu.RLock()
	slot := r.fastlets[id]
	r.mu.RUnlock()
	if slot == nil {
		return FastletInfo{}, false
	}
	slot.mu.RLock()
	result := cloneInfo(slot.info)
	slot.mu.RUnlock()
	return result, true
}

func (r *InMemoryRegistry) RemoveIfPodUID(id FastletID, podUID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	slot := r.fastlets[id]
	if slot == nil {
		return
	}
	slot.mu.RLock()
	matches := slot.info.PodUID == podUID
	slot.mu.RUnlock()
	if matches {
		delete(r.fastlets, id)
	}
}

func cloneInfo(info FastletInfo) FastletInfo {
	info.Admission = cloneAdmission(info.Admission)
	info.ResourceCapacity = ResourceDemand{CPU: info.ResourceCapacity.CPU.DeepCopy(), Memory: info.ResourceCapacity.Memory.DeepCopy()}
	info.Images = append([]string(nil), info.Images...)
	info.PreparedArtifacts = append([]string(nil), info.PreparedArtifacts...)
	info.WarmImages = cloneWarmImageStates(info.WarmImages)
	info.SandboxStatuses = cloneStatuses(info.SandboxStatuses)
	return info
}

func cloneAdmission(admission fastletapi.AdmissionStatus) fastletapi.AdmissionStatus {
	if admission.Resources != nil {
		resources := *admission.Resources
		admission.Resources = &resources
	}
	return admission
}

func cloneWarmImageStates(values []fastletapi.WarmImageState) []fastletapi.WarmImageState {
	return append([]fastletapi.WarmImageState(nil), values...)
}

func cloneStatuses(statuses map[string]fastletapi.SandboxStatus) map[string]fastletapi.SandboxStatus {
	if statuses == nil {
		return make(map[string]fastletapi.SandboxStatus)
	}
	result := make(map[string]fastletapi.SandboxStatus, len(statuses))
	for key, status := range statuses {
		status.InfraComponents = append([]fastletapi.InfraComponentDiagnostic(nil), status.InfraComponents...)
		status.ActionBindings = append([]fastletapi.ActionBindingStatus(nil), status.ActionBindings...)
		result[key] = status
	}
	return result
}
