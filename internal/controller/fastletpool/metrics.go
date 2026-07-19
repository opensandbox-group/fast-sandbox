package fastletpool

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	registryHeartbeatAge = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fast_sandbox_registry_heartbeat_age_seconds",
		Help:    "Age of Fastlet heartbeat observations when Top-K evaluates the local registry.",
		Buckets: []float64{1, 2, 5, 10, 20, 30, 45, 60, 120, 300},
	}, []string{"state"})
	registryCandidateCount = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fast_sandbox_registry_candidate_count",
		Help:    "Number of Fastlet candidates before and after hard filtering.",
		Buckets: prometheus.LinearBuckets(0, 1, 11),
	}, []string{"state"})
	imageAffinityResult = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fast_sandbox_image_affinity_result_total",
		Help: "Image-affinity outcome for Top-K selections.",
	}, []string{"result"})
)

func recordHeartbeatAges(candidates []FastletInfo, now time.Time, staleAfter time.Duration) {
	for _, candidate := range candidates {
		state := "fresh"
		age := 0.0
		if candidate.LastHeartbeat.IsZero() {
			state = "missing"
		} else {
			ageDuration := now.Sub(candidate.LastHeartbeat)
			if ageDuration < 0 {
				ageDuration = 0
			}
			age = ageDuration.Seconds()
			if ageDuration > staleAfter {
				state = "stale"
			}
		}
		registryHeartbeatAge.WithLabelValues(state).Observe(age)
	}
}

func recordTopK(watched int, eligible []FastletInfo, requestedImage string) {
	registryCandidateCount.WithLabelValues("watched").Observe(float64(watched))
	registryCandidateCount.WithLabelValues("eligible").Observe(float64(len(eligible)))
	result := "not_requested"
	if requestedImage != "" {
		result = "no_candidate"
		if len(eligible) > 0 {
			result = "miss"
			if imageHit(eligible[0], requestedImage) {
				result = "hit"
			}
		}
	}
	imageAffinityResult.WithLabelValues(result).Inc()
}
