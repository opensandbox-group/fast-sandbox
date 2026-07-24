package sandbox

import (
	"errors"
	"testing"
	"time"

	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestAdmissionMetricsUseBoundedOutcomeLabels(t *testing.T) {
	accepted := fastletAdmissionTotal.WithLabelValues("test-reserve", "accepted", "none")
	rejected := fastletAdmissionTotal.WithLabelValues("test-reserve", "rejected", string(fastletapi.ErrorCapacityRejected))
	failed := fastletAdmissionTotal.WithLabelValues("test-reserve", "error", "unknown")
	acceptedBefore := gatheredMetricValue(t, "fast_sandbox_fastlet_admission_total", map[string]string{"operation": "test-reserve", "result": "accepted", "reason": "none"})
	rejectedBefore := gatheredMetricValue(t, "fast_sandbox_fastlet_admission_total", map[string]string{"operation": "test-reserve", "result": "rejected", "reason": string(fastletapi.ErrorCapacityRejected)})
	failedBefore := gatheredMetricValue(t, "fast_sandbox_fastlet_admission_total", map[string]string{"operation": "test-reserve", "result": "error", "reason": "unknown"})

	recordAdmission("test-reserve", nil)
	recordAdmission("test-reserve", &fastletapi.FastletError{Code: fastletapi.ErrorCapacityRejected, Message: "full"})
	recordAdmission("test-reserve", errors.New("transport failure"))

	_ = accepted
	_ = rejected
	_ = failed
	require.Equal(t, acceptedBefore+1, gatheredMetricValue(t, "fast_sandbox_fastlet_admission_total", map[string]string{"operation": "test-reserve", "result": "accepted", "reason": "none"}))
	require.Equal(t, rejectedBefore+1, gatheredMetricValue(t, "fast_sandbox_fastlet_admission_total", map[string]string{"operation": "test-reserve", "result": "rejected", "reason": string(fastletapi.ErrorCapacityRejected)}))
	require.Equal(t, failedBefore+1, gatheredMetricValue(t, "fast_sandbox_fastlet_admission_total", map[string]string{"operation": "test-reserve", "result": "error", "reason": "unknown"}))
}

func TestAdmissionStatusMetricsReflectLatestSnapshot(t *testing.T) {
	recordAdmissionStatus(fastletapi.AdmissionStatus{
		Capacity: 8, Used: 7, Creating: 1, Running: 3, Deleting: 1,
	})

	require.Equal(t, float64(8), gatheredMetricValue(t, "fast_sandbox_fastlet_admission_slots", map[string]string{"state": "capacity"}))
	require.Equal(t, float64(7), gatheredMetricValue(t, "fast_sandbox_fastlet_admission_slots", map[string]string{"state": "used"}))
	require.Equal(t, float64(1), gatheredMetricValue(t, "fast_sandbox_fastlet_admission_slots", map[string]string{"state": "creating"}))
	require.Equal(t, float64(3), gatheredMetricValue(t, "fast_sandbox_fastlet_admission_slots", map[string]string{"state": "running"}))
	require.Equal(t, float64(1), gatheredMetricValue(t, "fast_sandbox_fastlet_admission_slots", map[string]string{"state": "deleting"}))
}

func TestUserProcessStartMetricsRejectUnprovenSupervisorStart(t *testing.T) {
	observedCounter := userProcessStartObservationTotal.WithLabelValues("test-runtime", "test-profile", string(fastletapi.UserProcessStartRuntimeDirect), "observed")
	unavailableCounter := userProcessStartObservationTotal.WithLabelValues("test-runtime", "test-profile", string(fastletapi.UserProcessStartSandboxInitUnreported), "unavailable")
	observedHistogram := userProcessStartLatency.WithLabelValues("test-runtime", "test-profile", string(fastletapi.UserProcessStartRuntimeDirect))
	_ = observedCounter
	_ = unavailableCounter
	_ = observedHistogram
	observedBefore := gatheredMetricValue(t, "fast_sandbox_user_process_start_observation_total", map[string]string{
		"runtime": "test-runtime", "infra_profile": "test-profile", "source": string(fastletapi.UserProcessStartRuntimeDirect), "result": "observed",
	})
	unavailableBefore := gatheredMetricValue(t, "fast_sandbox_user_process_start_observation_total", map[string]string{
		"runtime": "test-runtime", "infra_profile": "test-profile", "source": string(fastletapi.UserProcessStartSandboxInitUnreported), "result": "unavailable",
	})
	histogramBefore := gatheredHistogramCount(t, "fast_sandbox_user_process_start_latency_seconds", map[string]string{
		"runtime": "test-runtime", "infra_profile": "test-profile", "source": string(fastletapi.UserProcessStartRuntimeDirect),
	})

	started := time.Unix(1700000000, 0)
	observeUserProcessStart("test-runtime", "test-profile", started, &SandboxMetadata{
		UserProcessStartedAt: started.Add(40 * time.Millisecond), UserProcessStartSource: fastletapi.UserProcessStartRuntimeDirect,
	})
	observeUserProcessStart("test-runtime", "test-profile", started, &SandboxMetadata{
		UserProcessStartSource: fastletapi.UserProcessStartSandboxInitUnreported,
	})

	require.Equal(t, observedBefore+1, gatheredMetricValue(t, "fast_sandbox_user_process_start_observation_total", map[string]string{
		"runtime": "test-runtime", "infra_profile": "test-profile", "source": string(fastletapi.UserProcessStartRuntimeDirect), "result": "observed",
	}))
	require.Equal(t, unavailableBefore+1, gatheredMetricValue(t, "fast_sandbox_user_process_start_observation_total", map[string]string{
		"runtime": "test-runtime", "infra_profile": "test-profile", "source": string(fastletapi.UserProcessStartSandboxInitUnreported), "result": "unavailable",
	}))
	require.Equal(t, histogramBefore+1, gatheredHistogramCount(t, "fast_sandbox_user_process_start_latency_seconds", map[string]string{
		"runtime": "test-runtime", "infra_profile": "test-profile", "source": string(fastletapi.UserProcessStartRuntimeDirect),
	}))
}

func gatheredMetricValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			matches := len(metric.Label) == len(labels)
			for _, pair := range metric.Label {
				if labels[pair.GetName()] != pair.GetValue() {
					matches = false
					break
				}
			}
			if !matches {
				continue
			}
			if metric.Counter != nil {
				return metric.Counter.GetValue()
			}
			if metric.Gauge != nil {
				return metric.Gauge.GetValue()
			}
		}
	}
	t.Fatalf("metric %s with labels %v not found", name, labels)
	return 0
}

func gatheredHistogramCount(t *testing.T, name string, labels map[string]string) uint64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			matches := len(metric.Label) == len(labels)
			for _, pair := range metric.Label {
				if labels[pair.GetName()] != pair.GetValue() {
					matches = false
					break
				}
			}
			if matches && metric.Histogram != nil {
				return metric.Histogram.GetSampleCount()
			}
		}
	}
	t.Fatalf("histogram %s with labels %v not found", name, labels)
	return 0
}
