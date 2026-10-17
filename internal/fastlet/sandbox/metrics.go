package sandbox

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"fast-sandbox/internal/observability"
	fastletapi "fast-sandbox/internal/protocol/fastlet"
)

const (
	// metricLabelRuntime partitions latency and observation metrics by runtime name.
	metricLabelRuntime = "runtime"
	// metricLabelInfraRevision partitions data-plane metrics by Infra revision.
	metricLabelInfraRevision = "infra_revision"
	// metricLabelResult bounds the result label across metrics.
	metricLabelResult = "result"
	// metricResultError is the failure value of bounded result labels.
	metricResultError = "error"
	// metricValueUnknown is the fallback value for unknown runtime names,
	// infra revisions, and admission reasons.
	metricValueUnknown = "unknown"
)

var (
	fastletAdmissionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fast_sandbox_fastlet_admission_total",
		Help: "Fastlet admission operations partitioned by bounded result and reason enums.",
	}, []string{"operation", metricLabelResult, "reason"})
	fastletAdmissionSlots = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "fast_sandbox_fastlet_admission_slots",
		Help: "Current Fastlet slot accounting by state.",
	}, []string{"state"})
	fastletResourceAdmissionState = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "fast_sandbox_fastlet_resource_admission_state",
		Help: "Current resource-aware Fastlet admission state and bounded dominant reason.",
	}, []string{"state", "reason"})
	runtimeCreateLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fast_sandbox_runtime_create_latency_seconds",
		Help:    "Runtime Ensure latency from the Fastlet process boundary.",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 13),
	}, []string{metricLabelRuntime, "cache_hit", metricLabelResult})
	dataPlaneReadyLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fast_sandbox_data_plane_ready_latency_seconds",
		Help:    "Latency from runtime creation start until asynchronous Infra readiness and route publication complete.",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 14),
	}, []string{metricLabelRuntime, metricLabelInfraRevision, metricLabelResult})
	userProcessStartLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fast_sandbox_user_process_start_latency_seconds",
		Help:    "Ensure latency until a runtime adapter can prove the user process started.",
		Buckets: prometheus.ExponentialBuckets(0.005, 2, 14),
	}, []string{metricLabelRuntime, metricLabelInfraRevision, "source"})
	userProcessStartObservationTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fast_sandbox_user_process_start_observation_total",
		Help: "Availability of a trustworthy user-process-start observation.",
	}, []string{metricLabelRuntime, metricLabelInfraRevision, "source", metricLabelResult})
	warmImagePullTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fast_sandbox_warm_image_pull_total",
		Help: "Pool warm image preparation attempts by bounded result; image references are intentionally not labels.",
	}, []string{metricLabelResult})
	fastletCreateStageLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fast_sandbox_fastlet_create_stage_latency_seconds",
		Help:    "Latency of bounded synchronous Fastlet admission stages before and around RuntimeDriver Ensure.",
		Buckets: prometheus.ExponentialBuckets(.00025, 2, 15),
	}, []string{metricLabelRuntime, "stage", metricLabelResult})
)

func recordAdmission(operation string, err error) {
	result, reason := "accepted", "none"
	if err != nil {
		result, reason = metricResultError, metricValueUnknown
		var failure *fastletapi.FastletError
		if errors.As(err, &failure) {
			result, reason = "rejected", string(failure.Code)
			if failure.Reason != "" {
				reason = string(failure.Reason)
			}
		}
	}
	fastletAdmissionTotal.WithLabelValues(operation, result, reason).Inc()
}

func recordAdmissionStatus(status fastletapi.AdmissionStatus) {
	fastletAdmissionSlots.WithLabelValues("capacity").Set(float64(status.Capacity))
	fastletAdmissionSlots.WithLabelValues("used").Set(float64(status.Used))
	fastletAdmissionSlots.WithLabelValues("creating").Set(float64(status.Creating))
	fastletAdmissionSlots.WithLabelValues("running").Set(float64(status.Running))
	fastletAdmissionSlots.WithLabelValues("deleting").Set(float64(status.Deleting))
	fastletResourceAdmissionState.Reset()
	if status.Resources != nil {
		reason := string(status.Resources.Reason)
		if reason == "" {
			reason = "none"
		}
		fastletResourceAdmissionState.WithLabelValues(string(status.Resources.State), reason).Set(1)
	}
}

func metricResult(err error) string {
	if err == nil {
		return "success"
	}
	return metricResultError
}

func recordWarmImagePull(err error) {
	warmImagePullTotal.WithLabelValues(metricResult(err)).Inc()
}

func observeRuntimeCreate(runtimeName string, started time.Time, err error) {
	if runtimeName == "" {
		runtimeName = metricValueUnknown
	}
	// RuntimeDriver does not yet expose a trustworthy per-create cache-hit bit.
	// Keep the bounded label explicit instead of inferring from a stale inventory.
	runtimeCreateLatency.WithLabelValues(runtimeName, metricValueUnknown, metricResult(err)).Observe(time.Since(started).Seconds())
}

func startFastletCreateStage(ctx context.Context, runtimeName, stage string) (context.Context, func(error)) {
	if runtimeName == "" {
		runtimeName = metricValueUnknown
	}
	started := time.Now()
	stageContext, span := observability.Start(ctx, "fastlet.create."+stage)
	return stageContext, func(err error) {
		fastletCreateStageLatency.WithLabelValues(runtimeName, stage, metricResult(err)).Observe(time.Since(started).Seconds())
		observability.End(span, err)
	}
}

func observeDataPlaneReady(runtimeName, infraRevision string, started time.Time, err error) {
	if runtimeName == "" {
		runtimeName = metricValueUnknown
	}
	if infraRevision == "" {
		infraRevision = metricValueUnknown
	}
	dataPlaneReadyLatency.WithLabelValues(runtimeName, infraRevision, metricResult(err)).Observe(time.Since(started).Seconds())
}

func observeUserProcessStart(runtimeName, infraRevision string, ensureStarted time.Time, metadata *SandboxMetadata) {
	if runtimeName == "" {
		runtimeName = metricValueUnknown
	}
	if infraRevision == "" {
		infraRevision = metricValueUnknown
	}
	source := fastletapi.UserProcessStartUnknown
	if metadata != nil {
		switch metadata.UserProcessStartSource {
		case fastletapi.UserProcessStartRuntimeDirect, fastletapi.UserProcessStartSandboxInitUnreported, fastletapi.UserProcessStartExistingRuntime:
			source = metadata.UserProcessStartSource
		}
	}
	result := "unavailable"
	if source == fastletapi.UserProcessStartExistingRuntime {
		result = "not_applicable"
	} else if source == fastletapi.UserProcessStartRuntimeDirect && metadata != nil && !metadata.UserProcessStartedAt.IsZero() {
		result = "observed"
		latency := metadata.UserProcessStartedAt.Sub(ensureStarted)
		if latency < 0 {
			latency = 0
		}
		userProcessStartLatency.WithLabelValues(runtimeName, infraRevision, string(source)).Observe(latency.Seconds())
	}
	userProcessStartObservationTotal.WithLabelValues(runtimeName, infraRevision, string(source), result).Inc()
}
