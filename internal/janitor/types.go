package janitor

import (
	"sync"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"k8s.io/client-go/kubernetes"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultOrphanTimeout = 30 * time.Second
)

type Janitor struct {
	kubeClient kubernetes.Interface
	K8sClient  client.Client
	ctrdClient *containerd.Client
	nodeName   string
	namespaces []string

	queue     workqueue.RateLimitingInterface
	podLister listerv1.PodLister

	cleaning sync.Map // containerID -> struct{}{}

	ScanInterval time.Duration

	OrphanTimeout time.Duration
}

type CleanupTask struct {
	ContainerID     string
	FastletUID      string
	PodName         string
	Namespace       string
	SandboxName     string
	SandboxNotFound bool
}
