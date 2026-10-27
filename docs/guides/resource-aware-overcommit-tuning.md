# Resource-Aware Overcommit Tuning

Use this guide to choose Pool overcommit ratios and per-Sandbox runtime limits.
Callers provide limits only. Fast Sandbox derives requests from the Pool
policy and charges those requests to the Fastlet commitment ledger.

## Configure one resource authority

```yaml
spec:
  maxSandboxesPerPod: 10
  sandboxResources:
    mode: ResourceAware
    cpu: "2"       # default per-Sandbox limit
    memory: 5Gi    # default per-Sandbox limit
    pids: 256
    overcommitRatio:
      cpu: "8"
      memory: "1.25"
  fastletTemplate:
    spec:
      containers:
        - name: fastlet
          image: fast-sandbox/fastlet:dev
```

Do not set CPU or memory requests or limits on the first Fastlet container in
ResourceAware mode. The Controller owns those values. Sidecars may keep their
own Kubernetes resources.

Existing mode-less Pools remain LegacySlot. They retain flat
`sandboxResources.cpu`, `.memory`, and `.pids`, explicit first-container
resources, and slot-only admission.

## Understand limit, request, capacity, and envelope

For each Sandbox:

```text
request.cpu    = ceil(limit.cpu / overcommitRatio.cpu)
request.memory = ceil(limit.memory / overcommitRatio.memory)
```

The Pool defaults determine Fastlet capacity:

```text
commitmentCapacity = defaultRequest * maxSandboxesPerPod
sandboxRuntimeBudget = max(commitmentCapacity, defaultLimit)
runtimeEnvelope      = sandboxRuntimeBudget + fixedFastletOverhead
```

The runtime-owner container receives equal Kubernetes requests and limits of
`runtimeEnvelope`. `fixedFastletOverhead` is selected by the platform runtime
profile and added once per Fastlet Pod. It covers the Fastlet/runtime owner,
while each Sandbox or VM remains inside its caller-visible limit; there is no
separate per-Sandbox overhead field.

Fastlet admission, FastPath placement, and Pool scaling all consume the same
commitment capacity. A custom per-Sandbox limit is rejected if it exceeds
`sandboxRuntimeBudget`, so it cannot consume the fixed overhead reserve.

## Tune CPU and memory separately

The configured ratios are also the intended limit-to-request ratios:

```text
CPU overcommit ratio    = limit.cpu / request.cpu
Memory overcommit ratio = limit.memory / request.memory
```

CPU is compressible. A large CPU ratio can be reasonable when Sandboxes are
mostly idle or burst briefly. Watch sustained CPU demand, throttling, latency,
and throughput. The first implementation uses static CPU commitment and does
not block admission solely because current CPU utilization is high.

Memory is not safely compressible in the same way. Growth above the normal
working set can cause reclaim, severe latency, or OOM termination. Start with
a conservative memory ratio and watch RSS, working set, memory PSI, OOM
events, high-watermark residence, and application latency.

Fastlet combines the static memory ledger with a dynamic gate. Memory
watermarks, PSI, new OOM events, host memory pressure, or stale telemetry can
block new creates. The gate does not reclaim, pause, migrate, or evict an
existing Sandbox.

## Worked example

```text
default limit:         2 CPU, 5 GiB
overcommit ratio:      8 CPU, 1.25 memory
default request:       250m, 4 GiB
maxSandboxesPerPod:    10
```

The Fastlet contract is:

```text
commitmentCapacity.cpu    = 250m * 10 = 2500m
commitmentCapacity.memory = 4Gi * 10  = 40Gi

sandboxRuntimeBudget.cpu    = max(2500m, 2 CPU) = 2500m
sandboxRuntimeBudget.memory = max(40Gi, 5Gi)    = 40Gi

# With the container profile's 100m / 128Mi fixed overhead:
runtimeEnvelope.cpu    = 2600m
runtimeEnvelope.memory = 40Gi + 128Mi
```

A smaller Create request may specify:

```yaml
resources:
  limits:
    cpu: "1"
    memory: 2500Mi
```

Fast Sandbox derives `125m / 2Gi`. A larger Sandbox with `2 CPU / 5Gi`
charges `250m / 4Gi`. The Fastlet ledger sums these exact heterogeneous
requests; it does not multiply Sandbox count by the default request.

## Capacity-planning workflow

1. Measure sustained CPU demand, throttling, latency, and throughput.
2. Measure memory working set, RSS, PSI, reclaim, and OOM behavior.
3. Pick default limits that cover the common runtime shape.
4. Pick separate ratios that turn those limits into conservative commitments.
5. Set `maxSandboxesPerPod` for both resource density and non-resource costs
   such as netns entries, file descriptors, and runtime state.
6. Ensure the derived Sandbox runtime budget can accept the largest intended
   custom Sandbox limit; otherwise reduce the ratio, raise the slot count, or
   use a different Pool class.
7. Roll out gradually and compare static rejections, dynamic memory
   rejections, CPU throttling, latency, OOM events, and Pool scaling.

Do not raise ratios merely because one sample reported free pages or reclaim
succeeded. Those observations are transient and do not change commitment.

## Elasticity behavior

A `BLOCKED`, `UNKNOWN`, stale, or draining Fastlet contributes zero
schedulable free capacity. The Pool Controller adds capacity when pending
request vectors plus `bufferMin` exceed the aggregate free vectors. It also
packs each pending request against individual Fastlet free vectors and scales
out when aggregate capacity is fragmented so no single Fastlet can admit a
larger Sandbox.

An accepted Creating or Resuming Sandbox is already charged in the Fastlet
ledger and is not counted as pending demand again. A new Fastlet without a
heartbeat contributes nominal capacity only during startup grace.

Scale-down requires continuous excess capacity. The start time is the
`ScaleDownCandidate=True` Condition's `lastTransitionTime`. Only empty
Fastlets are selected; a candidate that becomes loaded has its scale-down
drain canceled instead of being force-deleted.

See the [architecture design](../design/resource-aware-overcommit-placement.md)
for the full protocol and the
[capacity validation guide](resource-aware-capacity-validation.md) for manual
cluster verification.
