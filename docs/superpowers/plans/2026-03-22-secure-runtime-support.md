# Secure Runtime Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add gVisor and Kata Containers support to Fast Sandbox with Pool-level configuration and RuntimeClass validation.

**Architecture:** Each SandboxPool specifies a runtime type (gvisor, kata-qemu, kata-fc, kata-clh). The Controller validates RuntimeClass existence and passes runtime configuration to Agent Pods via environment variables. Agent Pods run on runc and create sandbox containers using the specified secure runtime through containerd API.

**Tech Stack:** Go 1.25, Kubernetes controller-runtime, containerd v2 API, gVisor runsc, Kata Containers

**Design Doc:** `docs/secure-runtime-support-design.md`

---

## File Structure

| File | Action | Purpose |
|------|--------|---------|
| `api/v1alpha1/sandboxpool_types.go` | Modify | Add new RuntimeType constants, RuntimeClassName, ContainerdRuntimeHandler fields |
| `internal/agent/runtime/runtime.go` | Modify | Add Kata runtime types, update NewRuntime() function |
| `internal/agent/runtime/containerd_runtime.go` | Modify | No changes needed (already uses runtimeHandler) |
| `internal/controller/sandboxpool_controller.go` | Modify | Add RuntimeClass validation, update env vars, add helper functions |
| `internal/controller/sandboxpool_controller_test.go` | Modify | Add unit tests for runtime validation |
| `config/samples/pool.yaml` | Modify | Update sample with runtimeType examples |
| `test/e2e/suites/secureruntime/suite_test.go` | Create | E2E suite setup with runtime detection |
| `test/e2e/suites/secureruntime/gvisor_test.go` | Create | gVisor E2E tests |
| `test/e2e/suites/secureruntime/kata_test.go` | Create | Kata E2E tests |
| `test/e2e/suites/secureruntime/runtime_validation_test.go` | Create | Runtime validation tests |

---

## Task 1: Update CRD Types

**Files:**
- Modify: `api/v1alpha1/sandboxpool_types.go`

- [ ] **Step 1: Add new RuntimeType constants**

Replace the existing RuntimeType constants with expanded set:

```go
// RuntimeType defines the isolation level for sandboxes in this pool.
type RuntimeType string

const (
	// RuntimeContainer is the default runc runtime (process-level isolation).
	RuntimeContainer RuntimeType = "container"
	// RuntimeGVisor uses gVisor with runsc (user-space kernel).
	RuntimeGVisor RuntimeType = "gvisor"
	// RuntimeKataQemu uses Kata Containers with QEMU hypervisor.
	RuntimeKataQemu RuntimeType = "kata-qemu"
	// RuntimeKataFc uses Kata Containers with Firecracker microVM.
	RuntimeKataFc RuntimeType = "kata-fc"
	// RuntimeKataClh uses Kata Containers with Cloud Hypervisor.
	RuntimeKataClh RuntimeType = "kata-clh"
)
```

- [ ] **Step 2: Add new fields to SandboxPoolSpec**

Add RuntimeClassName and ContainerdRuntimeHandler fields after RuntimeType:

```go
type SandboxPoolSpec struct {
	Capacity PoolCapacity `json:"capacity"`

	MaxSandboxesPerPod int32 `json:"maxSandboxesPerPod,omitempty"`

	// RuntimeType specifies the secure runtime type for this pool.
	// Default: "container" (standard runc)
	// +kubebuilder:default=container
	RuntimeType RuntimeType `json:"runtimeType,omitempty"`

	// RuntimeClassName specifies the Kubernetes RuntimeClass to use for validation.
	// If not set, defaults to the string representation of RuntimeType.
	// Ignored when RuntimeType is "container".
	RuntimeClassName string `json:"runtimeClassName,omitempty"`

	// ContainerdRuntimeHandler overrides the containerd runtime handler.
	// If not set, defaults based on RuntimeType.
	ContainerdRuntimeHandler string `json:"containerdRuntimeHandler,omitempty"`

	AgentTemplate corev1.PodTemplateSpec `json:"agentTemplate"`
}
```

