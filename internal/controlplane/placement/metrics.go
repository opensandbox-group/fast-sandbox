package placement

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	registryHeartbeatAge = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fast_sandbox_registry_heartbeat_age_seconds",
		Help:    "Maximum Fastlet heartbeat age per state when Top-K evaluates the local registry.",
		Buckets: []float64{1, 2, 5, 10, 20, 30, 45, 60, 120, 300},
	}, []string{"state"})
	registryCandidateCount = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fast_sandbox_registry_candidate_count",
		Help:    "Number of Fastlet candidates before and after hard filtering.",
		Buckets: []float64{0, 1, 2, 3, 5, 10, 20, 50, 100, 250, 500, 1000, 2500, 5000},
	}, []string{"state"})
	imageAffinityResult = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fast_sandbox_image_affinity_result_total",
		Help: "Image-affinity outcome for Top-K selections.",
	}, []string{"result"})
)

type heartbeatAgeState struct {
	seen bool
	max  float64
}

type heartbeatAgeSummary struct {
	fresh   heartbeatAgeState
	stale   heartbeatAgeState
	missing heartbeatAgeState
}

func (s *heartbeatAgeSummary) observe(lastHeartbeat, now time.Time, staleAfter time.Duration) {
	state := &s.fresh
	age := 0.0
	if lastHeartbeat.IsZero() {
		state = &s.missing
	} else {
		ageDuration := now.Sub(lastHeartbeat)
		if ageDuration < 0 {
			ageDuration = 0
		}
		age = ageDuration.Seconds()
		if ageDuration > staleAfter {
			state = &s.stale
		}
	}
	state.seen = true
	if age > state.max {
		state.max = age
	}
}

func recordHeartbeatAgeSummary(summary heartbeatAgeSummary) {
	for _, state := range []struct {
		name string
		age  heartbeatAgeState
	}{
		{name: "fresh", age: summary.fresh},
		{name: "stale", age: summary.stale},
		{name: "missing", age: summary.missing},
	} {
		if state.age.seen {
			registryHeartbeatAge.WithLabelValues(state.name).Observe(state.age.max)
		}
	}
}

func recordTopK(watched, eligible int, requestedImage string, selectedImageHit bool) {
	registryCandidateCount.WithLabelValues("watched").Observe(float64(watched))
	registryCandidateCount.WithLabelValues("eligible").Observe(float64(eligible))
	result := "not_requested"
	if requestedImage != "" {
		result = "no_candidate"
		if eligible > 0 {
			result = "miss"
			if selectedImageHit {
				result = "hit"
			}
		}
	}
	imageAffinityResult.WithLabelValues(result).Inc()
}
