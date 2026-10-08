# Performance

Fast Sandbox does not use one unqualified startup number as a performance claim. Image pulls, runtime creation, VM boot, Infra readiness, route publication, cache state, and client concurrency are different costs and must be reported separately.

## Published results

There is no release-grade Sandbox Create benchmark report published in this repository.

The repository contains the dated engineering baseline below, runtime smoke observations, a scheduler microbenchmark, and a public Create load generator. They are engineering tools, not a fair cross-product or production baseline:

- `BenchmarkRegistryTopK1000` measures in-process candidate ranking, not Sandbox creation;
- percentile fixtures under `test/performance` are synthetic unit-test data;
- isolated warm-container samples are not a latency distribution;
- runtime results cannot be compared while hardware, cache state, readiness boundary, or workload changes.

A release claim may be published only with its raw report, command, commit,
environment, and interpretation. The engineering baseline is retained to explain
the current architecture and to guide optimization; it is not a release claim.

## Current engineering baseline

The following measurements were collected on 2026-07-27 while investigating the
imperative Create path. They describe the current architecture at base revision
`42fe03549598c3ab730b989c7757634b486697cf`. Additional measurement-only
instrumentation split the containerd client calls into RPC sub-stages without
changing lifecycle ordering.

### Environment

| Dimension | Value |
|---|---|
| Execution environment | KVM virtual machine |
| CPU | 8 vCPUs, Intel Xeon Platinum 8269CY at 2.50 GHz |
| Memory | 15 GiB, no swap |
| Kernel | Linux 5.15.0-173-generic, x86-64 |
| Cluster | Single-node kind v0.31.0 |
| Kubernetes node | v1.27.3 |
| Runtime used by the kind node | containerd 1.7.1 |
| Virtualization note | Kata runs with nested KVM in this environment |

### Measurement contract

- The client issued one request at a time; this is latency at concurrency 1,
  not a throughput result.
- Images and runtime artifacts were already present. Registry pull and first
  unpack latency are excluded.
- The measurement explicitly sets `CreateCompletion=RUNTIME_READY`, starts
  before the FastPath Create RPC, and ends when that early-return call returns.
- `RuntimeReady` means the RuntimeDriver created the runtime and started the
  user process. It does not wait for Infra readiness, proxy route publication,
  `DataPlaneReady`, or CRD status projection.
- The container sample used the minimal Infra profile. The gVisor and Kata
  validation samples injected OpenSandbox Execd; Execd readiness remained
  asynchronous and is outside the reported Create latency.
- Kata Firecracker and Dragonball were validated after this measurement run
  and therefore have no comparable Create result in this dated baseline. A
  later comparison is reported separately below.

### RuntimeReady observations

| Runtime | Samples | Mean client-observed Create | Mean RuntimeDriver work | Dominant runtime stage |
|---|---:|---:|---:|---|
| container (`runc`) | 20 | 76.02 ms | 67.76 ms | containerd `Tasks/Create` |
| gVisor (`runsc`) | 10 | 644.29 ms | 634.78 ms | task create and start |
| Kata Cloud Hypervisor | 10 | 1,359.59 ms | 1,350.26 ms | VM task create |
| Kata QEMU | 10 | 2,125.58 ms | 2,110.77 ms | VM task create |

For the 20 warm container samples, client-observed Create was p50 75.95 ms and
p95 83.15 ms. The other runtime observations were small diagnostic batches;
only their means are retained, so they must not be presented as percentile
distributions.

These rows are not a fair product or even runtime ranking. The secure-runtime
profiles have different initialization work, and Kata is measured under nested
virtualization. They show where the current Fast Sandbox integration spends
time on this environment.

### Kata 3.31 secure-runtime comparison

A later engineering run at revision
`cde6d5c8e82f568ae3dbfd919ea7284713603f13` compared the complete Kata 3.31
runtime set on a non-nested KVM host and a nested-KVM development VM. Both
environments used a single-node Kind cluster, containerd 1.7.18, a warm Alpine
image, pre-created network slots, no Infra Components, and concurrency 1. Each
row contains 20 measured Creates with `CreateCompletion=RUNTIME_READY`. Every runtime was warmed before measurement; the secure
runtimes used two warm-up requests, while nested runc was combined from two
separately warmed batches.

#### Non-nested KVM host

| Runtime | Mean | p50 | p95 | Maximum | Success |
|---|---:|---:|---:|---:|---:|
| container (`runc`) | 72.4 ms | 72.7 ms | 80.2 ms | 83.1 ms | 20/20 |
| Kata Firecracker | 560.6 ms | 559.6 ms | 622.8 ms | 626.9 ms | 20/20 |
| Kata QEMU | 1,065.8 ms | 1,050.6 ms | 1,142.1 ms | 1,170.0 ms | 20/20 |
| Kata Cloud Hypervisor | 829.4 ms | 828.8 ms | 861.6 ms | 871.5 ms | 20/20 |
| Kata Dragonball | 783.6 ms | 775.6 ms | 814.4 ms | 828.9 ms | 20/20 |

