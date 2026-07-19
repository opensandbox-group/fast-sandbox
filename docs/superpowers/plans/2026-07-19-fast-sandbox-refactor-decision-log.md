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
