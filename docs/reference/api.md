# API reference

Fast Sandbox exposes Kubernetes CRDs for declarative lifecycle and a gRPC
FastPath API for latency-sensitive clients.

## Version boundary

```text
CRD group: sandbox.fast.io
CRD version: v1alpha2
FastPath package: fastpath.v2
```

`v1alpha2` is the only canonical runtime representation. It removes the old
`infraProfile` reference in favor of inline Pool components.

## Sandbox

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: Sandbox
metadata:
  name: example
  namespace: fast-sandbox
  labels:
    owner: team-a
spec:
  image: docker.io/library/alpine:latest
  command: ["/bin/sleep"]
  args: ["3600"]
  poolRef: default-pool
  expireTime: "2026-07-30T00:00:00Z"
  failurePolicy: Manual
  recoveryTimeoutSeconds: 60
  actionBindings:
    - handler: egress
      input: '{"defaultAction":"deny"}'
```

### Spec

| Field | Required | Meaning |
| --- | ---: | --- |
| `image` | Yes | Workload OCI image |
| `command`, `args` | No | User process override |
| `envs` | No | Kubernetes `EnvVar` array |
| `workingDir` | No | User process working directory |
| `expireTime` | No | Absolute declarative expiry |
| `failurePolicy` | No | `Manual` or `AutoRecreate`; default `Manual` |
| `recoveryTimeoutSeconds` | No | Durable delay before recovery action; default 60 |
| `resetRevision` | No | Opaque monotonic reset trigger |
| `poolRef` | Yes | Same-namespace SandboxPool |
| `actionBindings` | No | Ordered atomic Handler/input list; order defines lifecycle invocation order |

User metadata is stored as ordinary Kubernetes labels. Labels under
`sandbox.fast.io/` are reserved for the platform.

### Status

| Field | Meaning |
| --- | --- |
| `observedGeneration` | Sandbox Spec generation represented by this Controller snapshot |
| `placement` | Assignment attempt, Fastlet name/Pod UID, and nested recovery deadline |
| `runtime` | Runtime state, concrete runtime generation, transition time/message, and accepted reset revision |
| `dataPlane` | Route state, route-generation fence, transition time, and message |
| `infraComponents` | Per-component name, `Starting`/`Ready`/`Failed`, transition time, and message |
| `actionBindings` | Per-Binding Handler, `Pending`/`Applying`/`Ready`/`Failed`, transition time, and message |
| `conditions` | One aggregate standard `Ready` Condition |

Runtime and DataPlane use separate state enums. Input digests, invocation IDs,
runtime IDs, and per-Handler fences remain internal and never appear in CRD
Status. `observedGeneration` means the Controller processed that Spec; current
convergence requires the aggregate `Ready` Condition to be `True` with its
`observedGeneration` equal to `metadata.generation`. A Binding transition time
comes from Fastlet and includes an internally observed `Ready -> Applying ->
Ready` cycle even if the Controller only polls the final `Ready` state.

## SandboxPool

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxPool
metadata:
  name: default-pool
  namespace: fast-sandbox
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
  warmImages:
    - docker.io/library/alpine:latest
  actionHandlers:
    - name: egress
      targetHTTPPort: 18080
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
        command: ["/.fast/components/execd/execd", "--port", "44772"]
        restartPolicy: OnFailure
        healthCheck:
          httpGet:
            path: /ping
          timeoutSeconds: 10
      endpoint:
        protocol: HTTP
        port: 44772
  fastletTemplate:
    spec:
      containers:
        - name: fastlet
          image: fast-sandbox/fastlet:dev
```

### Spec

| Field | Required | Meaning |
| --- | ---: | --- |
| `capacity` | Yes | Pool size and idle-buffer bounds |
| `maxSandboxesPerPod` | Yes | Fastlet-authoritative admission limit |
| `runtime` | Yes | Immutable runtime name |
| `sandboxResources` | Yes | Immutable per-Sandbox CPU, memory, and PID limits; their sum may exceed an explicitly lower Fastlet aggregate limit |
| `warmImages` | No | Asynchronous, GC-protected cache inputs |
| `infraComponents` | No | Inline immutable artifact/process/health/endpoint definitions |
| `actionHandlers` | No | Named Pod-loopback Binding receivers and their lifecycle Hook subscriptions |
| `fastletTemplate` | Yes | Kubernetes Pod template with platform-owned fields protected; runtime-owner limits define the optional aggregate overcommit budget |

Runtime names are `container`, `gvisor`, `kata-qemu`, `kata-clh`, `kata-fc`,
`kata-dragonball`, and `boxlite`.

Pool status exposes Fastlet capacity, runtime/Infra/Fastlet revisions, prepared
Fastlet counts, and per-image warm-cache aggregation. Handler and Registry
rollout details are not duplicated as nested status. Conditions are
`RuntimeReady`, `InfraReady`, and `RegistryReady`.

