package janitor

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var janitorCleanupTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "fast_sandbox_janitor_cleanup_total",
	Help: "NodeJanitor cleanup outcomes after authority revalidation.",
}, []string{"backend", "result", "reason"})

func recordJanitorCleanup(backend ResourceBackend, result, reason string) {
	if reason == "" {
		reason = "Unknown"
	}
	janitorCleanupTotal.WithLabelValues(string(backend), result, reason).Inc()
}
