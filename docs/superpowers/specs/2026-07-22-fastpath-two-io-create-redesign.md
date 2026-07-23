# FastPath 两次 IO 创建路径重设计

**状态**：已决策，进入实现

**日期**：2026-07-22

**范围**：仅重设计 `CreateSandbox` FastPath；声明式 Controller 路径不要求满足两次 IO

## 1. 结论

当前 `CreateSandbox` 名义上是 FastPath，实际 happy path 却包含约 13 次下游网络往返：

- Kubernetes API Server 约 10 次；
- Fastlet HTTP API 3 次；
- 还没有计算 Fastlet 内部访问 containerd、network、Infra 和 Fastlet Proxy 的 IO。

这种设计把幂等查询、Pool 解析、reservation、assignment CAS、重复 Pool 查询、状态写回和最终刷新全部串在用户请求延迟上，不符合 FastPath 的目标。

新的硬性目标是：

> 在无冲突、首个候选成功的 happy path 中，FastPath 只执行两次下游网络 IO。

最终选择 **CRD-first**：先创建包含完整 spec 和初始 assignment annotation 的 Sandbox CRD，再调用目标 Fastlet 的 `CreateSandbox`。

选择它的原因是：API Server 在产生节点资源前完成 `namespace/name` 唯一性裁决；Fastlet 第一次调用即可获得真实 CRD UID；FastPath 在两个 IO 之间崩溃时，Controller 可以从已经持久化的 assignment 接管；系统不需要 provisional runtime、确认 TTL 或“runtime 已创建但 CRD 不存在”的补偿协议。

Fastlet-first 作为被拒绝方案保留在本文的决策记录中，不再作为实现选项。

## 2. 对当前实现的批判

当前正常创建路径大致为：

```text
List Sandbox by request_id
  -> Get SandboxPool
  -> Fastlet ReserveSandbox
  -> Create Sandbox CRD
  -> Get Sandbox
  -> Patch status.assignment
  -> Get Sandbox
  -> Get SandboxPool again
  -> Fastlet EnsureSandbox
  -> Get Sandbox
  -> Update Sandbox status Ready
  -> Get Sandbox again
  -> Fastlet CancelReservation
```

主要问题不是某一个调用多余，而是整个流程错误地把声明式收敛工作塞进了同步 FastPath。

### 2.1 request_id 的实现过度设计

当前实现同时维护：

- `request_id`；
- 用户可选 `name`；
- request ID hash label；
- 根据 hash label 执行 Sandbox List；
- 没有 name 时再由 request ID hash 生成 name。

这些字段描述的是同一个用户创建身份，却形成了多套映射和一次额外 API Server 查询。

新设计规定：

```text
request_id == Sandbox metadata.name
```

不再存在独立的用户 `name` 和 request ID hash lookup。

- SDK/fastctl 在用户未指定身份时生成合法的 Kubernetes name，例如 `sb-<ulid>`；
- 用户显式指定名称时，该名称就是 request ID；
- request ID 必须满足 Kubernetes DNS name 约束；
- 幂等对象通过 `namespace/name` 直接寻址；
- 删除 `sandbox.fast.io/request-id-hash` 以及基于它的 List 查询。

### 2.2 Reservation 是额外协议，不是 FastPath 必需能力

现有 reservation 引入了：

- `ReserveSandbox`；
- reservation token；
- reservation TTL；
- `CancelReservation`；
- reservation 转正式 admission；
- FastPath/Controller 抢占 reservation 的复杂语义。

但节点容量的最终正确性本来就只能由 Fastlet 原子保证。既然 Fastlet 必须在真正创建时再次校验容量，reservation 只是为一次创建增加了额外 IO 和状态机。

新设计删除：

- `ReserveForCreate`；
- `ReserveSandbox` Fastlet API；
- `CancelReservation` Fastlet API；
- reservation token 和 reservation map；
- reservation 指标与测试。

Fastlet `CreateSandbox` 本身就是唯一 admission 原子点。

### 2.3 assignment 不应该在用户请求中执行多次 status CAS

当前 FastPath 为了写 assignment，执行 `Get -> Status Patch -> Get`；创建完成后又执行 `Get -> Status Update -> Get`。

这属于 Controller 的声明式职责，不应该进入同步 FastPath。

新设计将 Fastlet 分配结果与 Sandbox spec 一起写入首次 CRD Create：

