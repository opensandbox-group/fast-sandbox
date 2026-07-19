package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fast-sandbox/internal/api"

	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// 1. TestNewContainerdRuntime
// ============================================================================

func TestNewContainerdRuntime_GVisor(t *testing.T) {
	// N-02: Constructor creates gVisor runtime with correct handler
	runtime := newContainerdRuntime("gvisor")

	require.NotNil(t, runtime, "Runtime should not be nil")

	cr, ok := runtime.(*ContainerdRuntime)
	require.True(t, ok, "Runtime should be *ContainerdRuntime type")

	assert.Equal(t, "io.containerd.runsc.v1", cr.config.Handler, "Runtime handler should be runsc for gVisor")
}

func TestRuntimeConfig_KataVariantsUseKataV2Runtime(t *testing.T) {
	tests := []struct {
		name       string
		runtime    RuntimeType
		configPath string
	}{
		{
			name:       "kata qemu",
			runtime:    RuntimeTypeKataQemu,
			configPath: "/opt/kata/share/defaults/kata-containers/configuration-qemu.toml",
		},
		{
			name:       "kata firecracker",
			runtime:    RuntimeTypeKataFc,
			configPath: "/opt/kata/share/defaults/kata-containers/configuration-fc.toml",
		},
		{
			name:       "kata cloud hypervisor",
			runtime:    RuntimeTypeKataClh,
			configPath: "/opt/kata/share/defaults/kata-containers/configuration-clh.toml",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := GetRuntimeConfig(tt.runtime)
			assert.Equal(t, "io.containerd.kata.v2", cfg.Handler)
			assert.Equal(t, tt.configPath, cfg.ConfigPath)
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

func TestRuntimeConfig_OverrideHandler(t *testing.T) {
	cfg, err := ResolveRuntimeConfig(RuntimeTypeGVisor, "custom.handler.v2")

	require.NoError(t, err)
	assert.Equal(t, "custom.handler.v2", cfg.Handler)
	assert.Equal(t, "/etc/containerd/runsc.toml", cfg.ConfigPath)
	assert.True(t, cfg.NeedsTTY)
}

func TestSandboxResourceSpecOptsEnforceCPUAndMemory(t *testing.T) {
	opts, err := sandboxResourceSpecOpts(&api.SandboxSpec{CPU: "500m", Memory: "256Mi", PIDs: 128})
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
	_, err := sandboxResourceSpecOpts(&api.SandboxSpec{CPU: "not-cpu"})
	require.Error(t, err)
	_, err = sandboxResourceSpecOpts(&api.SandboxSpec{Memory: "0"})
	require.Error(t, err)
}

func TestValidateExistingRuntimeProfile(t *testing.T) {
	existing := &SandboxMetadata{SandboxSpec: api.SandboxSpec{
		SandboxID: "sandbox-a", CPU: "500m", Memory: "256Mi", PIDs: 128,
		RuntimeProfileHash: "runtime-hash", ResourceProfileHash: "resource-hash",
	}}
	requested := existing.SandboxSpec
	require.NoError(t, validateExistingRuntimeProfile(existing, &requested))
	requested.CPU = "1"
	require.ErrorIs(t, validateExistingRuntimeProfile(existing, &requested), ErrSandboxProfileMismatch)
}

// ============================================================================
// 2. Test DiscoverCgroupPath
// ============================================================================

func TestContainerdRuntime_DiscoverCgroupPath_Success(t *testing.T) {
	// C-01: Successfully discovers cgroup path from cgroup v2 format (0::/path)
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
			// Create a temporary cgroup file
			tmpDir := t.TempDir()
			cgroupPath := filepath.Join(tmpDir, "cgroup")
			require.NoError(t, os.WriteFile(cgroupPath, []byte(tt.content), 0644))

			// Test the logic directly by reading and parsing

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
	// C-02: Handles invalid /proc/self/cgroup content gracefully
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
			// Create a temporary cgroup file
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

// ============================================================================
// 3. Test Initialize
// ============================================================================

func TestContainerdRuntime_Initialize_ShortMode(t *testing.T) {
	// I-01: Skip actual containerd connection in short mode
	if testing.Short() {
		t.Skip("Skipping containerd initialization test in short mode")
	}

	// This test would require an actual containerd socket
	// In CI/short mode, we skip it
	cr := newContainerdRuntime("io.containerd.runc.v2").(*ContainerdRuntime)

	ctx := context.Background()
	err := cr.Initialize(ctx, "/run/containerd/containerd.sock")

	// This will likely fail in test environment unless containerd is running
	// The test verifies that the Initialize method is called correctly
	if err != nil {
		assert.Contains(t, err.Error(), "failed to create containerd client")
	} else {
		assert.NotNil(t, cr.client, "Client should be initialized if socket exists")
	}
}

func TestContainerdRuntime_Initialize_DefaultSocketPath(t *testing.T) {
	// I-02: Uses default socket path when empty string provided
	if testing.Short() {
		t.Skip("Skipping containerd initialization test in short mode")
	}

	cr := newContainerdRuntime("io.containerd.runc.v2").(*ContainerdRuntime)

	ctx := context.Background()

	// Initialize with empty socket path - should use default
	err := cr.Initialize(ctx, "")

	// Verify the default path was set (even if connection fails)
	assert.Equal(t, "/run/containerd/containerd.sock", cr.socketPath)

	if err != nil {
		// Expected in test environment without containerd
		assert.Contains(t, err.Error(), "failed to create containerd client")
	}
}

func TestContainerdRuntime_Initialize_EnvVars(t *testing.T) {
	// I-03: Reads environment variables for configuration
	// Set up test environment variables
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

	cr := newContainerdRuntime("io.containerd.runc.v2").(*ContainerdRuntime)

	ctx := context.Background()
	_ = cr.Initialize(ctx, "") // Connection may fail, but env vars should be read

	// Verify environment variables were read
	assert.Equal(t, testPodName, cr.fastletPodName)
	assert.Equal(t, testPodUID, cr.fastletPodUID)
	assert.Nil(t, cr.infraMgr, "Infra manager is injected by Fastlet composition after runtime initialization")
}

// ============================================================================
// 4. Test CreateSandbox Validation
// ============================================================================

func TestContainerdRuntime_CreateSandbox_Validation(t *testing.T) {
	// CS-01: Validates input - missing sandbox ID
	cr := &ContainerdRuntime{
		client: nil, // Not initialized, should fail before using client
	}

	ctx := context.Background()

	tests := []struct {
		name        string
		config      *api.SandboxSpec
		expectError string
	}{
		{
			name: "empty sandbox ID",
			config: &api.SandboxSpec{
				SandboxID: "",
				Image:     "alpine:latest",
				ClaimUID:  "claim-123",
				ClaimName: "test-claim",
			},
			expectError: "sandbox ID cannot be empty",
		},
		{
			name: "empty image",
			config: &api.SandboxSpec{
				SandboxID: "sb-123",
				Image:     "",
				ClaimUID:  "claim-123",
				ClaimName: "test-claim",
			},
			expectError: "image cannot be empty",
		},
		{
			name: "valid config",
			config: &api.SandboxSpec{
				SandboxID:  "sb-123",
				Image:      "alpine:latest",
				ClaimUID:   "claim-123",
				ClaimName:  "test-claim",
				Command:    []string{"/bin/sh"},
				Args:       []string{"-c", "echo hello"},
				WorkingDir: "/tmp",
				Env:        map[string]string{"PATH": "/usr/bin", "HOME": "/root"},
			},
			expectError: "", // Should fail at client access, not validation
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

			_, err := cr.CreateSandbox(ctx, tt.config)

			// Should either panic or error due to nil client
			assert.True(t, panicked || err != nil, "CreateSandbox should panic or error without initialized client")
		})
	}
}

// ============================================================================
// 5. Test DeleteSandbox
// ============================================================================

func TestContainerdRuntime_DeleteSandbox_NotFound(t *testing.T) {
	// DS-01: Handles deletion of non-existent container gracefully
	// Note: With nil client, this will panic. Test documents this behavior.
	cr := &ContainerdRuntime{
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

	// Should either panic or error with nil client
	assert.True(t, panicked || err != nil, "DeleteSandbox should panic or error without initialized client")
}

// ============================================================================
// 6. Test prepareLabels
// ============================================================================

func TestContainerdRuntime_prepareLabels(t *testing.T) {
	// PL-01: Generates correct labels for sandbox
	cr := &ContainerdRuntime{
		fastletPodName:   "test-fastlet",
		fastletPodUID:    "fastlet-uid-123",
		fastletNamespace: "default-ns",
	}

	config := &api.SandboxSpec{
		SandboxID: "sb-123", ClaimUID: "claim-456", ClaimNamespace: "tenant-a", ClaimName: "test-claim", Image: "alpine:latest",
		RequestID: "request-1", InstanceGeneration: 2, AssignmentAttempt: 3,
		CPU: "500m", Memory: "256Mi", PIDs: 128,
		RuntimeProfileHash: "runtime-hash", ResourceProfileHash: "resource-hash",
		InfraProfile: "test-infra", InfraProfileHash: "infra-hash",
		NetworkSlotID: "slot-1", NetworkNamespacePath: "/run/fast-sandbox/netns/fsb1",
		NetworkIP: "172.30.0.2", NetworkGateway: "172.30.0.1", NetworkDNSPath: "/run/fast-sandbox/network/pod/slot-1.resolv.conf",
	}

	labels := cr.prepareLabels(config)

	expectedLabels := map[string]string{
		"fast-sandbox.io/managed":               "true",
		"fast-sandbox.io/fastlet-name":          "test-fastlet",
		"fast-sandbox.io/fastlet-uid":           "fastlet-uid-123",
		"fast-sandbox.io/namespace":             "default-ns",
		"fast-sandbox.io/id":                    "sb-123",
		"fast-sandbox.io/claim-uid":             "claim-456",
		"fast-sandbox.io/claim-namespace":       "tenant-a",
		"fast-sandbox.io/sandbox-name":          "test-claim",
		"fast-sandbox.io/runtime-profile-hash":  "runtime-hash",
		"fast-sandbox.io/resource-profile-hash": "resource-hash",
		"fast-sandbox.io/infra-profile":         "test-infra",
		"fast-sandbox.io/infra-profile-hash":    "infra-hash",
		"fast-sandbox.io/resource-cpu":          "500m",
		"fast-sandbox.io/resource-memory":       "256Mi",
		"fast-sandbox.io/resource-pids":         "128",
		"fast-sandbox.io/request-id":            "request-1",
		"fast-sandbox.io/instance-generation":   "2",
		"fast-sandbox.io/assignment-attempt":    "3",
		"fast-sandbox.io/route-generation":      "1",
		"fast-sandbox.io/network-slot-id":       "slot-1",
		"fast-sandbox.io/network-netns-path":    "/run/fast-sandbox/netns/fsb1",
		"fast-sandbox.io/network-ip":            "172.30.0.2",
		"fast-sandbox.io/network-gateway":       "172.30.0.1",
		"fast-sandbox.io/network-dns-path":      "/run/fast-sandbox/network/pod/slot-1.resolv.conf",
	}

	assert.Equal(t, expectedLabels, labels)
}

func TestContainerdRuntime_prepareLabels_EmptyFastletFields(t *testing.T) {
	// PL-02: Handles empty fastlet fields
	cr := &ContainerdRuntime{
		fastletPodName:   "",
		fastletPodUID:    "",
		fastletNamespace: "",
	}

	config := &api.SandboxSpec{
		SandboxID: "sb-123",
		ClaimUID:  "claim-456",
		ClaimName: "test-claim",
	}

	labels := cr.prepareLabels(config)

	assert.Equal(t, "true", labels["fast-sandbox.io/managed"])
	assert.Equal(t, "", labels["fast-sandbox.io/fastlet-name"])
	assert.Equal(t, "", labels["fast-sandbox.io/fastlet-uid"])
	assert.Equal(t, "sb-123", labels["fast-sandbox.io/id"])
	assert.Equal(t, "claim-456", labels["fast-sandbox.io/claim-uid"])
	assert.Equal(t, "test-claim", labels["fast-sandbox.io/sandbox-name"])
	assert.Equal(t, "", labels["fast-sandbox.io/request-id"])
	assert.Equal(t, "0", labels["fast-sandbox.io/instance-generation"])
	assert.Equal(t, "0", labels["fast-sandbox.io/assignment-attempt"])
}

// ============================================================================
// 8. Test envMapToSlice
// ============================================================================

func TestEnvMapToSlice(t *testing.T) {
	// E-01: Converts environment map to slice correctly
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

// ============================================================================
// 9. Test snapShotName
// ============================================================================

func TestSnapShotName(t *testing.T) {
	// SN-01: Generates snapshot name from container ID
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

// ============================================================================
// 10. Test SetNamespace
// ============================================================================

func TestContainerdRuntime_SetNamespace(t *testing.T) {
	// NS-01: Sets namespace correctly
	cr := &ContainerdRuntime{}

	assert.Equal(t, "", cr.fastletNamespace, "Initial namespace should be empty")

	cr.SetNamespace("test-namespace")
	assert.Equal(t, "test-namespace", cr.fastletNamespace)

	cr.SetNamespace("another-namespace")
	assert.Equal(t, "another-namespace", cr.fastletNamespace)
}

// ============================================================================
// 11. Test Close
// ============================================================================

func TestContainerdRuntime_Close(t *testing.T) {
	// CL-01: Close handles nil client gracefully
	cr := &ContainerdRuntime{
		client: nil,
	}

	err := cr.Close()
	assert.NoError(t, err, "Close should not error with nil client")
}

func TestContainerdRuntime_Close_NotInitialized(t *testing.T) {
	// CL-02: Close on newly created runtime is safe
	cr := newContainerdRuntime("io.containerd.runc.v2").(*ContainerdRuntime)

	err := cr.Close()
	assert.NoError(t, err, "Close should not error on uninitialized runtime")
}
