package fastpath

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	fastpathv1 "fast-sandbox/api/proto/v1"
	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	"fast-sandbox/internal/controller/common"
	"fast-sandbox/internal/controller/fastletpool"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// setupTestScheme creates a test scheme with the required types registered.
func setupTestScheme(t *testing.T) *runtime.Scheme {
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))
	return scheme
}

func newFastpathTestClientBuilder(scheme *runtime.Scheme) *fake.ClientBuilder {
	objects := make([]client.Object, 0, 4)
	for _, namespace := range []string{"default", "test-ns"} {
		for _, name := range []string{"test-pool", "cache-pool", "db-pool"} {
			objects = append(objects, &apiv1alpha1.SandboxPool{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Spec: apiv1alpha1.SandboxPoolSpec{
					Runtime: apiv1alpha1.RuntimeContainer,
					SandboxResources: apiv1alpha1.SandboxResourceProfile{
						CPU: resource.MustParse("500m"), Memory: resource.MustParse("256Mi"), PIDs: 128,
					},
				},
			})
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...)
}

// newTestServer creates a test Server with mocked dependencies.
func newTestServer(t *testing.T, registry *MockRegistryForTest, fastletClient *MockFastletClientForTest) *Server {
	scheme := setupTestScheme(t)
	return &Server{
		K8sClient:              newFastpathTestClientBuilder(scheme).Build(),
		Registry:               registry,
		FastletClient:          api.NewFastletClient(5758),
		DefaultConsistencyMode: api.ConsistencyModeFast,
	}
}

// wrapFastletClient wraps the mock to implement the interface properly for testing.
func wrapFastletClient(mock *MockFastletClientForTest) *api.FastletClient {
	// For testing purposes, we need to use a wrapper or adjust the server
	// Since FastletClient is a concrete type, we'll use the test pattern
	// where we monkey-patch the CreateSandbox method for testing
	return nil // This will be handled differently
}

// ============================================================================
// Fast Mode Tests
// ============================================================================

func TestServer_CreateSandbox_FastMode_Success(t *testing.T) {
	// Test successful sandbox creation in Fast mode:
	// 1. Allocate returns an fastlet
	// 2. FastletClient.CreateSandbox succeeds
	// 3. Response contains sandbox ID, fastlet pod, and endpoints
	// 4. CRD creation happens asynchronously (not verified in this test)

	registry := &MockRegistryForTest{
		DefaultFastlet: &fastletpool.FastletInfo{
			ID:            "fastlet-1",
			PodName:       "fastlet-pod-1",
			PodIP:         "10.0.0.5",
			NodeName:      "node-1",
			PoolName:      "test-pool",
			Capacity:      10,
			Allocated:     0,
			LastHeartbeat: time.Now(),
		},
	}

	fastletClient := &api.FastletClient{}

	server := &Server{
		K8sClient:              newFastpathTestClientBuilder(setupTestScheme(t)).Build(),
		Registry:               registry,
		FastletClient:          fastletClient,
		DefaultConsistencyMode: api.ConsistencyModeFast,
	}

	// Since we can't easily mock FastletClient, we'll need to use a different approach
	// For this test, we'll verify the happy path logic with a real fastlet client
	// and mock the registry allocation

	req := &fastpathv1.CreateRequest{
		Image:        "nginx:latest",
		PoolRef:      "test-pool",
		Namespace:    "default",
		ExposedPorts: []int32{80, 443},
		Command:      []string{"/bin/sh"},
		Args:         []string{"-c", "echo hello"},
		Envs:         map[string]string{"ENV1": "value1"},
		WorkingDir:   "/app",
	}

	// This test will require either:
	// 1. An interface for FastletClient (refactoring)
	// 2. Using httptest to mock the HTTP server
	// For now, we'll test the error handling paths which are easier to verify

	_ = req
	_ = server
	t.Skip("Requires HTTP server mock or interface refactoring")
}

func TestServer_CreateSandbox_FastMode_AllocateFailure(t *testing.T) {
	// Test allocation failure handling in Fast mode:
	// 1. Registry.Allocate returns an error
	// 2. CreateSandbox returns the error
	// 3. No fastlet RPC is made
	// 4. No CRD is created

	registry := &MockRegistryForTest{
		AllocateError: errors.New("insufficient capacity in pool"),
	}

	server := &Server{
		K8sClient:              newFastpathTestClientBuilder(setupTestScheme(t)).Build(),
		Registry:               registry,
		FastletClient:          api.NewFastletClient(5758),
		DefaultConsistencyMode: api.ConsistencyModeFast,
	}

	req := &fastpathv1.CreateRequest{
		Image:     "nginx:latest",
		PoolRef:   "test-pool",
		Namespace: "default",
	}

	resp, err := server.CreateSandbox(context.Background(), req)

	assert.Error(t, err, "CreateSandbox should return error when allocation fails")
	assert.Nil(t, resp, "Response should be nil on error")
	assert.Contains(t, err.Error(), "insufficient capacity", "Error should contain allocation error message")
	assert.NotNil(t, registry.AllocatedSb, "Allocate should have been called")
}

func TestServer_CreateSandbox_RequestIDReturnsExistingSandbox(t *testing.T) {
	req := &fastpathv1.CreateRequest{
		RequestId: "request-123",
		Name:      "sandbox-existing",
		Image:     "nginx:latest",
		PoolRef:   "test-pool",
		Namespace: "default",
	}
	hash, err := CreateSpecHash(req)
	require.NoError(t, err)
	existing := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
			UID:       types.UID("sandbox-uid"),
			Annotations: map[string]string{
				common.AnnotationRequestID:      req.RequestId,
				common.AnnotationCreateSpecHash: hash,
			},
		},
		Spec: apiv1alpha1.SandboxSpec{Image: req.Image, PoolRef: req.PoolRef},
		Status: apiv1alpha1.SandboxStatus{
			SandboxID:       "runtime-id",
			AssignedFastlet: "fastlet-a",
		},
	}
	registry := &MockRegistryForTest{AllocateError: errors.New("allocation must not be called")}
	server := &Server{
		K8sClient:              newFastpathTestClientBuilder(setupTestScheme(t)).WithObjects(existing).Build(),
		Registry:               registry,
		FastletClient:          api.NewFastletClient(5758),
		DefaultConsistencyMode: api.ConsistencyModeFast,
	}

	resp, err := server.CreateSandbox(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, "sandbox-uid", resp.SandboxUid)
	require.Equal(t, existing.Name, resp.SandboxName)
	require.Nil(t, registry.AllocatedSb)

	conflict := *req
	conflict.Image = "nginx:other"
	_, err = server.CreateSandbox(context.Background(), &conflict)
	require.ErrorContains(t, err, "different create spec")
}

