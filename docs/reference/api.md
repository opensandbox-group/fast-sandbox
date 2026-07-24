# API reference

Fast Sandbox exposes Kubernetes CRDs for declarative lifecycle and a gRPC FastPath service for latency-sensitive clients.

## API group

```text
Group:   sandbox.fast.io
Version: v1alpha1
```

The version is alpha. Consumers should pin a release and treat schema changes according to that release's notes.

## Sandbox

```yaml
apiVersion: sandbox.fast.io/v1alpha1
kind: Sandbox
metadata:
  name: example
spec:
  image: docker.io/library/alpine:latest
  command: ["/bin/sleep"]
  args: ["3600"]
  envs: []
  workingDir: /
  poolRef: default-pool
  failurePolicy: Manual
  recoveryTimeoutSeconds: 60
```

### Sandbox spec

| Field | Type | Required | Meaning |
|---|---|---:|---|
| `image` | string | Yes | OCI image reference |
| `command` | string array | No | Entrypoint override |
| `args` | string array | No | Argument override |
| `envs` | Kubernetes `EnvVar` array | No | User environment |
| `workingDir` | string | No | User working directory |
| `expireTime` | Kubernetes timestamp | No | Declarative expiry |
| `failurePolicy` | `Manual` or `AutoRecreate` | No | Fastlet-loss behavior |
| `recoveryTimeoutSeconds` | integer | No | Delay before automatic recreation |
| `resetRevision` | timestamp | No | Opaque monotonic reset trigger |
| `poolRef` | string | Yes | Target SandboxPool |

### Sandbox status

| Field | Meaning |
|---|---|
| `assignment` | Active Fastlet name, Pod UID, node, and attempt |
| `assignmentAttempt` | Monotonic assignment fence |
| `instanceGeneration` | Reset/recreate fence |
| `routeGeneration` | Data-plane route fence |
| `runtimeState` | Independently observed runtime state |
| `dataPlaneState` | Required Infra and route state |
| `userProcessState` | User-process observation when available |
| `conditions` | Canonical Kubernetes Conditions |
| `acceptedResetRevision` | Last processed reset trigger |

Observed states are `Unknown`, `Pending`, `Creating`, `Ready`, `Draining`, `Stopped`, `Failed`, and `Unavailable`.

Canonical Conditions are `RuntimeReady` and `DataPlaneReady`.

## SandboxPool

```yaml
apiVersion: sandbox.fast.io/v1alpha1
kind: SandboxPool
metadata:
  name: default-pool
spec:
  capacity:
    poolMin: 1
    poolMax: 3
    bufferMin: 1
    bufferMax: 2
  maxSandboxesPerPod: 8
  runtime: container
  sandboxResources:
    cpu: "1"
    memory: 512Mi
    pids: 256
  infraProfile: minimal
  warmImages:
    - docker.io/library/alpine:latest
  fastletTemplate:
    spec:
      containers:
        - name: fastlet
          image: fast-sandbox/fastlet:dev
```

### SandboxPool spec

| Field | Required | Meaning |
|---|---:|---|
| `capacity` | Yes | Pool and idle-buffer limits |
| `maxSandboxesPerPod` | Yes | Fastlet-authoritative Sandbox limit |
| `runtime` | Yes | Immutable canonical runtime name |
| `sandboxResources` | Yes | Immutable CPU, memory, and PID limits per Sandbox |
| `warmImages` | No | Asynchronous protected cache inputs |
| `infraProfile` | No | Immutable Runtime Augmentation profile; default `minimal` |
| `fastletTemplate` | Yes | Kubernetes Pod template with platform-owned fields protected |

Runtime names are `container`, `gvisor`, `kata-qemu`, `kata-clh`, `kata-fc`, and `boxlite`.

Pool Conditions include `RuntimeReady` and `InfraReady`. Unsupported or incomplete capabilities keep the Pool unavailable with a bounded reason.

## FastPath gRPC service

The protobuf contract is `api/proto/v1/fastpath.proto`.

| RPC | Semantics |
|---|---|
| `CreateSandbox` | CRD-first synchronous Create through RuntimeReady |
| `DeleteSandbox` | Submit declarative Kubernetes deletion |
| `UpdateSandbox` | Update expiry, reset, failure policy, recovery timeout, or labels |
| `ListSandboxes` | List CRD and pending Fast-Path views |
| `GetSandbox` | Return one Sandbox view |
| `GetSandboxDiagnostics` | Return lifecycle diagnostics independent of Infra |
| `ResolveEndpoint` | Resolve a short-lived authenticated target-port route |
| `IssueRouteCredential` | Refresh a credential for an existing route |

### Create request

Canonical callers provide:

- `request_id`: required idempotency key and Sandbox name;
- `namespace`;
- `image`;
- `pool_ref`;
- `command` and `args`;
- environment values;
- working directory.

The same request ID must be retried with the same immutable Create values.

### Endpoint resolution

`ResolveEndpoint` accepts Sandbox UID, target port, and protocol hint. It returns:

- client-visible Sandbox Proxy endpoint;
- required request headers;
- route generation;
- credential expiry.

The response does not expose the private Sandbox IP or component token.

## Error semantics

- Local validation and no-candidate failures occur before CRD creation.
- Fastlet capacity rejection maps to a bounded resource-exhaustion result.
- Errors after CRD persistence leave durable intent for retry and reconciliation.
- A Create retry with a changed spec is rejected.
- Delete is successful when declarative deletion has been triggered; cleanup completes asynchronously.
