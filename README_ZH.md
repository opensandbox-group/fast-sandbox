# Fast Sandbox

在 Kubernetes 中基于预热 runtime pool，快速创建相互隔离的 container、gVisor 和 Kata Sandbox。

[English](README.md) · [Quick Start](docs/getting-started/quickstart.md) · [完整文档](docs/README.md) · [架构原理](docs/concepts/architecture.md)

![Fast Sandbox 系统总览](docs/assets/system-overview.svg)

Fast Sandbox 面向需要大量、短生命周期隔离执行环境的场景。一个预热的 Fastlet Pod 可以承载多个独立 Sandbox runtime，Kubernetes CRD 保存生命周期意图和状态。

命令式 Create 路径优化创建延迟；删除、reset、过期、恢复和 Pool 管理继续采用声明式 Reconcile。Exec/File 等数据面协议由 OpenSandbox Execd 之类的 Infra Component 提供。

## 核心能力

- **预热 runtime pool**：复用已就绪的 Fastlet Pod，不要求每个 Sandbox 都创建一个 Kubernetes Pod。
- **多种隔离 profile**：Pool 通过单一不可变字段选择 container、gVisor、Kata QEMU 或 Kata Cloud Hypervisor。
- **Sandbox 独立私网**：每个实例拥有独立地址空间和 NAT 出口，不做全局 host port 分配。
- **多活 Create**：Kubernetes 持久化提供幂等，Fastlet 原子 admission 最终执行容量限制。
- **协议无关的数据面**：注入、发现、鉴权并透明代理 Infra Component 的原生服务。
- **Kubernetes 原生生命周期**：不部署 Fast-Path 时，Controller 仍能根据 CRD 创建和管理 Sandbox。

## Quick Start

Quick Start 在 Linux 主机上准备可交互的 kind 环境。它不会运行 E2E suite，也不会自动创建 Sandbox。

```bash
make quickstart
```

终端 1：

```bash
make quickstart-forward
```

终端 2：

```bash
bin/fastctl --endpoint localhost:9090 \
  --proxy-endpoint http://localhost:18080 \
  run quickstart-execd-sandbox \
  --image docker.io/library/alpine:latest \
  --pool quickstart-execd-pool -- /bin/sleep 3600

kubectl wait --for=jsonpath='{.status.dataPlaneState}'=Ready \
  sandbox/quickstart-execd-sandbox --timeout=60s

bin/fastctl --endpoint localhost:9090 \
  --proxy-endpoint http://localhost:18080 \
  opensandbox exec quickstart-execd-sandbox -- uname -a

bin/fastctl --endpoint localhost:9090 delete quickstart-execd-sandbox
```

选择其他 runtime：

```bash
make quickstart RUNTIME=gvisor
make quickstart RUNTIME=kata-qemu
make quickstart RUNTIME=kata-clh
```

文件传输、diagnostics、声明式创建和排障说明见[完整 Quick Start](docs/getting-started/quickstart.md)。

## 架构

控制面明确分成两个角色：

- **Fast-Path Server**：多活，负责幂等的 CRD-first Create。
- **Reconciler**：选主，负责 Sandbox 和 SandboxPool 声明式收敛。

Fastlet 是节点侧 runtime 边界：

```text
Fastlet
  -> RuntimeDriver
       -> containerd + runc
       -> containerd + gVisor/runsc
       -> Kata QEMU
       -> Kata Cloud Hypervisor
       -> Kata Firecracker [能力门禁]
       -> BoxLite sidecar [实验性，fail closed]
```

用户数据通过独立链路：

```text
上游 SDK
  -> Sandbox Proxy
  -> Fastlet Proxy
  -> Sandbox 私网
  -> Infra Component
```

| 部署单元 | 可用性 | 职责 |
|---|---|---|
| Fast-Path Server | 多活 Deployment | Create、Local Registry、Top-K、路由凭证 |
| Sandbox/Pool Reconciler | 选主 Deployment | 声明式生命周期、Pool 扩缩容和 drain、恢复 |
| Sandbox Proxy | 多活 Deployment | 带鉴权的 HTTP/流式透明代理 |
| Fastlet Pod | Pool 管理的 Pod | 原子 admission、runtime/network/Infra、本地 Proxy |
| NodeJanitor | 每节点 DaemonSet | 带 fencing 的孤儿资源清理 |

完整原理见 [Architecture](docs/concepts/architecture.md) 和 [Control plane](docs/concepts/control-plane.md)。

## Runtime 状态

| Runtime | Pool 值 | Quick Start | Fast Sandbox 状态 |
|---|---|---:|---|
| OCI container | `container` | 支持 | 已验证 |
| gVisor | `gvisor` | 支持 | 已验证 |
| Kata QEMU | `kata-qemu` | 支持 | 已验证 |
| Kata Cloud Hypervisor | `kata-clh` | 支持 | 已验证 |
| Kata Firecracker | `kata-fc` | 不支持 | 保持 capability gate |
| BoxLite | `boxlite` | 不支持 | 实验性接入，fail closed |

这个表描述 Fast Sandbox 的验收状态，不代表上游 runtime 的通用能力。

## 性能语义

Create 在 `RuntimeReady` 时返回。Infra 服务启动和路由发布异步推进到 `DataPlaneReady`。

没有 commit、环境、runtime、缓存状态、并发、测量边界和分位数分布时，项目不发布单一延迟数字。详见 [Performance](docs/guides/performance.md)。

## 当前范围

- Sandbox 实例与 Fastlet Pod 绑定。Pod 消失后实例失效，`AutoRecreate` 可以创建新 generation。
- Snapshot、pause/resume、持久化存储和 live migration 不是当前能力。
- Kata Firecracker 和 BoxLite 继续保持显式 capability gate。
- 开发 Execd profile 使用公开测试材料，生产部署必须绑定可信 artifact 供应链。

## 文档

除本文件外，项目文档统一使用英文：

- [文档索引](docs/README.md)
- [Architecture](docs/concepts/architecture.md)
- [Runtime model](docs/concepts/runtimes.md)
- [Private networking](docs/concepts/networking.md)
- [Infra Components](docs/concepts/infra-components.md)
- [Deployment](docs/guides/deployment.md)
- [Testing](docs/guides/testing.md)
- [API reference](docs/reference/api.md)

## License

[MIT](LICENSE)
