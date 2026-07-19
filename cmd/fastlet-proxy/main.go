package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"fast-sandbox/internal/fastletproxy"
	"fast-sandbox/internal/routeauth"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/klog/v2"
)

func main() {
	publicKeys, err := routeauth.ParsePublicKeySet(os.Getenv("FAST_SANDBOX_ROUTE_VERIFY_PUBLIC_KEY"))
	if err != nil {
		klog.ErrorS(err, "FAST_SANDBOX_ROUTE_VERIFY_PUBLIC_KEY is required; Fastlet Proxy fails closed")
		os.Exit(1)
	}
	verifier, err := routeauth.NewVerifierSet(publicKeys, time.Now)
	if err != nil {
		klog.ErrorS(err, "Configure route credential verifier")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	store := fastletproxy.NewStore()
	control := &fastletproxy.ControlServer{Store: store, SocketPath: envOrDefault("FASTLET_PROXY_CONTROL_SOCKET", fastletproxy.DefaultControlSocket)}
	errorsChannel := make(chan error, 3)
	go func() { errorsChannel <- control.Serve(ctx) }()

	dataMux := http.NewServeMux()
	dataMux.HandleFunc("GET /readyz", func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusOK) })
	dataMux.Handle("/", &fastletproxy.Proxy{Store: store, Verifier: verifier})
	dataServer := &http.Server{
		Addr: envOrDefault("FASTLET_PROXY_DATA_ADDRESS", fastletproxy.DefaultDataAddress), Handler: dataMux,
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 5 * time.Minute,
	}
	go func() {
		klog.InfoS("Fastlet Proxy data server listening", "address", dataServer.Addr)
		errorsChannel <- dataServer.ListenAndServe()
	}()
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", promhttp.Handler())
	metricsServer := &http.Server{
		Addr: envOrDefault("FASTLET_PROXY_METRICS_ADDRESS", ":9093"), Handler: metricsMux, ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		klog.InfoS("Fastlet Proxy metrics server listening", "address", metricsServer.Addr)
		errorsChannel <- metricsServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
	case serveErr := <-errorsChannel:
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			klog.ErrorS(serveErr, "Fastlet Proxy server exited")
			cancel()
		}
	}
	shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = dataServer.Shutdown(shutdownContext)
	_ = metricsServer.Shutdown(shutdownContext)
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
