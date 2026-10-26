# Control plane

Fast Sandbox separates a multi-active Fast-Path from leader-elected Reconcilers. They use the same codebase and orchestration rules but serve different consistency and latency requirements.

## Roles

| Role | Leader election | Service membership | Main work |
|---|---:|---:|---|
| Fast-Path | No | Yes | Synchronous Create, lifecycle intent updates, diagnostics, endpoint resolution |
| Reconciler | Yes | No | Sandbox and SandboxPool convergence, recovery, drain, status projection |
| All-in-one | No | Development only | Both roles in one process |

Every Fast-Path replica is active. Only the elected Controller replica runs Reconcilers.

## Imperative and declarative operations

Create has a synchronous Fast-Path because clients need a runtime identity quickly. The remaining lifecycle operations are declarative:

- Delete marks the Kubernetes object for deletion.
- Reset updates `spec.resetRevision`.
- Expiry updates `spec.expireTime`.
- Failure behavior updates `spec.failurePolicy` and `spec.recoveryTimeoutSeconds`.

The Controller observes these changes and converges runtime, network, route, and Infra state.

Creating a Sandbox CRD directly is supported. Fast-Path is a latency optimization, not the only creation path.

## CRD-first Create

The successful Fast-Path path has two downstream operations:

1. create the Sandbox CRD with the complete initial intent and durable
   assignment annotation;
2. call the selected Fastlet's atomic Create operation.

Fast-Path does not write status on the request path.

```mermaid
sequenceDiagram
  participant C as Client
  participant F as Fast-Path
  participant R as Local Registry
  participant K as Kubernetes API
  participant L as Fastlet
  participant RC as Reconciler

  C->>F: CreateSandbox(request_id, spec)
  F->>R: rank bounded Top-K
  F->>K: Create Sandbox + assignment
  F->>L: atomic Create(fenced identity)
  alt runtime created or already present
    L-->>F: requested completion reached (default aggregate Ready)
    F-->>C: complete SandboxInfo
  else rejected before side effects
    F->>K: CAS assignment to another candidate
    F->>L: atomic Create(new attempt)
  else ambiguous outcome
    F-->>C: retryable error
    RC->>K: observe durable intent
    RC->>L: resume the same identity
  else every candidate rejects before side effects
    F->>K: delete the newly created intent
    F-->>C: ResourceExhausted
  end
```

## Request identity

`request_id` is required and is the canonical Sandbox CRD name. Retries must reuse it with the same immutable Create spec.

- A matching existing object is an idempotent replay.
- A different workload, expiry, metadata, failure, or Pool intent under the
  same request ID is rejected.
- A failure that occurs before candidate selection creates no CRD.
- An ambiguous failure after CRD creation leaves the same assignment durable
  for retry and reconciliation.
- If every Fastlet explicitly rejects before side effects, Fast-Path deletes
  the intent created by that API call before returning `ResourceExhausted`.

The initial CRD write includes absolute expiry and user metadata. Fast-Path
Create does not require a follow-up lifecycle update to complete initial
intent.

Transport ambiguity never authorizes reassignment. Only an explicit Fastlet rejection before side effects permits Fast-Path to CAS the assignment to the next bounded candidate.

## Assignment and status

The initial assignment is stored with the Sandbox Create. Reassignment changes it with Kubernetes resource-version compare-and-swap. Reconcilers project the durable assignment and independently observed subsystem states into status.

Runtime, data plane, Infra Components, and Action Bindings are deliberately
separate:

- `status.runtime` reports the runtime adapter;
- `status.dataPlane` reports route publication;
- `status.infraComponents` and `status.actionBindings` report ordered detailed
  convergence;
- the single `Ready` Condition aggregates them.

Create defaults to aggregate Ready. Explicit `RUNTIME_READY` returns early;
remaining convergence and CRD status projection then continue asynchronously.

## Local Registry

Each Fast-Path and Controller replica owns a local Registry. It receives:

- topology and identity changes from Kubernetes watches;
- admission, runtime, and cache facts from low-frequency jittered Fastlet heartbeats;
- immediate local feedback from placement attempts.

Registries need not be perfectly synchronized. They produce candidates; Fastlet admission is the capacity authority.

## Failure properties

| Failure | Result |
|---|---|
| Fast-Path crashes before CRD Create | No durable object or runtime exists |
| Fast-Path crashes after CRD Create | Reconciler observes and resumes the assignment |
| Fast-Path loses the Fastlet response | The same identity is retried; no speculative reassignment |
| Fastlet explicitly rejects before side effects | Fast-Path may CAS to another Top-K candidate |
| Controller leader is lost | Another Controller becomes leader; Fast-Path remains available |
| A Fast-Path replica is lost | Other replicas continue serving through the Service |

The model relies on Kubernetes persistence and fenced idempotency, not a distributed execution lease.
