# Fast Sandbox 架构重构验收报告

**日期**：2026-07-20

**基线**：`master@f92d8e34288365be227d2ee8a6f952687dc7be00`

**验收源码快照**：`feature/fast-sandbox-architecture-refactor` 本报告所在提交；远端 Gate 基于 `01fc5ae` 加本提交完整 worktree 同步执行

**结论**：实施计划最终完成定义中的阻塞项已经关闭，架构重构完成并可进入分支 Review；Kata Firecracker、BoxLite 生产支持、目标集群性能基线和多节点镜像缓存验收继续保持为显式发布限制。

## 1. 交付结论

本分支已经实现并验证：

- Fast-Path 与 Controller 角色分离；Fast-Path 3 副本多活，Controller 2 副本单 Leader；Controller-only 在 FastPath Pod 数为 0 时仍可工作；`role=all` 有单进程开发兼容 overlay 和真实集群 Gate；
- RPC Create 使用 request ID 幂等，快速失败不遗留 CRD；runtime 创建严格位于 CRD commit、UID 和 assignment 之后；
- Registry 由 Pod Watch、低频 Heartbeat、本地反馈和镜像亲和生成 Top-K，Fastlet admission 是容量最终权威；
- Pool 使用单一 canonical `runtime` 和固定 Sandbox ResourceProfile；
- 每个 Sandbox 使用独立私网、NetworkSlot 和 NAT，不再做 host-port 冲突调度；
- Sandbox Proxy 与 Fastlet Proxy 构成独立透明数据面，凭证受 Pod UID、assignment attempt 和 route generation fencing；
- Core 不定义 Exec/File 协议，通过 Infra Component 注入、服务发现、鉴权、透明代理和 Execd/Envd SDK Adapter 提供能力；
- container、gVisor、Kata QEMU/CLH/Firecracker 进入统一 RuntimeDriver 边界；QEMU/CLH 已验证，Firecracker 保持 capability fail closed；
- Pool 缩容和 template 变化都经过持久化 Drain；计划升级使用单 Pod ready-surge，并等待精确 Pod UID heartbeat 的 RuntimeReady/InfraReady 后才 Drain 旧 Fastlet；
- PodLost、generation fencing 和 NodeJanitor backend 已落地；
- `warmImages` 异步调用实际 RuntimeArtifactCache，heartbeat 上报真实 cache inventory，并通过有界 `fast_sandbox_warm_image_pull_total{result}` 观测；
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

所有 Kubernetes、containerd、网络和安全 runtime 验证均在远端 Linux 执行；Python SDK 纯单元 Gate 使用本地临时 Python 3.12 虚拟环境，结束后已删除。本地其余操作只包括编辑、静态检查和 Git 管理。

## 3. 当前快照 Gate

| Gate | 结果 | 关键证据 |
|---|---|---|
| 完整 unit package set | PASS | `GOFLAGS= go test -p=1` 执行 `UNIT_PACKAGES` 全集，退出 `0` |
| 关键并发 race | PASS | Controller、Heartbeat、Registry、runtime、port-forward 五组 package 全部退出 `0` |
| 生成契约 | PASS | `make generate manifests` 前后 protobuf、DeepCopy、两份 CRD SHA-256 完全一致 |
| 三 Fast-Path 多活 | PASS | 每个 Fast-Path Pod 直连 Create；40 路跨 3 副本并发严格受 capacity 限制 |
| Controller-only / `role=all` | PASS | FastPath Pod=0 时 direct CRD Ready；单 `role=all` Pod作为 Service 唯一 endpoint，RPC Create 返回同一 CRD UID |
| Controller Leader 故障 | PASS | 删除 Leader 期间 RPC Create 成功，新 Leader 接管，Controller 恢复 `2/2` |
| Sandbox Proxy 副本故障 | PASS | 删除 1 个 Proxy Pod，仅剩 survivor 时重新通过 Service 建连并成功路由，Deployment 恢复 `2/2` |
| 路由 fencing | PASS | Fastlet Proxy restart 后恢复；reset/delete 后旧 route credential 被拒绝 |
| warmImages / planned upgrade | PASS | 真实 runtime cache inventory + success metric；ready replacement 之后才持久化 `planned-upgrade` Drain |
| 临时资源清理 | PASS | 测试 namespace、port-forward 和测试进程均清理；拓扑切换后 Fast-Path `3/3`、Controller `2/2` |

当前快照的聚焦远端命令：

```bash
GOFLAGS= E2E_TEST_TIMEOUT=35m DOCKER_BUILD_FLAGS=--network=host \
go test ./test/e2e/suites/controlplane/... \
  -run '^TestMultiActiveControlPlane$' -v -count=1 -timeout 12m
# exit 0; package 454.960s（清空 build cache 后重建）；Controller-only 3.03s；role=all 22.68s

GOFLAGS= go test -race -vet=off -p=1 \
  ./internal/controller ./internal/controller/fastletcontrol \
  ./internal/controller/fastletpool ./internal/fastlet/runtime \
  ./test/e2e/support/portforward -count=1
# exit 0

GOFLAGS= go test -p=1 \
  ./api/... ./cmd/... ./internal/... ./pkg/... \
  ./test/e2e/env/... ./test/e2e/support/... ./test/performance/...
# exit 0

GOFLAGS= make generate manifests
# exit 0; 五份生成文件的 pre/post SHA-256 逐项相同
```

