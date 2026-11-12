package dart

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"k8s.io/klog/v2"

	"fast-sandbox/internal/componentlog"
)

func TestArgs(t *testing.T) {
	manager := New(Config{
		Listen:        "127.0.0.1:8145",
		Admin:         "127.0.0.1:8147",
		PeerListen:    ":9000",
		PeerAdvertise: "10.0.0.5:9000",
		SelfID:        "node-1",
		Discover:      "dns:dart.fast-sandbox-system.svc:9000",
		CacheDir:      "/var/lib/fast-sandbox/firecracker/cache/dart",
		CacheSize:     "20GiB",
	})
	require.Equal(t, []string{
		"-listen=127.0.0.1:8145",
		"-admin=127.0.0.1:8147",
		"-peer-listen=:9000",
		"-cache-dir=/var/lib/fast-sandbox/firecracker/cache/dart",
		"-cache-size=20GiB",
		"-peer-advertise=10.0.0.5:9000",
		"-self-id=node-1",
		"-discover=dns:dart.fast-sandbox-system.svc:9000",
	}, manager.Args())
}

func TestArgsDefaultsAndOptionalFlags(t *testing.T) {
	manager := New(Config{CacheDir: "/tmp/dart-cache"})
	args := manager.Args()
	require.Contains(t, args, "-listen=127.0.0.1:8145")
	require.Contains(t, args, "-admin=127.0.0.1:8147")
	require.Contains(t, args, "-peer-listen=:9000")
	require.Contains(t, args, "-cache-dir=/tmp/dart-cache")
	require.Contains(t, args, "-cache-size=20GiB")
	require.NotContains(t, args, "-peer-advertise=")
	require.NotContains(t, args, "-self-id=")
	require.NotContains(t, args, "-discover=")
}

// helperScript writes an executable shell child that either sleeps (until a
// signal kills it) or exits immediately, standing in for the DART binary.
func helperScript(t *testing.T, behavior string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "dart")
	body := "#!/bin/sh\n"
	switch behavior {
	case "sleep":
		body += "trap 'exit 0' TERM INT\nwhile true; do sleep 1; done\n"
	case "exit":
		body += "exit 7\n"
	}
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
	return path
}

// Child output can arrive while a test reads the buffer.
type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(payload []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(payload)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestManagerLogsUseComponentFileWithoutRedirectingChildOutput(t *testing.T) {
	state := klog.CaptureState()
	t.Cleanup(state.Restore)
	directory := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "logging.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf("directory: %q\n", directory)), 0o600))
	t.Setenv(componentlog.ConfigEnv, configPath)
	closeLogs, err := componentlog.Configure("firecracker-runtime")
	require.NoError(t, err)
	t.Cleanup(closeLogs)

	childOutput := &safeBuffer{}
	manager := New(Config{Binary: filepath.Join(t.TempDir(), "missing-dart"), Log: childOutput})
	_, _, err = manager.start(context.Background())
	require.Error(t, err)
	require.Empty(t, childOutput.String())

	binary := filepath.Join(t.TempDir(), "dart")
	require.NoError(t, os.WriteFile(binary, []byte("#!/bin/sh\nprintf 'child stdout\\n'\nprintf 'child stderr\\n' >&2\n"), 0o755))
	manager = New(Config{Binary: binary, Log: childOutput})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, waitCh, err := manager.start(ctx)
	require.NoError(t, err)
	require.NoError(t, <-waitCh)
	require.Contains(t, childOutput.String(), "child stdout\n")
	require.Contains(t, childOutput.String(), "child stderr\n")
	require.NotContains(t, childOutput.String(), "DART started")

	klog.Flush()
	data, err := os.ReadFile(filepath.Join(directory, "firecracker-runtime.log"))
	require.NoError(t, err)
	require.Contains(t, string(data), "DART start failed")
	require.Contains(t, string(data), "DART started")
	require.NotContains(t, string(data), "child stdout")
	require.NotContains(t, string(data), "child stderr")
}

func TestManagerRestartsCrashingChild(t *testing.T) {
	log := &safeBuffer{}
	manager := New(Config{Binary: helperScript(t, "exit"), CacheDir: t.TempDir(), Log: log})
	manager.backoff = time.Millisecond // shorten the crash-restart delay

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()

	// The child exits instantly; the supervisor must restart it.
	deadline := time.Now().Add(5 * time.Second)
	for manager.StartCount() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	require.GreaterOrEqual(t, manager.StartCount(), 3, "crash-looping child must be restarted (log: %s)", log.String())

	cancel()
	require.NoError(t, <-done, "Run must return cleanly on cancel")
	count := manager.StartCount()
	time.Sleep(20 * time.Millisecond)
	require.Equal(t, count, manager.StartCount(), "no restart may happen after cancellation")
}

func TestManagerGracefulStopOnCancel(t *testing.T) {
	log := &safeBuffer{}
	manager := New(Config{Binary: helperScript(t, "sleep"), CacheDir: t.TempDir(), Log: log})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()

	require.Eventually(t, func() bool { return manager.StartCount() == 1 }, 5*time.Second, 10*time.Millisecond)
	cancel()
	require.NoError(t, <-done, "Run must stop the child and return on cancel")
}

func TestManagerHealthTracksAdminPlane(t *testing.T) {
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	}))
	t.Cleanup(admin.Close)
	log := &safeBuffer{}
	manager := New(Config{
		Binary: helperScript(t, "sleep"), CacheDir: t.TempDir(), Log: log,
		Admin: admin.URL, // a full URL is accepted for tests
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()

	require.Eventually(t, manager.Healthy, 5*time.Second, 10*time.Millisecond,
		"Healthy must become true once the admin plane answers")
	cancel()
	require.NoError(t, <-done)
}

func TestManagerHealthyFalseWhenAdminDown(t *testing.T) {
	log := &safeBuffer{}
	manager := New(Config{Binary: helperScript(t, "sleep"), CacheDir: t.TempDir(), Log: log})
	manager.healthy.Store(true) // start from "previously healthy"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- manager.Run(ctx) }()
	// No admin plane answers: the first probe must flip Healthy to false
	// while the child keeps running.
	require.Eventually(t, func() bool { return !manager.Healthy() }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, 1, manager.StartCount(), "child must stay up with an unreachable admin plane")
	cancel()
	require.NoError(t, <-done)
}
