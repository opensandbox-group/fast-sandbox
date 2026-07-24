# Fast Sandbox

Create isolated container, gVisor, and Kata sandboxes from warm runtime pools inside Kubernetes.

[Chinese](README_ZH.md) | [Quick Start](docs/getting-started/quickstart.md) | [Documentation](docs/README.md) | [Architecture](docs/concepts/architecture.md)

![Fast Sandbox system overview](docs/assets/system-overview.svg)

Fast Sandbox is a runtime plane for workloads that need many short-lived, isolated execution environments. A warm Fastlet Pod hosts multiple independent Sandbox runtimes, while Kubernetes CRDs preserve lifecycle intent and status.

The imperative Create path is optimized for latency. Delete, reset, expiry, recovery, and Pool management remain declarative. Exec and file protocols belong to injected Infra Components such as OpenSandbox Execd.

## Why Fast Sandbox

- **Warm runtime pools** reuse prepared Fastlet Pods instead of creating one Kubernetes Pod for every Sandbox request.
- **Multiple isolation profiles** select container, gVisor, Kata QEMU, or Kata Cloud Hypervisor through one immutable Pool field.
- **Private Sandbox networking** gives each instance a private address space and NAT egress without global host-port allocation.
- **Multi-active Create** uses Kubernetes persistence for idempotency and Fastlet atomic admission for final capacity enforcement.
- **Protocol-neutral data plane** injects, discovers, authenticates, and transparently proxies an Infra Component's native service.
- **Kubernetes-native lifecycle** works through CRDs even when the optional Fast-Path deployment is absent.

## Quick Start

Quick Start prepares an interactive kind environment on a Linux host. It does not run an E2E suite or create a Sandbox automatically.

```bash
make quickstart
```

Keep the endpoints exposed in terminal 1:

```bash
make quickstart-forward
```

In terminal 2:

```bash
bin/fastctl --endpoint localhost:9090 \
  --proxy-endpoint http://localhost:18080 \
  run quickstart-execd-sandbox \
  --image docker.io/library/alpine:latest \
  --pool quickstart-execd-pool -- /bin/sleep 3600

kubectl wait --for=jsonpath='{.status.dataPlaneState}'=Ready \
  sandbox/quickstart-execd-sandbox --timeout=60s

bin/fastctl --endpoint localhost:9090 \
  --proxy-endpoint http://localhost:18080 \
  opensandbox exec quickstart-execd-sandbox -- uname -a

bin/fastctl --endpoint localhost:9090 delete quickstart-execd-sandbox
```

Select another runtime with:

```bash
make quickstart RUNTIME=gvisor
make quickstart RUNTIME=kata-qemu
make quickstart RUNTIME=kata-clh
```

See the [full Quick Start](docs/getting-started/quickstart.md) for file transfer, diagnostics, declarative CRD creation, and troubleshooting.

## Architecture

The control plane has two explicit roles:

- **Fast-Path Servers** are multi-active and serve idempotent CRD-first Create.
- **Reconcilers** are leader-elected and converge Sandbox and SandboxPool state.

Fastlet is the node-side runtime boundary:

```text
Fastlet
  -> RuntimeDriver
       -> containerd + runc
       -> containerd + gVisor/runsc
       -> Kata QEMU
       -> Kata Cloud Hypervisor
       -> Kata Firecracker [capability-gated]
       -> BoxLite sidecar [experimental, fail closed]
```

User data follows a separate path:

```text
Upstream SDK
  -> Sandbox Proxy
  -> Fastlet Proxy
  -> Sandbox private network
  -> Infra Component
```

| Deployment unit | Availability | Responsibility |
|---|---|---|
| Fast-Path Server | Multi-active Deployment | Create, local Registry, Top-K placement, route credentials |
| Sandbox/Pool Reconcilers | Leader-elected Deployment | Declarative lifecycle, Pool scaling and drain, recovery |
| Sandbox Proxy | Multi-active Deployment | Authenticated transparent HTTP and streaming proxy |
| Fastlet Pod | Pool-managed Pod | Atomic admission, runtime/network/Infra orchestration, local proxy |
| NodeJanitor | Per-node DaemonSet | Fenced orphan cleanup |

Read [Architecture](docs/concepts/architecture.md) and [Control plane](docs/concepts/control-plane.md) for the complete model.

## Runtime status

| Runtime | Pool value | Quick Start | Fast Sandbox status |
|---|---|---:|---|
| OCI container | `container` | Yes | Validated |
| gVisor | `gvisor` | Yes | Validated |
| Kata QEMU | `kata-qemu` | Yes | Validated |
| Kata Cloud Hypervisor | `kata-clh` | Yes | Validated |
| Kata Firecracker | `kata-fc` | No | Capability-gated |
| BoxLite | `boxlite` | No | Experimental integration; fail closed |

The table describes Fast Sandbox validation status, not the upstream runtimes' general capabilities.

## Fast Sandbox and Agent Sandbox

[Kubernetes SIGs Agent Sandbox](https://github.com/kubernetes-sigs/agent-sandbox) and Fast Sandbox solve adjacent problems with different workload units.

| | Fast Sandbox | Agent Sandbox |
|---|---|---|
| Primary abstraction | Runtime instance inside a warm Fastlet Pod | Stateful singleton Sandbox Pod |
| Warm capacity | One Fastlet hosts multiple runtimes | `SandboxWarmPool` prepares Sandbox Pods |
| Main focus | High-density runtime creation and a separate data plane | Stable Pod identity, persistence, and hibernation workflows |

This is an architectural comparison, not a performance claim.

## Performance semantics

Create returns at `RuntimeReady`. Required Infra services and route publication continue asynchronously until `DataPlaneReady`.

Fast Sandbox does not publish an unqualified headline latency. Results must record the commit, environment, runtime, cache state, concurrency, measurement boundary, and percentile distribution. See [Performance](docs/guides/performance.md).

## Current scope

- A Sandbox instance is bound to one Fastlet Pod. Pod loss destroys that instance; `AutoRecreate` may create a new generation.
- Snapshot, pause/resume, persistent storage, and live migration are not current capabilities.
- Kata Firecracker and BoxLite remain explicit capability gates.
- The development Execd profile uses public test material; production deployments must bind a trusted artifact supply chain.

## Documentation

- [Documentation index](docs/README.md)
- [Architecture](docs/concepts/architecture.md)
- [Runtime model](docs/concepts/runtimes.md)
- [Private networking](docs/concepts/networking.md)
- [Infra Components](docs/concepts/infra-components.md)
- [Deployment](docs/guides/deployment.md)
- [Testing](docs/guides/testing.md)
- [API reference](docs/reference/api.md)

## License

[MIT](LICENSE)
