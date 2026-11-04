package firecracker

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

// Terminal cleanup does not need a live API socket. Keep the fixture limited
// to the durable identity, tracked process, network slot, and runtime cgroup.
func terminalCleanupFixture(t *testing.T, networkDriver *flakyDestroyDriver) (*driverFixture, *fakeProcess, string, string) {
	t.Helper()
	config := firecrackerConfigForTest(t, t.TempDir())
	spec := fastletapi.RuntimeSandboxConfig{Identity: fastletapi.SandboxIdentity{
		SandboxUID: "sandbox-1", FastletPodUID: "pod-uid-a", RuntimeInstanceID: "runtime-1",
		InstanceGeneration: 1, AssignmentAttempt: 1,
	}}
	fixture := &driverFixture{stateRoot: config.StateRoot, sandboxSpec: spec}
	fixture.driver = &Driver{
		config: config, processes: make(map[string]Process),
		killProcess: func(pid int) error {
			fixture.killCalls = append(fixture.killCalls, pid)
			return nil
		},
	}
	fixture.manager = newNetworkManagerWithDriverForTest(t, networkDriver)
	fixture.driver.SetNetworkManager(fixture.manager)
	_, err := fixture.manager.Acquire(t.Context(), fixture.driver.networkOwner(&spec))
	require.NoError(t, err)
	directory, err := ensureSandboxDir(config.StateRoot, spec.Identity.SandboxUID)
	require.NoError(t, err)
	require.NoError(t, saveState(directory, &SandboxState{Config: spec, Phase: PhaseRunning, PID: 777}))
	runtimeCgroups, leaf := interruptedStartCgroup(t, spec.Identity)
	fixture.driver.runtimeCgroups = runtimeCgroups
	process := &fakeProcess{pid: 777}
	fixture.driver.processes[spec.Identity.SandboxUID] = process
	require.NoError(t, fixture.driver.RecordTerminalFailure(t.Context(), spec.Identity, "runtime exited"))
	return fixture, process, directory, leaf
}

func TestTerminalFailureCleanupRetryDoesNotSignalPersistedPID(t *testing.T) {
	for _, operation := range []string{"retain-terminal-record", "controller-delete"} {
		t.Run(operation, func(t *testing.T) {
			networkDriver := &flakyDestroyDriver{failuresRemaining: 1}
			fixture, process, directory, leaf := terminalCleanupFixture(t, networkDriver)
			identity := fixture.sandboxSpec.Identity
			events := filepath.Join(leaf, "cgroup.events")
			require.NoError(t, os.WriteFile(events, []byte("populated 0\n"), 0o600))

			require.ErrorContains(t, fixture.driver.CleanupTerminalFailure(t.Context(), identity), "netns delete failed")
			require.True(t, process.killed, "the original tracked process handle can still be retired")
			require.NotContains(t, fixture.driver.processes, identity.SandboxUID)
			require.Empty(t, fixture.killCalls)
			require.Equal(t, 1, fixture.manager.Snapshot().Destroying)
			state, err := loadState(directory)
			require.NoError(t, err)
			require.Equal(t, PhaseFailed, state.Phase)
			require.Equal(t, 777, state.PID, "cleanup has not reached its final durable update")

			retry := func() error {
				if operation == "controller-delete" {
					return fixture.driver.DeleteSandbox(t.Context(), identity.SandboxUID)
				}
				return fixture.driver.CleanupTerminalFailure(t.Context(), identity)
			}
			// A regular-file cgroup fixture cannot be removed until its events
			// file is removed, which also covers a later physical-cleanup error.
			require.ErrorContains(t, retry(), "remove Sandbox runtime cgroup")
			require.Empty(t, fixture.killCalls, "the old PID may already belong to another process")
			state, err = loadState(directory)
			require.NoError(t, err)
			require.Equal(t, identity, state.Config.Identity)
			require.Equal(t, 777, state.PID)

			require.NoError(t, os.Remove(events))
			require.NoError(t, retry())
			require.Empty(t, fixture.killCalls)
			require.NoDirExists(t, leaf)
			fixture.manager.Quiesce()
			require.Zero(t, fixture.manager.Snapshot().Bound)
			require.Zero(t, fixture.manager.Snapshot().Destroying)
			if operation == "controller-delete" {
				require.NoDirExists(t, directory)
			} else {
				state, err = loadState(directory)
				require.NoError(t, err)
				require.Equal(t, identity, state.Config.Identity)
				require.Equal(t, PhaseFailed, state.Phase)
				require.Zero(t, state.PID)
			}
		})
	}
}

func TestTerminalFailureCleanupRequiresAbsentRuntime(t *testing.T) {
	for _, operation := range []string{"retain-terminal-record", "controller-delete"} {
		for _, observation := range []string{"populated", "unreadable"} {
			t.Run(operation+"/"+observation, func(t *testing.T) {
				networkDriver := &flakyDestroyDriver{}
				fixture, process, directory, leaf := terminalCleanupFixture(t, networkDriver)
				identity := fixture.sandboxSpec.Identity
				events := filepath.Join(leaf, "cgroup.events")
				if observation == "populated" {
					require.NoError(t, os.WriteFile(events, []byte("populated 1\n"), 0o600))
				} else {
					require.NoError(t, os.Mkdir(events, 0o700))
				}

				var err error
				if operation == "controller-delete" {
					err = fixture.driver.DeleteSandbox(t.Context(), identity.SandboxUID)
				} else {
					err = fixture.driver.CleanupTerminalFailure(t.Context(), identity)
				}
				require.ErrorContains(t, err, "confirm terminal Firecracker sandbox")
				require.False(t, process.killed)
				require.Same(t, process, fixture.driver.processes[identity.SandboxUID])
				require.Empty(t, fixture.killCalls)
				require.Zero(t, networkDriver.destroyCalls)
				require.Equal(t, 1, fixture.manager.Snapshot().Bound)
				state, err := loadState(directory)
				require.NoError(t, err)
				require.Equal(t, PhaseFailed, state.Phase)
				require.Equal(t, identity, state.Config.Identity)
				require.Equal(t, 777, state.PID)
			})
		}
	}
}
