# Runtime model

A SandboxPool selects one immutable runtime name. Fast Sandbox combines the
platform-owned runtime definition with the node runtime environment selected by
the operator. The Pool Controller publishes the result as an immutable runtime
plan consumed by Fastlet.

## Public and internal abstractions

```text
SandboxPool.spec.runtime
  -> RuntimeCatalog
  -> RuntimeDefinition
  +  NodeRuntimeEnvironment
  -> ResolvedRuntimePlan
  -> RuntimeDriver
  -> backend runtime
```

- `RuntimeName` is the public Pool value.
- `RuntimeDefinition` is the code-owned behavior and capability contract.
- `NodeRuntimeEnvironment` records the installed containerd, kubelet, node
  selector, host path, and optional runtime binding.
- `ResolvedRuntimePlan` is the immutable merged contract mounted into one
  Fastlet generation.
- `RuntimeDriver` is Fastlet's runtime-neutral lifecycle interface.

Pool users cannot override containerd handlers, shim paths, runtime
configuration paths, network modes, or platform mounts independently. Runtime
environment configuration is restricted to platform administrators.

## Canonical runtime names

| Runtime name | Driver | Backend |
|---|---|---|
| `container` | containerd | `io.containerd.runc.v2` |
| `gvisor` | containerd | `io.containerd.runsc.v1` |
| `kata-qemu` | containerd | Kata shim with QEMU configuration |
| `kata-clh` | containerd | Kata shim with Cloud Hypervisor configuration |
| `kata-fc` | containerd | Kata shim with Firecracker configuration |
| `kata-dragonball` | containerd | Kata Rust shim with Dragonball configuration |
| `firecracker` | direct Firecracker | Fastlet-managed microVM restore |

The names define stable profiles, not unconditional production support. See [Runtime support](../reference/runtime-support.md).

## Runtime plan

A resolved runtime plan fixes:

- driver kind and backend configuration;
- privileged mode and host paths;
- KVM requirements;
- runtime overhead;
- network mode;
- Infra delivery modes;
- cache, recovery, and network capabilities;
- containerd socket, namespace, snapshotter and root;
- kubelet state root;
- a deterministic revision/profile hash.

The Pool Controller writes the complete plan into an immutable ConfigMap and
mounts it into Fastlet. Fastlet does not independently resolve the built-in
catalog. The revision lets Controllers, placement and Fastlets reject
incompatible generations instead of interpreting the same Pool differently.

See [Runtime environments](../guides/runtime-environments.md) for the operator
configuration and rollout behavior.

## RuntimeDriver

The runtime contract is defined in `internal/runtime/contract`. Its `Driver`
interface contains lifecycle operations:

```text
Initialize
ProbeCapabilities
EnsureSandbox
InspectSandbox
DeleteSandbox
ListManagedSandboxes
Close
```

Optional interfaces add image cache, recovery, resource admission, and access
descriptors. Fastlet assembles network and Infra dependencies around the
selected concrete driver; route publication belongs to the separate data-plane
contract.

Exec, file transfer, PTY, and user protocol operations are deliberately absent. They belong to Infra Components and their upstream SDKs.

## Container and gVisor

The container and gVisor profiles use the containerd driver and a Fastlet-owned Linux network namespace.

- `container` uses the runc v2 handler and host kernel namespace/cgroup isolation.
- `gvisor` uses the runsc handler and a user-space kernel boundary.

Both receive fixed CPU, memory, and PID limits from the Pool's Sandbox resource profile.

## Kata

Kata profiles use the containerd Kata shim. Fastlet still owns the network slot, while the shim exposes the interface to the guest.

QEMU, Cloud Hypervisor, Firecracker, and Dragonball are validated profiles.
Firecracker is bound to a runtime-specific configuration and snapshotter
override inside the node environment because it requires a compatible
`virtio-mmio` guest kernel and a block-device snapshotter rather than the
environment's default overlayfs snapshotter. Its lifecycle, networking, Infra
delivery, recovery, and cleanup paths are covered by E2E tests; its startup
latency remains especially sensitive to virtualization and storage.

Dragonball uses Kata's Rust shim and a separate Fast Sandbox compatibility
configuration. Keeping that file separate from the upstream configuration
makes upgrades and rollback explicit.

Kata supports Infra delivery through OCI bind mounts, image/template baking, preinstalled artifacts, or runtime-specific guest copy.

## Fixed Pool resources and aggregate overcommit

Every Sandbox in one Pool uses the same immutable CPU, memory, and PID limits. Fastlet passes those values to the selected RuntimeDriver and is the enforcement boundary.

By default, the Pool Controller sizes both the request and limit of the
resource-owning Fastlet from:

```text
per-Sandbox resources * maxSandboxesPerPod + runtime overhead
```

This default does not overcommit. An operator can opt into aggregate CPU or
memory overcommit by setting a lower limit on the resource-owning container in
`fastletTemplate`. For example, ten Sandboxes can each retain a `1` CPU limit
while the Fastlet runtime owner has a `5` CPU aggregate limit. Per-Sandbox
limits still isolate noisy neighbors, while the Kubernetes Pod cgroup caps
their combined execution.

Containerd-backed runtimes do not remain in containerd's top-level cgroup.
Fastlet discovers its Kubernetes Pod cgroup from the Pod UID and the mounted
host cgroup hierarchy, then gives every Sandbox a deterministic child cgroup.
The discovery supports cgroup v1/v2 and cgroupfs/systemd layouts; it does not
require a Runtime Environment field or a provider-specific annotation.
The driver uses the systemd unit encoding understood by runc and runsc, while
Kata and extension shims receive the equivalent filesystem-style child path.
Deletion is not considered complete until the runtime process has left and
the deterministic Sandbox cgroup can be removed.

All long-running containers in the Fastlet Pod must have CPU and memory
limits, otherwise Kubernetes cannot guarantee a bounded Pod parent cgroup.
Fast Sandbox supplies bounded defaults for platform-owned control/proxy
containers and rejects an unbounded user-provided sidecar. A configured
aggregate limit may be lower than the sum of Sandbox limits, but not lower
than the runtime's fixed overhead.

A runtime that cannot enforce the requested profile must reject the Pool capability instead of silently weakening isolation.

## Capability probe

Fastlet probes actual node/runtime capability before reporting readiness. A Kubernetes RuntimeClass object alone is not proof that its shim, configuration, host paths, KVM device, network mode, and resource enforcement work.

Capability states distinguish configured, available, ready, degraded, and unsupported profiles. Pool Conditions expose the resulting reason.
