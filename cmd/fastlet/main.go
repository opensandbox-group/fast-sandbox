package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	fastletnetwork "fast-sandbox/internal/fastlet/network"
	"fast-sandbox/internal/fastlet/runtime"
	"fast-sandbox/internal/fastlet/server"
	"fast-sandbox/internal/fastletproxy"
	"fast-sandbox/internal/infracatalog"
	"fast-sandbox/internal/observability"
	"fast-sandbox/internal/runtimecatalog"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"
)

func main() {
	flag.Parse()
	klog.Info("starting sandbox fastlet")
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
	runtimeName := getEnv("FAST_SANDBOX_RUNTIME", "container")
	runtimeSocket := getEnv("RUNTIME_SOCKET", "")
	runtimeProfile, err := runtimecatalog.Builtin().Resolve(apiv1alpha1.RuntimeName(runtimeName))
	if err != nil {
		klog.ErrorS(err, "Failed to resolve runtime profile")
		os.Exit(1)
	}
	injectedRuntimeHash := getEnv("FAST_SANDBOX_RUNTIME_PROFILE_HASH", runtimeProfile.ProfileHash)
	if injectedRuntimeHash != runtimeProfile.ProfileHash {
		klog.ErrorS(runtime.ErrSandboxProfileMismatch, "Injected runtime profile hash does not match built-in catalog", "injected", injectedRuntimeHash, "expected", runtimeProfile.ProfileHash)
		os.Exit(1)
	}
	resourceProfile, err := resourceProfileFromEnvironment()
	if err != nil {
		klog.ErrorS(err, "Failed to resolve Sandbox resource profile")
		os.Exit(1)
	}
	warmImages, err := warmImagesFromEnvironment()
	if err != nil {
		klog.ErrorS(err, "Failed to parse warmImages")
		os.Exit(1)
	}

	klog.InfoS("Fastlet Info", "PodName", podName, "PodIP", podIP, "NodeName", nodeName, "Namespace", namespace)
	klog.InfoS("Runtime", "Name", runtimeName, "Socket", runtimeSocket)

	ctx := context.Background()
	var rt runtime.RuntimeDriver

	rt, _, err = runtime.NewDriverFactory(runtimecatalog.Builtin(), runtime.NewHostCapabilityProber()).Create(ctx, runtimeProfile.Name, runtimeSocket)

	if err != nil {
		klog.ErrorS(err, "Failed to initialize runtime")
		os.Exit(1)
	}
	defer rt.Close()

	rt.SetNamespace(namespace)
	if runtimeProfile.UsesFastletNetNS() {
		networkManager, err := newNetworkManager(capacityFromEnvironment(), podUID)
		if err != nil {
			klog.ErrorS(err, "Failed to configure Fastlet-owned network")
			os.Exit(1)
		}
		if err := networkManager.Initialize(ctx); err != nil {
			klog.ErrorS(err, "Failed to initialize Fastlet-owned network")
			os.Exit(1)
		}
		configurable, ok := rt.(runtime.NetworkConfigurable)
		if !ok {
			klog.ErrorS(runtime.ErrUnsupportedRuntime, "Runtime profile requires Linux netns but driver is not network configurable")
			os.Exit(1)
		}
		configurable.SetNetworkManager(networkManager)
		klog.InfoS("Fastlet-owned network initialized", "capacity", networkManager.Snapshot().Capacity, "cleanSlots", networkManager.Snapshot().Clean)
	}
	infraProfileName := getEnv("FAST_SANDBOX_INFRA_PROFILE", "minimal")
	infraProfileHash := getEnv("FAST_SANDBOX_INFRA_PROFILE_HASH", "")
	infraManager, err := newInfraManager(podUID, runtimeProfile, infraProfileName, infraProfileHash)
	if err != nil {
		klog.ErrorS(err, "Failed to configure InfraProfile")
		os.Exit(1)
	}
	infraConfigurable, ok := rt.(runtime.InfraConfigurable)
	if !ok {
		klog.ErrorS(runtime.ErrUnsupportedRuntime, "Runtime driver cannot accept an InfraProfile augmentation plan")
		os.Exit(1)
	}
	infraConfigurable.SetInfraManager(infraManager)

	klog.InfoS("Runtime initialized successfully", "name", runtimeName)

	proxyControlClient := fastletproxy.NewControlClient(getEnv("FASTLET_PROXY_CONTROL_SOCKET", fastletproxy.DefaultControlSocket))
	sandboxManager, err := runtime.NewSandboxManagerWithConfig(rt, runtime.SandboxManagerConfig{
		Capacity: capacityFromEnvironment(), RuntimeName: runtimeProfile.Name, RuntimeProfileHash: runtimeProfile.ProfileHash, ResourceProfile: &resourceProfile,
		FastletPodUID: podUID, RecoverOnStart: true,
		WarmImages:     warmImages,
		RoutePublisher: fastletproxy.NewRoutePublisher(proxyControlClient),
		InfraProfile:   infraProfileName, InfraProfileHash: infraManager.ProfileHash(), InfraManager: infraManager,
	})
	if err != nil {
		klog.ErrorS(err, "Failed to initialize Sandbox manager")
		os.Exit(1)
	}
	defer sandboxManager.Close()
	go recoverUntilReady(ctx, sandboxManager, proxyControlClient)

	fastletServer := server.NewFastletServer(fastletPort, sandboxManager)
	klog.InfoS("Starting Fastlet HTTP Server", "port", fastletPort)

	if err := fastletServer.Start(); err != nil {
		klog.ErrorS(err, "Fastlet server failed")
		os.Exit(1)
	}
}

