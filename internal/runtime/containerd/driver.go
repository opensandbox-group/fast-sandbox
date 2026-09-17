package containerd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	dataplane "fast-sandbox/internal/dataplane/contract"
	"fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"fast-sandbox/internal/fastlet/podcgroup"
	"fast-sandbox/internal/nodecleanup"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/registryconfig"
	runtimecontract "fast-sandbox/internal/runtime/contract"

	runtimeoptions "github.com/containerd/containerd/api/types/runtimeoptions/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

type Driver struct {
	socketPath         string
	client             *containerd.Client
	fastletPodName     string
	fastletPodUID      string
	fastletNamespace   string
	podCgroup          *podcgroup.Layout
	infraMgr           *infra.Manager
	runtimeName        apiv1alpha2.RuntimeName // runtime profile identifier
	runtimeProfileHash string
	config             RuntimeConfig // cached runtime configuration
	networkManager     *fastletnetwork.Manager
	registryProvider   registryconfig.Provider
	residualProcess    runtimecatalog.ResidualProcessKind
	nodeCleanup        nodecleanup.RuntimeProcessCleaner
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
		Namespace: profile.Containerd.Namespace, Snapshotter: profile.Containerd.Snapshotter,
		Handler: profile.Containerd.Handler, RuntimePath: profile.Containerd.RuntimePath,
		ConfigPath: profile.Containerd.ConfigPath, NeedsTTY: profile.Containerd.NeedsTTY,
		OptionsType: profile.Containerd.OptionsType,
	}
	driver := newWithConfig(profile.Name, profile.ProfileHash, config)
	driver.residualProcess = profile.ResidualProcess
	return driver, nil
}

func newWithConfig(rt apiv1alpha2.RuntimeName, profileHash string, cfg RuntimeConfig) *Driver {
	return &Driver{
		runtimeName:        rt,
		runtimeProfileHash: profileHash,
		config:             cfg,
	}
}

func (r *Driver) Initialize(ctx context.Context, socketPath string) error {
	r.socketPath = socketPath
	if r.socketPath == "" {
		r.socketPath = "/run/containerd/containerd.sock"
	}
	r.fastletPodName = os.Getenv("POD_NAME")
	r.fastletPodUID = os.Getenv("POD_UID")
	if r.fastletPodUID != "" {
		layout, err := podcgroup.Discover(podcgroup.HostRoot, r.fastletPodUID)
		if err != nil {
			return fmt.Errorf("discover Fastlet Pod cgroup: %w", err)
		}
		r.podCgroup = &layout
		if r.runtimeName == apiv1alpha2.RuntimeContainer {
			if err := layout.EnsureShimGroup(podcgroup.HostRoot); err != nil {
				return fmt.Errorf("prepare Fastlet shim cgroup: %w", err)
			}
		}
		klog.InfoS("discovered Fastlet Pod cgroup", "version", layout.Version, "path", layout.PodPath, "systemd", layout.Systemd)
	} else {
		klog.InfoS("POD_UID is not set; Sandbox cgroup aggregation is disabled outside Kubernetes")
	}

	klog.InfoS("initializing runtime", "handler", r.config.Handler, "containerdNamespace", r.containerdNamespace())

	ctx, cancel := context.WithTimeout(ctx, defaultOperationTimeout)
	defer cancel()

	client, err := containerd.New(
		r.socketPath,
		containerd.WithDefaultNamespace(r.containerdNamespace()),
		containerd.WithExtraDialOpts([]grpc.DialOption{
			grpc.WithChainUnaryInterceptor(containerdCreateRPCInterceptor),
		}),
	)
	if err != nil {
		return fmt.Errorf("failed to create containerd client: %w", err)
	}

	r.client = client

	return nil
}

