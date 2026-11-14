package firecracker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"k8s.io/klog/v2"

	"fast-sandbox/internal/vmmexec"
)

// firecrackerIDLimit matches the NodeJanitor residual-process matcher
// (internal/nodecleanup/process.go): firecracker is launched with
// --id <sandboxID[:32]> so the node daemon can identify and clean up a VMM
// that outlived both the Sandbox object and the Fastlet Pod.
const firecrackerIDLimit = 32

// Jailer mode constants. When FirecrackerConfig.JailerPath is set, the VM
// runs under the jailer (--netns + --chroot-base-dir): the firecracker
// process enters the per-clone slot network namespace and only sees the
// files inside its jail root, so the API socket, the process log, the
// instance rootfs and the restore snapshots are addressed by their
// chroot-relative paths.
const (
	// jailerChrootBaseDir is the ChrootBase root under the StateRoot.
	jailerChrootBaseDir = "jails"
	// jailerChrootRootDir is the chroot directory name inside the jail
	// (the jailer layout: <chroot-base>/<exec-basename>/<id>/root/).
	jailerChrootRootDir = "root"
	// jailerChrootSnapshotsDir receives the restore snapshot hard links.
	jailerChrootSnapshotsDir = "snapshots"
	// jailerChrootAPISock is the chroot-relative API socket path.
	jailerChrootAPISock = "/api.sock"
	// jailerChrootLogPath is the chroot-relative firecracker log path.
	jailerChrootLogPath = "/fc.log"
)

// jailerRoot returns the host path of the jail root of a Sandbox: the
// directory firecracker sees as "/" after the jailer chroot, holding the
// api.sock, rootfs.img and snapshots/ prepared by the driver.
func jailerRoot(chrootBase, execBase, id string) string {
	return filepath.Join(chrootBase, execBase, id, jailerChrootRootDir)
}

// jailedVMMBinaryPath returns the jail-root path of the real Firecracker
// binary (host view of the in-jail /vmm/firecracker): the capability-drop
// helper execs it after the drop. The jailer-made /firecracker is the
// helper copy; the real binary must never occupy that path, because the
// jailer copies the exec file with a truncating create (a writable mount
// there would let the copy corrupt the managed host binary).
func jailedVMMBinaryPath(jailRoot string) string {
	return filepath.Join(jailRoot, vmmexec.VMMJailDir, vmmexec.VMMJailName)
}

// truncatedSandboxID returns the NodeJanitor-compatible VM id.
func truncatedSandboxID(id string) string {
	if len(id) > firecrackerIDLimit {
		return id[:firecrackerIDLimit]
	}
	return id
}

// Process is the host-side handle of one Firecracker microVM process.
type Process interface {
	PID() int
	Kill() error
	Wait() error
}

// ProcessRunner starts Firecracker processes. It is injectable so tests can
// substitute a fake process.
type ProcessRunner interface {
	Start(ctx context.Context, name string, args []string, logPath string) (Process, error)
}

// WorkingDirRunner starts Firecracker processes with a fixed working
// directory, so relative device paths baked in a snapshot vmstate resolve
// per instance.
type WorkingDirRunner interface {
	StartInDir(ctx context.Context, workingDir, name string, args []string, logPath string) (Process, error)
}

// CgroupRunner starts a process directly in a cgroup v2 leaf. Implementations
// must make placement part of process creation; launching first and moving a
// PID later leaves a window where a VMM can consume the Fastlet cgroup.
type CgroupRunner interface {
	StartInCgroup(ctx context.Context, cgroupPath, workingDir, name string, args []string, logPath string) (Process, error)
}

// ExecProcessRunner starts Firecracker with exec.Command. The VM deliberately
// outlives the caller context: only Kill terminates it. The process stdout
// and stderr are appended to logPath so a failed boot is diagnosable.
type ExecProcessRunner struct{}

// Start launches the process without binding its lifetime to ctx.
func (ExecProcessRunner) Start(_ context.Context, name string, args []string, logPath string) (Process, error) {
	return ExecProcessRunner{}.start("", "", name, args, logPath)
}

