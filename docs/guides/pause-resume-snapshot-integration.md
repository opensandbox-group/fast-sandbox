# Pause, resume, and snapshot integration

This guide is for platform integrators: an API server or control plane built
on top of Fast Sandbox that exposes lifecycle operations to end users. It
explains how to drive **pause/resume** (checkpoint a running Sandbox, release
its capacity, and restore it later, possibly on another host) and
**snapshots** (publish a live Sandbox as a bootable image) through the two
supported integration surfaces:

- the **FastPath v2 gRPC API** — one fenced, idempotent call per operation,
  completion observed via `GetSandbox` / `GetSandboxSnapshot`; or
- the **Kubernetes API server** — the same semantics expressed as Sandbox
  `spec.state` writes plus `SandboxSnapshot` objects, observed through status
  watches.

The protobuf contract is
[`api/proto/v2/fastpath.proto`](../../api/proto/v2/fastpath.proto). Nothing
below requires `fastctl`; the CLI is a thin client over the same gRPC API.

A Chinese field-level companion reference (request fields, CRD spec/status,
and state machines) is available at
[pause-resume-snapshot-integration.zh-CN.md](pause-resume-snapshot-integration.zh-CN.md).

## Choose the primitive

| | Pause / Resume | Snapshot |
| --- | --- | --- |
| Purpose | Free Fastlet capacity while keeping the instance resumable | Capture a reusable, bootable image |
| Durable record | `spec.state` + `status.runtime.checkpoint` on the Sandbox CR | Artifact store under a global `templateName`, plus a SandboxSnapshot CR |
| Restore target | The same Sandbox identity, on any eligible Fastlet (cross-host) | Any new `CreateSandbox(image=<templateName>)` |
| Consumption | One-shot: a completed resume clears the checkpoint | Repeatable: the published image boots indefinitely |
| Guest state captured | Full memory + disk | Full memory + disk, plus the source Sandbox's recorded action bindings |
| Visible interruption | None while `Paused` (the runtime is gone); resume boots from the checkpoint | Sub-second pause during the dump window (`Creating` phase) |

Both write artifacts to the configured artifact store. A pause checkpoint is
private to one Sandbox and is consumed by resume; a snapshot publishes an
index under `templateName` that any create can address, like a golden image.

Both capabilities are implemented by the Firecracker runtime. Gate the
feature to Firecracker pools at your integration layer; other runtimes reject
the task (`UNIMPLEMENTED` on the snapshot trigger, a `SnapshotUnsupported`
`Ready` condition reason for pause).

## Prerequisites

- The target Sandbox is `Ready` on a Firecracker pool.
- The artifact store is configured (the `fast-sandbox-artifact-store`
  ConfigMap).
- The node runtime-agent has a **write credential** for the store (a
  `writeSecretRef` on the registry rule). Without it, snapshots and pause
  fail at publish with a Forbidden-family error. Your platform operator
  provides this; the integration environment wires it automatically for
  `verify-snapshot` / `verify-pause`.

## Surface one: FastPath gRPC (recommended)

### Pause

`PauseSandbox` records the desired state `spec.state=Paused` and returns as
soon as the intent is durable. It does **not** wait for the checkpoint.

```proto
PauseSandbox(
  request_id          = "pause-2026-09-14-01",   // optional; validated, tracing
  sandbox             = { namespaced_name = { namespace: "default", name: "my-sandbox" },
                          expected_uid      = "<uid-from-create>" },
  expected_generation = 7                        // optional CAS fence
)
```

Request fields:

| Field | Required | Meaning |
| --- | --- | --- |
| `request_id` | No | Idempotency/tracing key. Pause is a desired state, so replaying an effective pause is naturally idempotent and returns the current state. |
| `sandbox.namespaced_name` | Yes | Target Sandbox. An omitted namespace resolves to the server default. |
| `sandbox.expected_uid` | Recommended | Rejects with `Aborted` if the Sandbox was deleted and recreated under the same name. Always send it if you hold the UID from create. |
| `expected_generation` | Optional | CAS against concurrent spec updates; `Aborted` on mismatch. |

