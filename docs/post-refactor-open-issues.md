# Fast Sandbox 架构重构遗留问题与后续决策

**盘点日期**：2026-07-21
**代码基线**：`feature/fast-sandbox-architecture-refactor` 当前工作区
**关联文档**：[架构重构完成度审计](./refactor-completion-audit.md)、[架构重构验收报告](./release-acceptance-report.md)

本文记录上一轮架构重构完成后新发现的真实缺陷、尚需确认的架构边界、保持 fail-closed 的能力门禁和目标环境验收项。它不推翻上一轮已经确认的架构不变量，但会修正“测试通过即不存在生命周期边界缺陷”的过度判断。

## 1. 状态与优先级

| 状态 | 含义 |
|---|---|
| ConfirmedBug | 已有真实运行现场，行为不符合既定语义 |
| FixedVerified | 修复已实现，并通过对应单元、race、远端 E2E 和现场收敛验收 |
| Verified | 实现与当前源码快照的完整 Gate 已核对通过 |
| DecisionPending | 实现前需要确认架构选择 |
| DecisionApproved | 架构选择已经确认，等待按决策实施或完成配套治理 |
| DesignGap | 尚未形成完整安全或能力闭环，当前实现可能暴露错误能力声明或信任边界 |
| CapabilityGate | 已实现 fail-closed 边界，尚不能声明能力可用 |
| ValidationGap | 实现已存在，但缺少目标环境或目标规模证据 |
| Deferred | 已明确不属于当前版本，不应当伪装成实现缺陷 |

| 优先级 | 含义 |
|---|---|
| P0 | 会让声明式生命周期永久卡住，下一轮首先修复 |
| P1 | 合入或生产集成前需要关闭，或需要保持显式门禁 |
| P2 | 不影响当前正确性，但影响完整能力、性能或可运维性 |

## 2. P0：Containerd 删除不是完全幂等

### 2.1 现场

原保留对象：`default/smoke-http`，Sandbox UID `dc8b3145-9b27-4875-87ad-1208df28c4e7`。

对象已有 `deletionTimestamp` 和 `sandbox.fast.io/cleanup` finalizer，状态长期停留在：

```text
runtimeState: Draining
dataPlaneState: Draining
```

Fastlet 日志稳定重复：

```text
container "dc8b..." in namespace "k8s.io": not found
snapshot dc8b...-snapshot does not exist: not found
Runtime deletion failed; retaining admission capacity for retry
```

这说明 runtime container 和 snapshot 都已经不存在，但 Fastlet 将 snapshot NotFound 继续作为删除失败返回；Controller 每秒重新触发删除，finalizer 无法完成。

### 2.2 根因

`ContainerdRuntime.deleteContainerdSandbox` 在 `LoadContainer` 返回 NotFound 后会补删 snapshot，但直接返回 `SnapshotService.Remove` 的错误。snapshot 同样不存在时，这个 NotFound 被错误地当成失败。

当前删除函数还存在同类不一致：

- `LoadContainer`、`Task`、`Task.Delete`、`Container.Delete` 和 `Snapshot.Remove` 没有统一 NotFound 语义；
- `forceCleanupSnapshot` 依赖错误文本包含 `not found` 或 `no such`，没有统一使用 containerd `errdefs.IsNotFound`；
- 已自行退出的 task 可能让 `Kill` 返回错误，即使后续资源已经删除，早期错误仍可能使整个删除失败；
- 当前测试只记录 nil client 可能 panic，没有覆盖真实幂等删除状态组合。

### 2.3 目标契约

`RuntimeDriver.DeleteSandbox(ctx, sandboxUID)` 的统一语义应当是：

> 确保该 RuntimeDriver 管理的 Sandbox runtime 资源最终不存在。目标在调用前、调用中或上一次重试中已经不存在，都返回成功；只有无法确认资源已经消失的错误才返回失败。

因此：

- NotFound 是删除成功，不是需要上抛给 Controller 的业务错误；
- 重复调用必须稳定返回成功；
- 中间操作失败但最终能够确认 container、task 和 snapshot 均不存在时，整体仍然成功；
- 无法确认 runtime 已消失时，不能提前释放可能仍被运行实例使用的 network slot；
- runtime 已确认消失后，Infra instance 和 network slot 清理继续保持可重试、幂等。

### 2.4 修复方案

