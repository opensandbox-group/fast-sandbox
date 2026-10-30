# Sandbox Actions

This concept defines the Pod-local Handler protocol, ordering, readiness,
retry, and recovery rules for `sandbox.fast.io/v1alpha2`. Exact public fields
are listed in the [API reference](../reference/api.md).

## 1. Model

Sandbox Actions let a Pool provide Pod-local extension processes and let each
Sandbox bind ordered, Handler-owned configuration to those processes.

The protocol has two orthogonal parts:

- **Binding synchronization** carries user configuration through `SetBinding`.
- **Lifecycle Hooks** notify a bound Handler that a Fastlet-local checkpoint
  has been reached.

Binding changes are not lifecycle events. The protocol therefore does not use
`CREATE`, `UPDATE`, and `DELETE` to represent both concepts.

The names `ActionHandler` and `ActionBinding` are retained for the first
version. They refer to a Fastlet-Pod extension and a per-Sandbox binding, not a
plugin running inside the Sandbox.

## 2. Public desired state

### 2.1 Pool Handler declaration

```yaml
spec:
  actionHandlers:
  - name: egress
    targetHTTPPort: 18080
    hooks:
    - sandbox.runtime-ready
    - sandbox.data-plane-ready
```

`targetHTTPPort` is always reached at `127.0.0.1` from the Fastlet container.
The Handler process itself belongs in `fastletTemplate` as a sidecar or another
Pod-local process.

`hooks` is the authoritative subscription list. An empty list defines a
config-only Handler that receives `SetBinding` and `RemoveBinding`, but no
Hooks. Version 1 supports exactly:

- `sandbox.runtime-ready`: `runtime.Driver.EnsureSandbox` succeeded. The
  runtime and its private network identity are available.
- `sandbox.data-plane-ready`: Infra Components are ready and the sandbox-proxy
  route has been published.

`LifecycleHook` is a string alias for future extensibility, but a Fastlet must
reject an unknown Hook in Pool configuration; it must never silently ignore it.
There is no `sandbox.ready` Hook because Hook success contributes to overall
Ready, and no `sandbox.before-delete` because terminal cleanup is already
represented by `RemoveBinding`.

### 2.2 Sandbox Binding declaration

```yaml
spec:
  actionBindings:
  - handler: audit
    input: |-
      sink: security-log
  - handler: egress
    input: '{"default":"deny"}'
```

The array is atomic and ordered. The Handler name must exist in the referenced
Pool and may occur only once. Forward setup and Hook delivery use declaration
order; live removal and terminal cleanup use reverse order.

`input` is an opaque UTF-8 string. Fast Sandbox validates only the Handler
reference, uniqueness, item count, and byte limits. It does not parse JSON,
canonicalize whitespace, reorder keys, or interpret `"null"`. YAML, DSL,
base64, ordinary strings, and the empty string are all valid Handler-owned
formats.

## 3. Handler HTTP protocol

The Handler exposes two Pod-loopback endpoints:

- `GET /_fastlet/v1/actions/status`
- `POST /_fastlet/v1/actions`

### 3.1 Status and process incarnation

```json
{
  "apiVersion": "sandbox.fast.io/actions/v1",
  "ready": true,
  "instanceId": "handler-process-7"
}
```

`instanceId` identifies one Handler process incarnation. It is unrelated to a
Sandbox runtime instance. A changed value means the Handler may have lost all
local state, so Fastlet invalidates affected Binding readiness and replays the
latest `SetBinding`, followed by all subscribed Hooks already reached by that
Sandbox.

### 3.2 Common request envelope

```json
{
  "apiVersion": "sandbox.fast.io/actions/v1",
  "operation": "SET_BINDING",
  "invocationId": "sha256:...",
  "sandbox": {
    "uid": "...",
    "name": "sandbox-a",
    "namespace": "tenant-a"
  },
  "revision": {
    "specGeneration": 4,
    "runtimeInstanceId": "...",
    "attachmentId": "sha256:...",
    "routeGeneration": 2
  },
  "attachment": {
    "network": {
      "ip": "10.42.0.8",
      "gateway": "10.42.0.1",
      "privateCidr": "10.42.0.0/24",
      "hostVeth": "veth..."
    }
  },
  "binding": {
    "input": "{\"default\":\"deny\"}"
  }
}
```

HTTP 200 is success. Any other status, transport error, or timeout is a failed
attempt. Calls are at-least-once; a retry of the same logical operation reuses
the same `invocationId`, and the Handler must be idempotent.

### 3.3 SetBinding

`SET_BINDING` synchronizes the complete desired value:

- `binding.input` is non-null for create/update. `""` and the literal string
  `"null"` are ordinary values.
- JSON `"input": null` means this Binding was removed from a still-live
  Sandbox. It is a Ready barrier and is retried until it succeeds.

Success means the Handler both stored the value and made it effective for the
Sandbox's current lifecycle state. Therefore an ordinary input update on an
already-ready Sandbox does not replay Hooks.

### 3.4 Lifecycle Hook

```json
{
  "operation": "LIFECYCLE_HOOK",
  "hook": {
    "name": "sandbox.data-plane-ready",
    "sequence": 2
  }
}
```

A Hook never repeats user input. For a Handler and generation, the current
`SetBinding` must succeed before any reached Hook is sent. If the checkpoint is
reached first, Fastlet records it as pending and later delivers Hooks in
sequence order.