```text
metadata.annotations["sandbox.fast.io/assignment"] = <versioned JSON>
```

Controller Watch 到对象后，再把 assignment 补齐到 `status.assignment`。

## 3. CRD-first 两次 IO Happy Path

### 3.1 前置条件

FastPath 的本地 Watch/Heartbeat Registry 必须已经包含完成候选选择所需的数据：

- namespace；
- Pool name；
- Fastlet Pod name/UID/IP；
- node name；
- runtime/profile hash；
- resource profile hash；
- InfraProfile hash；
- admission capacity；
- cache/image affinity；
- heartbeat freshness 和 draining 状态。

FastPath 请求处理中禁止为解析 Pool 再调用 API Server。

Pool 是同构单元，Fastlet 已经在启动环境中持有固定 runtime、资源规格、InfraProfile 和容量。FastPath 不需要重新读取并把这些配置逐项下发。

### 3.2 被拒绝方案：Fastlet-first

本节仅保留为决策记录，不进入实现。该方案需要 provisional runtime、确认 TTL 和无 CRD runtime 补偿，已被 CRD-first 取代。

```text
Client
  -> FastPath: CreateSandbox(request_id=name, image, pool, command...)

FastPath
  -> local Registry Top-K                         # 内存操作，不是 IO
  -> generate runtimeInstanceID                  # 本地操作，不是 IO

IO 1:
FastPath
  -> Fastlet: CreateSandbox(namespace, name, runtimeInstanceID, workload spec)
Fastlet
  -> atomic admission
  -> create runtime/network/infra/local route
  -> return Created

FastPath
  -> build Sandbox CRD
     spec = complete user desired state
     annotation = selected Fastlet assignment + runtimeInstanceID

IO 2:
FastPath
  -> Kubernetes API Server: Create Sandbox CRD
API Server
  -> return object including metadata.uid/resourceVersion

FastPath
  -> return CreateResponse to Client

Controller, asynchronously:
  -> observe CRD
  -> validate annotation assignment
  -> project assignment into status
  -> confirm/reconcile the Fastlet runtime
  -> update runtime/data-plane Conditions
```

### 3.3 被拒绝方案的 IO 预算

| 序号 | 调用 | 目的 |
|---|---|---|
| 1 | FastPath -> Fastlet `CreateSandbox` | 原子 admission，并同步创建 runtime |
| 2 | FastPath -> API Server `Create Sandbox` | 持久化完整 spec 和 assignment annotation |

以下操作不属于网络 IO：

- request validation；
- request ID/name 归一化；
- Registry Top-K；
- create spec 比较；
- runtimeInstanceID 生成；
- CRD 对象组装。

以下工作不进入 FastPath 用户请求延迟：

- annotation 到 status 的投影；
- Conditions 补齐；
- Controller 二次确认；
- 失败后的补偿删除；
- Janitor 兜底清理。

### 3.4 最终方案：CRD-first

另一种顺序是在本地 Top-K 得到候选后，立即创建 CRD：

```text
Client
  -> FastPath: CreateSandbox(request_id=name, image, pool, command...)

FastPath
  -> local Registry Top-K                         # 内存操作，不是 IO
  -> generate runtimeInstanceID                  # 本地操作，不是 IO

IO 1:
FastPath
  -> Kubernetes API Server: Create Sandbox CRD
     spec = complete user desired state
     annotation = candidate Fastlet assignment + runtimeInstanceID
API Server
  -> return object including metadata.uid/resourceVersion

IO 2:
FastPath
  -> Fastlet: CreateSandbox(CRD UID, namespace, name,
                            runtimeInstanceID, workload spec)
Fastlet
  -> atomic admission
  -> create runtime/network/infra/local route
  -> return Created

FastPath
  -> return CreateResponse to Client

Controller, asynchronously:
  -> observe CRD
  -> project assignment annotation into status
  -> reconcile runtime and Conditions
```

方案 B 的首个候选成功时仍然只有两次 IO：一次 API Server Create 和一次 Fastlet Create。其直接收益是：

- API Server 在产生任何节点资源前先完成 `namespace/name` 唯一性裁决；
- Fastlet 首次创建即可使用真实 CRD UID；
- FastPath 在两次 IO 之间崩溃时，Controller 能从已经存在的 CRD 接管；
- 不需要为“runtime 已创建但 CRD 不存在”设计 provisional TTL；
- 同名幂等重试不会先在多个 Fastlet 上制造 runtime。

