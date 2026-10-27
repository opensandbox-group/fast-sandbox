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

func NewFastletID(namespace, name string) FastletID {
	// Keep the historical default-namespace ID stable for in-process callers;
	// every other namespace is explicitly qualified so watched Pods cannot
	// overwrite each other in the cluster-wide Registry.
	if namespace == "" || namespace == "default" {
		return FastletID(name)
	}
	return FastletID(namespace + "/" + name)
}

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
	ResourceCommitted  ResourceDemand
	InFlightResources  ResourceDemand
	InFlightSlots      int
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

func (i FastletInfo) SchedulingUsed() int {
	return i.Admission.Used + i.InFlightSlots
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
	PodUID               string
	BaseResourceEpoch    string
	BaseResourceRevision uint64
	Code                 fastletapi.FastletErrorCode
	Reason               fastletapi.AdmissionReason
	Admission            *fastletapi.AdmissionStatus
	Sandbox              *fastletapi.SandboxStatus
	ObservedAt           time.Time
	RetryAfter           time.Duration
}

type fastletSlot struct {
	mu   sync.RWMutex
	info FastletInfo
}

type rankedFastlet struct {
	slot        *fastletSlot
	id          FastletID
	imageHit    bool
	loadBucket  int
	serverOrder uint64
	stableOrder uint64
}

type InMemoryRegistry struct {
	mu         sync.RWMutex
	fastlets   map[FastletID]*fastletSlot
	staleAfter atomic.Int64
	clock      func() time.Time
	serverSeed string
}

func NewInMemoryRegistry() *InMemoryRegistry {
	registry := &InMemoryRegistry{fastlets: make(map[FastletID]*fastletSlot), clock: time.Now}
	registry.staleAfter.Store(int64(DefaultObservationStaleAfter))
	return registry
}

func (r *InMemoryRegistry) SetStaleAfter(duration time.Duration) { r.staleAfter.Store(int64(duration)) }

// SetServerSeed gives one FastPath replica a stable, soft node preference.
// It changes ranking within a coarse load bucket and never creates ownership.
func (r *InMemoryRegistry) SetServerSeed(seed string) { r.serverSeed = seed }

// ReserveLocal records one request that this FastPath has selected but whose
// Fastlet response has not arrived yet. It prevents concurrent local creates
// from all consuming the same eventually-consistent free capacity.
func (r *InMemoryRegistry) ReserveLocal(id FastletID, demand ResourceDemand) bool {
	r.mu.RLock()
	slot := r.fastlets[id]
	r.mu.RUnlock()
	if slot == nil {
		return false
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.info.SchedulingUsed() >= slot.info.Capacity() {
		return false
	}
	if demand.CPU.Sign() > 0 && demand.Memory.Sign() > 0 {
		cpu := slot.info.ResourceCommitted.CPU.MilliValue() + slot.info.InFlightResources.CPU.MilliValue() + demand.CPU.MilliValue()
		memory := slot.info.ResourceCommitted.Memory.Value() + slot.info.InFlightResources.Memory.Value() + demand.Memory.Value()
		if cpu > slot.info.ResourceCapacity.CPU.MilliValue() || memory > slot.info.ResourceCapacity.Memory.Value() {
			return false
		}
		slot.info.InFlightResources.CPU.Add(demand.CPU)
		slot.info.InFlightResources.Memory.Add(demand.Memory)
	}
	slot.info.InFlightSlots++
	return true
}

func (r *InMemoryRegistry) ReleaseLocal(id FastletID, demand ResourceDemand) {
	r.mu.RLock()
	slot := r.fastlets[id]
	r.mu.RUnlock()
	if slot == nil {
		return
	}
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.info.InFlightSlots > 0 {
		slot.info.InFlightSlots--
	}
	if demand.CPU.Sign() > 0 && demand.Memory.Sign() > 0 {
		slot.info.InFlightResources.CPU.Sub(demand.CPU)
		slot.info.InFlightResources.Memory.Sub(demand.Memory)
	}
}

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
	target.ResourceCommitted = cloneDemand(previous.ResourceCommitted)
	target.InFlightResources = cloneDemand(previous.InFlightResources)
	target.InFlightSlots = previous.InFlightSlots
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
	applyAdmissionResources(&slot.info)
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
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if feedback.PodUID != "" && slot.info.PodUID != feedback.PodUID {
		return
	}
	admissionAccepted := feedback.Admission == nil || feedback.Admission.Resources == nil
	if feedback.Admission != nil && feedback.Admission.Resources != nil {
		admissionAccepted = applyFeedbackAdmission(&slot.info, *feedback.Admission, feedback)
		if admissionAccepted {
			applyAdmissionResources(&slot.info)
		}
	}
	if admissionAccepted && feedback.Sandbox != nil && feedback.Sandbox.SandboxID != "" {
		if slot.info.SandboxStatuses == nil {
			slot.info.SandboxStatuses = make(map[string]fastletapi.SandboxStatus)
		}
		status := cloneSandboxStatus(*feedback.Sandbox)
		current, exists := slot.info.SandboxStatuses[status.SandboxID]
		if !exists || sandboxObservationAtOrAfter(status, current) {
			slot.info.SandboxStatuses[status.SandboxID] = status
		}
	}
	if !admissionAccepted {
		return
	}
	switch feedback.Code {
	case fastletapi.ErrorCapacityRejected, fastletapi.ErrorDraining, fastletapi.ErrorRuntimeUnavailable, fastletapi.ErrorNetworkUnavailable, fastletapi.ErrorInfraUnavailable:
		slot.info.RejectedUntil = feedback.ObservedAt.Add(feedback.RetryAfter)
	default:
		if resourceRejectionCleared(slot.info) {
			slot.info.RejectedUntil = time.Time{}
		}
	}
}

