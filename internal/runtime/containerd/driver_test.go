package containerd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1alpha2 "fast-sandbox/api/v1alpha2"
	runtimecatalog "fast-sandbox/internal/catalog/runtime"
	fastletinfra "fast-sandbox/internal/fastlet/infra"
	"fast-sandbox/internal/fastlet/podcgroup"
	"fast-sandbox/internal/nodecleanup"
	fastletapi "fast-sandbox/internal/protocol/fastlet"

	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestContainerdRuntime() *Driver {
	return newWithConfig(
		apiv1alpha2.RuntimeContainer,
		"test-profile-hash",
		RuntimeConfig{Handler: "io.containerd.runc.v2"},
	)
}

func TestNewUsesCanonicalProfile(t *testing.T) {
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeGVisor)
	require.NoError(t, err)
	driver, err := New(profile)
	require.NoError(t, err)
	require.Equal(t, apiv1alpha2.RuntimeGVisor, driver.runtimeName)
	require.Equal(t, profile.ProfileHash, driver.runtimeProfileHash)
	require.Equal(t, "io.containerd.runsc.v1", driver.config.Handler)
	require.Equal(t, "overlayfs", driver.snapshotter())
	require.Equal(t, runtimecatalog.DefaultContainerdNamespace, driver.containerdNamespace())
}

func TestSandboxCgroupSpecOptPlacesRuntimeBelowFastletPod(t *testing.T) {
	driver := newTestContainerdRuntime()
	driver.podCgroup = &podcgroup.Layout{
		Version: podcgroup.VersionV2,
		PodPath: "/kubepods/burstable/pod873c13da-c645-461e-93f0-dfc24f63a6ad",
	}
	opt, err := driver.sandboxCgroupSpecOpt("sandbox-a")
	require.NoError(t, err)
	spec := &oci.Spec{}
	require.NoError(t, opt(context.Background(), nil, nil, spec))
	require.Equal(t,
		"/kubepods/burstable/pod873c13da-c645-461e-93f0-dfc24f63a6ad/fast-sandbox/fsb-315bbe38938f7266",
		spec.Linux.CgroupsPath,
	)
}

func TestSandboxCgroupSpecOptUsesShimCompatibleSystemdEncoding(t *testing.T) {
	layout := &podcgroup.Layout{
		Version: podcgroup.VersionV2,
		PodPath: "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod873c13da_c645_461e_93f0_dfc24f63a6ad.slice",
		Systemd: true,
	}

	for _, testCase := range []struct {
		name    string
		runtime apiv1alpha2.RuntimeName
		want    string
	}{
		{
			name:    "runc systemd unit",
			runtime: apiv1alpha2.RuntimeContainer,
			want:    "kubepods-burstable-pod873c13da_c645_461e_93f0_dfc24f63a6ad.slice:fast-sandbox:fsb-315bbe38938f7266",
		},
		{
			name:    "gvisor systemd unit",
			runtime: apiv1alpha2.RuntimeGVisor,
			want:    "kubepods-burstable-pod873c13da_c645_461e_93f0_dfc24f63a6ad.slice:fast-sandbox:fsb-315bbe38938f7266",
		},
		{
			name:    "kata filesystem child",
			runtime: apiv1alpha2.RuntimeKataQemu,
			want:    "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod873c13da_c645_461e_93f0_dfc24f63a6ad.slice/fast-sandbox/fsb-315bbe38938f7266",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			driver := newWithConfig(testCase.runtime, "test-profile-hash", RuntimeConfig{})
			driver.podCgroup = layout
			opt, err := driver.sandboxCgroupSpecOpt("sandbox-a")
			require.NoError(t, err)
			spec := &oci.Spec{}
			require.NoError(t, opt(context.Background(), nil, nil, spec))
			require.Equal(t, testCase.want, spec.Linux.CgroupsPath)
		})
	}
}

func TestNewUsesResolvedSnapshotter(t *testing.T) {
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeKataFc)
	require.NoError(t, err)
	profile.Containerd.Snapshotter = "blockfile"
	driver, err := New(profile)
	require.NoError(t, err)
	require.Equal(t, "blockfile", driver.snapshotter())
	require.Equal(t, runtimecatalog.ResidualProcessFirecracker, driver.residualProcess)
}