### 3.5 CRD-first 的冲突与改派

如果首个 Fastlet 明确返回容量冲突、draining 或其他可改派拒绝，方案 B 会离开两次 IO happy path：

```text
CRD annotation points to Fastlet A                 # IO 1 已完成
  -> Fastlet A CreateSandbox                       # IO 2，明确拒绝
  -> local Top-K chooses Fastlet B
  -> Patch assignment annotation: A -> B           # IO 3
  -> Fastlet B CreateSandbox                       # IO 4
  -> success and return
```

每次重选都需要用旧 assignment 作为 expected value，直接 CAS 更新到新 assignment，再调用新的 Fastlet。Patch 至少要用 `resourceVersion`/JSON Patch test 保护，并同时递增 `attempt`、`routeGeneration`，生成新的 `runtimeInstanceID`，防止旧请求、旧补偿任务或 Controller 覆盖新的分配结果。禁止先删除 assignment、再等待下一轮补写；没有替代候选时保留原 assignment 并维持 Pending/Creating，新候选随扩容或 heartbeat 出现后再执行一次原子 CAS。

CRD-first 必须定义 FastPath 与 Controller 的并发规则。这里不引入 FastPath lease、owner 字段或 takeover timeout；FastPath 和 Controller 可以同时对 annotation 指向的同一个 Fastlet、同一个 runtime identity 调用幂等 `CreateSandbox`。Fastlet 是唯一 admission 原子点。

assignment annotation 中只记录持久化身份和 fence：

```json
{
  "version": "v1",
  "attempt": 1,
  "fastletPodUID": "...",
  "runtimeInstanceID": "..."
}
```

规则如下：

1. Controller 看到 annotation 后，首先对相同目标做幂等创建/确认，不重新 Top-K；
2. FastPath 成功后不增加第三次同步 IO，Controller 异步把 annotation 投影到 status 并收敛 Conditions；
3. 只有 Fastlet 明确返回“未产生副作用”的可改派拒绝，才能 CAS 修改 annotation；
4. 网络超时、连接中断或结果不确定时，只能重试相同 Fastlet 和相同 runtime identity；
5. Fastlet Pod UID 已消失时，Controller 可以递增 attempt、生成新的 runtimeInstanceID 并 CAS 改派；
6. 多个执行者同时改派时，只有一个 annotation CAS 能成功，失败者重新读取后跟随 winner；
7. annotation 的目标变化是分配变更，任何组件都不能把它当作普通提示字段无条件覆盖。

### 3.6 决策记录：两种方案对比

| 维度 | 方案 A：Fastlet-first | 方案 B：CRD-first |
|---|---|---|
| 无冲突 happy path | 2 IO | 2 IO |
| IO 顺序 | Fastlet Create -> CRD Create | CRD Create -> Fastlet Create |
| Fastlet 首次创建身份 | name + runtimeInstanceID | CRD UID + name + runtimeInstanceID |
| 全局同名裁决时机 | runtime 创建之后 | runtime 创建之前 |
| 首个候选拒绝 | 可直接尝试下一 Fastlet；成功后一次创建 CRD | 每次改派都要先 Patch annotation，再尝试下一 Fastlet |
| FastPath 中途崩溃 | 可能遗留无 CRD runtime，需要 provisional TTL | CRD 已存在，Controller 可以接管 |
| API Create 失败/超时 | 需要检查 CRD 后 fenced delete runtime | 不会先产生 runtime；仅处理 API 结果歧义 |
| 多 FastPath 同名竞争 | 可能先创建多个 runtime，再由 API Server 决胜 | API Server 先决胜，失败者不接触 Fastlet |
| Controller 并发接管 | CRD 出现时 runtime 已创建，较简单 | 对同一 identity 幂等 Create；无需 lease |
| 复杂度主要位置 | 节点 provisional 与异步补偿 | assignment CAS、重选和所有权交接 |

最终选择方案 B。我们接受候选冲突路径需要额外 CAS Patch 的代价，优先保证全局幂等、真实 CRD UID、FastPath 崩溃接管以及不产生无 CRD 节点孤儿。

### 3.7 失败与恢复矩阵

