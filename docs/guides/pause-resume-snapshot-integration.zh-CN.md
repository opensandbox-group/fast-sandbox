# Pause / Resume / Snapshot 字段级参考（中文）

本文是 [Pause, resume, and snapshot integration](pause-resume-snapshot-integration.md)
的配套参考，面向通过 **FastPath v2 gRPC** 或 **Kubernetes API** 集成 Fast
Sandbox 的平台方（apiserver / 控制面）。逐字段解释三类操作（暂停、恢复、
快照）的请求与响应、两个 CRD 的 spec/status，以及完整的状态机转换。

所有内容依据 `api/proto/v2/fastpath.proto`、`api/v1alpha2`（Sandbox /
SandboxSnapshot）类型定义与控制面实现整理。

- 源码契约：[`api/proto/v2/fastpath.proto`](../../api/proto/v2/fastpath.proto)、
  [`api/v1alpha2/sandbox_types.go`](../../api/v1alpha2/sandbox_types.go)、
  [`api/v1alpha2/sandboxsnapshot_types.go`](../../api/v1alpha2/sandboxsnapshot_types.go)
- 版本边界：CRD `sandbox.fast.io/v1alpha2`；gRPC 包 `fastpath.v2`

## 目录

1. [概念模型](#1-概念模型)
2. [PauseSandbox 请求/响应](#2-pausesandbox)
3. [ResumeSandbox 请求/响应](#3-resumesandbox)
4. [CreateSandboxSnapshot 请求/响应](#4-createsandboxesnapshot)
5. [观测接口：GetSandbox / GetSandboxSnapshot / DeleteSandboxSnapshot](#5-观测接口)
6. [Sandbox CRD 字段](#6-sandbox-crd)
7. [SandboxSnapshot CRD 字段](#7-sandboxesnapshot-crd)
8. [状态机](#8-状态机)
9. [gRPC 与 CRD 状态枚举映射](#9-grpc-与-crd-状态枚举映射)
10. [交互规则速查](#10-交互规则速查)

---

## 1. 概念模型

| 概念 | Pause / Resume | Snapshot |
| --- | --- | --- |
| 目的 | 释放 Fastlet 容量，同时保留"可恢复的实例" | 把运行中的 Sandbox 固化为可反复启动的镜像 |
| 期望状态的载体 | Sandbox CR 的 `spec.state` 字段（`Paused`/`Running`） | 一个独立的 SandboxSnapshot CR（一次性对象） |
| 制品去向 | artifact store 中一份**实例私有** checkpoint（不写模板索引，不能当镜像用） | artifact store 中按 `templateName` 发布的全局索引（可被任意 `CreateSandbox` 引用） |
| 恢复方式 | `ResumeSandbox`（或 `spec.state: Running`），同一 Sandbox 身份，可跨主机 | `CreateSandbox(image=<templateName>)`，与普通创建无异 |
| 消耗性 | 一次性：resume 成功即清除 checkpoint | 可重复：镜像可无限次启动 |

两个 RPC（Pause/Resume）的内部实现都是对 `spec.state` 的**比较并交换
（CAS）patch**：调用返回即表示期望状态已持久化；真正的停机/恢复由
Controller 异步收敛，通过 `GetSandbox` 观测。

---

## 2. PauseSandbox

```proto
rpc PauseSandbox(PauseSandboxRequest) returns (PauseSandboxResponse);
```

### 2.1 PauseSandboxRequest 字段

| 字段 | 类型 | 必填 | 释义 |
| --- | --- | --- | --- |
| `request_id` | string | 否 | 幂等/追踪键。设置时必须是合法的 Kubernetes DNS 子域名（`IsDNS1123Subdomain`），否则 `InvalidArgument`。Pause 的语义是"期望状态"，天然幂等：对已生效的 pause 重放同一调用，返回当前状态而不报错 |
| `sandbox` | SandboxReference | 是 | 暂停目标。缺失时 `InvalidArgument` |
| `sandbox.namespaced_name.namespace` | string | 建议 | 目标命名空间。省略时使用 server 端默认命名空间（默认 `fast-sandbox`） |
| `sandbox.namespaced_name.name` | string | 是 | 目标 Sandbox 名 |
| `sandbox.expected_uid` | string | 建议 | 身份 fence：必须匹配当前 Sandbox 的 `metadata.uid`。目的是防御"同名对象被删除重建"——你持有的 uid 已失效时返回 `Aborted`，不会误暂停新对象。创建时拿到过 uid 就应该带上 |
| `expected_generation` | int64 | 否 | 乐观锁：非 0 时必须等于当前 `metadata.generation`，否则 `Aborted`（CAS 防并发 spec 修改） |

### 2.2 服务端校验顺序与拒绝条件

| gRPC 错误码 | 触发条件 |
| --- | --- |
| `InvalidArgument` | 缺 `sandbox` 引用；`request_id` 已设置但格式非法；`expected_generation < 0` |
| `NotFound` | Sandbox 不存在 |
| `FailedPrecondition` | Sandbox 正在删除（有 deletionTimestamp）；或 runtime 处于 `Stopped`/`Stopping`/`Failed`——只有活着的 runtime 可暂停 |
| `Aborted` | `expected_uid` 或 `expected_generation` fence 不匹配 |

注意：runtime 处于 `Pending`/`Creating`/`Unavailable`/`Resuming` 等**非终态
时允许调用**——RPC 返回成功，Controller 会等 runtime Ready 后再开始
checkpoint。

### 2.3 PauseSandboxResponse 字段

| 字段 | 类型 | 释义 |
| --- | --- | --- |
| `sandbox` | SandboxInfo | patch 之后的 CRD 投影（见 [5.1](#51-sandboxinfo)）。此刻 runtime 仍在服务，`runtime.state` 通常仍是 `READY` |
| `generation` | int64 | 返回时 Sandbox 的 `metadata.generation`（本次 patch 使其 +1） |

**关键语义**：返回 ≠ 暂停完成。`Paused` 是 durable-first——只有 checkpoint
制品集在 store 中完整落库、runtime 被释放后，观测才会进入 `PAUSED`。

---

## 3. ResumeSandbox

```proto
rpc ResumeSandbox(ResumeSandboxRequest) returns (ResumeSandboxResponse);
```

### 3.1 ResumeSandboxRequest 字段

| 字段 | 类型 | 必填 | 释义 |
| --- | --- | --- | --- |
| `request_id` | string | 否 | 同 Pause（设置时校验 DNS 子域名格式）。Resume 同样是期望状态语义，重放幂等 |
| `sandbox` | SandboxReference | 是 | 恢复目标，字段同 [2.1](#21-pausesandboxrequest-字段) |
| `expected_generation` | int64 | 否 | 同 Pause 的 CAS fence |
| `expected_checkpoint_id` | string | 建议 | 必须匹配 `status.runtime.checkpoint.checkpointID`。它 fence 的是这样一个竞态：你 `GetSandbox` 读到 checkpoint 之后、发起 resume 之前，Sandbox 又被重新 pause 并产生了**新的 checkpoint**——不匹配返回 `Aborted`。标准的读-恢复流程必须带上 |

### 3.2 语义细则

| 场景 | 行为 |
| --- | --- |
| `spec.state` 已是 `Running`（从未暂停、或 resume 正在收敛、或已完成） | 幂等成功，直接返回当前状态 |
| `spec.state=Paused` 但 runtime 尚未释放（还在 `Pausing`，checkpoint 未落库） | **取消暂停**：翻回 `Running` 即可，无副作用 |
| `spec.state=Paused`、runtime=`Paused`、但没有 `checkpoint` | `FailedPrecondition`："no recorded checkpoint"——这种状态无法 resume，只能通过 `resetRevision` 起新实例 |
| `expected_checkpoint_id` 与当前 checkpoint 不一致 | `Aborted`（说明发生了 re-pause） |
| Sandbox 正在删除 | `FailedPrecondition` |

### 3.3 ResumeSandboxResponse 字段

与 [2.3](#23-pausesandboxresponse-字段) 完全相同：返回 CRD 投影 +
generation。观测 `PAUSED → RESUMING → READY` 的收敛见 [8.2](#82-runtime观测状态runtimestate)。

**Resume 成功后的集成义务**：

- checkpoint 是一次性的：成功后 `status.runtime.checkpoint` 被清除，
  `pauseAttempt` 归零，同一暂停周期无法二次恢复；
- Sandbox 可能落在另一台 Fastlet 上，`routeGeneration` 前进——**之前
  ResolveEndpoint 拿到的地址与路由凭证全部失效**，必须重新解析；
- 首次在某节点恢复需要从 store 拉取制品集（多 GiB 需要秒到分钟级），
  用户可见的 resume 超时要按此设定。

---

## 4. CreateSandboxSnapshot

```proto
rpc CreateSandboxSnapshot(CreateSandboxSnapshotRequest)
    returns (CreateSandboxSnapshotResponse);
```

### 4.1 CreateSandboxSnapshotRequest 字段

| 字段 | 类型 | 必填 | 释义 |
| --- | --- | --- | --- |
| `request_id` | string | **是** | 幂等键，同时成为 SandboxSnapshot CR 的 `metadata.name`。以相同字段重放：返回该 snapshot 的当前状态；同一 ID 携带不同意图：`AlreadyExists` 冲突（服务端对去掉 request_id 后的请求体做确定性 spec hash 比对）。省略 namespace 的请求与其显式 namespace 重放的 hash 一致 |
| `sandbox` | SandboxReference | 是 | 快照目标。`expected_uid` 会被 fastpath 从已校验的对象填入 CR spec（之后同名重建的 Sandbox 会导致快照失败）。省略 namespace 时用 server 默认 |
| `template_name` | string | **是** | 发布的制品索引键：写入 `index/<sha256(templateName)>.json`，之后 `CreateSandbox(image=template_name)` 即可从该快照启动。约束：≤255 字符，匹配 `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]…)*(/[a-zA-Z0-9._-]+)*(:[a-zA-Z0-9._-]{1,127})?$`；**跨命名空间全局唯一**、发布后长期有效（同名的再次发布会覆盖索引，旧制品集失去寻址）。请像镜像 tag 一样命名（建议带版本/时间戳后缀）。对同一 `request_id` 不可变 |
| `metadata` | map<string,string> | 否 | 投影为 SandboxSnapshot 的 label（键加 `sandbox.fast.io/` 前缀保留域之外的用户前缀，值须满足 K8s label value 约束），对齐 CreateSandbox 的 metadata 语义 |

### 4.2 并发 fence（重入规则）

| 冲突 | 行为 |
| --- | --- |
| 同一 Sandbox 已有非终态快照，且处于 `Pending`/`Creating`（停机窗口） | `FailedPrecondition`：等它到达 `Publishing` |
| 同一 Sandbox 的快照已到 `Publishing`（VM 已恢复，仅剩上传） | **不再阻塞**同一 Sandbox 的下一个快照 |
| 同一 `template_name` 被任意命名空间的非终态快照持有 | `FailedPrecondition`：模板名 fence 持有到终态（防止发布期 last-writer-wins 覆盖） |
| 目标 runtime 不是 `Ready` | `FailedPrecondition`（CR 投影滞后时会先向所辖 Fastlet 做一次实时确认再拒绝） |
| `request_id` 已存在且意图不同 | `AlreadyExists` |

### 4.3 触发期确定性失败

fastpath 持久化意图后会直连所辖 Fastlet 触发快照。确定性拒绝会立刻把
对象标为 `Failed` 并返回错误；其它（传输类/瞬态）失败保留 `Pending`，由
Controller 重试接管。

| gRPC 错误码 | Fastlet 拒绝原因 |
| --- | --- |
| `Unimplemented` | `SnapshotUnsupported`（该 Pool 的 runtime 不支持快照，如 container/gvisor） |
| `NotFound` | 目标 runtime 不存在 |
| `FailedPrecondition` | `SnapshotInProgress`（快照已在进行） |
| `Aborted` | `Conflict` / `StaleAssignment` / `StaleGeneration` / `GenerationFenced`（身份/代际 fence） |
| `Unavailable` | 其它一切瞬态错误——对象保持 `Pending` |

### 4.4 CreateSandboxSnapshotResponse 字段

| 字段 | 类型 | 释义 |
| --- | --- | --- |
| `snapshot` | SandboxSnapshotInfo | 意图持久化（或重放命中）后的快照观测，见 [5.3](#53-sandboxesnapshotinfo) |

---

## 5. 观测接口

### 5.1 SandboxInfo

`GetSandbox` / Pause / Resume 的返回体。字段与 CRD 投影的对应关系：

| 字段 | 类型 | 释义 |
| --- | --- | --- |
| `identity` | SandboxIdentity | `{uid, name, namespace}`，即 CR 的 `metadata.uid/name/namespace` |
| `applied_generation` | int64 | `status.observedGeneration`：Controller 已处理的 spec 代数（注意不是"已收敛"） |
| `runtime` | RuntimeInfo | `runtime.state`：具体 runtime 的观测状态（见 [第 9 节](#9-grpc-与-crd-状态枚举映射)） |
| `data_plane` | DataPlaneInfo | `data_plane.state`：路由面状态。`Pausing` 时置 `UNAVAILABLE`，`Paused` 期间保持 `UNAVAILABLE`，resume 后经 `PENDING` 回到 `READY` |
| `infra_components` | repeated InfraComponentInfo | 每个命名组件的 `{name, state(STARTING/READY/FAILED), message}` |
| `action_bindings` | repeated ActionBindingInfo | 每个 Binding 的 `{handler, state(PENDING/APPLYING/READY/FAILED), last_transition_unix_seconds, message}` |
| `ready` | bool | 聚合 `Ready` Condition（runtime + dataplane + 全部组件 + 全部 Binding） |
| `state` | SandboxState | **期望状态**投影：`SANDBOX_STATE_RUNNING` / `SANDBOX_STATE_PAUSED`（对应 `spec.state`，字段缺省按 Running 处理） |
| `checkpoint` | CheckpointInfo | 仅当 runtime 状态 ∈ {`PAUSING`, `PAUSED`, `RESUMING`} 时出现且有效（`CheckpointActive`）；其余状态一律视为"无 checkpoint" |

**读取语义**：`Paused` 的 Sandbox 没有 placement，`GetSandbox` 不会失败，
而是直接返回 CRD 投影（这也是暂停期唯一可用的观测面）。

### 5.2 CheckpointInfo

| 字段 | 类型 | 释义 |
| --- | --- | --- |
| `checkpoint_id` | string | 产生该 checkpoint 的 Fastlet 任务 ID，由 `sha256(Sandbox UID : spec generation : pauseAttempt)` 确定性派生（重试未知结局时重放同一任务，新尝试永不复用旧 ID） |
| `manifest_ref` | string | checkpoint manifest 的 `s3://` URI，标准制品集布局（rootfs/vmstate/memory/SHA256SUMS） |
| `artifact_digest` | string | manifest 文档本身的 sha256，可独立于 `manifest_ref` 寻址制品集 |
| `size_bytes` | int64 | 制品集逻辑总大小 |
| `paused_unix_seconds` | int64 | checkpoint 在 store 中落库的时间（`status.runtime.checkpoint.pausedAt`） |
| `fastlet_name` | string | 拍摄 checkpoint 的 Fastlet。**仅诊断用途**：该 Fastlet 可能已经不存在，恢复并不承诺回到它 |

### 5.3 SandboxSnapshotInfo

`CreateSandboxSnapshot` / `GetSandboxSnapshot` 的返回体：

| 字段 | 类型 | 释义 |
| --- | --- | --- |
| `identity` | SandboxIdentity | SandboxSnapshot 对象自身的 `{uid, name, namespace}`；name == request_id |
| `sandbox_name` / `sandbox_uid` | string | 被快照 Sandbox 的名字与其当时的 UID |
| `template_name` | string | 发布索引键（== spec，不可变） |
| `phase` | SnapshotPhase | `PENDING → CREATING → PUBLISHING → SUCCEEDED/FAILED`，见 [8.4](#84-snapshot-阶段机snapshotphase) |
| `message` | string | 最新进度或失败原因 |
| `fastlet_name` | string | 触发时按 Sandbox 当时 assignment 解析出的 Fastlet（投影，placement 注解才是权威） |
| `manifest_ref` | string | 发布 manifest 的 `s3://` URI（仅 `SUCCEEDED`） |
| `artifact_digest` | string | 发布 manifest 文档的 sha256 |
| `size_bytes` | int64 | 发布制品集逻辑总大小 |
| `created_unix_seconds` / `completed_unix_seconds` | int64 | 对象创建时间 / 到达终态时间 |

### 5.4 GetSandboxSnapshotRequest / DeleteSandboxSnapshotRequest

| 字段 | 释义 |
| --- | --- |
| `snapshot` (Get) | `{namespace, name}` 的 NamespacedName；namespace 省略用 server 默认 |
| `expected_uid` (Get) | 设置时必须匹配 SandboxSnapshot 的 UID，否则 `Aborted` |
| `snapshot` (Delete) | SandboxReference 形式（`namespaced_name` + `expected_uid` fence，语义同上） |

Delete 是幂等的：对象不存在直接成功。**删除 CR 不会取消发布已存储的
制品**——store 生命周期独立管理。

---

## 6. Sandbox CRD

`apiVersion: sandbox.fast.io/v1alpha2, kind: Sandbox`。

### 6.1 spec

| 字段 | 必填 | 释义 |
| --- | --- | --- |
| `image` | 是 | 业务 OCI 镜像（≥1 字符） |
| `command` / `args` | 否 | 覆盖镜像入口进程的 argv |
| `envs` | 否 | Kubernetes `EnvVar` 数组 |
| `workingDir` | 否 | 业务进程工作目录 |
| `expireTime` | 否 | 绝对过期时间，到期垃圾回收。**暂停期间仍然生效**——暂停不续命；未设置则永不过期 |
| `failurePolicy` | 否 | Fastlet 失联时的策略：`Manual`（默认，只上报不动作）/ `AutoRecreate`（超时后自动重排新实例；不保留内存/本地盘/网络身份） |
| `recoveryTimeoutSeconds` | 否 | 失联后等待多久执行 failurePolicy，默认 60 |
| `resetRevision` | 否 | 不透明单调触发器（通常为时间戳）：`spec.resetRevision > status.runtime.acceptedResetRevision` 时触发 reset——排空旧路由、删除旧 runtime、代际前进、按当前 spec 起新实例。**reset 会丢弃 checkpoint 谱系**（Paused 的 Sandbox 被 reset 后不能 resume，只能得到全新实例） |
| `state` | 否 | **期望生命周期**：`Running`（默认）/ `Paused`。Pause/Resume 的全部入口。注意：带 checkpoint 的 Sandbox 翻回 `Running` 时走"从 checkpoint 恢复"，而不是冷启动 |
| `poolRef` | 是 | 同命名空间的 SandboxPool 名 |
| `actionBindings` | 否 | 有序 `{handler, input}` 列表（≤16，handler 唯一，`listType=atomic` 整体替换）。顺序即 Handler 生命周期调用顺序；`input` 是 Handler 自有的不透明数据（≤64KiB，平台逐字节透传不解析）。列表存在快照 manifest 里，恢复/重启镜像时按"显式传入优先、目标 Pool 未声明的 handler 丢弃"规则合并 |

用户自定义 metadata 存放在普通 Kubernetes label 上；`sandbox.fast.io/`
前缀为平台保留。

### 6.2 status

| 字段 | 释义 |
| --- | --- |
| `observedGeneration` | 该 status 快照所代表的 spec generation。判断"已收敛"必须用聚合 `Ready=True` 且其 `observedGeneration == metadata.generation` |
| `placement` | `{attempt, fastletName, fastletPodUID, recovery{detectedAt, deadline}}`：调度尝试代数、当前所在 Fastlet、恢复倒计时。**Paused 状态下此组字段被清空（无 placement）** |
| `runtime` | RuntimeStatus，见下表 |
| `dataPlane` | `{state, routeGeneration, lastTransitionTime, message}`：路由面状态与路由代数（resume 后前进，旧凭证因此失效） |
| `infraComponents[]` | `{name, state(Starting/Ready/Failed), lastTransitionTime, message}` |
| `actionBindings[]` | `{handler, state(Pending/Applying/Ready/Failed), lastTransitionTime, message}` |
| `conditions[]` | `Ready`（聚合就绪）与 `Suspended`（仅"持久暂停完成"为 True），见 [8.3](#83-suspended-condition) |

### 6.3 status.runtime（RuntimeStatus）

| 字段 | 释义 |
| --- | --- |
| `state` | 具体运行时的观测状态，枚举见 [8.2](#82-runtime观测状态runtimestate) |
| `generation` | 具体 runtime 的实例代数（reset/AutoRecreate 时前进） |
| `lastTransitionTime` / `message` | 状态迁移时间与最新说明 |
| `acceptedResetRevision` | 已受理的 resetRevision |
| `pauseAttempt` | pause FSM 的重试纪元。Controller 每次因瞬态失败发起新 checkpoint 尝试前 +1，并以此派生 Fastlet 任务 ID；成功 resume 后归零。可用于区分"同一轮重试"与"新一轮尝试" |
| `checkpoint` | CheckpointStatus，见下表。**仅在 `state ∈ {Pausing, Paused, Resuming}` 时是权威的**（`CheckpointActive`）；其它状态一律按"无 checkpoint"读，Controller 会将其清除 |

### 6.4 status.runtime.checkpoint（CheckpointStatus）

| 字段 | 释义 |
| --- | --- |
| `checkpointID` | Fastlet 侧任务身份，派生自 Sandbox UID + 发起暂停的 spec generation + pause 纪元（== gRPC `CheckpointInfo.checkpoint_id`） |
| `manifestRef` | checkpoint manifest 的 `s3://` URI |
| `artifactDigest` | manifest 文档 sha256 |
| `sizeBytes` | 制品集逻辑总大小 |
| `pausedAt` | checkpoint 落库时间（== gRPC `paused_unix_seconds`） |
| `fastletName` / `fastletPodUID` | 拍摄时的 Fastlet（仅诊断；恢复目标由调度决定，可能是任意节点） |

与 SandboxSnapshot 的区别：checkpoint 是**实例私有**的——不写模板索引，
不能被其它 Sandbox 当镜像引用。

---

## 7. SandboxSnapshot CRD

`apiVersion: sandbox.fast.io/v1alpha2, kind: SandboxSnapshot`。一次性对象：
spec 不可变（CEL `self == oldSelf` 强制）；重新快照 = 新建对象。

### 7.1 spec

| 字段 | 必填 | 释义 |
| --- | --- | --- |
| `sandboxRef.name` / `sandboxRef.namespace` | 是 | 目标 Sandbox；触发时必须 `Ready` 且已分配 |
| `sandboxRef.uid` | 否 | 身份 fence。空表示接受当前对象；fastpath 会用已校验对象的 UID 回填。此后同名重建会使该快照失败 |
| `templateName` | 是 | 发布索引键（模式/长度约束同 [4.1](#41-createsandboxesnapshotrequest-字段)）。全局跨命名空间；只写这一个索引键（不像 SandboxTemplate 构建会补写 `sha256(image)` 默认键） |

### 7.2 status

| 字段 | 释义 |
| --- | --- |
| `observedGeneration` | 已观测的 spec generation（spec 不可变，通常恒等） |
| `phase` | `Pending → Creating → Publishing → Succeeded/Failed`，见 [8.4](#84-snapshot-阶段机snapshotphase) |
| `message` | 最新进度或失败原因 |
| `sandboxUID` | 被快照 Sandbox 那一代的 UID |
| `fastletName` / `fastletPodUID` | 触发时解析的 Fastlet（assignment 注解是权威，此组是投影） |
| `triggered` | SnapshotTrigger：触发时刻钉死的完整 placement + 身份 fence（`fastletName`、`fastletPodUid`、`runtimeInstanceId`、`instanceGeneration`、`assignmentAttempt`）。后续观测（以及尽力清理）都解析到它，而不是 Sandbox 的**当前** assignment——中途 Sandbox 被重排也不会把观测悄悄指到别的 Fastlet。残余窗口：恰好在最后一次索引上传时 Fastlet 崩溃，对象可能终态 `Failed` 而索引已落地（模板名 last-writer-wins）；其它一切失败路径都不发布任何东西 |
| `snapshotID` | Fastlet 侧快照任务身份 |
| `manifestRef` | 发布 manifest 的 `s3://` URI（仅 Succeeded） |
| `artifactDigest` / `sizeBytes` | manifest sha256 / 制品集逻辑总大小 |
| `startedAt` | Fastlet 接受快照的时间 |
| `completedAt` | 到达终态的时间 |
| `conditions[]` | `Completed`：True=Succeeded；False 时 `reason` 说明失败原因 |

---

## 8. 状态机

### 8.1 期望状态（spec.state）

```text
            PauseSandbox / patch spec.state=Paused
  Running ────────────────────────────────────────► Paused
      ▲                                               │
      │           ResumeSandbox / patch spec.state=Running
      └────────────────────────────────────────────────┘
```

- 这是对**期望**的声明，不是对**结果**的确认；实际转换由 Controller 异步
  完成（见 8.2）。
- `Paused → Running` 且存在有效 checkpoint 时，恢复走 checkpoint；
  checkpoint 不存在或已被消耗时，`Running` 等价于按 spec 冷启动。

### 8.2 runtime 观测状态（RuntimeState）

| 状态 | 含义 |
| --- | --- |
| `Unknown` | 尚无观测 |
| `Pending` / `Creating` | runtime 排队 / 创建中 |
| `Ready` | 运行中，可服务。**唯一可发起快照/进入暂停窗口的状态** |
| `Pausing` | checkpoint 窗口：dump + 上传进行中。runtime 仍可服务（durable-first），但**不可 resume** |
| `Paused` | checkpoint 已在 store 落库、runtime 与 assignment 已释放、不占 Fastlet 容量。`GetSandbox` 走 CRD 投影 |
| `Resuming` | 正在（可能跨主机）从 checkpoint 物化新 runtime。`dataPlane=PENDING`，旧路由已失效 |
| `Stopping` / `Stopped` | 删除/reset 过程中 / 已停止 |
| `Failed` | 终态失败（Fastlet 丢失且 Manual 等） |
| `Unavailable` | 暂时不可观测/不可用（如 Fastlet 瞬断） |

Pause/Resume 主链路转换：

```text
                 PauseSandbox(spec.state=Paused)
  Ready ────────────────────────────────────────► Pausing
    ▲                                              │
    │  dump 失败/Fastlet 丢任务：runtime 继续服务，   │ checkpoint 落库且 runtime 释放
    │  pauseAttempt+1，约 5s 后自动重试               ▼
    └────────────────────── Paused ──── ResumeSandbox ───► Resuming ──► Ready
                            (Suspended=True)                (checkpoint 清除, pauseAttempt=0)
```

要点：

- **暂停失败不伤运行时**：所有失败路径上 dump 都会先恢复 VM，业务无感；
  Controller 用新的 `pauseAttempt` 纪元自动重试。确定性拒绝（如 runtime
  不支持快照）通过 `Ready` Condition 的 reason 上报（如
  `SnapshotUnsupported`），runtime 保持 `Ready` 继续服务。
- **Pausing → Paused 不可跳过**：`Paused` 之前的 Sandbox 既不可用也不可
  恢复（`Pausing` 期间 resume = 取消暂停）。
- **checkpoint 一次性**：`Resuming → Ready` 成功即清除
  `status.runtime.checkpoint`（制品本身按 store 生命周期另行管理）。
- 暂停期的其它规则：`expireTime` 仍生效；`CreateSandboxSnapshot` 被拒
  （要求 Ready runtime）；reset / 过期会丢弃 checkpoint 谱系；删除对象
  连同 checkpoint 一起删除。

### 8.3 Suspended Condition

| Status | Reason（示例） | 含义 |
| --- | --- | --- |
| `False` | `Resuming` | 恢复等待/进行中 |
| `False` | 各 Fastlet 任务阶段码（dump 窗口进度） | Pausing 进行中 |
| `False` | `CheckpointPublished` | checkpoint 已落库、等待 runtime 释放 |
| `False` | `PauseFailed` / `FastletUnavailable` / `PauseWaitingRuntime` / 确定性拒绝码 | 暂停被阻塞或被拒，runtime 保持 `Ready` 服务 |
| **`True`** | `CheckpointPublished` | **且仅当** `runtime.state == Paused`：持久暂停完成，不占容量 |
| `False` | `Resumed` | 已从 checkpoint 恢复 |

集成方应以 `Suspended == True` 作为"容量已释放、可以记账"的准确信号，
而不是 RPC 返回或 `PAUSING`。

### 8.4 Snapshot 阶段机（SnapshotPhase）

```text
 Pending ──► Creating ──► Publishing ──► Succeeded
    │            │              │
    └────────────┴──────────────┴──► Failed（终态；用新 request_id 重试）
```

| 阶段 | 含义 | 用户可见影响 |
| --- | --- | --- |
| `Pending` | 意图已持久化，尚未被 Fastlet 接纳（调度追赶/重试窗口） | 无 |
| `Creating` | 停机窗口：VM 暂停 → rootfs reflink → vmstate/内存 dump → VM 恢复 | 唯一的业务可见中断（配置 spill 区时为亚秒级） |
| `Publishing` | dump 完成、**VM 已恢复**，制品集上传中 | 无（同一 Sandbox 的下一个快照已可发起） |
| `Succeeded` | 终态。`manifestRef`/`artifactDigest`/`sizeBytes` 描述已发布制品集 | `image=templateName` 可创建 |
| `Failed` | 终态。`Completed.reason` 说明原因；本次产生的任何东西都不可寻址 | 换新 `request_id` 重试 |

Fence 与阶段的对应：

- **Sandbox fence**：只覆盖 `Pending`/`Creating`；
- **模板名 fence**：持有到终态（任意命名空间）；
- **终态单调**：`Succeeded`/`Failed` 之后状态不会再回退（含 Controller
  与 fastpath 并发写 status 的场景）；对终态对象重放 create 不会重新触发。

控制器兜底：10 分钟未被接纳、45 分钟不可观测的僵死任务会被判
`Failed` 并释放 fence。

---

## 9. gRPC 与 CRD 状态枚举映射

`SandboxInfo.runtime.state`（proto）与 `status.runtime.state`（CRD）一一
对应：

| CRD | proto | 说明 |
| --- | --- | --- |
| `Unknown` | `RUNTIME_STATE_UNKNOWN` | 无观测 |
| `Pending` | `RUNTIME_STATE_PENDING` | |
| `Creating` | `RUNTIME_STATE_CREATING` | |
| `Ready` | `RUNTIME_STATE_READY` | 可暂停/可快照 |
| `Pausing` | `RUNTIME_STATE_PAUSING` | checkpoint 窗口，不可 resume |
| `Paused` | `RUNTIME_STATE_PAUSED` | checkpoint 落库、容量已释放 |
| `Resuming` | `RUNTIME_STATE_RESUMING` | checkpoint 物化中 |
| `Stopping` / `Stopped` | `RUNTIME_STATE_STOPPING` / `RUNTIME_STATE_STOPPED` | |
| `Failed` | `RUNTIME_STATE_FAILED` | |
| `Unavailable` | `RUNTIME_STATE_UNAVAILABLE` | |

`SandboxInfo.state`（期望状态）：`SANDBOX_STATE_RUNNING`/`SANDBOX_STATE_PAUSED`
↔ `spec.state: Running`/`Paused`。快照阶段 `SnapshotPhase` ↔ `status.phase`
同名单射（`PENDING`/`CREATING`/`PUBLISHING`/`SUCCEEDED`/`FAILED`）。

---

## 10. 交互规则速查

| 规则 | 说明 |
| --- | --- |
| RPC 返回 = 意图持久化 | Pause/Resume/快照都不等收敛；完成一律经 `GetSandbox` / `GetSandboxSnapshot` 观测 |
| fence 全带上 | `expected_uid` 每次都带；`expected_generation`、`expected_checkpoint_id` 能带就带——它们把"同名重建 / 并发改写 / 读后 re-pause"竞态变成显式 `Aborted` |
| Paused 无 placement | `GetSandbox` 走 CRD 投影不会失败；`placement` 字段已清空 |
| `expireTime` 暂停不豁免 | 到期照样回收（连带 checkpoint） |
| checkpoint 一次性 | resume 成功即清除；二次恢复同一暂停周期不可能 |
| resume 后必须重解析路由 | `routeGeneration` 前进，旧 endpoint/凭证全部失效 |
| 快照与暂停互斥 | 快照要求 Ready runtime；`PAUSING`/`PAUSED` 下发起快照被 `FailedPrecondition` 拒绝 |
| 同 Sandbox 连续快照 | 前一个到 `Publishing` 即可发起下一个，无需等上传完成 |
| 模板名全局 | 跨命名空间唯一且发布后长期有效；发布后同名再发布覆盖索引（旧制品集失去寻址） |
| 删除快照 ≠ 删除制品 | CR 与本地暂存删除，store 生命周期独立管理 |
| runtime 能力门控 | pause/resume/snapshot 由 Firecracker runtime 实现；其它 runtime 上快照触发返回 `UNIMPLEMENTED`，暂停以 `SnapshotUnsupported` Condition 上报 |
