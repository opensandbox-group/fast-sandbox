# Fast Sandbox

Fast Sandbox is a Kubernetes-native runtime plane for low-latency, isolated sandboxes. It keeps Fastlet Pods warm, creates multiple Sandbox runtimes inside each Fastlet, and preserves declarative lifecycle state in Kubernetes CRDs.

The current architecture separates the multi-active request path from leader-elected reconciliation. It also separates lifecycle control from user data protocols: Fast Sandbox resolves and authenticates a route to an injected Infra Component, while the component's upstream SDK owns exec and file semantics.

Chinese documentation: [README_ZH.md](README_ZH.md). Detailed component and workflow documentation: [ARCHITECTURE.md](ARCHITECTURE.md).

## Architecture at a glance

| Deployment unit | Availability model | Responsibility |
|---|---|---|
| `fastctl` / Go / Python SDK | Client | Lifecycle calls, diagnostics, endpoint resolution, upstream SDK hand-off |
| Fast-Path Server | Multi-active Deployment | CRD-first imperative `CreateSandbox`, idempotency, Top-K placement, route credentials |
| Sandbox/Pool Controller | Leader-elected Deployment | Declarative reconciliation, Pool scaling, delete/reset/expiry, failure policy |
| Sandbox Proxy | Multi-active Deployment | Authenticated, transparent HTTP/streaming proxy to the assigned Fastlet |
| Fastlet Pod | Pool-managed Pod | Atomic admission, runtime creation, private networking, Infra injection, local proxy |
| NodeJanitor | Per-node DaemonSet | Cleanup of orphan containerd, network, Infra, and BoxLite resources |

The `controller` binary supports three roles:

- `--role=fastpath`: serves gRPC without leader election; every replica is active.
- `--role=controller`: runs Sandbox and SandboxPool reconcilers with leader election.
- `--role=all`: single-process development mode.

Only Create is an imperative fast-path operation. Delete, reset, expiry, and failure-policy updates change the Sandbox CRD and are completed by reconciliation. Creating a Sandbox CRD directly therefore remains fully supported when Fast-Path is not deployed.

## Core properties

- **Bounded multi-active Create**: a stable `request_id` plus Kubernetes persistence makes retries idempotent; Fastlet performs the final atomic `maxSandboxesPerPod` admission.
- **Watch + heartbeat scheduling**: every control-plane replica builds a local Registry from Kubernetes watches and low-frequency jittered Fastlet heartbeats. Top-K selection considers available slots and image-cache affinity; stale candidates fail fast and retry within a bounded list.
- **Private Sandbox networks**: container-based runtimes receive an independent network namespace, veth, private address, and NAT egress. Every Sandbox can use the full private port space without global port reservation.
- **Authenticated two-hop proxy**: Sandbox Proxy resolves `Sandbox UID -> Fastlet Pod`; Fastlet Proxy resolves the runtime-local AccessHandle. Credentials are fenced by Fastlet Pod UID, assignment attempt, and route generation.
- **Runtime profiles**: a Pool selects one immutable `runtime`: `container`, `gvisor`, `kata-qemu`, `kata-clh`, `kata-fc`, or `boxlite`.
- **Runtime Augmentation**: platform-owned `sandbox-init`, binaries, configuration, tokens, and readiness rules are injected without rebuilding the user's OCI image. OpenSandbox Execd is the primary integration case; production artifact binding remains fail closed until supplied by the release.
- **Fixed Pool resources**: every Sandbox in a Pool uses the same immutable CPU, memory, and PID profile; Fastlet/runtime adapters are the enforcement boundary.
- **Fenced recovery**: CRD UID, instance generation, assignment attempt, Fastlet Pod UID, and route generation prevent stale runtime and proxy operations.

## Quick start (kind)

Quick Start is a reproducible kind acceptance path and must run on a Linux host. Install Go, Docker, kind, kubectl, and make first. Container, network, and secure-runtime behavior must not be validated locally on macOS; see [docs/TESTING.md](docs/TESTING.md).

### Prepare a container environment

```bash
make quickstart
```

This is an alias for `make quickstart-container`. It only prepares and retains an interactive environment:

- create or reuse the `fsb-e2e-basic` kind cluster;
- build and load the Controller, Fastlet, Proxy, and Janitor images from the current source tree;
- deploy the development control plane;
- create `quickstart-pool` and wait for a Ready Fastlet;
- build `bin/fastctl`;
- print the port-forward and Sandbox creation commands.

`make quickstart` does not run `go test`, create a Sandbox automatically, or clean up the Pool or kind cluster when it exits. The development manifests contain a public test signing key and must not be used in production.

### Use Fast-Path interactively

After the environment is ready, expose the in-cluster Fast-Path to the host in one terminal:

```bash
kubectl port-forward service/fast-sandbox-fastpath 9090:9090
```

Create and inspect a Sandbox from another terminal:

```bash
fastctl --endpoint localhost:9090 run my-sandbox \
  --image docker.io/library/alpine:latest \
  --pool quickstart-pool -- /bin/sleep 3600

kubectl wait --for=jsonpath='{.status.runtimeState}'=Ready \
  sandbox/my-sandbox --timeout=60s
fastctl --endpoint localhost:9090 get my-sandbox
fastctl --endpoint localhost:9090 diagnostics sandbox my-sandbox
fastctl --endpoint localhost:9090 delete my-sandbox
```