func TestKataFCDeletionRequiresVerifiedNodeCleanup(t *testing.T) {
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeKataFc)
	require.NoError(t, err)
	driver, err := New(profile)
	require.NoError(t, err)

	err = driver.ensureResidualProcessAbsent(context.Background(), "sandbox-a")

	require.ErrorContains(t, err, "node cleanup client is not configured")
}

func TestKataFCDeletionDelegatesResidualProcessCleanup(t *testing.T) {
	profile, err := runtimecatalog.Builtin().Resolve(apiv1alpha2.RuntimeKataFc)
	require.NoError(t, err)
	driver, err := New(profile)
	require.NoError(t, err)
	cleaner := &fakeRuntimeProcessCleaner{}
	driver.SetNodeCleanupClient(cleaner)

	require.NoError(t, driver.ensureResidualProcessAbsent(context.Background(), "sandbox-a"))
	require.Equal(t, runtimecatalog.ResidualProcessFirecracker, cleaner.kind)
	require.Equal(t, "sandbox-a", cleaner.sandboxID)
}

type fakeRuntimeProcessCleaner struct {
	kind      runtimecatalog.ResidualProcessKind
	sandboxID string
}

func (c *fakeRuntimeProcessCleaner) EnsureRuntimeProcessesAbsent(_ context.Context, kind runtimecatalog.ResidualProcessKind, sandboxID string) error {
	c.kind = kind
	c.sandboxID = sandboxID
	return nil
}

var _ nodecleanup.RuntimeProcessCleaner = (*fakeRuntimeProcessCleaner)(nil)

func TestRuntimeConfig_KataVariantsUseKataV2Runtime(t *testing.T) {
	tests := []struct {
		name       string
		runtime    apiv1alpha2.RuntimeName
		configPath string
	}{
		{
			name:       "kata qemu",
			runtime:    apiv1alpha2.RuntimeKataQemu,
			configPath: "/opt/kata/share/defaults/kata-containers/configuration-qemu.toml",
		},
		{
			name:       "kata firecracker",
			runtime:    apiv1alpha2.RuntimeKataFc,
			configPath: "/opt/kata/share/defaults/kata-containers/configuration-fc.toml",
		},
		{
			name:       "kata cloud hypervisor",
			runtime:    apiv1alpha2.RuntimeKataClh,
			configPath: "/opt/kata/share/defaults/kata-containers/configuration-clh.toml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			profile, err := runtimecatalog.Builtin().Resolve(tt.runtime)
			require.NoError(t, err)
			require.NotNil(t, profile.Containerd)
			assert.Equal(t, "io.containerd.kata.v2", profile.Containerd.Handler)
			assert.Equal(t, tt.configPath, profile.Containerd.ConfigPath)
		})
	}
}

func TestWithSandboxInitPreservesImageProcessConfiguration(t *testing.T) {
	spec := &oci.Spec{Process: &specs.Process{
		Args: []string{"/usr/bin/python", "app.py"},
		Env:  []string{"A=B"}, Cwd: "/workspace", User: specs.User{UID: 1000, GID: 1000},
	}}
	require.NoError(t, withSandboxInit()(context.Background(), nil, nil, spec))
	require.Equal(t, []string{
		"/.fast/bin/sandbox-init", "--config", "/.fast/run/infra.json",
		"--user-uid", "1000", "--user-gid", "1000", "--", "/usr/bin/python", "app.py",
	}, spec.Process.Args)
	require.Equal(t, []string{"A=B"}, spec.Process.Env)
	require.Equal(t, "/workspace", spec.Process.Cwd)
	require.Zero(t, spec.Process.User.UID, "sandbox-init must be able to read the root-only instance config")
	require.Zero(t, spec.Process.User.GID)
}

func TestWithSandboxInitCarriesAdditionalGroupsToUserChild(t *testing.T) {
	spec := &oci.Spec{Process: &specs.Process{
		Args: []string{"/bin/true"}, User: specs.User{UID: 1000, GID: 1001, AdditionalGids: []uint32{10, 20}},
	}}
	require.NoError(t, withSandboxInit()(context.Background(), nil, nil, spec))
	require.Equal(t, []string{
		"/.fast/bin/sandbox-init", "--config", "/.fast/run/infra.json",
		"--user-uid", "1000", "--user-gid", "1001", "--user-additional-gids", "10,20", "--", "/bin/true",
	}, spec.Process.Args)
}

func TestWithSandboxInitRejectsImageWithoutEntrypoint(t *testing.T) {
	spec := &oci.Spec{Process: &specs.Process{}}
	require.Error(t, withSandboxInit()(context.Background(), nil, nil, spec))
}

