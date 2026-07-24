package sandboxproxy

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var sandboxProxyRouteLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "fast_sandbox_sandbox_proxy_route_latency_seconds",
	Help:    "Sandbox Proxy request/stream lifetime by bounded routing result.",
	Buckets: prometheus.ExponentialBuckets(.001, 2, 16),
}, []string{"result"})

func observeSandboxProxy(result string, started time.Time) {
	sandboxProxyRouteLatency.WithLabelValues(result).Observe(time.Since(started).Seconds())
}