func applyFeedbackAdmission(info *FastletInfo, incoming fastletapi.AdmissionStatus, feedback LocalFeedback) bool {
	resourceStatus := incoming.Resources
	if resourceStatus == nil {
		return false
	}
	current := info.Admission.Resources
	if current != nil {
		if resourceStatus.Epoch == current.Epoch {
			if resourceStatus.Revision < current.Revision {
				return false
			}
		} else if feedback.BaseResourceEpoch != current.Epoch || feedback.BaseResourceRevision != current.Revision {
			return false
		}
	}
	info.Admission = cloneAdmission(incoming)
	info.ResourceReceivedAt = feedback.ObservedAt
	if feedback.Code == fastletapi.ErrorCapacityRejected || resourceStatus.State == fastletapi.AdmissionStateBlocked || resourceStatus.State == fastletapi.AdmissionStateUnknown {
		info.resourceFence = resourceRevision{Epoch: resourceStatus.Epoch, Revision: resourceStatus.Revision}
	}
	return true
}

func sandboxObservationAtOrAfter(incoming, current fastletapi.SandboxStatus) bool {
	if incoming.InstanceGeneration != current.InstanceGeneration {
		return incoming.InstanceGeneration > current.InstanceGeneration
	}
	if incoming.AssignmentAttempt != current.AssignmentAttempt {
		return incoming.AssignmentAttempt > current.AssignmentAttempt
	}
	if incoming.AcceptedGeneration != current.AcceptedGeneration {
		return incoming.AcceptedGeneration > current.AcceptedGeneration
	}
	return incoming.AppliedGeneration >= current.AppliedGeneration
}

