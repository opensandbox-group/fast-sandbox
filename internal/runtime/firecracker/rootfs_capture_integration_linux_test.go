package firecracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/firecrackervmm"
	"fast-sandbox/internal/guestcontrol"
)

// captureLabLease is only for a disposable lab VM owned by a single operator; it does not replace the production lifecycle coordinator.
type captureLabLease struct {
	vm          *Client
	device      *os.File
	engine      string
	released    bool
	quarantined bool
}

func (l *captureLabLease) Verify(ctx context.Context) (rootfsCaptureBoundary, error) {
	var config struct {
		Drives []struct {
			Path   string `json:"path_on_host"`
			Engine string `json:"io_engine"`
		} `json:"drives"`
	}
	if err := l.vm.do(ctx, "GET", "/vm/config", nil, &config); err != nil {
		return rootfsCaptureBoundary{}, err
	}
	if len(config.Drives) != 1 || config.Drives[0].Engine != l.engine {
		return rootfsCaptureBoundary{}, errors.New("lab requires one matching explicitly configured drive")
	}
	live, err := os.Stat(config.Drives[0].Path)
	if err != nil {
		return rootfsCaptureBoundary{}, err
	}
	held, err := l.device.Stat()
	if err != nil {
		return rootfsCaptureBoundary{}, err
	}
	if !os.SameFile(live, held) {
		return rootfsCaptureBoundary{}, errors.New("VM device differs from held descriptor")
	}
	state, err := l.vm.VMState(ctx)
	if err != nil {
		return rootfsCaptureBoundary{}, err
	}
	if state != "Running" {
		return rootfsCaptureBoundary{}, fmt.Errorf("unexpected VM state %s", state)
	}
	return rootfsCaptureBoundary{IOEngine: l.engine, Filesystem: "ext4", FilesystemExclusive: true}, nil
}
func (l *captureLabLease) Release() error         { l.released = true; return nil }
func (l *captureLabLease) Quarantine(error) error { l.quarantined = true; return nil }

