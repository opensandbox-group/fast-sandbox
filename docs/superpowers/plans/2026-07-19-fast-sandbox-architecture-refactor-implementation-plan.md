# Fast Sandbox 架构重构开发计划

**日期**：2026-07-19  
**状态**：执行中（阶段 0 完成，阶段 1 进行中）  
**代码基线**：`master@f92d8e34288365be227d2ee8a6f952687dc7be00`  
**本地仓库**：`/Users/fengjianhui/WorkSpaceL/fast-sandbox`  
**远端开发机**：SSH alias `fast`  
**远端仓库**：`~/fast-sandbox`

## 1. 目标

本计划把以下五份已确认设计落实为可部署、可测试的代码：

- [多活 Fast-Path 控制面设计](../specs/2026-07-18-multi-active-fastpath-control-plane-design.md)
- [Fastlet 网络架构设计](../specs/2026-05-05-fastlet-network-architecture-design.md)
- [控制面与数据面分离设计](../specs/2026-07-19-control-data-plane-separation-design.md)
- [Sandbox Runtime 抽象设计](../specs/2026-07-19-sandbox-runtime-abstraction-design.md)
- [跨模块架构决策](../specs/2026-07-19-fast-sandbox-cross-cutting-architecture-decisions.md)

最终交付结果：

```text
fastctl / Python SDK
  -> multi-active Fast-Path Server
  -> Sandbox CRD + assignment CAS
  -> Fastlet atomic admission
  -> RuntimeDriver

Sandbox data request
  -> multi-active Sandbox Proxy
  -> assigned Fastlet Proxy
  -> Sandbox private network
  -> injected Infra Component

Sandbox / Pool Controller
  -> leader-elected declarative Reconcile

NodeJanitor
  -> orphan runtime/network/backend cleanup only
```

完成时必须满足：

1. Fast-Path Server 可多活横向扩展，不依赖 Controller Leader 才能服务；
2. Controller-only 部署可以仅根据 CRD 完成 Sandbox 生命周期；
3. RPC Create 快速失败不遗留 CRD，成功创建具备 request ID 幂等性；
4. Fastlet 原子保证单 Pod admission、generation fencing 和进程重启恢复；
5. Pool 固定 runtime 和单 Sandbox ResourceProfile，资源限制由 Fastlet 实际执行；
6. Registry 使用 Watch、低频 Heartbeat、本地反馈和镜像亲和生成 Top-K；
7. 每个 Sandbox 拥有独立私有网络，可以重复使用任意内部端口；
8. Sandbox Proxy 和 Fastlet Proxy 按 Sandbox UID + target port 透明代理；
9. Fast Sandbox Core 不定义 Exec/File 协议，通过 Infra Component 注入和 SDK Adapter 提供用户能力；
10. container、gVisor、Kata 和 BoxLite 通过统一 RuntimeProfile/RuntimeDriver 边界接入；
11. Fastlet Pod 丢失后重建 Sandbox，不接管旧 runtime；
12. Pool 缩容和计划升级经过 Drain，Janitor 只做异常兜底。

## 2. 当前代码基线与主要差距

| 模块 | 当前 master | 目标状态 |
|---|---|---|
| 进程部署 | `cmd/controller` 同时启动 FastPath、两个 Controller、2 秒扫描 Loop | `fastpath/controller/all` 三角色；FastPath 多活，Controller 单活 |
| Leader Election | Manager 未配置 Leader Election | 仅 Controller 角色启用 Leader Election |
| Create | Fast 模式先创建 runtime 再异步写 CRD；Strong 模式先 CRD 后 runtime | 删除 Fast/Strong 双语义；RPC 先接纳、再提交 CRD、再 Ensure |
| RPC 幂等 | 没有 `request_id`，空名称按时间生成 | 稳定 request ID、参数摘要、同请求返回同 Sandbox |
| Assignment | Registry 本地 `Allocate` 加计数，annotation 搬运到 status | CRD status assignment CAS + Pod UID/attempt/generation fencing |
| Registry | 2 秒全量列 Pod，串行轮询 Fastlet；本地 Allocate 被当作占位事实 | Watch + 低频 Heartbeat；Registry 只生成候选，不拥有容量 |
| 调度 | 单候选，镜像加权，端口冲突，本地 `Allocated++` | Top-K、镜像亲和、稳定扰动；Fastlet admission 最终裁决 |
| Fastlet admission | SandboxManager 写 Creating placeholder，但没有检查 capacity | reservation + creating + running 原子上限和结构化拒绝 |
| Fastlet 恢复 | 进程重启后内存 map 为空 | 按 Fastlet Pod UID 扫描 RuntimeDriver 并恢复状态后 Ready |
| Pool Runtime API | `runtimeType + runtimeClassName + handler override` | 单一不可变 `spec.runtime` + 内部 RuntimeCatalog |
| Runtime | 所有 runtime 都映射到 ContainerdRuntime；无 BoxLite | ContainerdDriver 与 BoxLiteDriver 独立，统一 RuntimeDriver |
| 资源 | CPU/Memory 字段存在但未落实到 OCI/VM；cgroup path 基本未使用 | Pool ResourceProfile 传递到 Fastlet并成为实际硬限制 |
| 网络 | 只创建空 network namespace，无 veth/IP/NAT；endpoint 是 PodIP:port | NetworkManager/SlotPool、独立私网、AccessHandle、NAT |
| 数据面 | Fastlet Control 5758 同时提供 runtime logs；feature 分支把 Exec/File 加入控制面 | Sandbox Proxy + Fastlet Proxy；具体协议由 execd/envd 等提供 |
| Infra | shell `fs-helper` initContainer；真实 bind mount 代码被注释 | InfraProfile、Artifact Store、Injector、Lifecycle、readiness |
| Pool 缩容 | 直接删除 PodList 前几个 Pod | Ready -> Draining -> empty/timeout -> delete |
| Janitor | 只清 containerd task/container/snapshot/FIFO | Runtime/Network/BoxLite backend 插件化清理并二次校验 |
| 状态 | 一个 Phase 混合 runtime、用户进程、数据面、Fastlet | Conditions + 独立 observed state；Phase 仅兼容派生 |
| SDK | master 无 Python SDK；feature 分支 SDK 直连 FastPath Exec/File RPC | 生命周期 SDK + endpoint resolution + Execd/Envd Adapter |
| e2e | 36 个测试函数，部分测试明确验证端口互斥和旧 RuntimeClass | 迁移为多活、重复端口、代理、Infra、generation、Drain 测试 |