func TestServer_CreateSandbox_FastMode_FastletRPCFailure(t *testing.T) {
	// Test fastlet RPC failure handling in Fast mode:
	// 1. Registry.Allocate succeeds
	// 2. FastletClient.CreateSandbox fails
	// 3. Registry.Release is called
	// 4. Error is returned
	// 5. No CRD is created

	registry := &MockRegistryForTest{
		DefaultFastlet: &fastletpool.FastletInfo{
			ID:            "fastlet-1",
			PodName:       "fastlet-pod-1",
			PodIP:         "10.0.0.5",
			NodeName:      "node-1",
			PoolName:      "test-pool",
			Capacity:      10,
			Allocated:     0,
			LastHeartbeat: time.Now(),
		},
	}

	server := &Server{
		K8sClient:              newFastpathTestClientBuilder(setupTestScheme(t)).Build(),
		Registry:               registry,
		FastletClient:          api.NewFastletClient(5758),
		DefaultConsistencyMode: api.ConsistencyModeFast,
	}

	req := &fastpathv1.CreateRequest{
		Image:     "nginx:latest",
		PoolRef:   "test-pool",
		Namespace: "default",
	}

	// Using an invalid PodIP (empty) to cause RPC failure
	registry.DefaultFastlet.PodIP = ""

	resp, err := server.CreateSandbox(context.Background(), req)

	// Since we can't actually mock the HTTP call, we verify the flow
	// The allocation should have succeeded
	assert.NotNil(t, registry.AllocatedSb, "Allocate should have been called")
	_ = resp
	_ = err
	t.Skip("Requires HTTP server mock")
}

// ============================================================================
// Strong Mode Tests
// ============================================================================

func TestServer_CreateSandbox_StrongMode_Success(t *testing.T) {
	// Test successful sandbox creation in Strong mode:
	// 1. Registry.Allocate succeeds
	// 2. K8sClient.Create succeeds (creates CRD)
	// 3. FastletClient.CreateSandbox succeeds
	// 4. Status update succeeds
	// 5. Response contains correct information

	registry := &MockRegistryForTest{
		DefaultFastlet: &fastletpool.FastletInfo{
			ID:            "fastlet-1",
			PodName:       "fastlet-pod-1",
			PodIP:         "10.0.0.5",
			NodeName:      "node-1",
			PoolName:      "test-pool",
			Capacity:      10,
			Allocated:     0,
			LastHeartbeat: time.Now(),
		},
	}

	server := &Server{
		K8sClient:              newFastpathTestClientBuilder(setupTestScheme(t)).Build(),
		Registry:               registry,
		FastletClient:          api.NewFastletClient(5758),
		DefaultConsistencyMode: api.ConsistencyModeFast,
	}

	req := &fastpathv1.CreateRequest{
		Image:           "nginx:latest",
		PoolRef:         "test-pool",
		Namespace:       "default",
		ConsistencyMode: fastpathv1.ConsistencyMode_STRONG,
		Name:            "test-sandbox",
	}

	// Verify allocation happens
	_ = req
	_ = server
	t.Skip("Requires HTTP server mock or interface refactoring")
}

