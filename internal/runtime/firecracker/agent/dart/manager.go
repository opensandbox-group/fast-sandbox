// Package dart supervises the node-local DART P2P daemon that backs the
// stage-2 artifact data plane. DART runs as an independent process inside
// the runtime-agent container (it cannot be imported: its code lives under
// internal/ upstream), speaking the prefix HTTP API
// (GET /dart/<presigned-s3-url>) on the loopback address and serving
// peer-to-peer distribution on the node address.
//
// The manager owns the child lifecycle end to end: start, admin-plane
// health probing, crash restart with exponential backoff, and graceful
// shutdown (SIGTERM, bounded wait, then kill). A broken DART never fails
// the agent: the pull client treats it as an opaque gateway and falls back
// to the direct S3 path, and agent UDS health stays green with P2PUp
// reporting the daemon state.
package dart

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"k8s.io/klog/v2"
)

const (
	defaultBinary     = "dart"
	defaultListen     = "127.0.0.1:8145" // client plane (agent pulls)
	defaultAdmin      = "127.0.0.1:8147" // admin plane (/healthz, /metrics)
	defaultPeerListen = ":9000"          // peer block server (node address)
	defaultCacheSize  = "20GiB"

	// restartBackoffBase is the first delay before a crash restart; the
	// delay doubles on every consecutive crash up to restartBackoffMax.
	restartBackoffBase = time.Second
	restartBackoffMax  = 30 * time.Second

	gracefulStopTimeout = 5 * time.Second
	healthProbeInterval = 2 * time.Second
	healthProbeTimeout  = time.Second
)

// Config describes the DART child process. All paths and addresses may be
// empty to take the defaults.
type Config struct {
	// Binary is the DART executable ("" = "dart" from PATH).
	Binary string
	// Listen is the client-plane listen address ("" = 127.0.0.1:8145), the
	// address the pull client's WithPeerGateway base points at.
	Listen string
	// Admin is the admin-plane address serving /healthz and /metrics
	// ("" = 127.0.0.1:8147). A full URL (http://host:port) is also
	// accepted for tests.
	Admin string
	// PeerListen is the peer block-server listen address ("" = :9000).
	PeerListen string
	// PeerAdvertise is the advertise address peers connect back to
	// (nodeIP:9000); empty omits the flag.
	PeerAdvertise string
	// SelfID is the stable node identity HRW placement is derived from
	// (the node name); empty omits the flag.
	SelfID string
	// Discover seeds peer discovery (e.g. dns:dart.<ns>.svc:9000); empty
	// omits the flag (single-node mode: cache only, no P2P).
	Discover string
	// CacheDir is the block cache directory under the StateRoot.
	CacheDir string
	// CacheSize bounds the block cache ("" = 20GiB).
	CacheSize string
	// Log receives the child's stdout/stderr (defaults to io.Discard).
	Log io.Writer
}

// Manager supervises one DART child process.
type Manager struct {
	config Config
	log    io.Writer
	logMu  sync.Mutex // serializes manager logs and child output

	mu         sync.Mutex
	startCount int
	healthy    atomic.Bool
	backoff    time.Duration
}

// New assembles the supervisor for config.
func New(config Config) *Manager {
	if config.Binary == "" {
		config.Binary = defaultBinary
	}
	if config.Listen == "" {
		config.Listen = defaultListen
	}
	if config.Admin == "" {
		config.Admin = defaultAdmin
	}
	if config.PeerListen == "" {
		config.PeerListen = defaultPeerListen
	}
	if config.CacheSize == "" {
		config.CacheSize = defaultCacheSize
	}
	if config.Log == nil {
		config.Log = io.Discard
	}
	return &Manager{config: config, log: config.Log}
}

// Args returns the DART command-line arguments the manager builds.
func (m *Manager) Args() []string {
	args := []string{
		"-listen=" + m.config.Listen,
		"-admin=" + m.config.Admin,
		"-peer-listen=" + m.config.PeerListen,
		"-cache-dir=" + m.config.CacheDir,
		"-cache-size=" + m.config.CacheSize,
	}
	if m.config.PeerAdvertise != "" {
		args = append(args, "-peer-advertise="+m.config.PeerAdvertise)
	}
	if m.config.SelfID != "" {
		args = append(args, "-self-id="+m.config.SelfID)
	}
	if m.config.Discover != "" {
		args = append(args, "-discover="+m.config.Discover)
	}
	return args
}

// StartCount reports how many times the child has been started (including
// crash restarts); tests assert the crash-restart discipline on it.
func (m *Manager) StartCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startCount
}

// Healthy reports whether the DART admin plane answered its last probe. A
// running child that has not bound its admin listener yet (or a crash
// between probes) reports false, which keeps artifact pulls on the direct
// S3 fallback path.
func (m *Manager) Healthy() bool {
	return m.healthy.Load()
}