// StartInDir launches the process with the given working directory.
func (ExecProcessRunner) StartInDir(_ context.Context, workingDir, name string, args []string, logPath string) (Process, error) {
	return ExecProcessRunner{}.start("", workingDir, name, args, logPath)
}

// StartInCgroup launches the process atomically into cgroupPath.
func (ExecProcessRunner) StartInCgroup(_ context.Context, cgroupPath, workingDir, name string, args []string, logPath string) (Process, error) {
	return ExecProcessRunner{}.start(cgroupPath, workingDir, name, args, logPath)
}

func (ExecProcessRunner) start(cgroupPath, workingDir, name string, args []string, logPath string) (Process, error) {
	command := exec.Command(name, args...)
	if workingDir != "" {
		command.Dir = workingDir
	}
	var logFile *os.File
	if logPath != "" {
		var err error
		logFile, err = os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
		if err != nil {
			return nil, err
		}
		command.Stdout = logFile
		command.Stderr = logFile
	}
	if err := startCommandInCgroup(command, cgroupPath); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, err
	}
	if logFile != nil {
		_ = logFile.Close()
	}
	return &execProcess{command: command}, nil
}

type execProcess struct {
	command *exec.Cmd
}

func (p *execProcess) PID() int {
	if p.command.Process == nil {
		return 0
	}
	return p.command.Process.Pid
}

func (p *execProcess) Kill() error {
	if p.command.Process == nil {
		return nil
	}
	return p.command.Process.Kill()
}

func (p *execProcess) Wait() error {
	if p.command.Process == nil {
		return nil
	}
	return p.command.Wait()
}

// launchConfig is the immutable per-Sandbox Firecracker launch plan.
type launchConfig struct {
	// BinaryPath is the host path of the real firecracker binary: the
	// capability-drop exec target (direct mode names it with --exec, and
	// jailer mode places it in the jail at /vmm/firecracker).
	BinaryPath string
	// ExecFile is the capability-drop exec helper (FSEP-0003): what the
	// jailer execs inside the jail (--exec-file) and what a direct launch
	// starts with the --exec target. Installed with the "firecracker"
	// basename so the jailer-derived jail root stays unchanged. Empty
	// preserves the legacy direct launch of BinaryPath; the driver sets it
	// unconditionally and the launch-time capability assertion fails any
	// create that skipped the drop.
	ExecFile string
	// JailerPath activates the jailer; empty keeps the direct launch.
	JailerPath string
	// ChrootBase is the jailer chroot base (a subdirectory of the StateRoot
	// in the driver); required in jailer mode.
	ChrootBase string
	// NetNSPath is the per-clone slot network namespace path (jailer
	// --netns); required in jailer mode.
	NetNSPath string
	// SandboxID is the full Sandbox identity; the argv id is truncated.
	SandboxID string
	// APIAddress is the host path of the per-Sandbox API Unix socket
	// (direct mode); jailer mode uses the chroot-relative /api.sock.
	APIAddress string
	// WorkingDir is the process working directory. Snapshot restore opens
	// the block device paths baked in the vmstate relative to the
	// Firecracker process cwd (design: each instance resolves "rootfs.img"
	// to its own reflink copy). Direct mode only; the jailer chroot fixes
	// the working directory to the jail root.
	WorkingDir string
	// LogPath receives the firecracker stdout/stderr (empty discards output).
	LogPath string
	// CgroupPath is the writable host path of this Sandbox's cgroup v2 leaf.
	// Empty preserves the legacy process placement.
	CgroupPath string
}

