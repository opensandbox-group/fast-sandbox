# Firecracker 按需加载：节点级 runtime-agent 与系统分层边界设计

> 文档类型：技术方案（设计主文档）
>
> 日期：2026-08-27
>
> 状态：草案（提交评审）
>
> 实现细节见 [firecracker-on-demand-loading-details.md](firecracker-on-demand-loading-details.md)
> （协议、流程、参数、维护策略，实现时以细节文档为准）。
>
> 参考实现：
> - [kvcache-ai/AgentENV](https://github.com/kvcache-ai/AgentENV)（MIT，OverlayBD + ublk 统一数据面）
> - aone-sandbox-coordination `docs/design/2026-08-21-opensandbox-firecracker-on-demand-loading.md`（OpenSandbox 原生 Firecracker 方案）
> - aone-sandbox-coordination `docs/investigation/2026-08-21-agentenv-on-demand-loading.md`、`2026-08-21-e2b-on-demand-loading.md`

---

- [摘要](#摘要)
- [背景与现状](#背景与现状)
- [目标与边界](#目标与边界)
- [系统分层](#系统分层)
- [架构选择](#架构选择)
- [核心设计决策](#核心设计决策)
- [关键能力概述](#关键能力概述)
- [风险与问题](#风险与问题)
- [实施阶段](#实施阶段)
- [文档索引](#文档索引)

## 摘要

fast-sandbox 的 Firecracker runtime 目前只支持冷启动：按 `SandboxSpec.Image`
引用本地 rootfs 缓存，无快照 restore、无按需加载、无节点间分发。builder
（`cmd/sandboxtemplate-builder`）已能产出完整快照集（rootfs/vmstate/memory +
overlaybd layer），但**消费端与发布端尚未对齐**（寻址键不同，`PullImage`
只查本地缓存从不拉取）。

本方案定义五件事：

1. **一条寻址链**：`SandboxSpec.Image → 引用索引 → manifest → content-addressed
   layer`，fastlet 协议只加不改；
2. **一个节点级 DaemonSet**（`firecracker-runtime-agent`）：独占块数据面
   （OverlayBD + ublk、有界块缓存、P2P、S3 只读凭证），多个 fastlet Pod
   共享只读设备与宿主 page cache；
3. **一条设备租约 UDS API**：fastlet driver 保留全部 VM 生命周期，仅新增
   Pin/Lease 客户端（Seal 属阶段 4）；
4. **一个 P2P 层**（集成 DART，native/overlaybd 共用）：源选择链
   `DART 本地缓存 → DART peer → 对象存储`，冷启动风暴不直接回源；
5. **运行态增量快照与 API 保存/恢复（阶段 4，后续实施）**：共享 golden
   base + 内容寻址增量链，用户显式 API 保存/恢复运行中沙箱。**当前
   焦点是启动流程（阶段 1-3），快照/snapshot/resume 不阻塞启动**。

## 背景与现状

| 面 | 现状 |
|----|------|
| 发布端（builder） | 产物按 `sha256(manifest)[:16]` 寻址；**无 imageRef→manifestRef 索引**；`SHA256SUMS` 只生成不发布 |
| 消费端（driver） | 缓存 key `sha256(imageRef)`；`PullImage` 只查本地；无 S3 客户端/凭据/manifest 解析/digest 校验；无 snapshot restore |
| 需求缺口 | ① 无法消费 builder 产物；② 无按需加载；③ 无 P2P（回源风暴）；④ 节点间不共享缓存；⑤ **无运行态快照**（与 architecture.md non-goal 冲突） |

## 目标与边界

### 目标

- artifact 寻址与拉取契约完整，**fastlet API 协议语义不变**（只加字段/RPC）；
- 节点级 agent 分层边界清晰，多 Pod 共享只读设备/块缓存/page cache；
- `native`（全量 eager）与 `overlaybd`（按需 range read）两种消费模式；
  部署环境明确 Linux 6.8+（ublk），无降级路径；
- P2P 降低回源压力；快照 restore 支持跨节点调度（compatibility class）；
- **运行态增量快照 + API 保存/恢复（阶段 4，后续实施）**：用户显式触发，
  链式增量 + 定期 flatten；控制面快照资源模型（owner/TTL/配额/引用）。

### 范围边界（本期明确不做）

- **cold boot 降级路径**：restore 为唯一启动路径（golden snapshot），
  删除冷启动分支与恢复失败的冷启动 fallback；kernel（vmlinux.bin）从
  节点运行时资产移除（仅构建期需要）；compat 匹配升级为调度硬约束
  （不匹配拒绝调度，不"尽力而为"）；
- **运行态快照保存/恢复（snapshot/resume）**：阶段 4 实施；启动流程
  （阶段 1-3）不依赖快照设计；相关协议增量（`SnapshotRef` 字段、
  `CreateSandboxSnapshot` RPC、`SandboxSnapshot` CRD）随之推迟；
- 节点故障/置换**被动迁移**（drain/failover 批量快照）；
- 跨主机恢复**专门机制**（源亲和、增量层拉取优化——恢复统一对象存储回源）；
- 二者为后续演进方向。

### 非目标

- 不引入 AgentENV/E2B 的控制面（API/Gateway/调度/envd/租户模型）；
- 不在 driver 内实现 OverlayBD/ublk/P2P（数据面归 agent）；
- 不承诺跨 CPU/kernel/Firecracker 版本的 snapshot 兼容（compat class 隔离）；
- 不把 memory.snap/rootfs.ext4 挂成 OSSFS；
- 首期不支持未 quiesce 工作负载的强一致快照。

## 系统分层

```text
┌──────────────────────────────────────────────────────────────────┐
│ L0 控制面 controlplane（K8s）                                     │
│   SandboxPool / SandboxTemplate / SandboxSnapshot reconciler、   │
│   placement registry、fastpath：调度、warm images、artifact 索引   │
└───────────────┬──────────────────────────────────────────────────┘
                │ 控制面协议（阶段 1-3 零改动；阶段 4 只加不改）
┌───────────────▼──────────────────────────────────────────────────┐
│ L1 fastlet（Pod × N）                                             │
│   firecracker driver：VM 进程/API、网络 slot、Infra、状态、恢复、   │
│   快照编排；新增 runtime-agent UDS client                         │
└───────────────┬──────────────────────────────────────────────────┘
                │ UDS（租约 / Pin / Seal / Health）
┌───────────────▼──────────────────────────────────────────────────┐
│ L2 节点 daemonset firecracker-runtime-agent（每节点 1 个，三进程） │
│   Go 编排：UDS 管理面、租约、缓存目录、layer GC、源选择/租户边界、 │
│             S3 只读凭证（presigned 签发）、快照增量 seal/上传      │
│   Rust 块设备进程（vendored AgentENV ublk-daemon）：               │
│             OverlayBD、ublk 设备创建/删除/restack                  │
│   DART 进程：block 缓存、P2P 分发树、peer-listen、admin/metrics    │
│             （独立二进制，HTTP 前缀模式，零代码集成）              │
│   └─ 设备 bind-mount 目录：/run/fast-sandbox/firecracker/devices/ │
└───────────────┬──────────────────────────────────────────────────┘
                │ HTTP Range / S3 只读 / P2P
┌───────────────▼──────────────────────────────────────────────────┐
│ L3 数据源：对象存储（模板产物 + 运行态快照增量层）+ P2P peer         │
└──────────────────────────────────────────────────────────────────┘
```

### 职责矩阵

| 能力 | L0 控制面 | L1 driver | L2 agent |
|------|-----------|-----------|----------|
| 生命周期/VM 进程/网络/Infra/状态 | 编排 | **拥有** | — |
| artifact 寻址与拉取 | 可选（二期 peer 发现） | 触发 | **拥有** |
| OverlayBD/ublk 设备、缓存/GC/P2P/S3 凭证 | — | 消费设备 | **拥有** |
| 兼容性自检/上报 | 消费 | 校验 | **拥有** |

**分层原则**：

1. **数据面不跨层**：L1 永远不直接碰 overlaybd/ublk/S3 大对象；
2. **控制面不落地**：L0 只调度 image 引用与 compatibility class，不知道
   设备/缓存/peer；
3. **协议单向**：L1→L2 只有 UDS 管理请求；P2P 端口只服务同层节点。

### 数据路径

```text
guest 读 block
  → virtio-blk (rootfs) / file-backed memory (memory)
  → [L1] Pod 内 bind-mount 设备节点 → [L2] ublk → OverlayBD ImageFile
  → cache miss → DART（本地缓存 → peer → S3；eager 仅 manifest/vmstate 等小对象）
```

> native（阶段 2）与 overlaybd（阶段 3+）的**完整数据链路图**（拉取、组装、
> 共享、写层、DART 入口差异）见细节文档 §6.4。要点：native 是"agent 组装
> 完整文件 + 每实例写层（节点两份数据）"；overlaybd 是"只读 lower 设备共享
> + 按需 range read（节点只有 DART 一份数据）"。

## 架构选择

| 选择 | 结论 | 备选与理由 |
|------|------|-----------|
| 数据面归属 | **节点级 DaemonSet（双进程：Go 编排 + vendored Rust 块设备进程）** | Pod sidecar（弃：无法共享块缓存/设备/page cache）；driver 内嵌（弃：数据面与生命周期耦合） |
| Firecracker 进程归属 | **保留在 fastlet Pod** | agent 全权持有（弃：生命周期零迁移优先；快照 memory 导出可用 driver 代读规避 ptrace） |
| 设备数据面 | **OverlayBD + ublk（AgentENV 路径，统一 rootfs/memory）** | E2B UFFD+NBD 双路径（弃：两套数据面）；NBD-only（弃：memory 按需不可用） |
| 上游依赖 | **vendor AgentENV `storage/` 子图 + pin**（实测自包含，无 cgo） | 参考重写（弃：overlaybd 3.5 万行重写不现实）；fork 为必要时才走的维护路径 |
| 内核基线 | **Linux 6.8+，部署环境要求** | 5.10 + NBD 降级（弃：双数据面维护成本） |
| P2P | **集成 [DART](https://github.com/data-accelerator/dart)（同容器独立进程，零代码集成）** | DART：Go 只读缓存 + P2P 分发树（HRW 确定性归属，无 tracker/leader）、4MiB block HTTP Range、DaemonSet 形态、零依赖（go.sum 为空）、Apache-2.0；overlaybd 经其 HTTP 前缀模式对接（上游官方用法）。自研 seed 方案降级为备选；AgentENV 的 iroh P2P 为实验特性（DHT/中继、未生产验证、平台面不在 vendor 范围）不采用 |
| 快照触发 | **仅用户显式 API** | 被动迁移（弃：批量风暴与控制面编排复杂度本期不做） |
| 快照模型 | **控制面 SandboxSnapshot CRD + 链式增量 + 定期 flatten** | fastlet 本地快照（弃：无法跨节点恢复/配额治理） |
| 恢复拉取 | **统一对象存储回源** | 源亲和/增量层 P2P（弃：本期不做跨主机专门机制） |
| UDS 认证 | **socket 属组 + namespace/PodUID 身份头** | mTLS（弃：平台自有组件间的误用防护足够） |
| 凭据 | **builder 写 AK / agent 只读 AK 分离** | 共享 Secret（弃：写凭据扩散到只读节点） |

## 核心设计决策

| 决策 | 结论 | 备注 |
|------|------|------|
| runtime-agent 部署形态 | 节点级 **DaemonSet**，不做 Pod sidecar | 共享块缓存、只读设备与宿主 page cache |
| Firecracker 进程归属 | 保留在 **fastlet Pod** | driver 生命周期零迁移；agent 只拥有块数据面 |
| P2P 发现机制 | **由 DART 承担**（DNS seed + roster 交换 / EndpointSlice watch），不自研 | 无 tracker/leader（HRW 确定性归属）；原 seed/placement registry 设计废弃 |
| UDS 认证 | **socket 属组 + namespace/PodUID 身份头** | mTLS 仅在信任模型变化时评估 |
| 运行态快照与 API 保存/恢复 | **阶段 4 实施**（当前焦点为启动流程） | 仅用户显式触发；需修订 architecture.md non-goal 对应条款；启动流程不依赖 |
| 快照资源模型 | **控制面 `SandboxSnapshot` CRD**（阶段 4） | owner/租户/TTL/配额/引用计数/状态机 |
| 增量链策略 | **链式增量 + 定期 flatten**（阶段 4） | 链深阈值触发后台 compact；共享层引用计数保护 |
| memory 捕获机制 | **dirty-range 导出（`process_vm_readv`），driver 代读优先** | 已验证（`scripts/firecracker-mem-backend-check.sh`，v1.16.1）：file-backed memory restore 为 **MAP_PRIVATE（COW）**，guest 写不落文件——seal upper layer 方案排除；driver 是 firecracker 父进程，默认 ptrace_scope 下代读合法 |
| 启动路径 | **restore 为唯一路径（golden snapshot），cold boot 降级删除** | 产物永远完整 + 调度强制 compat 匹配 → cold boot 无触发场景（死代码）；节点不再预装 kernel；恢复失败 = 显式 Failed + 重试调度 |
| overlaybd/ublk 依赖方式 | **vendor 集成**（AgentENV `storage/` 子图 + `libublk-rs-sys` pin） | 不 cgo；Rust daemon 独立进程；上游风险与 fork 触发见细节文档 §2.5 |
| 节点内核基线 | **Linux 6.8+（部署环境要求）** | 无 5.10/NBD 降级路径；自检 fail-closed |
| P2P 载体 | **集成 [DART](https://github.com/data-accelerator/dart)**（同容器独立进程，零代码集成） | 无 tracker/leader（HRW）、零依赖 Go、HTTP 前缀模式对接 overlaybd（上游官方用法）；自研 seed 方案废弃；AgentENV iroh P2P（实验性）不采用 |

## 关键能力概述

> 实现细节一律见 [firecracker-on-demand-loading-details.md](firecracker-on-demand-loading-details.md) 对应章节。

| 能力 | 概述 | 细节 |
|------|------|------|
| Artifact 契约 | `Image → index → manifest → layers`；builder 补 index + SHA256SUMS；manifest 增强 compatibility 元组与 layers 字段；凭据读写分离 | §1 |
| runtime-agent | DaemonSet **三进程**（Go 编排 + vendored Rust 块设备进程 + DART）；UDS v1（Pin/Lease/Seal/Health）；设备租约生命周期；上游依赖维护策略（fork 触发条件）；6.8+ 部署环境要求 | §2 |
| driver 改造 | 保留生命周期骨架；替换 rootfs 拷贝为设备租约、PullImage 为 PinImage；**restore 为唯一启动路径**（golden snapshot，cold boot 已移除） | §3 |
| 运行态快照 | 阶段 4 实施（不阻塞启动流程）；两阶段可见性（本地 seal → 远端 commit）；guest quiesce；memory 捕获机制依赖写穿验证（待办 1）；增量链 + flatten；恢复失败不静默降级 | §4 |
| 设备/缓存/GC | 只读设备节点级共享、writable 层 per-lease；引用来源 = pin + lease；LFU GC 保留快照引用豁免 | §5 |
| P2P | 集成 DART（同容器独立进程，HTTP 前缀模式）；agent 保留源选择链与租户边界（公共对象走 dart、增量层直连 S3）；无自研 seed/placement registry | §6 |
| 安全/兼容/可观测 | UDS 属组 + 身份头；compat class 匹配 restore；启动阶段拆分与 SLO | §7-9 |

## 风险与问题

### 风险表

| 风险 | 影响 | 缓解 |
|------|------|------|
| AgentENV crate 未稳定解耦 | 升级/维护成本 | 固定 commit + 定期 rebase 评估；fork 触发条件明确（细节 §2.5） |
| Firecracker fork 漂移 | snapshot ABI 与安全补丁压力 | 小补丁集、自动 rebase/CVE 跟踪、compat class 隔离 |
| 6.8+ ublk 内核要求 | 节点池受限 | 部署环境明确要求；自检 fail-closed；不提供降级 |
| 对象存储进入同步 I/O | guest tail latency 放大 | 共享 cache、前台优先、P2P、熔断、容量限流 |
| 快照链过深 | lookup/GC 复杂 | 最大层数、后台 flatten/compact |
| 双控制面状态漂移（fastlet↔agent） | 泄漏或错误状态 | agent journal、lease、周期 reconcile |
| 跨租户 P2P/去重 | 侧信道/权限风险 | 公共层内容寻址 + 节点身份；tenant 层回源 |
| guest 未 quiesce 的快照 | 恢复后状态不一致 | 快照前 execd quiesce 强校验；未就绪拒绝 |
| 从快照恢复失败 | 运行状态丢失 | 显式 Failed 不静默降级；重试调度（无冷启动 fallback，restore 为唯一路径） |
| memory 捕获机制不确定（写穿 vs COW） | 快照数据路径设计翻盘 | 写穿验证（待办 1）前置，验证前不冻结阶段 4 实现 |

### DART 集成风险（重点问题）

P2P 载体采用 DART（未生产就绪、无社区），主要风险：

1. **上游状态**：README 明示 "hardening in progress"（0 star/0 fork/36 commits）；
   缓解：固定 commit pin + 独立进程隔离故障、390 tests race-clean 为质量
   基线、Apache-2.0 + 纯标准库使 fork 自维护成本可控；
2. **发现机制**：DNS seed + roster 交换（5s 加入/60s 移除）或 k8s
   EndpointSlice watch，无 tracker/共识——peer 平面信任集群内网（上游安全
   模型如此），与我们的信任域一致；
3. **租户边界**：DART 无租户概念，隔离靠 agent"哪些 URL 交给 DART"决策
   （公共对象 → DART，增量层 → 直连 S3）；
4. **presigned URL**：长对象拉取需足够 TTL（agent 签发时控制）。

详见细节文档 §6。原自研 seed 方案（TTL 滞后/种子热点/自身 QPS 缺陷）已
废弃，不再演进。

### 待决策项

1. **阶段 1 agent 部署载体**：独立 DaemonSet（推荐，阶段 2/3 原地扩展）
   vs 临时并置容器（与 fastlet 同 Pod，阶段 2 再拆）。
2. **快照 CRD 语义细节**（阶段 4）：TTL 默认值/上限、每租户配额、引用计数
   控制面 vs agent 分界。

> 已定案：memory 捕获机制 = dirty-range 导出 + driver 代读（写穿验证完成，
> 见决策记录）；待办 1 已完成（`scripts/firecracker-mem-backend-check.sh`）。
> 已定案：**restore 为唯一启动路径**，cold boot 降级删除（恢复失败不再
> fallback 冷启动；compat 匹配为调度硬约束）。

> 后续演进（本期明确不做）：运行态快照保存/恢复（snapshot/resume，阶段 4
> 实施）、被动迁移（drain/failover 批量快照）与跨主机恢复的增量层拉取优化。

### 待办事项

1. ~~**验证 Firecracker file-backed memory 写穿语义**~~ **已完成**
   （v1.16.1）：结果为 **MAP_PRIVATE（COW）**——判定 1 maps 标志 `rw-p`；
   判定 2 Shared_Dirty=0；判定 3 restore-mem.bin 与 memory.snap 一致。
   memory 捕获机制定案：dirty-range 导出（`process_vm_readv`），driver
   代读优先（父进程，默认 ptrace_scope 下合法），agent 提权为备选。
2. **修订 docs/concepts/architecture.md non-goal**（阶段 4 随快照能力一起）：
   快照/保存/恢复改为 API 触发能力（跨 Fastlet 实例生存仍维持 non-goal）。

## 实施阶段

按"消费模式演进"划分四阶段，每阶段一个独立价值增量、依赖线性：

```text
阶段 1  native 全量 + 回源 S3    → 打通寻址链 + restore（无 P2P）
阶段 2  native 全量 + P2P        → 验证 P2P 价值（native 场景，采集命中率）
阶段 3  overlaybd 按需 + P2P     → 数据面换 OverlayBD/ublk，复用 P2P
阶段 4  运行态增量快照/恢复       → 用户显式 API 保存/恢复（后续实施）
```

### 阶段 1：native 全量 + 回源 S3（无内核依赖）

- builder：index 对象 + SHA256SUMS 发布；
- **agent 最小集部署**（Go 编排：UDS v1、PinImage/PullImage、本地缓存、
  S3 只读凭证；**无 P2P**，全部回源）；
- driver：拉取链（index→manifest→digest 校验落盘）+ snapshot restore
  （全量 eager），**不直连 S3、不持凭据**；
- 验收：`Image` 引用发布产物、空缓存 restore 成功；`warmImages` 真正拉取；
  E2E 用 `scripts/firecracker-e2e.sh` 扩展 restore 用例。

### 阶段 2：native 全量 + P2P（集成 DART）

- **DART 作为 agent 容器内独立进程接入**（零代码集成）：`dart` 二进制 +
  配置（cache-dir = agent 块缓存目录、DNS 发现 headless Service、
  peer-listen 节点地址）；agent/Rust daemon 经
  `http://127.0.0.1:8145/dart/<upstream URL>`（presigned URL）拉取；
- 公共对象走 DART（P2P 分发），per-tenant 增量层直连 S3（agent 源选择
  决策，DART 不感知租户）；
- **本阶段核心产出是 P2P 价值验证数据**：DART 的
  `block_source{source="cache|peer|origin"}` 指标 → 回源占比、命中率；
- 验收：空缓存 warmImages 可从 peer 命中；冷启动风暴回源显著下降
  （origin 放大 < 1/N）；DART 升级/回滚独立于 agent。

### 阶段 3：overlaybd 按需 + P2P

- Rust 块设备 daemon（vendored AgentENV）接入，UDS v1 设备租约
  （LeaseDevices/ReleaseDevices）；
- rootfs OverlayBD + ublk 按需，memory 共享只读 ublk 设备；块缓存 +
  layer 引用计数 GC；
- P2P 复用阶段 2 能力（对象 range read 服务单元）；
- 验收：多 Pod 共享只读设备与宿主 page cache；空缓存 rootfs 按需加载且
  可从 peer 命中；删除 sandbox 后引用归零、设备回收。

### 阶段 4：运行态增量快照/恢复（后续实施，启动流程不依赖）

- 待办 1：写穿语义验证（memory 捕获机制前置）；
- 快照创建 v1（rootfs seal + 按验证结果定 memory 捕获）与 agent
  SealSnapshot；本地 provisional 快照同节点恢复；
- 控制面 `SandboxSnapshot` CRD（owner/TTL/配额/引用/状态机）与恢复调度
  （compat class 匹配）；协议增量 `SnapshotRef` / `CreateSandboxSnapshot`；
- 从快照恢复：`EnsureSandbox(snapshotRef)` + 增量链叠加（对象存储回源）
  + guest resume hook；
- 验收：用户显式保存/恢复 E2E（同租户、compat 匹配）；快照链 GC/引用
  计数正确；恢复后 execd ready。

### 后续演进（不独立成阶段，随上述阶段增强）

- 增量链 flatten/compact 上线、快照配额/TTL 治理（阶段 4 之后）；
- 工作集预取（模板 working-set manifest）、容量治理、多 compat class 灰度；
- DART 升级到生产就绪版本 / 吞吐优化（上游 roadmap）随阶段 2-4 跟进。

## 文档索引

| 文档 | 内容 | 状态 |
|------|------|------|
| [firecracker-on-demand-loading.md](firecracker-on-demand-loading.md) | 本文件：设计决策、分层、架构选择、风险 | 评审中 |
| [firecracker-on-demand-loading-details.md](firecracker-on-demand-loading-details.md) | 实现细节：协议、流程、参数、维护策略 | 评审中 |
| [sandboxtemplate-golden-image-builds.md](sandboxtemplate-golden-image-builds.md) | 发布端（builder）方案，本文消费其产物 | 上游参考 |
