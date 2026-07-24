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
- user-process state;
- assignment and generation fields;
- canonical `RuntimeReady` and `DataPlaneReady` Conditions.

A runtime may be ready while its required Infra Component is still starting. A data-plane failure does not rewrite runtime truth.

## Create

Fast-Path and direct CRD creation converge through the same Orchestrator and Fastlet protocol. Fastlet Create is idempotent for the complete fenced identity:

- the same identity resumes or returns the existing result;
- a stale identity cannot replace a newer one;
- capacity includes creating, running, deleting, and cleanup-failed instances until absence is proven.

## Delete

Deletion uses a finalizer:

1. stop publishing the route;
2. delete the runtime through an ensure-absent backend operation;
3. release network and Infra resources;
4. retain cleanup state and retry on partial failure;
5. remove the finalizer only when absence is proven.

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

## Non-goals

The lifecycle contract does not provide:

- live migration between Fastlet Pods;
- snapshot or restore;
- pause or resume;
- persistent Sandbox storage;
- survival of a Fastlet Pod loss.
