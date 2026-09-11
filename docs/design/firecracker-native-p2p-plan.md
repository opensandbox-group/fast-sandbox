# 阶段 2 施工方案：native + P2P（DART 集成）

> 文档类型：施工任务书（转交实现）
>
> 日期：2026-08-29
>
> 设计定案：见 [firecracker-on-demand-loading.md](firecracker-on-demand-loading.md)
> 架构选择表/决策记录（P2P = 集成 DART）+ 细节文档 §6（进程拓扑、数据链路、
> 源选择、租户边界）。已验证：DART 可构建（pin `a85f39f`，零依赖 Go，
> `go build ./cmd/dart` 直接出二进制，无 release 版本号 = dev 构建）。
>
> 范围：阶段 2 = native 全量拉取走 DART P2P（overlaybd 为阶段 3）。

---

- [背景与目标](#背景与目标)
- [已定案要点](#已定案要点)
- [工作分解](#工作分解)
- [测试计划](#测试计划)
- [验收标准](#验收标准)
- [风险与待验证项](#风险与待验证项)
- [参考](#参考)

## 背景与目标

阶段 1（native 全量回源 S3）已闭环。阶段 2 把拉取路径接入 **DART**
（只读缓存 + P2P 分发树），使同一对象在 N 个节点上只回源 ~1 份，并采集
P2P 价值验证数据（回源占比/命中率）——这是后续是否投入更多 P2P 治理的
决策依据。

**验收目标（阶段 2）**：

- 空缓存 warmImages 可从 peer 命中（非回源）；
- 多节点拉同一对象，origin 拉取 ≈ 1 份（`block_source{origin}` 观测）；
- DART 独立进程随 agent 生命周期管理（可升级/回滚，故障隔离）。

## 已定案要点

| 项 | 定案 |
|----|------|
| 载体 | DART 独立进程（**零代码集成**，Go internal 包不可 import） |
| 进程拓扑 | runtime-agent 容器 = Go 编排 + DART（Rust daemon 阶段 3 再加） |
| DART 接口 | `http://127.0.0.1:8145/dart/<presigned-s3-url>`（前缀模式） |
| 端口 | http :8145、admin/metrics :8147、peer-listen :9000（节点 IP） |
| 发现 | DNS（headless Service）；`-self-id=$NODE_NAME`（HRW 稳定身份） |
| 缓存 | `-cache-dir=<StateRoot>/cache/dart`（block arena，与文件缓存并列） |
| 源选择 | 公共对象走 DART；增量层直连 S3（阶段 4 才有增量层，本期全走 DART） |
| 认证 | agent 签发 **presigned URL**（SigV4 query 签名）交 DART，AK/SK 不出 agent |
| 校验 | agent 侧整对象 sha256（与 manifest 一致）；DART 不感知 |
| fallback | DART 进程不可用 → agent 回退**直连 S3**（现有 header 签名路径保留） |

## 工作分解

### A. DART 二进制集成（镜像构建）

- [ ] **DART pin**：`data-accelerator/dart` commit `a85f39f`（vendor 进
  构建仓库或构建时 fetch+verify）；记录 commit 到镜像 label/版本文件；
- [ ] agent 镜像构建（`build/` 新增 dart 构建阶段）：
  `go build ./cmd/dart`（零依赖，Go ≥1.22）；
- [ ] 上游风险记录：无 release（`dart dev`），固定 commit pin + 独立
  升级（对齐 AgentENV storage 的上游维护策略——fork 触发条件同样适用）；
- [ ] **验收**：镜像内 `dart -version` 可运行；构建可重制（pin 固定）。

### B. presigned URL（s3client 扩展）

- [ ] `internal/runtime/firecracker/agent/s3client.go` 新增
  `presign(method, key, ttl)`：生成 SigV4 **query 签名** URL
  （`X-Amz-Algorithm/Credential/Date/Expires/SignedHeaders/Signature`，
  复用现有 `sign` 的派生逻辑，Header 签名与 Query 签名共用 signing key）；
- [ ] 与现有 header 签名路径共存（header 签名保留作直连 fallback）；
- [ ] 单测：presigned URL 的 query 参数与签名正确性（与 header 签名的
  canonical request 一致性——同一 canonical 派生，仅签名载体不同）；
- [ ] **验收**：presigned URL 可被 curl 直接 GET MinIO 对象（单测用
  httptest 验证签名参数构造；真机验证项见 §风险）。

### C. agent 拉取路径改造

- [ ] `agent` 配置增加 DART 地址（`http://127.0.0.1:8145`，env：
  `FAST_SANDBOX_DART_ADDR`，空 = 直连模式）；
- [ ] `Client.PullImage` 的下载路径：对象下载从"header 签名直连"改为
  **presign → GET <dart>/dart/<presigned-url>**；
  - index/manifest 小对象也走 DART（缓存 + P2P 收益一致）；
  - DART 不可达（连接失败）→ **fallback 直连 S3**（header 签名路径，
    现有逻辑保留为 fallback）；
- [ ] 幂等/校验/落盘不变（整对象 sha256 在 agent 侧）;
- [ ] 单测：fake DART（httptest 验证前缀 URL 转发语义——`/dart/<full
  URL>` 透传）、fallback 触发（DART 不可达 → 直连调用计数）、presign
  只在走 DART 时发生；
- [ ] **验收**：现有 agent 测试全绿（fallback 保持）；`PullImage` 在
  DART 模式经前缀 URL 完整落盘。

### D. DART 进程管理（agent 容器内）

- [ ] Go 编排进程**拉起 dart 子进程**（对齐 Rust daemon 的管理模式——
  agent 拥有子进程生命周期）：
  - 启动参数：`-listen=127.0.0.1:8145 -admin=127.0.0.1:8147
    -cache-dir=<StateRoot>/cache/dart -discover=dns:dart.<ns>.svc:9000
    -peer-advertise=$NODE_IP:9000 -peer-listen=:9000 -self-id=$NODE_NAME`
  - 健康：`GET :8147/metrics` 或 admin 探活；失败 → 日志 + agent Health
    降级（DART 不可用不影响 agent UDS 健康，仅拉取 fallback 直连）；
  - 日志：dart 输出进 agent 日志文件（logs/ 规范）；
  - 优雅关闭：agent 退出前先停 dart（缓存 flush）；
  - 崩溃重启：dart 退出 → agent 重启子进程（带 backoff）；
- [ ] 单测：dart 进程参数构造、重启逻辑（fake 子进程）、健康检查集成
  （agent Health 反映 dart 状态）；
- [ ] **验收**：容器内 dart 进程随 agent 启停；崩溃后自动重启；缓存目录
  权限正确。

### E. headless Service + 部署清单

- [ ] `config/dev/agent-daemonset.yaml` 更新：
  - dart 相关 env（`FAST_SANDBOX_DART_ADDR`、peer-advertise 用节点 IP）；
  - hostPath 增 `cache/dart`（与 StateRoot 同盘，reflink 要求沿用）；
- [ ] 新增 headless Service：`dart`（selector 匹配 agent pod，
  `clusterIP: None`）——DART DNS 发现用；
- [ ] 镜像 tag：agent 镜像含 dart 二进制（A 节）；
- [ ] **验收**：agent pod 内 dart 起来、admin 可访问、Service DNS 可解析。

### F. 观测

- [ ] DART admin `/metrics`（`block_source_total{source="cache|peer|origin"}`
  等）采集：agent 健康上报携带（或集成环境 verify 直接 curl 提取）；
- [ ] 集成环境 `status` 增加 dart 指标摘要（回源/命中/peer 计数）；
- [ ] **验收**：`block_source` 指标可见且可区分 cache/peer/origin。

### G. 集成环境验证（2 节点）

- [ ] Kind 集群扩展为 **2 个节点**（1 control + 1 worker，均透传 KVM——
  对齐 firecracker 集成环境 kind 配置；worker 上同样部署 agent DaemonSet
  与 fastlet pool，nodeSelector 两节点都打 label）；
- [ ] `integration-env.sh` 增加 `verify-p2p` 阶段：
  1. 两节点 agent 都启动、dart 都注册（roster 收敛）；
  2. 节点 A warmImages 拉取（缓存落盘）；
  3. 节点 B warmImages 拉取同一 image → **断言 origin 拉取不翻倍**
     （MinIO 访问计数 or dart `block_source{origin}` 计数：B 的拉取
     origin=0 或显著 < A）；
  4. 两节点各自 sandbox restore 成功（guest 可达——回归）；
- [ ] 日志/回收沿用规范（down 清理 2 节点集群）；
- [ ] **验收**：`verify-p2p` 全绿；origin 放大 < 1/N 的实测数据记录。

## 测试计划

| 层 | 覆盖 |
|----|------|
| 单测（s3client） | presign 参数/签名构造、与 header 签名 canonical 一致性 |
| 单测（agent） | 拉取走 DART 前缀（fake DART）、fallback 直连触发、幂等保持 |
| 单测（进程管理） | dart 参数、崩溃重启、健康集成 |
| 集成（2 节点 Kind） | verify-p2p：peer 命中、origin 计数、sandbox 回归 |

## 验收标准

1. `go build ./...`、`go test ./internal/runtime/firecracker/... -count=1
   -race`、`go vet ./...` 全绿；
2. agent 测试全绿（直连 fallback 保持——DART 缺席时行为与现状一致）；
3. `verify-p2p` 全绿：B 节点 warmImages 从 peer 命中，origin 拉取 ≈1 份
   （计数证据）；
4. presigned URL 真机验证通过（见下）；
5. 零新三方依赖（DART 二进制经构建阶段产出，不引入 Go module）；
6. 设计文档实测项（§风险）记录结论。

## 风险与待验证项

| 项 | 说明 | 缓解/验证 |
|----|------|-----------|
| presigned URL 对 MinIO 的真实签名正确性 | 单测只验参数构造 | **真机验证**（参考机：agent presign → curl MinIO GET）；或集成环境 verify-p2p 首跑即覆盖 |
| DART 无 release（dev 构建） | 版本语义弱 | pin commit + 构建记录；上游维护策略（fork 触发）适用 |
| DART 缓存目录与文件缓存并存 | 两份磁盘（native 语义已知） | 接受（细节 §6.4）；容量治理后续 |
| peer-advertise 需节点 IP | DaemonSet env 注入 | 用 `status.podIP` 的 downward API（节点 IP = pod IP，hostNetwork 或常规）——实现时确认 |
| Kind 2 节点的 KVM 透传 | 双节点都要 /dev/kvm | kind 配置两节点 extraMounts |
| single-node 集成环境（现 1 节点） | verify-p2p 需要 2 节点 | G 节扩展；1 节点下 dart 链路仍可验证（cache 命中 + admin 指标） |

## 参考

- 设计：#21 架构选择表/决策记录（P2P 行）、细节文档 §6（进程拓扑/
  数据链路/源选择/租户边界/观测）；
- DART：`github.com/data-accelerator/dart`（pin `a85f39f`，Apache-2.0，
  零依赖；README：block 缓存/HRW 分发树/cut-through/熔断/presigned
  upstream/DNS 发现）；
- 现状代码：`internal/runtime/firecracker/agent/{s3client,pull}.go`、
  `cmd/firecracker-runtime-agent/main.go`、`config/dev/agent-daemonset.yaml`、
  `scripts/integration-env.sh`（集成环境）。