- [ ] **Step 3: Add Pool condition constants**

Add after the SandboxPoolStatus struct:

```go
// Pool condition types
const (
	PoolConditionRuntimeReady = "RuntimeReady"
)

// Pool condition reasons
const (
	ReasonRuntimeAvailable   = "RuntimeAvailable"
	ReasonRuntimeUnavailable = "RuntimeUnavailable"
)
```

- [ ] **Step 4: Verify compilation**

```bash
cd /home/fengjianhui.fjh/fast-sandbox && go build ./api/...
```

Expected: No errors

- [ ] **Step 5: Commit CRD changes**

```bash
git add api/v1alpha1/sandboxpool_types.go
git commit -m "feat(api): add Kata runtime types and RuntimeClassName to SandboxPool CRD

- Add RuntimeKataQemu, RuntimeKataFc, RuntimeKataClh constants
- Add RuntimeClassName field for Kubernetes RuntimeClass validation
- Add ContainerdRuntimeHandler field for custom handler override
- Add PoolConditionRuntimeReady condition type

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 2: Update Runtime Package

**Files:**
- Modify: `internal/agent/runtime/runtime.go`
- Modify: `internal/agent/runtime/errors.go`

- [ ] **Step 1: Update RuntimeType constants in runtime.go**

Replace existing constants:

```go
type RuntimeType string

const (
	RuntimeTypeContainer RuntimeType = "container"
	RuntimeTypeGVisor    RuntimeType = "gvisor"
	RuntimeTypeKataQemu  RuntimeType = "kata-qemu"
	RuntimeTypeKataFc    RuntimeType = "kata-fc"
	RuntimeTypeKataClh   RuntimeType = "kata-clh"
)
```

- [ ] **Step 2: Add default runtime handler mapping**

Add after the constants:

```go
// defaultRuntimeHandlers maps RuntimeType to containerd runtime handler.
var defaultRuntimeHandlers = map[RuntimeType]string{
	RuntimeTypeContainer: "io.containerd.runc.v2",
	RuntimeTypeGVisor:    "io.containerd.runsc.v1",
	RuntimeTypeKataQemu:  "io.containerd.kata-qemu.v2",
	RuntimeTypeKataFc:    "io.containerd.kata-fc.v2",
	RuntimeTypeKataClh:   "io.containerd.kata-clh.v2",
}

// GetRuntimeHandler returns the containerd runtime handler for the given type.
func GetRuntimeHandler(rt RuntimeType) string {
	if handler, ok := defaultRuntimeHandlers[rt]; ok {
		return handler
	}
	return defaultRuntimeHandlers[RuntimeTypeContainer]
}
```

- [ ] **Step 3: Update NewRuntime function**

Replace the existing NewRuntime function:

```go
func NewRuntime(ctx context.Context, runtimeType RuntimeType, socketPath string) (Runtime, error) {
	handler := GetRuntimeHandler(runtimeType)

	var rt Runtime
	switch runtimeType {
	case RuntimeTypeContainer, RuntimeTypeGVisor,
		RuntimeTypeKataQemu, RuntimeTypeKataFc, RuntimeTypeKataClh:
		rt = newContainerdRuntime(handler)
	default:
		return nil, ErrUnsupportedRuntime
	}

	if err := rt.Initialize(ctx, socketPath); err != nil {
		return nil, err
	}
	return rt, nil
}
```

- [ ] **Step 4: Verify compilation**

```bash
cd /home/fengjianhui.fjh/fast-sandbox && go build ./internal/agent/runtime/...
```

Expected: No errors

- [ ] **Step 5: Commit runtime changes**

```bash
git add internal/agent/runtime/runtime.go
git commit -m "feat(runtime): add Kata runtime types and handler mapping

- Add RuntimeTypeKataQemu, RuntimeTypeKataFc, RuntimeTypeKataClh
- Add defaultRuntimeHandlers mapping
- Add GetRuntimeHandler helper function
- Update NewRuntime to support all runtime types

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 3: Update SandboxPoolController

**Files:**
- Modify: `internal/controller/sandboxpool_controller.go`
- Modify: `internal/controller/sandboxpool_controller_test.go`

