# Infra Components

An Infra Component adds a managed process to a user's Sandbox without rebuilding
the user's image:

```text
user image and command
+ immutable component artifact
+ supervised component process
+ health-checked named endpoint
= augmented Sandbox
```

Components are declared directly on a `SandboxPool`. There is no global
`InfraProfile` catalog and no per-Sandbox component override.

## Pool contract

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxPool
metadata:
  name: opensandbox-pool
  namespace: fast-sandbox
spec:
  runtime: container
  infraComponents:
    - name: execd
      artifact:
        source:
          image:
            reference: ghcr.io/opensandbox/execd@sha256:<digest>
        mappings:
          - sourcePath: /execd
            targetPath: /.fast/components/execd/execd
      process:
        command:
          - /.fast/components/execd/execd
          - --port
          - "44772"
        env:
          LOG_LEVEL: info
        restartPolicy: OnFailure
        healthCheck:
          httpGet:
            path: /ping
          timeoutSeconds: 10
      endpoint:
        protocol: HTTP
        port: 44772
```

Every declared component is required. `DataPlaneReady` means that all component
health checks pass and their named routes have been acknowledged by Fastlet
Proxy.

## Artifact delivery

Exactly one immutable source is required:

- an OCI image pinned by `@sha256:...`; or
- an HTTPS gzip-compressed tar archive with the complete archive SHA-256.

The OCI image is an artifact carrier, not another running container. Fastlet
extracts only the declared mappings. A mapping may select a file or directory
and places it under `/.fast/components/<component-name>/`.

Fastlet rejects mutable OCI references, digest mismatch, absolute or traversing
archive entries, escaping symlinks, device entries, and overlapping target
paths. Artifacts are prepared before a Fastlet enters placement and are not
downloaded on the Sandbox Create path.

Runtime adapters implement the same logical mapping differently:

| Runtime | Delivery |
| --- | --- |
| container / gVisor | read-only artifact mount |
| Kata | artifact mount visible to the guest |
| Firecracker | guest copy into the restored rootfs |

## Process supervision

`sandbox-init` is the small process supervisor injected into the Sandbox. It:

- starts all Infra Components and the user process concurrently;
- executes argv directly without shell expansion;
- applies Pool-owned component environment variables;
- preserves the user's original command and environment;
- applies `Never`, `OnFailure`, or `Always` restart policy;
- uses bounded platform-owned restart backoff.

Process exit policy and health are separate. A live but unhealthy process is
not killed automatically; its named route becomes unavailable until health
recovers.

## Health and readiness

A component declares one HTTP or TCP health check on its endpoint port. Fastlet
probes through the runtime's local access descriptor, never through Sandbox
Proxy.

The milestones are independent:

| Milestone | Meaning |
| --- | --- |
| `RuntimeReady` | Runtime and user process started; component convergence may continue |
| `ComponentReady` | One component passed health and its local named route was published |
| `DataPlaneReady` | The interaction route and every Pool component are Ready |
| aggregate `Ready` | Runtime, DataPlane, every component, and every Action Binding are Ready |

Create defaults to aggregate `READY`; explicit `RUNTIME_READY` is the
early-return mode. Later convergence is observed by polling the assigned
Fastlet through `GetSandbox`, without waiting for CRD status propagation.

Health continues after initial readiness. When a component becomes unhealthy,
Fastlet revokes the instance-fenced route and reports the data plane
Unavailable. The same local worker republishes the route after the component
recovers.

## Named routing

The component name is an immutable routing key shared by Pool validation,
instance state, status, Fastlet Proxy, Sandbox Proxy, FastPath, and protocol
adapters:

```text
/v2/sandboxes/<uid>/components/<component-name>/...
```

The suffix, query, body, streaming behavior, WebSocket upgrade, and application
headers are forwarded without application-protocol translation. A component
port is reserved and cannot be reached through the raw-port route.

Fast Sandbox authenticates routes with
`X-Fast-Sandbox-Route-Credential`. It preserves application `Authorization`
and removes the platform credential before reaching the Sandbox.

## OpenSandbox Execd

The sample Pool defines an `execd` component and fastctl uses the official
OpenSandbox SDK:

```bash
fastctl opensandbox exec my-sandbox -- ls -la
fastctl opensandbox exec my-sandbox --component custom-execd -- ls -la
```

`opensandbox` selects the SDK adapter; `--component` selects the Pool component
that implements that protocol. Execd runs without its optional
`EXECD_ACCESS_TOKEN` mechanism.

See the [Infra Components reference](../reference/infra-components.md) for the
complete field and validation contract, and
[OpenSandbox Execd](../guides/opensandbox-execd.md) for a concrete integration.
