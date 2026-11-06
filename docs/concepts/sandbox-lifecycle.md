# Sandbox lifecycle

A Sandbox CRD represents durable lifecycle intent. A runtime instance is a replaceable, generation-fenced realization of that intent on one Fastlet Pod.

## Identity model

The active identity combines:

```text
Sandbox CRD UID
+ instance generation
+ Fastlet Pod UID
+ assignment attempt
+ runtime instance ID
+ route generation
```

Each element serves a different purpose:

- **CRD UID** prevents a deleted and recreated object with the same name from owning old resources.
- **Instance generation** fences reset and automatic recreation.
- **Fastlet Pod UID** fences Pod replacement even when the Pod name is reused.
- **Assignment attempt** fences movement to another Fastlet.
- **Runtime instance ID** identifies the concrete backend object.
- **Route generation** invalidates old data-plane credentials and caches.

## Independent states

The Sandbox status separates:

- runtime state;
- data-plane state;
- Infra Component and Action Binding state;
- placement/recovery and subsystem-owned generations;
- one aggregate standard `Ready` Condition.

A runtime may be ready while its required Infra Component is still starting. A data-plane failure does not rewrite runtime truth.

## Create

Fast-Path and direct CRD creation converge through the same Orchestrator and Fastlet protocol. Fastlet Create is idempotent for the complete fenced identity:

- the same identity resumes or returns the existing result;
- a stale identity cannot replace a newer one;
- capacity includes creating, running, deleting, and cleanup-failed instances until absence is proven.

Fast-Path writes the complete initial lifecycle intent in the first Sandbox
CRD operation, including absolute expiry, metadata, failure policy, and
recovery timeout. Create defaults to aggregate `Ready`; explicit
`RUNTIME_READY` returns early while components, Actions, and DataPlane converge.

## Delete

Deletion uses a finalizer:

1. mark the local Sandbox Terminating, then attempt Action Binding
   `RemoveBinding` in reverse order under one shared five-second deadline;
2. stop publishing the route;
3. delete the runtime through an ensure-absent backend operation;
4. release network and Infra resources;
5. retain platform cleanup state and retry on partial failure;
6. remove the finalizer only when platform resource absence is proven.

Missing task, container, snapshot, network, or Infra state is treated as success when the desired state is absence. This makes repeated deletion and deletion after a workload exits idempotent.

## Reset

Reset is requested by advancing `spec.resetRevision`. Reconciliation:

1. drains the old route;
2. deletes the old runtime and associated resources;
3. advances instance and route generations;
4. creates a replacement under the new identity;
5. records the accepted reset revision.

Old credentials and runtime callbacks cannot affect the new generation.

## Fastlet loss

The Fastlet Pod is the instance lifetime boundary.

- `Manual` reports loss and leaves recovery to the user.
- `AutoRecreate` waits for `recoveryTimeoutSeconds`, advances the instance identity, and schedules a new runtime.

AutoRecreate does not preserve process memory, local filesystem state, or network identity. It creates a new instance from the CRD spec.

## Runtime exit while Fastlet remains online

When a created runtime reports `Stopped` and its dedicated cgroup is confirmed empty, Fastlet durably records the complete instance identity as `Failed` before reclaiming physical resources, even while Infra, routes, or Actions are still pending or unavailable. Terminal recording fences lifecycle callbacks and cancels data-plane work; a failed write retains the identity and resource commitment for the next heartbeat to retry. Route publication and removal are ordered so cleanup cannot release the network slot ahead of an in-flight route write, and proxy reconnects exclude terminal identities. Heartbeat, Inspect, and an idempotent Ensure for that identity continue to return the same terminal record across a Fastlet restart. The Controller revalidates that the CR assignment still names the observed identity, persists `Failed`, and only then acknowledges deletion of the Fastlet record; the persisted CR state prevents a later `NotFound` from recreating the failed identity.

`failurePolicy` remains scoped to Fastlet Pod loss. An online single-runtime exit stays `Failed` until an explicit reset advances the instance identity; it does not implicitly apply `AutoRecreate`.