- [ ] **Step 1: Add imports for RuntimeClass**

Add to imports:

```go
import (
	// ... existing imports ...
	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/errors"
)
```

- [ ] **Step 2: Add helper functions for runtime config**

Add after the existing helper functions:

```go
// getRuntimeClassName returns the RuntimeClassName for the pool.
// Returns empty string for default container runtime.
func getRuntimeClassName(pool *apiv1alpha1.SandboxPool) string {
	if pool.Spec.RuntimeType == "" || pool.Spec.RuntimeType == apiv1alpha1.RuntimeContainer {
		return ""
	}
	if pool.Spec.RuntimeClassName != "" {
		return pool.Spec.RuntimeClassName
	}
	return string(pool.Spec.RuntimeType)
}

// getRuntimeHandler returns the containerd runtime handler for the pool.
func getRuntimeHandler(pool *apiv1alpha1.SandboxPool) string {
	if pool.Spec.ContainerdRuntimeHandler != "" {
		return pool.Spec.ContainerdRuntimeHandler
	}
	return runtime.GetRuntimeHandler(runtime.RuntimeType(pool.Spec.RuntimeType))
}
```

- [ ] **Step 3: Add RuntimeClass validation method**

Add after constructPod:

```go
// validateRuntimeClass checks if the specified RuntimeClass exists.
func (r *SandboxPoolReconciler) validateRuntimeClass(ctx context.Context, pool *apiv1alpha1.SandboxPool) error {
	runtimeClassName := getRuntimeClassName(pool)
	if runtimeClassName == "" {
		return nil // No validation needed for default runtime
	}

	runtimeClass := &nodev1.RuntimeClass{}
	if err := r.Get(ctx, client.ObjectKey{Name: runtimeClassName}, runtimeClass); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("RuntimeClass %q not found", runtimeClassName)
		}
		return fmt.Errorf("failed to get RuntimeClass %q: %w", runtimeClassName, err)
	}
	return nil
}

// updatePoolCondition updates a condition on the pool status.
func (r *SandboxPoolReconciler) updatePoolCondition(ctx context.Context, pool *apiv1alpha1.SandboxPool, condition metav1.Condition) error {
	condition.LastTransitionTime = metav1.Now()

	found := false
	for i, c := range pool.Status.Conditions {
		if c.Type == condition.Type {
			pool.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		pool.Status.Conditions = append(pool.Status.Conditions, condition)
	}

	return r.Status().Update(ctx, pool)
}
```

- [ ] **Step 4: Update Reconcile to validate RuntimeClass**

Modify the Reconcile function to add validation after fetching the pool:

```go
func (r *SandboxPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := klog.FromContext(ctx)

	var pool apiv1alpha1.SandboxPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Validate RuntimeClass if specified
	if err := r.validateRuntimeClass(ctx, &pool); err != nil {
		logger.Error(err, "RuntimeClass validation failed")
		_ = r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type:    apiv1alpha1.PoolConditionRuntimeReady,
			Status:  metav1.ConditionFalse,
			Reason:  apiv1alpha1.ReasonRuntimeUnavailable,
			Message: err.Error(),
		})
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Update condition to ready if using secure runtime
	if getRuntimeClassName(&pool) != "" {
		_ = r.updatePoolCondition(ctx, &pool, metav1.Condition{
			Type:    apiv1alpha1.PoolConditionRuntimeReady,
			Status:  metav1.ConditionTrue,
			Reason:  apiv1alpha1.ReasonRuntimeAvailable,
			Message: fmt.Sprintf("RuntimeClass %s is available", getRuntimeClassName(&pool)),
		})
	}

	// ... rest of existing Reconcile logic ...
```

- [ ] **Step 5: Update constructPod to pass runtime env vars**

Modify the env vars section in constructPod:

