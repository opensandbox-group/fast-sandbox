package infra

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var infraReadyLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "fast_sandbox_infra_ready_latency_seconds",
	Help:    "Per-service Infra initialization and readiness latency.",
	Buckets: prometheus.ExponentialBuckets(0.005, 2, 13),
}, []string{"profile", "component", "runtime", "result"})

func (m *Manager) observeInfraReady(component string, started time.Time, err error) {
	result := "success"
	if err != nil {
		result = "error"
	}
	profile := m.plan.ProfileName
	if profile == "" {
		profile = "minimal"
	}
	runtimeName := string(m.config.RuntimeProfile.Name)
	if runtimeName == "" {
		runtimeName = "unknown"
	}
	infraReadyLatency.WithLabelValues(profile, component, runtimeName, result).Observe(time.Since(started).Seconds())
}
