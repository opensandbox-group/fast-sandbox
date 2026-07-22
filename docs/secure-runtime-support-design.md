# Secure Runtime Support Design

> **历史设计，已被取代。** 本文描述的 `runtimeType + runtimeClassName + containerdRuntimeHandler` 多字段抽象从未进入生产契约，当前代码不接受这些字段。唯一规范字段是 Pool `spec.runtime`；请参阅 [Runtime/Profile 设计](superpowers/specs/2026-07-19-sandbox-runtime-abstraction-design.md)。

Date: 2026-03-22
Status: Superseded
Author: Fast Sandbox Team

## Overview

This document describes the design for adding secure container runtime support (gVisor and Kata Containers) to Fast Sandbox.

### Goals

- Support 4 secure runtime types: gVisor, Kata (QEMU), Kata (Firecracker), Kata (CLH)
- Pool-level configuration: each SandboxPool uses a single runtime type
- Automatic RuntimeClass validation with clear error messages
- End-to-end E2E testing for all runtime types

### Supported Runtime Types

| Type | Isolation Mechanism | Startup Overhead | RuntimeClass | Containerd Handler |
|------|---------------------|-------------------|--------------|-------------------|
| `container` | Process-level cgroups | ~0ms | (default) | `io.containerd.runc.v2` |
| `gvisor` | User-space kernel (syscall interception) | ~10-50ms | `gvisor` | `io.containerd.runsc.v1` |
| `kata-qemu` | Full VM with QEMU | ~500ms | `kata-qemu` | `io.containerd.kata-qemu.v2` |
| `kata-fc` | MicroVM with Firecracker | ~125ms | `kata-fc` | `io.containerd.kata-fc.v2` |
| `kata-clh` | Cloud Hypervisor | ~200ms | `kata-clh` | `io.containerd.kata-clh.v2` |

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                        SandboxPool CRD                          │
│  spec:                                                          │
│    runtimeType: gvisor | kata-qemu | kata-fc | kata-clh        │
│    runtimeClassName: gvisor (optional, default inferred)        │
│    containerdRuntimeHandler: io.containerd.runsc.v1 (optional)   │
│  status:                                                         │
│    conditions: [RuntimeReady: True/False]                         │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                  SandboxPoolController                          │
│  - Validate RuntimeClass exists at startup and during reconcile  │
│  - Update Pool Condition (RuntimeReady: True/False)              │
│  - Create Agent Pod (always using runc)                          │
│  - Pass environment variables:                                    │
│      RUNTIME_TYPE, RUNTIME_CLASS_NAME, RUNTIME_HANDLER           │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                      Agent Pod (runs on runc)                    │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │                 Runtime Initialization                        │   │
│  │  Select runtime handler based on RUNTIME_HANDLER:           │   │
│  │    - runc: io.containerd.runc.v2                              │   │
│  │    - gvisor: io.containerd.runsc.v1                           │   │
│  │    - kata-qemu: io.containerd.kata-qemu.v2                  │   │
│  │    - kata-fc: io.containerd.kata-fc.v2                      │   │
│  │    - kata-clh: io.containerd.kata-clh.v2                   │   │
│  └─────────────────────────────────────────────────────────┘   │
│                              │                                  │
│                              ▼                                  │
│  ┌─────────────────────────────────────────────────────────┐   │
│  │          Create Sandbox Container (Secure Runtime)          │   │
│  │  containerd.WithRuntime(runtimeHandler, nil)                │   │
│  └─────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────┘
```

## API Changes

### SandboxPool CRD

```go
// RuntimeType defines the isolation level for sandboxes in this pool.
type RuntimeType string

const (
    // RuntimeContainer is the default runc runtime.
    RuntimeContainer RuntimeType = "container"
    // RuntimeGVisor uses gVisor with runsc.
    RuntimeGVisor RuntimeType = "gvisor"
    // RuntimeKataQemu uses Kata Containers with QEMU hypervisor.
    RuntimeKataQemu RuntimeType = "kata-qemu"
    // RuntimeKataFc uses Kata Containers with Firecracker hypervisor.
    RuntimeKataFc RuntimeType = "kata-fc"
    // RuntimeKataClh uses Kata Containers with Cloud Hypervisor.
    RuntimeKataClh RuntimeType = "kata-clh"
)

