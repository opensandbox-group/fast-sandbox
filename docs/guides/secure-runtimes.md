# Secure runtimes

Fast Sandbox supports runtime profiles with different isolation boundaries. The profile selects a complete platform contract; users do not configure individual RuntimeClass names or containerd handlers.

## Runtime profiles

| Runtime | Isolation | Node requirement | Fast Sandbox status |
|---|---|---|---|
| `container` | Host kernel namespaces and cgroups | containerd | Validated |
| `gvisor` | runsc user-space kernel | runsc shim and configuration | Validated |
| `kata-qemu` | QEMU virtual machine | KVM and Kata installation | Validated |
| `kata-clh` | Cloud Hypervisor virtual machine | KVM and Kata installation | Validated |
| `kata-fc` | Firecracker virtual machine | KVM, compatible kernel, block snapshotter | Capability-gated |

See [Runtime support](../reference/runtime-support.md) for the canonical capability matrix.

## gVisor prerequisites

Eligible nodes must provide:

- `/usr/local/bin/runsc`;
- `/usr/local/bin/containerd-shim-runsc-v1`;
- `/etc/containerd/runsc.toml`;
- a working `io.containerd.runsc.v1` handler;
- the Fastlet host paths and Linux network prerequisites.

The Pool Controller injects these platform-owned paths from the RuntimeProfile. A Pool cannot override them.

Prepare the development environment:

```bash
make env PROFILE=gvisor
```

Run the interactive walkthrough:

```bash
make quickstart RUNTIME=gvisor
```

## Kata prerequisites

Eligible nodes must provide:

- `/dev/kvm`;
- a compatible Kata installation under `/opt/kata`;
- `containerd-shim-kata-v2`;
- runtime-specific Kata configuration;
- nested virtualization when running inside a development VM;
- Fastlet network host paths.

Prepare and run:

```bash
make env PROFILE=kata-qemu
make quickstart RUNTIME=kata-qemu

make env PROFILE=kata-clh
make quickstart RUNTIME=kata-clh
```

Kata uses a Fastlet-owned network slot. The Kata shim carries its interface and supported OCI mounts into the guest.

## Firecracker capability gate

The Fast Sandbox profile for `kata-fc` remains degraded with reason `KataFirecrackerNotValidated`.

The validated development node exposed two independent problems:

1. the Kata Firecracker kernel lacked `CONFIG_VIRTIO_MMIO`, so the guest could not discover the root block device;
2. after replacing the kernel, the kind/containerd environment still used overlayfs, while the Firecracker path required a block-device snapshotter such as devmapper to deliver the workload rootfs.

Increasing timeouts, disabling the jailer, or replacing only the kernel does not satisfy the profile.

The gate may be removed only after:

- a pinned compatible Kata and Firecracker artifact set;
- a kernel with the required MMIO drivers;
- a block snapshotter and storage configuration;
- a direct minimal Kubernetes runtime test;
- Fast Sandbox runtime, network, Infra, recovery, and cleanup E2E tests.

## Capability validation

A RuntimeClass object is not sufficient evidence. Fastlet probes actual backend capability, and Pool readiness must fail closed if the handler, binary, configuration, KVM device, network mode, or resource contract is unavailable.

Run:

```bash
make e2e SUITE=runtime RUNTIME=gvisor
make e2e SUITE=runtime RUNTIME=kata
```

A skipped runtime test is not a passing capability gate.