一旦 IO 1 成功，CRD 就是持久化事实；FastPath 不通过同步删除 CRD 来伪装“快速失败”。只有 CRD 创建前可以确定的失败才保证不产生对象。

| 失败位置 | CRD | Runtime | 必须执行的行为 | 允许改派 |
|---|---|---|---|---|
| 请求、Registry、Profile 或无候选等前置校验失败 | 不存在 | 不存在 | 直接返回错误 | 不适用 |
| API Server 明确拒绝 Create | 不存在 | 不存在 | 返回 API 错误 | 不适用 |
| API Server Create 结果未知 | 未知 | 不存在 | 按 `namespace/name` Get；确认对象及 intent 后再继续 | 否 |
| AlreadyExists 且 intent 相同 | 已存在 | 未知 | 恢复 annotation 指向的同一 identity | 否 |
| AlreadyExists 且 intent 不同 | 已存在 | 不触碰 | 返回冲突 | 否 |
| Fastlet 明确返回无副作用的容量/draining 拒绝 | 已存在 | 不存在 | CAS annotation 到下一候选，attempt 递增并生成新 runtimeInstanceID | 是 |
| Fastlet Profile/Fastlet Pod UID 不匹配 | 已存在 | 不存在 | 标记 Registry stale；CAS 到有效候选 | 是 |
| Fastlet 创建中、网络超时或响应丢失 | 已存在 | 未知 | 对原 Fastlet、原 identity 幂等重试；FastPath 不用一次额外 Inspect 把结果未知误判成可改派 | 否 |
| Fastlet 部分创建后失败 | 已存在 | 可能存在 | 原节点完成幂等恢复或 fenced cleanup | 清理确认前否 |
| Fastlet Pod UID 已消失 | 已存在 | 随 Pod 失效 | Controller 递增 attempt/generation fence 后重新分配 | 是 |
| 所有候选均明确拒绝 | 已存在 | 不存在 | 保留 Pending/Failed，由 Controller 根据新 heartbeat 重试 | 暂时否 |
| FastPath 在 IO 1 后崩溃 | 已存在 | 不存在或未知 | Controller 对 annotation 指向的同一 identity 调用 Create | 否 |
| FastPath 在 IO 2 成功后丢失响应 | 已存在 | 已存在 | 客户端或 Controller 幂等重试并返回同一结果 | 否 |
| 创建期间发生 Delete | deletionTimestamp 已设置 | 任意 | Delete fence 优先；Fastlet 为该 generation 保留 tombstone，拒绝晚到 Create | 否 |
| 创建期间发生 Reset | 已存在 | 旧 generation 任意 | 新 generation 生效；Fastlet 拒绝所有旧 generation 请求 | 仅新 generation |

Fastlet 错误不仅要给出 code，还要明确 outcome：

```text
RejectedBeforeSideEffects  -> 可以 CAS 改派
InProgress                 -> 同目标重试
Created                    -> 幂等成功
FailedNeedsCleanup         -> 同目标恢复/清理
Unknown                    -> 同目标确认，禁止改派
GenerationFenced           -> 永久拒绝旧请求
```

## 4. CRD-first 身份模型

### 4.1 被拒绝方案的身份限制

方案 A 中，Fastlet 创建发生在 CRD Create 之前，因此 IO 1 时 Kubernetes `metadata.uid` 尚不存在。节点侧首次创建协议不能要求 Sandbox CRD UID，需要使用：

```text
SandboxKey       = namespace/name
runtimeInstanceID = FastPath 本地生成的随机、不可复用 fence
```

`runtimeInstanceID` 不是用于查找对象的第二个 name，也不是 request ID hash；它只用于防止以下场景误删新实例：

- 相同 name 删除后重建；
- 两个 FastPath 副本并发处理同一 name；
- API Server Create 结果未知；
- 旧补偿任务晚到；
- Fastlet 重启恢复旧 runtime。

如果拒绝引入 `runtimeInstanceID`，则必须禁止 namespace/name 在任何历史实例清理完成前复用，否则补偿删除无法安全 fencing。

CRD 创建后获得的 Kubernetes UID 仍然保留为 Kubernetes 对象 incarnation，但在方案 A 中不是 Fastlet 第一次创建 runtime 的必要输入。

### 4.2 最终身份模型

