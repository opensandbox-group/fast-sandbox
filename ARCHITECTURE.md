# Fast Sandbox Architecture

> **Superseded target architecture:** This file documents the pre-refactor implementation. Use [the 2026-07-19 cross-cutting architecture decisions](docs/superpowers/specs/2026-07-19-fast-sandbox-cross-cutting-architecture-decisions.md) as the source of truth for new development.

## 1. Overview

**Fast Sandbox** is a Kubernetes-based high-performance sandbox management system. The core objective is to provide millisecond-scale container startup latency for scenarios sensitive to startup delay, such as serverless functions and code sandbox execution.

The core design philosophy is: **Fast-Path First** + **Resource Pooling** + **Image Affinity Scheduling**.

## 2. Core Architecture

The system uses a **Controller-Fastlet** separation architecture built on Kubernetes.

![ARCHITECTURE](ARCHITECTURE.png)

### 2.1 Communication Channels

| Channel | Protocol | Purpose |
|---------|----------|---------|
| **CLI → Controller** | gRPC | Fast-Path API for <50ms latency |
| **Controller → Fastlet** | HTTP | Sandbox create/delete requests |
| **CLI → Fastlet** | HTTP (tunneled) | Log streaming, future exec |
| **Control Plane** | K8s CRD | Persistent storage and eventual consistency |

## 3. Core Components

### 3.1 Fast-Path Server (gRPC)

**Location**: `internal/controller/fastpath/server.go`

**Port**: `9090`

**Services**:
- `CreateSandbox` - Fast sandbox creation
- `DeleteSandbox` - Fast sandbox deletion
- `UpdateSandbox` - Update sandbox config (expiry, restart, policy)
- `ListSandboxes` - List sandboxes in namespace
- `GetSandbox` - Get sandbox details

**Consistency Modes**:
- **FAST** (default): Fastlet creates first → async CRD write. Latency <50ms
- **STRONG**: Write CRD (Pending) → Watch triggers → Fastlet creates. Latency ~200ms

### 3.2 Registry (In-Memory State)

**Location**: `internal/controller/fastletpool/registry.go`

**Responsibilities**:
- Maintain real-time Fastlet status (capacity, allocated, images, ports)
- Atomic allocation with mutex locks
- Image affinity scoring (prefers fastlets with cached images)

**Allocation Algorithm**:
1. Filter candidates by pool, namespace, capacity, port conflicts
2. Score candidates: `score = allocated + (no_image ? 1000 : 0)`
3. Select lowest score (image hit wins ties)

**Performance**: ~1.3ms for 100 fastlets, ~14ms for 1000 fastlets

### 3.3 SandboxController

**Location**: `internal/controller/sandbox_controller.go`

**Responsibilities**:
- CRD state machine management
- Finalizer resource cleanup
- Status synchronization with Registry
- Failure policy handling (Manual/AutoRecreate)

**State Transitions**:
```
Pending → Creating → Running → Deleting → Gone
                ↓               ↓
             Failed         Lost
```

### 3.4 SandboxPoolController

**Location**: `internal/controller/sandboxpool_controller.go`

**Responsibilities**:
- Manage Fastlet Pod lifecycle (Min/Max capacity)
- Inject privileged configuration for Containerd access
- Maintain Registry state via heartbeats

### 3.5 Fastlet (Data Plane)

**Location**: `internal/fastlet/`

**Components**:
- **Sandbox Manager**: Lifecycle management (create/delete/status)
- **Containerd Runtime**: Direct host containerd socket integration
- **HTTP Server**: API endpoints on port `5758`

**HTTP Endpoints**:
```
POST /api/v1/fastlet/create
POST /api/v1/fastlet/delete
GET  /api/v1/fastlet/status
GET  /api/v1/fastlet/logs?follow=true
```

**Key Features**:
- Host containerd integration for zero-pull startup
- Log persistence to host filesystem for streaming
- Graceful shutdown with SIGTERM → SIGKILL flow

### 3.6 Node Janitor

**Location**: `internal/janitor/`

**Responsibilities**:
- Scan for orphan containers (no matching CRD)
- Cleanup when Fastlet Pod disappears
- Remove FIFO files and containerd snapshots

**Orphan Detection Criteria**:
1. Fastlet Pod disappeared (UID not in pod lister)
2. Sandbox CRD not found
3. UID mismatch between container and CRD

**Protection Window**: 10 seconds (configurable) for Fast-Path async CRD writes

### 3.7 CLI (fastctl)

**Location**: `cmd/fastctl/`

**Features**:
- Interactive YAML editing for sandbox creation
- Auto port-forward tunneling to Fastlet Pods
- Streaming log viewing
- Configuration layers: Flags > File > Interactive

## 4. Key Workflows

### 4.1 Create Sandbox (Fast Mode)