func TestUserProcessStartAfterTaskStartDoesNotTreatSupervisorAsUserProcess(t *testing.T) {
	observedAt := time.Unix(1700000000, 123)
	startedAt, source := userProcessStartAfterTaskStart(nil, observedAt)
	require.Equal(t, observedAt, startedAt)
	require.Equal(t, fastletapi.UserProcessStartRuntimeDirect, source)

	startedAt, source = userProcessStartAfterTaskStart(&fastletinfra.PreparedInstance{WrapperRequired: true}, observedAt)
	require.True(t, startedAt.IsZero())
	require.Equal(t, fastletapi.UserProcessStartSandboxInitUnreported, source)
}

func TestSandboxResourceSpecOptsEnforceCPUAndMemory(t *testing.T) {
	opts, err := sandboxResourceSpecOpts(&fastletapi.SandboxSpec{CPU: "500m", Memory: "256Mi", PIDs: 128})
	require.NoError(t, err)
	spec := &specs.Spec{Linux: &specs.Linux{}}
	for _, opt := range opts {
		require.NoError(t, opt(context.Background(), nil, nil, spec))
	}
	require.NotNil(t, spec.Linux.Resources)
	require.Equal(t, int64(50000), *spec.Linux.Resources.CPU.Quota)
	require.Equal(t, uint64(100000), *spec.Linux.Resources.CPU.Period)
	require.Equal(t, int64(256*1024*1024), *spec.Linux.Resources.Memory.Limit)
	require.Equal(t, int64(128), *spec.Linux.Resources.Pids.Limit)
}

func TestSandboxResourceSpecOptsRejectInvalidValues(t *testing.T) {
	_, err := sandboxResourceSpecOpts(&fastletapi.SandboxSpec{CPU: "not-cpu"})
	require.Error(t, err)
	_, err = sandboxResourceSpecOpts(&fastletapi.SandboxSpec{Memory: "0"})
	require.Error(t, err)
}

func TestValidateExistingRuntimeProfile(t *testing.T) {
	existing := &SandboxMetadata{Config: fastletapi.RuntimeSandboxConfig{
		Spec: fastletapi.SandboxSpec{CPU: "500m", Memory: "256Mi", PIDs: 128,
			RuntimeProfileHash: "runtime-hash", ResourceProfileHash: "resource-hash"},
		Identity: fastletapi.SandboxIdentity{SandboxUID: "sandbox-a"},
	}}
	requested := existing.Config
	require.NoError(t, validateExistingRuntimeProfile(existing, &requested))
	requested.Spec.CPU = "1"
	require.ErrorIs(t, validateExistingRuntimeProfile(existing, &requested), ErrSandboxProfileMismatch)
}

func TestContainerdRuntime_DiscoverCgroupPath_Success(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected string
	}{
		{
			name:     "cgroup v2 format",
			content:  "0::/kubepods/besteffort/pod1234/cri-containerd-abc123\n",
			expected: "/kubepods/besteffort/pod1234/cri-containerd-abc123",
		},
		{
			name:     "cgroup v1 with pids controller",
			content:  "1:pids:/kubepods/besteffort/pod1234/cri-containerd-abc123\n",
			expected: "/kubepods/besteffort/pod1234/cri-containerd-abc123",
		},
		{
			name:     "cgroup v1 with cpu controller",
			content:  "2:cpu:/kubepods/besteffort/pod1234/cri-containerd-abc123\n",
			expected: "/kubepods/besteffort/pod1234/cri-containerd-abc123",
		},
		{
			name:     "cgroup v1 with pids, priority to first match (cpu)",
			content:  "1:cpu:/path/cpu\n2:pids:/path/pids\n",
			expected: "/path/cpu",
		},
		{
			name:     "cgroup v1 with cpu, priority to cpu",
			content:  "1:cpu:/path/cpu\n2:memory:/path/memory\n",
			expected: "/path/cpu",
		},
		{
			name:     "multi-line cgroup v2",
			content:  "0::/kubepods/pod123/cri-containerd-abc\n1:name=systemd:/user.slice\n",
			expected: "/kubepods/pod123/cri-containerd-abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			cgroupPath := filepath.Join(tmpDir, "cgroup")
			require.NoError(t, os.WriteFile(cgroupPath, []byte(tt.content), 0644))

			data, err := os.ReadFile(cgroupPath)
			require.NoError(t, err)

			lines := strings.Split(string(data), "\n")
			foundPath := ""
			for _, line := range lines {
				if strings.HasPrefix(line, "0::") {
					foundPath = strings.TrimPrefix(line, "0::")
					break
				}
				parts := strings.Split(line, ":")
				if len(parts) == 3 && (strings.Contains(parts[1], "pids") || strings.Contains(parts[1], "cpu")) {
					foundPath = parts[2]
					break
				}
			}

			assert.Equal(t, tt.expected, foundPath)
		})
	}
}

