# Fast Sandbox 架构重构开发计划

**日期**：2026-07-19  
**状态**：开发分支验收完成（Kata Firecracker、BoxLite 生产能力仍 fail closed；目标集群性能与多节点镜像缓存报告是发布前 Gate）
**代码基线**：`master@f92d8e34288365be227d2ee8a6f952687dc7be00`  
**本地仓库**：`/Users/fengjianhui/WorkSpaceL/fast-sandbox`  
**远端开发机**：SSH alias `fast`  
**远端仓库**：`~/fast-sandbox`

> **2026-07-21 收口说明：** 项目尚未生产部署，计划早期记录的 deprecated 字段、兼容 adapter、迁移 CLI 和投影状态均已被直接切换决策取代并从代码删除。本文保留阶段性执行记录；当前契约以五份设计文档、`ARCHITECTURE.md` 和现行 CRD/protobuf 为准。

> **2026-07-23 Create 语义修订：** 本文中“Create 等待 required Infra/DataPlaneReady”的历史执行记录已由 [CreateSandbox RuntimeReady 快速返回语义](../specs/2026-07-23-runtime-ready-create-semantics.md) 替代。

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

### 8.6 实施结果（2026-07-19）

- Controller/Fastlet 共用六个 canonical RuntimeProfile；BoxLite 已注册但明确 Unsupported；
- Fastlet Pod 不设置 Sandbox RuntimeClass，只接收 canonical runtime/profile/resource 输入；
- RuntimeDriver factory 在实际节点检查 socket、config、binary 和 KVM，初始化后才报告 Ready；
- RuntimeDriver 公共边界不再包含 Exec/File，日志和 artifact cache 仅保留为迁移期私有可选接口；
- ContainerdDriver 将 CPU、memory、PIDs 编译到 OCI spec，并通过 Ensure 提供 runtime identity 幂等入口；
- Fastlet 启动及每次 Ensure 严格校验完整 runtime/resource profile hash 和 CPU/memory/PIDs，单请求不能覆盖 Pool profile；
- 远端 unit gate：`make test-unit`，退出状态 `0`；
- 远端 runtime e2e：BoxLite `RuntimeReady=False/RuntimeUnsupported`；container Fastlet Pod 无 RuntimeClass；
- 远端实际 cgroup v2：`250m / 256Mi / 128 PIDs` 分别得到 `cpu.max=25000 100000`、`memory.max=268435456`、`pids.max=128`；
- protobuf、DeepCopy 与两份 CRD manifest 连续生成前后 SHA-256 不变，并与本地分支完全一致。

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

### 9.6 实施结果（2026-07-19）

- Fastlet Control v2 已提供 `ReserveSandbox/CancelReservation/EnsureSandbox/InspectSandbox/DeleteSandbox/Heartbeat/RuntimeDiagnostics/SetDraining`，v1 create/delete/status 作为迁移适配层保留；
- reservation 原子绑定 request ID、创建参数摘要、完整 runtime/resource profile hash 和目标 Fastlet Pod UID，支持幂等获取、主动取消和 TTL 回收；
- `reservation + creating + running + deleting` 统一计入本地权威 capacity，Fastlet 在 runtime 调用前原子占位，失败释放，100 并发争夺 5 slot 无超卖；
- Ensure 使用 Sandbox UID、instanceGeneration、assignmentAttempt、Fastlet Pod UID 做 fencing；重复 Ensure 只创建一次 runtime，旧 generation/Pod UID 和 claim/profile 冲突均返回结构化错误；
- 删除优先于进行中的创建；延迟删除只允许移除原 manager entry，不能误删同 UID 的后续实例；
- Fastlet 启动先进入 `Recovering/NotReady`，按本 Pod UID 从 RuntimeDriver 恢复 managed runtime 和 generation/capacity，capability Ready 后才开放 admission；Draining 会关闭 readiness，但不伪装成 runtime 故障；
- PoolController 强制注入平台控制的 `FASTLET_CONTROL_PORT=:5758` 和 HTTP `/readyz` probe，用户 template 不能覆盖，Kubernetes Pod Ready 因而与 recovery/admission readiness 对齐；
- containerd managed runtime label 已持久化 request ID、generation、assignment attempt、Pod UID 和完整 profile/resource identity；
- 远端 unit gate：`make test-unit`，退出状态 `0`；
- 远端 race gate：`go test -race ./internal/fastlet/runtime ./internal/fastlet/server ./internal/api -count=1`，退出状态 `0`；
- 远端 containerd e2e：Fastlet Pod UID `00a702f8-d1e7-455b-bfbd-edddc9fd5345` 保持不变，container restart count 从 `0` 变为 `1`；恢复后 `runtimeReady=true`、`recovering=false`、`running=1`、`used=1`，Sandbox CRD 仍为 Running 且 runtime identity 未变化；
- 当前阶段尚无 NetworkManager/RouteStore，因此 e2e 只验证 runtime/capacity 恢复；route metadata 恢复在阶段 7 实现后纳入同一故障测试。

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

### 10.6 实施结果（2026-07-19）

- Fastlet Pod 成员关系改由 controller-runtime 共享 Pod informer 驱动；删除事件使用 Pod UID fencing，旧 Pod 的迟到事件不能移除同名新实例；
- Heartbeat 默认周期 20 秒、带 0.8～1.2 jitter、并发上限 8、单请求超时 5 秒，均可通过控制面参数配置；不再每 2 秒全量 List Pod 并串行探测；
- 每个 Registry 维护本地、最终一致的 Pod/Heartbeat/cache/feedback 视图；Watch 决定成员身份，过期 Heartbeat 只使候选 fail closed，不删除 Watch 成员；
- Registry 删除 `UsedPorts`、端口冲突和本地 `Allocated++/--`；兼容 `Allocate` 只选候选，`Release` 不再拥有容量，Fastlet admission 是唯一 slot 裁决者；
- Top-K 依次执行 Pool/runtime/profile/PodReady/runtimeReady/Draining/stale/capacity hard filter，再按 normalized image hit、负载比例和 request ID/Sandbox UID 稳定扰动排序；Fastlet 明确拒绝会进入短期本地反馈抑制；
- Heartbeat 增加 Pod UID、sequence、admission、runtime/resource profile、cache epoch/revision/full/complete；同游标且清单不变时不返回 inventory，revision gap 或清单超限时关闭镜像亲和而不影响 capacity 正确性；
- Pool `warmImages` 由 Controller 作为平台配置注入；Fastlet recovery/Ready 完成后并发度 2 异步 pull，不阻塞 Ready；PoolWarm、ActiveSandbox、InfraArtifact、HotImage 均进入 runtime-neutral 保护索引；
- warmImages 增加有界 Prometheus counter `fast_sandbox_warm_image_pull_total{result}`；真实远端 Gate 使用 kind 节点已加载的 `alpine:latest`，同时断言 Fastlet heartbeat 完整 cache inventory 与 success counter，证明调用了实际 RuntimeArtifactCache，而不依赖外部 registry 网络；
- containerd `k8s.io` image store 是节点共享事实，阶段 5 不执行 Fastlet 私有破坏性 GC；实际删除等待 node-scoped coordinator/lease 证明，详见 REF-0011；
- 删除 runtime 失败时 Fastlet 保留 `delete-failed` entry、capacity 与 active-image 保护，允许幂等重试，避免资源仍存活但 slot/缓存保护提前释放；
- Pool CRD 对 `warmImages` 增加最多 128 项约束，Controller 平台路径线性去重；三份 sample 均符合固定 SandboxResourceProfile 约束（Kubernetes 1.27 的 `uniqueItems` 限制见 REF-0012）；
- 远端 unit gate：`make test-unit`，退出状态 `0`；
- 远端 race gate：`go test -race ./internal/controller/fastletpool ./internal/controller/fastletcontrol ./internal/fastlet/cache ./internal/fastlet/runtime ./internal/fastlet/server ./internal/api -count=1`，退出状态 `0`。
- 2026-07-20 最终快照定向远端 Gate：`E2E_TEST_TIMEOUT=35m DOCKER_BUILD_FLAGS=--network=host go test ./test/e2e/suites/basicvalidation/... -run "^TestPoolWarmImagesReachRuntimeCacheInventory$" -v -count=1 -timeout 12m`，退出状态 `0`，耗时 `35.477s`；外部 Docker Hub 不可达曾使 `busybox:1.36.1` pull 超时，改用测试环境已明确加载的 alpine 后消除了环境耦合，产品逻辑不因此降级。

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