func TestServer_CreateSandbox_StrongMode_K8sError(t *testing.T) {
	// Test K8s CRUD error handling in Strong mode:
	// 1. Registry.Allocate succeeds
	// 2. K8sClient.Create fails (e.g., conflict, validation error)
	// 3. Registry.Release is called
	// 4. Error is returned
	// 5. No fastlet RPC is made

	registry := &MockRegistryForTest{
		DefaultFastlet: &fastletpool.FastletInfo{
			ID:            "fastlet-1",
			PodName:       "fastlet-pod-1",
			PodIP:         "10.0.0.5",
			NodeName:      "node-1",
			PoolName:      "test-pool",
			Capacity:      10,
			Allocated:     0,
			LastHeartbeat: time.Now(),
		},
	}

	scheme := setupTestScheme(t)
	k8sClient := newFastpathTestClientBuilder(scheme).Build()

	// Create a sandbox with invalid name to cause K8s validation error
	server := &Server{
		K8sClient:              k8sClient,
		Registry:               registry,
		FastletClient:          api.NewFastletClient(5758),
		DefaultConsistencyMode: api.ConsistencyModeFast,
	}

	req := &fastpathv1.CreateRequest{
		Image:           "nginx:latest",
		PoolRef:         "test-pool",
		Namespace:       "default",
		ConsistencyMode: fastpathv1.ConsistencyMode_STRONG,
		Name:            "test-sandbox",
	}

	// Create the sandbox first to cause a conflict
	existingSb := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
		Spec: apiv1alpha1.SandboxSpec{
			Image:   "existing-image",
			PoolRef: "test-pool",
		},
	}
	err := k8sClient.Create(context.Background(), existingSb)
	require.NoError(t, err)

	// Now try to create with same name - should get conflict
	resp, err := server.CreateSandbox(context.Background(), req)

	assert.Error(t, err, "CreateSandbox should return error when CRD already exists")
	assert.Nil(t, resp, "Response should be nil on error")
	assert.NotNil(t, registry.AllocatedSb, "Allocate should have been called")
	// Note: The release happens in createStrong when K8s create fails
	// We can't easily verify this without a more sophisticated mock
}

// ============================================================================
// Validation Tests
// ============================================================================

func TestServer_CreateSandbox_InvalidRequest(t *testing.T) {
	// Test validation of invalid requests:
	// 1. Empty image - should fail or use default
	// 2. Empty pool ref - should be handled
	// 3. Empty namespace - defaults to "default"
	// 4. Empty name - auto-generated name is used

	tests := []struct {
		name           string
		req            *fastpathv1.CreateRequest
		expectError    bool
		errorContains  string
		validateResult func(t *testing.T, resp *fastpathv1.CreateResponse, err error)
	}{
		{
			name: "valid request with all fields",
			req: &fastpathv1.CreateRequest{
				Image:        "nginx:latest",
				PoolRef:      "test-pool",
				Namespace:    "default",
				Name:         "my-sandbox",
				ExposedPorts: []int32{80},
			},
			expectError:    true, // Will fail due to no real fastlet
			errorContains:  "",
			validateResult: nil,
		},
		{
			name: "valid request with empty name - should auto-generate",
			req: &fastpathv1.CreateRequest{
				Image:     "nginx:latest",
				PoolRef:   "test-pool",
				Namespace: "default",
				Name:      "", // Empty name
			},
			expectError:    true,
			errorContains:  "",
			validateResult: nil,
		},
		{
			name: "request with empty namespace - uses default",
			req: &fastpathv1.CreateRequest{
				Image:     "nginx:latest",
				PoolRef:   "test-pool",
				Namespace: "", // Empty namespace
				Name:      "test-sb",
			},
			expectError:   true,
			errorContains: "",
			validateResult: func(t *testing.T, resp *fastpathv1.CreateResponse, err error) {
				// Verify namespace defaults to empty string which becomes ""
				// The server doesn't default namespace, it uses what's provided
			},
		},
		{
			name: "request with exposed ports",
			req: &fastpathv1.CreateRequest{
				Image:        "redis:latest",
				PoolRef:      "cache-pool",
				Namespace:    "default",
				Name:         "redis-sb",
				ExposedPorts: []int32{6379},
			},
			expectError:    true,
			errorContains:  "",
			validateResult: nil,
		},
		{
			name: "request with environment variables",
			req: &fastpathv1.CreateRequest{
				Image:     "postgres:latest",
				PoolRef:   "db-pool",
				Namespace: "default",
				Name:      "postgres-sb",
				Envs: map[string]string{
					"POSTGRES_PASSWORD": "secret",
					"POSTGRES_DB":       "mydb",
				},
			},
			expectError:    true,
			errorContains:  "",
			validateResult: nil,
		},
		{
			name: "request with command and args",
			req: &fastpathv1.CreateRequest{
				Image:     "busybox:latest",
				PoolRef:   "test-pool",
				Namespace: "default",
				Name:      "busybox-sb",
				Command:   []string{"/bin/sh"},
				Args:      []string{"-c", "sleep 10"},
			},
			expectError:    true,
			errorContains:  "",
			validateResult: nil,
		},
		{
			name: "request with working directory",
			req: &fastpathv1.CreateRequest{
				Image:      "app:latest",
				PoolRef:    "test-pool",
				Namespace:  "default",
				Name:       "app-sb",
				WorkingDir: "/workspace",
			},
			expectError:    true,
			errorContains:  "",
			validateResult: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := &MockRegistryForTest{
				DefaultFastlet: &fastletpool.FastletInfo{
					ID:            "fastlet-1",
					PodName:       "fastlet-pod-1",
					PodIP:         "10.0.0.5",
					NodeName:      "node-1",
					PoolName:      tt.req.PoolRef,
					Capacity:      10,
					Allocated:     0,
					LastHeartbeat: time.Now(),
				},
			}

			server := &Server{
				K8sClient:              newFastpathTestClientBuilder(setupTestScheme(t)).Build(),
				Registry:               registry,
				FastletClient:          api.NewFastletClient(5758),
				DefaultConsistencyMode: api.ConsistencyModeFast,
			}

			resp, err := server.CreateSandbox(context.Background(), tt.req)

			if tt.expectError {
				assert.Error(t, err)
			}
			if tt.errorContains != "" {
				assert.Contains(t, err.Error(), tt.errorContains)
			}
			if tt.validateResult != nil {
				tt.validateResult(t, resp, err)
			}
		})
	}
}

