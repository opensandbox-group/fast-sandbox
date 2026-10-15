package fastpath

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/grpc/status"
	controllermetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"fast-sandbox/internal/observability"
)

// metricsResultLabel is the label key carrying the RPC outcome on latency metrics.
const metricsResultLabel = "result"

var (
	controlPlaneMetricFactory = promauto.With(controllermetrics.Registry)
	createSandboxDuration     = controlPlaneMetricFactory.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "fastpath_create_sandbox_duration_seconds",
			Help:    "Duration of CreateSandbox RPC calls",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1},
		},
		[]string{"mode", "success"},
	)
	createAcceptedLatency = controlPlaneMetricFactory.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fast_sandbox_create_accepted_latency_seconds",
		Help:    "FastPath latency until an idempotent existing request or a Fastlet reservation is accepted.",
		Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1},
	}, []string{"path", metricsResultLabel})
	createRuntimeReadyLatency = controlPlaneMetricFactory.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fast_sandbox_create_runtime_ready_latency_seconds",
		Help:    "End-to-end CreateSandbox latency until the runtime is ready or the RPC terminates.",
		Buckets: prometheus.ExponentialBuckets(.005, 2, 14),
	}, []string{metricsResultLabel})
	createStageLatency = controlPlaneMetricFactory.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fast_sandbox_create_stage_latency_seconds",
		Help:    "Latency of bounded synchronous FastPath CreateSandbox stages.",
		Buckets: prometheus.ExponentialBuckets(.00025, 2, 15),
	}, []string{"stage", metricsResultLabel})
	handlerRelayDuration = controlPlaneMetricFactory.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fastpath_invoke_handler_action_duration_seconds",
		Help:    "Duration of InvokeHandlerAction RPC calls",
		Buckets: prometheus.ExponentialBuckets(.005, 2, 14),
	}, []string{metricsResultLabel})
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

func startCreateStage(ctx context.Context, stage string) (context.Context, func(error)) {
	started := time.Now()
	stageContext, span := observability.Start(ctx, "fastpath.create."+stage)
	return stageContext, func(err error) {
		result := "success"
		if err != nil {
			result = "error"
		}
		createStageLatency.WithLabelValues(stage, result).Observe(time.Since(started).Seconds())
		observability.End(span, err)
	}
}