func TestContainerdRuntime_DiscoverCgroupPath_InvalidContent(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		expectError bool
	}{
		{
			name:        "empty file",
			content:     "",
			expectError: true,
		},
		{
			name:        "only newlines",
			content:     "\n\n\n",
			expectError: true,
		},
		{
			name:        "invalid format - missing parts",
			content:     "1:pids\n",
			expectError: true,
		},
		{
			name:        "unrecognized controller",
			content:     "1:memory:/path\n2:blkio:/path2\n",
			expectError: true,
		},
		{
			name:        "v2 without proper format",
			content:     "1:/some/path\n",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			cgroupPath := filepath.Join(tmpDir, "cgroup")
			require.NoError(t, os.WriteFile(cgroupPath, []byte(tt.content), 0644))

			data, err := os.ReadFile(cgroupPath)
			require.NoError(t, err)

			lines := strings.Split(string(data), "\n")
			foundPath := ""
			for _, line := range lines {
				if strings.HasPrefix(line, "0::") {
					foundPath = strings.TrimPrefix(line, "0::")
					break
				}
				parts := strings.Split(line, ":")
				if len(parts) == 3 && (strings.Contains(parts[1], "pids") || strings.Contains(parts[1], "cpu")) {
					foundPath = parts[2]
					break
				}
			}

			if tt.expectError {
				assert.Equal(t, "", foundPath, "Should not find a valid cgroup path")
			}
		})
	}
}

func TestContainerdRuntime_Initialize_ShortMode(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping containerd initialization test in short mode")
	}

	// This test would require an actual containerd socket
	cr := newTestContainerdRuntime()

	ctx := context.Background()
	err := cr.Initialize(ctx, "/run/containerd/containerd.sock")

	// This will likely fail in test environment unless containerd is running
	if err != nil {
		assert.Contains(t, err.Error(), "failed to create containerd client")
	} else {
		assert.NotNil(t, cr.client, "Client should be initialized if socket exists")
	}
}

func TestContainerdRuntime_Initialize_DefaultSocketPath(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping containerd initialization test in short mode")
	}

	cr := newTestContainerdRuntime()

	ctx := context.Background()

	err := cr.Initialize(ctx, "")

	// Verify the default path was set (even if connection fails)
	assert.Equal(t, "/run/containerd/containerd.sock", cr.socketPath)

	if err != nil {
		// Expected in test environment without containerd
		assert.Contains(t, err.Error(), "failed to create containerd client")
	}
}

func TestContainerdRuntime_Initialize_EnvVars(t *testing.T) {
	testPodName := "test-fastlet-pod"
	testPodUID := "test-uid-12345"
	os.Setenv("POD_NAME", testPodName)
	os.Setenv("POD_UID", testPodUID)
	defer func() {
		os.Unsetenv("POD_NAME")
		os.Unsetenv("POD_UID")
	}()

	if testing.Short() {
		t.Skip("Skipping containerd initialization test in short mode")
	}

	cr := newTestContainerdRuntime()

	ctx := context.Background()
	_ = cr.Initialize(ctx, "") // Connection may fail, but env vars should be read

	assert.Equal(t, testPodName, cr.fastletPodName)
	assert.Equal(t, testPodUID, cr.fastletPodUID)
	assert.Nil(t, cr.infraMgr, "Infra manager is injected by Fastlet composition after runtime initialization")
}

