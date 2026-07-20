# Fast Sandbox 架构重构验收报告

**日期**：2026-07-19

**基线**：`master@f92d8e34288365be227d2ee8a6f952687dc7be00`

**验收源码快照**：`feature/fast-sandbox-architecture-refactor@23c0390`

**结论**：约定范围内的架构重构可进入分支 Review；Kata Firecracker、BoxLite 生产支持、目标集群性能基线和多节点镜像缓存验收必须继续保持为显式限制。

## 1. 交付结论

本分支已经实现并验证：

- Fast-Path 与 Controller 角色分离；Fast-Path 3 副本多活，Controller 2 副本单 Leader；
- RPC Create 使用 request ID 幂等，快速失败不遗留 CRD；Controller-only 的直接 CRD Create 可以独立工作；
- Registry 由 Pod Watch、低频 Heartbeat、本地反馈和镜像亲和生成 Top-K，Fastlet admission 是容量最终权威；
- Pool 使用单一 canonical `runtime` 和固定 Sandbox ResourceProfile；
- 每个 Sandbox 使用独立私网、NetworkSlot 和 NAT，不再做 host-port 冲突调度；
- Sandbox Proxy 与 Fastlet Proxy 构成独立透明数据面，凭证受 Pod UID、assignment attempt 和 route generation fencing；
- Core 不定义 Exec/File 协议，通过 Infra Component 注入、服务发现、鉴权、透明代理和 Execd/Envd SDK Adapter 提供能力；
- container、gVisor、Kata QEMU/CLH/Firecracker 进入统一 RuntimeDriver 边界；QEMU/CLH 已验证，Firecracker 保持 capability fail closed；
- Pool Drain、PodLost、generation fencing 和 NodeJanitor backend 已落地；
- Metrics、W3C Trace Context、结构化 lifecycle identity、迁移工具和生产/开发部署 overlay 已补齐。

## 2. 验收环境

| 项目 | 值 |
|---|---|
| SSH 环境 | `fast:~/fast-sandbox` |
| Host | Linux `5.15.0-173-generic`, x86_64 |
| Go | `go1.25.7 linux/amd64` |
| Docker | `28.0.4` |
| kubectl / kustomize | `v1.35.0` / `v5.7.1` |
| basic kind | Kubernetes `v1.27.3`, containerd `1.7.1` |
| 集群 | `fsb-e2e-basic`, `fsb-e2e-gvisor`, `fsb-e2e-kata` |
| CPU（Top-K benchmark） | Intel Xeon Platinum 8269CY |

所有 Kubernetes、containerd、网络和安全 runtime 验证均在远端 Linux 执行；本地 macOS 只用于编辑和 Git 管理。

## 3. 当前快照 Gate

| Gate | 结果 | 关键证据 |
|---|---|---|
| 完整 unit package set | PASS | 相同 `UNIT_PACKAGES` 与 `-gcflags="all=-N -l"`，增加 `-p=1` 后退出 `0` |
| Registry Top-K unit/race | PASS | `go test -race -p=1 ./internal/controller/fastletpool -count=1` |
| e2e 环境/port-forward race | PASS | `go test -race -p=1 ./test/e2e/env/... ./test/e2e/support/portforward -count=1` |
| 三 Fast-Path 多活 | PASS | 每个 Fast-Path Pod 直连 Create；40 路请求跨 3 副本并发，capacity=3 时仅 3 个成功且 UID/name 唯一 |
| Controller Leader 故障 | PASS | 删除 Leader 期间 RPC Create 成功，新 Leader 接管，Controller 恢复 `2/2` |
| Sandbox Proxy 副本故障 | PASS | 删除 1 个 Proxy Pod，仅剩 survivor 时重新通过 Service 建连并成功路由，Deployment 恢复 `2/2` |
| 路由 fencing | PASS | Fastlet Proxy restart 后恢复；reset/delete 后旧 route credential 被拒绝 |
| 临时资源清理 | PASS | 测试 namespace、port-forward 和测试进程均清理；Fast-Path `3/3`、Controller `2/2`、Sandbox Proxy `2/2` |

当前快照的聚焦远端命令：

