# Fast Sandbox testing guide

## Local and remote boundary

Local macOS execution is suitable for editing, formatting, generated-file checks when tools are available, and pure Go unit tests. Use a Linux remote development VM for anything involving Kubernetes, kind, containerd, CRI, Docker image loading, network namespaces/NAT, gVisor, Kata, BoxLite native runtime behavior, or end-to-end tests.

The repository's default remote workflow is documented in `AGENTS.md` and uses `ssh-fast:~/fast-sandbox` through the `remote-dev-run` helper.

## Static and unit gates

```bash
make verify          # regenerate/check protobuf, deepcopy, CRDs; then unit tests
make test-unit       # packages that do not require a live runtime
make test-race       # unit packages under the race detector
make test-python-sdk
go test ./test/performance/... # load-report logic; does not create cluster resources
```

If remote disk pressure makes the full race target impractical, split the same package set with `go test -race -p=1 ...` and record every command. A skipped package is not equivalent to a passing race gate.

## Generated contracts

```bash
make generate
make manifests
make verify-generated
```

Generated protobuf, deepcopy, and CRD output must be committed with its source change. `verify-generated` fails when tracked output is stale.

## Linux integration gates

```bash
make test-network-integration
make test-e2e-controlplane
make test-e2e-network
make test-e2e-proxy
make test-e2e-infra
make test-e2e-sdk
make test-e2e-drain
```

Runtime capability gates are explicit:

```bash
make test-e2e-runtime-container
make test-e2e-runtime-gvisor
make test-e2e-runtime-kata
make test-e2e-runtime-boxlite
```

A runtime cannot be marked supported because a test skipped. The gate must exercise the actual handler/sidecar and verify the advertised capability. In particular, BoxLite currently fails closed at the resource-enforcement capability boundary.

## Full e2e suites

`make test-e2e` runs suites that prepare their own profile. Individual suites include:

- `basicvalidation`: CRDs, namespace isolation, private networking, proxy and Infra augmentation;
- `controlplane`: direct Create against every Fast-Path replica, leader election, Service isolation, request idempotency, and cross-replica concurrent admission;
- `lifecycle`: create/delete and graceful shutdown;
- `scheduling`: Pool selection, private-port independence, capacity, and autoscaling;
- `cliintegration`: `fastctl` lifecycle and adapters;
- `secureruntime`: gVisor and Kata capability behavior;
- `drain`: Pool scale-down and durable drain semantics;
- `faultrecovery`: Pod loss and generation fencing;
- `cleanupjanitor`: orphan cleanup backends;
- `advancedfeatures`: additional lifecycle behavior.

Use `make help` for the exact target names and profile variables.

## Deployment manifest checks

Run on a host with `kubectl`:

```bash
kubectl kustomize config/default >/tmp/fast-sandbox-default.yaml
kubectl kustomize config/dev >/tmp/fast-sandbox-dev.yaml
kubectl apply --dry-run=client --validate=false -k config/crd
kubectl apply --dry-run=client --validate=false -k config/default
kubectl apply --dry-run=client --validate=false -f config/network-policy/default.yaml
```

The `config/dev` overlay contains a public test key. Production tests must create `fast-sandbox-route-keys` from a secret manager and use `config/default`.

Do not use `kubectl apply -f config/crd/`: that treats `kustomization.yaml` as a Kubernetes object. The e2e environment manager and manual workflows both use `kubectl apply -k config/crd`.

## Migration checks

```bash
go run ./cmd/fastctl migrate pool --file config/samples/pool.yaml --check
go run ./cmd/fastctl migrate pool --file config/samples/pool-gvisor.yaml --check
go run ./cmd/fastctl migrate pool --file config/samples/pool-kata.yaml --check
```

For migrated user manifests, also run `kubectl apply --server-side --dry-run=server` against the target cluster.

## Reporting results

Every remote or release validation report should include:

- exact command and exit status;
- commit SHA and whether uncommitted files were synced;
- cluster/profile and runtime capability;
- key pass/fail output;
- skipped or unavailable gates with reason;
- resources intentionally created or deleted during the run.

Performance-specific reporting requirements are in [PERFORMANCE.md](PERFORMANCE.md).
Trace propagation and OTLP validation are documented in [observability.md](observability.md).