`feature/fastctl-exec` 不直接合并。只复用以下内容：

- fastctl `exec/cp/files` 的交互设计；
- Python SDK 的 Client/Sandbox/Files 对象模型；
- streaming、cancel、文件传输等测试场景；
- fast-helper artifact staging、原子写入、digest 和只读 mount 实现。

明确不复用：

- FastPath Exec/File RPC；
- Fastlet Control 5758 上的用户数据面接口；
- RuntimeDriver 公共 Exec/File API；
- FastPath 转发大文件、PTY、SSE 或 WebSocket 的实现。

## 3. 开发和验收工作流

### 3.1 本地与远端职责

```text
本地 macOS
  -> 阅读、设计、编辑、review、git 管理

fast Linux VM
  -> Go build/test
  -> containerd/CRI/runtime 验证
  -> Docker image build
  -> kind/Kubernetes e2e
  -> gVisor/Kata/KVM/BoxLite 验证
```

本地代码始终是编辑源。远端是验证镜像，不在远端直接长期维护另一份代码。

### 3.2 remote-dev-run 配置

第 0 阶段在仓库创建 git-ignored 的 `.remote-dev-run.env`：

```text
REMOTE_DEV_RUN_HOST=fast
REMOTE_DEV_RUN_REMOTE_DIR=~/fast-sandbox
REMOTE_DEV_RUN_SYNC_MODE=changed-files
REMOTE_DEV_RUN_PROTECT_REMOTE_DIRTY=true
REMOTE_DEV_RUN_INCLUDE_IGNORED=false
```

`.gitignore` 必须包含：

```text
.remote-dev-run.env
.remote-dev-run.local.env
```

配置文件不能提交。

### 3.3 每个开发批次的固定循环

1. 本地检查：

   ```bash
   git status --short --branch
   git diff --name-only
   ```

2. 远端检查：

   ```bash
   bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
     'git status --short --branch'
   ```

3. 只同步当前批次文件，不默认同步整个目录：

   ```bash
   bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/sync_changed_files.sh \
     path/to/code.go path/to/code_test.go
   ```

4. 如果远端目标文件因上一次同步已经 dirty：

   - 先检查远端 `git diff -- <目标文件>`；
   - 确认只有上一次本地镜像内容；
   - 再对明确文件使用 `--force`；
   - 出现任何未知远端修改立即停止，不覆盖。

5. 确认远端收到预期文件：

   ```bash
   bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
     'git status --short && git diff --name-only'
   ```

6. 先运行最小相关测试，再运行阶段 gate。

7. 每个阶段通过后形成一个可回滚 commit；下一阶段不建立在未通过 gate 的代码上。

### 3.4 远端环境基线

已确认：

- Ubuntu Linux x86_64；
- Docker `28.0.4`；
- kubectl `v1.35.0`；
- kind 已安装；
- host containerd/ctr 已安装；
- `/dev/kvm` 存在；
- 已存在 `fsb-e2e-basic` 和 `fsb-e2e-kata` 集群；
- `fsb-e2e-kata` 已有 `kata-qemu/kata-clh/kata-fc` RuntimeClass；
- 远端 `master` 与 `origin/master` 都是 `f92d8e3`，工作区干净；
- 远端原 PATH 中没有 Go；阶段 0 已将现有 Go 1.25.7/gofmt 以非覆盖符号链接加入 `/usr/local/bin`。

正式开发前必须安装或配置兼容 `toolchain go1.25.5` 的 Go 工具链，并补齐生成工具。当前远端复用已安装的 Go `1.25.7`：

```text
go 1.25.7
protoc
protoc-gen-go
protoc-gen-go-grpc
controller-gen
```

生成工具版本应通过 `tools.go`、Makefile 变量或版本文件固定，不能依赖开发机上的随机版本。

## 4. 总体里程碑

| 阶段 | 里程碑 | 主要交付 | 是否阻塞下一阶段 |
|---|---|---|---|
| 0 | 开发基线可复现 | remote-dev-run、Go 工具链、master 基线测试 | 是 |
| 1 | 契约和测试骨架冻结 | 文档一致、生成链、测试分层、兼容策略 | 是 |
| 2 | CRD/RPC API 基座 | runtime/resources/generation/request ID/route API | 是 |
| 3 | Runtime 与资源基座 | RuntimeCatalog、ResourceProfile、ContainerdDriver | 是 |
| 4 | Fastlet Core v2 | admission、reservation、Ensure、恢复、fencing | 是 |
| 5 | Registry/Scheduler v2 | Watch、Heartbeat、Top-K、cache/warmImages | 是 |
| 6 | 多活控制面 | 角色分离、Create/Controller 共用状态机、幂等 | 是 |
| 7 | Sandbox 私有网络 | NetworkManager、SlotPool、AccessDescriptor、NAT | 是 |
| 8 | 独立数据面代理 | Fastlet Proxy、Sandbox Proxy、鉴权、endpoint | 是 |
| 9 | Runtime Augmentation | InfraProfile、sandbox-init、execd/envd Adapter、SDK | 是 |
| 10 | Drain 与故障清理 | Pool Drain、PodLost、Janitor backend | 是 |
| 11 | Runtime 全矩阵 | gVisor、Kata、BoxLite | 是 |
| 12 | 性能、迁移与发布 | SLO、负载、文档、兼容清理、最终矩阵 | 最终 Gate |

## 5. 阶段 0：建立可复现开发基线

### 5.1 工作内容

1. 从当前 master 创建重构分支，建议：

   ```text
   feature/fast-sandbox-architecture-refactor
   ```

2. 先提交五份设计文档和本实施计划，形成不可变设计基线。
3. 增加 `.gitignore` 的 remote-dev-run 本地配置规则。
4. 创建本地 `.remote-dev-run.env`，指向 `fast:~/fast-sandbox`。
5. 在远端配置兼容 `toolchain go1.25.5` 的 Go 和代码生成工具。
6. 记录远端 Docker、kind、kubectl、KVM、gVisor、Kata、BoxLite capability。
7. 运行当前 master 的 unit 和现有 e2e，保存基线结果和已知失败。

### 5.2 基线验收

远端执行：

