# Scheduling and capacity

Fast Sandbox uses local scheduling Registries for low-latency placement and Fastlet atomic admission for correctness.

## Fixed Pool model

One SandboxPool fixes:

- runtime;
- Sandbox CPU, memory, and PID limits;
- an immutable compiled Infra Component revision;
- `maxSandboxesPerPod`;
- warm images;
- Fastlet Pod template;
- minimum, maximum, and buffer sizing.

Runtime, resource, and Infra profiles are immutable because changing them would alter the meaning of already assigned Sandboxes.

## Logical capacity and physical budget

`maxSandboxesPerPod` is always a hard Sandbox-count boundary. It limits runtime
state, network namespaces, file descriptors, and control-plane bookkeeping in
addition to CPU and memory.

### LegacySlot

A flat `sandboxResources.cpu` and `.memory` profile keeps the existing slot
model. The first Fastlet container may declare an explicit aggregate request
and limit, including a limit lower than the sum of per-Sandbox limits. Existing
Pool manifests and aggregate overcommit behavior are unchanged.

### ResourceAware

An explicit `sandboxResources.mode: ResourceAware` selects the resource-aware
model. Flat CPU and memory are default per-Sandbox limits; separate Pool ratios
derive requests:

```text
defaultRequest = ceil(defaultLimit / overcommitRatio)
```

Callers may provide different per-Sandbox limits. Their requests are derived
with the same ratios. The first Fastlet container must not declare CPU or
memory resources, because they would create a second capacity authority. The
Controller derives:

```text
commitmentCapacity = defaultRequest * maxSandboxesPerPod

runtimeEnvelope = max(commitmentCapacity, default Sandbox limit)

runtimeOwner.requests = runtimeOwner.limits = runtimeEnvelope
```

Commitment capacity is shared unchanged by Fastlet admission, FastPath
placement, and Pool scaling. The runtime envelope ensures the parent cgroup can
honor at least one default Sandbox limit. Sidecar resources remain independent.

Each RuntimeDriver receives only Fastlet-resolved Sandbox limits. Requests are
ledger values; they are not currently translated to CPU weight, `memory.low`,
or another per-Sandbox QoS setting.

CPU and memory are asymmetric. CPU contention normally produces throttling and
latency, so live CPU use does not hard-block admission in the initial release.
Memory pressure may cause reclaim, long stalls, or OOM termination, so memory
also has a dynamic admission gate based on watermarks, PSI, OOM events, node
pressure, and telemetry freshness. That gate only blocks new creates; it does
not reclaim, pause, migrate, or evict existing Sandboxes.

See [Resource-aware overcommit tuning](../guides/resource-aware-overcommit-tuning.md)
for a complete sizing example and operational guidance.

## Registry inputs

Every Fast-Path and Controller replica maintains its own Registry from:

1. Kubernetes watches for Pod membership, Pool labels, assignments, Pod UID changes, and lifecycle intent;
2. jittered Fastlet heartbeats for runtime capability, aggregate commitment,
   pressure, phase counts, image-cache state, Infra revision, and Registry
   revision;
3. full aggregate admission snapshots piggybacked on Create success,
   definitive rejection, and Delete responses;
4. short-lived process-local reservations while Create RPCs are in flight.

Watches provide topology changes without full polling. Heartbeats refresh runtime-local facts and repair missed or stale observations.

Registries are eventually convergent by design. They do not coordinate through
a global lock; Fastlet atomic admission resolves cross-replica races.

## Top-K placement

The Orchestrator:

1. filters by namespace, Pool, readiness, runtime profile, Infra revision, Registry revision, and stale heartbeat;
2. requires fresh eligible slot and resource ledgers in ResourceAware mode;
3. prioritizes image-cache affinity;
4. ranks by a coarse dominant-load bucket, FastPath Pod UID seed, and a stable
   Sandbox tie-breaker;
5. returns a bounded Top-K candidate list.

