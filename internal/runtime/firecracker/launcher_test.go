package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"fast-sandbox/internal/vmmexec"
)

type fakeProcess struct {
	pid    int
	killed bool
}

func (f *fakeProcess) PID() int { return f.pid }
func (f *fakeProcess) Kill() error {
	f.killed = true
	return nil
}
func (f *fakeProcess) Wait() error { return nil }

type fakeProcessRunner struct {
	started [][]string
	cgroups []string
	err     error
	// processes records every process handed out so tests can assert the
	// killAndForget path (tracked processes die via Kill, not killProcess).
	processes []*fakeProcess
}

func (f *fakeProcessRunner) StartInCgroup(_ context.Context, cgroupPath, _ string, name string, args []string, _ string) (Process, error) {
	f.cgroups = append(f.cgroups, cgroupPath)
	return f.Start(context.Background(), name, args, "")
}

func (f *fakeProcessRunner) Start(_ context.Context, name string, args []string, _ string) (Process, error) {
	f.started = append(f.started, append([]string{name}, args...))
	if f.err != nil {
		return nil, f.err
	}
	process := &fakeProcess{pid: 4242}
	f.processes = append(f.processes, process)
	return process, nil
}

func TestBuildArgvTruncatesID(t *testing.T) {
	longID := strings.Repeat("s", 40)
	argv := (launchConfig{SandboxID: longID, APIAddress: "/var/lib/fast-sandbox/api.sock"}).buildArgv()
	require.Equal(t, []string{"--id", strings.Repeat("s", 32), "--api-sock", "/var/lib/fast-sandbox/api.sock"}, argv)

	shortID := "sandbox-1"
	argv = (launchConfig{SandboxID: shortID, APIAddress: "/var/lib/fast-sandbox/api.sock"}).buildArgv()
	require.Contains(t, argv, "--id")
	require.Equal(t, shortID, argv[argvIndex(argv, "--id")+1])
}

func TestBuildArgvWithChrootBase(t *testing.T) {
	// chrooting is the jailer's job (--chroot-base-dir); direct mode has no
	// chroot.
	argv := (launchConfig{SandboxID: "sandbox-1", APIAddress: "/run/api.sock", ChrootBase: "/var/lib/fast-sandbox/jails"}).buildArgv()
	require.NotContains(t, argv, "--chroot-base")
	require.Contains(t, argv, "--api-sock")
}

func TestLaunchValidation(t *testing.T) {
	runner := &fakeProcessRunner{}
	ctx := context.Background()

	_, err := launch(ctx, runner, launchConfig{SandboxID: "sandbox-1", APIAddress: "/run/api.sock"})
	require.ErrorIs(t, err, ErrInvalidConfig)

	_, err = launch(ctx, runner, launchConfig{BinaryPath: "/usr/local/bin/firecracker", APIAddress: "relative.sock"})
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Empty(t, runner.started)
}

func TestLaunchInvokesRunner(t *testing.T) {
	runner := &fakeProcessRunner{}
	ctx := context.Background()

	process, err := launch(ctx, runner, launchConfig{BinaryPath: "/usr/local/bin/firecracker", SandboxID: "sandbox-1", APIAddress: "/run/api.sock"})
	require.NoError(t, err)
	require.Equal(t, 4242, process.PID())
	require.Len(t, runner.started, 1)
	require.Equal(t, "/usr/local/bin/firecracker", runner.started[0][0])
	require.Contains(t, runner.started[0], "--api-sock")
}

func TestLaunchInvokesRunnerAtomicallyInSandboxCgroup(t *testing.T) {
	runner := &fakeProcessRunner{}
	process, err := launch(context.Background(), runner, launchConfig{
		BinaryPath: "/usr/local/bin/firecracker", SandboxID: "sandbox-1", APIAddress: "/run/api.sock",
		WorkingDir: "/var/lib/fast-sandbox/sandbox-1", CgroupPath: "/sys/fs/cgroup/pod/fast-sandbox/fsb-1",
	})
	require.NoError(t, err)
	require.Equal(t, 4242, process.PID())
	require.Equal(t, []string{"/sys/fs/cgroup/pod/fast-sandbox/fsb-1"}, runner.cgroups)
	require.Equal(t, "/usr/local/bin/firecracker", runner.started[0][0])
}

func TestBuildArgvJailer(t *testing.T) {
	argv := (launchConfig{
		BinaryPath: "/usr/local/bin/firecracker", JailerPath: "/usr/local/bin/jailer",
		ChrootBase: "/var/lib/fast-sandbox/firecracker/jails", NetNSPath: "/run/netns/fsb-pod-1",
		SandboxID: strings.Repeat("s", 40),
	}).buildArgv()
	// The jailer consumes the identity/netns/chroot flags; --id appears
	// exactly once (before --) and the truncated value matches NodeJanitor.
	require.Equal(t, []string{
		"--id", strings.Repeat("s", 32),
		"--netns", "/run/netns/fsb-pod-1",
		"--uid", "0",
		"--gid", "0",
		"--exec-file", "/usr/local/bin/firecracker",
		"--chroot-base-dir", "/var/lib/fast-sandbox/firecracker/jails",
		"--",
		"--api-sock", jailerChrootAPISock,
		"--log-path", jailerChrootLogPath,
	}, argv)
	require.Equal(t, 1, countOccurrences(argv, "--id"))
	require.Equal(t, 1, countOccurrences(argv, "--api-sock"))
}