CRD-first 中，API Server Create 是 IO 1，Fastlet Create 是 IO 2，因此 Fastlet 从第一次请求开始同时使用：

```text
KubernetesObject = namespace/name + metadata.uid
RuntimeFence      = runtimeInstanceID + assignment attempt
```

最终方案保留 `runtimeInstanceID`。UID 能区分 CRD 删除重建，但不能区分同一个 CRD 内的 reset、改派或多次 runtime incarnation；runtimeInstanceID 负责节点侧删除、超时补偿和迟到请求的精确 fencing。

## 5. Assignment Annotation 契约

使用一个 versioned JSON annotation，而不是多个可能部分更新的 annotation：

```yaml
metadata:
  name: sb-01k...
  annotations:
    sandbox.fast.io/assignment: |
      {
        "version": "v1",
        "fastletName": "pool-fastlet-abc",
        "fastletPodUID": "...",
        "nodeName": "worker-1",
        "attempt": 1,
        "instanceGeneration": 1,
        "routeGeneration": 1,
        "runtimeInstanceID": "...",
        "runtimeProfileHash": "...",
        "resourceProfileHash": "...",
        "infraProfileHash": "..."
      }
```

规则：

1. annotation 与 CRD spec 在同一次 API Server Create 中写入；
2. annotation 是 FastPath 创建对象的有效初始 assignment，不是普通提示信息；
3. status 尚未补齐时，Controller、查询和恢复逻辑必须承认 annotation assignment；
4. status 补齐后，`status.assignment` 是声明式投影；
5. annotation 与 status 同时存在且不一致时必须 fail closed，不能静默重新调度；
6. annotation 至少在本实例生命周期内保留，用于审计、恢复和冲突判断；
7. reset/reassignment 必须使用新的 attempt/runtimeInstanceID，并通过明确的 Controller 状态机和 resourceVersion CAS 修改，不能任意覆盖初始 annotation；
8. annotation 不包含 owner、lease 或 timeout；并发安全来自 API Server CAS 和 Fastlet identity 幂等。

可以定义统一函数：

```text
EffectiveAssignment(Sandbox):
  if status.assignment exists:
      require it matches annotation fence
      return status.assignment
  else if assignment annotation exists:
      return annotation assignment
  else:
      return unassigned
```

## 6. Fastlet `CreateSandbox` 契约

当前 `EnsureSandbox` 改名为 `CreateSandbox`，并承担 reservation 和 Ensure 的合并语义。

请求至少包含：

- namespace/name；
- runtimeInstanceID；
- target Fastlet Pod UID；
- image、command、args、env、workingDir；
- 必要的 create spec identity。

Fastlet 内部处理顺序：

```text
lock admission state
  -> validate Fastlet Pod UID
  -> reject if recovering/draining/runtime/infra unavailable
  -> deleted tombstone for same/higher generation: GenerationFenced
  -> same CRD UID/generation/runtimeInstanceID: idempotent return or resume
  -> same CRD UID with a conflicting runtimeInstanceID/fence: explicit conflict
  -> validate used + creating < maxSandboxesPerPod
  -> insert Creating placeholder atomically
unlock

create runtime/network/infra/route
  -> success: mark Ready
  -> failure before side effects: remove placeholder and release capacity
  -> partial failure: retain FailedNeedsCleanup and identity until cleanup completes
```

Fastlet 返回的错误必须区分：

- `CapacityRejected`：可选择下一个 Top-K；
- `Draining`：可选择下一个 Top-K；
- `RuntimeUnavailable` / `NetworkUnavailable` / `InfraUnavailable`：只有 `RejectedBeforeSideEffects` 才可选择下一个 Top-K；
- `IdentityConflict`：同名不同实例，进入冲突处理；
- `GenerationFenced`：Delete/Reset 已经使该 generation 永久失效；
- `UnknownOutcome`：不得立即改选其他 Fastlet，必须先对同一 Fastlet、同一 runtimeInstanceID 幂等重试或检查。

只有明确的可重试拒绝才允许尝试下一个候选。网络超时不能直接在另一个 Fastlet 创建，否则会主动制造双实例。

## 7. Controller 如何接管

### 7.1 FastPath 创建的 CRD

Controller 看到 assignment annotation 后：

