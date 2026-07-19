package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/routeauth"
	"fast-sandbox/internal/sandboxproxy"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

func main() {
	var address string
	var metricsAddress string
	var publicKeyValue string
	var fastletPort int
	flag.StringVar(&address, "bind-address", sandboxproxy.DefaultAddress, "Sandbox Proxy listen address.")
	flag.StringVar(&metricsAddress, "metrics-bind-address", ":9094", "Prometheus metrics listen address; empty disables the server.")
	flag.StringVar(&publicKeyValue, "route-verify-public-key", os.Getenv("FAST_SANDBOX_ROUTE_VERIFY_PUBLIC_KEY"), "Comma-separated base64 Ed25519 route credential public keys.")
	flag.IntVar(&fastletPort, "fastlet-proxy-port", 5780, "Fastlet Proxy data port.")
	flag.Parse()

	publicKeys, err := routeauth.ParsePublicKeySet(publicKeyValue)
	if err != nil {
		klog.ErrorS(err, "route-verify-public-key is required; Sandbox Proxy fails closed")
		os.Exit(1)
	}
	verifier, err := routeauth.NewVerifierSet(publicKeys, time.Now)
	if err != nil {
		klog.ErrorS(err, "Configure route credential verifier")
		os.Exit(1)
	}
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(apiv1alpha1.AddToScheme(scheme))
	restConfig, err := config.GetConfig()
	if err != nil {
		klog.ErrorS(err, "Load Kubernetes configuration")
		os.Exit(1)
	}
	watchCache, err := ctrlcache.New(restConfig, ctrlcache.Options{
		Scheme: scheme,
		ByObject: map[client.Object]ctrlcache.ByObject{
			&corev1.Pod{}: {Label: labels.SelectorFromSet(labels.Set{"app": "sandbox-fastlet"})},
		},
	})
	if err != nil {
		klog.ErrorS(err, "Create Sandbox Proxy watch cache")
		os.Exit(1)
	}
	directClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		klog.ErrorS(err, "Create Sandbox Proxy direct API client")
		os.Exit(1)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	index := sandboxproxy.NewIndex()
	if err := registerInformers(ctx, watchCache, index); err != nil {
		klog.ErrorS(err, "Register Sandbox Proxy informers")
		os.Exit(1)
	}
	go func() {
		if err := watchCache.Start(ctx); err != nil {
			klog.ErrorS(err, "Sandbox Proxy watch cache exited")
			cancel()
		}
	}()
	var cacheReady atomic.Bool
	go func() {
		if watchCache.WaitForCacheSync(ctx) {
			cacheReady.Store(true)
			klog.Info("Sandbox Proxy watch cache synchronized")
		}
	}()

	resolver := &sandboxproxy.KubernetesResolver{Index: index, Client: directClient}
	dataProxy := &sandboxproxy.Proxy{Resolver: resolver, Verifier: verifier, FastletPort: fastletPort}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(writer http.ResponseWriter, _ *http.Request) {
		if !cacheReady.Load() {
			http.Error(writer, "watch cache has not synchronized", http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
	})
	mux.Handle("/", dataProxy)
	server := &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 5 * time.Minute}
	var metricsServer *http.Server
	if metricsAddress != "" {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("GET /metrics", promhttp.Handler())
		metricsServer = &http.Server{Addr: metricsAddress, Handler: metricsMux, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			klog.InfoS("Sandbox Proxy metrics server listening", "address", metricsAddress)
			if serveErr := metricsServer.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
				klog.ErrorS(serveErr, "Sandbox Proxy metrics server exited")
				cancel()
			}
		}()
	}
	go func() {
		<-ctx.Done()
		shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		_ = server.Shutdown(shutdownContext)
		if metricsServer != nil {
			_ = metricsServer.Shutdown(shutdownContext)
		}
	}()
	klog.InfoS("Sandbox Proxy listening", "address", address)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		klog.ErrorS(err, "Sandbox Proxy server exited")
		os.Exit(1)
	}
}

func registerInformers(ctx context.Context, watchCache ctrlcache.Cache, index *sandboxproxy.Index) error {
	sandboxInformer, err := watchCache.GetInformer(ctx, &apiv1alpha1.Sandbox{})
	if err != nil {
		return err
	}
	_, err = sandboxInformer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(object any) {
			if sandbox, ok := object.(*apiv1alpha1.Sandbox); ok {
				index.UpsertSandbox(sandbox)
			}
		},
		UpdateFunc: func(_, current any) {
			if sandbox, ok := current.(*apiv1alpha1.Sandbox); ok {
				index.UpsertSandbox(sandbox)
			}
		},
		DeleteFunc: func(object any) {
			if sandbox := deletedSandbox(object); sandbox != nil {
				index.DeleteSandbox(sandbox)
			}
		},
	})
	if err != nil {
		return err
	}
	podInformer, err := watchCache.GetInformer(ctx, &corev1.Pod{})
	if err != nil {
		return err
	}
	_, err = podInformer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc: func(object any) {
			if pod, ok := object.(*corev1.Pod); ok {
				index.UpsertPod(pod)
			}
		},
		UpdateFunc: func(_, current any) {
			if pod, ok := current.(*corev1.Pod); ok {
				index.UpsertPod(pod)
			}
		},
		DeleteFunc: func(object any) {
			if pod := deletedPod(object); pod != nil {
				index.DeletePod(pod)
			}
		},
	})
	return err
}

func deletedSandbox(object any) *apiv1alpha1.Sandbox {
	if sandbox, ok := object.(*apiv1alpha1.Sandbox); ok {
		return sandbox
	}
	if tombstone, ok := object.(toolscache.DeletedFinalStateUnknown); ok {
		sandbox, _ := tombstone.Obj.(*apiv1alpha1.Sandbox)
		return sandbox
	}
	return nil
}

func deletedPod(object any) *corev1.Pod {
	if pod, ok := object.(*corev1.Pod); ok {
		return pod
	}
	if tombstone, ok := object.(toolscache.DeletedFinalStateUnknown); ok {
		pod, _ := tombstone.Obj.(*corev1.Pod)
		return pod
	}
	return nil
}