```bash
go test ./test/e2e/suites/controlplane/... \
  -run '^TestMultiActiveControlPlane$' -v -count=1 -timeout 12m
# exit 0; package 49.907s

go test ./test/e2e/suites/basicvalidation/... \
  -run '^TestSandboxProxyDataPlane$' -v -count=1 -timeout 12m
# exit 0; package 90.239s

go test -p=1 -gcflags="all=-N -l" \
  ./api/... ./cmd/... ./internal/... ./pkg/... \
  ./test/e2e/env/... ./test/e2e/support/... ./test/performance/...
# exit 0
```

## 4. 分能力远端矩阵

以下证据来自本分支对应实现提交；后续提交未修改相关核心 runtime 语义，最新快照又通过了完整 unit set。

| 能力 | 远端 Gate | 结果 |
|---|---|---|
| 生成契约 | protobuf、DeepCopy、两份 CRD 重新生成后逐文件 SHA-256 与当前分支一致 | PASS |
| 控制面全集 | `E2E_TEST_TIMEOUT=35m make test-e2e-controlplane` | PASS |
| Linux 私网/NAT | `make DOCKER_BUILD_FLAGS=--network=host test-network-integration` | PASS |
| Kubernetes 网络 | `E2E_TEST_TIMEOUT=35m DOCKER_BUILD_FLAGS=--network=host make test-e2e-network` | PASS |
| Proxy | 当前快照聚焦命令，另有完整 `make test-e2e-proxy` 历史 Gate | PASS |
| Infra 注入 | `E2E_TEST_TIMEOUT=35m DOCKER_BUILD_FLAGS=--network=host make test-e2e-infra` | PASS |
| SDK Adapter | `E2E_TEST_TIMEOUT=35m DOCKER_BUILD_FLAGS=--network=host make test-e2e-sdk` | PASS |
| Drain | `E2E_TEST_TIMEOUT=35m DOCKER_BUILD_FLAGS=--network=host make test-e2e-drain` | PASS |
| PodLost/Reset | fault-recovery suite，包括 AutoRecreate、Manual Lost、reset、orphan | PASS |
| Janitor | `E2E_TEST_TIMEOUT=35m DOCKER_BUILD_FLAGS=--network=host make test-e2e-cleanupjanitor` | PASS |
| container | `E2E_TEST_TIMEOUT=20m DOCKER_BUILD_FLAGS=--network=host make test-e2e-runtime-container` | PASS |
| gVisor | `E2E_TEST_TIMEOUT=40m DOCKER_BUILD_FLAGS=--network=host make test-e2e-runtime-gvisor` | PASS |
| Kata QEMU/CLH | `E2E_TEST_TIMEOUT=40m DOCKER_BUILD_FLAGS=--network=host make test-e2e-runtime-kata` | PASS（实际 runtime 支持） |
| Kata Firecracker | 同一 Kata 聚合 Gate | PASS（只证明 `KataFirecrackerNotValidated` fail closed） |
| BoxLite capability boundary | `E2E_PROFILE=basic E2E_TEST_TIMEOUT=20m make test-e2e-runtime-boxlite` | PASS（只证明 fail closed） |

Kata 聚合 Gate 覆盖 QEMU、CLH 和 Firecracker。Isolation/private-network/proxy/recovery 的实际支持证据只覆盖 QEMU 与 CLH；当前远端曾出现 Firecracker CRD 已 Running 后 VM/shim 消失，因此 `kata-firecracker` 明确返回 `KataFirecrackerNotValidated`，并且不创建 Fastlet Pod。具体运行记录见[实施计划](superpowers/plans/2026-07-19-fast-sandbox-architecture-refactor-implementation-plan.md)。

## 5. 性能和容量证据

### 5.1 Registry Top-K

1000 watched Fastlets、K=3、五次中位数：

| 实现 | Time/op | Bytes/op | Allocs/op |
|---|---:|---:|---:|
| 全量深拷贝 + 逐 Fastlet metric observation | 2.156 ms | 515,080 B | 2,005 |
| 有界 rank projection + 每状态 heartbeat summary | 0.431 ms | 67,272 B | 12 |

结果约为 CPU 时间降低 80%、分配字节降低 87%、分配次数降低 99%。排序仍保留 hard filter、normalized image hit、负载比例和 request ID 稳定扰动；Fastlet admission 语义不变。

### 5.2 Create smoke

