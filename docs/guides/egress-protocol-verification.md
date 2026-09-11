# Egress 协议交叉验证核对表（fast-sandbox × OpenSandbox actionhandler）

> 文档类型：协议核对表（任务 1 产出物）
>
> 日期：2026-09-01
>
> 核对基准：
>
> - fast-sandbox 侧：`internal/protocol/action/types.go`（master @ 7df44ad）+
>   `internal/fastlet/action/manager.go`（HTTPCaller / buildRequest）+ 投递语义；
> - OpenSandbox 侧：`Pangjiping/OpenSandbox @ feat/egress-actions-handler`
>   pin `460b1cb`，`components/egress/pkg/actionhandler/actionhandler.go` +
>   `components/egress/fastsandbox_actions.go`（lifecycle 语义）+ `fastsandbox_server.go`
>   （status 端点）。
>
> 方法：两侧独立实现同一协议（`sandbox.fast.io/actions/v1`），逐字段字节级
> 核对 + egress 侧 `go build ./components/egress/...` 编译验证（已通过，
> 见 §验证）。
>
> 结论：**协议一致，可互操作**；差异均为文档化差异（§差异），实现者不修改
> 协议，交回评审决策。

---

## 1. 协议常量与枚举

| 项 | fast-sandbox | egress | 一致 |
|----|--------------|--------|------|
| `apiVersion` | `sandbox.fast.io/actions/v1`（`types.go:4`） | `sandbox.fast.io/actions/v1`（`actionhandler.go:41`） | ✅ |
| Operation | `SET_BINDING` / `LIFECYCLE_HOOK` / `REMOVE_BINDING` | 同左 | ✅ |
| LifecycleHook | `sandbox.runtime-ready` / `sandbox.data-plane-ready` | 同左（`HookRuntimeReady` / `HookDataPlaneReady`） | ✅ |

## 2. 请求字段（envelope）

| 字段 | fast-sandbox | egress | 一致 |
|------|--------------|--------|------|
| `invocationId` | 必填（stable hash，重试同值） | 透传（不校验内容） | ✅ |
| `sandbox.uid` | 必填（构建请求总是填充） | 必填（Validate 拒绝空） | ✅ |
| `sandbox.name` / `namespace` | 填充；omitempty | 不校验 | ✅ |
| `revision.specGeneration` | `int64`，总是填充 | `uint64`，总是填充 | ✅（wire 均为数字） |
| `revision.runtimeInstanceId` | 必填（构建总是填充） | **必填**（Validate 拒绝空） | ✅ |
| `revision.attachmentId` | 必填（构建总是填充） | **必填**（Validate 拒绝空） | ✅ |
| `revision.routeGeneration` | `int64` omitempty，总是填充 | `uint64` omitempty，总是填充 | ✅ |
| `attachment.network.ip` | string，总是填充 | `netip.Addr`（JSON 字符串），SET_BINDING 非 removal 必填 | ✅ |
| `attachment.network.gateway` | string | `netip.Addr` | ✅ |
| `attachment.network.privateCidr` | string | `netip.Prefix`（JSON 字符串 "cidr/prefix"） | ✅ |
| `attachment.network.hostVeth` | string，总是填充 | 必填（`requireAttachment`） | ✅ |
| `binding.input` | `*string`：nil → `"input":null`；非 nil → 字符串（含 `""` 与字面量 `"null"`） | `json.RawMessage`：`"null"` = removal（`IsRemoval`）；字符串经 `InputString()` 校验 | ✅ |
| `hook.name` / `hook.sequence` | 必填，枚举校验 | 必填；未知 Hook 名拒绝 | ✅ |

fastlet 请求构造核对：`buildRequest`（`manager.go:914`）总是填充
`revision{specGeneration,runtimeInstanceId,attachmentId,routeGeneration}` 与
`attachment.network{ip,gateway,privateCidr,hostVeth}`（`actionAttachment`，
`sandbox/actions.go:104`），满足 egress 侧 fencing 与 attachment 必填要求。

## 3. 错误语义

