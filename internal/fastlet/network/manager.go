package network

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fast-sandbox/internal/observability"

	"k8s.io/klog/v2"
)

const currentSlotVersion = 1

// Defaults of the Fastlet slot data plane; exported for callers (fastlet
// main) that need the same values for pre-flight validation.
const (
	DefaultPrivateCIDR = "172.30.0.0/24"
	DefaultBridge      = "fsb0"
	// DefaultMTU matches the guest eth0 MTU baked into templates (the
	// kernel default; boot args cannot set MTU), so the default data plane
	// is MSS-aligned without PMTUD. Overlay deployments must lower it via
	// FAST_SANDBOX_NETWORK_MTU (the create-time guest MTU warning then
	// flags the resulting mismatch).
	DefaultMTU = 1500
)

type Config struct {
	Capacity         int
	PodUID           string
	PodName          string
	PodNamespace     string
	PrivateCIDR      string
	Bridge           string
	MTU              int
	EgressDevice     string
	StateRoot        string
	NetNSRoot        string
	HostNetNSRoot    string
	IDGenerator      func() (string, error)
	Now              func() time.Time
	ReplenishTimeout time.Duration
}

func DefaultConfig(capacity int, podUID string) Config {
	return Config{
		Capacity: capacity, PodUID: podUID, PrivateCIDR: DefaultPrivateCIDR,
		Bridge: DefaultBridge, MTU: DefaultMTU,
		StateRoot: "/run/fast-sandbox/network", NetNSRoot: "/run/netns",
		HostNetNSRoot: "/run/fast-sandbox/netns", ReplenishTimeout: time.Minute,
	}
}

type Manager struct {
	mu        sync.RWMutex
	prepareMu sync.Mutex
	config    Config
	driver    Driver
	store     StateStore
	ipam      *IPv4IPAM
	slots     map[string]*Slot
	closed    chan struct{} // closed by Close: no more slot preparation
	hit       atomic.Uint64
	miss      atomic.Uint64
}