// TestBuildArgvJailerExecHelper pins the FSEP-0003 exec-file contract: the
// jailer's exec file is the capability-drop helper, installed with the
// "firecracker" basename so the jailer-derived jail root
// (<chroot-base>/firecracker/<id>/root) and every Fastlet path consumer
// stay unchanged.
func TestBuildArgvJailerExecHelper(t *testing.T) {
	argv := (launchConfig{
		BinaryPath: "/opt/fast-sandbox/firecracker/versions/1.17.0/firecracker",
		ExecFile:   "/usr/local/libexec/fast-sandbox/vmm-capdrop/firecracker",
		JailerPath: "/opt/fast-sandbox/firecracker/versions/1.17.0/jailer",
		ChrootBase: "/var/lib/fast-sandbox/jails", NetNSPath: "/run/netns/fsb-pod-1",
		SandboxID: "sandbox-1",
	}).buildArgv()
	require.Equal(t, "/usr/local/libexec/fast-sandbox/vmm-capdrop/firecracker", argv[argvIndex(argv, "--exec-file")+1])
}

// TestLaunchJailerRejectsForeignHelperBasename keeps the jail-path contract
// loud: an exec file with any other basename would move the jail root
// without a single path consumer following it.
func TestLaunchJailerRejectsForeignHelperBasename(t *testing.T) {
	runner := &fakeProcessRunner{}
	_, err := launch(context.Background(), runner, launchConfig{
		BinaryPath: "/usr/local/bin/firecracker",
		ExecFile:   "/usr/local/libexec/fast-sandbox/vmm-capdrop/vmm-exec",
		JailerPath: "/usr/local/bin/jailer", ChrootBase: "/jails",
		NetNSPath: "/run/netns/fsb-1", SandboxID: "sandbox-1", APIAddress: "/run/api.sock",
	})
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.ErrorContains(t, err, "basename")
	require.Empty(t, runner.started)
}

// TestLaunchDirectInsertsExecHelper pins the direct-launch argv: the helper
// runs and names the real binary with --exec; argv and process identity
// stay otherwise unchanged, with or without the netns entry.
func TestLaunchDirectInsertsExecHelper(t *testing.T) {
	helper := "/usr/local/libexec/fast-sandbox/vmm-capdrop/firecracker"
	for _, testCase := range []struct {
		name         string
		netns        string
		expectedName string
		expectedArgs []string
	}{
		{
			name: "plain", netns: "",
			expectedName: helper,
			expectedArgs: []string{helper, vmmexec.ExecFlag, "/usr/local/bin/firecracker", "--id", "sandbox", "--api-sock", "/run/api.sock"},
		},
		{
			name: "netns", netns: "/run/netns/slot",
			expectedName: "nsenter",
			expectedArgs: []string{"nsenter", "--net=/run/netns/slot", "--", helper,
				vmmexec.ExecFlag, "/usr/local/bin/firecracker", "--id", "sandbox", "--api-sock", "/run/api.sock"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			runner := &fakeProcessRunner{}
			_, err := launch(context.Background(), runner, launchConfig{
				SandboxID: "sandbox", BinaryPath: "/usr/local/bin/firecracker", ExecFile: helper,
				APIAddress: "/run/api.sock", NetNSPath: testCase.netns,
			})
			require.NoError(t, err)
			require.Equal(t, testCase.expectedArgs, runner.started[0])
		})
	}
	// Without the helper the launch stays the legacy direct exec of the
	// binary (the E2E preparation VM keeps its privileges; production
	// resolves the helper unconditionally and the launch-time capability
	// assertion fails any create that skipped the drop).
	runner := &fakeProcessRunner{}
	_, err := launch(context.Background(), runner, launchConfig{
		SandboxID: "sandbox", BinaryPath: "/usr/local/bin/firecracker", APIAddress: "/run/api.sock",
	})
	require.NoError(t, err)
	require.Equal(t, "/usr/local/bin/firecracker", runner.started[0][0])
	require.NotContains(t, runner.started[0], vmmexec.ExecFlag)
}