func (r *Driver) CreateSandbox(ctx context.Context, input *fastletapi.EnsureSandboxInput, allocation fastletapi.RuntimeAllocation) (_ *SandboxMetadata, resultErr error) {
	config := &input.Sandbox
	identity := config.Identity
	spec := config.Spec
	network := allocation.Network
	totalStart := time.Now()
	ctx, finishTotal := startContainerdCreateStage(ctx, string(r.runtimeName), "total")
	defer func() { finishTotal(resultErr) }()
	logger := klog.FromContext(ctx).WithValues("sandboxID", identity.SandboxUID)

	logger.Info("creating sandbox", "image", spec.Image, "runtime", r.config.Handler, "netns", network.NamespacePath)
	ctx, cancel := context.WithTimeout(ctx, defaultOperationTimeout)
	defer cancel()
	ctx = r.withNamespace(ctx)
	ctx = withContainerdCreateRPCMetrics(ctx, string(r.runtimeName))

	pullStart := time.Now()
	imageContext, finishImage := startContainerdCreateStage(ctx, string(r.runtimeName), "image")
	image, err := r.prepareImage(imageContext, spec.Image)
	finishImage(err)
	if err != nil {
		logger.Error(err, "Failed to prepare image")
		return nil, err
	}
	pullDuration := time.Since(pullStart)

	containerID := identity.SandboxUID
	specContext, finishSpec := startContainerdCreateStage(ctx, string(r.runtimeName), "spec")
	specOpts, infraInstance, err := r.prepareSpecOpts(specContext, config, allocation, image)
	finishSpec(err)
	if err != nil {
		return nil, fmt.Errorf("invalid sandbox resource profile: %w", err)
	}
	created := false
	if infraInstance != nil {
		defer func() {
			if !created {
				if removeErr := r.infraMgr.RemoveInstance(config); removeErr != nil {
					logger.V(2).Info("infra instance removal on failed create leaked resources", "err", removeErr)
				}
			}
		}()
	}
	labels := r.prepareLabels(config, allocation)

	createStart := time.Now()
	logger.Info("creating containerd container object")

	containerContext, finishContainer := startContainerdCreateStage(ctx, string(r.runtimeName), "container")
	container, err := r.client.NewContainer(
		containerContext,
		containerID,
		instrumentContainerOption(string(r.runtimeName), "container_image_opt", containerd.WithImage(image)),
		instrumentContainerOption(string(r.runtimeName), "container_snapshotter_opt", containerd.WithSnapshotter(r.snapshotter())),
		instrumentContainerOption(string(r.runtimeName), "snapshot_prepare_opt", containerd.WithNewSnapshot(snapShotName(containerID), image)),
		instrumentContainerOption(string(r.runtimeName), "container_runtime_opt", containerd.WithRuntime(r.config.Handler, r.getRuntimeOptions())),
		instrumentContainerOption(string(r.runtimeName), "spec_generate_opt", containerd.WithNewSpec(specOpts...)),
		instrumentContainerOption(string(r.runtimeName), "container_labels_opt", containerd.WithContainerLabels(labels)),
	)
	finishContainer(err)
	if err != nil {
		logger.Error(err, "Failed to create container object")
		return nil, fmt.Errorf("failed to create container: %w", err)
	}
	createDuration := time.Since(createStart)

	logStarted := time.Now()
	_, finishLog := startContainerdCreateStage(ctx, string(r.runtimeName), "log")
	logDir := "/var/log/fast-sandbox"
	if err := os.MkdirAll(logDir, 0755); err != nil {
		finishLog(err)
		return nil, fmt.Errorf("failed to create log dir: %w", err)
	}
	logPath := filepath.Join(logDir, fmt.Sprintf("%s.log", containerID))

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		finishLog(err)
		return nil, fmt.Errorf("failed to open log file: %w", err)
	}
	finishLog(nil)
	logDuration := time.Since(logStarted)

	logger.Info("creating containerd task")

	var cioOpts []cio.Opt
	if r.config.NeedsTTY {
		cioOpts = append(cioOpts, cio.WithTerminal)
	}
	cioOpts = append(cioOpts, cio.WithStreams(nil, logFile, logFile))

	var taskOpts []containerd.NewTaskOpts
	if r.config.RuntimePath != "" {
		taskOpts = append(taskOpts, containerd.WithRuntimePath(r.config.RuntimePath))
	}
	// WithShimCgroup mutates the runc-v2 task options and therefore is only
	// valid for the built-in runc profile. Other shims still place their main
	// workload/VMM through the OCI cgroupsPath below the Fastlet Pod.
	if r.podCgroup != nil && r.runtimeName == apiv1alpha2.RuntimeContainer {
		taskOpts = append(taskOpts, containerd.WithShimCgroup(r.podCgroup.ShimPath()))
	}

	taskCreateStarted := time.Now()
	taskContext, finishTaskCreate := startContainerdCreateStage(ctx, string(r.runtimeName), "task_create")
	ioCreator := cio.NewCreator(cioOpts...)
	instrumentedIOCreator := func(id string) (cio.IO, error) {
		_, finishIO := startContainerdCreateStage(taskContext, string(r.runtimeName), "task_io")
		taskIO, createErr := ioCreator(id)
		finishIO(createErr)
		return taskIO, createErr
	}
	task, err := container.NewTask(taskContext, instrumentedIOCreator, taskOpts...)
	finishTaskCreate(err)
	if err != nil {
		logger.Error(err, "Failed to create containerd task", "logPath", logPath)
		logFile.Close()
		if deleteErr := container.Delete(ctx, containerd.WithSnapshotCleanup); deleteErr != nil {
			logger.V(2).Info("container cleanup on failed task create leaked resources", "err", deleteErr)
		}
		return nil, fmt.Errorf("failed to create task: %w", err)
	}
	taskCreateDuration := time.Since(taskCreateStarted)

	logger.Info("starting containerd task", "pid", task.Pid())
	taskStartStarted := time.Now()
	taskStartContext, finishTaskStart := startContainerdCreateStage(ctx, string(r.runtimeName), "task_start")
	err = task.Start(taskStartContext)
	finishTaskStart(err)
	if err != nil {
		logger.Error(err, "Failed to start containerd task")
		if _, deleteErr := task.Delete(ctx, containerd.WithProcessKill); deleteErr != nil {
			logger.V(2).Info("task cleanup on failed start leaked resources", "err", deleteErr)
		}
		if deleteErr := container.Delete(ctx, containerd.WithSnapshotCleanup); deleteErr != nil {
			logger.V(2).Info("container cleanup on failed start leaked resources", "err", deleteErr)
		}
		return nil, fmt.Errorf("failed to start task: %w", err)
	}
	userProcessStartedAt, userProcessStartSource := userProcessStartAfterTaskStart(infraInstance, time.Now())
	taskStartDuration := time.Since(taskStartStarted)

	totalDuration := time.Since(totalStart)

	logger.Info("runtime CreateSandbox timing",
		"total_ms", totalDuration.Milliseconds(),
		"pull_ms", pullDuration.Milliseconds(),
		"create_ms", createDuration.Milliseconds(),
		"log_ms", logDuration.Milliseconds(),
		"task_create_ms", taskCreateDuration.Milliseconds(),
		"task_start_ms", taskStartDuration.Milliseconds(),
		"start_ms", (taskCreateDuration + taskStartDuration).Milliseconds())

	metadata := &SandboxMetadata{
		Config:                 *config,
		Allocation:             allocation,
		ContainerID:            containerID,
		Phase:                  "running",
		CreatedAt:              time.Now().Unix(),
		PID:                    int(task.Pid()),
		UserProcessStartedAt:   userProcessStartedAt,
		UserProcessStartSource: userProcessStartSource,
	}
	if infraInstance != nil {
		metadata.InfraServices = append([]infra.ServiceEndpoint(nil), infraInstance.Services...)
	}
	created = true
	logger.Info("sandbox created successfully", "pid", task.Pid())
	return metadata, nil
}

