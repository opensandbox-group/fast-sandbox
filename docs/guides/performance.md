# Performance

Fast Sandbox does not use one unqualified startup number as a performance claim. Image pulls, runtime creation, VM boot, Infra readiness, route publication, cache state, and client concurrency are different costs and must be reported separately.

## Published results

There is no release-grade Sandbox Create benchmark report published in this repository.

The repository contains runtime smoke observations, a scheduler microbenchmark, and a public Create load generator. They are engineering tools, not a fair cross-product or production baseline:

- `BenchmarkRegistryTopK1000` measures in-process candidate ranking, not Sandbox creation;
- percentile fixtures under `test/performance` are synthetic unit-test data;
- isolated warm-container samples are not a latency distribution;
- runtime results cannot be compared while hardware, cache state, readiness boundary, or workload changes.

A result may be published only with its raw report, command, commit, environment, and interpretation.

## Latency milestones

| Milestone | Meaning |
|---|---|
| Request accepted | The request passed client and API validation |
| Intent persisted | Kubernetes stored the Sandbox and durable assignment |
| Fastlet admitted | Fastlet accepted capacity and runtime identity |
| RuntimeReady | RuntimeDriver completed Ensure; Create may return |
| DataPlaneReady | Required Infra services are ready and routes are published |
| Declarative status ready | The Controller projected observed state to the CRD |

The public Create RPC measures through RuntimeReady. It does not wait for DataPlaneReady or CRD status projection.

## Required benchmark dimensions

Record:

- commit SHA and exact command;
- CPU, memory, storage, virtualization, kernel, Kubernetes, and containerd;
- component replica counts;
- total requests, concurrency, and request rate;
- runtime and InfraProfile;
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
  --commit "$(git rev-parse HEAD)" \
  --environment '8c/32GiB, Linux kernel X, kind X, containerd X' \
  --runtime container \
  --infra-profile minimal \
  --image-state warm \
  --image-affinity hit \
  --network-slot-state clean \
  --fastpath-replicas 3 \
  --controller-replicas 2 \
  --sandbox-proxy-replicas 2 \
  >create-load-warm.json
```

Use a unique request-ID prefix for every sample. Reusing it measures idempotent replay.

The process reports cleanup separately and exits non-zero on Create failure, duplicate identity, or requested cleanup failure while still writing JSON.

## Scheduler microbenchmark

```bash
go test ./internal/controller/fastletpool -run '^$' \
  -bench '^BenchmarkRegistryTopK1000$' -benchmem -count=5
```

Use it only to compare Registry implementations on the same machine and commit range. Retain raw output and use `benchstat`. It excludes Kubernetes, Fastlet admission, runtime/network creation, Infra readiness, and routing.

## Metrics

Relevant series include:

- `fast_sandbox_create_accepted_latency_seconds`;
- `fast_sandbox_create_runtime_ready_latency_seconds`;
- `fast_sandbox_runtime_create_latency_seconds`;
- `fast_sandbox_user_process_start_latency_seconds`;
- `fast_sandbox_data_plane_ready_latency_seconds`;
- `fast_sandbox_registry_heartbeat_age_seconds`;
- `fast_sandbox_registry_candidate_count`;
- `fast_sandbox_image_affinity_result_total`;
- `fast_sandbox_topk_retry_total`;
- `fast_sandbox_fastlet_admission_total`;
- `fast_sandbox_network_slot_available`;
- `fast_sandbox_network_slot_inuse`;
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