See the [Infra Components reference](infra-components.md) for the complete
artifact, process, health, endpoint, validation, and revision contract.
See [Sandbox Actions](../concepts/sandbox-actions.md) for Binding, lifecycle
Hook, ordering, readiness, and recovery semantics.

## SandboxTemplate

SandboxTemplate declares a golden-image build for the Firecracker runtime: an
OCI source image, runtime injection, guest init, readiness, and output
artifacts published to an S3-compatible store. The build is executed by a
controller as a privileged Pod on a KVM node.

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxTemplate
metadata:
  name: ai-office-sandbox
  namespace: fast-sandbox
spec:
  image: registry.example.com/sandbox:v1.0.21
  kernel: vmlinux.bin
  machine:
    vcpu: "2"
    memory: "2Gi"
  entrypoint: ["/opt/gem/run.sh"]
  init: /usr/local/sbin/sandbox-init
  envs:
    - name: FOO
      value: bar
  readiness:
    warmupSeconds: 60
  output:
    rootfsSize: "30Gi"
    format: overlaybd
    publish: s3://sandbox-images/publish
    publishSecretRef:
      name: sandbox-images-readwrite
```

### Spec

| Field | Required | Default | Meaning |
| --- | ---: | --- | --- |
| `image` | Yes | — | OCI image reference to convert; becomes the byte-identical consumer-side addressing key |
| `kernel` | Yes | — | Guest kernel embedded in the builder image (build-time asset, not a node runtime asset) |
| `machine.vcpu` | Yes | `"1"` | Snapshot VM vCPUs (resource quantity) |
| `machine.memory` | Yes | `"2Gi"` | Snapshot VM memory (resource quantity); restore requires ≥ this |
| `entrypoint` | No | `["tail","-f","/dev/null"]` | Guest business command as an argv list |
| `execd` | No | — | OpenSandbox execd image injected into the guest rootfs |
| `init` | No | `/usr/local/sbin/sandbox-init` | Injected guest PID 1 (readiness marker + heartbeat) |
| `envs` | No | — | Literal `EnvVar` array written to `/etc/sandbox-init.env`; `valueFrom` unsupported, image `Config.Env` not merged, published verbatim in the manifest |
| `readiness.probe` | No | — | Custom gate first: `tcp://host:port` or `cmd://<command>` |
| `readiness.warmupSeconds` | Yes | `60` | Fallback time-based warmup |
| `readiness.healthCheck` | No | — | Fallback health command; empty uses image `CMD-SHELL` |
| `output.rootfsSize` | Yes | `"30Gi"` | Logical rootfs capacity (minimum, rounded up to SI GiB) |
| `output.format` | Yes | `overlaybd` | `native` or `overlaybd` (both contain the full snapshot set) |
| `output.publish` | Yes | — | S3-compatible digest-addressed publish target, e.g. `s3://bucket/prefix` |
| `output.publishSecretRef` | No | — | Write-credential Secret name in the template's namespace (`accessKeyId`/`secretAccessKey`/`endpoint`/`region`) |
| `output.prime` | No | — | Reserved: seed-node cache priming; not yet implemented |

Readiness precedence: custom `probe` → execd `/ping` → `warmupSeconds` +
`healthCheck`.

### Status

| Field | Meaning |
| --- | --- |
| `phase` | `Pending`, `Building`, `Succeeded`, or `Failed` |
| `conditions` | `BuildSucceeded` with reason/message on terminal states |
| `manifestRef` | Published manifest URI (`s3://…/<sha256(manifest)[:16]>/manifest.json`) |
| `artifactDigest` | sha256 of the manifest document itself |
| `lastBuildTime` | When the latest build completed |
| `observedGeneration` | Generation of the last applied build |

Build Pods run in the template's own namespace as
`<template>-build-<generation>`, owned by the template (deleting the
template cascades to its Pods via the garbage collector), pinned to
`sandbox.fast.io/kvm=true` nodes; editing the spec triggers a rebuild
(generation bump). Finished Pods are reaped after the build TTL (24 h).

See the [SandboxTemplate guide](../guides/sandboxtemplate-golden-images.md)
for the end-to-end workflow, artifact layout, and consumption contract.

## SandboxSnapshot

SandboxSnapshot is a one-shot live checkpoint of a running Sandbox: the
Firecracker driver pauses the VM, dumps the artifact set, resumes, and
publishes it to the artifact store in the SandboxTemplate layout, addressable
under `spec.templateName`. The spec is immutable (CEL-enforced).

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxSnapshot
metadata:
  name: checkpoint-1            # == the create request_id (idempotency key)
spec:
  sandboxRef:
    name: my-sandbox
    namespace: default
    uid: ""                     # fastpath fills the live UID; mismatches fence
  templateName: my-app-v2
