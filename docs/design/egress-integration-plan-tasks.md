# Egress 集成施工任务清单（转交实现）

> 文档类型：施工任务清单（转交实现）
>
> 日期：2026-08-29
>
> 方案：见 [egress-integration-plan.md](egress-integration-plan.md)
> （本清单每一项对应方案章节；分歧以方案文档为准）。
>
> 依赖：OpenSandbox PR #1678（egress 实现，未合并）；fast-sandbox #30
> （Actions 协议/CRD，已合入）。**egress 镜像固定为
> `docker.io/opensandbox/egress:latest`（需求方提供，直接写死使用）**，
> 本清单不包含镜像构建。
>
> **本期范围：仅 network policy 通道（actions 驱动）**——actions 生命周期
> （SET_BINDING deny-first + `binding.input` 携带策略 / hooks /
> REMOVE_BINDING）+ A1 host-process 交付模式。**credential 通道不在本期**
> （credential vault 端点/凭据存储 + fastlet-proxy host upstream / UID
> 传播 / egress route parsing 随 vault 一起延后），涉及处已标注。

---

## 任务总览

| # | 任务 | 产出物 | 依赖 |
|---|------|--------|------|
| 1 | 协议交叉验证 | 核对结论（文档/测试） | — |
| 2 | A1：host-process 交付模式 | catalog + CRD + sandbox-init 生成逻辑改动 | — |
| 3（延后） | A2+A3：proxy host upstream + UID 头 | fastlet-proxy 改动（credential 通道） | — |
| 4（延后） | A4：egress route parsing | fastlet-proxy 改动（credential 通道） | 3 |
| 5 | Pool 样例 + egress 容器部署形态 | `pool-firecracker-egress.yaml` + fastletTemplate 更新 | 2 |
| 6 | 集成测试（verify-egress） | integration-env.sh 扩展 | 5 + egress 镜像 |
| 7 | 回归与清理 | 全量测试绿 + 文档 | 全部 |

---

## 任务 1：协议交叉验证（集成测试第一项的前置）

**背景**：fast-sandbox `internal/protocol/action` 与 OpenSandbox
`components/egress/pkg/actionhandler` 是两侧独立实现的同一协议
（`sandbox.fast.io/actions/v1`）——必须先核对字节级一致性。

- [x] 拉取 egress 源码：`Pangjiping/OpenSandbox` @ `feat/egress-actions-handler`
  （pin `460b1cb`），路径 `components/egress/pkg/actionhandler/`
- [x] 逐项核对（核对表已产出：`docs/guides/egress-protocol-verification.md`）：
  - [x] `apiVersion` 常量与校验（`sandbox.fast.io/actions/v1`）
  - [x] Operation 枚举：`SET_BINDING` / `LIFECYCLE_HOOK` / `REMOVE_BINDING`
    （fast-sandbox `internal/protocol/action/types.go`）
  - [x] LifecycleHook 枚举：`sandbox.runtime-ready` /
    `sandbox.data-plane-ready`
  - [x] 请求字段：`invocationId`、`sandbox{uid,name,namespace}`、
    `revision{specGeneration,runtimeInstanceId,attachmentId,routeGeneration}`、
    `attachment.network{ip,gateway,privateCidr,hostVeth}`
  - [x] `binding.input` 语义（null vs 字符串"null"——两侧 pointer 处理）
    ——本期策略体经此字段传递，已核对两侧编码约定（egress
    `ParsePolicy` 接受 `""`/`"null"`/`"{}"` → 默认 deny）
  - [x] 错误语义：未知 handler / 无 pending 策略的 data-plane-ready（409）等
  - [x] `GET /_fastlet/v1/actions/status` 的 `HandlerStatus` 字段
    （apiVersion/ready/instanceId/message）
- [x] 差异项全部记录（核对表 §5：D1-D5——类型/omitempty/status message/
  ready 语义/策略编码；实现者不修改协议，交回评审决策）
- [x] 快速构建验证：egress 侧 `go build ./components/egress/...` 通过
  （@ `460b1cb`，不产出镜像——镜像由需求方提供）

**验收**：核对表完成；无未记录的协议差异；两侧独立实现可互操作
（留待任务 6 实测——`verify-egress` 阶段 0/2 已含 status 端点字节断言）。

## 任务 2：A1 host-process 交付模式

**位置**：`internal/catalog/runtime/catalog.go` +
`internal/fastlet/`（sandbox-init supervisor 生成逻辑）

