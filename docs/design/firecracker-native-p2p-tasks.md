# native+P2P（DART 集成）施工任务清单（转交实现）

> 文档类型：施工任务清单（转交实现）
>
> 日期：2026-08-29
>
> 方案：见 [firecracker-native-p2p-plan.md](firecracker-native-p2p-plan.md)
> （本清单每一项对应方案 A-G；分歧以方案文档为准）。
>
> 设计定案：#21（P2P = DART 集成）、细节文档 §6。

---

## 任务总览

| # | 任务 | 产出物 | 依赖 |
|---|------|--------|------|
| 1 | presigned URL（s3client 扩展） | s3client 改动 + 单测 | — |
| 2 | agent 拉取路径改造（DART 前缀 + fallback） | pull.go 改动 + 单测 | 1 |
| 3 | DART 进程管理（agent 子进程） | 进程管理 + 单测 | 2 |
| 4 | DART 二进制集成 + 部署清单（headless Service） | 镜像构建阶段 + config 更新 | 3 |
| 5 | 观测（block_source 指标） | 指标采集 + status 摘要 | 4 |
| 6 | 集成环境验证（2 节点 verify-p2p） | integration-env.sh 扩展 | 5 |
| 7 | 回归与文档 | 全量测试绿 + 实测记录 | 全部 |

---

## 任务 1：presigned URL（SigV4 query 签名）

**位置**：`internal/runtime/firecracker/agent/s3client.go`

- [ ] 新增 `presign(key string, ttl time.Duration) (string, error)`：
  - SigV4 query 签名：`X-Amz-Algorithm=AWS4-HMAC-SHA256`、
    `X-Amz-Credential=<ak>/<scope>`、`X-Amz-Date`、`X-Amz-Expires`、
    `X-Amz-SignedHeaders=host`、`X-Amz-Signature`
  - **与现有 header 签名（`sign`）共用 signing key 派生**
    （`deriveSigningKey`/`hmacSHA256` 复用）；canonical request 一致，
    仅签名载体不同（header vs query）
- [ ] presigned URL 含完整 path（bucket/key）与 prefix
- [ ] 单测（`s3client_test.go`）：
  - query 参数齐全（6 个 X-Amz-*）
  - `X-Amz-Expires` = ttl
  - 与 header 签名的 canonical request 一致性（同一 key/date 下
    signature 可复算）
  - 不同 key → 不同 URL（含 prefix 拼接正确）

**验收**：单测绿；presign 与 sign 共用派生逻辑无重复实现。

## 任务 2：agent 拉取路径改造（DART 前缀 + fallback）

**位置**：`internal/runtime/firecracker/agent/{pull,index,manifest}.go`

- [ ] 配置：agent 增加 DART 地址（env `FAST_SANDBOX_DART_ADDR`，默认空 =
  直连模式；`NewClient` Option `WithDartAddr`）
- [ ] 下载路径改造（index/manifest/artifact 三类对象下载统一走新路径）：
  - DART 模式：`presign(key, ttl)` → `GET <dartAddr>/dart/<presigned-url>`
  - 直连模式（DART 地址空 或 DART 连接失败）：现有 header 签名直连
    **保留为 fallback**
- [ ] fallback 语义：DART 请求连接失败/超时 → 直连 S3（不因 DART 可用性
  阻塞拉取）；DART 返回 4xx/5xx → 记录并 fallback（4xx 需判断：DART 对
  未知对象的 404 与源 404 语义）
- [ ] 幂等/整对象 sha256 校验/落盘逻辑不变
- [ ] 单测（`pull_test.go` / 新 fake）：
  - DART 模式：请求 URL = `/dart/<full presigned url>`（httptest 断言
    路径与 query 透传）
  - DART 不可达 → fallback 直连（httptest 计数断言：直连请求发生）
  - DART 模式成功 → 无直连请求
  - 既有直连模式测试全绿（无 DART 地址时行为不变）

**验收**：单测绿；fallback 路径与现状行为一致（DART 缺席零影响）。

## 任务 3：DART 进程管理（agent 子进程）

**位置**：`cmd/firecracker-runtime-agent/`（或新 `agent/runtime` 包）

- [ ] Go 编排进程拉起 dart 子进程（DART 地址配置非空时）：
  - 参数：`-listen=127.0.0.1:8145 -admin=127.0.0.1:8147
    -cache-dir=<StateRoot>/cache/dart -discover=dns:dart.<ns>.svc:9000
    -peer-advertise=<podIP>:9000 -peer-listen=:9000 -self-id=<nodeName>`
  - `cache/dart` 目录创建（0750）
- [ ] 健康：dart admin/metrics 探活；不可用 → 日志 + agent Health 反映
  （`HealthResponse` 增 dart 状态字段，driver `ProbeCapabilities` 不因
  DART 降级——拉取自动 fallback）
- [ ] 崩溃重启（backoff）+ 优雅关闭（agent 退出先停 dart）
- [ ] 日志：dart stdout/stderr 进 agent 日志
- [ ] 单测：参数构造、重启逻辑（fake 子进程退出 → 重启计数）、关闭顺序

