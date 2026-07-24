package fastpath

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc/status"
)

var (
	createSandboxDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "fastpath_create_sandbox_duration_seconds",
			Help:    "Duration of CreateSandbox RPC calls",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1},
		},
		[]string{"mode", "success"},
	)
	createAcceptedLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fast_sandbox_create_accepted_latency_seconds",
		Help:    "FastPath latency until an idempotent existing request or a Fastlet reservation is accepted.",
		Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1},
	}, []string{"path", "result"})
	createRuntimeReadyLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fast_sandbox_create_runtime_ready_latency_seconds",
		Help:    "End-to-end CreateSandbox latency until the runtime is ready or the RPC terminates.",
		Buckets: prometheus.ExponentialBuckets(.005, 2, 14),
	}, []string{"result"})
)

func grpcMetricResult(err error) string {
	if err == nil {
		return "OK"
	}
	return status.Code(err).String()
}

func observeCreateAccepted(path string, started time.Time, err error) {
	createAcceptedLatency.WithLabelValues(path, grpcMetricResult(err)).Observe(time.Since(started).Seconds())
}
