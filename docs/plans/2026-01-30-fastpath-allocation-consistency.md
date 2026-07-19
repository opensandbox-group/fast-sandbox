# FastPath 分配一致性修复设计

> **Superseded（2026-07-19）**：本文的 Fast/Strong 双模式和 annotation 搬运方案仅用于解释旧实现。重构后的权威方案是 reservation-before-CRD、`request_id` 幂等、status assignment CAS，以及 Fastlet 原子 admission。参见[多活 Fast-Path 控制面设计](../superpowers/specs/2026-07-18-multi-active-fastpath-control-plane-design.md)。

## 问题背景

当前 FastPath Server 创建 Sandbox 时存在竞态条件：

1. **Fast 模式**：Registry.Allocate() → Agent API → 异步创建 CRD → 返回响应
   - 异步 CRD 创建期间，Controller 可能看到空的 Status，触发重复分配

2. **Strong 模式**：Registry.Allocate() → 创建 CRD → Agent API → 更新 Status
   - CRD 创建后到 Status 更新前有时间窗口，Controller 可能误认为未分配

**根本原因**：K8s CRD 的 spec 和 status 分开更新，无法在一次 Create 调用中同时设置。

## 解决方案

使用 annotation 作为临时传输媒介：
- FastPath 创建时一次性写入 `sandbox.fast.io/allocation` annotation
- Controller `reconcilePending` 检测到 annotation 后搬运到 Status，然后删除 annotation
- 搬运后完全走原有逻辑

### Annotation 数据结构

```go
const AnnotationAllocation = "sandbox.fast.io/allocation"

type AllocationInfo struct {
    AssignedPod  string `json:"assignedPod"`   // 分配的 Agent Pod
    AssignedNode string `json:"assignedNode"`  // 分配的 Node
    AllocatedAt  string `json:"allocatedAt"`   // RFC3339 时间戳
}

// 示例 JSON：
// {"assignedPod":"sandbox-agent-abc123","assignedNode":"node-1","allocatedAt":"2024-01-30T10:00:00Z"}
```

## 实现计划

### Stage 1: 定义通用结构和常量

**文件**: `internal/controller/common/annotations.go` (新建)

**内容**:
```go
package common

const (
    // AnnotationAllocation 临时存储 FastPath 的分配信息，Controller 会搬运到 status 后删除
    AnnotationAllocation = "sandbox.fast.io/allocation"
)

// AllocationInfo 临时分配信息
type AllocationInfo struct {
    AssignedPod  string `json:"assignedPod"`
    AssignedNode string `json:"assignedNode"`
    AllocatedAt  string `json:"allocatedAt"`
}

// BuildAllocationJSON 构建 allocation JSON
func BuildAllocationJSON(assignedPod, assignedNode string) string {
    info := AllocationInfo{
        AssignedPod:  assignedPod,
        AssignedNode: assignedNode,
        AllocatedAt:  time.Now().Format(time.RFC3339Nano),
    }
    data, _ := json.Marshal(info)
    return string(data)
}

// ParseAllocationInfo 从 annotation 解析分配信息
func ParseAllocationInfo(annotations map[string]string) (*AllocationInfo, error) {
    if annotations == nil {
        return nil, nil
    }
    data, ok := annotations[AnnotationAllocation]
    if !ok || data == "" {
        return nil, nil
    }
    var info AllocationInfo
    if err := json.Unmarshal([]byte(data), &info); err != nil {
        return nil, err
    }
    return &info, nil
}
```

**测试**: 无需单独测试，将被其他测试覆盖

**成功标准**:
- 文件创建完成
- 代码编译通过

---

### Stage 2: 修改 FastPath Server

**文件**: `internal/controller/fastpath/server.go`

**修改点**:

1. **import 新包**:
```go
import (
    "fast-sandbox/internal/controller/common"
    // ...
)
```

