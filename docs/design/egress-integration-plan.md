# Egress 集成方案：fast-sandbox × OpenSandbox（OSEP-0022 / PR #1678）

> 文档类型：技术方案（集成 + 测试）
>
> 日期：2026-08-29
>
> 依赖：OpenSandbox PR #1678（egress fast-sandbox profile 改造，待合并）；
> fast-sandbox #30（Sandbox Actions 协议与 CRD，已合入 master）。
>
> 关联：OSEP-0022（多沙箱 egress 控制面）、opensandbox-group/OpenSandbox#1582、
> fast-sandbox #28（per-clone netns，egress source-IP dispatch 前置）。
>
> **本期范围：仅 network policy 通道（actions 驱动）**——actions 生命周期
> （SET_BINDING deny-first + `binding.input` 携带策略 / hooks /
> REMOVE_BINDING）。**credential 通道不在本期，延后处理**：
> credential vault（`/credential-vault` 端点与凭据存储）以及配套的
> fastlet-proxy host upstream / UID 传播 / egress route parsing（A2/A3/A4）
> 均随 vault 一起延后（下文对 PR #1678 的背景描述如实保留）。

---

- [背景与 PR #1678 要点](#背景与-pr-1678-要点)
- [现状盘点：fast-sandbox 已具备 / 缺失](#现状盘点fast-sandbox-已具备--缺失)
- [集成架构](#集成架构)
- [工作分解](#工作分解)
- [测试方案](#测试方案)
- [验收标准](#验收标准)
- [风险与依赖](#风险与依赖)
- [待决策项](#待决策项)

## 背景与 PR #1678 要点

OSEP-0022 原方案：egress 通过**观察 fastlet slot-store 文件**（
`/run/fast-sandbox/network/*.json`）驱动 subject 生命周期。PR #1678 将
该通道改为 **fastlet Sandbox Actions Handler 协议**
（`sandbox.fast.io/actions/v1`）：

- fastlet 经 Pod-loopback HTTP 投递 `SET_BINDING` / `LIFECYCLE_HOOK` /
  `REMOVE_BINDING`，egress 进程作为 Handler；
- `SET_BINDING`：注册 subject 并 deny-first（attachment 提供
  IP/gateway/veth/CIDR）；`binding.input` 携带策略（opaque string）；
  null input 回退 deny-first；
- `LIFECYCLE_HOOK`：`sandbox.runtime-ready` 确认、`sandbox.data-plane-ready`
  应用策略 → active；无 pending 策略 fail-closed（409）；
- `REMOVE_BINDING`：终态清理（fence 不匹配忽略）；
- 删除文件观察（pkg/slotsource/sandboxnft/resolvrewrite 共 -3133 行）；
- 凭据通道（PR #1678 设计）：仍走 proxy route `/credential-vault`
  （memory-only）——**不在本期范围，延后处理**，见"本期范围"。

**fast-sandbox 侧要求（PR #1678 Notes）**：
1. Pool `actionHandlers` 声明 egress（`targetHTTPPort 18080`，hooks：
   runtime-ready + data-plane-ready）；
2. OSEP-0022 的"四内部新增"：**host-process 交付模式（本期 A1）**；
   fastlet-proxy host upstream / UID 传播 / egress route parsing
   （credential 通道，随 vault 延后）。

## 现状盘点：fast-sandbox 已具备 / 缺失

### 已具备（#30 已合入 master）

| 项 | 位置 | 说明 |
|----|------|------|
| Actions 协议 | `internal/protocol/action` | 与 PR #1678 同一 `sandbox.fast.io/actions/v1`：SET_BINDING/LIFECYCLE_HOOK/REMOVE_BINDING、NetworkAttachment、HandlerStatus |
| Pool CRD | `api/v1alpha2/sandboxpool_types.go` | `spec.actionHandlers`（targetHTTPPort/hooks/校验：名称不可删改） |
| fastlet 投递 | `internal/fastlet/action/manager.go` + `sandbox/actions.go` | SetBinding 在 runtime 创建前投递；RecordHook/ReachHook（runtime-ready/data-plane-ready）；Handler 重启重放；bindings 状态机（Pending→…） |
| data-plane-ready 触发 | `internal/fastlet/sandbox/infra_lifecycle.go` / `admission.go` | 数据面就绪时投递 hook（sequence 2） |
| attachment 网络信息 | `sandbox/actions.go:actionAttachment` | 从 SandboxMetadata（slot）取 IP/gateway/veth/CIDR |
| 错误码 | `fastletapi.ErrorActionUnavailable` 等 | 协议映射就绪 |

### 缺失（本方案工作）

| 项 | 说明 | 归属 |
|----|------|------|
| **host-process 交付模式** | `InfraDeliveryMode` 新增 `host-process`：Pool revision 携带、但不进 in-sandbox 的 sandbox-init supervisor 配置；egress daemon 由 FastletTemplate 部署；readiness 探测 Pod-netns listener | fast-sandbox（本期） |
| **fastlet-proxy host upstream** | egress route 转发到 Pod-netns listener（`127.0.0.1:18080`）而非 sandbox Access 地址 | fast-sandbox（延后，credential 通道） |
| **UID 传播** | proxy 注入 `X-Fast-Sandbox-Uid`（subject 标识） | fast-sandbox（延后，credential 通道） |
| **egress route parsing** | `parseTarget` 增加 `/v1/sandboxes/{sandboxId}/egress/*` 分支（凭据校验 + 目标 egress） | fast-sandbox（延后，credential 通道） |
| **egress 实现/部署** | OpenSandbox fast-sandbox egress（PR #1678）+ 集成环境部署（host 域组件，共享 slot-store 不需要了——actions 驱动） | OpenSandbox |
| **firecracker 衔接验证** | data-plane-ready 时机（restore + 网络就绪）；clone 网络下 egress 流量 src=slot.IP（#28 已保证唯一性） | 集成测试 |

## 集成架构

```text
┌─ Kind 集群（Pod netns = fastlet Pod 网络）────────────────────────────┐
│                                                                       │
│  fastlet pod                                                          │
│   ├─ fastlet（特权）：                                                │
│   │    SET_BINDING（attachment + binding.input 策略）/ hooks          │
│   │    runtime-ready / data-plane-ready ──actions HTTP──▶ egress      │
│   │                                         │          ▼              │
│   │   （fastlet-proxy egress route：credential 通道，本期延后）        │
│   │                                         │    (Handler，Pod netns)  │
│   └─ egress（host-process 交付，Pod netns）◀────────────┘              │
│        ├─ deny-first（SET_BINDING 即装 nft；input 暂存策略）           │
│        ├─ data-plane-ready 应用策略 → active                          │
│        ├─ DNS proxy（:53 网关绑定）+ nft per-subject set              │
│        └─（credential vault：本期不做，延后）                          │
│                                                                       │
│   sandbox 流量：guest ──► slot netns（#28：SNAT 后 src=slot.IP 唯一）  │
│                  ──► host forward hook（egress 主 enforcement）        │
└───────────────────────────────────────────────────────────────────────┘
```

数据流（对应 OSEP-0022 Sequences，通道为 actions）：

```text
创建：server（controller）写入 sandbox.spec.actionBindings[egress].input
      → fastlet 预投递 SET_BINDING（attachment + binding.input 策略，
      deny-first + 策略暂存）
      → runtime-ready hook（确认）
      → data-plane-ready hook（应用策略 → active）
删除：REMOVE_BINDING（fence 校验）→ 卸载
```

## 工作分解

### A. fast-sandbox 内部新增

| 子项 | 改动 | 测试 |
|------|------|------|
| A1 host-process 交付模式 | `internal/catalog/runtime/catalog.go` 新增 `InfraDeliveryHostProcess`；CRD `infraComponents[].delivery: host-process`（artifact 可选）；`sandbox-init` supervisor 生成时排除 host-process 组件；readiness 探测目标为 Pod-netns listener | 单测：plan 编译/排除/探测目标 |
| A2-A4（延后） | fastlet-proxy host upstream / UID 传播 / egress route parsing——credential 通道，随 vault 延后 | — |

### B. egress 构建与集成（直接集成，非 mock）

**直接用 PR #1678 head 分支构建真实 egress**（已确认可构建）：

```text
repo:    Pangjiping/OpenSandbox @ feat/egress-actions-handler（pin commit 460b1cb）
路径:    components/egress/（fastsandbox.go / fastsandbox_actions.go / pkg/actionhandler / pkg/fastsandboxnft）
产物:    egress 镜像（集成环境 `kind load docker-image`）
```

- **pin commit 缓解 PR 未合入的演进风险**：PR 合并后切官方 tag/镜像；
- **交叉验证（mock 无法替代的核心价值）**：fast-sandbox
  `internal/protocol/action` 与 OpenSandbox `pkg/actionhandler` 是两侧
  独立实现的同一协议——集成测试第一项即字节级一致性核对
  （apiVersion/operation 枚举/attachment 字段/错误语义）。

### C. Pool 声明（集成环境样例）

```yaml
spec:
  actionHandlers:
  - name: egress
    targetHTTPPort: 18080
    hooks: [sandbox.runtime-ready, sandbox.data-plane-ready]
  infraComponents:
  - name: egress
    delivery: host-process
    process:
      command: ["egress"]
      healthCheck:
        tcpConnect: {}
    endpoint: {protocol: HTTP, port: 18080}
```

- 新增 `config/samples/pool-firecracker-egress.yaml`（在 pool-firecracker
  基础上 + actionHandlers + host-process 组件 + egress 容器进
  fastletTemplate）；
- 策略内容由 server 写入 Sandbox 的 `spec.actionBindings[egress].input`，
  fastlet 经 `SET_BINDING.binding.input` 投递（#30 已有，声明即生效）。

### D. egress 侧（OpenSandbox，跨仓库）

- egress 构建：`Pangjiping/OpenSandbox @ feat/egress-actions-handler`
  （pin `460b1cb`）的 `components/egress/`——直接集成，见 B 节；
- 集成环境部署：egress 容器进 fastlet pod（host-process 交付，共享 Pod
  netns），监听 18080 + DNS 网关端口。

### E. firecracker 衔接验证

- **data-plane-ready 时机**：restore 完成 + 网络就绪（guest eth0 + slot
  netns 数据面 #28）后投递——确认 infra 生命周期在 firecracker 模式下
  的触发点正确；
- **egress dispatch**：guest 出网经 netns SNAT（#28）→ host forward 处
  src=slot.IP 每实例唯一 → egress 按 source IP 分 subject（OSEP-0022
  前置的实测证明）；
- **兄弟隔离**：egress 的 nft per-subject 策略 vs netns FORWARD 隔离
  （#28）双保险。

## 测试方案

### 单元测试（fast-sandbox 侧）

- **A1**：host-process 组件不出现在 sandbox-init supervisor 配置；
  readiness 探测目标解析为 Pod-netns listener；无 host-process 声明时
  行为与现状一致（回归）；
- A2/A3/A4（proxy 通道）单测：延后，随 credential 通道排期。

### 集成测试（Kind 集成环境，扩展 integration-env.sh）

新增 `verify-egress` 阶段（复用现有 up 环境）：

1. **部署**：egress 容器进 fastlet pod（host-process），Pool 声明
   actionHandlers；
2. **生命周期断言**：
   - Sandbox 创建：SET_BINDING（attachment + binding.input 策略）→
     deny-first（创建后、策略应用前 sandbox 无法出网）；
   - runtime-ready / data-plane-ready hooks 送达（egress 侧日志/状态）；
   - data-plane-ready 应用策略 → active → sandbox 出网恢复；
3. **per-subject 隔离**：同一 fastlet 上两个 sandbox 不同策略（allow
   A / deny B）分别验证 DNS + 出网；
4. **firecracker 模式**：egress 流量 src=slot.IP（`nft trace`/计数）；
   两个 clone sandbox 各自受控；
5. **fail-closed 时序**：策略晚于绑定到达（binding.input 为 null/无
   pending 策略）→ 期间 deny；data-plane-ready 时无 pending 策略
   → 409（fail-closed）；策略到位后恢复；
6. **删除**：REMOVE_BINDING → 规则卸载、subject 释放；重复删除幂等；
7. **重启恢复**：egress 重启 → fastlet 重放（Handler restart replay）；
   ApplyReset 后重新 deny → server 重写 actionBindings → 重推 → active；
8. **清理**：全部 sandbox 删除后 egress 无残留规则（nft list 断言）。

### 回归

- 无 egress 声明的既有 Pool 行为不变（actions 未配置时零影响）；
- container/gvisor/kata 的既有 egress（sidecar profile）不变
  （OSEP-0022：sidecar 模式 untouched）。

## 验收标准

1. A1（host-process 交付模式）单测全绿（-race + vet）；
2. `verify-egress` 在集成环境全绿：生命周期/隔离/firecracker 模式/
   fail-closed/删除/重启 8 项断言通过；
3. firecracker egress 流量 src=slot.IP 每实例唯一（nft 观测证据）；
4. 无 egress 声明的 Pool 与既有 egress sidecar 回归全绿；
5. 日志/回收要求沿用集成环境既有规范（logs/、down 无残留）。

## 风险与依赖

| 项 | 影响 | 缓解 |
|----|------|------|
| OpenSandbox PR #1678 未合并 | 代码演进风险 | **直接集成**（pin commit 460b1cb 构建，已确认可构建）；PR 合并后切官方 tag |
| 两侧独立实现同一协议（fast-sandbox action vs egress actionhandler） | 字节级不一致 | **交叉验证列为集成测试第一项**（apiVersion/枚举/attachment/错误语义核对）——mock 无法发现的问题，真实联调直接暴露 |
| 策略经 actions 通道（binding.input）传递 | input 语义（opaque string / null）两侧不一致 | 任务 1 协议核对表重点核对 `binding.input` 的 null vs 字符串"null"、策略体编码（base64/JSON） |
| A1 跨 CRD/catalog/fastlet 三个子系统 | 改动面 | 单测先行，每项独立提交 |
| data-plane-ready 在 firecracker 模式的时机语义 | egress 生效窗口 | 集成测试断言（fail-closed 窗口验证） |
| firecracker clone 网络与 egress dispatch 联调 | source-IP 唯一性 | #28 已保证；集成测试 nft 观测确认 |
| **credential 通道延后**（vault + A2/A3/A4） | 本期策略完全依赖 actions 通道；后续策略更新频度受限（需更新 actionBindings） | 本期只做 actions 通道；vault/route 通道另行排期 |

## 待决策项

1. **A1 落地顺序**：单测先行，一次提交；
2. **egress 容器形态**：进 fastlet pod（共享 Pod netns，host-process）
   vs 独立 DaemonSet——host-process 交付模式定义后定；
3. **egress 镜像来源**：直接构建 PR head（起步）vs 等官方镜像发布；
4. **credential 通道（vault + A2/A3/A4）落地时机**：本期明确延后；
   随 egress 侧 vault 排期另行规划。