```go
		c.Env = append(c.Env,
			// ... existing env vars ...
			corev1.EnvVar{
				Name:  "RUNTIME_TYPE",
				Value: string(getRuntimeType(pool)),
			},
			corev1.EnvVar{
				Name:  "RUNTIME_HANDLER",
				Value: getRuntimeHandler(pool),
			},
			corev1.EnvVar{Name: "RUNTIME_SOCKET", Value: "/run/containerd/containerd.sock"},
			corev1.EnvVar{Name: "INFRA_DIR_IN_POD", Value: "/opt/fast-sandbox/infra"},
		)
```

- [ ] **Step 6: Write unit test for runtime validation**

Create test in `internal/controller/sandboxpool_controller_test.go`:

```go
func TestGetRuntimeClassName(t *testing.T) {
	tests := []struct {
		name     string
		pool     *apiv1alpha1.SandboxPool
		expected string
	}{
		{
			name: "default container runtime returns empty",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: apiv1alpha1.RuntimeContainer},
			},
			expected: "",
		},
		{
			name: "gvisor returns gvisor",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: apiv1alpha1.RuntimeGVisor},
			},
			expected: "gvisor",
		},
		{
			name: "kata-qemu returns kata-qemu",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: apiv1alpha1.RuntimeKataQemu},
			},
			expected: "kata-qemu",
		},
		{
			name: "custom RuntimeClassName overrides default",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{
					RuntimeType:      apiv1alpha1.RuntimeGVisor,
					RuntimeClassName: "custom-gvisor",
				},
			},
			expected: "custom-gvisor",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getRuntimeClassName(tt.pool)
			if result != tt.expected {
				t.Errorf("getRuntimeClassName() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestGetRuntimeHandler(t *testing.T) {
	tests := []struct {
		name     string
		pool     *apiv1alpha1.SandboxPool
		expected string
	}{
		{
			name: "container returns runc handler",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: apiv1alpha1.RuntimeContainer},
			},
			expected: "io.containerd.runc.v2",
		},
		{
			name: "gvisor returns runsc handler",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: apiv1alpha1.RuntimeGVisor},
			},
			expected: "io.containerd.runsc.v1",
		},
		{
			name: "kata-qemu returns kata handler",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{RuntimeType: apiv1alpha1.RuntimeKataQemu},
			},
			expected: "io.containerd.kata-qemu.v2",
		},
		{
			name: "custom handler overrides default",
			pool: &apiv1alpha1.SandboxPool{
				Spec: apiv1alpha1.SandboxPoolSpec{
					RuntimeType:             apiv1alpha1.RuntimeGVisor,
					ContainerdRuntimeHandler: "custom-handler",
				},
			},
			expected: "custom-handler",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := getRuntimeHandler(tt.pool)
			if result != tt.expected {
				t.Errorf("getRuntimeHandler() = %q, want %q", result, tt.expected)
			}
		})
	}
}
```

- [ ] **Step 7: Run unit tests**

```bash
cd /home/fengjianhui.fjh/fast-sandbox && go test ./internal/controller/... -v -run "TestGetRuntime"
```

Expected: All tests pass

- [ ] **Step 8: Verify full compilation**

```bash
cd /home/fengjianhui.fjh/fast-sandbox && go build ./...
```

Expected: No errors

- [ ] **Step 9: Commit controller changes**

```bash
git add internal/controller/sandboxpool_controller.go internal/controller/sandboxpool_controller_test.go
git commit -m "feat(controller): add RuntimeClass validation for secure runtimes

- Add getRuntimeClassName() and getRuntimeHandler() helpers
- Add validateRuntimeClass() to check RuntimeClass existence
- Add updatePoolCondition() for status updates
- Update Reconcile to validate RuntimeClass and update conditions
- Pass RUNTIME_HANDLER env var to Agent Pods
- Add unit tests for runtime config helpers

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 4: Update Sample YAML

**Files:**
- Modify: `config/samples/pool.yaml`

- [ ] **Step 1: Update pool.yaml with examples**

```yaml
apiVersion: sandbox.fast.io/v1alpha1
kind: SandboxPool
metadata:
  name: default-pool
  namespace: default
spec:
  agentTemplate:
    spec:
      containers:
      - image: fast-sandbox/agent:dev
        imagePullPolicy: IfNotPresent
        name: agent
  capacity:
    poolMax: 1
    poolMin: 1
  maxSandboxesPerPod: 5
  runtimeType: container
