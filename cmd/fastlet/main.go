package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	infracatalog "fast-sandbox/internal/catalog/infra"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	"fast-sandbox/internal/dataplane/fastletproxy"
	fastletaction "fast-sandbox/internal/fastlet/action"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"fast-sandbox/internal/fastlet/podcgroup"
	fastletresourceadmission "fast-sandbox/internal/fastlet/resourceadmission"
	fastletsandbox "fast-sandbox/internal/fastlet/sandbox"
	"fast-sandbox/internal/fastlet/server"
	"fast-sandbox/internal/fastletsettings"
	"fast-sandbox/internal/nodecleanup"
	"fast-sandbox/internal/observability"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/registryconfig"
	runtimecontract "fast-sandbox/internal/runtime/contract"
	runtimefactory "fast-sandbox/internal/runtime/factory"
	"fast-sandbox/internal/runtimeenv"
)

func main() {
	klog.InitFlags(nil)
	flag.Parse()
	defer klog.Flush()
	klog.InfoS("starting sandbox fastlet")
	traceShutdown, err := observability.Configure(context.Background(), "fast-sandbox-fastlet")
	if err != nil {
		klog.ErrorS(err, "Configure OpenTelemetry")
		os.Exit(1)
	}
	defer shutdownTracing(traceShutdown)

	podName := getEnv("POD_NAME", "")
	podUID := getEnv("POD_UID", "")
	podIP := getEnv("POD_IP", "")
	nodeName := getEnv("NODE_NAME", "")
	namespace := getEnv("NAMESPACE", "")
	fastletPort := getEnv("FASTLET_CONTROL_PORT", ":5758")
	startup, err := loadFastletStartupSettings()
	if err != nil {
		klog.ErrorS(err, "Failed to load Fastlet startup settings")
		os.Exit(1)
	}
	runtimePlan := startup.runtimePlan
	runtimeProfile := runtimePlan.Profile
	runtimeName := string(runtimeProfile.Name)
	runtimeSocket := runtimePlan.Containerd.Socket
	resourceProfile := startup.resourceProfile
	resourceAdmission := startup.resourceAdmission
	actionManager := startup.actionManager
	actionManager.SetVaultHandler(fastletaction.VaultHandlerFromEnv())

	klog.InfoS("fastlet starting",
		"podName", podName, "podIP", podIP, "nodeName", nodeName,
		"namespace", namespace, "capacity", capacityFromEnvironment())
	klog.InfoS("runtime resolved", "runtime", runtimeName, "socket", runtimeSocket, "settingsDir", startup.settingsDir)

	ctx := context.Background()
	var rt runtimecontract.Driver

	rt, _, err = runtimefactory.New(runtimecatalog.Builtin(), runtimefactory.NewHostCapabilityProber()).CreateProfile(ctx, runtimeProfile, runtimeSocket)

	if err != nil {
		klog.ErrorS(err, "Failed to initialize runtime")
		os.Exit(1)
	}
	defer rt.Close()

	rt.SetNamespace(namespace)
	if runtimeProfile.ResidualProcess != runtimecatalog.ResidualProcessNone {
		configurable, ok := rt.(nodeCleanupConfigurable)
		if !ok {
			klog.ErrorS(runtimecontract.ErrUnsupportedRuntime, "Runtime profile requires node process cleanup but driver is not configurable", "kind", runtimeProfile.ResidualProcess)
			os.Exit(1)
		}
		configurable.SetNodeCleanupClient(nodecleanup.NewClient(getEnv("FAST_SANDBOX_NODE_CLEANUP_SOCKET", nodecleanup.DefaultSocketPath)))
	}
	// The firecracker driver optionally talks to the node-level firecracker
	// runtime-agent over its UDS socket (FAST_SANDBOX_RUNTIME_AGENT_SOCKET;
	// empty = local mode: no remote pulls, warm images behave as before).
	// The agent's deployment carrier is a deployment decision, so the env is
	// only injected by the deployer via the pool fastletTemplate, never by
	// the control plane.
	if configurable, ok := rt.(agentClientConfigurable); ok {
		configurable.SetFastletPodUID(podUID)
		configurable.SetAgentSocket(getEnv("FAST_SANDBOX_RUNTIME_AGENT_SOCKET", ""))
	}
	// The driver-invoked egress state transfers (OSEP-0022 vault, plus the
	// recorded network policy): checkpoints stage the subject's vault and
	// policy files into the bundle before the runtime release, restores
	// import them before the data plane becomes ready. Optional: drivers
	// without the capability (and pools without an egress handler) simply
	// never move state files.
	if configurable, ok := rt.(stateMoverConfigurable); ok {
		configurable.SetStateMover(actionManager)
	}
	registryProvider := registryconfig.NewFileProvider(getEnv("FAST_SANDBOX_REGISTRY_CONFIG_PATH", registryconfig.MountPath))
	if revision, err := registryProvider.Refresh(); err != nil {
		klog.ErrorS(err, "Failed to load Registry configuration")
		os.Exit(1)
	} else {
		klog.InfoS("registry configuration loaded", "revision", revision)
	}
	if configurable, ok := rt.(registryConfigurable); ok {
		configurable.SetRegistryProvider(registryProvider)
	}
	if runtimeProfile.UsesFastletNetNS() {
		tuneNeighborGCThresholds()
		// The slot CIDR must not overlap any live pod/node route (CNI pod
		// CIDR, Docker bridge pool, VPC overlay): the slot bridge would
		// hijack the range and the isolation rules would blackhole it.
		if err := fastletnetwork.DetectCIDROverlap(ctx,
			getEnv("FAST_SANDBOX_NETWORK_CIDR", fastletnetwork.DefaultPrivateCIDR),
			getEnv("FAST_SANDBOX_NETWORK_BRIDGE", fastletnetwork.DefaultBridge),
			fastletnetwork.ExecRunner{}); err != nil {
			klog.ErrorS(err, "Fastlet slot CIDR conflicts with live host routes; set FAST_SANDBOX_NETWORK_CIDR to a free range")
			os.Exit(1)
		}
		networkManager, err := newNetworkManager(capacityFromEnvironment(), podUID, runtimeProfile.NetworkMode)
		if err != nil {
			klog.ErrorS(err, "Failed to configure Fastlet-owned network")
			os.Exit(1)
		}
		if err := networkManager.Initialize(ctx); err != nil {
			klog.ErrorS(err, "Failed to initialize Fastlet-owned network")
			os.Exit(1)
		}
		configurable, ok := rt.(networkConfigurable)
		if !ok {
			klog.ErrorS(runtimecontract.ErrUnsupportedRuntime, "Runtime profile requires Linux netns but driver is not network configurable")
			os.Exit(1)
		}
		configurable.SetNetworkManager(networkManager)
		klog.InfoS("fastlet-owned network initialized", "capacity", networkManager.Snapshot().Capacity, "cleanSlots", networkManager.Snapshot().Clean)
	}
	infraRevision := getEnv("FAST_SANDBOX_INFRA_REVISION", "")
	infraManager, err := newInfraManager(
		podUID,
		runtimeProfile,
		getEnv("FAST_SANDBOX_INFRA_PLAN_PATH", "/etc/fast-sandbox/infra/plan.json"),
		infraRevision,
		runtimePlan,
		registryProvider,
	)
	if err != nil {
		klog.ErrorS(err, "Failed to configure Infra Components")
		os.Exit(1)
	}
	infraConfigurable, ok := rt.(infraConfigurable)
	if !ok {
		klog.ErrorS(runtimecontract.ErrUnsupportedRuntime, "Runtime driver cannot accept an Infra Component plan")
		os.Exit(1)
	}
	infraConfigurable.SetInfraManager(infraManager)

	klog.InfoS("runtime initialized successfully", "name", runtimeName)

	proxyControlClient := fastletproxy.NewControlClient(getEnv("FASTLET_PROXY_CONTROL_SOCKET", fastletproxy.DefaultControlSocket))
	sandboxManager, err := fastletsandbox.NewSandboxManagerWithConfig(rt, fastletsandbox.SandboxManagerConfig{
		Capacity: capacityFromEnvironment(), RuntimeName: runtimeProfile.Name, RuntimeProfileHash: runtimeProfile.ProfileHash, ResourceProfile: &resourceProfile,
		ResourceAdmission: resourceAdmission,
		FastletPodUID:     podUID, RecoverOnStart: true,
		WarmImages:     startup.warmImages,
		RoutePublisher: fastletproxy.NewRoutePublisher(proxyControlClient),
		InfraRevision:  infraManager.Revision(), InfraManager: infraManager,
		RegistryProvider: registryProvider,
		ActionManager:    actionManager,
	})
	if err != nil {
		klog.ErrorS(err, "Failed to initialize Sandbox manager")
		os.Exit(1)
	}
	defer sandboxManager.Close()
	if resourceAdmission != nil && resourceAdmission.MemoryAdmission {
		if err := startResourceAdmissionObserver(ctx, sandboxManager, resourceAdmission, podUID, runtimeProfile.Driver == runtimecatalog.DriverKindContainerd); err != nil {
			klog.ErrorS(err, "Resource admission telemetry is unavailable; admission remains UNKNOWN")
		}
	}
	actionManager.Start(ctx)
	go recoverUntilReady(ctx, sandboxManager, proxyControlClient)

	fastletServer := server.NewFastletServer(fastletPort, sandboxManager)
	klog.InfoS("starting fastlet HTTP server", "port", fastletPort)

	if err := fastletServer.Start(); err != nil {
		klog.ErrorS(err, "Fastlet server failed")
		os.Exit(1)
	}
}

