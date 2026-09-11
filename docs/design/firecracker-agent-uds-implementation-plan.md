# 阶段 1 施工方案：runtime-agent UDS 接线（PinImage/LeaseDevices + driver client）

> 文档类型：施工任务书（转交实现）
>
> 日期：2026-08-27
>
> 前置依赖：#23（agent pull 层，已合并，`internal/runtime/firecracker/agent/`
> 的 `Client.PullImage` 已完成）；#21（设计文档，含本任务的协议与代码落点
> 定案）。
>
> 实现人按本任务书独立完成，有分歧以本任务书 + #21 设计文档为准。

---

## 1. 背景与目标

pull 层（`agent.Client.PullImage`）已能完成 `Image → index → manifest →
digest 校验落盘`。本任务把它暴露为**节点级 UDS 服务**，并给 fastlet driver
接上客户端，打通阶段 1 验收链路：

1. `warmImages` 真正拉取：`SandboxPool.Spec.WarmImages → FAST_SANDBOX_WARM_IMAGES
   → driver.PullImage → UDS PinImage → agent.PullImage → 缓存落盘 →
   driver 现有 `resolveRootfsImage` 命中（下游零改动）；
2. 空缓存 create 消费发布产物 restore 成功；
3. agent 不可用时 driver 能力降级（不 panic、不误报 Running）。

**范围边界（本次不做）**：

- 不做 P2P（阶段 2）、overlaybd/ublk 设备（阶段 3）、快照 RPC
  （`PinSnapshot`/`LeaseSnapshotDevices`/`SealSnapshot`，阶段 4）；
- `LeaseDevices` 在 native 阶段返回**缓存文件路径**（`<StateRoot>/images/
  <key>/rootfs.img`），设备语义留待 overlaybd 阶段；
- agent 部署载体（独立 DaemonSet vs 并置容器，待决策项）不阻塞本任务——
  代码按"独立进程"形态写，部署形态最后定；
- 不改 fastlet 协议、不改 `resolveRootfsImage`/GC 现有语义。

## 2. 代码位置与包结构（#21 §3.0 定案）

```text
internal/runtime/firecracker/
├── driver.go                    # 现有 driver（改造点见 §7）
├── agent_client.go              # 新增：driver 侧 UDS client
└── agent/
    ├── protocol/                # 新增：UDS 协议类型（driver + agent 共享）
    │   ├── types.go             # 请求/响应/错误码
    │   └── types_test.go
    ├── server/                  # 新增：UDS HTTP server
    │   ├── server.go
    │   └── server_test.go
    └── state/                   # 新增：租约与引用计数
        ├── leases.go            # lease 记录 + 引用计数 + journal
        └── leases_test.go
cmd/firecracker-runtime-agent/   # 新增：agent 入口（薄）
└── main.go
```

依赖单向：driver → `agent/protocol`（仅消息类型）；agent server 自用
`agent/{protocol,state}` + 既有 `agent.Client`（pull 层）；`cmd` 为薄入口。
镜像 `internal/runtime/boxlite/{driver,protocol,server,state}` 的形态。

## 3. 协议定义（`agent/protocol/`）

消息均为 JSON over HTTP（`http://localhost/<path>`，UDS transport），
风格对齐 `internal/runtime/boxlite/protocol/types.go`。

```go
// 请求/响应（示意，字段以实现为准但语义必须一致）
type Request struct {
    RequestID string `json:"requestId"`     // 幂等键，必填
    Namespace string `json:"namespace"`     // 身份头（见 §5）
    PodUID    string `json:"podUid"`
    // 各 RPC 的载荷字段见下
}

type PinImageRequest struct { Request; Image string `json:"image"` }
type PinImageResponse struct { ManifestDigest string `json:"manifestDigest"`; Ready bool `json:"ready"` }

type UnpinImageRequest struct { Request; Image string `json:"image"` }

type LeaseDevicesRequest struct {
    Request
    SandboxID      string `json:"sandboxId"`
    Image          string `json:"image"`
    MemSizeMiB     int    `json:"memSizeMiB"`       // 资源个性化（阶段 3 用，native 可忽略）
    RootfsWritable bool   `json:"rootfsWritable"`
}
type LeaseDevicesResponse struct {
    LeaseID       string `json:"leaseId"`
    RootfsDev     string `json:"rootfsDev"`     // native: 缓存 rootfs.img 绝对路径
    MemDev        string `json:"memDev"`        // native: 可为空
    ManifestDigest string `json:"manifestDigest"`
}

type ReleaseDevicesRequest struct { Request; LeaseID string `json:"leaseId"` }
type ListLeasesRequest struct { Request }
type CompatibilityRequest struct { Request }
type HealthRequest struct { Request }
```

