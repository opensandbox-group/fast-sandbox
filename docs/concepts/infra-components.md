# Infra Components

Runtime Augmentation starts the user's OCI workload while adding platform-owned helpers that provide Sandbox management services.

The result combines:

```text
user image and entrypoint
+ immutable platform artifacts
+ per-instance configuration and credentials
+ activation and readiness policy
= augmented Sandbox runtime
```

Fast Sandbox does not rebuild the user's image for every Sandbox.

## InfraProfile

A SandboxPool selects one immutable `infraProfile`. The platform-owned InfraCatalog resolves it into:

- allowed runtimes;
- version and deterministic profile hash;
- component dependency graph;
- immutable artifacts and digests;
- delivery modes;
- activation commands and restart policy;
- per-instance initialization;
- internal credential bindings;
- published services and readiness probes;
- required or optional readiness semantics.

An unknown, invalid, unconfigured, or runtime-incompatible profile fails closed.

## Artifact sources

Supported source classes are:

- embedded artifact;
- immutable OCI artifact;
- platform static file;
- preinstalled runtime content.

Non-preinstalled artifacts require an immutable reference, a SHA-256 digest, an executable policy, and a destination path.

## Delivery modes

RuntimeProfiles advertise supported Infra delivery modes:

- OCI bind mount;
- image layer;
- preinstalled content;
- template bake;
- guest copy;
- artifact volume.

The InfraCatalog selects the first mode supported by both the component and runtime. The component contract stays the same even when container, Kata, and BoxLite use different delivery mechanisms.

## Activation

Activation modes include:

- entrypoint supervisor;
- component bootstrap;
- system service.

The platform can start a required component before the user process, restart it according to policy, and preserve the user's original command as the supervised workload.

`sandbox-init` is the small in-runtime supervisor used by the entrypoint-supervisor path. It reads generation-fenced instance configuration, starts the selected components, applies restart policy, launches the original user process, and emits readiness observations.

## Per-instance state

Fastlet prepares a generation-specific instance directory containing:

- compiled component plan;
- resolved paths and environment;
- service definitions;
- private internal credentials;
- diagnostic state.

State is written atomically. A stale Sandbox generation cannot reuse the directory or credentials of a newer instance.

## Credentials

An InfraProfile can bind a Fastlet-generated per-Sandbox credential to:

- an environment variable visible to the component;
- an upstream HTTP header injected only by Fastlet Proxy.

Callers receive a route credential, not the component's private token. This separates caller authorization from component-native authentication.

## Readiness

Required services use HTTP, TCP, or explicit readiness. Probes start immediately and use a bounded backoff with a 10 ms ceiling.

Runtime and data-plane readiness remain separate:

- `RuntimeReady` means the RuntimeDriver completed Ensure;
- `DataPlaneReady` means every required Infra service is ready and its route is published.

Create returns at RuntimeReady. Clients that need an Infra service must wait for DataPlaneReady.

## OpenSandbox Execd

The development profile `opensandbox-execd-quickstart` injects a pinned Execd artifact into container, gVisor, Kata QEMU, and Kata Cloud Hypervisor profiles. It publishes HTTP service port `44772` and uses the official OpenSandbox SDK for command and file semantics.

The production profile `opensandbox-execd` remains unconfigured until a platform release binds a trusted immutable artifact and digest.

See [OpenSandbox Execd](../guides/opensandbox-execd.md).

## Adding another component

A new component requires:

1. an immutable artifact supply chain;
2. a valid InfraProfile entry;
3. at least one delivery mode shared with its target runtime;
4. activation, initialization, and readiness rules;
5. a stable service name and target port;
6. a component-facing SDK or protocol client;
7. lifecycle, credential, streaming, and cleanup tests.

Fast Sandbox core does not add a new Exec/File API for the component.
