# Fast Sandbox 可观测性

Fast Sandbox 把可观测性分为三层：Prometheus metrics 用于 SLO 和告警，OpenTelemetry trace 用于跨进程因果链，结构化日志用于按生命周期身份排障。三者共享语义，但不会共享同一套字段基数。

## Trace 边界

进程使用 W3C Trace Context（`traceparent`/`tracestate`）传播上下文。目前不传播 baggage，避免把用户或高基数身份未经约束地带过控制面与数据面边界。

同步调用链包括：

```text
fastctl / Go SDK / Python SDK
  -> Fast-Path gRPC
  -> Fastlet HTTP control API

Execd-compatible SDK
  -> Sandbox Proxy
  -> Fastlet Proxy
  -> injected Infra Component
```

Fast-Path、Fastlet、Sandbox Proxy 和 Fastlet Proxy 都能导出 span。Go/Python SDK 在应用已经安装 OpenTelemetry provider 时传播当前上下文；Python SDK 的集成通过可选的 `telemetry` extra 启用。

Controller Reconcile 由 Kubernetes Watch 异步触发，因此创建新的 root span。它不会伪造与 Create RPC 的同步 parent/child 关系；排障时通过 `request_id`、Sandbox UID 和 generation 字段关联两条 trace。

## 生命周期身份

以下字段写入 span attribute，并同步加入当前 `klog` context：

| Span attribute | Log key | 含义 |
|---|---|---|
| `fast_sandbox.request_id` | `request_id` | Create 幂等身份 |
| `fast_sandbox.namespace` | `namespace` | Sandbox namespace |
| `fast_sandbox.sandbox_name` | `sandbox_name` | Sandbox CRD 名称 |
| `fast_sandbox.sandbox_uid` | `sandbox_uid` | CRD UID / runtime claim identity |
| `fast_sandbox.fastlet_pod_uid` | `fastlet_pod_uid` | 当前 Fastlet Pod incarnation |
| `fast_sandbox.instance_generation` | `instance_generation` | Sandbox runtime 代际 |
| `fast_sandbox.assignment_attempt` | `assignment_attempt` | 调度 assignment fencing |
| `fast_sandbox.route_generation` | `route_generation` | 数据面路由 fencing |
| `fast_sandbox.target_port` | `target_port` | 注入组件目标端口 |

这些字段都是高基数或潜在高基数字段，只能进入 trace/log，禁止增加为 Prometheus label。Prometheus 只保留 `runtime`、`profile`、`result`、`reason`、`state` 等代码内有界分类；完整指标清单和延迟边界见 [PERFORMANCE.md](PERFORMANCE.md)。

## OTLP 配置

二进制仅在显式设置 OTLP endpoint 时安装 OTLP/gRPC exporter；未配置时 provider 保持 no-op，但上下文仍会透明传播。常用环境变量：

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector.observability.svc:4317
OTEL_SERVICE_NAME=fast-sandbox-fastpath
OTEL_RESOURCE_ATTRIBUTES=deployment.environment=prod,service.namespace=fast-sandbox
```

也可使用 traces 专用的 `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`。exporter 接受标准的 OTLP gRPC headers、TLS/insecure 和 timeout 环境变量。当前二进制固定使用 OTLP/gRPC，`OTEL_EXPORTER_OTLP_PROTOCOL` 应省略或设为 `grpc`。`OTEL_SDK_DISABLED=true` 强制禁用导出。

默认 service name：

| 进程 | 默认 `service.name` |
|---|---|
| Fast-Path / Controller role | `fast-sandbox-fastpath` / `fast-sandbox-controller` / `fast-sandbox-all` |
| Fastlet | `fast-sandbox-fastlet` |
| Sandbox Proxy | `fast-sandbox-proxy` |
| Fastlet Proxy | `fast-sandbox-fastlet-proxy` |
| fastctl | `fastctl` |

生产环境建议由 Collector 做 tail sampling 和敏感字段治理。开启 exporter 后，进程退出会用最多 5 秒刷新 batch span；Collector 不可用不能改变 Sandbox 请求语义，但初始 exporter 配置错误会使进程 fail closed，避免部署看似启用了 tracing 实际完全丢数。

## 验证

单元测试覆盖 W3C HTTP/gRPC 上下文、server/client span kind、生命周期身份 attribute，以及两跳 proxy 到最终 Infra upstream 的同一 trace ID。远端 Linux gate：

```bash
go test -race -p=1 \
  ./internal/observability ./internal/api ./internal/controller/fastpath \
  ./internal/controller ./internal/fastlet/server ./internal/fastlet/runtime \
  ./internal/sandboxproxy ./internal/fastletproxy ./pkg/sandboxclient -count=1

make test-unit
make test-python-sdk
```

集群验收时还应连接临时 Collector，验证 Create、声明式 Reconcile 和 Execd 代理三条 trace 可以分别检索，并确认未配置 endpoint 的部署不会发起 exporter 连接。