// Run supervises the DART child until ctx is done: start, probe the admin
// plane, restart on crash with backoff, and stop the child gracefully on
// cancellation. Run returns nil after a clean stop.
func (m *Manager) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return nil //nolint:nilerr // cancellation is a clean stop; Run documents returning nil
		}
		cmd, waitCh, err := m.start(ctx)
		if err != nil {
			// The binary is missing or not executable: retrying in a loop
			// would only spin, but a container restart (image fix) should
			// pick it up, so keep the backoff discipline and report.
			if !m.backoffDelay(ctx) {
				return nil
			}
			continue
		}
		m.runProbes(ctx, cmd, waitCh)
		if ctx.Err() != nil {
			return nil //nolint:nilerr // cancellation is a clean stop; Run documents returning nil
		}
		if !m.backoffDelay(ctx) {
			return nil
		}
	}
}

// lockedWriter serializes writes to the underlying writer: the child's
// output pipe goroutine and the manager's own logs share the sink.
type lockedWriter struct {
	writer io.Writer
	mu     *sync.Mutex
}

func (w *lockedWriter) Write(payload []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(payload)
}

// logf writes a manager log line through the shared sink.
func (m *Manager) logf(format string, arguments ...any) {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	fmt.Fprintf(m.log, format, arguments...)
}

// start spawns the child and returns its process and a channel that yields
// the Wait error once it exits.
func (m *Manager) start(ctx context.Context) (*exec.Cmd, <-chan error, error) {
	command := exec.CommandContext(ctx, m.config.Binary, m.Args()...)
	command.Stdout = &lockedWriter{writer: m.log, mu: &m.logMu}
	command.Stderr = &lockedWriter{writer: m.log, mu: &m.logMu}
	if err := command.Start(); err != nil {
		m.logf("dart: start failed: %v\n", err)
		return nil, nil, err
	}
	m.mu.Lock()
	m.startCount++
	m.mu.Unlock()
	m.healthy.Store(false) // not probed yet
	waitCh := make(chan error, 1)
	go func() { waitCh <- command.Wait() }()
	m.logf("dart: started pid=%d args=%s\n", command.Process.Pid, strings.Join(m.Args(), " "))
	return command, waitCh, nil
}

// runProbes loops the admin health probe while the child is alive; it
// returns when the child exits or the context is canceled. On context
// cancellation the child is stopped gracefully (SIGTERM, bounded wait).
func (m *Manager) runProbes(ctx context.Context, cmd *exec.Cmd, waitCh <-chan error) {
	ticker := time.NewTicker(healthProbeInterval)
	defer ticker.Stop()
	for {
		m.probe()
		select {
		case <-ctx.Done():
			m.stop(cmd, waitCh)
			return
		case <-ticker.C:
		case err := <-waitCh:
			m.healthy.Store(false)
			if err != nil && !errors.Is(err, context.Canceled) {
				m.logf("dart: process exited: %v\n", err)
			}
			return
		}
	}
}

// stop terminates the child: SIGTERM first, then a bounded wait, then SIGKILL.
func (m *Manager) stop(cmd *exec.Cmd, waitCh <-chan error) {
	if cmd.Process == nil {
		return
	}
	m.logf("dart: stopping pid=%d\n", cmd.Process.Pid)
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-waitCh:
	case <-time.After(gracefulStopTimeout):
		_ = cmd.Process.Kill()
		<-waitCh
	}
}

// backoffDelay sleeps between crash restarts with exponential backoff; it
// returns false when the context ends during the wait.
func (m *Manager) backoffDelay(ctx context.Context) bool {
	m.mu.Lock()
	delay := m.backoff
	if delay == 0 {
		delay = restartBackoffBase
	}
	m.backoff = delay * 2
	if m.backoff > restartBackoffMax {
		m.backoff = restartBackoffMax
	}
	m.mu.Unlock()
	m.logf("dart: restarting in %s\n", delay)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// probe checks the admin plane /healthz endpoint and records the result.
// A successful probe also resets the restart backoff, so a long-running
// healthy child restarts immediately after an isolated crash.
func (m *Manager) probe() {
	url := m.config.Admin
	if !strings.Contains(url, "://") {
		url = "http://" + url
	}
	url = strings.TrimRight(url, "/") + "/healthz"
	client := &http.Client{Timeout: healthProbeTimeout}
	response, err := client.Get(url)
	if err != nil {
		if wasHealthy := m.healthy.Swap(false); wasHealthy {
			klog.ErrorS(err, "DART admin plane health probe failed; marking DART down", "admin", m.config.Admin)
		}
		return
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		if wasHealthy := m.healthy.Swap(false); wasHealthy {
			klog.ErrorS(nil, "DART admin plane health probe returned non-200; marking DART down", "admin", m.config.Admin, "statusCode", response.StatusCode)
		}
		return
	}
	m.healthy.Store(true)
	m.mu.Lock()
	m.backoff = 0
	m.mu.Unlock()
}