type SandboxPoolSpec struct {
    Capacity PoolCapacity `json:"capacity"`
    MaxSandboxesPerPod int32 `json:"maxSandboxesPerPod,omitempty"`

    // RuntimeType specifies the secure runtime type for this pool.
    // Default: "container" (standard runc)
    RuntimeType RuntimeType `json:"runtimeType,omitempty"`

    // RuntimeClassName specifies the Kubernetes RuntimeClass to use.
    // If not set, defaults to the string representation of RuntimeType.
    // Example: "gvisor", "kata-qemu", "kata-fc", "kata-clh"
    RuntimeClassName string `json:"runtimeClassName,omitempty"`

    // ContainerdRuntimeHandler overrides the containerd runtime handler.
    // If not set, defaults based on RuntimeType:
    //   - container: io.containerd.runc.v2
    //   - gvisor: io.containerd.runsc.v1
    //   - kata-qemu: io.containerd.kata-qemu.v2
    //   - kata-fc: io.containerd.kata-fc.v2
    //   - kata-clh: io.containerd.kata-clh.v2
    ContainerdRuntimeHandler string `json:"containerdRuntimeHandler,omitempty"`

    AgentTemplate corev1.PodTemplateSpec `json:"agentTemplate"`
}

// Condition types for SandboxPool
const (
    PoolConditionRuntimeReady = "RuntimeReady"
)

type SandboxPoolStatus struct {
    ObservedGeneration int64              `json:"observedGeneration,omitempty"`
    CurrentPods        int32              `json:"currentPods,omitempty"`
    ReadyPods          int32              `json:"readyPods,omitempty"`
    TotalAgents        int32              `json:"totalAgents,omitempty"`
    IdleAgents         int32              `json:"idleAgents,omitempty"`
    BusyAgents         int32              `json:"busyAgents,omitempty"`
    Conditions         []metav1.Condition `json:"conditions,omitempty"`
}
```

### Runtime API (internal/agent/runtime/runtime.go)

```go
type RuntimeType string

const (
    RuntimeTypeContainerd RuntimeType = "container"
    RuntimeTypeGVisor    RuntimeType = "gvisor"
    RuntimeTypeKataQemu  RuntimeType = "kata-qemu"
    RuntimeTypeKataFc    RuntimeType = "kata-fc"
    RuntimeTypeKataClh   RuntimeType = "kata-clh"
)

// NewRuntime creates a Runtime instance based on the runtime type.
func NewRuntime(ctx context.Context, runtimeType RuntimeType, socketPath string) (Runtime, error)
```

### Default Runtime Handler Mapping

```go
var defaultRuntimeHandlers = map[RuntimeType]string{
    RuntimeTypeContainerd: "io.containerd.runc.v2",
    RuntimeTypeGVisor:    "io.containerd.runsc.v1",
    RuntimeTypeKataQemu:  "io.containerd.kata-qemu.v2",
    RuntimeTypeKataFc:    "io.containerd.kata-fc.v2",
    RuntimeTypeKataClh:  "io.containerd.kata-clh.v2",
}
```

## Controller Changes
### SandboxPoolController
- Add `validateRuntimeClass()` method to check RuntimeClass existence
- Call validation at startup and during each reconcile
- Update Pool status condition `RuntimeReady`
- Pass runtime configuration to Agent Pod via environment variables

```go
func (r *SandboxPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    // ... existing logic ...

    // Validate RuntimeClass if specified
    if err := r.validateRuntimeClass(ctx, &pool); err != nil {
        r.updatePoolCondition(ctx, &pool, metav1.Condition{
            Type:   apiv1alpha1.PoolConditionRuntimeReady,
            Status: metav1.ConditionFalse,
            Reason: "RuntimeClassNotFound",
            Message: err.Error(),
        })
        return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
    }

    // Update condition to ready
    r.updatePoolCondition(ctx, &pool, metav1.Condition{
        Type:   apiv1alpha1.PoolConditionRuntimeReady,
        Status: metav1.ConditionTrue,
        Reason: "RuntimeClassValidated",
        Message: fmt.Sprintf("RuntimeClass %s is available", runtimeClassName),
    })

    // ... continue with pod creation ...
}

func (r *SandboxPoolReconciler) validateRuntimeClass(ctx context.Context, pool *apiv1alpha1.SandboxPool) error {
    if pool.Spec.RuntimeType == "" || pool.Spec.RuntimeType == apiv1alpha1.RuntimeContainer {
        return nil // No validation needed for default runtime
    }

    runtimeClassName := r.getRuntimeClassName(pool)
    if runtimeClassName == "" {
        return nil
    }

    runtimeClass := &nodev1.RuntimeClass{}
    if err := r.Client.Get(ctx, client.ObjectKey{Name: runtimeClassName}, runtimeClass); err != nil {
        if errors.IsNotFound(err) {
            return fmt.Errorf("RuntimeClass %q not found", runtimeClassName)
        }
        return err
    }
    return nil
}

