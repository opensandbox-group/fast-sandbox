# Configuration reference

Fast Sandbox configuration is split between Kubernetes CRDs, deployment flags,
and platform-owned runtime implementation details.

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
| `--runtime-environment-namespace` | `fast-sandbox-system` | Namespace of the platform runtime environment ConfigMap |
| `--runtime-environment-configmap` | `fast-sandbox-runtime-environments` | Platform runtime environment ConfigMap name |

Image and route-key flags can also be supplied by environment:

- `FASTLET_PROXY_IMAGE`;
- `BOXLITE_RUNTIME_IMAGE`;
- `FAST_SANDBOX_ROUTE_VERIFY_PUBLIC_KEY`;
- `FAST_SANDBOX_ROUTE_SIGNING_PRIVATE_KEY`;
- `FAST_SANDBOX_PROXY_BASE_URL`.

Only Fast-Path receives the route-signing private key. Controllers and proxies use verification public keys.

## Artifact store

The S3-compatible golden-image store is platform-owned and deployed as the
`fast-sandbox-artifact-store` ConfigMap in `fast-sandbox-system`
(`config/artifact-store`). The controller and the runtime-agent **mount** it
(one projected file per key) and read it at use time — the controller per
reconcile, the agent per pull — so an edit applies without restarting
anything and no ConfigMap RBAC is involved. Builds receive the resolved
values from the controller when their Pod is created.

| Key | Meaning |
|---|---|
| `store` | Store root, `s3://bucket/prefix`. The controller defaults an empty `SandboxTemplate.spec.output.publish` with it and fails a template that names a different store |
| `endpoint` | Optional S3-compatible endpoint (`scheme://host:port`); empty derives it from the credential |

There is no flag or environment override for these values: the mount is the
single source. Credentials stay separate — builds use the template's
`output.publishSecretRef` (write) and agents use the pool-compiled registry
Secret (read).

## Fastlet environment

The Pool Controller injects platform-owned Fastlet configuration. Important groups are:

### Runtime

- `FAST_SANDBOX_RUNTIME`;
- `FAST_SANDBOX_RUNTIME_PROFILE_HASH`;
- `FAST_SANDBOX_RESOURCE_CPU`;
- `FAST_SANDBOX_RESOURCE_MEMORY`;
- `FAST_SANDBOX_RESOURCE_PIDS`;
- `FAST_SANDBOX_WARM_IMAGES`.
- `FAST_SANDBOX_RUNTIME_PLAN_PATH`.

### Infra

- `FAST_SANDBOX_INFRA_REVISION`;
- `FAST_SANDBOX_INFRA_PLAN_PATH`;
- `FAST_SANDBOX_INFRA_STORE_ROOT`;
- `FAST_SANDBOX_INFRA_HOST_ROOT`;
- `FAST_SANDBOX_SANDBOX_INIT_PATH`;
- `FAST_SANDBOX_SANDBOX_TUNNEL_PATH`.

### Registry

- `FAST_SANDBOX_REGISTRY_CONFIG_PATH`.

The default file is the Pool Controller's read-only compiled Secret projection
at `/etc/fast-sandbox/registry/registry.json`. See
[Private registries](../guides/private-registries.md).

### Network

- `FAST_SANDBOX_NETWORK_CIDR`;
- `FAST_SANDBOX_NETWORK_BRIDGE`;
- `FAST_SANDBOX_NETWORK_EGRESS_DEVICE`;
- `FAST_SANDBOX_NETWORK_STATE_ROOT`;
- `FAST_SANDBOX_NETWORK_NETNS_ROOT`;
- `FAST_SANDBOX_NETWORK_HOST_NETNS_ROOT`;
- `FAST_SANDBOX_NETWORK_MTU`.

Pool templates cannot override these variables or other platform-owned containers, mounts, runtime handlers, and security settings. Fastlet Pods never mount the service-account token (`automountServiceAccountToken: false` is forced), so they have no Kubernetes API access; control-plane components reach Fastlet over the Pod network.

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
| `--runtime-environments-file` | `/etc/fast-sandbox/runtime-environments/runtime-environments.yaml` |

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

- Pool users select stable runtime, resource, inline Infra Component, and
  capacity values.
- Platform operators own backend binaries, handlers, paths, security settings,
  route keys, and Registry credentials.
- Runtime capability probes decide whether the node satisfies the selected profile.

See the [Infra Components reference](infra-components.md) for the Pool-owned
artifact, process, health, and endpoint fields.
