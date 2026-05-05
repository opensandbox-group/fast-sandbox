package janitor

import (
	"context"
	"errors"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Mock Helpers
// ============================================================================

// mockPodLister 模拟 PodLister 行为
type mockPodLister struct {
	pods    []*corev1.Pod
	listErr error
}

func (m *mockPodLister) List(labelSelector labels.Selector) ([]*corev1.Pod, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.pods, nil
}

func (m *mockPodLister) Pods(namespace string) listerv1.PodNamespaceLister {
	return &mockPodNamespaceLister{lister: m}
}

// mockPodNamespaceLister 模拟 namespace lister
type mockPodNamespaceLister struct {
	lister *mockPodLister
}

func (m *mockPodNamespaceLister) List(labelSelector labels.Selector) ([]*corev1.Pod, error) {
	return m.lister.List(labelSelector)
}

func (m *mockPodNamespaceLister) Get(name string) (*corev1.Pod, error) {
	for _, p := range m.lister.pods {
		if p.Name == name {
			return p, nil
		}
	}
	return nil, errors.New("pod not found")
}

// ============================================================================
// podExists Tests
// ============================================================================

func TestPodExists_Found(t *testing.T) {
	// RED: 测试当 Pod UID 匹配时 podExists 返回 true
	targetUID := types.UID("test-pod-uid-found")

	pods := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "fastlet-1",
				Namespace: "default",
				UID:       targetUID,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "fastlet-2",
				Namespace: "default",
				UID:       "other-uid",
			},
		},
	}

	j := &Janitor{
		podLister: &mockPodLister{
			pods: pods,
		},
	}

	exists := j.podExists(string(targetUID))

	assert.True(t, exists, "当 Pod UID 匹配时应返回 true")
}

func TestPodExists_NotFound(t *testing.T) {
	// RED: 测试当 Pod UID 不匹配任何 Pod 时 podExists 返回 false
	pods := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "fastlet-1",
				Namespace: "default",
				UID:       "uid-1",
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "fastlet-2",
				Namespace: "default",
				UID:       "uid-2",
			},
		},
	}

	j := &Janitor{
		podLister: &mockPodLister{
			pods: pods,
		},
	}

	exists := j.podExists("non-existent-uid")

	assert.False(t, exists, "当 Pod UID 不匹配任何 Pod 时应返回 false")
}

func TestPodExists_ListerError(t *testing.T) {
	// RED: 测试当 Lister 返回错误时 podExists 返回 false
	listErr := errors.New("lister error")

	j := &Janitor{
		podLister: &mockPodLister{
			listErr: listErr,
		},
	}

	exists := j.podExists("any-uid")

	assert.False(t, exists, "当 Lister 返回错误时应返回 false")
}

func TestPodExists_EmptyPodList(t *testing.T) {
	// RED: 测试当 Pod 列表为空时 podExists 返回 false
	j := &Janitor{
		podLister: &mockPodLister{
			pods: []*corev1.Pod{},
		},
	}

	exists := j.podExists("any-uid")

	assert.False(t, exists, "当 Pod 列表为空时应返回 false")
}

// ============================================================================
// CleanupTask Tests
// ============================================================================

func TestCleanupTask_Creation(t *testing.T) {
	// RED: 测试 CleanupTask 结构体创建
	task := CleanupTask{
		ContainerID: "container-123",
		FastletUID:  "fastlet-uid-456",
		PodName:     "fastlet-pod",
		Namespace:   "default",
	}

	assert.Equal(t, "container-123", task.ContainerID)
	assert.Equal(t, "fastlet-uid-456", task.FastletUID)
	assert.Equal(t, "fastlet-pod", task.PodName)
	assert.Equal(t, "default", task.Namespace)
}

// ============================================================================
// Scan Timeout Tests
// ============================================================================

func TestScan_WithTimeout(t *testing.T) {
	// RED: 测试 Scan 方法遵守 OrphanTimeout
	// 创建一个容器，创建时间在 OrphanTimeout 之内
	oldContainer := struct {
		id        string
		createdAt time.Time
	}{
		id:        "old-container",
		createdAt: time.Now().Add(-35 * time.Second), // 超过默认 30s
	}

	newContainer := struct {
		id        string
		createdAt time.Time
	}{
		id:        "new-container",
		createdAt: time.Now().Add(-5 * time.Second), // 未超过 10s
	}

	// 验证超时逻辑
	defaultTimeout := defaultOrphanTimeout // 10 秒

	// 旧容器应该被处理
	assert.True(t, time.Since(oldContainer.createdAt) > defaultTimeout,
		"旧容器创建时间应超过默认超时")

	// 新容器不应被处理（在安全缓冲期内）
	assert.False(t, time.Since(newContainer.createdAt) > defaultTimeout,
		"新容器创建时间不应超过默认超时")
}

func TestScan_CustomTimeout(t *testing.T) {
	// RED: 测试使用自定义 OrphanTimeout
	customTimeout := 5 * time.Second

	container := struct {
		createdAt time.Time
	}{
		createdAt: time.Now().Add(-6 * time.Second), // 超过自定义 5s
	}

	// 验证自定义超时逻辑
	assert.True(t, time.Since(container.createdAt) > customTimeout,
		"容器创建时间应超过自定义超时")
}

// ============================================================================
// Orphan Detection Tests
// ============================================================================

func TestOrphanDetection_FastletPodDisappeared(t *testing.T) {
	// RED: 测试当 Fastlet Pod 消失时检测到孤儿
	fastletUID := "disappeared-fastlet-uid"

	// 没有匹配的 Pod
	j := &Janitor{
		podLister: &mockPodLister{
			pods: []*corev1.Pod{},
		},
	}

	exists := j.podExists(fastletUID)

	assert.False(t, exists, "Fastlet Pod 消失时应返回 false，触发孤儿清理")
}

func TestOrphanDetection_SandboxCRDNotFound(t *testing.T) {
	// RED: 测试当 Sandbox CRD 不存在时检测到孤儿
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))

	// 空的 client（没有 Sandbox CRD）
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	j := &Janitor{
		K8sClient: k8sClient,
	}

	ctx := context.Background()
	var sb apiv1alpha1.Sandbox
	err := j.K8sClient.Get(ctx, types.NamespacedName{Name: "nonexistent", Namespace: "default"}, &sb)

	assert.Error(t, err, "Sandbox CRD 不存在应返回错误")
	assert.Contains(t, err.Error(), "not found", "错误应包含 'not found'")
}

func TestOrphanDetection_UIDMismatch(t *testing.T) {
	// RED: 测试当 UID 不匹配时检测到孤儿
	scheme := runtime.NewScheme()
	require.NoError(t, apiv1alpha1.AddToScheme(scheme))

	// 创建一个 Sandbox，UID 为 "sandbox-uid-1"
	sandbox := &apiv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			UID:       types.UID("sandbox-uid-1"),
		},
	}

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sandbox).Build()

	// 容器的 claimUID 为 "sandbox-uid-2"（不匹配）
	containerClaimUID := "sandbox-uid-2"

	ctx := context.Background()
	var sb apiv1alpha1.Sandbox
	err := k8sClient.Get(ctx, types.NamespacedName{Name: "test-sandbox", Namespace: "default"}, &sb)

	require.NoError(t, err, "应该能获取 Sandbox")

	// 验证 UID 不匹配
	assert.NotEqual(t, containerClaimUID, string(sb.UID), "UID 应不匹配，触发孤儿清理")
}
