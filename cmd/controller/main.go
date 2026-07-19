package main

import (
	"context"
	"flag"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"time"

	"fast-sandbox/internal/api"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	fastpathv1 "fast-sandbox/api/proto/v1"
	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/controller"
	"fast-sandbox/internal/controller/fastletcontrol"
	"fast-sandbox/internal/controller/fastletpool"
	"fast-sandbox/internal/controller/fastpath"
	"fast-sandbox/internal/runtimecatalog"

	"google.golang.org/grpc"
)

var (
	scheme = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apiv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var fastletPort int
	var fastpathConsistencyMode string
	var fastpathOrphanTimeout time.Duration
	flag.IntVar(&fastletPort, "fastlet-port", 5758, "The port the fastlet server binds to.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":9091", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":5758", "The address the probe endpoint binds to.")
	flag.StringVar(&fastpathConsistencyMode, "fastpath-consistency-mode", "fast", "Fast-Path consistency mode: fast (default) or strong")
	flag.DurationVar(&fastpathOrphanTimeout, "fastpath-orphan-timeout", 10*time.Second, "Fast-Path orphan cleanup timeout (for Fast mode)")

	flag.Parse()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		klog.ErrorS(err, "unable to start manager")
		os.Exit(1)
	}

	reg := fastletpool.NewInMemoryRegistry()
	runtimeCatalog := runtimecatalog.Builtin()
	fastletHTTPClient := api.NewFastletClient(fastletPort)
	if err = (&controller.SandboxReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Registry:      reg,
		FastletClient: fastletHTTPClient,
		Catalog:       runtimeCatalog,
	}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create controller", "controller", "Sandbox")
		os.Exit(1)
	}

	if err = (&controller.SandboxPoolReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		Registry: reg,
		Catalog:  runtimeCatalog,
	}).SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "unable to create controller", "controller", "SandboxPool")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	loop := fastletcontrol.NewLoop(mgr.GetClient(), reg, fastletHTTPClient)
	go loop.Start(ctx)

	lis, err := net.Listen("tcp", ":9090")
	if err != nil {
		klog.ErrorS(err, "failed to listen on port 9090 for fast-path")
		os.Exit(1)
	}
	grpcServer := grpc.NewServer()

	consistencyMode := api.ConsistencyModeFast
	if fastpathConsistencyMode == "strong" {
		consistencyMode = api.ConsistencyModeStrong
	}

	fastpathv1.RegisterFastPathServiceServer(grpcServer, &fastpath.Server{
		K8sClient:              mgr.GetClient(),
		Registry:               reg,
		FastletClient:          fastletHTTPClient,
		DefaultConsistencyMode: consistencyMode,
		Catalog:                runtimeCatalog,
	})
	klog.InfoS("Starting Fast-Path gRPC server V2", "port", 9090, "consistency-mode", consistencyMode, "orphan-timeout", fastpathOrphanTimeout)
	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			klog.ErrorS(err, "failed to serve gRPC")
		}
	}()

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		klog.ErrorS(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		klog.ErrorS(err, "unable to set up ready check")
		os.Exit(1)
	}

	klog.InfoS("starting manager")

	// Set up controller-runtime logging to use klog
	ctrl.SetLogger(klog.NewKlogr())

	go func() {
		klog.InfoS("Starting pprof server", "address", ":6060")
		klog.ErrorS(http.ListenAndServe("localhost:6060", nil), "pprof server exited")
	}()

	go func() {
		if mgr.GetCache().WaitForCacheSync(context.Background()) {
			klog.InfoS("Cache synced, restoring registry state from cluster")
			if err := reg.Restore(context.Background(), mgr.GetClient()); err != nil {
				klog.ErrorS(err, "failed to restore registry state")
			} else {
				klog.InfoS("Registry state restored successfully")
			}
		}
	}()

	if err := mgr.Start(ctx); err != nil {
		klog.ErrorS(err, "problem running manager")
		os.Exit(1)
	}
}
