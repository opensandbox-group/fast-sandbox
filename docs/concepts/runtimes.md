# Runtime model

A SandboxPool selects one immutable runtime name. Fast Sandbox resolves that name through a platform-owned RuntimeCatalog and passes the resulting RuntimeProfile to both the Pool Controller and Fastlet.

## Public and internal abstractions

```text
SandboxPool.spec.runtime
  -> RuntimeCatalog
  -> RuntimeProfile
  -> RuntimeDriver
  -> backend runtime
```

- `RuntimeName` is the public Pool value.
- `RuntimeProfile` is the platform-owned deployment and capability contract.
- `RuntimeDriver` is Fastlet's runtime-neutral lifecycle interface.

Users cannot override containerd handlers, shim paths, runtime configuration paths, network modes, or platform mounts independently.

## Canonical runtime names

| Runtime name | Driver | Backend |
|---|---|---|
| `container` | containerd | `io.containerd.runc.v2` |
| `gvisor` | containerd | `io.containerd.runsc.v1` |
| `kata-qemu` | containerd | Kata shim with QEMU configuration |
| `kata-clh` | containerd | Kata shim with Cloud Hypervisor configuration |
| `kata-fc` | containerd | Kata shim with Firecracker configuration |
| `boxlite` | BoxLite | Pod-local BoxLite runtime sidecar |

The names define stable profiles, not unconditional production support. See [Runtime support](../reference/runtime-support.md).

## RuntimeProfile

A RuntimeProfile fixes:

- driver kind and backend configuration;
- privileged mode and host paths;
- KVM and sidecar requirements;
- runtime overhead;
- network mode;
- Infra delivery modes;
- cache, recovery, and network capabilities;
- a deterministic profile hash.

The profile hash lets Controllers and Fastlets detect incompatible configuration instead of interpreting the same Pool differently.

## RuntimeDriver

The RuntimeDriver interface contains lifecycle operations:

```text
Initialize
ProbeCapabilities
EnsureSandbox
InspectSandbox
DeleteSandbox
ListManagedSandboxes
Close
```

Optional interfaces add image cache, recovery, resource admission, network injection, Infra injection, access descriptors, and route publication.

Exec, file transfer, PTY, and user protocol operations are deliberately absent. They belong to Infra Components and their upstream SDKs.

## Container and gVisor

The container and gVisor profiles use the containerd driver and a Fastlet-owned Linux network namespace.

- `container` uses the runc v2 handler and host kernel namespace/cgroup isolation.
- `gvisor` uses the runsc handler and a user-space kernel boundary.

Both receive fixed CPU, memory, and PID limits from the Pool's Sandbox resource profile.

## Kata

Kata profiles use the containerd Kata shim. Fastlet still owns the network slot, while the shim exposes the interface to the guest.

QEMU and Cloud Hypervisor are validated profiles. Firecracker remains capability-gated because the validation environment lacks the complete kernel and block-snapshotter contract required by that profile.

Kata supports Infra delivery through OCI bind mounts, image/template baking, preinstalled artifacts, or runtime-specific guest copy.

## BoxLite

BoxLite does not use a containerd runtime handler. Fastlet talks to a `boxlite-runtime` sidecar over a versioned Pod-local Unix socket. The sidecar contains native/CGO integration and owns BoxLite state.

BoxLite networking produces a local-forward access descriptor rather than a Fastlet-managed netns. The current profile remains fail closed because the upstream API cannot yet prove the required host-enforced per-Box resource contract.

## Fixed Pool resources

Every Sandbox in one Pool uses the same immutable CPU, memory, and PID limits. Fastlet passes those values to the selected RuntimeDriver and is the enforcement boundary.

The Pool Controller sizes the resource-owning Fastlet or runtime sidecar from:

```text
per-Sandbox resources * maxSandboxesPerPod + runtime overhead
```

A runtime that cannot enforce the requested profile must reject the Pool capability instead of silently weakening isolation.

## Capability probe

Fastlet probes actual node/runtime capability before reporting readiness. A Kubernetes RuntimeClass object alone is not proof that its shim, configuration, host paths, KVM device, network mode, and resource enforcement work.

Capability states distinguish configured, available, ready, degraded, and unsupported profiles. Pool Conditions expose the resulting reason.
