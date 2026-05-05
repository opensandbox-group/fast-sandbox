package fastpath

import (
	"context"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/internal/api"
	"fast-sandbox/internal/controller/fastletpool"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MockRegistryForTest is a mock implementation of FastletRegistry for testing.
type MockRegistryForTest struct {
	AllocateFunc   func(sb *apiv1alpha1.Sandbox) (*fastletpool.FastletInfo, error)
	ReleaseFunc    func(id fastletpool.FastletID, sb *apiv1alpha1.Sandbox)
	AllocatedSb    *apiv1alpha1.Sandbox
	ReleasedID     fastletpool.FastletID
	ReleasedSb     *apiv1alpha1.Sandbox
	DefaultFastlet *fastletpool.FastletInfo
	AllocateError  error
	Fastlets       map[fastletpool.FastletID]fastletpool.FastletInfo
}

func (m *MockRegistryForTest) RegisterOrUpdate(info fastletpool.FastletInfo) {
	if m.Fastlets == nil {
		m.Fastlets = make(map[fastletpool.FastletID]fastletpool.FastletInfo)
	}
	m.Fastlets[info.ID] = info
}

func (m *MockRegistryForTest) GetAllFastlets() []fastletpool.FastletInfo {
	result := make([]fastletpool.FastletInfo, 0, len(m.Fastlets))
	for _, a := range m.Fastlets {
		result = append(result, a)
	}
	return result
}

func (m *MockRegistryForTest) GetFastletByID(id fastletpool.FastletID) (fastletpool.FastletInfo, bool) {
	if a, ok := m.Fastlets[id]; ok {
		return a, true
	}
	return fastletpool.FastletInfo{}, false
}

func (m *MockRegistryForTest) Allocate(sb *apiv1alpha1.Sandbox) (*fastletpool.FastletInfo, error) {
	m.AllocatedSb = sb
	if m.AllocateFunc != nil {
		return m.AllocateFunc(sb)
	}
	if m.AllocateError != nil {
		return nil, m.AllocateError
	}
	if m.DefaultFastlet != nil {
		return m.DefaultFastlet, nil
	}
	return &fastletpool.FastletInfo{
		ID:            "test-fastlet",
		PodName:       "test-fastlet",
		PodIP:         "10.0.0.1",
		NodeName:      "test-node",
		PoolName:      "test-pool",
		Capacity:      10,
		Allocated:     0,
		LastHeartbeat: time.Now(),
	}, nil
}

func (m *MockRegistryForTest) Release(id fastletpool.FastletID, sb *apiv1alpha1.Sandbox) {
	m.ReleasedID = id
	m.ReleasedSb = sb
	if m.ReleaseFunc != nil {
		m.ReleaseFunc(id, sb)
	}
}

func (m *MockRegistryForTest) Restore(ctx context.Context, c client.Reader) error {
	return nil
}

func (m *MockRegistryForTest) Remove(id fastletpool.FastletID) {
	if m.Fastlets != nil {
		delete(m.Fastlets, id)
	}
}

func (m *MockRegistryForTest) CleanupStaleFastlets(timeout time.Duration) int {
	return 0
}

// MockFastletClientForTest is a mock implementation of FastletAPIClient for testing.
type MockFastletClientForTest struct {
	CreateSandboxFunc    func(endpoint string, req *api.CreateSandboxRequest) (*api.CreateSandboxResponse, error)
	DeleteSandboxFunc    func(endpoint string, req *api.DeleteSandboxRequest) (*api.DeleteSandboxResponse, error)
	GetFastletStatusFunc func(ctx context.Context, endpoint string) (*api.FastletStatus, error)
	CreateCalled         bool
	DeleteCalled         bool
	LastCreateEndpoint   string
	LastDeleteEndpoint   string
	LastCreateReq        *api.CreateSandboxRequest
	LastDeleteReq        *api.DeleteSandboxRequest
	CreateError          error
	DeleteError          error
}

func (m *MockFastletClientForTest) CreateSandbox(endpoint string, req *api.CreateSandboxRequest) (*api.CreateSandboxResponse, error) {
	m.CreateCalled = true
	m.LastCreateEndpoint = endpoint
	m.LastCreateReq = req
	if m.CreateSandboxFunc != nil {
		return m.CreateSandboxFunc(endpoint, req)
	}
	if m.CreateError != nil {
		return nil, m.CreateError
	}
	return &api.CreateSandboxResponse{
		Success:   true,
		SandboxID: req.Sandbox.SandboxID,
		CreatedAt: time.Now().Unix(),
	}, nil
}

func (m *MockFastletClientForTest) DeleteSandbox(endpoint string, req *api.DeleteSandboxRequest) (*api.DeleteSandboxResponse, error) {
	m.DeleteCalled = true
	m.LastDeleteEndpoint = endpoint
	m.LastDeleteReq = req
	if m.DeleteSandboxFunc != nil {
		return m.DeleteSandboxFunc(endpoint, req)
	}
	if m.DeleteError != nil {
		return nil, m.DeleteError
	}
	return &api.DeleteSandboxResponse{
		Success: true,
	}, nil
}

func (m *MockFastletClientForTest) GetFastletStatus(ctx context.Context, endpoint string) (*api.FastletStatus, error) {
	if m.GetFastletStatusFunc != nil {
		return m.GetFastletStatusFunc(ctx, endpoint)
	}
	return &api.FastletStatus{
		FastletID: "test-fastlet",
		NodeName:  "test-node",
		Capacity:  10,
		Allocated: 0,
	}, nil
}