// buildArgv produces the Firecracker command line. In jailer mode the jailer
// consumes --id/--netns/--uid/--gid/--exec-file/--chroot-base-dir and
// forwards the remaining arguments (after --) to firecracker; --id must not
// be repeated after the separator (the jailer passes it on automatically).
// The final process name must stay "firecracker" and the --id value must
// match the NodeJanitor matcher: in jailer mode the exec file is the
// capability-drop helper (installed with the firecracker basename, and the
// helper execs /vmm/firecracker, preserving the basename in argv[0]).
func (c launchConfig) buildArgv() []string {
	if c.JailerPath != "" {
		execFile := c.BinaryPath
		if c.ExecFile != "" {
			execFile = c.ExecFile
		}
		return []string{
			"--id", c.truncatedID(),
			"--netns", c.NetNSPath,
			"--uid", "0",
			"--gid", "0",
			"--exec-file", execFile,
			"--chroot-base-dir", c.ChrootBase,
			"--",
			"--api-sock", jailerChrootAPISock,
			"--log-path", jailerChrootLogPath,
		}
	}
	arguments := []string{
		"--id", c.truncatedID(),
		"--api-sock", c.APIAddress,
	}
	return arguments
}

// truncatedID returns the NodeJanitor-compatible VM id.
func (c launchConfig) truncatedID() string {
	return truncatedSandboxID(c.SandboxID)
}

// launch invokes the firecracker (or jailer) binary with the launch
// configuration.
func launch(ctx context.Context, runner ProcessRunner, config launchConfig) (Process, error) {
	if strings.TrimSpace(config.SandboxID) == "" || strings.TrimSpace(config.APIAddress) == "" {
		return nil, fmt.Errorf("%w: sandbox id and API address are required", ErrInvalidConfig)
	}
	if config.JailerPath != "" {
		if strings.TrimSpace(config.BinaryPath) == "" || strings.TrimSpace(config.ChrootBase) == "" || strings.TrimSpace(config.NetNSPath) == "" {
			return nil, fmt.Errorf("%w: jailer mode requires the firecracker binary, chroot base, and slot netns", ErrInvalidConfig)
		}
		// The jailer derives the jail root from the exec-file basename; with
		// the helper as exec file the basename must match the firecracker
		// layout every Fastlet path consumer computes
		// (<chroot-base>/firecracker/<id>/root).
		if config.ExecFile != "" && filepath.Base(config.ExecFile) != vmmexec.HelperBasename {
			return nil, fmt.Errorf("%w: the capability-drop helper must be installed with the %q basename (the jail root is derived from the exec-file basename)", ErrInvalidConfig, vmmexec.HelperBasename)
		}
		if config.CgroupPath != "" {
			cgroupRunner, ok := runner.(CgroupRunner)
			if !ok {
				return nil, fmt.Errorf("%w: process runner cannot launch into a cgroup", ErrInvalidConfig)
			}
			return cgroupRunner.StartInCgroup(ctx, config.CgroupPath, "", config.JailerPath, config.buildArgv(), config.LogPath)
		}
		return runner.Start(ctx, config.JailerPath, config.buildArgv(), config.LogPath)
	}
	if strings.TrimSpace(config.BinaryPath) == "" {
		return nil, fmt.Errorf("%w: firecracker binary is required", ErrInvalidConfig)
	}
	if !filepath.IsAbs(config.APIAddress) {
		return nil, fmt.Errorf("%w: firecracker API address must be an absolute path", ErrInvalidConfig)
	}
	// Direct launches must also enter the allocated netns; otherwise VMs contend for identically named TAP devices on the host.
	// With the capability-drop helper resolved, the helper is what runs: it
	// drops the fixed capability set and execs the real binary named by
	// --exec, preserving argv and process identity.
	target, args := config.BinaryPath, config.buildArgv()
	if config.ExecFile != "" {
		target = config.ExecFile
		args = append([]string{vmmexec.ExecFlag, config.BinaryPath}, args...)
	}
	name, launchArgs := target, args
	if config.NetNSPath != "" {
		name = "nsenter"
		launchArgs = append([]string{"--net=" + config.NetNSPath, "--", target}, args...)
	}
	if config.CgroupPath != "" {
		cgroupRunner, ok := runner.(CgroupRunner)
		if !ok {
			return nil, fmt.Errorf("%w: process runner cannot launch into a cgroup", ErrInvalidConfig)
		}
		return cgroupRunner.StartInCgroup(ctx, config.CgroupPath, config.WorkingDir, name, launchArgs, config.LogPath)
	}
	if runnerInDir, ok := runner.(WorkingDirRunner); ok && config.WorkingDir != "" {
		return runnerInDir.StartInDir(ctx, config.WorkingDir, name, launchArgs, config.LogPath)
	}
	return runner.Start(ctx, name, launchArgs, config.LogPath)
}

