# Fast Sandbox performance contract

Fast Sandbox reports latency by milestone and workload profile. A single end-to-end number hides image pull, VM boot, Infra readiness, and route publication, so results must always identify the measured boundary.

## Milestones

| Milestone | Meaning |
|---|---|
| Create accepted | An existing idempotent request was found, or a Fastlet atomically accepted a reservation |
| Runtime created | The selected runtime adapter completed Ensure |
| User process started | The adapter proved that the original user process started; unavailable for unreported `sandbox-init` paths |
| Data plane ready | Runtime, Infra Component readiness, and Fastlet route publication completed |
| Create RPC complete | Fast-Path returned success or a terminal error |

The historical `<50ms` value is an observation target only for a warm `container` Pool with an image cache hit and minimal InfraProfile. It is not a target for cold pulls, gVisor/Kata/BoxLite, or full data-plane readiness.

## Required test dimensions

Every benchmark or load report must record:

- commit SHA and exact command;
- node/cluster hardware and Kubernetes/containerd versions;
- Fast-Path, Controller, Sandbox Proxy, and Fastlet replica counts;
- concurrent clients and request rate;
- warm/cold image and image hit/miss;
- runtime and InfraProfile;
- NetworkSlot hit/recovery state;
- Fast-Path or direct-CRD create path;
- p50, p95, p99, error count, admission rejection count, and retry count.

Do not compare profiles while changing more than one of these dimensions.

## Metrics

Important Prometheus series include:

- `fast_sandbox_create_accepted_latency_seconds{path,result}`
- `fast_sandbox_create_data_plane_ready_latency_seconds{result}`
- `fast_sandbox_runtime_create_latency_seconds{runtime,cache_hit,result}`
- `fast_sandbox_user_process_start_latency_seconds{runtime,infra_profile,source}`
- `fast_sandbox_user_process_start_observation_total{runtime,infra_profile,source,result}`
- `fast_sandbox_data_plane_ready_latency_seconds{runtime,infra_profile,result}`
- `fast_sandbox_registry_heartbeat_age_seconds{state}`
- `fast_sandbox_registry_candidate_count{state}`
- `fast_sandbox_image_affinity_result_total{result}`
- `fast_sandbox_topk_retry_total{result,reason}`
- `fast_sandbox_fastlet_admission_total{operation,result,reason}`
- `fast_sandbox_fastlet_reservation_inflight`
- `fast_sandbox_fastlet_admission_slots{state}`
- `fast_sandbox_network_slot_available` / `fast_sandbox_network_slot_inuse`
- `fast_sandbox_infra_ready_latency_seconds{profile,component,runtime,result}`
- `fast_sandbox_sandbox_proxy_route_latency_seconds{result}`
- `fast_sandbox_fastlet_proxy_upstream_latency_seconds{access,result}`

`cache_hit` is currently reported as `unknown` for runtime creation because an inventory snapshot cannot prove whether that specific create pulled or unpacked data. Do not infer a per-create hit until RuntimeDriver returns a trustworthy result.

Metrics use bounded labels. Request ID, Sandbox UID, Pod UID, assignment attempt, and route generation belong in logs/traces and must not become Prometheus labels.

## Microbenchmark

The current scheduler microbenchmark measures Top-K ranking with 1000 watched Fastlets:

```bash
go test ./internal/controller/fastletpool -run '^$' \
  -bench '^BenchmarkRegistryTopK1000$' -benchmem -count=5
```

Microbenchmark numbers are not Create latency. They exclude Fastlet admission, Kubernetes persistence, runtime, networking, Infra readiness, and proxy route readiness.

## Load and failure acceptance

A release report must demonstrate:

1. concurrent multi-active Create never exceeds Fastlet capacity and never creates duplicate runtime identity;
2. image-hit candidates are observably preferred;
3. heartbeat traffic remains bounded when Fast-Path replicas increase;
4. proxy SSE/WebSocket/file streams are not fully buffered and cancellation reaches the upstream;
5. deleting the Controller leader does not remove Fast-Path Service availability;
6. losing one Sandbox Proxy replica preserves aggregate route availability;
7. stale route credentials fail after reset, reassignment, and deletion.

## Create load report

`test/performance/create_load` drives the public FastPath gRPC API and writes one JSON report to stdout. It measures full Create RPC latency; it does not rename that value to CreateAccepted or DataPlaneReady. Use the Prometheus milestone histograms above for server-side phase boundaries.

Example warm container run through a port-forward:

```bash
go run ./test/performance/create_load \
  --endpoint 127.0.0.1:19090 \
  --namespace perf --pool perf-pool \
  --requests 100 --concurrency 10 --cleanup \
  --commit "$(git rev-parse HEAD)" \
  --environment '8c/32GiB, kind vX, containerd vY' \
  --runtime container --infra-profile minimal \
  --image-state warm --image-affinity hit --network-slot-state clean \
  --fastpath-replicas 3 --controller-replicas 2 --sandbox-proxy-replicas 2 \
  >create-load-warm.json
```

The default workload sleeps for one hour so a successful runtime remains observable. `--cleanup` submits declarative Delete calls after measurement; cleanup time is reported separately and never included in Create percentiles. The process exits non-zero on any Create failure, duplicate Sandbox UID/name, or requested cleanup failure, while still emitting the JSON report. Use a unique `--request-id-prefix` for each sample; reusing one intentionally measures idempotent replay instead of creation.

The report requires callers to state runtime, InfraProfile, image/cache state, NetworkSlot state, replica counts, commit, and environment. Values default to `unspecified` rather than being inferred from client-visible latency.

The implementation plan records exact remote verification commands and known limitations: [2026-07-19 implementation plan](superpowers/plans/2026-07-19-fast-sandbox-architecture-refactor-implementation-plan.md).

## Profiling

The Controller exposes pprof on loopback `localhost:6060`:

```bash
go tool pprof 'http://localhost:6060/debug/pprof/profile?seconds=30'
```

Profile production-like Linux runs. Local macOS profiles are suitable for pure Go scheduling analysis only and are not evidence for containerd, network, or secure-runtime performance.