---
# Example: gVisor pool for untrusted code execution
apiVersion: sandbox.fast.io/v1alpha1
kind: SandboxPool
metadata:
  name: gvisor-pool
  namespace: default
spec:
  agentTemplate:
    spec:
      containers:
      - image: fast-sandbox/agent:dev
        imagePullPolicy: IfNotPresent
        name: agent
  capacity:
    poolMax: 3
    poolMin: 1
  maxSandboxesPerPod: 5
  runtimeType: gvisor
---
# Example: Kata QEMU pool for maximum isolation
apiVersion: sandbox.fast.io/v1alpha1
kind: SandboxPool
metadata:
  name: kata-pool
  namespace: default
spec:
  agentTemplate:
    spec:
      containers:
      - image: fast-sandbox/agent:dev
        imagePullPolicy: IfNotPresent
        name: agent
  capacity:
    poolMax: 2
    poolMin: 1
  maxSandboxesPerPod: 3
  runtimeType: kata-qemu
```

- [ ] **Step 2: Commit sample updates**

```bash
git add config/samples/pool.yaml
git commit -m "docs: add gVisor and Kata pool examples

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 5: Create E2E Test Suite

**Files:**
- Create: `test/e2e/suites/secureruntime/suite_test.go`
- Create: `test/e2e/suites/secureruntime/gvisor_test.go`
- Create: `test/e2e/suites/secureruntime/kata_test.go`
- Create: `test/e2e/suites/secureruntime/runtime_validation_test.go`

- [ ] **Step 1: Create suite_test.go**

```go
package secureruntime

import (
	"os"
	"testing"

	nodev1 "k8s.io/api/node/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/klient/conf"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/test/e2e/support/suiteenv"
)

var testSuite = suiteenv.New()

func TestMain(m *testing.M) {
	os.Exit(testSuite.Env().Run(m))
}

// SecureRuntimeTestClient provides helpers for secure runtime tests.
type SecureRuntimeTestClient struct {
	client client.Client
	scheme *runtime.Scheme
}

// MustSecureRuntimeClient creates a test client with RuntimeClass support.
func MustSecureRuntimeClient(t *testing.T) *SecureRuntimeTestClient {
	t.Helper()

	cfg, err := conf.New(testSuite.Config().KubeconfigFile())
	if err != nil {
		t.Fatalf("resolve kubeconfig: %v", err)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := apiv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add fast-sandbox scheme: %v", err)
	}
	// Add RuntimeClass scheme
	if err := scheme.AddGeneratedNameFunc(scheme.Internals()); err != nil {
		t.Fatalf("add scheme internals: %v", err)
	}
	scheme.AddKnownTypes(schema.GroupVersion{Group: "node.k8s.io", Version: "v1"}, &nodev1.RuntimeClass{}, &nodev1.RuntimeClassList{})

	k8sClient, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create kube client: %v", err)
	}

	return &SecureRuntimeTestClient{client: k8sClient, scheme: scheme}
}

// RuntimeClassExists checks if a RuntimeClass exists.
func (c *SecureRuntimeTestClient) RuntimeClassExists(ctx context.Context, name string) (bool, error) {
	runtimeClass := &nodev1.RuntimeClass{}
	err := c.client.Get(ctx, client.ObjectKey{Name: name}, runtimeClass)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// SkipIfRuntimeClassNotExists skips the test if RuntimeClass doesn't exist.
func (c *SecureRuntimeTestClient) SkipIfRuntimeClassNotExists(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	exists, err := c.RuntimeClassExists(ctx, name)
	if err != nil {
		t.Fatalf("check RuntimeClass: %v", err)
	}
	if !exists {
		t.Skipf("RuntimeClass %q not found, skipping test", name)
	}
}

func (c *SecureRuntimeTestClient) Client() client.Client {
	return c.client
}
```

- [ ] **Step 2: Create gvisor_test.go**

