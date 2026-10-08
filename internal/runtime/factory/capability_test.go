package factory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	"fast-sandbox/internal/runtime/containerd"
	firecrackerdriver "fast-sandbox/internal/runtime/firecracker"
)

func TestHostCapabilityProberContainerAvailable(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "containerd.sock")
	require.NoError(t, os.WriteFile(socketPath, nil, 0o600))
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeContainer)
	require.NoError(t, err)

	report := NewHostCapabilityProber().Probe(context.Background(), profile, socketPath)
	require.Equal(t, runtimecatalog.CapabilityAvailable, report.State)
	require.Empty(t, report.Missing)
}

func TestHostCapabilityProberFailsClosed(t *testing.T) {
	catalog := runtimecatalog.Builtin()
	kata, err := catalog.Resolve(apiv1alpha2.RuntimeKataQemu)
	require.NoError(t, err)
	prober := NewHostCapabilityProber()
	prober.stat = func(path string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	report := prober.Probe(context.Background(), kata, "/missing/containerd.sock")
	require.Equal(t, runtimecatalog.CapabilityDegraded, report.State)
	require.Equal(t, "KVMUnavailable", report.Reason)
	require.Contains(t, report.Missing, "/dev/kvm")
	require.Contains(t, report.Missing, kata.Containerd.ConfigPath)
}

func TestHostCapabilityProberAcceptsConfiguredFirecrackerProfile(t *testing.T) {
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeKataFc)
	require.NoError(t, err)
	prober := NewHostCapabilityProber()
	prober.stat = func(string) (os.FileInfo, error) { return fakeFileInfo{}, nil }
	report := prober.Probe(context.Background(), profile, "/run/containerd/containerd.sock")
	require.Equal(t, runtimecatalog.CapabilityAvailable, report.State)
	require.Empty(t, report.Missing)
}

func TestHostCapabilityProberFirecrackerDriverGate(t *testing.T) {
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeFirecracker)
	require.NoError(t, err)
	// The builtin firecracker profile is production-configured and needs
	// no node-side kernel (#84).
	require.Equal(t, runtimecatalog.CapabilityConfigured, profile.Capabilities.DefaultState)
	require.Empty(t, profile.Firecracker.KernelPath)
	prober := NewHostCapabilityProber()
	prober.stat = func(string) (os.FileInfo, error) { return fakeFileInfo{}, nil }
	report := prober.Probe(context.Background(), profile, "")
	require.Equal(t, runtimecatalog.CapabilityAvailable, report.State)
	require.Empty(t, report.Missing)

	// An operator-pinned kernelPath (direct boot) is probed again.
	prober.stat = func(path string) (os.FileInfo, error) {
		if path == "/missing/vmlinux.bin" {
			return nil, os.ErrNotExist
		}
		return fakeFileInfo{}, nil
	}
	profile.Firecracker.KernelPath = "/missing/vmlinux.bin"
	report = prober.Probe(context.Background(), profile, "")
	require.Equal(t, runtimecatalog.CapabilityDegraded, report.State)
	require.Equal(t, "RuntimeKernelUnavailable", report.Reason)
	require.Contains(t, report.Missing, "/missing/vmlinux.bin")
	profile.Firecracker.KernelPath = ""

	profile.Firecracker.BootTimeoutSeconds = 0
	report = prober.Probe(context.Background(), profile, "")
	require.Equal(t, runtimecatalog.CapabilityDegraded, report.State)
	require.Equal(t, "RuntimeProfileInvalid", report.Reason)
}

func TestHostCapabilityProberRequiresFastSandboxCLHCgroupMode(t *testing.T) {
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeKataClh)
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
	container, err := catalog.Resolve(apiv1alpha2.RuntimeContainer)
	require.NoError(t, err)
	driver, err := buildDriver(container)
	require.NoError(t, err)
	require.IsType(t, &containerd.Driver{}, driver)
	firecracker, err := catalog.Resolve(apiv1alpha2.RuntimeFirecracker)
	require.NoError(t, err)
	driver, err = buildDriver(firecracker)
	require.NoError(t, err)
	require.IsType(t, &firecrackerdriver.Driver{}, driver)
}
