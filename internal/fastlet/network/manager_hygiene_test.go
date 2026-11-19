package network

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeHygienistDriver extends the shared fakeDriver with the hygiene seam.
// DeepReset can block so tests observe that Returning capacity is not
// acquirable and that a concurrent release cannot double-reset.
type fakeHygienistDriver struct {
	fakeDriver
	mu              sync.Mutex
	goldenCaptures  int
	resetCalls      []string
	auditCalls      []string
	resetErr        error
	auditViolations []Violation
	resetDelay      time.Duration
	releaseGate     chan struct{}
	releaseGateOnce sync.Once
}

func (d *fakeHygienistDriver) CaptureGolden(_ context.Context, _ *Slot) (*GoldenState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.goldenCaptures++
	return &GoldenState{
		Links:     []GoldenLink{{Name: "eth0", Type: "ether", AdminUp: true, MTU: 1500, MAC: "6a:00:00:00:00:01"}},
		Addresses: []GoldenAddress{{Link: "eth0", Family: "inet", Address: "172.30.0.2/24"}},
		Sysctls:   map[string]string{"net.ipv4.ip_forward": "1"},
	}, nil
}

func (d *fakeHygienistDriver) DeepReset(ctx context.Context, slot *Slot, golden *GoldenState) error {
	d.mu.Lock()
	d.resetCalls = append(d.resetCalls, slot.ID)
	gate := d.releaseGate
	d.mu.Unlock()
	if d.resetDelay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.resetDelay):
		}
	}
	if gate != nil {
		<-gate
		d.releaseGateOnce.Do(func() {})
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.resetErr
}

func (d *fakeHygienistDriver) Audit(_ context.Context, slot *Slot, _ *GoldenState) ([]Violation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.auditCalls = append(d.auditCalls, slot.ID)
	return d.auditViolations, nil
}

func (d *fakeHygienistDriver) stats() (int, []string, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.goldenCaptures, append([]string(nil), d.resetCalls...), append([]string(nil), d.auditCalls...)
}

func newCleanReturnTestManager(t *testing.T, capacity int, root string, driver Driver, ids ...string) *Manager {
	t.Helper()
	index := 0
	config := DefaultConfig(capacity, "pod-uid-1")
	config.CleanReturn = true
	config.ResetTimeout = 2 * time.Second
	config.StateRoot = root
	config.NetNSRoot = filepath.Join(root, "netns")
	config.HostNetNSRoot = filepath.Join(root, "host-netns")
	config.IDGenerator = func() (string, error) {
		if index < len(ids) {
			id := ids[index]
			index++
			return id, nil
		}
		id := "slot-x" + string(rune('a'+index))
		index++
		return id, nil
	}
	manager, err := NewManager(config, driver, NewFileStateStore(filepath.Join(root, config.PodUID)))
	require.NoError(t, err)
	t.Cleanup(manager.Quiesce)
	return manager
}

func TestManagerCleanReturnReusesSlotIdentity(t *testing.T) {
	root := t.TempDir()
	driver := &fakeHygienistDriver{}
	manager := newCleanReturnTestManager(t, 1, root, driver, "slot-a")
	require.NoError(t, manager.Initialize(context.Background()))

	first, err := manager.Acquire(context.Background(), owner("sandbox-a", 1))
	require.NoError(t, err)
	require.NoError(t, manager.Release(context.Background(), owner("sandbox-a", 1)))

	require.Equal(t, 1, manager.Snapshot().Clean)
	require.Equal(t, 0, manager.Snapshot().Returning)
	require.Equal(t, 0, manager.Snapshot().Destroying)

	// The slot is reused with its original identity; no destroy, no
	// asynchronous replenish.
	_, resets, audits := driver.stats()
	require.Equal(t, []string{"slot-a"}, resets)
	require.Equal(t, []string{"slot-a"}, audits)
	require.Empty(t, driver.destroyed)

	second, err := manager.Acquire(context.Background(), owner("sandbox-b", 1))
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, first.IP, second.IP)
	require.Equal(t, first.NetNSName, second.NetNSName)

	// The released slot no longer belongs to the previous owner.
	_, exists := manager.Lookup("sandbox-a")
	require.False(t, exists)
}