```bash
bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'go version && docker version --format "{{.Server.Version}}" && kubectl version --client && kind get clusters'

bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'make test'

bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'go test ./test/e2e/suites/basicvalidation/... ./test/e2e/suites/lifecycle/... ./test/e2e/suites/scheduling/... ./test/e2e/suites/cleanupjanitor/... -v -count=1 -timeout 30m'
```

secure runtime 基线：

```bash
bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'go test ./test/e2e/suites/secureruntime/... -v -count=1 -timeout 45m'
```

### 5.3 退出条件

- 远端 `go version` 不低于 `go1.25.5`，当前为 `go1.25.7`；
- `make test` 通过；
- basic/lifecycle/scheduling/cleanupjanitor 基线通过；
- gVisor/Kata/BoxLite 缺失能力被记录为环境缺口，不被静默跳过；
- 没有在远端遗留未知修改；
- 设计基线形成独立 commit。

## 6. 阶段 1：冻结公共契约和测试骨架

### 6.1 工作内容

1. 按跨模块决策修正四份专题文档中的旧表述：

   - RuntimeDriver 不再包含公共 Exec/File；
   - 底层 route key 使用 Sandbox UID + target port；
   - service name 只是 InfraProfile/SDK alias；
   - RPC 快速失败不遗留 CRD；
   - request ID 已确定；
   - `warmImages + 自然缓存` 已确定；
   - 新 Fastlet Ready 不等待 warm image 预热。

2. 给旧 README、ARCHITECTURE、Fast/Strong 文档标记 `Superseded` 或迁移期说明。
3. 建立生成命令：

   ```text
   make generate
   make manifests
   make verify-generated
   ```

4. 建立测试分层入口：

   ```text
   make test-unit
   make test-race
   make test-e2e-controlplane
   make test-e2e-network
   make test-e2e-runtime
   make verify
   ```

5. 建立 fake RuntimeDriver、fake Fastlet、fake clock、fake Heartbeat 和并发测试工具，避免后续只能依赖重型 e2e。
6. 固定兼容策略：

   - 旧 CRD runtime 字段保留有限读取窗口；
   - 新对象只写 `spec.runtime`；
   - 新旧字段同时出现时拒绝；
   - proto 旧 field number 不复用；
   - `exposed_ports` 和 `consistency_mode` 先 deprecated，服务端不再使用它们完成调度和一致性选择；
   - `endpoints` 先 deprecated，新的访问入口使用 ResolveEndpoint。

### 6.2 验收

```bash
bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'make generate && make manifests && make verify-generated && make test-unit'
```

退出条件：

- 重复运行生成命令不产生 diff；
- 文档不再对 RuntimeDriver Exec/File、service route 和 target-port route 给出冲突结论；
- 新测试 helper 可以在不启动 containerd 的情况下验证状态机；
- 当前功能行为仍保持不变。

## 7. 阶段 2：重构 CRD、RPC 和内部领域模型

### 7.1 SandboxPool API

新增目标字段：

```yaml
spec:
  runtime: container
  sandboxResources:
    cpu: "1"
    memory: 1Gi
  maxSandboxesPerPod: 5
  warmImages: []
  infraProfile: minimal
```

实现：

- canonical runtime enum：`container/gvisor/kata-qemu/kata-clh/kata-fc/boxlite`；
- `runtime` 和 `sandboxResources` 不可变；
- `runtimeType/runtimeClassName/containerdRuntimeHandler` deprecated；
- Pool 普通用户不能覆盖 handler、runtime path 或 config path；
- `infraProfile` 第一阶段引用内置受控 ProfileCatalog，不引入独立 InfraProfile CRD；
- `warmImages` 允许为空，不要求 Fastlet Ready 前完成。

### 7.2 Sandbox API

新增或规范：

```text
status.assignment.fastletName
status.assignment.fastletPodUID
status.assignment.nodeName
status.assignment.attempt
status.instanceGeneration
status.routeGeneration
status.runtimeState
status.dataPlaneState
status.userProcessState
status.conditions
```

兼容期保留但不再作为目标模型：

```text
status.phase
status.assignedFastlet
status.sandboxID
status.endpoints
spec.exposedPorts
```

Phase 由新状态派生，不能再作为状态机唯一事实。

### 7.3 FastPath proto

新增：

```text
CreateRequest.request_id
CreateResponse.sandbox_uid
ResolveEndpoint(sandbox_uid, target_port)
IssueRouteCredential(sandbox_uid, target_port)
```

request ID 语义：

- SDK/fastctl 自动生成；
- 支持调用方显式传入；
- 成功请求把 request ID 和 immutable create spec hash 存入 CRD annotation；
- idempotency 第一阶段与 Sandbox CRD 生命周期一致；
- 同 request ID + 同 hash 返回原 Sandbox；
- 同 request ID + 不同 hash 返回 Conflict；
- Sandbox CRD 删除后不保留全局 tombstone。

### 7.4 状态写入责任

固定 writer ownership：

| 数据 | Writer |
|---|---|
| Sandbox Spec/删除意图 | 用户、fastctl/SDK、FastPath lifecycle API |
| request ID/create hash | FastPath 创建时写 metadata |
| assignment | FastPath 或 SandboxController，通过 resourceVersion CAS |
| runtime/data/user Conditions | SandboxController，根据 Fastlet observed state 写入 |
| Pool status | SandboxPoolController |
| Local RouteStore | Fastlet Control 写入 Fastlet Proxy，不进 CRD |

所有 status 更新使用小范围 Patch/CAS helper，禁止读取后覆盖整个 status 导致不同 writer 丢字段。

### 7.5 验收

单元测试：

- runtime 默认值、enum 和不可变校验；
- Pool 新旧字段冲突；
- old-to-new runtime 映射；
- generation 默认值和递增 helper；
- assignment CAS 并发冲突；
- request ID/spec hash；
- proto wire compatibility 和旧 field number 未复用；
- CRD schema validation。

远端：

```bash
bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'make generate && make manifests && make verify-generated && go test ./api/... ./internal/controller/common/... ./internal/controller/fastpath/...'

bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'go test ./test/e2e/suites/basicvalidation/... -run "TestSandboxCRDValidation|TestSandboxPool" -v -count=1 -timeout 20m'
```

退出条件：新 API 可用，旧对象能按兼容规则读取，尚未改造的 runtime 路径仍可通过适配层工作。

## 8. 阶段 3：建立 RuntimeCatalog 和 ResourceProfile

### 8.1 共享 RuntimeCatalog