// prepareJailRoot assembles the jail root of a Sandbox before launch: the
// instance rootfs reflink copy at the chroot root (so the relative
// "rootfs.img" path baked in the vmstate resolves inside the chroot), the
// real Firecracker binary reflink-or-copied at /vmm/firecracker (the
// capability-drop helper execs it; the jailer-made /firecracker is the
// helper copy, so the real binary must live elsewhere — see
// jailedVMMBinaryPath), and hard links of the golden snapshot files under
// snapshots/ (restore addresses them with the chroot-relative /snapshots/
// paths; memory.snap is COW-read, so sharing the cached file is safe). Hard
// links fall back to copies across filesystems. A stale jail root from a
// crashed run is removed first.
func prepareJailRoot(jailRoot, cachedRootfs, vmstatePath, memoryPath, vmmBinary string) error {
	if jailRoot == "" || filepath.Base(jailRoot) != jailerChrootRootDir {
		return fmt.Errorf("%w: invalid jail root %q", ErrInvalidConfig, jailRoot)
	}
	// A stale jail root may hold a surviving spill bind: unmount it first,
	// or the RemoveAll below would descend into the SHARED spill root and
	// delete other sandboxes' dumps. The same applies to the real VMM
	// binary bind of the RootfsOnly/OCI paths.
	_ = unmountPath(filepath.Join(jailRoot, jailerSpillDirName))
	_ = unmountPath(jailedVMMBinaryPath(jailRoot))
	_ = os.RemoveAll(filepath.Dir(jailRoot))
	if err := os.MkdirAll(filepath.Join(jailRoot, jailerChrootSnapshotsDir), 0o750); err != nil {
		return fmt.Errorf("create jail root: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(jailRoot, vmmexec.VMMJailDir), 0o750); err != nil {
		return fmt.Errorf("create jail VMM directory: %w", err)
	}
	if err := copyReflinkOrCopy(cachedRootfs, filepath.Join(jailRoot, rootfsImageName)); err != nil {
		return fmt.Errorf("copy instance rootfs into the jail root: %w", err)
	}
	if err := copyReflinkOrCopy(vmmBinary, jailedVMMBinaryPath(jailRoot)); err != nil {
		return fmt.Errorf("place the VMM binary in the jail root: %w", err)
	}
	// The plain-copy fallback of copyReflinkOrCopy would leave the binary
	// non-executable (0640); the helper execve needs the execute bit.
	if err := os.Chmod(jailedVMMBinaryPath(jailRoot), 0o750); err != nil {
		return fmt.Errorf("make the jail VMM binary executable: %w", err)
	}
	for source, name := range map[string]string{
		vmstatePath: vmstateSnapshotName,
		memoryPath:  memorySnapshotName,
	} {
		if err := linkOrCopy(source, filepath.Join(jailRoot, jailerChrootSnapshotsDir, name)); err != nil {
			return fmt.Errorf("link %s into the jail root: %w", name, err)
		}
	}
	return nil
}

// bindFileReadOnly bind-mounts a host file over an empty target file
// read-only. The real VMM binary enters the jail this way: read-only so the
// jailed VMM cannot rewrite the managed host binary through the mount.
func bindFileReadOnly(source, target string) error {
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return bindMountReadOnly(source, target)
}

// linkOrCopy hard-links source to target and falls back to a full copy when
// the filesystem does not support hard links across devices.
func linkOrCopy(source, target string) error {
	if err := os.Link(source, target); err == nil {
		return nil
	}
	klog.V(4).InfoS("hard link unavailable, copying", "source", source, "target", target)
	return copyFile(source, target)
}
