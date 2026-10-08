# Quick Start

This guide prepares a reusable kind environment and then lets you run lifecycle, diagnostics, exec, and file operations manually. Quick Start is an interactive product walkthrough; it does not run an E2E suite or create a Sandbox automatically.

## Prerequisites

Run Quick Start on a Linux host with:

- Go;
- Docker;
- kind;
- kubectl;
- GNU make.

The first run builds project images and prepares the selected runtime. It is substantially slower than later runs. Do not use a macOS host to validate containerd, networking, gVisor, or Kata behavior.

## Prepare the container environment

```bash
make quickstart
```

The default is equivalent to:

```bash
make quickstart RUNTIME=container INFRA=execd
```

It creates or reuses the `fsb-e2e-basic` kind cluster, builds and loads the
development images, deploys the control plane, creates
`quickstart-execd-pool`, waits for one ready Fastlet built from the current
development image, builds `bin/fastctl`, and prints copy-and-paste commands.

If `.fastctl/config.json` does not exist, Quick Start creates it with the local
Fast-Path and Sandbox Proxy endpoints. The directory is Git-ignored. If the
file already exists, Quick Start preserves it byte-for-byte and prints the
environment-variable overrides instead.

Quick Start records the local Fastlet image ID on the Pool's Pod template. When
the `:dev` image changes, the Pool Controller performs its normal ready-surge
replacement instead of silently retaining a Fastlet from an earlier run.

Quick Start retains the cluster and Pool for interactive use.

To exercise Sandbox Actions as well, enable the demo Handler:

```bash
make quickstart RUNTIME=container INFRA=execd ACTIONS=demo
```

This builds and loads a separate `sandbox-action-fixture` E2E image, declares
an `egress` Handler with both lifecycle Hooks, and prints commands that create,
inspect, update, and delete a Binding. The fixture is not included in the
Fastlet production image.

## Expose the endpoints

Keep the following command running in terminal 1:

```bash
make quickstart-forward
```

It owns two port-forwards:

- Fast-Path gRPC: `localhost:9090`;
- Sandbox Proxy: `http://localhost:18080`.

Ctrl-C stops both forwards.

## Create and inspect a Sandbox

On a fresh checkout, `make quickstart` has already configured `fastctl`.
Run the following commands in terminal 2:

```bash
bin/fastctl run quickstart-execd-sandbox \
  --image docker.io/library/alpine:latest \
  --pool quickstart-execd-pool -- /bin/sleep 3600

bin/fastctl list
bin/fastctl get quickstart-execd-sandbox
bin/fastctl diagnostics sandbox quickstart-execd-sandbox
```

Create defaults to aggregate `Ready`, evaluated inside the assigned Fastlet
call, so the returned Sandbox can immediately resolve `execd` without waiting
for CRD status projection or a separate `kubectl wait`.

## Execute a command

```bash
bin/fastctl opensandbox exec quickstart-execd-sandbox -- \
  sh -lc 'printf "hello from execd\n" > /tmp/execd.txt && cat /tmp/execd.txt'
```

## Transfer files

```bash
printf 'hello from host\n' > /tmp/fast-sandbox-quickstart.txt

bin/fastctl opensandbox cp /tmp/fast-sandbox-quickstart.txt \
  quickstart-execd-sandbox:/tmp/from-host.txt

bin/fastctl opensandbox files stat \
  quickstart-execd-sandbox /tmp/from-host.txt

bin/fastctl opensandbox files read \
  quickstart-execd-sandbox /tmp/from-host.txt

bin/fastctl opensandbox cp quickstart-execd-sandbox:/tmp/execd.txt \
  /tmp/execd-downloaded.txt
```

## Delete the Sandbox

```bash
bin/fastctl delete quickstart-execd-sandbox
bin/fastctl list
```

Delete is declarative: Fast-Path submits deletion intent and the Controller completes route, runtime, network, and Infra cleanup before removing the finalizer.

## Select another runtime

```bash
make quickstart RUNTIME=container
make quickstart RUNTIME=gvisor
make quickstart RUNTIME=kata-qemu
make quickstart RUNTIME=kata-clh
make quickstart RUNTIME=kata-fc
make quickstart RUNTIME=kata-dragonball
```

Use `INFRA=minimal` only with `RUNTIME=container` to prepare a lifecycle-only Pool without exec or file operations:

```bash
make quickstart RUNTIME=container INFRA=minimal
```

gVisor setup installs and validates runsc. Kata QEMU, Cloud Hypervisor,
Firecracker, and Dragonball require KVM; when the development host is itself a
VM, it must expose nested KVM. The Firecracker setup also installs a compatible
guest kernel and configures containerd's blockfile snapshotter.
## Declarative creation

Fast-Path is optional. The Controller can create a Sandbox directly from a CRD:

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: Sandbox
metadata:
  name: my-declarative-sandbox
  namespace: fast-sandbox
spec:
  image: docker.io/library/alpine:latest
  poolRef: quickstart-execd-pool
  command: ["/bin/sleep"]
  args: ["3600"]
  failurePolicy: Manual
```

```bash
kubectl apply -f sandbox.yaml
kubectl get sandbox my-declarative-sandbox -n fast-sandbox -w
```

## Troubleshooting

### The host cannot resolve the proxy Service

An address such as
`fast-sandbox-proxy.fast-sandbox-system.svc` is an in-cluster Service.
Keep `make quickstart-forward` running and configure its local endpoints:

```bash
export FAST_SANDBOX_ENDPOINT=localhost:9090
export FAST_SANDBOX_PROXY_ENDPOINT=http://localhost:18080
```

Command-line flags remain available and take precedence over these environment
variables. Lifecycle calls use the Fast-Path endpoint; Execd calls use both.

Quick Start never overwrites an existing `.fastctl/config.json`. You can
instead edit its `endpoint` and `proxy-endpoint` fields. When the file is
absent, custom forward ports are shared by configuration generation and the
printed forwarding command:

```bash
QUICKSTART_FASTPATH_PORT=19090 \
QUICKSTART_PROXY_PORT=18081 \
make quickstart

QUICKSTART_FASTPATH_PORT=19090 \
QUICKSTART_PROXY_PORT=18081 \
make quickstart-forward
```

### Create succeeded but exec is not ready

Inspect the independent runtime and data-plane states:

```bash
kubectl get sandbox quickstart-execd-sandbox \
  -o jsonpath='{.status.runtime.state}{" "}{.status.dataPlane.state}{" "}{.status.conditions[?(@.type=="Ready")].status}{"\n"}'
```

The `fastctl opensandbox` adapter waits for `execd` directly. A non-Ready
projection indicates that component health or local route publication should
be inspected; callers do not need to add a separate CRD wait.

### Setup takes a long time

Inspect the cluster before rerunning setup:

```bash
kubectl get pods -A -o wide
kubectl get sandboxpool,sandbox -n fast-sandbox
kubectl get deployment -n fast-sandbox-system
```

See [Testing](../guides/testing.md) for automated validation and runtime prerequisites.

The development manifests contain public test signing material. Do not reuse them in production.