func TestManagerCleanReturnQuarantinesOnResetFailure(t *testing.T) {
	root := t.TempDir()
	driver := &fakeHygienistDriver{resetErr: errors.New("reset shell failed")}
	manager := newCleanReturnTestManager(t, 1, root, driver, "slot-a")
	require.NoError(t, manager.Initialize(context.Background()))

	_, err := manager.Acquire(context.Background(), owner("sandbox-a", 1))
	require.NoError(t, err)
	require.NoError(t, manager.Release(context.Background(), owner("sandbox-a", 1)))

	// Quarantine destroys the suspect slot and rebuilds capacity from
	// scratch; Release itself still succeeds. The replacement preparation is
	// asynchronous, so only the settled state is asserted.
	require.Equal(t, []string{"slot-a"}, driver.destroyed)
	require.Eventually(t, func() bool { return manager.Snapshot().Clean == 1 }, testEventuallyTimeout, testEventuallyInterval)
	require.Equal(t, 0, manager.Snapshot().Destroying)
	replacement, err := manager.Acquire(context.Background(), owner("sandbox-b", 1))
	require.NoError(t, err)
	require.NotEqual(t, "slot-a", replacement.ID)
}

func TestManagerCleanReturnQuarantinesOnAuditViolation(t *testing.T) {
	root := t.TempDir()
	driver := &fakeHygienistDriver{auditViolations: []Violation{
		{Domain: "sysctl", Detail: "net.ipv4.ip_forward=0, want 1"},
		{Domain: "foreign", Detail: "resident process pid 4321"},
	}}
	manager := newCleanReturnTestManager(t, 1, root, driver, "slot-a")
	require.NoError(t, manager.Initialize(context.Background()))

	_, err := manager.Acquire(context.Background(), owner("sandbox-a", 1))
	require.NoError(t, err)
	require.NoError(t, manager.Release(context.Background(), owner("sandbox-a", 1)))
	require.Contains(t, driver.destroyed, "slot-a")
	require.Eventually(t, func() bool { return manager.Snapshot().Clean == 1 }, testEventuallyTimeout, testEventuallyInterval)
}

func TestManagerCleanReturnQuarantinesOnResetTimeout(t *testing.T) {
	root := t.TempDir()
	driver := &fakeHygienistDriver{resetDelay: 500 * time.Millisecond}
	manager := newCleanReturnTestManager(t, 1, root, driver, "slot-a")
	manager.config.ResetTimeout = 50 * time.Millisecond
	require.NoError(t, manager.Initialize(context.Background()))

	_, err := manager.Acquire(context.Background(), owner("sandbox-a", 1))
	require.NoError(t, err)
	require.NoError(t, manager.Release(context.Background(), owner("sandbox-a", 1)))
	require.Contains(t, driver.destroyed, "slot-a")
}

func TestManagerCleanReturnWithoutGoldenFallsBackToDestroy(t *testing.T) {
	root := t.TempDir()
	// A bound slot recovered from durable state written before this feature
	// has no golden state, and its namespace still holds tenant residue, so
	// it must be destroyed rather than returned to the pool.
	manager := newCleanReturnTestManager(t, 1, root, &fakeHygienistDriver{}, "slot-a")
	require.NoError(t, manager.Initialize(context.Background()))
	_, err := manager.Acquire(context.Background(), owner("sandbox-a", 1))
	require.NoError(t, err)

	slotPath := filepath.Join(root, "pod-uid-1", "slot-a.json")
	raw, err := os.ReadFile(slotPath)
	require.NoError(t, err)
	var stored Slot
	require.NoError(t, json.Unmarshal(raw, &stored))
	require.Equal(t, SlotPhaseBound, stored.Phase)
	stored.Golden = nil
	updated, err := json.Marshal(&stored)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(slotPath, updated, 0o600))

	driver := &fakeHygienistDriver{}
	reloaded := newCleanReturnTestManager(t, 1, root, driver, "slot-b")
	require.NoError(t, reloaded.Initialize(context.Background()))
	require.NoError(t, reloaded.Release(context.Background(), owner("sandbox-a", 1)))
	require.Contains(t, driver.destroyed, "slot-a")
}

