package firecracker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// MemoryPressurePolicy is opt-in Fastlet admission policy. It never changes
// guest RAM, inflates a balloon, or interprets guest statistics as headroom.
type MemoryPressurePolicy struct {
	NodeMinAvailableBytes uint64  `json:"nodeMinAvailableBytes"`
	CgroupHighPercent     float64 `json:"cgroupHighPercent"`
	CgroupLowPercent      float64 `json:"cgroupLowPercent"`
	MaxPSISomeAvg10       float64 `json:"maxPSISomeAvg10"`
	RecoverySeconds       int     `json:"recoverySeconds"`
}

type memoryPressureSample struct {
	available, current, limit uint64
	psi                       float64
}

type memoryPressureGate struct {
	mu        sync.Mutex
	policy    MemoryPressurePolicy
	read      func() (memoryPressureSample, error)
	updated   time.Time
	healthyAt time.Time
	blocked   bool
	reason    string
}

var memoryPressureValues = promauto.NewGaugeVec(prometheus.GaugeOpts{
	Name: "fast_sandbox_memory_pressure", Help: "Opt-in host memory admission observations; PSI is percent, other memory observations are bytes.",
}, []string{"observation"})

// ConfigureMemoryPressure starts a bounded, low-frequency telemetry loop.
func (d *Driver) ConfigureMemoryPressure(ctx context.Context, raw string) error {
	if raw == "" {
		return nil
	}
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return fmt.Errorf("memory pressure policy must be a JSON object")
	}
	p := MemoryPressurePolicy{NodeMinAvailableBytes: 1 << 30, CgroupHighPercent: 90, CgroupLowPercent: 80, MaxPSISomeAvg10: 10, RecoverySeconds: 5}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return fmt.Errorf("memory pressure policy: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("memory pressure policy must contain exactly one JSON object")
	}
	if p.CgroupLowPercent <= 0 || p.CgroupHighPercent >= 100 || p.CgroupLowPercent >= p.CgroupHighPercent || p.MaxPSISomeAvg10 < 0 || p.MaxPSISomeAvg10 > 100 || p.RecoverySeconds < 1 || p.RecoverySeconds > 300 {
		return fmt.Errorf("invalid memory pressure thresholds")
	}
	gate := &memoryPressureGate{policy: p, read: func() (memoryPressureSample, error) {
		return readMemoryPressureAt("/proc", "/sys/fs/cgroup", p.MaxPSISomeAvg10 > 0)
	}}
	gate.sample(time.Now())
	loopContext, cancel := context.WithCancel(ctx)
	d.mu.Lock()
	if d.memoryPressureCancel != nil {
		d.memoryPressureCancel()
	}
	d.memoryPressure, d.memoryPressureCancel = gate, cancel
	d.mu.Unlock()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		samples := 0
		for {
			select {
			case <-loopContext.Done():
				return
			case now := <-ticker.C:
				gate.sample(now)
				samples++
				if samples%5 == 0 {
					d.observeResidentMemory()
				}
			}
		}
	}()
	return nil
}

// Resident observations are aggregated over this driver's VMM handles only.
// PSS avoids counting each shared golden-memory page once per process.
func (d *Driver) observeResidentMemory() {
	d.mu.RLock()
	pids := make([]int, 0, len(d.processes))
	for _, process := range d.processes {
		pids = append(pids, process.PID())
	}
	d.mu.RUnlock()
	var rss, pss, observed uint64
	for _, pid := range pids {
		payload, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", pid))
		if err != nil {
			continue
		}
		observed++
		for _, line := range strings.Split(string(payload), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 || (fields[0] != "Rss:" && fields[0] != "Pss:") {
				continue
			}
			n, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				continue
			}
			if fields[0] == "Rss:" {
				rss += n * 1024
			} else {
				pss += n * 1024
			}
		}
	}
	for name, value := range map[string]uint64{"vmm_rss_bytes": rss, "vmm_pss_bytes": pss, "vmm_observed_processes": observed, "vmm_expected_processes": uint64(len(pids))} {
		memoryPressureValues.WithLabelValues(name).Set(float64(value))
	}
}

// CheckRuntimeResourceAdmission rejects before any new runtime side effects.
// Existing instances and their data plane remain available while admission is blocked.
func (d *Driver) CheckRuntimeResourceAdmission() error {
	d.mu.RLock()
	g := d.memoryPressure
	d.mu.RUnlock()
	if g == nil {
		return nil
	}
	return g.check(time.Now())
}

