# Observability

Fast Sandbox uses Prometheus metrics for bounded SLO signals, OpenTelemetry traces for cross-process causality, and structured logs for lifecycle identity.

## Trace propagation

Processes propagate W3C Trace Context through `traceparent` and `tracestate`. Baggage is not propagated.

Synchronous chains include:

```text
fastctl / Go SDK / Python SDK
  -> Fast-Path gRPC
  -> Fastlet control API

Infra SDK
  -> Sandbox Proxy
  -> Fastlet Proxy
  -> Infra Component
```

Kubernetes watches trigger asynchronous reconciliation. A Reconcile creates a new root span rather than pretending to be a synchronous child of the Create RPC. Lifecycle identity fields correlate the traces.

## Lifecycle identity

| Span attribute | Log key |
|---|---|
| `fast_sandbox.request_id` | `request_id` |
| `fast_sandbox.namespace` | `namespace` |
| `fast_sandbox.sandbox_name` | `sandbox_name` |
| `fast_sandbox.sandbox_uid` | `sandbox_uid` |
| `fast_sandbox.fastlet_pod_uid` | `fastlet_pod_uid` |
| `fast_sandbox.instance_generation` | `instance_generation` |
| `fast_sandbox.assignment_attempt` | `assignment_attempt` |
| `fast_sandbox.route_generation` | `route_generation` |
| `fast_sandbox.target_port` | `target_port` |

These values are high-cardinality fields. They belong in logs and traces, not Prometheus labels.

Prometheus labels are restricted to bounded values such as runtime, profile, result, reason, and state.

## OTLP configuration

An OTLP/gRPC exporter is installed only when an endpoint is configured:

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector.observability.svc:4317
OTEL_SERVICE_NAME=fast-sandbox-fastpath
OTEL_RESOURCE_ATTRIBUTES=deployment.environment=prod,service.namespace=fast-sandbox
```

`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is also supported. Use the standard OTLP gRPC header, TLS, insecure, and timeout environment variables. Set `OTEL_SDK_DISABLED=true` to disable export.

Default service names are:

| Process | `service.name` |
|---|---|
| Fast-Path | `fast-sandbox-fastpath` |
| Controller | `fast-sandbox-controller` |
| All-in-one | `fast-sandbox-all` |
| Fastlet | `fast-sandbox-fastlet` |
| Sandbox Proxy | `fast-sandbox-proxy` |
| Fastlet Proxy | `fast-sandbox-fastlet-proxy` |
| fastctl | `fastctl` |

The process allows up to five seconds to flush spans during shutdown. An unavailable Collector does not change Sandbox request semantics, but invalid exporter configuration fails process startup.

## Metrics

Important metric families cover:

- Create and DataPlaneReady latency;
- Registry candidate count and heartbeat age;
- image affinity and Top-K retries;
- Fastlet admission and active slots;
- runtime, network, Infra, and cache operations;
- both proxy hops and streaming lifetime;
- NodeJanitor cleanup.

`fast_sandbox_warm_image_pull_total{result}` records actual cache pull results without image, Pool, Pod, or Sandbox labels.

See [Performance](performance.md) for the complete latency boundary and benchmark contract.

## Diagnostics

```bash
bin/fastctl --endpoint localhost:9090 \
  diagnostics sandbox <sandbox-name>
```

Diagnostics combine CRD state with a bounded Fastlet lifecycle event ring. They remain available when Execd is absent or unhealthy and do not represent user process stdout.

## Validation

Trace tests must verify:

- W3C HTTP and gRPC propagation;
- correct client/server span kinds;
- lifecycle identity attributes;
- one trace ID across both proxy hops and the final upstream.

Cluster validation should connect a temporary Collector and confirm that Create, asynchronous Reconcile, and Infra proxy traces can be found independently.
