# Fast Sandbox

Fast Sandbox 是一个面向低延迟、强隔离 Sandbox 的 Kubernetes 原生运行时平面。它通过预先保持 Fastlet Pod 就绪，在单个 Fastlet 中创建多个 Sandbox runtime，同时用 Kubernetes CRD 保存声明式生命周期状态。

当前架构把多活请求路径与选主 Reconcile 分开，也把生命周期控制与用户数据协议分开：Fast Sandbox 负责解析、鉴权并透明代理到注入的 Infra Component，组件的官方上游 SDK 负责具体的 exec/file 语义。

英文说明见 [README.md](README.md)，组件与工作流详解见 [ARCHITECTURE_ZH.md](ARCHITECTURE_ZH.md)。

## 部署组件

| 部署单元 | 可用性形态 | 职责 |
|---|---|---|
| `fastctl` / Go / Python SDK | 客户端 | 生命周期调用、平台诊断、Endpoint 解析、上游 SDK hand-off |
| Fast-Path Server | 多活 Deployment | CRD-first 命令式 Create、幂等、Top-K placement、路由凭证 |
| Sandbox/Pool Controller | 选主 Deployment | 声明式 Reconcile、Pool 扩缩容、删除/reset/过期、故障策略 |
| Sandbox Proxy | 多活 Deployment | 带鉴权地透明代理 HTTP/流式请求到目标 Fastlet |
| Fastlet Pod | Pool 管理的 Pod | 原子 admission、runtime 创建、私网、Infra 注入、本地代理 |
| NodeJanitor | 每节点 DaemonSet | 清理孤儿 containerd、网络、Infra 和 BoxLite 资源 |

`controller` 二进制有三种角色：

- `--role=fastpath`：不选主，所有 gRPC 副本都提供服务；
- `--role=controller`：运行 Sandbox/SandboxPool Reconciler，通过 Lease 选主；
- `--role=all`：单进程开发模式。

只有 Create 是同步命令式快路径。Delete、reset、expireTime、failurePolicy 更新都会先修改 Sandbox CRD，再由 Controller Reconcile 完成。即使不部署 Fast-Path，用户直接创建 Sandbox CRD 也能完整工作。

## 核心能力

- **有界的多活 Create**：稳定 `request_id` 与 Kubernetes 持久化提供幂等；Fastlet 对 `maxSandboxesPerPod` 做最终原子 admission，阻止副本间 Registry 偏差造成超卖。
- **Watch + 心跳调度**：每个控制面副本通过 Kubernetes Watch 与低频、带抖动的 Fastlet 心跳建立本地 Registry。Top-K 同时考虑空闲 slot 和镜像缓存亲和；候选过期或冲突后只在有界集合内重试。
- **每 Sandbox 独立私网**：容器类 runtime 使用独立 netns、veth、私有地址和 NAT 出口。每个 Sandbox 都能使用完整私有端口空间，无需全局端口预留。
- **带鉴权的两跳代理**：Sandbox Proxy 解析 `Sandbox UID -> Fastlet Pod`；Fastlet Proxy 再解析 runtime-local AccessHandle。凭证绑定 Fastlet Pod UID、assignment attempt 和 route generation。
- **统一 Runtime Profile**：Pool 只选择一个不可变的 `runtime`：`container`、`gvisor`、`kata-qemu`、`kata-clh`、`kata-fc` 或 `boxlite`。
- **Runtime Augmentation**：平台注入 `sandbox-init`、二进制、配置、内部 token 和 readiness 规则，不要求重建用户 OCI 镜像。Quick Start 固定了真实 OpenSandbox Execd 开发 artifact；独立的生产 profile 在 release 供应链完成前仍保持 fail closed。
- **Pool 固定资源规格**：同一 Pool 内每个 Sandbox 共享不可变的 CPU、内存和 PID 规格，Fastlet/runtime adapter 是最终执行边界。
- **代际 fencing**：CRD UID、instance generation、assignment attempt、Fastlet Pod UID、route generation 一起阻止旧 runtime 或旧路由重新生效。

## 快速开始（kind）

Quick Start 是一条可重复的 kind 验收路径，必须在 Linux 主机运行。需要预先安装 Go、Docker、kind、kubectl 和 make；容器、网络和安全 runtime 不应在 macOS 本地验证，详见 [docs/TESTING.md](docs/TESTING.md)。

### 一键准备 container 环境

```bash
make quickstart
```

该命令等价于 `make quickstart-container`，会准备并保留一套带真实 OpenSandbox Execd 的可交互环境：

