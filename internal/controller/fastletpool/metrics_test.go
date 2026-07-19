package fastletpool

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestTopKMetricsClassifyImageAffinityWithoutIdentityLabels(t *testing.T) {
	hit := imageAffinityResult.WithLabelValues("hit")
	miss := imageAffinityResult.WithLabelValues("miss")
	hitBefore := gatheredCounterValue(t, "fast_sandbox_image_affinity_result_total", map[string]string{"result": "hit"})
	missBefore := gatheredCounterValue(t, "fast_sandbox_image_affinity_result_total", map[string]string{"result": "miss"})

	recordTopK(
		1,
		[]FastletInfo{{ID: "eligible", CacheComplete: true, Images: []string{"registry.example/app:v1"}}},
		"registry.example/app:v1",
	)
	recordTopK(
		1,
		[]FastletInfo{{ID: "eligible", CacheComplete: true, Images: []string{"registry.example/other:v1"}}},
		"registry.example/app:v1",
	)

	_ = hit
	_ = miss
	require.Equal(t, hitBefore+1, gatheredCounterValue(t, "fast_sandbox_image_affinity_result_total", map[string]string{"result": "hit"}))
	require.Equal(t, missBefore+1, gatheredCounterValue(t, "fast_sandbox_image_affinity_result_total", map[string]string{"result": "miss"}))
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
