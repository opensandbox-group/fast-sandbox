# Runtime node installation

Fast Sandbox runtime configuration does not install a runtime. Production node
preparation has three separate layers:

1. the operator installs and validates the runtime binaries, guest artifacts,
   containerd handler, and optional snapshotter on eligible nodes;
2. the runtime environment ConfigMap tells Fast Sandbox where that installation
   lives;
3. a SandboxPool selects a stable Fast Sandbox runtime name.

Do not use `make env` as a production installer. It prepares disposable Kind
nodes for E2E testing and intentionally performs host mutations that belong in
a node image or an operator-owned installer in production.

## Kata Containers

Fast Sandbox E2E pins [Kata Containers `3.31.0`](https://github.com/kata-containers/kata-containers/releases/tag/3.31.0).
It consumes the exact `kata-deploy` image as an artifact source, then validates
QEMU, Cloud Hypervisor, Firecracker, and Dragonball against the resulting
installation. Production clusters should install the same pinned release with
the upstream [`kata-deploy` DaemonSet](https://github.com/kata-containers/kata-containers/tree/3.31.0/tools/packaging/kata-deploy/helm-chart/kata-deploy)
or bake the identical artifacts into the node image.

The repository provides reviewed example values at
`config/runtime-installers/kata-deploy-values.yaml`. They deliberately enable
QEMU, Cloud Hypervisor, and Dragonball but leave Firecracker disabled until its
snapshotter is provisioned.

Label only eligible nodes:

```bash
kubectl label node <node> fast-sandbox.io/kata-node=true
```

Check out the matching upstream tag and install its chart from the pinned
source tree:

```bash
git clone --depth 1 --branch 3.31.0 \
  https://github.com/kata-containers/kata-containers.git

helm upgrade --install kata-deploy \
  ./kata-containers/tools/packaging/kata-deploy/helm-chart/kata-deploy \
  --namespace kube-system \
  --values ./fast-sandbox/config/runtime-installers/kata-deploy-values.yaml

kubectl rollout status daemonset/kata-deploy \
  --namespace kube-system --timeout=20m
```

In a controlled production pipeline, mirror the chart and its images and pin
them by digest. Do not deploy an unreviewed moving tag.

### Independent Kubernetes smoke test

RuntimeClass is not used by Fastlet's direct containerd API, but it is a useful
installer test. Run it before deploying a Kata SandboxPool:

```bash
kubectl apply -f config/runtime-installers/kata-verification-pod.yaml
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded \
  pod/fast-sandbox-kata-verification --timeout=3m
kubectl logs pod/fast-sandbox-kata-verification
kubectl delete pod/fast-sandbox-kata-verification
```

This proves that kubelet can use the installed handler. Fast Sandbox E2E is a
separate gate because it uses the containerd API directly and additionally
tests its private network, Infra delivery, proxy route, recovery, and cleanup.

### Fast Sandbox-specific Kata bindings

The upstream installation owns the source configuration. Fast Sandbox-specific
files must be separate files so an upstream upgrade cannot silently overwrite
the active contract.

| Runtime | Upstream source | Fast Sandbox binding |
| --- | --- | --- |
| `kata-qemu` | `configuration-qemu.toml` | source file directly |
| `kata-clh` | `configuration-clh.toml` | source file directly |
| `kata-fc` | `configuration-fc.toml` | operator-created `configuration-fc-fast-sandbox.toml` plus a block snapshotter |
| `kata-dragonball` | `runtime-rs/configuration-dragonball.toml` | operator-created `runtime-rs/configuration-dragonball-fast-sandbox.toml` |

The 3.31.0 Dragonball compatibility file used by the validated E2E is copied
from the upstream source and changes only these settings:

```toml
[hypervisor.dragonball]
kernel = "/opt/kata/share/kata-containers/vmlinux.container"
default_vcpus = 2

[runtime]
static_sandbox_resource_mgmt = true
```

The upstream `configuration-dragonball.toml` remains untouched. An operator
should generate the compatibility file during node image construction or in a
post-install step owned by the same rollout as `kata-deploy`.

Firecracker additionally requires a block-device snapshotter and a guest
kernel with `CONFIG_VIRTIO_MMIO=y`. The Kind E2E uses containerd `blockfile`.
The upstream Kata chart defaults Firecracker to `devmapper`, which requires
operator preconfiguration. Production may use either validated backend, but
the selected snapshotter, its storage, the containerd handler, and the runtime
environment binding must agree. Do not copy the E2E scratch file layout into a
production node unchanged.

## gVisor

gVisor does not provide a `kata-deploy`-equivalent production DaemonSet. Follow
the upstream [containerd installation guide](https://gvisor.dev/docs/user_guide/containerd/quick_start/)
or bake a pinned `runsc` and `containerd-shim-runsc-v1` into the node image. If
an organization uses its own privileged installer DaemonSet, that DaemonSet
should:

- target only nodes labelled for gVisor;
- install versioned binaries atomically and verify their checksums;
- install `/etc/containerd/runsc.toml` and the `io.containerd.runsc.v1`
  handler;
- restart or reload containerd safely, one node at a time;
- expose readiness only after an independent RuntimeClass smoke Pod succeeds;
- retain the previous installation for rollback.

Fast Sandbox E2E defaults to the validated `20260714` release rather than the
moving `latest` path. Set `GVISOR_RELEASE` only when intentionally qualifying
a newer release; a production node image should pin the same release and
verify the published checksums.

After the node installer has created the `gvisor` RuntimeClass, validate it:

```bash
kubectl apply -f config/runtime-installers/gvisor-verification-pod.yaml
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded \
  pod/fast-sandbox-gvisor-verification --timeout=3m
kubectl logs pod/fast-sandbox-gvisor-verification
kubectl delete pod/fast-sandbox-gvisor-verification
```

The verification command checks the guest-visible `dmesg` output for gVisor,
matching the upstream runtime guide. Kernel release text from `uname` is not a
reliable gVisor identity check.

Fast Sandbox expects the default installation paths shown in
[Secure runtimes](secure-runtimes.md). Override them through a runtime
environment when the node image uses different paths.

## Bind the installed nodes

The default runtime environment is suitable only when all relevant nodes share
the same installation. A production cluster should normally use explicit
environments and selectors:

```yaml
version: v1alpha2
environments:
  kata:
    nodeSelector:
      fast-sandbox.io/kata-node: "true"
    containerd:
      socket: /run/containerd/containerd.sock
      namespace: k8s.io
      defaultSnapshotter: overlayfs
      root: /var/lib/containerd
    kubelet:
      root: /var/lib/kubelet
    runtimes:
      kata-qemu: {}
      kata-clh: {}
      kata-fc:
        snapshotter: blockfile
        configPath: /opt/kata/share/defaults/kata-containers/configuration-fc-fast-sandbox.toml
      kata-dragonball:
        configPath: /opt/kata/share/defaults/kata-containers/runtime-rs/configuration-dragonball-fast-sandbox.toml
```

Changing a runtime environment creates a new immutable runtime-plan revision
and rolls Fastlet Pods through the existing surge-and-drain lifecycle. It does
not mutate running Sandboxes in place. Restart NodeJanitor after changing
containerd namespaces or roots so orphan discovery uses the same environment
set.

## Production acceptance gate

For every runtime and node image revision:

1. wait for the installer DaemonSet or node-image rollout;
2. run the independent RuntimeClass smoke Pod;
3. verify containerd reports the configured handler and snapshotter as healthy;
4. deploy the runtime environment and wait for the new Fastlet generation;
5. run the matching Fast Sandbox secure-runtime E2E;
6. create and delete twice, then verify that no task, container, snapshot,
   network namespace, Infra state, or VMM process remains;
7. roll back the node image/runtime environment if any gate fails.

## Firecracker OCI tools and node cache

The runtime-agent DaemonSet installs fsb-vm and OverlayBD tools on the node and runs one optional ublkd container per node. Fastlet executes the shared tools locally and connects directly to the daemon socket. See the [OverlayBD OCI guide](overlaybd-oci-sandboxes.md) for installation paths, activation, shared-cache sizing, credential restrictions, and drain-before-upgrade requirements. Standalone fsb-vm remains usable with its existing tool and socket flags; Kubernetes and runtime-agent are not required.

The example DaemonSet gives each node-service container explicit Kubernetes
resources. Treat those values as starting points and expose equivalent
per-container overrides in production packaging: the agent defaults to
`250m / 256Mi` requests with a `1Gi` memory limit, OverlayBD to
`100m / 128Mi` requests with a `1Gi` memory limit, and janitor to
`50m / 64Mi` requests with `250m / 256Mi` limits. These are node-service
budgets outside every Fastlet Pod cgroup; they are not part of a Pool's
`fastletOverhead` or Sandbox commitment capacity. Size them from observed
artifact concurrency, OverlayBD device count, cache activity, and janitor scan
cost rather than increasing Pool runtime envelopes.