func NewManager(config Config, driver Driver, store StateStore) (*Manager, error) {
	if config.Capacity <= 0 || config.PodUID == "" || driver == nil || store == nil {
		return nil, fmt.Errorf("capacity, Pod UID, network driver, and state store are required")
	}
	if config.PrivateCIDR == "" {
		config.PrivateCIDR = DefaultPrivateCIDR
	}
	if config.Bridge == "" {
		config.Bridge = DefaultBridge
	}
	if config.MTU <= 0 {
		config.MTU = DefaultMTU
	}
	if config.NetNSRoot == "" || config.HostNetNSRoot == "" || config.StateRoot == "" {
		return nil, fmt.Errorf("state, Fastlet netns, and host netns roots are required")
	}
	if config.IDGenerator == nil {
		config.IDGenerator = randomID
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.ReplenishTimeout <= 0 {
		config.ReplenishTimeout = time.Minute
	}
	var (
		ipam *IPv4IPAM
		err  error
	)
	if _, guestDataPlane := driver.(guestDataPlane); guestDataPlane {
		// Guest-VM runtimes reserve the baked guest address so slot IPs can
		// never collide with the shared clone address.
		ipam, err = NewGuestVMIPAM(config.PrivateCIDR)
	} else {
		ipam, err = NewIPv4IPAM(config.PrivateCIDR)
	}
	if err != nil {
		return nil, err
	}
	return &Manager{
		config: config, driver: driver, store: store, ipam: ipam,
		slots: make(map[string]*Slot), closed: make(chan struct{}),
	}, nil
}

// Initialize loads and validates durable state, retries interrupted destroys,
// and prepares the clean capacity needed before Fastlet becomes Ready.
func (m *Manager) Initialize(ctx context.Context) error {
	loaded, err := m.store.LoadAll(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	for _, slot := range loaded {
		if err := m.validateStoredSlot(slot); err != nil {
			m.mu.Unlock()
			return err
		}
		if _, duplicate := m.slots[slot.ID]; duplicate {
			m.mu.Unlock()
			return fmt.Errorf("%w: duplicate slot %s", ErrStateInconsistent, slot.ID)
		}
		m.slots[slot.ID] = slot
	}
	m.mu.Unlock()
	if len(loaded) > m.config.Capacity {
		return fmt.Errorf("%w: stored slots %d exceed capacity %d", ErrStateInconsistent, len(loaded), m.config.Capacity)
	}

	for _, slot := range loaded {
		if slot.Phase == SlotPhaseDestroying {
			if err := m.destroySlot(ctx, slot.ID); err != nil {
				return err
			}
			continue
		}
		if err := m.driver.Validate(ctx, slot); err != nil {
			if slot.Phase == SlotPhaseBound {
				return fmt.Errorf("%w: bound slot %s failed validation: %v", ErrStateInconsistent, slot.ID, err)
			}
			if destroyErr := m.markAndDestroy(ctx, slot.ID); destroyErr != nil {
				return fmt.Errorf("invalid clean slot %s: %v; destroy: %w", slot.ID, err, destroyErr)
			}
		}
	}
	if err := m.Replenish(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	m.recordPhasesLocked()
	m.mu.Unlock()
	return nil
}

// Reconcile binds durable slots to the runtime inventory during Fastlet
// restart. Runtime objects without state are unsafe; state without a runtime is
// an orphan and is destroyed before new admission is enabled.
func (m *Manager) Reconcile(ctx context.Context, runtimeOwners []Owner) error {
	owners := make(map[string]Owner, len(runtimeOwners))
	for _, owner := range runtimeOwners {
		if owner.SandboxUID == "" {
			continue
		}
		owners[owner.SandboxUID] = owner
	}
	m.mu.RLock()
	bound := make([]*Slot, 0)
	for _, slot := range m.slots {
		if slot.Phase == SlotPhaseBound {
			copy := *slot
			bound = append(bound, &copy)
		}
	}
	m.mu.RUnlock()
	seen := make(map[string]struct{}, len(bound))
	for _, slot := range bound {
		owner, exists := owners[slot.Owner.SandboxUID]
		if !exists {
			if err := m.markAndDestroy(ctx, slot.ID); err != nil {
				return err
			}
			continue
		}
		if !slot.Owner.Equal(owner) {
			return fmt.Errorf("%w: slot %s owner does not match runtime identity", ErrStateInconsistent, slot.ID)
		}
		seen[owner.SandboxUID] = struct{}{}
	}
	for sandboxUID := range owners {
		if _, exists := seen[sandboxUID]; !exists {
			return fmt.Errorf("%w: runtime sandbox %s has no network slot", ErrStateInconsistent, sandboxUID)
		}
	}
	return m.Replenish(ctx)
}

func (m *Manager) Acquire(ctx context.Context, owner Owner) (_ *Slot, resultErr error) {
	started := time.Now()
	ctx, span := observability.Start(ctx, "fastlet.network.acquire")
	result := "error"
	defer func() {
		observeSlotAcquire(result, started)
		observability.End(span, resultErr)
	}()
	if owner.SandboxUID == "" || owner.InstanceGeneration <= 0 || owner.AssignmentAttempt <= 0 {
		return nil, fmt.Errorf("invalid network slot owner")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, slot := range m.slots {
		if slot.Phase == SlotPhaseBound && slot.Owner.SandboxUID == owner.SandboxUID {
			if !slot.Owner.Equal(owner) {
				return nil, ErrOwnerConflict
			}
			m.hit.Add(1)
			recordSlotAcquire("hit")
			result = "existing"
			return cloneSlot(slot), nil
		}
	}
	ids := m.sortedSlotIDsLocked()
	for _, id := range ids {
		slot := m.slots[id]
		if slot.Phase != SlotPhaseClean {
			continue
		}
		now := m.config.Now()
		candidate := *slot
		candidate.Phase = SlotPhaseBound
		candidate.Owner = owner
		candidate.BoundAt = &now
		candidate.Access = AccessDescriptor{Kind: AccessKindDirectIP, Address: candidate.IP, NetNSPath: candidate.HostNetNSPath}
		persistStarted := time.Now()
		persistContext, persistSpan := observability.Start(ctx, "fastlet.network.acquire.persist")
		err := m.store.Save(persistContext, &candidate)
		observeSlotPersist(persistStarted, err)
		observability.End(persistSpan, err)
		if err != nil {
			return nil, err
		}
		*slot = candidate
		m.hit.Add(1)
		recordSlotAcquire("hit")
		m.recordPhasesLocked()
		result = "bound"
		return cloneSlot(slot), nil
	}
	m.miss.Add(1)
	recordSlotAcquire("miss")
	result = "empty"
	klog.ErrorS(ErrNoCleanSlot, "network slot acquire missed",
		"sandboxID", owner.SandboxUID, "capacity", m.config.Capacity, "slotCount", len(m.slots))
	// Per-slot detail is diagnostics, not an error per slot: under
	// saturation it would repeat capacity×QPS identical lines.
	for _, id := range m.sortedSlotIDsLocked() {
		slot := m.slots[id]
		boundOwner := ""
		if slot.Owner.SandboxUID != "" {
			boundOwner = slot.Owner.SandboxUID
		}
		klog.V(2).InfoS("slot pool detail",
			"id", id, "phase", slot.Phase, "owner", boundOwner,
			"generation", slot.Owner.InstanceGeneration, "createdAt", slot.CreatedAt.Format(time.RFC3339Nano))
	}
	return nil, ErrNoCleanSlot
}

func (m *Manager) Lookup(sandboxUID string) (*Slot, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, slot := range m.slots {
		if slot.Phase == SlotPhaseBound && slot.Owner.SandboxUID == sandboxUID {
			return cloneSlot(slot), true
		}
	}
	return nil, false
}

// guestDataPlane is the optional interface of network drivers that own a
// per-restore guest NAT data plane (guest-VM runtimes): slots are prepared
// before the image (and its baked guest address) is known, so the guest-
// specific rules are applied when the runtime driver restores a Sandbox.
type guestDataPlane interface {
	ApplyGuest(ctx context.Context, slot *Slot, guestIP string) error
}

// ApplyGuest installs the guest NAT data plane for the baked guest address
// on the slot bound to owner and persists the applied address for teardown.
// The netns commands run OUTSIDE the manager lock: parallel restores would
// otherwise serialize on them (~15 ms of exec per slot). The slot is
// re-validated under the lock before the result is committed, so a
// concurrent release cannot be overwritten. Drivers without a guest data
// plane (container runtimes) record nothing.
func (m *Manager) ApplyGuest(ctx context.Context, owner Owner, guestIP string) error {
	m.mu.Lock()
	var applied *Slot
	for _, slot := range m.slots {
		if slot.Phase == SlotPhaseBound && slot.Owner.Equal(owner) {
			applied = cloneSlot(slot)
			break
		}
	}
	m.mu.Unlock()
	if applied == nil {
		return ErrSlotNotFound
	}
	if guestDriver, ok := m.driver.(guestDataPlane); ok {
		if err := guestDriver.ApplyGuest(ctx, applied, guestIP); err != nil {
			return err
		}
	} else {
		applied.GuestIP = guestIP
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.slots[applied.ID]
	if current == nil || current.Phase != SlotPhaseBound || !current.Owner.Equal(owner) {
		return ErrSlotNotFound
	}
	*current = *applied
	return m.store.Save(ctx, current)
}

// Release destroys a used slot. It never returns that slot directly to the
// clean pool; a new slot with a new ID/resources is prepared asynchronously.
func (m *Manager) Release(ctx context.Context, owner Owner) error {
	m.mu.RLock()
	var target *Slot
	for _, slot := range m.slots {
		if slot.Owner.SandboxUID == owner.SandboxUID && (slot.Phase == SlotPhaseBound || slot.Phase == SlotPhaseDestroying) {
			target = cloneSlot(slot)
			break
		}
	}
	m.mu.RUnlock()
	if target == nil {
		return nil
	}
	if !target.Owner.Equal(owner) {
		return ErrOwnerConflict
	}
	if err := m.markAndDestroy(ctx, target.ID); err != nil {
		return err
	}
	go func() {
		replenishCtx, cancel := context.WithTimeout(context.Background(), m.config.ReplenishTimeout)
		defer cancel()
		if err := m.Replenish(replenishCtx); err != nil {
			klog.V(2).InfoS("background slot replenish failed", "err", err)
		}
	}()
	return nil
}

// destroyFailedSlot tears down a slot whose preparation failed; a destroy
// failure here leaks host network resources, so it is never silent.
func (m *Manager) destroyFailedSlot(ctx context.Context, slot *Slot, stage string) {
	if err := m.driver.Destroy(ctx, slot); err != nil {
		klog.ErrorS(err, "Failed slot destroy after failed preparation", "slot", slot.ID, "stage", stage)
	}
}

func (m *Manager) Replenish(ctx context.Context) error {
	for {
		m.mu.RLock()
		select {
		case <-m.closed:
			m.mu.RUnlock()
			return nil
		default:
		}
		count := len(m.slots)
		m.mu.RUnlock()
		if count >= m.config.Capacity {
			return nil
		}
		if err := m.prepareOne(ctx); err != nil {
			return err
		}
	}
}

// Close stops slot preparation and destroys every remaining slot (clean
// pool, bound, and destroying leftovers), leaving the host with no
// per-slot resources. It is idempotent; a Close racing an in-flight
// preparation discards the freshly created slot and tears its resources
// down. Acquire after Close fails with ErrNoCleanSlot (the clean pool is
// empty); Release after Close is a silent no-op.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	select {
	case <-m.closed:
		m.mu.Unlock()
		return nil
	default:
		close(m.closed)
	}
	m.mu.Unlock()

	for {
		m.mu.RLock()
		ids := m.sortedSlotIDsLocked()
		m.mu.RUnlock()
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			if err := m.destroySlot(ctx, id); err != nil {
				klog.Warningf("network slot %s close-destroy failed: %v", id, err)
			}
		}
	}
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := Snapshot{Capacity: m.config.Capacity, Hit: m.hit.Load(), Miss: m.miss.Load()}
	for _, slot := range m.slots {
		switch slot.Phase {
		case SlotPhaseClean:
			result.Clean++
		case SlotPhaseBound:
			result.Bound++
		case SlotPhaseDestroying:
			result.Destroying++
		}
	}
	return result
}

func (m *Manager) prepareOne(ctx context.Context) error {
	m.prepareMu.Lock()
	defer m.prepareMu.Unlock()
	m.mu.Lock()
	if len(m.slots) >= m.config.Capacity {
		m.mu.Unlock()
		return nil
	}
	used := make(map[string]struct{}, len(m.slots))
	for _, slot := range m.slots {
		used[slot.IP] = struct{}{}
	}
	ip, address, err := m.ipam.Allocate(used)
	if err != nil {
		m.mu.Unlock()
		return err
	}
	id, err := m.config.IDGenerator()
	if err != nil {
		m.mu.Unlock()
		return err
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if !safeSlotID.MatchString(id) {
		m.mu.Unlock()
		return fmt.Errorf("ID generator returned invalid slot id %q", id)
	}
	if _, duplicate := m.slots[id]; duplicate {
		m.mu.Unlock()
		return fmt.Errorf("slot ID generator returned duplicate %q", id)
	}
	netnsName := resourceName("fsb", m.config.PodUID, id, 63)
	hostVeth := resourceName("fh", m.config.PodUID, id, 15)
	peerVeth := resourceName("fp", m.config.PodUID, id, 15)
	slot := &Slot{
		Version: currentSlotVersion, ID: id,
		OwnerPodUID: m.config.PodUID, OwnerPodName: m.config.PodName, OwnerNamespace: m.config.PodNamespace,
		Phase:     SlotPhaseClean,
		NetNSName: netnsName, NetNSPath: filepath.Join(m.config.NetNSRoot, netnsName),
		HostNetNSPath: filepath.Join(m.config.HostNetNSRoot, netnsName),
		HostVeth:      hostVeth, PeerVeth: peerVeth, Bridge: m.config.Bridge,
		GuestTap: guestVMDefaultTapName,
		Address:  address, IP: ip, Gateway: m.ipam.Gateway(), PrivateCIDR: m.ipam.CIDR(),
		DNSPath: filepath.Join(m.config.StateRoot, m.config.PodUID, id+".resolv.conf"),
		MTU:     m.config.MTU, EgressDevice: m.config.EgressDevice, CreatedAt: m.config.Now(),
	}
	// Reserve the IP/ID in-memory while the relatively slow Linux preparation
	// runs. Acquire cannot observe it until its state becomes Clean.
	reserved := cloneSlot(slot)
	reserved.Phase = SlotPhaseDestroying
	m.slots[id] = reserved
	m.mu.Unlock()

	if err := m.driver.Prepare(ctx, slot); err != nil {
		m.destroyFailedSlot(context.Background(), slot, "prepare")
		m.mu.Lock()
		delete(m.slots, id)
		m.mu.Unlock()
		return fmt.Errorf("prepare network slot %s: %w", id, err)
	}
	slot.Phase = SlotPhaseClean
	if err := m.store.Save(ctx, slot); err != nil {
		m.destroyFailedSlot(context.Background(), slot, "save")
		m.mu.Lock()
		delete(m.slots, id)
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	select {
	case <-m.closed:
		// Close raced this preparation: the slot must not surface, and its
		// freshly created host resources are torn down so Close leaves no
		// leftovers.
		m.mu.Unlock()
		m.destroyFailedSlot(context.Background(), slot, "close-race")
		_ = m.store.Delete(ctx, id)
		m.mu.Lock()
		delete(m.slots, id)
		m.mu.Unlock()
		return nil
	default:
		m.slots[id] = slot
	}
	m.recordPhasesLocked()
	m.mu.Unlock()
	return nil
}

func (m *Manager) markAndDestroy(ctx context.Context, slotID string) error {
	m.mu.Lock()
	slot := m.slots[slotID]
	if slot == nil {
		m.mu.Unlock()
		return nil
	}
	if slot.Phase != SlotPhaseDestroying {
		candidate := *slot
		candidate.Phase = SlotPhaseDestroying
		if err := m.store.Save(ctx, &candidate); err != nil {
			m.mu.Unlock()
			return err
		}
		*slot = candidate
		m.recordPhasesLocked()
	}
	m.mu.Unlock()
	return m.destroySlot(ctx, slotID)
}

func (m *Manager) destroySlot(ctx context.Context, slotID string) error {
	m.mu.RLock()
	slot := cloneSlot(m.slots[slotID])
	m.mu.RUnlock()
	if slot == nil {
		return nil
	}
	if err := m.driver.Destroy(ctx, slot); err != nil {
		// A leaked namespace keeps owning its slot IP on the shared bridge
		// and shadows the next slot with the same address; log the reason.
		klog.Warningf("network slot %s destroy failed: %v", slotID, err)
		return fmt.Errorf("destroy network slot %s: %w", slotID, err)
	}
	if err := m.store.Delete(ctx, slotID); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.slots, slotID)
	m.recordPhasesLocked()
	m.mu.Unlock()
	return nil
}

func (m *Manager) validateStoredSlot(slot *Slot) error {
	if slot == nil || slot.Version != currentSlotVersion || slot.OwnerPodUID != m.config.PodUID ||
		!safeSlotID.MatchString(slot.ID) || slot.NetNSName == "" || slot.NetNSPath == "" ||
		slot.HostNetNSPath == "" || slot.IP == "" || slot.PrivateCIDR != m.ipam.CIDR() {
		return fmt.Errorf("%w: invalid stored network slot", ErrStateInconsistent)
	}
	if slot.Phase != SlotPhaseClean && slot.Phase != SlotPhaseBound && slot.Phase != SlotPhaseDestroying {
		return fmt.Errorf("%w: invalid phase %q", ErrStateInconsistent, slot.Phase)
	}
	if slot.Phase == SlotPhaseBound && slot.Owner.SandboxUID == "" {
		return fmt.Errorf("%w: bound slot has no owner", ErrStateInconsistent)
	}
	return nil
}

func (m *Manager) sortedSlotIDsLocked() []string {
	ids := make([]string, 0, len(m.slots))
	for id := range m.slots {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (m *Manager) recordPhasesLocked() {
	clean, bound, destroying := 0, 0, 0
	for _, slot := range m.slots {
		switch slot.Phase {
		case SlotPhaseClean:
			clean++
		case SlotPhaseBound:
			bound++
		case SlotPhaseDestroying:
			destroying++
		}
	}
	recordSlotPhases(clean, bound, destroying)
}

func cloneSlot(slot *Slot) *Slot {
	if slot == nil {
		return nil
	}
	copy := *slot
	if slot.BoundAt != nil {
		boundAt := *slot.BoundAt
		copy.BoundAt = &boundAt
	}
	return &copy
}

func randomID() (string, error) {
	data := make([]byte, 10)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return "slot-" + hex.EncodeToString(data), nil
}

func resourceName(prefix, podUID, id string, limit int) string {
	digest := sha256.Sum256([]byte(podUID + "\x00" + id))
	compact := hex.EncodeToString(digest[:])
	if len(compact) > limit-len(prefix) {
		compact = compact[:limit-len(prefix)]
	}
	return prefix + compact
}