func TestManagerCleanReturnRegoldensRecoveredCleanSlot(t *testing.T) {
	root := t.TempDir()
	// A clean slot from durable state written before this feature has not
	// been handed to a tenant, so its current state is the pool-build state
	// and is captured on load.
	manager := newCleanReturnTestManager(t, 1, root, &fakeHygienistDriver{}, "slot-a")
	require.NoError(t, manager.Initialize(context.Background()))
	slotPath := filepath.Join(root, "pod-uid-1", "slot-a.json")
	raw, err := os.ReadFile(slotPath)
	require.NoError(t, err)
	var stored Slot
	require.NoError(t, json.Unmarshal(raw, &stored))
	stored.Golden = nil
	updated, err := json.Marshal(&stored)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(slotPath, updated, 0o600))

	driver := &fakeHygienistDriver{}
	reloaded := newCleanReturnTestManager(t, 1, root, driver, "slot-b")
	require.NoError(t, reloaded.Initialize(context.Background()))
	captures, _, _ := driver.stats()
	require.Equal(t, 1, captures)

	_, err = reloaded.Acquire(context.Background(), owner("sandbox-a", 1))
	require.NoError(t, err)
	require.NoError(t, reloaded.Release(context.Background(), owner("sandbox-a", 1)))
	_, resets, _ := driver.stats()
	require.Equal(t, []string{"slot-a"}, resets)
	require.Empty(t, driver.destroyed)
}

func TestManagerCleanReturnCapturesAndPersistsGolden(t *testing.T) {
	root := t.TempDir()
	driver := &fakeHygienistDriver{}
	manager := newCleanReturnTestManager(t, 1, root, driver, "slot-a")
	require.NoError(t, manager.Initialize(context.Background()))
	captures, _, _ := driver.stats()
	require.Equal(t, 1, captures)

	raw, err := os.ReadFile(filepath.Join(root, "pod-uid-1", "slot-a.json"))
	require.NoError(t, err)
	var stored Slot
	require.NoError(t, json.Unmarshal(raw, &stored))
	require.NotNil(t, stored.Golden)
	require.Equal(t, "eth0", stored.Golden.Links[0].Name)
	require.Equal(t, "1", stored.Golden.Sysctls["net.ipv4.ip_forward"])
}

func TestManagerCleanReturnIsDisabledWithoutConfiguration(t *testing.T) {
	root := t.TempDir()
	driver := &fakeHygienistDriver{}
	manager := newTestManager(t, 1, root, driver, "slot-a")
	require.NoError(t, manager.Initialize(context.Background()))
	_, err := manager.Acquire(context.Background(), owner("sandbox-a", 1))
	require.NoError(t, err)
	require.NoError(t, manager.Release(context.Background(), owner("sandbox-a", 1)))
	// The destroy-per-slot contract is unchanged when pooling is disabled.
	require.Contains(t, driver.destroyed, "slot-a")
	_, _, _ = driver.stats()
}

func TestNewManagerCleanReturnRequiresHygienistDriver(t *testing.T) {
	config := DefaultConfig(1, "pod-uid-1")
	config.CleanReturn = true
	_, err := NewManager(config, &fakeDriver{}, NewFileStateStore(t.TempDir()))
	require.ErrorContains(t, err, "residue audit")
}