The terminal CR keeps its placement and assignment identity after acknowledgement so a later `NotFound` cannot recreate the failed generation. That terminal decision no longer counts as pending admission or active placement demand, but it does not prove physical cleanup has finished: drain completion still requires the Fastlet's recovery-complete, empty admission and resource snapshot. If the original Pod is gone, the Controller stops waiting for its acknowledgement and keeps the terminal decision; NodeJanitor remains responsible for orphaned host resources.

## Recovery with uncertain runtime state

For ResourceAware direct Firecracker, a live process outside its expected cgroup or an unavailable VMM API with a populated cgroup blocks recovery of that Fastlet. Recovery retains the instance record, network slot, and resource ownership and retries; API failure alone does not authorize terminating a guest that may still be running. Recovery errors identify the Sandbox, recorded PID, Pod UID, and phase, and verbose API probe logs retain the underlying error. Operators should inspect those ownership and API observations before choosing an explicit recovery action. This version does not automatically terminate a live VMM after a timeout or admit around an unresolved instance; per-instance recovery isolation requires a separate design.

## Pool drain

Pool scale-down and template replacement use a persisted drain sequence. A draining Fastlet stops receiving new admission, existing Sandboxes are handled according to lifecycle policy, and normal drain completion requires a matching attempt to return an atomic, recovery-complete empty admission snapshot. Non-protected planned replacements retain their timeout policy after a successful drain RPC. During a rolling upgrade from an older Fastlet, deletion always requires a fresh Heartbeat RPC started after the drain acknowledgement to identify the same Pod and carry the same empty, recovery-complete evidence; cached heartbeats and timeout alone cannot authorize legacy deletion.

Planned replacement can use a ready surge so the new Fastlet proves runtime and Infra capability before the old one drains.

## Orphan cleanup

If the owning Fastlet disappears before cleanup, NodeJanitor evaluates backend resources on that node. Cleanup requires:

- a minimum orphan age;
- a fresh Kubernetes ownership lookup;
- a mismatch or absence of the complete owner fence.

This prevents an old observation from deleting a runtime that is still owned by an active Sandbox generation.

## Pause and resume

`spec.state: Paused` checkpoints the Sandbox and releases its Fastlet
capacity; the Sandbox CR stays in the cluster as the resume ticket:

1. the assigned Fastlet dumps the runtime (rootfs + vmstate + memory) and
   publishes the set to the artifact store (a checkpoint, not a template);
2. the Controller persists the artifact address in
   `status.runtime.checkpoint` **before** touching the runtime;
3. the runtime and the durable assignment are released, and the Sandbox
   reports `Paused` with the `Suspended` Condition.

`Paused` is durable-first: the runtime keeps serving until the checkpoint is
complete in the store. Resuming flips `spec.state` back to `Running`; the
Controller schedules the checkpoint on any eligible Fastlet (cross-host) and
restores its memory under the same Sandbox identity, advancing the route
generation. A completed resume consumes the one-shot checkpoint
(`status.runtime.checkpoint` is cleared; store objects follow store
lifecycle).

While paused there is no placement, `expireTime` still applies, a template
snapshot is rejected (it requires a running runtime), and reset/expiry drop
the checkpoint lineage. Pause failures leave the runtime running (the dump
resumes the VM on every failure path) and retry with a new attempt epoch
(`status.runtime.pauseAttempt`).

Fast-Path clients may request pause immediately after create returns Ready, even if the Controller has not projected that observation into the Sandbox status yet. The Controller continues creation/observation until the runtime is ready to checkpoint. `Paused` is published only after runtime deletion and durable assignment cleanup complete, so clients can resume immediately when they observe it. Once a checkpoint is persisted, release completes even if a concurrent resume request changes the desired state to Running; the subsequent reconciliation restores that checkpoint with a fresh assignment. If pause is requested again during an unfinished restore, the Controller preserves the original checkpoint lineage, finishes restoring, and then checkpoints the restored runtime. Assignment cleanup also resumes after an interrupted status write. No client-side settle delay is required.

## Non-goals

The lifecycle contract does not provide:

- live migration of a live instance between Fastlet Pods;
- snapshot or restore of a live instance (snapshots publish bootable images
  and pause/resume restores a checkpoint, both via the artifact store);
- persistent Sandbox storage;
- survival of a Fastlet Pod loss.
