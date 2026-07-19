package fastletpool

import (
	"context"
	"errors"
	"hash/fnv"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	fastletcache "fast-sandbox/internal/fastlet/cache"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

type FastletID string

var (
	ErrNoCandidate      = errors.New("no schedulable Fastlet candidate")
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

	RuntimeName         apiv1alpha1.RuntimeName
	RuntimeProfileHash  string
	ResourceProfileHash string
	InfraProfile        string
	InfraProfileHash    string
	InfraReady          bool
	PreparedArtifacts   []string
	PodReady            bool
	RuntimeReady        bool
	DrainRequested      bool
	Draining            bool

	Capacity  int
	Allocated int // compatibility projection of Admission.Used; never mutated by selection.
	Admission api.AdmissionStatus

	Images          []string
	CacheEpoch      string
	CacheRevision   uint64
	CacheComplete   bool
	SandboxStatuses map[string]api.SandboxStatus

	HeartbeatSequence uint64
	LastHeartbeat     time.Time
	PodObservedAt     time.Time
	RejectedUntil     time.Time
}

func (i FastletInfo) Used() int {
	if i.Admission.Capacity > 0 || i.Admission.Used > 0 {
		return i.Admission.Used
	}
	return i.Allocated
}

// FastletRegistry retains Allocate/Release as migration adapters for the old
// controllers. Allocate performs candidate selection only and Release is a
// no-op; neither method owns capacity.
type FastletRegistry interface {
	RegisterOrUpdate(info FastletInfo)
	GetAllFastlets() []FastletInfo
	GetFastletByID(id FastletID) (FastletInfo, bool)
	Allocate(sb *apiv1alpha1.Sandbox) (*FastletInfo, error)
	Release(id FastletID, sb *apiv1alpha1.Sandbox)
	Restore(ctx context.Context, c client.Reader) error
	Remove(id FastletID)
	CleanupStaleFastlets(timeout time.Duration) int
}

type CandidateRequest struct {
	Namespace           string
	PoolName            string
	RuntimeName         apiv1alpha1.RuntimeName
	RuntimeProfileHash  string
	ResourceProfileHash string
	InfraProfileHash    string
	Image               string
	StableKey           string
	Now                 time.Time
}

type LocalFeedback struct {
	Code       api.FastletErrorCode
	ObservedAt time.Time
	RetryAfter time.Duration
}

type fastletSlot struct {
	mu   sync.RWMutex
	info FastletInfo
}

type InMemoryRegistry struct {
	mu         sync.RWMutex
	fastlets   map[FastletID]*fastletSlot
	staleAfter atomic.Int64
	clock      func() time.Time
}

func NewInMemoryRegistry() *InMemoryRegistry {
	registry := &InMemoryRegistry{fastlets: make(map[FastletID]*fastletSlot), clock: time.Now}
	registry.staleAfter.Store(int64(45 * time.Second))
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
		info.SandboxStatuses = make(map[string]api.SandboxStatus)
	}
	slot.info = cloneInfo(info)
}

// RegisterOrUpdate is a compatibility entry point for tests and migration
// callers that already provide a combined Pod/Heartbeat view.
func (r *InMemoryRegistry) RegisterOrUpdate(info FastletInfo) {
	if info.ID == "" {
		return
	}
	if info.PodObservedAt.IsZero() {
		info.PodObservedAt = r.clock()
	}
	r.mu.Lock()
	slot := r.fastlets[info.ID]
	if slot == nil {
		slot = &fastletSlot{}
		r.fastlets[info.ID] = slot
	}
	r.mu.Unlock()
	slot.mu.Lock()
	slot.info = cloneInfo(info)
	slot.mu.Unlock()
}

func preserveHeartbeat(target *FastletInfo, previous FastletInfo) {
	target.RuntimeReady = previous.RuntimeReady
	target.InfraReady = previous.InfraReady
	target.Draining = target.DrainRequested || previous.Draining
	target.Capacity = previous.Capacity
	target.Allocated = previous.Allocated
	target.Admission = previous.Admission
	target.Images = append([]string(nil), previous.Images...)
	target.PreparedArtifacts = append([]string(nil), previous.PreparedArtifacts...)
	target.CacheEpoch = previous.CacheEpoch
	target.CacheRevision = previous.CacheRevision
	target.CacheComplete = previous.CacheComplete
	target.SandboxStatuses = cloneStatuses(previous.SandboxStatuses)
	target.HeartbeatSequence = previous.HeartbeatSequence
	target.LastHeartbeat = previous.LastHeartbeat
	target.RejectedUntil = previous.RejectedUntil
}

