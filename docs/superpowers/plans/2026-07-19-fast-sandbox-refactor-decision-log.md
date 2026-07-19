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
