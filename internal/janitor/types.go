package janitor

import (
	"context"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const defaultOrphanTimeout = 30 * time.Second

type ResourceBackend string

const (
	BackendContainerd   ResourceBackend = "containerd"
	BackendLinuxNetwork ResourceBackend = "linux-network"
	BackendBoxLite      ResourceBackend = "boxlite"
)

// ResourceIdentity is the common fencing identity for every node-local
// resource. A backend may delete a resource only after Janitor proves that the
// exact Fastlet Pod and Sandbox assignment which created it are no longer live.
type ResourceIdentity struct {
	Backend             ResourceBackend
	ResourceID          string
	FastletPodUID       string
	FastletPodName      string
	FastletPodNamespace string
	SandboxUID          string
	SandboxName         string
	SandboxNamespace    string
	InstanceGeneration  int64
	AssignmentAttempt   int64
	RouteGeneration     int64
	CreatedAt           time.Time
	NetworkSlotID       string
	NetworkStatePodUID  string
}

type CleanupBackend interface {
	Name() ResourceBackend
	Scan(context.Context) ([]ResourceIdentity, error)
	Cleanup(context.Context, ResourceIdentity) error
}

type CleanupTask struct {
	Resource ResourceIdentity
}

type CleanupDecision struct {
	Eligible bool
	Reason   string
}

type Janitor struct {
	kubeClient kubernetes.Interface
	K8sClient  client.Client
	nodeName   string

	queue    workqueue.RateLimitingInterface
	scanMu   sync.Mutex
	backends []CleanupBackend

	ScanInterval  time.Duration
	OrphanTimeout time.Duration
	Now           func() time.Time
}

func (j *Janitor) AddBackend(backend CleanupBackend) {
	if backend != nil {
		j.backends = append(j.backends, backend)
	}
}

func (j *Janitor) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

func (j *Janitor) orphanTimeout() time.Duration {
	if j.OrphanTimeout > 0 {
		return j.OrphanTimeout
	}
	return defaultOrphanTimeout
}