**RPC 路由（HTTP method + path，仿 boxlite server 的版本化风格）**：

| RPC | Method/Path | 语义 |
|-----|-------------|------|
| PinImage | `POST /v1/pin-image` | 拉取并保活 image（幂等：已 pin → 直接返回；已缓存 → 幂等返回） |
| UnpinImage | `POST /v1/unpin-image` | 引用减一；归零释放 pin |
| LeaseDevices | `POST /v1/lease-devices` | 创建租约，返回缓存文件路径；同 sandboxID 重复 → 幂等返回既有租约 |
| ReleaseDevices | `POST /v1/release-devices` | 释放租约（校验归属：租约的 PodUID 必须匹配请求头） |
| ListLeases | `POST /v1/list-leases` | 返回全部租约（恢复/清点用） |
| Compatibility | `POST /v1/compatibility` | 返回节点 compatibility class（阶段 3 校验用，native 可返回占位） |
| Health | `POST /v1/health` | 返回 `{ok, cacheBytes, leaseCount, ...}` |

**错误码（`protocol` 包内定义，driver 侧映射）**：

```go
type ErrorCode string
const (
    ErrorInvalidRequest ErrorCode = "InvalidRequest"   // 400
    ErrorUnauthorized   ErrorCode = "Unauthorized"     // 403（身份头不符）
    ErrorConflict       ErrorCode = "Conflict"         // 409（幂等键冲突/归属不符）
    ErrorNotFound       ErrorCode = "NotFound"         // 404（image 未发布 → driver 映射 ErrImageNotReady）
    ErrorInternal       ErrorCode = "Internal"         // 500
)
type ErrorResponse struct { Code ErrorCode `json:"code"`; Message string `json:"message"` }
```

**幂等契约**：相同 `request_id` 的变更 RPC（PinImage/UnpinImage/Lease/
Release）重放时返回**首次执行的结果**（结果缓存于 journal），不重复副作用。
LeaseDevices 额外以 `sandbox_id` 为业务幂等键：同 sandbox 重复租用返回
既有 lease（即使 request_id 不同）——与 driver `EnsureSandbox` 的幂等
语义对齐。

## 4. UDS server（`agent/server/`）

- **传输**：`net.Listen("unix", socketPath)` + `http.Serve`，或直接镜像
  boxlite server（`internal/runtime/boxlite/server/server.go` 是现成先例，
  版本化路由 + JSON 编解码 + 错误响应已实现，可参考其结构）；
- **socket 权限**：`0660`，属组 `fast-sandbox`（创建后 `os.Chmod` +
  `os.Chgrp`，属组名解析失败时告警并继续）；
- **认证（#21 §7 定案）**：每个请求校验 `Namespace` + `PodUID` 身份头：
  - 空 PodUID → 拒绝（403）；
  - 幂等键缓存与租约记录**绑定 PodUID**——跨 PodUID 重放/释放一律
    Conflict（防跨 pool 误操作）；
- **journal**：变更 RPC 先写 journal（`<StateRoot>/agent/journal.log`，
  append-only JSON 行：`{request_id, podUID, op, args, result, at}`）再
  执行；执行完成后回填 result。重放时查 journal 命中直接返回记录结果；
  崩溃后加载 journal 重建幂等缓存（租约恢复见 §5）；
- **并发**：`request_id` 幂等查重需要互斥（per-key 或全局锁，实现自选，
  并发测试覆盖）；
- 服务生命周期：`Serve()` 阻塞 + `Shutdown(ctx)` 优雅关闭（幂等缓存
  flush）。

## 5. 租约与引用（`agent/state/`）

```go
type Lease struct {
    LeaseID   string    `json:"leaseId"`   // uuid v4
    SandboxID string    `json:"sandboxId"`
    Image     string    `json:"image"`
    PodUID    string    `json:"podUid"`
    Namespace string    `json:"namespace"`
    RootfsDev string    `json:"rootfsDev"` // native: 缓存文件路径
    MemDev    string    `json:"memDev"`
    CreatedAt time.Time `json:"createdAt"`
}

type State struct { /* leases map + image 引用计数（pin/lease 数） */ }
```

