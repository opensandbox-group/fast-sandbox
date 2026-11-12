package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	_ "net/http/pprof" //nolint:gosec // pprof is intentionally exposed on localhost:6060 for troubleshooting

	fastpathv2 "fast-sandbox/api/proto/v2"
	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	"fast-sandbox/internal/artifactstore"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	"fast-sandbox/internal/componentlog"
	"fast-sandbox/internal/controlplane"
	"fast-sandbox/internal/controlplane/fastletcontrol"
	"fast-sandbox/internal/controlplane/fastpath"
	orchestration "fast-sandbox/internal/controlplane/orchestrator"
	"fast-sandbox/internal/controlplane/placement"
	"fast-sandbox/internal/controlplane/reconciler"
	routeauth "fast-sandbox/internal/dataplane/auth"
	"fast-sandbox/internal/observability"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/runtimeenv"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apiv1alpha2.AddToScheme(scheme))
}

func main() { //nolint:gocognit,maintidx // pre-existing flag wiring and role bootstrap; refactor tracked separately
	var roleValue string
	var metricsAddress string
	var probeAddress string
	var fastPathAddress string
	var fastletPort int
	var heartbeatInterval time.Duration
	var heartbeatTimeout time.Duration
	var heartbeatConcurrency int
	var fastletDrainTimeout time.Duration
	var fastletProxyImage string
	var sandboxTemplateBuilderImage string
	var sandboxReconcileWorkers int
	var routeVerifyPublicKey string
	var routeSigningPrivateKey string
	var routeCredentialTTL time.Duration
	var sandboxProxyBaseURL string
	var runtimeEnvironmentNamespace string
	var runtimeEnvironmentConfigMap string
	var resourceAwareStaticAdmission bool
	var resourceAwareMemoryAdmission bool
	var resourceAwarePlacement bool
	var resourceAwarePoolScaling bool
	var resourceAwareStartupGrace time.Duration
	var resourceAwareScaleDownStabilization time.Duration
	var cacheNamespaces string
	var kubeClientQPS float64
	var kubeClientBurst int

	flag.StringVar(&roleValue, "role", "all", "Control-plane role: fastpath, controller, or all.")
	flag.StringVar(&metricsAddress, "metrics-bind-address", ":9091", "Metrics listen address.")
	flag.StringVar(&probeAddress, "health-probe-bind-address", ":5758", "Health probe listen address.")
	flag.StringVar(&fastPathAddress, "fastpath-bind-address", ":9090", "FastPath gRPC listen address.")
	flag.IntVar(&fastletPort, "fastlet-port", 5758, "Fastlet control port.")
	flag.DurationVar(&heartbeatInterval, "fastlet-heartbeat-interval", 20*time.Second, "Base Fastlet heartbeat interval; actual probes use jitter.")
	flag.DurationVar(&heartbeatTimeout, "fastlet-heartbeat-timeout", 5*time.Second, "Timeout for one Fastlet heartbeat request.")
	flag.IntVar(&heartbeatConcurrency, "fastlet-heartbeat-concurrency", 8, "Maximum concurrent Fastlet heartbeat requests.")
	flag.DurationVar(&fastletDrainTimeout, "fastlet-drain-timeout", 5*time.Minute, "Maximum time to wait for a draining Fastlet Pod to become empty before applying Sandbox failure policies.")
	flag.StringVar(&fastletProxyImage, "fastlet-proxy-image", envOrDefault("FASTLET_PROXY_IMAGE", "fast-sandbox/fastlet-proxy:dev"), "Image injected as the platform-owned Fastlet Proxy sidecar.")
	flag.StringVar(&sandboxTemplateBuilderImage, "sandboxtemplate-builder-image", envOrDefault("SANDBOXTEMPLATE_BUILDER_IMAGE", "fast-sandbox/sandboxtemplate-builder:dev"), "Image that executes SandboxTemplate golden-image builds.")
	flag.StringVar(&routeVerifyPublicKey, "route-verify-public-key", os.Getenv("FAST_SANDBOX_ROUTE_VERIFY_PUBLIC_KEY"), "Comma-separated base64 Ed25519 public keys injected into data-plane proxies.")
	flag.StringVar(&routeSigningPrivateKey, "route-signing-private-key", os.Getenv("FAST_SANDBOX_ROUTE_SIGNING_PRIVATE_KEY"), "Base64 Ed25519 seed/private key used only by FastPath.")
	flag.DurationVar(&routeCredentialTTL, "route-credential-ttl", 5*time.Minute, "Lifetime of a Sandbox route credential.")
	flag.StringVar(&sandboxProxyBaseURL, "sandbox-proxy-base-url", envOrDefault("FAST_SANDBOX_PROXY_BASE_URL", "http://fast-sandbox-proxy.fast-sandbox-system.svc:8080"), "Client-visible Sandbox Proxy base URL.")
	flag.IntVar(&sandboxReconcileWorkers, "sandbox-reconcile-workers", 4, "Concurrent Sandbox reconcilers (per-key serialization is guaranteed by the workqueue).")
	flag.StringVar(&runtimeEnvironmentNamespace, "runtime-environment-namespace", envOrDefault("FAST_SANDBOX_RUNTIME_ENVIRONMENT_NAMESPACE", runtimeenv.SystemNamespace), "Namespace containing the platform runtime environment ConfigMap.")
	flag.StringVar(&runtimeEnvironmentConfigMap, "runtime-environment-configmap", envOrDefault("FAST_SANDBOX_RUNTIME_ENVIRONMENT_CONFIGMAP", runtimeenv.ConfigMapName), "Platform runtime environment ConfigMap name.")
	flag.BoolVar(&resourceAwareStaticAdmission, "resource-aware-static-admission", true, "Enable Fastlet CPU and memory commitment admission for ResourceAware Pools.")
	flag.BoolVar(&resourceAwareMemoryAdmission, "resource-aware-memory-admission", true, "Enable Fastlet dynamic memory-pressure admission for ResourceAware Pools.")
	flag.BoolVar(&resourceAwarePlacement, "resource-aware-placement", true, "Enable FastPath ResourceAware eligibility and ranking.")
	flag.BoolVar(&resourceAwarePoolScaling, "resource-aware-pool-scaling", true, "Enable ResourceAware effective-capacity Pool scaling.")
	flag.DurationVar(&resourceAwareStartupGrace, "resource-aware-fastlet-startup-grace", 2*time.Minute, "Maximum time a Fastlet without a heartbeat contributes nominal in-flight capacity.")
	flag.DurationVar(&resourceAwareScaleDownStabilization, "resource-aware-scale-down-stabilization", time.Minute, "Continuous excess-capacity window required before ResourceAware scale-down.")
	flag.StringVar(&cacheNamespaces, "namespaces", "", "Comma-separated namespaces the informer cache watches; empty watches the whole cluster. Must cover every namespace holding SandboxPool/Sandbox objects and the runtime environment ConfigMap.")
	flag.Float64Var(&kubeClientQPS, "kube-client-qps", 0, "API-server client rate limit in queries per second; 0 keeps the client-go default.")
	flag.IntVar(&kubeClientBurst, "kube-client-burst", 0, "API-server client burst budget for the rate limiter; 0 keeps the client-go default.")
	componentlog.InitFlags(nil)
	flag.Parse()

	role, err := controlplane.ParseRole(roleValue)
	if err != nil {
		klog.ErrorS(err, "Invalid control-plane role")
		os.Exit(1)
	}
	closeLogs, err := componentlog.Configure(string(role))
	if err != nil {
		klog.ErrorS(err, "configure component logging")
		klog.Flush()
		os.Exit(1)
	}
	defer closeLogs()
	traceShutdown, err := observability.Configure(context.Background(), "fast-sandbox-"+string(role))
	if err != nil {
		klog.ErrorS(err, "Configure OpenTelemetry")
		os.Exit(1)
	}
	defer shutdownTracing(traceShutdown)
	if heartbeatInterval <= 0 || heartbeatTimeout <= 0 || heartbeatConcurrency <= 0 || fastletDrainTimeout <= 0 || resourceAwareStartupGrace <= 0 || resourceAwareScaleDownStabilization <= 0 {
		klog.ErrorS(nil, "Heartbeat and drain timing values must be positive")
		os.Exit(1)
	}
	if resourceAwarePlacement && !resourceAwareStaticAdmission {
		klog.ErrorS(nil, "resource-aware-placement requires resource-aware-static-admission")
		os.Exit(1)
	}
	if resourceAwarePoolScaling && !resourceAwarePlacement {
		klog.ErrorS(nil, "resource-aware-pool-scaling requires resource-aware-placement")
		os.Exit(1)
	}
	if err := validateKubeClientTuning(kubeClientQPS, kubeClientBurst); err != nil {
		klog.ErrorS(err, "Invalid kube client tuning")
		os.Exit(1)
	}
	cacheNamespaceSet := parseCacheNamespaces(cacheNamespaces)
	if err := validateControllerCacheScoping(cacheNamespaceSet, runtimeEnvironmentNamespace, role.RunsControllers()); err != nil {
		klog.ErrorS(err, "Invalid cache scoping")
		os.Exit(1)
	}
	if role.RunsControllers() && routeVerifyPublicKey == "" {
		klog.ErrorS(nil, "route-verify-public-key is required for controller roles; data plane fails closed")
		os.Exit(1)
	}
	if role.RunsControllers() {
		if _, parseErr := routeauth.ParsePublicKeySet(routeVerifyPublicKey); parseErr != nil {
			klog.ErrorS(parseErr, "route-verify-public-key is invalid; data plane fails closed")
			os.Exit(1)
		}
	}
	var credentialIssuer *routeauth.Issuer
	if role.RunsFastPath() {
		privateKey, parseErr := routeauth.ParsePrivateKey(routeSigningPrivateKey)
		if parseErr != nil {
			klog.ErrorS(parseErr, "route-signing-private-key is required for FastPath roles; credential issuance fails closed")
			os.Exit(1)
		}
		credentialIssuer, err = routeauth.NewIssuer(privateKey, routeCredentialTTL, time.Now)
		if err != nil {
			klog.ErrorS(err, "Configure route credential issuer")
			os.Exit(1)
		}
	}
	ctrl.SetLogger(klog.NewKlogr())

	restConfig := ctrl.GetConfigOrDie()
	if kubeClientQPS > 0 {
		restConfig.QPS = float32(kubeClientQPS)
		restConfig.Burst = kubeClientBurst
	}
	managerOptions := ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       metricsserver.Options{BindAddress: metricsAddress},
		HealthProbeBindAddress:        probeAddress,
		LeaderElection:                role.LeaderElection(),
		LeaderElectionID:              "fast-sandbox-reconciler.sandbox.fast.io",
		LeaderElectionReleaseOnCancel: true,
	}
	if namespaces, scoped := cacheNamespaceConfigs(cacheNamespaceSet); scoped {
		managerOptions.Cache = cache.Options{DefaultNamespaces: namespaces}
		klog.InfoS("scoping informer cache to namespaces", "namespaces", strings.Join(sortedCacheNamespaceNames(namespaces), ","))
	}
	manager, err := ctrl.NewManager(restConfig, managerOptions)
	if err != nil {
		klog.ErrorS(err, "Create controller-runtime manager")
		os.Exit(1)
	}
	durableClient, err := client.New(manager.GetConfig(), client.Options{Scheme: manager.GetScheme()})
	if err != nil {
		klog.ErrorS(err, "Create direct API-server client")
		os.Exit(1)
	}

	registry := placement.NewInMemoryRegistry()
	registry.SetServerSeed(os.Getenv("POD_UID"))
	catalog := runtimecatalog.Builtin()
	fastletClient := fastletapi.NewFastletClient(fastletPort)
	orchestrator := &orchestration.Orchestrator{
		Client: durableClient, Registry: registry, FastletClient: fastletClient, Catalog: catalog,
		DisableResourceAwarePlacement: !resourceAwarePlacement,
	}

	if role.RunsControllers() {
		if err := (&reconciler.SandboxReconciler{
			Client: manager.GetClient(), Scheme: manager.GetScheme(), Orchestrator: orchestrator,
			MaxConcurrentReconciles: sandboxReconcileWorkers,
		}).SetupWithManager(manager); err != nil {
			klog.ErrorS(err, "Register Sandbox controller")
			os.Exit(1)
		}
		if err := (&reconciler.SandboxPoolReconciler{
			Client: manager.GetClient(), DurableReader: durableClient, Scheme: manager.GetScheme(), Registry: registry, Catalog: catalog,
			FastletDrainer: fastletClient, DrainTimeout: fastletDrainTimeout,
			FastletProxyImage: fastletProxyImage, RouteVerifyPublicKey: routeVerifyPublicKey,
			RuntimeEnvironmentNamespace: runtimeEnvironmentNamespace, RuntimeEnvironmentConfigMap: runtimeEnvironmentConfigMap,
			FastletStartupGrace:                 resourceAwareStartupGrace,
			ResourceScaleDownStabilization:      resourceAwareScaleDownStabilization,
			DisableResourceAwareStaticAdmission: !resourceAwareStaticAdmission,
			DisableResourceAwareMemoryAdmission: !resourceAwareMemoryAdmission,
			DisableResourceAwarePoolScaling:     !resourceAwarePoolScaling,
		}).SetupWithManager(manager); err != nil {
			klog.ErrorS(err, "Register SandboxPool controller")
			os.Exit(1)
		}
		if err := (&reconciler.SandboxTemplateReconciler{
			Client: manager.GetClient(), Scheme: manager.GetScheme(),
			BuilderImage: sandboxTemplateBuilderImage, BuildTTL: 24 * time.Hour,
			ArtifactStore: artifactstore.Loader{},
		}).SetupWithManager(manager); err != nil {
			klog.ErrorS(err, "Register SandboxTemplate controller")
			os.Exit(1)
		}
		if err := (&reconciler.SandboxSnapshotReconciler{
			Client: manager.GetClient(), Scheme: manager.GetScheme(), Orchestrator: orchestrator,
		}).SetupWithManager(manager); err != nil {
			klog.ErrorS(err, "Register SandboxSnapshot controller")
			os.Exit(1)
		}
	}

	runContext := ctrl.SetupSignalHandler()
	heartbeatLoop := fastletcontrol.NewLoop(manager.GetCache(), registry, fastletClient)
	heartbeatLoop.Interval = heartbeatInterval
	heartbeatLoop.RequestTimeout = heartbeatTimeout
	heartbeatLoop.MaxConcurrent = heartbeatConcurrency
	go heartbeatLoop.Start(runContext)

	var cacheReady atomic.Bool
	go func() {
		if manager.GetCache().WaitForCacheSync(runContext) {
			cacheReady.Store(true)
		}
	}()
	if err := manager.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		klog.ErrorS(err, "Register health check")
		os.Exit(1)
	}
	if err := manager.AddReadyzCheck("cache-ready", func(*http.Request) error {
		if !cacheReady.Load() {
			return errors.New("Kubernetes watch cache has not synchronized")
		}
		return nil
	}); err != nil {
		klog.ErrorS(err, "Register readiness check")
		os.Exit(1)
	}

	if role.RunsFastPath() {
		listener, err := net.Listen("tcp", fastPathAddress)
		if err != nil {
			klog.ErrorS(err, "Listen for FastPath", "address", fastPathAddress)
			os.Exit(1)
		}
		grpcServer := grpc.NewServer(grpc.ChainUnaryInterceptor(observability.UnaryServerInterceptor("fastpath")))
		fastpathv2.RegisterFastPathServiceServer(grpcServer, &fastpath.Server{
			K8sClient: durableClient, RouteCache: manager.GetClient(), Orchestrator: orchestrator,
			DiagnosticsClient: fastletClient,
			CredentialIssuer:  credentialIssuer, SandboxProxyBaseURL: sandboxProxyBaseURL,
		})
		go func() {
			<-runContext.Done()
			grpcServer.GracefulStop()
		}()
		go func() {
			klog.InfoS("fastPath serving", "address", fastPathAddress, "leaderElection", false)
			if err := grpcServer.Serve(listener); err != nil {
				klog.ErrorS(err, "FastPath gRPC server exited")
			}
		}()
	}

	go func() {
		klog.ErrorS(http.ListenAndServe("localhost:6060", nil), "pprof server exited")
	}()
	klog.InfoS("starting control plane", "role", role, "leaderElection", role.LeaderElection())
	if err := manager.Start(runContext); err != nil {
		klog.ErrorS(err, "Control-plane manager exited")
		os.Exit(1)
	}
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// parseCacheNamespaces splits the comma-separated --namespaces value
// into a set. Empty or whitespace-only entries are dropped; an empty result
// means cluster-wide watching.
func parseCacheNamespaces(value string) map[string]struct{} {
	namespaces := map[string]struct{}{}
	for _, name := range strings.Split(value, ",") {
		if name = strings.TrimSpace(name); name != "" {
			namespaces[name] = struct{}{}
		}
	}
	return namespaces
}