| 场景 | egress 行为 | fast-sandbox 行为 | 一致 |
|------|-------------|-------------------|------|
| 未知 operation / 未知 Hook / 畸形 envelope | 400（fail-closed，绝不静默忽略） | 非 200 → 投递失败重试（at-least-once，同 invocationId） | ✅ |
| SET_BINDING 缺 binding / 缺 attachment / input 非字符串 | 400 | 同上 | ✅ |
| 非法策略 input | 400（`policy.ParsePolicy` 失败，状态无变更） | 同上 | ✅ |
| Hook 对未注册 subject | **409** | 重试 | ✅ |
| Hook fence 不匹配（stale instance） | **409**（不消费 pending policy） | 重试 | ✅ |
| data-plane-ready 无 pending policy | **409** fail-closed（peek 不消费，重试可成功） | 重试 | ✅ |
| enforcement 失败（nft 等） | 500（状态无变更，重试可恢复） | 重试 | ✅ |
| REMOVE_BINDING fence 不匹配 | **200 忽略**（永不卸载当前 subject） | 删除幂等 | ✅ |
| REMOVE_BINDING 未注册 subject | 200（清理缓存） | 删除幂等 | ✅ |

幂等/重放核对（egress 侧 `fastsandbox_actions.go` vs fast-sandbox `manager.go`）：

- SET_BINDING 重放：`RegisterAndEnforce` 幂等；input 更新对 active subject 原地应用、**不重放 Hooks** —— 与 fastlet `replayHooks` 逻辑一致；
- egress 重启：新 `instanceId`（status 端点）→ fastlet 检测变化 → 置 Pending 并重放最新 SET_BINDING + 已到达 Hooks（`manager.go:318`）——与 egress 侧注释契约一致；
- data-plane-ready peek-不-consume：瞬态 nft 失败后同 Hook 重试成功，不会永久 409。

## 4. status 端点（GET /_fastlet/v1/actions/status）

| 字段 | fast-sandbox `HandlerStatus` | egress `StatusResponse` | 一致 |
|------|------------------------------|-------------------------|------|
| `apiVersion` | 校验必须匹配 | 输出 `sandbox.fast.io/actions/v1` | ✅ |
| `ready` | 必须 `true` 才注册 Handler | `true`（MITM 未 pending 时） | ✅ |
| `instanceId` | 必须非空（重放触发） | 进程级随机，重启必变 | ✅ |
| `message` | omitempty（读取端不要求） | **不输出** | ⚠️ D3 |

fast-sandbox `HTTPCaller.Status`（`manager.go:46`）：非 200 / apiVersion 不匹配 /
`!ready` / 空 instanceId 均视为 Handler 未就绪。

## 5. 差异记录（不修改协议，交回评审）

| # | 差异 | 影响 | 建议 |
|---|------|------|------|
| D1 | `specGeneration` / `routeGeneration` 类型 `int64`（fast-sandbox）vs `uint64`（egress） | wire 均为 JSON 数字；fast-sandbox 从不产生负值，egress 拒绝负值 | 无实际影响，维持现状 |
| D2 | `revision.runtimeInstanceId` / `attachmentId` / `routeGeneration` 带 omitempty（fast-sandbox）vs 总是输出（egress） | fastlet 总是填充，不存在缺字段场景 | 无实际影响，维持现状 |
| D3 | status 响应 `message` 字段仅 fast-sandbox 定义 | 读取端 omitempty 兼容，egress 不输出 | 无实际影响，维持现状 |
| D4 | `ready` 语义独立定义（fast-sandbox 不解释含义，只读字段；egress 仅 MITM gate 置 false） | 语义不冲突 | 集成测试观测即可 |
| D5 | 策略体编码：`binding.input` 为 opaque string；egress `policy.ParsePolicy` 接受 `""` / `"null"` / `"{}"` → 默认 deny，其余须为 `NetworkPolicy` JSON | fast-sandbox 不解析内容 | 集成测试用 `{}` 起步；策略格式由 OpenSandbox 侧文档化 |

## 6. 验证

- egress 侧构建：`go build ./components/egress/...` @ `460b1cb` **通过**；
- 实测闭环：见 `integration-env.sh verify-egress` 阶段 0/2（status 端点字节断言）
  与 egress 日志断言（SET_BINDING applied / data-plane-ready, policy active /
  REMOVE_BINDING complete / restart replay）。

## 7. 参考

- fast-sandbox 协议：`internal/protocol/action/types.go`、`internal/fastlet/action/manager.go`
- egress 协议：`Pangjiping/OpenSandbox @ 460b1cb` `components/egress/pkg/actionhandler/actionhandler.go`、
  `components/egress/fastsandbox_actions.go`、`components/egress/fastsandbox_server.go`
- 方案：`docs/design/egress-integration-plan.md`；任务清单：`docs/design/egress-integration-plan-tasks.md`
