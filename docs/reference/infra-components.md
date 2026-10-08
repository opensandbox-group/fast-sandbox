# Infra Components reference

Infra Components are immutable, Pool-defined processes injected into every
Sandbox created from that Pool. Each component consists of:

```text
name
+ verified artifact and file mappings
+ supervised process
+ health check
+ one named HTTP endpoint
```

The component runs inside the user Sandbox. An OCI image used as an artifact
source is only a file carrier; Fast Sandbox does not start it as another
container.

## Complete example

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxPool
metadata:
  name: opensandbox-pool
  namespace: fast-sandbox
spec:
  runtime: container
  sandboxResources:
    cpu: "1"
    memory: 512Mi
    pids: 256
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
        env:
          LOG_LEVEL: info
        restartPolicy: OnFailure
        healthCheck:
          httpGet:
            path: /ping
          timeoutSeconds: 10
      endpoint:
        protocol: HTTP
        port: 44772
  capacity:
    poolMin: 1
    poolMax: 3
    bufferMin: 1
    bufferMax: 2
  maxSandboxesPerPod: 8
  fastletTemplate:
    spec:
      containers:
        - name: fastlet
          image: fast-sandbox/fastlet:dev
```

## Field reference

| Field | Required | Default | Meaning |
| --- | ---: | --- | --- |
| `infraComponents[].name` | Yes | — | Unique DNS label and immutable routing key |
| `artifact.source.image.reference` | One source | — | OCI image or index pinned by `@sha256:` |
| `artifact.source.archive.url` | One source | — | Absolute HTTPS URL for a gzip-compressed tar archive |
| `artifact.source.archive.sha256` | With archive | — | Lowercase SHA-256 of the complete archive |
| `artifact.mappings[]` | Yes | — | One to 64 file or directory mappings |
| `artifact.mappings[].sourcePath` | Yes | — | Clean absolute path under the verified artifact root |
| `artifact.mappings[].targetPath` | Yes | — | Final path under `/.fast/components/<name>/` |
| `process.command[]` | Yes | — | Non-empty argv executed without a shell |
| `process.env` | No | Empty | Static Pool-owned environment values |
| `process.restartPolicy` | No | `OnFailure` | `Never`, `OnFailure`, or `Always` |
| `process.healthCheck.httpGet.path` | One probe | — | Absolute HTTP path on the component endpoint |
| `process.healthCheck.tcpConnect` | One probe | — | Empty object selecting a TCP connect probe |
| `process.healthCheck.timeoutSeconds` | No | `10` | Startup or transition timeout from 1 to 300 seconds |
| `endpoint.protocol` | Yes | — | `HTTP` |
| `endpoint.port` | Yes | — | Unique port from 1 to 65535 |

`image` and `archive` are mutually exclusive. `httpGet` and `tcpConnect` are
also mutually exclusive.

Every declared component is required. A Sandbox reaches `DataPlaneReady` only
after every component passes health and Fastlet Proxy acknowledges its named
route.

## Component names

The name is not a display label. It is shared by:

- Pool revision hashing and validation;
- Fastlet instance and health state;
- Fastlet Proxy route publication;
- FastPath endpoint resolution;
- Sandbox Proxy paths and route credentials;
- fastctl protocol adapters;
- trusted platform ingress integrations.

Names must:

- be unique within the Pool;
- satisfy the Kubernetes DNS label rules;
- not use the reserved `fast-sandbox-` prefix.

Common integration names are:

| Integration | Default name |
| --- | --- |
| OpenSandbox Execd | `execd` |
| E2B envd | `envd` |
| Rocklet | `rocklet` |

A name selects a route. It does not identify or validate the application
protocol; the calling SDK remains responsible for that protocol.

## Artifact sources

Exactly one artifact source is required.

### OCI image

```yaml
artifact:
  source:
    image:
      reference: ghcr.io/example/component@sha256:<digest>
```

Fastlet pulls the image through the namespace Registry configuration, verifies
the immutable reference, and exposes its root filesystem as the mapping source.
It never starts the artifact image.

Mutable tags are rejected. Private artifact images use the same
namespace-scoped Registry configuration as workload images.

### HTTPS archive

```yaml
artifact:
  source:
    archive:
      url: https://downloads.example.com/rocklet-v1.2.0-linux-amd64.tar.gz
      sha256: <lowercase-sha256>
```

Archives are always interpreted as gzip-compressed tar files. Fastlet verifies
the complete archive before extraction and rejects:

- absolute or traversing archive paths;
- unsafe device entries;
- symlinks that escape the extraction root;
- digest mismatches.

Archive authentication is not supported. Use an OCI source for private
platform artifacts.

## Artifact mappings

```yaml
mappings:
  - sourcePath: /bin/rocklet
    targetPath: /.fast/components/rocklet/rocklet
  - sourcePath: /assets
    targetPath: /.fast/components/rocklet/assets
