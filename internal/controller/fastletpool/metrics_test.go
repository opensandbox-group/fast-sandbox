package fastletpool

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestTopKMetricsClassifyImageAffinityWithoutIdentityLabels(t *testing.T) {
	hit := imageAffinityResult.WithLabelValues("hit")
	miss := imageAffinityResult.WithLabelValues("miss")
	hitBefore := gatheredCounterValue(t, "fast_sandbox_image_affinity_result_total", map[string]string{"result": "hit"})
	missBefore := gatheredCounterValue(t, "fast_sandbox_image_affinity_result_total", map[string]string{"result": "miss"})

	recordTopK(1, 1, "registry.example/app:v1", true)
	recordTopK(1, 1, "registry.example/app:v1", false)

	_ = hit
	_ = miss
	require.Equal(t, hitBefore+1, gatheredCounterValue(t, "fast_sandbox_image_affinity_result_total", map[string]string{"result": "hit"}))
	require.Equal(t, missBefore+1, gatheredCounterValue(t, "fast_sandbox_image_affinity_result_total", map[string]string{"result": "miss"}))
}

func TestHeartbeatAgeSummaryEmitsAtMostOneSamplePerState(t *testing.T) {
	now := time.Unix(1000, 0)
	beforeFresh := gatheredHistogramCount(t, "fast_sandbox_registry_heartbeat_age_seconds", map[string]string{"state": "fresh"})
	beforeStale := gatheredHistogramCount(t, "fast_sandbox_registry_heartbeat_age_seconds", map[string]string{"state": "stale"})
	beforeMissing := gatheredHistogramCount(t, "fast_sandbox_registry_heartbeat_age_seconds", map[string]string{"state": "missing"})

	summary := heartbeatAgeSummary{}
	summary.observe(now.Add(-time.Second), now, 30*time.Second)
	summary.observe(now.Add(-2*time.Second), now, 30*time.Second)
	summary.observe(now.Add(-time.Minute), now, 30*time.Second)
	summary.observe(now.Add(-2*time.Minute), now, 30*time.Second)
	summary.observe(time.Time{}, now, 30*time.Second)
	summary.observe(time.Time{}, now, 30*time.Second)
	recordHeartbeatAgeSummary(summary)

	require.Equal(t, beforeFresh+1, gatheredHistogramCount(t, "fast_sandbox_registry_heartbeat_age_seconds", map[string]string{"state": "fresh"}))
	require.Equal(t, beforeStale+1, gatheredHistogramCount(t, "fast_sandbox_registry_heartbeat_age_seconds", map[string]string{"state": "stale"}))
	require.Equal(t, beforeMissing+1, gatheredHistogramCount(t, "fast_sandbox_registry_heartbeat_age_seconds", map[string]string{"state": "missing"}))
}

func gatheredCounterValue(t *testing.T, name string, labels map[string]string) float64 {
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
			if matches && metric.Counter != nil {
				return metric.Counter.GetValue()
			}
		}
	}
	t.Fatalf("counter %s with labels %v not found", name, labels)
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
	return 0
}
