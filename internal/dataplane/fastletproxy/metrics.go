package fastletproxy

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var fastletProxyUpstreamLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "fast_sandbox_fastlet_proxy_upstream_latency_seconds",
	Help:    "Fastlet Proxy request/stream lifetime by access transport and bounded result.",
	Buckets: prometheus.ExponentialBuckets(.001, 2, 16),
}, []string{"access", "result"})

func observeFastletProxy(access, result string, started time.Time) {
	if access == "" {
		access = "unresolved"
	}
	fastletProxyUpstreamLatency.WithLabelValues(access, result).Observe(time.Since(started).Seconds())
}