// Requires a dedicated Guest: an independent helper freezes ext4, with no raw block, swap, or other writers.
// The external lab creates and destroys VM/OverlayBD resources; this test never formats or unmounts devices.
func TestRootfsCaptureTransactionIntegration(t *testing.T) {
	if os.Getenv("FSB_TEST_ROOTFS_CAPTURE") != "1" {
		t.Skip("requires disposable single-drive VM and OverlayBD upper")
	}
	required := func(name string) string { v := os.Getenv(name); require.NotEmpty(t, v, name); return v }
	vm := NewClient(required("FSB_TEST_API_SOCKET"))
	defer vm.Close()
	guest := guestcontrol.Client{SocketPath: required("FSB_TEST_VSOCK_SOCKET"), Port: 1234, Timeout: 2 * time.Second}
	if value := os.Getenv("FSB_TEST_GUEST_PORT"); value != "" {
		n, e := strconv.ParseUint(value, 10, 32)
		require.NoError(t, e)
		guest.Port = uint32(n)
	}
	device, err := os.OpenFile(required("FSB_TEST_FLUSH_BLOCK_DEVICE"), os.O_RDWR, 0)
	require.NoError(t, err)
	defer device.Close()
	engine := required("FSB_TEST_CAPTURE_ENGINE")
	sources := []string{required("FSB_TEST_UPPER_DATA"), required("FSB_TEST_UPPER_INDEX")}
	output := required("FSB_TEST_CAPTURE_OUTPUT")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	version, err := vm.Version(ctx)
	require.NoError(t, err)
	require.Equal(t, "1.16.1", version)
	var caps guestcontrol.Response
	// The Guest workload startup marker does not mean the independent helper is listening; retry only the handshake, which has no side effects.
	require.Eventually(t, func() bool {
		caps, err = guest.Call(ctx, guestcontrol.Request{Protocol: guestcontrol.Protocol, Method: "capabilities"})
		return err == nil
	}, 5*time.Second, 50*time.Millisecond, "guest helper handshake: %v", err)
	require.GreaterOrEqual(t, caps.MaxLeaseMillis, int64(10000))
	for i, scenario := range []string{"success", "flush-error", "cut-error", "cancel-cut", "success-after-errors"} {
		if !t.Run(scenario, func(t *testing.T) {
			staging := filepath.Join(output, scenario)
			require.NoError(t, os.Mkdir(staging, 0700))
			lease := &captureLabLease{vm: vm, device: device, engine: engine}
			operation, cancelOp := context.WithCancel(ctx)
			defer cancelOp()
			request := guestcontrol.Request{Protocol: guestcontrol.Protocol, Epoch: caps.Epoch, Generation: caps.Generation + uint64(i+1), OperationID: "transaction-" + scenario, LeaseMillis: 10000}
			journal, err := newCaptureJournal(filepath.Join(staging, "journal"), captureRecord{Incarnation: "disposable-lab-vm", Device: device.Name(), Staging: staging, Request: request})
			require.NoError(t, err)
			copied := false
			tx := rootfsCapture{Progress: journal, Lease: lease, VM: vm, Guest: guest, Request: request, RecoveryTimeout: 5 * time.Second,
				Flush: func() error {
					if err := flushCapturedBlock(device); err != nil {
						return err
					}
					if scenario == "flush-error" {
						return errors.New("injected flush failure")
					}
					return nil
				},
				Cut: func() error {
					copied = true
					for j, source := range sources {
						if err := copyFrozenUpper(source, filepath.Join(staging, []string{"data", "index"}[j])); err != nil {
							return err
						}
						if j == 0 && scenario == "cut-error" {
							return errors.New("injected partial cut failure")
						}
					}
					if scenario == "cancel-cut" {
						cancelOp()
					}
					return nil
				},
			}
			err = tx.run(operation)
			if scenario == "success" || scenario == "success-after-errors" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			saved, journalErr := loadCaptureJournal(filepath.Join(staging, "journal"))
			require.NoError(t, journalErr)
			require.Equal(t, captureRecovered, saved.record.Phase)
			require.True(t, lease.released)
			require.False(t, lease.quarantined)
			if scenario == "flush-error" {
				require.False(t, copied)
			}
			if scenario == "cancel-cut" {
				require.ErrorIs(t, err, context.Canceled)
			}
			statusReq := request
			statusReq.Method = "status"
			statusReq.LeaseMillis = 0
			state, err := guest.Call(ctx, statusReq)
			require.NoError(t, err)
			require.Equal(t, guestcontrol.Released, state.State)
			require.False(t, state.CleanupPending)
			vmState, err := vm.VMState(ctx)
			require.NoError(t, err)
			require.Equal(t, "Running", vmState)
			t.Logf("engine=%s scenario=%s VM Running; Guest released; lease released", engine, scenario)
		}) {
			break
		}
	}
}

// This target is only for controlled experiments with a lab-pinned incarnation; never use it for production leases.
type captureLabRecovery struct{ *Client }

func (l captureLabRecovery) VerifyIdentity(ctx context.Context, r captureRecord) error {
	if r.Incarnation != "disposable-lab-vm" {
		return errors.New("unexpected lab incarnation")
	}
	_, err := l.VMState(ctx)
	return err
}

