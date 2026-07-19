package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/runtimecatalog"

	"github.com/stretchr/testify/require"
)

func TestHostCapabilityProberContainerAvailable(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "containerd.sock")
	require.NoError(t, os.WriteFile(socketPath, nil, 0o600))
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha1.RuntimeContainer)
	require.NoError(t, err)

	report := NewHostCapabilityProber().Probe(context.Background(), profile, socketPath)
	require.Equal(t, runtimecatalog.CapabilityAvailable, report.State)
	require.Empty(t, report.Missing)
}

func TestHostCapabilityProberFailsClosed(t *testing.T) {
	catalog := runtimecatalog.Builtin()

	boxlite, err := catalog.Resolve(apiv1alpha1.RuntimeBoxLite)
	require.NoError(t, err)
	report := NewHostCapabilityProber().Probe(context.Background(), boxlite, "")
	require.Equal(t, runtimecatalog.CapabilityUnsupported, report.State)
	require.Equal(t, "BoxLiteDriverNotImplemented", report.Reason)

	kata, err := catalog.Resolve(apiv1alpha1.RuntimeKataFc)
	require.NoError(t, err)
	prober := NewHostCapabilityProber()
	prober.stat = func(path string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	report = prober.Probe(context.Background(), kata, "/missing/containerd.sock")
	require.Equal(t, runtimecatalog.CapabilityDegraded, report.State)
	require.Equal(t, "KVMUnavailable", report.Reason)
	require.Contains(t, report.Missing, "/dev/kvm")
	require.Contains(t, report.Missing, kata.Containerd.ConfigPath)
}

func TestBuildRuntimeDriverSelection(t *testing.T) {
	catalog := runtimecatalog.Builtin()
	container, err := catalog.Resolve(apiv1alpha1.RuntimeContainer)
	require.NoError(t, err)
	driver, err := buildRuntimeDriver(container)
	require.NoError(t, err)
	require.IsType(t, &ContainerdRuntime{}, driver)

	boxlite, err := catalog.Resolve(apiv1alpha1.RuntimeBoxLite)
	require.NoError(t, err)
	driver, err = buildRuntimeDriver(boxlite)
	require.Nil(t, driver)
	require.True(t, errors.Is(err, ErrUnsupportedRuntime))
}
