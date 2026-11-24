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

## FastPath caller authentication

FastPath supports optional caller authentication and verified TLS (`--fastpath-auth-enabled=true`, default `false`), requiring a Kubernetes TokenRequest token with the single audience `fast-sandbox-fastpath` on every native gRPC and gRPC-Web business call when enabled. Keeping `--fastpath-auth-enabled=false` by default preserves zero-prerequisite plaintext gRPC compatibility for existing deployments and local environments. Once enabled (`--fastpath-auth-enabled=true`), there is no automatic downgrade to plaintext or anonymous access. This is separate from the `fast-sandbox-fastlet` credential used on downstream calls.

When enabling caller authentication, the `config/manager/fastpath-auth.yaml` manifest provides a reference cert-manager `Certificate` referencing an **existing** `fast-sandbox-serving` ClusterIssuer, along with the caller ConfigMap, ServiceAccounts, and token-minter Roles. To keep `config/default` and `config/all-in-one` zero-prerequisite when `--fastpath-auth-enabled=false` (the default), `fastpath-auth.yaml` is not included in `config/manager/kustomization.yaml` by default and the `fastpath-callers` / `fastpath-tls` volumes are marked `optional: true`. Install cert-manager and provision your issuer separately; patch `config/manager/fastpath-auth.yaml` for your issuer, external serving DNS names, caller subjects, and resource namespaces, apply it, and pass `--fastpath-auth-enabled=true --fastpath-auth-policy-file=/etc/fast-sandbox/callers/callers.json --fastpath-tls-cert-file=/etc/fast-sandbox/tls/tls.crt --fastpath-tls-key-file=/etc/fast-sandbox/tls/tls.key`. Neither the overlays nor the binary create a CA. Serving certificates reload at each new TLS handshake; mounted files must not use `subPath`. Private keys stay on the serving side. Distribute public trust bundles through your existing trust-management process.

The caller ConfigMap maps exact ServiceAccount subjects (`opensandbox-server` and `opensandbox-ingress-gateway`) to `server` or `gateway` RPC profiles and explicit namespaces. Empty namespace scopes and empty request namespaces are rejected; `["*"]` deliberately grants all namespaces. Gateway can only resolve endpoints, read sandboxes, and resume sandboxes. Change the policy and roll out every FastPath Pod to revoke a subject; authorization is checked on every RPC, not stored in the identity cache. Explicitly bind the matching named `*-token-minter` Role to the actual Pod ServiceAccount or kubeconfig identity; the standalone overlay cannot infer external kubeconfig identities and creates no such bindings.

Clients request 3,600-second tokens. The verifier independently requires numeric `iat` and `exp`, `0 < exp - iat <= 3600`, at most 30 seconds of future `iat` skew, and strict expiry without a grace period. It accepts only the deployment's issuer and same-origin JWKS endpoint. In addition to throttled unknown-key refresh, active traffic refreshes discovery/JWKS after five minutes; cached keys older than fifteen minutes fail closed. Rebuilding the verifier discards cached identity verdicts for removed keys. Removing TokenRequest permission stops new issuance but does not invalidate already issued tokens.

When `--auth-enabled=true` is set (default `false`), `fastctl` uses TLS and obtains its caller token for `opensandbox-server` through the Kubernetes client using `--kubeconfig` or default kubeconfig/in-cluster loading rules (resolving the identity namespace from `POD_NAMESPACE`, defaulting to `fast-sandbox-system`). Configure `--tls-ca-file` and, for a port-forward, `--tls-server-name` matching a certificate DNS name. These authentication settings are CLI flags, not persisted tokens. TokenRequest requires verified HTTPS to the Kubernetes API; plaintext endpoints and `insecure-skip-tls-verify` are rejected. Python SDK users default to plaintext (`auth_enabled=False`) or supply `auth_enabled=True` with a verified `grpc.Channel` or composite `grpc.ChannelCredentials` whose metadata callback obtains a short-lived caller token. Credential rotation belongs in that callback and must not forward the kubeconfig bootstrap token.

For a smooth rolling upgrade, roll out the new binaries with the default disabled setting (`--fastpath-auth-enabled=false` and client `auth-enabled=false`), provision cert-manager certificates, CA bundles, and TokenRequest RBAC, and then enable authenticated TLS mode across FastPath and its callers. The OpenSandbox Server and ingress gateway implement the automatic TokenRequest cache/refresh lifecycle. Cluster-external Server uses its existing kubeconfig; the gateway remains in-cluster.

## Control authentication rollout

Fastlet control calls carry audience-bound ServiceAccount tokens, and Fastlets reject unauthenticated callers. The two control-plane Deployments (Fast-Path and Controller) roll independently of the Fastlet pools, so a version upgrade that enables control authentication must be sequenced: the window where an old Fast-Path calls a new Fastlet fails with 401, and a new Fast-Path without the token-mint RBAC fails closed against every Fastlet. The order below keeps every mixed-version window functional.

The same sequence applies whether the manifests come from `config/default` or the Helm chart; only the apply command differs.

1. Apply the RBAC grants and verify token minting before any image roll. The grants ship in `config/rbac` (included by `config/default`) or in the chart's RBAC templates.

```bash
kubectl apply -k config/rbac
kubectl auth can-i create serviceaccounts/token -n fast-sandbox-system \
  --as=system:serviceaccount:fast-sandbox-system:fast-sandbox-fastpath
```

The preflight must answer `yes`. A `no` here means a rolled Fast-Path would fail closed on every Fastlet call.

2. Roll Fast-Path and wait for completion. New Fast-Path replicas send tokens on every control call; Fastlets still on the old image ignore the header.

```bash
kubectl -n fast-sandbox-system set image deployment/fast-sandbox-fastpath \
  manager=fast-sandbox/controller:<version>
kubectl -n fast-sandbox-system rollout status deployment/fast-sandbox-fastpath --timeout=5m
```

3. Roll the Controller and wait for completion. The new Controller injects the issuer anchor, the caller allow list, and the audience into every Fastlet Pod; the injected environment changes the Pod template hash, so each pool rolls itself with normal Pod replacement.

```bash
kubectl -n fast-sandbox-system set image deployment/fast-sandbox-controller \
  manager=fast-sandbox/controller:<version>
kubectl -n fast-sandbox-system rollout status deployment/fast-sandbox-controller --timeout=5m
```

4. Wait for the Fastlet pools to finish rolling.

```bash
kubectl -n fast-sandbox wait --for=condition=Ready pod \
  -l fast-sandbox.io/runtime --timeout=10m
```

Verify on the Fastlet Pods: the startup log `Fastlet control authentication enabled`, and the absence of TokenRequest `Forbidden` errors on the control plane. Authentication rejections are visible in the Fastlet's own spans (`http.response.status_code` 401/403) once tracing is exported.

### Rollback

Roll back in reverse: pools first, then the control plane.

1. Revert the Fastlet image in each SandboxPool template and wait for the pools to settle. The previous Fastlet ignores the injected authentication environment, and tokens sent by the still-new control plane are accepted but not verified.

2. Revert the Controller and Fast-Path Deployments (either order) and wait for both rollouts.

3. The RBAC grants are harmless to leave in place.

Do not revert the Controller while pools still run the new Fastlet image: the new Fastlet fails startup without the injected issuer anchor and crash-loops until the Controller is rolled forward again.

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
