package hostready

import (
	"context"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// DefaultInterval is the recheck cadence: frequent enough to pull the
// labels within minutes of a host healing (KVM module reloaded), rare
// enough to be free.
const DefaultInterval = 5 * time.Minute

// Settings is the mutable subset of the manager configuration, re-read
// before every pass (a mounted ConfigMap file can retune the thresholds,
// the asset source and the cadence without a restart).
type Settings struct {
	Check    CheckConfig
	Assets   *AssetConfig
	Interval time.Duration
}

// ManagerConfig assembles the readiness manager.
type ManagerConfig struct {
	// NodeName is the Node object to label (the DaemonSet injects
	// spec.nodeName). Empty disables node writes (checks-only mode).
	NodeName string
	// Check carries the paths and thresholds of the checks.
	Check CheckConfig
	// Assets configures the Firecracker asset installation; the agent
	// config always produces one (a pass installs before it checks).
	// Tests may pass nil to keep a pass checks-only.
	Assets *AssetConfig
	// Interval is the recheck cadence (0 = DefaultInterval).
	Interval time.Duration
	// Settings re-reads the mutable subset before every pass (hot config
	// reload); nil keeps the static config above. A reload error keeps
	// the previous settings (a broken config edit never stops the loop).
	Settings func() (Settings, error)
	// Probes overrides the host observations (tests).
	Probes Probes
	// Client performs the node writes; nil keeps the checks local (no
	// labels or condition are written).
	Client NodeClient
}

// Manager runs the check/install/label loop: one full pass at startup,
// then a recheck every interval. Every pass converges the node labels and
// the FirecrackerReady condition onto the latest report; a degraded host
// loses its labels until the next healthy pass.
type Manager struct {
	config     ManagerConfig
	reconciler *NodeReconciler
	// current is the settings of the upcoming pass (Run-goroutine only).
	current Settings

	mu      sync.RWMutex
	last    *Report
	checked time.Time
}

// NewManager assembles the readiness manager.
func NewManager(config ManagerConfig) *Manager {
	manager := &Manager{config: config}
	manager.current = manager.staticSettings()
	if config.Client != nil && config.NodeName != "" {
		manager.reconciler = NewNodeReconciler(config.Client, config.NodeName)
	}
	return manager
}

// staticSettings renders the fixed config as the initial settings.
func (m *Manager) staticSettings() Settings {
	interval := m.config.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	return Settings{Check: m.config.Check, Assets: m.config.Assets, Interval: interval}
}

// reloadSettings refreshes the mutable subset; an error (or a Settings
// func absent) keeps whatever is loaded.
func (m *Manager) reloadSettings() {
	if m.config.Settings == nil {
		return
	}
	settings, err := m.config.Settings()
	if err != nil {
		klog.ErrorS(err, "host readiness settings reload failed; keeping the previous settings")
		return
	}
	if settings.Interval <= 0 {
		settings.Interval = DefaultInterval
	}
	m.current = settings
}

// Run executes passes until the context is canceled. The first pass runs
// immediately; every error is logged and folded into the next pass — the
// manager never takes the agent down (the node keeps serving pulls).
func (m *Manager) Run(ctx context.Context) {
	m.reloadSettings()
	m.pass(ctx)
	for {
		timer := time.NewTimer(m.current.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		m.reloadSettings()
		m.pass(ctx)
	}
}

// pass runs one full check/install/label cycle.
func (m *Manager) pass(ctx context.Context) {
	klog.InfoS("host readiness pass started")
	if m.current.Assets != nil {
		if err := m.current.Assets.Ensure(ctx); err != nil {
			// The fc-assets check below fails on the same condition;
			// the error detail goes to the log for diagnosis.
			klog.ErrorS(err, "firecracker asset installation failed; the fc-assets check will report the outcome")
		}
	}
	report := RunChecks(m.current.Check, m.config.Probes)
	if m.reconciler != nil {
		if err := m.reconciler.Apply(ctx, report); err != nil {
			klog.ErrorS(err, "node readiness convergence failed", "ready", report.Ready)
		}
	}
	m.mu.Lock()
	m.last = &report
	m.checked = time.Now()
	m.mu.Unlock()
	if report.Ready {
		klog.InfoS("host readiness check passed", "summary", report.Summary)
	} else {
		klog.InfoS("host readiness check failed", "summary", report.Summary)
	}
	klog.V(2).InfoS("host readiness detail", "report", report.String())
}

// Snapshot returns the last report and whether one has completed.
func (m *Manager) Snapshot() (Report, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.last == nil {
		return Report{}, false
	}
	return *m.last, true
}

// Health reports the readiness state for the agent UDS health endpoint:
// the boolean is the last check outcome, the string the summary ("check
// pending" before the first pass completes).
func (m *Manager) Health() (bool, string) {
	report, ok := m.Snapshot()
	if !ok {
		return false, "check pending"
	}
	return report.Ready, report.Summary
}
