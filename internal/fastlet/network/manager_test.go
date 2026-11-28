package network

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	runtimecatalog "fast-sandbox/internal/catalog/runtime"
)

type fakeDriver struct {
	mu          sync.Mutex
	prepared    []string
	destroyed   []string
	destroyErr  error
	invalidSlot map[string]error
}

func (d *fakeDriver) Prepare(_ context.Context, slot *Slot) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prepared = append(d.prepared, slot.ID)
	return nil
}

func (d *fakeDriver) Validate(_ context.Context, slot *Slot) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.invalidSlot[slot.ID]
}

func (d *fakeDriver) Destroy(_ context.Context, slot *Slot) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.destroyErr != nil {
		return d.destroyErr
	}
	d.destroyed = append(d.destroyed, slot.ID)
	return nil
}

func newTestManager(t *testing.T, capacity int, root string, driver Driver, ids ...string) *Manager {
	t.Helper()
	index := 0
	config := DefaultConfig(capacity, "pod-uid-1")
	config.StateRoot = root
	config.NetNSRoot = filepath.Join(root, "netns")
	config.HostNetNSRoot = filepath.Join(root, "host-netns")
	config.IDGenerator = func() (string, error) {
		if index < len(ids) {
			id := ids[index]
			index++
			return id, nil
		}
		id := fmt.Sprintf("slot-%d", index+1)
		index++
		return id, nil
	}
	manager, err := NewManager(config, driver, NewFileStateStore(filepath.Join(root, config.PodUID)))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, manager.Stop(context.Background()))
	})
	return manager
}

func owner(id string, attempt int64) Owner {
	return Owner{SandboxUID: id, InstanceGeneration: 1, AssignmentAttempt: attempt}
}

func TestOwnerEqualIgnoresCleanupHint(t *testing.T) {
	legacy := owner("sandbox-a", 1)
	current := legacy
	current.ResidualProcess = runtimecatalog.ResidualProcessFirecracker

	require.True(t, legacy.Equal(current))
	require.True(t, current.Equal(legacy))
}

// guestDataPlaneDriver records ApplyGuest calls. Its ApplyGuest takes the
// manager read lock while "working": if the manager held its write lock
// during the guest data plane, this would deadlock.
type guestDataPlaneDriver struct {
	fakeDriver
	manager *Manager
	applied []string
}

func (d *guestDataPlaneDriver) ApplyGuest(_ context.Context, slot *Slot, guestIP string) error {
	if d.manager != nil {
		_ = d.manager.Snapshot()
	}
	slot.GuestIP = guestIP
	d.applied = append(d.applied, slot.ID)
	return nil
}

func TestManagerApplyGuestRecordsAndPersists(t *testing.T) {
	root := t.TempDir()
	driver := &guestDataPlaneDriver{}
	manager := newTestManager(t, 1, root, driver, "slot-a")
	driver.manager = manager
	require.NoError(t, manager.Initialize(context.Background()))

	owner := owner("sandbox-1", 1)
	_, err := manager.Acquire(context.Background(), owner)
	require.NoError(t, err)
	require.NoError(t, manager.ApplyGuest(context.Background(), owner, "10.17.0.9"))
	require.Equal(t, []string{"slot-a"}, driver.applied)

	slot, exists := manager.Lookup("sandbox-1")
	require.True(t, exists)
	require.Equal(t, "10.17.0.9", slot.GuestIP)

	// The applied address is persisted: a reloaded manager sees it.
	reloaded, err := NewManager(DefaultConfig(1, "pod-uid-1"), &guestDataPlaneDriver{}, NewFileStateStore(filepath.Join(root, "pod-uid-1")))
	require.NoError(t, err)
	require.NoError(t, reloaded.Initialize(context.Background()))
	t.Cleanup(func() {
		require.NoError(t, reloaded.Stop(context.Background()))
	})
	slot, exists = reloaded.Lookup("sandbox-1")
	require.True(t, exists)
	require.Equal(t, "10.17.0.9", slot.GuestIP)
}

func TestManagerApplyGuestDoesNotHoldLockDuringExec(t *testing.T) {
	// The guest data plane fake takes the manager read lock inside its
	// ApplyGuest; with the write lock held this deadlocks (and the timeout
	// fails the test).
	driver := &guestDataPlaneDriver{}
	manager := newTestManager(t, 1, t.TempDir(), driver, "slot-a")
	driver.manager = manager
	require.NoError(t, manager.Initialize(context.Background()))

	owner := owner("sandbox-1", 1)
	_, err := manager.Acquire(context.Background(), owner)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- manager.ApplyGuest(context.Background(), owner, "10.17.0.9") }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("ApplyGuest held the manager write lock while executing the guest data plane")
	}
}

