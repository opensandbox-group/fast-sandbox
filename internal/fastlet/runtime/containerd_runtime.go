package runtime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"fast-sandbox/internal/api"
	"fast-sandbox/internal/fastlet/infra"
	"fast-sandbox/internal/runtimecatalog"

	runtimeoptions "github.com/containerd/containerd/api/types/runtimeoptions/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

type ContainerdRuntime struct {
	socketPath         string
	client             *containerd.Client
	cgroupPath         string
	netnsPath          string
	fastletPodName     string
	fastletPodUID      string
	fastletNamespace   string
	infraMgr           *infra.Manager
	allowedPluginPaths []string
	runtimeType        RuntimeType   // runtime type identifier
	config             RuntimeConfig // cached runtime configuration
}

const (
	// defaultOperationTimeout is the timeout for container operations.
	// Set to 120s to accommodate secure runtimes (gVisor, Kata) which may take
	// longer to create/start sandbox containers than standard runc.
	// gVisor in particular can take 60-90 seconds in nested virtualization environments.
	defaultOperationTimeout = 120 * time.Second
	waitStopTimeout         = 10 * time.Second
)

func newContainerdRuntime(rt RuntimeType) Runtime {
	return newContainerdRuntimeWithConfig(rt, GetRuntimeConfig(rt))
}

func newContainerdRuntimeWithConfig(rt RuntimeType, cfg RuntimeConfig) Runtime {
	return &ContainerdRuntime{
		runtimeType: rt,
		config:      cfg,
	}
}

// Initialize init containerd client
func (r *ContainerdRuntime) Initialize(ctx context.Context, socketPath string) error {
	r.socketPath = socketPath
	if r.socketPath == "" {
		r.socketPath = "/run/containerd/containerd.sock"
	}
	klog.InfoS("Initializing runtime", "handler", r.config.Handler)

	ctx, cancel := context.WithTimeout(ctx, defaultOperationTimeout)
	defer cancel()

	client, err := containerd.New(r.socketPath, containerd.WithDefaultNamespace("k8s.io"))
	if err != nil {
		return fmt.Errorf("failed to create containerd client: %w", err)
	}

	r.client = client
	r.fastletPodName = os.Getenv("POD_NAME")
	r.fastletPodUID = os.Getenv("POD_UID")

	allowedPaths := os.Getenv("ALLOWED_PLUGIN_PATHS")
	if allowedPaths != "" {
		r.allowedPluginPaths = strings.Split(allowedPaths, ":")
	} else {
		infraPodPath := os.Getenv("INFRA_DIR_IN_POD")
		if infraPodPath == "" {
			infraPodPath = "/opt/fast-sandbox/infra"
		}
		r.allowedPluginPaths = []string{infraPodPath}
	}

	infraPodPath := os.Getenv("INFRA_DIR_IN_POD")
	if infraPodPath == "" {
		infraPodPath = "/opt/fast-sandbox/infra"
	}
	r.infraMgr = infra.NewManager(infraPodPath)

	if err := r.discoverCgroupPath(); err != nil {
		klog.ErrorS(err, "Failed to discover cgroup path")
		r.cgroupPath = ""
	}

	if err := r.discoverNetNSPath(ctx); err != nil {
		klog.ErrorS(err, "Failed to discover network namespace")
	}

	return nil
}

func (r *ContainerdRuntime) discoverCgroupPath() error {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "0::") {
			r.cgroupPath = strings.TrimPrefix(line, "0::")
			return nil
		}
		parts := strings.Split(line, ":")
		if len(parts) == 3 && (strings.Contains(parts[1], "pids") || strings.Contains(parts[1], "cpu")) {
			r.cgroupPath = parts[2]
			return nil
		}
	}
	return fmt.Errorf("cgroup path not found")
}

func (r *ContainerdRuntime) discoverNetNSPath(ctx context.Context) error {
	if r.cgroupPath == "" {
		return fmt.Errorf("cgroup path is required")
	}
	var containerID string
	if strings.Contains(r.cgroupPath, "cri-containerd-") {
		parts := strings.Split(r.cgroupPath, "cri-containerd-")
		containerID = strings.Split(parts[1], ".")[0]
	} else if strings.Contains(r.cgroupPath, "cri-containerd:") {
		parts := strings.Split(r.cgroupPath, "cri-containerd:")
		containerID = parts[len(parts)-1]
	} else if strings.Contains(r.cgroupPath, "kubepods") {
		parts := strings.Split(strings.Trim(r.cgroupPath, "/"), "/")
		containerID = parts[len(parts)-1]
	} else {
		return fmt.Errorf("could not parse ID")
	}

	ctx = namespaces.WithNamespace(ctx, "k8s.io")
	container, err := r.client.LoadContainer(ctx, containerID)
	if err != nil {
		return err
	}
	spec, err := container.Spec(ctx)
	if err != nil {
		return err
	}
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == specs.NetworkNamespace {
			if ns.Path != "" {
				r.netnsPath = ns.Path
				return nil
			}
		}
	}
	return fmt.Errorf("netns not found")
}