新增共享包，Controller 和 Fastlet 使用同一事实源：

```text
RuntimeName
RuntimeProfile
RuntimeDriverKind
ContainerdRuntimeConfig
BoxLiteRuntimeConfig
DeploymentRequirements
RuntimeCapabilities
RuntimeNetworkMode
InfraDeliveryModes
ProfileVersion/ProfileHash
```

内置六个 profile。`boxlite` 可以先注册为 profile，但在 Driver 完成前 capability 必须返回 Unsupported，不能错误 Ready。

### 8.2 SandboxPool Controller

- 从 `spec.runtime` 解析 RuntimeProfile；
- 根据 DeploymentRequirements 合成 Fastlet Pod；
- 只注入 `FAST_SANDBOX_RUNTIME=<canonical-name>`；
- 不再注入用户可控 handler/path/config；
- 不把 Sandbox runtimeClassName 设置到 Fastlet Pod；
- 注入 ResourceProfile 和 profile hash；
- 计算 Fastlet Pod requests：overhead + slot × sandbox resources；
- 实际 RuntimeReady 最终以 Fastlet heartbeat capability 为准。

### 8.3 RuntimeDriver 边界

目标接口只承载平台生命周期和内部诊断：

```text
Initialize
ProbeCapabilities
Ensure
Inspect
Delete
ListManagedSandboxes
GetAccessDescriptor
ListCacheInventory
Pull/WarmArtifact
RuntimeDiagnostics
Close
```

不加入公共 Exec/File/PTY/Session API。runtime-native exec 仅作为内部 bootstrap 能力放在私有 adapter。

### 8.4 资源执行

- Pool ResourceProfile 随 Ensure 请求传给 Fastlet；
- Fastlet 校验 profile hash；
- ContainerdDriver 把 CPU、memory、pids 写入 OCI/cgroup spec；
- Kata profile 把资源编译为 Kata VM/guest 参数；
- BoxLite 后续写入 BoxOptions；
- 控制面不直接操作单 Sandbox cgroup。

### 8.5 验收

```bash
bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'go test ./internal/runtimecatalog/... ./internal/controller/... ./internal/fastlet/runtime/... -count=1'

bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'go test ./test/e2e/suites/secureruntime/... -run "TestRuntimeValidation" -v -count=1 -timeout 30m'
```

新增远端 e2e 必须验证：

- Fastlet Pod 没有设置 Sandbox RuntimeClass；
- Pool runtime 修改被拒绝；
- unknown runtime fail closed；
- 缺少 KVM/shim/config 时 RuntimeReady=False；
- container Sandbox 的实际 cgroup CPU/memory 与 Pool Profile 一致；
- 单次 Create 不能覆盖 Pool 资源。

## 9. 阶段 4：Fastlet Core v2——Admission、Ensure、Fencing 和恢复

### 9.1 Fastlet Control 协议

替换当前 create/delete/status 的松散语义，建立版本化内部协议：

```text
ReserveSandbox
CancelReservation
EnsureSandbox
InspectSandbox
DeleteSandbox
Heartbeat
RuntimeDiagnostics
SetDraining
```

请求身份至少包含：

```text
request ID / reservation token
Sandbox UID
instanceGeneration
assignmentAttempt
assigned Fastlet Pod UID
runtime profile hash
resource profile hash
```

### 9.2 RPC 快速失败的 reservation

为了保证“无容量快速失败不创建 CRD”，实现短期 reservation：

```text
FastPath -> Fastlet Reserve(request ID)
  no capacity -> reject, no CRD
  accepted -> expiring reservation token

FastPath -> create CRD + assignment CAS
FastPath -> Fastlet Ensure(CRD UID, reservation token)
```

要求：

- reservation + creating + running 不超过 maxSandboxesPerPod；
- reservation 有短 TTL；
- FastPath 在 CRD 提交失败时主动 Cancel；
- FastPath 在 reservation 后崩溃时由 TTL 自动释放；
- Controller 声明式路径可以直接 Ensure，容量不足时保持 Pending 并重新调度；
- reservation 不创建 runtime，不需要跨 Fastlet 进程重启恢复。

### 9.3 原子 Ensure

Fastlet 内部状态机：

```text
Reserved -> Creating -> Running -> Draining/Deleting -> Removed
```

同身份 tuple：

- Running：幂等成功；
- Creating：加入同一个结果或返回 InProgress；
- 不存在：原子占位后创建；
- 超容量：CapacityRejected；
- 旧 Pod UID/generation/attempt：StaleAssignment；
- 相同 UID 但 claim/profile 冲突：Conflict。

明确错误分类：

```text
CapacityRejected
Draining
InProgress
Conflict
StaleGeneration
RuntimeUnavailable
NetworkUnavailable
InfraUnavailable
UnknownOutcome
```

### 9.4 Fastlet 进程恢复

Fastlet Container 重启、Pod UID 不变时：

```text
RuntimeDriver.ListManagedSandboxes
  -> verify owner Pod UID
  -> recover runtime metadata and generations
  -> recover capacity counters
  -> recover network/access descriptor when available
  -> recover route metadata when available
  -> capability probe
  -> Ready
```

Ready probe 不再只是 HTTP 端口存活。

### 9.5 验收

单元/并发测试：

```bash
bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'go test -race ./internal/fastlet/... ./internal/api/... -count=1'
```

必须覆盖：

- 100 个并发请求争夺 5 个 slot，任何时刻 reservation+creating+running <= 5；
- 重复 Ensure 只创建一个 runtime；
- runtime 创建失败释放占位；
- reservation 超时释放；
- 旧 generation/Pod UID 请求拒绝；
- Delete 重复调用幂等；
- Fastlet Container 重启后在同 Pod UID 内恢复 running 数量；
- 恢复完成前 readiness=False。

远端 containerd e2e：创建 Sandbox，重启 Fastlet container 而不改变 Pod UID，确认 Sandbox、capacity 和 route metadata 恢复。

## 10. 阶段 5：Local Registry、Heartbeat、Top-K 和镜像缓存

### 10.1 Registry 数据源

拆分为：

```text
Pool Watch Store
Fastlet Pod Watch Store
Sandbox Assignment Watch Store
Heartbeat Store
Local Feedback Store
Candidate/TopK Engine
```

删除：

- Registry 全局占 slot 的语义；
- `UsedPorts`；
- `Allocate()` 内的 `Allocated++`；
- 端口冲突检测；
- 2 秒高频全量串行扫描。