// Discard the in-memory transaction after persisting intent, then reload the record and recover the real Guest.
// This verifies reconciliation logic, not a complete lifecycle E2E with a SIGKILLed Fastlet process.
func TestRootfsCaptureRestartIntegration(t *testing.T) {
	if os.Getenv("FSB_TEST_ROOTFS_CAPTURE") != "1" {
		t.Skip("requires disposable single-drive VM")
	}
	vm := NewClient(os.Getenv("FSB_TEST_API_SOCKET"))
	defer vm.Close()
	guest := guestcontrol.Client{SocketPath: os.Getenv("FSB_TEST_VSOCK_SOCKET"), Port: 1234, Timeout: 2 * time.Second}
	caps, err := guest.Call(t.Context(), guestcontrol.Request{Protocol: guestcontrol.Protocol, Method: "capabilities"})
	require.NoError(t, err)
	require.Equal(t, "/", caps.ProtectedMount)
	require.Equal(t, "ext4", caps.Filesystem)
	for i, phase := range []capturePhase{capturePrepared, captureFreezing, capturePausing, captureFlushing, captureCutting, captureRecovering} {
		t.Run(string(phase), func(t *testing.T) {
			dir := filepath.Join(os.Getenv("FSB_TEST_CAPTURE_OUTPUT"), "restart-"+string(phase))
			req := guestcontrol.Request{Protocol: guestcontrol.Protocol, Epoch: caps.Epoch, Generation: caps.Generation + uint64(i+1), OperationID: "restart-" + string(phase), LeaseMillis: 10000}
			j, e := newCaptureJournal(dir, captureRecord{Incarnation: "disposable-lab-vm", Device: os.Getenv("FSB_TEST_FLUSH_BLOCK_DEVICE"), Staging: dir, Request: req})
			require.NoError(t, e)
			if phase != capturePrepared {
				require.NoError(t, j.Record(captureFreezing))
				_, e = guest.FreezeAndWait(t.Context(), req)
				require.NoError(t, e)
				if phase != captureFreezing {
					require.NoError(t, j.Record(capturePausing))
					require.NoError(t, vm.Pause(t.Context()))
				}
			}
			j.record.Phase = phase
			require.NoError(t, j.persist())
			recovered, e := loadCaptureJournal(dir)
			require.NoError(t, e)
			require.NoError(t, reconcileCapture(t.Context(), recovered, captureLabRecovery{vm}, guest, 5*time.Second))
			state, e := vm.VMState(t.Context())
			require.NoError(t, e)
			require.Equal(t, "Running", state)
			require.Equal(t, captureRecovered, recovered.record.Phase)
		})
	}
}

// Inject a transport failure before the real Resume/Thaw request is delivered.
type captureLabRecoveryFailure struct {
	captureLabRecovery
	failResume bool
}

func (r captureLabRecoveryFailure) Resume(ctx context.Context) error {
	if r.failResume {
		return errors.New("injected Resume transport failure")
	}
	return r.Client.Resume(ctx)
}

type captureLabThawFailure struct {
	guestcontrol.Client
	failThaw bool
}

func (g captureLabThawFailure) ThawAndWait(ctx context.Context, req guestcontrol.Request) (guestcontrol.Response, error) {
	if g.failThaw {
		return guestcontrol.Response{}, errors.New("injected Thaw transport failure")
	}
	return g.Client.ThawAndWait(ctx, req)
}

