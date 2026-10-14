# Fast Sandbox

**A high-performance Sandbox runtime on Kubernetes, powered by Firecracker microVMs.**

[中文](README_ZH.md) · [Use with OpenSandbox](#use-with-opensandbox-recommended) · [Standalone deployment](#standalone-deployment) · [Documentation](docs/README.md) · [Architecture](docs/concepts/architecture.md)

Fast Sandbox brings Firecracker microVM isolation and snapshot-based startup to
Kubernetes. It is built for workloads that frequently create and recycle
execution environments: AI agents, code interpreters, and on-demand development
or test sandboxes.

Kubernetes manages capacity and desired lifecycle state. Warm **Fastlet Pods**
each host multiple Sandboxes, while the Firecracker runtime restores each
instance from a prepared snapshot. Creating a Sandbox reuses that capacity
without scheduling and starting a new Kubernetes Pod for every request.

We recommend using Fast Sandbox through [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox),
which already integrates this runtime with its API, SDKs, ingress, and command
execution. You can also deploy Fast Sandbox independently as the Sandbox
scheduling and runtime layer for your own platform.

![Fast Sandbox system overview](docs/assets/system-overview.svg)

## Why Fast Sandbox

- **Firecracker microVM isolation.** Each Firecracker Sandbox has its own
  microVM, guest kernel, private network namespace, and instance rootfs.
- **Snapshot-based startup.** Initialize an environment once, then restore new
  instances from saved VM and memory state. XFS reflink clones the rootfs
  without a full disk copy on the creation path.
- **Warm, high-density capacity.** A SandboxPool maintains ready Fastlet Pods;
  each Fastlet admits multiple instances within configured resource and
  capacity limits.
- **A dedicated creation path.** Multi-active Fast-Path servers persist Sandbox
  intent, select a ready Fastlet, and call it directly. Fastlet owns atomic
  admission; Kubernetes reconciliation handles lifecycle convergence.
- **Shared artifacts and on-demand loading.** A node runtime-agent caches
  snapshots for local Fastlets; P2P delivery reduces repeated origin downloads.
  The OverlayBD + ublk-backed `fsb-vm` path reads rootfs and memory snapshot blocks on
  demand, so startup does not require downloading the entire artifact set.
- **Checkpoint and reuse.** Pause an instance to release capacity and resume
  it on a compatible Fastlet, or publish a running Sandbox as a reusable image.
- **Kubernetes-native operations.** CRDs describe Sandboxes, Pools, templates,
  and snapshots, with declarative lifecycle management and observable status.

[How it works](docs/concepts/architecture.md)

## Use with OpenSandbox (recommended)

[OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) already integrates
Fast Sandbox. Use its public API and SDKs to build templates, create microVM
Sandboxes, execute commands, manage files, and drive pause/resume and snapshots.
Fast Sandbox supplies Pool placement, warm capacity, and Firecracker lifecycle;
OpenSandbox supplies the application-facing interface and authenticated ingress.

```text
Your application -> OpenSandbox API / SDK
                         -> Fast Sandbox -> Fastlet -> Firecracker microVM
```

For a complete development environment, start from the latest OpenSandbox
checkout on a bare-metal Linux host with KVM, Docker, Go ≥ 1.25, cgroup v2,
and sudo. Install `uv` for the CLI and SDK verification. The script prepares
the Kind cluster, artifact store, golden image, runtime, server, and ingress,
then verifies Sandbox creation and access. First setup builds and downloads images.

```bash
git clone https://github.com/opensandbox-group/OpenSandbox.git
cd OpenSandbox

WARM_IMAGES=1 ./scripts/fast-sandbox-env/integration-env.sh up
./scripts/fast-sandbox-env/integration-env.sh status
./scripts/fast-sandbox-env/integration-env.sh sdk-e2e
```

The lifecycle API listens on `http://127.0.0.1:18080`; the environment's API key
is `fast-sandbox-env`. Choose a template whose build has reached `Succeeded`
from the OpenSandbox template catalog, then use the Python SDK:

```python
import asyncio
from datetime import timedelta

from opensandbox.config import ConnectionConfig
from opensandbox.sandbox import Sandbox


async def main():
    sandbox = await Sandbox.create_from_template(
        "tpl_<succeeded-template-id>",
        timeout=timedelta(minutes=10),
        ready_timeout=timedelta(minutes=5),
        connection_config=ConnectionConfig(
            domain="127.0.0.1:18080",
            protocol="http",
            api_key="fast-sandbox-env",
        ),
    )
    try:
        result = await sandbox.commands.run("echo hello-firecracker")
        for message in result.logs.stdout:
            print(message.text)
    finally:
        await sandbox.destroy()


asyncio.run(main())
```

In the current OpenSandbox server, Fast Sandbox is integrated under
`runtime.type = "kubernetes"`. Template-based creation (`templateId`, exposed
as `Sandbox.create_from_template()` in Python) selects the Fast Sandbox backend.
The integration environment wires this configuration automatically and builds
Fast Sandbox from the revision pinned by OpenSandbox.

See the [OpenSandbox integration environment](https://github.com/opensandbox-group/OpenSandbox/tree/main/scripts/fast-sandbox-env),
[Helm deployment guide](https://github.com/opensandbox-group/OpenSandbox/blob/main/manifests/HELM-DEPLOYMENT.md#fast-sandbox-runtime-firecracker),
and [Python SDK](https://github.com/opensandbox-group/OpenSandbox/tree/main/sdks/sandbox/python).
Clean up the development stack with
`./scripts/fast-sandbox-env/integration-env.sh down` from the OpenSandbox checkout.

## Standalone deployment

Deploy Fast Sandbox independently when your platform already has a Sandbox API
or needs its own workload protocols. Use the Fast-Path gRPC API, Fast Sandbox
SDKs, or Kubernetes CRDs to connect your control plane to Pool scheduling,
atomic admission, microVM creation, endpoint resolution, and cleanup.

```text
Your Sandbox API -> Fast-Path / Kubernetes CRDs -> Fastlet -> Firecracker microVM
```

From this repository, the standalone Firecracker development environment needs
Linux with KVM, `/dev/net/tun`, Docker, root or sudo, and sufficient disk space.
It prepares a two-node Kind cluster, XFS reflink state storage, MinIO,
runtime-agents, a golden image, and a Firecracker Pool:

```bash
# Prewarm artifacts and retain the existing Docker data-root configuration.
DOCKER_DATA_ROOT= WARM_IMAGES=1 ./scripts/integration-env.sh up

# Create Sandboxes, verify execd /ping through their routes, and delete them.
./scripts/integration-env.sh verify
./scripts/integration-env.sh status
./scripts/integration-env.sh down
```

See [Standalone deployment](docs/guides/deployment.md),
[Firecracker setup](docs/guides/firecracker-integration-env.md), and
[API reference](docs/reference/api.md). To explore the control plane with a
container runtime, use `make quickstart` and follow the
[Quick Start](docs/getting-started/quickstart.md).

## Performance

Fast Sandbox targets low creation latency by reusing ready Fastlets, preparing
network slots, caching snapshots, cloning rootfs files with reflink, and
restoring initialized microVMs.

The [OpenSandbox ACK report](https://github.com/opensandbox-group/OpenSandbox/blob/main/docs/architecture/fast-sandbox/performance.md)
measured the complete Python SDK create path on 2026-10-08, from
`Sandbox.create_from_template()` through the lifecycle server, FastPath, ingress
gateway, and the guest's first successful `/ping`:

| Warm SDK create | Success | p50 | p99 | Mean |
| --- | ---: | ---: | ---: | ---: |
| Serial repeat | 100/100 | 77.9 ms | 112.5 ms | 78.2 ms |
| Concurrency 10 | 100/100 | 102.7 ms | 125.4 ms | 102.4 ms |

The runs used two ACK KVM nodes with AMD EPYC 9T95 CPUs, Firecracker 1.16.1,
XFS reflink storage, cached Alpine + Execd templates (1 vCPU / 2 GiB memory),
and 20 prepared slots. The SDK polled readiness every 10 ms, compared with its
200 ms default. Deletion and batch reclamation pauses are excluded.

These are dated engineering measurements, not production SLAs or measurements
of the latest checkout. The concurrency-20 run succeeded in only 51/100 attempts;
the report includes capacity rejections and a readiness timeout. It does not
establish sustainable throughput. Cold artifact delivery is a separate cost.
See the [raw samples](https://github.com/opensandbox-group/OpenSandbox/blob/main/docs/public/benchmarks/fast-sandbox-ack-2026-10-08.json)
for environment, versions, failures, and runtime-stage timings.

At the Fast Sandbox API layer, Create defaults to aggregate `Ready`. Explicit
`RuntimeReady` completion returns earlier, before component and route readiness.
Benchmarks must name their completion boundary and cache state. See
[Performance](docs/guides/performance.md) for measurement tools.

## Application protocols

Applications expose their own protocols inside the guest; HTTP, SSE, and
WebSocket traffic can pass through authenticated proxies. Optional
[Infra Components](docs/concepts/infra-components.md) add managed processes,
health checks, and named endpoints. OpenSandbox Execd can be baked into the
Firecracker template for command execution and file operations.

## Runtime options and current scope

The direct `firecracker` runtime is the snapshot-restore path described above.
Fast Sandbox also supports `container`, `gvisor`, `kata-qemu`, `kata-clh`,
`kata-fc`, and `kata-dragonball` through containerd. `kata-fc` uses Kata's
Firecracker integration and has a different startup path from `firecracker`.
See [Runtime model](docs/concepts/runtimes.md) and
[Runtime support](docs/reference/runtime-support.md) for profile details.

OverlayBD on-demand loading currently uses the host-side
[`fsb-vm` path](docs/guides/fsb-vm-user-guide.en.md). The OpenSandbox/Kubernetes
integration uses fully cached native snapshot artifacts.

- The CRD and Fast-Path API version is `v1alpha2` and is still alpha.
- Firecracker requires compatible KVM hosts and snapshot artifacts; the direct
  runtime requires an XFS state directory with reflink support.
- Firecracker Sandboxes consume published snapshots. Prepare an OCI image with
  `SandboxTemplate` before using it on this runtime.
- Pause/resume and live snapshots are implemented by the Firecracker runtime;
  cross-host restore requires snapshot-compatible hosts.
- Fastlet Pod loss destroys its live instances. Automatic recreation creates a
  new generation. Persistent storage and live migration are outside the scope.

## Documentation

- [Documentation index](docs/README.md)
- [Firecracker deployment and verification](docs/guides/firecracker-integration-env.md)
- [Golden-image builds](docs/guides/sandboxtemplate-golden-images.md)
- [Live snapshots](docs/guides/sandbox-snapshots.md)
- [Pause and resume](docs/guides/pause-resume-snapshot-integration.md)
- [Snapshot CPU compatibility](docs/guides/snapshot-cpu-compatibility.md)
- [Private networking](docs/concepts/networking.md)
- [Deployment](docs/guides/deployment.md)
- [Observability](docs/guides/observability.md)
- [API reference](docs/reference/api.md) · [CLI reference](docs/reference/fastctl.md)

## License

[Apache-2.0](LICENSE)
