package containerd

import (
	"context"
	"errors"
	dataplane "fast-sandbox/internal/dataplane/contract"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	"fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	runtimecontract "fast-sandbox/internal/runtime/contract"

	runtimeoptions "github.com/containerd/containerd/api/types/runtimeoptions/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

type Driver struct {
	socketPath         string
	client             *containerd.Client
	fastletPodName     string
	fastletPodUID      string
	fastletNamespace   string
	infraMgr           *infra.Manager
	runtimeName        apiv1alpha1.RuntimeName // runtime profile identifier
	runtimeProfileHash string
	config             RuntimeConfig // cached runtime configuration
	networkManager     *fastletnetwork.Manager
}

const (
	// defaultOperationTimeout is the timeout for container operations.
	// Set to 120s to accommodate secure runtimes (gVisor, Kata) which may take
	// longer to create/start sandbox containers than standard runc.
	// gVisor in particular can take 60-90 seconds in nested virtualization environments.
	defaultOperationTimeout = 120 * time.Second
	waitStopTimeout         = 10 * time.Second
)

func New(profile runtimecatalog.RuntimeProfile) (*Driver, error) {
	if profile.Containerd == nil {
		return nil, fmt.Errorf("containerd runtime profile %q has no private configuration", profile.Name)
	}
	config := RuntimeConfig{
		Handler: profile.Containerd.Handler, RuntimePath: profile.Containerd.RuntimePath,
		ConfigPath: profile.Containerd.ConfigPath, NeedsTTY: profile.Containerd.NeedsTTY,
		OptionsType: profile.Containerd.OptionsType,
	}
	return newWithConfig(profile.Name, profile.ProfileHash, config), nil
}

func newWithConfig(rt apiv1alpha1.RuntimeName, profileHash string, cfg RuntimeConfig) *Driver {
	return &Driver{
		runtimeName:        rt,
		runtimeProfileHash: profileHash,
		config:             cfg,
	}
}

// Initialize init containerd client
func (r *Driver) Initialize(ctx context.Context, socketPath string) error {
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

	return nil
}

func (r *Driver) CreateSandbox(ctx context.Context, config *fastletapi.SandboxSpec) (*SandboxMetadata, error) {
	totalStart := time.Now()
	logger := klog.FromContext(ctx).WithValues("sandbox_id", config.SandboxID)

	logger.Info("Creating sandbox", "image", config.Image, "runtime", r.config.Handler, "netns", config.NetworkNamespacePath)
	ctx, cancel := context.WithTimeout(ctx, defaultOperationTimeout)
	defer cancel()
	ctx = namespaces.WithNamespace(ctx, "k8s.io")

	// 1. Image preparation
	pullStart := time.Now()
	image, err := r.prepareImage(ctx, config.Image)
	if err != nil {
		logger.Error(err, "Failed to prepare image")
		return nil, err
	}
	pullDuration := time.Since(pullStart)

	containerID := config.SandboxID
	specOpts, infraInstance, err := r.prepareSpecOpts(ctx, config, image)
	if err != nil {
		return nil, fmt.Errorf("invalid sandbox resource profile: %w", err)
	}
	created := false
	if infraInstance != nil {
		defer func() {
			if !created {
				_ = r.infraMgr.RemoveInstance(config)
			}
		}()
	}
	labels := r.prepareLabels(config)

	// 2. Create container
	createStart := time.Now()
	logger.Info("Creating containerd container object")

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
		logger.Error(err, "Failed to create container object")
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
	logger.Info("Creating containerd task")

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
		logger.Error(err, "Failed to create containerd task", "logPath", logPath)
		logFile.Close()
		_ = container.Delete(ctx, containerd.WithSnapshotCleanup)
		return nil, fmt.Errorf("failed to create task: %w", err)
	}

	logger.Info("Starting containerd task", "pid", task.Pid())
	if err = task.Start(ctx); err != nil {
		logger.Error(err, "Failed to start containerd task")
		_, _ = task.Delete(ctx, containerd.WithProcessKill)
		_ = container.Delete(ctx, containerd.WithSnapshotCleanup)
		return nil, fmt.Errorf("failed to start task: %w", err)
	}
	userProcessStartedAt, userProcessStartSource := userProcessStartAfterTaskStart(infraInstance, time.Now())
	startDuration := time.Since(startStart)

	totalDuration := time.Since(totalStart)

	logger.Info("Runtime CreateSandbox timing",
		"total_ms", totalDuration.Milliseconds(),
		"pull_ms", pullDuration.Milliseconds(),
		"create_ms", createDuration.Milliseconds(),
		"start_ms", startDuration.Milliseconds())

	metadata := &SandboxMetadata{
		SandboxSpec:            *config,
		ContainerID:            containerID,
		Phase:                  "running",
		CreatedAt:              time.Now().Unix(),
		PID:                    int(task.Pid()),
		UserProcessStartedAt:   userProcessStartedAt,
		UserProcessStartSource: userProcessStartSource,
	}
	if infraInstance != nil {
		metadata.InfraServices = append([]infra.ServiceEndpoint(nil), infraInstance.Services...)
		metadata.InfraUpstreamHeadersByPort = infra.UpstreamHeadersByServicePort(infraInstance.Services, infraInstance.UpstreamHeaders)
	}
	created = true
	logger.Info("Sandbox created successfully", "pid", task.Pid())
	return metadata, nil
}

