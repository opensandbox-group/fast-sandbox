# Testing

Pure Go and generated-file checks can run on any supported development host. Kubernetes, kind, containerd, Linux networking, gVisor, Kata, BoxLite, and E2E validation require Linux.

## Public test interface

```bash
make verify
make test
make test SCOPE=race
make test SCOPE=network
```

`make generate` updates protobuf, Python protobuf, deepcopy, and CRD output. Generated output must be committed with its source change.

## E2E interface

Run every suite:

```bash
make e2e
```

Run one suite:

```bash
make e2e SUITE=controlplane
make e2e SUITE=proxy
make e2e SUITE=infra
make e2e SUITE=drain
make e2e SUITE=quickstart
```

Run runtime capability gates:

```bash
make e2e SUITE=runtime RUNTIME=container
make e2e SUITE=runtime RUNTIME=gvisor
make e2e SUITE=runtime RUNTIME=kata
make e2e SUITE=runtime RUNTIME=boxlite
```

A skipped runtime test is not a passing capability gate. BoxLite and Kata Firecracker gates prove fail-closed behavior until their requirements are satisfied.

## Suite coverage

| Suite | Coverage |
|---|---|
| `basicvalidation` | CRDs, namespace isolation, private networking, Proxy, Infra |
| `controlplane` | multi-active Fast-Path, leader election, idempotency, concurrent admission |
| `lifecycle` | create, delete, and graceful shutdown |
| `scheduling` | Pool selection, capacity, image affinity, autoscaling |
| `cliintegration` | fastctl lifecycle, diagnostics, and SDK adapters |
| `secureruntime` | container, gVisor, Kata, and BoxLite capability behavior |
| `drain` | scale-down, ready surge, and persisted drain |
| `faultrecovery` | Pod loss and generation fencing |
| `cleanupjanitor` | orphan cleanup backends |
| `advancedfeatures` | additional lifecycle behavior |

## Focused Go tests

E2E suites remain normal Go tests:

```bash
go test ./test/e2e/suites/controlplane/... \
  -run '^TestMultiActiveControlPlane$' -v -count=1

go test ./test/e2e/suites/basicvalidation/... \
  -run '^TestPoolWarmImagesReachRuntimeCacheInventory$' -v -count=1

go test ./test/e2e/suites/drain/... \
  -run '^TestPoolPlannedUpgradeUsesReadySurgeAndDurableDrain$' -v -count=1
```

## Environment preparation

Prepare a profile without running a test:

```bash
make env PROFILE=basic
make env PROFILE=gvisor
make env PROFILE=kata-qemu
make env PROFILE=kata-clh
make env PROFILE=kata-fc
```

## Manifest checks

```bash
kubectl kustomize config/default >/tmp/fast-sandbox-default.yaml
kubectl kustomize config/dev >/tmp/fast-sandbox-dev.yaml
kubectl kustomize config/all-in-one >/tmp/fast-sandbox-all-in-one.yaml
kubectl apply --dry-run=client --validate=false -k config/crd
kubectl apply --dry-run=client --validate=false -k config/default
kubectl apply --dry-run=client --validate=false \
  -f config/network-policy/default.yaml
```

Canonical samples should also pass server-side dry-run against a cluster with the CRDs installed.

## Reporting results

Record:

- exact command and exit status;
- commit SHA and whether uncommitted files were synchronized;
- cluster profile and runtime;
- key pass or failure output;
- skipped or unavailable gates and their reason;
- resources intentionally created or removed.

See [Performance](performance.md) for benchmark reporting.
