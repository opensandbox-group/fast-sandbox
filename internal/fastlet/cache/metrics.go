package cache

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	cacheRevision = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "fast_sandbox_cache_revision",
		Help: "Current process-local cache inventory revision.",
	})
	cacheInventorySize = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "fast_sandbox_cache_inventory_size",
		Help: "Current complete, bounded cache inventory size.",
	})
	cacheSnapshotTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fast_sandbox_cache_snapshot_total",
		Help: "Cache inventory snapshot outcomes.",
	}, []string{"result"})
	cacheGCDecisionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fast_sandbox_cache_gc_decision_total",
		Help: "Cache GC planning decisions; this does not imply runtime deletion completed.",
	}, []string{"result"})
)
