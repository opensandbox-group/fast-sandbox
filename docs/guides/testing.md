# Testing

Pure Go and generated-file checks can run on any supported development host. Kubernetes, kind, containerd, Linux networking, gVisor, Kata, Firecracker, and E2E validation require Linux.

## Public test interface

```bash
make verify
make test
make test SCOPE=race
make test SCOPE=network
```

`make generate` updates protobuf, Python protobuf, deepcopy, and CRD output. Generated output must be committed with its source change.

## E2E interface

Run every Kubernetes suite:

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
```

A skipped runtime test is not a passing capability gate. The Kata Firecracker
test is a positive runtime, network, Infra, proxy, and recovery test.

The Kubernetes suite also checks that a direct Firecracker pool without a
ready Fastlet heartbeat keeps sandboxes unassigned. This readiness check does
not boot or restore a Firecracker VM.

Direct Firecracker uses separate host-level suites, outside `make e2e`:
`scripts/firecracker-e2e.sh` validates the driver lifecycle, and
`scripts/firecracker-chain-e2e.sh` validates the builder, artifact store,
runtime-agent, and driver restore chain. These require root, `/dev/kvm`,
`/dev/net/tun`, and an XFS StateRoot with reflink enabled. See
[Firecracker runtime driver E2E](firecracker-runtime-e2e.md) and
[Firecracker full-chain E2E](firecracker-chain-e2e.md) for setup and commands.

## Suite coverage

| Suite | Coverage |
|---|---|
| `basicvalidation` | CRDs, namespace isolation, private networking, Proxy, Infra |
| `controlplane` | multi-active Fast-Path, leader election, idempotency, concurrent admission |
| `lifecycle` | create, delete, and graceful shutdown |
| `scheduling` | Pool selection, capacity, image affinity, autoscaling |
| `cliintegration` | fastctl lifecycle, diagnostics, and SDK adapters |
| `secureruntime` | container, gVisor, Kata (including Kata Firecracker), and direct Firecracker heartbeat gating |
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
make env PROFILE=kata-dragonball
```

The Kata profiles use the pinned upstream `kata-deploy` image as their artifact
source. gVisor defaults to the pinned `20260714` release and verifies upstream
checksums. Production installation and the independent RuntimeClass smoke gates
are covered by [Runtime node installation](runtime-node-installation.md).

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
