package main

import (
	"context"
	"errors"
	"flag"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"sync/atomic"
	"time"

	fastpathv1 "fast-sandbox/api/proto/v1"
	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	"fast-sandbox/internal/controller"
	"fast-sandbox/internal/controller/controlplane"
	"fast-sandbox/internal/controller/fastletcontrol"
	"fast-sandbox/internal/controller/fastletpool"
	"fast-sandbox/internal/controller/fastpath"
	"fast-sandbox/internal/controller/sandboxorchestrator"
	"fast-sandbox/internal/infracatalog"
	"fast-sandbox/internal/routeauth"
	"fast-sandbox/internal/runtimecatalog"

	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apiv1alpha1.AddToScheme(scheme))
}

func main() {
	var roleValue string
	var metricsAddress string
	var probeAddress string
	var fastPathAddress string
	var fastletPort int
	var heartbeatInterval time.Duration
	var heartbeatTimeout time.Duration
	var heartbeatConcurrency int
	var fastletDrainTimeout time.Duration
	var deprecatedConsistency string
	var deprecatedOrphanTimeout time.Duration
	var fastletProxyImage string
	var routeVerifyPublicKey string
	var routeSigningPrivateKey string
	var routeCredentialTTL time.Duration
	var sandboxProxyBaseURL string

	flag.StringVar(&roleValue, "role", "all", "Control-plane role: fastpath, controller, or all.")
	flag.StringVar(&metricsAddress, "metrics-bind-address", ":9091", "Metrics listen address.")
	flag.StringVar(&probeAddress, "health-probe-bind-address", ":5758", "Health probe listen address.")
	flag.StringVar(&fastPathAddress, "fastpath-bind-address", ":9090", "FastPath gRPC listen address.")
	flag.IntVar(&fastletPort, "fastlet-port", 5758, "Fastlet control port.")
	flag.DurationVar(&heartbeatInterval, "fastlet-heartbeat-interval", 20*time.Second, "Base Fastlet heartbeat interval; actual probes use jitter.")
	flag.DurationVar(&heartbeatTimeout, "fastlet-heartbeat-timeout", 5*time.Second, "Timeout for one Fastlet heartbeat request.")
	flag.IntVar(&heartbeatConcurrency, "fastlet-heartbeat-concurrency", 8, "Maximum concurrent Fastlet heartbeat requests.")
	flag.DurationVar(&fastletDrainTimeout, "fastlet-drain-timeout", 5*time.Minute, "Maximum time to wait for a draining Fastlet Pod to become empty before applying Sandbox failure policies.")
	flag.StringVar(&deprecatedConsistency, "fastpath-consistency-mode", "", "Deprecated and ignored; Create always uses reservation -> CRD CAS -> Ensure.")
	flag.DurationVar(&deprecatedOrphanTimeout, "fastpath-orphan-timeout", 0, "Deprecated and ignored; CRD reconciliation owns recovery.")
	flag.StringVar(&fastletProxyImage, "fastlet-proxy-image", envOrDefault("FASTLET_PROXY_IMAGE", "fast-sandbox/fastlet-proxy:dev"), "Image injected as the platform-owned Fastlet Proxy sidecar.")
	flag.StringVar(&routeVerifyPublicKey, "route-verify-public-key", os.Getenv("FAST_SANDBOX_ROUTE_VERIFY_PUBLIC_KEY"), "Comma-separated base64 Ed25519 public keys injected into data-plane proxies.")
	flag.StringVar(&routeSigningPrivateKey, "route-signing-private-key", os.Getenv("FAST_SANDBOX_ROUTE_SIGNING_PRIVATE_KEY"), "Base64 Ed25519 seed/private key used only by FastPath.")
	flag.DurationVar(&routeCredentialTTL, "route-credential-ttl", 5*time.Minute, "Lifetime of a Sandbox route credential.")
	flag.StringVar(&sandboxProxyBaseURL, "sandbox-proxy-base-url", envOrDefault("FAST_SANDBOX_PROXY_BASE_URL", "http://fast-sandbox-proxy.default.svc:8080"), "Client-visible Sandbox Proxy base URL.")
	flag.Parse()

	role, err := controlplane.ParseRole(roleValue)
	if err != nil {
		klog.ErrorS(err, "Invalid control-plane role")
		os.Exit(1)
	}
	if heartbeatInterval <= 0 || heartbeatTimeout <= 0 || heartbeatConcurrency <= 0 || fastletDrainTimeout <= 0 {
		klog.ErrorS(nil, "Heartbeat and drain timing values must be positive")
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
	if deprecatedConsistency != "" || deprecatedOrphanTimeout != 0 {
		klog.InfoS("Deprecated Fast/Strong flags are ignored", "consistency", deprecatedConsistency, "orphanTimeout", deprecatedOrphanTimeout)
	}
	ctrl.SetLogger(klog.NewKlogr())

	manager, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       metricsserver.Options{BindAddress: metricsAddress},
		HealthProbeBindAddress:        probeAddress,
		LeaderElection:                role.LeaderElection(),
		LeaderElectionID:              "fast-sandbox-controller.sandbox.fast.io",
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		klog.ErrorS(err, "Create controller-runtime manager")
		os.Exit(1)
	}
	if err := manager.GetFieldIndexer().IndexField(context.Background(), &apiv1alpha1.Sandbox{}, fastpath.SandboxUIDIndexField, func(object client.Object) []string {
		if object.GetUID() == "" {
			return nil
		}
		return []string{string(object.GetUID())}
	}); err != nil {
		klog.ErrorS(err, "Register Sandbox UID route index")
		os.Exit(1)
	}
	durableClient, err := client.New(manager.GetConfig(), client.Options{Scheme: manager.GetScheme()})
	if err != nil {
		klog.ErrorS(err, "Create direct API-server client")
		os.Exit(1)
	}

	registry := fastletpool.NewInMemoryRegistry()
	catalog := runtimecatalog.Builtin()
	infraCatalog := infracatalog.Builtin()
	fastletClient := api.NewFastletClient(fastletPort)
	orchestrator := &sandboxorchestrator.Orchestrator{
		Client: durableClient, Registry: registry, FastletClient: fastletClient, Catalog: catalog, InfraCatalog: infraCatalog,
	}

	if role.RunsControllers() {
		if err := (&controller.SandboxReconciler{
			Client: manager.GetClient(), Scheme: manager.GetScheme(), Orchestrator: orchestrator,
		}).SetupWithManager(manager); err != nil {
			klog.ErrorS(err, "Register Sandbox controller")
			os.Exit(1)
		}
		if err := (&controller.SandboxPoolReconciler{
			Client: manager.GetClient(), DurableReader: durableClient, Scheme: manager.GetScheme(), Registry: registry, Catalog: catalog, InfraCatalog: infraCatalog,
			FastletDrainer: fastletClient, DrainTimeout: fastletDrainTimeout,
			FastletProxyImage: fastletProxyImage, RouteVerifyPublicKey: routeVerifyPublicKey,
		}).SetupWithManager(manager); err != nil {
			klog.ErrorS(err, "Register SandboxPool controller")
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
		grpcServer := grpc.NewServer()
		fastpathv1.RegisterFastPathServiceServer(grpcServer, &fastpath.Server{
			K8sClient: durableClient, RouteCache: manager.GetClient(), Orchestrator: orchestrator,
			CredentialIssuer: credentialIssuer, SandboxProxyBaseURL: sandboxProxyBaseURL,
		})
		go func() {
			<-runContext.Done()
			grpcServer.GracefulStop()
		}()
		go func() {
			klog.InfoS("FastPath serving", "address", fastPathAddress, "leaderElection", false)
			if err := grpcServer.Serve(listener); err != nil {
				klog.ErrorS(err, "FastPath gRPC server exited")
			}
		}()
	}

	go func() {
		klog.ErrorS(http.ListenAndServe("localhost:6060", nil), "pprof server exited")
	}()
	klog.InfoS("Starting control plane", "role", role, "leaderElection", role.LeaderElection())
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
