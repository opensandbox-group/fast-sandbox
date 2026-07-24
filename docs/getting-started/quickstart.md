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

Quick Start records the local Fastlet image ID on the Pool's Pod template. When
the `:dev` image changes, the Pool Controller performs its normal ready-surge
replacement instead of silently retaining a Fastlet from an earlier run.

Quick Start retains the cluster and Pool for interactive use.

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

Run the following commands in terminal 2:

```bash
bin/fastctl --endpoint localhost:9090 \
  --proxy-endpoint http://localhost:18080 \
  run quickstart-execd-sandbox \
  --image docker.io/library/alpine:latest \
  --pool quickstart-execd-pool -- /bin/sleep 3600

bin/fastctl --endpoint localhost:9090 list
bin/fastctl --endpoint localhost:9090 get quickstart-execd-sandbox
bin/fastctl --endpoint localhost:9090 \
  diagnostics sandbox quickstart-execd-sandbox
```

Create returns at `RuntimeReady`. The Controller projects CRD status and prepares the data plane asynchronously. Wait before calling Execd:

```bash
kubectl wait --for=jsonpath='{.status.dataPlaneState}'=Ready \
  sandbox/quickstart-execd-sandbox --timeout=60s
```

## Execute a command

```bash
bin/fastctl --endpoint localhost:9090 \
  --proxy-endpoint http://localhost:18080 \
  opensandbox exec quickstart-execd-sandbox -- \
  sh -lc 'printf "hello from execd\n" > /tmp/execd.txt && cat /tmp/execd.txt'
```

## Transfer files

```bash
printf 'hello from host\n' > /tmp/fast-sandbox-quickstart.txt

bin/fastctl --endpoint localhost:9090 \
  --proxy-endpoint http://localhost:18080 \
  opensandbox cp /tmp/fast-sandbox-quickstart.txt \
  quickstart-execd-sandbox:/tmp/from-host.txt

bin/fastctl --endpoint localhost:9090 \
  --proxy-endpoint http://localhost:18080 \
  opensandbox files stat quickstart-execd-sandbox /tmp/from-host.txt

bin/fastctl --endpoint localhost:9090 \
  --proxy-endpoint http://localhost:18080 \
  opensandbox files read quickstart-execd-sandbox /tmp/from-host.txt

bin/fastctl --endpoint localhost:9090 \
  --proxy-endpoint http://localhost:18080 \
  opensandbox cp quickstart-execd-sandbox:/tmp/execd.txt \
  /tmp/execd-downloaded.txt
```

## Delete the Sandbox

```bash
bin/fastctl --endpoint localhost:9090 delete quickstart-execd-sandbox
bin/fastctl --endpoint localhost:9090 list
```

Delete is declarative: Fast-Path submits deletion intent and the Controller completes route, runtime, network, and Infra cleanup before removing the finalizer.

## Select another runtime

```bash
make quickstart RUNTIME=container
make quickstart RUNTIME=gvisor
make quickstart RUNTIME=kata-qemu
make quickstart RUNTIME=kata-clh
```

Use `INFRA=minimal` only with `RUNTIME=container` to prepare a lifecycle-only Pool without exec or file operations:

```bash
make quickstart RUNTIME=container INFRA=minimal
```

gVisor setup installs and validates runsc. Kata QEMU and Cloud Hypervisor require nested KVM. Kata Firecracker and BoxLite have no Quick Start profile because their Fast Sandbox capability gates are not satisfied.

## Declarative creation

Fast-Path is optional. The Controller can create a Sandbox directly from a CRD:

```yaml
apiVersion: sandbox.fast.io/v1alpha1
kind: Sandbox
metadata:
  name: my-declarative-sandbox
spec:
  image: docker.io/library/alpine:latest
  poolRef: quickstart-execd-pool
  command: ["/bin/sleep"]
  args: ["3600"]
  failurePolicy: Manual
```

```bash
kubectl apply -f sandbox.yaml
kubectl get sandbox my-declarative-sandbox -w
```

## Troubleshooting

### The host cannot resolve the proxy Service

An address such as `fast-sandbox-proxy.default.svc` is an in-cluster Service. Keep `make quickstart-forward` running and pass:

```text
--proxy-endpoint http://localhost:18080
```

Lifecycle calls need only `--endpoint localhost:9090`. Execd calls need both endpoints.

### Create succeeded but exec is not ready

Inspect the independent runtime and data-plane states:

```bash
kubectl get sandbox quickstart-execd-sandbox \
  -o jsonpath='{.status.runtimeState}{" "}{.status.dataPlaneState}{"\n"}'
```

Wait for `dataPlaneState=Ready` before exec and file operations.

### Setup takes a long time

Inspect the cluster before rerunning setup:

```bash
kubectl get pods -A -o wide
kubectl get sandboxpool,sandbox
kubectl get deployment fast-sandbox-controller
```

See [Testing](../guides/testing.md) for automated validation and runtime prerequisites.

The development manifests contain public test signing material. Do not reuse them in production.