func TestRootfsCaptureQuarantineRetryIntegration(t *testing.T) {
	if os.Getenv("FSB_TEST_ROOTFS_CAPTURE") != "1" {
		t.Skip("requires disposable single-drive VM")
	}
	vm := NewClient(os.Getenv("FSB_TEST_API_SOCKET"))
	defer vm.Close()
	guest := guestcontrol.Client{SocketPath: os.Getenv("FSB_TEST_VSOCK_SOCKET"), Port: 1234, Timeout: 2 * time.Second}
	for _, failure := range []string{"resume", "thaw"} {
		t.Run(failure, func(t *testing.T) {
			caps, err := guest.Call(t.Context(), guestcontrol.Request{Protocol: guestcontrol.Protocol, Method: "capabilities"})
			require.NoError(t, err)
			req := guestcontrol.Request{Protocol: guestcontrol.Protocol, Epoch: caps.Epoch, Generation: caps.Generation + 1, OperationID: "retry-" + failure, LeaseMillis: 10000}
			dir := filepath.Join(os.Getenv("FSB_TEST_CAPTURE_OUTPUT"), "quarantine-"+failure)
			j, err := newCaptureJournal(dir, captureRecord{Incarnation: "disposable-lab-vm", Device: os.Getenv("FSB_TEST_FLUSH_BLOCK_DEVICE"), Staging: dir, Request: req})
			require.NoError(t, err)
			_, err = guest.FreezeAndWait(t.Context(), req)
			require.NoError(t, err)
			require.NoError(t, vm.Pause(t.Context()))
			// Always attempt recovery even if an assertion below fails.
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if state, e := vm.VMState(cleanup); e == nil && state == vmPaused {
					_ = vm.Resume(cleanup)
				}
				_, _ = guest.ThawAndWait(cleanup, req)
			})
			target := captureLabRecoveryFailure{captureLabRecovery: captureLabRecovery{vm}, failResume: failure == "resume"}
			faultyGuest := captureLabThawFailure{Client: guest, failThaw: failure == "thaw"}
			require.Error(t, reconcileCapture(t.Context(), j, target, faultyGuest, 5*time.Second))
			j, err = loadCaptureJournal(dir)
			require.NoError(t, err)
			require.Equal(t, captureQuarantined, j.record.Phase)
			state, err := vm.VMState(t.Context())
			require.NoError(t, err)
			if failure == "resume" {
				require.Equal(t, vmPaused, state)
			} else {
				require.Equal(t, vmRunning, state)
			}
			require.NoError(t, reconcileCapture(t.Context(), j, captureLabRecovery{vm}, guest, 5*time.Second))
			state, err = vm.VMState(t.Context())
			require.NoError(t, err)
			require.Equal(t, vmRunning, state)
			require.Equal(t, captureRecovered, j.record.Phase)
			req.Method, req.LeaseMillis = "status", 0
			status, err := guest.Call(t.Context(), req)
			require.NoError(t, err)
			require.Equal(t, guestcontrol.Released, status.State)
			require.False(t, status.CleanupPending)
			t.Logf("%s failure: reloaded quarantine recovered; VMM Running and Guest Released", failure)
		})
	}
}

func TestFlushCapturedBlockRejectsNonBlockDevice(t *testing.T) {
	require.ErrorContains(t, flushCapturedBlock(nil), "open block device")
	file, err := os.Create(filepath.Join(t.TempDir(), "regular"))
	require.NoError(t, err)
	require.ErrorContains(t, flushCapturedBlock(file), "requires a block device")
	require.NoError(t, file.Close())
	require.Error(t, flushCapturedBlock(file))
}

// The operator must explicitly provide a disposable test block device.
// This test does not write, format, mount, capture, or tear down devices.
func TestFlushCapturedBlockIntegration(t *testing.T) {
	path := os.Getenv("FSB_TEST_FLUSH_BLOCK_DEVICE")
	if path == "" {
		t.Skip("requires explicit disposable block device")
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer file.Close()
	err = flushCapturedBlock(file)
	if os.Getenv("FSB_TEST_FLUSH_EXPECT_ERROR") == "1" {
		require.ErrorIs(t, err, syscall.EIO, "fault injection must reach the caller")
	} else {
		require.NoError(t, err)
	}
}

// A jailer copy has a different inode but must contain the selected VMM bytes.
func TestManagedVMMQualificationAcceptsCopyAndRejectsReplacement(t *testing.T) {
	executable, err := os.Executable()
	require.NoError(t, err)
	copyPath := filepath.Join(t.TempDir(), "firecracker")
	payload, err := os.ReadFile(executable)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(copyPath, payload, 0600))
	state := &SandboxState{PID: os.Getpid(), VMM: &firecrackervmm.Selection{BinaryPath: copyPath}}
	require.NoError(t, qualifyVMMExecutable(state, ""))
	require.Error(t, qualifyVMMExecutable(state, "wrong-explicit-digest"))
	require.NoError(t, os.WriteFile(copyPath, []byte("different executable"), 0600))
	require.ErrorContains(t, qualifyVMMExecutable(state, ""), "does not match")
}
