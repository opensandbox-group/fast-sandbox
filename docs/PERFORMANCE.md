# Fast Sandbox Performance Analysis

## Critical Path Latency Breakdown

Target: <50ms end-to-end for FastPath mode

### CLI -> Controller (FastPath gRPC)
- Expected: 1-5ms
- Measured: TBD

### Registry.Allocate
- Expected: <1ms
- Measured: ~1.3ms (from benchmarks)
- Sub-operations (100 fastlets):
  - Candidate filtering: <0.5ms
  - Scoring: <0.5ms
  - Selection: <0.5ms
- Large pool (1000 fastlets): ~14ms

### Fastlet.CreateSandbox RPC
- Expected: 5-20ms
- Measured: TBD

### containerd Runtime
- Expected: 10-30ms
- Measured: TBD
- Sub-operations:
  - Image pull (cached): 0ms
  - Container create: TBD
  - Container start: TBD

## Benchmark Results

### Registry Allocation (Baseline)

**Test Environment:**
- CPU: Apple M4 Pro
- OS: darwin/arm64
- Date: 2026-01-26

| Benchmark | ns/op | B/op | allocs/op | Notes |
|-----------|-------|------|-----------|-------|
| BenchmarkRegistryAllocate | 1312 | 993 | 4 | Standard allocation (100 fastlets) |
| BenchmarkRegistryAllocateWithPorts | 1469 | 993 | 4 | With port constraints |
| BenchmarkRegistryAllocateNoImageMatch | 1349 | 993 | 4 | No pre-image match |
| BenchmarkRegistryAllocateLargePool | 14613 | 8297 | 4 | Large pool (1000 fastlets) |
| BenchmarkRegistryRegisterOrUpdate | 127.9 | 91 | 4 | Fastlet registration |
| BenchmarkRegistryGetAllFastlets | 4810 | 19328 | 2 | Get all fastlets (100) |
| BenchmarkRegistryGetAllFastletsLargePool | 51290 | 188419 | 2 | Get all fastlets (1000) |
| BenchmarkRegistryGetFastletByID | 27.06 | 0 | 0 | Map lookup - zero alloc |
| BenchmarkRegistryRelease | 14.49 | 0 | 0 | Fastlet release - zero alloc |
| BenchmarkRegistryCleanupStaleFastlets | 38.58 | 0 | 0 | Stale cleanup - zero alloc |
| BenchmarkParallelAllocate | 1513 | 1016 | 6 | Concurrent allocation |

Run benchmarks with:
```bash
go test ./internal/controller/fastletpool/ -bench=. -benchmem
```

## Profiling

### CPU Profiling

Start controller with profiling:
```bash
./scripts/profile.sh
```

Capture 30-second profile:
```bash
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30 > /tmp/controller_cpu.prof
```

View profile:
```bash
go tool pprof -http=:8080 /tmp/controller_cpu.prof
```

### Flamegraph

Generate flamegraph from captured profile:
```bash
./scripts/flamegraph.sh
```

## Metrics

Prometheus metrics are available for FastPath operations:

- `fastpath_create_sandbox_duration_seconds` - Histogram of CreateSandbox RPC duration
  - Labels: `mode` (fast/strong), `success` (true/false)
  - Buckets: 1ms, 5ms, 10ms, 25ms, 50ms, 100ms, 250ms, 500ms, 1s

## Timing Logs

Enable detailed timing logs with verbosity level 2:

```bash
# Controller
./bin/controller -v=2

# Fastlet
./bin/fastlet -v=2
```

This will show:
- Registry allocation timing breakdown
- Fastlet RPC call timing
- containerd Runtime timing breakdown

## Optimization Targets

1. [ ] Registry allocation - minimize lock contention (currently ~1.3ms for 100 fastlets)
2. [ ] Fastlet RPC - consider connection pooling (gRPC connection reuse)
3. [ ] containerd - ensure image cache hit (zero-pull goal)
4. [ ] Controller reconcile - optimize periodic sync interval
5. [ ] FastPath gRPC server - measure actual gRPC call overhead

## Performance Goals

| Operation | Target | Current | Status |
|-----------|--------|---------|--------|
| FastPath CreateSandbox (e2e) | <50ms | TBD | 🔍 To Measure |
| Registry.Allocate (100 fastlets) | <2ms | ~1.3ms | ✅ Pass |
| Registry.Allocate (1000 fastlets) | <20ms | ~14ms | ✅ Pass |
| Fastlet.CreateSandbox RPC | <20ms | TBD | 🔍 To Measure |
| containerd container start | <30ms | TBD | 🔍 To Measure |

## Debugging Performance Issues

If performance degrades:

1. **Run benchmarks** to detect regression:
   ```bash
   go test ./internal/controller/fastletpool/ -bench=. -benchmem
   ```

2. **Capture CPU profile** to identify hotspots:
   ```bash
   go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30 > /tmp/cpu.prof
   go tool pprof -list Allocate /tmp/cpu.prof
   ```

3. **Check logs** with `-v=2` to see timing breakdown

4. **Check metrics** at `:9091/metrics` for Prometheus histograms