func (r *ContainerdRuntime) CreateSandbox(ctx context.Context, config *api.SandboxSpec) (*SandboxMetadata, error) {
	totalStart := time.Now()

	klog.InfoS("Creating sandbox", "sandbox", config.SandboxID, "image", config.Image, "runtime", r.config.Handler, "netns", r.netnsPath)
	ctx, cancel := context.WithTimeout(ctx, defaultOperationTimeout)
	defer cancel()
	ctx = namespaces.WithNamespace(ctx, "k8s.io")

	// 1. Image preparation
	pullStart := time.Now()
	image, err := r.prepareImage(ctx, config.Image)
	if err != nil {
		klog.ErrorS(err, "Failed to prepare image", "sandbox", config.SandboxID)
		return nil, err
	}
	pullDuration := time.Since(pullStart)

	containerID := config.SandboxID
	specOpts, err := r.prepareSpecOpts(config, image)
	if err != nil {
		return nil, fmt.Errorf("invalid sandbox resource profile: %w", err)
	}
	labels := r.prepareLabels(config)

	// 2. Create container
	createStart := time.Now()
	klog.InfoS("Creating containerd container object", "sandbox", containerID)

	container, err := r.client.NewContainer(
		ctx,
		containerID,
		containerd.WithImage(image),
		containerd.WithNewSnapshot(snapShotName(containerID), image),
		containerd.WithRuntime(r.config.Handler, r.getRuntimeOptions()),
		containerd.WithNewSpec(specOpts...),
		containerd.WithContainerLabels(labels),
	)
	if err != nil {
		klog.ErrorS(err, "Failed to create container object", "sandbox", containerID)
		return nil, fmt.Errorf("failed to create container: %w", err)
	}
	createDuration := time.Since(createStart)

	logDir := "/var/log/fast-sandbox"
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log dir: %w", err)
	}
	logPath := filepath.Join(logDir, fmt.Sprintf("%s.log", containerID))

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file: %w", err)
	}

	// 3. Start container
	startStart := time.Now()
	klog.InfoS("Creating containerd task", "sandbox", containerID)

	// Build CIO options based on runtime configuration
	var cioOpts []cio.Opt
	if r.config.NeedsTTY {
		cioOpts = append(cioOpts, cio.WithTerminal)
	}
	cioOpts = append(cioOpts, cio.WithStreams(nil, logFile, logFile))

	var taskOpts []containerd.NewTaskOpts
	if r.config.RuntimePath != "" {
		taskOpts = append(taskOpts, containerd.WithRuntimePath(r.config.RuntimePath))
	}

	task, err := container.NewTask(ctx, cio.NewCreator(cioOpts...), taskOpts...)
	if err != nil {
		klog.ErrorS(err, "Failed to create containerd task", "sandbox", containerID, "logPath", logPath)
		logFile.Close()
		_ = container.Delete(ctx, containerd.WithSnapshotCleanup)
		return nil, fmt.Errorf("failed to create task: %w", err)
	}

	klog.InfoS("Starting containerd task", "sandbox", containerID, "pid", task.Pid())
	if err = task.Start(ctx); err != nil {
		klog.ErrorS(err, "Failed to start containerd task", "sandbox", containerID)
		_, _ = task.Delete(ctx, containerd.WithProcessKill)
		_ = container.Delete(ctx, containerd.WithSnapshotCleanup)
		return nil, fmt.Errorf("failed to start task: %w", err)
	}
	startDuration := time.Since(startStart)

	totalDuration := time.Since(totalStart)

	klog.InfoS("Runtime CreateSandbox timing",
		"sandboxID", config.SandboxID,
		"total_ms", totalDuration.Milliseconds(),
		"pull_ms", pullDuration.Milliseconds(),
		"create_ms", createDuration.Milliseconds(),
		"start_ms", startDuration.Milliseconds())

	metadata := &SandboxMetadata{
		SandboxSpec: *config,
		ContainerID: containerID,
		Phase:       "running",
		CreatedAt:   time.Now().Unix(),
		PID:         int(task.Pid()),
	}
	klog.InfoS("Sandbox created successfully", "sandbox", containerID, "pid", task.Pid())
	return metadata, nil
}

