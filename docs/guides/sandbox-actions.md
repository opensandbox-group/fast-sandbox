# Sandbox Actions guide

Sandbox Actions deliver ordered, per-Sandbox configuration to Action Handlers
running in the Fastlet Pod and notify them at two local lifecycle checkpoints.
A Handler is a Pool-managed Pod-local extension, not a plugin running inside
the user's Sandbox.

For the protocol invariants, see the [Sandbox Actions concept](../concepts/sandbox-actions.md).

## Quick start

Enable the demo Handler in the development environment:

```bash
make quickstart RUNTIME=container INFRA=minimal ACTIONS=demo
```

This builds the standalone `sandbox-action-fixture` E2E image, creates a Pool
with an `egress` Handler, and prints commands for creating, updating,
inspecting, and deleting a Sandbox. The fixture is not included in the
production Fastlet image.

## Declare a Handler in SandboxPool

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: SandboxPool
metadata:
  name: actions-pool
spec:
  capacity: {poolMin: 1, poolMax: 1, bufferMin: 0, bufferMax: 1}
  maxSandboxesPerPod: 8
  runtime: container
  sandboxResources:
    cpu: 500m
    memory: 512Mi
    pids: 128
  actionHandlers:
  - name: egress
    targetHTTPPort: 18080
    hooks:
    - sandbox.runtime-ready
    - sandbox.data-plane-ready
  fastletTemplate:
    spec:
      containers:
      - name: fastlet
        image: fast-sandbox/fastlet:dev
      - name: egress-handler
        image: example.com/egress-handler:v1
        readinessProbe:
          httpGet:
            host: 127.0.0.1
            port: 18080
            path: /_fastlet/v1/actions/status
```

Handler names and ports must be unique within a Pool. Fastlet always connects
to `targetHTTPPort` through Pod loopback. An empty `hooks` list creates a
config-only Handler that receives Binding synchronization and terminal cleanup.

The strict transport is experimental and supports only the direct Firecracker runtime in this release. Platform administrators select it in the existing `fast-sandbox-runtime-environments` ConfigMap with the top-level setting `actionsApiVersion: sandbox.fast.io/actions/v2`; omitting the setting preserves v1. Pool and Sandbox callers cannot select or override the protocol. The controller includes the mode in the immutable runtime plan and Fastlet records it in durable runtime metadata. The egress process must separately set `OPENSANDBOX_EGRESS_ACTIONS_API_VERSION=sandbox.fast.io/actions/v2`. A protocol mismatch fails closed with no v1 fallback. Verify isolation of the Pod-local management listener before enabling v2: owner fields fence stale runtime identity but do not authenticate callers, and other containers in the Pod may be able to reach loopback. V2 currently supports legacy-mode SET/Hook/REMOVE and Vault/Policy relay calls only; explicit snapshot, pause/resume, and restore operations fail before their runtime effects. Use verified clean-base images only; ordinary images with template carry are not universally rejected before VM startup. This is not credential-bound TLS interception.

Strict-mode admission probes every declared Handler concurrently under one shared five-second deadline before image delivery or runtime effects, including for Sandboxes with no Bindings. Handler latency and outages therefore affect Create latency and availability; enablement should include monitoring and alerting on Handler p99 latency and probe failures. Retirement also covers every declared Handler and retains runtime resources and capacity until confirmed. A failed probe cancels its peers, and admission waits for all probes to finish before returning. The shared timeout is unchanged; tuning it requires deployment latency measurements. Image delivery must not start before every fence check succeeds.

Do not switch Actions protocol versions on a Fastlet with existing runtime state. A recovered owner whose persisted protocol differs from the configured manager blocks recovery and keeps the node unavailable. This deliberately retains the owner fence and resources rather than silently skipping retirement. Recover with the matching protocol and compatible Handlers, complete cleanup, then enable the new protocol on a clean deployment. Per-Sandbox quarantine without blocking unrelated recovery is deferred.

The supported Hooks are:

- `sandbox.runtime-ready`: runtime creation completed and the private network
  identity is available;
- `sandbox.data-plane-ready`: Infra Components are healthy and the proxy route
  is published.

Unknown Hooks are rejected rather than ignored.

## Implement the Handler HTTP protocol

Expose the status endpoint:

```text
GET /_fastlet/v1/actions/status
```

```json
{
  "apiVersion": "sandbox.fast.io/actions/v1",
  "ready": true,
  "instanceId": "egress-process-7"
}
```

`instanceId` identifies one Handler process incarnation and must change after
a process restart. Fastlet then replays the latest `SetBinding`, followed by
the subscribed Hooks that the Sandbox has already reached.

Accept calls at:

```text
POST /_fastlet/v1/actions
Content-Type: application/json
```

Example `SetBinding`:

```json
{
  "apiVersion": "sandbox.fast.io/actions/v1",
  "operation": "SET_BINDING",
  "invocationId": "sha256:...",
  "sandbox": {
    "uid": "7f...",
    "name": "agent-a",
    "namespace": "default"
  },
  "revision": {
    "specGeneration": 3,
    "runtimeInstanceId": "runtime-2",
    "attachmentId": "sha256:...",
    "routeGeneration": 4
  },
  "attachment": {
    "network": {
      "ip": "172.30.0.2",
      "gateway": "172.30.0.1",
      "privateCidr": "172.30.0.0/24",
      "hostVeth": "fh..."
    }
  },
  "binding": {
    "input": "{\"defaultAction\":\"deny\"}"
  }
}
```

The operations are deliberately separate:

- `SET_BINDING` synchronizes the complete input. A present input may be `""`
  or the literal string `"null"`; JSON `null` removes a Binding from a live
  Sandbox.
- `LIFECYCLE_HOOK` announces a reached checkpoint without repeating input.
- `REMOVE_BINDING` performs terminal cleanup for the complete Sandbox and has
  no input.

Only HTTP 200 means success. Delivery is at-least-once, and retries for the
same logical operation reuse `invocationId`; the Handler must be idempotent.
A successful `SetBinding` means the configuration is already effective at the
Sandbox's current lifecycle stage, so an ordinary input update does not replay
Hooks.

## Create a Sandbox with Bindings

The CRD uses an ordered atomic array:

```yaml
apiVersion: sandbox.fast.io/v1alpha2
kind: Sandbox
metadata:
  name: agent-a