```go
package secureruntime

import (
	"context"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestGVisorSandbox(t *testing.T) {
	suiteenv.SkipUnlessEnabled(t)

	feature := features.New("gvisor-sandbox").
		WithLabel("suite", "secureruntime").
		WithLabel("runtime", "gvisor").
		Assess("gVisor pool creates sandbox successfully", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			secureClient := MustSecureRuntimeClient(t)
			secureClient.SkipIfRuntimeClassNotExists(t, ctx, "gvisor")

			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("gvisor")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			// Create gVisor pool
			pool := newSecureRuntimePool(namespace, "gvisor-pool", apiv1alpha1.RuntimeGVisor, 1, 1)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create gvisor pool: %v", err)
			}

			// Wait for ready agent pods
			poolWaitCtx, cancelPoolWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelPoolWait()
			if _, err := fixture.WaitForReadyAgentPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready agent pods: %v", err)
			}

			// Create sandbox
			sandbox := newSecureRuntimeSandbox(namespace, "sb-gvisor", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			// Wait for sandbox running
			runCtx, cancelRunWait := context.WithTimeout(ctx, 60*time.Second)
			defer cancelRunWait()
			_, err := fixture.WaitForSandbox(runCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.AssignedPod != "" &&
					(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
			})
			if err != nil {
				t.Fatalf("wait for running sandbox: %v", err)
			}

			// Verify gVisor runtime (check Pool condition)
			updatedPool := &apiv1alpha1.SandboxPool{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, updatedPool); err != nil {
				t.Fatalf("get updated pool: %v", err)
			}

			// Check RuntimeReady condition
			var runtimeReady *metav1.Condition
			for _, c := range updatedPool.Status.Conditions {
				if c.Type == apiv1alpha1.PoolConditionRuntimeReady {
					runtimeReady = &c
					break
				}
			}
			if runtimeReady == nil || runtimeReady.Status != metav1.ConditionTrue {
				t.Errorf("expected RuntimeReady condition to be True, got: %v", runtimeReady)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func newSecureRuntimePool(namespace, name string, runtimeType apiv1alpha1.RuntimeType, min, max int32) *apiv1alpha1.SandboxPool {
	return &apiv1alpha1.SandboxPool{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1alpha1.GroupVersion.String(),
			Kind:       "SandboxPool",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: apiv1alpha1.SandboxPoolSpec{
			Capacity: apiv1alpha1.PoolCapacity{
				PoolMin: min,
				PoolMax: max,
			},
			MaxSandboxesPerPod: 5,
			RuntimeType:        runtimeType,
			AgentTemplate: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "agent",
						Image: suiteenv.AgentImage(),
					}},
				},
			},
		},
	}
}

func newSecureRuntimeSandbox(namespace, name, pool string) *apiv1alpha1.Sandbox {
	return &apiv1alpha1.Sandbox{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiv1alpha1.GroupVersion.String(),
			Kind:       "Sandbox",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: apiv1alpha1.SandboxSpec{
			Image:   "docker.io/library/alpine:latest",
			Command: []string{"/bin/sleep", "60"},
			PoolRef: pool,
		},
	}
}
```

- [ ] **Step 3: Create kata_test.go**