The host exposed 104 vCPUs and 187 GiB of memory. The Pool capacity was 24 and
the 20 measured Sandboxes remained live until the batch cleanup. Firecracker
used the resolved `blockfile` snapshotter. Its mean RuntimeDriver stages were
approximately 1.1 ms for image preparation, 21.7 ms for `NewContainer`, 528.3
ms for `NewTask`, and 1.5 ms for `Start`.

#### Resource-constrained nested-KVM VM

| Runtime | Mean | p50 | p95 | Maximum | Success |
|---|---:|---:|---:|---:|---:|
| container (`runc`) | 73.2 ms | 73.1 ms | 77.2 ms | 82.9 ms | 20/20 |
| Kata Firecracker | 5,474.6 ms | 5,462.3 ms | 5,575.1 ms | 5,725.3 ms | 20/20 |
| Kata QEMU | 2,068.7 ms | 2,065.9 ms | 2,153.6 ms | 2,193.8 ms | 20/20 |
| Kata Cloud Hypervisor | 1,338.5 ms | 1,335.4 ms | 1,373.5 ms | 1,403.1 ms | 20/20 |
| Kata Dragonball | 2,425.6 ms | 2,414.5 ms | 2,499.9 ms | 2,545.2 ms | 20/20 |

The VM had 8 vCPUs, 16 GiB of memory, nested KVM, no swap, and a nearly full
root disk during the run. To stay within its disk and memory limits, the runc
samples ran in two batches of ten, while each Kata sample was deleted and
allowed to settle before the next Create. The method therefore differs from
the non-nested batch and the ratio is directional, not a controlled A/B result.

The contrast explains why one Firecracker number is misleading. On the
non-nested host its work was dominated by VM task creation; in the constrained
nested VM, `NewContainer` and blockfile preparation grew from about 21.7 ms to
4.34 s, while `NewTask` grew from about 528 ms to 1.11 s. QEMU, Cloud
Hypervisor, and Dragonball were dominated primarily by nested VM task creation.
Runc remained near 73 ms in both environments.

These measurements validate the runtime paths and provide regression evidence.
They do not include `DataPlaneReady`, Execd health, registry pulls, or an
OpenSandbox client path, and they are not release-grade latency promises. No VM
template, snapshot restore, or prepared-VM mechanism was enabled.

### Warm container stage breakdown

The mean 67.76 ms RuntimeDriver duration breaks down as follows:

| Stage | Mean |
|---|---:|
| containerd `NewContainer` | 21.95 ms |
| containerd `NewTask` | 36.87 ms |
| task `Start` | 7.89 ms |

The client-observed mean exceeds RuntimeDriver work by approximately 8.26 ms.
That remainder includes FastPath orchestration, Fastlet RPC and admission,
network-slot acquisition, serialization, and client/server transport.

The following measurements are nested inside the stages above and are therefore
not additive:

| Nested operation | Mean |
|---|---:|
| snapshot option inside `NewContainer` | 10.82 ms |
| snapshotter `Prepare` RPC | 7.82 ms |
| lease create and delete | 5.38 ms |
| container metadata create | 2.63 ms |
| OCI spec generation | 3.02 ms |
| containerd `Tasks/Create` RPC | 35.00 ms |
| `NewTask` pre-RPC work, including shim setup | approximately 1.78 ms |
| containerd `Tasks/Start` RPC | 7.86 ms |

`strace` confirmed the expected runc lifecycle for each new Sandbox:

```text
containerd-shim-runc-v2 ... start
  -> long-running containerd-shim-runc-v2
  -> runc create
  -> runc start
```

The trace was used only to establish process structure because tracing changes
timing materially.

### Interpretation

On this environment, most warm-container time is below the Fast Sandbox Go
orchestration layer. `Tasks/Create`, shim/runc setup, task start, snapshot
preparation, and container metadata account for most of the 76 ms result.
Caching and small orchestration changes may remove several milliseconds, but
they do not remove the per-Sandbox shim and runc lifecycle.

The secure-runtime results are even more strongly runtime-dominated. gVisor
spends roughly 607.76 ms in task create and start. Kata Cloud Hypervisor spends
approximately 1,321.62 ms there, and Kata QEMU approximately 2,015.47 ms.

The architectural implication is that sub-50-ms startup cannot be treated only
as a FastPath code-optimization target. It requires a runtime path that moves
heavy work out of the request, such as prepared instances, clone/resume, or a
runtime with a different single-machine lifecycle. The existing measurements
remain useful as regression baselines while that path is developed.

## Latency milestones

| Milestone | Meaning |
|---|---|
| Request accepted | The request passed client and API validation |
| Intent persisted | Kubernetes stored the Sandbox and durable assignment |
| Fastlet admitted | Fastlet accepted capacity and runtime identity |
| RuntimeReady | RuntimeDriver completed Ensure; Create may return only in explicit early-return mode |
| DataPlaneReady | Required Infra services are ready and routes are published |
| Declarative status ready | The Controller projected observed state to the CRD |

These measurements use the explicit RuntimeReady completion boundary. The API
default is aggregate Ready and is intentionally not represented by these
runtime-only numbers.

## Required benchmark dimensions

Record:

- commit SHA and exact command;
- CPU, memory, storage, virtualization, kernel, Kubernetes, and containerd;
- component replica counts;
- total requests, concurrency, and request rate;
- runtime and Infra Component revision;
- image reference and warm/cold hit/miss state;
- network-slot state;
- Fast-Path or direct-CRD path;
- start and end milestones;
- p50, p95, p99, maximum, errors, admission rejections, and retries.

A cross-project comparison must disclose architecture differences such as one Pod per Sandbox versus multiple runtimes per warm Pod.

## Create load tool

```bash
go run ./test/performance/create_load \
  --endpoint 127.0.0.1:19090 \
  --namespace perf \
  --pool perf-pool \
  --requests 100 \
  --concurrency 10 \
  --cleanup \
  --cleanup-network-slots 10 \
  --cleanup-registry-settle 25s \
  --fastlet-metrics-url http://127.0.0.1:18081/metrics \
  --commit "$(git rev-parse HEAD)" \
  --environment '8c/32GiB, Linux kernel X, kind X, containerd X' \
  --runtime container \
  --infra-revision sha256:<pool-infra-revision> \
  --image-state warm \
  --image-affinity hit \
  --network-slot-state clean \
  --fastpath-replicas 3 \
  --controller-replicas 2 \
  --sandbox-proxy-replicas 2 \
  >create-load-warm.json
```

Use a unique request-ID prefix for every sample. Reusing it measures idempotent replay.

Cleanup submits deletion for every deterministic request ID, including a request
whose RPC failed after its CRD was persisted. It then waits until those CRDs are
gone. For repeatable batches, forward each participating Fastlet metrics endpoint,
repeat `--fastlet-metrics-url`, and set `--cleanup-network-slots` to the aggregate
clean-slot baseline. This additionally waits for asynchronous network-slot
replenishment before the process exits.

Fast-Path replicas maintain local heartbeat-driven registries. For immediately
repeated batches, set `--cleanup-registry-settle` to at least the configured
heartbeat interval plus jitter. Network readiness and scheduling-view freshness
are reported as separate waits; neither is silently included in Create latency.

The JSON includes both percentile summaries and sorted per-request RPC latency
samples so independent runs can be merged without averaging percentiles. The
process reports deletion submission and convergence separately. It exits non-zero
on Create failure, duplicate identity, cleanup submission failure, or cleanup
convergence timeout while still writing JSON.

## Scheduler microbenchmark

```bash
go test ./internal/controlplane/placement -run '^$' \
  -bench '^BenchmarkRegistryTopK1000$' -benchmem -count=5
```

Use it only to compare Registry implementations on the same machine and commit range. Retain raw output and use `benchstat`. It excludes Kubernetes, Fastlet admission, runtime/network creation, Infra readiness, and routing.

## Metrics

Relevant series include:

- `fast_sandbox_create_accepted_latency_seconds`;
- `fast_sandbox_create_runtime_ready_latency_seconds`;
- `fast_sandbox_create_stage_latency_seconds`;
- `fast_sandbox_fastlet_create_stage_latency_seconds`;
- `fast_sandbox_runtime_create_latency_seconds`;
- `fast_sandbox_containerd_create_stage_latency_seconds`;
- `fast_sandbox_user_process_start_latency_seconds`;
- `fast_sandbox_data_plane_ready_latency_seconds`;
- `fast_sandbox_registry_heartbeat_age_seconds`;
- `fast_sandbox_registry_candidate_count`;
- `fast_sandbox_image_affinity_result_total`;
- `fast_sandbox_topk_retry_total`;
- `fast_sandbox_fastlet_admission_total`;
- `fast_sandbox_network_slot_available`;
- `fast_sandbox_network_slot_inuse`;
- `fast_sandbox_network_slot_acquire_latency_seconds`;
- `fast_sandbox_network_slot_persist_latency_seconds`;
- `fast_sandbox_infra_instance_stage_latency_seconds`;
- `fast_sandbox_infra_ready_latency_seconds`;
- `fast_sandbox_sandbox_proxy_route_latency_seconds`;
- `fast_sandbox_fastlet_proxy_upstream_latency_seconds`.

An inventory snapshot is a scheduling hint, not proof that one Create avoided pull or unpack work. Per-request `cache_hit` remains `unknown` unless the RuntimeDriver provides a trustworthy observation.

## Release acceptance

A performance report must also prove:

1. multi-active Create does not exceed Fastlet capacity or duplicate identity;
2. image-hit candidates are preferred without hiding admission conflicts;
3. heartbeat and Registry cost remain bounded as replicas scale;
4. SSE, WebSocket, and file streams are not fully buffered;
5. Controller and Proxy replica loss preserves aggregate availability;
6. reset, reassignment, and deletion invalidate stale route credentials.

Correctness E2E tests do not constitute throughput claims. A single-node kind cluster cannot model distinct node image caches.

## Profiling

```bash
go tool pprof 'http://localhost:6060/debug/pprof/profile?seconds=30'
```

Use Linux runtime environments for containerd, network, and secure-runtime profiles. Local macOS profiling is suitable only for pure Go work.