`fast-sandbox-fastpath.default.svc` is an in-cluster DNS name and cannot be resolved by a host-side `fastctl`. Host access requires a port-forward, Ingress, LoadBalancer, or another explicit exposure mechanism.

Fast-Path Create returns after the runtime is created, while the Controller projects CRD `status` asynchronously. An immediate Get may therefore briefly show `Creating/Pending`; the `kubectl wait` above waits for the declarative view to catch up.

### Prepare other runtimes

These entry points reuse the kind profile provisioner but do not execute E2E cases:

```bash
make quickstart-container
make quickstart-gvisor
make quickstart-kata-qemu
make quickstart-kata-clh
```

- `container` prepares `fsb-e2e-basic` and `quickstart-pool`.
- `gVisor` prepares `fsb-e2e-gvisor`, installs and verifies runsc, and creates `gvisor-pool`.
- `kata-qemu` and `kata-clh` prepare `fsb-e2e-kata`, require nested KVM on the host, and create `kata-qemu-pool` and `kata-clh-pool`, respectively.

Kata Firecracker and BoxLite do not have Quick Start entry points because they are not currently runnable capabilities. Their fail-closed behavior remains covered by `test-e2e-runtime-kata` and `test-e2e-runtime-boxlite`. All Quick Start targets retain reusable kind clusters and Pools. The first run builds images and prepares runtimes, so it takes substantially longer than later runs.

Automated acceptance remains separate from Quick Start:

```bash
make test-e2e-runtime-container
make test-e2e-runtime-gvisor
make test-e2e-runtime-kata
make test-e2e-runtime-boxlite
```

### Declarative API

The Controller-only path does not depend on Fast-Path Create:

```yaml
apiVersion: sandbox.fast.io/v1alpha1
kind: Sandbox
metadata:
  name: my-declarative-sandbox
spec:
  image: docker.io/library/alpine:latest
  poolRef: quickstart-pool
  command: ["/bin/sleep"]
  args: ["3600"]
  failurePolicy: Manual
```

```bash
kubectl apply -f sandbox.yaml
kubectl get sandbox my-declarative-sandbox -w
```

### OpenSandbox Execd

The basic Quick Start uses `infraProfile: minimal` and does not pretend that OpenSandbox Execd is injected. See the [OpenSandbox Execd integration guide](docs/opensandbox-execd-integration-guide.md) for production artifact/profile binding, Sandbox Proxy forwarding, and `fastctl opensandbox exec/cp/files`.

Fast Sandbox intentionally does not define a new Exec/File wire protocol. User-process execution, logs, and files belong to the injected component's protocol.

## API contracts

The FastPath gRPC service exposes:

- `CreateSandbox`, `DeleteSandbox`, `UpdateSandbox`, `ListSandboxes`, `GetSandbox`, `GetSandboxDiagnostics`
- `ResolveEndpoint`, `IssueRouteCredential`

Create callers must send a stable `request_id`; it is also the canonical Sandbox CRD name. `fastctl diagnostics sandbox NAME` reports CRD state and bounded Fastlet lifecycle events independently of Execd. The API accepts only the canonical contracts documented above; pre-refactor field names are not part of the schema. Metrics, trace propagation, lifecycle identity fields, and OTLP configuration are documented in [docs/observability.md](docs/observability.md).

## Validation

```bash
make verify
make test-race
make test-python-sdk
```

Focused Linux/Kubernetes gates include `test-network-integration`, `test-e2e-controlplane`, `test-e2e-proxy`, `test-e2e-infra`, `test-e2e-sdk`, and per-runtime capability targets listed by `make help`.

## Current scope and limitations

- A Sandbox is bound to a Fastlet Pod. If that Pod disappears, the active Sandbox instance is lost; `AutoRecreate` may create a new instance according to policy.
- Snapshot, pause/resume, and persistent Sandbox storage are intentionally outside this refactor.
- Kata Firecracker remains capability-gated as `KataFirecrackerNotValidated`; QEMU and Cloud Hypervisor are the currently verified Kata profiles.
- BoxLite lifecycle, Infra injection, authenticated local forwarding, and cleanup are integrated. BoxLite v0.9.7 does not provide an unescapable host resource-enforcement contract, so BoxLite Pools fail the resource-capability gate rather than claiming production support.
- `<50ms` is an observed target only for a warm container profile, not a promise for cold images, Kata, BoxLite, Infra readiness, or the full data-plane route.

## Design documents

- [Cross-cutting architecture decisions](docs/superpowers/specs/2026-07-19-fast-sandbox-cross-cutting-architecture-decisions.md)
- [Multi-active Fast-Path control plane](docs/superpowers/specs/2026-07-18-multi-active-fastpath-control-plane-design.md)
- [Fastlet network architecture](docs/superpowers/specs/2026-05-05-fastlet-network-architecture-design.md)
- [Control/data-plane separation and Infra injection](docs/superpowers/specs/2026-07-19-control-data-plane-separation-design.md)
- [Runtime abstraction](docs/superpowers/specs/2026-07-19-sandbox-runtime-abstraction-design.md)
- [Implementation plan and verification log](docs/superpowers/plans/2026-07-19-fast-sandbox-architecture-refactor-implementation-plan.md)
- [Architecture refactor acceptance report](docs/release-acceptance-report.md)

## License

[MIT](LICENSE)
