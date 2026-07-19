package runtime

import (
	"errors"
	"testing"

	"fast-sandbox/internal/api"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestAdmissionMetricsUseBoundedOutcomeLabels(t *testing.T) {
	accepted := fastletAdmissionTotal.WithLabelValues("test-reserve", "accepted", "none")
	rejected := fastletAdmissionTotal.WithLabelValues("test-reserve", "rejected", string(api.ErrorCapacityRejected))
	failed := fastletAdmissionTotal.WithLabelValues("test-reserve", "error", "unknown")
	acceptedBefore := gatheredMetricValue(t, "fast_sandbox_fastlet_admission_total", map[string]string{"operation": "test-reserve", "result": "accepted", "reason": "none"})
	rejectedBefore := gatheredMetricValue(t, "fast_sandbox_fastlet_admission_total", map[string]string{"operation": "test-reserve", "result": "rejected", "reason": string(api.ErrorCapacityRejected)})
	failedBefore := gatheredMetricValue(t, "fast_sandbox_fastlet_admission_total", map[string]string{"operation": "test-reserve", "result": "error", "reason": "unknown"})

	recordAdmission("test-reserve", nil)
	recordAdmission("test-reserve", &api.FastletError{Code: api.ErrorCapacityRejected, Message: "full"})
	recordAdmission("test-reserve", errors.New("transport failure"))

	_ = accepted
	_ = rejected
	_ = failed
	require.Equal(t, acceptedBefore+1, gatheredMetricValue(t, "fast_sandbox_fastlet_admission_total", map[string]string{"operation": "test-reserve", "result": "accepted", "reason": "none"}))
	require.Equal(t, rejectedBefore+1, gatheredMetricValue(t, "fast_sandbox_fastlet_admission_total", map[string]string{"operation": "test-reserve", "result": "rejected", "reason": string(api.ErrorCapacityRejected)}))
	require.Equal(t, failedBefore+1, gatheredMetricValue(t, "fast_sandbox_fastlet_admission_total", map[string]string{"operation": "test-reserve", "result": "error", "reason": "unknown"}))
}

func TestAdmissionStatusMetricsReflectLatestSnapshot(t *testing.T) {
	recordAdmissionStatus(api.AdmissionStatus{
		Capacity: 8, Reservations: 2, Used: 7, Creating: 1, Running: 3, Deleting: 1,
	})

	require.Equal(t, float64(2), gatheredMetricValue(t, "fast_sandbox_fastlet_reservation_inflight", nil))
	require.Equal(t, float64(8), gatheredMetricValue(t, "fast_sandbox_fastlet_admission_slots", map[string]string{"state": "capacity"}))
	require.Equal(t, float64(7), gatheredMetricValue(t, "fast_sandbox_fastlet_admission_slots", map[string]string{"state": "used"}))
	require.Equal(t, float64(1), gatheredMetricValue(t, "fast_sandbox_fastlet_admission_slots", map[string]string{"state": "creating"}))
	require.Equal(t, float64(3), gatheredMetricValue(t, "fast_sandbox_fastlet_admission_slots", map[string]string{"state": "running"}))
	require.Equal(t, float64(1), gatheredMetricValue(t, "fast_sandbox_fastlet_admission_slots", map[string]string{"state": "deleting"}))
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