Rejection codes:

| Code | Condition |
| --- | --- |
| `InvalidArgument` | Missing reference, malformed `request_id` |
| `NotFound` | Sandbox does not exist |
| `FailedPrecondition` | Sandbox is being deleted, or its runtime is `Stopped`/`Stopping`/`Failed` (only a live runtime can be paused; an in-flight create may be paused and simply waits for Ready) |
| `Aborted` | `expected_uid` or `expected_generation` fence mismatch |

### Observe the pause converging

Completion is asynchronous. Poll `GetSandbox` (or watch the Sandbox status)
and surface this state machine to your caller:

```text
runtime_state: READY ──► PAUSING ──► PAUSED
                                │
        runtime keeps serving ──┘  (the checkpoint must be durable first)
```

- `PAUSING`: the dump/upload is running. `checkpoint` is populated as soon as
  the artifact set is durable; the runtime is still being released. Not
  resumable yet.
- `PAUSED`: the checkpoint is complete, the runtime and its durable
  assignment are released, and no Fastlet capacity is occupied. The aggregate
  `Ready` condition flips to `False` with reason `Suspended`, and the
  `Suspended` condition is `True`.
- `checkpoint` is authoritative only while the runtime state is `PAUSING`,
  `PAUSED`, or `RESUMING` (`CheckpointInfo`: `checkpoint_id`, `manifest_ref`,
  `artifact_digest`, `size_bytes`, `paused_unix_seconds`, `fastlet_name`).

A paused Sandbox has no placement, so `GetSandbox` serves the durable CRD
projection instead of a live Fastlet inspection — the read never fails
because the runtime is gone.

Failure behavior: the runtime keeps serving through every pause failure (the
dump resumes the VM on each failure path). The controller retries with a new
attempt epoch (`status.runtime.pauseAttempt`); a deterministic rejection
surfaces as a `Ready` condition reason such as `PauseUnsupported`. Do not
treat `PAUSING` as downtime, and do not time out the pause before the
checkpoint is durable.

While paused, remember for your user-facing model:

- `expireTime` still applies — a paused Sandbox can expire;
- `CreateSandboxSnapshot` on this Sandbox is rejected (it requires a running
  runtime);
- reset (`resetRevision`) and expiry drop the checkpoint lineage;
- deletion removes the checkpoint together with the object.

### Resume

`ResumeSandbox` flips the desired state back to `Running` and schedules the
recorded checkpoint, possibly on a different Fastlet.

```proto
ResumeSandbox(
  request_id             = "resume-2026-09-14-01",
  sandbox                = { namespaced_name = { namespace: "default", name: "my-sandbox" },
                             expected_uid    = "<uid>" },
  expected_generation    = 8,
  expected_checkpoint_id = "<checkpoint_id-from-GetSandbox>"
)
```

| Field | Required | Meaning |
| --- | --- | --- |
| `expected_checkpoint_id` | Recommended | Fences the resume against a re-pause that happened between your `GetSandbox` and this call; mismatch returns `Aborted`. |
| `expected_generation`, `expected_uid` | Optional | Same CAS fences as pause. |

Rejection codes:

| Code | Condition |
| --- | --- |
| `FailedPrecondition` | The Sandbox is durably `Paused` with **no** recorded checkpoint (resume is impossible; a fresh instance requires `resetRevision`), or the Sandbox is being deleted |
| `Aborted` | Fence mismatches, including `expected_checkpoint_id` |
| — | Resuming a Sandbox that is already `Running` (or one whose pause is still converging) is an idempotent success; before the runtime is released it simply cancels the pause |

Observe completion the same way:

```text
runtime_state: PAUSED ──► RESUMING ──► READY
```

Integration obligations when `READY` returns:

- The one-shot checkpoint is consumed: `checkpoint` is cleared. A second
  resume of the same paused period is impossible.
