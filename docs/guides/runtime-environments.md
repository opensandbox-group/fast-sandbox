# Runtime environments

Runtime definitions describe stable Fast Sandbox behavior. Runtime environments
bind those definitions to the container runtime installation on a class of
nodes. This keeps host-specific namespaces, roots, sockets and selectors out of
SandboxPool and out of generic runtime drivers.

The production manifests install
`fast-sandbox-system/fast-sandbox-runtime-environments`. Its data key is
`runtime-environments.yaml`:

```yaml
version: v1alpha2
environments:
  default:
    containerd:
      socket: /run/containerd/containerd.sock
      namespace: k8s.io
      defaultSnapshotter: overlayfs
      root: /var/lib/containerd
    kubelet:
      root: /var/lib/kubelet
    runtimes:
      container: {}
      gvisor: {}
      kata-qemu: {}
      kata-clh: {}
      kata-fc:
        snapshotter: blockfile
        configPath: /opt/kata/share/defaults/kata-containers/configuration-fc-fast-sandbox.toml
      kata-dragonball:
        configPath: /opt/kata/share/defaults/kata-containers/runtime-rs/configuration-dragonball-fast-sandbox.toml
```

An environment describes one node installation: the containerd endpoint and
namespace, its default snapshotter and state root, the kubelet root, and the
node selector used to place Fastlet Pods. A runtime binding records only the
exceptions required by that runtime. For example, `kata-fc` shares the same
containerd installation as the other runtimes but overrides the default
`overlayfs` snapshotter with `blockfile` and selects a Firecracker-specific Kata
configuration.

The configuration only binds an installed runtime. It does not install Kata or
gVisor and it does not edit the upstream runtime files. See
[Runtime node installation](runtime-node-installation.md) for the production
node preparation and independent smoke-test sequence.

`version` is mandatory for operator-supplied configuration. This prevents an
old manifest from being silently interpreted with new field semantics.

An operator can bind a runtime to another node environment without adding
branches to Controller, Fastlet, the containerd driver, Infra delivery or
NodeJanitor:

```yaml
version: v1alpha2
environments:
  custom:
    nodeSelector:
      runtime.example.com/enabled: "true"
    containerd:
      socket: /run/containerd/containerd.sock
      namespace: custom
      defaultSnapshotter: overlayfs
      root: /srv/containerd/root
    kubelet:
      root: /srv/kubelet
    runtimes:
      container:
        handler: io.containerd.custom.v2
        runtimePath: /opt/custom/bin/containerd-shim-custom-v2
        snapshotter: custom-snapshotter
```

Defining a runtime under a new environment moves that runtime from its previous
environment. One runtime may have only one active environment binding.

Snapshotter selection follows one rule:

```text
runtime binding snapshotter, when set
  -> otherwise environment containerd.defaultSnapshotter
```

The resolved value is written into the immutable Pool runtime plan. Fastlet,
image unpack, Sandbox create/delete, Infra artifact delivery, and NodeJanitor
must all consume that same resolved value.

The Pool Controller merges the selected environment with the built-in runtime
definition and creates an immutable per-Pool ConfigMap named like:

```text
<pool>-runtime-<revision>
```

The plan revision participates in the Fastlet Pod template hash. Updating the
platform ConfigMap therefore creates a new Fastlet generation and uses the
existing surge-and-drain lifecycle; running Sandboxes are not silently mutated.

NodeJanitor reads the same configuration at startup and scans every configured
containerd namespace. Restart the NodeJanitor DaemonSet after changing the
platform ConfigMap so cleanup and newly rolled Fastlets use the same environment
set.

The configuration is privileged platform input. Do not expose it as a
tenant-editable SandboxPool field: it controls node selectors, host paths,
runtime binaries and containerd namespaces.
