# Configuration reference

Fast Sandbox configuration is split between Kubernetes CRDs, deployment flags, and platform-owned Runtime and Infra catalogs.

## Controller roles

| Flag | Default | Meaning |
|---|---|---|
| `--role` | `all` | `fastpath`, `controller`, or `all` |
| `--metrics-bind-address` | `:9091` | Prometheus endpoint |
| `--health-probe-bind-address` | `:5758` | Health endpoint |
| `--fastpath-bind-address` | `:9090` | FastPath gRPC listener |
| `--fastlet-port` | `5758` | Fastlet control port |
| `--fastlet-heartbeat-interval` | `20s` | Jittered heartbeat base interval |
| `--fastlet-heartbeat-timeout` | `5s` | One heartbeat timeout |
| `--fastlet-heartbeat-concurrency` | `8` | Heartbeat concurrency limit |
| `--fastlet-drain-timeout` | `5m` | Drain deadline before failure policy |
| `--route-credential-ttl` | `5m` | Caller route credential lifetime |
| `--sandbox-proxy-base-url` | cluster Service URL | Client-visible proxy base URL |

Image and route-key flags can also be supplied by environment:

- `FASTLET_PROXY_IMAGE`;
- `BOXLITE_RUNTIME_IMAGE`;
- `FAST_SANDBOX_ROUTE_VERIFY_PUBLIC_KEY`;
- `FAST_SANDBOX_ROUTE_SIGNING_PRIVATE_KEY`;
- `FAST_SANDBOX_PROXY_BASE_URL`.

Only Fast-Path receives the route-signing private key. Controllers and proxies use verification public keys.

## Fastlet environment

The Pool Controller injects platform-owned Fastlet configuration. Important groups are:

### Runtime

- `FAST_SANDBOX_RUNTIME`;
- `FAST_SANDBOX_RUNTIME_PROFILE_HASH`;
- `FAST_SANDBOX_RESOURCE_CPU`;
- `FAST_SANDBOX_RESOURCE_MEMORY`;
- `FAST_SANDBOX_RESOURCE_PIDS`;
- `FAST_SANDBOX_WARM_IMAGES`.

### Infra

- `FAST_SANDBOX_INFRA_PROFILE`;
- `FAST_SANDBOX_INFRA_PROFILE_HASH`;
- `FAST_SANDBOX_INFRA_STORE_ROOT`;
- `FAST_SANDBOX_INFRA_HOST_ROOT`;
- `FAST_SANDBOX_INFRA_STATIC_ROOTS`;
- `FAST_SANDBOX_SANDBOX_INIT_PATH`;
- `FAST_SANDBOX_SANDBOX_TUNNEL_PATH`.

### Network

- `FAST_SANDBOX_NETWORK_CIDR`;
- `FAST_SANDBOX_NETWORK_BRIDGE`;
- `FAST_SANDBOX_NETWORK_EGRESS_DEVICE`;
- `FAST_SANDBOX_NETWORK_STATE_ROOT`;
- `FAST_SANDBOX_NETWORK_NETNS_ROOT`;
- `FAST_SANDBOX_NETWORK_HOST_NETNS_ROOT`;
- `FAST_SANDBOX_NETWORK_MTU`.

Pool templates cannot override these variables or other platform-owned containers, mounts, runtime handlers, and security settings.

## Proxy configuration

Sandbox Proxy flags:

- `--bind-address`;
- `--metrics-bind-address`;
- `--route-verify-public-key`;
- `--fastlet-proxy-port`.

Fastlet Proxy environment:

- `FAST_SANDBOX_ROUTE_VERIFY_PUBLIC_KEY`;
- `FASTLET_PROXY_CONTROL_SOCKET`;
- `FASTLET_PROXY_DATA_ADDRESS`;
- `FASTLET_PROXY_METRICS_ADDRESS`.

Data and metrics listeners are separate.

## NodeJanitor

| Flag | Default |
|---|---|
| `--node-name` | `NODE_NAME` |
| `--containerd-socket` | `/run/containerd/containerd.sock` |
| `--orphan-timeout` | `30s` |
| `--scan-interval` | `2m` |
| `--network-state-root` | `/run/fast-sandbox/network` |
| `--boxlite-state-root` | `/var/lib/fast-sandbox/boxlite` |
| `--metrics-address` | `:9092` |

## OpenTelemetry

Standard OTLP/gRPC environment variables configure tracing:

- `OTEL_EXPORTER_OTLP_ENDPOINT`;
- `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`;
- `OTEL_SERVICE_NAME`;
- `OTEL_RESOURCE_ATTRIBUTES`;
- `OTEL_SDK_DISABLED`.

See [Observability](../guides/observability.md).

## Ownership

Configuration follows this rule:

- Pool users select stable runtime, resource, Infra, and capacity values.
- Platform operators own backend binaries, handlers, paths, security settings, credentials, and artifact bindings.
- Runtime capability probes decide whether the node satisfies the selected profile.