- The Sandbox may be on a different Fastlet: the route generation advanced,
  so every previously resolved endpoint and route credential is stale. Drop
  cached `ResolveEndpoint` results taken before the pause and re-resolve.
- The first restore on a node pulls the artifact set from the store
  (seconds to minutes for multi-GiB sets); later restores on that node are
  fast. Size the user-visible resume timeout accordingly.

### Snapshot

`CreateSandboxSnapshot` takes a live checkpoint of a Ready Sandbox and
publishes it as a bootable image. It returns after the intent is persisted
and the snapshot is triggered; it does not wait for the dump.

```proto
CreateSandboxSnapshot(
  request_id    = "ckpt-2026-09-14-01",   // required; becomes the CR name and idempotency key
  sandbox       = { namespaced_name = { namespace: "default", name: "my-sandbox" },
                    expected_uid    = "<uid>" },
  template_name = "my-app-v2",            // the image name this snapshot publishes
  metadata      = {"team": "search"}      // optional, becomes labels
)
```

| Field | Required | Meaning |
| --- | --- | --- |
| `request_id` | Yes | Idempotency key. Replaying with identical fields returns the current snapshot state; changing any field under the same ID is an `AlreadyExists` conflict. |
| `sandbox.expected_uid` | Recommended | Fills the CR fence; a recreated same-name Sandbox fails the snapshot. |
| `template_name` | Yes | Artifact-store index key. **Global across namespaces** and effectively permanent: after publishing, `CreateSandbox(image=template_name)` boots from this set. Pick it like an image tag. |
| `metadata` | No | Projected as labels on the SandboxSnapshot object. |

Rejection codes:

| Code | Condition |
| --- | --- |
| `InvalidArgument` | Missing/malformed `request_id`, `template_name`, or metadata |
| `FailedPrecondition` | The Sandbox runtime is not Ready; or the same Sandbox already has a snapshot in `Pending`/`Creating`; or the same `template_name` is held by any non-terminal snapshot cluster-wide |
| `AlreadyExists` | The `request_id` belongs to a different snapshot intent |
| `Unimplemented` | The pool's runtime does not support snapshots |
| `Aborted` | Deterministic fastlet rejections (stale assignment/generation); the snapshot object is marked `Failed` |

The concurrency fences are asymmetric: a previous snapshot of the same
Sandbox stops blocking once it reaches `Publishing` (the VM is already
resumed), while the template-name fence holds until the holder terminates.

### Observe the snapshot converging

Poll `GetSandboxSnapshot(request_id-as-name, expected_uid)` or watch the
SandboxSnapshot status:

| Phase | Meaning |
| --- | --- |
| `Pending` | Intent persisted, not yet admitted by a Fastlet |
| `Creating` | The pause window: VM paused, memory + disk dumped, VM resumed. The only user-visible interruption lives here |
| `Publishing` | Dump complete, VM already resumed; the artifact set is uploading |
| `Succeeded` | Terminal. `manifest_ref` / `artifact_digest` / `size_bytes` describe the published set |
| `Failed` | Terminal. `message` explains; retry with a **new** `request_id` (the object is one-shot) |

Rules to surface in your API:

- After `Failed`, issue a new snapshot with a new `request_id`; nothing the
  failed attempt produced is addressable.
- `DeleteSandboxSnapshot` removes the object (idempotent; `expected_uid`
  fenced) but **never unpublishes the artifacts** — store lifecycle is
  managed out of band.

### Boot from the snapshot

Nothing snapshot-specific: the template name is just an image reference.

```proto
CreateSandbox(
  request_id = "sbx-from-ckpt-01",
  namespace  = "default",
  image      = "my-app-v2",
  pool_ref   = "firecracker-pool"
)
```

Fastpath resolves the image through the artifact store at create time and
re-applies the action bindings recorded in the snapshot manifest:

- bindings you pass explicitly on the create win per handler;
- recorded handlers the target Pool does not declare are dropped;
- resolution is best-effort — an unreachable store proceeds without recorded
  policy, so create availability never hinges on the object store.

## Surface two: Kubernetes API server