// EnsureSandbox is idempotent for a Sandbox runtime identity. It returns the
// existing managed runtime when a retry observes an already-created Sandbox.
func (r *Driver) EnsureSandbox(ctx context.Context, config *fastletapi.SandboxSpec) (*SandboxMetadata, error) {
	existing, err := r.InspectSandbox(ctx, config.SandboxID)
	if err == nil {
		if sameRuntimeIdentity(existing, config) {
			if err := validateExistingRuntimeProfile(existing, config); err != nil {
				return nil, err
			}
			existing.UserProcessStartSource = fastletapi.UserProcessStartExistingRuntime
			return existing, nil
		}
		klog.InfoS("Replacing stale runtime owned by a previous Sandbox instance",
			"sandbox", config.SandboxID,
			"existingFastletPodUID", existing.FastletPodUID,
			"requestedFastletPodUID", config.FastletPodUID,
			"existingInstanceGeneration", existing.InstanceGeneration,
			"requestedInstanceGeneration", config.InstanceGeneration)
		if err := r.DeleteSandbox(ctx, config.SandboxID); err != nil {
			return nil, fmt.Errorf("replace stale Sandbox runtime: %w", err)
		}
		err = ErrSandboxNotFound
	}
	if !errors.Is(err, ErrSandboxNotFound) {
		return nil, err
	}
	createConfig := *config
	var owner fastletnetwork.Owner
	if r.networkManager != nil {
		owner = networkOwner(config)
		slot, acquireErr := r.networkManager.Acquire(ctx, owner)
		if acquireErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrNetworkUnavailable, acquireErr)
		}
		createConfig.NetworkSlotID = slot.ID
		createConfig.NetworkNamespacePath = slot.HostNetNSPath
		createConfig.NetworkIP = slot.IP
		createConfig.NetworkGateway = slot.Gateway
		createConfig.NetworkDNSPath = slot.DNSPath
	}
	metadata, createErr := r.CreateSandbox(ctx, &createConfig)
	if createErr != nil && r.networkManager != nil {
		releaseErr := r.networkManager.Release(ctx, owner)
		return nil, errors.Join(createErr, releaseErr)
	}
	return metadata, createErr
}

func userProcessStartAfterTaskStart(instance *infra.PreparedInstance, observedAt time.Time) (time.Time, fastletapi.UserProcessStartSource) {
	if instance != nil && instance.WrapperRequired {
		return time.Time{}, fastletapi.UserProcessStartSandboxInitUnreported
	}
	return observedAt, fastletapi.UserProcessStartRuntimeDirect
}

func validateExistingRuntimeProfile(existing *SandboxMetadata, requested *fastletapi.SandboxSpec) error {
	return runtimecontract.ValidateProfile(existing, requested)
}

func sameRuntimeIdentity(existing *SandboxMetadata, requested *fastletapi.SandboxSpec) bool {
	if existing == nil || requested == nil {
		return false
	}
	return existing.SandboxID == requested.SandboxID &&
		existing.ClaimUID == requested.ClaimUID &&
		existing.ClaimNamespace == requested.ClaimNamespace &&
		existing.ClaimName == requested.ClaimName &&
		existing.FastletPodUID == requested.FastletPodUID &&
		existing.InstanceGeneration == requested.InstanceGeneration &&
		existing.RuntimeInstanceID == requested.RuntimeInstanceID &&
		existing.AssignmentAttempt == requested.AssignmentAttempt
}

func (r *Driver) prepareImage(ctx context.Context, imageName string) (containerd.Image, error) {
	image, err := r.client.GetImage(ctx, imageName)
	if err != nil {
		image, err = r.client.Pull(ctx, imageName, containerd.WithPullUnpack)
		if err != nil {
			return nil, err
		}
	}
	return image, nil
}