### 11.6 实施结果（2026-07-19）

- 控制面镜像实现 `fastpath/controller/all` 三角色；生产配置拆为 3 副本无选主 FastPath Deployment 与 2 副本单 Leader Controller Deployment，Service 只选择 FastPath，RBAC、PDB、HPA 分离；
- FastPath 和 SandboxController 共用 `sandboxorchestrator.Orchestrator`，创建路径统一为 Top-K reservation -> CRD commit -> assignment CAS -> Fastlet Ensure；旧 Fast/Strong 字段不再改变持久化顺序；
- FastPath 持久化关键路径使用 direct API-server client，避免 informer cache 不具备 read-after-write 一致性；request ID 使用短 SHA-256 label 做可扩展查询，完整 ID 与 create spec hash 仍在 annotation 中做最终校验；
- assignment attempt 由 CAS helper 根据 API Server 中的 durable high-water mark 单调分配，调用方缓存对象不能选择或复用 attempt；Pod UID、instance generation 和 assignment attempt 全部进入 Fastlet identity fence；
- Fastlet reservation 原子绑定 request ID、create hash、claim namespace/name、profile hashes 和 Pod UID；CRD 已提交后，Controller 可以凭完全匹配的 durable claim 无 token 接管 reservation，消除 Controller/FastPath 并发 Ensure 导致的 slot 浪费；
- RPC 无候选/容量拒绝发生在 CRD 前；同 request ID 同 spec 返回同 UID，不同 spec 返回 AlreadyExists/Conflict；CRD commit 后的错误保留同一 Sandbox 供 Controller 或客户端重试；
- Delete、Reset、expireTime/failurePolicy 只提交声明式期望；Finalizer、generation fencing 和 Controller Reconcile 执行底层生命周期；Pod-bound 丢失模型按 failurePolicy 标记 Lost 或重建；
- Fastlet Watch 在新 Pod Ready/UID/IP 变化时立即执行一次受并发上限保护的 Heartbeat，不再让新 Pool 等待完整低频周期；
- e2e Pool fixture 显式使用固定小型 ResourceProfile，保持真实 `单 Sandbox 资源 × maxSandboxesPerPod + overhead` 乘算；namespace 增加每次 test process 的随机 run ID，消除跨运行 Terminating 冲突；
- 新增专用多活 e2e：验证 3 FastPath/2 Controller/单 Leader、Service endpoint 隔离、PDB/HPA、Leader 删除期间 RPC 可用、直接 CRD 创建、request ID 幂等/冲突，以及 40 并发请求严格受 3-slot Fastlet admission 限制；
- 增加 `config/all-in-one` 开发兼容 overlay：一个 `--role=all` Controller Pod 同时运行 FastPath 和 Reconciler，FastPath Service 只选择该 Pod，独立 FastPath Deployment/HPA/PDB 不进入渲染结果，Controller PDB 改选 `role=all`；生产 `config/default` 保持 split topology；
- controlplane e2e 会把 FastPath Deployment 实际缩为 0 验证 Controller-only；还会临时切换为单 Pod `role=all`、删除 HPA、确认 Service 唯一 endpoint 与 RPC Create/CRD UID，再完整恢复生产 split topology。2026-07-20 最终快照命令退出 `0`，总耗时 `454.960s`；Controller-only `3.03s`，role=all `22.68s`；
- 远端 unit gate：`make test-unit`，退出状态 `0`；
- 远端 race gate：`go test -race ./internal/controller/common ./internal/controller/controlplane ./internal/controller/fastletcontrol ./internal/controller/fastletpool ./internal/controller/fastpath ./internal/controller/sandboxorchestrator ./internal/controller ./internal/fastlet/runtime ./internal/fastlet/server ./internal/api -count=1`，退出状态 `0`；
- 远端 Kubernetes gate：`E2E_TEST_TIMEOUT=35m make test-e2e-controlplane`，多活、basicvalidation、lifecycle、scheduling、cleanupjanitor 全部通过，退出状态 `0`。

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

### 12.5 实施结果（2026-07-19）