### 10.2 Heartbeat

Heartbeat 至少返回：

```text
Pod UID / sequence / observed timestamp
Ready / Draining
runtime/profile/resource hash
capacity max/reserved/creating/running
cacheRevision
changed cache inventory
prepared Infra artifacts
managed runtime summary
```

第一阶段：

- 10～30 秒可配置周期；
- jitter；
- 有限并发；
- stale threshold；
- Watch 负责成员增删，Heartbeat 负责 live/cache 状态；
- 每个 FastPath 副本维护自己的视图。

### 10.3 Top-K

顺序：

```text
hard filter
  -> Pool/runtime/profile/Ready/!Draining/!stale/capacity hint
cache affinity
  -> exact digest when already available
  -> normalized image reference
load
stable hash perturbation
```

RPC 在 CRD UID 产生前使用 request ID 做稳定扰动；声明式路径使用 Sandbox UID。

明确拒绝才换下一个候选；UnknownOutcome 保留 assignment 并查询原 Fastlet。

### 10.4 warmImages 和 GC

- Pool `warmImages` 与自然请求缓存并存；
- Create 热路径不主动解析 image digest；
- Fastlet Ready 后异步预热 warmImages；
- Heartbeat 只上报实际已缓存内容；
- GC 保护使用中、Pool warm、Infra artifact 和热点镜像；
- cacheRevision 在 pull/unpack/GC 后递增；
- 私有镜像凭证暂不实现。

### 10.5 验收

```bash
bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'go test -race ./internal/controller/registry/... ./internal/controller/scheduler/... ./internal/fastlet/cache/... -count=1'
```

必须验证：

- Registry 过期不能突破 Fastlet capacity；
- 不再存在 port conflict；
- image hit 优先于相同条件的 cache miss；
- stale/draining Fastlet 永不进入 Top-K；
- Heartbeat unchanged revision 不返回完整 inventory；
- 新 Fastlet 在 warmImages 未完成时仍可 Ready；
- GC 不删除保护内容；
- 多个 Registry 最终收敛但不要求瞬时一致。

## 11. 阶段 6：控制面角色分离和多活 Create 状态机

### 11.1 进程角色

同一控制面镜像支持：

```text
--role=fastpath
--role=controller
--role=all
```

- `fastpath`：无 Leader Election，启动 FastPath API、Watch Cache、Registry/Heartbeat；
- `controller`：启用 Leader Election，只启动 Sandbox/Pool Reconcile；
- `all`：开发/兼容模式，同时启动两类能力；
- 生产 FastPath Service selector 只选 `role=fastpath`；
- RBAC、Service、Deployment、PDB、HPA 分开。

### 11.2 统一 Ensure Orchestrator

FastPath 和 SandboxController 共享同一应用层状态机：

```text
EnsureIdentity
EnsureAssignmentCAS
EnsureRuntime
ObserveRuntime
EnsureDataPlane
PatchConditions
```

不复制两套创建逻辑。

### 11.3 RPC Create

完整流程：

```text
validate request/request ID
  -> lookup idempotent result
  -> Local Registry Top-K
  -> Fastlet reservation
  -> no candidate: return fast failure, no CRD
  -> create Sandbox CRD with request metadata
  -> assignment CAS with Pod UID/attempt
  -> EnsureSandbox using CRD UID
  -> wait until DataPlaneReady
  -> return access identity
```

如果 CRD 已提交后 FastPath 宕机，Controller 从 CRD 继续；客户端使用同 request ID 重试并挂回原 Sandbox。

删除旧 Fast/Strong mode。旧 proto `consistency_mode` 只做兼容解析，不再选择不同持久化顺序。

### 11.4 声明式创建和生命周期

- 直接创建 CRD：无容量时 Pending，PoolController 可以扩容；
- Delete：只提交 CRD deletion，由 Finalizer Reconcile；
- Reset：更新 reset revision，Controller 递增 instance/route generation；
- expireTime/failurePolicy：只修改 Spec；
- Fastlet PodLost：按 policy 标记或重建，不接管旧 runtime；
- Controller 和 FastPath 同时 Ensure 由 CRD CAS + Fastlet idempotency 吸收。

### 11.5 验收

新增 control-plane e2e：

1. 3 个 FastPath 副本 + 2 个 Controller 副本，只有 1 个 Controller Leader；
2. 连续删除/重建 Controller Leader，FastPath Create 仍可用；
3. 不部署 FastPath，直接创建 CRD 能完成 Sandbox；
4. `role=all` 保持开发兼容；
5. 100 个并发 Create 不超过 Fastlet 总容量；
6. 相同 request ID + 相同 spec 返回同 UID；
7. 相同 request ID + 不同 spec 返回 Conflict；
8. 当前无容量时 RPC 返回失败且集群中没有对应 CRD；
9. CRD 声明式创建在无容量时 Pending，扩容后 Running；
10. FastPath 在 CRD commit 后故障，Controller 完成创建；
11. Fastlet response lost 不导致第二个 runtime；
12. Delete/Reset/ExpireTime 返回的是期望状态已提交，不假装底层已同步完成。

远端 gate：

```bash
bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'make test-unit && make test-e2e-controlplane'
```

该阶段完成后形成第一个可独立交付里程碑：多活控制面重构完成，暂时仍可使用兼容网络/数据面适配层。

## 12. 阶段 7：Sandbox 私有网络和 AccessHandle

### 12.1 网络抽象

新增：

```text
NetworkDriver
NetworkManager
NetworkSlotPool
IPAM
EgressManager
AccessDescriptor
AccessHandle factory
NetworkStateStore
```

`AccessHandle` 是进程内 dial 抽象；Fastlet Control 与 Fastlet Proxy 分进程，因此通过 UDS 同步可恢复的本地 `AccessDescriptor`，Fastlet Proxy 再构造 AccessHandle。Descriptor 不进入 CRD。

### 12.2 LinuxNetnsDriver

为 container/runc 实现：

```text
Prepare clean slot
  -> netns
  -> veth pair
  -> bridge
  -> unique private IP in Fastlet Pod network namespace
  -> DNS/MTU/routes
  -> SNAT/MASQUERADE to Fastlet Pod eth0
```

Create：Acquire clean slot -> containerd task 使用 netns path -> 生成 DirectIP/NetNS descriptor。

