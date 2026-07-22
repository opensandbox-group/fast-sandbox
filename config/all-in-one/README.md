# All-in-one control plane

This overlay is the development deployment shape for running
one `controller` process with `--role=all`. The process serves the FastPath gRPC
API and runs Sandbox/SandboxPool reconcilers without leader election. The
existing `fast-sandbox-fastpath` Service selects that one Pod.

It intentionally removes the separate FastPath Deployment, HPA, and PDB. It is
not a production HA topology and includes the public development route key via
`config/dev`.

```bash
kubectl apply -k config/all-in-one
```

Use that command for a clean development installation. Kustomize delete patches
remove split-role objects from the rendered manifest; plain `kubectl apply` does
not prune matching objects left by an earlier `config/default` installation.
Changing an existing split development deployment to all-in-one therefore requires
explicitly deleting the old FastPath Deployment/HPA/PDB;
do not use broad `--prune` flags as a shortcut.

Production deployments must use `config/default`, separate the Controller and
FastPath roles, and provide `fast-sandbox-route-keys` from a secret manager.
