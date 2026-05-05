# Fast Sandbox

Fast Sandbox is a high-performance, cloud-native (Kubernetes-native) sandbox management system designed to provide **millisecond-scale cold container startup** and **controlled self-healing** capabilities for AI Agents, Serverless functions, and compute-intensive tasks.

By pre-warming "Fastlet Pod" resource pools and directly integrating with host-level container management, Fast Sandbox bypasses the significant overhead of traditional Kubernetes Pod creation, achieving ultra-fast task distribution with physical isolation.

## Features

- **Fast-Path API**: gRPC-based fast path supporting **<50ms** end-to-end startup latency. Dual-mode switching between **Fast Mode** (Fastlet-First, ultra-fast) and **Strong Mode** (CRD-First, strong consistency).
  <img width="480" height="97" alt="image" src="https://github.com/user-attachments/assets/c37c3a99-59a3-469b-9797-c6fe5530cfa9" />
- **Developer CLI (`fastctl`)**: Docker-like command-line experience with interactive creation, configuration management, streaming log viewing (`logs -f`), and status queries.
- **Zero-Pull Startup**: Leverages **Host Containerd Integration** to launch microcontainers directly on the host, reusing node image cache.
- **Smart Scheduling**: Allocation algorithm based on **Image Affinity** and **Atomic Slots**, eliminating image pull latency and avoiding port conflicts.
- **Resilient Design**:
  - **Controlled Self-Healing**: Supports `AutoRecreate` policy and manual `resetRevision`.
  - **Graceful Shutdown**: Complete SIGTERM → SIGKILL flow preventing zombie processes.
  - **Node Janitor**: Independent DaemonSet for automatic orphan container and file cleanup.

## Architecture

The system uses a "centralized control plane decision, extreme data plane execution" architecture:
![ARCHITECTURE](ARCHITECTURE.png)

### Control Plane
- **Fast-Path Server (gRPC)**: Handles high-concurrency sandbox create/delete requests, direct CLI access
  - Port: `9090`
  - Services: `CreateSandbox`, `DeleteSandbox`, `UpdateSandbox`, `ListSandboxes`, `GetSandbox`
- **SandboxController**: Manages CRD state machine, Finalizer resource cleanup, and dual-mode consistency coordination
- **SandboxPoolController**: Manages Fastlet Pod resource pools (Min/Max capacity)
- **Atomic Registry**: In-memory state center supporting high-concurrency mutex allocation and image weight scoring

### Data Plane (Fastlet)
- Privileged Pods running on hosts, communicating via HTTP with the control plane
- **Runtime Integration**: Direct Containerd Socket access for container lifecycle and **log persistence**
- **HTTP Server**: Listens on port `5758`
  - `POST /api/v1/fastlet/create` - Create sandbox
  - `POST /api/v1/fastlet/delete` - Delete sandbox
  - `GET /api/v1/fastlet/status` - Get fastlet status
  - `GET /api/v1/fastlet/logs?follow=true` - Stream logs

### Tooling
- **fastctl**: Developer CLI with `run`, `list`, `get`, `logs`, `delete` commands

## Quick Start

### 1. Install CLI

```bash
make build
# Generates bin/fastctl
export PATH=$PWD/bin:$PATH
```

### 2. Create a Sandbox (Interactive)

```bash
fastctl run my-sandbox
# Opens editor for configuration (image, ports, command, env)
```

### 3. View Real-time Logs

```bash
fastctl logs my-sandbox -f
```

### 4. Declarative YAML

You can also use Kubernetes CRD directly:

```yaml
apiVersion: sandbox.fast.io/v1alpha1
kind: Sandbox
metadata:
  name: my-sandbox
  namespace: default
spec:
  image: alpine:latest
  exposedPorts: [8080]
  poolRef: default-pool
  consistencyMode: fast  # or strong
  failurePolicy: AutoRecreate
```

## Consistency Modes

### Fast Mode (Default)
1. CLI → Controller gRPC request
2. Registry allocates Fastlet
3. Controller → Fastlet HTTP create request
4. Fastlet starts container via Containerd
5. Controller returns success to CLI
6. Controller *async* creates K8s CRD

**Latency**: <50ms
**Trade-off**: CRD creation failure may cause orphan (cleaned by Janitor)

### Strong Mode
1. CLI → Controller gRPC request
2. Controller creates K8s CRD (Pending phase)
3. Controller Watch triggers
4. Controller → Fastlet HTTP create request
5. Fastlet starts container
6. CRD status updated to Running

**Latency**: ~200ms
**Guarantee**: Strong consistency, no orphans

## Configuration

### Controller Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--fastlet-port` | `5758` | Fastlet HTTP server port |
| `--metrics-bind-address` | `:9091` | Prometheus metrics endpoint |
| `--health-probe-bind-address` | `:5758` | Health check endpoint |
| `--fastpath-consistency-mode` | `fast` | Consistency mode: fast or strong |
| `--fastpath-orphan-timeout` | `10s` | Fast mode orphan cleanup timeout |

### Fastlet Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--containerd-socket` | `/run/containerd/containerd.sock` | Containerd socket path |
| `--http-port` | `5758` | HTTP server port |

### Environment Variables

| Variable | Description |
|----------|-------------|
| `FASTLET_CAPACITY` | Max sandboxes per fastlet (default: 5) |

## gRPC API

```protobuf
service FastPathService {
  rpc CreateSandbox(CreateRequest) returns (CreateResponse);
  rpc DeleteSandbox(DeleteRequest) returns (DeleteResponse);
  rpc UpdateSandbox(UpdateRequest) returns (UpdateResponse);
  rpc ListSandboxes(ListRequest) returns (ListResponse);
  rpc GetSandbox(GetRequest) returns (SandboxInfo);
}
```

### ConsistencyMode
- `FAST`: Create container first, async CRD write
- `STRONG`: Write CRD first, then create container

### FailurePolicy
- `MANUAL`: Report status only, no auto-recovery
- `AUTO_RECREATE`: Automatically reschedule on failure

## Development

### Running Tests

```bash
# All tests
go test ./... -v

# With coverage
go test ./... -coverprofile=coverage.out

# Specific module
go test ./internal/controller/fastletpool/ -v
```

See [docs/TESTING.md](docs/TESTING.md) for detailed testing documentation.

### Performance Profiling

```bash
# CPU profiling
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30 > cpu.prof

# View profile
go tool pprof -http=:8080 cpu.prof
```

See [docs/PERFORMANCE.md](docs/PERFORMANCE.md) for performance analysis.

## Roadmap

- [x] Phase 1: Core Runtime (Containerd) & gRPC framework
- [x] Phase 2: Fast-Path API & Registry scheduling
- [x] Phase 3: CLI (`fastctl`) & interactive experience
- [x] Phase 4: Log streaming & auto tunneling
- [x] Phase 5: Unified logging (klog)
- [x] Phase 6: Performance instrumentation & unit tests
- [ ] Phase 7: Supports custom volume mounting.
- [ ] Phase 8: Container checkpoint/restore (CRIU)
- [ ] Phase 9: Web console & traffic proxy
- [ ] Phase 10: gVisor support for secure sandboxing
- [ ] Phase 11: CLI exec bash & Python SDK (Modal-like)
- [ ] Phase 12: GPU container support

## License

[MIT](LICENSE)
