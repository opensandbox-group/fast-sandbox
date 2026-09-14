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

## Pool drain

Pool scale-down and template replacement use a persisted drain sequence. A draining Fastlet stops receiving new admission, existing Sandboxes are handled according to lifecycle policy, and the Pod is removed only after the drain contract is satisfied.

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

## Non-goals

The lifecycle contract does not provide:

- live migration of a live instance between Fastlet Pods;
- snapshot or restore of a live instance (snapshots publish bootable images
  and pause/resume restores a checkpoint, both via the artifact store);
- persistent Sandbox storage;
- survival of a Fastlet Pod loss.