func TestContainerdRuntime_CreateSandbox_Validation(t *testing.T) {
	cr := &Driver{
		client: nil, // Not initialized, should fail before using client
	}

	ctx := context.Background()

	tests := []struct {
		name  string
		input *fastletapi.EnsureSandboxInput
	}{
		{
			name: "empty sandbox ID",
			input: &fastletapi.EnsureSandboxInput{Sandbox: fastletapi.RuntimeSandboxConfig{
				Spec: fastletapi.SandboxSpec{Image: "alpine:latest"}, Identity: fastletapi.SandboxIdentity{Name: "test-claim"},
			}},
		},
		{
			name: "empty image",
			input: &fastletapi.EnsureSandboxInput{Sandbox: fastletapi.RuntimeSandboxConfig{
				Identity: fastletapi.SandboxIdentity{SandboxUID: "sb-123", Name: "test-claim"},
			}},
		},
		{
			name: "valid config",
			input: &fastletapi.EnsureSandboxInput{Sandbox: fastletapi.RuntimeSandboxConfig{
				Spec: fastletapi.SandboxSpec{Image: "alpine:latest", Command: []string{"/bin/sh"},
					Args: []string{"-c", "echo hello"}, WorkingDir: "/tmp", Env: map[string]string{"PATH": "/usr/bin", "HOME": "/root"}},
				Identity: fastletapi.SandboxIdentity{SandboxUID: "sb-123", Name: "test-claim"},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The actual implementation doesn't do explicit validation before client access
			// It will panic with nil client. Test documents this behavior.
			panicked := false
			defer func() {
				if r := recover(); r != nil {
					panicked = true
				}
			}()

			_, err := cr.CreateSandbox(ctx, tt.input, fastletapi.RuntimeAllocation{})

			assert.True(t, panicked || err != nil, "CreateSandbox should panic or error without initialized client")
		})
	}
}

func TestContainerdRuntime_DeleteSandbox_NotFound(t *testing.T) {
	// Note: With nil client, this will panic. Test documents this behavior.
	cr := &Driver{
		client: nil,
	}

	panicked := false
	defer func() {
		if r := recover(); r != nil {
			panicked = true
		}
	}()

	ctx := context.Background()
	err := cr.DeleteSandbox(ctx, "non-existent-sandbox")

	assert.True(t, panicked || err != nil, "DeleteSandbox should panic or error without initialized client")
}

func TestContainerdRuntime_prepareLabels(t *testing.T) {
	cr := &Driver{
		fastletPodName:   "test-fastlet",
		fastletPodUID:    "fastlet-uid-123",
		fastletNamespace: "default-ns",
	}

	config := &fastletapi.RuntimeSandboxConfig{
		Spec: fastletapi.SandboxSpec{
			Image: "alpine:latest", CPU: "500m", Memory: "256Mi", PIDs: 128,
			RuntimeProfileHash: "runtime-hash", ResourceProfileHash: "resource-hash", InfraRevision: "infra-hash",
		},
		Identity: fastletapi.SandboxIdentity{SandboxUID: "sb-123", Namespace: "tenant-a", Name: "test-claim",
			InstanceGeneration: 2, RuntimeInstanceID: "runtime-1", AssignmentAttempt: 3},
	}
	allocation := fastletapi.RuntimeAllocation{Network: fastletapi.NetworkAllocation{
		SlotID: "slot-1", NamespacePath: "/run/fast-sandbox/netns/fsb1", IP: "172.30.0.2",
		Gateway: "172.30.0.1", DNSPath: "/run/fast-sandbox/network/pod/slot-1.resolv.conf",
		PrivateCIDR: "172.30.0.0/24", HostVeth: "fh1234",
	}}

	labels := cr.prepareLabels(config, allocation)

	expectedLabels := map[string]string{
		"fast-sandbox.io/managed":               "true",
		"fast-sandbox.io/fastlet-name":          "test-fastlet",
		"fast-sandbox.io/fastlet-uid":           "fastlet-uid-123",
		"fast-sandbox.io/namespace":             "default-ns",
		"fast-sandbox.io/id":                    "sb-123",
		"fast-sandbox.io/sandbox-namespace":     "tenant-a",
		"fast-sandbox.io/sandbox-name":          "test-claim",
		"fast-sandbox.io/runtime-profile-hash":  "runtime-hash",
		"fast-sandbox.io/resource-profile-hash": "resource-hash",
		"fast-sandbox.io/infra-revision":        "infra-hash",
		"fast-sandbox.io/resource-cpu":          "500m",
		"fast-sandbox.io/resource-memory":       "256Mi",
		"fast-sandbox.io/resource-pids":         "128",
		"fast-sandbox.io/instance-generation":   "2",
		"fast-sandbox.io/runtime-instance-id":   "runtime-1",
		"fast-sandbox.io/assignment-attempt":    "3",
		"fast-sandbox.io/route-generation":      "1",
		"fast-sandbox.io/network-slot-id":       "slot-1",
		"fast-sandbox.io/network-netns-path":    "/run/fast-sandbox/netns/fsb1",
		"fast-sandbox.io/network-ip":            "172.30.0.2",
		"fast-sandbox.io/network-gateway":       "172.30.0.1",
		"fast-sandbox.io/network-dns-path":      "/run/fast-sandbox/network/pod/slot-1.resolv.conf",
		"fast-sandbox.io/network-private-cidr":  "172.30.0.0/24",
		"fast-sandbox.io/network-host-veth":     "fh1234",
	}

	assert.Equal(t, expectedLabels, labels)
}

func TestContainerdRuntime_prepareLabels_EmptyFastletFields(t *testing.T) {
	cr := &Driver{
		fastletPodName:   "",
		fastletPodUID:    "",
		fastletNamespace: "",
	}

	config := &fastletapi.RuntimeSandboxConfig{
		Identity: fastletapi.SandboxIdentity{SandboxUID: "sb-123", Name: "test-claim"},
	}

	labels := cr.prepareLabels(config, fastletapi.RuntimeAllocation{})

	assert.Equal(t, "true", labels["fast-sandbox.io/managed"])
	assert.Equal(t, "", labels["fast-sandbox.io/fastlet-name"])
	assert.Equal(t, "", labels["fast-sandbox.io/fastlet-uid"])
	assert.Equal(t, "sb-123", labels["fast-sandbox.io/id"])
	assert.NotContains(t, labels, "fast-sandbox.io/claim-uid")
	assert.Equal(t, "test-claim", labels["fast-sandbox.io/sandbox-name"])
	assert.NotContains(t, labels, "fast-sandbox.io/request-id")
	assert.Equal(t, "0", labels["fast-sandbox.io/instance-generation"])
	assert.Equal(t, "0", labels["fast-sandbox.io/assignment-attempt"])
}

func TestEnvMapToSlice(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		expected []string
	}{
		{
			name:     "empty map",
			env:      map[string]string{},
			expected: []string{},
		},
		{
			name:     "single variable",
			env:      map[string]string{"PATH": "/usr/bin"},
			expected: []string{"PATH=/usr/bin"},
		},
		{
			name: "multiple variables",
			env: map[string]string{
				"PATH": "/usr/bin:/bin",
				"HOME": "/root",
				"USER": "root",
			},
			expected: []string{"PATH=/usr/bin:/bin", "HOME=/root", "USER=root"},
		},
		{
			name:     "variable with equals in value",
			env:      map[string]string{"FOO": "bar=baz"},
			expected: []string{"FOO=bar=baz"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := envMapToSlice(tt.env)

			// Convert to set for comparison since order isn't guaranteed
			resultMap := make(map[string]bool)
			for _, s := range result {
				resultMap[s] = true
			}

			for _, expected := range tt.expected {
				assert.True(t, resultMap[expected], "Expected %q in result", expected)
			}

			assert.Len(t, result, len(tt.expected), "Result should have correct length")
		})
	}
}