1. 将 containerd 删除重构为 `ensureContainerdSandboxAbsent` 状态机，而不是一组顺序执行后直接 `JoinErrors` 的命令。
2. 增加统一的 `ignoreNotFound`/`removeSnapshotIfExists` helper，所有 containerd NotFound 使用 `errdefs.IsNotFound` 判断，并删除基于错误文本的匹配逻辑。
3. `LoadContainer` 已 NotFound 时，只补删可能残留的 snapshot；snapshot 也 NotFound 立即成功。
4. task 存在时先读取状态：运行中才执行 SIGTERM 和有界等待；已经停止则直接删除 task。超时后使用 SIGKILL/`WithProcessKill`。
5. `Task.Delete`、`Container.Delete(containerd.WithSnapshotCleanup)` 和最后一次 snapshot cleanup 都把 NotFound 归一化为成功。
6. 中间的 signal/delete 错误先收集；结束前检查 container 和 snapshot 的存在性。二者都不存在时返回成功，否则返回仍能说明资源未清理或状态未知的错误。
7. `ContainerdRuntime.DeleteSandbox` 只有在 runtime absence 已确认后，才继续清理 Infra state 和释放 NetworkSlot。
8. `SandboxManager` 删除成功后移除本地 metadata、归还 admission capacity；Controller 下一轮 Inspect 得到 NotFound 后移除 finalizer。Controller finalizer 语义无需放宽。
9. 删除临时 `[DEBUG-FASTLET]` 日志，改为结构化的 delete outcome/latency/retry reason；真实失败保留告警，幂等 NotFound 不按 Error 记录。

不建议只在当前 `LoadContainer NotFound` 分支增加一条 `if snapshot NotFound return nil`。该补丁可以解除 `smoke-http`，但 task 已停止、部分删除成功和二次重试仍会留下相同类型的问题。

### 2.5 验收矩阵

| 场景 | 期望 |
|---|---|
| container、snapshot 调用前都不存在 | 第一次和重复删除都成功 |
| workload 自行退出，task 已 stopped | 删除 task/container/snapshot，finalizer 完成 |
| task 不存在，container 仍存在 | 删除 container/snapshot，最终成功 |
| container 已由上一次调用删除，snapshot 也已删除 | 重试成功，不保留 admission capacity |
| snapshot 存在且删除成功 | 整体成功 |
| snapshot 返回非 NotFound 的 permission/unavailable | 保持失败并重试，不提前释放 network slot |
| 正在运行的 workload | SIGTERM 有界等待，必要时 SIGKILL，最终删除成功 |
| 同一 Sandbox 并发/重复 Delete | 同一时刻最多一个本地清理流程，最终收敛到 absent |
| Controller 声明式删除 | Sandbox 在期限内消失，finalizer 不残留 |

自动化测试至少包括：

- containerd 删除状态机单元测试，使用可注入 fake 覆盖上述错误组合；
- `SandboxManager` 重复删除和 capacity 归还测试；
- Controller finalizer 对 runtime 已不存在的测试；
- 远端 `ssh-fast`/`fast` 上新增“workload 先自行退出，再删除 CRD”的 e2e；
- 保留正常运行 workload 的声明式删除回归。

现场验收顺序：先部署修复镜像，不手工删除 finalizer；观察保留的 `smoke-http` 自动完成删除，以证明修复能够接管历史卡住对象。

### 2.6 修复结果

2026-07-21 已实现 `ensureContainerdSandboxAbsent` 状态机，所有 NotFound 使用 containerd `errdefs.IsNotFound`，并在结束前验证 container 和 snapshot 确实不存在。已完成：

- containerd 删除状态组合单测；
- `internal/fastlet/runtime` 普通测试和 race；
- 远端 `make test-e2e-lifecycle`，包含“workload 自行退出后删除 CRD并立即复用容量”；
- 正常运行 workload 的 graceful deletion 回归；
- 保留 `smoke-http` 未手工移除 finalizer，在同一 Fastlet Pod UID 更新进程后自动消失。

因此 DEL-001 已关闭。历史现场在进程重启后通过 runtime inventory 确认 runtime 不存在；新 E2E 是删除状态机对自行退出 workload 的直接回归证据。

## 3. Fastlet 与 Fastlet Proxy 是否合并

### 3.1 当前边界

从部署形态看，它们已经属于同一个 **Fastlet Pod**；从进程和容器边界看，它们是两个内部子组件：

```text
Fastlet Pod
├── fastlet
│   ├── admission / runtime lifecycle / recovery
│   ├── containerd、netns、iptables、Infra 管理
│   └── Fastlet control API :5758
└── fastlet-proxy
    ├── RouteStore 与 generation tombstone
    ├── route credential 验证
    └── 用户数据透明代理 :5780
```