func TestLaunchJailerValidation(t *testing.T) {
	runner := &fakeProcessRunner{}
	ctx := context.Background()
	base := launchConfig{
		BinaryPath: "/usr/local/bin/firecracker", JailerPath: "/usr/local/bin/jailer",
		ChrootBase: "/var/lib/fast-sandbox/firecracker/jails", NetNSPath: "/run/netns/fsb-1",
		SandboxID: "sandbox-1", APIAddress: "/run/api.sock",
	}
	_, err := launch(ctx, runner, base)
	require.NoError(t, err)
	require.Len(t, runner.started, 1)
	require.Equal(t, "/usr/local/bin/jailer", runner.started[0][0])
	require.Equal(t, "/usr/local/bin/firecracker", runner.started[0][argvIndex(runner.started[0], "--exec-file")+1])

	_, err = launch(ctx, runner, launchConfig{JailerPath: "/usr/local/bin/jailer", SandboxID: "sandbox-1", APIAddress: "/run/api.sock"})
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Len(t, runner.started, 1)

	_, err = launch(ctx, runner, launchConfig{JailerPath: "/usr/local/bin/jailer", BinaryPath: "/usr/local/bin/firecracker",
		ChrootBase: "/jails", SandboxID: "sandbox-1", APIAddress: "/run/api.sock"})
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.Len(t, runner.started, 1)
}

func TestPrepareJailRoot(t *testing.T) {
	root := t.TempDir()
	jailRoot := filepath.Join(root, "firecracker", "sandbox-1", "root")
	cached := filepath.Join(root, "cache", "rootfs.img")
	vmstate := filepath.Join(root, "cache", "vmstate.snap")
	memory := filepath.Join(root, "cache", "memory.snap")
	vmmBinary := filepath.Join(root, "bin", "firecracker")
	for path, content := range map[string]string{cached: "rootfs-data", vmstate: "vmstate-data", memory: "memory-data", vmmBinary: "vmm-binary-data"} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o640))
	}

	require.NoError(t, prepareJailRoot(jailRoot, cached, vmstate, memory, vmmBinary))

	rootfs, err := os.ReadFile(filepath.Join(jailRoot, rootfsImageName))
	require.NoError(t, err)
	require.Equal(t, "rootfs-data", string(rootfs))
	// The real VMM binary is content-identical at the fixed in-jail
	// convention the capability-drop helper execs (/vmm/firecracker); it is
	// a copy, not a hard link (the jailer rejects multi-link copy targets,
	// and the managed host binary must not be reachable for mutation
	// through the jail).
	vmmInJail, err := os.ReadFile(jailedVMMBinaryPath(jailRoot))
	require.NoError(t, err)
	require.Equal(t, "vmm-binary-data", string(vmmInJail))
	vmmInJailInfo, err := os.Stat(jailedVMMBinaryPath(jailRoot))
	require.NoError(t, err)
	require.Equal(t, uint64(1), uint64(vmmInJailInfo.Sys().(*syscall.Stat_t).Nlink), "the jail VMM binary must not be a hard link")
	// The snapshot files are hard-linked to the cache (same inode).
	vmstateInJail, err := os.Stat(filepath.Join(jailRoot, jailerChrootSnapshotsDir, vmstateSnapshotName))
	require.NoError(t, err)
	vmstateInCache, err := os.Stat(vmstate)
	require.NoError(t, err)
	require.True(t, os.SameFile(vmstateInJail, vmstateInCache), "vmstate must be hard-linked")
	memoryInJail, err := os.Stat(filepath.Join(jailRoot, jailerChrootSnapshotsDir, memorySnapshotName))
	require.NoError(t, err)
	memoryInCache, err := os.Stat(memory)
	require.NoError(t, err)
	require.True(t, os.SameFile(memoryInJail, memoryInCache), "memory must be hard-linked")

	// A stale jail root is replaced.
	require.NoError(t, os.WriteFile(filepath.Join(jailRoot, "stale"), []byte("stale"), 0o640))
	require.NoError(t, prepareJailRoot(jailRoot, cached, vmstate, memory, vmmBinary))
	_, err = os.Stat(filepath.Join(jailRoot, "stale"))
	require.True(t, os.IsNotExist(err))
}

func TestJailerRootPath(t *testing.T) {
	require.Equal(t,
		filepath.Join("/var/lib/fast-sandbox/firecracker/jails", "firecracker", "sandbox-1", "root"),
		jailerRoot("/var/lib/fast-sandbox/firecracker/jails", "firecracker", "sandbox-1"))
}

func countOccurrences(values []string, target string) int {
	count := 0
	for _, value := range values {
		if value == target {
			count++
		}
	}
	return count
}

func argvIndex(argv []string, flag string) int {
	for index, value := range argv {
		if value == flag {
			return index
		}
	}
	return -1
}

// Identically named TAP devices are independent in each netns; neither direct nor cgroup launches may bypass that boundary.
func TestLaunchDirectEntersAssignedNetworkNamespace(t *testing.T) {
	for _, group := range []string{"", "/sys/fs/cgroup/test"} {
		runner := &fakeProcessRunner{}
		_, err := launch(t.Context(), runner, launchConfig{SandboxID: "sandbox", BinaryPath: "/usr/bin/firecracker", APIAddress: "/run/api.sock", NetNSPath: "/run/netns/slot", CgroupPath: group})
		require.NoError(t, err)
		require.Equal(t, []string{"nsenter", "--net=/run/netns/slot", "--", "/usr/bin/firecracker", "--id", "sandbox", "--api-sock", "/run/api.sock"}, runner.started[0])
		if group != "" {
			require.Equal(t, []string{group}, runner.cgroups)
		}
	}
}