// EnsureSandbox is idempotent for a Sandbox runtime identity. It returns the
// existing managed runtime when a retry observes an already-created Sandbox.
func (r *Driver) EnsureSandbox(ctx context.Context, input *fastletapi.EnsureSandboxInput) (*SandboxMetadata, error) {
	if input == nil {
		return nil, fmt.Errorf("%w: Sandbox input is required", runtimecontract.ErrInvalidConfig)
	}
	config := &input.Sandbox
	identity := config.Identity
	inspectContext, finishInspect := startContainerdCreateStage(ctx, string(r.runtimeName), "inspect_existing")
	existing, err := r.InspectSandbox(inspectContext, identity.SandboxUID)
	inspectErr := err
	if errors.Is(err, ErrSandboxNotFound) {
		inspectErr = nil
	}
	finishInspect(inspectErr)
	if err == nil {
		if runtimecontract.SameRuntimeIdentity(existing.Config.Identity, identity) {
			if err := validateExistingRuntimeProfile(existing, config); err != nil {
				return nil, err
			}
			existing.UserProcessStartSource = fastletapi.UserProcessStartExistingRuntime
			return existing, nil
		}
		klog.InfoS("replacing stale runtime owned by a previous Sandbox instance",
			"sandbox", identity.SandboxUID,
			"existingFastletPodUID", existing.Config.Identity.FastletPodUID,
			"requestedFastletPodUID", identity.FastletPodUID,
			"existingInstanceGeneration", existing.Config.Identity.InstanceGeneration,
			"requestedInstanceGeneration", identity.InstanceGeneration)
		if err := r.DeleteSandbox(ctx, identity.SandboxUID); err != nil {
			return nil, fmt.Errorf("replace stale Sandbox runtime: %w", err)
		}
		err = ErrSandboxNotFound
	}
	if !errors.Is(err, ErrSandboxNotFound) {
		return nil, err
	}
	allocation := fastletapi.RuntimeAllocation{}
	var owner fastletnetwork.Owner
	if r.networkManager != nil {
		owner = r.networkOwner(config)
		networkContext, finishNetwork := startContainerdCreateStage(ctx, string(r.runtimeName), "network_acquire")
		slot, acquireErr := r.networkManager.Acquire(networkContext, owner)
		finishNetwork(acquireErr)
		if acquireErr != nil {
			return nil, fmt.Errorf("%w: %v", ErrNetworkUnavailable, acquireErr)
		}
		allocation.Network = fastletapi.NetworkAllocation{
			SlotID: slot.ID, NamespacePath: slot.HostNetNSPath, IP: slot.IP,
			Gateway: slot.Gateway, DNSPath: slot.DNSPath, PrivateCIDR: slot.PrivateCIDR, HostVeth: slot.HostVeth,
		}
	}
	metadata, createErr := r.CreateSandbox(ctx, input, allocation)
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

func validateExistingRuntimeProfile(existing *SandboxMetadata, requested *fastletapi.RuntimeSandboxConfig) error {
	return runtimecontract.ValidateProfile(existing, requested)
}

func (r *Driver) prepareImage(ctx context.Context, imageName string) (containerd.Image, error) {
	image, err := r.client.GetImage(ctx, imageName)
	if err != nil {
		image, err = r.pullImage(ctx, imageName)
		if err != nil {
			return nil, err
		}
		return image, nil
	}
	unpacked, err := image.IsUnpacked(ctx, r.snapshotter())
	if err != nil {
		return nil, fmt.Errorf("inspect image %q in snapshotter %q: %w", imageName, r.snapshotter(), err)
	}
	if !unpacked {
		if err := image.Unpack(ctx, r.snapshotter()); err != nil {
			return nil, fmt.Errorf("unpack image %q in snapshotter %q: %w", imageName, r.snapshotter(), err)
		}
	}
	return image, nil
}

func (r *Driver) SetRegistryProvider(provider registryconfig.Provider) {
	r.registryProvider = provider
}

func (r *Driver) pullImage(ctx context.Context, imageName string) (containerd.Image, error) {
	options := []containerd.RemoteOpt{containerd.WithPullUnpack, containerd.WithPullSnapshotter(r.snapshotter())}
	if r.registryProvider != nil {
		credential, found, err := r.registryProvider.Credentials(imageName)
		if err != nil {
			return nil, err
		}
		if found {
			secret := credential.Password
			if credential.IdentityToken != "" {
				secret = credential.IdentityToken
			}
			resolver := docker.NewResolver(docker.ResolverOptions{
				Credentials: func(string) (string, string, error) {
					return credential.Username, secret, nil
				},
			})
			options = append(options, containerd.WithResolver(resolver))
		}
	}
	return r.client.Pull(ctx, imageName, options...)
}

func (r *Driver) prepareSpecOpts(ctx context.Context, config *fastletapi.RuntimeSandboxConfig, allocation fastletapi.RuntimeAllocation, image containerd.Image) ([]oci.SpecOpts, *infra.PreparedInstance, error) {
	spec := &config.Spec
	network := allocation.Network
	originalArgs := append(spec.Command, spec.Args...)

	var mounts []specs.Mount
	var infraInstance *infra.PreparedInstance
	if r.infraMgr != nil {
		prepared, err := r.infraMgr.PrepareInstance(ctx, config)
		if err != nil {
			return nil, nil, fmt.Errorf("prepare Infra Component instance: %w", err)
		}
		infraInstance = &prepared
		for _, mount := range prepared.Mounts {
			mounts = append(mounts, specs.Mount{Source: mount.Source, Destination: mount.Destination, Type: "bind", Options: append([]string(nil), mount.Options...)})
		}
	}

	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(image),
		oci.WithEnv(envMapToSlice(spec.Env)),
	}
	if len(originalArgs) > 0 {
		specOpts = append(specOpts, oci.WithProcessArgs(originalArgs...))
	}
	resourceOpts, err := sandboxResourceSpecOpts(spec)
	if err != nil {
		return nil, nil, err
	}
	specOpts = append(specOpts, resourceOpts...)
	if r.podCgroup != nil {
		cgroupOpt, err := r.sandboxCgroupSpecOpt(config.Identity.SandboxUID)
		if err != nil {
			return nil, nil, fmt.Errorf("derive Sandbox cgroup path: %w", err)
		}
		specOpts = append(specOpts, cgroupOpt)
	}

	if r.config.NeedsTTY {
		specOpts = append(specOpts, oci.WithTTY)
	}

	if spec.WorkingDir != "" {
		specOpts = append(specOpts, oci.WithProcessCwd(spec.WorkingDir))
	}

	if network.DNSPath != "" {
		mounts = append(mounts, specs.Mount{
			Source: network.DNSPath, Destination: "/etc/resolv.conf", Type: "bind",
			Options: []string{"ro", "rbind", "nosuid", "nodev", "noexec"},
		})
	}
	if len(mounts) > 0 {
		specOpts = append(specOpts, oci.WithMounts(mounts))
	}
	if infraInstance != nil && infraInstance.WrapperRequired {
		specOpts = append(specOpts, withSandboxInit())
	}

	networkNamespace := specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: network.NamespacePath}
	specOpts = append(specOpts, oci.WithLinuxNamespace(networkNamespace))

	return specOpts, infraInstance, nil
}

