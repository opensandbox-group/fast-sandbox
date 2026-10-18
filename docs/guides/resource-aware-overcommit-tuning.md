# Resource-Aware Overcommit Tuning

Use this guide to choose CPU and memory requests and limits for a
`ResourceAware` SandboxPool. The request-to-limit gap is the overcommit policy;
there is no separate ratio field and no Fastlet Pod resource override.

## Configure one resource authority

Configure only these capacity inputs:

```yaml
spec:
  maxSandboxesPerPod: 10
  sandboxResources:
    requests:
      cpu: 250m
      memory: 4Gi
    limits:
      cpu: "2"
      memory: 5Gi
    pids: 256
  fastletTemplate:
    spec:
      containers:
        - name: fastlet
          image: fast-sandbox/fastlet:dev
```

Do not set CPU or memory requests or limits on the first Fastlet container in
`ResourceAware` mode. The Controller rejects that configuration because it
would create a second source of truth. Sidecars may keep their own independent
Kubernetes resources.

Existing `LegacySlot` Pools are different. Their flat
`sandboxResources.cpu` and `.memory` fields, slot admission, explicit first
container resources, and aggregate overcommit behavior remain supported.

## Understand the two derived budgets

For each resource, the Controller derives pure Sandbox commitment capacity:

```text
commitmentCapacity = Sandbox request * maxSandboxesPerPod
```

It then derives the runtime-owner envelope:

```text
runtimeEnvelope = fixedFastletOverhead
                + max(commitmentCapacity, one Sandbox limit)

runtimeOwner.requests = runtimeEnvelope
runtimeOwner.limits   = runtimeEnvelope
```

The fixed overhead is selected by the platform runtime profile and is added
once per Fastlet Pod. It is not a user field and it is not per Sandbox. The
`max` term ensures that the parent cgroup can honor one Sandbox at its legal
limit, even when the total commitment is smaller.

Fastlet admission, FastPath placement, and Pool scaling consume the same
resolved commitment capacity. They do not inspect the first container's
Kubernetes request to reconstruct a different budget.

## Tune CPU and memory separately

Calculate the ratios independently:

```text
CPU overcommit ratio
  = sandboxResources.limits.cpu / sandboxResources.requests.cpu

Memory overcommit ratio
  = sandboxResources.limits.memory / sandboxResources.requests.memory
```

CPU is compressible. A larger CPU request-to-limit gap is often acceptable
when Sandboxes are usually idle or burst briefly. The main failure mode is
contention: throttling, latency, and reduced throughput. The initial policy
uses CPU requests for static commitment admission but does not hard-block a
new Sandbox solely because real-time CPU usage is high.

Memory is not safely compressible in the same way. A workload can grow from
its normal working set toward its limit after admission. Reclaim may add severe
latency, and exhaustion may terminate processes. Start with a conservative
memory gap and expand it only from measured working-set and pressure data.

ResourceAware Fastlets combine the static memory ledger with a dynamic gate.
New creates are blocked by configured high watermarks, memory PSI, OOM events,
node memory pressure, or stale telemetry. This protection does not reclaim,
pause, wake, migrate, evict, or preempt an existing Sandbox.

## Worked example

Given the Pool above:

```text
CPU request:                  250m
CPU limit:                       2
Memory request:               4 GiB
Memory limit:                 5 GiB
maxSandboxesPerPod:              10
example fixed overhead: 250m, 256 MiB
```

The independent overcommit ratios are:

```text
CPU:    2 / 250m = 8:1
Memory: 5Gi / 4Gi = 1.25:1
```

The derived capacities are:

```text
commitmentCapacity.cpu    = 250m * 10 = 2500m
commitmentCapacity.memory = 4Gi * 10  = 40Gi

runtimeEnvelope.cpu
  = 250m + max(2500m, 2) = 2750m

runtimeEnvelope.memory
  = 256Mi + max(40Gi, 5Gi) = 40Gi + 256Mi
```

The runtime-owner container receives equal Kubernetes requests and limits of
2750m CPU and 40 GiB plus 256 MiB memory. The Fastlet ledger still exposes
only 2500m CPU and 40 GiB memory to Sandboxes; fixed overhead cannot be spent
by Sandbox admission.

Ten idle or normally loaded Sandboxes may be admitted. Their CPU limits sum to
20 CPUs and their memory limits sum to 50 GiB, but the parent envelope bounds
correlated bursts. CPU contention degrades performance. Memory pressure can
block the tenth create before the static ledger is full if existing Sandboxes
are already bursting above their 4 GiB commitments.

## Capacity-planning workflow

1. Measure sustained CPU demand, CPU throttling, request latency, and throughput
   for representative Sandboxes.
2. Measure memory working set, RSS, high-watermark residence time, memory PSI,
   reclaim behavior, and OOM events.
3. Choose requests that cover normal sustained demand rather than the idle
   baseline.
4. Choose limits that permit useful bursts without making a correlated burst
   exceed acceptable parent-cgroup risk.
5. Set `maxSandboxesPerPod` from both resource density and non-resource actor
   costs such as network namespaces, file descriptors, and runtime state.
6. Roll out gradually and compare static rejections, dynamic memory
   rejections, CPU throttling, latency, OOM events, and Pool scale events.

Do not raise the configured ratios merely because memory was reclaimed or a
sample reported free pages. Those observations are transient and do not change
the immutable commitment policy.

## Elasticity behavior

A Fastlet with `BLOCKED`, `UNKNOWN`, stale, or unsupported resource state
contributes no effective free capacity. The Pool Controller may create a
replacement Fastlet when the remaining capacity cannot cover pending demand
and `bufferMin`.

A new Fastlet without a heartbeat contributes nominal capacity only during the
startup grace period. After the grace expires it contributes zero until a
fresh heartbeat arrives, preventing a CrashLooping Pod from hiding a capacity
deficit.

`RuntimeCreating` already owns a Fastlet reservation, so it is not counted as
pending demand a second time. Scale-down requires continuous excess capacity
for the configured stabilization window, and the candidate start time is
persisted in SandboxPool status.

## Rollout controls

The Controller exposes independent switches for static Fastlet admission,
dynamic memory admission, FastPath placement, and Pool scaling. Placement
requires static admission, and Pool scaling requires placement. Disable the
newest decision layer first during rollback; disabling a switch never changes
the meaning of an immutable resource profile.

See the [configuration reference](../reference/configuration.md) for flag names
and defaults, and the
[architecture design](../design/resource-aware-overcommit-placement.md) for
the complete protocol, failure, and recovery contract. Use the
[capacity validation guide](resource-aware-capacity-validation.md) to exercise
derived resources, demand-driven elasticity, pressure heartbeats, and
stabilized scale-down on a development cluster.