- 创建或复用 `fsb-e2e-basic` kind 集群；
- 构建并加载当前源码的 Controller、Fastlet、Proxy 和 Janitor 镜像；
- 部署开发控制面；
- 创建 `quickstart-execd-pool`，等待 Fastlet 完成 Execd artifact 准备并 Ready；
- 构建 `bin/fastctl`；
- 打印可以直接复制的生命周期、diagnostics、exec 和 file 命令。

`make quickstart` 不执行 `go test`、不自动创建 Sandbox，也不在结束时清理 Pool 或 kind 集群。开发清单包含公开测试签名密钥，禁止用于生产。

### 手动体验

环境准备完成后，在一个终端同时暴露两个宿主机入口：

```bash
make quickstart-forward
```

这个前台命令拥有两个 port-forward，按 Ctrl-C 会同时清理：

- Fast-Path gRPC：`localhost:9090`；
- Sandbox Proxy：`http://localhost:18080`。

在另一个终端创建并检查 Sandbox：

```bash
bin/fastctl --endpoint localhost:9090 \
  --proxy-endpoint http://localhost:18080 \
  run quickstart-execd-sandbox \
  --image docker.io/library/alpine:latest \
  --pool quickstart-execd-pool -- /bin/sleep 3600

kubectl wait --for=jsonpath='{.status.dataPlaneState}'=Ready \
  sandbox/quickstart-execd-sandbox --timeout=60s
bin/fastctl --endpoint localhost:9090 get quickstart-execd-sandbox
bin/fastctl --endpoint localhost:9090 \
  diagnostics sandbox quickstart-execd-sandbox
```

通过两层透明代理调用真实 OpenSandbox Execd：

```bash
bin/fastctl --endpoint localhost:9090 \
  --proxy-endpoint http://localhost:18080 \
  opensandbox exec quickstart-execd-sandbox -- \
  sh -lc 'printf "hello from execd\n" > /tmp/execd.txt && cat /tmp/execd.txt'

printf 'hello from host\n' > /tmp/fast-sandbox-quickstart.txt
bin/fastctl --endpoint localhost:9090 \
  --proxy-endpoint http://localhost:18080 \
  opensandbox cp /tmp/fast-sandbox-quickstart.txt \
  quickstart-execd-sandbox:/tmp/from-host.txt
bin/fastctl --endpoint localhost:9090 \
  --proxy-endpoint http://localhost:18080 \
  opensandbox files read quickstart-execd-sandbox /tmp/from-host.txt
bin/fastctl --endpoint localhost:9090 \
  --proxy-endpoint http://localhost:18080 \
  opensandbox cp quickstart-execd-sandbox:/tmp/execd.txt \
  /tmp/execd-downloaded.txt

bin/fastctl --endpoint localhost:9090 delete quickstart-execd-sandbox
```

Fast-Path 返回的 Sandbox Proxy 地址可能是 `fast-sandbox-proxy.default.svc` 这样的集群内 DNS。宿主机上的 `fastctl` 无法解析它，因此 Quick Start 的数据面命令必须传 `--proxy-endpoint http://localhost:18080`；仅访问生命周期 API 的命令只需要 Fast-Path endpoint。

Fast-Path Create 成功时 runtime 已经创建完成，但 CRD `status` 由 Controller 异步投影，因此紧接着执行 Get 可能短暂看到 `Creating/Pending`；上面的 `kubectl wait` 用于等待声明式视图追平。

### 准备其他 runtime

以下入口复用同一套 kind profile provisioner，但不会执行 E2E case：

```bash
make quickstart-container
make quickstart-minimal
make quickstart-gvisor
make quickstart-kata-qemu
make quickstart-kata-clh
```

- `container` 准备 `fsb-e2e-basic` 和带真实 Execd 的 `quickstart-execd-pool`。
- `minimal` 准备不含 Execd、只验证生命周期的 `quickstart-pool`。
- `gVisor` 准备 `fsb-e2e-gvisor`、安装并校验 runsc，并创建 `gvisor-execd-pool`。
- `kata-qemu` 和 `kata-clh` 准备 `fsb-e2e-kata`，要求宿主机支持嵌套 KVM，分别创建 `kata-qemu-execd-pool` 和 `kata-clh-execd-pool`。

Container、gVisor、Kata QEMU 和 Kata CLH Quick Start 现在都会注入固定版本的 OpenSandbox Execd，并打印相同的 `exec`、上传、读取和下载示例。Kata 通过 runtime 的 OCI shared bind-mount 路径把只读 Infra bundle 带入 guest。

