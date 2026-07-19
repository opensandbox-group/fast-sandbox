# Fast Sandbox 重构问题与决策日志

**开始日期**：2026-07-19  
**关联计划**：[Fast Sandbox 架构重构开发计划](./2026-07-19-fast-sandbox-architecture-refactor-implementation-plan.md)

## 使用规则

- 已有五份设计文档和跨模块决策是当前实现基线；
- 不改变目标架构的普通代码细节由开发阶段直接确定并通过测试验证；
- 会改变公共 API、核心语义、安全边界、Runtime 支持范围或交付范围的问题记录在本文；
- 非阻塞问题记录后继续推进，不等待即时决策；
- 真正阻塞当前依赖链且没有安全替代路径的问题，记录证据、影响和候选方案后再请求决策；
- 每个问题必须保留发现时的命令、测试或代码证据，关闭时补充最终结论和验证证据。

## 状态定义

```text
Observed          已发现，尚未完成分析
Investigating     正在收集证据
DecisionPending   需要最终方案决策，但当前可继续其他工作
Blocked           阻塞关键依赖链且没有安全替代路径
Resolved          已解决并有验证证据
Deferred          已明确不属于本轮范围
```

## 问题列表

| ID | 阶段 | 状态 | 是否阻塞 | 问题 | 当前处理 |
|---|---|---|---|---|---|
| REF-0001 | 0 | Resolved | 否 | 仓库说明默认 SSH alias 是 `ssh-fast`，当前机器实际 alias 是 `fast` | 使用 git-ignored `.remote-dev-run.env` 固定 `fast:~/fast-sandbox` |
| REF-0002 | 0 | Resolved | 否 | 远端 PATH 中没有 Go，但 `/usr/local/go` 已安装 Go 1.25.7 | 已将现有 `go/gofmt` 链接到 `/usr/local/bin`，baseline 可以直接执行 |
| REF-0003 | 0/11 | DecisionPending | 否 | 远端 host 当前未发现 runsc、Kata shim host command 和 BoxLite；Kata RuntimeClass 已存在于 kind 节点 | 阶段 0 只记录 capability；阶段 11 分别准备并验证，不用 skip 作为支持证据 |
| REF-0004 | 1 | Resolved | 否 | 专题文档中仍存在 service-name route、RuntimeDriver Exec/File 等被跨模块文档覆盖的旧表述 | 已统一为 target-port route、无公共 Exec/File RuntimeDriver，以及 reservation-before-CRD Create 流程 |
| REF-0005 | 0/1 | Resolved | 否 | master 的 `make test` 实际包含所有 e2e 包，Go 会并行运行多个 Suite 并同时操作同一个 kind 集群 | 阶段 1 将 `test`/`test-unit` 限定为纯单测，e2e 继续通过 `-p 1` 的独立目标串行执行 |
| REF-0006 | 2 | Resolved | 否 | Pool immutable schema e2e 首次更新与 Controller status 更新发生 resourceVersion 竞争 | e2e 使用 RetryOnConflict 重新读取后提交；CEL immutable 规则验证通过，产品语义不变 |
| REF-0007 | 3/12 | DecisionPending | 否 | v1alpha1 无法区分“升级前已存在且未写 sandboxResources”的 Pool 与“升级后新建但遗漏字段”的 Pool | 过渡期仅对全字段缺省采用固定兼容 profile；新 sample/fixture 全部显式写入，发布前决定是否通过 v1beta1/转换 webhook 改为必填 |
| REF-0008 | 3 | Resolved | 否 | SHA-256 RuntimeProfile hash 直接作为 Pod label value 超过 Kubernetes 63 字符限制，导致 Fastlet Pod 无法创建 | label 改为 `version + 12位短 hash`，完整 hash 存 annotation 和 Fastlet env；e2e 继续验证完整 hash 链路 |
| REF-0009 | 3 | Resolved | 否 | 远端恢复为 master 后，`verify-generated` 会把本地分支已提交但远端 Git 未包含的合法生成物误报为 drift | changed-files 模式下改用远端连续生成前后 hash，并与本地分支 hash 交叉比对；五份生成物完全一致 |
| REF-0010 | 5 | Resolved | 否 | Heartbeat 发送 containerd 全量镜像清单会随节点缓存无限放大 | 使用 cache epoch/revision/cursor 和受限 normalized inventory；不完整时只关闭镜像亲和 |
| REF-0011 | 5 | Resolved | 否 | containerd cache 是节点共享状态，单个 Fastlet 无法证明镜像可安全删除 | Fastlet 只生成保护索引/eviction plan，破坏性删除等待 node-scoped coordinator |
| REF-0012 | 6 | Resolved | 否 | Kubernetes 1.27 拒绝 CRD `uniqueItems` 的二次复杂度校验 | `maxItems=128`，Controller 平台路径 O(n) 去重 |
| REF-0013 | 6 | Resolved | 否 | 原地升级不能修改存量 Controller Deployment selector | 保留旧 Controller selector，Pod 增加 role label；新 FastPath selector/Service 严格选择 fastpath role |
| REF-0014 | 6 | Resolved | 否 | informer cache 不保证 FastPath CRD Create/status Patch 后立即读到 UID/assignment | 持久化状态机使用 direct API client；request ID 通过哈希 label 查询并用完整 annotation 复核 |
| REF-0015 | 6 | Resolved | 否 | Controller 与 FastPath 并发 Ensure 会让无 token Controller 被已有 reservation 拒绝，并触发旧 attempt 重用 | durable claim 可接管完全匹配 reservation；attempt 只由 CAS 内部根据高水位分配 |
| REF-0016 | 6 | Resolved | 否 | 不同 e2e 进程重置 namespace 计数，会撞到上一轮仍在 Terminating 的同名 namespace | namespace 加每个 test process 的随机 run ID 和进程内计数 |
| REF-0017 | 7 | Resolved | 否 | Fastlet 容器内创建的普通 `/run/netns` 路径对宿主 containerd 不可见 | 使用 hostPath 双向传播 `/run/fast-sandbox/netns -> /run/netns`，descriptor 同时记录 Fastlet path 和 host path |
| REF-0018 | 7 | Resolved | 否 | 单条 iptables 规则不能同时用两个 destination 表达“私网拒绝但 gateway 例外” | 在 Sandbox netns OUTPUT 中先 ACCEPT gateway，再 REJECT 私网 CIDR；privileged Linux gate 验证真实语法与顺序 |