**验收**：单测绿；进程生命周期与 agent 一致。

## 任务 4：DART 二进制集成 + 部署清单

- [ ] 镜像构建：`build/` 新增 dart 构建阶段（fetch
  `data-accelerator/dart` @ pin `a85f39f` → `go build ./cmd/dart`），
  产物进 agent 镜像；commit 记录到镜像 label
- [ ] `config/dev/agent-daemonset.yaml` 更新：
  - env：`FAST_SANDBOX_DART_ADDR=http://127.0.0.1:8145`、
    peer-advertise 来源（`status.podIP` downward API 或节点 IP——实现
    时确认，记录结论）
  - hostPath：StateRoot（含 cache/dart，与现有共享）
- [ ] 新增 headless Service（`dart`，selector 匹配 agent pod，
  `clusterIP: None`）——DNS 发现 `dart.<ns>.svc`
- [ ] 验证：agent pod 内 `dart` 进程起、admin :8147 可访问、Service
  DNS 可解析（`getent hosts dart.<ns>.svc`）

**验收**：镜像含 dart（`dart -version`）；单节点集成环境 agent 起来后
dart 健康；DNS 解析正常。

## 任务 5：观测（block_source 指标）

- [ ] 确认 dart admin `/metrics` 暴露 `dart_block_source_total{source=...}`
  （README 先例）——agent Health 或独立探活采集
- [ ] 集成环境 `status` 增加 dart 指标摘要（cache/peer/origin 计数、
  命中率）
- [ ] agent HealthResponse 增 dart 字段（可选，任务 3 已定）与 driver
  侧展示对齐

**验收**：指标可见且三源可区分。

## 任务 6：集成环境验证（2 节点 verify-p2p）

**位置**：`config/dev/kind-firecracker.yaml` + `scripts/integration-env.sh`

- [ ] Kind 集群扩展 2 节点（1 control + 1 worker；两节点均透传 KVM——
  worker 补 extraMounts）；节点 label 两节点都打
- [ ] agent DaemonSet / fastlet pool 覆盖两节点（nodeSelector 或
  双节点 label）
- [ ] `integration-env.sh` 新增 `verify-p2p` 阶段：
  1. 两节点 agent+dart 均健康（roster 收敛：`status` 可见两节点）
  2. 节点 A warmImages 拉取完成（缓存落盘，origin 计数 = 1 份基线）
  3. 节点 B warmImages 同一 image → 断言 **origin 拉取不翻倍**
     （MinIO 请求计数 or dart `block_source{origin}`：B 的拉取
     origin 请求 ≈ 0 或显著低于全量）
  4. 两节点各自 sandbox restore + execd 可达（回归，复用 verify）
- [ ] 单节点模式保留（verify-p2p 需要 ≥2 节点时提示；1 节点下 dart
  cache 命中链路仍可验）
- [ ] 日志/回收沿用规范（down 清理 2 节点集群）

**验收**：`verify-p2p` 全绿；origin 放大实测数据记录（目标 < 1/N）；
presign 对 MinIO 的真实签名正确性由此覆盖（风险项闭环）。

## 任务 7：回归与文档

- [ ] `go build ./...`、`go test ./internal/runtime/firecracker/...
   -count=1 -race`、`go vet ./...` 全绿
- [ ] agent 缺席/DART 缺席路径回归（现有 firecracker 测试全绿——
  fallback 保证）
- [ ] 更新设计文档实测记录：presign 验证结论、peer-advertise 取值、
  origin 放大实测、DART pin 变更记录

**验收**：全量测试绿；文档同步。

---

## 交付物汇总

| 产出 | 路径 |
|------|------|
| presign + 拉取改造 | `internal/runtime/firecracker/agent/{s3client,pull,index,manifest}.go` + 单测 |
| DART 进程管理 | `cmd/firecracker-runtime-agent/` + 单测 |
| 镜像构建阶段 | `build/`（dart 构建） |
| 部署清单 | `config/dev/agent-daemonset.yaml` + headless Service |
| Kind 2 节点 | `config/dev/kind-firecracker.yaml` |
| verify-p2p | `scripts/integration-env.sh` |
| 文档 | 设计文档实测更新 |

## 验收标准（整体）

1. 任务 1-7 勾选完成，每项验收通过；
2. `verify-p2p` 全绿：B 节点 peer 命中、origin ≈1 份（计数证据）；
3. presign 对 MinIO 真实签名验证通过（集成环境首跑覆盖）；
4. DART 缺席时行为与现状完全一致（fallback 回归）；
5. 日志/回收要求沿用集成环境既有规范。

## 参考

- 方案：`docs/design/firecracker-native-p2p-plan.md`
- 设计：#21（P2P = DART 集成）、细节文档 §6
- DART：`github.com/data-accelerator/dart` @ `a85f39f`（已本地验证
  `go build ./cmd/dart` 通过）
- 现状代码：`internal/runtime/firecracker/agent/s3client.go`（header
  签名，presign 复用派生）、`pull.go`、`cmd/firecracker-runtime-agent/`、
  `scripts/integration-env.sh`、`config/dev/agent-daemonset.yaml`