type fastletStartupSettings struct {
	settingsDir       string
	runtimePlan       runtimeenv.ResolvedRuntimePlan
	resourceProfile   apiv1alpha2.SandboxResourceProfile
	resourceAdmission *fastletsettings.ResourceAdmissionSettings
	warmImages        []string
	actionManager     *fastletaction.Manager
}

func loadFastletStartupSettings() (fastletStartupSettings, error) {
	settingsDir := getEnv(fastletsettings.EnvDir, fastletsettings.MountPath)
	runtimeName := getEnv("FAST_SANDBOX_RUNTIME", "container")
	runtimePlan, err := loadRuntimePlan(getEnv("FAST_SANDBOX_RUNTIME_PLAN_PATH", runtimeenv.PlanMountPath+"/"+runtimeenv.PlanFileName))
	if err != nil {
		return fastletStartupSettings{}, fmt.Errorf("load runtime plan: %w", err)
	}
	if runtimePlan.Profile.Name != apiv1alpha2.RuntimeName(runtimeName) {
		return fastletStartupSettings{}, fmt.Errorf("%w: runtime plan name %q does not match injected runtime %q", runtimecontract.ErrSandboxProfileMismatch, runtimePlan.Profile.Name, runtimeName)
	}
	injectedRuntimeHash := getEnv("FAST_SANDBOX_RUNTIME_PROFILE_HASH", runtimePlan.Profile.ProfileHash)
	if injectedRuntimeHash != runtimePlan.Profile.ProfileHash {
		return fastletStartupSettings{}, fmt.Errorf("%w: injected runtime profile hash %q does not match runtime plan %q", runtimecontract.ErrSandboxProfileMismatch, injectedRuntimeHash, runtimePlan.Profile.ProfileHash)
	}
	resourceProfile, err := resourceProfileFromSettings(settingsDir)
	if err != nil {
		return fastletStartupSettings{}, fmt.Errorf("resolve Sandbox resource profile: %w", err)
	}
	resourceAdmissionValue, resourceAdmissionFound, err := resourceAdmissionFromSettings(settingsDir, resourceProfile)
	if err != nil {
		return fastletStartupSettings{}, fmt.Errorf("resolve resource admission settings: %w", err)
	}
	var resourceAdmission *fastletsettings.ResourceAdmissionSettings
	if resourceAdmissionFound {
		resourceAdmission = &resourceAdmissionValue
	}
	warmImages, err := warmImagesFromSettings(settingsDir)
	if err != nil {
		return fastletStartupSettings{}, fmt.Errorf("parse warmImages: %w", err)
	}
	actionHandlers, err := actionHandlersFromSettings(settingsDir)
	if err != nil {
		return fastletStartupSettings{}, fmt.Errorf("parse action handlers: %w", err)
	}
	actionManager, err := fastletaction.NewManagerWithAPIVersion(actionHandlers, runtimePlan.EffectiveActionsAPIVersion(), nil)
	if err != nil {
		return fastletStartupSettings{}, fmt.Errorf("configure action handlers: %w", err)
	}
	return fastletStartupSettings{
		settingsDir: settingsDir, runtimePlan: runtimePlan, resourceProfile: resourceProfile,
		resourceAdmission: resourceAdmission, warmImages: warmImages, actionManager: actionManager,
	}, nil
}