func TestManagerRecoversReturningSlotByDestroying(t *testing.T) {
	root := t.TempDir()
	driver := &fakeHygienistDriver{}
	manager := newCleanReturnTestManager(t, 1, root, driver, "slot-a")
	require.NoError(t, manager.Initialize(context.Background()))
	_, err := manager.Acquire(context.Background(), owner("sandbox-a", 1))
	require.NoError(t, err)

	// Simulate a crash mid-return by rewriting the durable phase.
	slotPath := filepath.Join(root, "pod-uid-1", "slot-a.json")
	raw, err := os.ReadFile(slotPath)
	require.NoError(t, err)
	var stored Slot
	require.NoError(t, json.Unmarshal(raw, &stored))
	stored.Phase = SlotPhaseReturning
	updated, err := json.Marshal(&stored)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(slotPath, updated, 0o600))

	reloaded := newCleanReturnTestManager(t, 1, root, &fakeHygienistDriver{}, "slot-b")
	require.NoError(t, reloaded.Initialize(context.Background()))
	// The interrupted return is destroyed, not trusted.
	require.Contains(t, reloaded.driver.(*fakeHygienistDriver).destroyed, "slot-a")
	require.Equal(t, 1, reloaded.Snapshot().Clean)
}

func TestManagerCleanReturnReleaseWhileReturningIsIdempotent(t *testing.T) {
	root := t.TempDir()
	driver := &fakeHygienistDriver{releaseGate: make(chan struct{})}
	manager := newCleanReturnTestManager(t, 1, root, driver, "slot-a")
	require.NoError(t, manager.Initialize(context.Background()))
	_, err := manager.Acquire(context.Background(), owner("sandbox-a", 1))
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- manager.Release(context.Background(), owner("sandbox-a", 1)) }()
	require.Eventually(t, func() bool {
		_, resets, _ := driver.stats()
		return len(resets) == 1
	}, testEventuallyTimeout, testEventuallyInterval)
	// While the reset is in flight the slot capacity is reserved.
	_, err = manager.Acquire(context.Background(), owner("sandbox-b", 1))
	require.ErrorIs(t, err, ErrNoCleanSlot)
	// A concurrent release for the same binding neither errors nor
	// double-resets.
	require.NoError(t, manager.Release(context.Background(), owner("sandbox-a", 1)))
	_, resets, audits := driver.stats()
	require.Len(t, resets, 1)
	require.Empty(t, audits)

	close(driver.releaseGate)
	require.NoError(t, <-done)
	require.Eventually(t, func() bool { return manager.Snapshot().Clean == 1 }, testEventuallyTimeout, testEventuallyInterval)
	_, resets, audits = driver.stats()
	require.Len(t, resets, 1)
	require.Len(t, audits, 1)
	require.Empty(t, driver.destroyed)
}

func TestManagerCleanReturnCommitAfterCloseIsDropped(t *testing.T) {
	root := t.TempDir()
	driver := &fakeHygienistDriver{releaseGate: make(chan struct{})}
	manager := newCleanReturnTestManager(t, 1, root, driver, "slot-a")
	require.NoError(t, manager.Initialize(context.Background()))
	_, err := manager.Acquire(context.Background(), owner("sandbox-a", 1))
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- manager.Release(context.Background(), owner("sandbox-a", 1)) }()
	require.Eventually(t, func() bool {
		_, resets, _ := driver.stats()
		return len(resets) == 1
	}, testEventuallyTimeout, testEventuallyInterval)
	require.NoError(t, manager.Close(context.Background()))
	close(driver.releaseGate)
	// The commit observes the destroyed slot and does not resurrect it.
	require.NoError(t, <-done)
	require.Equal(t, 0, manager.Snapshot().Clean)
	require.NoError(t, manager.Replenish(context.Background()))
	require.Equal(t, 0, manager.Snapshot().Clean)
}