func (g *memoryPressureGate) sample(now time.Time) {
	s, err := g.read()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.updated = now
	used := float64(0)
	if s.limit > 0 {
		used = 100 * float64(s.current) / float64(s.limit)
	}
	reason := ""
	switch {
	case err != nil:
		reason = "memory telemetry unavailable: " + err.Error()
	case s.available < g.policy.NodeMinAvailableBytes:
		reason = "node available memory is below the admission floor"
	case used >= g.policy.CgroupHighPercent:
		reason = "runtime cgroup memory exceeds the high watermark"
	case g.policy.MaxPSISomeAvg10 > 0 && s.psi >= g.policy.MaxPSISomeAvg10:
		reason = "memory PSI exceeds the admission threshold"
	}
	if reason != "" {
		g.blocked, g.reason, g.healthyAt = true, reason, time.Time{}
	} else if g.blocked {
		// Retain the block until *all* signals satisfy the recovery thresholds.
		if used > g.policy.CgroupLowPercent || (g.policy.MaxPSISomeAvg10 > 0 && s.psi >= g.policy.MaxPSISomeAvg10/2) || s.available < g.policy.NodeMinAvailableBytes+g.policy.NodeMinAvailableBytes/10 {
			g.healthyAt = time.Time{}
		} else {
			if g.healthyAt.IsZero() {
				g.healthyAt = now
			}
			if now.Sub(g.healthyAt) >= time.Duration(g.policy.RecoverySeconds)*time.Second {
				g.blocked, g.reason = false, ""
			}
		}
	}
	valid, blocked := 1., 0.
	if err != nil {
		valid = 0
	}
	if g.blocked {
		blocked = 1
	}
	for name, value := range map[string]float64{
		"telemetry_valid": valid, "admission_blocked": blocked,
		"node_available_bytes": float64(s.available), "cgroup_current_bytes": float64(s.current),
		"cgroup_limit_bytes": float64(s.limit), "psi_some_avg10_percent": s.psi,
	} {
		memoryPressureValues.WithLabelValues(name).Set(value)
	}
	psiEnabled := 0.
	if g.policy.MaxPSISomeAvg10 > 0 {
		psiEnabled = 1
	}
	memoryPressureValues.WithLabelValues("psi_gate_enabled").Set(psiEnabled)
}

func (g *memoryPressureGate) check(now time.Time) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.updated.IsZero() || now.Sub(g.updated) > 3*time.Second {
		return fmt.Errorf("memory pressure admission: telemetry is stale")
	}
	if g.blocked {
		return fmt.Errorf("memory pressure admission: %s", g.reason)
	}
	return nil
}

// Check each finite ancestor budget, including the owning Pod. Privileged
// pods may see the host cgroup root, while private namespaces see only their
// own subtree; probe the process path first and fall back to the mount root.
func readMemoryPressureAt(proc, root string, requirePSI bool) (memoryPressureSample, error) {
	var s memoryPressureSample
	meminfo, err := os.ReadFile(filepath.Join(proc, "meminfo"))
	if err != nil {
		return s, err
	}
	for _, line := range strings.Split(string(meminfo), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			n, e := strconv.ParseUint(fields[1], 10, 64)
			if e != nil {
				return s, e
			}
			s.available = n * 1024
		}
	}
	if s.available == 0 {
		return s, fmt.Errorf("MemAvailable missing or zero")
	}
	if requirePSI {
		psi, err := os.ReadFile(filepath.Join(proc, "pressure", "memory"))
		if err != nil {
			return s, err
		}
		s.psi, err = parseMemoryPSI(string(psi))
		if err != nil {
			return s, err
		}
	}
	s.current, s.limit, err = readCgroupBudget(proc, root)
	return s, err
}

func readCgroupBudget(proc, root string) (uint64, uint64, error) {
	membership, err := os.ReadFile(filepath.Join(proc, "self", "cgroup"))
	if err != nil {
		return 0, 0, err
	}
	path := visibleCgroupPath(root, string(membership))
	var worst float64
	var selectedCurrent, selectedLimit uint64
	observedCgroup := false
	for {
		limitBytes, e := os.ReadFile(filepath.Join(path, "memory.max"))
		if e != nil {
			if path != root {
				return 0, 0, e
			}
			break // host root has no memory.max; finite descendants were checked
		}
		observedCgroup = true
		if value := strings.TrimSpace(string(limitBytes)); value != "max" {
			limit, e := strconv.ParseUint(value, 10, 64)
			if e != nil || limit == 0 {
				return 0, 0, fmt.Errorf("invalid cgroup memory.max at %s", path)
			}
			current, e := readCgroupCurrent(path)
			if e != nil {
				return 0, 0, e
			}
			ratio := float64(current) / float64(limit)
			if ratio >= worst {
				worst, selectedCurrent, selectedLimit = ratio, current, limit
			}
		}
		if path == root {
			break
		}
		path = filepath.Dir(path)
	}
	if !observedCgroup {
		return 0, 0, fmt.Errorf("cgroup v2 memory budget interface unavailable")
	}
	return selectedCurrent, selectedLimit, nil
}

func readCgroupCurrent(path string) (uint64, error) {
	usage, err := os.ReadFile(filepath.Join(path, "memory.current"))
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(usage)), 10, 64)
}

func parseMemoryPSI(payload string) (float64, error) {
	for _, line := range strings.Split(payload, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "some" {
			continue
		}
		for _, field := range fields[1:] {
			if strings.HasPrefix(field, "avg10=") {
				value, err := strconv.ParseFloat(strings.TrimPrefix(field, "avg10="), 64)
				if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
					return 0, fmt.Errorf("invalid memory PSI some avg10")
				}
				return value, nil
			}
		}
	}
	return 0, fmt.Errorf("memory PSI some avg10 missing")
}

func visibleCgroupPath(root, membership string) string {
	path := root
	for _, line := range strings.Split(membership, "\n") {
		if strings.HasPrefix(line, "0::/") {
			candidate := filepath.Join(root, strings.TrimPrefix(line, "0::/"))
			// A cgroup namespace may report a path above its visible root.
			// Never walk outside the mounted hierarchy when resolving it.
			relative, e := filepath.Rel(root, candidate)
			if e == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				if _, e := os.Stat(filepath.Join(candidate, "memory.current")); e == nil {
					path = candidate
				}
			}
		}
	}
	return path
}
