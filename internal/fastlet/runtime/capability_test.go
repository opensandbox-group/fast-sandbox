package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	require.Equal(t, "BoxLiteRuntimeSidecarNotPackaged", report.Reason)

	kata, err := catalog.Resolve(apiv1alpha1.RuntimeKataQemu)
	require.NoError(t, err)
	prober := NewHostCapabilityProber()
	prober.stat = func(path string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	report = prober.Probe(context.Background(), kata, "/missing/containerd.sock")
	require.Equal(t, runtimecatalog.CapabilityDegraded, report.State)
	require.Equal(t, "KVMUnavailable", report.Reason)
	require.Contains(t, report.Missing, "/dev/kvm")
	require.Contains(t, report.Missing, kata.Containerd.ConfigPath)
}

func TestHostCapabilityProberRejectsUnvalidatedFirecrackerProfile(t *testing.T) {
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha1.RuntimeKataFc)
	require.NoError(t, err)
	report := NewHostCapabilityProber().Probe(context.Background(), profile, "/run/containerd/containerd.sock")
	require.Equal(t, runtimecatalog.CapabilityDegraded, report.State)
	require.Equal(t, "KataFirecrackerNotValidated", report.Reason)
}

func TestHostCapabilityProberRequiresFastSandboxCLHCgroupMode(t *testing.T) {
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha1.RuntimeKataClh)
	require.NoError(t, err)

	prober := NewHostCapabilityProber()
	prober.stat = func(string) (os.FileInfo, error) { return fakeFileInfo{}, nil }
	prober.readFile = func(string) ([]byte, error) {
		return []byte("sandbox_cgroup_only = false\n"), nil
	}
	report := prober.Probe(context.Background(), profile, "/run/containerd/containerd.sock")
	require.Equal(t, runtimecatalog.CapabilityDegraded, report.State)
	require.Equal(t, "RuntimeConfigIncompatible", report.Reason)
	require.Contains(t, report.Missing, profile.Containerd.ConfigPath+":sandbox_cgroup_only=true")

	prober.readFile = func(string) ([]byte, error) {
		return []byte("# sandbox_cgroup_only = false\nsandbox_cgroup_only = true\n"), nil
	}
	report = prober.Probe(context.Background(), profile, "/run/containerd/containerd.sock")
	require.Equal(t, runtimecatalog.CapabilityAvailable, report.State)
}

type fakeFileInfo struct{}

func (fakeFileInfo) Name() string       { return "runtime-capability" }
func (fakeFileInfo) Size() int64        { return 0 }
func (fakeFileInfo) Mode() os.FileMode  { return 0 }
func (fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (fakeFileInfo) IsDir() bool        { return false }
func (fakeFileInfo) Sys() any           { return nil }

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
	require.NoError(t, err)
	require.IsType(t, &BoxLiteDriver{}, driver)
}