func (r *Driver) prepareSpecOpts(ctx context.Context, config *fastletapi.SandboxSpec, image containerd.Image) ([]oci.SpecOpts, *infra.PreparedInstance, error) {
	originalArgs := append(config.Command, config.Args...)

	var mounts []specs.Mount
	var infraInstance *infra.PreparedInstance
	if r.infraMgr != nil {
		prepared, err := r.infraMgr.PrepareInstance(ctx, config)
		if err != nil {
			return nil, nil, fmt.Errorf("prepare InfraProfile instance: %w", err)
		}
		infraInstance = &prepared
		for _, mount := range prepared.Mounts {
			mounts = append(mounts, specs.Mount{Source: mount.Source, Destination: mount.Destination, Type: "bind", Options: append([]string(nil), mount.Options...)})
		}
	}

	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(image),
		oci.WithEnv(envMapToSlice(config.Env)),
	}
	if len(originalArgs) > 0 {
		specOpts = append(specOpts, oci.WithProcessArgs(originalArgs...))
	}
	resourceOpts, err := sandboxResourceSpecOpts(config)
	if err != nil {
		return nil, nil, err
	}
	specOpts = append(specOpts, resourceOpts...)

	// Add TTY option if required by runtime (e.g., gVisor)
	if r.config.NeedsTTY {
		specOpts = append(specOpts, oci.WithTTY)
	}

	if config.WorkingDir != "" {
		specOpts = append(specOpts, oci.WithProcessCwd(config.WorkingDir))
	}

	if config.NetworkDNSPath != "" {
		mounts = append(mounts, specs.Mount{
			Source: config.NetworkDNSPath, Destination: "/etc/resolv.conf", Type: "bind",
			Options: []string{"ro", "rbind", "nosuid", "nodev", "noexec"},
		})
	}
	if len(mounts) > 0 {
		specOpts = append(specOpts, oci.WithMounts(mounts))
	}
	if infraInstance != nil && infraInstance.WrapperRequired {
		specOpts = append(specOpts, withSandboxInit())
	}

	networkNamespace := specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: config.NetworkNamespacePath}
	specOpts = append(specOpts, oci.WithLinuxNamespace(networkNamespace))

	return specOpts, infraInstance, nil
}

func withSandboxInit() oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, spec *oci.Spec) error {
		if spec.Process == nil || len(spec.Process.Args) == 0 || spec.Process.Args[0] == "" {
			return errors.New("user image has no entrypoint for sandbox-init to supervise")
		}
		original := append([]string(nil), spec.Process.Args...)
		originalUser := spec.Process.User
		wrapper := []string{
			infra.SandboxInitContainerPath, "--config", infra.InstanceConfigPath,
			"--user-uid", strconv.FormatUint(uint64(originalUser.UID), 10),
			"--user-gid", strconv.FormatUint(uint64(originalUser.GID), 10),
		}
		if len(originalUser.AdditionalGids) > 0 {
			groups := make([]string, len(originalUser.AdditionalGids))
			for index, group := range originalUser.AdditionalGids {
				groups[index] = strconv.FormatUint(uint64(group), 10)
			}
			wrapper = append(wrapper, "--user-additional-gids", strings.Join(groups, ","))
		}
		wrapper = append(wrapper, "--")
		spec.Process.Args = append(wrapper, original...)
		// The supervisor must read the root-only per-instance configuration.
		// It restores originalUser only on the user child process.
		spec.Process.User = specs.User{}
		return nil
	}
}

