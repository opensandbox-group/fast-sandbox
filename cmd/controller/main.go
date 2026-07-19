package main

import (
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
	var deprecatedConsistency string
	var deprecatedOrphanTimeout time.Duration

	flag.StringVar(&roleValue, "role", "all", "Control-plane role: fastpath, controller, or all.")
	flag.StringVar(&metricsAddress, "metrics-bind-address", ":9091", "Metrics listen address.")
	flag.StringVar(&probeAddress, "health-probe-bind-address", ":5758", "Health probe listen address.")
	flag.StringVar(&fastPathAddress, "fastpath-bind-address", ":9090", "FastPath gRPC listen address.")
	flag.IntVar(&fastletPort, "fastlet-port", 5758, "Fastlet control port.")
	flag.DurationVar(&heartbeatInterval, "fastlet-heartbeat-interval", 20*time.Second, "Base Fastlet heartbeat interval; actual probes use jitter.")
	flag.DurationVar(&heartbeatTimeout, "fastlet-heartbeat-timeout", 5*time.Second, "Timeout for one Fastlet heartbeat request.")
	flag.IntVar(&heartbeatConcurrency, "fastlet-heartbeat-concurrency", 8, "Maximum concurrent Fastlet heartbeat requests.")
	flag.StringVar(&deprecatedConsistency, "fastpath-consistency-mode", "", "Deprecated and ignored; Create always uses reservation -> CRD CAS -> Ensure.")
	flag.DurationVar(&deprecatedOrphanTimeout, "fastpath-orphan-timeout", 0, "Deprecated and ignored; CRD reconciliation owns recovery.")
	flag.Parse()

	role, err := controlplane.ParseRole(roleValue)
	if err != nil {
		klog.ErrorS(err, "Invalid control-plane role")
		os.Exit(1)
	}
	if heartbeatInterval <= 0 || heartbeatTimeout <= 0 || heartbeatConcurrency <= 0 {
		klog.ErrorS(nil, "Heartbeat interval, timeout, and concurrency must be positive")
		os.Exit(1)
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
	durableClient, err := client.New(manager.GetConfig(), client.Options{Scheme: manager.GetScheme()})
	if err != nil {
		klog.ErrorS(err, "Create direct API-server client")
		os.Exit(1)
	}

	registry := fastletpool.NewInMemoryRegistry()
	catalog := runtimecatalog.Builtin()
	fastletClient := api.NewFastletClient(fastletPort)
	orchestrator := &sandboxorchestrator.Orchestrator{
		Client: durableClient, Registry: registry, FastletClient: fastletClient, Catalog: catalog,
	}

	if role.RunsControllers() {
		if err := (&controller.SandboxReconciler{
			Client: manager.GetClient(), Scheme: manager.GetScheme(), Orchestrator: orchestrator,
		}).SetupWithManager(manager); err != nil {
			klog.ErrorS(err, "Register Sandbox controller")
			os.Exit(1)
		}
		if err := (&controller.SandboxPoolReconciler{
			Client: manager.GetClient(), Scheme: manager.GetScheme(), Registry: registry, Catalog: catalog,
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
			K8sClient: durableClient, Orchestrator: orchestrator,
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