func TestManagerApplyGuestRequiresBoundSlot(t *testing.T) {
	root := t.TempDir()
	driver := &guestDataPlaneDriver{}
	manager := newTestManager(t, 1, root, driver, "slot-a")
	driver.manager = manager
	require.NoError(t, manager.Initialize(context.Background()))

	// No bound slot for the owner.
	require.ErrorIs(t, manager.ApplyGuest(context.Background(), owner("nobody", 1), "10.17.0.9"), ErrSlotNotFound)

	// A released slot is no longer applicable.
	bound := owner("sandbox-1", 1)
	_, err := manager.Acquire(context.Background(), bound)
	require.NoError(t, err)
	require.NoError(t, manager.Release(context.Background(), bound))
	require.ErrorIs(t, manager.ApplyGuest(context.Background(), bound, "10.17.0.9"), ErrSlotNotFound)
	require.Empty(t, driver.applied)
}

func TestManagerCloseDestroysRemainingSlots(t *testing.T) {
	root := t.TempDir()
	driver := &fakeDriver{}
	manager := newTestManager(t, 2, root, driver, "slot-a", "slot-b")
	require.NoError(t, manager.Initialize(context.Background()))
	require.Equal(t, 2, manager.Snapshot().Clean)

	// A bound slot is destroyed too.
	_, err := manager.Acquire(context.Background(), owner("sandbox-1", 1))
	require.NoError(t, err)
	require.NoError(t, manager.Close(context.Background()))
	require.Equal(t, 0, manager.Snapshot().Clean)
	require.Equal(t, 0, manager.Snapshot().Bound)
	require.ElementsMatch(t, []string{"slot-a", "slot-b"}, driver.destroyed)

	// Idempotent.
	require.NoError(t, manager.Close(context.Background()))

	// No further slot preparation happens after Close: a Release (which
	// would normally replenish) must not recreate resources.
	require.NoError(t, manager.Replenish(context.Background()))
	require.Equal(t, 0, manager.Snapshot().Clean)
}

func TestManagerCloseRacingPreparationDiscardsSlot(t *testing.T) {
	root := t.TempDir()
	driver := &fakeDriver{}
	manager := newTestManager(t, 1, root, driver, "slot-a")
	require.NoError(t, manager.Initialize(context.Background()))
	require.Equal(t, []string{"slot-a"}, driver.prepared)

	// Close destroys the prepared slot and is idempotent.
	require.NoError(t, manager.Close(context.Background()))
	require.Equal(t, 0, manager.Snapshot().Clean)
	require.Equal(t, []string{"slot-a"}, driver.destroyed)

	// Replenish after Close must not prepare any further slot.
	require.NoError(t, manager.Replenish(context.Background()))
	require.Equal(t, []string{"slot-a"}, driver.prepared)
	require.Equal(t, 0, manager.Snapshot().Clean)
}

func TestManagerAcquireReleaseDestroysUsedSlot(t *testing.T) {
	root := t.TempDir()
	driver := &fakeDriver{}
	manager := newTestManager(t, 2, root, driver, "slot-a", "slot-b", "slot-c")
	require.NoError(t, manager.Initialize(context.Background()))
	require.Equal(t, 2, manager.Snapshot().Clean)

	first, err := manager.Acquire(context.Background(), owner("sandbox-a", 1))
	require.NoError(t, err)
	second, err := manager.Acquire(context.Background(), owner("sandbox-b", 1))
	require.NoError(t, err)
	require.NotEqual(t, first.IP, second.IP)
	_, err = manager.Acquire(context.Background(), owner("sandbox-c", 1))
	require.ErrorIs(t, err, ErrNoCleanSlot)
	require.Equal(t, uint64(2), manager.Snapshot().Hit)
	require.Equal(t, uint64(1), manager.Snapshot().Miss)

	require.NoError(t, manager.Release(context.Background(), owner("sandbox-a", 1)))
	require.NoError(t, manager.Replenish(context.Background()))
	require.Eventually(t, func() bool { return manager.Snapshot().Clean == 1 }, testEventuallyTimeout, testEventuallyInterval)
	replacement, ok := manager.Lookup("sandbox-a")
	require.False(t, ok)
	require.Nil(t, replacement)
	driver.mu.Lock()
	require.Contains(t, driver.destroyed, first.ID)
	require.NotContains(t, driver.prepared[2:], first.ID)
	driver.mu.Unlock()
}