func cloneStringMap(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	result := make(map[string]string, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func sandboxResourceSpecOpts(config *fastletapi.SandboxSpec) ([]oci.SpecOpts, error) {
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

// getRuntimeOptions returns runtime-specific options for containerd.
// It uses config.OptionsType and config.ConfigPath to build the options.
func (r *Driver) getRuntimeOptions() *runtimeoptions.Options {
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

func (r *Driver) prepareLabels(config *fastletapi.SandboxSpec) map[string]string {
	routeGeneration := config.RouteGeneration
	if routeGeneration <= 0 {
		routeGeneration = 1
	}
	return map[string]string{
		"fast-sandbox.io/managed":               "true",
		"fast-sandbox.io/fastlet-name":          r.fastletPodName,
		"fast-sandbox.io/fastlet-uid":           r.fastletPodUID,
		"fast-sandbox.io/namespace":             r.fastletNamespace,
		"fast-sandbox.io/id":                    config.SandboxID,
		"fast-sandbox.io/claim-uid":             config.ClaimUID,
		"fast-sandbox.io/claim-namespace":       config.ClaimNamespace,
		"fast-sandbox.io/sandbox-name":          config.ClaimName,
		"fast-sandbox.io/runtime-profile-hash":  config.RuntimeProfileHash,
		"fast-sandbox.io/resource-profile-hash": config.ResourceProfileHash,
		"fast-sandbox.io/infra-profile":         config.InfraProfile,
		"fast-sandbox.io/infra-profile-hash":    config.InfraProfileHash,
		"fast-sandbox.io/resource-cpu":          config.CPU,
		"fast-sandbox.io/resource-memory":       config.Memory,
		"fast-sandbox.io/resource-pids":         strconv.FormatInt(config.PIDs, 10),
		"fast-sandbox.io/request-id":            config.RequestID,
		"fast-sandbox.io/instance-generation":   strconv.FormatInt(config.InstanceGeneration, 10),
		"fast-sandbox.io/runtime-instance-id":   config.RuntimeInstanceID,
		"fast-sandbox.io/assignment-attempt":    strconv.FormatInt(config.AssignmentAttempt, 10),
		"fast-sandbox.io/route-generation":      strconv.FormatInt(routeGeneration, 10),
		"fast-sandbox.io/network-slot-id":       config.NetworkSlotID,
		"fast-sandbox.io/network-netns-path":    config.NetworkNamespacePath,
		"fast-sandbox.io/network-ip":            config.NetworkIP,
		"fast-sandbox.io/network-gateway":       config.NetworkGateway,
		"fast-sandbox.io/network-dns-path":      config.NetworkDNSPath,
	}
}

func (r *Driver) SetNetworkManager(manager *fastletnetwork.Manager) {
	r.networkManager = manager
}

func (r *Driver) SetInfraManager(manager *infra.Manager) {
	r.infraMgr = manager
}

func (r *Driver) SetNamespace(ns string) {
	r.fastletNamespace = ns
}

func (r *Driver) DeleteSandbox(ctx context.Context, sandboxID string) error {
	var owner fastletnetwork.Owner
	if r.networkManager != nil {
		if slot, exists := r.networkManager.Lookup(sandboxID); exists {
			owner = slot.Owner
		}
	}
	if err := r.deleteContainerdSandbox(ctx, sandboxID); err != nil {
		return err
	}
	var infraErr error
	if r.infraMgr != nil {
		if err := r.infraMgr.RemoveSandboxInstances(sandboxID); err != nil {
			infraErr = fmt.Errorf("remove Infra instance state: %w", err)
		}
	}
	var networkErr error
	if r.networkManager != nil && owner.SandboxUID != "" {
		if err := r.networkManager.Release(ctx, owner); err != nil {
			networkErr = fmt.Errorf("release network slot: %w", err)
		}
	}
	return errors.Join(infraErr, networkErr)
}

func (r *Driver) deleteContainerdSandbox(ctx context.Context, sandboxID string) error {
	ctx = namespaces.WithNamespace(ctx, "k8s.io")
	return ensureContainerdSandboxAbsent(
		ctx,
		containerdDeleteClient{client: r.client},
		sandboxID,
		snapShotName(sandboxID),
		waitStopTimeout,
	)
}

func (r *Driver) GetSandboxStatus(ctx context.Context, sandboxID string) (string, error) {
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

func (r *Driver) InspectSandbox(ctx context.Context, sandboxID string) (*SandboxMetadata, error) {
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
		SandboxSpec: fastletapi.SandboxSpec{
			SandboxID:            sandboxID,
			RequestID:            info.Labels["fast-sandbox.io/request-id"],
			ClaimUID:             info.Labels["fast-sandbox.io/claim-uid"],
			ClaimNamespace:       info.Labels["fast-sandbox.io/claim-namespace"],
			ClaimName:            info.Labels["fast-sandbox.io/sandbox-name"],
			FastletPodUID:        info.Labels["fast-sandbox.io/fastlet-uid"],
			RuntimeInstanceID:    info.Labels["fast-sandbox.io/runtime-instance-id"],
			Image:                info.Image,
			CPU:                  info.Labels["fast-sandbox.io/resource-cpu"],
			Memory:               info.Labels["fast-sandbox.io/resource-memory"],
			RuntimeProfileHash:   info.Labels["fast-sandbox.io/runtime-profile-hash"],
			ResourceProfileHash:  info.Labels["fast-sandbox.io/resource-profile-hash"],
			InfraProfile:         info.Labels["fast-sandbox.io/infra-profile"],
			InfraProfileHash:     info.Labels["fast-sandbox.io/infra-profile-hash"],
			NetworkSlotID:        info.Labels["fast-sandbox.io/network-slot-id"],
			NetworkNamespacePath: info.Labels["fast-sandbox.io/network-netns-path"],
			NetworkIP:            info.Labels["fast-sandbox.io/network-ip"],
			NetworkGateway:       info.Labels["fast-sandbox.io/network-gateway"],
			NetworkDNSPath:       info.Labels["fast-sandbox.io/network-dns-path"],
		},
		ContainerID: sandboxID,
		CreatedAt:   info.CreatedAt.Unix(),
		Phase:       "stopped",
	}
	metadata.PIDs, _ = strconv.ParseInt(info.Labels["fast-sandbox.io/resource-pids"], 10, 64)
	metadata.InstanceGeneration, _ = strconv.ParseInt(info.Labels["fast-sandbox.io/instance-generation"], 10, 64)
	metadata.AssignmentAttempt, _ = strconv.ParseInt(info.Labels["fast-sandbox.io/assignment-attempt"], 10, 64)
	metadata.RouteGeneration, _ = strconv.ParseInt(info.Labels["fast-sandbox.io/route-generation"], 10, 64)
	if task, taskErr := container.Task(ctx, nil); taskErr == nil {
		metadata.PID = int(task.Pid())
		if status, statusErr := task.Status(ctx); statusErr == nil {
			metadata.Phase = string(status.Status)
		}
	}
	return metadata, nil
}

func (r *Driver) RecoverRuntimeResources(ctx context.Context, managed []*SandboxMetadata) error {
	if r.networkManager == nil {
		return nil
	}
	owners := make([]fastletnetwork.Owner, 0, len(managed))
	for _, metadata := range managed {
		if metadata == nil {
			continue
		}
		slot, exists := r.networkManager.Lookup(metadata.SandboxID)
		if !exists || metadata.NetworkSlotID == "" || metadata.NetworkSlotID != slot.ID ||
			metadata.NetworkNamespacePath != slot.HostNetNSPath || metadata.NetworkIP != slot.IP {
			return fmt.Errorf("%w: runtime sandbox %s does not match its durable network descriptor", fastletnetwork.ErrStateInconsistent, metadata.SandboxID)
		}
		owners = append(owners, networkOwner(&metadata.SandboxSpec))
	}
	return r.networkManager.Reconcile(ctx, owners)
}

func (r *Driver) RuntimeResourceAvailable() bool {
	return r.networkManager == nil || r.networkManager.Snapshot().Clean > 0
}

func (r *Driver) GetAccessDescriptor(sandboxID string) (dataplane.AccessDescriptor, error) {
	if r.networkManager == nil {
		return dataplane.AccessDescriptor{}, ErrNetworkUnavailable
	}
	slot, exists := r.networkManager.Lookup(sandboxID)
	if !exists {
		return dataplane.AccessDescriptor{}, ErrSandboxNotFound
	}
	return slot.Access, nil
}

func networkOwner(config *fastletapi.SandboxSpec) fastletnetwork.Owner {
	generation := config.InstanceGeneration
	if generation <= 0 {
		generation = 1
	}
	attempt := config.AssignmentAttempt
	if attempt <= 0 {
		attempt = 1
	}
	return fastletnetwork.Owner{
		SandboxUID: config.SandboxID, SandboxName: config.ClaimName, SandboxNamespace: config.ClaimNamespace,
		InstanceGeneration: generation, RuntimeInstanceID: config.RuntimeInstanceID,
		AssignmentAttempt: attempt,
	}
}

func (r *Driver) ListManagedSandboxes(ctx context.Context) ([]*SandboxMetadata, error) {
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

func (r *Driver) ProbeCapabilities(ctx context.Context) CapabilityReport {
	report := CapabilityReport{Runtime: r.runtimeName, ProfileHash: r.runtimeProfileHash, State: runtimecatalog.CapabilityDegraded}
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

func (r *Driver) ListImages(ctx context.Context) ([]string, error) {
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

func (r *Driver) PullImage(ctx context.Context, image string) error {
	ctx = namespaces.WithNamespace(ctx, "k8s.io")
	_, err := r.client.GetImage(ctx, image)
	if err == nil {
		return nil
	}
	_, err = r.client.Pull(ctx, image, containerd.WithPullUnpack)
	return err
}

func (r *Driver) Close() error {
	if r.client != nil {
		return r.client.Close()
	}
	return nil
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
