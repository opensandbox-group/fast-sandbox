package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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
	err     error
	// processes records every process handed out so tests can assert the
	// killAndForget path (tracked processes die via Kill, not killProcess).
	processes []*fakeProcess
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
	for path, content := range map[string]string{cached: "rootfs-data", vmstate: "vmstate-data", memory: "memory-data"} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o640))
	}

	require.NoError(t, prepareJailRoot(jailRoot, cached, vmstate, memory))

	rootfs, err := os.ReadFile(filepath.Join(jailRoot, rootfsImageName))
	require.NoError(t, err)
	require.Equal(t, "rootfs-data", string(rootfs))
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
	require.NoError(t, prepareJailRoot(jailRoot, cached, vmstate, memory))
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
