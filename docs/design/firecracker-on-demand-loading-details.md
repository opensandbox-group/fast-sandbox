# Firecracker 按需加载：实现细节

> 文档类型：设计细节（配套主文档
> [firecracker-on-demand-loading.md](firecracker-on-demand-loading.md)，实现时以本文为准）
>
> 日期：2026-08-27

主文档聚焦设计决策、系统分层、架构选择与风险；本文档承载各主题的
**实现细节**（协议、流程、参数、约束），与主文档章节一一对应。

---

- [§1 Artifact 契约](#1-artifact-契约发布端与消费端对齐)
- [§2 runtime-agent 细节](#2-runtime-agent-细节)
- [§3 fastlet driver 改造细节](#3-fastlet-driver-改造细节)
- [§4 运行态快照细节](#4-运行态快照细节)
- [§5 设备共享、缓存与 GC](#5-设备共享缓存与-gc)
- [§6 P2P 分发细节](#6-p2p-分发细节)
- [§7 安全与凭据](#7-安全与凭据)
- [§8 快照兼容性](#8-快照兼容性)
- [§9 可观测性](#9-可观测性)

## 1. Artifact 契约：发布端与消费端对齐

### 1.1 寻址链

```text
SandboxSpec.Image (fastlet 协议，唯一请求引用)
  → <store>/index/<sha256(image)>.json        {manifestRef, artifactDigest, updatedAt}
  → <store>/<manifestDigest16>/manifest.json  校验结构 + files.digest
  → 按 digest 下载缺失文件（native 全量 / overlaybd 先只拉 manifest+vmstate）
  → <StateRoot>/images/<sha256(image)>/       本地缓存落盘
```

- 消费端缓存 key 不变（`sha256(imageRef)`），下游 `EnsureSandbox`/
  `resolveRootfsImage`/GC 全部零改动（native 阶段）。
- 发布顺序保证：文件 → manifest → index；消费端看到 index 时产物必完整。
- 新 build 覆盖 index 指向新 manifest；旧 manifest 目录保持（content-addressed），
  由对象存储生命周期策略或引用 GC 清理。

### 1.2 发布端改动（builder，`publish.go`）

1. publish 成功后，额外上传 `index/<sha256(spec.Image)>.json`（manifest 之后）；
2. 补传 `SHA256SUMS`（消费端以 manifest.files 校验为准，该文件供审计）。

**index 语义约束（评审定案）**：

- **image 引用必须逐字节一致**：index key 由原始 image 字符串推导，与
  消费端 `SandboxSpec.Image` 必须完全一致——**不做任何规范化**（不加默认
  tag、不 pin digest、不 trim 空白）。发布端与消费端对空/空白 image
  均显式拒绝（空串会 hash 出合法 key，静默覆盖 `index/<空串hash>.json`）；
- **last-writer-wins**：同一 store root 下同 image 并发发布时 index 指针
  覆盖。每次写入前构建已完整（artifacts → manifest → index 顺序），
  因此不存在半成品可见状态，但赢家不确定——发布方按 image 串行化；
- **manifestRef 命名空间**：build 目录前缀为 `sha256(manifest)[:16]`
  （64 位）。index 使该前缀进入公开寻址链，碰撞即两构建共享命名空间——
  概率可接受（content-addressed 的既有语义），**不得进一步缩短前缀**。

### 1.3 manifest 增强

在现有 `compatibility` 基础上按 aone 方案结构化为可机器匹配的元组：

```json
{
  "compatibility": {
    "architecture": "x86_64",
    "firecracker": {"version": "1.16.1", "binaryDigest": "sha256:...", "snapshotFormatVersion": "..."},
    "cpu": {"vendor": "GenuineIntel", "template": "T2", "featureHash": "sha256:..."},
    "kernel": {"digest": "sha256:...", "bootArgsHash": "sha256:..."},
    "guest": {"abi": 1}
  },
  "rootfs": {"logicalSize": 9771050598, "layers": [{"digest": "sha256:...", "size": 987654321}]},
  "memory": {"logicalSize": 4294967296, "layers": [{"digest": "sha256:...", "size": 123456789}]}
}
```

`files`（发布文件名 → digest/size）与 `layers`（数据段 → digest/size）并存：
`files` 服务 native 全量拉取，`layers` 服务 overlaybd range read。两者 digest
一致（layer.lsmt 的 sha256 与 `files["overlaybd/rootfs/layer.lsmt"].sha256`
相同），实现时以同一来源生成。

### 1.4 凭据分离

- **builder**：写 AK（`output.publishSecretRef`，现状不变）；
- **runtime-agent**：只读 AK 或节点 IRSA/实例角色（**不共享 builder 的写
  凭据**）；agent 持有，**不向 guest 暴露**。

**两端字段映射（实测确认，chain E2E 已真实跑通）**：两端没有共享同一套
JSON schema，字段名不同，语义一一对应：

| 语义 | builder 侧（`publishSecretRef` Secret keys） | agent 侧（`registryconfig.Credential`） |
|------|----------------------------------------------|------------------------------------------|
| AccessKeyId | `accessKeyId` | `Username` |
| SecretAccessKey | `secretAccessKey` | `Password` |
| 存储端点（连接地址，可带 scheme/端口） | `endpoint`（注入为 `AWS_ENDPOINT_URL`） | `Endpoint`（新增字段；缺省回退 `Host` 补 `https://`） |
| 端点 host（匹配键） | — | `Host`（`registry.json` 条目，匹配 store root / `FAST_SANDBOX_ARTIFACT_ENDPOINT` 的 host） |
| region | `region`（注入为 `AWS_DEFAULT_REGION`；agent 侧默认 `us-east-1`，可用 `WithRegion` 覆盖） | —（Credential 无 region 字段） |

实际注入路径（chain E2E `gen-registry.go` 的手搓映射即此对应关系）：
`publishSecretRef.{accessKeyId,secretAccessKey,endpoint,region}` 与
`registry.json` 的 `{host, username, password, endpoint}` 值一致、语义等价。
`Host` 是 registryconfig 的既有匹配键（镜像仓库语义），agent 场景下承载
store endpoint host；`Endpoint` 是本设计新增的**连接地址**字段（带
scheme，不被 `NormalizeHost` 剥掉），两者都要配。部署时由一个值来源
（Secret）派生两处，避免手抄不一致。

## 2. runtime-agent 细节

### 2.1 部署形态

- 每节点一个 **DaemonSet**（privileged，hostPath: `/dev/ublk-control`、
  `/dev/` 访问、块缓存目录 `/var/lib/fast-sandbox/firecracker/cache`、
  设备目录 `/run/fast-sandbox/firecracker/devices`、UDS socket
  `/run/fast-sandbox/firecracker/runtime.sock`）。
- **不做 Pod sidecar**：fastlet Pod 只挂载少量 hostPath（UDS socket +
  设备目录），共享能力全部在节点层。
- 节点预检（启动自检 + 周期上报）：`/dev/kvm`、`/dev/ublk-control`、
  内核版本、OverlayBD 版本、P2P 端口、对象存储连通性、CPU/内核与已缓存
  manifest 的 compatibility。
- **节点不预装 guest kernel（vmlinux.bin）**：restore 为唯一启动路径，
  不引导内核（快照内已含 guest 状态）；kernel 是构建期资产（builder
  拍快照时使用），manifest 记录其 digest 仅供 compat 校验。

### 2.2 UDS 管理 API（版本化，v1）

> 启动流程（阶段 1-3）只使用：PinImage / UnpinImage / LeaseDevices /
> ReleaseDevices / ListLeases / Compatibility / Health。快照相关
> （PinSnapshot / LeaseSnapshotDevices / SealSnapshot）随阶段 4 一并实现。

```text
PinImage(request_id, image)                       -> {manifest_digest, ready}
UnpinImage(request_id, image)
PinSnapshot(request_id, snapshot_ref)             -> {manifest_digest, ready}   # 阶段 4：快照恢复
LeaseDevices(request_id, sandbox_id, image,
             mem_size, rootfs_writable)           -> {rootfs_dev, mem_dev,
                                                      lease_id, manifest_digest}
LeaseSnapshotDevices(request_id, sandbox_id,
                     snapshot_ref)                -> {rootfs_dev, mem_dev,      # 阶段 4：增量链叠加
                                                      lease_id, manifest_digest}
ReleaseDevices(request_id, lease_id)
SealSnapshot(request_id, lease_id, snapshot_id)   # 阶段 4：运行态快照 seal + 上传
ListLeases(request_id)                            # 恢复/清点
Compatibility(request_id)                         -> 节点 compatibility class
Health(request_id)                                -> {ok, cache_bytes, devices, ...}
```

- 所有变更 RPC 带 `request_id` 幂等；agent 本地 journal 先落盘再执行
  （两段提交原则，防止超时重试产生双租约）。
- socket 权限：`0660`，属组 `fast-sandbox`；fastlet Pod 以 hostPath 挂载
  socket 文件并通过 namespace+PodUID 身份头做最小鉴权（见 §7）。

### 2.3 设备生命周期

```text
LeaseDevices:
  1. PinImage（index→manifest→layer 定位）
  2. rootfs: 只读 lower layers（内容寻址，已缓存共享）+ per-lease writable
     upper layer → overlaybd Image → ublk 设备
  3. memory: 共享只读 ublk 设备（同 image 的 leases 共享同一设备，page cache
     复用）；write 全部走 guest RAM，无需 writable layer
  4. 把设备节点 bind-mount 到 <devices>/<lease_id>/{rootfs,memory}（hostPath
     目录，Pod 已挂载）
  5. journal 提交，返回 lease_id

ReleaseDevices:
  1. 校验 lease 归属 → 移除 bind-mount
  2. 引用计数减一；计数归零才 detach ublk 设备
  3. writable upper layer seal/丢弃按 snapshot 策略
```

### 2.4 块设备进程（vendored Rust daemon）

AgentENV 的 `storage/` 目录已验证为**自包含子图**（实测 commit `4a2d610`）：

- `storage/util`（1.8k 行）、`storage/overlaybd`（34.9k 行，纯 Rust 重实现
  的 LSMT/range-read/block-cache，无 C++ 依赖）、`storage/ublk`
  （2.7k 行，ublk 设备服务）只依赖 crates.io + `libublk-rs-sys`
  （git pin `c6a3e06`），**可独立 vendor**；
- `storage/overlaybd` 的 `backend/oss.rs` 引用 `object-store-operator`
  （667 行，仅依赖 opendal，本身独立）且被 `full` feature 门控——需要
  远端 range read 时连同 vendor；
- `storage/ublk-daemon` 是唯一纠缠 AgentENV 平台薄 crate（warm-pool /
  observability / linux-cap，均数百行且各自独立）的组件；它本来就是
  **独立进程 + RPC 管理**形态（client/server/protocol），直接作为
  runtime-agent 内的第二个进程 vendor，剥离或原样带这三个薄依赖；
- 需要消除的 AgentENV 痕迹：`metrics.rs` 的 `agentenv_*` 指标前缀
  （几十处改名）与许可证保留（MIT，fork 需保留版权声明）。

集成形态：**Go 编排进程拉起 Rust daemon 子进程，经进程内 UDS 控制
设备**（Go 侧不直接引用任何 Rust crate，不引入 cgo）。daemon 提供的
设备池化/restack（warm-pool）能力即 agent 的 `LeaseDevices/ReleaseDevices`
底层实现，RPC 协议适配成本低。

### 2.5 上游依赖与维护策略

vendored 的是**两条上游**，风险与维护机制不同，必须明确：

| 上游 | 形态 | 风险 | 缓解 |
|------|------|------|------|
| AgentENV `storage/`（util/overlaybd/ublk/ublk-daemon） | GitHub repo，workspace 内子目录，无独立版本发布（0.1.0）、**无稳定 API 承诺** | ① commit 级漂移：升级=重审 API；② 上游重构/删除我们依赖的接口；③ bug/CVE 修复依赖上游节奏；④ 大仓库（CI 拉取、浅 clone 成本） | 固定 commit pin + 定期（如每月）rebase 评估；构建产物由 CI 锁定；license 保留 MIT 声明。**注意：AgentENV 的 P2P 层（src/p2p/ + src/overlaybd/p2p/，iroh 传输）位于平台面，不在 storage/ 子图，不在 vendor 范围** |
| `libublk-rs-sys`（ublk-org/libublk-rs） | git 依赖，rev pin `c6a3e06` | git 依赖无法锁包管理器镜像，构建环境需能访问 GitHub；上游改动影响内核 ABI 绑定 | vendor 到内部仓库/镜像；它是内核社区官方库（相对稳定），保持 rev pin 即可 |
| `object-store-operator`（feature "full" 时） | AgentENV 子 crate，667 行 | 同上，但代码量小 | 连带 vendor；或禁用 full feature 自实现 backend（成本低） |

**fork 触发条件（有必要时）**：

1. 上游修复我们提交的 bug/CVE 响应周期超过阈值（如 2 周无回应/无 release）；
2. 需要修改 `storage/` crate 内部（协议适配、性能优化、指标改名）且上游
   不愿合入；
3. 上游重大重构（如 overlaybd 格式/API 破坏性变更）导致固定 pin 升级成本
   失控。

**fork 后的维护机制**（触发即执行，不预先建设）：

- fork 保留最小 patch 集（协议剥离、指标前缀、构建适配），与上游差异
  记录在独立 PATCHES 清单；
- 定期 rebase 上游（自动 CI 检查冲突，冲突人工裁决），保持安全补丁
  可跟进；
- 固定 commit 的构建 + KVM E2E 纳入本仓库 CI，fork 漂移立即暴露；
- 若 fork 后长期无法合回上游，视维护负担决策是否继续 fork（成本是
  overlaybd crate ~3.5 万行，需专职维护者）。

**结论**：默认以 vendor + pin 运行（低成本）；fork 是备用的"必要的
维护路径"，由上述触发条件驱动，而不是一开始就 fork。

### 2.6 部署环境要求（内核基线）

**部署环境明确为 Linux 6.8+ 内核节点池**（与 AgentENV 同基线，ublk 数据面
所需），不提供 5.10/NBD 降级路径——本项目不背双数据面维护成本。

| 数据面 | 内核要求 | 部署环境（6.8+ 节点） |
|--------|----------|-----------------------|
| rootfs 按需 | ublk: 6.0+ | **ublk** |
| memory 按需 | ublk: 6.0+ | **ublk** |
| eager native | 无 | ✓（native 作为消费模式保留，不依赖 ublk） |

节点启动自检 `/dev/ublk-control` + 内核版本，不满足则 agent fail-closed
（不注册为可用节点），调度侧不向其分配按需加载的 sandbox；native 模式
无此要求。

## 3. fastlet driver 改造细节

### 3.0 代码落点（runtime-agent UDS client）

runtime-agent 是 **Firecracker 专用组件**（只服务 firecracker driver），
协议与实现全部内聚在 `internal/runtime/firecracker/` 下，镜像
`internal/runtime/boxlite/{driver,protocol,server,state}` 的先例，不新建
`internal/protocol/` 公共协议包（该层级只给跨领域共享组件使用）：

```
internal/runtime/firecracker/
├── driver.go / client.go / images.go...   # 现有 driver（L1）
├── agent_client.go                        # 新增：UDS client（driver 内）
└── agent/                                 # 新增：agent（L2）全部内聚
    ├── protocol/                          # UDS 协议类型（driver + agent 共享）
    ├── server/                            # UDS server
    ├── state/                             # 租约 journal / 引用计数
    └── ...                                # 缓存 / DART 拉起 / Rust daemon 管理
cmd/firecracker-runtime-agent/             # agent 入口（薄）
```

- 依赖单向：driver → `agent/protocol`（仅消息类型）；agent server 自用
  本包；`cmd` 为薄入口；
- client 实现仿 boxlite driver 的 UDS HTTP client（DialContext: unix）+
  driver 现有 `newClient` 注入模式（测试可 fake）；
- agent socket 路径：环境变量 `FAST_SANDBOX_RUNTIME_AGENT_SOCKET`
  （仿 `FAST_SANDBOX_NODE_CLEANUP_SOCKET` 先例，cmd/fastlet/main.go:97）；
- agent 包内组织：UDS server、租约 state（journal）、缓存管理、DART
  子进程拉起、Rust 块设备 daemon 子进程管理（进程内 UDS）、S3 client
  （presigned 签发）、源选择链。driver 保持 `runtimecontract.Driver`
  接口不变，fastlet sandbox manager 无感。

### 3.1 保留不动

- `EnsureSandbox` 的生命周期骨架（幂等、OTel 追踪、阶段拆分）；
- Firecracker 进程管理（launcher、probe、killAndForget、恢复）；
- 网络 slot（Acquire/Release）、`GetAccessDescriptor`；
- Infra Components（`PrepareInstance` + GuestCopy）；
- Sandbox 状态持久化（`sandboxes/<id>/meta.json`）与 `RecoverRuntimeResources`；
- NodeJanitor 对接（`--id` 截断约定）。

### 3.2 替换

| 现状 | 替换为 |
|------|--------|
| `resolveRootfsImage` + `prepareInstanceRootfs`（reflink 拷贝） | agent `LeaseDevices` 返回的设备路径 |
| `configureVM` 的 `AttachDrive(rootfs.img)` | `AttachDrive(rootfs_dev)`；memory restore 时 `LoadSnapshot(mem_file_path=mem_dev)` |
| `PullImage`（查本地） | agent `PinImage`（节点级去重预热） |
| driver 内 image 缓存目录 + LFU GC（images.go） | 删除；缓存与 GC 归 agent（layer 引用计数） |

### 3.3 restore 流程（唯一启动路径，native 首期 / overlaybd 二期）

> **restore 是唯一启动路径**：golden snapshot 恢复，cold boot 分支已删除
> （产物永远完整 + 调度强制 compat 匹配 → 冷启动无触发场景）。恢复失败 =
> 显式 Failed + 重试调度，不 fallback 冷启动（§8 兼容性硬约束）。

```text
EnsureSandbox(image, cpu, mem, ...):
  1. agent.PinImage(image)                      # index→manifest→（native: 全量拉取）
  2. agent.LeaseDevices(image, mem)
  3. 网络 slot（不变）
  4. firecracker 进程启动（不变）
  5. restore 配置：
     machine config（按请求 cpu/mem）         # mem 不得小于快照 size
     + drive(rootfs_dev) + nic
     + PUT /snapshot/load{snapshot_path: vmstate.snap,
                          mem_file_path: mem_dev}
     （无 boot-source：快照内已含 guest 状态，不引导 kernel）
  6. InstanceStart + 轮询 Running（不变）
  7. sandbox 删除/失败 → agent.ReleaseDevices + agent.UnpinImage(按引用)
```

- `vmstate.snap` 与 manifest 由 agent 在 PinImage 时 eager 拉取并缓存；
- restore 前 agent 做 compatibility 匹配，**不匹配拒绝调度**（稳定错误
  `SNAPSHOT_INCOMPATIBLE`，见 §8）；
- 请求级个性化（cpu/mem/entrypoint/env）不受影响：cpu/mem 走
  `resolveMachineConfig`（mem 下限 = 快照 size），entrypoint/env 走既有
  Infra 注入。

## 4. 运行态快照细节（阶段 4，后续实施）

> 当前焦点为启动流程（阶段 1-3），本节设计保留但**不阻塞启动**；相关协议
> 增量与 CRD 随阶段 4 一起实施。

### 4.1 产品语义

docs/concepts/architecture.md 现行 non-goal："不提供跨 Fastlet 实例生存、
快照、暂停/恢复，或持久化 Sandbox 存储"。运行态快照是产品决策的
**修订**：用户显式 API 的**保存/恢复**成为本方案核心能力之一；**跨
Fastlet 实例生存（节点故障时的自动迁移）仍维持 non-goal**。该架构文档
需随本方案同步修订（独立文档变更）。

fastlet 协议原则：**新能力只允许新增字段/RPC，不改变既有语义**。
协议增量完整清单：

| 层 | 增量 | 说明 |
|----|------|------|
| fastlet API | `SandboxSpec.SnapshotRef`（新增字段，阶段 4） | 从运行态快照恢复（优先级高于 `Image`）；`Image` 语义不变 |
| fastlet API | `CreateSandboxSnapshot`（新增 RPC，阶段 4） | 对运行中 sandbox 拍快照；`request_id` 幂等语义同现有 RPC；流程：控制面建 CR → fastlet 触发 → driver（quiesce/pause/vmstate）→ agent（UDS SealSnapshot）→ 控制面标记 CR Ready |
| 控制面 | `SandboxSnapshot` CRD（新增资源，阶段 4） | owner/TTL/配额/引用/状态机；`SandboxPool`/`SandboxTemplate` 零改动 |
| 不变 | `SandboxSpec.Image` 语义、现有全部 RPC 语义 | 冷启动/黄金 restore 路径不受影响 |

### 4.2 触发场景

| 场景 | 触发方 | 说明 |
|------|--------|------|
| 用户显式保存/恢复 | 用户 API | 对运行中 sandbox 拍快照（快照资源）；之后从快照新建 sandbox 恢复 |

### 4.3 快照资源模型（L0 控制面）

新增 `SandboxSnapshot` CRD（归属控制面，与 SandboxTemplate 同级）：

- 字段：`spec.sandboxRef`（来源沙箱）、`spec.ttlSeconds`、租户/命名空间、
  `spec.reason`（explicit）；
- 状态机：`Creating → Ready / Failed → Deleting`；
- 语义：owner/租户隔离、配额（每租户快照数/总字节）、TTL 过期清理；
- 引用：快照引用 golden base + 增量链 layer，删除快照只删引用并减少
  layer 计数（layer 延迟 GC，绝不盲删共享层）；
- 恢复：`CreateSandboxRequest` 携带快照引用（新增字段），控制面调度到
  compat class 匹配的节点；增量链层统一从对象存储拉取（快照保存时已上传，
  天然跨节点可用，不需要源亲和/专门拉取机制）。

### 4.4 快照创建流程

两阶段可见性：**本地 seal 先行，远端 commit 后至**——同节点可立即用本地
provisional 快照恢复；远端 manifest 提交后才允许跨节点恢复。

```text
guest fs quiesce（execd sync/fsfreeze）
  → driver: FC pause
  → driver: state-only snapshot → vmstate.snap
  → agent: seal rootfs writable upper layer → 内容寻址 layer（rootfs 增量）
  → agent: 捕获 memory 增量（dirty-range 导出，已验证定案，见下）
  → agent: 上传 layers + vmstate → 最后 commit 快照 manifest
  → 控制面: 快照资源标记 Ready
  → source VM: 继续运行（resume-on-error）或按策略删除
```

- **memory 捕获机制（已验证定案，`scripts/firecracker-mem-backend-check.sh`）**：
  v1.16.1 的 file-backed memory restore 为 **MAP_PRIVATE（COW）**——guest
  写落在进程匿名内存，文件只保留干净页。因此快照的 memory 增量**必须**
  dirty-range 导出（`process_vm_readv`）：
  - 优先"driver 代读"：driver 是 firecracker 的父进程，默认
    ptrace_scope=1 下读取子进程内存合法，无需节点 sysctl 或提权；
    读取的 dirty ranges 经 UDS 流式交给 agent 打包 layer；
  - 备选：agent 提权治理（节点 ptrace_scope / CAP_SYS_PTRACE），仅在
    driver 代读不可行时评估。
- 一致性约束：rootfs 增量与 memory 增量必须对应**同一个 pause epoch**；
  source VM 继续运行的前提是快照失败时可完整回滚（resume-on-error）；
- 快照创建 API 超时不得取消已进入 commit 阶段的上传，控制面通过快照资源
  状态查询最终结果（幂等）。

### 4.5 增量链与 flatten

- 快照 = golden base + 有序增量链 `Δ1 → Δ2 → ...`（rootfs 与 memory 各自
  成链），链上每层内容寻址、可被多个快照共享；
- 链深超阈值（如 >8）触发后台 flatten/compact：把链折叠为较少的层；
- GC 只回收无任何快照/沙箱引用的 layer；flatten 期间旧层仍被引用，
  完成后原子切换引用（manifest 条件写）。

### 4.6 从快照恢复流程

```text
控制面调度（compat class 匹配）
  → fastlet: EnsureSandbox(snapshotRef)      # 新增字段
  → agent: PinSnapshot（manifest + vmstate eager；增量链层从对象存储拉取）
  → agent: LeaseDevices（lower = golden base + 增量链，叠加实例写层）
  → driver: restore 分支（vmstate + File memory backend + rootfs_dev）
  → guest resume hook（时钟/网络恢复）
  → execd health 通过 → Running
```

- 恢复目标节点：控制面按 compat class 匹配调度（**硬约束**：不匹配拒绝
  调度，不限定来源节点）；
- 恢复失败策略：增量层缺失/损坏 → 快照显式 Failed，不静默降级；无
  cold boot fallback（restore 为唯一启动路径）——失败走重试调度。

### 4.7 失败处理

| 故障 | 处理 |
|------|------|
| guest 未 quiesce 就快照 | 快照前 execd 强校验/超时，未就绪则拒绝拍快照 |
| seal/上传中断 | 本地 provisional 保留，远端 commit 未发生 → 快照资源 Failed，重试 |
| source VM resume 失败 | 强制 teardown 并标记 sandbox lost，不得把 frozen VM 标 Running |
| 增量层丢失/损坏 | digest 校验失败 → 隔离该层，快照 Failed，恢复方不静默降级 |
| 从快照恢复失败 | 快照显式 Failed + 重试调度（无 cold boot fallback） |

## 5. 设备共享、缓存与 GC

### 5.1 共享模型

- **只读 lower layers / memory 设备**：节点级唯一，多个 lease 共享
  （ublk 设备 + 宿主 page cache 复用是 AgentENV 已验证的核心收益）；
- **writable rootfs upper layer**：per-lease，不能共享；
- 缓存 key：`layer digest + logical offset`（不能只用 image 引用）。

### 5.2 缓存层次

```text
guest 访问 → Linux page cache → OverlayBD bounded block cache → [P2P] → S3
```

### 5.3 引用与 GC

- 引用来源：PinImage/PinSnapshot 的 pin（warmImages/模板/快照保活）＋
  活跃 LeaseDevices；
- agent 周期 GC：无引用的 layer 按 LFU 淘汰（保留 warm pin 与快照引用
  豁免；快照引用的 layer 由控制面引用计数保护，agent 无权删除）；
- 删除顺序：lease 释放 / 快照资源删除 → 引用减一 → GC 淘汰，绝不盲删
  被引用 layer；
- 与现有 driver GC 的关系：driver 的 `TriggerImageGC` 语义迁移为
  agent 的 `UnpinImage`/周期 GC；SandboxPool 的 warmImages 机制不变
  （`FAST_SANDBOX_WARM_IMAGES → PullImage → PinImage`）。

## 6. P2P 分发细节

### 6.1 定位：数据面公共能力（对象分片级）

P2P 是**数据面公共能力**，native 与 overlaybd 共用，不依赖 OverlayBD/ublk：

| 消费模式 | 拉取单元 | P2P 服务单元（DART） |
|----------|----------|----------------------|
| native（全量 eager） | 整个对象文件（rootfs.ext4/vmstate/memory） | 对象按 block 分片拉取 + 整对象 sha256 校验（agent 侧） |
| overlaybd（按需） | layer 数据段 | 对象按 range read（DART 从 block 边界服务） |

两种消费模式走同一 DART 数据面，只有"缓存对象粒度/校验粒度"不同。native
冷启动风暴是 P2P 的核心受益场景之一，不是 overlaybd 专属。

### 6.2 集成 DART 承担 P2P 数据面

P2P 分发由 **DART**（[github.com/data-accelerator/dart](https://github.com/data-accelerator/dart)，
Apache-2.0，Go 零依赖）承担，**不自研 peer/分片缓存/发现/熔断**：

| 原自研设计点 | DART 提供 |
|--------------|-----------|
| 对象分片缓存（4MiB block） | block 缓存（磁盘 arena + 内存热集），双预算 + TinyLFU 准入 |
| peer server（HTTP Range） | 任意 HTTP Range 从块边界服务（8KiB 读传一个块） |
| 发现（seed/registry） | DNS seed + roster 交换（5s 加入）或 k8s EndpointSlice watch（dart-k8s 变体） |
| 无 tracker 无 leader | 加权 HRW 确定性归属：每节点推导同一棵分发树，无需共识 |
| 熔断/hedging/回源兜底 | per-peer 熔断、慢父节点 hedge 到祖父节点、origin 兜底 |
| cut-through 中继 | 边收边转发边缓存（多跳管线化） |
| 观测 | Prometheus：`block_source{source="cache\|peer\|origin"}` 等 |

**agent 保留的职责**：源选择链的外层（哪些对象走 DART、哪些直连 S3）、
租户边界（公共对象 → DART；per-tenant 增量层 → 直连 S3）、S3 presigned
URL 签发（DART 支持 presigned object-storage upstream）。

**来源说明（更正认知）**：AgentENV 最新版（`4a2d610`）自带**实验性 P2P**：
`P2pTransport` trait + iroh 实现（QUIC + DHT/中继去中心化，src/p2p/iroh/
transport.rs）+ overlaybd HTTP facade（src/overlaybd/p2p/facade.rs，1450 行）。
config 标注 "EXPERIMENTAL: P2P has not been tested in production"，默认禁用；
且位于平台面 `src/`，不在 vendor 的 `storage/` 子图内。**不采用**：iroh
去中心化栈与内网可控路线哲学不同、上游未生产验证。其 `P2pTransport` 接口
设计（`lookup_with_hints` provider 提示机制）可作参考，但无复用必要。

### 6.3 部署形态（同容器独立进程）

DART 作为 **runtime-agent 容器内的第三个独立进程**（零代码集成，不 fork
源码）：

- 已选理由：DART 全部实现在 `internal/`（Go internal 包规则禁止外部 module
  import），"源代码集成"实际等于 fork + module rename——维护成本高于独立
  进程；且 DART 的定位就是独立部署进程（admin 平面 + metrics + peer 监听
  齐全），HTTP 前缀模式是上游设计入口（overlaybd 官方对接方式）。
- 进程拓扑（一个 DaemonSet 容器，entrypoint 拉起三进程）：
  ```
  runtime-agent 容器
  ├── dart              :8145 http（agent/Rust daemon 经 127.0.0.1 调用）
  │                      :8147 admin/metrics    :9000 peer-listen（节点 IP）
  ├── Go 编排进程        UDS :/run/fast-sandbox/firecracker/runtime.sock
  └── Rust 块设备进程    OverlayBD + ublk（vendored AgentENV）
  ```
- 关键配置：`-cache-dir=/var/lib/fast-sandbox/firecracker/cache/dart`
  （与 agent 块缓存同目录体系）、`-discover=dns:dart.default.svc.cluster.local:9000`
  （headless Service，零 RBAC）、`-peer-advertise=$NODE_IP:9000`、
  `-self-id=$NODE_NAME`（稳定身份，HRW keyspace 依赖）。
- 独立升级/回滚：dart 二进制随 agent 镜像发布但版本独立管理。

### 6.4 数据链路全景（native vs overlaybd）

两种消费模式的数据链路差异在于 **firecracker 的数据来源**：native 是
"agent 组装完整文件 + 每实例写层"，overlaybd 是"ublk 设备 + 只读 lower
共享 + 按需 range read"。DART 在两条链路中都是统一的 P2P 分发点。

**native 全量（阶段 2）**

```text
[拉取/预热（PinImage / warmImages）]
agent ──GET 127.0.0.1:8145/dart/<presigned-s3-url>──▶ DART
                                                     │ block 命中? ──serve
                                                     │ miss → HRW 父节点 → peer
                                                     │ peer miss → 回源 S3
agent ◀────── HTTP 流（cut-through 中继，边收边缓存）─┘
agent 组装 → <cache>/<digest>/rootfs.img   （只读共享 base，所有 Pod 共用）
每实例写层：reflink 拷贝（现状，唯一 raw 文件路径；见下）

[运行（firecracker）]
guest I/O ──▶ virtio-blk / File memory backend
          ──▶ fastlet Pod 内文件路径（hostPath 缓存目录上的实例文件）
          ──▶ 节点磁盘（写层在实例文件；base 共享只读）
```

- 节点上两份数据：DART block arena（P2P 共享）+ agent 完整文件（firecracker
  文件语义所需）；多 Pod 共享后者，每实例只有薄写层
- **写层实现约束（阶段 1-2 定案）**：Firecracker 的 virtio-blk 只支持
  **raw 文件**（drive 无格式参数，无 qemu block layer），因此 qcow2
  overlay（backing=共享 base）不能直接作为 drive attach——实例写层
  只有 reflink 拷贝一条路：
  - reflink 依赖文件系统支持（xfs/btrfs；`cp --reflink=always`），COW
    语义下拷贝是 O(metadata)；**ext4 等不支持 reflink 的文件系统静默
    回退为全量拷贝**（3GiB rootfs 实测 ~1.8s/实例，即 chain E2E
    NoInfra create 的全部耗时）——StateRoot 所在文件系统是部署要求
    （`scripts/firecracker-xfs-stateroot.sh`）；
  - 彻底消除实例拷贝（数据面换设备语义）是阶段 3 overlaybd/ublk 的
    目标，不在阶段 1-2 引入格式转换层
- 拉取收益：N 节点拉同一对象 origin 只出 ~1 份；本节点二次拉取走 DART
  本地缓存，不重复回源

**overlaybd 按需（阶段 3+）**

```text
[设备准备]
agent ──PinImage/LeaseDevices──▶ Rust daemon（vendored overlaybd + ublk）
Rust daemon ──lowest = 内容寻址 layer（只读，节点级唯一，多 lease 共享）
            ──upper = per-lease writable layer
            ──创建 ublk 设备 → bind-mount <devices>/<lease_id>/{rootfs,memory}
            ──fastlet Pod 挂载该目录

[运行]
guest I/O ──▶ virtio-blk（rootfs ublk）/ file-backed memory（memory ublk）
          ──▶ ublk 设备 ──▶ Rust daemon（OverlayBD ImageFile）
          ──▶ range read ──▶ GET 127.0.0.1:8145/dart/<presigned-s3-url>
                         ──▶ DART: block 缓存 → peer → 回源 S3
                         ──▶ 数据落 DART 一处，多 lease 共享 page cache
```

- 节点上只有一份数据：DART 块缓存（OverlayBD 自身块缓存层关闭/极小）；
  不存在"agent 完整文件"
- 共享形态：只读 lower ublk 设备节点级唯一，per-lease 只有 writable upper
  增量；firecracker 不经 agent 本地文件副本

**对比**

| 维度 | native（阶段 2） | overlaybd（阶段 3+） |
|------|------------------|----------------------|
| firecracker 数据来源 | 完整文件（agent 组装落盘） | ublk 设备（OverlayBD range read） |
| 节点数据份数 | 2 份（DART arena + 完整文件） | 1 份（DART 块缓存） |
| 共享单元 | 只读 base 文件（多 Pod 共用） | 只读 lower 设备（多 lease 共用 page cache） |
| 写层 | 实例文件（reflink 拷贝；qcow2 不适用，见上文） | per-lease upper layer |
| 拉取语义 | 全量组装（整对象 sha256 校验） | 按需 range read（block 粒度） |
| 到 DART 的入口 | agent 拉流 | Rust daemon 配 repoBlobUrl = DART |

**DART 层（两条链路共用）**

```text
GET 127.0.0.1:8145/dart/<presigned-s3-url>
  → DART block 缓存命中? → 返回
  → miss → HRW 分发树选父节点 → peer range 请求（cut-through 中继）
  → 父节点 miss → 回源 S3（origin 兜底，正确性来源）
```

- native：agent 按对象分片拉取整对象 + 整对象 sha256 校验（DART 不感知
  校验，由 agent 负责，与 manifest digest 一致）；
- overlaybd：Rust daemon 直接配 DART 地址，range read 从块边界服务；
- presigned URL 由 agent 签发（DART 原生支持 presigned upstream）。

### 6.5 源选择链（agent 侧外层）

```text
agent 决策（每次拉取前）：
  公共对象（模板 rootfs/memory/layer，URL 含 content digest）
    → 走 DART（P2P 分发）
  per-tenant 增量层（运行态快照写层/脏层）
    → 直连 S3（不经 DART，天然不进 P2P）
```

- DART 不感知租户：隔离完全由"哪些 URL 交给 DART"决定；
- S3 认证：agent 签发 presigned URL 交给 DART（DART 原生支持 presigned
  upstream），AK/SK 永不出 agent 进程；
- DART 缓存按 URL 中可恢复的 digest 去重（同一 layer 跨 registry 只缓存一次）。

### 6.6 观测与验收指标

- `dart_block_source_total{source="cache|peer|origin"}`：回源占比、命中率、
  origin 放大系数（目标：N 节点拉同一对象，origin 拉取 ≈ 1 份）；
- DART 自带 admin 平面（/metrics、集群状态）；
- 阶段 2 验收即基于这些指标：空缓存 warmImages 可从 peer 命中、冷启动风暴
  回源显著下降。

### 6.7 租户边界

- P2P 仅覆盖经 DART 的**公共对象**（模板 rootfs/memory/layer）；
- per-tenant 增量层不经 DART，统一直连 S3（快照保存时增量层已上传，恢复
  直接回源即可）；
- DART peer 平面信任集群内网（上游安全模型明确：peer/admin 未认证，属
  集群网络），与我们的内网信任域一致。

### 6.8 风险与演进

| 风险 | 影响 | 缓解 |
|------|------|------|
| DART 未生产就绪（hardening in progress，0 star/无社区） | 稳定性风险 | 固定 commit pin + vendor 二进制；390 tests race-clean 为质量基线；独立进程隔离故障；必要时 fork（Apache-2.0，纯标准库，维护成本低） |
| DART 演进漂移 | 升级成本 | commit pin + 版本独立管理；HTTP 前缀模式接口稳定，升级风险低 |
| presigned URL 过期窗口 | 长对象拉取中断 | agent 签发长 TTL presigned URL（如 1h）或按需续签 |
| 100 Gbps 吞吐目标未达 | 峰值带宽受限 | 上游 roadmap 事项；先按容量模型验收，不预设绝对值 |

## 7. 安全与凭据

| 面 | 原则 |
|----|------|
| 对象存储 | agent 用节点身份/只读 AK；guest 永不可见 AK/SK；layer 读取校验 digest/size，损坏即隔离缓存项 |
| UDS | socket 0660 + 属组隔离；fastlet 请求带 namespace+PodUID 身份头，agent 校验幂等与归属 |
| P2P | 只服务内容寻址公共层；节点身份认证（同集群信任域） |
| 发布 | builder 写凭据与 agent 读凭据分离（§1.4）；manifest 绑定 image 引用，双端校验 |
| 快照 | 不兼容 manifest 显式失败，禁止篡改 CPU/ABI 后静默恢复 |

## 8. 快照兼容性

```text
compatibilityClass = hash(
  architecture,
  CPU vendor + template + required features,
  Firecracker binary digest,
  snapshot format version,
  kernel digest,
  boot args hash,
  guest ABI
)
```

- agent 启动自检并上报本节点 class（`Compatibility` RPC）；
- restore 前 manifest 元组与节点 class 严格匹配，**不匹配拒绝调度**
  （稳定错误 `SNAPSHOT_INCOMPATIBLE`；restore 为唯一启动路径，无
  cold boot fallback）；
- 灰度升级：新旧 compatibility pool 并存，**先重建模板快照再迁移流量**
  （不依赖"旧快照冷启动过渡"）；
- 不依赖 Firecracker snapshot 的向后兼容假设。

## 9. 可观测性

用户可见启动阶段拆分：

```text
schedule → manifest fetch → device prepare → firecracker spawn/load
→ first memory block → first rootfs block → guest resume → execd ready
```

核心指标：

- create/restore/execd-ready 的 P50/P95/P99；
- remote read bytes/requests、首块与单块 range latency P95/P99、回源 vs P2P
  占比、peer 熔断次数；
- memory/rootfs cache hit ratio、淘汰数、warm pin 命中；
- ublk queue depth/latency/error；
- 前台等待、fetch dedup waiter、后台预热有效性；
- 兼容性失败、digest 失败、降级（native fallback）计数；
- 节点 agent health：KVM、ublk、磁盘、对象存储连通。

SLO 验收以"完整下载后恢复"为基线对照（不预设未 benchmark 的绝对值）：

- 空缓存 restore 下载字节显著小于 logical size（overlaybd）；
- execd-ready P95 相对 native 基线改善 ≥ 50%；
- 对象存储正常时 restore 成功率 ≥ 99.9%；
- 故障注入下：无 silent corruption、无跨租户读取、无不可回收 VM。
