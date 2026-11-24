package containerd

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	fastletnetwork "fast-sandbox/internal/fastlet/network"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

const recoverTestSandboxUID = "sandbox-1"

// noopNetDriver stands in for the host netns driver: recovery exercises
// manager bookkeeping, not host namespace commands.
type noopNetDriver struct{}

func (noopNetDriver) Prepare(context.Context, *fastletnetwork.Slot) error  { return nil }
func (noopNetDriver) Validate(context.Context, *fastletnetwork.Slot) error { return nil }
func (noopNetDriver) Destroy(context.Context, *fastletnetwork.Slot) error  { return nil }

// memStateStore keeps slots in memory for the manager fixture.
type memStateStore struct {
	mu    sync.Mutex
	slots map[string]*fastletnetwork.Slot
}

func newMemStateStore() *memStateStore {
	return &memStateStore{slots: make(map[string]*fastletnetwork.Slot)}
}

func (s *memStateStore) LoadAll(context.Context) ([]*fastletnetwork.Slot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	slots := make([]*fastletnetwork.Slot, 0, len(s.slots))
	for _, slot := range s.slots {
		clone := *slot
		slots = append(slots, &clone)
	}
	return slots, nil
}

func (s *memStateStore) Save(_ context.Context, slot *fastletnetwork.Slot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := *slot
	s.slots[slot.ID] = &clone
	return nil
}

func (s *memStateStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.slots, id)
	return nil
}

// newRecoverNetworkManager builds an in-process network manager with one
// pre-created Clean slot, mirroring the driver test fixture of the
// Firecracker runtime.
func newRecoverNetworkManager(t *testing.T) *fastletnetwork.Manager {
	t.Helper()
	root := t.TempDir()
	manager, err := fastletnetwork.NewManager(fastletnetwork.Config{
		Capacity: 1, PodUID: "pod-1", PrivateCIDR: "172.30.0.0/24",
		StateRoot: root, NetNSRoot: root + "/netns", HostNetNSRoot: root + "/host-netns",
		IDGenerator: func() (string, error) { return "slot-1", nil },
		Now:         func() time.Time { return time.Unix(1720000000, 0) },
	}, noopNetDriver{}, newMemStateStore())
	require.NoError(t, err)
	require.NoError(t, manager.Initialize(context.Background()))
	t.Cleanup(manager.Quiesce)
	return manager
}

func acquireRecoverSlot(t *testing.T, manager *fastletnetwork.Manager) *fastletnetwork.Slot {
	t.Helper()
	slot, err := manager.Acquire(context.Background(), fastletnetwork.Owner{
		SandboxUID: recoverTestSandboxUID, SandboxName: recoverTestSandboxUID, SandboxNamespace: "tenant-a",
		InstanceGeneration: 1, RuntimeInstanceID: "runtime-1", AssignmentAttempt: 1,
	})
	require.NoError(t, err)
	return slot
}

// recoverMetadataFor returns durable inventory metadata whose network
// descriptor matches the slot exactly, as Fastlet would have persisted it.
func recoverMetadataFor(slot *fastletnetwork.Slot) *SandboxMetadata {
	return &SandboxMetadata{
		Config: fastletapi.RuntimeSandboxConfig{
			Identity: fastletapi.SandboxIdentity{
				SandboxUID: slot.Owner.SandboxUID, Namespace: slot.Owner.SandboxNamespace,
				Name: slot.Owner.SandboxName, InstanceGeneration: slot.Owner.InstanceGeneration,
				RuntimeInstanceID: slot.Owner.RuntimeInstanceID, AssignmentAttempt: slot.Owner.AssignmentAttempt,
			},
		},
		Allocation: fastletapi.RuntimeAllocation{Network: fastletapi.NetworkAllocation{
			SlotID: slot.ID, NamespacePath: slot.HostNetNSPath, IP: slot.IP,
		}},
	}
}

func TestRecoverRuntimeResourcesWithoutManagerIsNoOp(t *testing.T) {
	driver := newTestContainerdRuntime()
	metadata := recoverMetadataFor(&fastletnetwork.Slot{Owner: fastletnetwork.Owner{SandboxUID: recoverTestSandboxUID}})
	require.NoError(t, driver.RecoverRuntimeResources(context.Background(), []*SandboxMetadata{metadata}, nil))
}