## 详细记录

### REF-0001：remote-dev-run SSH alias

**证据**：

```text
ssh-fast -> Could not resolve hostname
fast     -> /home/fengjianhui.fjh/fast-sandbox
```

**结论**：本项目实际使用 SSH alias `fast`，远端目录 `~/fast-sandbox`。通过本地忽略配置固定，不修改包含通用默认值的 skill。

### REF-0002：远端非交互 PATH 缺少 Go

**证据**：

```text
remote PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:...
初始 command -v go -> empty
/usr/local/go/bin/go version -> go1.25.7 linux/amd64
go.mod: go 1.25.0, toolchain go1.25.5
```

**影响**：初始状态无法运行 `make test`、生成 protobuf/CRD、构建镜像和启动 Go e2e。

**结论**：不重复下载安装。已创建以下不覆盖目标的符号链接：

```text
/usr/local/bin/go -> /usr/local/go/bin/go
/usr/local/bin/gofmt -> /usr/local/go/bin/gofmt
```

**验证证据**：`go version go1.25.7 linux/amd64`，`gofmt --help` 正常返回。Go 1.25.7 满足仓库 `toolchain go1.25.5` 要求。

### REF-0003：Secure Runtime 环境能力不完整

**证据**：

```text
/dev/kvm exists
kind clusters: fsb-e2e-basic, fsb-e2e-kata
fsb-e2e-kata RuntimeClass: kata-qemu, kata-clh, kata-fc
host command -v runsc/containerd-shim-kata-v2 -> empty
```

**影响**：不影响控制面、container reference path 和大部分网络开发；会影响阶段 11 的 gVisor/Kata/BoxLite 完整支持声明。

**决策点**：如果现有 `fast` VM 无法提供某个 Runtime 的真实执行环境，需要最终决定扩展该 VM，还是增加专用 runtime 验收节点。当前继续推进不依赖这些 runtime 的阶段。

### REF-0004：专题文档旧表述

**证据**：

- 网络文档部分章节仍以 `Sandbox UID + service name` 为底层 route key；
- 网络文档旧 RuntimeDriver 列表仍包含 Exec/File/Logs；
- 多活文档的 Create 流程尚未合入 reservation-before-CRD 和 request ID 最终决策。

**结论**：四份专题文档已经按跨模块架构决策统一：