```

### Spec

| Field | Required | Meaning |
| --- | ---: | --- |
| `sandboxRef.name`/`namespace` | Yes | Target running Sandbox |
| `sandboxRef.uid` | No | Filled by fastpath from the validated object; a recreated same-name Sandbox fails the snapshot |
| `templateName` | Yes | Artifact-store index key the set is published under; global across namespaces, same pattern as SandboxTemplate `indexKey`. Only this one index key is written — never a default `sha256(image)` |

### Status

| Field | Meaning |
| --- | --- |
| `phase` | `Pending` → `Creating` (pause window) → `Publishing` (upload only, VM resumed) → `Succeeded`/`Failed` |
| `triggered` | Placement + identity fence pinned at trigger time; observations resolve it across later reassignments |
| `fastletName`/`fastletPodUID`, `snapshotID`, `sandboxUID` | Placement projection and node-local task identity |
| `manifestRef`, `artifactDigest`, `sizeBytes` | Published artifact facts (Succeeded only) |
| `conditions` | `Completed` (True on Succeeded; False + reason on Failed) |

Fences are split by resource: the **Sandbox** fence covers only
`Pending`/`Creating` (the pause window) — a snapshot in `Publishing` no
longer blocks the next snapshot of the same Sandbox. The **template-name**
fence holds to terminal. `Failed` never moved the store index, so nothing it
produced is addressable.

See the [Sandbox Snapshots guide](../guides/sandbox-snapshots.md) for the
pause window, spill area, artifact layout, and write credentials.

## FastPath v2

The protobuf contract is
[`api/proto/v2/fastpath.proto`](../../api/proto/v2/fastpath.proto).

| RPC | Semantics |
| --- | --- |
| `CreateSandbox` | Atomic durable intent followed by Fastlet admission; omitted completion waits for aggregate `READY`, explicit `RUNTIME_READY` returns early |
| `GetSandbox` | Fenced point-in-time live view from the assigned Fastlet |
| `ListSandboxes` | Lightweight CRD-backed identity/generation summaries with metadata filtering |
| `UpdateSandbox` | Asynchronously commit a typed desired-state mutation, complete ordered Binding replacement, or metadata upsert/delete; the returned generation is not an applied/Ready acknowledgement |
| `DeleteSandbox` | Submit declarative deletion |
| `GetSandboxDiagnostics` | Lifecycle and Fastlet diagnostics, not process stdout |
| `ResolveEndpoint` | Non-blocking resolution of a named component or raw user port in central/direct mode; requires live aggregate Ready |
| `CreateSandboxSnapshot` | One-shot live checkpoint of a Ready Sandbox; `request_id` is the idempotency key, sandbox fence until `Publishing`, template-name fence until terminal |
| `GetSandboxSnapshot` | CR-backed phase/artifact/placement observation |
| `DeleteSandboxSnapshot` | Deletes the object (`expected_uid` fenced); never unpublishes stored artifacts |
| `GetPool`, `ListPools` | Runtime, fixed resources, components, capacity, and warm-image discovery |

### Atomic Create

`CreateRequest` includes:

- `request_id`, namespace, image, Pool, command, args, environment, and working directory;
- absolute `expires_at_unix_seconds`;
- initial `metadata`;
- failure policy and recovery timeout.
- ordered `action_bindings` and a `CreateCompletion` boundary.

The first CRD write contains the complete initial intent. A retry with the same
normalized intent is idempotent; a changed intent under the same request ID is
a conflict. Fast Sandbox does not extend an absolute expiry during retry.

### Metadata update and list

`UpdateRequest` carries `metadata_upsert` and `metadata_delete_keys`. A key is
preserved when absent, replaced when upserted, and removed only when explicitly
listed for deletion.

`ListRequest.metadata` has AND semantics. Filtering happens before bounded
pagination.

### Endpoint targets

`SandboxReference` uses namespace/name for lookup and accepts an optional
`expected_uid` identity fence. `EndpointTarget` is
exactly one of:

- `component_name`; or
- raw `port`.

Resolution never waits. A non-Ready live Sandbox returns
`FailedPrecondition`; stale/missing assignment state returns `Unavailable`.
The response contains one nested `endpoint` with the final component identity
(when applicable), protocol, and port, plus route generation, expiry, proxy URL,
and `X-Fast-Sandbox-Route-Credential`.

Central mode returns:

```text
Sandbox Proxy -> Fastlet Proxy -> Sandbox
```

Direct mode is for a trusted platform ingress:

```text
trusted ingress -> Fastlet Proxy -> Sandbox
```

Application `Authorization` is not consumed by Fast Sandbox route
authentication.

See [OpenSandbox integration](../guides/opensandbox-integration.md) for the
trusted direct-ingress contract.

## Error semantics

- local validation and no-capacity errors occur before CRD creation;
- only an explicit side-effect-free Fastlet rejection permits trying another
  Top-K candidate;
- ambiguous transport failure preserves and retries the durable assignment;
- failures after CRD persistence leave durable intent for Reconciler takeover;
- Delete is accepted before asynchronous finalizer cleanup completes;
- stale Pod UID, instance generation, assignment attempt, route generation, or
  route credential is rejected.