func (r *InMemoryRegistry) ApplyHeartbeat(id FastletID, expectedPodUID string, heartbeat *api.HeartbeatResponse, observedAt time.Time) error {
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
	if (slot.info.RuntimeProfileHash != "" && heartbeat.Diagnostics.RuntimeProfileHash != slot.info.RuntimeProfileHash) ||
		(slot.info.ResourceProfileHash != "" && heartbeat.ResourceProfileHash != slot.info.ResourceProfileHash) ||
		(slot.info.InfraProfileHash != "" && heartbeat.InfraProfileHash != slot.info.InfraProfileHash) {
		slot.info.RuntimeReady = false
		slot.info.LastHeartbeat = observedAt
		return ErrProfileMismatch
	}

	slot.info.RuntimeReady = heartbeat.RuntimeReady
	slot.info.InfraReady = heartbeat.InfraReady
	slot.info.Draining = slot.info.DrainRequested || heartbeat.Draining
	slot.info.PreparedArtifacts = append([]string(nil), heartbeat.PreparedArtifacts...)
	slot.info.Capacity = heartbeat.Admission.Capacity
	if slot.info.Capacity <= 0 {
		slot.info.Capacity = heartbeat.Capacity
	}
	slot.info.Admission = heartbeat.Admission
	slot.info.Allocated = heartbeat.Admission.Used
	if slot.info.RuntimeProfileHash == "" {
		slot.info.RuntimeProfileHash = heartbeat.Diagnostics.RuntimeProfileHash
	}
	if slot.info.ResourceProfileHash == "" {
		slot.info.ResourceProfileHash = heartbeat.ResourceProfileHash
	}
	if slot.info.InfraProfile == "" {
		slot.info.InfraProfile = heartbeat.InfraProfile
	}
	if slot.info.InfraProfileHash == "" {
		slot.info.InfraProfileHash = heartbeat.InfraProfileHash
	}
	slot.info.SandboxStatuses = make(map[string]api.SandboxStatus, len(heartbeat.SandboxStatuses))
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
	slot.info.RejectedUntil = time.Time{}
	return nil
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
	switch feedback.Code {
	case api.ErrorCapacityRejected, api.ErrorDraining, api.ErrorRuntimeUnavailable, api.ErrorNetworkUnavailable, api.ErrorInfraUnavailable:
		slot.mu.Lock()
		slot.info.RejectedUntil = feedback.ObservedAt.Add(feedback.RetryAfter)
		slot.mu.Unlock()
	}
}

func (r *InMemoryRegistry) TopK(request CandidateRequest, k int) []FastletInfo {
	if request.Now.IsZero() {
		request.Now = r.clock()
	}
	request.Image = fastletcache.NormalizeReference(request.Image)
	candidates := r.GetAllFastlets()
	filtered := candidates[:0]
	for _, info := range candidates {
		if !r.hardFilter(info, request) {
			continue
		}
		filtered = append(filtered, info)
	}
	sort.Slice(filtered, func(i, j int) bool {
		leftHit := imageHit(filtered[i], request.Image)
		rightHit := imageHit(filtered[j], request.Image)
		if leftHit != rightHit {
			return leftHit
		}
		leftUsed, rightUsed := filtered[i].Used(), filtered[j].Used()
		if leftUsed*filtered[j].Capacity != rightUsed*filtered[i].Capacity {
			return leftUsed*filtered[j].Capacity < rightUsed*filtered[i].Capacity
		}
		leftHash := stableOrder(request.StableKey, filtered[i].ID)
		rightHash := stableOrder(request.StableKey, filtered[j].ID)
		if leftHash != rightHash {
			return leftHash < rightHash
		}
		return filtered[i].ID < filtered[j].ID
	})
	if k > 0 && len(filtered) > k {
		filtered = filtered[:k]
	}
	return filtered
}

func (r *InMemoryRegistry) hardFilter(info FastletInfo, request CandidateRequest) bool {
	if info.Namespace != request.Namespace || info.PoolName != request.PoolName {
		return false
	}
	if !info.PodReady || !info.RuntimeReady || info.Draining || info.LastHeartbeat.IsZero() {
		return false
	}
	if request.Now.Sub(info.LastHeartbeat) > time.Duration(r.staleAfter.Load()) || request.Now.Before(info.RejectedUntil) {
		return false
	}
	if info.Capacity <= 0 || info.Used() >= info.Capacity {
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
	if request.InfraProfileHash != "" && (!info.InfraReady || info.InfraProfileHash != request.InfraProfileHash) {
		return false
	}
	return true
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

func (r *InMemoryRegistry) Allocate(sandbox *apiv1alpha1.Sandbox) (*FastletInfo, error) {
	stableKey := string(sandbox.UID)
	if stableKey == "" {
		stableKey = sandbox.Namespace + "/" + sandbox.Name
	}
	candidates := r.TopK(CandidateRequest{
		Namespace: sandbox.Namespace, PoolName: sandbox.Spec.PoolRef, Image: sandbox.Spec.Image, StableKey: stableKey,
	}, 1)
	if len(candidates) == 0 {
		return nil, ErrNoCandidate
	}
	result := candidates[0]
	return &result, nil
}

func (*InMemoryRegistry) Release(FastletID, *apiv1alpha1.Sandbox) {
	// Capacity belongs to Fastlet admission and is refreshed by Heartbeat.
}

func (*InMemoryRegistry) Restore(context.Context, client.Reader) error {
	// Pod membership is restored by informer replay. Sandbox assignments are
	// watched by the assignment store introduced with the shared orchestrator;
	// they must never create phantom Fastlet slots.
	return nil
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

func (r *InMemoryRegistry) Remove(id FastletID) {
	r.mu.Lock()
	delete(r.fastlets, id)
	r.mu.Unlock()
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

// CleanupStaleFastlets reports stale heartbeat views but deliberately does not
// delete them. Kubernetes Watch owns membership; TopK filters stale entries.
func (r *InMemoryRegistry) CleanupStaleFastlets(timeout time.Duration) int {
	now := r.clock()
	count := 0
	for _, info := range r.GetAllFastlets() {
		if info.LastHeartbeat.IsZero() || now.Sub(info.LastHeartbeat) > timeout {
			count++
		}
	}
	return count
}

func cloneInfo(info FastletInfo) FastletInfo {
	info.Images = append([]string(nil), info.Images...)
	info.PreparedArtifacts = append([]string(nil), info.PreparedArtifacts...)
	info.SandboxStatuses = cloneStatuses(info.SandboxStatuses)
	return info
}

func cloneStatuses(statuses map[string]api.SandboxStatus) map[string]api.SandboxStatus {
	if statuses == nil {
		return make(map[string]api.SandboxStatus)
	}
	result := make(map[string]api.SandboxStatus, len(statuses))
	for key, status := range statuses {
		result[key] = status
	}
	return result
}