- 底层 route key 固定为 `Sandbox UID + target port`；
- service name 只作为 InfraProfile/SDK Adapter alias；
- RuntimeDriver 公共边界不含 Exec/File/Logs；
- RPC Create 固定为 `request_id -> Reservation -> CRD/status assignment CAS -> Ensure`；
- 明确容量拒绝不创建 Sandbox CRD。

根 README、ARCHITECTURE 和旧 Fast/Strong consistency 方案已增加迁移/Superseded 标记。

### REF-0005：`make test` 混入并行 e2e

**证据**：远端 master 执行 `make test` 后，普通包测试均通过，但同一个 `go test ./...` 同时启动了 `advancedfeatures/basicvalidation/cleanupjanitor/cliintegration/faultrecovery/lifecycle/scheduling` 七个 e2e test binary。它们又同时执行：

```text
kind load docker-image fast-sandbox/fastlet:dev --name fsb-e2e-basic
```

多个进程持续等待同一 kind/containerd 导入路径。该基线执行在保留进程树证据后终止，已向进程组发送 `SIGTERM`，并验证没有遗留测试进程。

**结论**：这是测试入口分层错误，不是产品基线单测失败。`make test` 保持“纯单测”语义，并新增明确的 `test-unit`；所有真实 e2e 仅从独立目标启动，整套 e2e 使用 Go package parallelism `-p 1`。这也是阶段 1 测试骨架的一部分。

**验证证据**：远端执行 `make generate && make manifests && make test-unit`，退出状态 `0`。生成前后 protobuf、DeepCopy 和两份 CRD manifest 的 SHA256 完全一致；纯单测覆盖 api/cmd/internal/pkg 与轻量 e2e support 包，不再启动 kind 或 container runtime。

### REF-0006：CRD immutable e2e 与 status writer 竞争

**证据**：`TestSandboxPoolCRDValidation` 创建 Pool 后，SandboxPoolController 很快更新 status，使测试持有的 `resourceVersion` 失效；首次 immutable runtime update 返回 Kubernetes `Conflict`，尚未到达 CEL validation。

**结论**：这是正常的并发 writer 行为。测试改为 `RetryOnConflict`，每次重新读取对象后只修改目标 spec。随后远端 e2e 验证以下行为均通过：

- `runtime + runtimeType` 同时出现被 CRD 拒绝；
- 非法 runtime enum 被拒绝；
- `spec.runtime` 更新被 immutable CEL 拒绝；
- `spec.sandboxResources` 更新被 immutable CEL 拒绝。

### REF-0007：旧 Pool 缺省 ResourceProfile 的迁移语义

**证据**：`sandboxResources` 是新增字段；Kubernetes 读取旧 v1alpha1 对象与新建但遗漏该字段的对象时，Go API 都呈现为 CPU/memory/PIDs 全零，当前单版本 API 无法可靠区分两者。

**影响**：直接将字段改为 required 会使存量 Pool 在 CRD 升级后不可更新；继续把全零传给 Fastlet 又会违反“Fastlet 必须实际执行固定资源限制”的已确认语义。

**当前过渡方案**：

- CPU/memory/PIDs 全部缺省时解析为固定兼容 profile：`1 CPU / 512Mi / 256 PIDs`；
- 只填写部分字段或任何字段非正数时 fail closed；
- hash、Fastlet Pod request 和 Ensure 请求统一基于解析后的 effective profile；
- 所有新 sample 和测试 fixture 改为显式 canonical `spec.runtime`，关键 fixture 显式写 `sandboxResources`。

**发布前决策**：进入 v1beta1 时选择“字段 required”或由转换/defaulting webhook 显式落盘默认值；不能长期依赖读取时隐式默认。

### REF-0008：RuntimeProfile hash 不能直接作为 label value

**证据**：阶段 3 container e2e 中 API Server 拒绝 Fastlet Pod：64 位 SHA-256 label value 超过 63 字符上限，Controller 因此持续重试 scale-up。

**结论**：Pod label `fast-sandbox.io/runtime-profile` 只承载可索引的 `profileVersion + 12位短 hash`；完整 SHA-256 保存在 annotation `fast-sandbox.io/runtime-profile-hash`，并继续通过 `FAST_SANDBOX_RUNTIME_PROFILE_HASH` 和 Ensure 请求传给 Fastlet 做严格校验。短值只用于查询，不参与安全或一致性判断。

