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

三项跨架构边界的定向 Gate 可用于快速回归；它们仍然必须在最终完整 suite 之外执行或被完整 suite 覆盖：

```bash
# 真实 Controller-only：测试会把 FastPath Deployment 缩容为 0，再直接创建 CRD。
go test ./test/e2e/suites/controlplane/... -run '^TestMultiActiveControlPlane$' -v -count=1

# warmImages：断言实际 runtime cache inventory 和有界 success metric。
go test ./test/e2e/suites/basicvalidation/... -run '^TestPoolWarmImagesReachRuntimeCacheInventory$' -v -count=1

# 计划升级：replacement 完成 K8s/Runtime/Infra readiness 后才 Drain 旧 Fastlet。
go test ./test/e2e/suites/drain/... -run '^TestPoolPlannedUpgradeUsesReadySurgeAndDurableDrain$' -v -count=1
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
- `drain`: Pool scale-down、基于 template hash 的 ready-surge 滚动升级和持久化 drain semantics；
- `faultrecovery`: Pod loss and generation fencing;
- `cleanupjanitor`: orphan cleanup backends;
- `advancedfeatures`: additional lifecycle behavior.

Use `make help` for the exact target names and profile variables.

## Deployment manifest checks

Run on a host with `kubectl`:

```bash
kubectl kustomize config/default >/tmp/fast-sandbox-default.yaml
kubectl kustomize config/dev >/tmp/fast-sandbox-dev.yaml
kubectl kustomize config/all-in-one >/tmp/fast-sandbox-all-in-one.yaml
kubectl apply --dry-run=client --validate=false -k config/crd
kubectl apply --dry-run=client --validate=false -k config/default
kubectl apply --dry-run=client --validate=false -f config/network-policy/default.yaml
```

The `config/dev` overlay contains a public test key. Production tests must create `fast-sandbox-route-keys` from a secret manager and use `config/default`.

`config/all-in-one` is a development overlay built on `config/dev`: it renders one `--role=all` Controller Pod, routes the FastPath Service to it, and removes the separate FastPath Deployment/HPA/PDB. It is not a production HA topology.

Do not use `kubectl apply -f config/crd/`: that treats `kustomization.yaml` as a Kubernetes object. The e2e environment manager and manual workflows both use `kubectl apply -k config/crd`.

## Canonical manifest checks

```bash
kubectl apply --server-side --dry-run=server -f config/samples/pool.yaml
kubectl apply --server-side --dry-run=server -f config/samples/pool-gvisor.yaml
kubectl apply --server-side --dry-run=server -f config/samples/pool-kata.yaml
```

The samples and user manifests must use only the canonical CRD fields; no client-side migration command is provided.

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