- **引用计数**：`image → {pinCount, leaseCount}`；UnpinImage 只减
  pinCount；ReleaseDevices 减 leaseCount；两者都归零才允许 GC 淘汰
  （GC 是 pull 层/后续任务的事，本任务只维护计数与查询）；
- **journal 恢复**：启动时加载 journal → 重建 lease 表与幂等缓存；
  journal 行损坏（部分写）→ 截断到最后一个完整行（append-only 的
  性质，容忍尾部截断）；
- `LeaseDevices` 的 rootfs 路径解析：`stateRoot + images/ + imageKey(image)
  + /rootfs.img`——**路径推导必须与 pull 层 `imageDir` 逐字节一致**
  （复用 `agent/cache.go` 的 `imageDir`，不要重复实现）；
- 校验：PinImage 后必须 `resolveRootfsImage` 等价检查（缓存完整才返回
  ready；不完整 → 先触发 `Client.PullImage` 再验）。

## 6. driver 侧 client（`internal/runtime/firecracker/agent_client.go`）

```go
// AgentClient 是 driver 对 runtime-agent 的视图（测试可 fake 注入）。
type AgentClient interface {
    PinImage(ctx context.Context, requestID, image string) (string, error) // -> manifestDigest
    UnpinImage(ctx context.Context, requestID, image string) error
    LeaseDevices(ctx context.Context, requestID string, spec *fastletapi.SandboxSpec) (Lease, error)
    ReleaseDevices(ctx context.Context, requestID, leaseID string) error
    ListLeases(ctx context.Context) ([]Lease, error)
    Compatibility(ctx context.Context) (string, error)
    Health(ctx context.Context) error
}
```

- 实现：`http.Client` + `DialContext: unix`（boxlite driver 先例，
  `internal/runtime/boxlite/driver/driver.go` 的 UDS client）；
- 请求头：`Namespace`/`PodUID` 从 driver 配置（`SetNamespace` +
  podUID env）填充；
- 错误映射：`ErrorNotFound → runtimecontract.ErrImageNotReady`、
  `ErrorUnauthorized/Conflict → ErrInvalidConfig` 包装、网络错误 →
  包装（调用方决定降级）；
- driver 字段注入：`newAgentClient func(socketPath string) (AgentClient, error)`
  （仿 `newClient` 模式，测试 fake）；socket 路径来自环境变量
  `FAST_SANDBOX_RUNTIME_AGENT_SOCKET`（仿 `FAST_SANDBOX_NODE_CLEANUP_SOCKET`
  先例）；无 socket 配置时 client 为 nil，driver 走"本地模式"
  （见 §7 降级）。

## 7. driver 接线（`internal/runtime/firecracker/`）

| 改动点 | 行为 |
|--------|------|
| `PullImage`（images.go） | agent client 可用 → `PinImage` 代理（幂等返回 nil）；不可用 → 保持现状（本地缓存检查，兼容单机调试） |
| `ProbeCapabilities` | agent 可用 → `Health` 失败则 `CapabilityDegraded`（Reason: AgentUnavailable）；可用 → 现有一致 |
| `EnsureSandbox`（native 分支） | 保留 `resolveRootfsImage` 路径（缓存由 agent 拉取后命中），**不接 LeaseDevices 到设备**（阶段 3）；LeaseDevices 调用仅当后续阶段需要时启用——本任务保持 `resolveRootfsImage` 为主路径 |
| `DeleteSandbox` | agent 可用 → `ReleaseDevices`（若曾租用）+ `UnpinImage`（按引用） |
| `SetNamespace`/podUID | 已有 `SetNamespace`；podUID 由构造时配置传入 client 请求头 |

**降级语义（重点）**：agent 不可达/未部署时 driver 必须**回退本地模式**
（与现状行为一致），不得让 warmImages/create 全挂——阶段 1 允许
"agent 缺席 = 无远程拉取，仍可本地冷启动"。

## 8. agent 入口（`cmd/firecracker-runtime-agent/`）