```go
package secureruntime

import (
	"context"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestKataQemuSandbox(t *testing.T) {
	suiteenv.SkipUnlessEnabled(t)

	feature := features.New("kata-qemu-sandbox").
		WithLabel("suite", "secureruntime").
		WithLabel("runtime", "kata").
		Assess("Kata QEMU pool creates sandbox successfully", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			secureClient := MustSecureRuntimeClient(t)
			secureClient.SkipIfRuntimeClassNotExists(t, ctx, "kata-qemu")

			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("kata-qemu")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			// Create Kata QEMU pool
			pool := newSecureRuntimePool(namespace, "kata-qemu-pool", apiv1alpha1.RuntimeKataQemu, 1, 1)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create kata pool: %v", err)
			}

			// Wait for ready agent pods
			poolWaitCtx, cancelPoolWait := context.WithTimeout(ctx, 120*time.Second) // Kata needs more time
			defer cancelPoolWait()
			if _, err := fixture.WaitForReadyAgentPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready agent pods: %v", err)
			}

			// Create sandbox
			sandbox := newSecureRuntimeSandbox(namespace, "sb-kata-qemu", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			// Wait for sandbox running (Kata takes longer)
			runCtx, cancelRunWait := context.WithTimeout(ctx, 120*time.Second)
			defer cancelRunWait()
			_, err := fixture.WaitForSandbox(runCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.AssignedPod != "" &&
					(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
			})
			if err != nil {
				t.Fatalf("wait for running sandbox: %v", err)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}

func TestKataFcSandbox(t *testing.T) {
	suiteenv.SkipUnlessEnabled(t)

	feature := features.New("kata-fc-sandbox").
		WithLabel("suite", "secureruntime").
		WithLabel("runtime", "kata-fc").
		Assess("Kata Firecracker pool creates sandbox successfully", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			secureClient := MustSecureRuntimeClient(t)
			secureClient.SkipIfRuntimeClassNotExists(t, ctx, "kata-fc")

			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("kata-fc")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			pool := newSecureRuntimePool(namespace, "kata-fc-pool", apiv1alpha1.RuntimeKataFc, 1, 1)
			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create kata-fc pool: %v", err)
			}

			poolWaitCtx, cancelPoolWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelPoolWait()
			if _, err := fixture.WaitForReadyAgentPods(poolWaitCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, 1); err != nil {
				t.Fatalf("wait for ready agent pods: %v", err)
			}

			sandbox := newSecureRuntimeSandbox(namespace, "sb-kata-fc", pool.Name)
			if _, err := fixture.CreateSandbox(ctx, namespace, sandbox); err != nil {
				t.Fatalf("create sandbox: %v", err)
			}

			runCtx, cancelRunWait := context.WithTimeout(ctx, 90*time.Second)
			defer cancelRunWait()
			_, err := fixture.WaitForSandbox(runCtx, types.NamespacedName{Name: sandbox.Name, Namespace: namespace}, func(sb *apiv1alpha1.Sandbox) bool {
				return sb.Status.AssignedPod != "" &&
					(sb.Status.Phase == string(apiv1alpha1.PhaseBound) || sb.Status.Phase == string(apiv1alpha1.PhaseRunning))
			})
			if err != nil {
				t.Fatalf("wait for running sandbox: %v", err)
			}

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}
```

- [ ] **Step 4: Create runtime_validation_test.go**

```go
package secureruntime

import (
	"context"
	"testing"
	"time"

	apiv1alpha1 "fast-sandbox/api/v1alpha1"
	"fast-sandbox/test/e2e/support/fixtures"
	"fast-sandbox/test/e2e/support/suiteenv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

func TestInvalidRuntimeClass(t *testing.T) {
	suiteenv.SkipUnlessEnabled(t)

	feature := features.New("invalid-runtime-class").
		WithLabel("suite", "secureruntime").
		WithLabel("tier", "validation").
		Assess("pool with invalid RuntimeClass shows error condition", func(ctx context.Context, t *testing.T, _ *envconf.Config) context.Context {
			k8sClient := testSuite.MustKubeClient(t)
			fixture := fixtures.New(k8sClient, fixtures.WithPollInterval(250*time.Millisecond))

			namespace := testSuite.AllocateNamespace("invalid-runtime")
			if err := k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
				t.Fatalf("create namespace: %v", err)
			}
			defer suiteenv.DeleteNamespace(ctx, t, k8sClient, namespace)

			// Create pool with non-existent RuntimeClass
			pool := &apiv1alpha1.SandboxPool{
				TypeMeta: metav1.TypeMeta{
					APIVersion: apiv1alpha1.GroupVersion.String(),
					Kind:       "SandboxPool",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "invalid-runtime-pool",
					Namespace: namespace,
				},
				Spec: apiv1alpha1.SandboxPoolSpec{
					Capacity: apiv1alpha1.PoolCapacity{
						PoolMin: 1,
						PoolMax: 1,
					},
					MaxSandboxesPerPod: 5,
					RuntimeType:        apiv1alpha1.RuntimeGVisor,
					RuntimeClassName:   "non-existent-runtime",
					AgentTemplate: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name:  "agent",
								Image: suiteenv.AgentImage(),
							}},
						},
					},
				},
			}

			if _, err := fixture.CreateSandboxPool(ctx, namespace, pool); err != nil {
				t.Fatalf("create pool: %v", err)
			}

			// Wait for condition to be updated
			conditionCtx, cancelCondition := context.WithTimeout(ctx, 30*time.Second)
			defer cancelCondition()

			var runtimeReady *metav1.Condition
			for {
				updatedPool := &apiv1alpha1.SandboxPool{}
				if err := k8sClient.Get(conditionCtx, types.NamespacedName{Name: pool.Name, Namespace: namespace}, updatedPool); err != nil {
					t.Fatalf("get pool: %v", err)
				}

				for _, c := range updatedPool.Status.Conditions {
					if c.Type == apiv1alpha1.PoolConditionRuntimeReady {
						runtimeReady = &c
						break
					}
				}

				if runtimeReady != nil || conditionCtx.Err() != nil {
					break
				}

				time.Sleep(500 * time.Millisecond)
			}

			if runtimeReady == nil {
				t.Fatal("expected RuntimeReady condition to be set")
			}
			if runtimeReady.Status != metav1.ConditionFalse {
				t.Errorf("expected RuntimeReady condition to be False, got: %v", runtimeReady.Status)
			}
			if runtimeReady.Reason != apiv1alpha1.ReasonRuntimeUnavailable {
				t.Errorf("expected Reason to be RuntimeUnavailable, got: %v", runtimeReady.Reason)
			}

			t.Logf("Pool condition correctly shows error: %s", runtimeReady.Message)

			return ctx
		}).
		Feature()

	testSuite.Env().Test(t, feature)
}
```