### REF-0009：changed-files 远端镜像与 `verify-generated`

**证据**：用户将远端仓库恢复为 master 后，本地 Phase 2 commit 不存在于远端 Git object/baseline。`make generate && make manifests` 生成内容正确，但 `verify-generated` 内部的 `git diff --exit-code` 必然把 Phase 2 的 protobuf/CRD 变更视为未提交差异。

**结论**：本地分支仍执行正常 Git diff 审核；远端 changed-files 镜像采用“生成前 SHA-256 -> generate/manifests -> 生成后 SHA-256”，并将结果与本地分支交叉比对。阶段 3 的五份生成物前后及本地/远端 hash 均完全一致，随后 `make test-unit` 退出状态为 `0`。如果以后把本地 commit 同步为远端同名分支，可恢复直接使用 `make verify-generated`。

### REF-0010：Heartbeat 不能持续发送 containerd 全量镜像清单

**证据**：阶段 4 真实 containerd 恢复 e2e 中，Fastlet 的首次 v2 Heartbeat 直接返回 node containerd namespace 的全部镜像引用，除用户镜像外还包含 Kubernetes 系统镜像、大量 `import-*` alias 和 `sha256:*` 内容引用。单次响应已经远大于 admission/runtime 状态本身。

**影响**：如果多个 Fast-Path 副本每 10～30 秒持续拉取完整清单，节点镜像数量增长后会放大 Fastlet CPU、序列化、网络和 Registry 内存成本；而大量系统/content alias 对 Sandbox 镜像亲和没有调度价值。

**结论**：阶段 5 Heartbeat 使用 `cacheRevision + changed inventory`：revision 未变化不返回完整 inventory；inventory 只保留可用于 Sandbox 请求匹配的 normalized image reference/digest，排除裸 content digest、kind import alias 和无关系统镜像。首次握手或 revision gap 才发送受大小限制的 full snapshot，超限时 fail closed 为“缓存信息不完整”，不得影响 capacity/admission 正确性。

### REF-0011：containerd cache GC 是节点级共享状态，不能由单个 Fastlet 独立删除

**证据**：当前 ContainerdDriver 连接节点 `/run/containerd/containerd.sock` 的共享 `k8s.io` namespace。同一节点上的多个 Fastlet/Pool 能看到相同 image store；阶段 4 e2e 的 Fastlet Heartbeat 也实际观察到了该节点其他工作负载与平台镜像。

**影响**：如果每个 Fastlet 根据自己的 `warmImages/active/hot/infra` 保护集合独立执行 `ImageService.Delete`，一个 Pool 的 Fastlet 可能删除另一个 Pool 正在预热或使用的 image reference。进程内保护集合只能证明“本 Fastlet 不应删除”，不能证明“节点上无人需要”。

**当前结论**：阶段 5 实现 runtime-neutral ProtectionIndex 和 eviction plan，`PoolWarm/ActiveSandbox/InfraArtifact/HotImage` 全部 fail closed 保护；Fastlet 异步预热仍正常执行，但 ContainerdDriver 暂不执行破坏性的节点共享 image reference 删除。实际 containerd eviction 必须由后续 node-scoped coordinator 汇总同节点所有 Pool 的保护声明，或采用具备等价引用/lease 证明的机制；BoxLite 等私有 artifact store 可以在各自 driver ownership 边界内独立 GC。该限制不影响镜像亲和与创建正确性，但在 coordinator 完成前不宣称 containerd 主动回收能力。

### REF-0012：Kubernetes 1.27 拒绝 CRD `uniqueItems`，warmImages 在平台路径线性去重

**证据**：阶段 6 首次将生成后的 SandboxPool CRD 提交到真实 kind v1.27.3 apiserver 时，服务端拒绝 `spec.warmImages.uniqueItems: true`，错误明确指出该校验运行时复杂度会成为二次方；`kubectl --dry-run=client` 未发现该问题。

**结论**：CRD 继续以 `maxItems: 128` 限制输入规模，不使用 `uniqueItems`。Controller 在合成 Fastlet Pod 前按首次出现顺序 O(n) 去重并忽略空项，Fastlet 侧的保护索引继续做幂等归一化。重复 warm image 不影响 API 语义，也不会造成重复平台配置或缓存保护泄漏。

### REF-0013：角色拆分不能修改存量 Controller Deployment selector

