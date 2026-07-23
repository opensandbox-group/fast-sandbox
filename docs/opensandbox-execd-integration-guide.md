# OpenSandbox execd 接入 Fast Sandbox 指南

**状态**：开发 Quick Start 已绑定真实 artifact；生产 artifact 尚未绑定
**更新时间**：2026-07-23
**上游参考**：[OpenSandbox architecture](https://github.com/alibaba/OpenSandbox/blob/main/docs/architecture.md)、[execd OpenAPI](https://github.com/alibaba/OpenSandbox/blob/main/specs/execd-api.yaml)

## 1. 核心结论

Fast Sandbox 不定义自己的 Exec/File/PTY 协议。OpenSandbox `execd` 作为 Infra Component 注入 Sandbox，Fast Sandbox 只负责组件投递、启动、实例初始化、服务发现、鉴权材料、readiness 和透明代理；调用方继续使用 OpenSandbox SDK/execd 协议。

```text
OpenSandbox SDK / execd client
  -> Fast Sandbox resolve access endpoint
  -> Sandbox Proxy
  -> Fastlet Proxy
  -> Sandbox private network :44772
  -> opensandbox-execd
```

Proxy 对 HTTP、SSE、WebSocket 和 PTY 字节流透明，不解释 `/command`、`/files`、`/pty` 等业务协议。

## 2. 双方职责

### Fast Sandbox

- 通过 Pool `infraProfile=opensandbox-execd` 选择组件；
- 解析并校验不可变 execd artifact digest；
- 把 execd、bootstrap 和必要依赖投递到 Sandbox；
- 生成每个 Sandbox generation 独立的内部 token/config；
- 启动和监督组件；
- 对 `GET /ping` 做 readiness；
- 将逻辑 service `execd` 注册为私网端口 `44772`；
- 发布带 generation fence 的代理路由；
- SDK Adapter 返回 endpoint 和必须的请求 header；
- 删除/reset/reassignment 时撤销旧路由和旧 token。

### OpenSandbox execd

- command、background command、session 和 PTY；
- file/directory API；
- code context/Jupyter adapter；
- SSE/WebSocket 协议；
- execd 内部进程、文件和 metrics 语义；
- 校验 `X-EXECD-ACCESS-TOKEN`；
- 保持其 OpenAPI 和官方 SDK 兼容。

Fast Sandbox 不复制 execd OpenAPI，也不把 execd 方法加入 RuntimeDriver。

## 3. 真正的 Infra Component 抽象

抽象不是“在 Fastlet 中硬编码一个 helper”，而是：

> 基于用户提供的 OCI image 启动 workload，同时向 Sandbox 注入一组由平台选择和管理的 let/agent 组件，使同一个 Sandbox 同时具备用户镜像能力和平台数据面能力。

它由五部分组成：

```text
InfraProfile
  Artifact   -> 什么二进制/脚本，版本和 digest
  Delivery   -> 如何在用户进程启动前进入 Sandbox
  Activation -> 谁启动和监督
  InstanceInit -> 本次 generation 的 token/config
  Services   -> 私网端口、readiness 和 route metadata
```

`opensandbox-execd` 的映射是：

```text
Artifact       OCI image/artifact 中的 execd + bootstrap + optional bwrap
Delivery       container/gVisor: read-only bundle mount
Activation     ComponentBootstrap，后续可统一为 EntrypointSupervisor
InstanceInit   root-only config/environment，包含 access token
Service        execd/http/44772
Readiness      GET /ping
```

## 4. sandbox-init 是什么

`sandbox-init` 是 Fast Sandbox 提供的最小 PID1/supervisor，不是 Exec/File Agent，也不是代理。

它存在的原因是普通用户 OCI image 通常没有 systemd，但平台又需要在一个 Sandbox 中可靠运行 execd 和用户 entrypoint。

职责：

- 读取由 Fastlet 生成的 root-only Infra instance config；
- 恢复用户 image 的原始 entrypoint、args、env、cwd、UID、GID 和附加组；
- 启动 execd/bootstrap 等 Infra Component；
- 按依赖和 `startBeforeUser` 控制启动顺序；
- 执行 readiness 前置动作并报告 component diagnostics；
- 以原始用户身份启动用户进程，内部 token 不进入用户环境；
- 转发 SIGTERM/SIGKILL、管理进程组、回收孤儿子进程；
- 保留用户进程退出码和 Sandbox 停止语义；
- 根据声明执行 component restart policy。

它不提供：

- 对外 Exec/File/PTY API；
- Sandbox Proxy 能力；
- 调度或 CRD reconcile；
- runtime-native lifecycle API。

## 5. 启动顺序与延迟

默认情况下，execd 和用户进程可以并行启动：

```text
containerd/VM task started
  -> sandbox-init
       +-> execd/bootstrap -> /ping Ready -> route publish
       +-> user entrypoint
```

只有 execd bootstrap 必须先修改用户进程依赖的 mount、CA 或安全环境时，才使用 `startBeforeUser=true`：

```text
task -> sandbox-init -> execd/bootstrap Ready -> user entrypoint
```

对 CreateSandbox 时延的影响包括：

1. artifact 命中检查、digest 校验和必要的下载/解包；
2. bundle mount 或 VM artifact delivery；
3. `sandbox-init` 自身启动；
4. bootstrap/execd 进程启动；
5. `/ping` readiness；
6. route publish。

降低时延的原则：

- execd artifact 进入 Fastlet 保护缓存，Pool 预热镜像不被 GC；
- container/gVisor 使用只读 bundle mount，避免逐 Sandbox 复制；
- VM/BoxLite 优先 TemplateBake/Preinstalled；
- execd 与用户 entrypoint 在语义允许时并行；
- `CreateSandbox` 在 RuntimeReady 后返回，execd readiness 和 route publication 异步推进到 DataPlaneReady；
- 不把 sandbox-init 启动时间冒充用户原始进程启动时间。

## 6. 开发 Quick Start 与生产 artifact 绑定

### 6.1 可运行的开发绑定

`make quickstart` 使用独立的 `opensandbox-execd-quickstart` profile：

- Fastlet 镜像从 OpenSandbox Execd v1.0.21 的不可变 amd64 image digest 提取 `/execd`；
- profile 同时固定 `/execd` 的文件级 SHA-256，Fastlet 准备 artifact 时再次校验；
- artifact 位于 Fastlet 镜像只读路径 `/opt/fast-sandbox/components`，再进入 Fastlet 的 content-addressed store；
- `sandbox-init` 并行启动 Execd 和用户 entrypoint；
- 每个 Sandbox generation 的随机 token 通过 `EXECD_ACCESS_TOKEN` 传给 Execd；
- Fastlet Proxy 仅在 Execd 上游 hop 注入 `X-EXECD-ACCESS-TOKEN`；
- `GET /ping` Ready 后才发布 `:44772` route。

该 profile 只允许 `runtime=container`，只用于 kind/开发验收。它没有签名验证、多架构 artifact 选择、私有镜像源和 release compatibility matrix，因此不能作为生产 profile 使用。

自动化门禁是：

```bash
make test-e2e-quickstart
```

它通过真实 `fastctl` 和官方 OpenSandbox Go SDK 验证 create、get、diagnostics、exec、file upload/stat/read/download 与声明式 delete；不会使用返回固定结果的 `test-infra`。

### 6.2 生产绑定

当前内置 profile 有意 `Configured=false`。发布系统需要提供：

- 不可变 execd OCI reference；
- digest，而不是浮动 tag；
- execd、bootstrap、optional bwrap 的文件清单和 mode；
- artifact 签名和 verifier policy；
- 离线/私有镜像源策略；
- execd API/version 与 Fast Sandbox Adapter 的兼容矩阵。

推荐把 release binding 与代码中的逻辑 profile 分离：逻辑 profile 描述 service/activation，部署配置选择具体 artifact digest。未绑定或校验失败时 Pool 必须 fail closed，不能创建不带 execd 的 Sandbox 后谎报 DataPlaneReady。

## 7. 鉴权和服务发现

每个 Sandbox generation 生成独立 token：

- token 不写入 Sandbox CRD；
- 只进入 root-only instance config、execd 和对应 Fastlet local route；
- Sandbox Proxy 对外使用平台 route credential；
- Fastlet Proxy 向 execd 注入 `X-EXECD-ACCESS-TOKEN`；
- reset/reassignment 后旧 generation token 和 route 必须失效。

SDK Adapter 返回：

```text
base URL: /v1/sandboxes/{sandboxUID}/ports/44772/...
headers:  Fast Sandbox route authentication headers
```

OpenSandbox SDK 仍负责构造 execd 业务请求；调用方不需要知道 Fastlet Pod IP 或 Sandbox 私网 IP。

## 8. 不同 Runtime 的 Delivery

| Runtime | 推荐 Delivery | Activation | 说明 |
|---|---|---|---|
| container | BindMount bundle | sandbox-init/bootstrap | 最低启动成本 |
| gVisor | BindMount bundle | sandbox-init/bootstrap | 与 container 保持相同逻辑 profile |
| kata-qemu/clh | TemplateBake/GuestCopy | guest service 或 sandbox-init | 需要 guest channel/模板能力 |
| kata-fc | 暂不承诺 | 暂不承诺 | Firecracker 支持门禁尚未解除 |
| boxlite | ArtifactVolume/TemplateBake | sandbox-init/bootstrap | 生产资源和 native tunnel 门禁尚未解除 |

Runtime-specific delivery 只影响 Injector，不改变 execd service identity、端口或 SDK。

## 9. 生命周期语义

### Create

```text
resolve InfraProfile
  -> prepare/verify artifact
  -> inject bundle + instance config
  -> start sandbox-init/execd/user process
  -> execd /ping
  -> publish route
  -> DataPlaneReady
```

### Delete

删除是声明式的：CRD 进入 Draining，先撤销 route，再停止 runtime。execd 不承担 CRD 删除逻辑。

### Reset/Reassignment

- lifecycle generation 增加；
- 生成新 token/config；
- 旧 route generation 失效；
- 重新执行 readiness 后才发布新路由。

Pause/resume、snapshot 和持久化 storage 不在本指南范围内，后续必须同样刷新动态身份。

## 10. E2B envd 的定位

`e2b-envd` 不再作为当前支持能力：内置 profile、专属 Adapter 和 SDK
入口均已删除，避免在没有 VM template、artifact 和端到端证据时形成
虚假的产品承诺。其 TemplateBake + SystemService + `/init` 模型只作为
历史设计案例保留；未来若有明确消费者，必须重新走 profile、供应链、
官方 SDK hand-off 和 E2E capability gate，不能直接恢复旧声明。

## 11. 接入验收清单

- 生产 execd artifact/digest/signature 已绑定；
- container 和 gVisor 的 bundle 注入通过；
- 非 root 用户镜像的 UID/GID/cwd/env/entrypoint 保持一致；
- token 不进入用户环境或 CRD；
- `/ping` 通过后才发布 route；
- OpenSandbox command SSE、background logs、file、PTY WebSocket 通过官方 SDK E2E；
- reset/reassignment 后旧 token/route 被拒绝；
- Fastlet 重启可从 runtime inventory 恢复 service route；
- execd 失败时 DataPlaneReady=False，用户 workload 状态单独可见；
- 启动时延按 artifact、runtime、execd readiness、route 分解报告。

完成以上条件后，`opensandbox-execd` 才能从未配置 profile 变为生产可选 InfraProfile。