1. 解析并校验 version、Fastlet Pod UID、attempt、runtimeInstanceID；
2. 如果 status.assignment 为空，把 annotation 投影到 status；
3. 如果 status 与 annotation 一致，继续观察 Fastlet；
4. 如果二者冲突，设置 Conflict Condition 并 fail closed；
5. 使用同一个 runtimeInstanceID 对 Fastlet 执行幂等确认/观察；
6. 补齐 RuntimeReady/DataPlaneReady Conditions。

Controller 不应因为 status 暂时为空而重新 Top-K 调度。

### 7.2 用户直接创建的 CRD

Direct-CRD 路径没有 assignment annotation 时，Controller 仍然可以按声明式语义工作：

```text
Watch Sandbox
  -> Top-K
  -> CAS 初始化 assignment annotation
  -> annotation 异步投影到 status.assignment
  -> Fastlet CreateSandbox
  -> 写 Conditions
```

该路径不属于 FastPath，不要求两次 IO。Fastlet 明确无副作用拒绝时，Controller 只有在另一个候选已经存在时才直接 CAS 改派；没有替代候选时保留当前 annotation。这样既能在 Pool 扩容后使用新 Fastlet，也不会形成 assignment 为空的抖动窗口。

## 8. 被拒绝方案记录：Fastlet-first 的异步补偿

本节不进入实现，仅说明拒绝 Fastlet-first 的原因。

Fastlet 已经创建 runtime、但 API Server Create 失败时，FastPath 不应该同步增加更多 IO 来拖长正常路径。

FastPath 返回创建失败，并启动有 fence 的异步补偿任务。

补偿任务不能收到错误后直接删除，因为 API Server timeout 可能是“请求成功、响应丢失”。安全流程是：

```text
Get Sandbox namespace/name with bounded retry

if CRD exists and annotation.runtimeInstanceID == ours:
    CRD commit succeeded; do not delete

if CRD exists and annotation.runtimeInstanceID != ours:
    another FastPath won; delete our Fastlet runtime

if CRD remains absent after the ambiguity window:
    delete our Fastlet runtime
```

删除请求必须携带：

- namespace/name；
- runtimeInstanceID；
- Fastlet Pod UID。

Fastlet 只允许删除完全匹配的实例。

### 8.1 AlreadyExists

第二次 IO 返回 AlreadyExists 时进入非 happy path：

1. 直接 `Get namespace/name`，不做 label List；
2. 比较现有 CRD spec；
3. 如果是同一幂等请求，返回现有对象，并清理当前多余 runtime；
4. 如果 spec 不同，返回冲突，并清理当前 runtime。

幂等重试和多活竞争路径允许超过两次 IO；两次 IO约束只针对无竞争的首次 happy path。

方案 B 在 CRD Create 失败时尚未调用 Fastlet，不需要清理节点 runtime；如果 API Server 返回 timeout 等未知结果，仍需按 `namespace/name` 查询确认是否实际创建成功，但不会产生节点孤儿。

## 9. 决策记录：两种方案的 FastPath 崩溃窗口

方案 A 最危险的窗口是：

```text
Fastlet CreateSandbox 成功
  -> FastPath 进程崩溃
  -> Sandbox CRD 尚未创建
```

这时 FastPath 补偿 goroutine 不会运行，必须有第二道机制。

Fastlet 应把 CRD 尚未确认的实例标记为 `Provisional`，并设置 bounded TTL。Controller 看到成功创建的 CRD 后，使用相同 runtimeInstanceID 幂等确认该实例。长期未被确认的 provisional runtime 由 Fastlet 自清理。

这个确认可以复用幂等 `CreateSandbox`，携带 `committed=true` 和 CRD UID，也可以设计独立的内部确认接口。它发生在 Controller 异步路径，不计入 FastPath 两次 IO预算。

如果不提供 provisional TTL/确认机制，FastPath 在 IO 1 与 IO 2 之间崩溃会在一个仍然存活的 Fastlet Pod 中永久泄漏 runtime，而当前 Node Janitor 因 Fastlet Pod 仍存在不会清理它。

CRD-first 的崩溃窗口是 CRD 已创建、Fastlet 尚未创建。该状态不会形成节点孤儿。Controller 读取 annotation 后，可以立即使用相同 UID、attempt 和 runtimeInstanceID 执行幂等 Create；即使 FastPath 尚在工作，Fastlet 也只会创建一个实例。Controller 不得仅因为 status 尚未投影就重新调度。

