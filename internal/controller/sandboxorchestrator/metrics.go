package sandboxorchestrator

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var topKRetryTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "fast_sandbox_topk_retry_total",
	Help: "Top-K reservation retries after a candidate rejects admission.",
}, []string{"result"})

func recordTopKRetry(result string) {
	topKRetryTotal.WithLabelValues(result).Inc()
}
