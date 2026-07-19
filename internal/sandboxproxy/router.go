package sandboxproxy

import (
	"context"
	"errors"
	"fmt"
	"sync"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	ErrSandboxNotFound    = errors.New("sandbox route not found")
	ErrSandboxNotReady    = errors.New("sandbox data plane is not ready")
	ErrFastletUnavailable = errors.New("assigned Fastlet Pod is unavailable")
)

type Route struct {
	Namespace         string
	SandboxUID        string
	FastletName       string
	FastletPodUID     string
	FastletPodIP      string
	AssignmentAttempt int64
	RouteGeneration   int64
}

type Resolver interface {
	Resolve(context.Context, string) (Route, error)
	ResolveFresh(context.Context, string) (Route, error)
}

type Index struct {
	mu        sync.RWMutex
	sandboxes map[string]apiv1alpha1.Sandbox
	pods      map[types.UID]corev1.Pod
}

func NewIndex() *Index {
	return &Index{sandboxes: make(map[string]apiv1alpha1.Sandbox), pods: make(map[types.UID]corev1.Pod)}
}

func (i *Index) UpsertSandbox(sandbox *apiv1alpha1.Sandbox) {
	if sandbox == nil || sandbox.UID == "" {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.sandboxes[string(sandbox.UID)] = *sandbox.DeepCopy()
}

func (i *Index) DeleteSandbox(sandbox *apiv1alpha1.Sandbox) {
	if sandbox == nil || sandbox.UID == "" {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.sandboxes, string(sandbox.UID))
}

func (i *Index) UpsertPod(pod *corev1.Pod) {
	if pod == nil || pod.UID == "" {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	i.pods[pod.UID] = *pod.DeepCopy()
}

func (i *Index) DeletePod(pod *corev1.Pod) {
	if pod == nil || pod.UID == "" {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.pods, pod.UID)
}

func (i *Index) Resolve(sandboxUID string) (Route, error) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	sandbox, exists := i.sandboxes[sandboxUID]
	if !exists {
		return Route{}, ErrSandboxNotFound
	}
	if sandbox.Status.Assignment == nil || sandbox.Status.DataPlaneState != apiv1alpha1.ObservedStateReady {
		return Route{}, ErrSandboxNotReady
	}
	pod, exists := i.pods[types.UID(sandbox.Status.Assignment.FastletPodUID)]
	if !exists || pod.Name != sandbox.Status.Assignment.FastletName || pod.Namespace != sandbox.Namespace || pod.Status.PodIP == "" {
		return Route{}, ErrFastletUnavailable
	}
	return routeFromObjects(&sandbox, &pod)
}

type KubernetesResolver struct {
	Index  *Index
	Client client.Client
}

func (r *KubernetesResolver) Resolve(ctx context.Context, sandboxUID string) (Route, error) {
	if r.Index != nil {
		if route, err := r.Index.Resolve(sandboxUID); err == nil {
			return route, nil
		}
	}
	return r.ResolveFresh(ctx, sandboxUID)
}

// ResolveFresh is the bounded, authoritative read-after-create fallback. It
// runs only on a cache miss/lag or credential-generation mismatch; the steady
// state stays entirely watch-driven.
func (r *KubernetesResolver) ResolveFresh(ctx context.Context, sandboxUID string) (Route, error) {
	if r.Client == nil || sandboxUID == "" {
		return Route{}, ErrSandboxNotFound
	}
	var sandboxes apiv1alpha1.SandboxList
	if err := r.Client.List(ctx, &sandboxes); err != nil {
		return Route{}, fmt.Errorf("list Sandboxes for UID fallback: %w", err)
	}
	var sandbox *apiv1alpha1.Sandbox
	for index := range sandboxes.Items {
		if string(sandboxes.Items[index].UID) == sandboxUID {
			sandbox = sandboxes.Items[index].DeepCopy()
			break
		}
	}
	if sandbox == nil {
		return Route{}, ErrSandboxNotFound
	}
	if sandbox.Status.Assignment == nil || sandbox.Status.DataPlaneState != apiv1alpha1.ObservedStateReady {
		return Route{}, ErrSandboxNotReady
	}
	var pod corev1.Pod
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Status.Assignment.FastletName}, &pod); err != nil {
		return Route{}, fmt.Errorf("%w: %v", ErrFastletUnavailable, err)
	}
	if string(pod.UID) != sandbox.Status.Assignment.FastletPodUID || pod.Status.PodIP == "" {
		return Route{}, ErrFastletUnavailable
	}
	if r.Index != nil {
		r.Index.UpsertSandbox(sandbox)
		r.Index.UpsertPod(&pod)
	}
	return routeFromObjects(sandbox, &pod)
}

func routeFromObjects(sandbox *apiv1alpha1.Sandbox, pod *corev1.Pod) (Route, error) {
	if sandbox.Status.Assignment == nil {
		return Route{}, ErrSandboxNotReady
	}
	generation := sandbox.Status.RouteGeneration
	if generation <= 0 {
		generation = 1
	}
	return Route{
		Namespace: sandbox.Namespace, SandboxUID: string(sandbox.UID),
		FastletName: sandbox.Status.Assignment.FastletName, FastletPodUID: sandbox.Status.Assignment.FastletPodUID,
		FastletPodIP: pod.Status.PodIP, AssignmentAttempt: sandbox.Status.Assignment.Attempt,
		RouteGeneration: generation,
	}, nil
}
