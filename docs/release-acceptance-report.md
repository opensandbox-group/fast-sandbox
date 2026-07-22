# Fast Sandbox 架构重构验收报告

**日期**：2026-07-21

**基线**：`master@f92d8e34288365be227d2ee8a6f952687dc7be00`

**验收源码快照**：`feature/fast-sandbox-architecture-refactor` 本报告所在 worktree；远端 Gate 使用完整变更文件同步执行

**结论**：实施计划最终完成定义中的阻塞项已经关闭，架构重构完成并可进入分支 Review；本轮同时关闭了 Containerd 自退出 workload 的删除幂等性缺陷。Kata Firecracker、BoxLite 生产支持、目标集群性能基线和多节点镜像缓存验收继续保持为显式发布限制。

## 1. 交付结论

本分支已经实现并验证：

- Fast-Path 与 Controller 角色分离；Fast-Path 3 副本多活，Controller 2 副本单 Leader；Controller-only 在 FastPath Pod 数为 0 时仍可工作；`role=all` 有单进程开发 overlay 和真实集群 Gate；
- RPC Create 使用 request ID 幂等，快速失败不遗留 CRD；runtime 创建严格位于 CRD commit、UID 和 assignment 之后；
- Registry 由 Pod Watch、低频 Heartbeat、本地反馈和镜像亲和生成 Top-K，Fastlet admission 是容量最终权威；
- Pool 使用单一 canonical `runtime` 和固定 Sandbox ResourceProfile；
- 每个 Sandbox 使用独立私网、NetworkSlot 和 NAT，不再做 host-port 冲突调度；
- Sandbox Proxy 与 Fastlet Proxy 构成独立透明数据面，凭证受 Pod UID、assignment attempt 和 route generation fencing；
- Core 不定义 Exec/File 协议，通过 Infra Component 注入、服务发现、鉴权、透明代理和 Execd/Envd SDK Adapter 提供能力；
- container、gVisor、Kata QEMU/CLH/Firecracker 进入统一 RuntimeDriver 边界；QEMU/CLH 已验证，Firecracker 保持 capability fail closed；
- Pool 缩容和 template 变化都经过持久化 Drain；计划升级使用单 Pod ready-surge，并等待精确 Pod UID heartbeat 的 RuntimeReady/InfraReady 后才 Drain 旧 Fastlet；
- PodLost、generation fencing 和 NodeJanitor backend 已落地；
- Containerd 删除采用 ensure-absent 状态机，自行退出的 workload、已缺失 task/container/snapshot 和重复删除均收敛成功，Controller finalizer 不再停留在 Draining；
- `warmImages` 异步调用实际 RuntimeArtifactCache，heartbeat 上报真实 cache inventory，并通过有界 `fast_sandbox_warm_image_pull_total{result}` 观测；
- Metrics、W3C Trace Context、结构化 lifecycle identity 和生产/开发部署 overlay 已补齐；未部署过的旧 API 已直接删除。

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

所有 Kubernetes、containerd、网络和安全 runtime 验证均在远端 Linux 执行；Python SDK 纯单元 Gate 使用 Codex 工作区 Python 及 `/private/tmp` 依赖目录。本地其余操作只包括编辑、代码生成、Python 单元测试、静态检查和 Git 管理。

## 3. 当前快照 Gate

| Gate | 结果 | 关键证据 |
|---|---|---|
| 完整 unit package set | PASS | `make test-unit` 执行 `UNIT_PACKAGES` 全集，退出 `0` |
| 完整 E2E suite 集合 | PASS | 当前源码快照的 10 个 suite 通过 controlplane 超集和剩余分包命令全部执行，均退出 `0` |
| 完整 race | PASS | `make test-race` 执行 `UNIT_PACKAGES` 全集，退出 `0` |
| 生成契约 | PASS | 临时 Git index 下 `make verify-generated` 退出 `0`；Python protobuf 用 `grpcio-tools==1.76.0` 连续生成逐字节一致 |
| 三 Fast-Path 多活 | PASS | 每个 Fast-Path Pod 直连 Create；40 路跨 3 副本并发严格受 capacity 限制 |
| Controller-only / `role=all` | PASS | FastPath Pod=0 时 direct CRD Ready；单 `role=all` Pod作为 Service 唯一 endpoint，RPC Create 返回同一 CRD UID |
| Controller Leader 故障 | PASS | 删除 Leader 期间 RPC Create 成功，新 Leader 接管，Controller 恢复 `2/2` |
| Sandbox Proxy 副本故障 | PASS | 删除 1 个 Proxy Pod，仅剩 survivor 时重新通过 Service 建连并成功路由，Deployment 恢复 `2/2` |
| 路由 fencing | PASS | Fastlet Proxy restart 后恢复；reset/delete 后旧 route credential 被拒绝 |
| warmImages / planned upgrade | PASS | 真实 runtime cache inventory + success metric；ready replacement 之后才持久化 `planned-upgrade` Drain |
| Containerd 删除幂等性 | PASS | 新增自退出 workload E2E，删除完成后同一容量立即创建 replacement；历史 `smoke-http` 未手工移除 finalizer 即自动消失 |
| 临时资源清理 | PASS | 测试 namespace、port-forward 和测试进程均清理；三个可复用 kind runtime profile 集群按开发环境策略保留；既有 `demo-http/smoke-python` 保持 Ready |