- 新增 runtime-neutral `NetworkDriver/NetworkManager/NetworkSlotPool/IPAM/NetworkStateStore/AccessDescriptor` 边界；Linux reference driver 管理 bridge、独立 netns、veth、唯一私有 IP、DNS、MTU、默认路由、SNAT 和 Sandbox 间默认隔离；
- slot 状态固定为 `Clean -> Bound -> Destroying -> 删除`。已使用 slot 不回到 clean pool；runtime 删除完成后才销毁，失败保持占用并允许幂等重试，随后异步创建全新 slot；
- slot owner 使用 Sandbox UID、instanceGeneration 和 assignmentAttempt fencing；状态以 `0600` JSON 原子写入 `/run/fast-sandbox/network/<podUID>`，并持久化 host/container netns path、veth、IP、DNS 和 AccessDescriptor；
- container/gVisor RuntimeProfile 自动挂载 `/run/fast-sandbox/netns -> /run/netns`（Bidirectional）和 network state hostPath。host containerd 使用宿主可见 netns path，Sandbox `/etc/resolv.conf` 绑定到 slot resolver 文件；
- ContainerdDriver 在 Create 前 acquire，在失败时销毁；runtime labels 持久化 slot/netns/IP/gateway/DNS identity。Fastlet restart 会用 runtime inventory 与本地 descriptor 双向校验，缺失或不匹配时 fail closed，不开放 readiness；
- admission 容量与 clean slot 联动：Pool capacity 满时返回 `CapacityRejected`；容量未满但 clean slot 暂缺时返回 retryable `NetworkUnavailable`；
- Fastlet `/metrics` 暴露 `fastlet_network_slot_acquire_total{result=hit|miss}` 和 `fastlet_network_slots{phase=clean|bound|destroying}`；
- Fastlet 开发镜像包含 `iproute2/iptables`；`make test-network-integration` 在一次性 privileged 容器内真实验证 netns/veth/bridge/default route/NAT/隔离规则和幂等清理；
- 远端 unit gate：`make test-unit`，退出状态 `0`；目标 race gate：`go test -race ./internal/fastlet/network ./internal/fastlet/runtime ./internal/fastlet/server ./internal/runtimecatalog ./internal/controller`，退出状态 `0`；
- 远端 privileged Linux gate：`make DOCKER_BUILD_FLAGS=--network=host test-network-integration`，退出状态 `0`；
- 远端 Kubernetes focused e2e：同一 Fastlet 上两个 Alpine Sandbox 同时监听 `8080`，DNS/NAT 成功、私有 IP 不同、双向不可直连；删除一个不影响另一个；Fastlet container restart 后 Pod UID、slot ID、IP 和 netns 不变且 HTTP 继续可达，退出状态 `0`；
- 远端完整网络 gate：`E2E_TEST_TIMEOUT=35m DOCKER_BUILD_FLAGS=--network=host make test-e2e-network`，包含全部 basicvalidation 与私网强用例，退出状态 `0`；
- Pod 删除后的跨 Pod orphan 网络清理由阶段 10 `LinuxNetworkJanitor` 负责；本阶段状态文件已包含 Janitor 二次确认所需 owner Pod UID、Sandbox UID 和 generation/attempt，不让正常 Create/Delete 依赖 Janitor。

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

### 13.5 实施结果（2026-07-19）

- 新增独立 `cmd/fastlet-proxy` 和 `cmd/sandbox-proxy` 二进制及镜像。Sandbox Proxy 以 2 副本 Deployment + Service 运行；PoolController 为每个 Fastlet Pod 注入平台所有的 Fastlet Proxy sidecar、共享 UDS EmptyDir、独立 `5780` 数据端口和 readiness；Fastlet Control `5758` 不承载用户代理流量；
- Fastlet Proxy `RouteStore` 保存 `Sandbox UID -> assignment attempt / route generation / AccessDescriptor / state`，并在删除后保留 generation tombstone；旧 Apply、旧 Delete、冲突的同 generation route 均 fail closed。控制 UDS 提供 Apply/Delete/MarkDraining/Snapshot/Watch；
- Fastlet SandboxManager 将 route publication 作为 Create 的最后一步。route 暂时发布失败进入 `route-pending` 并原地重试，不清除 assignment、不重复创建 runtime；Delete 先 MarkDraining/DeleteRoute，再删除 runtime/network；恢复先用 runtime inventory + NetworkState 重建 RouteStore，再开放 Fastlet Ready；
- Fastlet Control 对 UDS Watch 保持长连接。Fastlet Proxy sidecar 单独重启导致 Watch 断开时立即撤销 route readiness，重新连接后用 Snapshot/Reconcile 全量恢复；真实 e2e 验证 Fastlet Proxy container restart 后原 Sandbox route 自动恢复；
- 新增 Ed25519 短期 route credential，绑定 namespace、Sandbox UID、target port、Fastlet Pod UID、assignment attempt、route generation、expiration 和随机 nonce。签名私钥只进入 FastPath，Sandbox Proxy/Fastlet Proxy 只持有公钥集合；验签器支持 `old,new` 重叠公钥窗口，覆盖“先发布双公钥、再切 signer、等待最大 TTL、最后移除旧公钥”的无中断轮换。缺 key 或非法 key 时三个生产二进制均 fail closed。仓库的固定 RFC 8032 key 只存在于显式 `development-only` e2e Secret；
- FastPath 实现 `ResolveEndpoint` 和 `IssueRouteCredential`。稳态 UID 查询使用 informer field index，cache miss 才进行一次 direct API fallback；Sandbox Proxy 通过 Sandbox/Pod Watch 维护路由，cache miss 或 token 与缓存 generation 不匹配时只进行一次权威补查，不持续轮询；Pod informer 只 Watch `app=sandbox-fastlet`；
- Sandbox Proxy 只根据 UID + target port 选择 assigned Fastlet Pod IP:5780，覆盖用户伪造的内部 fencing headers；Fastlet Proxy 再同时校验 fencing headers、签名 credential 和最终 Local RouteStore，通过 runtime-neutral DirectIP AccessDescriptor 连接 Sandbox 私网。两个 proxy 均不解析 execd/envd payload；
- HTTP reverse proxy 使用 streaming request/response、禁用整包缓冲并保留 backpressure/cancellation；单测真实覆盖 keep-alive 基础路径、chunked 2MiB response、SSE、WebSocket upgrade、request cancellation、任意 target port、outer credential/header 剥离和 upstream internal header 注入；第一阶段明确只开放 HTTP/1.1 + SSE + WebSocket，raw TCP、HTTP/2 和 gRPC 留给声明该协议需求的后续 profile；
- 修复 FastPath 与 Controller 在 CRD Create 后竞争 assignment 的边界：若 Controller 抢先选择的 Fastlet admission 明确拒绝，而 FastPath 已在另一 Fastlet 持有 reservation，则先 CAS 清除被拒绝 assignment，再消费原 reservation；回归单测验证 assignment attempt 和 route generation 均前进且不向用户泄漏可恢复的 CapacityRejected；
- 远端 unit gate：`make test-unit`，退出状态 `0`；目标 race gate：`go test -race ./internal/routeauth ./internal/fastletproxy ./internal/sandboxproxy ./internal/fastlet/runtime ./internal/controller/fastpath ./internal/controller/sandboxorchestrator ./internal/controller ./test/e2e/env`，退出状态 `0`；
- 远端 focused e2e：逐个访问两个 Sandbox Proxy 副本；两个容量为 1、位于不同 Fastlet 的 Sandbox 分别监听 8080/18080，无需 exposedPorts；Create 后有界重试可访问正确实例；Fastlet Proxy restart 后恢复；reset/delete 后旧 token/route 拒绝，退出状态 `0`；
- 新增可重复执行的 `make test-e2e-proxy` 专项门禁；远端执行 `E2E_TEST_TIMEOUT=25m DOCKER_BUILD_FLAGS=--network=host make test-e2e-proxy`，退出状态 `0`，总耗时 `113.523s`；
- 远端 lifecycle 回归：`E2E_TEST_TIMEOUT=25m DOCKER_BUILD_FLAGS=--network=host make test-e2e-lifecycle`，同名删除后立即重建和 Terminating 删除流程均通过，退出状态 `0`，总耗时 `103.092s`；
- 远端完整 basicvalidation gate：`E2E_TEST_TIMEOUT=35m DOCKER_BUILD_FLAGS=--network=host make test-e2e-network`，包含 CRD validation、env/workingDir、namespace isolation、private network 和 proxy data plane，退出状态 `0`，总耗时 `385.442s`；
- `verify-generated` 按 REF-0009 的 changed-files 远端基线规则执行：远端固定工具生成后，将 protobuf/CRD canonical 文件与本地分支交叉同步；本地 `git status` 对五份生成物无差异。远端仓库 HEAD 仍是 master，因此不把其 `git diff --exit-code` 对已提交 Phase 2～8 变更的预期差异误报为 drift。

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
- fastctl/Python SDK exec、files 和 command SSE 通过 ExecdAdapter；
- PTY 仅在所选 Infra Component 明确声明兼容的 WebSocket 扩展时由对应 Adapter 提供；OpenSandbox 公共 Execd 契约未声明 PTY 时必须 fail closed，Fast Sandbox Core 不补充私有 PTY 协议；
- FastPath/Fastlet Control 抓包或 handler 列表中不存在公共 Exec/File API。

