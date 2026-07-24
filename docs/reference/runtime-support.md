# Runtime support

This matrix describes Fast Sandbox validation status. It is not a statement about the upstream runtime's general capabilities.

| Runtime | Driver | Network | Infra delivery | Quick Start | Status |
|---|---|---|---|---:|---|
| `container` | containerd/runc | Linux netns | bind mount, image layer, preinstalled | Yes | Validated |
| `gvisor` | containerd/runsc | Linux netns | bind mount, image layer | Yes | Validated |
| `kata-qemu` | containerd/Kata | Fastlet slot to guest NIC | bind mount, template, preinstalled, guest copy | Yes | Validated |
| `kata-clh` | containerd/Kata | Fastlet slot to guest NIC | bind mount, template, preinstalled, guest copy | Yes | Validated |
| `kata-fc` | containerd/Kata | Fastlet slot to guest NIC | profile-defined | No | Degraded; fail closed |
| `boxlite` | BoxLite sidecar | runtime-owned LocalForward | template, preinstalled, artifact volume | No | Unsupported; fail closed |

## Meaning of validated

A validated profile has remote Linux/Kubernetes evidence for:

- backend capability detection;
- fixed Sandbox resources;
- runtime creation and deletion;
- private networking;
- Infra injection where the selected profile requires it;
- identity fencing and recovery;
- cleanup and negative capability behavior.

Validation does not imply snapshot, pause/resume, persistent storage, or live migration.

## Container

The default profile uses `io.containerd.runc.v2`, Fastlet-owned Linux network slots, image cache inventory, and containerd ensure-absent cleanup.

## gVisor

The gVisor profile uses `io.containerd.runsc.v1`. Eligible nodes must provide runsc, its containerd shim, and configuration at the platform-owned paths.

## Kata QEMU and Cloud Hypervisor

Both profiles use `containerd-shim-kata-v2` with runtime-specific configuration. Eligible nodes require KVM and the pinned Kata installation.

## Kata Firecracker

The profile remains degraded with reason `KataFirecrackerNotValidated`. The development environment lacks the combined MMIO-capable kernel and block snapshotter required by the path.

See [Secure runtimes](../guides/secure-runtimes.md).

## BoxLite

The profile remains unsupported with reason `BoxLiteResourceEnforcementIncomplete`. The sidecar integration exists, but host-enforced CPU, memory, and PID semantics are incomplete.

See [BoxLite integration](../guides/boxlite.md).

## Infra profile compatibility

| InfraProfile | Allowed runtimes | Status |
|---|---|---|
| `minimal` | all profiles subject to runtime capability | Configured |
| `opensandbox-execd-quickstart` | container, gVisor, Kata QEMU, Kata CLH | Development only |
| `opensandbox-execd` | container, gVisor | Unconfigured until production artifact binding |
| `test-infra` | container | E2E only |

An incompatible or unconfigured combination prevents Pool readiness.