The gRPC operations above are compare-and-set spec writes plus status
observation; a declarative integration can express them directly.

**Pause** — patch the desired state and watch the status:

```bash
kubectl patch sandbox my-sandbox -n default --type=merge \
  -p '{"spec":{"state":"Paused"}}'

kubectl get sandbox my-sandbox -n default -w \
  -o jsonpath='{.status.runtime.state}{"\n"}'   # Ready → Pausing → Paused
```

**Resume** — flip the desired state back (fences are not available on this
surface; use gRPC when you need `expected_checkpoint_id`):

```bash
kubectl patch sandbox my-sandbox -n default --type=merge \
  -p '{"spec":{"state":"Running"}}'
```

**Snapshot** — create the object (name = idempotency key, spec is
CEL-immutable) and watch the phase:

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxSnapshot
metadata:
  name: ckpt-2026-09-14-01
  namespace: default
spec:
  sandboxRef:
    name: my-sandbox
    namespace: default
  templateName: my-app-v2
```

```bash
kubectl get sandboxsnapshot ckpt-2026-09-14-01 -w \
  -o jsonpath='{.status.phase}{"\n"}'
```

Status mapping between the surfaces:

| Concept | FastPath v2 | Sandbox CR / SandboxSnapshot CR |
| --- | --- | --- |
| Desired state | `SandboxInfo.state` (`RUNNING`/`PAUSED`) | `spec.state` (`Running`/`Paused`) |
| Runtime state | `runtime.state` (`PAUSING`/`PAUSED`/`RESUMING`) | `status.runtime.state` |
| Checkpoint | `SandboxInfo.checkpoint` | `status.runtime.checkpoint` (+ `pauseAttempt`) |
| Paused condition | `ready=false`, checkpoint set | `Ready=False` reason `Suspended`; `Suspended=True` |
| Snapshot lifecycle | `SnapshotPhase` | `status.phase` (+ `Completed` condition) |

Declarative pause/resume has no request fencing: concurrent spec writers can
race. If your apiserver already serializes user intent, that is equivalent;
otherwise prefer the gRPC fences.

## Integration checklist

An apiserver exposing these operations should:

1. Gate the feature to Firecracker pools (check `GetPool().runtime`).
2. Return immediately to the user after the RPC/patch succeeds — the call
   acknowledges durable intent, not completion.
3. Poll `GetSandbox` until `PAUSED` (pause) or `READY` (resume), with
   timeouts sized for an artifact-store round trip, and expose
   `runtime_state` plus `checkpoint.size_bytes` as progress.
4. On resume success, invalidate every cached endpoint/route credential for
   the Sandbox (route generation advanced) and re-resolve lazily.
5. Send `expected_uid` on every call, and `expected_generation` /
   `expected_checkpoint_id` when the flow allows it — they convert recreate
   and re-pause races into explicit `Aborted` errors.
6. Model the checkpoint as one-shot: after a successful resume the paused
   period cannot be restored again.
7. Treat snapshot `Failed` as final for that `request_id`; retry with a new
   one. Treat snapshot `Publishing` as safe for the source Sandbox (next
   snapshot allowed).
8. Keep user-visible expiry semantics honest: `expireTime` applies while
   paused, and deletion of a paused Sandbox discards its checkpoint.

## End-to-end validation

The integration environment exercises both flows, including fencing and
artifact validation:

```bash
./scripts/integration-env.sh up
./scripts/integration-env.sh verify-snapshot   # live snapshot → boot-from-image E2E
./scripts/integration-env.sh verify-pause      # cross-host pause/resume E2E
```

## References

- [API reference](../reference/api.md) — CRD fields and the full RPC table
- [Sandbox lifecycle](../concepts/sandbox-lifecycle.md) — pause/resume state
  machine, identity model, non-goals
- [Sandbox Snapshots](sandbox-snapshots.md) — snapshot internals: spill
  area, fences, artifact layout, recorded network policy
- [OpenSandbox integration](opensandbox-integration.md) — create, endpoint
  resolution, and route contract around these operations
