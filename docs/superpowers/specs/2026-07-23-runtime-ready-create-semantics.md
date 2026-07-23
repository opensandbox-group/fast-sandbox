# CreateSandbox RuntimeReady 快速返回语义

**状态**：已决策，实施中

**日期**：2026-07-23

## 1. 决策

`CreateSandbox` 成功返回只同步保证 Sandbox runtime 已可靠建立，不再等待 Infra Component readiness 和本地代理路由发布。

成功返回点必须满足：

```text
Sandbox CRD 已持久化并携带 assignment annotation
  -> Fastlet Admission 已原子占用容量
  -> instance config/state 已按最终内容持久化
  -> runtime 和私有网络已创建
  -> container/task 已启动
  -> Fastlet 已登记相同 UID/generation/assignment 的 runtime
  -> CreateSandbox 返回成功
```

返回后由 Fastlet 本地异步推进：

```text
Infra Instance Init
  -> required Infra Service Readiness
  -> Fastlet Proxy route publication
  -> DataPlaneReady=True
```

## 2. 状态语义

| Fastlet phase | RuntimeState | DataPlaneState |
|---|---|---|
| `creating` | Creating | Pending |
| `infra-pending` / `initializing-infra` | Ready | Creating |
| `route-pending` / `publishing-route` | Ready | Creating |
| `infra-unavailable` / `route-unavailable` | Ready | Unavailable |
| `running` | Ready | Ready |

`UserProcessState` 独立观察，不作为 Create 成功条件。`required` 表示该组件是 `DataPlaneReady` 的必要条件，不表示它必须阻塞 Create RPC。

## 3. 幂等与失败语义

- 相同 runtime identity 在 DataPlane 初始化阶段的重复 Create 直接返回幂等成功，不同步重做 readiness。
- Runtime 建立前的失败仍走 Top-K rejection、相同 identity 接管和 CRD-first 失败语义。
- Create 成功后的 Infra/route 失败不回滚 runtime，也不推翻已经返回的成功；Fastlet 标记 DataPlane Unavailable、记录 diagnostics 并异步重试。
- Delete/reset 取消异步任务；UID、instance generation、assignment attempt 和 route generation 共同阻止旧任务影响新实例。
- `ResolveEndpoint` 在 DataPlane 未 Ready 时 fail closed，明确区分“初始化中”和“不可用”。

## 4. 性能和持久化

- Infra Service Readiness 立即执行第一次探测，随后按 `1/2/4/8/10ms` 退避，间隔上限为 10ms，总时长仍受 profile timeout 控制。
- `infra.json` 和 `state.json` 保持职责分离，但在最终路径和 mounts 全部计算完成后各写一次，删除重复 rewrite/fsync。
- Create RPC 指标记录 RuntimeReady；DataPlaneReady 延迟在异步状态真正进入 Ready 时记录，不能在 RPC 返回时冒充 DataPlaneReady。

## 5. API 和客户端体验

FastPath happy path 仍保持两次下游 IO：

```text
IO 1: Kubernetes CRD Create
IO 2: Fastlet admission + runtime create
```

FastPath 不为状态投影增加第三次 Kubernetes 写。Controller watch 异步写入独立的 Runtime/DataPlane Conditions。

`fastctl run` 输出必须明确为 `Sandbox runtime created`，并提示 DataPlane 正在异步初始化。紧接着执行 exec/file 时，调用方应等待 `DataPlaneState=Ready` 或处理可重试的 `Unavailable`。

## 6. 远端验收记录

2026-07-23 在 `ssh-fast`、kind context `kind-fsb-e2e-basic` 完成：

- 完整 `make test-unit GOFLAGS=` 通过；
- runtime/controller/fastctl 定向 `go test -race` 通过；
- `TestQuickStartOpenSandboxExecd` 通过，覆盖 diagnostics、真实 Execd exec、文件上传/下载和声明式删除；
- `TestInfraRuntimeAugmentation` 通过，证明 required Infra readiness 前不发布路由，之后可正常访问。

同一 warm container + Execd Pool 连续四次 Create RPC 为：

```text
89.341635ms
89.691751ms
87.188365ms
83.108690ms
```

其中一个样本的 Fastlet diagnostics 为：

```text
admission -> runtime/infra-pending: 70.931206ms
infra-pending -> route-pending:     17.222084ms
route-pending -> running:            0.628937ms
```

这证明旧的固定 100ms readiness 等待已经从 Create 热路径移除。当前约 67–74ms 的 containerd runtime 阶段仍是后续恢复 40–50ms 目标的主要优化对象。