Delete：先摘 route -> runtime delete -> destroy slot -> 异步补充，不复用脏 slot。

### 12.3 API 和调度清理

- `spec.exposedPorts` 不参与调度；
- `status.endpoints` 不再生成 `PodIP:port`；
- 多个 Sandbox 可以监听同一内部端口；
- 目标端口无需 Create 时声明；
- 第一阶段 egress allow，ingress deny，sandbox-to-sandbox deny；
- 完整 network policy 只保留扩展边界。

### 12.4 验收

远端 Linux 单元/集成测试：

- slot 并发 acquire/release；
- 使用过的 slot 销毁而非回池；
- state file 可恢复；
- orphan veth/netns/rule 可识别；
- 删除中途失败可重试。

e2e：

1. 同一 Fastlet 创建两个 Sandbox，二者都监听 8080；
2. 两个 Sandbox 拥有不同私有 IP；
3. 两者都能 DNS 和出网，外部观察源为 Fastlet Pod 网络；
4. Sandbox 之间默认不可直连；
5. 删除一个 Sandbox 不影响另一个；
6. Fastlet Container 重启恢复 NetworkState；
7. Pod 删除后 Janitor 能识别 orphan 网络资源；
8. 创建 warm slot hit/miss 指标正确。

```bash
bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'make test-e2e-network'
```

## 13. 阶段 8：Fastlet Proxy、Sandbox Proxy 和访问解析

### 13.1 Fastlet Proxy

新增独立二进制和 sidecar：

```text
cmd/fastlet-proxy
internal/fastletproxy
port 5780
```

职责：

- UDS 接收 ApplyRoute/DeleteRoute/MarkDraining/SnapshotRoutes/WatchRoutes；
- 本地 RouteStore key：Sandbox UID + routeGeneration；
- 请求携带 target port；
- 通过 AccessHandle 透明 dial；
- 校验 Pod UID、assignment attempt、route generation 和凭证；
- HTTP/SSE/WebSocket 流式转发；
- 不解析 execd/envd payload。

### 13.2 Sandbox Proxy

新增：

```text
cmd/sandbox-proxy
internal/sandboxproxy
Deployment fast-sandbox-proxy
Service fast-sandbox-proxy
```

职责：

- Watch Sandbox assignment、DataPlaneReady 和 Fastlet Pod；
- 路由 Sandbox UID + target port 到 assigned Fastlet Proxy；
- cache miss/lag 时进行一次权威补查；
- 临时未收敛返回 retryable，不返回永久 NotFound；
- 支持 request cancellation、backpressure、streaming、WebSocket、SSE；
- 不参与 lifecycle 或 scheduling。

### 13.3 Endpoint 和鉴权

FastPath 提供：

```text
ResolveEndpoint(Sandbox UID, target port)
IssueRouteCredential(Sandbox UID, target port)
```

鉴权实现通过接口隔离。第一阶段 route credential 至少绑定：

```text
tenant/namespace
Sandbox UID
target port
Fastlet Pod UID
assignment attempt
route generation
expiration
```

生产部署 fail closed。开发 bypass 只能通过显式非默认 flag 使用。凭证不写入 Sandbox Status。

### 13.4 验收

代理单元/集成测试：

- route apply/delete/generation fencing；
- stale token/stale assignment 拒绝；
- target port 校验；
- cache miss fallback；
- HTTP keep-alive、chunked、大文件、SSE、WebSocket、cancel、backpressure；
- Header allowlist/denylist；
- Fastlet Proxy route 是最后权威。

e2e：

1. 通过 Sandbox Proxy 分别访问两个同端口 Sandbox；
2. Create 返回后立即访问，Watch 落后时 SDK 有界重试成功；
3. 任意 Sandbox 内端口无需预声明即可解析；
4. 删除/reset 后旧 token、旧 route 不能访问新实例；
5. 断开一个 Sandbox Proxy 副本不影响服务；
6. 请求永远不会被 Kubernetes Service 随机转到非 assigned Fastlet；
7. Fastlet Control 5758 不承载用户代理流量。

```bash
bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'go test -race ./internal/fastletproxy/... ./internal/sandboxproxy/... && make test-e2e-proxy'
```

## 14. 阶段 9：Infra Component Runtime Augmentation 和 SDK Adapter

### 14.1 InfraProfile 和 Artifact Store

第一阶段内置：

```text
minimal
opensandbox-execd
e2b-envd
test-infra（仅 e2e fixture）
```

实现：

- ProfileCatalog；
- content-addressed Artifact Store；
- OCI/static/preinstalled artifact resolver；
- digest/signature hook；
- Runtime Injector；
- Infra Lifecycle Manager；
- readiness/service alias；
- prepared artifact capability/heartbeat；
- GC protection。

Profile artifact 准备异步进行。Fastlet Pod Ready 不等待普通 warm image；调度只在 required Infra artifact 已 Prepared 时把该 Fastlet 视为目标 Profile 的可接纳候选。RPC 未准备好时快速失败，声明式 CRD 路径等待准备完成。

### 14.2 sandbox-init

为普通 container runtime 实现最小 supervisor：

- 恢复用户原始 entrypoint/args/env/cwd/user；
- 启动和监督 Infra Component；
- 默认 Infra 与用户进程并行；
- `startBeforeUser=true` 时串行；
- 信号转发和子进程回收；
- 保留用户退出码；
- component restart/readiness 状态进入 diagnostics；
- sandbox-init 本身不实现 Exec/File。

### 14.3 OpenSandbox 和 E2B 适配

OpenSandbox execd：

```text
OCI artifact extraction
  -> read-only bundle mount
  -> ComponentBootstrap
  -> per-instance token/config
  -> /ping
  -> alias execd=44772
```

E2B envd：

```text
TemplateBake/Preinstalled where supported
  -> SystemService
  -> per-instance /init
  -> /health
  -> alias envd=49983
```

不要求普通 container profile 第一阶段完整模拟 E2B VM/systemd 环境；不支持的 runtime/profile 组合必须 capability fail closed。

### 14.4 Create 成功条件

这一阶段将 Create 的最终成功 gate 切换为：

```text
RuntimeReady
required Infra InstanceInit succeeded
required Infra readiness passed
Fastlet local route published
DataPlaneReady=True
```

不等待所有 Sandbox Proxy Watch 收敛。

### 14.5 fastctl 和 Python SDK

从 `feature/fastctl-exec` 迁移用户模型，但重写传输层：