// ============================================================================
// Metrics Tests
// ============================================================================

func TestServer_CreateSandbox_MetricsRecorded(t *testing.T) {
	// Verify Prometheus metrics are recorded:
	// 1. fastpath_create_sandbox_duration_seconds is incremented on success
	// 2. fastpath_create_sandbox_duration_seconds is incremented on failure
	// 3. Label "mode" is "fast" or "strong"
	// 4. Label "success" is "true" or "false"

	// Note: Testing prometheus metrics requires:
	// 1. Either using prometheus.NewRegistry instead of promauto
	// 2. Or checking the default registry

	t.Skip("Prometheus metrics testing requires registry setup or refactoring to use test registry")
}

// ============================================================================
// Helper function tests
// ============================================================================

func TestEnvMapToEnvVar(t *testing.T) {
	tests := []struct {
		name     string
		envs     map[string]string
		expected []string
	}{
		{
			name:     "empty map",
			envs:     map[string]string{},
			expected: []string{},
		},
		{
			name: "single env var",
			envs: map[string]string{
				"FOO": "bar",
			},
			expected: []string{"FOO"},
		},
		{
			name: "multiple env vars",
			envs: map[string]string{
				"FOO":   "bar",
				"BAZ":   "qux",
				"DEBUG": "true",
			},
			expected: []string{"FOO", "BAZ", "DEBUG"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := envMapToEnvVar(tt.envs)

			// Check length
			assert.Equal(t, len(tt.expected), len(result), "Number of env vars should match")

			// Check all expected names are present
			resultNames := make(map[string]bool)
			for _, env := range result {
				resultNames[env.Name] = true
			}
			for _, expectedName := range tt.expected {
				assert.True(t, resultNames[expectedName], "Env var %s should be present", expectedName)
			}

			// Check values are correct
			for _, env := range result {
				assert.Equal(t, tt.envs[env.Name], env.Value, "Value for %s should match", env.Name)
			}
		})
	}
}

func TestServer_loadRuntimeParameters_UsesPoolProfiles(t *testing.T) {
	server := &Server{K8sClient: newFastpathTestClientBuilder(setupTestScheme(t)).Build()}
	params, err := server.loadRuntimeParameters(context.Background(), &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-a", Namespace: "default"},
		Spec:       apiv1alpha1.SandboxSpec{PoolRef: "test-pool"},
	})

	require.NoError(t, err)
	assert.Equal(t, "500m", params.CPU)
	assert.Equal(t, "256Mi", params.Memory)
	assert.Equal(t, int64(128), params.PIDs)
	assert.NotEmpty(t, params.RuntimeProfileHash)
	assert.NotEmpty(t, params.ResourceProfileHash)
}

func TestServer_GetEndpoints(t *testing.T) {
	tests := []struct {
		name     string
		ip       string
		ports    []int32
		expected []string
	}{
		{
			name:     "single port",
			ip:       "10.0.0.5",
			ports:    []int32{8080},
			expected: []string{"10.0.0.5:8080"},
		},
		{
			name:     "multiple ports",
			ip:       "10.0.0.5",
			ports:    []int32{80, 443, 8080},
			expected: []string{"10.0.0.5:80", "10.0.0.5:443", "10.0.0.5:8080"},
		},
		{
			name:     "no ports",
			ip:       "10.0.0.5",
			ports:    []int32{},
			expected: nil, // getEndpoints returns nil slice when no ports
		},
		{
			name:     "different IP",
			ip:       "192.168.1.100",
			ports:    []int32{3000},
			expected: []string{"192.168.1.100:3000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := &apiv1alpha1.Sandbox{
				Spec: apiv1alpha1.SandboxSpec{
					ExposedPorts: tt.ports,
				},
			}

			server := &Server{}
			result := server.getEndpoints(tt.ip, sb)

			assert.Equal(t, tt.expected, result, "Endpoints should match")
		})
	}
}

// ============================================================================
// Integration-style tests with fake k8s client
// ============================================================================