远端：

```bash
bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'go test -race ./internal/fastlet/infra/... ./internal/sandboxinit/... ./cmd/fastctl/... && make test-e2e-infra && make test-e2e-sdk'
```

### 14.7 实施结果（2026-07-19）

- 新增平台所有的 `infracatalog`，内置 `minimal`、`test-infra`、`opensandbox-execd` 和 `e2b-envd`。Profile hash 覆盖完整不可变配置和 artifact digest；Pool `infraProfile` 设为不可变，Controller 同时将规范化 profile 名与 hash 注入 Fastlet Pod，Registry heartbeat、reservation 和 Fastlet admission 全链路校验同一 hash；
- `opensandbox-execd` 在生产 artifact release binding、OCI opener 和签名策略尚未配置时保持 `Configured=false`，Pool 在创建 Fastlet 前 fail closed；`e2b-envd` 只描述支持 runtime 的 TemplateBake/Preinstalled + SystemService 契约，SDK 只向官方 E2B Connect client 交付 route URL/header，不复制 envd protobuf；
- 新增 Fastlet 本地 content-addressed Artifact Store：cache hit 不重新打开 registry/static source，写入前做大小限制和 sha256 校验，原子发布后按 data/executable 变体分别固定为 `0444/0555`，损坏和 digest mismatch 均 fail closed；Static resolver 限制在平台 root，OCI opener 和签名/attestation verifier 为显式注入点；已 Prepared 的 supervisor/component digest 进入 `InfraArtifact` GC 保护索引和 heartbeat；
- artifact preparation 与 warmImages、Pod Ready 解耦。Fastlet 可先完成 Kubernetes readiness，但 `InfraReady=false` 时 Registry hard filter 和 Fastlet reservation/ensure 都拒绝该 Profile；required artifact 准备支持原地重试，声明式路径保持 pending；
- 新增 `sandbox-init` 最小 supervisor。它保留用户 entrypoint/args/env/cwd/退出码，默认让 Infra 和用户进程并行，支持 `startBeforeUser`、依赖拓扑、restart policy、readiness、进程组信号转发和子进程回收；平台 supervisor 以容器 root 读取 `0400` 实例配置，再按 OCI 原始 UID/GID/附加组降权启动用户进程，因而兼容非 root 用户镜像且不把内部 token 放进用户环境；
- 每个 instance generation/assignment attempt 生成独立 token 和只读配置；reset 生成新 token，恢复只接受完全匹配的持久化 identity。required init/readiness 失败保持 `infra-pending` 且不发布 route，optional 失败进入 component diagnostics 但不阻塞 Create；Create 最终成功 gate 为 runtime、required Infra、route publication 全部完成；
- Fastlet Proxy 的 upstream credential 改为按 target port 作用域保存。转发前剥离调用方伪造的所有平台 upstream header，只向对应 Infra service 端口注入真实值；真实 e2e 同时验证 Infra 端口能鉴权、同 Sandbox 的普通用户端口收不到内部 token；
- 新增公共 Go `pkg/sandboxclient` 和 Python SDK：生命周期调用 FastPath，`EndpointResolver` 获取短期、generation-fenced Sandbox Proxy route；OpenSandbox ExecdAdapter 实现 `/command` SSE 与 file API，Go/Python 文件下载均支持流式传输；EnvdAdapter 仅提供 native client hand-off。fastctl 新增 `exec/cp/files`、`--proxy-endpoint` 和 `--adapter`，`run` 自动生成或接受显式 request ID，`logs` 明确为 runtime diagnostics；
- FastPath proto compatibility test 明确禁止公共 Exec/File/PTY 方法。当前 Execd 公共契约没有 PTY 时，fastctl/Python 对 stdin/TTY fail closed；透明代理已能承载 WebSocket，但只有未来明确声明兼容扩展的 Adapter 才开放 PTY；
- 远端目标 race gate：`go test -race ./api/proto/v1 ./internal/fastlet/infra/... ./internal/sandboxinit/... ./internal/infracatalog/... ./internal/fastlet/runtime/... ./internal/fastletproxy/... ./internal/controller/fastletpool/... ./internal/controller/sandboxorchestrator/... ./pkg/sandboxclient/... ./cmd/fastctl/... ./cmd/sandbox-init`，退出状态 `0`；
- 远端完整 unit gate：`make test-unit`，退出状态 `0`；`make manifests` 使用固定远端工具链重新生成 CRD，退出状态 `0`；本地 `generate_proto.sh` 可重复生成 Python stub，`git diff --check` 通过；
- 远端 Infra e2e：`E2E_TEST_TIMEOUT=35m DOCKER_BUILD_FLAGS=--network=host make test-e2e-infra`，退出状态 `0`，总耗时 `59.879s`；
- 远端 SDK Adapter e2e：`E2E_TEST_TIMEOUT=35m DOCKER_BUILD_FLAGS=--network=host make test-e2e-sdk`，验证 `Adapter -> Sandbox Proxy -> Fastlet Proxy -> injected component` 的 command SSE 与 file download，退出状态 `0`，总耗时 `22.821s`；
- Python 3.12 SDK unit：`python -m unittest discover -s sdk/python/tests -v`，3 个用例全部通过，覆盖 request ID、Execd route/SSE/files/streaming download 和 envd hand-off，退出状态 `0`。

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

### 15.5 实施结果（2026-07-19）

