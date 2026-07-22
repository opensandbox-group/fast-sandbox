# Fast Sandbox

Fast Sandbox 是一个面向低延迟、强隔离 Sandbox 的 Kubernetes 原生运行时平面。它通过预先保持 Fastlet Pod 就绪，在单个 Fastlet 中创建多个 Sandbox runtime，同时用 Kubernetes CRD 保存声明式生命周期状态。

当前架构把多活请求路径与选主 Reconcile 分开，也把生命周期控制与用户数据协议分开：Fast Sandbox 负责解析、鉴权并透明代理到注入的 Infra Component，Execd/Envd 兼容 SDK 负责具体的 exec/file 语义。

英文说明见 [README.md](README.md)，组件与工作流详解见 [ARCHITECTURE_ZH.md](ARCHITECTURE_ZH.md)。

## 部署组件

| 部署单元 | 可用性形态 | 职责 |
|---|---|---|
| `fastctl` / Go / Python SDK | 客户端 | 生命周期调用、Endpoint 解析、Infra 协议 Adapter |
| Fast-Path Server | 多活 Deployment | 命令式 Create、幂等、Top-K reservation、CRD 提交、路由凭证 |
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
- **Runtime Augmentation**：平台注入 `sandbox-init`、二进制、配置、内部 token 和 readiness 规则，不要求重建用户 OCI 镜像。内置适配方向包括 OpenSandbox Execd 与 E2B Envd。
- **Pool 固定资源规格**：同一 Pool 内每个 Sandbox 共享不可变的 CPU、内存和 PID 规格，Fastlet/runtime adapter 是最终执行边界。
- **代际 fencing**：CRD UID、instance generation、assignment attempt、Fastlet Pod UID、route generation 一起阻止旧 runtime 或旧路由重新生效。

## 快速开始

### 构建

```bash
make build
export PATH="$PWD/bin:$PATH"
```

容器、网络、Kubernetes 与安全 runtime 行为必须在 Linux 开发机验证，详见 [docs/TESTING.md](docs/TESTING.md)。

### 安装开发清单

开发 overlay 包含公开测试签名密钥，禁止用于生产：

```bash
kubectl apply -k config/dev
```

生产环境应先从密钥管理系统创建 `fast-sandbox-route-keys`，再应用 `config/default`。私钥只进入 Fast-Path；Controller、Sandbox Proxy 与 Fastlet Proxy 只获得公钥。

### 创建 Pool

```yaml
apiVersion: sandbox.fast.io/v1alpha1
kind: SandboxPool
metadata:
  name: default-pool
spec:
  capacity:
    poolMin: 1
    poolMax: 10
    bufferMin: 1
    bufferMax: 2
  maxSandboxesPerPod: 5
  runtime: container
  sandboxResources:
    cpu: "1"
    memory: 512Mi
    pids: 256
  warmImages:
  - docker.io/library/alpine:latest
  infraProfile: minimal
  fastletTemplate:
    spec:
      containers:
      - name: fastlet
        image: fast-sandbox/fastlet:dev
```

container、gVisor、Kata 示例见 [config/samples](config/samples)。

### 创建 Sandbox

使用 Fast-Path：

```bash
fastctl --endpoint fast-sandbox-fastpath.default.svc:9090 \
  run my-sandbox --image docker.io/library/alpine:latest --pool default-pool
```

使用声明式 API：

```yaml
apiVersion: sandbox.fast.io/v1alpha1
kind: Sandbox
metadata:
  name: my-sandbox
spec:
  image: docker.io/library/alpine:latest
  poolRef: default-pool
  command: ["/bin/sleep"]
  args: ["3600"]
  failurePolicy: Manual
```

```bash
kubectl apply -f sandbox.yaml
```

### 访问注入服务

SDK 先通过 Fast-Path 解析 `(Sandbox UID, target port)`，再携带短期 bearer credential 访问 Sandbox Proxy。Fast Sandbox 透明转发 HTTP、SSE 和 WebSocket；具体 path 与 payload 由选择的 Infra Adapter 定义。

```bash
fastctl --proxy-endpoint http://fast-sandbox-proxy.default.svc:8080 \
  --adapter execd exec my-sandbox -- /bin/sh -lc 'echo hello'
```

Fast Sandbox 不定义新的 Exec/File wire protocol；用户进程执行、日志和文件都属于注入组件协议。

## API 契约

FastPath gRPC 暴露：

- `CreateSandbox`、`DeleteSandbox`、`UpdateSandbox`、`ListSandboxes`、`GetSandbox`；
- `ResolveEndpoint`、`IssueRouteCredential`。

Create 调用必须发送稳定的 `request_id`。API 只接受本文列出的 canonical contract；重构前的字段名不属于当前 schema。Metrics、trace 传播、生命周期身份字段和 OTLP 配置见 [docs/observability.md](docs/observability.md)。

## 验证

```bash
make verify
make test-race
make test-python-sdk
```

Linux/Kubernetes 专项 gate 包括 `test-network-integration`、`test-e2e-controlplane`、`test-e2e-proxy`、`test-e2e-infra`、`test-e2e-sdk` 和 `make help` 中列出的各 runtime capability target。

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