func startResourceAdmissionObserver(
	ctx context.Context,
	manager *fastletsandbox.SandboxManager,
	settings *fastletsettings.ResourceAdmissionSettings,
	podUID string,
	aggregatePodCgroup bool,
) error {
	if manager == nil || settings == nil {
		return errors.New("resource admission manager and settings are required")
	}
	if err := manager.SetResourceAdmissionState(fastletapi.AdmissionStateUnknown, fastletapi.AdmissionReasonTelemetryMissing); err != nil {
		return err
	}
	config, err := memoryObserverConfigFromEnvironment()
	if err != nil {
		return err
	}
	limit, err := resourceAdmissionObserverMemoryLimit(settings, aggregatePodCgroup)
	if err != nil {
		return err
	}
	var reader fastletresourceadmission.Reader
	if aggregatePodCgroup {
		layout, discoverErr := podcgroup.Discover(podcgroup.HostRoot, podUID)
		if discoverErr != nil {
			return discoverErr
		}
		if layout.Version != podcgroup.VersionV2 {
			return fmt.Errorf("resource admission observer requires cgroup v2, found %s", layout.Version)
		}
		path := filepath.Join(podcgroup.HostRoot, strings.TrimPrefix(filepath.Clean(layout.PodPath), string(filepath.Separator)))
		reader, err = fastletresourceadmission.NewCgroupPathReader(path, "/proc/meminfo", uint64(limit))
	} else {
		reader, err = fastletresourceadmission.NewSelfCgroupReader(uint64(limit))
	}
	if err != nil {
		return err
	}
	observer, err := fastletresourceadmission.NewObserver(reader, manager, config)
	if err != nil {
		return err
	}
	go observer.Run(ctx)
	return nil
}