// EnsureSandbox is idempotent for a Sandbox runtime identity. It returns the
// existing managed runtime when a retry observes an already-created Sandbox.
func (r *ContainerdRuntime) EnsureSandbox(ctx context.Context, config *api.SandboxSpec) (*SandboxMetadata, error) {
	existing, err := r.InspectSandbox(ctx, config.SandboxID)
	if err == nil {
		if err := validateExistingRuntimeProfile(existing, config); err != nil {
			return nil, err
		}
		return existing, nil
	}
	if !errors.Is(err, ErrSandboxNotFound) {
		return nil, err
	}
	return r.CreateSandbox(ctx, config)
}

func validateExistingRuntimeProfile(existing *SandboxMetadata, requested *api.SandboxSpec) error {
	if existing.RuntimeProfileHash != requested.RuntimeProfileHash ||
		existing.ResourceProfileHash != requested.ResourceProfileHash ||
		existing.CPU != requested.CPU || existing.Memory != requested.Memory || existing.PIDs != requested.PIDs {
		return fmt.Errorf("%w: existing runtime identity %q has different runtime/resource profile", ErrSandboxProfileMismatch, requested.SandboxID)
	}
	return nil
}

func (r *ContainerdRuntime) prepareImage(ctx context.Context, imageName string) (containerd.Image, error) {
	image, err := r.client.GetImage(ctx, imageName)
	if err != nil {
		image, err = r.client.Pull(ctx, imageName, containerd.WithPullUnpack)
		if err != nil {
			return nil, err
		}
	}
	return image, nil
}

func (r *ContainerdRuntime) prepareSpecOpts(config *api.SandboxSpec, image containerd.Image) ([]oci.SpecOpts, error) {
	originalArgs := append(config.Command, config.Args...)

	var mounts []specs.Mount
	finalArgs := originalArgs

	if r.infraMgr != nil {
		//plugins := r.infraMgr.GetPlugins()
		//for _, p := range plugins {
		//	hostPath := r.infraMgr.GetHostPath(p.BinName)
		//	if hostPath == "" {
		//		continue
		//	}
		//
		//	if !r.isPluginPathAllowed(hostPath) {
		//		klog.InfoS("SECURITY: Plugin path is not in allowed paths, skipping", "path", hostPath)
		//		continue
		//	}
		//
		//	if _, err := os.Stat(hostPath); err != nil {
		//		klog.InfoS("Warning: Plugin binary not accessible", "path", hostPath, "err", err)
		//		continue
		//	}
		//
		//	mounts = append(mounts, specs.Mount{
		//		Source:      hostPath,
		//		Destination: p.ContainerPath,
		//		Type:        "bind",
		//		Options:     []string{"ro", "rbind", "nosuid", "nodev"}, // 只读绑定，添加安全选项
		//	})
		//
		//	if p.IsWrapper {
		//		wrapped := []string{p.ContainerPath, "--"}
		//		finalArgs = append(wrapped, finalArgs...)
		//	}
		//}
	}

	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(image),
		oci.WithProcessArgs(finalArgs...),
		oci.WithEnv(envMapToSlice(config.Env)),
	}
	resourceOpts, err := sandboxResourceSpecOpts(config)
	if err != nil {
		return nil, err
	}
	specOpts = append(specOpts, resourceOpts...)

	// Add TTY option if required by runtime (e.g., gVisor)
	if r.config.NeedsTTY {
		specOpts = append(specOpts, oci.WithTTY)
	}

	if config.WorkingDir != "" {
		specOpts = append(specOpts, oci.WithProcessCwd(config.WorkingDir))
	}

	if len(mounts) > 0 {
		specOpts = append(specOpts, oci.WithMounts(mounts))
	}

	// Always add network namespace - gVisor requires it.
	// If netnsPath is set, use the host's network namespace (for sharing with fastlet).
	// If netnsPath is empty, create a new isolated network namespace.
	specOpts = append(specOpts, oci.WithLinuxNamespace(specs.LinuxNamespace{
		Type: specs.NetworkNamespace,
	}))

	//if r.netnsPath != "" {
	//	specOpts = append(specOpts, oci.WithLinuxNamespace(specs.LinuxNamespace{
	//		Type: specs.NetworkNamespace,
	//		Path: r.netnsPath,
	//	}))
	//} else {
	//	// Create a new network namespace for isolation
	//	specOpts = append(specOpts, oci.WithLinuxNamespace(specs.LinuxNamespace{
	//		Type: specs.NetworkNamespace,
	//	}))
	//}

	return specOpts, nil
}