```text
LifecycleClient -> FastPath RPC
EndpointResolver -> ResolveEndpoint
RouteCredentialProvider -> IssueRouteCredential
ExecdAdapter / EnvdAdapter -> Sandbox Proxy
Sandbox.files / Sandbox.exec -> selected protocol Adapter
```

fastctl：

- `run` 自动生成 request ID，支持显式 `--request-id`；
- `exec/cp/files` 使用 Adapter；
- `logs` 明确区分 runtime diagnostics 与 Infra command logs；
- 不调用 FastPath Exec/File RPC。

### 14.6 验收

- artifact cache hit 时 Create 不执行 registry 下载；
- digest 不匹配拒绝；
- 同 bundle 多 Sandbox 只读复用；
- 用户 entrypoint 和 Infra 默认并行；
- required Infra 失败导致 DataPlaneReady=False；
- optional Infra 失败不阻塞 Create；
- reset 后重新 InstanceInit，旧 token 无效；
- fastctl/Python SDK exec、files、SSE/PTY 通过 ExecdAdapter；
- FastPath/Fastlet Control 抓包或 handler 列表中不存在公共 Exec/File API。

远端：

```bash
bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'go test -race ./internal/fastlet/infra/... ./internal/sandboxinit/... ./cmd/fastctl/... && make test-e2e-infra && make test-e2e-sdk'
```

## 15. 阶段 10：Drain、PodLost 和 Janitor 扩展

### 15.1 Pool Drain

PoolController 缩容和 rollout：

```text
select drain candidate
  -> mark Draining
  -> Registry excludes candidate
  -> Fastlet rejects new reservation/admission
  -> existing Sandbox continues
  -> wait empty or timeout policy
  -> remove Fastlet Pod
```

第一阶段不迁移运行中 Sandbox。强制超时后的处理遵循 FailurePolicy。

### 15.2 PodLost

- Watch 到 Pod UID 消失/替换；
- 旧 runtime instance 标记 Lost；
- AutoRecreate 递增 instanceGeneration、assignmentAttempt、routeGeneration；
- 选择新 Fastlet 创建新实例；
- Manual 保持 Lost，等待用户动作；
- 新 Pod 永远不认领旧 Pod UID 资源。

### 15.3 Janitor backend

重构为：

```text
ContainerdJanitor
LinuxNetworkJanitor
BoxLiteJanitor
Route/State cleanup
```

每次清理前二次确认：

- Fastlet Pod UID 不存在或不再 owner；
- Sandbox CRD 不存在，或 assignment/generation 已不指向旧实例；
- runtime/network state label 匹配 Sandbox UID；
- 超过 orphan grace period。

正常 Create/Delete 不依赖 Janitor。

### 15.4 验收

1. 缩容不会直接删除承载 Sandbox 的 Fastlet；
2. Draining Fastlet 不接受新请求；
3. 空 Fastlet 优先被删除；
4. Pod 消失时 Manual/AutoRecreate 行为正确；
5. 新 Pod UID 不接管旧 runtime；
6. Janitor 清理 containerd、netns、veth、NAT、state；
7. assignment/generation 仍有效时 Janitor 拒绝误删；
8. Janitor 重复执行幂等；
9. Controller Leader 切换不破坏 Drain 状态。

```bash
bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'make test-e2e-drain && make test-e2e-faultrecovery && make test-e2e-cleanupjanitor'
```

## 16. 阶段 11：gVisor、Kata 和 BoxLite Runtime 全矩阵

### 16.1 container

作为 reference implementation 先通过全部生命周期、网络、Infra、代理、资源和恢复测试。

### 16.2 gVisor

验证：

- runsc/shim/config capability；
- LinuxNetnsDriver 兼容；
- DNS/egress；
- WebSocket/SSE/长连接；
- bundle mount/sandbox-init；
- CPU/memory；
- Fastlet restart recovery。

### 16.3 Kata

分别验证 `kata-qemu/kata-clh/kata-fc`：

- KVM、shim、hypervisor config、vsock capability；
- NetworkDriver/AccessDescriptor；
- guest artifact delivery；
- ResourceProfile；
- Infra readiness；
- runtime recovery和 Janitor。

`kata-fc` 当前环境曾出现 hybrid vsock 超时。完成标准不是“跳过测试”，而是：

- 在支持 Firecracker 的远端环境通过完整 e2e；或
- capability 明确报告 Unavailable，Pool RuntimeReady=False，产品不宣称该 profile 可用。

要声明本轮 `kata-fc` 支持完成，必须满足前一种条件。

### 16.4 BoxLite 原型 Gate

先进行 time-boxed spike，对比：

```text
Go SDK in-process
boxlite serve sidecar
```

评估项：

- CGO/native library 和 Fastlet image；
- 故障隔离；
- 一次 Runtime 多 Box；
- CRD UID identity；
- List/Inspect/Delete/Recover；
- `/dev/kvm` 和 cgroup；
- `BOXLITE_HOME` per Fastlet；
- image cache inventory；
- gvproxy/libslirp；
- 任意 target port 的动态 forwarding；
- warm/cold create latency。

原型结论记录为实现 ADR，然后在统一 `BoxLiteDriver` 边界下完成实现。不能把 BoxLite 伪装成 containerd handler。

### 16.5 BoxLite 验收

1. 一个长期 Runtime 管理多个 Box；
2. 每个 Sandbox UID 对应一个 Box；
3. 两个 Box 同时监听相同 guest port；
4. Fastlet Proxy 使用 LocalForward/等价 AccessHandle；
5. 任意 target port 可以动态访问；
6. CPU/memory 由 Pool Profile 落实；
7. Fastlet Container 重启同 Pod UID 内恢复；
8. Fastlet Pod 丢失后不由新 Pod 接管；
9. BoxLiteJanitor 清理 Box/shim/state/network；
10. `BOXLITE_HOME` 不在不同 Fastlet Pod 间共享写；
11. BoxLite image cache 进入 Heartbeat 和 Top-K；
12. InfraProfile 能通过 TemplateBake/Preinstalled/GuestCopy 中至少一种方式工作。

远端 runtime gate：

```bash
bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'make test-e2e-runtime-container && make test-e2e-runtime-gvisor && make test-e2e-runtime-kata && make test-e2e-runtime-boxlite'
```

## 17. 阶段 12：可观测性、性能、迁移和最终发布

### 17.1 Metrics 和 tracing