```

### Environment Variables Passed to Agent
```go
c.Env = append(c.Env,
    corev1.EnvVar{Name: "RUNTIME_TYPE", Value: string(getRuntimeType(pool))},
    corev1.EnvVar{Name: "RUNTIME_CLASS_NAME", Value: r.getRuntimeClassName(pool)},
    corev1.EnvVar{Name: "RUNTIME_HANDLER", Value: r.getRuntimeHandler(pool)},
)
```

## Agent Changes
### cmd/agent/main.go
No changes needed - already reads RUNTIME_TYPE environment variable.

### Runtime Implementation
Update `NewRuntime()` to support all runtime types with proper handler resolution:

```go
func NewRuntime(ctx context.Context, runtimeType RuntimeType, socketPath string) (Runtime, error) {
    handler := getRuntimeHandler(runtimeType)

    var rt Runtime
    switch runtimeType {
    case RuntimeTypeContainerd, RuntimeTypeGVisor,
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

### Agent Pod Security Context
Agent Pod continues to run on runc (privileged) and creates sandbox containers using the specified secure runtime.

No changes to Agent Pod's `runtimeClassName` - it remains unset (default runc).

## E2E Testing
### Test Suite Structure
```
test/e2e/suites/secureruntime/
├── suite_test.go              # Suite setup with runtime detection
├── gvisor_test.go             # gVisor specific tests
├── kata_test.go               # Kata Containers tests
└── runtime_validation_test.go # Cross-runtime validation tests
```

### Test Cases
1. **RuntimeClass Detection**
   - Check if gVisor RuntimeClass exists
   - Check if Kata RuntimeClass exists
   - Skip tests if runtime not available

2. **gVisor Tests**
   - Create gVisor pool
   - Create sandbox
   - Verify runsc-sandbox process exists on host
   - Verify sandbox isolation

3. **Kata Tests**
   - Create Kata pool
   - Create sandbox
   - Verify container kernel differs from host kernel
   - Verify sandbox isolation

4. **Runtime Validation Tests**
   - Test invalid RuntimeClass handling
   - Test Pool condition updates
   - Test error messages

### Runtime Verification Methods
```go
// Verify gVisor: check runsc-sandbox process
func verifyGVisorRuntime(ctx context.Context, sandboxID string) error {
    // Check for runsc-sandbox process on host
    output, err := exec.Command("pgrep", "-f", "runsc-sandbox").Output()
    if err != nil || len(output) == 0 {
        return fmt.Errorf("gVisor runtime not detected: no runsc-sandbox process found")
    }
    return nil
}

// Verify Kata: check kernel version inside container
func verifyKataRuntime(ctx context.Context, podName, namespace string) error {
    // Get container kernel version
    containerKernel, err := kubectlExec(podName, namespace, "uname", "-r")
    if err != nil {
        return err
    }

    // Get host kernel version
    hostKernel, err := exec.Command("uname", "-r").Output()
    if err != nil {
        return err
    }

    if strings.TrimSpace(containerKernel) == strings.TrimSpace(hostKernel) {
        return fmt.Errorf("Kata runtime not detected: container kernel matches host kernel")
    }
    return nil
}
```

### Test Skip Logic
```go
func (s *SecureRuntimeSuite) skipIfRuntimeNotAvailable(runtimeType string) bool {
    runtimeClassName := map[string]string{
        "gvisor":    "gvisor",
        "kata-qemu": "kata-qemu",
        "kata-fc":   "kata-fc",
        "kata-clh":  "kata-clh",
    }[runtimeType]

    runtimeClass := &nodev1.RuntimeClass{}
    err := s.client.Get(s.ctx, client.ObjectKey{Name: runtimeClassName}, runtimeClass)
    if errors.IsNotFound(err) {
        s.T().Skipf("RuntimeClass %s not found, skipping test", runtimeClassName)
        return true
    }
    return false
}
```

## Implementation Order
1. Update CRD types (api/v1alpha1/sandboxpool_types.go)
2. Update Runtime types (internal/agent/runtime/runtime.go)
3. Update SandboxPoolController (internal/controller/sandboxpool_controller.go)
4. Update E2E tests (test/e2e/suites/secureruntime/)
5. Update CRD YAML examples (config/samples/)
6. Run E2E tests to validate all runtimes

## Dependencies
- Kubernetes cluster with containerd runtime
- gVisor installed on nodes (optional)
- Kata Containers installed on nodes (optional)
- Corresponding RuntimeClass CRDs created (optional)