```

Mapping rules:

- `sourcePath` addresses the final file or directory in the verified source;
- `targetPath` is the final Sandbox path, not a parent directory;
- targets must stay under `/.fast/components/<component-name>/`;
- a directory is mapped recursively;
- ownership, mode, and executable bits come from the artifact;
- targets are read-only;
- duplicate and overlapping targets are rejected;
- mappings cannot overwrite user-image, platform, or another component's
  paths.

There is no public mode or executable override.

## Process

The command is executed directly as argv. It does not use a shell or perform
shell expansion. `command[0]` must exist after artifact mappings are applied.

The process inherits the Sandbox base environment and then applies its static
component environment. User-request environment is not copied into the
component automatically. Environment names beginning with `FAST_SANDBOX_` are
reserved.

`sandbox-init` starts all components and the user process concurrently. There
is no dependency graph or configurable start order.

Restart policies behave as follows:

| Policy | Behavior |
| --- | --- |
| `Never` | Do not restart after exit |
| `OnFailure` | Restart after a non-zero exit |
| `Always` | Restart after every exit |

The supervisor applies bounded platform-owned restart backoff. A failed health
check makes the route unavailable but does not kill an otherwise running
process.

## Health and endpoint

The health probe always uses `endpoint.port`; the port is not repeated inside
the health check.

```yaml
healthCheck:
  httpGet:
    path: /ping
  timeoutSeconds: 10
```

or:

```yaml
healthCheck:
  tcpConnect: {}
  timeoutSeconds: 10
```

Fastlet probes the private IP from the runtime-local `DirectIP` access
descriptor.

Health traffic does not traverse Sandbox Proxy.

Each component has one HTTP endpoint. HTTP includes ordinary requests, SSE,
and WebSocket upgrade. The endpoint port is reserved; callers cannot bypass
component-name resolution by requesting the same port as a raw user port.

## Revision and rollout

The Pool Controller computes a deterministic `infraRevision` from normalized
component definitions. Artifact preparation is Pool/Fastlet work and is not
performed on the Sandbox Create path.

When `infraComponents` changes:

1. the Pool receives a new Infra revision;
2. replacement Fastlets prepare that revision;
3. only prepared Fastlets enter placement;
4. old Fastlets drain;
5. existing Sandboxes continue using their admitted revision.

Each Sandbox assignment records the admitted revision. No running Sandbox
receives an in-place component update.

`SandboxPool.status` reports:

- desired `infraRevision`;
- prepared and total Fastlet counts;
- safe component summaries containing name, protocol, port, and health kind;
- the `InfraReady` condition.

## Runtime delivery

The public mapping contract is runtime-neutral:

| Runtime | Delivery mechanism |
| --- | --- |
| container / gVisor | Read-only artifact mount |
| Kata | Guest-visible artifact volume or copy before process start |
| Firecracker | Guest copy into the restored rootfs before process start |

The destination paths, process command, health semantics, and named endpoint
remain the same.

## Status and readiness

| Milestone | Meaning |
| --- | --- |
| `RuntimeReady` | Runtime and user process have started; component/route convergence may continue |
| `ComponentReady` | One component passed health and its route was acknowledged |
| `DataPlaneReady` | The interaction route is published and every declared component is `ComponentReady` |
| aggregate `Ready` | Runtime, DataPlane, all components, and all Action Bindings are Ready |

Create defaults to aggregate `READY`; explicit `RUNTIME_READY` is the
early-return option. After an update, clients poll the assigned Fastlet through
`GetSandbox`. `ResolveEndpoint` is non-blocking and returns
`FailedPrecondition` until aggregate Ready.

Sandbox status eventually projects each component's:

- name;
- `Starting`, `Ready`, or `Failed` state;
- last transition time;
- bounded diagnostic message.

Protocol and port remain in `SandboxPool.spec.infraComponents` and are returned
by endpoint resolution rather than duplicated into every Sandbox status.

Status never exposes artifact credentials, component environment values, or
route credentials.

## Named endpoint security

Named component paths use:

```text
/v2/sandboxes/<uid>/components/<component-name>/...
```

Fast Sandbox forwards the suffix, query, body, streaming behavior, WebSocket
upgrade, and application headers without translating the application protocol.

External routes use the short-lived
`X-Fast-Sandbox-Route-Credential`. The credential is bound to the Sandbox,
physical assignment, target component, resolved port, route generation, and
expiry. Fastlet Proxy removes it before forwarding to the component, while the
application `Authorization` header is preserved.

See [Infra Components](../concepts/infra-components.md),
[Data plane](../concepts/data-plane.md), and
[OpenSandbox Execd](../guides/opensandbox-execd.md).