**证据**：阶段 6 在已有集群原地应用双角色 Deployment 时，apiserver 拒绝给 `fast-sandbox-controller.spec.selector` 增加 `fast-sandbox.io/control-plane-role=controller`，因为 Deployment selector 是不可变字段。

**结论**：Controller Deployment 为兼容原地升级继续使用既有 `control-plane=controller-manager,app=fast-sandbox-controller` selector，Pod template 仍明确写入 `role=controller`；新建的 FastPath Deployment selector 和对外 Service selector 都包含 `role=fastpath`。因此旧 Controller 不会进入 FastPath Service，同时升级不需要破坏性重建 Controller Deployment。

### REF-0014：FastPath 持久化路径不能依赖 informer read-after-write

**证据**：真实 FastPath e2e 中，CRD Create 和 status assignment Patch 均已成功，但紧接着从 manager cache 读取到的对象仍缺少最新 assignment，RPC 返回 `persisted Sandbox UID and assignment are required`。fake client 会立即可见，原有单测无法暴露该问题。

**结论**：FastPath 及共享 durable orchestrator 使用 controller-runtime direct API client 完成幂等查询、CRD 写入和 status CAS；Watch cache 只承担 Registry membership/heartbeat 等最终一致视图。自定义 informer field index 不适用于 direct client，因此 Sandbox 增加 `sandbox.fast.io/request-id-hash` label，以 128-bit SHA-256 前缀查询候选，再以完整 request ID annotation 复核，兼顾 API Server 可查询性和碰撞安全。

### REF-0015：Controller 接管 FastPath reservation 与 assignment attempt 高水位

**证据**：3-slot Fastlet 的 40 并发真实 e2e 首次只成功 2 个请求。第三个请求已经 reservation，但 Controller 在 FastPath Ensure 前先 Reconcile；无 token Ensure 因三个 reservation 占满容量而被拒绝，Controller 清理 assignment。随后 FastPath 基于旧对象再次提交 attempt 1，被 durable high-water mark 1 拒绝。

**结论**：reservation 增加 claim namespace/name，并继续绑定 request ID、create hash、profile hash 和目标 Pod UID。CRD commit 后，Controller 发出的 Ensure 如果与 reservation 完全匹配，可以凭 durable claim 原子转换 reservation，无需持有 ephemeral token；不匹配时 fail closed。assignment CAS 不再接收调用方计算的 attempt，而是在每次权威读取后使用 `highWater+1`。修复后同一 40 并发 e2e 稳定得到 3 成功、37 个 ResourceExhausted，且只存在 3 个 CRD。

### REF-0016：e2e namespace 跨进程复用

**证据**：单独运行 `TestPortValidation` 后立即运行完整 gate，两个独立 Go test process 都分配 `fsb-e2e-port-7`；前一次 defer 只提交 namespace 删除，新一次在其 Terminating 期间创建对象被 apiserver Forbidden。

**结论**：SuiteEnv 在每个 test process 初始化 8 位随机 run ID，namespace 结构变为 `prefix-name-runID-counter`，并在 63 字符裁剪时保留唯一后缀。单测覆盖跨 SuiteEnv 实例不复用；完整 control-plane gate 随后通过。

### REF-0017：host containerd 的 netns 可见性

**证据**：Fastlet 在 Pod mount namespace 内执行 `ip netns add` 时，默认只在容器自身的 `/run/netns` 创建 bind mount；节点上的 containerd/runc 无法用该容器私有路径加入目标 netns。仅把 netns 元数据写入 CRD 或使用空 network namespace 都不能建立可恢复的真实数据面。

**结论**：container/gVisor RuntimeProfile 注入 hostPath `/run/fast-sandbox/netns`，在 Fastlet 内挂载为 `/run/netns` 并使用 `Bidirectional` mount propagation。NetworkSlot 同时保存 `/run/netns/<name>` 与宿主 `/run/fast-sandbox/netns/<name>`；Fastlet 用前者配置网络，containerd OCI spec 使用后者。状态目录 `/run/fast-sandbox/network` 以同路径 hostPath 挂载，使 resolver 和 descriptor 同时对 Fastlet、containerd 与后续 Janitor 可见。真实 kind e2e 已创建两个使用该路径的 containerd task，并在 Fastlet container restart 后恢复成功。

### REF-0018：Sandbox 私网隔离规则必须使用两条有序规则

