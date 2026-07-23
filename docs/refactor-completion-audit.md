# Fast Sandbox 架构重构完成度审计

**审计日期**：2026-07-22

**审计分支**：`feature/fast-sandbox-architecture-refactor`

**目的**：把五份设计文档中的架构不变量、实施计划的最终完成定义，与当前代码和自动化测试逐项关联。本文是持续更新的完成度账本；某项历史上曾经通过、但未在最终源码快照重新执行的，不标记为最终 Gate 已通过。

## 1. 状态定义

- `已覆盖`：存在实现及直接自动化测试，测试断言与不变量语义一致；
- `证据偏弱`：实现存在，但只有间接测试、单元模型，或缺少真实部署/运行时验证；
- `缺口`：设计要求尚无完整实现或没有对应自动化测试；
- `待复验`：测试已存在且历史通过，但最终源码快照尚未重新执行。

## 2. 二十条核心不变量追踪

| # | 不变量 | 主要实现 | 自动化证据 | 当前状态 |
|---:|---|---|---|---|
| 1 | CRD 成功创建前不能创建 runtime | FastPath 先用一次 API Server Create 持久化 spec 和完整 assignment annotation，再用一次 Fastlet `CreateSandbox` 原子创建 runtime；reservation 已删除 | `TestCreateHappyPathUsesExactlyTwoDownstreamIO`、`TestFastletRejectionKeepsPersistedIntent`、`TestCreateRetryUsesSameCRDAndRuntimeIdentity` | 已覆盖；最终快照定向 e2e 已通过 |
| 2 | Sandbox UID 是 runtime 全局逻辑身份 | assignment annotation、原子 Create、runtime label 均传递 CRD UID | `TestCreateRetryUsesSameCRDAndRuntimeIdentity`、`TestSameRuntimeIdentityIncludesPodAndGenerationFences`；controlplane e2e 校验 assignment 与 CRD UID fencing | 已覆盖；最终快照定向 e2e 已通过 |
| 3 | Fastlet Pod UID 是 runtime 物理 owner | admission identity、runtime label、containerd stale-runtime replacement、Janitor authority check | `TestIdentityFencingAndClaimConflict`、`TestSameRuntimeIdentityIncludesPodAndGenerationFences`、严格 AutoRecreate e2e、`TestCleanupDecisionRequiresPodAndAssignmentFences` | 已覆盖 |
| 4 | 旧 generation 不能影响新实例 | instance/assignment/route generation fence 与 tombstone | `TestResetDeletesOldRuntimeThenAdvancesGeneration`、`TestStoreGenerationFencingAndTombstone`、`TestCredentialRoundTripAndFencing`、Proxy reset/delete e2e | 已覆盖；最终快照 Proxy/fault e2e 已通过 |
| 5 | Registry 过期不能突破 Fastlet Admission | Registry 只生成候选；Fastlet 原子 admission 是容量权威 | `TestStaleRegistryHintsCannotExceedFastletCapacity`；40 路跨 FastPath 副本 capacity e2e | 已覆盖；最终快照 controlplane e2e 已通过 |
| 6 | Admission 原子统计所有未清理 slot | Fastlet 原子 `CreateSandbox` 在同一锁域完成容量占用、identity fencing 和创建状态登记；creating/running/deleting/create-cleanup 均占容量，只有 ensure-absent 成功后才释放 slot | `TestAdmissionNeverExceedsCapacityUnderConcurrency`、`TestAtomicCreateRejectsUnavailableNetworkResource`、`TestFailedCreateCleanupIsRetriedBySameIdentity`、`TestDeleteDuringCreateWinsWithoutOrphan`，race Gate | 已覆盖；最终快照完整 race 已通过 |
| 7 | Pool ResourceProfile 是实际 runtime 参数 | Controller 注入 canonical profile；Fastlet 驱动接收资源配置 | `TestConstructPodUsesRuntimeProfileAndFixedResources`、`TestSandboxResourceSpecOptsEnforceCPUAndMemory`、runtime e2e | 已覆盖；最终快照 container/gVisor/Kata e2e 已通过 |
| 8 | 资源限制由 Fastlet/RuntimeDriver 实际执行 | containerd OCI/cgroup 参数；BoxLite capability 不完整时 fail closed | `TestSandboxResourceSpecOptsEnforceCPUAndMemory`、`TestNativeResourceOptionsAndCapabilityBoundary`、container/gVisor/Kata 远端 Gate | 已覆盖；QEMU/CLH 实际通过，Kata FC/BoxLite 精确 fail closed |
| 9 | 镜像命中是 Top-K 核心评分项 | normalized cache inventory + image-first ranking | `TestTopKPrefersImageThenLoadAndDoesNotAllocate`、`BenchmarkRegistryTopK1000` | 已覆盖；多节点真实 cache 证据偏弱 |
| 10 | CRD 创建前可判定的 RPC 快速失败无 CRD；IO1 后的明确拒绝保留声明式 intent | Registry 无候选时直接失败；CRD-first 写入成功后不通过同步删除伪装快速失败。无替代候选时保留 identity；新 Fastlet 出现后从旧 assignment 直接 CAS 到新 assignment，不产生空窗 | `TestNoCandidateFailsBeforeCRDCreate`、`TestFastletRejectionKeepsPersistedIntent`、`TestExplicitCapacityRejectionPreservesDurableAssignmentAndAttemptFence`、`TestReassignDeclarativeAfterRejectionCASesDirectlyToAlternative`、capacity/autoscaling e2e | 已覆盖；最终快照 unit/controlplane/scheduling e2e 已通过 |
| 11 | 相同 request ID + spec 只对应一个 Sandbox | request ID/name、spec hash、API Server Create 唯一裁决和 assignment CAS | `TestConcurrentSameRequestConvergesToOneCRDAndIdentity`、`TestCreateRetryUsesSameCRDAndRuntimeIdentity`、controlplane e2e | 已覆盖；最终快照 controlplane e2e 已通过 |
| 12 | warm/Infra/hot/active 内容受缓存 GC 保护 | runtime-neutral `ProtectionIndex`；containerd 未取得 node-scoped authority 前不执行破坏性删除 | `TestProtectionIndexNeverEvictsWarmActiveInfraOrHotContent` | 已覆盖；node-scoped 主动 GC 明确不宣称 |
| 13 | Fastlet Ready 不等待 warm image | recovery/readiness 与异步 `WarmCache` 分离；pull 结果由有界 counter 观测 | `TestWarmImagesAreAsynchronousAndProtectedFromEviction`；`TestPoolWarmImagesReachRuntimeCacheInventory` 断言真实 cache inventory 与 success metric | 已覆盖；最终快照定向 e2e 已通过 |
| 14 | Sandbox 私网端口不参与全局调度/冲突检测 | 公共 API 无端口声明；Top-K 无端口约束；私网隔离 | private-network e2e 验证独立 netns、地址、DNS、NAT 与回收 | 已覆盖；最终快照 network integration/e2e 已通过 |
| 15 | 两级 Proxy 不解析 execd/envd payload | Proxy 仅校验路由/凭证并透明流式转发目标端口 | `TestProxyForwardsArbitraryPortAndStripsRouteAuthority`、stream/SSE/WebSocket tests、`TestEndpointResolverPreservesRoutePathWhenAuthorityIsOverridden` | 已覆盖 |
| 16 | Create 在 RuntimeReady 后快速返回，required Infra 和 local route 异步决定 DataPlaneReady | Fastlet 原子 Create 完成 runtime 后返回，后台按 fenced identity 推进 Infra 和 route；Controller 独立投影 Runtime/DataPlane 状态 | `TestRoutePublicationContinuesAfterRuntimeReadyWithoutRecreatingRuntime`、`TestDeleteFencesAsynchronousRoutePublication`、`TestResolveEndpointRequiresDataPlaneReady`、Infra/官方 OpenSandbox SDK e2e | 已覆盖；2026-07-23 远端 unit/race、Quick Start Execd 和 Infra e2e 通过 |
| 17 | Sandbox Proxy cache lag 不能变成永久 NotFound | watch cache miss/lag 回源 API Server并回填索引 | `TestKubernetesResolverUsesAuthoritativeFallbackAndWarmsIndex`、`TestSandboxProxyFallsBackOnStaleWatchAndForwardsToAssignedFastlet` | 已覆盖 |
| 18 | 新 Pod 不把旧 Pod runtime 当作己有实例 | assignment 同时绑定 Pod name + UID；PodLost 走 generation rebuild；containerd 发现旧 identity 时 ensure-absent 后再创建。SIGKILL 后等待退出；创建补偿失败可由同 identity 续做，用户删除失败不可由 Create 复活 | `TestReplacementPodWithSameNameCannotClaimOldAssignment`、`TestSameRuntimeIdentityIncludesPodAndGenerationFences`、`TestFailedCreateCleanupIsRetriedBySameIdentity`、`TestUserDeleteFailureCannotBeResurrectedByCreateRetry`、严格 AutoRecreate e2e | 已覆盖；最终快照 fault-recovery e2e 已通过 |
| 19 | Pool 缩容和计划升级先 Drain | desired template hash；单 Pod ready surge；精确 Pod UID heartbeat Runtime/Infra readiness；持久化 `planned-upgrade` intent；复用 ack/load/timeout/Leader 接管 | `TestPlannedUpgradeWaitsForReadySurgeThenDrainsOldTemplate`；`TestPoolPlannedUpgradeUsesReadySurgeAndDurableDrain`；既有 scale-down unit/e2e | 已覆盖；最终快照定向 unit/e2e 已通过 |
| 20 | Janitor 只做异常兜底，不进入正常热路径 | Janitor 独立 DaemonSet/进程，仅扫描带 identity fence 的孤儿后端资源；正常 delete 由 Controller -> Fastlet 完成 | Janitor authority/fail-closed tests、`TestJanitorRecovery`；正常删除由 `TestDeletionFinalizerWaitsForV2RuntimeDeletion` 覆盖 | 已覆盖；最终快照 fault/janitor e2e 已通过 |

