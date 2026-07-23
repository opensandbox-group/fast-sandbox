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
		Help:    "Latency from runtime creation start until asynchronous Infra readiness and route publication complete.",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 14),
	}, []string{"runtime", "infra_profile", "result"})
	userProcessStartLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fast_sandbox_user_process_start_latency_seconds",
		Help:    "Ensure latency until a runtime adapter can prove the user process started.",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 14),
	}, []string{"runtime", "infra_profile", "source"})
	userProcessStartObservationTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fast_sandbox_user_process_start_observation_total",
		Help: "Availability of a trustworthy user-process-start observation.",
	}, []string{"runtime", "infra_profile", "source", "result"})
	warmImagePullTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fast_sandbox_warm_image_pull_total",
		Help: "Pool warm image preparation attempts by bounded result; image references are intentionally not labels.",
	}, []string{"result"})
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

func recordWarmImagePull(err error) {
	warmImagePullTotal.WithLabelValues(metricResult(err)).Inc()
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

func observeUserProcessStart(runtimeName, infraProfile string, ensureStarted time.Time, metadata *SandboxMetadata) {
	if runtimeName == "" {
		runtimeName = "unknown"
	}
	if infraProfile == "" {
		infraProfile = "minimal"
	}
	source := api.UserProcessStartUnknown
	if metadata != nil {
		switch metadata.UserProcessStartSource {
		case api.UserProcessStartRuntimeDirect, api.UserProcessStartSandboxInitUnreported, api.UserProcessStartExistingRuntime:
			source = metadata.UserProcessStartSource
		}
	}
	result := "unavailable"
	if source == api.UserProcessStartExistingRuntime {
		result = "not_applicable"
	} else if source == api.UserProcessStartRuntimeDirect && metadata != nil && !metadata.UserProcessStartedAt.IsZero() {
		result = "observed"
		latency := metadata.UserProcessStartedAt.Sub(ensureStarted)
		if latency < 0 {
			latency = 0
		}
		userProcessStartLatency.WithLabelValues(runtimeName, infraProfile, string(source)).Observe(latency.Seconds())
	}
	userProcessStartObservationTotal.WithLabelValues(runtimeName, infraProfile, string(source), result).Inc()
}