- Pool 缩容不再按 PodList 顺序直接删除。Controller 通过 direct API reader 根据 CRD 中精确的 `FastletName/FastletPodUID` assignment 计数，避免 Leader 切换后的 cache lag 把 loaded Pod 误判为空，并优先选择真实空 Fastlet；Drain 期望通过 `fast-sandbox.io/draining`、started-at、reason 和 acked-at annotation 持久化，新的 Controller Leader 可只凭 Pod 状态继续；
- Drain 先持久化 annotation，再调用 Fastlet `SetDraining`。所有 Registry Pod Watch 立即把 annotation 投影成 `DrainRequested/Draining`，Heartbeat 即使暂时报 `Draining=false` 也不能清除平台期望；Fastlet 原子 admission 拒绝新 UID 的 reservation/ensure，但同一 identity 的已有 Sandbox Ensure 仍幂等成功，避免 Controller Reconcile 因 `ErrorDraining` 错误清除 active assignment；
- 已有 Sandbox 在 Drain 期间继续运行。空且至少成功 ack 过的 Fastlet 才正常删除；有负载的 Fastlet 等待 `--fastlet-drain-timeout`，超时删除后统一进入既有 PodLost `FailurePolicy`。需求恢复时先 RPC 取消 Drain，成功后才移除 annotation；
- SandboxController 以 Kubernetes Pod Name + UID + deletionTimestamp 判断 PodLost。本地 Registry 暂时缺少 endpoint 只标记 `FastletRegistryPending/Unavailable` 并原地重试，不再触发错误 AutoRecreate；同名 replacement Pod 因 UID 不同必然进入 PodLost；
- `Manual` 在确切 PodLost 后保持 assignment 并标记 `Lost`；`AutoRecreate` 清除旧 assignment，同时递增 instanceGeneration 和 routeGeneration，下一次 CAS 使用更大的 assignmentAttempt。删除、过期和 reset 路径在 Registry 不可用时也先核对真实 Pod UID，避免提前移除 finalizer；
- NodeJanitor 重构为统一 `ResourceIdentity + CleanupBackend`。Containerd 与 Linux network backend 都产出 Fastlet Pod UID、Sandbox UID、instance generation、assignment attempt 和创建时间；发现阶段与删除前使用同一 authority decision 做两次校验，任何 Pod/Sandbox API 错误、legacy fence 缺失或资源 identity 前移都 fail closed；
- Containerd backend 清理 task/container/snapshot/FIFO，并在删除前重新读取 label fence 防止 ID 复用；Linux network backend 扫描 `/run/fast-sandbox/network/<podUID>`，校验 state directory/owner fence，幂等清理持久 named netns、veth peer、DNS 和 JSON state。Pod network namespace 内的 bridge/NAT 随 Fastlet Pod 自动销毁，不作为跨 Pod 共享规则删除；
- Janitor DaemonSet 增加 host network、network state hostPath 和 Bidirectional named-netns mount，镜像包含 iproute2/iptables。BoxLite 使用同一 backend contract 和保留的 `BackendBoxLite` 类型；实际 Box/shim/state scanner 随阶段 11 BoxLiteDriver 一起实现，不能借 containerd scanner 伪装支持；
- Expired/clear-assignment 同步清空 deprecated `sandboxID/endpoints` 投影，保持旧 SDK/E2E 兼容，但它们仍不是权威 identity；
- 新增 Pool Drain unit/e2e：覆盖空 Fastlet 优先、loaded Fastlet 等待 timeout、fresh reconciler 从 annotation 接管、Registry heartbeat 不能清除 Drain、leader election 切换后 Drain 状态保留，以及新 Sandbox 避开 Draining Pod；
- PoolController 为期望 Fastlet template 计算稳定 SHA-256 并写入 Pod annotation；template 变化时只创建一个 surge，必须等待新 Pod Kubernetes Ready 和精确 Pod UID heartbeat 的 RuntimeReady/InfraReady 后，才对一个旧 template Pod 持久化 `planned-upgrade` Drain。旧 Pod 沿用原有 ack、负载等待和 timeout 语义，删除后再滚动下一个；
- 新增 planned-upgrade unit/e2e：验证 replacement heartbeat 未就绪时旧 Pod 不 Drain、replacement 就绪后旧 Pod 收到持久化 reason、已有 Sandbox 退出后旧 Pod 才删除；远端命令 `GOFLAGS= E2E_TEST_TIMEOUT=35m DOCKER_BUILD_FLAGS=--network=host go test ./test/e2e/suites/drain/... -run "^TestPoolPlannedUpgradeUsesReadySurgeAndDurableDrain$" -v -count=1 -timeout 12m`，退出状态 `0`，耗时 `165.748s`，核心断言 `16.21s`；
- 远端完整 unit gate：`make test-unit`，退出状态 `0`；Drain/Controller/Janitor 目标 package gate 与 Kubernetes manifest client dry-run 均退出 `0`；
- 远端 Drain e2e：`E2E_TEST_TIMEOUT=35m DOCKER_BUILD_FLAGS=--network=host make test-e2e-drain`，最终用例包含真实 Controller Leader 删除/重选，退出状态 `0`；最终代码快照总耗时 `79.360s`，核心断言 `28.88s`（首次含全量环境准备运行 `338.399s`）；
- 远端 fault-recovery：AutoRecreate、Manual Lost、reset、registry create/delete cycle 和 Pod orphan 路径通过；修复兼容投影后定向 `TestAutoExpiry` 通过，退出状态 `0`，耗时 `144.038s`；
- 远端 cleanup-janitor e2e：`E2E_TEST_TIMEOUT=35m DOCKER_BUILD_FLAGS=--network=host make test-e2e-cleanupjanitor`，退出状态 `0`，耗时 `83.596s`。
- Controller-only e2e 不再只绕过 FastPath client：测试会把 FastPath Deployment 实际缩容为 0，等待无 FastPath Pod，直接创建 Sandbox CRD并等待 assignment/runtime Ready，再恢复三个 FastPath 副本；远端定向 controlplane Gate 退出状态 `0`，总耗时 `119.943s`、Controller-only 核心断言 `3.03s`。

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
12. InfraProfile 能通过 TemplateBake/Preinstalled/ArtifactVolume 中至少一种方式工作。

远端 runtime gate：

```bash
bash /Users/fengjianhui/.codex/superpowers/skills/remote-dev-run/scripts/remote_exec.sh \
  'make test-e2e-runtime-container && make test-e2e-runtime-gvisor && make test-e2e-runtime-kata && make test-e2e-runtime-boxlite'
```

### 16.6 实施结果（2026-07-19）