- [ ] `InfraDeliveryMode` 新增 `InfraDeliveryHostProcess`（"host-process"）
- [ ] 语义：host-process 组件进入 Pool revision（编译进 infra plan），但
  **不出现在 in-sandbox 的 sandbox-init supervisor 配置**（guest 侧不
  感知）；readiness 探测目标 = **Pod-netns listener**（非 sandbox IP）
- [ ] 定位 sandbox-init supervisor 配置生成处（`internal/fastlet/infra/`
  或相关），host-process 组件排除逻辑
- [ ] 定位 readiness 探测目标解析（`internal/fastlet/sandbox/readiness.go`
  或 infra lifecycle），host-process 组件改为探测 Pod-netns
- [ ] 单测：
  - host-process 组件不进 supervisor 配置（生成断言）
  - readiness 探测目标解析为 Pod-netns（不 dial sandbox IP）
  - 非 host-process 组件行为不变（回归）

**验收**：单测绿；无 host-process 声明时行为与现状完全一致。

## 任务 3（延后）：A2+A3 proxy host upstream + UID 头

> **本期不做**：credential 通道（fastlet-proxy host upstream、UID 头）
> 随 credential vault 延后。任务保留以备排期。

**位置**：`internal/dataplane/fastletproxy/proxy.go`

- [ ] A2：egress 目标类型——proxy 对 egress route 转发到
  **Pod-netns listener（127.0.0.1:18080）**（区别于现有 DirectIP/
  LocalForward 目标语义）
- [ ] A3：proxy 注入 `X-Fast-Sandbox-Uid` 头（值为 sandbox 标识）；
  **不参与 `stripRouteHeaders`**（route 剥离逻辑不删该头）；
  route credential 头**维持现有剥离**（proxy 已校验，egress 不依赖）
- [ ] 单测（`proxy_test.go`）：
  - egress 目标转发到 127.0.0.1:18080（fake upstream 断言目标与头）
  - UID 头注入正确（sandboxId）；剥离逻辑不剥离 UID 头
  - 非 egress 路由（DirectIP/LocalForward）行为不变（回归）

**验收**：单测绿；既有 proxy 测试全绿。

## 任务 4（延后）：A4 egress route parsing

> **本期不做**：同任务 3，随 credential 通道延后。

**位置**：`internal/dataplane/fastletproxy/proxy.go`（`parseTarget`）

- [ ] `parseTarget` 增加 `/v1/sandboxes/{sandboxId}/egress/*` 分支：
  - 解析 sandbox 路由（sandboxId 定位）
  - 校验 route credential（与现有分支一致）
  - 目标 = egress listener（127.0.0.1:18080，host 转发语义见任务 3）
- [ ] 与 `ResolveEndpoint` 的 egress 目标语义对齐（两侧必须一致——
  OpenSandbox 侧 `fastpath_client.resolve_endpoint` 的 egress target）
- [ ] 单测：路径匹配、凭据校验、错误分支（未知 sandbox/凭据不匹配/
  非法路径）、非 egress 前缀行为不变

**验收**：单测绿；`/v1/sandboxes/`、`/v2/sandboxes/` 既有分支回归。

## 任务 5：Pool 样例 + egress 容器部署形态

- [ ] 新增 `config/samples/pool-firecracker-egress.yaml`（基于
  pool-firecracker）：
  - `actionHandlers: [{name: egress, targetHTTPPort: 18080,
    hooks: [sandbox.runtime-ready, sandbox.data-plane-ready]}]`
  - egress 容器进 fastletTemplate（host-process，共享 Pod netns，
    镜像 **`docker.io/opensandbox/egress:latest`** 直接写死）
  - egress 的 host-process 组件声明（`infraComponents[].delivery:
    host-process`，无 artifact，endpoint 18080 + tcpConnect 探活）
- [ ] 确认 fastlet 对 actionHandlers 的投递在 firecracker runtime 下生效
  （#30 已实现，声明即用——验证 `recordActionHook` 的 data-plane-ready
  在 restore + 网络就绪后触发）
- [ ] 确认策略经 `binding.input` 到达 egress：server 在 Sandbox
  `spec.actionBindings[egress].input` 写入策略（opaque string 编码），
  fastlet 的 SET_BINDING 原样携带（任务 1 核对 `binding.input` 语义）
- [ ] 集成环境文档同步：`docs/guides/firecracker-integration-environment.md`
  补 egress 部署步骤

**验收**：Pool apply 后 egress 容器运行、actionHandlers 状态 Pending
（等待 sandbox 创建触发 SET_BINDING）。