func resourceAdmissionObserverMemoryLimit(settings *fastletsettings.ResourceAdmissionSettings, aggregatePodCgroup bool) (int64, error) {
	if settings == nil {
		return 0, errors.New("resource admission settings are required")
	}
	limit := settings.RuntimeEnvelope.Memory.Value()
	if aggregatePodCgroup {
		limit = settings.PodMemoryBudget.Value()
	}
	if limit <= 0 {
		return 0, errors.New("resource admission observer memory limit must be positive")
	}
	return limit, nil
}

func memoryObserverConfigFromEnvironment() (fastletresourceadmission.Config, error) {
	config := fastletresourceadmission.DefaultConfig()
	var err error
	config.Interval, err = time.ParseDuration(getEnv("FAST_SANDBOX_MEMORY_OBSERVER_INTERVAL", config.Interval.String()))
	if err != nil {
		return config, fmt.Errorf("parse FAST_SANDBOX_MEMORY_OBSERVER_INTERVAL: %w", err)
	}
	floatValues := []struct {
		name   string
		target *float64
	}{
		{"FAST_SANDBOX_MEMORY_HIGH_WATERMARK", &config.HighWatermark},
		{"FAST_SANDBOX_MEMORY_LOW_WATERMARK", &config.LowWatermark},
		{"FAST_SANDBOX_MEMORY_PSI_AVG10_THRESHOLD", &config.PSIAvg10Threshold},
		{"FAST_SANDBOX_NODE_MEMORY_LOW_WATERMARK", &config.NodeLowWatermark},
		{"FAST_SANDBOX_NODE_MEMORY_RECOVERY_WATERMARK", &config.NodeRecoveryWatermark},
	}
	for _, value := range floatValues {
		parsed, parseErr := strconv.ParseFloat(getEnv(value.name, strconv.FormatFloat(*value.target, 'f', -1, 64)), 64)
		if parseErr != nil {
			return config, fmt.Errorf("parse %s: %w", value.name, parseErr)
		}
		*value.target = parsed
	}
	intValues := []struct {
		name   string
		target *int
	}{
		{"FAST_SANDBOX_MEMORY_HIGH_SAMPLES", &config.HighSamples},
		{"FAST_SANDBOX_MEMORY_RECOVERY_SAMPLES", &config.RecoverySamples},
	}
	for _, value := range intValues {
		parsed, parseErr := strconv.Atoi(getEnv(value.name, strconv.Itoa(*value.target)))
		if parseErr != nil {
			return config, fmt.Errorf("parse %s: %w", value.name, parseErr)
		}
		*value.target = parsed
	}
	if err := config.Validate(); err != nil {
		return config, err
	}
	return config, nil
}

