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
| `--resource-aware-static-admission` | `true` | Enable Fastlet CPU and memory commitment admission for ResourceAware Pools |
| `--resource-aware-memory-admission` | `true` | Enable the Fastlet dynamic memory-pressure gate |
| `--resource-aware-placement` | `true` | Enable FastPath resource eligibility and ranking |
| `--resource-aware-pool-scaling` | `true` | Enable ResourceAware effective-capacity Pool scaling |
| `--resource-aware-fastlet-startup-grace` | `2m` | Time a new Fastlet without a heartbeat contributes nominal capacity |
| `--resource-aware-scale-down-stabilization` | `1m` | Continuous excess-capacity period required before scale-down |

Resource-aware placement requires static admission. Resource-aware Pool
scaling requires placement. These constraints prevent a downstream component
from acting on capacity that an upstream authority does not enforce.

Image and route-key flags can also be supplied by environment:

- `FASTLET_PROXY_IMAGE`;
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

## Fastlet configuration

Fastlet configuration is delivered by level:

- **Pool-declared declarative settings** (warmImages, actionHandlers,
  sandboxResources) travel in an immutable, content-addressed
  `<pool>-settings-<revision>` ConfigMap mounted at
  `/etc/fast-sandbox/settings` as `warm-images.json`,
  `action-handlers.json`, `resource-profile.json`, and, for ResourceAware
  Pools, `resource-admission.json`. The ConfigMap name is
  embedded in the Pod template hash, so any settings change rolls Fastlet
  Pods. The `FAST_SANDBOX_SETTINGS_DIR` environment variable points the
  Fastlet at the mount; when the mount is absent (local runs without the
  control plane) the Fastlet falls back to the legacy
  `FAST_SANDBOX_WARM_IMAGES`, `FAST_SANDBOX_ACTION_HANDLERS`, and
  `FAST_SANDBOX_RESOURCE_*` environment variables.
- **Identity and coordination values** stay environment variables: pod
  identity via the Downward API (`POD_NAME`, `POD_UID`, `POD_IP`,
  `NODE_NAME`, `NAMESPACE`), pod resources (`CPU_LIMIT`, `MEMORY_LIMIT`),
  and simple scalars/paths (`FASTLET_CAPACITY`, `FAST_SANDBOX_RUNTIME`,
  `FAST_SANDBOX_RUNTIME_PROFILE_HASH`, `FAST_SANDBOX_INFRA_REVISION`, and
  the `*_PATH` pointers to the mounted plans below).
- **Resolved runtime and infra plans** stay in their own revisioned
  ConfigMaps (`runtime-plan`, `infra-plan`); the Fastlet reads the full
  containerd socket, snapshotter, and kubelet root from the runtime plan
  file rather than duplicated environment variables.
- **Registry credentials** stay in the compiled Secret.

The Pool Controller injects the remaining platform-owned environment groups:

For ResourceAware Pools, `resource-admission.json` is the single resolved
contract shared by Fastlet admission and the runtime boundary. Its `capacity`
is the pure Sandbox commitment capacity; `sandboxRuntimeBudget` is the maximum
aggregate child-runtime budget and cannot be consumed by fixed Fastlet cost;
`runtimeEnvelope` adds the runtime profile's one-per-Pod `fastletOverhead`;
`podMemoryBudget` adds all long-running regular sidecar and restartable
init-sidecar memory limits for runtimes whose pressure observer reads the
aggregate Pod cgroup. One-shot init containers are excluded. This
observer-only budget never changes Sandbox commitment capacity.
The Controller passes the compiled capacity directly to the Pool scaler and
projects that same value through a platform-owned Pod annotation for Registry
bootstrap, FastPath placement, and status. No consumer re-derives capacity
from the first container's Kubernetes request.

Per-Sandbox limits are compiled through the same resolver for FastPath,
declarative reconciliation, and Pool scaling. A direct CRD whose limits exceed
one Fastlet's `sandboxRuntimeBudget` remains visible with
`ResourceResolved=False`, but is excluded from admissible pending demand so it
cannot suppress scale-out for valid Sandboxes or the configured buffer.

Each runtime binding in `fast-sandbox-runtime-environments` may configure
`fastletOverhead.cpu` and `.memory`. The Controller adds it exactly once when
deriving the first Fastlet container's equal Kubernetes requests and limits.
Node-level runtime-agent, OverlayBD, and janitor DaemonSet resources are
separate workload budgets and never enter a SandboxPool commitment ledger.

### Runtime

- `FAST_SANDBOX_RUNTIME`;
- `FAST_SANDBOX_RUNTIME_PROFILE_HASH`;
- `FAST_SANDBOX_SETTINGS_DIR`;
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