2. **createFast() 函数**:
```go
func (s *Server) createFast(tempSB *apiv1alpha1.Sandbox, agent *agentpool.AgentInfo, req *fastpathv1.CreateRequest) (*fastpathv1.CreateResponse, error) {
    // ... 前半部分不变 ...

    // Agent 创建成功后，设置 allocation annotation
    tempSB.SetAnnotations(map[string]string{
        common.AnnotationAllocation: common.BuildAllocationJSON(agent.PodName, agent.NodeName),
    })
    // Status 留空，由 Controller 从 annotation 同步

    klog.InfoS("Sandbox created on agent, starting async CRD creation with allocation annotation",
        "name", tempSB.Name, "namespace", tempSB.Namespace,
        "allocationAnnotation", tempSB.Annotations[common.AnnotationAllocation])

    asyncCtx, _ := context.WithTimeout(context.Background(), 30*time.Second)
    go s.asyncCreateCRDWithRetry(asyncCtx, tempSB)
    return &fastpathv1.CreateResponse{SandboxId: tempSB.Name, AgentPod: agent.PodName, Endpoints: s.getEndpoints(agent.PodIP, tempSB)}, nil
}
```

3. **createStrong() 函数**:
```go
func (s *Server) createStrong(ctx context.Context, tempSB *apiv1alpha1.Sandbox, agent *agentpool.AgentInfo, req *fastpathv1.CreateRequest) (*fastpathv1.CreateResponse, error) {
    // ... 前半部分不变，移除原有的 status 更新逻辑 ...

    // 创建 CRD 前设置 allocation annotation
    tempSB.SetAnnotations(map[string]string{
        common.AnnotationAllocation: common.BuildAllocationJSON(agent.PodName, agent.NodeName),
    })

    if err = s.K8sClient.Create(ctx, tempSB); err != nil {
        // ... 错误处理不变 ...
    }

    // 调用 Agent，然后 Controller 会搬运 annotation 到 status
    _, err = s.AgentClient.CreateSandbox(agent.PodIP, &api.CreateSandboxRequest{...})
    if err != nil {
        // ... 错误处理不变，但不需要再更新 status ...
    }

    // 移除原有的 status 更新逻辑（line 177-187）
    // Controller 会通过搬运 annotation 来设置 status

    return &fastpathv1.CreateResponse{...}, nil
}
```

4. **删除 asyncCreateCRDWithRetry 中的 status 设置**:
```go
// 原 line 195-211: 移除 sb.Status 的设置，只负责创建 CRD
func (s *Server) asyncCreateCRDWithRetry(ctx context.Context, sb *apiv1alpha1.Sandbox) {
    klog.InfoS("Starting async CRD creation", "name", sb.Name, "namespace", sb.Namespace)
    for attempt := 0; attempt < maxRetries; attempt++ {
        sb := sb // 创建副本避免并发问题
        err := s.K8sClient.Create(ctx, sb)
        if err == nil {
            klog.InfoS("Async CRD creation succeeded", "name", sb.Name)
            return
        }
        // ... 重试逻辑 ...
    }
}
```

**测试**: `internal/controller/fastpath/server_test.go`

需要新增/修改的测试：
- `TestServer_CreateSandbox_FastMode_SetsAllocationAnnotation`: 验证 fast 模式设置 annotation
- `TestServer_CreateSandbox_StrongMode_SetsAllocationAnnotation`: 验证 strong 模式设置 annotation
- 修改现有的 CreateSandbox 测试，验证 annotation 存在

**成功标准**:
- 所有新增/修改的代码有测试覆盖
- 现有测试仍然通过
- Fast 模式创建后 CRD 包含正确的 allocation annotation
- Strong 模式创建后 CRD 包含正确的 allocation annotation
- Status 保持为空（由 Controller 填充）

---

### Stage 3: 修改 Controller 逻辑

**文件**: `internal/controller/sandbox_controller.go`

**修改点**:

1. **import 新包**:
```go
import (
    "fast-sandbox/internal/controller/common"
    // ...
)
```

2. **reconcilePending() 函数开头添加搬运逻辑**:
```go
func (r *SandboxReconciler) reconcilePending(ctx context.Context, sandbox *apiv1alpha1.Sandbox) (ctrl.Result, error) {
    logger := klog.FromContext(ctx)

    // === Step 0: 搬运 allocation annotation 到 status ===
    if allocInfo, err := common.ParseAllocationInfo(sandbox.Annotations); err != nil {
        logger.Error(err, "Failed to parse allocation annotation, clearing it")
        r.clearAllocationAnnotation(ctx, sandbox)
        return ctrl.Result{Requeue: true}, nil
    } else if allocInfo != nil {
        logger.Info("Found allocation annotation, moving to status",
            "assignedPod", allocInfo.AssignedPod, "assignedNode", allocInfo.AssignedNode)

        if err := r.moveAllocationToStatus(ctx, sandbox, allocInfo); err != nil {
            logger.Error(err, "Failed to move allocation to status")
            return ctrl.Result{}, err
        }

        logger.Info("Allocation moved to status, annotation cleared, requeueing")
        return ctrl.Result{Requeue: true}, nil
    }

    // === 原有逻辑保持不变 ===
    // ...
}
```