func sandboxResourceSpecOpts(config *api.SandboxSpec) ([]oci.SpecOpts, error) {
	var opts []oci.SpecOpts
	if config.CPU != "" {
		cpu, err := resource.ParseQuantity(config.CPU)
		if err != nil {
			return nil, fmt.Errorf("cpu %q: %w", config.CPU, err)
		}
		if cpu.Sign() <= 0 {
			return nil, fmt.Errorf("cpu must be greater than zero")
		}
		const period uint64 = 100000
		quota := cpu.MilliValue() * int64(period) / 1000
		if quota < 1000 {
			quota = 1000
		}
		opts = append(opts, oci.WithCPUCFS(quota, period))
	}
	if config.Memory != "" {
		memory, err := resource.ParseQuantity(config.Memory)
		if err != nil {
			return nil, fmt.Errorf("memory %q: %w", config.Memory, err)
		}
		if memory.Sign() <= 0 {
			return nil, fmt.Errorf("memory must be greater than zero")
		}
		opts = append(opts, oci.WithMemoryLimit(uint64(memory.Value())))
	}
	if config.PIDs > 0 {
		opts = append(opts, oci.WithPidsLimit(config.PIDs))
	}
	return opts, nil
}

func (r *ContainerdRuntime) isPluginPathAllowed(pluginPath string) bool {
	resolvedPath, err := filepath.EvalSymlinks(pluginPath)
	if err != nil {
		return false
	}

	for _, allowedPath := range r.allowedPluginPaths {
		cleanAllowed := filepath.Clean(allowedPath)
		if strings.HasPrefix(resolvedPath, cleanAllowed+string(filepath.Separator)) || resolvedPath == cleanAllowed {
			return true
		}
	}
	return false
}

// getRuntimeOptions returns runtime-specific options for containerd.
// It uses config.OptionsType and config.ConfigPath to build the options.
func (r *ContainerdRuntime) getRuntimeOptions() *runtimeoptions.Options {
	// If OptionsType is set, include TypeUrl (required for gVisor)
	if r.config.OptionsType != "" {
		return &runtimeoptions.Options{
			TypeUrl:    r.config.OptionsType,
			ConfigPath: r.config.ConfigPath,
		}
	}

	// For other runtimes, only include ConfigPath if set
	if r.config.ConfigPath != "" {
		return &runtimeoptions.Options{
			ConfigPath: r.config.ConfigPath,
		}
	}

	return nil
}

func (r *ContainerdRuntime) prepareLabels(config *api.SandboxSpec) map[string]string {
	return map[string]string{
		"fast-sandbox.io/managed":               "true",
		"fast-sandbox.io/fastlet-name":          r.fastletPodName,
		"fast-sandbox.io/fastlet-uid":           r.fastletPodUID,
		"fast-sandbox.io/namespace":             r.fastletNamespace,
		"fast-sandbox.io/id":                    config.SandboxID,
		"fast-sandbox.io/claim-uid":             config.ClaimUID,
		"fast-sandbox.io/sandbox-name":          config.ClaimName,
		"fast-sandbox.io/runtime-profile-hash":  config.RuntimeProfileHash,
		"fast-sandbox.io/resource-profile-hash": config.ResourceProfileHash,
		"fast-sandbox.io/resource-cpu":          config.CPU,
		"fast-sandbox.io/resource-memory":       config.Memory,
		"fast-sandbox.io/resource-pids":         strconv.FormatInt(config.PIDs, 10),
	}
}

func (r *ContainerdRuntime) SetNamespace(ns string) {
	r.fastletNamespace = ns
}

