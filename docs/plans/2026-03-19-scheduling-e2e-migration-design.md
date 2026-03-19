# Scheduling Resources E2E 测试迁移设计

## 概述

将 `test/e2e/02-scheduling-resources/` 目录下的 Shell 脚本测试迁移到 Go e2e-framework 测试框架。

## 迁移范围

| 原文件 | 功能描述 | 新测试函数 |
|--------|----------|------------|
| `port-mutual-exclusion.sh` | 验证相同端口 Sandbox 调度到不同 Pod | `TestPortMutualExclusion` |
| `resource-slot.sh` | 验证 maxSandboxesPerPod 容量限制 | `TestResourceSlotCapacity` |
| `autoscaling.sh` | 验证 Pool 按需扩容 | `TestAutoScaling` |

## 文件结构

```
test/e2e/suites/scheduling/
├── suite_test.go           # TestMain 入口
└── scheduling_test.go      # 3 个测试用例 + helper 函数
```

## 测试用例设计

### TestPortMutualExclusion

验证相同端口的 Sandbox 被调度到不同 Pod。

**步骤：**
1. 创建 Pool (poolMin: 2, poolMax: 2, maxSandboxesPerPod: 5)
2. 等待 2 个 Agent Pod 就绪
3. 创建 Sandbox A (端口 8080)，等待分配
4. 创建 Sandbox B (端口 8080)，等待分配
5. 断言: A.assignedPod != B.assignedPod
6. 验证 Endpoint 状态正确填充

### TestResourceSlotCapacity

验证 maxSandboxesPerPod 容量限制正确生效。

**步骤：**
1. 创建 Pool (poolMin: 1, poolMax: 1, maxSandboxesPerPod: 2)
2. 等待 Agent Pod 就绪
3. 创建 Sandbox 1 -> 等待运行成功
4. 创建 Sandbox 2 -> 等待运行成功
5. 创建 Sandbox 3 -> 验证被拒绝 (容量已满)
6. 删除不存在的资源 -> 验证优雅处理

### TestAutoScaling

验证 Pool 根据需求从 1 扩容到 2 个 Pod。

**步骤：**
1. 创建 Pool (poolMin: 1, poolMax: 2, maxSandboxesPerPod: 1)
2. 等待 1 个 Agent Pod 就绪
3. 创建 2 个 Sandbox
4. 等待 Pool 扩容到 2 个 Pod
5. 断言: 两个 Sandbox 都成功分配
6. 断言: Sandbox 分配到不同 Pod

## 共享 Helper 函数

```go
// 复用 basicvalidation 已有
// - createNamespace()
// - waitForPoolReady()
// - waitForAssignedSandbox()

// 新增 scheduling 特有
func createSchedulingPool(namespace, name string, min, max, maxPerPod int32) *apiv1alpha1.SandboxPool
func countReadyPods(ctx context.Context, client ctrlclient.Client, namespace, poolName string) (int, error)
func createSimpleSandbox(namespace, name, pool string, ports []int32) *apiv1alpha1.Sandbox
```

## 超时配置

| 配置项 | 值 | 说明 |
|--------|-----|------|
| poolReadyTimeout | 90s | 等待 Pool 就绪 |
| sandboxRunTimeout | 60s | 等待 Sandbox 运行 |
| scaleTimeout | 120s | 等待扩容完成 |
| pollInterval | 250ms | 轮询间隔 |

## 依赖

- `suiteenv` - 测试环境配置
- `fixtures` - K8s 资源操作工具
- `controller-runtime` - K8s client
- `sigs.k8s.io/e2e-framework` - 测试框架

## 标签

```go
WithLabel("suite", "scheduling")
WithLabel("tier", "smoke")
```