func TestManagerAcquireIsIdentityIdempotentAndFenced(t *testing.T) {
	manager := newTestManager(t, 1, t.TempDir(), &fakeDriver{}, "slot-a")
	require.NoError(t, manager.Initialize(context.Background()))
	first, err := manager.Acquire(context.Background(), owner("sandbox-a", 1))
	require.NoError(t, err)
	second, err := manager.Acquire(context.Background(), owner("sandbox-a", 1))
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)
	_, err = manager.Acquire(context.Background(), owner("sandbox-a", 2))
	require.ErrorIs(t, err, ErrOwnerConflict)
}

func TestManagerConcurrentAcquireNeverOvercommits(t *testing.T) {
	const capacity = 8
	manager := newTestManager(t, capacity, t.TempDir(), &fakeDriver{})
	require.NoError(t, manager.Initialize(context.Background()))

	var group sync.WaitGroup
	results := make(chan *Slot, capacity*2)
	for index := 0; index < capacity*2; index++ {
		index := index
		group.Add(1)
		go func() {
			defer group.Done()
			slot, err := manager.Acquire(context.Background(), owner(fmt.Sprintf("sandbox-%d", index), 1))
			if err == nil {
				results <- slot
			}
		}()
	}
	group.Wait()
	close(results)
	ids := map[string]struct{}{}
	for slot := range results {
		ids[slot.ID] = struct{}{}
	}
	require.Len(t, ids, capacity)
	require.Equal(t, capacity, manager.Snapshot().Bound)
}

func TestManagerRecoversStateAndDestroysRuntimeOrphan(t *testing.T) {
	root := t.TempDir()
	firstDriver := &fakeDriver{}
	first := newTestManager(t, 2, root, firstDriver, "slot-a", "slot-b")
	require.NoError(t, first.Initialize(context.Background()))
	bound, err := first.Acquire(context.Background(), owner("sandbox-a", 1))
	require.NoError(t, err)

	secondDriver := &fakeDriver{}
	second := newTestManager(t, 2, root, secondDriver, "slot-c")
	require.NoError(t, second.Initialize(context.Background()))
	recovered, ok := second.Lookup("sandbox-a")
	require.True(t, ok)
	require.Equal(t, bound.ID, recovered.ID)
	require.NoError(t, second.Reconcile(context.Background(), nil))
	require.Eventually(t, func() bool { return second.Snapshot().Clean == 2 }, testEventuallyTimeout, testEventuallyInterval)
	secondDriver.mu.Lock()
	require.Contains(t, secondDriver.destroyed, bound.ID)
	secondDriver.mu.Unlock()
}

func TestManagerRetriesInterruptedDestroyOnInitialize(t *testing.T) {
	root := t.TempDir()
	driver := &fakeDriver{destroyErr: errors.New("busy")}
	first := newTestManager(t, 1, root, driver, "slot-a")
	require.NoError(t, first.Initialize(context.Background()))
	require.NoError(t, func() error {
		_, err := first.Acquire(context.Background(), owner("sandbox-a", 1))
		return err
	}())
	require.Error(t, first.Release(context.Background(), owner("sandbox-a", 1)))
	require.Equal(t, 1, first.Snapshot().Destroying)

	driver.destroyErr = nil
	second := newTestManager(t, 1, root, driver, "slot-b")
	require.NoError(t, second.Initialize(context.Background()))
	require.Equal(t, 1, second.Snapshot().Clean)
}

func TestManagerInvalidRecoveredSlotIdentity(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(fmt.Sprintf("bound=%t", bound), func(t *testing.T) {
			root := t.TempDir()
			first := newTestManager(t, 1, root, &fakeDriver{}, "old-tap")
			require.NoError(t, first.Initialize(context.Background()))
			if bound {
				_, err := first.Acquire(context.Background(), owner("sandbox-a", 1))
				require.NoError(t, err)
			}
			driver := &fakeDriver{invalidSlot: map[string]error{"old-tap": errors.New("gateway MAC mismatch")}}
			reloaded := newTestManager(t, 1, root, driver, "fixed-tap")
			err := reloaded.Initialize(context.Background())
			if bound {
				require.ErrorIs(t, err, ErrStateInconsistent)
				require.Empty(t, driver.destroyed)
				require.Empty(t, driver.prepared)
				return
			}
			require.NoError(t, err)
			require.Equal(t, []string{"old-tap"}, driver.destroyed)
			slot, err := reloaded.Acquire(context.Background(), owner("sandbox-a", 1))
			require.NoError(t, err)
			require.Equal(t, "fixed-tap", slot.ID)
		})
	}
}

