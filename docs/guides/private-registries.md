# Private Registries

Registry credentials are namespace-scoped platform configuration. They are not
stored on a SandboxPool and callers cannot pass credentials per Sandbox.

## Configure matching rules

Create the conventional ConfigMap in the resource namespace:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: fast-sandbox-registry
  namespace: fast-sandbox
data:
  registries.yaml: |
    registries:
      - host: ghcr.io
        repositoryPrefix: opensandbox
        secretRef:
          name: registry-ghcr
      - host: registry.example.com
        repositoryPrefix: team-a
        secretRef:
          name: registry-team-a
      - host: registry.example.com
        repositoryPrefix: team-b
        secretRef:
          name: registry-team-b
```

Each Secret must be a same-namespace
`kubernetes.io/dockerconfigjson` Secret:

```bash
kubectl create secret docker-registry registry-team-a \
  -n fast-sandbox \
  --docker-server=registry.example.com \
  --docker-username="$REGISTRY_USER" \
  --docker-password="$REGISTRY_PASSWORD"
```

See [`config/samples/registry-config.yaml`](../../config/samples/registry-config.yaml).

## Matching

Fast Sandbox normalizes the image reference and selects:

```text
exact Registry host
-> longest matching repository prefix
-> anonymous pull when no rule matches
```

This permits different credentials for different repositories under one
Registry host. All Pools in one namespace share the rules. Use another
Kubernetes namespace when two security domains need different policy for the
same repository.

## Reconciliation and rotation

The existing SandboxPool Controller watches the ConfigMap and referenced
Secrets. It validates the complete input and writes one stable-name compiled
Secret for each Pool. Fastlet Pods mount that Secret.

- invalid configuration leaves the last valid compiled Secret in place;
- a missing ConfigMap means anonymous pulls;
- Secret rotation does not expose credentials in Pool status;
- containerd workload pulls and OCI Infra artifact pulls atomically reload the
  projected file;
- failed warm-image pulls retry and Pool status reports bounded errors;
- Registry configuration does not make warm images a Fastlet Ready gate.

`RegistryReady=True` means the Controller validated the source configuration and persisted the compiled Secret. It does not acknowledge projection or loading by every Fastlet, or prove that the credentials can authenticate. Kubernetes projects Secret updates asynchronously, and credential lookups reload the projected file when its contents change.

Compiled registry configuration contains credentials only; no revision is generated, required, or reported in Fastlet status. Update the source ConfigMap and Secrets rather than editing Controller-managed compiled Secrets.

## Security

- FastPath, Sandbox CRs, and Pool reads never return Registry credentials.
- The Controller reads only same-namespace referenced Secrets.
- The compiled Secret is mounted read-only into the Fastlet.
- Registry credentials are used for workload images and OCI Infra artifact
  images; HTTPS archive authentication is not supported.