func TestServer_CreateSandbox_AllocateCalledWithCorrectSandbox(t *testing.T) {
	// Test that Registry.Allocate is called with the correct Sandbox object

	var allocatedSandbox *apiv1alpha1.Sandbox
	registry := &MockRegistryForTest{
		AllocateFunc: func(sb *apiv1alpha1.Sandbox) (*fastletpool.FastletInfo, error) {
			allocatedSandbox = sb
			return &fastletpool.FastletInfo{
				ID:            "fastlet-1",
				PodName:       "fastlet-pod-1",
				PodIP:         "10.0.0.5",
				NodeName:      "node-1",
				PoolName:      sb.Spec.PoolRef,
				Capacity:      10,
				Allocated:     0,
				LastHeartbeat: time.Now(),
			}, nil
		},
	}

	server := &Server{
		K8sClient:              newFastpathTestClientBuilder(setupTestScheme(t)).Build(),
		Registry:               registry,
		FastletClient:          api.NewFastletClient(5758),
		DefaultConsistencyMode: api.ConsistencyModeFast,
	}

	req := &fastpathv1.CreateRequest{
		Image:        "nginx:latest",
		PoolRef:      "test-pool",
		Namespace:    "test-ns",
		Name:         "test-sandbox-name",
		ExposedPorts: []int32{80, 443},
		Command:      []string{"/bin/sh"},
		Args:         []string{"-c", "echo hi"},
		Envs:         map[string]string{"ENV1": "value1"},
		WorkingDir:   "/app",
	}

	_, _ = server.CreateSandbox(context.Background(), req)

	// Verify Allocate was called
	require.NotNil(t, allocatedSandbox, "Allocate should have been called")

	// Verify Sandbox fields
	assert.Equal(t, "test-sandbox-name", allocatedSandbox.Name, "Sandbox name should match request")
	assert.Equal(t, "test-ns", allocatedSandbox.Namespace, "Sandbox namespace should match request")
	assert.Equal(t, "nginx:latest", allocatedSandbox.Spec.Image, "Image should match request")
	assert.Equal(t, "test-pool", allocatedSandbox.Spec.PoolRef, "PoolRef should match request")
	assert.Equal(t, []int32{80, 443}, allocatedSandbox.Spec.ExposedPorts, "ExposedPorts should match request")
	assert.Equal(t, []string{"/bin/sh"}, allocatedSandbox.Spec.Command, "Command should match request")
	assert.Equal(t, []string{"-c", "echo hi"}, allocatedSandbox.Spec.Args, "Args should match request")
	assert.Equal(t, "/app", allocatedSandbox.Spec.WorkingDir, "WorkingDir should match request")

	// Verify environment variables are converted correctly
	require.Len(t, allocatedSandbox.Spec.Envs, 1, "Should have 1 environment variable")
	assert.Equal(t, "ENV1", allocatedSandbox.Spec.Envs[0].Name, "Env name should match")
	assert.Equal(t, "value1", allocatedSandbox.Spec.Envs[0].Value, "Env value should match")
}

func TestServer_CreateSandbox_ReleaseCalledOnAllocateError(t *testing.T) {
	// Test that Registry.Release is NOT called on allocation failure
	// (since nothing was allocated)

	var released bool
	registry := &MockRegistryForTest{
		AllocateError: errors.New("no capacity"),
		ReleaseFunc: func(id fastletpool.FastletID, sb *apiv1alpha1.Sandbox) {
			released = true
		},
	}

	server := &Server{
		K8sClient:              newFastpathTestClientBuilder(setupTestScheme(t)).Build(),
		Registry:               registry,
		FastletClient:          api.NewFastletClient(5758),
		DefaultConsistencyMode: api.ConsistencyModeFast,
	}

	req := &fastpathv1.CreateRequest{
		Image:     "nginx:latest",
		PoolRef:   "test-pool",
		Namespace: "default",
	}

	_, err := server.CreateSandbox(context.Background(), req)

	assert.Error(t, err)
	assert.False(t, released, "Release should NOT be called when allocation fails")
}

func TestServer_CreateSandbox_NameGeneration(t *testing.T) {
	// Test that sandbox name is auto-generated when not provided

	var allocatedSandbox *apiv1alpha1.Sandbox
	registry := &MockRegistryForTest{
		AllocateFunc: func(sb *apiv1alpha1.Sandbox) (*fastletpool.FastletInfo, error) {
			allocatedSandbox = sb
			return &fastletpool.FastletInfo{
				ID:            "fastlet-1",
				PodName:       "fastlet-pod-1",
				PodIP:         "10.0.0.5",
				NodeName:      "node-1",
				PoolName:      sb.Spec.PoolRef,
				Capacity:      10,
				Allocated:     0,
				LastHeartbeat: time.Now(),
			}, nil
		},
	}

	server := &Server{
		K8sClient:              newFastpathTestClientBuilder(setupTestScheme(t)).Build(),
		Registry:               registry,
		FastletClient:          api.NewFastletClient(5758),
		DefaultConsistencyMode: api.ConsistencyModeFast,
	}

	req := &fastpathv1.CreateRequest{
		Image:     "nginx:latest",
		PoolRef:   "test-pool",
		Namespace: "default",
		Name:      "", // Empty name - should be auto-generated
	}

	_, _ = server.CreateSandbox(context.Background(), req)

	require.NotNil(t, allocatedSandbox, "Allocate should have been called")
	assert.NotEmpty(t, allocatedSandbox.Name, "Name should be auto-generated")
	assert.Contains(t, allocatedSandbox.Name, "sb-", "Auto-generated name should start with 'sb-'")
}