const (
	testEventuallyTimeout  = 2e9
	testEventuallyInterval = 10e6
)

type burstChaosDriver struct {
	mu                sync.Mutex
	enableChaos       atomic.Bool
	attempts          atomic.Int32
	inFlight          atomic.Int32
	maxInFlight       atomic.Int32
	destroyByID       map[string]int
	prepareDelay      time.Duration
	failFastCount     int32
	stallTimeoutTo    int32
	allowFirstFailure <-chan struct{}
}

func (d *burstChaosDriver) Prepare(ctx context.Context, slot *Slot) error {
	if !d.enableChaos.Load() {
		return nil
	}
	current := d.inFlight.Add(1)
	defer d.inFlight.Add(-1)
	for {
		observed := d.maxInFlight.Load()
		if current <= observed || d.maxInFlight.CompareAndSwap(observed, current) {
			break
		}
	}

	attempt := d.attempts.Add(1)
	if attempt == 1 {
		<-d.allowFirstFailure
	}
	if attempt <= d.failFastCount {
		return errors.New("transient netlink busy")
	}
	if attempt <= d.stallTimeoutTo {
		<-ctx.Done()
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d.prepareDelay):
	}
	return nil
}

func (d *burstChaosDriver) Validate(_ context.Context, _ *Slot) error { return nil }

func (d *burstChaosDriver) Destroy(_ context.Context, slot *Slot) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.destroyByID == nil {
		d.destroyByID = make(map[string]int)
	}
	d.destroyByID[slot.ID]++
	return nil
}

func gatherGaugeValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			if len(metric.Label) != len(labels) {
				continue
			}
			matched := true
			for _, pair := range metric.Label {
				if labels[pair.GetName()] != pair.GetValue() {
					matched = false
					break
				}
			}
			if matched && metric.Gauge != nil {
				return metric.Gauge.GetValue()
			}
		}
	}
	t.Fatalf("gauge %s with labels %v not found", name, labels)
	return 0
}

