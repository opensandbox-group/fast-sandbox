package network

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"fast-sandbox/internal/observability"
)

// resultLabel distinguishes acquire/persist outcomes on the slot metrics.
const (
	resultLabel         = "result"
	metricResultError   = "error"
	metricResultSuccess = "success"
)

var (
	networkSlotAcquireTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fastlet_network_slot_acquire_total",
		Help: "Number of Fastlet network slot acquisitions by warm-pool result.",
	}, []string{resultLabel})
	networkSlots = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "fastlet_network_slots",
		Help: "Current Fastlet network slots by durable phase.",
	}, []string{"phase"})
	networkSlotAvailable = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "fast_sandbox_network_slot_available",
		Help: "Current number of clean Fastlet-owned network slots.",
	})
	networkSlotInUse = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "fast_sandbox_network_slot_inuse",
		Help: "Current number of bound or destroying Fastlet-owned network slots.",
	})
	networkSlotAcquireLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fast_sandbox_network_slot_acquire_latency_seconds",
		Help:    "Latency to resolve or durably bind a pre-created network slot.",
		Buckets: prometheus.ExponentialBuckets(.00025, 2, 15),
	}, []string{resultLabel})
	networkSlotPersistLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fast_sandbox_network_slot_persist_latency_seconds",
		Help:    "Latency of the durable state write performed when a clean network slot is bound.",
		Buckets: prometheus.ExponentialBuckets(.00025, 2, 15),
	}, []string{resultLabel})
	networkSlotReturnTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fastlet_network_slot_return_total",
		Help: "Number of pooled network slot returns by result (clean or quarantined).",
	}, []string{resultLabel})
	networkSlotReturnStageLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "fastlet_network_slot_return_stage_latency_seconds",
		Help:    "Latency of pooled return stages: deep reset and post-reset audit.",
		Buckets: prometheus.ExponentialBuckets(.00025, 2, 17),
	}, []string{"stage", resultLabel})
)

func recordSlotAcquire(result string) {
	networkSlotAcquireTotal.WithLabelValues(result).Inc()
}

func recordSlotPhases(clean, bound, destroying, returning int) {
	networkSlots.WithLabelValues("clean").Set(float64(clean))
	networkSlots.WithLabelValues("bound").Set(float64(bound))
	networkSlots.WithLabelValues("destroying").Set(float64(destroying))
	networkSlots.WithLabelValues("returning").Set(float64(returning))
	networkSlotAvailable.Set(float64(clean))
	networkSlotInUse.Set(float64(bound + destroying + returning))
}

func recordSlotReturn(result string) {
	networkSlotReturnTotal.WithLabelValues(result).Inc()
}

func observeSlotReturnStage(stage string, started time.Time, err error) {
	result := metricResultSuccess
	if err != nil {
		result = metricResultError
	}
	networkSlotReturnStageLatency.WithLabelValues(stage, result).Observe(time.Since(started).Seconds())
}

func observeSlotAcquire(result string, started time.Time) {
	networkSlotAcquireLatency.WithLabelValues(result).Observe(time.Since(started).Seconds())
}

func observeSlotPersist(started time.Time, err error) {
	result := metricResultSuccess
	if err != nil {
		result = metricResultError
	}
	networkSlotPersistLatency.WithLabelValues(result).Observe(time.Since(started).Seconds())
}

var guestApplyStageLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "fast_sandbox_network_guest_apply_stage_latency_seconds",
	Help:    "Latency of guest network apply stages; driver_apply contains route/NAT/ARP leaves.",
	Buckets: prometheus.ExponentialBuckets(.00025, 2, 16),
}, []string{"stage", "result"})

func startGuestApplyStage(ctx context.Context, stage string) (context.Context, func(error)) {
	started := time.Now()
	stageContext, span := observability.Start(ctx, "fastlet.network.guest_apply."+stage)
	return stageContext, func(err error) {
		result := metricResultSuccess
		if err != nil {
			result = metricResultError
		}
		guestApplyStageLatency.WithLabelValues(stage, result).Observe(time.Since(started).Seconds())
		observability.End(span, err)
	}
}
