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

The default overlay contains:

- CRDs and RBAC;
- separate Fast-Path and Controller Deployments;
- Sandbox Proxy;
- Fast-Path and Proxy Services;
- PDB and HPA examples;
- NodeJanitor DaemonSet.

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

## SandboxPools

Start from the canonical samples under `config/samples`. A production Pool must define:

- fixed capacity and `maxSandboxesPerPod`;
- one immutable runtime;
- immutable per-Sandbox CPU, memory, and PID limits;
- one immutable InfraProfile;
- a platform-controlled Fastlet Pod template;
- optional warm images.

Runtime handlers, runtime paths, proxy sidecars, platform mounts, and security settings are platform-owned and cannot be overridden by the Pool template.

## NetworkPolicy

`config/network-policy/default.yaml` demonstrates ingress isolation for a single-namespace deployment. It is intentionally not included in `config/default`.

Before applying it:

1. label authorized control-plane and data-plane client Pods;
2. label Prometheus Pods that scrape administrative ports;
3. copy or adapt the Fastlet policy for every namespace containing a Pool;
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
