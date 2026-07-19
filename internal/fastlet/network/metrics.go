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
)

func recordSlotAcquire(result string) {
	networkSlotAcquireTotal.WithLabelValues(result).Inc()
}

func recordSlotPhases(clean, bound, destroying int) {
	networkSlots.WithLabelValues("clean").Set(float64(clean))
	networkSlots.WithLabelValues("bound").Set(float64(bound))
	networkSlots.WithLabelValues("destroying").Set(float64(destroying))
}
