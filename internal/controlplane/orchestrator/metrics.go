package orchestrator

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var topKRetryTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "fast_sandbox_topk_retry_total",
	Help: "Top-K atomic Create retries after a candidate rejects admission before side effects.",
}, []string{"result"})

func RecordTopKRetry(result string) {
	topKRetryTotal.WithLabelValues(result).Inc()
}
