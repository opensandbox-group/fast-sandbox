package runtime

import (
	"errors"
	"time"

	"fast-sandbox/internal/api"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	fastletAdmissionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fast_sandbox_fastlet_admission_total",
		Help: "Fastlet admission operations partitioned by bounded result and reason enums.",
	}, []string{"operation", "result", "reason"})
	fastletReservationInflight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "fast_sandbox_fastlet_reservation_inflight",
		Help: "Current number of uncommitted Fastlet reservations.",
	})
	fastletAdmissionSlots = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "fast_sandbox_fastlet_admission_slots",
		Help: "Current Fastlet slot accounting by state.",
	}, []string{"state"})
	runtimeCreateLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fast_sandbox_runtime_create_latency_seconds",
		Help:    "Runtime Ensure latency from the Fastlet process boundary.",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 13),
	}, []string{"runtime", "cache_hit", "result"})
	dataPlaneReadyLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fast_sandbox_data_plane_ready_latency_seconds",
		Help:    "Fastlet Ensure latency through runtime, Infra readiness, and route publication.",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 14),
	}, []string{"runtime", "infra_profile", "result"})
)

func recordAdmission(operation string, err error) {
	result, reason := "accepted", "none"
	if err != nil {
		result, reason = "error", "unknown"
		var failure *api.FastletError
		if errors.As(err, &failure) {
			result, reason = "rejected", string(failure.Code)
		}
	}
	fastletAdmissionTotal.WithLabelValues(operation, result, reason).Inc()
}

func recordAdmissionStatus(status api.AdmissionStatus) {
	fastletReservationInflight.Set(float64(status.Reservations))
	fastletAdmissionSlots.WithLabelValues("capacity").Set(float64(status.Capacity))
	fastletAdmissionSlots.WithLabelValues("used").Set(float64(status.Used))
	fastletAdmissionSlots.WithLabelValues("creating").Set(float64(status.Creating))
	fastletAdmissionSlots.WithLabelValues("running").Set(float64(status.Running))
	fastletAdmissionSlots.WithLabelValues("deleting").Set(float64(status.Deleting))
}

func metricResult(err error) string {
	if err == nil {
		return "success"
	}
	return "error"
}

func observeRuntimeCreate(runtimeName string, started time.Time, err error) {
	if runtimeName == "" {
		runtimeName = "unknown"
	}
	// RuntimeDriver does not yet expose a trustworthy per-create cache-hit bit.
	// Keep the bounded label explicit instead of inferring from a stale inventory.
	runtimeCreateLatency.WithLabelValues(runtimeName, "unknown", metricResult(err)).Observe(time.Since(started).Seconds())
}

func observeDataPlaneReady(runtimeName, infraProfile string, started time.Time, err error) {
	if runtimeName == "" {
		runtimeName = "unknown"
	}
	if infraProfile == "" {
		infraProfile = "minimal"
	}
	dataPlaneReadyLatency.WithLabelValues(runtimeName, infraProfile, metricResult(err)).Observe(time.Since(started).Seconds())
}