3. **新增辅助函数**:
```go
// moveAllocationToStatus 搬运 annotation 到 status，然后删除 annotation
func (r *SandboxReconciler) moveAllocationToStatus(ctx context.Context, sandbox *apiv1alpha1.Sandbox, allocInfo *common.AllocationInfo) error {
    return retry.RetryOnConflict(retry.DefaultRetry, func() error {
        latest := &apiv1alpha1.Sandbox{}
        if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
            return err
        }

        // 设置 status
        latest.Status.AssignedPod = allocInfo.AssignedPod
        latest.Status.NodeName = allocInfo.AssignedNode
        latest.Status.Phase = string(apiv1alpha1.PhaseBound)

        // 删除 annotation（搬运完成，不再需要）
        if latest.Annotations != nil {
            delete(latest.Annotations, common.AnnotationAllocation)
        }

        return r.Status().Update(ctx, latest)
    })
}

// clearAllocationAnnotation 清除损坏的 annotation
func (r *SandboxReconciler) clearAllocationAnnotation(ctx context.Context, sandbox *apiv1alpha1.Sandbox) {
    retry.RetryOnConflict(retry.DefaultRetry, func() error {
        latest := &apiv1alpha1.Sandbox{}
        if err := r.Get(ctx, client.ObjectKeyFromObject(sandbox), latest); err != nil {
            return err
        }
        if latest.Annotations != nil {
            delete(latest.Annotations, common.AnnotationAllocation)
        }
        return r.Update(ctx, latest)
    })
}
```

**测试**: `internal/controller/sandbox_controller_test.go`

需要新增的测试：
- `TestReconcilePending_MoveAllocationToStatus`: 验证 annotation 搬运到 status
- `TestReconcilePending_ClearInvalidAllocationAnnotation`: 验证损坏的 annotation 被清除
- `TestReconcilePending_AllocationAlreadyInStatus`: 验证 status 已同步后不再处理

**成功标准**:
- 所有新增/修改的代码有测试覆盖
- 现有测试仍然通过
- Controller 检测到 allocation annotation 时正确搬运到 status
- 搬运后 annotation 被删除
- 搬运后的 sandbox 走正常 reconcile 流程

---

### Stage 4: 端到端测试

**文件**: `test/e2e/fastpath_test.go` (新建或修改)

**测试场景**:
1. **Fast 模式创建**:
   - 调用 FastPath API 创建 sandbox
   - 等待 CRD 创建
   - 验证 CRD 有 allocation annotation
   - 等待 Controller reconcile
   - 验证 status 被填充，annotation 被删除

2. **Strong 模式创建**:
   - 同上

3. **竞态条件测试**:
   - 模拟 FastPath 创建后立即 reconcile
   - 验证不会触发重复分配

**成功标准**:
- E2E 测试通过
- 没有重复分配发生
- Registry.Allocated 计数正确

---

## 不需要修改的部分

- `Registry.Restore()`: 从 status 读取（annotation 已被搬运）
- Agent lost 处理: 搬运后完全走原有逻辑
- 删除逻辑: 完全不受影响

---

## 风险和注意事项

1. **向后兼容**:
   - 旧的 sandbox 没有 allocation annotation，不受影响
   - Controller 只处理有 annotation 的情况

2. **幂等性**:
   - `moveAllocationToStatus` 是幂等的，重复 reconcile 安全
   - 如果 annotation 已被删除，函数不会被调用

3. **并发安全**:
   - 使用 `retry.RetryOnConflict` 处理并发更新
   - annotation 和 status 更新是分开的，但 status 更新在 annotation 删除之后

4. **错误处理**:
   - 损坏的 annotation 会被清除，sandbox 重新走调度流程
   - 这可能导致重复分配，但比无法恢复要好

---

## 时间线

| 阶段 | 预估工作量 |
|------|-----------|
| Stage 1 | 30 分钟 |
| Stage 2 | 2 小时 |
| Stage 3 | 2 小时 |
| Stage 4 | 2 小时 |
| **总计** | **6.5 小时** |