func (r *ContainerdRuntime) DeleteSandbox(ctx context.Context, sandboxID string) error {
	ctx = namespaces.WithNamespace(ctx, "k8s.io")
	snapshotName := snapShotName(sandboxID)

	container, err := r.client.LoadContainer(ctx, sandboxID)
	if err != nil {
		klog.ErrorS(err, "Failed to load container", "sandbox", sandboxID)
		// Container load failed, try to clean up orphaned snapshot
		snapErr := r.client.SnapshotService("").Remove(ctx, snapshotName)
		if snapErr != nil {
			klog.InfoS("Snapshot cleanup", "sandbox", sandboxID, "err", snapErr)
		}
		return JoinErrors(err, snapErr)
	}

	task, err := container.Task(ctx, nil)
	if err != nil {
		// Task doesn't exist, delete container and clean up snapshot
		delErr := container.Delete(ctx)
		snapErr := r.forceCleanupSnapshot(ctx, snapshotName)
		return JoinErrors(err, delErr, snapErr)
	}

	// Task exists, try graceful shutdown first
	if taskKillErr := task.Kill(ctx, syscall.SIGTERM); taskKillErr != nil {
		exitS, taskDelErr := task.Delete(ctx, containerd.WithProcessKill)
		containerDelErr := container.Delete(ctx)
		snapErr := r.forceCleanupSnapshot(ctx, snapshotName)
		klog.InfoS("Failed to kill task, force delete", "sandbox", sandboxID, "taskKillErr", taskKillErr, "taskDelErr", taskDelErr, "containerDelErr", containerDelErr, "snapErr", snapErr, "exitStatus", exitS)
		return JoinErrors(taskKillErr, taskDelErr, containerDelErr, snapErr)
	}

	waitCh, err := task.Wait(ctx)
	if err != nil {
		klog.InfoS("Failed to wait for task", "sandbox", sandboxID, "err", err)
	}

	var taskKillErr error
	select {
	case <-waitCh:
	case <-time.After(waitStopTimeout):
		klog.InfoS("Sandbox did not exit after timeout, sending SIGKILL", "sandbox", sandboxID, "timeout", waitStopTimeout)
		taskKillErr = task.Kill(ctx, syscall.SIGKILL)
		<-waitCh
	}

	exitS, taskDelErr := task.Delete(ctx, containerd.WithProcessKill)
	containerDelErr := container.Delete(ctx)
	snapErr := r.forceCleanupSnapshot(ctx, snapshotName)
	klog.InfoS("Task delete completed", "sandbox", sandboxID, "taskKillErr", taskKillErr, "taskDelErr", taskDelErr, "containerDelErr", containerDelErr, "snapErr", snapErr, "exitStatus", exitS)
	return JoinErrors(taskDelErr, containerDelErr, snapErr)
}

// forceCleanupSnapshot explicitly removes the snapshot, ignoring "not found" errors
func (r *ContainerdRuntime) forceCleanupSnapshot(ctx context.Context, snapshotName string) error {
	snapErr := r.client.SnapshotService("").Remove(ctx, snapshotName)
	// Ignore "not found" errors - snapshot may have already been cleaned up
	if snapErr != nil && !strings.Contains(snapErr.Error(), "not found") && !strings.Contains(snapErr.Error(), "no such") {
		return snapErr
	}
	return nil
}

func (r *ContainerdRuntime) GetSandboxStatus(ctx context.Context, sandboxID string) (string, error) {
	ctx = namespaces.WithNamespace(ctx, "k8s.io")
	container, err := r.client.LoadContainer(ctx, sandboxID)
	if err != nil {
		// 容器不存在
		return "terminated", nil
	}

	task, err := container.Task(ctx, nil)
	if err != nil {
		// 任务不存在，容器已停止
		return "stopped", nil
	}

	status, err := task.Status(ctx)
	if err != nil {
		return "unknown", err
	}

	return string(status.Status), nil
}

func (r *ContainerdRuntime) InspectSandbox(ctx context.Context, sandboxID string) (*SandboxMetadata, error) {
	if r.client == nil {
		return nil, ErrRuntimeNotInitialized
	}
	ctx = namespaces.WithNamespace(ctx, "k8s.io")
	container, err := r.client.LoadContainer(ctx, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSandboxNotFound, err)
	}
	info, err := container.Info(ctx)
	if err != nil {
		return nil, err
	}
	metadata := &SandboxMetadata{
		SandboxSpec: api.SandboxSpec{
			SandboxID:           sandboxID,
			ClaimUID:            info.Labels["fast-sandbox.io/claim-uid"],
			ClaimName:           info.Labels["fast-sandbox.io/sandbox-name"],
			Image:               info.Image,
			CPU:                 info.Labels["fast-sandbox.io/resource-cpu"],
			Memory:              info.Labels["fast-sandbox.io/resource-memory"],
			RuntimeProfileHash:  info.Labels["fast-sandbox.io/runtime-profile-hash"],
			ResourceProfileHash: info.Labels["fast-sandbox.io/resource-profile-hash"],
		},
		ContainerID: sandboxID,
		CreatedAt:   info.CreatedAt.Unix(),
		Phase:       "stopped",
	}
	metadata.PIDs, _ = strconv.ParseInt(info.Labels["fast-sandbox.io/resource-pids"], 10, 64)
	if task, taskErr := container.Task(ctx, nil); taskErr == nil {
		metadata.PID = int(task.Pid())
		if status, statusErr := task.Status(ctx); statusErr == nil {
			metadata.Phase = string(status.Status)
		}
	}
	return metadata, nil
}

