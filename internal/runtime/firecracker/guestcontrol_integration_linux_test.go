package firecracker

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/guestcontrol"
)

// TestGuestControlPlainResume verifies device continuity without creating a snapshot.
// It mutates a running disposable VM; never pass a production socket.
// The Guest must provide fsb-freeze-v1; this test does not freeze the filesystem, avoiding a frozen mount if Resume fails.
func TestGuestControlPlainResume(t *testing.T) {
	if os.Getenv("FSB_TEST_VSOCK_RESUME") != "1" {
		t.Skip("requires an explicitly supplied disposable Firecracker VM")
	}
	apiSocket := os.Getenv("FSB_TEST_API_SOCKET")
	vsockSocket := os.Getenv("FSB_TEST_VSOCK_SOCKET")
	port, err := strconv.ParseUint(os.Getenv("FSB_TEST_GUEST_PORT"), 10, 32)
	require.NoError(t, err)
	require.NotEmpty(t, apiSocket)
	require.NotEmpty(t, vsockSocket)
	require.NotZero(t, port)
	require.NotEqual(t, uint64(1<<32-1), port)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	vm := NewClient(apiSocket)
	defer vm.Close()
	version, err := vm.Version(ctx)
	require.NoError(t, err)
	t.Logf("Firecracker version: %s", version)
	state, err := vm.VMState(ctx)
	require.NoError(t, err)
	require.Equal(t, "Running", state)
	guest := guestcontrol.Client{SocketPath: vsockSocket, Port: uint32(port), Timeout: 2 * time.Second}
	request := guestcontrol.Request{Protocol: guestcontrol.Protocol, Method: "capabilities"}
	before, err := guest.Call(ctx, request)
	require.NoError(t, err, "Guest control must work before Pause")
	t.Log("Guest control baseline passed")

	// A lost Pause or Resume response leaves the outcome uncertain.
	// Retry only Resume with a separate, bounded recovery context; never create a snapshot or issue another Pause.
	mayBePaused := false
	defer func() {
		if mayBePaused {
			recovery, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			if err := vm.Resume(recovery); err != nil {
				t.Errorf("recovery Resume failed; disposable VM needs cleanup: %v", err)
			}
		}
	}()
	for cycle := 1; cycle <= 3; cycle++ {
		mayBePaused = true
		require.NoError(t, vm.Pause(ctx), "cycle %d Pause", cycle)
		require.NoError(t, vm.Resume(ctx), "cycle %d Resume", cycle)
		mayBePaused = false
		t.Logf("cycle %d: Pause and Resume API replies succeeded", cycle)
		after, err := guest.Call(ctx, request)
		require.NoError(t, err, "cycle %d: Resume API success did not restore Guest control; do not enable vsock-based capture", cycle)
		require.Equal(t, before.Epoch, after.Epoch, "Guest helper must not restart during plain Resume")
	}
}

// TestGuestControlClockCalibration verifies settime end to end on a
// disposable Guest: the helper applies the host reading and the guest clock
// lands within a bounded skew. Never pauses the VM.
func TestGuestControlClockCalibration(t *testing.T) {
	if os.Getenv("FSB_TEST_VSOCK_RESUME") != "1" {
		t.Skip("requires an explicitly supplied disposable Firecracker VM")
	}
	vsockSocket := os.Getenv("FSB_TEST_VSOCK_SOCKET")
	port, err := strconv.ParseUint(os.Getenv("FSB_TEST_GUEST_PORT"), 10, 32)
	require.NoError(t, err)
	require.NotEmpty(t, vsockSocket)
	require.NotZero(t, port)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	guest := guestcontrol.Client{SocketPath: vsockSocket, Port: uint32(port), Timeout: 2 * time.Second}
	caps, err := guest.Call(ctx, guestcontrol.Request{Protocol: guestcontrol.Protocol, Method: "capabilities"})
	require.NoError(t, err)
	require.NotEmpty(t, caps.Epoch)
	request := guestcontrol.Request{Protocol: guestcontrol.Protocol, Epoch: caps.Epoch}

	// Register before the first offset write: a failed assertion or a lost
	// reply right after applying the offset must still leave the disposable
	// guest usable.
	t.Cleanup(func() {
		restore, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := guest.SetTime(restore, request, time.Now()); err != nil {
			t.Errorf("failed to restore the guest clock: %v", err)
		}
	})

	// A guest already within the tolerance of the host would pass a plain
	// skew assertion even if the setter silently did nothing, so first move
	// the guest clock a known offset and require the echoed reading to
	// follow: that proves CLOCK_REALTIME actually moved.
	const offset = time.Hour
	moved, err := guest.SetTime(ctx, request, time.Now().Add(-offset))
	require.NoError(t, err, "guest clock calibration failed; rebuild the Guest helper")
	echoSkew := time.Unix(0, moved.TimeUnixNano).Sub(time.Now().Add(-offset))
	require.Less(t, echoSkew.Abs(), 5*time.Second, "guest clock did not take the offset; settime is a no-op")

	// Calibrate back and assert the closed skew.
	host := time.Now()
	restored, err := guest.SetTime(ctx, request, host)
	require.NoError(t, err)
	skew := time.Unix(0, restored.TimeUnixNano).Sub(time.Now())
	require.Less(t, skew.Abs(), 5*time.Second, "guest clock diverges from the host after settime")
}
