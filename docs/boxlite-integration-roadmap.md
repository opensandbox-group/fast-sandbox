# Fast Sandbox × BoxLite 重点投入路线

**状态**：方向已确认；运行时代码暂时保持现状
**更新时间**：2026-07-21
**上游基线**：[boxlite-ai/boxlite v0.9.7](https://github.com/boxlite-ai/boxlite/releases/tag/v0.9.7)，commit `8803834036205cf2cac5cfca98bb3875812c897a`
**已有实现 ADR**：[BoxLite Runtime Adapter ADR](superpowers/specs/2026-07-19-boxlite-runtime-adapter-adr.md)

## 1. 结论

BoxLite 是 Fast Sandbox 后续 Runtime 扩展的重点投入方向，但当前不能把“已经接通 native Sidecar 原型”表述为“已经具备生产支持”。本轮不继续修改 BoxLite 代码，也不解除 `RuntimeUnsupported` 门禁；先把需要与上游共同推进的产品边界、接口缺口和验证条件统一成路线图。

Fast Sandbox 的目标不是重新实现 BoxLite，而是让 BoxLite 通过稳定的 RuntimeDriver、AccessHandle、Infra Component 和 cache/recovery 契约进入统一控制面：

```text
Fast Sandbox control plane
  -> RuntimeProfile(runtime=boxlite)
  -> Fastlet BoxLiteDriver (pure Go)
  -> Pod-local versioned UDS
  -> boxlite-runtime sidecar (Go SDK + CGO/native)
  -> one persistent Box per Sandbox UID
```

BoxLite 的 native library、KVM、libkrun、gvproxy 和持久状态继续隔离在独立 Sidecar 中；Fastlet 不链接 CGO，不把 BoxLite 伪装成 containerd handler。

## 2. 当前事实

BoxLite v0.9.7 已公开支持 OCI image、per-Box microVM、CPU/memory resource、volume、TCP/UDP forward、image cache、persistent Box、metrics 和 Go/C/Rust/Python/Node SDK。Fast Sandbox 当前代码已经具备：

- `BoxLiteDriver` 的纯 Go UDS client；
- `boxlite-runtime` Sidecar 和版本化 wire model；
- Sandbox UID、Fastlet Pod UID、instance generation、assignment attempt fencing；
- ArtifactVolume 形式的 `sandbox-init`/Infra bundle 注入；
- guest `sandbox-tunnel` + credential-protected LocalForward；
- List/Inspect/Delete/Recover 和 image cache 接口；
- BoxLite Janitor scanner；
- capability negotiation 和 fail-closed profile。

但下面三项仍然阻止生产支持声明。

## 3. BOX-001：资源语义必须由 host 强制执行

### 3.1 当前问题

Fast Sandbox 的 Pool ResourceProfile 要求 CPU、memory 和 PIDs 是平台边界，而不是 guest root 用户可以修改的建议值。v0.9.7 的 Go/C 高层 API能设置整数 vCPU 和 VM memory，但没有完整暴露底层 jailer 已存在的 host cgroup resource setters，也没有与 guest workload 等价的 PIDs 控制面。

仅由 `sandbox-init` 在 guest cgroup v2 中写 `cpu.max`、`memory.max`、`pids.max` 不够：默认 root workload 可以修改控制文件或把进程移回父 cgroup。通过删除 root capabilities 强行封锁也不可接受，因为它会暗中改变用户 OCI image 的 root、chown、低端口和系统工具语义。

### 3.2 需要与上游讨论的接口

推荐优先推动一个版本化的 create-time `ResourceLimits` API：

```text
cpu quota / period or millicores
memory hard limit
guest workload PIDs limit
optional swap / oom behavior
host-side immutable enforcement identity
effective limits returned by Inspect/Stats
```

它必须在用户进程启动前生效，并且由 host/jailer 强制执行。若上游选择 guest OCI cgroup 方案，则需要提供 root workload 不可迁移、不可放宽的可信边界，而不是只写一次配置文件。

### 3.3 解除门禁条件

- Go/C API 能表达 Pool 的 fractional CPU、memory、PIDs；
- Sidecar `EnsureBox` 返回 effective resource limits；
- guest root 负测不能放宽限制、迁出受控 cgroup 或绕过 PIDs；
- Sidecar restart/recovery 后限制不丢失；
- 对应能力进入 protocol capability，例如 `resource-limits-v2`。

## 4. BOX-002：从静态 Host Port 迁移到 native tunnel

### 4.1 当前兼容层

v0.9.7 只能在 Box 创建时固定 port mapping，并且 Go SDK 不接受非空 `HostIP`。当前 Fast Sandbox 因而为每个 Box 租一个 Pod TCP 端口，固定映射到 guest `sandbox-tunnel:19090`，再通过带 credential 的 tunnel preamble 转发任意 target port。

这个方案保持了“不同 Sandbox 可使用全部私有端口”的外部语义，但仍有几个成本：

- 每个 Box 消耗一个 Pod-local port lease；
- gvproxy forward 的监听面大于 loopback，需要额外 credential 封住旁路；
- Sidecar/guest tunnel recovery 需要同时恢复 port、credential 和 relay；
- 大规模 Pool 下端口池和 conntrack 会成为额外容量维度。

### 4.2 长期目标

建议与上游讨论暴露 per-Box native stream/tunnel handle：

```text
Dial(boxID, network=tcp, targetHost, targetPort) -> bidirectional stream
or
OpenTunnel(boxID) -> Unix socket / FD / framed stream
```

优先级是 Go/C FFI，随后由 `boxlite-runtime` Sidecar通过 UDS streaming CONNECT 暴露给 Fastlet Proxy。Fastlet Proxy 仍只消费统一 AccessHandle，不理解 gvproxy 或 libkrun 内部协议。

目标形态：

```text
Sandbox Proxy
  -> Fastlet Proxy
  -> Pod-local UDS stream
  -> boxlite-runtime
  -> BoxLite native network handle
  -> guest private target port
```

该方向可以删除每 Box host port、guest `sandbox-tunnel` 和 LocalForward credential 兼容层，同时保持上层透明 target-port 路由。

### 4.3 解除兼容层条件

- native stream 支持任意 TCP target port；
- cancel、half-close、backpressure、long-lived SSE/WebSocket 行为明确；
- stream 与 Box identity/owner fence 绑定；
- Sidecar 重启后能重新获得 handle，或明确使旧连接失败并重建路由；
- 双 Box 同 guest port 和跨 Box 隔离 E2E 通过。

## 5. BOX-003：Kubernetes 网络和恢复必须实证

当前不能只根据 gvproxy 源码推断 Kubernetes 行为。需要在真实 Linux/KVM Kubernetes 节点验证：

- Box 出站流量在 Pod/Node 上看到的源地址；
- CNI NetworkPolicy 是否能观察和约束该流量；
- DNS、conntrack、NAT 和大量长连接行为；
- Fastlet Pod network namespace 重建后的 Box/forward 结果；
- `boxlite-runtime` Sidecar restart 后 state、tunnel 和 AccessHandle 恢复；
- Fastlet 进程重启、Fastlet Pod 删除、node reboot 三种边界；
- BoxLite state root、`.lock`、owner fence 与 Janitor 的并发安全；
- 多 Box、同端口、跨 namespace、跨 Pool 的隔离。

如果 BoxLite native tunnel 先落地，网络矩阵应直接验证 native tunnel；现有 LocalForward 只作为 v0.9.7 兼容层保留一套回归测试。

## 6. 与 BoxLite 创始团队建议讨论的议题

按合作价值排序：

1. **Host-enforced ResourceLimits API**：Fast Sandbox 可以贡献需求模型、负测和 Go/C binding。
2. **Per-Box native tunnel/stream**：确认 libkrun/gvproxy 已有能力、最小 FFI 和 cancellation 语义。
3. **稳定恢复契约**：Box identity、List、Inspect、GetOrCreate、Reconnect、ForceRemove 和错误分类。
4. **Pool/prewarm 接口**：base image、rootfs/template、VM boot artifact 的预热和可观测 hit/miss。
5. **Kubernetes 支持矩阵**：Sidecar/DaemonSet、rootless、KVM device、NetworkPolicy、node reboot。
6. **Capability negotiation**：版本化报告资源、tunnel、recovery、cache 和 metrics 能力。
7. **发布与供应链**：Go SDK、C ABI、native library、guest image 和 gvproxy 的兼容矩阵与签名 artifact。
8. **Infra Component 结合**：在 Box create 前挂载不可变 bundle，或通过 image/template bake 提前准备 execd。

Fast Sandbox 可以提供的上游贡献包括：

- Kubernetes/KVM 的可复现测试环境；
- resource escape 负测；
- native tunnel Go/C API 原型和压力测试；
- Sidecar recovery、fencing 和 Kubernetes E2E；
- OpenSandbox execd 作为实际 Infra Component 的联合集成 case。

## 7. 分阶段投入计划

### Stage 0：冻结现有原型

- 代码维持当前 UDS Sidecar + LocalForward；
- `boxlite` profile 继续 fail closed；
- 不把 capability gate 通过描述为生产支持。

### Stage 1：上游接口对齐和 spike

- 与上游确认 ResourceLimits 和 native tunnel 路线；
- 在独立分支验证最小 Go/C API；
- 确定 BoxLite 版本与 ABI 支持周期。

### Stage 2：native tunnel

- Sidecar 提供 UDS streaming；
- AccessHandle 增加/启用 `RuntimeTunnel`；
- 保留 LocalForward fallback，但默认不再分配 host port。

### Stage 3：资源边界

- 接入 host-enforced ResourceLimits；
- 完成 CPU、memory、PIDs 正向和逃逸负测；
- 恢复路径重新验证 effective limits。

### Stage 4：Kubernetes 全矩阵

- 网络、NetworkPolicy、重启、node loss、Janitor、cache、Infra readiness；
- 多节点和规模压力测试。

### Stage 5：生产支持声明

只有 capability、E2E、race、故障恢复和观测证据全部成立，才把内置 profile 从 Unsupported 改为 Ready。

## 8. 明确不做的事情

- 不把 BoxLite 接入伪装成 containerd runtime handler；
- 不把 guest 自写 cgroup 当成 host resource boundary；
- 不让 Fastlet 直接链接 CGO/native library；
- 不让 Fast Sandbox 定义 BoxLite 内部 exec/file 协议；
- 不把静态 host port 暴露为用户端口分配模型；
- 不在缺少 Kubernetes 实证时解除 fail-closed 门禁。

## 9. 完成定义

BoxLite 的生产接入只有同时满足下列条件才算完成：

- 资源限制对 guest root 不可绕过；
- 任意 target port 通过 native、身份绑定的 stream 访问；
- Kubernetes 网络和 NetworkPolicy 行为有真实证据；
- Fastlet/Sidecar/Pod/node 故障恢复边界清楚；
- Infra Component 能在用户 entrypoint 前可靠注入并探活；
- Pool cache/prewarm、heartbeat 和 Top-K 能消费统一 inventory；
- 所有宣称能力有远端 E2E 和负向 Gate。