## 3. 最终完成 Gate 审计

| Gate | 状态 | 结论/证据 |
|---|---|---|
| 设计文档与代码无架构冲突 | 已满足 | CRD-first、Proxy 待讨论边界、planned-upgrade/role=all 已回写；旧 runtime 文档显式标为 superseded |
| 20 条不变量均有自动化测试 | 已满足 | unit/race/远端功能矩阵均在最终源码快照执行 |
| FastPath 多活 | 已满足 | 三 Pod 直连、Leader 切换和并发 capacity e2e 通过 |
| Controller 单 Leader | 已满足 | 2 副本单 Leader及删除接管 e2e 通过 |
| Controller-only | 已满足 | controlplane e2e 实际将 FastPath Deployment 缩为 0，CRD 仍完成 Ready，再恢复 3 副本 |
| `role=all` 开发模式 | 已满足 | overlay dry-run；真实单 Pod/唯一 Service endpoint/RPC Create e2e 通过并恢复 split topology |
| request ID / CRD-first 失败语义 | 已满足 | `request_id == metadata.name`；创建前无候选不写 CRD；IO1 后失败保留 intent；unit/race + controlplane e2e 通过 |
| admission/recovery/generation | 已满足 | unit/race + fault/proxy e2e 通过 |
| Pool runtime/resources/warmImages 实际生效 | 已满足 | runtime/profile 与真实 warm cache inventory/metric e2e 通过 |
| 私网、同端口、NAT、代理 | 已满足 | privileged network integration + network/proxy e2e 通过 |
| required Infra / SDK Adapter | 已满足 | Fast Sandbox Core 不复制 Execd 协议；`fastctl opensandbox` 与 Go Adapter 使用官方 OpenSandbox Go SDK，远端命令流/文件下载 e2e 通过；envd 专属入口已删除 |
| runtime 支持声明 | 已满足声明边界 | container/gVisor/QEMU/CLH 实际通过；Kata FC/BoxLite 准确 fail closed |
| Drain / Janitor | 已满足 | scale-down/planned-upgrade/fault/janitor e2e 通过 |
| 完整 remote verification matrix | 已满足约定能力矩阵 | 当前源码快照的 10 个 E2E suite 已通过 `test-e2e-controlplane` 超集和其余分包命令全部执行；精确命令、退出码和分包耗时见验收报告 |
| 性能、部署、运维文档 | 已满足本轮边界 | 生产硬件和多节点 cache 报告明确保留为发布前限制；未部署旧 API 无迁移路径 |
| 禁止路径不存在 | 已满足 | proto 无 Exec/File；调度无端口冲突状态；FastPath 无 reservation，仅在 CRD commit/UID/assignment 后调用一次原子 Fastlet Create |

## 4. 当前非阻塞限制

这些项目不阻塞本轮约定架构边界，但必须保持显式，不得包装成已支持能力：

- `kata-firecracker` 已定位为节点基线同时缺少 Firecracker MMIO kernel 和 block snapshotter；修复路径见 [Kata Firecracker 调查](./kata-firecracker-investigation.md)，当前继续 capability fail closed；
- BoxLite v0.9.7 缺少不可绕过的完整资源限制，`runtime=boxlite` 保持 fail closed；
- containerd image GC 已确定由调度层生成 `NodeCachePlan`、NodeJanitor 本地复核执行；协议实现前不执行破坏性共享镜像删除；
- 生产 OpenSandbox execd 是重点 Infra 接入 case，artifact、签名和版本绑定后续按接入指南完成；E2B envd 的内置 profile、专属 Adapter 和 SDK 入口已删除；
- 目标生产硬件性能、多节点真实 cache affinity 和私有镜像凭证刷新仍是发布/后续议题；
- snapshot、pause/resume、持久化 storage、live migration 不在本轮范围。

## 5. 完成判断

本轮设计和实施计划中的阻塞项已关闭。第 4 节仍是发布/后续能力边界，不得在产品声明中省略；它们不改变本轮架构重构完成判断。