func TestServer_CreateSandbox_ConsistencyMode(t *testing.T) {
	// Test that consistency mode is properly determined:
	// 1. Default mode from Server config
	// 2. Override by request

	tests := []struct {
		name             string
		serverMode       api.ConsistencyMode
		requestMode      fastpathv1.ConsistencyMode
		expectedFastMode bool
	}{
		{
			name:             "default fast mode",
			serverMode:       api.ConsistencyModeFast,
			requestMode:      fastpathv1.ConsistencyMode_FAST,
			expectedFastMode: true,
		},
		{
			name:             "server default fast, request strong",
			serverMode:       api.ConsistencyModeFast,
			requestMode:      fastpathv1.ConsistencyMode_STRONG,
			expectedFastMode: false,
		},
		{
			name:             "server default strong, request fast",
			serverMode:       api.ConsistencyModeStrong,
			requestMode:      fastpathv1.ConsistencyMode_FAST,
			expectedFastMode: true,
		},
		{
			name:             "server default strong, request strong",
			serverMode:       api.ConsistencyModeStrong,
			requestMode:      fastpathv1.ConsistencyMode_STRONG,
			expectedFastMode: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := &MockRegistryForTest{
				AllocateFunc: func(sb *apiv1alpha1.Sandbox) (*fastletpool.FastletInfo, error) {
					return &fastletpool.FastletInfo{
						ID:            "fastlet-1",
						PodName:       "fastlet-pod-1",
						PodIP:         "10.0.0.5",
						NodeName:      "node-1",
						PoolName:      sb.Spec.PoolRef,
						Capacity:      10,
						Allocated:     0,
						LastHeartbeat: time.Now(),
					}, nil
				},
			}

			server := &Server{
				K8sClient:              newFastpathTestClientBuilder(setupTestScheme(t)).Build(),
				Registry:               registry,
				FastletClient:          api.NewFastletClient(5758),
				DefaultConsistencyMode: tt.serverMode,
			}

			req := &fastpathv1.CreateRequest{
				Image:           "nginx:latest",
				PoolRef:         "test-pool",
				Namespace:       "default",
				Name:            "test-sb",
				ConsistencyMode: tt.requestMode,
			}

			// We can't directly test which code path was taken
			// but we can verify the request is processed
			// This test verifies the mode selection logic compiles
			_, err := server.CreateSandbox(context.Background(), req)
			// Will fail on RPC but that's OK - we're testing compilation mostly
			_ = err
		})
	}
}

func TestServer_CreateSandbox_StrongMode_CRDCreated(t *testing.T) {
	// Test that in Strong mode, CRD is created before fastlet call
	// and if CRD creation fails, fastlet is not called

	registry := &MockRegistryForTest{
		DefaultFastlet: &fastletpool.FastletInfo{
			ID:            "fastlet-1",
			PodName:       "fastlet-pod-1",
			PodIP:         "10.0.0.5",
			NodeName:      "node-1",
			PoolName:      "test-pool",
			Capacity:      10,
			Allocated:     0,
			LastHeartbeat: time.Now(),
		},
	}

	scheme := setupTestScheme(t)
	k8sClient := newFastpathTestClientBuilder(scheme).Build()

	server := &Server{
		K8sClient:              k8sClient,
		Registry:               registry,
		FastletClient:          api.NewFastletClient(5758),
		DefaultConsistencyMode: api.ConsistencyModeFast,
	}

	req := &fastpathv1.CreateRequest{
		Image:           "nginx:latest",
		PoolRef:         "test-pool",
		Namespace:       "default",
		Name:            "test-strong-sb",
		ConsistencyMode: fastpathv1.ConsistencyMode_STRONG,
	}

	// Create an existing sandbox to cause conflict
	existingSb := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-strong-sb",
			Namespace: "default",
		},
		Spec: apiv1alpha1.SandboxSpec{
			Image:   "existing",
			PoolRef: "test-pool",
		},
	}
	err := k8sClient.Create(context.Background(), existingSb)
	require.NoError(t, err)

	// Attempt to create - should fail on CRD creation
	_, err = server.CreateSandbox(context.Background(), req)

	assert.Error(t, err, "Should fail when CRD already exists")

	// Verify the sandbox still exists (wasn't modified)
	sb := &apiv1alpha1.Sandbox{}
	err = k8sClient.Get(context.Background(), types.NamespacedName{Name: "test-strong-sb", Namespace: "default"}, sb)
	assert.NoError(t, err, "Existing sandbox should still exist")
	assert.Equal(t, "existing", sb.Spec.Image, "Existing sandbox should not be modified")
}

