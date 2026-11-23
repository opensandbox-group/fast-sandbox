package main

import (
	"context"
	"crypto/tls"
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
	"google.golang.org/grpc/credentials"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
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
	"fast-sandbox/internal/fastletauth"
	"fast-sandbox/internal/observability"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
	"fast-sandbox/internal/runtimeenv"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apiv1alpha2.AddToScheme(scheme))
}

func main() { //nolint:gocognit,gocyclo,funlen,maintidx // pre-existing flag wiring and role bootstrap; refactor tracked separately
	var roleValue string
	var metricsAddress string
	var probeAddress string
	var fastPathAddress string
	var fastPathHTTPPort int
	var fastPathAuthEnabled bool
	var fastPathPolicy, fastPathCert, fastPathKey string
	var fastletPort int
	var heartbeatInterval time.Duration
	var heartbeatTimeout time.Duration
	var heartbeatConcurrency int
	var fastletDrainTimeout time.Duration
	var fastletProxyImage string
	var fastletProxyRequestCPU string
	var fastletProxyRequestMemory string
	var fastletProxyLimitCPU string
	var fastletProxyLimitMemory string
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
	var fastletAuthSubjects string
	defaultFastletProxyResources := reconciler.DefaultFastletProxyResources()

	flag.StringVar(&roleValue, "role", "all", "Control-plane role: fastpath, controller, or all.")
	flag.StringVar(&metricsAddress, "metrics-bind-address", ":9091", "Metrics listen address.")
	flag.StringVar(&probeAddress, "health-probe-bind-address", ":5758", "Health probe listen address.")
	flag.StringVar(&fastPathAddress, "fastpath-bind-address", ":9090", "FastPath gRPC listen address.")
	flag.IntVar(&fastPathHTTPPort, "fastpath-http-port", 0, "FastPath gRPC-Web port for HTTP/1.1 clients (0 disables this listener).")
	flag.BoolVar(&fastPathAuthEnabled, "fastpath-auth-enabled", false, "Enable FastPath caller authentication and TLS (default false; set true to enforce caller authentication over TLS).")
	flag.StringVar(&fastPathPolicy, "fastpath-auth-policy-file", "", "Required FastPath caller policy JSON file when --fastpath-auth-enabled is true.")
	flag.StringVar(&fastPathCert, "fastpath-tls-cert-file", "", "Required FastPath serving certificate PEM file when --fastpath-auth-enabled is true.")
	flag.StringVar(&fastPathKey, "fastpath-tls-key-file", "", "Required FastPath serving private key PEM file when --fastpath-auth-enabled is true.")
	flag.IntVar(&fastletPort, "fastlet-port", 5758, "Fastlet control port.")
	flag.DurationVar(&heartbeatInterval, "fastlet-heartbeat-interval", 20*time.Second, "Base Fastlet heartbeat interval; actual probes use jitter.")
	flag.DurationVar(&heartbeatTimeout, "fastlet-heartbeat-timeout", 5*time.Second, "Timeout for one Fastlet heartbeat request.")
	flag.IntVar(&heartbeatConcurrency, "fastlet-heartbeat-concurrency", 8, "Maximum concurrent Fastlet heartbeat requests.")
	flag.DurationVar(&fastletDrainTimeout, "fastlet-drain-timeout", 5*time.Minute, "Maximum time to wait for a draining Fastlet Pod to become empty before applying Sandbox failure policies.")
	flag.StringVar(&fastletProxyImage, "fastlet-proxy-image", envOrDefault("FASTLET_PROXY_IMAGE", "fast-sandbox/fastlet-proxy:dev"), "Image injected as the platform-owned Fastlet Proxy sidecar.")
	flag.StringVar(&fastletProxyRequestCPU, "fastlet-proxy-request-cpu", defaultFastletProxyResources.Requests.Cpu().String(), "CPU request for the injected Fastlet Proxy sidecar.")
	flag.StringVar(&fastletProxyRequestMemory, "fastlet-proxy-request-memory", defaultFastletProxyResources.Requests.Memory().String(), "Memory request for the injected Fastlet Proxy sidecar.")
	flag.StringVar(&fastletProxyLimitCPU, "fastlet-proxy-limit-cpu", defaultFastletProxyResources.Limits.Cpu().String(), "CPU limit for the injected Fastlet Proxy sidecar.")
	flag.StringVar(&fastletProxyLimitMemory, "fastlet-proxy-limit-memory", defaultFastletProxyResources.Limits.Memory().String(), "Memory limit for the injected Fastlet Proxy sidecar.")
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
	flag.StringVar(&fastletAuthSubjects, fastletauth.SubjectsFlag, envOrDefault(fastletauth.SubjectsEnv, ""), "Comma-separated ServiceAccount subjects allowed to call the Fastlet control API; empty allows the platform ServiceAccounts (fast-sandbox-fastpath, fast-sandbox-controller) in this control plane's own namespace. Set it when the deployment uses other identities. Custom namespaces must also replicate the namespaced Role/RoleBinding (serviceaccounts/token create, resourceNames-fenced) that config/rbac/base.yaml ships in fast-sandbox-system, or every Fastlet call fails closed with Forbidden.")
	flag.Float64Var(&kubeClientQPS, "kube-client-qps", 0, "API-server client rate limit in queries per second; 0 keeps the client-go default.")
	flag.IntVar(&kubeClientBurst, "kube-client-burst", 0, "API-server client burst budget for the rate limiter; 0 keeps the client-go default.")
	componentlog.InitFlags(nil)
	flag.Parse()

	role, err := controlplane.ParseRole(roleValue)
	if err != nil {
		klog.ErrorS(err, "Invalid control-plane role")
		os.Exit(1)
	}
	fastletProxyResources := mustParseFastletProxyResources(
		role.RunsControllers(), defaultFastletProxyResources,
		fastletProxyRequestCPU, fastletProxyRequestMemory,
		fastletProxyLimitCPU, fastletProxyLimitMemory,
	)
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
	fastletAuthAudience := envOrDefault(fastletauth.AudienceEnv, fastletauth.Audience)
	fastletAuthIssuer, fastletAuthCABundle, fastletAuthIdentity, err := installFastletAuthTokenSource(restConfig, fastletClient, fastletAuthAudience)
	if err != nil {
		klog.ErrorS(err, "Configure Fastlet control authentication: it is always enabled and cannot start without it")
		os.Exit(1)
	}
	// The allow list travels to Fastlet Pods through the Pool Controller: a
	// flag set with no usable subject is a configuration error, not a signal
	// to fall back to the built-in default silently.
	fastletAuthSubjectList := fastletauth.ParseSubjects(fastletAuthSubjects)
	if fastletAuthSubjects != "" && len(fastletAuthSubjectList) == 0 {
		klog.ErrorS(nil, "--fastlet-auth-subjects is set but contains no usable subject")
		os.Exit(1)
	}
	// Effective list: the flag override, else the platform ServiceAccounts in
	// this control plane's own namespace — the deployment convention keeps the
	// platform identities callable without a per-namespace flag, and the
	// validation below still cross-checks the result against the identity
	// tokens this process actually mints.
	if len(fastletAuthSubjectList) == 0 {
		fastletAuthSubjectList = fastletauth.PlatformSubjectsForNamespace(fastletAuthIdentity.Namespace)
	}
	if err := validateFastletAuthSubjects(fastletAuthSubjectList, fastletAuthIdentity); err != nil {
		klog.ErrorS(err, "Invalid Fastlet control allow list")
		os.Exit(1)
	}
	if len(fastletAuthSubjects) > 0 {
		klog.InfoS("Fastlet control allow list override", "subjects", strings.Join(fastletAuthSubjectList, ","))
	}
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
			FastletProxyImage: fastletProxyImage, FastletProxyResources: fastletProxyResources, RouteVerifyPublicKey: routeVerifyPublicKey,
			RuntimeEnvironmentNamespace: runtimeEnvironmentNamespace, RuntimeEnvironmentConfigMap: runtimeEnvironmentConfigMap,
			FastletStartupGrace:                 resourceAwareStartupGrace,
			ResourceScaleDownStabilization:      resourceAwareScaleDownStabilization,
			DisableResourceAwareStaticAdmission: !resourceAwareStaticAdmission,
			DisableResourceAwareMemoryAdmission: !resourceAwareMemoryAdmission,
			DisableResourceAwarePoolScaling:     !resourceAwarePoolScaling,
			FastletAuthIssuer:                   fastletAuthIssuer, FastletAuthCABundle: fastletAuthCABundle,
			FastletAuthSubjects: fastletAuthSubjectList,
			FastletAuthAudience: fastletAuthAudience,
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
		var grpcServer *grpc.Server
		var tlsConfig *tls.Config
		if fastPathAuthEnabled {
			tlsConfig, err = fastPathTLS(fastPathCert, fastPathKey)
			if err != nil {
				klog.ErrorS(err, "Configure FastPath TLS")
				os.Exit(1)
			}
			grants, err := fastpath.LoadCallerPolicy(fastPathPolicy)
			if err != nil {
				klog.ErrorS(err, "Load FastPath caller policy")
				os.Exit(1)
			}
			callerAuth, err := fastpath.NewCallerAuthenticator(fastletAuthIssuer, grants, nil)
			if err != nil {
				klog.ErrorS(err, "Initialize FastPath caller authentication")
				os.Exit(1)
			}
			grpcServer = grpc.NewServer(
				grpc.Creds(credentials.NewTLS(tlsConfig)),
				grpc.ChainUnaryInterceptor(callerAuth.UnaryInterceptor, observability.UnaryServerInterceptor("fastpath")),
				grpc.ChainStreamInterceptor(callerAuth.StreamInterceptor),
			)
		} else {
			klog.InfoS("FastPath caller authentication disabled; serving legacy plaintext gRPC")
			grpcServer = grpc.NewServer(
				grpc.ChainUnaryInterceptor(observability.UnaryServerInterceptor("fastpath")),
			)
		}
		fastpathv2.RegisterFastPathServiceServer(grpcServer, &fastpath.Server{
			K8sClient: durableClient, RouteCache: manager.GetClient(), Orchestrator: orchestrator,
			DiagnosticsClient: fastletClient,
			CredentialIssuer:  credentialIssuer, SandboxProxyBaseURL: sandboxProxyBaseURL,
		})
		if httpServer := newFastPathHTTPServer(fastPathHTTPPort, grpcServer); httpServer != nil {
			if tlsConfig != nil {
				httpTLS := tlsConfig.Clone()
				httpTLS.NextProtos = []string{"http/1.1"}
				httpListener, err := tls.Listen("tcp", httpServer.Server.Addr, httpTLS)
				if err != nil {
					klog.ErrorS(err, "Listen for FastPath HTTPS")
					os.Exit(1)
				}
				httpServer.Listener = httpListener
			}
			if err := manager.Add(httpServer); err != nil {
				klog.ErrorS(err, "Configure FastPath HTTP listener", "port", fastPathHTTPPort)
				os.Exit(1)
			}
		}
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

func mustParseFastletProxyResources(
	runsControllers bool,
	defaults corev1.ResourceRequirements,
	requestCPU, requestMemory, limitCPU, limitMemory string,
) corev1.ResourceRequirements {
	if !runsControllers {
		return defaults
	}
	resources, err := parseFastletProxyResources(requestCPU, requestMemory, limitCPU, limitMemory)
	if err != nil {
		klog.ErrorS(err, "Invalid Fastlet Proxy resources")
		os.Exit(1)
	}
	return resources
}

func parseFastletProxyResources(requestCPU, requestMemory, limitCPU, limitMemory string) (corev1.ResourceRequirements, error) {
	parse := func(flagName, value string) (resource.Quantity, error) {
		quantity, err := resource.ParseQuantity(value)
		if err != nil {
			return resource.Quantity{}, fmt.Errorf("--%s must be a Kubernetes resource quantity: %w", flagName, err)
		}
		if quantity.Sign() <= 0 {
			return resource.Quantity{}, fmt.Errorf("--%s must be greater than zero", flagName)
		}
		return quantity, nil
	}

	cpuRequest, err := parse("fastlet-proxy-request-cpu", requestCPU)
	if err != nil {
		return corev1.ResourceRequirements{}, err
	}
	memoryRequest, err := parse("fastlet-proxy-request-memory", requestMemory)
	if err != nil {
		return corev1.ResourceRequirements{}, err
	}
	cpuLimit, err := parse("fastlet-proxy-limit-cpu", limitCPU)
	if err != nil {
		return corev1.ResourceRequirements{}, err
	}
	memoryLimit, err := parse("fastlet-proxy-limit-memory", limitMemory)
	if err != nil {
		return corev1.ResourceRequirements{}, err
	}
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: cpuRequest, corev1.ResourceMemory: memoryRequest},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: cpuLimit, corev1.ResourceMemory: memoryLimit},
	}
	if err := reconciler.ValidateFastletProxyResources(resources); err != nil {
		return corev1.ResourceRequirements{}, err
	}
	return resources, nil
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// installFastletAuthTokenSource wires the Fastlet control authentication onto
// the shared Fastlet client and resolves the issuer anchor for the Pool
// Controller to inject into Fastlet Pods. Tokens are minted via the
// TokenRequest API for this Pod's own ServiceAccount; a cluster without the
// serviceaccounts/token grant fails closed at the first Fastlet call. It
// returns the resolved identity so startup can cross-check the allow list.
func installFastletAuthTokenSource(restConfig *rest.Config, fastletClient *fastletapi.FastletClient, audience string) (issuer, caBundle string, identity fastletauth.PodServiceAccountIdentity, err error) {
	identity, err = fastletauth.ResolvePodServiceAccountIdentity(
		os.Getenv(fastletauth.NamespaceEnv), os.Getenv(fastletauth.ServiceAccountEnv))
	if err != nil {
		return "", "", fastletauth.PodServiceAccountIdentity{}, fmt.Errorf("resolve Fastlet auth identity: %w", err)
	}
	authenticationClient, err := corev1client.NewForConfig(restConfig)
	if err != nil {
		return "", "", fastletauth.PodServiceAccountIdentity{}, fmt.Errorf("create core client for Fastlet auth TokenRequest: %w", err)
	}
	tokenSource := fastletauth.NewTokenRequestSource(func(ctx context.Context, namespace, name string, request *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error) {
		return authenticationClient.ServiceAccounts(namespace).CreateToken(ctx, name, request, metav1.CreateOptions{})
	}, identity, audience)
	fastletClient.SetTokenSource(tokenSource)
	issuer, caBundle, err = fastletauth.ResolveIssuerAnchor(os.Getenv(fastletauth.IssuerEnv), os.Getenv(fastletauth.CABundleEnv))
	if err != nil {
		// Auth is always on and the anchor cannot be re-resolved while
		// running: refuse to start rather than create Fastlets that
		// crash-loop.
		return "", "", fastletauth.PodServiceAccountIdentity{}, fmt.Errorf("resolve Fastlet auth issuer anchor: %w", err)
	}
	klog.InfoS("Fastlet control authentication enabled",
		"namespace", identity.Namespace, "serviceAccount", identity.ServiceAccount, "audience", audience, "issuer", issuer)
	return issuer, caBundle, identity, nil
}

// validateFastletAuthSubjects rejects configurations that deterministically
// fail every later Fastlet call, surfacing them at startup instead of as
// Forbidden on every control operation: a subject a ServiceAccount token can
// never present (token subjects are always
// system:serviceaccount:<namespace>:<name>), and an allow list that excludes
// this process's own minting identity.
func validateFastletAuthSubjects(subjects []string, identity fastletauth.PodServiceAccountIdentity) error {
	const subjectPrefix = "system:serviceaccount:"
	for _, subject := range subjects {
		rest, found := strings.CutPrefix(subject, subjectPrefix)
		if !found {
			return fmt.Errorf("subject %q is not a %s<namespace>:<name> identity: no ServiceAccount token can present it", subject, subjectPrefix)
		}
		namespace, name, found := strings.Cut(rest, ":")
		if !found || namespace == "" || name == "" {
			return fmt.Errorf("subject %q is not a parseable service account identity", subject)
		}
	}
	own := subjectPrefix + identity.Namespace + ":" + identity.ServiceAccount
	for _, subject := range subjects {
		if subject == own {
			return nil
		}
	}
	return fmt.Errorf("allow list does not contain this process's own identity %s: every Fastlet call would fail with Forbidden", own)
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