- [ ] **Step 5: Verify E2E tests compile**

```bash
cd /home/fengjianhui.fjh/fast-sandbox && go build ./test/e2e/...
```

Expected: No errors

- [ ] **Step 6: Commit E2E tests**

```bash
git add test/e2e/suites/secureruntime/
git commit -m "test(e2e): add secure runtime E2E test suite

- Add suite_test.go with RuntimeClass detection helpers
- Add gvisor_test.go for gVisor sandbox tests
- Add kata_test.go for Kata QEMU and Firecracker tests
- Add runtime_validation_test.go for error handling tests
- Tests skip automatically if RuntimeClass not available

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Task 6: Final Verification

- [ ] **Step 1: Run all unit tests**

```bash
cd /home/fengjianhui.fjh/fast-sandbox && go test ./... -v -short
```

Expected: All tests pass

- [ ] **Step 2: Build all components**

```bash
cd /home/fengjianhui.fjh/fast-sandbox && go build ./cmd/...
```

Expected: No errors

- [ ] **Step 3: Create final commit with summary**

```bash
git add -A
git commit -m "feat: complete secure runtime support implementation

Summary of changes:
- API: Added Kata runtime types and RuntimeClassName field
- Runtime: Added handler mapping for all secure runtimes
- Controller: Added RuntimeClass validation with conditions
- E2E: Added comprehensive test suite for secure runtimes

Supported runtimes:
- gVisor (io.containerd.runsc.v1)
- Kata QEMU (io.containerd.kata-qemu.v2)
- Kata Firecracker (io.containerd.kata-fc.v2)
- Kata CLH (io.containerd.kata-clh.v2)

Co-Authored-By: Claude Opus 4.6 <noreply@anthropic.com>"
```

---

## Execution Notes

1. **Prerequisites for E2E tests:**
   - Kubernetes cluster with containerd
   - gVisor installed (optional) - tests will skip if not available
   - Kata Containers installed (optional) - tests will skip if not available
   - RuntimeClass CRDs created (e.g., `gvisor`, `kata-qemu`)

2. **Running E2E tests:**
   ```bash
   FAST_SANDBOX_E2E=1 go test ./test/e2e/suites/secureruntime/... -v
   ```

3. **Testing specific runtime:**
   ```bash
   FAST_SANDBOX_E2E=1 go test ./test/e2e/suites/secureruntime/... -v -run TestGVisor
   ```