func newNetworkManager(capacity int, podUID string) (*fastletnetwork.Manager, error) {
	config := fastletnetwork.DefaultConfig(capacity, podUID)
	config.PodName = os.Getenv("POD_NAME")
	config.PodNamespace = os.Getenv("NAMESPACE")
	config.PrivateCIDR = getEnv("FAST_SANDBOX_NETWORK_CIDR", config.PrivateCIDR)
	config.Bridge = getEnv("FAST_SANDBOX_NETWORK_BRIDGE", config.Bridge)
	config.EgressDevice = getEnv("FAST_SANDBOX_NETWORK_EGRESS_DEVICE", "")
	config.StateRoot = getEnv("FAST_SANDBOX_NETWORK_STATE_ROOT", config.StateRoot)
	config.NetNSRoot = getEnv("FAST_SANDBOX_NETWORK_NETNS_ROOT", config.NetNSRoot)
	config.HostNetNSRoot = getEnv("FAST_SANDBOX_NETWORK_HOST_NETNS_ROOT", config.HostNetNSRoot)
	mtu, err := strconv.Atoi(getEnv("FAST_SANDBOX_NETWORK_MTU", strconv.Itoa(config.MTU)))
	if err != nil || mtu <= 0 {
		return nil, runtime.ErrInvalidConfig
	}
	config.MTU = mtu
	store := fastletnetwork.NewFileStateStore(filepath.Join(config.StateRoot, podUID))
	return fastletnetwork.NewManager(config, fastletnetwork.NewLinuxNetNSDriver(fastletnetwork.LinuxDriverConfig{}), store)
}

func recoverUntilReady(ctx context.Context, manager *runtime.SandboxManager, proxyClient *fastletproxy.ControlClient) {
	for {
		if err := manager.Recover(ctx); err == nil {
			klog.Info("Fastlet runtime recovery completed")
			go func() {
				if err := manager.WarmCache(ctx); err != nil {
					klog.ErrorS(err, "Asynchronous warmImages preparation failed")
				}
			}()
			go prepareInfraUntilReady(ctx, manager)
			go watchProxyRoutes(ctx, manager, proxyClient)
			return
		} else {
			klog.ErrorS(err, "Fastlet runtime recovery failed; readiness remains false")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func prepareInfraUntilReady(ctx context.Context, manager *runtime.SandboxManager) {
	for ctx.Err() == nil {
		if err := manager.PrepareInfra(ctx); err == nil {
			klog.Info("Fastlet InfraProfile preparation completed")
			return
		} else {
			klog.ErrorS(err, "Fastlet InfraProfile preparation failed; profile admission remains disabled")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func newInfraManager(podUID string, runtimeProfile runtimecatalog.RuntimeProfile, profileName, expectedHash string) (*fastletinfra.Manager, error) {
	podRoot, hostRoot, err := fastletinfra.DefaultStorePaths(podUID)
	if err != nil {
		return nil, err
	}
	podRoot = getEnv("FAST_SANDBOX_INFRA_STORE_ROOT", podRoot)
	hostRoot = getEnv("FAST_SANDBOX_INFRA_HOST_ROOT", hostRoot)
	store, err := fastletinfra.NewArtifactStore(podRoot, hostRoot)
	if err != nil {
		return nil, err
	}
	staticRoots := []string(nil)
	if value := getEnv("FAST_SANDBOX_INFRA_STATIC_ROOTS", ""); value != "" {
		staticRoots = filepath.SplitList(value)
	}
	return fastletinfra.NewManagerWithConfig(fastletinfra.ManagerConfig{
		Catalog: infracatalog.Builtin(), RuntimeProfile: runtimeProfile, ProfileName: profileName,
		ExpectedProfileHash: expectedHash, Store: store, Resolver: fastletinfra.NewPlatformResolver(staticRoots),
		SandboxInitPath:   getEnv("FAST_SANDBOX_SANDBOX_INIT_PATH", "/opt/fast-sandbox/bin/sandbox-init"),
		SandboxTunnelPath: getEnv("FAST_SANDBOX_SANDBOX_TUNNEL_PATH", "/opt/fast-sandbox/bin/sandbox-tunnel"),
	})
}

func watchProxyRoutes(ctx context.Context, manager *runtime.SandboxManager, proxyClient *fastletproxy.ControlClient) {
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

func warmImagesFromEnvironment() ([]string, error) {
	value := getEnv("FAST_SANDBOX_WARM_IMAGES", "[]")
	var images []string
	if err := json.Unmarshal([]byte(value), &images); err != nil {
		return nil, err
	}
	return images, nil
}

func resourceProfileFromEnvironment() (apiv1alpha1.SandboxResourceProfile, error) {
	cpu, err := resource.ParseQuantity(getEnv("FAST_SANDBOX_RESOURCE_CPU", "1"))
	if err != nil {
		return apiv1alpha1.SandboxResourceProfile{}, err
	}
	memory, err := resource.ParseQuantity(getEnv("FAST_SANDBOX_RESOURCE_MEMORY", "512Mi"))
	if err != nil {
		return apiv1alpha1.SandboxResourceProfile{}, err
	}
	pids, err := strconv.ParseInt(getEnv("FAST_SANDBOX_RESOURCE_PIDS", "256"), 10, 64)
	if err != nil {
		return apiv1alpha1.SandboxResourceProfile{}, err
	}
	profile := apiv1alpha1.SandboxResourceProfile{CPU: cpu, Memory: memory, PIDs: pids}
	if err := apiv1alpha1.ValidateSandboxResourceProfile(profile); err != nil {
		return apiv1alpha1.SandboxResourceProfile{}, err
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