### 3.5 RemoveBinding

```json
{
  "operation": "REMOVE_BINDING"
}
```

`REMOVE_BINDING` is terminal cleanup for the whole Sandbox/runtime identity. It
has no `binding` or `hook` payload and is distinct from `SetBinding(null)`.
Missing Handler state is success.

### 3.6 Strict Actions v2 transport

The experimental transport is selected by the platform administrator through the existing `fast-sandbox-runtime-environments` ConfigMap's top-level `actionsApiVersion` setting. It is not selected by a Pool, Sandbox, or Create request. The controller carries the value in the content-addressed runtime plan; Fastlet stamps it into durable runtime metadata and requires the same version during recovery. An omitted setting preserves v1. With v2 configured, only the direct Firecracker runtime is supported, v2 uses separate `/_fastlet/v2` management and relay paths with the complete owner tuple (Sandbox UID, Fastlet Pod UID, runtime instance, generation, assignment attempt, route generation, and attachment ID), and no v1 fallback is attempted. Every egress Handler must independently be configured with `OPENSANDBOX_EGRESS_ACTIONS_API_VERSION=sandbox.fast.io/actions/v2`.

This is an experimental management transport for legacy Vault and Policy CRUD. It does not enable credential-bound TLS interception or Vault v2 writes. Explicit snapshot, pause/resume, and restore operations are rejected before their runtime effects. Only verified clean-base images are supported: ordinary images with template carry are not universally rejected before VM startup; carry admission hardening is deferred. Management owner fields are fencing data, not caller authentication. The listener is Pod-netns loopback and may be reachable by other containers in that Pod; do not enable v2 until deployment-specific management-port isolation has been verified. This release adds no authentication or network policy for that endpoint.

## 4. Ordering and concurrency

For one Sandbox and Handler, Fastlet serializes `SetBinding`, Hook, and
`RemoveBinding`. Different Sandboxes may execute concurrently.

Across Handlers:

- setup and each Hook use current Binding declaration order;
- removed live Bindings are cleared in reverse old-list order;
- terminal `RemoveBinding` uses reverse current order.

State commits are fenced by Sandbox spec generation, runtime/attachment
identity, Handler `instanceId`, Hook sequence, and the logical invocation
identity. A delayed response from an obsolete operation cannot make the new
state Ready.

## 5. Ready contract

One Binding is Ready only when:

1. its current opaque input has completed `SetBinding`; and
2. every subscribed Hook reached by the current Sandbox has succeeded for the
   current Handler incarnation.

Overall Sandbox Ready is:

```text
Runtime Ready
&& DataPlane Ready
&& every Infra Component Ready
&& every current Action Binding Ready
&& Fastlet applied generation == current Sandbox generation
```

Handler failure does not stop runtime creation, Infra initialization, or route
publication. It keeps overall Ready false and Fastlet retries locally. Public
Status exposes only aggregate Binding state and diagnostics; per-Hook history,
pending invocation IDs, Handler incarnation, digests, and retry state stay
inside Fastlet.

`CreateCompletion` selects the synchronous creation boundary:

- `READY` (and protobuf `UNSPECIFIED`) waits on the complete local predicate.
- `RUNTIME_READY` returns after runtime creation; data plane and Actions may
  still be converging.

There is no public or internal `WaitSandboxReady` RPC. Fastlet's Create path
waits on its own state notification channel, so it does not wait for a CRD
watch/status round trip.

## 6. Recovery ownership

Fastlet is the only Lifecycle Hook dispatcher. Sandbox Controller sends only
the complete persisted Binding list when a generation has not been accepted or
a restarted Fastlet needs rehydration; it never infers a Hook from CRD Status.

On Fastlet restart, runtime recovery reconstructs reached checkpoints. After
Controller rehydrates desired Bindings, Fastlet sends `SetBinding` and then the
subscribed reached Hooks.

On Handler restart, Fastlet's single probe owner detects the new `instanceId`,
invalidates affected readiness, and runs the same replay locally.

## 7. Deletion and failure handling

Fastlet first marks the Sandbox `Terminating`, which prevents new Binding or
Hook work. It then attempts `RemoveBinding` in reverse order under one fixed
five-second total deadline shared by all Handlers. Handler errors are recorded
but never block route, runtime, or network teardown.

If runtime creation fails after Binding work started, Fastlet attempts runtime
and Handler cleanup. A cleanup failure retains the local identity as
`create-cleanup-failed`; a retry of the same identity resumes cleanup before
creation. A user-requested delete has a distinct terminal state and cannot be
resurrected by a delayed Create retry.

## 8. Security

- Handler endpoints are fixed Pod-loopback HTTP endpoints; users cannot supply
  a host, path, method, or headers.
- Pool administrators control Handler containers, ports, and subscriptions.
- Sandbox users may choose only declared Handler names and opaque values.
- Input is persisted in the Sandbox CRD and is not a secret transport. Secret
  material needs a separate reference/credential design.
- Inputs and diagnostics must not be logged by the platform by default.

Strict v2 requests carry `mode: legacy`. An absent egress owner requires REMOVE to persist a retirement fence before runtime teardown; absence alone is not proof of retirement. Dead Firecracker recovery retires the same persisted owner before releasing resources, and preserves state on cleanup failure so recovery can retry. Same-UID rebind and live binding removal are unsupported in this slice.