func (r *ContainerdRuntime) ListManagedSandboxes(ctx context.Context) ([]*SandboxMetadata, error) {
	if r.client == nil {
		return nil, ErrRuntimeNotInitialized
	}
	ctx = namespaces.WithNamespace(ctx, "k8s.io")
	containers, err := r.client.Containers(ctx)
	if err != nil {
		return nil, err
	}
	managed := make([]*SandboxMetadata, 0, len(containers))
	for _, container := range containers {
		info, err := container.Info(ctx)
		if err != nil || info.Labels["fast-sandbox.io/managed"] != "true" {
			continue
		}
		if r.fastletPodUID != "" && info.Labels["fast-sandbox.io/fastlet-uid"] != r.fastletPodUID {
			continue
		}
		metadata, err := r.InspectSandbox(ctx, container.ID())
		if err != nil {
			continue
		}
		managed = append(managed, metadata)
	}
	return managed, nil
}

func (r *ContainerdRuntime) ProbeCapabilities(ctx context.Context) CapabilityReport {
	report := CapabilityReport{Runtime: r.runtimeType, State: runtimecatalog.CapabilityDegraded}
	if profile, err := sharedRuntimeCatalog.Resolve(r.runtimeType); err == nil {
		report.ProfileHash = profile.ProfileHash
	}
	if r.client == nil {
		report.Reason = "RuntimeDriverNotInitialized"
		report.Message = "containerd client is not initialized"
		return report
	}
	if _, err := r.client.Version(ctx); err != nil {
		report.Reason = "ContainerdUnavailable"
		report.Message = err.Error()
		return report
	}
	report.State = runtimecatalog.CapabilityReady
	report.Reason = "RuntimeDriverReady"
	report.Message = "containerd runtime driver is ready"
	return report
}

func (r *ContainerdRuntime) ListImages(ctx context.Context) ([]string, error) {
	ctx = namespaces.WithNamespace(ctx, "k8s.io")
	images, err := r.client.ListImages(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, img := range images {
		names = append(names, img.Name())
	}
	return names, nil
}

func (r *ContainerdRuntime) PullImage(ctx context.Context, image string) error {
	ctx = namespaces.WithNamespace(ctx, "k8s.io")
	_, err := r.client.GetImage(ctx, image)
	if err == nil {
		return nil
	}
	_, err = r.client.Pull(ctx, image, containerd.WithPullUnpack)
	return err
}

func (r *ContainerdRuntime) Close() error {
	if r.client != nil {
		return r.client.Close()
	}
	return nil
}

func (r *ContainerdRuntime) GetSandboxLogs(ctx context.Context, sandboxID string, follow bool, stdout io.Writer) error {
	logPath := filepath.Join("/var/log/fast-sandbox", fmt.Sprintf("%s.log", sandboxID))
	file, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("log file not found")
		}
		return err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	// 读取现有内容
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			if _, wErr := stdout.Write([]byte(line)); wErr != nil {

				return wErr

			}
		}
		if err != nil {
			if err == io.EOF {

				break

			}
			return err
		}
	}
	if !follow {

		return nil

	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			for {
				line, err := reader.ReadString('\n')
				if line != "" {
					if _, wErr := stdout.Write([]byte(line)); wErr != nil {

						return wErr

					}
				}
				if err == io.EOF {

					break

				}
				if err != nil {

					return err

				}
			}
		}
	}
}

func envMapToSlice(env map[string]string) []string {
	var res []string
	for k, v := range env {
		res = append(res, fmt.Sprintf("%s=%s", k, v))
	}
	return res
}

func snapShotName(containerID string) string {
	return containerID + "-snapshot"
}