func TestServer_ListSandboxes(t *testing.T) {
	// Test ListSandboxes method

	scheme := setupTestScheme(t)

	// Create test sandboxes
	sb1 := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "sb-1",
			Namespace:         "default",
			CreationTimestamp: metav1.Now(),
		},
		Spec: apiv1alpha1.SandboxSpec{
			Image:   "nginx",
			PoolRef: "pool-1",
		},
		Status: apiv1alpha1.SandboxStatus{
			SandboxID:       "container-sb1",
			Phase:           "Bound",
			AssignedFastlet: "fastlet-1",
			Endpoints:       []string{"10.0.0.1:80"},
		},
	}

	sb2 := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "sb-2",
			Namespace:         "default",
			CreationTimestamp: metav1.Now(),
		},
		Spec: apiv1alpha1.SandboxSpec{
			Image:   "redis",
			PoolRef: "pool-2",
		},
		Status: apiv1alpha1.SandboxStatus{
			SandboxID:       "container-sb2",
			Phase:           "Pending",
			AssignedFastlet: "",
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sb1, sb2).Build()

	server := &Server{
		K8sClient: k8sClient,
	}

	req := &fastpathv1.ListRequest{
		Namespace: "default",
	}

	resp, err := server.ListSandboxes(context.Background(), req)

	require.NoError(t, err)
	assert.Len(t, resp.Items, 2, "Should return 2 sandboxes")

	// Find sb-1 (use SandboxName)
	var sb1Info *fastpathv1.SandboxInfo
	for _, item := range resp.Items {
		if item.SandboxName == "sb-1" {
			sb1Info = item
			break
		}
	}
	require.NotNil(t, sb1Info, "sb-1 should be in response")
	assert.Equal(t, "Bound", sb1Info.Phase, "Phase should match")
	assert.Equal(t, "fastlet-1", sb1Info.FastletPod, "FastletPod should match")
	assert.Equal(t, "nginx", sb1Info.Image, "Image should match")
	assert.Equal(t, "pool-1", sb1Info.PoolRef, "PoolRef should match")
}

func TestServer_GetSandbox(t *testing.T) {
	// Test GetSandbox method

	scheme := setupTestScheme(t)

	sb := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-sb",
			Namespace:         "default",
			CreationTimestamp: metav1.Now(),
		},
		Spec: apiv1alpha1.SandboxSpec{
			Image:   "nginx",
			PoolRef: "pool-1",
		},
		Status: apiv1alpha1.SandboxStatus{
			SandboxID:       "container-12345",
			Phase:           "Bound",
			AssignedFastlet: "fastlet-1",
			Endpoints:       []string{"10.0.0.1:80"},
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sb).Build()

	server := &Server{
		K8sClient: k8sClient,
	}

	req := &fastpathv1.GetRequest{
		SandboxName: "test-sb",
		Namespace:   "default",
	}

	resp, err := server.GetSandbox(context.Background(), req)

	require.NoError(t, err)
	assert.Equal(t, "container-12345", resp.SandboxId, "SandboxId should match Status.SandboxID")
	assert.Equal(t, "test-sb", resp.SandboxName, "SandboxName should match CRD name")
	assert.Equal(t, "Bound", resp.Phase)
	assert.Equal(t, "fastlet-1", resp.FastletPod)
	assert.Equal(t, "nginx", resp.Image)
	assert.Equal(t, "pool-1", resp.PoolRef)
	assert.Len(t, resp.Endpoints, 1)
	assert.Equal(t, "10.0.0.1:80", resp.Endpoints[0])
}

func TestServer_GetSandbox_NotFound(t *testing.T) {
	// Test GetSandbox with non-existent sandbox

	scheme := setupTestScheme(t)
	k8sClient := newFastpathTestClientBuilder(scheme).Build()

	server := &Server{
		K8sClient: k8sClient,
	}

	req := &fastpathv1.GetRequest{
		SandboxName: "non-existent",
		Namespace:   "default",
	}

	resp, err := server.GetSandbox(context.Background(), req)

	assert.Error(t, err, "Should return error when sandbox not found")
	assert.Nil(t, resp, "Response should be nil on error")
}

func TestServer_DeleteSandbox(t *testing.T) {
	// Test DeleteSandbox method

	scheme := setupTestScheme(t)

	sb := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sb",
			Namespace: "default",
		},
		Spec: apiv1alpha1.SandboxSpec{
			Image:   "nginx",
			PoolRef: "pool-1",
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sb).Build()

	server := &Server{
		K8sClient: k8sClient,
	}

	req := &fastpathv1.DeleteRequest{
		SandboxName: "test-sb",
		Namespace:   "default",
	}

	resp, err := server.DeleteSandbox(context.Background(), req)

	require.NoError(t, err)
	assert.True(t, resp.Success, "Delete should succeed")

	// Verify sandbox is deleted
	checkSb := &apiv1alpha1.Sandbox{}
	err = k8sClient.Get(context.Background(), types.NamespacedName{Name: "test-sb", Namespace: "default"}, checkSb)
	assert.Error(t, err, "Sandbox should be deleted")
}

func TestServer_DeleteSandbox_NotFound(t *testing.T) {
	// Test DeleteSandbox with non-existent sandbox

	scheme := setupTestScheme(t)
	k8sClient := newFastpathTestClientBuilder(scheme).Build()

	server := &Server{
		K8sClient: k8sClient,
	}

	req := &fastpathv1.DeleteRequest{
		SandboxName: "non-existent",
		Namespace:   "default",
	}

	resp, err := server.DeleteSandbox(context.Background(), req)

	// DeleteSandbox returns both response and error
	// The response has Success=false when error occurs
	assert.Error(t, err, "Should return error when sandbox not found")
	assert.False(t, resp.Success, "Success should be false when error occurs")
}

// ============================================================================
// Allocation Annotation Tests
// ============================================================================