Container、gVisor 和 Kata 路径中的 fastlet 是 privileged，并挂载 containerd、宿主 netns/network state 等资源；fastlet-proxy 不挂载 containerd socket，也不需要 privileged。两者通过 Pod 内 UDS 同步 RouteStore，Proxy 重启后由 Fastlet runtime inventory 和 NetworkState 重建路由。

### 3.2 可选方案

| 方案 | 收益 | 代价/风险 | 判断 |
|---|---|---|---|
| 一个进程、一个容器 | RouteStore 可直接使用内存对象；删除 UDS、Snapshot/Watch 和重连恢复；配置较少 | 用户流量处理代码进入 privileged 进程；Proxy 漏洞可直接获得 containerd/netns 权限；Proxy panic/OOM 会同时中断 admission、runtime recovery 和所有数据流；控制流与大流量争抢同一资源 | 不推荐生产使用 |
| 一个二进制，两种 mode、两个容器 | 可以统一版本、构建和配置模型，同时保留权限/故障隔离 | UDS 和路由恢复仍然存在；单个镜像更大 | 可选的工程简化，不改变架构 |
| 一个 Fastlet Pod、两个专用进程/容器 | privileged runtime 管理与用户数据面隔离；可独立限流、重启、观测；Proxy 重启不影响 runtime | 需要 UDS 控制协议、RouteStore 重建和版本一致性治理 | 推荐生产形态 |

### 3.3 结论

生产部署不建议把 fastlet 和 fastlet-proxy 合并成同一进程。决定性原因不是代码组织，而是权限和故障域：一个接触不可信用户数据，另一个掌握节点级 runtime/network 权限。

部署组件目录中应只把 **Fastlet Pod** 作为一个顶层组件；`fastlet agent` 和 `Fastlet Data Proxy` 是它的内部进程，不再与 control plane、NodeJanitor 并列计算部署形态。这样顶层仍是：

1. fastctl / SDK；
2. control plane；
3. Fastlet Pod；
4. NodeJanitor。

可以在不合并进程的情况下做以下简化：

- Controller 始终原子生成同版本的 Fastlet/Proxy 容器配置，禁止独立升级；
- 为 UDS control API 增加显式 protocol version/capability handshake，版本不匹配时 Pod fail closed；
- 将两者的 readiness 聚合为 Fastlet Pod 的单一产品状态，文档和运维只暴露 Fastlet Pod；
- 统一 lifecycle identity、日志字段和 release version；
- 保留 RouteStore 易失、由 Fastlet runtime inventory 重建的原则，不增加第三份持久化真相；
- 根据真实流量为 Proxy 设置独立 CPU/memory、连接数和并发限制，避免数据面挤压 admission/recovery。

需要注意：两个容器共享 Pod network namespace，因此当前分离不是完整的应用层信任边界。fastlet-proxy 可以访问 `:5758`，而 Fastlet control API 当前主要依赖 NetworkPolicy。应用层身份认证和防重放是已知后续安全项，但按当前范围暂缓，不阻塞本轮重构关闭；不能用“已经拆容器”描述成该风险已经消失。

如果未来坚持合并进程，前提应是先把 privileged runtime/network 操作迁移到另一个最小化的特权 daemon。否则只是为了删除 UDS，把更大的攻击面引入最高权限进程。

## 4. 上一轮重构遗留问题清单