- container reference gate 已覆盖默认 runtime 的实际资源限制和 lifecycle 基线。远端命令 `E2E_TEST_TIMEOUT=20m DOCKER_BUILD_FLAGS=--network=host make test-e2e-runtime-container`，退出状态 `0`，总耗时 `90.883s`，核心断言 `2.51s`；
- gVisor kind 节点准备流程补齐 runsc host binary、SHA-512 校验和 `/etc/containerd/runsc.toml`；Pool `RuntimeReady` 只接受对应 Fastlet Pod UID 的 capability heartbeat。恢复阶段若 runtime 已存在但 Infra 尚未恢复，RouteLifecycle 不再提前删除 route tombstone；
- gVisor 远端命令 `E2E_TEST_TIMEOUT=40m DOCKER_BUILD_FLAGS=--network=host make test-e2e-runtime-gvisor`，退出状态 `0`，总耗时 `170.957s`。用例覆盖单 Sandbox、guest/host kernel 隔离、DNS、私网、Fastlet Proxy/Sandbox Proxy、Fastlet 重启恢复，以及三个并发 Sandbox；guest kernel 为 `4.19.0-gvisor`，host kernel 为 `5.15.0-173-generic`；
- Kata QEMU 和 Cloud Hypervisor 使用 Fastlet 创建并持久化的 Linux netns。CLH capability probe 强制校验 `sandbox_cgroup_only = true`；资源验收从 containerd OCI spec 验证 Fastlet 的最终执行边界，CPU `25000/100000`、memory `268435456`、PIDs `128`；
- Kata 聚合远端命令 `E2E_TEST_TIMEOUT=40m DOCKER_BUILD_FLAGS=--network=host make test-e2e-runtime-kata`，退出状态 `0`，总耗时 `223.169s`。QEMU 核心断言 `27.80s`、guest kernel `6.18.12`、private IP `172.30.0.3`；CLH 核心断言 `26.02s`、guest kernel `6.18.12`、private IP `172.30.0.5`；两者均验证隔离、资源、私网、代理链路和 Fastlet recovery；
- Firecracker 在当前远端环境中曾出现 CRD 已 Running 后 VM/shim 消失，不能声明支持。内置 `kata-fc` profile 现在明确 `CapabilityDegraded/KataFirecrackerNotValidated`，Pool `RuntimeReady=False/RuntimeUnavailable` 且不创建 Fastlet Pod；同一 Kata 聚合 gate 对此 fail-close 行为完成验证；
- BoxLite spike 结论固化到 [BoxLite Runtime Adapter ADR](../specs/2026-07-19-boxlite-runtime-adapter-adr.md)：采用独立 native Runtime Sidecar + 纯 Go Fastlet UDS client；上游 `boxlite serve` 因缺少 volume/port mapping surface 不能直接使用。一个长期 Runtime 管理当前 Fastlet Pod 的多个 Box，Sandbox UID 是稳定 Box name；
- 已实现版本化 Sidecar lifecycle/cache contract、owner fence、profile conflict、显式 `RecoverBox`、`ArtifactVolume`、`AccessKindLocalForward` 和 Fastlet Proxy authenticated target-port preamble。boot-critical artifact 在 Box create 时作为只读 `/.fast` bundle 挂载；`GuestCopy` 不再用于描述这个实现；
- `sandbox-tunnel` 已实现固定 40-byte `FSBF/version/TCP/targetPort/credential` 握手，只允许连接 guest loopback；target port 0 是 Sidecar 到 guest 的 authenticated health handshake。Sidecar 为每 Box 生成独立 256-bit credential，持久化在 fenced local record 并只通过 UDS/AccessDescriptor 交给 Fastlet，不写 CRD；另一个 Box 的 credential 会在 target dial 前被常量时间拒绝。Fastlet Infra lifecycle 对 `LocalForward` 使用同一 transport，HTTP/TCP readiness 不依赖 guest IP。该实现补偿 BoxLite v0.9.7 host forward 强制绑定所有 interfaces 的限制，Sidecar 现在报告 `local-forward-v1=true`；
- Controller 已按 RuntimeProfile 注入平台-owned `boxlite-runtime` Sidecar、UDS/infra EmptyDir、`/dev/kvm` 和 state HostPath。Pool 总资源 request/limit 的 owner 是 Sidecar，不再错误记到 Fastlet；Sidecar readiness 会校验 protocol 和全部 required capability，资源语义不完整时 Pod 不 Ready。保留卷名和挂载路径现在覆盖检查 Fastlet、全部用户 sidecar 和 init container，用户容器不能借平台随后注入的 `proxy-control`/`boxlite-control` 卷访问本地控制 UDS；
- native Sidecar 固定 BoxLite Go SDK/native v0.9.7，通过独立 Dockerfile 构建；Dockerfile-specific ignore 把 build context 从约 574MB 收敛到 1.16MB，且不影响其他使用 `bin/` 的镜像。包含 authenticated LocalForward 的当前源码在远端执行 `make docker-boxlite-runtime`，使用 checksum-verified native archive 构建成功，image `sha256:c98962af7bb1c5b381cac79897ee2feefe340d61b07df609ea99adc16d3124f1`、大小 `189478601` bytes；容器内 `--help` 运行通过，证明动态运行依赖完整；
- 真实 KVM spike 使用一个长期 Runtime 创建多个 Box。Alpine Box 成功返回 `running`，重复 Ensure 返回同一 Box ID/PID/HostPort；注入的 guest HTTP fixture 经 `LocalForward -> sandbox-tunnel -> 127.0.0.1:18080` 返回 `boxlite-ok`。Sidecar SIGTERM/重启后从 durable record 列出原 Box，`POST RecoverBox` 保持同一 identity，恢复后同一 target port 继续可访问；
- BoxLite image registry 支持显式 host/HTTPS-or-HTTP/skip-verify/search 配置；storage/image/network/RPC transport failure 映射为可重试 `Unavailable`，不再误报 Internal。外部 Registry 在当前远端不可达时，spike 使用 localhost OCI registry 隔离验证 Runtime 代码与外部网络；
- NodeJanitor 已接入 BoxLite backend：Sidecar写入 home owner fence，Janitor 扫描 immutable Sandbox records，并在删除前重新校验 Fastlet Pod/Sandbox generation/attempt 以及 Runtime `.lock`；仍有 Sidecar 持锁时 fail closed，最后一条 record 清理后回收 per-Pod home。Sidecar 恢复记录同时校验 Pod owner、record filename、create-spec hash 和 bundle root，避免损坏记录跨 Pod/路径接管。Janitor manifest 增加 BoxLite state HostPath，client dry-run 通过；
- BoxLite v0.9.7 资源边界已补充真实 KVM 负向实验：guest cgroup v2 的 `cpu/memory/pids` 写入和进程迁入均成功，但 root workload 可以把 `cpu.max` 改回 `max` 并迁回父 cgroup；remount read-only 失败，`chown/chmod` 也会被默认 OCI capabilities 绕过。Rust jailer 虽有 host `ResourceLimits`，v0.9.7 Go/C AdvancedBoxOptions 没有 resource setters，且 host `pids.max` 不等价于 guest process 数。静默剥夺 root capabilities 会破坏用户 OCI 语义，因此不作为实现。`resource-limits-v1=false`、native Sidecar `Ready=false` 和 `BoxLiteResourceEnforcementIncomplete` 继续保持；解除门禁要求上游提供版本化、创建前且不可被 guest 绕过的完整资源入口，并通过 root workload 逃逸负测。当前代码的远端命令 `E2E_PROFILE=basic E2E_TEST_TIMEOUT=20m make test-e2e-runtime-boxlite` 退出状态 `0`，总耗时 `115.818s`，Pool condition 精确断言上述 reason；该结果只证明 fail closed，不是 BoxLite 已支持；
- 本阶段当前代码的远端完整 `make test-unit` 退出 `0`；新增代码的 race gate 拆分运行 `go test -race ./internal/boxlitesidecar ./internal/fastlet/infra ./internal/fastlet/network ./internal/fastlet/runtime ./internal/fastletproxy ./internal/sandboxtunnel -count=1` 和 `go test -race ./internal/controller ./internal/janitor -count=1`，均退出 `0`。authenticated LocalForward 后再次运行 network/runtime/proxy/tunnel race，以及 reserved-volume 修复后的 Controller race，均退出 `0`；测试明确验证另一个 Box 的 credential 在 target dial 前被拒绝。恢复 fencing 补丁另以 `CGO_ENABLED=1 go test -tags boxlite_native ./cmd/boxlite-runtime -count=1` 验证，tunnel 取消路径的普通/race gate 也均退出 `0`。远端 `make verify` 已完成生成步骤；由于远端 Git 索引仍是 `master`，其 `git diff --exit-code` 会把阶段 1～10 的分支生成物识别为差异，不能作为分支 gate；把远端重新生成的 protobuf、DeepCopy 和 CRD 同步回本地分支后，受检路径 `git diff --exit-code` 为 `0`。阶段 11 按“已验证能力才可宣称支持”的口径完成：container、gVisor、Kata 进入支持矩阵，BoxLite native adapter/恢复/代理链路已有证据，但资源语义不完整，因此明确留在 `RuntimeUnsupported`，不把 fail-closed gate 伪装成支持完成。

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