The candidate list is not a reservation. A newly accepted Fast-Path create
refreshes Top-K after a batch of proven side-effect-free rejections, excluding
every Fastlet already tried by that request. Compatibility revisions are
filtered before Top-K. Refreshes use only the local Registry and retain the
initial assignment's runtime, resource, and Infra profiles. Work is bounded by
32 distinct Fastlet attempts and the caller deadline; the first-create happy
path still uses two Kubernetes IOs. Every reassignment uses an assignment CAS.
Replayed existing intents remain pinned to their persisted assignment.

During a proven-rejection candidate switch, Fast-Path waits up to two seconds
(with bounded exponential backoff and the caller deadline) if the full durable
assignment still matches but status has not caught up. It rechecks the complete
annotation on every attempt, retains the strict status-projection check, and
does not call the next Fastlet until CAS succeeds. A changed assignment,
non-projection error, cancellation, or an exhausted wait stops the switch and
retains the intent for reconciliation. This adds no wait or Kubernetes IO to
the first-candidate success path; it does not permit reassigning an ambiguous
runtime outcome.

The colocated Fast-Path and Controller share a process-local active-create
tracker, acquired before persisting the intent and released when the RPC exits.
Reconciliation continues projecting the annotation into status while an RPC
is active, but defers status-based Pod-loss recovery and runtime
ensure/reassignment until the RPC releases it. A deleted or replaced previous
Fastlet Pod cannot prevent the new assignment from being projected.
This avoids competing candidate switches in the same process without another
Kubernetes write. Declarative-only reconciliation and crash recovery remain
unchanged. Separate processes do not share this tracker; the durable CAS and
fencing still reject their concurrent identity changes.

## Atomic admission

Fastlet serializes capacity and identity transitions in one admission boundary. A slot remains occupied while an instance is:

- creating;
- running;
- deleting;
- waiting for failed-Create cleanup.

Capacity is released only after runtime, network, Infra, and route absence is proven.

This prevents stale or inconsistent Registries across Fast-Path replicas from
exceeding slots or CPU/memory commitment capacity.

## Retry semantics

| Result | Scheduler action |
|---|---|
| Explicit rejection before side effects | Record local feedback and try the next candidate; refresh Top-K within the request's bounded budget for a newly accepted intent |
| Idempotently present | Return the existing identity |
| Transport ambiguity | Retry the same assignment and identity |
| Create in progress | Retry or let Reconciliation continue |
| Cleanup required | Preserve assignment and resume cleanup |
| No candidate | Return a bounded capacity error without creating a CRD |

Only a proven side-effect-free rejection permits reassignment.

## Image affinity

Fastlet heartbeats report an image inventory and cache revision. Normalized image references let the Registry prefer a candidate that already has the requested content.

Inventory is a placement hint. It does not prove that a specific Create avoided pull or unpack work, so per-request cache metrics remain `unknown` unless the RuntimeDriver reports a trustworthy result.

`warmImages` are pulled asynchronously and protected from ordinary cache garbage collection. New Fastlets become Ready before every warm image is present; scheduling naturally improves as cache state arrives.

Protected cache classes include:

- Pool warm images;
- Infra Component artifacts;
- active runtime content;
- recently hot images.

## Pool scaling and drain

The Pool Controller maintains minimum/maximum Fastlet counts and buffer
capacity. ResourceAware scale-up uses the exact summed request vectors of
pending Sandboxes plus `bufferMin`; accepted Creating and Resuming Sandboxes
are not counted twice. Fastlets without a heartbeat contribute nominal
capacity only during startup grace.

Scale-down begins only after a continuous excess-capacity window represented
by the `ScaleDownCandidate` Condition. It selects empty Fastlets for persisted
drain rather than deleting an arbitrary Pod. If a scale-down candidate becomes
loaded, its drain is canceled after timeout instead of forcing deletion.

The scheduling Registry excludes draining Fastlets from new placement.

## Node-scoped cleanup

Some cache and runtime state is node-scoped rather than Fastlet-scoped. Global policy can choose what to protect, while execution remains in the node-side runtime/cache owner. NodeJanitor handles orphan correctness, not placement.