// validateKubeClientTuning requires qps and burst to be set as a pair and to
// stay non-negative; zero keeps the client-go defaults.
func validateKubeClientTuning(qps float64, burst int) error {
	if (qps == 0) != (burst == 0) {
		return errors.New("kube-client-qps and kube-client-burst must be set together")
	}
	if qps < 0 || burst < 0 {
		return errors.New("kube-client-qps and kube-client-burst must not be negative")
	}
	return nil
}

// validateControllerCacheScoping rejects a scoped controller-role cache that
// cannot observe the platform runtime environment ConfigMap; an empty set
// keeps cluster-wide semantics and needs no check.
func validateControllerCacheScoping(namespaces map[string]struct{}, runtimeEnvironmentNamespace string, runsControllers bool) error {
	if !runsControllers || len(namespaces) == 0 {
		return nil
	}
	runtimeNamespace := runtimeEnvironmentNamespace
	if runtimeNamespace == "" {
		runtimeNamespace = runtimeenv.SystemNamespace
	}
	if _, ok := namespaces[runtimeNamespace]; !ok {
		return fmt.Errorf("--namespaces must include the runtime environment namespace %q", runtimeNamespace)
	}
	return nil
}

// cacheNamespaceConfigs converts a namespace set into cache-scoping configs;
// the second return value reports whether the cache should be scoped at all.
func cacheNamespaceConfigs(namespaces map[string]struct{}) (map[string]cache.Config, bool) {
	if len(namespaces) == 0 {
		return nil, false
	}
	configs := make(map[string]cache.Config, len(namespaces))
	for name := range namespaces {
		configs[name] = cache.Config{}
	}
	return configs, true
}

func sortedCacheNamespaceNames(configs map[string]cache.Config) []string {
	names := make([]string, 0, len(configs))
	for name := range configs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func shutdownTracing(shutdown observability.Shutdown) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		klog.ErrorS(err, "Flush OpenTelemetry traces")
	}
}