## 10. CreateSandbox 成功语义变化

两次 IO设计意味着 FastPath 成功返回前同步保证：

1. Sandbox CRD 已经持久化并包含 assignment annotation；
2. Fastlet 已经创建 runtime 和私有网络；
3. Pool 声明的必需 Infra Component 已通过 readiness；
4. Fastlet local route 已发布，因此所需 DataPlane 已 Ready。

FastPath 不再同步等待：

- Controller 把 assignment 写入 status；
- Controller Conditions 收敛；
- status 的 read-after-write 投影。

Fastlet `CreateSandbox` 是第二次 IO，调用时已经持有 CRD UID，因此 route publication 不需要等待 Controller。任何必需 Infra 或 route 未 Ready 都不能返回成功；CRD 保持 Pending/Creating，由 FastPath 重试或 Controller 收敛。Controller status 投影仍然是异步的，不进入成功路径 IO 预算。

## 11. 接口变化

删除：

- `ReserveForCreate`；
- Fastlet `ReserveSandbox`；
- Fastlet `CancelReservation`；
- reservation token、TTL、reservation 状态；
- request ID hash label 和 List lookup；
- FastPath 同步 assignment status CAS；
- FastPath 同步 MarkReady status Update；
- 返回前最后一次 Sandbox Get。

改名/重定义：

- Fastlet `EnsureSandbox` -> `CreateSandbox`；
- `request_id` 与 `metadata.name` 合一；
- Fastlet 创建身份使用 `namespace/name + CRD UID + instanceGeneration + runtimeInstanceID`；
- assignment annotation 成为 Controller 补 status 前的有效分配结果。

保留：

- 本地 Registry + Top-K；
- Fastlet 原子 admission；
- Fastlet Pod UID fencing；
- Controller 声明式删除/reset/expire/recovery；
- Node Janitor 异常兜底；
- 多活 FastPath；
- Kubernetes API Server 作为最终 CRD 唯一性裁决者。

## 12. 方案代价

两种顺序都把 happy path 从约 13 次下游 IO 降到 2 次，但代价不同。

方案 A 的代价：

- runtime 先于 CRD 创建，存在明确的 provisional 窗口；
- 多活同名竞争可能先在多个 Fastlet 创建，再由 API Server Create 决定 winner；
- 失败路径需要异步检查和 fenced delete；
- Fastlet 必须支持 provisional TTL；
- 节点侧初始身份不能依赖尚未生成的 CRD UID。

方案 B 的代价：

- 首个候选一旦拒绝，每次改派至少增加一次 assignment Patch 和一次 Fastlet Create；
- Controller 与 FastPath 可以并发幂等 Create，但只能在明确无副作用拒绝或 Pod UID 消失后 CAS 改派；
- assignment annotation 从创建意图变成最终分配结果时，需要 CAS、attempt 和 fence 共同保证一致性；
- CRD 可能短暂存在但 runtime 尚未创建，查询接口必须通过 Conditions 准确表达这一状态。

这些代价是可以接受的，因为：

- 正常创建延迟是产品核心能力；
- 冲突和异常不是 happy path；
- Fastlet 本来就是最终资源所有者和 admission 原子点；
- Kubernetes API Server 仍然负责全局名称唯一性；
- 复杂度被移动到了可重试、可观测的后台补偿，而不是阻塞用户请求。

## 13. 最终目标流程

```text
request_id/name validation                    # 0 IO
local Registry Top-K                         # 0 IO

Kubernetes Create Sandbox CRD                # IO 1
  - complete spec
  - initial assignment annotation

Fastlet CreateSandbox                        # IO 2
  - CRD UID + generation + runtimeInstanceID fencing
  - atomic capacity check
  - runtime/network/infra/route creation
  - required data plane ready

return to user

Controller asynchronously:
  - annotation -> status
  - idempotent same-assignment confirmation
  - Conditions/data-plane convergence

candidate rejection path:
  - CAS Patch assignment with next attempt     # additional IO
  - call next Fastlet CreateSandbox            # additional IO
```

FastPath 的核心不应该是“在一次 RPC 内把整个声明式系统收敛完”，而应该是：

> 用最少的同步 IO完成实际 runtime 创建和全局持久化，把可恢复、可重试的投影与补偿留给 Controller。