Kata Firecracker 和 BoxLite 当前尚未达到可运行能力，因此没有 Quick Start 入口；对应的 fail-closed 行为仍由 `test-e2e-runtime-kata` 和 `test-e2e-runtime-boxlite` 验证。所有 Quick Start 目标都会保留可复用的 kind 集群和 Pool，首次运行需要构建镜像和准备 runtime，耗时会明显高于后续运行。

自动化验收与 Quick Start 分离：

```bash
make test-e2e-runtime-container
make test-e2e-runtime-gvisor
make test-e2e-runtime-kata
make test-e2e-runtime-boxlite
```

### 声明式 API

Controller-only 路径不依赖 Fast-Path Create：

```yaml
apiVersion: sandbox.fast.io/v1alpha1
kind: Sandbox
metadata:
  name: my-declarative-sandbox
spec:
  image: docker.io/library/alpine:latest
  poolRef: quickstart-execd-pool
  command: ["/bin/sleep"]
  args: ["3600"]
  failurePolicy: Manual
```

```bash
kubectl apply -f sandbox.yaml
kubectl get sandbox my-declarative-sandbox -w
```

### OpenSandbox Execd

Quick Start 使用 `infraProfile: opensandbox-execd-quickstart`：固定 Execd v1.0.21 amd64 artifact，在 Fastlet 中校验文件 digest，通过 `sandbox-init` 注入，并用官方 OpenSandbox SDK 验证 command/file 调用。这个开发 profile 与继续 fail-closed 的生产 `opensandbox-execd` profile 明确分开，详见 [OpenSandbox Execd 接入指南](docs/opensandbox-execd-integration-guide.md)。

Fast Sandbox 不定义新的 Exec/File wire protocol；用户进程执行、日志和文件属于注入组件协议。

## API 契约

FastPath gRPC 暴露：

- `CreateSandbox`、`DeleteSandbox`、`UpdateSandbox`、`ListSandboxes`、`GetSandbox`、`GetSandboxDiagnostics`；
- `ResolveEndpoint`、`IssueRouteCredential`。

Create 调用必须发送稳定的 `request_id`，它同时是 canonical Sandbox CRD 名。`fastctl diagnostics sandbox NAME` 独立于 Execd 返回 CRD 状态和有界 Fastlet 生命周期事件。API 只接受本文列出的 canonical contract；重构前的字段名不属于当前 schema。Metrics、trace 传播、生命周期身份字段和 OTLP 配置见 [docs/observability.md](docs/observability.md)。

## 验证

```bash
make verify
make test-race
make test-python-sdk
```

Linux/Kubernetes 专项 gate 包括 `test-network-integration`、`test-e2e-controlplane`、`test-e2e-proxy`、`test-e2e-infra`、`test-e2e-sdk`、`test-e2e-quickstart` 和 `make help` 中列出的各 runtime capability target。

## 当前边界

- Sandbox 与 Fastlet Pod 绑定；Fastlet Pod 消失后当前实例失效，`AutoRecreate` 可按策略生成新实例。
- snapshot、pause/resume 和持久化 Sandbox storage 不在本轮重构范围。
- BoxLite 已接入生命周期、Infra 注入、带鉴权的 local forward 和清理；但 BoxLite v0.9.7 没有不可逃逸的 host 资源执行契约，因此 BoxLite Pool 当前会关闭资源能力 gate，而不是宣称生产支持。
- `<50ms` 仅是 warm container profile 的观测目标，不是 cold image、Kata、BoxLite、Infra Ready 或完整 DataPlaneReady 的统一承诺。

## 设计文档

- [跨模块架构决策](docs/superpowers/specs/2026-07-19-fast-sandbox-cross-cutting-architecture-decisions.md)
- [多活 Fast-Path 控制面](docs/superpowers/specs/2026-07-18-multi-active-fastpath-control-plane-design.md)
- [Fastlet 网络架构](docs/superpowers/specs/2026-05-05-fastlet-network-architecture-design.md)
- [控制面/数据面分离与 Infra 注入](docs/superpowers/specs/2026-07-19-control-data-plane-separation-design.md)
- [Runtime 抽象](docs/superpowers/specs/2026-07-19-sandbox-runtime-abstraction-design.md)
- [实施计划与验证记录](docs/superpowers/plans/2026-07-19-fast-sandbox-architecture-refactor-implementation-plan.md)

## License

[MIT](LICENSE)