func TestRecoverRuntimeResourcesRetainsMatchingSandbox(t *testing.T) {
	driver := newTestContainerdRuntime()
	manager := newRecoverNetworkManager(t)
	driver.SetNetworkManager(manager)

	slot := acquireRecoverSlot(t, manager)
	metadata := recoverMetadataFor(slot)

	require.NoError(t, driver.RecoverRuntimeResources(context.Background(), []*SandboxMetadata{metadata}, nil))
	require.Equal(t, 1, manager.Snapshot().Bound, "the bound slot must survive recovery")

	// A Fastlet restart re-runs recovery against the same inventory and
	// must stay idempotent.
	require.NoError(t, driver.RecoverRuntimeResources(context.Background(), []*SandboxMetadata{metadata}, nil))
	require.Equal(t, 1, manager.Snapshot().Bound)
}

func TestRecoverRuntimeResourcesRejectsDescriptorDrift(t *testing.T) {
	drifts := []struct {
		name  string
		shift func(*SandboxMetadata)
	}{
		{"missing slot", func(m *SandboxMetadata) { m.Config.Identity.SandboxUID = "sandbox-orphan" }},
		{"empty slot id", func(m *SandboxMetadata) { m.Allocation.Network.SlotID = "" }},
		{"slot id mismatch", func(m *SandboxMetadata) { m.Allocation.Network.SlotID = "slot-other" }},
		{"netns path mismatch", func(m *SandboxMetadata) { m.Allocation.Network.NamespacePath = "/host/netns/other" }},
		{"ip mismatch", func(m *SandboxMetadata) { m.Allocation.Network.IP = "172.30.0.250" }},
	}
	for _, drift := range drifts {
		t.Run(drift.name, func(t *testing.T) {
			driver := newTestContainerdRuntime()
			manager := newRecoverNetworkManager(t)
			driver.SetNetworkManager(manager)

			slot := acquireRecoverSlot(t, manager)
			metadata := recoverMetadataFor(slot)
			drift.shift(metadata)

			err := driver.RecoverRuntimeResources(context.Background(), []*SandboxMetadata{metadata}, nil)
			require.ErrorIs(t, err, fastletnetwork.ErrStateInconsistent)
			require.Equal(t, 1, manager.Snapshot().Bound, "a drift failure must not release the slot")
		})
	}
}

func TestRecoverRuntimeResourcesSkipsNilInventoryEntries(t *testing.T) {
	driver := newTestContainerdRuntime()
	manager := newRecoverNetworkManager(t)
	driver.SetNetworkManager(manager)

	slot := acquireRecoverSlot(t, manager)
	metadata := recoverMetadataFor(slot)

	require.NoError(t, driver.RecoverRuntimeResources(context.Background(),
		[]*SandboxMetadata{nil, metadata, nil}, nil))
	require.Equal(t, 1, manager.Snapshot().Bound)
}

func TestRecoverRuntimeResourcesFencesStaleOwnerGeneration(t *testing.T) {
	// The durable network descriptor matches, but the inventory carries a
	// newer instance generation than the slot owner: recovery must forward
	// the full owner identity so Reconcile rejects a stale generation from
	// adopting a live slot.
	driver := newTestContainerdRuntime()
	manager := newRecoverNetworkManager(t)
	driver.SetNetworkManager(manager)

	slot := acquireRecoverSlot(t, manager)
	metadata := recoverMetadataFor(slot)
	metadata.Config.Identity.InstanceGeneration = 2

	err := driver.RecoverRuntimeResources(context.Background(), []*SandboxMetadata{metadata}, nil)
	require.ErrorIs(t, err, fastletnetwork.ErrStateInconsistent)
	require.Equal(t, 1, manager.Snapshot().Bound)
}

func TestRecoverRuntimeResourcesDefaultsMissingIdentityCounters(t *testing.T) {
	// Durable rows persisted before generation/attempt fencing exist carry
	// zero counters; recovery must reconcile them as generation 1 / attempt 1.
	driver := newTestContainerdRuntime()
	manager := newRecoverNetworkManager(t)
	driver.SetNetworkManager(manager)

	slot := acquireRecoverSlot(t, manager)
	metadata := recoverMetadataFor(slot)
	metadata.Config.Identity.InstanceGeneration = 0
	metadata.Config.Identity.AssignmentAttempt = 0

	require.NoError(t, driver.RecoverRuntimeResources(context.Background(), []*SandboxMetadata{metadata}, nil))
	require.Equal(t, 1, manager.Snapshot().Bound)
}
