# Scheduling and capacity

Fast Sandbox uses local scheduling Registries for low-latency placement and Fastlet atomic admission for correctness.

## Fixed Pool model

One SandboxPool fixes:

- runtime;
- Sandbox CPU, memory, and PID limits;
- InfraProfile;
- `maxSandboxesPerPod`;
- warm images;
- Fastlet Pod template;
- minimum, maximum, and buffer sizing.

Runtime, resource, and Infra profiles are immutable because changing them would alter the meaning of already assigned Sandboxes.

## Registry inputs

Every Fast-Path and Controller replica maintains its own Registry from:

1. Kubernetes watches for Pod membership, Pool labels, assignments, Pod UID changes, and lifecycle intent;
2. low-frequency jittered Fastlet heartbeats for runtime capability, admission inventory, phase counts, image-cache state, and cache revision;
3. immediate feedback from local placement attempts.

Watches provide topology changes without full polling. Heartbeats refresh runtime-local facts and repair missed or stale observations.

Registries are eventually convergent by design. They do not coordinate through a global lock.

## Top-K placement

The Orchestrator:

1. filters by namespace, Pool, readiness, runtime profile, Infra profile, and stale heartbeat;
2. requires a positive locally observed available-slot count;
3. prioritizes image-cache affinity;
4. ranks by load and a stable tie-breaker;
5. returns a bounded Top-K candidate list.

The candidate list bounds network calls and retry work. It is not a reservation.

## Atomic admission

Fastlet serializes capacity and identity transitions in one admission boundary. A slot remains occupied while an instance is:

- creating;
- running;
- deleting;
- waiting for failed-Create cleanup.

Capacity is released only after runtime, network, Infra, and route absence is proven.

This prevents stale or inconsistent Registries across Fast-Path replicas from exceeding `maxSandboxesPerPod`.

## Retry semantics

| Result | Scheduler action |
|---|---|
| Explicit rejection before side effects | Record local feedback and try the next Top-K candidate |
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

The Pool Controller maintains minimum/maximum Fastlet counts and buffer capacity. Scale-down and template replacement select Fastlets for persisted drain rather than deleting an arbitrary Pod.

The scheduling Registry excludes draining Fastlets from new placement.

## Node-scoped cleanup

Some cache and runtime state is node-scoped rather than Fastlet-scoped. Global policy can choose what to protect, while execution remains in the node-side runtime/cache owner. NodeJanitor handles orphan correctness, not placement.
