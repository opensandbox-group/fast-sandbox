package vmmexec

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDropSet pins the kernel capability numbers of the fixed drop set
// (FSEP-0003): net_admin=12, net_raw=13, ipc_lock=14, perfmon=38, bpf=39.
// Any change to the set is a reviewed security decision made through a code
// change here, never through configuration.
func TestDropSet(t *testing.T) {
	require.Equal(t, []Capability{
		{12, "net_admin"}, {13, "net_raw"}, {14, "ipc_lock"}, {38, "perfmon"}, {39, "bpf"},
	}, DropSet)
	// The mask covers exactly the drop set's bits.
	mask := uint64(0)
	for _, capability := range DropSet {
		mask |= 1 << uint(capability.Value)
		require.NotContains(t, capability.Name, " ")
	}
	require.Equal(t, uint64(0xC000007000), DropMask())
	require.Equal(t, []int{12, 13, 14, 38, 39}, DropValues())
	require.Equal(t, []string{"net_admin", "net_raw", "ipc_lock", "perfmon", "bpf"}, DropNames())
}

func TestOffender(t *testing.T) {
	capability, ok := Offender(DropMask())
	require.True(t, ok)
	require.Equal(t, "net_admin", capability.Name)

	_, ok = Offender(0)
	require.False(t, ok)

	// A mask holding several dropped bits reports the first in drop-set
	// order, so diagnostics are deterministic.
	capability, ok = Offender((1 << 39) | (1 << 14))
	require.True(t, ok)
	require.Equal(t, "ipc_lock", capability.Name)
}

func TestParseArgsJailerConvention(t *testing.T) {
	// The jailer owns argv: the helper receives the Firecracker command
	// line (--id, start-time flags, post--- arguments) and execs the fixed
	// in-jail convention, forwarding argv unchanged.
	target, forwarded, err := ParseArgs([]string{"/firecracker", "--id", "sandbox-1",
		"--start-time-us", "17", "--api-sock", "/api.sock", "--log-path", "/fc.log"})
	require.NoError(t, err)
	require.Equal(t, JailerVMMPath, target)
	require.Equal(t, []string{"--id", "sandbox-1", "--start-time-us", "17",
		"--api-sock", "/api.sock", "--log-path", "/fc.log"}, forwarded)
}

func TestParseArgsExplicitTarget(t *testing.T) {
	// Direct-launch mode: the Fastlet names the target and the remaining
	// argv passes through untouched.
	target, forwarded, err := ParseArgs([]string{"/opt/helper/firecracker",
		ExecFlag, "/opt/firecracker/versions/1.17.0/firecracker", "--id", "sandbox-1", "--api-sock", "/run/api.sock"})
	require.NoError(t, err)
	require.Equal(t, "/opt/firecracker/versions/1.17.0/firecracker", target)
	require.Equal(t, []string{"--id", "sandbox-1", "--api-sock", "/run/api.sock"}, forwarded)

	// The flag is only recognized as the first argument; a bare --exec
	// without an absolute target is a loud error, not a silent fallback.
	_, _, err = ParseArgs([]string{"helper", ExecFlag, "relative/firecracker"})
	require.ErrorContains(t, err, "absolute exec target")
	_, _, err = ParseArgs([]string{"helper", ExecFlag})
	require.ErrorContains(t, err, "absolute exec target")
}

// TestRunOrder verifies the helper contract end to end against hooks: the
// thread lock precedes every transition, the drop precedes the
// self-verification, and the exec carries the target with argv[0] replaced
// by the target path (the only argv difference is the exec path prefix).
func TestRunOrder(t *testing.T) {
	events := []string{}
	var target string
	var argv []string
	var envv []string
	require.NoError(t, run([]string{"/firecracker", "--id", "sandbox-1", "--api-sock", "/api.sock"},
		[]string{"PATH=/bin"},
		func() { events = append(events, "lock") },
		func(path string, execArgv []string, execEnvv []string) error {
			events = append(events, "exec")
			target, argv, envv = path, execArgv, execEnvv
			return nil
		},
		func() error { events = append(events, "drop"); return nil },
		func() error { events = append(events, "verify"); return nil }))
	// The thread lock precedes every transition and exec follows the
	// self-verification.
	require.Equal(t, []string{"lock", "drop", "verify", "exec"}, events)
	require.Equal(t, JailerVMMPath, target)
	require.Equal(t, []string{JailerVMMPath, "--id", "sandbox-1", "--api-sock", "/api.sock"}, argv)
	require.Equal(t, []string{"PATH=/bin"}, envv)
}

func TestRunDirectTarget(t *testing.T) {
	var target string
	var argv []string
	require.NoError(t, run([]string{"helper", ExecFlag, "/usr/local/bin/firecracker", "--id", "sandbox-1"},
		nil, func() {}, func(path string, execArgv []string, _ []string) error {
			target, argv = path, execArgv
			return nil
		}, func() error { return nil }, func() error { return nil }))
	require.Equal(t, "/usr/local/bin/firecracker", target)
	require.Equal(t, []string{"/usr/local/bin/firecracker", "--id", "sandbox-1"}, argv)
}

// TestRunHooksRequired keeps the threading contract explicit: a caller that
// cannot pin the thread must not perform the transitions at all.
func TestRunHooksRequired(t *testing.T) {
	noop := func() error { return nil }
	exec := func(string, []string, []string) error { return nil }
	require.ErrorContains(t, run(nil, nil, nil, exec, noop, noop), "thread lock hook")
	require.ErrorContains(t, run(nil, nil, func() {}, nil, noop, noop), "exec hook")
}

func TestRunPropagatesDropFailure(t *testing.T) {
	// DropCapabilities is the unsupported non-Linux stub on this build
	// (Linux builds exercise it in capabilities_linux_test.go); Run must
	// surface the failure instead of execing.
	if runtime.GOOS == "linux" {
		t.Skip("DropCapabilities runs for real on Linux; covered by capabilities_linux_test.go")
	}
	err := Run([]string{"helper"}, nil, func() {}, func(string, []string, []string) error {
		return errors.New("must not exec")
	})
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "Linux"), err.Error())
}

func TestRunPropagatesVerifyFailure(t *testing.T) {
	err := run([]string{"helper"}, nil, func() {}, func(string, []string, []string) error {
		return errors.New("must not exec")
	}, func() error { return nil }, func() error { return errors.New("bounding set still holds bpf") })
	require.ErrorContains(t, err, "self-check failed")
	require.ErrorContains(t, err, "bounding set")
}