- 配置（env）：`FAST_SANDBOX_RUNTIME_AGENT_SOCKET`（默认
  `/run/fast-sandbox/firecracker/runtime.sock`）、`FAST_SANDBOX_ARTIFACT_STORE`
  （`s3://bucket/prefix`）、`FAST_SANDBOX_STATE_ROOT`、凭据走既有
  `registryconfig` 挂载（`/etc/fast-sandbox/registry/registry.json`，
  Credential 按 Host 匹配 store endpoint）；
- 组装：`agent.NewClient(storeRoot, credential)`（pull 层）→
  `state.New(...)` → `server.New(..., socketPath)` → `Serve()`；
- 信号处理：SIGTERM/SIGINT → 优雅关闭。

## 9. 测试计划

### 单元测试

**protocol**：
- 请求/响应编解码；错误码 JSON 形态；
- 幂等契约的 request_id 必填校验。

**state（leases）**：
- lease CRUD、sandboxID 幂等（重复租用返回既有）、PodUID 归属校验；
- 引用计数：pin/lease 增删、归零边界（负数保护）；
- journal：落盘顺序（先写后执行）、崩溃恢复（重放幂等缓存 + lease 表）、
  尾部截断容忍、损坏行跳过。

**server**：
- 路由分发（7 个 RPC）；未知路径 → 404/错误响应；
- 身份头：空 PodUID → 403；跨 PodUID 释放/重放 → Conflict；
- 幂等重放：同 request_id 第二次返回首次结果（副作用不重复——用
  PinImage 计数 fake pull 断言只拉一次）；
- 并发同 request_id → 只执行一次。

**driver client（agent_client.go）**：
- UDS dial 失败 → 包装错误；超时 → 包装；
- 错误码映射（NotFound → ErrImageNotReady 等）；
- fake AgentClient 注入 → driver 各调用点行为（见下）。

**driver 接线**：
- `PullImage` 代理：fake agent 断言 PinImage 调用参数（image/requestID）
  与幂等返回；
- `ProbeCapabilities`：agent Health 失败 → Degraded(AgentUnavailable)；
  agent nil → 现状行为不变；
- `DeleteSandbox`：释放 + unpin 调用（fake 断言）；
- 降级：agent nil / 网络错误 → PullImage 回退本地缓存检查（现有测试
  保持绿色）。

### 集成测试

- 真实 unix socket：server + client 端到端——`PinImage` 全链路（真实
  `agent.Client.PullImage` + fake store 或 httptest S3）→ `resolveRootfsImage`
  命中；`LeaseDevices` 返回缓存路径；`ReleaseDevices` 幂等；
- `warmImages` 链路：`driver.PullImage(ctx, image)`（agent 在线）→ 缓存
  落盘 → `EnsureSandbox` 冷启动可用；
- agent 进程未启动（socket 不存在）：driver 全路径不 panic、行为与
  现状一致（复用现有 driver 测试套件跑一遍）。

### 验收标准

1. `go build ./...`、`go test ./internal/runtime/firecracker/... ./cmd/firecracker-runtime-agent/...
   -count=1 -race`、`go vet ./...` 全绿；
2. 上述测试清单全部覆盖；
3. 协议与 #21 §2.2 一致（无快照 RPC；`Health`/`Compatibility`/`ListLeases`
   存在）；
4. 不引入新三方依赖；
5. 降级路径验证：agent 缺席时现有 firecracker driver 测试不改行为。

## 10. 参考材料

- 先例：`internal/runtime/boxlite/{driver,protocol,server,state}`（UDS
  HTTP client/server 形态）；
- #21 设计：`docs/design/firecracker-on-demand-loading.md` §2.2（UDS API）、
  §3.0（代码落点）、§7（UDS 认证定案）；细节文档 §2.2/§2.3；
- 已合并 pull 层：`internal/runtime/firecracker/agent/{pull.go,index.go,
  manifest.go,s3client.go,cache.go}`（`imageDir`/`Client.PullImage` 复用）；
- driver 现状：`internal/runtime/firecracker/{driver.go,images.go}`（注入
  模式、PullImage/ProbeCapabilities/DeleteSandbox 接线点）。

## 11. 交付物

- 上述包 + 单测/集成测试；
- 实现说明（PR 描述）：协议路由表、幂等实现方式（journal 结构）、
  降级语义、与 #23 pull 层的复用点、部署载体待决策项说明。
