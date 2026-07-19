# BoxLite Runtime Adapter ADR

**状态**：已决策，生产能力门禁未解除
**调研基线**：[boxlite-ai/boxlite v0.9.7](https://github.com/boxlite-ai/boxlite/tree/v0.9.7)，commit `8803834036205cf2cac5cfca98bb3875812c897a`

## 1. 结论

Fast Sandbox 的 BoxLite 接入选择“独立 Runtime Sidecar + 纯 Go Fastlet Client”，不把 BoxLite CGO/native library 链接进 Fastlet，也不把 BoxLite 伪装成 containerd handler。

Sidecar 内部使用 BoxLite Go SDK 的 in-process Runtime。一个长期 Runtime 管理当前 Fastlet Pod 的多个 Box；每个 Sandbox UID 映射为一个稳定 Box name。Fastlet 只通过 Pod-local UDS 调用窄化后的 lifecycle/cache API。

上游 `boxlite serve` 不能直接作为该 Sidecar。v0.9.7 的 REST create schema 没有 volume 和 port mapping 字段，并且拒绝未知字段；port mapping 只能在 Box 创建时通过 in-process SDK 设置，不能动态增删。它因此无法同时满足 Infra Component 注入和任意 target port 透明代理。

在 Sidecar 能完整执行资源限制并通过远端 E2E 前，内置 `boxlite` profile 必须保持 fail closed：`RuntimeReady=False/RuntimeUnsupported`，不能只因为节点存在 `/dev/kvm`、已打包 Sidecar/guest tunnel 或 `boxlite` CLI 就报告 Ready。

## 2. 原型比较

| 方向 | 结论 | 原因 |
|---|---|---|
| Go SDK in-process，直接链接 Fastlet | 否决 | SDK 需要 Go 1.24+、CGO 和预编译 native library；native 崩溃、ABI 和依赖会进入 Fastlet 控制进程 |
| 原样运行 `boxlite serve` | 否决 | lifecycle/list/recover 可用，但 REST schema 不暴露 volume/port mapping，无法注入 helper，也无法建立 tunnel |
| 独立 Sidecar，内部使用 Go SDK | 采用 | native 故障域独立；可以使用 `WithVolume`、`WithPort`、resource、List/GetOrCreate/ForceRemove 和 image cache API；Fastlet 保持纯 Go |

这不是把数据面协议重新收回 Fast Sandbox。Sidecar 只实现 runtime lifecycle、artifact delivery 和 runtime-specific access handle；exec/files 仍由用户选择的 Infra Component 提供。

## 3. 目标部署形态

```text
Fastlet Pod
├── fastlet
│   └── BoxLiteDriver (pure-Go UDS client)
├── fastlet-proxy
│   └── LocalForward transport
└── boxlite-runtime
    ├── one BoxliteRuntime
    ├── one Box per Sandbox UID
    ├── libboxlite / libkrun / gvproxy
    └── per-Box host-forward lease

Box guest
├── user OCI image workload
├── injected Infra Components
└── sandbox-tunnel :19090
```

`boxlite-runtime` 容器需要 `/dev/kvm`，并把 `/var/lib/fast-sandbox/boxlite` 作为宿主扫描根目录。实际 `BOXLITE_HOME` 必须是 `/var/lib/fast-sandbox/boxlite/<FastletPodUID>`，不同 Fastlet Pod 不共享可写 home。

Fastlet 与 Sidecar 共享：

- `/run/fast-sandbox/boxlite`：控制 UDS；
- `/opt/fast-sandbox/infra`：Sidecar 可见的只读 artifact store；
- 当前 Pod UID 对应的 BoxLite state 目录。

## 4. 生命周期协议

Sidecar UDS protocol 只暴露以下 runtime primitive：

```text
Probe(version, capabilities)
EnsureBox(identity, image, process, env, resources, artifacts, tunnel)
InspectBox(sandboxUID)
RecoverBox(sandboxUID)
DeleteBox(identity)
ListBoxes(ownerFastletPodUID)
ListImages / PullImage
```

关键语义：

1. `EnsureBox` 以 Sandbox UID 作为 Box name，通过 `GetOrCreate` 幂等；
2. Sidecar 在返回成功前校验 image、CPU/memory、instance generation、assignment attempt 和 owner Pod UID；
3. 已存在 Box 的 immutable create spec 不一致时返回 Conflict，不覆盖；
4. Fastlet 进程重启后重新连接 Sidecar并通过 `ListBoxes` 恢复；
5. Fastlet Pod UID 改变时禁止接管旧 Box；旧资源只进入 Janitor；
6. Sidecar 重启时从自己的唯一 `BOXLITE_HOME` 恢复 Runtime；Fastlet 在发布 Route 前显式调用 `RecoverBox` 重建/确认 guest tunnel；
7. PIDs 是 Fastlet admission 和 guest policy 的约束。BoxLite v0.9.7 SDK 没有等价 PIDs knob，因此在该约束可验证前 profile 不能宣称资源语义完整。

## 5. 任意端口的 LocalForward

上游固定 port mapping 只用于承载一个内部 tunnel，不把用户 target port 固化进 registry 或 Box 配置：

1. 每个 Box 创建时，Sidecar 从 Pod-local port pool 租一个 host port；
2. 通过 BoxLite `WithPort` 固定映射到 guest `sandbox-tunnel:19090`；
3. `sandbox-tunnel` 的每条连接先接收目标端口和协议 preamble，再连接 guest loopback 的实际 target port；
4. `BoxLiteDriver.GetAccessDescriptor` 返回 `LocalForward(127.0.0.1:<leased-port>)`；
5. Fastlet Proxy 根据外部请求里的 signed target port 写入 preamble，随后透明转发 HTTP/WebSocket/SSE 字节流。

因此：

- 同一 Box 的任意端口无需动态修改 gvproxy；
- 两个 Box 可以同时监听相同 guest port；
- 端口池仅是 Fastlet Pod 内 runtime transport 资源，不是 Sandbox 对外端口分配，也不进入调度 Registry；
- 不同 Fastlet Pod 有独立网络 namespace，可以复用同一组 tunnel host ports。

LocalForward 建立失败、preamble 被拒绝或 guest tunnel 未 Ready 都必须使 `DataPlaneReady=False`，不能回退成 PodIP 或预声明用户端口。
Sidecar/guest tunnel 收到终止信号时会关闭 active relay；长期 HTTP、WebSocket 或 SSE 连接不能阻塞 Runtime 回收。

v0.9.7 还有一个必须显式门禁的限制：Go SDK 会拒绝非空 `HostIP`，native port forward 固定绑定所有 host interfaces。因此把 AccessDescriptor 写成 `127.0.0.1:<port>` 并不能阻止其他 Pod 直接访问 Fastlet PodIP 上的同一端口。当前功能 spike 已证明动态转发可行，但 `local-forward-v1=false`；生产实现必须增加每 Box 不可猜测的 tunnel authentication（并防止跨 Box 复用），或在 Pod network namespace 建立经过测试的非 loopback DROP policy，且由 recovery/Janitor 一并管理。该门禁解除前不能开放 Route。

## 6. Infra Component 注入

实现阶段验证了一个重要的生命周期约束：`sandbox-init` 和 `sandbox-tunnel` 必须在用户 entrypoint 与 tunnel readiness 之前已经存在，而 BoxLite `CopyInto` 只能在 Box 已可执行后使用。因此首个实现不再把 create-time bootstrap 误称为 `GuestCopy`，而是明确采用 `ArtifactVolume`：

1. Fastlet 用 Infra Catalog 解析 digest，并把 artifact 放入 Pod 共享的只读 store；
2. Fastlet 把 immutable prepared plan 和 Sidecar 可见的 artifact path 发送给 Sidecar；
3. Sidecar只接受共享 store 根目录内、目标位于 `/.fast` 下的 regular file，复制成按 create-spec hash 隔离的只读 bundle；
4. Box 创建时通过 SDK `WithVolumeReadOnly(bundle, "/.fast")` 挂入 guest，保证 `sandbox-init`、`sandbox-tunnel`、组件和 instance config 在用户进程启动前可见；
5. Sidecar通过 Box execution 启动 tunnel，并等待协议级 health handshake；Fastlet 再通过 LocalForward 完成 required Infra readiness，最后发布 Route。

该模式对 Fastlet 的统一抽象仍是 artifact delivery，不暴露 BoxLite SDK。后续可用 TemplateBake/Preinstalled 降低逐 Sandbox bundle 成本；`CopyInto` 只适合启动后的增量更新，不能承担 boot-critical artifact。任何实现都不能改变 InfraProfile identity、digest 校验和 readiness 语义。

## 7. Cache、恢复和 Janitor

- Sidecar 的 `ListImages` 和 `PullImage` 对应 Fastlet `RuntimeArtifactCache`，Heartbeat/Top-K 仍只消费统一 image inventory；
- warmImages 异步拉取，不阻塞新 Fastlet 进入 Ready；
- Box metadata 持久化 Sandbox UID、Fastlet Pod UID、instance generation、assignment attempt、leased tunnel port 和 create-spec hash；每个 home 另有不可逆目录名之外的 owner fence 文件；Sidecar 加载时同时校验 owner Pod UID、record filename、spec hash 和 bundle root，损坏或跨 Pod 的记录 fail closed；
- BoxLiteJanitor 扫描 `/var/lib/fast-sandbox/boxlite/<hash(podUID)>` 的 owner/record/bundle/state，先查询 Pod UID 是否仍存活，再以 record fence 和 Runtime `.lock` 二次校验；仍被 Runtime 持锁时 fail closed，最后一个 record 消失后才回收该 Pod 的 image/base/db home；
- Janitor 不通过 containerd scanner 猜测 BoxLite 资源；
- Pod 删除模型仍是“所属 Sandbox 全部 Lost/重建”，不允许新 Fastlet Pod 接管旧 Box。

## 8. 能力门禁

只有下列条件全部成立，`boxlite` profile 才能从 Unsupported 改为 Configured/Ready：

1. Sidecar protocol version 与 Fastlet catalog 匹配；
2. `/dev/kvm`、native SDK、gvproxy、state root 和 UDS 全部可用；
3. Sidecar能验证 `GetOrCreate/List/Inspect/Delete/Recover`；
4. guest tunnel 和 ArtifactVolume 通过版本、路径边界和摘要校验；
5. CPU/memory/PIDs 的产品语义都有可执行证据；
6. host forward 不能从 Fastlet Pod loopback 之外绕过 route credential；
7. BoxLiteJanitor 已接入真实 scanner；
8. 远端 E2E 覆盖双 Box 同 guest port、任意 target port、Infra readiness、Fastlet 重启、Pod 丢失和 cache heartbeat。

当前 `make test-e2e-runtime-boxlite` 是显式的 fail-closed capability gate，不是 BoxLite 支持完成的证据。它的存在是为了避免 CI skip 或误报 Ready。

## 9. 上游依赖和后续实现切片

实现按以下顺序推进：

1. 定义并生成 Sidecar UDS protocol，完成 fake server contract tests；
2. 实现 `AccessKindLocalForward` 和 tunnel transport 的 unit/integration tests；
3. 建立独立 Sidecar build（Go 1.24+、CGO、固定 BoxLite SDK/native artifact 版本）；
4. 实现 `sandbox-tunnel` 和 ArtifactVolume，接入 Infra readiness；
5. Controller按 RuntimeProfile 注入平台-owned Sidecar、volume 和 security context；
6. 实现 BoxLiteDriver lifecycle/cache/recovery；
7. 实现 BoxLiteJanitor scanner；
8. 完成远端 BoxLite 全矩阵后解除 capability gate。

若上游未来原生支持创建时 volume/port mapping 的 REST schema、动态 forwarding 或等价的 per-Box Unix access socket，可替换 Sidecar内部实现；Fastlet 的 RuntimeDriver、LocalForward 和 InfraProfile 抽象不变。
