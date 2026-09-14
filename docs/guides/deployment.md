# Deployment

Fast Sandbox provides split-role production manifests, a development overlay, opt-in network policy examples, and canonical SandboxPool samples.

## Deployment shapes

| Overlay | Purpose | Control-plane topology |
|---|---|---|
| `config/default` | Production base | Multi-active Fast-Path plus leader-elected Controllers |
| `config/dev` | Development | Default topology plus public development route keys |
| `config/all-in-one` | Local development | One `--role=all` process without leader election |

The all-in-one overlay is not a production high-availability topology.

## Production base

Render and inspect the manifests:

```bash
kubectl kustomize config/default
```

Apply CRDs and the default deployment:

```bash
kubectl apply -k config/crd
kubectl apply -k config/default
```

`config/artifact-store` ships `fast-sandbox-artifact-store` (store root and
optional endpoint) and is included in `config/default`. Edit the ConfigMap for
the target environment; the controller and the runtime-agent mount it and read
it per reconcile/pull, so the change lands without a restart. A
`SandboxTemplate` that omits `spec.output.publish` inherits its `store`.

### Alpha API upgrade

`v1alpha2` is an explicit breaking alpha revision and this release does not
ship a conversion webhook for `v1alpha1` objects. Before upgrading a
production or shared cluster:

1. export any `v1alpha1` SandboxPool and Sandbox intent that must be retained;
2. drain or delete the corresponding runtime instances;
3. delete the two old CRDs only after verifying the export;
4. apply the `v1alpha2` CRDs and recreate the Pools and Sandboxes in canonical
   form.

Kubernetes rejects an in-place CRD apply while `v1alpha1` remains in
`status.storedVersions`. The project-owned Kind environments used by
`make env` and `make quickstart` detect this exact alpha boundary and
automatically reset incompatible CRDs. They also remove the old
default-namespace control-plane workloads before installing the split
namespaces, preventing a legacy host port from blocking NodeJanitor. These are
disposable development clusters; production deployment commands never delete
CRDs or legacy workloads automatically.

The default overlay creates two project-owned namespaces:

| Namespace | Resources |
| --- | --- |
| `fast-sandbox-system` | Controller, FastPath, Sandbox Proxy, NodeJanitor, ServiceAccounts, and RBAC |
| `fast-sandbox` | SandboxPools, Fastlet Pods, and Sandbox CRs |

The default overlay contains:

- CRDs and RBAC;
- separate Fast-Path and Controller Deployments;
- Sandbox Proxy;
- Fast-Path and Proxy Services;
- PDB and HPA examples;
- NodeJanitor DaemonSet.
- the platform-owned runtime environment ConfigMap.

Production deployments must create `fast-sandbox-route-keys` through a secret manager. Do not copy the public development key from `config/dev`.

## All-in-one development mode

```bash
kubectl apply -k config/all-in-one
```

This overlay removes the separate Fast-Path Deployment, HPA, and PDB. The Fast-Path Service selects the single all-in-one Pod.

Applying an overlay does not prune objects left by another overlay. When changing an existing split development deployment to all-in-one, explicitly delete the old Fast-Path Deployment, HPA, and PDB. Do not use a broad `--prune` operation.

## Runtime nodes

Fastlet and NodeJanitor are privileged node components. Production clusters should:

- isolate runtime nodes from general workloads;
- restrict who can create or modify SandboxPools;
- provide required host paths and KVM only on eligible nodes;
- apply runtime-specific node selectors and taints;
- protect route-signing private keys;
- monitor privileged Pods and host cleanup.

See [Secure runtimes](secure-runtimes.md) for gVisor and Kata prerequisites.
See [Runtime node installation](runtime-node-installation.md) for the
production Kata DaemonSet, gVisor node preparation, and runtime smoke gates.
See [Runtime environments](runtime-environments.md) when containerd or kubelet
uses non-default sockets, namespaces, roots, handlers, or node selectors.

## SandboxPools

Start from the canonical samples under `config/samples`. A production Pool must define:

- fixed capacity and `maxSandboxesPerPod`;
- one immutable runtime;
- immutable per-Sandbox CPU, memory, and PID limits;
- zero or more inline immutable Infra Components;
- a platform-controlled Fastlet Pod template;
- optional warm images.

Runtime handlers, runtime paths, proxy sidecars, platform mounts, and security settings are platform-owned and cannot be overridden by the Pool template.

## NetworkPolicy

`config/network-policy/default.yaml` demonstrates ingress isolation across the
default system and resource namespaces. It is intentionally not included in
`config/default`.

Before applying it:

1. label authorized control-plane and data-plane client Pods;
2. label Prometheus Pods that scrape administrative ports;
3. copy or adapt the Fastlet policy for every non-default resource namespace;
4. verify that the cluster CNI enforces NetworkPolicy;
5. define Sandbox egress according to DNS, registry, metadata, and tenant policy.

The example restricts ingress only.

## Validation

```bash
kubectl kustomize config/default >/tmp/fast-sandbox-default.yaml
kubectl kustomize config/dev >/tmp/fast-sandbox-dev.yaml
kubectl kustomize config/all-in-one >/tmp/fast-sandbox-all-in-one.yaml

kubectl apply --dry-run=client --validate=false -k config/crd
kubectl apply --dry-run=client --validate=false -k config/default
kubectl apply --dry-run=client --validate=false \
  -f config/network-policy/default.yaml
```

Use `kubectl apply -k config/crd`, not `kubectl apply -f config/crd/`.
