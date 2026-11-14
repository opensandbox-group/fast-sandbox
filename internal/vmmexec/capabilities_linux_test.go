//go:build linux

package vmmexec

import (
	"errors"
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDropAndVerifyCapabilities exercises the real privilege transitions:
// after the drop, every promised set lacks the fixed bits and NoNewPrivs is
// set. The goroutine stays locked forever (the thread keeps its dropped set
// and must never return to the runtime pool).
func TestDropAndVerifyCapabilities(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root: dropping bounding-set capabilities needs CAP_SETPCAP")
	}
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := DropCapabilities(); err != nil {
			done <- err
			return
		}
		done <- VerifyDropped()
	}()
	require.NoError(t, <-done)
}

// TestDropCapabilitiesLoudWithoutSetpcap pins the fail-closed behavior of an
// unprivileged caller (for example a future non-privileged Fastlet Pod or a
// plain CI runner): already-absent capabilities are accepted, but a present
// capability that cannot be dropped from the bounding set is a loud error,
// never a silent launch with the privilege still in place.
func TestDropCapabilitiesLoudWithoutSetpcap(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires a non-root caller: root holds CAP_SETPCAP and the drop succeeds")
	}
	done := make(chan error, 1)
	go func() {
		// The thread may end up half-transitioned; keep it out of the
		// runtime pool.
		runtime.LockOSThread()
		done <- DropCapabilities()
	}()
	err := <-done
	require.ErrorContains(t, err, "refusing to launch")
}

// TestRunExecutesRealTransitions exercises Run end to end on Linux: the
// real transitions succeed and the (never-returning) exec hook receives the
// target only after the self-verification passed.
func TestRunExecutesRealTransitions(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root: dropping bounding-set capabilities needs CAP_SETPCAP")
	}
	type execCall struct {
		path string
		argv []string
	}
	done := make(chan error, 1)
	calls := make(chan execCall, 1)
	go func() {
		// The thread keeps the dropped set and never returns to the
		// runtime pool.
		runtime.LockOSThread()
		done <- Run([]string{"/firecracker", "--id", "sandbox-1", "--api-sock", "/api.sock"},
			nil, runtime.LockOSThread, func(path string, argv []string, _ []string) error {
				calls <- execCall{path: path, argv: argv}
				return errors.New("exec replaced by a test hook")
			})
	}()
	// Run propagates the exec hook's result; reaching it at all proves every
	// privilege transition and the self-verification succeeded.
	err := <-done
	require.ErrorContains(t, err, "exec replaced by a test hook")
	call := <-calls
	require.Equal(t, JailerVMMPath, call.path)
	require.Equal(t, []string{JailerVMMPath, "--id", "sandbox-1", "--api-sock", "/api.sock"}, call.argv)
}

// TestDropOneFromBoundingSet pins the classification contract: an unknown
// capability number is an accepted skip (the kernel does not define it, so
// it cannot be held), and a present capability that cannot be dropped fails
// loudly instead of silently proceeding.
func TestDropOneFromBoundingSet(t *testing.T) {
	// Capability 63 is beyond CAP_LAST_CAP on every supported kernel.
	require.NoError(t, dropOneFromBoundingSet(63))
	if os.Geteuid() == 0 {
		require.NoError(t, dropOneFromBoundingSet(12))
		// The already-absent capability is accepted without privilege.
		require.NoError(t, dropOneFromBoundingSet(12))
		return
	}
	// Non-root lacks CAP_SETPCAP: dropping a present capability is a real
	// EPERM and must be loud.
	err := dropOneFromBoundingSet(12)
	require.ErrorContains(t, err, "refusing to launch")
}
