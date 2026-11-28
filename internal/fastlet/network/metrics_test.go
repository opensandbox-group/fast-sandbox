package network

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	fsbtest "fast-sandbox/internal/testutil"
)

func TestGuestApplyRecordsDriverFailureWithoutPersisting(t *testing.T) {
	driver := &failingGuestApplyDriver{}
	manager := newTestManager(t, 1, t.TempDir(), driver, "slot-a")
	require.NoError(t, manager.Initialize(context.Background()))
	bound := owner("sandbox-guest-metrics", 1)
	_, err := manager.Acquire(context.Background(), bound)
	require.NoError(t, err)
	failureLabels := map[string]string{"stage": "driver_apply", "result": "error"}
	guestApplyStageLatency.WithLabelValues("driver_apply", "error")
	before, err := fsbtest.HistogramSampleCount("fast_sandbox_network_guest_apply_stage_latency_seconds", failureLabels)
	require.NoError(t, err)
	require.ErrorContains(t, manager.ApplyGuest(context.Background(), bound, "10.17.0.9"), "guest apply failed")
	after, err := fsbtest.HistogramSampleCount("fast_sandbox_network_guest_apply_stage_latency_seconds", failureLabels)
	require.NoError(t, err)
	require.Equal(t, before+1, after)
	slot, ok := manager.Lookup(bound.SandboxUID)
	require.True(t, ok)
	require.Empty(t, slot.GuestIP, "failed driver work must not replace the durable slot")
}

type failingGuestApplyDriver struct{ fakeDriver }

func (*failingGuestApplyDriver) ApplyGuest(context.Context, *Slot, string) error {
	return errors.New("guest apply failed")
}

func TestNetworkLatencyMetricsAreCollectable(t *testing.T) {
	acquireLabels := map[string]string{"result": "bound"}
	persistLabels := map[string]string{"result": "success"}
	prepareSuccessLabels := map[string]string{"result": "success"}
	prepareErrorLabels := map[string]string{"result": "error"}
	acquireBefore, _ := fsbtest.HistogramSampleCount("fast_sandbox_network_slot_acquire_latency_seconds", acquireLabels)
	persistBefore, _ := fsbtest.HistogramSampleCount("fast_sandbox_network_slot_persist_latency_seconds", persistLabels)
	prepareSuccessBefore, _ := fsbtest.HistogramSampleCount("fast_sandbox_network_slot_prepare_latency_seconds", prepareSuccessLabels)
	prepareErrorBefore, _ := fsbtest.HistogramSampleCount("fast_sandbox_network_slot_prepare_latency_seconds", prepareErrorLabels)
	observeSlotAcquire("bound", time.Now())
	observeSlotPersist(time.Now(), nil)
	observeSlotPrepare(time.Now(), nil)
	observeSlotPrepare(time.Now(), errors.New("boom"))
	acquireAfter, err := fsbtest.HistogramSampleCount("fast_sandbox_network_slot_acquire_latency_seconds", acquireLabels)
	require.NoError(t, err)
	persistAfter, err := fsbtest.HistogramSampleCount("fast_sandbox_network_slot_persist_latency_seconds", persistLabels)
	require.NoError(t, err)
	prepareSuccessAfter, err := fsbtest.HistogramSampleCount("fast_sandbox_network_slot_prepare_latency_seconds", prepareSuccessLabels)
	require.NoError(t, err)
	prepareErrorAfter, err := fsbtest.HistogramSampleCount("fast_sandbox_network_slot_prepare_latency_seconds", prepareErrorLabels)
	require.NoError(t, err)
	require.Equal(t, acquireBefore+1, acquireAfter)
	require.Equal(t, persistBefore+1, persistAfter)
	require.Equal(t, prepareSuccessBefore+1, prepareSuccessAfter)
	require.Equal(t, prepareErrorBefore+1, prepareErrorAfter)
}