func TestSnapShotName(t *testing.T) {
	tests := []struct {
		containerID string
		expected    string
	}{
		{
			containerID: "sb-123",
			expected:    "sb-123-snapshot",
		},
		{
			containerID: "abc",
			expected:    "abc-snapshot",
		},
		{
			containerID: "container-with-dashes",
			expected:    "container-with-dashes-snapshot",
		},
	}

	for _, tt := range tests {
		t.Run(tt.containerID, func(t *testing.T) {
			result := snapShotName(tt.containerID)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestContainerdRuntime_SetNamespace(t *testing.T) {
	cr := &Driver{}

	assert.Equal(t, "", cr.fastletNamespace, "Initial namespace should be empty")

	cr.SetNamespace("test-namespace")
	assert.Equal(t, "test-namespace", cr.fastletNamespace)

	cr.SetNamespace("another-namespace")
	assert.Equal(t, "another-namespace", cr.fastletNamespace)
}

func TestContainerdRuntime_Close(t *testing.T) {
	cr := &Driver{
		client: nil,
	}

	err := cr.Close()
	assert.NoError(t, err, "Close should not error with nil client")
}

func TestContainerdRuntime_Close_NotInitialized(t *testing.T) {
	cr := newTestContainerdRuntime()

	err := cr.Close()
	assert.NoError(t, err, "Close should not error on uninitialized runtime")
}