**证据**：首次 privileged Linux integration gate 返回：

```text
iptables v1.8.13 (nf_tables): multiple --destination options not allowed
```

原表达式试图在一条 OUTPUT 规则中同时匹配 private CIDR 并排除 gateway。

**结论**：每个 Sandbox netns 中按顺序写入：

```text
ACCEPT destination=<gateway>/32
REJECT destination=<private CIDR>
```

外部目标仍按默认 allow 出网；Fastlet Proxy 到 Sandbox 的入向连接不经过该 OUTPUT 拒绝；Sandbox 对 gateway 的响应先命中 ACCEPT；Sandbox 到同 Fastlet 的其它私有 IP 命中 REJECT。privileged Docker gate 和双 Sandbox Kubernetes e2e 均已通过。

### REF-0019：FastPath reservation 不能在 Controller 抢先 assignment 后立即丢弃

**证据**：阶段 8 双 Fastlet、每 Pod 容量 1 的真实 e2e 中，第一个 Sandbox 占满 Fastlet A；第二个 FastPath 请求已经在 Fastlet B 获得 reservation，但 Controller 在 CRD Create 和 FastPath assignment CAS 之间抢先把 CRD 指向缓存中仍排名靠前的 Fastlet A。FastPath 看到 CAS loser 后立即丢弃 B 的 reservation，随后 A 返回 `CapacityRejected`，导致一个本可成功的 Create 错误返回上层。

**结论**：CAS winner 仍是第一次 Ensure 的权威目标，但 FastPath 在请求结束前保留自己已获得的 reservation。只有 winner Ensure 成功，才取消多余 reservation；如果 winner 返回明确可重调度的本地 admission 拒绝，则 CAS 清除该 rejected assignment，并尝试把同一 CRD 指向 reservation Fastlet 后消费 token。assignment attempt 和 route generation 都必须前进。任何 unknown outcome 都不得触发迁移，以免创建双 runtime。fake-client 竞态回归和双 Fastlet 真实 e2e 均已通过。

### REF-0020：Fastlet Proxy RouteStore 是可重建的易失状态，sidecar 重启必须主动通知 Fastlet Control

**证据**：Fastlet Control restart 会执行 runtime/network recovery 并重新 ApplyRoute；但 Fastlet Proxy 是独立 sidecar，单独重启时其内存 RouteStore 清空，而 Fastlet Control 进程和 readiness 原本保持不变。此时 Sandbox Status 仍为 DataPlaneReady，所有请求会稳定得到 route not found。

**结论**：RouteStore 不另建第三份持久化真相；Fastlet Control 的 runtime inventory + durable NetworkState/AccessDescriptor 是恢复来源。Control 与 Proxy 通过 UDS `WatchRoutes` 保持长连接：断线立即撤销 Fastlet route readiness，重连后先 Snapshot，删除 orphan route，再全量 Apply 当前 runtime routes，成功后恢复 Ready。Fastlet Proxy 本地 generation tombstone只负责单进程内延迟消息 fencing；进程重启后的安全性来自当前 runtime 全量重建，旧 UDS 连接不会跨进程存活。真实 container restart e2e 已验证原 route 恢复。

### REF-0021：route credential 使用非对称短期签名，开发 key 不进入生产默认路径

**证据**：如果 Fastlet Proxy 持有与 FastPath 相同的 HMAC secret，则任意被攻陷的 Fastlet Pod 都能为其它 Sandbox/Fastlet 伪造 route token；如果代理每次回查 FastPath，则数据面延迟和控制面可用性被绑定。

**结论**：第一阶段使用 Ed25519 本地验签。FastPath 独占私钥；Sandbox Proxy 和所有 Fastlet Proxy 仅获得公钥集合。token 绑定 namespace、Sandbox UID、target port、Fastlet Pod UID、assignment attempt、route generation、expiration 和 nonce，默认 TTL 5 分钟；reset/reassignment 不等待 TTL，而由 generation/attempt 立即失效。缺失或非法 key 时进程启动 fail closed。验签配置接受逗号分隔的公钥集合，rotation 顺序固定为：代理先发布 `old,new`，FastPath 再切换新私钥，等待最大 token TTL 后移除旧公钥。`config/manager/dev-route-keys.yaml` 使用公开 RFC 8032 test vector，标记 `development-only` 且仅由 e2e manager 显式应用；生产必须由 secret manager 提供同名 Secret。
