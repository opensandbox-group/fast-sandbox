package podcgroup

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Scrape live leaves on demand. Empty sandbox_uid identifies the Pod's VM
// pool. Ephemeral Sandbox IDs disappear at deletion, without retained series.
var runtimeGauges = []struct {
	name, file, key string
	scale           float64
	kind            prometheus.ValueType
}{
	{"cpu_seconds_total", "cpu.stat", "usage_usec", 1e-6, prometheus.CounterValue},
	{"cpu_throttled_periods_total", "cpu.stat", "nr_throttled", 1, prometheus.CounterValue},
	{"memory_bytes", "memory.current", "", 1, prometheus.GaugeValue},
	{"memory_peak_bytes", "memory.peak", "", 1, prometheus.GaugeValue},
	{"memory_limit_bytes", "memory.max", "", 1, prometheus.GaugeValue},
	{"oom_kills_total", "memory.events", "oom_kill", 1, prometheus.CounterValue},
	{"memory_psi_some_avg10", "memory.pressure", "some avg10=", 1, prometheus.GaugeValue},
}

type runtimeMetrics struct {
	manager *RuntimeManager
	descs   []*prometheus.Desc
}

var setupLatency = prometheus.NewHistogram(prometheus.HistogramOpts{
	Name:    "fast_sandbox_runtime_cgroup_leaf_setup_seconds",
	Help:    "Time to prepare a Sandbox cgroup, including lock wait and controller writes.",
	Buckets: prometheus.ExponentialBuckets(0.00001, 2, 17),
})

func observeSetup(start time.Time) { setupLatency.Observe(time.Since(start).Seconds()) }

func (m *RuntimeManager) RegisterMetrics(reg prometheus.Registerer) error {
	c := &runtimeMetrics{manager: m}
	for _, g := range runtimeGauges {
		c.descs = append(c.descs, prometheus.NewDesc("fast_sandbox_runtime_cgroup_"+g.name,
			"Host cgroup "+g.file+" "+g.key+"; empty sandbox_uid selects the shared runtime pool.", []string{"sandbox_uid"}, nil))
	}
	if err := reg.Register(c); err != nil {
		return err
	}
	return reg.Register(setupLatency)
}

func (c *runtimeMetrics) Describe(out chan<- *prometheus.Desc) {
	for _, desc := range c.descs {
		out <- desc
	}
}

func (c *runtimeMetrics) Collect(out chan<- prometheus.Metric) { //nolint:gocognit // metric files and keyed fields form one bounded collector loop
	m := c.manager
	m.mu.Lock()
	paths := map[string]string{"": m.AggregateFilesystemPath()}
	for id, path := range m.leaves {
		paths[id] = path
	}
	m.mu.Unlock()
	for id, path := range paths {
		for i, g := range runtimeGauges {
			data, err := os.ReadFile(filepath.Join(path, g.file))
			if err != nil {
				continue
			}
			raw := strings.TrimSpace(string(data))
			if g.key != "" {
				raw = ""
				for _, line := range strings.Split(string(data), "\n") {
					fields := strings.Fields(line)
					if len(fields) < 2 {
						continue
					}
					if fields[0] == g.key {
						raw = fields[1]
						break
					}
					if strings.HasSuffix(g.key, "=") && strings.HasPrefix(line, g.key) {
						values := strings.Fields(strings.TrimPrefix(line, g.key))
						if len(values) > 0 {
							raw = values[0]
						}
						break
					}
				}
			}
			// cgroup v2 represents an unlimited memory ceiling as "max".
			if g.file == "memory.max" && raw == "max" {
				raw = "+Inf"
			}
			if n, err := strconv.ParseFloat(raw, 64); err == nil {
				out <- prometheus.MustNewConstMetric(c.descs[i], g.kind, n*g.scale, id)
			}
		}
	}
}
