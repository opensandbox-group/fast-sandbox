# OpenSandbox Execd

OpenSandbox Execd is an Infra Component integration. Fast Sandbox delivers, starts, discovers, authenticates, and transparently proxies Execd; the OpenSandbox SDK and Execd API continue to own command, file, PTY, and process semantics.

## Request path

```text
OpenSandbox SDK
  -> Fast Sandbox ResolveEndpoint
  -> Sandbox Proxy
  -> Fastlet Proxy
  -> Sandbox private address:44772
  -> OpenSandbox Execd
```

The proxies preserve HTTP, SSE, WebSocket, and file streaming behavior. They do not interpret `/command`, `/files`, `/pty`, or another Execd endpoint.

## Responsibility boundary

Fast Sandbox owns:

- InfraProfile selection;
- immutable artifact and digest verification;
- delivery before runtime start;
- `sandbox-init` activation and supervision;
- per-generation token and instance configuration;
- `GET /ping` readiness;
- service registration on port `44772`;
- fenced route publication and revocation;
- endpoint and required-header hand-off.

OpenSandbox Execd owns:

- command and background command behavior;
- sessions and PTY;
- file and directory APIs;
- SSE and WebSocket protocol semantics;
- process and service metrics;
- validation of `X-EXECD-ACCESS-TOKEN`;
- OpenAPI and official SDK compatibility.

## Development profile

`opensandbox-execd-quickstart` is a runnable development profile:

- it pins the Execd v1.0.21 amd64 image in the Fastlet build;
- it verifies the extracted `/execd` file SHA-256;
- it mounts the artifact read-only at `/.fast/infra/opensandbox-execd`;
- it supports `container`, `gvisor`, `kata-qemu`, and `kata-clh`;
- it starts Execd and the user entrypoint through `sandbox-init`;
- it generates a random `EXECD_ACCESS_TOKEN` for each Sandbox generation;
- Fastlet Proxy injects `X-EXECD-ACCESS-TOKEN` only on the upstream hop;
- it publishes the route after `GET /ping` succeeds.

Use [Quick Start](../getting-started/quickstart.md) to exercise it.

The development profile includes public test signing material and does not provide a production supply-chain contract.

## Production profile

The built-in `opensandbox-execd` profile is intentionally unconfigured. A production release must bind:

- an immutable OCI reference and digest;
- an artifact manifest and file modes;
- signature and verification policy;
- architecture selection;
- private/offline registry policy;
- Execd API and Adapter compatibility;
- upgrade and rollback rules.

An unbound or invalid artifact keeps the Pool unavailable. Fast Sandbox must not create a Sandbox without Execd and report `DataPlaneReady`.

## Startup semantics

```text
prepare and verify artifact
  -> compile per-instance plan
  -> inject bundle and configuration
  -> start sandbox-init
       +-> Execd
       +-> user entrypoint
  -> GET /ping
  -> publish route
  -> DataPlaneReady
```

When no dependency requires ordering, Execd and the user entrypoint start in parallel. `CreateSandbox` returns at RuntimeReady; Execd readiness and route publication complete asynchronously.

## Authentication

The component token:

- is unique to one Sandbox generation;
- is not stored in the Sandbox CRD;
- is not returned to callers;
- is available only in protected instance state and the Execd process;
- is injected into upstream requests by Fastlet Proxy.

The caller receives a separate short-lived Fast Sandbox route credential. Reset, reassignment, deletion, and Pod replacement invalidate both route and component identity.

## Runtime delivery

| Runtime | Development delivery | Production direction |
|---|---|---|
| container | Read-only bind mount | Immutable bundle or image layer |
| gVisor | Read-only bind mount | Immutable bundle or image layer |
| Kata QEMU/CLH | OCI bind mount carried through the shared filesystem | Template-baked or preinstalled artifact |
| Kata Firecracker | Not supported | Requires the runtime capability gate first |
| BoxLite | Not supported by the Quick Start profile | Artifact volume or template bake after BoxLite gates |

Runtime-specific delivery does not change the logical service name, port, or OpenSandbox SDK.

## Validation

The acceptance path must cover:

- create and DataPlaneReady;
- command SSE;
- upload, stat, read, and download;
- PTY/WebSocket when enabled by the Adapter;
- non-root user image semantics;
- token isolation from the user environment;
- old-token and old-route rejection after reset or reassignment;
- Fastlet route recovery;
- independent runtime and data-plane failure reporting.

Run:

```bash
make e2e SUITE=quickstart
make e2e SUITE=infra
```

Fast Sandbox does not ship an E2B envd integration. A future component must enter through the same InfraProfile, supply-chain, SDK hand-off, and capability-gate process.