当前快照的最终命令：

```bash
make test-unit && make test-race
# exit 0

make test-python-sdk \
  PYTHON=/Users/fengjianhui/.cache/codex-runtimes/codex-primary-runtime/dependencies/python/bin/python3 \
  PYTHONPATH=/private/tmp/fast-sandbox-sdk-test
# exit 0; Ran 4 tests; OK

cp .git/index /tmp/fast-sandbox-verify-index-20260721
GIT_INDEX_FILE=/tmp/fast-sandbox-verify-index-20260721 \
  git add api/proto/v1 api/v1alpha1/zz_generated.deepcopy.go config/crd
GIT_INDEX_FILE=/tmp/fast-sandbox-verify-index-20260721 make verify-generated
# exit 0；真实 index 未改变

make test-e2e-controlplane
E2E_TEST_TIMEOUT=40m make test-e2e-runtime
E2E_TEST_TIMEOUT=35m make test-e2e-drain
E2E_TEST_TIMEOUT=35m make test-e2e-faultrecovery
make test-e2e-cliintegration
make test-e2e-advancedfeatures
# 全部 exit 0；合计覆盖 test/e2e/suites 下全部 10 个 suite

CGO_ENABLED=0 go test -c -o bin/network.test ./internal/fastlet/network
docker build --network=host --build-arg FASTLET_IMAGE=fast-sandbox/fastlet:dev \
  -t fast-sandbox/network-test:dev -f build/Dockerfile.network-test .
docker run --rm --privileged \
  -e FAST_SANDBOX_RUN_PRIVILEGED_NETWORK_TEST=1 \
  fast-sandbox/network-test:dev \
  -test.run '^TestLinuxNetNSDriverPrivileged$' -test.v
# exit 0；TestLinuxNetNSDriverPrivileged PASS
```

## 4. 分能力远端矩阵

以下证据均对应本报告描述的最终 worktree；远端使用显式文件同步，没有切换或修改远端用户分支/index。

| 能力 | 远端 Gate | 结果 |
|---|---|---|
| 生成契约 | protobuf、DeepCopy、两份 CRD 重新生成后逐文件 SHA-256 与当前分支一致 | PASS |
| 控制面三形态 | 完整 `test/e2e/suites/controlplane` | PASS，`83.028s` |
| Linux 私网/NAT | `test-network-integration` 的编译、test image 和 privileged run 三步 | PASS，privileged test `0.15s` |
| Kubernetes 基础能力 | 完整 `test/e2e/suites/basicvalidation`：canonical CRD、私网/NAT、Proxy、Infra、SDK Adapter、warmImages | PASS，`451.193s` |
| Lifecycle | 同名重建、自退出 workload 删除、正常 graceful deletion | PASS，`175.000s`；自退出核心断言 `6.80s` |
| Scheduling | capacity 与自动扩容 | PASS，`65.435s` |
| Drain | 完整 `test/e2e/suites/drain` | PASS，`145.559s` |
| PodLost/Reset | 完整 fault-recovery suite，包括 expiry、严格 AutoRecreate、reset、orphan | PASS，`398.945s` |
| Janitor | 完整 cleanupjanitor suite | PASS，`93.662s` |
| gVisor / Kata / runtime boundary | 完整 `test/e2e/suites/secureruntime` | PASS，`396.812s` |
| Kata QEMU/CLH | isolation、resource、private network、proxy、recovery | PASS；均在 secure runtime 整包中实际运行 |
| Kata Firecracker / BoxLite | 同一 secure runtime 整包 | PASS（只证明精确 capability fail closed） |
| Python SDK | Codex 工作区 Python + 临时依赖目录，`make test-python-sdk` | PASS，`4 tests` |
| fastctl CLI | run/get/update/reset 与 config-file | PASS，`115.475s` |
| Advanced Infra wiring | platform-owned InfraProfile 注入 Fastlet Pod | PASS，`32.843s` |
| all-in-one manifest | `kubectl kustomize config/all-in-one` + client dry-run | PASS；无独立 FastPath Deployment/HPA/PDB |