### 17.5 第一批可观测性实施进度（2026-07-19）

- FastPath 增加 `fast_sandbox_create_accepted_latency_seconds{path,result}` 与 `fast_sandbox_create_data_plane_ready_latency_seconds{result}`；拒绝、reservation accepted、合法幂等命中分开计数，request ID 冲突不会误记为 accepted；
- Registry/Orchestrator 增加 heartbeat age、watch/eligible candidate count、image affinity outcome 和 Top-K candidate rejection/retry 指标；指标只表达最终有界分类，不把 Fastlet ID、Pod UID、request ID 或 Sandbox UID 放入 label；
- Fastlet admission 增加 reserve/cancel/ensure outcome、reservation inflight 和 capacity/used/creating/running/deleting gauges；runtime create、Infra service readiness、DataPlaneReady、cache snapshot/revision/GC decision、network clean/in-use slot 均已覆盖；
- Sandbox Proxy、Fastlet Proxy 分别记录 route/upstream 全请求或流生命周期；指标 server 使用独立 `9094/9093` 管理端口，用户数据端口不暴露 `/metrics`，现有 streaming、WebSocket、SSE、cancel/backpressure 转发实现不包裹 `ResponseWriter`；Sandbox Proxy 与 Janitor manifest 已提供独立 scrape 端口，Fastlet Proxy sidecar 暴露命名的 `proxy-metrics` 端口供 PodMonitor 抓取；
- NodeJanitor cleanup 指标在删除前二次确认、跳过、backend unavailable、backend error 和 cleaned 各路径采样，backend/reason 都来自代码内枚举集合；
- `runtime_create_latency{cache_hit}` 当前明确写为 `unknown`，因为进程内 CacheSnapshot 不能证明本次 runtime create 的真实 pull/unpack 命中；在 RuntimeDriver 提供 per-create result 前不做推断；
- `fast_sandbox_user_process_start_latency_seconds` 只在 runtime adapter 能证明直接用户进程已经启动时采样：containerd task start 和新建 BoxLite Box 的 direct-entrypoint 路径标记 `source=runtime_direct`；幂等命中标记 `existing_runtime/not_applicable`。使用 sandbox-init supervisor 的路径只增加 `sandbox_init_unreported/unavailable` observation counter，不把 supervisor/VM task start 冒充 user-process latency；后续由 sandbox-init 回传可信 started signal 后再补齐这一类样本。trace context/structured identity log 仍留在下一切片，不用 metrics label 代替 tracing；
- 远端定向 gate：`go test ./internal/controller/fastletpool ./internal/controller/fastpath ./internal/controller/sandboxorchestrator ./internal/fastlet/cache ./internal/fastlet/infra ./internal/fastlet/network ./internal/fastlet/runtime ./internal/fastletproxy ./internal/sandboxproxy ./internal/janitor ./cmd/fastlet ./cmd/fastlet-proxy ./cmd/sandbox-proxy ./cmd/janitor -count=1`，退出状态 `0`；
- 远端完整 unit gate：`make test-unit`，退出状态 `0`；race gate 拆为 `go test -race -p=1 ./internal/controller/fastletpool ./internal/controller/sandboxorchestrator ./internal/fastlet/cache ./internal/fastlet/infra ./internal/fastlet/runtime -count=1`、`go test -race -p=1 ./internal/controller/fastpath ./internal/sandboxproxy ./internal/fastlet/network ./internal/janitor -count=1` 和 `go test -race -p=1 ./internal/controller ./internal/fastletproxy ./internal/sandboxproxy -count=1`，均退出状态 `0`；代理管理端口调整后再次运行 `make test-unit` 也退出 `0`。首次全并行 race 因远端 `/tmp` 空间耗尽中断；只删除精确的 stale `/tmp/go-build*` 与可再生 Go build cache 后改为 `-p=1`，未删除仓库、镜像、volume 或集群资源。
- user-process signal 补充后远端运行 `go test -race -p=1 ./internal/fastlet/runtime -count=1 && CGO_ENABLED=1 go test -race -tags boxlite_native ./cmd/boxlite-runtime -count=1 && make test-unit`，三个 gate 均退出状态 `0`；测试覆盖 direct task、sandbox-init supervisor 不可冒充、BoxLite wire timestamp/source 和 Prometheus observed/unavailable 分类。
- 新增本地只读 `fastctl migrate pool`：把 legacy `runtimeType/runtimeClassName/containerdRuntimeHandler` 转为 canonical `runtime`，物化旧对象有效的 `sandboxResources/maxSandboxesPerPod/infraProfile` 默认值，保留未知 metadata/template 字段并支持多 YAML 文档；不一致的 handler/runtimeClass override fail closed。`--check` 可作为配置 CI gate，仓库的 `pool.yaml/pool-gvisor.yaml/pool-kata.yaml` 已全部补齐 `infraProfile: minimal` 并通过检查；迁移步骤和 Create/Sandbox 兼容字段边界记录在 `docs/migration-guide.md`。
- `fastctl run` 不再发送 deprecated `consistency_mode/exposed_ports`，也不再打印已经废弃的 host-port endpoints；旧 flag/config key 只给出 ignored warning。FastPath 对可检测的旧字段记录 `fast_sandbox_deprecated_create_field_total{field}`，但持久化 Sandbox 时主动丢弃 exposed ports，保证旧客户端不能重新引入端口调度语义。
- 迁移切片远端运行 `go test -race -p=1 ./cmd/fastctl/cmd ./internal/controller/fastpath -count=1`、三个 canonical sample 的 `go run ./cmd/fastctl migrate pool --file <sample> --check` 以及 `make test-unit`，全部退出状态 `0`。
- 根 README 与中英文 ARCHITECTURE 已切换为现行架构，不再把 Fast/Strong、host-port reservation 或 PodIP endpoint 当成有效语义；同步更新 FastPath proto 注释、性能契约与测试手册。新增标准 `config/default` kustomize base、带开发测试 key 的 `config/dev` overlay，以及默认不启用的单 namespace NetworkPolicy 样例；e2e 环境改用 overlay 内开发 key。生产 base 仍要求外部 secret manager 提供 `fast-sandbox-route-keys`。
- 部署/文档切片在远端运行 `kubectl kustomize config/default`、`kubectl kustomize config/dev`、两个 overlay 的 `kubectl apply --dry-run=client --validate=false -k ...`、NetworkPolicy client dry-run、`go test ./api/proto/v1 ./test/e2e/env/... -count=1` 和 `make test-unit`，全部退出状态 `0`；首次直接引用父目录 YAML 被 kustomize load restrictor 拒绝后，已改为每个 base 自带 kustomization 的标准结构并重新通过。
- Trace/structured identity 切片增加 W3C Trace Context 的 gRPC/HTTP 传播，覆盖 fastctl/Go/Python SDK、Fast-Path、Fastlet control API、Sandbox Proxy、Fastlet Proxy 和 Execd upstream；Controller Reconcile 保持异步 root span，通过 request ID、Sandbox UID、Fastlet Pod UID 及三类 generation/attempt 字段关联。高基数字段只进入 span attribute 与 context logger，不进入 Prometheus label。各二进制仅在显式配置 endpoint 时安装 OTLP/gRPC exporter，未配置时 no-op，退出最多等待 5 秒 flush；配置和验证边界记录在 `docs/observability.md`。远端两组 `go test -race -p=1` 覆盖 observability/API/controller/Fastlet/proxy/Go SDK，退出状态均为 `0`；真实本地 gRPC TraceService smoke test 验证 `Configure -> batch export -> shutdown flush` 和 `service.name`，unit/race 均通过；`make test-unit` 退出状态 `0`；Python 3.12 临时容器安装 `.[dev,telemetry]` 后 `python -m pytest -q` 为 `4 passed`，容器随后删除。`make verify` 的 `git diff` 因远端 index 按约定保持 master 而不能作为分支 gate；重新生成的两份 protobuf Go、deepcopy 和两份 CRD 与本地当前分支逐文件 SHA-256 完全一致。
- 新增 `test/performance/create_load` FastPath Create 负载报告工具，输出带 schema version 的 JSON：明确记录 commit/environment/runtime/InfraProfile/warm-cold/image affinity/NetworkSlot/副本数/请求形态，区分 attempted/not-attempted，报告全 RPC 与成功 RPC 的 p50/p95/p99、bounded gRPC code、缺失/重复 Sandbox UID/name，并把声明式 cleanup 单独计时。工具不把 client-observed Create RPC 冒充 CreateAccepted/DataPlaneReady，也不伪装支持 direct-CRD path；unit/race/build 均通过并纳入 `UNIT_PACKAGES`。在现有专用 `kind-fsb-e2e-basic` 上使用独立临时 namespace、5-slot container Pool 完成 3 请求/2 并发 smoke：3/3 成功、UID/name 全唯一、cleanup 3/3，工具进程退出 `0`；观测到 full RPC p50 `198.970ms`、p95 `214.175ms`，仅证明工具与当前 API 链可用，不作为最终性能基线。临时 namespace、Pool、Sandbox、manifest、构建 ELF 和 port-forward 进程均已精确清理。
- Registry Top-K 不再为每次调度深拷贝全部 Fastlet 完整状态，也不再按 watched Fastlet 数逐个写 heartbeat histogram；锁内只生成有界排序投影并预计算 image hit/load/stable rank，最终仅深拷贝选中的 K 个结果。heartbeat age 改为每个 present state 记录一次最大值，candidate histogram 扩展到 5000 Fastlet，eligible 明确为 hard filter 后、K 截断前。Linux VM 上相同 `BenchmarkRegistryTopK1000` 五次中位数由 `2.156ms / 515080 B / 2005 allocs` 降至 `0.431ms / 67272 B / 12 allocs`；聚焦 unit 与 `go test -race -p=1 ./internal/controller/fastletpool -count=1` 均退出 `0`。Fastlet admission 仍是容量最终权威。
- 强化真实故障 Gate：controlplane e2e 不再用一条 Service gRPC 连接冒充多活，而是分别直连 3 个 Fast-Path Pod，逐副本验证 Create，再把 40 路并发请求轮询分发到 3 个副本，单 Fastlet capacity=3 时严格只有 3 个唯一 UID/name 和 Ready CRD/runtime identity，其余全部 `ResourceExhausted`；同时继续覆盖 Controller Leader 删除期间 Create 成功。Proxy e2e 删除一个 Sandbox Proxy Pod，在仅有 survivor 时重新经 Service 建连并成功路由，随后等待 2/2 恢复，再覆盖 Fastlet Proxy restart 与 reset/delete 旧凭证失效。远端聚焦命令 `go test ./test/e2e/suites/controlplane/... -run '^TestMultiActiveControlPlane$' -v -count=1 -timeout 12m`（`49.907s`）和 `go test ./test/e2e/suites/basicvalidation/... -run '^TestSandboxProxyDataPlane$' -v -count=1 -timeout 12m`（`90.239s`）均退出 `0`，临时 namespace/测试进程均清理且 Deployment 恢复为 Controller `2/2`、Fast-Path `3/3`、Sandbox Proxy `2/2`。真实 Gate 同时发现 e2e 环境仍用 `kubectl apply -f config/crd/` 读取已引入的 kustomization；已改为 `kubectl apply -k config/crd` 并更新命令顺序单测。首次构建因远端根盘只剩 `408MiB` 失败，仅执行 `go clean -cache` 删除约 `4.1GiB` 可再生 Go build cache 后重试；e2e 镜像构建后完整 `make test-unit` 又在链接 `internal/janitor.test` 时因空间不足中断，其余已执行包通过。再次清理约 `2.3GiB` Go build cache 后，用相同 `UNIT_PACKAGES` 和 flags 加 `-p=1` 串行运行，退出 `0`。全程未删除镜像、volume、集群或仓库数据。
- 最终能力矩阵、当前快照命令、性能数据、资源清理记录和未解除限制已汇总到 [架构重构验收报告](../../release-acceptance-report.md)。开发分支按约定范围完成；报告明确禁止把 Kata Firecracker/BoxLite fail-closed Gate、单节点 kind Create smoke 或缺失 user-process signal 的样本包装成生产支持证据。

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
