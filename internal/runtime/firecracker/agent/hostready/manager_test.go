package hostready

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/stretchr/testify/require"
)

func TestManagerPassInstallsChecksAndApplies(t *testing.T) {
	assetsDir := filepath.Join(t.TempDir(), "fc")
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A pre-verified asset layout keeps the manager off the network.
	config := AssetConfig{
		Dir:          assetsDir,
		KernelURL:    "http://127.0.0.1:1/kernel",
		ReleaseBase:  "http://127.0.0.1:1",
		VerifyBinary: func(string) error { return nil },
	}
	if err := os.WriteFile(filepath.Join(assetsDir, "vmlinux.bin"), []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	probes, _ := healthyProbes(t)
	client := &fakeNodeClient{node: &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}}}
	manager := NewManager(ManagerConfig{
		NodeName: "node-1",
		Client:   client,
		Check:    CheckConfig{StateRoot: t.TempDir(), AssetsDir: assetsDir},
		Assets:   &config,
		Probes:   probes.probes(assetsDir),
	})
	manager.pass(context.Background())
	report, ok := manager.Snapshot()
	if !ok || !report.Ready {
		t.Fatalf("expected a ready snapshot, ok=%v report=%s", ok, report.String())
	}
	ready, summary := manager.Health()
	if !ready || summary != report.Summary {
		t.Fatalf("Health must mirror the report: %v %q", ready, summary)
	}
	if len(client.labelPatches) != 1 {
		t.Fatalf("the pass must converge the labels, got %d patches", len(client.labelPatches))
	}
}

func TestManagerHealthBeforeFirstPass(t *testing.T) {
	manager := NewManager(ManagerConfig{})
	ready, summary := manager.Health()
	if ready || summary != "check pending" {
		t.Fatalf("expected a pending health, got %v %q", ready, summary)
	}
}

func TestManagerReloadsSettingsEachPass(t *testing.T) {
	probes, assets := healthyProbes(t)
	passes := 0
	manager := NewManager(ManagerConfig{
		Check:  CheckConfig{StateRoot: t.TempDir(), AssetsDir: assets},
		Probes: probes.probes(assets),
		Settings: func() (Settings, error) {
			passes++
			return Settings{Check: CheckConfig{StateRoot: t.TempDir(), AssetsDir: assets}, Interval: time.Millisecond}, nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		manager.Run(ctx)
		close(done)
	}()
	// Several fast passes (1ms interval) must complete; the Settings func
	// is re-read before every pass.
	deadline := time.After(5 * time.Second)
	for passes < 3 {
		select {
		case <-done:
			t.Fatal("Run must keep looping while the context is live")
		case <-deadline:
			t.Fatal("expected periodic passes with the reloaded interval")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run must return when the context is canceled")
	}
	report, ok := manager.Snapshot()
	require.True(t, ok, "expected a completed pass")
	require.True(t, report.Ready)
}

func TestManagerKeepsSettingsOnReloadError(t *testing.T) {
	probes, assets := healthyProbes(t)
	manager := NewManager(ManagerConfig{
		Check:  CheckConfig{StateRoot: t.TempDir(), AssetsDir: assets},
		Probes: probes.probes(assets),
		Settings: func() (Settings, error) {
			return Settings{}, errors.New("config broke")
		},
	})
	manager.reloadSettings()
	// The static settings survive the failed reload.
	require.Equal(t, DefaultInterval, manager.current.Interval)
	_, ok := manager.Snapshot()
	require.False(t, ok, "no pass has run yet")
}
