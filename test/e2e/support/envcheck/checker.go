package envcheck

import (
	"context"
	"fmt"
	goruntime "runtime"
	"strings"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/klient/conf"
)

// RuntimeSupportInfo contains information about runtime support.
type RuntimeSupportInfo struct {
	// Available RuntimeClasses
	AvailableRuntimeClasses map[string]bool
	// Node supports KVM (detected via node labels or RuntimeClass)
	KVMSupportDetected bool
	// Kind cluster name (if running in kind)
	KindClusterName string
}

// Checker provides runtime environment checking capabilities.
type Checker struct {
	k8sClient client.Client
	info      *RuntimeSupportInfo
}

// NewChecker creates a new environment checker.
func NewChecker() (*Checker, error) {
	cfg, err := conf.New("")
	if err != nil {
		return nil, fmt.Errorf("resolve kubeconfig: %w", err)
	}

	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("add core scheme: %w", err)
	}
	// Add corev1 for Node access
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("add corev1 scheme: %w", err)
	}
	scheme.AddKnownTypes(schema.GroupVersion{Group: "node.k8s.io", Version: "v1"}, &nodev1.RuntimeClass{}, &nodev1.RuntimeClassList{})

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create kube client: %w", err)
	}

	return &Checker{k8sClient: k8sClient}, nil
}

// Check performs environment checks and caches results.
func (c *Checker) Check(ctx context.Context) (*RuntimeSupportInfo, error) {
	if c.info != nil {
		return c.info, nil
	}

	info := &RuntimeSupportInfo{
		AvailableRuntimeClasses: make(map[string]bool),
	}

	// Check available RuntimeClasses
	runtimeClasses := &nodev1.RuntimeClassList{}
	if err := c.k8sClient.List(ctx, runtimeClasses); err == nil {
		for _, rc := range runtimeClasses.Items {
			info.AvailableRuntimeClasses[rc.Name] = true
			// If any kata RuntimeClass exists, assume KVM is supported
			if strings.HasPrefix(rc.Name, "kata-") {
				info.KVMSupportDetected = true
			}
		}
	}

	// Check nodes for kind label to detect kind cluster
	nodes := &corev1.NodeList{}
	if err := c.k8sClient.List(ctx, nodes); err == nil {
		for _, node := range nodes.Items {
			if name, ok := node.Labels["kubernetes.io/hostname"]; ok {
				if strings.Contains(name, "-control-plane") {
					// Extract kind cluster name from node name
					parts := strings.Split(name, "-control-plane")
					if len(parts) > 0 {
						info.KindClusterName = parts[0]
					}
				}
			}
		}
	}

	c.info = info
	return info, nil
}

// ShouldRunKataClh determines if kata-clh tests should run.
// kata-clh (Cloud Hypervisor) works well in nested virtualization.
func (c *Checker) ShouldRunKataClh(ctx context.Context) (bool, string) {
	info, err := c.Check(ctx)
	if err != nil {
		return false, fmt.Sprintf("environment check failed: %v", err)
	}

	if !info.AvailableRuntimeClasses["kata-clh"] {
		return false, "RuntimeClass 'kata-clh' not found"
	}

	return true, "RuntimeClass 'kata-clh' present"
}

// ShouldRunKataQemu determines if kata-qemu tests should run.
// kata-qemu has vsock issues in nested virtualization environments.
// In kind clusters (which run in Docker containers, i.e., nested virtualization),
// we skip kata-qemu tests.
func (c *Checker) ShouldRunKataQemu(ctx context.Context) (bool, string) {
	info, err := c.Check(ctx)
	if err != nil {
		return false, fmt.Sprintf("environment check failed: %v", err)
	}

	if !info.AvailableRuntimeClasses["kata-qemu"] {
		return false, "RuntimeClass 'kata-qemu' not found"
	}

	// Running in kind means nested virtualization, skip kata-qemu
	if info.KindClusterName != "" {
		return false, "running in kind cluster - nested virtualization, kata-qemu vsock may not work"
	}

	return true, "RuntimeClass 'kata-qemu' present, not in nested environment"
}

// ShouldRunGVisor determines if gVisor tests should run.
// gVisor uses user-space kernel (runsc), so it works without KVM
// and can run in nested virtualization environments like kind.
func (c *Checker) ShouldRunGVisor(ctx context.Context) (bool, string) {
	info, err := c.Check(ctx)
	if err != nil {
		return false, fmt.Sprintf("environment check failed: %v", err)
	}

	// Check RuntimeClass
	if !info.AvailableRuntimeClasses["gvisor"] {
		return false, "RuntimeClass 'gvisor' not found"
	}

	return true, "RuntimeClass 'gvisor' present, gVisor works in nested environments"
}

// ShouldRunKataFc determines if kata-fc (Firecracker) tests should run.
func (c *Checker) ShouldRunKataFc(ctx context.Context) (bool, string) {
	info, err := c.Check(ctx)
	if err != nil {
		return false, fmt.Sprintf("environment check failed: %v", err)
	}

	if !info.AvailableRuntimeClasses["kata-fc"] {
		return false, "RuntimeClass 'kata-fc' not found"
	}

	return true, "RuntimeClass 'kata-fc' present"
}

// RuntimeClassExists checks if a RuntimeClass exists.
func (c *Checker) RuntimeClassExists(ctx context.Context, name string) (bool, error) {
	runtimeClass := &nodev1.RuntimeClass{}
	err := c.k8sClient.Get(ctx, client.ObjectKey{Name: name}, runtimeClass)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// GetInfo returns cached environment info or performs check.
func (c *Checker) GetInfo(ctx context.Context) (*RuntimeSupportInfo, error) {
	return c.Check(ctx)
}

// Global checker instance
var globalChecker *Checker

// GetChecker returns the global checker instance.
func GetChecker() (*Checker, error) {
	if globalChecker != nil {
		return globalChecker, nil
	}

	checker, err := NewChecker()
	if err != nil {
		return nil, err
	}
	globalChecker = checker
	return checker, nil
}

// GetSystemInfo returns a summary of system information for debugging.
func GetSystemInfo() map[string]string {
	info := make(map[string]string)
	info["os"] = goruntime.GOOS
	info["arch"] = goruntime.GOARCH
	return info
}