func newNetworkManager(capacity int, podUID string, networkMode runtimecatalog.NetworkMode) (*fastletnetwork.Manager, error) {
	config := fastletnetwork.DefaultConfig(capacity, podUID)
	config.PodName = os.Getenv("POD_NAME")
	config.PodNamespace = os.Getenv("NAMESPACE")
	config.PrivateCIDR = getEnv("FAST_SANDBOX_NETWORK_CIDR", fastletnetwork.DefaultPrivateCIDR)
	config.Bridge = getEnv("FAST_SANDBOX_NETWORK_BRIDGE", fastletnetwork.DefaultBridge)
	config.EgressDevice = getEnv("FAST_SANDBOX_NETWORK_EGRESS_DEVICE", "")
	config.StateRoot = getEnv("FAST_SANDBOX_NETWORK_STATE_ROOT", config.StateRoot)
	config.NetNSRoot = getEnv("FAST_SANDBOX_NETWORK_NETNS_ROOT", config.NetNSRoot)
	config.HostNetNSRoot = getEnv("FAST_SANDBOX_NETWORK_HOST_NETNS_ROOT", config.HostNetNSRoot)
	mtu, err := strconv.Atoi(getEnv("FAST_SANDBOX_NETWORK_MTU", strconv.Itoa(config.MTU)))
	if err != nil || mtu <= 0 {
		return nil, runtimecontract.ErrInvalidConfig
	}
	config.MTU = mtu
	store := fastletnetwork.NewFileStateStore(filepath.Join(config.StateRoot, podUID))
	var driver fastletnetwork.Driver = fastletnetwork.NewLinuxNetNSDriver(fastletnetwork.LinuxDriverConfig{})
	if networkMode == runtimecatalog.NetworkModeFirecracker {
		driver = fastletnetwork.NewGuestVMNetNSDriver(fastletnetwork.LinuxDriverConfig{})
	}
	return fastletnetwork.NewManager(config, driver, store)
}

type networkConfigurable interface {
	SetNetworkManager(*fastletnetwork.Manager)
}

// tuneNeighborGCThresholds raises the neighbour cache GC thresholds so a
// full slot pool (plus host peers) does not thrash the ARP cache into
// constant eviction and re-resolution. Best-effort: fastlet may lack the
// privilege in restricted environments and slot preparation must not
// depend on it.
func tuneNeighborGCThresholds() {
	for _, tuning := range []struct {
		name  string
		value int
	}{
		{"net.ipv4.neigh.default.gc_thresh1", 4096},
		{"net.ipv4.neigh.default.gc_thresh2", 8192},
		{"net.ipv4.neigh.default.gc_thresh3", 16384},
	} {
		argument := fmt.Sprintf("%s=%d", tuning.name, tuning.value)
		if output, err := exec.Command("sysctl", "-w", argument).CombinedOutput(); err != nil {
			klog.Warningf("raise sysctl %s failed: %v: %s", tuning.name, err, strings.TrimSpace(string(output)))
		}
	}
}

type infraConfigurable interface {
	SetInfraManager(*fastletinfra.Manager)
}

type registryConfigurable interface {
	SetRegistryProvider(registryconfig.Provider)
}

type nodeCleanupConfigurable interface {
	SetNodeCleanupClient(nodecleanup.RuntimeProcessCleaner)
}

// agentClientConfigurable wires the optional firecracker runtime-agent
// client into the driver (empty socket = local mode).
type agentClientConfigurable interface {
	SetFastletPodUID(podUID string)
	SetAgentSocket(socketPath string)
}

// stateMoverConfigurable is the optional driver capability for the
// egress-owned state transfers (credential vault, recorded network policy)
// across pause/resume.
type stateMoverConfigurable interface {
	SetStateMover(mover runtimecontract.StateMover)
}