## 4. 分能力远端矩阵

以下证据均对应本报告描述的最终 worktree；远端使用显式文件同步，没有切换或修改远端用户分支/index。

| 能力 | 远端 Gate | 结果 |
|---|---|---|
| 生成契约 | protobuf、DeepCopy、两份 CRD 重新生成后逐文件 SHA-256 与当前分支一致 | PASS |
| 控制面三形态 | `go test ./test/e2e/suites/controlplane/... -run '^TestMultiActiveControlPlane$'` | PASS，`454.960s` |
| Linux 私网/NAT | `GOFLAGS= DOCKER_BUILD_FLAGS=--network=host make test-network-integration` | PASS，privileged test `0.14s` |
| Kubernetes 网络 | `TestSandboxPrivateNetwork` + `TestPortValidation` | PASS，`96.044s` |
| Proxy / Infra / SDK Adapter | 三项 basicvalidation 聚合定向 Gate | PASS，`129.596s` |
| warmImages | `TestPoolWarmImagesReachRuntimeCacheInventory` | PASS，`35.477s` |
| Drain | 完整 `test/e2e/suites/drain` | PASS，`127.762s` |
| PodLost/Reset | 完整 fault-recovery suite，包括 expiry、AutoRecreate、Manual Lost、reset、orphan | PASS，`350.335s` |
| Janitor | 完整 cleanupjanitor suite | PASS，`103.527s` |
| container / BoxLite boundary | container 实际 Gate + BoxLite fail-closed Gate | PASS，`86.422s` |
| gVisor | actual sandbox、guest kernel、私网/恢复、多 Sandbox | PASS，`131.060s` |
| Kata QEMU/CLH | isolation、resource、private network、proxy、recovery | PASS，`220.167s` 聚合 Gate内实际通过 |
| Kata Firecracker | 同一 Kata 聚合 Gate | PASS（只证明 `KataFirecrackerNotValidated` fail closed） |
| Python SDK | 临时 Python 3.12 环境安装 `.[dev,telemetry]` 后 `pytest -q` | PASS，`4 passed in 0.52s` |
| all-in-one manifest | `kubectl kustomize config/all-in-one` + client dry-run | PASS；无独立 FastPath Deployment/HPA/PDB |

Kata 聚合 Gate 覆盖 QEMU、CLH 和 Firecracker。Isolation/private-network/proxy/recovery 的实际支持证据只覆盖 QEMU 与 CLH；当前远端曾出现 Firecracker CRD 已 Running 后 VM/shim 消失，因此 `kata-firecracker` 明确返回 `KataFirecrackerNotValidated`，并且不创建 Fastlet Pod。具体运行记录见[实施计划](superpowers/plans/2026-07-19-fast-sandbox-architecture-refactor-implementation-plan.md)。

### 4.1 非功能性执行记录

- warmImages 首次使用未预载的 `busybox:1.36.1` 时，外部 Docker Hub 连接在 30 秒后超时；Gate 改用 e2e 明确预载到 kind 节点的 `alpine:latest`，从而验证真实 RuntimeArtifactCache 而不把公网可用性混入产品结论；
- controlplane 最终 Gate 第一次在测试逻辑启动前因根盘只剩约 `1.2GiB` 而编译失败。只清理了可再生成的 Go/BuildKit cache和未打标签、未被容器使用的旧 E2E image 记录；没有删除 tagged dev/base image、容器、active volume 或 kind 集群。释放约 `4.5GiB` 后相同命令退出 `0`；
- runtime 矩阵继续增加 active kind volume 占用；验收结束时没有为了腾空间破坏这些运行环境，后续环境容量治理应独立处理；
- 远端系统 Python 3.10 缺少 `httpx`，测试未进入 SDK 逻辑。最终改用本地临时 Python 3.12 虚拟环境安装仓库声明的 extras，`4 passed` 后精确删除该临时目录。

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

- Metrics 覆盖 CreateAccepted/DataPlaneReady、runtime/user-process、Fastlet admission、Registry/Top-K、cache warm result、NetworkSlot、Infra、Proxy 和 Janitor；高基数字段不进入 label；
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
3. 合入目标分支后按 CI policy 重跑生成检查、完整串行 unit/race 和受影响的 remote Gate；本分支最终源码快照已经完成同等验证；
4. 生产发布前补充目标集群 Create 性能与多节点 cache/affinity 报告；
5. 不得把单节点 kind smoke、BoxLite fail-closed Gate 或缺失 user-process signal 的样本包装成生产能力证据。

详细设计与执行记录见：

- [当前架构](../ARCHITECTURE.md)
- [性能契约](PERFORMANCE.md)
- [测试指南](TESTING.md)
- [可观测性](observability.md)
- [迁移指南](migration-guide.md)
- [完整实施计划与逐阶段证据](superpowers/plans/2026-07-19-fast-sandbox-architecture-refactor-implementation-plan.md)