## 任务 6：集成测试（verify-egress，扩展 integration-env.sh）

**前置**：`docker.io/opensandbox/egress:latest`（需求方提供）；
`kind load docker-image` 进集群（或在 fastlet pod 中直接可拉取）。
策略通道为本期 **actions 通道**（binding.input），不依赖 proxy route。

- [x] `integration-env.sh` 新增 `verify-egress` 阶段（复用现有 up 环境）：
  - [x] **0. 协议一致性实测**：egress 容器 ready + `actions/status` 端点
    字节断言（apiVersion/ready/instanceId）——任务 1 核对表的实测闭环
  - [x] **1. 生命周期**：Sandbox 创建（`--action egress=<deny 策略>`）→
    SET_BINDING（deny-first + binding.input 策略）→ runtime-ready →
    data-plane-ready（egress 日志 "policy active"）→ binding Ready
  - [x] **2. 真实数据面断言**：guest 内 `wget 1.1.1.1` 在 deny 策略下被
    阻断（fail-closed 窗口 + 规则真实生效）
  - [x] **3. 策略更新（patch）**：`kubectl patch` actionBindings.input
    allow 1.1.1.1 → fastlet 重投 SET_BINDING（日志计数增长）→ guest
    出网恢复；patch 回 deny → 再次阻断
  - [x] **4. per-subject 隔离**：同 fastlet 第二个 sandbox（allow）出网
    成功、第一个（deny）仍阻断
  - [x] **5. 重启恢复**：egress 容器 `kill 1` → 重启 → fastlet 重放
    （SET_BINDING 计数增长）→ deny 策略重启后仍生效
  - [x] **6. 删除**：fastctl delete → egress 日志 REMOVE_BINDING complete
  - [x] **7. 清理**：egress pool 删除 + 无残留 subject 规则（nft 断言）
- [x] 日志/回收沿用既有规范（logs/、failure 落盘、down 无残留）

**验收**：`verify-egress` 在集成环境全绿（生命周期 + 数据面强制 + 策略
更新 + 隔离）——脚本已实现，待需求方提供 egress 镜像后在集成环境实测。

## 任务 7：回归与清理

- [ ] `go build ./...`、`go test ./internal/... -count=1 -race`、
  `go vet ./...` 全绿
- [ ] 无 egress 声明的既有 Pool 行为不变（集成环境回归：不带 egress 的
  pool-firecracker 全链路仍绿）
- [ ] egress sidecar profile（OpenSandbox 既有模式）不受影响
  （跨仓库回归说明）
- [ ] 更新方案文档/集成环境文档：实测值、协议核对结论、egress 镜像
  tag、已知差异

**验收**：全量测试绿；文档同步。

---

## 交付物汇总

| 产出 | 路径 |
|------|------|
| 协议核对表 | `docs/guides/egress-protocol-verification.md` |
| A1 | `internal/catalog/runtime/catalog.go` + CRD `delivery` + sandbox-init 生成改动 + 单测 |
| A2+A3（延后） | `internal/dataplane/fastletproxy/proxy.go` + 单测（credential 通道） |
| A4（延后） | `internal/dataplane/fastletproxy/proxy.go`（parseTarget）+ 单测（credential 通道） |
| Pool 样例 | `config/samples/pool-firecracker-egress.yaml` |
| 集成测试 | `scripts/integration-env.sh`（+verify-egress） |
| 文档 | 集成环境指南 + 方案文档实测更新 |

## 验收标准（整体）

1. 任务 1-7 勾选完成，每项验收通过；
2. 协议交叉验证闭环（核对表 + 实测）；
3. `verify-egress` 8 项断言全绿（含 firecracker src=slot.IP 观测）；
4. 无 egress 声明的 Pool 与既有行为零影响；
5. 日志/回收要求沿用集成环境既有规范。

## 参考

- 方案：`docs/design/egress-integration-plan.md`
- egress 源码：`Pangjiping/OpenSandbox @ feat/egress-actions-handler`
  （pin `460b1cb`，`components/egress/`；镜像由需求方构建提供）
- fast-sandbox 已有：#30（`internal/protocol/action`、`ActionHandlers`
  CRD、fastlet 投递/重放）、#28（per-clone netns，egress source-IP
  dispatch 前置）、#29（集成环境 `integration-env.sh`）
- 协议：OSEP-0022（fast-sandbox 副本
  `docs/design/egress-integration-plan.md` 关联；OpenSandbox 侧 OSEP 原文）
