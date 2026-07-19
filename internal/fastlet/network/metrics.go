package network

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	networkSlotAcquireTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fastlet_network_slot_acquire_total",
		Help: "Number of Fastlet network slot acquisitions by warm-pool result.",
	}, []string{"result"})
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
)

func recordSlotAcquire(result string) {
	networkSlotAcquireTotal.WithLabelValues(result).Inc()
}

func recordSlotPhases(clean, bound, destroying int) {
	networkSlots.WithLabelValues("clean").Set(float64(clean))
	networkSlots.WithLabelValues("bound").Set(float64(bound))
	networkSlots.WithLabelValues("destroying").Set(float64(destroying))
	networkSlotAvailable.Set(float64(clean))
	networkSlotInUse.Set(float64(bound + destroying))
}