至少增加：

```text
create_accepted_latency
user_process_start_latency
data_plane_ready_latency
fastlet_admission_total{result,reason}
reservation_inflight
registry_heartbeat_age
registry_candidate_count
topk_retry_count
image_affinity_result
cache_revision/cache_gc
network_slot_available/inuse
runtime_create_latency{runtime,cache_hit}
infra_ready_latency{profile,component,runtime}
sandbox_proxy_route_latency/result
fastlet_proxy_upstream_latency/result
janitor_cleanup_total{backend,reason}
```

request ID、Sandbox UID、generation、assignment attempt 和 route generation 贯穿日志和 trace。

### 17.2 性能验收

分别报告：

- warm/cold；
- image hit/miss；
- runtime；
- InfraProfile；
- NetworkSlot hit/miss；
- FastPath/Controller path。

必须证明：

- warm container `CreateAcceptedLatency` 没有明显回退；
- Registry/Heartbeat 不随 FastPath 副本数产生高频风暴；
- 多活并发下无 slot 超卖、无重复 runtime；
- image hit 调度可观察且确实优先；
- Proxy streaming 不整包缓冲；
- Controller Leader 切换期间 FastPath 可用；
- Sandbox Proxy 单副本故障不影响已有路由的整体可用性。

旧 `<50ms` 只作为特定 warm container profile 的观测目标，不强加给 Kata/BoxLite/DataPlaneReady。

### 17.3 兼容和清理

- 新 samples 只使用 `spec.runtime/sandboxResources/warmImages/infraProfile`；
- 删除运行代码中的端口冲突和 `UsedPorts`；
- 停止生成 PodIP:port endpoints；
- 删除 Fast/Strong 分支实现；
- 删除公共 FastPath/Fastlet Exec/File API；
- 旧字段仅保留明确的兼容读取和 deprecation metrics；
- 提供 Pool manifest 转换/检查工具；
- 更新 README、ARCHITECTURE、部署清单、运维手册和故障诊断；
- 更新 Helm/kustomize/RBAC/PDB/HPA/NetworkPolicy 样例。

### 17.4 最终远端验收矩阵

```bash
bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'make verify'

bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'make test-race'

bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'make test-e2e-controlplane'

bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'make test-e2e-network && make test-e2e-proxy && make test-e2e-infra && make test-e2e-sdk'

bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'make test-e2e-runtime-container && make test-e2e-runtime-gvisor && make test-e2e-runtime-kata && make test-e2e-runtime-boxlite'

bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'make test-e2e-drain && make test-e2e-faultrecovery && make test-e2e-cleanupjanitor'
```

最终输出一份验收报告，记录：

- exact command；
- exit status；
- commit SHA；
- cluster/profile；
- runtime capability；
- 关键指标；
- 已知限制；
- 未执行/跳过项及原因。

任何 runtime 不能以“skip”作为支持完成的证据。

## 18. 推荐的代码提交/PR 切分

不要用一个超大提交完成重构。推荐顺序：

1. `docs/tooling: freeze architecture and remote validation workflow`
2. `api: add runtime resources generations request-id and route contracts`
3. `runtime: introduce shared runtime catalog and resource profiles`
4. `fastlet: add reservation atomic admission ensure and recovery`
5. `scheduler: replace registry allocation with watch heartbeat and top-k`
6. `controlplane: split roles and implement multi-active create semantics`
7. `network: add private network manager slot pool and access descriptors`
8. `proxy: add fastlet proxy sandbox proxy and endpoint resolution`
9. `infra: add runtime augmentation sandbox-init and built-in profiles`
10. `sdk: migrate fastctl and Python SDK to protocol adapters`
11. `lifecycle: add drain pod-lost fencing and janitor backends`
12. `runtime: complete gvisor kata and boxlite adapters`
13. `observability: add SLO metrics load tests migration and docs`

每个提交必须：

- 编译；
- 通过相关 unit/race tests；
- 保持兼容 gate 或显式完成迁移；
- 有独立回滚价值；
- 不夹带与本阶段无关的格式化或重命名。

## 19. 风险和控制点

### 19.1 最大技术风险

1. FastPath reservation、CRD commit 和 Fastlet Ensure 跨三个状态源；
2. Fastlet 进程重启后的 runtime/network/route 恢复；
3. host containerd 使用 Fastlet 创建的 netns；
4. Kata 各 hypervisor 的网络和 Infra delivery 差异；
5. BoxLite 动态 forwarding、CGO 和状态恢复；
6. 多 writer 更新 Sandbox status；
7. 代理长连接在 reset/drain/generation 变化时的语义；
8. Infra readiness 对 Create 长尾的影响。

### 19.2 控制策略

- reservation 和 generation 先用 fake runtime 做模型/并发测试，再接 containerd；
- 网络先只完成 container reference path，再扩展 secure runtime；
- BoxLite 先 spike 再选 SDK/sidecar；
- 所有 status writer 经过统一 patch helper；
- 每个阶段保留兼容 adapter，下一层稳定后再移除旧路径；
- 长连接、proxy、runtime 失败都用故障注入 e2e；
- 远端环境能力缺失时 fail closed，不通过 skip 伪装成功；
- snapshot/pause/resume/storage/GPU 等明确不进入本计划。

## 20. 最终完成定义

只有同时满足以下条件，才能宣布本轮开发重构完成：

- 五份设计文档和代码实现无架构冲突；
- 所有 20 条跨模块核心不变量有自动化测试；
- FastPath 多活、Controller 单活和 Controller-only 三种部署都通过 e2e；
- request ID 幂等和 RPC 快速失败无 CRD 通过并发/故障测试；
- Fastlet admission、恢复和 generation fencing 通过 race/e2e；
- Pool runtime/resources/warmImages 实际生效；
- container 私有网络、任意同端口、NAT 和代理链路通过；
- required Infra Component 成为 DataPlaneReady gate；
- fastctl/Python SDK 通过 Adapter 完成 exec/files 用户路径；
- container、gVisor、Kata、BoxLite 的宣称支持项都有远端证据；
- Drain 和 Janitor 不误删、不漏删，并且不进入正常热路径；
- 完整 remote verification matrix 全绿；
- 性能报告、迁移指南、部署清单和运维诊断文档齐全；
- 代码中不再存在被目标架构禁止的 FastPath Exec/File、端口冲突调度和 runtime-before-CRD Fast 模式。