func recoverUntilReady(ctx context.Context, manager *fastletsandbox.SandboxManager, proxyClient *fastletproxy.ControlClient) {
	for attempt := 1; ; attempt++ {
		if err := manager.Recover(ctx); err == nil {
			klog.InfoS("fastlet runtime recovery completed")
			go warmCacheUntilReady(ctx, manager)
			go prepareInfraUntilReady(ctx, manager)
			go watchProxyRoutes(ctx, manager, proxyClient)
			return
		} else if attempt <= 1 {
			klog.ErrorS(err, "Fastlet runtime recovery failed; readiness remains false")
		} else {
			klog.V(2).InfoS("fastlet runtime recovery still failing", "consecutiveFailures", attempt, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func warmCacheUntilReady(ctx context.Context, manager *fastletsandbox.SandboxManager) {
	for attempt := 1; ctx.Err() == nil; attempt++ {
		if err := manager.WarmCache(ctx); err == nil {
			klog.InfoS("asynchronous warmImages preparation completed")
			return
		} else if attempt <= 1 {
			klog.ErrorS(err, "Asynchronous warmImages preparation failed; retrying")
		} else {
			klog.V(2).InfoS("asynchronous warmImages preparation still failing", "consecutiveFailures", attempt, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

func prepareInfraUntilReady(ctx context.Context, manager *fastletsandbox.SandboxManager) {
	for attempt := 1; ctx.Err() == nil; attempt++ {
		if err := manager.PrepareInfra(ctx); err == nil {
			klog.InfoS("fastlet infra component preparation completed")
			return
		} else if attempt <= 1 {
			klog.ErrorS(err, "Fastlet Infra Component preparation failed; revision admission remains disabled")
		} else {
			klog.V(2).InfoS("fastlet infra component preparation still failing", "consecutiveFailures", attempt, "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func newInfraManager(
	podUID string,
	runtimeProfile runtimecatalog.RuntimeProfile,
	planPath string,
	expectedRevision string,
	runtimePlan runtimeenv.ResolvedRuntimePlan,
	registryProvider registryconfig.Provider,
) (*fastletinfra.Manager, error) {
	podRoot, hostRoot, err := fastletinfra.StorePaths(
		podUID,
		runtimePlan.Kubelet.Root,
	)
	if err != nil {
		return nil, err
	}
	podRoot = getEnv("FAST_SANDBOX_INFRA_STORE_ROOT", podRoot)
	hostRoot = getEnv("FAST_SANDBOX_INFRA_HOST_ROOT", hostRoot)
	store, err := fastletinfra.NewArtifactStore(podRoot, hostRoot)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(planPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var plan infracatalog.Plan
	if err := json.NewDecoder(file).Decode(&plan); err != nil {
		return nil, err
	}
	if expectedRevision != "" && plan.Revision != expectedRevision {
		return nil, fmt.Errorf("infra plan revision %s does not match expected %s", plan.Revision, expectedRevision)
	}
	ociOpener := fastletinfra.NewContainerdOCIArtifactOpener(
		runtimePlan.Containerd.Socket,
		runtimePlan.Containerd.Snapshotter,
		runtimeProfile.ContainerdNamespace(),
		registryProvider,
	)
	return fastletinfra.NewManagerWithConfig(fastletinfra.ManagerConfig{
		Plan: plan, RuntimeProfile: runtimeProfile, Store: store,
		Resolver: fastletinfra.NewPlatformResolverWithOptions(fastletinfra.PlatformResolverOptions{
			OCI: ociOpener,
		}),
		SandboxInitPath: getEnv("FAST_SANDBOX_SANDBOX_INIT_PATH", "/opt/fast-sandbox/bin/sandbox-init"),
	})
}

func loadRuntimePlan(path string) (runtimeenv.ResolvedRuntimePlan, error) {
	file, err := os.Open(path)
	if err != nil {
		return runtimeenv.ResolvedRuntimePlan{}, err
	}
	defer file.Close()
	return runtimeenv.DecodePlan(file)
}

func watchProxyRoutes(ctx context.Context, manager *fastletsandbox.SandboxManager, proxyClient *fastletproxy.ControlClient) {
	for ctx.Err() == nil {
		if err := manager.ReconcileProxyRoutes(ctx); err != nil {
			klog.ErrorS(err, "Reconcile Fastlet Proxy routes after control reconnect")
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
				continue
			}
		}
		if err := proxyClient.Watch(ctx, func(fastletproxy.Event) error { return nil }); err != nil && ctx.Err() == nil {
			manager.MarkProxyRouteUnavailable()
			klog.ErrorS(err, "Fastlet Proxy control watch disconnected; route readiness revoked")
		}
	}
}

// readSetting returns the settings file content when the settings ConfigMap
// is mounted (ok=true). A missing file reports ok=false so callers can fall
// back to the legacy environment variables used by local runs without the
// control plane.
func readSetting(dir, key string) ([]byte, bool, error) {
	path := filepath.Join(dir, key)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read Fastlet setting %s: %w", path, err)
	}
	return data, true, nil
}

func warmImagesFromSettings(dir string) ([]string, error) {
	data, ok, err := readSetting(dir, fastletsettings.WarmImagesKey)
	if err != nil {
		return nil, err
	}
	if ok {
		var images []string
		if err := json.Unmarshal(data, &images); err != nil {
			return nil, err
		}
		return images, nil
	}
	return warmImagesFromEnvironment()
}

func actionHandlersFromSettings(dir string) ([]apiv1alpha2.ActionHandler, error) {
	data, ok, err := readSetting(dir, fastletsettings.ActionHandlersKey)
	if err != nil {
		return nil, err
	}
	if ok {
		var handlers []apiv1alpha2.ActionHandler
		if err := json.Unmarshal(data, &handlers); err != nil {
			return nil, err
		}
		return handlers, nil
	}
	return actionHandlersFromEnvironment()
}

func resourceProfileFromSettings(dir string) (apiv1alpha2.SandboxResourceProfile, error) {
	data, ok, err := readSetting(dir, fastletsettings.ResourceProfileKey)
	if err != nil {
		return apiv1alpha2.SandboxResourceProfile{}, err
	}
	if ok {
		var profile apiv1alpha2.SandboxResourceProfile
		if err := json.Unmarshal(data, &profile); err != nil {
			return apiv1alpha2.SandboxResourceProfile{}, err
		}
		if err := apiv1alpha2.ValidateSandboxResourceProfile(profile); err != nil {
			return apiv1alpha2.SandboxResourceProfile{}, err
		}
		return profile, nil
	}
	return resourceProfileFromEnvironment()
}

func resourceAdmissionFromSettings(dir string, profile apiv1alpha2.SandboxResourceProfile) (fastletsettings.ResourceAdmissionSettings, bool, error) {
	data, ok, err := readSetting(dir, fastletsettings.ResourceAdmissionKey)
	if err != nil {
		return fastletsettings.ResourceAdmissionSettings{}, false, err
	}
	if !ok {
		if profile.ResourceMode() == apiv1alpha2.SandboxResourceModeResourceAware {
			return fastletsettings.ResourceAdmissionSettings{}, false, fmt.Errorf("%s is required for a ResourceAware Pool", fastletsettings.ResourceAdmissionKey)
		}
		return fastletsettings.ResourceAdmissionSettings{}, false, nil
	}
	var settings fastletsettings.ResourceAdmissionSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return fastletsettings.ResourceAdmissionSettings{}, false, err
	}
	if err := settings.Validate(profile); err != nil {
		return fastletsettings.ResourceAdmissionSettings{}, false, err
	}
	return settings, true, nil
}

func warmImagesFromEnvironment() ([]string, error) {
	value := getEnv("FAST_SANDBOX_WARM_IMAGES", "[]")
	var images []string
	if err := json.Unmarshal([]byte(value), &images); err != nil {
		return nil, err
	}
	return images, nil
}

func actionHandlersFromEnvironment() ([]apiv1alpha2.ActionHandler, error) {
	value := getEnv("FAST_SANDBOX_ACTION_HANDLERS", "[]")
	var handlers []apiv1alpha2.ActionHandler
	if err := json.Unmarshal([]byte(value), &handlers); err != nil {
		return nil, err
	}
	return handlers, nil
}

func resourceProfileFromEnvironment() (apiv1alpha2.SandboxResourceProfile, error) {
	cpu, err := resource.ParseQuantity(getEnv("FAST_SANDBOX_RESOURCE_CPU", "1"))
	if err != nil {
		return apiv1alpha2.SandboxResourceProfile{}, err
	}
	memory, err := resource.ParseQuantity(getEnv("FAST_SANDBOX_RESOURCE_MEMORY", "512Mi"))
	if err != nil {
		return apiv1alpha2.SandboxResourceProfile{}, err
	}
	pids, err := strconv.ParseInt(getEnv("FAST_SANDBOX_RESOURCE_PIDS", "256"), 10, 64)
	if err != nil {
		return apiv1alpha2.SandboxResourceProfile{}, err
	}
	profile := apiv1alpha2.NewLegacySandboxResourceProfile(cpu, memory, pids)
	if err := apiv1alpha2.ValidateSandboxResourceProfile(profile); err != nil {
		return apiv1alpha2.SandboxResourceProfile{}, err
	}
	return profile, nil
}

func capacityFromEnvironment() int {
	value, err := strconv.Atoi(getEnv("FASTLET_CAPACITY", "5"))
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func shutdownTracing(shutdown observability.Shutdown) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		klog.ErrorS(err, "Flush OpenTelemetry traces")
	}
}