func TestManagerBurstReleaseConvergesUnderSlowAndTransientFailingPrepare(t *testing.T) {
	const capacity = 50
	root := t.TempDir()
	allowFirstFailure := make(chan struct{})
	allowFailure := sync.OnceFunc(func() { close(allowFirstFailure) })
	defer allowFailure()
	driver := &burstChaosDriver{
		prepareDelay:      8 * time.Millisecond,
		failFastCount:     3,
		stallTimeoutTo:    6,
		allowFirstFailure: allowFirstFailure,
	}
	var nextID atomic.Int64
	config := DefaultConfig(capacity, "pod-burst")
	config.PrivateCIDR = "172.30.0.0/20"
	config.StateRoot = root
	config.NetNSRoot = filepath.Join(root, "netns")
	config.HostNetNSRoot = filepath.Join(root, "host-netns")
	config.ReplenishTimeout = 200 * time.Millisecond
	config.ReplenishBaseBackoff = 5 * time.Millisecond
	config.ReplenishMaxBackoff = 20 * time.Millisecond
	config.IDGenerator = func() (string, error) {
		return fmt.Sprintf("slot-%d", nextID.Add(1)), nil
	}
	manager, err := NewManager(config, driver, NewFileStateStore(filepath.Join(root, config.PodUID)))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, manager.Stop(context.Background()))
	})
	require.NoError(t, manager.Initialize(context.Background()))
	require.Equal(t, capacity, manager.Snapshot().Clean)

	owners := make([]Owner, capacity)
	initialIDs := make(map[string]struct{}, capacity)
	for i := range capacity {
		owners[i] = owner(fmt.Sprintf("sandbox-%d", i), 1)
		slot, acquireErr := manager.Acquire(context.Background(), owners[i])
		require.NoError(t, acquireErr)
		initialIDs[slot.ID] = struct{}{}
	}
	require.Zero(t, manager.Snapshot().Clean)
	require.Equal(t, capacity, manager.Snapshot().Bound)

	driver.enableChaos.Store(true)

	var releaseWG sync.WaitGroup
	releaseResults := make(chan error, capacity)
	for i := range capacity {
		releaseOwner := owners[i]
		releaseWG.Add(1)
		go func() {
			defer releaseWG.Done()
			releaseResults <- manager.Release(context.Background(), releaseOwner)
		}()
	}
	releaseWG.Wait()
	close(releaseResults)
	for releaseErr := range releaseResults {
		require.NoError(t, releaseErr)
	}
	// No Release or Replenish call follows the first preparation failure.
	allowFailure()
	require.Eventually(t, func() bool {
		return manager.Snapshot().Clean == capacity
	}, 10*time.Second, testEventuallyInterval)

	snap := manager.Snapshot()
	require.Equal(t, capacity, snap.Clean)
	require.Zero(t, snap.Bound)
	require.Zero(t, snap.Destroying)
	require.Equal(t, int32(1), driver.maxInFlight.Load())
	require.Equal(t, int32(capacity)+driver.stallTimeoutTo, driver.attempts.Load())

	require.InDelta(t, float64(capacity), gatherGaugeValue(t, "fastlet_network_slots", map[string]string{"phase": "clean"}), 0)
	require.InDelta(t, 0, gatherGaugeValue(t, "fastlet_network_slots", map[string]string{"phase": "bound"}), 0)
	require.InDelta(t, 0, gatherGaugeValue(t, "fastlet_network_slots", map[string]string{"phase": "destroying"}), 0)
	require.InDelta(t, float64(capacity), gatherGaugeValue(t, "fast_sandbox_network_slot_available", nil), 0)
	require.InDelta(t, 0, gatherGaugeValue(t, "fast_sandbox_network_slot_inuse", nil), 0)

	driver.mu.Lock()
	for id, count := range driver.destroyByID {
		if _, wasInitial := initialIDs[id]; !wasInitial {
			require.Equal(t, 1, count, "failed preparation of %s must tear down once", id)
		}
	}
	driver.mu.Unlock()
}

type blockingPrepareDriver struct {
	fakeDriver
	blockNext atomic.Bool
	entered   chan struct{}
	unblock   chan struct{}
}

func (d *blockingPrepareDriver) Prepare(ctx context.Context, slot *Slot) error {
	if d.blockNext.CompareAndSwap(true, false) {
		close(d.entered)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-d.unblock:
		}
	}
	return d.fakeDriver.Prepare(ctx, slot)
}

func TestManagerSyncReplenishWaitsForInFlightBackgroundPrepare(t *testing.T) {
	driver := &blockingPrepareDriver{
		entered: make(chan struct{}),
		unblock: make(chan struct{}),
	}
	manager := newTestManager(t, 1, t.TempDir(), driver, "slot-a", "slot-b")
	require.NoError(t, manager.Initialize(context.Background()))

	bound := owner("sandbox-sync", 1)
	_, err := manager.Acquire(context.Background(), bound)
	require.NoError(t, err)

	driver.blockNext.Store(true)
	require.NoError(t, manager.Release(context.Background(), bound))
	<-driver.entered
	require.Zero(t, manager.Snapshot().Clean)
	require.Zero(t, manager.Snapshot().Destroying, "in-flight preparation must not report as Destroying")
	waitCtx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, manager.Replenish(waitCtx), context.Canceled)

	replenishDone := make(chan error, 1)
	go func() {
		replenishDone <- manager.Replenish(context.Background())
	}()
	select {
	case prematureErr := <-replenishDone:
		t.Fatalf("Replenish returned %v before in-flight slot finished preparing", prematureErr)
	case <-time.After(30 * time.Millisecond):
	}

	close(driver.unblock)
	require.NoError(t, <-replenishDone)
	require.Equal(t, 1, manager.Snapshot().Clean)
}

func TestManagerCloseCancelsInFlightPrepare(t *testing.T) {
	driver := &blockingPrepareDriver{
		entered: make(chan struct{}),
		unblock: make(chan struct{}),
	}
	manager := newTestManager(t, 1, t.TempDir(), driver, "slot-a", "slot-b")
	require.NoError(t, manager.Initialize(context.Background()))

	bound := owner("sandbox-close", 1)
	_, err := manager.Acquire(context.Background(), bound)
	require.NoError(t, err)

	driver.blockNext.Store(true)
	require.NoError(t, manager.Release(context.Background(), bound))
	<-driver.entered

	started := time.Now()
	require.NoError(t, manager.Close(context.Background()))
	require.Less(t, time.Since(started), time.Second)
	require.Zero(t, manager.Snapshot().Clean)
	require.Zero(t, manager.Snapshot().Bound)
	require.Zero(t, manager.Snapshot().Destroying)
}