func (r *Driver) sandboxCgroupSpecOpt(sandboxID string) (oci.SpecOpts, error) {
	if r.podCgroup == nil {
		return nil, errors.New("Fastlet Pod cgroup is not initialized")
	}
	path, err := r.podCgroup.SandboxPath(sandboxID)
	if r.runtimeName == apiv1alpha2.RuntimeContainer || r.runtimeName == apiv1alpha2.RuntimeGVisor {
		path, err = r.podCgroup.SandboxSystemdPath(sandboxID)
	}
	if err != nil {
		return nil, err
	}
	return oci.WithCgroup(path), nil
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

func (r *Driver) getRuntimeOptions() *runtimeoptions.Options {
	// If OptionsType is set, include TypeUrl (required for gVisor)
	if r.config.OptionsType != "" {
		return &runtimeoptions.Options{
			TypeUrl:    r.config.OptionsType,
			ConfigPath: r.config.ConfigPath,
		}
	}

	if r.config.ConfigPath != "" {
		return &runtimeoptions.Options{
			ConfigPath: r.config.ConfigPath,
		}
	}

	return nil
}

func (r *Driver) prepareLabels(config *fastletapi.RuntimeSandboxConfig, allocation fastletapi.RuntimeAllocation) map[string]string {
	identity := config.Identity
	spec := config.Spec
	network := allocation.Network
	routeGeneration := identity.RouteGeneration
	if routeGeneration <= 0 {
		routeGeneration = 1
	}
	labels := map[string]string{
		"fast-sandbox.io/managed":               "true",
		"fast-sandbox.io/fastlet-name":          r.fastletPodName,
		"fast-sandbox.io/fastlet-uid":           r.fastletPodUID,
		"fast-sandbox.io/namespace":             r.fastletNamespace,
		"fast-sandbox.io/id":                    identity.SandboxUID,
		"fast-sandbox.io/sandbox-namespace":     identity.Namespace,
		"fast-sandbox.io/sandbox-name":          identity.Name,
		"fast-sandbox.io/runtime-profile-hash":  spec.RuntimeProfileHash,
		"fast-sandbox.io/resource-profile-hash": spec.ResourceProfileHash,
		"fast-sandbox.io/infra-revision":        spec.InfraRevision,
		"fast-sandbox.io/resource-cpu":          spec.CPU,
		"fast-sandbox.io/resource-memory":       spec.Memory,
		"fast-sandbox.io/resource-pids":         strconv.FormatInt(spec.PIDs, 10),
		"fast-sandbox.io/instance-generation":   strconv.FormatInt(identity.InstanceGeneration, 10),
		"fast-sandbox.io/runtime-instance-id":   identity.RuntimeInstanceID,
		"fast-sandbox.io/assignment-attempt":    strconv.FormatInt(identity.AssignmentAttempt, 10),
		"fast-sandbox.io/route-generation":      strconv.FormatInt(routeGeneration, 10),
		"fast-sandbox.io/network-slot-id":       network.SlotID,
		"fast-sandbox.io/network-netns-path":    network.NamespacePath,
		"fast-sandbox.io/network-ip":            network.IP,
		"fast-sandbox.io/network-gateway":       network.Gateway,
		"fast-sandbox.io/network-dns-path":      network.DNSPath,
	}
	if network.PrivateCIDR != "" {
		labels["fast-sandbox.io/network-private-cidr"] = network.PrivateCIDR
	}
	if network.HostVeth != "" {
		labels["fast-sandbox.io/network-host-veth"] = network.HostVeth
	}
	return labels
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

func (r *Driver) SetNodeCleanupClient(client nodecleanup.RuntimeProcessCleaner) {
	r.nodeCleanup = client
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
	ctx = r.withNamespace(ctx)
	if err := ensureContainerdSandboxAbsent(
		ctx,
		containerdDeleteClient{client: r.client, snapshotter: r.snapshotter()},
		sandboxID,
		snapShotName(sandboxID),
		waitStopTimeout,
	); err != nil {
		return err
	}
	if err := r.ensureResidualProcessAbsent(ctx, sandboxID); err != nil {
		return err
	}
	if r.podCgroup != nil {
		if err := r.podCgroup.RemoveSandboxGroups(podcgroup.HostRoot, sandboxID); err != nil {
			return fmt.Errorf("remove Sandbox cgroup: %w", err)
		}
	}
	return nil
}

func (r *Driver) ensureResidualProcessAbsent(ctx context.Context, sandboxID string) error {
	if r.residualProcess == runtimecatalog.ResidualProcessNone {
		return nil
	}
	if r.nodeCleanup == nil {
		return fmt.Errorf("verify %s residual process cleanup for sandbox %q: node cleanup client is not configured", r.residualProcess, sandboxID)
	}
	if err := r.nodeCleanup.EnsureRuntimeProcessesAbsent(ctx, r.residualProcess, sandboxID); err != nil {
		return fmt.Errorf("verify %s residual process cleanup for sandbox %q: %w", r.residualProcess, sandboxID, err)
	}
	return nil
}

func (r *Driver) GetSandboxStatus(ctx context.Context, sandboxID string) (string, error) {
	ctx = r.withNamespace(ctx)
	container, err := r.client.LoadContainer(ctx, sandboxID)
	if err != nil {
		// No container object: nothing remains of the sandbox.
		return "terminated", nil
	}

	task, err := container.Task(ctx, nil)
	if err != nil {
		// Container object exists but has no running task: the process
		// already exited.
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
	ctx = r.withNamespace(ctx)
	container, err := r.client.LoadContainer(ctx, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSandboxNotFound, err)
	}
	info, err := container.Info(ctx)
	if err != nil {
		return nil, err
	}
	metadata := &SandboxMetadata{
		Config: fastletapi.RuntimeSandboxConfig{
			Spec: fastletapi.SandboxSpec{
				Image:               info.Image,
				CPU:                 info.Labels["fast-sandbox.io/resource-cpu"],
				Memory:              info.Labels["fast-sandbox.io/resource-memory"],
				RuntimeProfileHash:  info.Labels["fast-sandbox.io/runtime-profile-hash"],
				ResourceProfileHash: info.Labels["fast-sandbox.io/resource-profile-hash"],
				InfraRevision:       info.Labels["fast-sandbox.io/infra-revision"],
			},
			Identity: fastletapi.SandboxIdentity{
				SandboxUID: sandboxID, Namespace: info.Labels["fast-sandbox.io/sandbox-namespace"],
				Name: info.Labels["fast-sandbox.io/sandbox-name"], FastletPodUID: info.Labels["fast-sandbox.io/fastlet-uid"],
				RuntimeInstanceID: info.Labels["fast-sandbox.io/runtime-instance-id"],
			},
		},
		Allocation: fastletapi.RuntimeAllocation{Network: fastletapi.NetworkAllocation{
			SlotID: info.Labels["fast-sandbox.io/network-slot-id"], NamespacePath: info.Labels["fast-sandbox.io/network-netns-path"],
			IP: info.Labels["fast-sandbox.io/network-ip"], Gateway: info.Labels["fast-sandbox.io/network-gateway"],
			DNSPath: info.Labels["fast-sandbox.io/network-dns-path"], PrivateCIDR: info.Labels["fast-sandbox.io/network-private-cidr"],
			HostVeth: info.Labels["fast-sandbox.io/network-host-veth"],
		}},
		ContainerID: sandboxID,
		CreatedAt:   info.CreatedAt.Unix(),
		Phase:       "stopped",
	}
	metadata.Config.Spec.PIDs, _ = strconv.ParseInt(info.Labels["fast-sandbox.io/resource-pids"], 10, 64)
	metadata.Config.Identity.InstanceGeneration, _ = strconv.ParseInt(info.Labels["fast-sandbox.io/instance-generation"], 10, 64)
	metadata.Config.Identity.AssignmentAttempt, _ = strconv.ParseInt(info.Labels["fast-sandbox.io/assignment-attempt"], 10, 64)
	metadata.Config.Identity.RouteGeneration, _ = strconv.ParseInt(info.Labels["fast-sandbox.io/route-generation"], 10, 64)
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
		identity := metadata.Config.Identity
		network := metadata.Allocation.Network
		slot, exists := r.networkManager.Lookup(identity.SandboxUID)
		if !exists || network.SlotID == "" || network.SlotID != slot.ID ||
			network.NamespacePath != slot.HostNetNSPath || network.IP != slot.IP {
			return fmt.Errorf("%w: runtime sandbox %s does not match its durable network descriptor", fastletnetwork.ErrStateInconsistent, identity.SandboxUID)
		}
		owners = append(owners, r.networkOwner(&metadata.Config))
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

func (r *Driver) networkOwner(config *fastletapi.RuntimeSandboxConfig) fastletnetwork.Owner {
	identity := config.Identity
	generation := identity.InstanceGeneration
	if generation <= 0 {
		generation = 1
	}
	attempt := identity.AssignmentAttempt
	if attempt <= 0 {
		attempt = 1
	}
	return fastletnetwork.Owner{
		SandboxUID: identity.SandboxUID, SandboxName: identity.Name, SandboxNamespace: identity.Namespace,
		InstanceGeneration: generation, RuntimeInstanceID: identity.RuntimeInstanceID,
		AssignmentAttempt: attempt, ResidualProcess: r.residualProcess,
	}
}

func (r *Driver) ListManagedSandboxes(ctx context.Context) ([]*SandboxMetadata, error) {
	if r.client == nil {
		return nil, ErrRuntimeNotInitialized
	}
	ctx = r.withNamespace(ctx)
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
	if err := r.client.SnapshotService(r.snapshotter()).Walk(ctx, func(context.Context, snapshots.Info) error { return nil }); err != nil {
		report.Reason = "ContainerdSnapshotterUnavailable"
		report.Message = fmt.Sprintf("containerd snapshotter %q is unavailable: %v", r.snapshotter(), err)
		return report
	}
	report.State = runtimecatalog.CapabilityReady
	report.Reason = "RuntimeDriverReady"
	report.Message = "containerd runtime driver is ready"
	return report
}

func (r *Driver) ListImages(ctx context.Context) ([]string, error) {
	ctx = r.withNamespace(ctx)
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
	ctx = r.withNamespace(ctx)
	_, err := r.prepareImage(ctx, image)
	return err
}

func (r *Driver) Close() error {
	if r.client != nil {
		return r.client.Close()
	}
	return nil
}

func (r *Driver) containerdNamespace() string {
	if r.config.Namespace != "" {
		return r.config.Namespace
	}
	return runtimecatalog.DefaultContainerdNamespace
}

func (r *Driver) snapshotter() string {
	if r.config.Snapshotter != "" {
		return r.config.Snapshotter
	}
	return "overlayfs"
}

func (r *Driver) withNamespace(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, r.containerdNamespace())
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