```
User                    Controller                  Fastlet
  │                         │                         │
  ├─ run my-sb ────────────>│                         │
  │                         │                         │
  │                         ├─ Allocate() ──────────>│
  │                         │  (Registry selects)     │
  │                         │<────────────────────────┤
  │                         │  (Fastlet selected)       │
  │                         │                         │
  │                         ├─ HTTP POST /create ───>│
  │                         │                         │
  │                         │                         ├─ containerd.Create()
  │                         │                         │
  │                         │<────────────────────────┤
  │                         │  (ContainerID)          │
  │                         │                         │
  │<────────────────────────┤                         │
  │  (Success, Endpoints)   │                         │
  │                         │                         │
  │                         ├─ async: Create CRD ────>│ (K8s)
```

**Latency Breakdown**:
- Registry Allocate: ~1.3ms (100 fastlets)
- Fastlet HTTP RPC: ~10-30ms
- Containerd create: <10ms (cached image)
- **Total**: <50ms

### 4.2 Create Sandbox (Strong Mode)

```
User                    Controller              K8s                 Fastlet
  │                         │                    │                    │
  ├─ run my-sb ────────────>│                    │                    │
  │                         │                    │                    │
  │                         ├─ Create CRD ───────>│                    │
  │                         │  (Phase: Pending)   │                    │
  │                         │<────────────────────┤                    │
  │                         │                    │                    │
  │                         ├─ Allocate() ──────>│                    │
  │                         │<────────────────────┤                    │
  │                         │                    │                    │
  │                         │                    ├─ Watch trigger ───>│
  │                         │                    │                    │
  │                         ├─ HTTP POST /create ─────────────────────>│
  │                         │                    │                    │
  │                         │<─────────────────────────────────────────┤
  │                         │                    │                    │
  │                         ├─ Update CRD ──────>│                    │
  │                         │  (Phase: Running)   │                    │
  │<────────────────────────┤                    │                    │
  │  (Success)              │                    │                    │
```

**Latency**: ~200ms (dominated by ETCD + Watch)

### 4.3 Log Streaming

```
CLI                      Controller                Fastlet
  │                         │                      │
  ├─ logs my-sb ───────────>│                      │
  │                         │                      │
  │<─ Fastlet Pod IP ──────────┤                      │
  │                         │                      │
  ├─ kubectl port-forward ──────────────────────────>│
  │                         │                      │
  ├─ GET /api/v1/fastlet/logs?follow=true ────────────>│
  │<─ Chunked log stream ─────────────────────────────┤
```

## 5. Configuration

### 5.1 Controller Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--fastlet-port` | `5758` | Fastlet HTTP server port |
| `--metrics-bind-address` | `:9091` | Prometheus metrics endpoint |
| `--health-probe-bind-address` | `:5758` | Health check endpoint |
| `--fastpath-consistency-mode` | `fast` | Consistency mode: fast/strong |
| `--fastpath-orphan-timeout` | `10s` | Fast mode orphan cleanup timeout |

### 5.2 Fastlet Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `FASTLET_CAPACITY` | `5` | Max sandboxes per fastlet |
| `CONTAINERD_SOCKET` | `/run/containerd/containerd.sock` | Containerd socket path |

### 5.3 Sandbox CRD Spec

```yaml
spec:
  image: string              # Container image
  poolRef: string            # Target pool name
  exposedPorts: []int32      # Ports to expose
  command: []string          # Entrypoint command
  args: []string             # Command arguments
  envs: map[string]string    # Environment variables
  workingDir: string         # Working directory
  consistencyMode: fast|strong  # Consistency mode
  failurePolicy: manual|autoRecreate  # Failure recovery
  expireTimeSeconds: int64   # Optional expiration
```

## 6. Horizontal Scaling Considerations

### Current Limitation

The Fast-Path gRPC service runs on the Controller with an in-memory Registry, which must be a singleton to avoid allocation conflicts. This limits horizontal scalability.

### Considered Approaches

We have explored two architectural approaches for multi-replica deployment:

1. **Leader-Follower with Read-Write Separation**: One Leader handles CreateSandbox (requires Registry), Followers handle read operations and forward CreateSandbox to Leader. See [Leader-Follower HA Design](docs/plans/2025-02-09-leader-follower-ha-design.md).

2. **Controller Sharding with Client-Side Routing**: Each Pool is bound to a specific Controller, clients maintain a routing table. See [Controller Sharding Design](docs/plans/2025-02-09-controller-sharding-design.md).

### Recommendation

For large-scale production deployments requiring horizontal scalability, we recommend **application-level sharding** (e.g., separate Controller deployments per team/environment) rather than implementing complex intra-cluster sharding. This keeps the architecture simple while providing isolation.

---

## 7. Logging

Fast Sandbox uses [klog](https://github.com/kubernetes/klog), the Kubernetes ecosystem's standard logging library.

### Log Levels

| Level | Usage |
|-------|-------|
| `klog.InfoS()` | Informational messages |
| `klog.ErrorS()` | Errors (always logged) |
| `klog.V(2).InfoS()` | Verbose info (enable with `-v=2`) |
| `klog.V(4).InfoS()` | Debug info (enable with `-v=4`) |

### Enable Debug Logging

```bash
# Controller
./bin/controller -v=2

# Fastlet
./bin/fastlet -v=4

# CLI
fastctl -v=4 list
```