spec:
  poolRef: actions-pool
  image: python:3.13
  command: ["python", "-m", "http.server", "8080"]
  actionBindings:
  - handler: egress
    input: '{"defaultAction":"deny"}'
```

`input` is an opaque string. These are distinct, byte-preserved values:

```yaml
input: ""
input: "null"
input: |-
  default: deny
  allow: [example.com]
```

The equivalent Fastctl command is:

```bash
fastctl run agent-a \
  --pool actions-pool \
  --image python:3.13 \
  --action 'egress={"defaultAction":"deny"}'
```

FastPath Create has two completion boundaries:

- omitted or `READY`: return after Runtime, DataPlane, Infra Components, and
  all Bindings are Ready;
- `RUNTIME_READY`: return after runtime creation while later stages continue
  converging.

Waiting happens inside Fastlet and does not require a CRD Status watch. There
is no `WaitSandboxReady` RPC.

## Observe readiness

```yaml
status:
  observedGeneration: 1
  runtime:
    state: Ready
    generation: 1
  dataPlane:
    state: Ready
    routeGeneration: 1
  actionBindings:
  - handler: egress
    state: Ready
  conditions:
  - type: Ready
    status: "True"
    observedGeneration: 1
```

A Binding is Ready when its current input has completed `SetBinding` and all
subscribed, reached Hooks have succeeded. Handler failure does not roll back an
already-created runtime, Infra Component, or route, but aggregate Ready remains
false while Fastlet retries locally.

FastPath `GetSandbox` asks the assigned Fastlet for the live `SandboxInfo`.
`UpdateSandbox` success only commits desired state and returns
`committed_generation`; it does not wait for Handlers. After an update, poll
Get until the response `generation` and live `applied_generation` both equal
`committed_generation`, aggregate `ready` is true, and the relevant Bindings
are Ready. A greater response generation means a later update superseded the
generation being checked.

## Update or remove Bindings

An update replaces the complete ordered array:

```bash
fastctl update agent-a \
  --action 'audit=tenant=demo' \
  --action 'egress={"defaultAction":"allow"}'
```

Or patch the CRD directly:

```bash
kubectl patch sandbox agent-a --type=merge -p='{
  "spec":{"actionBindings":[
    {"handler":"egress","input":"{\"defaultAction\":\"allow\"}"}
  ]}
}'
```

New and retained Bindings use the new declaration order. Removed live Bindings
are cleared in reverse old-list order with `SetBinding(input=null)`, which is a
Ready barrier and is retried until successful.

Clear all Bindings with:

```bash
fastctl update agent-a --clear-actions
```

This is not terminal `RemoveBinding`; the Sandbox remains live and may bind the
Handler again later.

## Restart and deletion behavior

- Set and Hook delivery follows Binding declaration order; live removal and
  terminal cleanup use reverse order.
- One Sandbox/Handler operation stream is serialized; different Sandboxes may
  progress concurrently.
- A Hook reached before the current generation's Set succeeds remains Pending
  and is delivered afterward.
- Handler restart replay is owned by Fastlet. Fastlet restart rehydration gets
  desired Bindings from the Controller, but Hooks are still dispatched only by
  Fastlet from local checkpoints.

On Sandbox deletion, Fastlet first marks it Terminating, blocks new Set/Hook
work, and sends reverse-order `RemoveBinding`. All Handlers share one fixed
five-second deadline. Handler failures are diagnostic only and do not block
route, runtime, or network teardown.

## ResolveEndpoint

`ResolveEndpoint` does not wait. It reads live state from the assigned Fastlet,
requires aggregate Ready and a Ready target route, then signs a short-lived
credential fenced by `routeGeneration`. `FailedPrecondition` means the caller
should retry with backoff.

A synchronized cache avoids Kubernetes API-server IO. If the requested
`expected_generation` is ahead of the local cache, the service performs one
direct GET.

## Test

Run the focused unit tests:

```bash
go test ./internal/fastlet/action \
  ./internal/fastlet/sandbox \
  ./internal/controlplane/orchestrator \
  ./internal/controlplane/reconciler \
  ./internal/controlplane/fastpath
```

Run the Actions E2E suite:

```bash
make e2e SUITE=sandboxactions E2E_TEST_TIMEOUT=15m
```

The suite covers opaque and empty inputs, the literal `"null"`, declaration
order, both Hooks, Ready barriers, update/removal, Handler and Fastlet restart
replay, and reverse terminal cleanup.