type failedPreparationDriver struct {
	fakeDriver
	allowCleanup    chan struct{}
	cleanupAttempts atomic.Int32
}

func (d *failedPreparationDriver) Prepare(ctx context.Context, slot *Slot) error {
	if slot.ID == "slot-b" {
		return errors.New("partial preparation failed")
	}
	return d.fakeDriver.Prepare(ctx, slot)
}

func (d *failedPreparationDriver) Destroy(ctx context.Context, slot *Slot) error {
	if slot.ID == "slot-b" {
		d.cleanupAttempts.Add(1)
		select {
		case <-d.allowCleanup:
		default:
			return errors.New("failed preparation still owns network resources")
		}
	}
	return d.fakeDriver.Destroy(ctx, slot)
}

func TestManagerRetriesCleanupBeforePreparingAnotherSlot(t *testing.T) {
	driver := &failedPreparationDriver{allowCleanup: make(chan struct{})}
	manager := newTestManager(t, 1, t.TempDir(), driver, "slot-a", "slot-b", "slot-c")
	ctx := t.Context()
	require.NoError(t, manager.Initialize(ctx))
	bound := owner("sandbox-cleanup", 1)
	_, err := manager.Acquire(ctx, bound)
	require.NoError(t, err)
	require.NoError(t, manager.Release(ctx, bound))

	require.Eventually(t, func() bool {
		return driver.cleanupAttempts.Load() >= 2
	}, testEventuallyTimeout, testEventuallyInterval)
	require.Zero(t, manager.Snapshot().Clean)
	durable, err := manager.store.LoadAll(ctx)
	require.NoError(t, err)
	require.Len(t, durable, 1)
	require.Equal(t, "slot-b", durable[0].ID)
	require.Equal(t, SlotPhaseDestroying, durable[0].Phase)
	waitCtx, cancel := context.WithTimeout(ctx, testEventuallyTimeout)
	defer cancel()
	require.ErrorContains(t, manager.Replenish(waitCtx), "failed preparation still owns network resources")

	close(driver.allowCleanup)
	require.Eventually(t, func() bool {
		return manager.Snapshot().Clean == 1
	}, testEventuallyTimeout, testEventuallyInterval)
	durable, err = manager.store.LoadAll(ctx)
	require.NoError(t, err)
	require.Len(t, durable, 1)
	require.Equal(t, "slot-c", durable[0].ID)
	require.Equal(t, SlotPhaseClean, durable[0].Phase)
}

func TestManagerCloseCancelsReplenishBackoff(t *testing.T) {
	driver := &failedPreparationDriver{allowCleanup: make(chan struct{})}
	manager := newTestManager(t, 1, t.TempDir(), driver, "slot-a", "slot-b", "slot-c")
	manager.config.ReplenishBaseBackoff = time.Minute
	manager.config.ReplenishMaxBackoff = time.Minute
	ctx := t.Context()
	require.NoError(t, manager.Initialize(ctx))
	bound := owner("sandbox-backoff", 1)
	_, err := manager.Acquire(ctx, bound)
	require.NoError(t, err)
	require.NoError(t, manager.Release(ctx, bound))
	require.Eventually(t, func() bool {
		manager.mu.RLock()
		defer manager.mu.RUnlock()
		return manager.replenishFailureCount > 0
	}, testEventuallyTimeout, testEventuallyInterval)

	close(driver.allowCleanup)
	closeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	require.NoError(t, manager.Close(closeCtx))
	require.Zero(t, manager.Snapshot().Clean)
	require.Equal(t, []string{"slot-a"}, driver.prepared)
}

func TestManagerCanceledContextSkipsSlotAllocation(t *testing.T) {
	driver := &fakeDriver{}
	manager := newTestManager(t, 1, t.TempDir(), driver, "slot-never")
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, manager.Initialize(canceledCtx), context.Canceled)
	require.Empty(t, driver.prepared)
	require.Empty(t, driver.destroyed)
}
