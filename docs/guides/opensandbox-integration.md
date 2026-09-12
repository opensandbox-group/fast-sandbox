# OpenSandbox integration

OpenSandbox can use Fast Sandbox as a runtime backend while keeping its public
SDK and Execd protocol unchanged. Fast Sandbox supplies low-latency lifecycle
operations, Pool placement, runtime creation, Infra Component injection, and a
transparent route to the selected Sandbox service.

The projects retain separate responsibilities:

| Layer | Responsibility |
| --- | --- |
| OpenSandbox | Public Sandbox API, SDK contract, public ingress, secure access, and Execd protocol |
| Fast Sandbox FastPath | Lifecycle intent, low-latency Create, Pool discovery, live observation, and endpoint resolution |
| Fastlet | Admission, runtime, private network, Infra processes, and local readiness |
| Fastlet Proxy | Pod-local named routing and assignment fencing |
| Sandbox Proxy | Optional central Fast Sandbox data-plane entry |

## Deployment prerequisites

Deploy the normal split Fast Sandbox topology:

- multi-active Fast-Path replicas;
- leader-elected Controllers;
- Sandbox Proxy when central routes are required;
- one or more SandboxPools and their Fastlet Pods;
- NodeJanitor on runtime nodes.

The default namespace layout is:

| Namespace | Resources |
| --- | --- |
| `fast-sandbox-system` | Controller, Fast-Path, Sandbox Proxy, NodeJanitor, ServiceAccounts, and RBAC |
| `fast-sandbox` | SandboxPools, Fastlet Pods, Sandbox CRs, Registry ConfigMap, and Registry Secrets |

A Sandbox, its SandboxPool, and the assigned Fastlet Pod must use the same
namespace. Fast Sandbox does not add a separate tenant abstraction. Different
security domains use different Kubernetes namespaces and Pools.

OpenSandbox Server must be able to reach Fast-Path. A trusted direct ingress
must additionally reach Fastlet Proxy on the Fastlet Pod network.

## Pool contract

An OpenSandbox Pool normally provides:

- one fixed runtime and resource profile;
- capacity and `maxSandboxesPerPod`;
- an Infra Component named `execd`;
- optional warm workload images;
- a Fastlet Pod template.

Apply the project sample:

```bash
kubectl apply -f config/samples/pool-container-execd.yaml
```

OpenSandbox can call `GetPool` or `ListPools` before Create to discover:

- runtime and fixed Sandbox resources;
- total, Ready, and idle Fastlets;
- component names, protocols, ports, and health kinds;
- the active Infra revision and prepared Fastlet count;
- aggregated warm-image state.

Pool reads never return Secret content, artifact credentials, or route
credentials.

## Create mapping

OpenSandbox should use one stable Sandbox ID as Fast Sandbox `request_id`.
`CreateSandbox` accepts the complete initial durable intent:

| OpenSandbox intent | FastPath field |
| --- | --- |
| Sandbox ID | `request_id` |
| Resource namespace | `namespace` |
| Image URI | `image` |
| SandboxPool selection | `pool_ref` |
| Entrypoint | `command` and `args` |
| Environment | `envs` |
| Working directory | `working_dir` |
| Absolute expiry | `expires_at_unix_seconds` |
| User metadata | `metadata` |

OpenSandbox does not need to expose Fast Sandbox failure policy. When omitted,
the policy is `MANUAL` and the recovery timeout is 60 seconds.

Expiry and metadata belong in Create. They do not require a follow-up
`UpdateSandbox` call.

The same `request_id` and normalized intent are idempotent. Reusing an ID with
different image, command, expiry, metadata, Pool, or failure settings returns a
conflict. Retry processing never recalculates or extends an absolute expiry.

Create persists the complete Sandbox CRD and assignment before calling the
selected Fastlet. Omitted completion defaults to aggregate `READY`; callers
that only need the started process may explicitly request `RUNTIME_READY`.

## Readiness mapping

Fast Sandbox deliberately separates runtime creation from service readiness:

| Fast Sandbox milestone | OpenSandbox use |
| --- | --- |
| `RuntimeReady` | FastPath Create can return only when explicitly requested |
| `ComponentReady("execd")` | Execd health passed and its local route is published |
| aggregate `Ready` | Runtime, DataPlane, every component, and every Action Binding are Ready |
| CRD status projection | Durable observation for declarative clients and auditing |

OpenSandbox should report the Sandbox as Running only after
aggregate `Ready`. The simplest path is `CreateCompletion=READY`, which waits
inside the existing Fastlet create RPC and does not round-trip through CRD
Status. After an explicit `RUNTIME_READY` create or an update, poll
`GetSandbox` until `ready=true` and `applied_generation` reaches the committed
generation.

`ResolveEndpoint(component_name="execd")` is non-blocking. It performs one
live Fastlet inspection and returns `FailedPrecondition` until aggregate Ready;
it does not provide a separate wait mode.

## Execd

Execd is declared inline on the SandboxPool:

```yaml
infraComponents:
  - name: execd
    artifact:
      source:
        image:
          reference: ghcr.io/opensandbox/execd@sha256:<digest>
      mappings:
        - sourcePath: /execd
          targetPath: /.fast/components/execd/execd
    process:
      command:
        - /.fast/components/execd/execd
        - --port
        - "44772"
      restartPolicy: OnFailure
      healthCheck:
        httpGet:
          path: /ping
        timeoutSeconds: 10
    endpoint:
      protocol: HTTP
      port: 44772
```

Fast Sandbox resolves it by the logical name `execd`; the integration does not
need to use port 44772 as the route identity.

Execd runs without its optional `EXECD_ACCESS_TOKEN`. Fast Sandbox and
OpenSandbox protect their gateway boundaries independently and do not inject
`X-EXECD-ACCESS-TOKEN`.

See [OpenSandbox Execd](opensandbox-execd.md) for the complete execution and
file-operation flow.

## Endpoint modes

FastPath resolves either a named component or a raw user port. A component
reference accepts Sandbox UID or namespace/name.

### Central mode

Central mode is the default:

```text
OpenSandbox ingress or client
-> Sandbox Proxy
-> Fastlet Proxy
-> Sandbox
```

It is appropriate when callers cannot reach Fastlet Pods directly.

### Direct mode

A trusted OpenSandbox ingress can request `DIRECT_FASTLET_PROXY`:

```text
OpenSandbox SDK
-> OpenSandbox ingress
-> Fastlet Proxy
-> component execd
```

FastPath returns a complete upstream route containing:

- Fastlet Proxy base URL and target path;
- protocol and resolved component port;
- short-lived `X-Fast-Sandbox-Route-Credential`;
- route generation and credential expiry.

The OpenSandbox ingress exposes its own stable public URL. It must not expose
the Fastlet Pod IP or Fast Sandbox credential to the SDK.

NetworkPolicy must restrict direct Fastlet Proxy access to trusted platform
ingress identities.

## Ingress route contract

A host-only ingress provider is insufficient for direct mode. The provider
must receive the requested public target and return a complete upstream route:

```text
public Sandbox identity and target
-> scheme + authority + base path
+ upstream-only headers
+ route expiry
+ public access policy
```

The ingress then:

1. validates OpenSandbox public access;
2. resolves or reads a cached Fast Sandbox route;
3. joins the original request suffix and query to the provider base path;
4. removes caller-supplied values for reserved upstream headers;
5. injects the Fast Sandbox route credential only on the upstream hop;
6. forwards HTTP, SSE, WebSocket, and file streams without protocol
   translation.

Application `Authorization` is preserved. OpenSandbox removes its public
access proof, and Fastlet Proxy removes the Fast Sandbox route credential,
before the request reaches Execd.

Route caches are keyed by Sandbox and target and expire before the credential.
Reassignment, route-stale responses, credential expiry, and Fastlet Pod
failure invalidate the entry. A proxy must not blindly replay a
non-idempotent or streaming request after refreshing a route.

## Metadata and lifecycle

OpenSandbox metadata maps to user Kubernetes labels:

- labels under `sandbox.fast.io/` are reserved;
- user metadata remains on CRD labels and is not duplicated into live
  `SandboxInfo` or `SandboxSummary`;
- list metadata filters use AND semantics;
- filtering happens before bounded pagination;
- metadata updates use explicit upsert and delete-key fields.

Lifecycle operations remain declarative:

- expiry updates `spec.expireTime`;
- reset advances `spec.resetRevision`;
- delete starts finalizer-driven cleanup;
- the Reconciler projects placement/recovery, runtime, data-plane, component,
  Action Binding, and aggregate Ready states.

Delete acceptance does not mean runtime cleanup has already completed.

## Registry and warm images

Registry credentials are namespace-scoped and not supplied by OpenSandbox
Create requests or stored on individual Pools. Configure the conventional
`fast-sandbox-registry` ConfigMap and same-namespace
`kubernetes.io/dockerconfigjson` Secrets.

Rules match the exact Registry host and longest repository prefix. Secret
rotation is reconciled without recreating Fastlet Pods for containerd-backed
runtimes.

`warmImages` are asynchronous cache inputs. They do not gate Fastlet Ready.
Pool discovery reports cached, pulling, and failed Fastlet counts so an
integration can distinguish runtime capacity from cache preparation.

See [Private Registries](private-registries.md).

## Unsupported semantics

The Fast Sandbox runtime model does not provide:

- per-Sandbox Kubernetes volumes;
- snapshot, pause, or resume;
- persistent Sandbox storage;
- per-request Registry credentials;
- per-Sandbox node placement;
- live migration or state preservation after Fastlet loss;
- generic interpretation of Infra Component protocols.

OpenSandbox should reject unsupported request fields explicitly instead of
silently dropping them.

## End-to-end validation

A complete integration test should cover:

```text
Create with expiry and metadata
-> default Create returns at aggregate Ready
-> resolve component execd
-> OpenSandbox exec and file operations
-> metadata add, replace, delete, and filter
-> expiry renewal
-> declarative delete and eventual cleanup
```

It should also verify:

- idempotent Create replay and changed-intent conflict;
- namespace and cross-Pool isolation;
- stale assignment, route, and credential rejection;
- route refresh after reassignment or credential expiry;
- preservation of application `Authorization`;
- Registry Secret rotation and failed warm-image retry.