`test/performance/create_load` 已通过 unit/race/build，并在 basic kind 中完成 3 请求、2 并发、3/3 成功、UID/name 唯一、cleanup 3/3 的 API smoke。观测到 full Create RPC p50 `198.970ms`、p95 `214.175ms`。

这组值只证明报告工具和 Create 链路可用，不是最终 SLO，也不能代替 `CreateAccepted` 或 `DataPlaneReady` Prometheus milestone。生产发布仍需在目标硬件分别采集 warm/cold、image hit/miss、runtime、InfraProfile 和 NetworkSlot 维度。

## 6. 可观测性和迁移

- Metrics 覆盖 CreateAccepted/DataPlaneReady、runtime/user-process、Fastlet admission、Registry/Top-K、cache、NetworkSlot、Infra、Proxy 和 Janitor；高基数字段不进入 label；
- W3C Trace Context 覆盖 fastctl、Go/Python SDK、Fast-Path、Fastlet control、Sandbox Proxy、Fastlet Proxy 和 Execd upstream；
- OTLP/gRPC exporter 仅在显式配置 endpoint 时启用，真实本地 TraceService smoke 验证 batch export 与 shutdown flush；
- `fastctl migrate pool` 支持多 YAML 文档、canonical runtime/resource/InfraProfile 转换和 `--check` CI Gate；
- 生产 `config/default` 不携带开发私钥，`config/dev` 使用显式 development-only key；
- CRD 目录使用 kustomize，自动环境和手工流程都必须执行 `kubectl apply -k config/crd`。

## 7. 明确限制

### 7.1 BoxLite 尚不是生产支持 runtime

BoxLite native Sidecar、UDS client、authenticated local forward、恢复记录和 Janitor backend 已实现并有构建/单元证据。但 BoxLite v0.9.7 没有创建前、完整且不可被 guest root 绕过的 CPU/memory/PIDs enforcement surface。

因此当前必须保持：

- `resource-limits-v1=false`；
- native Sidecar `Ready=false`；
- Pool reason `BoxLiteResourceEnforcementIncomplete`；
- `runtime=boxlite` fail closed。

BoxLite Gate 退出 `0` 只证明拒绝原因准确，不能表述为 BoxLite 已支持。

### 7.2 仍需目标环境报告

- Kata Firecracker 仍是 capability fail closed，不能与已验证的 QEMU/CLH 一起声明生产支持；
- 单节点 kind 共享节点镜像存储，不能模拟不同节点 cache inventory；需要多节点目标集群补充 image-affinity 和 cache-GC 生命周期报告；
- warm container `<50ms` 仍是特定 profile 的观测目标，当前没有生产硬件最终基线；
- Create load 工具当前只驱动 FastPath RPC；direct-CRD path 已有功能 e2e，但尚未纳入同一负载报告；
- `sandbox-init` supervisor 路径尚无可信 user-process-start 回传时，只记录 unavailable，不把 supervisor start 冒充用户进程启动；
- 生产 opensandbox-execd artifact release/signature binding 和 E2B envd 预装镜像由实际集成环境提供；仓库只验证注入边界、透明路由和 Adapter；
- 私有镜像凭证刷新，以及 snapshot、pause/resume、持久化 storage 属于后续议题，不作为本次重构完成条件。

## 8. 发布判断

本分支可以按以下口径进入 Review 和集成环境：

1. `container`、`gvisor`、`kata-qemu`、`kata-clh` 可以按已验证矩阵声明；
2. `kata-firecracker` 和 `boxlite` 必须继续声明为 capability fail closed；
3. 合入前在目标分支重新运行生成检查、完整串行 unit/race 和受影响的 controlplane/proxy Gate；
4. 生产发布前补充目标集群 Create 性能与多节点 cache/affinity 报告；
5. 不得把单节点 kind smoke、BoxLite fail-closed Gate 或缺失 user-process signal 的样本包装成生产能力证据。

详细设计与执行记录见：

- [当前架构](../ARCHITECTURE.md)
- [性能契约](PERFORMANCE.md)
- [测试指南](TESTING.md)
- [可观测性](observability.md)
- [迁移指南](migration-guide.md)
- [完整实施计划与逐阶段证据](superpowers/plans/2026-07-19-fast-sandbox-architecture-refactor-implementation-plan.md)