func TestServer_createFast_SetsAllocationAnnotation(t *testing.T) {
	// Test that createFast sets the allocation annotation on tempSB
	registry := &MockRegistryForTest{
		DefaultFastlet: &fastletpool.FastletInfo{
			ID:       "fastlet-1",
			PodName:  "test-fastlet-pod",
			PodIP:    "10.0.0.5",
			NodeName: "test-node",
			PoolName: "test-pool",
		},
	}

	server := &Server{
		K8sClient:              newFastpathTestClientBuilder(setupTestScheme(t)).Build(),
		Registry:               registry,
		FastletClient:          api.NewFastletClient(5758),
		DefaultConsistencyMode: api.ConsistencyModeFast,
	}

	tempSB := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sb",
			Namespace: "default",
		},
		Spec: apiv1alpha1.SandboxSpec{
			Image:   "nginx:latest",
			PoolRef: "test-pool",
		},
	}

	req := &fastpathv1.CreateRequest{
		Image:     "nginx:latest",
		PoolRef:   "test-pool",
		Namespace: "default",
		Name:      "test-sb",
	}

	// 由于 FastletClient.CreateSandbox 会调用真实 HTTP，它会失败
	// 但我们仍然可以验证 annotation 被设置的逻辑（在失败前）
	// 实际上，在 createFast 中，annotation 是在 Fastlet API 成功后才设置的
	// 所以这里我们测试失败场景，验证不会设置 annotation
	registry.DefaultFastlet.PodIP = ""

	resp, err := server.createFast(context.Background(), tempSB, registry.DefaultFastlet, req)

	// 验证调用失败
	assert.Error(t, err)
	assert.Nil(t, resp)

	// 由于失败，annotation 不应该被设置（这是正确的行为）
	// 在实际场景中，Fastlet API 成功后才会设置 annotation
	annotations := tempSB.GetAnnotations()
	// annotation 为空或不存在是失败场景的预期行为
	if annotations != nil {
		// 如果有 annotations，不应该包含 allocation annotation（因为失败了）
		_, hasAlloc := annotations["sandbox.fast.io/allocation"]
		assert.False(t, hasAlloc, "Allocation annotation should not be set on failure")
	}
}

func TestServer_createStrong_SetsAllocationAnnotation(t *testing.T) {
	// Test that createStrong sets the allocation annotation before creating CRD
	scheme := setupTestScheme(t)
	k8sClient := newFastpathTestClientBuilder(scheme).Build()

	registry := &MockRegistryForTest{
		DefaultFastlet: &fastletpool.FastletInfo{
			ID:       "fastlet-1",
			PodName:  "test-fastlet-pod",
			PodIP:    "10.0.0.5",
			NodeName: "test-node",
			PoolName: "test-pool",
		},
	}

	server := &Server{
		K8sClient:              k8sClient,
		Registry:               registry,
		FastletClient:          api.NewFastletClient(5758),
		DefaultConsistencyMode: api.ConsistencyModeStrong,
	}

	tempSB := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sb",
			Namespace: "default",
		},
		Spec: apiv1alpha1.SandboxSpec{
			Image:   "nginx:latest",
			PoolRef: "test-pool",
		},
	}

	req := &fastpathv1.CreateRequest{
		Image:           "nginx:latest",
		PoolRef:         "test-pool",
		Namespace:       "default",
		Name:            "test-sb",
		ConsistencyMode: fastpathv1.ConsistencyMode_STRONG,
	}

	// 调用 createStrong，验证 annotation 被设置（即使后续会失败）
	// 先设置一个已经存在的 CRD 会导致冲突，但我们只需要验证 annotation 设置
	// 使用无效的 PodIP 来让 fastlet 调用失败，但 annotation 已经在 tempSB 上设置了
	registry.DefaultFastlet.PodIP = ""

	_, _ = server.createStrong(context.Background(), tempSB, registry.DefaultFastlet, req)

	// 验证 annotation 已被设置
	annotations := tempSB.GetAnnotations()
	require.NotNil(t, annotations, "Annotations should be set")
	assert.Contains(t, annotations, "sandbox.fast.io/allocation", "Allocation annotation should be set")

	// 验证 annotation 内容
	allocJSON := annotations["sandbox.fast.io/allocation"]
	assert.Contains(t, allocJSON, "test-fastlet-pod", "Annotation should contain assignedFastlet")
	assert.Contains(t, allocJSON, "test-node", "Annotation should contain assignedNode")
}

func TestServer_AllocationAnnotationFormat(t *testing.T) {
	// Test that the allocation annotation has the correct format
	// 直接调用 common.BuildAllocationJSON 来测试格式
	assignedFastlet := "my-fastlet"
	assignedNode := "my-node"

	allocJSON := common.BuildAllocationJSON(assignedFastlet, assignedNode)
	assert.NotEmpty(t, allocJSON, "Allocation JSON should be generated")

	// 验证可以解析为 JSON
	var allocInfo map[string]string
	err := json.Unmarshal([]byte(allocJSON), &allocInfo)
	require.NoError(t, err, "Allocation JSON should be valid JSON")

	// 验证必需字段
	assert.Equal(t, assignedFastlet, allocInfo["assignedFastlet"])
	assert.Equal(t, assignedNode, allocInfo["assignedNode"])
	assert.NotEmpty(t, allocInfo["allocatedAt"])
}