func resourceRejectionCleared(info FastletInfo) bool {
	status := info.Admission.Resources
	if status == nil {
		return true
	}
	if status.State != fastletapi.AdmissionStateOpen {
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
		load := dominantResourceLoad(info, request)
		serverOrder := uint64(0)
		if r.serverSeed != "" {
			serverOrder = stableOrder(r.serverSeed, info.ID)
		}
		ranked = append(ranked, rankedFastlet{
			slot:        slot,
			id:          info.ID,
			imageHit:    imageHit(info, request.Image),
			loadBucket:  int(load * 10),
			serverOrder: serverOrder,
			stableOrder: stableOrder(request.StableKey, info.ID),
		})
		slot.mu.RUnlock()
	}
	recordHeartbeatAgeSummary(heartbeats)
	sort.Slice(ranked, func(i, j int) bool {
		left, right := ranked[i], ranked[j]
		if left.imageHit != right.imageHit {
			return left.imageHit
		}
		if left.loadBucket != right.loadBucket {
			return left.loadBucket < right.loadBucket
		}
		if left.serverOrder != right.serverOrder {
			return left.serverOrder < right.serverOrder
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
	if info.Capacity() <= 0 || info.SchedulingUsed() >= info.Capacity() {
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
	infos := make([]FastletInfo, 0, len(slots))
	for _, slot := range slots {
		slot.mu.RLock()
		infos = append(infos, slot.info)
		slot.mu.RUnlock()
	}
	return FreshHostMemoryPressureNodes(infos, now, staleAfter)
}

// FreshHostMemoryPressureNodes returns the host-wide pressure fence reported by
// any fresh Fastlet observation, independent of Pool ownership.
func FreshHostMemoryPressureNodes(infos []FastletInfo, now time.Time, staleAfter time.Duration) map[string]struct{} {
	blocked := make(map[string]struct{})
	for _, info := range infos {
		if HasFreshHostMemoryPressure(info, now, staleAfter) {
			blocked[info.NodeName] = struct{}{}
		}
	}
	return blocked
}

// HasFreshHostMemoryPressure reports whether one observation blocks every
// Fastlet on the same host. Pool scaling uses the same predicate as placement
// so aggregate capacity never includes a sibling Fastlet that Top-K rejects.
func HasFreshHostMemoryPressure(info FastletInfo, now time.Time, staleAfter time.Duration) bool {
	status := info.Admission.Resources
	return info.NodeName != "" && status != nil && status.Version == "v2" &&
		status.State == fastletapi.AdmissionStateBlocked &&
		(status.Reason == fastletapi.AdmissionReasonHostMemoryPressure || status.Reason == fastletapi.AdmissionReasonNodeMemoryPressure) &&
		!info.ResourceReceivedAt.IsZero() && now.Sub(info.ResourceReceivedAt) <= staleAfter &&
		!info.LastHeartbeat.IsZero() && now.Sub(info.LastHeartbeat) <= staleAfter
}

func resourceEligible(info FastletInfo, request CandidateRequest, staleAfter time.Duration) bool {
	view := ResourceSchedulability(info, request.Now, staleAfter)
	return view.Fits(request.Demand)
}

type SchedulabilityView struct {
	Schedulable   bool
	FreeSlots     int32
	FreeResources ResourceDemand
}

func (v SchedulabilityView) Fits(demand ResourceDemand) bool {
	return v.Schedulable && demand.CPU.Sign() > 0 && demand.Memory.Sign() > 0 &&
		demand.CPU.Cmp(v.FreeResources.CPU) <= 0 && demand.Memory.Cmp(v.FreeResources.Memory) <= 0
}

// ResourceSchedulability is the shared Fastlet capacity view used by placement,
// Pool scaling, and the operator summary. Profile and node-pressure filters are
// deliberately applied by their callers.
func ResourceSchedulability(info FastletInfo, now time.Time, staleAfter time.Duration) SchedulabilityView {
	status := info.Admission.Resources
	if !info.PodReady || !info.RuntimeReady || !info.InfraReady || info.Draining || info.DrainRequested || info.LastHeartbeat.IsZero() ||
		now.Sub(info.LastHeartbeat) > staleAfter || now.Before(info.RejectedUntil) || status == nil || status.Version != "v2" ||
		info.ResourceReceivedAt.IsZero() || now.Sub(info.ResourceReceivedAt) > staleAfter ||
		status.State != fastletapi.AdmissionStateOpen {
		return SchedulabilityView{}
	}
	free := ResourceDemand{CPU: info.ResourceCapacity.CPU.DeepCopy(), Memory: info.ResourceCapacity.Memory.DeepCopy()}
	free.CPU.Sub(info.ResourceCommitted.CPU)
	free.CPU.Sub(info.InFlightResources.CPU)
	free.Memory.Sub(info.ResourceCommitted.Memory)
	free.Memory.Sub(info.InFlightResources.Memory)
	freeSlots := int32(max(0, info.Capacity()-info.SchedulingUsed()))
	if freeSlots <= 0 || free.CPU.Sign() <= 0 || free.Memory.Sign() <= 0 {
		return SchedulabilityView{}
	}
	return SchedulabilityView{Schedulable: true, FreeSlots: freeSlots, FreeResources: free}
}

// EffectiveFreeCapacity returns the immediately schedulable Sandbox count for
// a resource-aware Fastlet. It combines the slot ledger, static commitment,
// dynamic pressure state, and local receive-time freshness.
func EffectiveFreeCapacity(info FastletInfo, demand ResourceDemand, now time.Time, staleAfter time.Duration) int32 {
	view := ResourceSchedulability(info, now, staleAfter)
	if !view.Fits(demand) {
		return 0
	}
	cpuFree := view.FreeResources.CPU.MilliValue() / demand.CPU.MilliValue()
	memoryFree := view.FreeResources.Memory.Value() / demand.Memory.Value()
	free := min(int64(view.FreeSlots), cpuFree, memoryFree)
	if free <= 0 {
		return 0
	}
	return int32(free)
}

func dominantResourceLoad(info FastletInfo, request CandidateRequest) float64 {
	slotLoad := float64(info.SchedulingUsed()) / float64(info.Capacity())
	if request.ResourceMode != apiv1alpha2.SandboxResourceModeResourceAware {
		return slotLoad
	}
	cpuLoad := float64(info.ResourceCommitted.CPU.MilliValue()+info.InFlightResources.CPU.MilliValue()) / float64(info.ResourceCapacity.CPU.MilliValue())
	memoryLoad := float64(info.ResourceCommitted.Memory.Value()+info.InFlightResources.Memory.Value()) / float64(info.ResourceCapacity.Memory.Value())
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
	info.ResourceCommitted = cloneDemand(info.ResourceCommitted)
	info.InFlightResources = cloneDemand(info.InFlightResources)
	info.Images = append([]string(nil), info.Images...)
	info.PreparedArtifacts = append([]string(nil), info.PreparedArtifacts...)
	info.WarmImages = cloneWarmImageStates(info.WarmImages)
	info.SandboxStatuses = cloneStatuses(info.SandboxStatuses)
	return info
}

func cloneDemand(demand ResourceDemand) ResourceDemand {
	return ResourceDemand{CPU: demand.CPU.DeepCopy(), Memory: demand.Memory.DeepCopy()}
}

func applyAdmissionResources(info *FastletInfo) {
	if info == nil || info.Admission.Resources == nil {
		return
	}
	status := info.Admission.Resources
	capacity, capacityOK := parseResourceVector(status.Capacity)
	committed, committedOK := parseResourceVector(status.Committed)
	if !capacityOK || !committedOK {
		info.ResourceReceivedAt = time.Time{}
		return
	}
	// The Pod annotation is the Controller projection; the Fastlet snapshot
	// must agree before its ledger can become scheduling input.
	if info.ResourceCapacity.CPU.Sign() > 0 && (capacity.CPU.Cmp(info.ResourceCapacity.CPU) != 0 || capacity.Memory.Cmp(info.ResourceCapacity.Memory) != 0) {
		info.ResourceReceivedAt = time.Time{}
		return
	}
	info.ResourceCapacity = capacity
	info.ResourceCommitted = committed
}

func parseResourceVector(vector fastletapi.SandboxResourceVector) (ResourceDemand, bool) {
	cpu, err := resource.ParseQuantity(vector.CPU)
	if err != nil || cpu.Sign() < 0 {
		return ResourceDemand{}, false
	}
	memory, err := resource.ParseQuantity(vector.Memory)
	if err != nil || memory.Sign() < 0 {
		return ResourceDemand{}, false
	}
	return ResourceDemand{CPU: cpu, Memory: memory}, true
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
		result[key] = cloneSandboxStatus(status)
	}
	return result
}

func cloneSandboxStatus(status fastletapi.SandboxStatus) fastletapi.SandboxStatus {
	status.InfraComponents = append([]fastletapi.InfraComponentDiagnostic(nil), status.InfraComponents...)
	status.ActionBindings = append([]fastletapi.ActionBindingStatus(nil), status.ActionBindings...)
	return status
}