Kata 聚合 Gate 覆盖 QEMU、CLH 和 Firecracker。Isolation/private-network/proxy/recovery 的实际支持证据只覆盖 QEMU 与 CLH；Firecracker 已通过独立最小 Pod、直接 VMM 串口和替换 kernel 实验定位：Kata 3.27 默认 kernel 缺 `CONFIG_VIRTIO_MMIO`，当前 overlayfs 节点又缺少 Firecracker 所需 block snapshotter。`kata-firecracker` 因此继续返回 `KataFirecrackerNotValidated`，并且不创建 Fastlet Pod。完整证据和节点修复路径见 [Kata Firecracker 调查](kata-firecracker-investigation.md)。

### 4.1 非功能性执行记录

- warmImages 首次使用未预载的 `busybox:1.36.1` 时，外部 Docker Hub 连接在 30 秒后超时；Gate 改用 e2e 明确预载到 kind 节点的 `alpine:latest`，从而验证真实 RuntimeArtifactCache 而不把公网可用性混入产品结论；
- 初次整包 Gate 在创建 gVisor kind 环境时因远端根盘空间不足而失败。只删除了该次失败留下的 `fsb-e2e-gvisor` 集群并清理可再生成的 BuildKit cache/dangling image，保留 basic/kata 集群和 tagged dev/base image；重新创建 gVisor 环境后 secure runtime 通过，本轮又以分包矩阵覆盖全部 10 个 suite；
- 本轮 `make test-network-integration` 的 Fastlet image 重建卡在 Alpine 外部包源，未产生产品测试失败；停止悬挂 build 后复用同一源码此前 E2E 已构建的 Fastlet image，逐条执行 target 中的 network test 编译、test image build 和 privileged run，最终 `TestLinuxNetNSDriverPrivileged` 在 `0.15s` 通过；
- runtime 矩阵继续增加 active kind volume 占用；验收结束时没有为了腾空间破坏这些运行环境，后续环境容量治理应独立处理；
- 删除幂等性修复保留了真实 `smoke-http` 现场；新 lifecycle E2E直接证明自退出 task 的 Delete/finalizer/capacity reuse，随后在不手工删除 finalizer 的前提下更新同一 Fastlet Pod UID 内的进程，历史对象自动收敛；
- fault-recovery 原测试曾把 AutoRecreate 超时只记录为 warning。最终 Gate 复现出新 Fastlet 错误复用旧 Pod runtime、但缺少旧网络槽的问题；containerd 幂等身份现包含 Fastlet Pod UID、instance generation 和 assignment attempt，旧实例会先回收再重建，E2E 断言也已改为超时直接失败；
- macOS 系统 Python 3.9 与临时 `grpcio` 二进制不匹配；改用 Codex 工作区 Python 后 `make test-python-sdk` 的四项测试全部通过。

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

## 6. 可观测性和部署

- Metrics 覆盖 CreateAccepted/DataPlaneReady、runtime/user-process、Fastlet admission、Registry/Top-K、cache warm result、NetworkSlot、Infra、Proxy 和 Janitor；高基数字段不进入 label；
- W3C Trace Context 覆盖 fastctl、Go/Python SDK、Fast-Path、Fastlet control、Sandbox Proxy、Fastlet Proxy 和 Execd upstream；
- OTLP/gRPC exporter 仅在显式配置 endpoint 时启用，真实本地 TraceService smoke 验证 batch export 与 shutdown flush；
- SandboxPool 只保留 canonical runtime/resource/InfraProfile contract；项目未生产部署，不提供旧字段迁移工具；
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

- Kata Firecracker 仍是 capability fail closed；生产节点必须先 pin 含 MMIO fragment 的 kernel/runtime/image 组合、配置 devmapper block snapshotter，并通过最小 CRI 与完整 Fast Sandbox E2E；
- 单节点 kind 共享节点镜像存储，不能模拟不同节点 cache inventory；需要多节点目标集群补充 image-affinity 和 cache-GC 生命周期报告；
- warm container `<50ms` 仍是特定 profile 的观测目标，当前没有生产硬件最终基线；
- Create load 工具当前只驱动 FastPath RPC；direct-CRD path 已有功能 e2e，但尚未纳入同一负载报告；
- `sandbox-init` supervisor 路径尚无可信 user-process-start 回传时，只记录 unavailable，不把 supervisor start 冒充用户进程启动；
- 生产 OpenSandbox execd artifact release/signature binding 按接入指南后续完成；execd 是重点方向。E2B envd 不作为重点，可删除内置 profile 或仅保留 fail-closed 示例；
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
- [Containerd 删除、组件边界与遗留项](post-refactor-open-issues.md)
- [Kata Firecracker 根因与节点修复](kata-firecracker-investigation.md)
- [BoxLite 重点投入路线](boxlite-integration-roadmap.md)
- [OpenSandbox execd 接入指南](opensandbox-execd-integration-guide.md)
- [完整实施计划与逐阶段证据](superpowers/plans/2026-07-19-fast-sandbox-architecture-refactor-implementation-plan.md)