| ID | 优先级 | 状态 | 问题 | 当前边界/后续动作 |
|---|---:|---|---|---|
| DEL-001 | P0 | FixedVerified | containerd 删除把已不存在的 snapshot 当成失败，finalizer 永久停留 | 已实现 ensure-absent 状态机，并通过单元/race/lifecycle E2E和 `smoke-http` 收敛验收 |
| FST-001 | P1 | DecisionApproved | Fastlet/Fastlet Proxy 的产品组件边界容易被理解为两个部署组件 | 已确认顶层合并为 Fastlet Pod，生产进程不合并；后续补 protocol handshake 和聚合状态 |
| SEC-001 | P2 | Deferred | Fastlet control API 主要依赖 NetworkPolicy；同 Pod Proxy 可直接访问管理端口 | 已知风险，按本轮决策暂不投入，不阻塞当前重构关闭 |
| BOX-001 | P1 | CapabilityGate | BoxLite v0.9.7 无法提供 guest root 不可绕过的 CPU/memory/PIDs 完整限制 | 继续 fail closed；统一跟踪于 [BoxLite 重点投入路线](./boxlite-integration-roadmap.md) |
| BOX-002 | P1 | DecisionApproved | BoxLite 当前用静态 `WithPort` + 注入 `sandbox-tunnel`，每 Box 占一个 Pod TCP 端口 | 当前为兼容层，目标已确定为 native tunnel + sidecar UDS streaming；见 BoxLite 路线图 |
| BOX-003 | P1 | ValidationGap | BoxLite gvproxy 出站源地址、CNI NetworkPolicy、conntrack 和 sidecar/backend 恢复尚无完整 Kubernetes 证据 | 代码暂时保持现状，后续按 BoxLite 路线图完成真实 Kubernetes 矩阵 |
| KATA-001 | P1 | CapabilityGate | `kata-fc` 节点基线缺少 Firecracker MMIO kernel 和 block snapshotter | 根因与修复路径已固化到 [Kata Firecracker 调查](./kata-firecracker-investigation.md)；完成 pinned artifact + devmapper + 真实 E2E 前继续 fail closed |
| CACHE-001 | P1 | CapabilityGate | containerd image store 是 node-scoped 共享状态，单 Fastlet 无权执行破坏性 GC | 已确定“调度层生成带 fence/TTL 的 NodeCachePlan，NodeJanitor 本地复核并执行”；该协议实现前只生成 protection/eviction plan |
| INFRA-001 | P1 | CapabilityGate | 生产 opensandbox-execd artifact reference、digest、签名和 OCI opener 尚未绑定；当前 profile 有意 `Configured=false` | execd 是重点接入 case，边界与后续步骤见 [OpenSandbox execd 接入指南](./opensandbox-execd-integration-guide.md)；artifact 绑定后续实施 |
| INFRA-002 | P2 | Deferred | E2B envd 的 template/preinstalled capability 没有真实绑定 | envd 不再作为重点方向；后续可删除内置 profile，或仅保留为 fail-closed 示例 |
| API-001 | P1 | Verified | 当前工作区删除旧兼容字段/CLI 后需要最终源码快照 Gate | protobuf/DeepCopy/CRD/Python proto、完整 unit/race、CLI/SDK 和远端声明式/快速路径均已通过 |
| PERF-001 | P2 | Deferred | 当前 Create 数据只有单节点 kind smoke，不是生产 SLO | 后续在目标硬件形成独立性能报告，不阻塞本轮架构关闭 |
| PERF-002 | P2 | Deferred | 单节点 kind 不能证明多节点 cache affinity 和低频 heartbeat 的规模行为 | 后续规模测试，不阻塞本轮架构关闭 |
| PERF-003 | P2 | Deferred | direct-CRD 声明式路径没有进入同一负载报告 | 后续性能报告补充，不作为当前重点 |
| OBS-001 | P2 | ValidationGap | `sandbox-init` 路径不能可信确认原始用户进程启动时间 | 增加 supervisor→Fastlet 的 generation-fenced callback；此前继续标记 unavailable |
| REG-001 | P2 | Deferred | 私有镜像凭证的下发、刷新和失效 | 单独设计 secret ownership、节点缓存与轮换，不阻塞当前公共镜像路径 |

### 4.1 不应重新归类为本轮缺陷的事项

以下能力已经明确属于后续产品阶段，不应混入当前修复范围：

- snapshot；
- pause/resume；
- 持久化 storage；
- live migration；
- Sandbox 跨 Fastlet Pod 存活。

当前 Pod-bound 语义保持不变：Fastlet Pod 消失后，所属 runtime 失效，由 `FailurePolicy` 决定保持 Lost 或重建。

## 5. 后续实施顺序

DEL-001 和 API-001 已关闭。后续按以下顺序推进：

1. **BOX-001/BOX-002/BOX-003**：按 BoxLite 路线图优先推进上游 ResourceLimits、native tunnel 与 Kubernetes 实证。
2. **INFRA-001**：按接入指南绑定 OpenSandbox execd 生产 artifact、签名和官方 SDK E2E；envd 不作为重点。
3. **KATA-001**：由节点基础设施提供 MMIO kernel + devmapper 基线，再解除 Fast Sandbox capability gate。
4. **CACHE-001**：实现调度层 `NodeCachePlan` 和 NodeJanitor 本地执行协议；此前不做 node-scoped destructive GC。
5. **FST-001**：保持一个 Fastlet Pod、两个内部进程；protocol handshake 和聚合状态作为后续工程治理。
6. **SEC/PERF/OBS/私有镜像**：按已经确认的 Deferred/目标环境边界另行排期。

## 6. 已确认决策

已确认以下架构结论：

> Fastlet 与 Fastlet Proxy 在产品和部署清单中合并为一个 **Fastlet Pod** 组件，但生产环境继续保留两个内部进程/容器；不实施同进程合并。

删除幂等性修复属于既定声明式语义的 bugfix，不需要新增产品决策。
