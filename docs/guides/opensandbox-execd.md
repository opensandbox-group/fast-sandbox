# OpenSandbox Execd

Fast Sandbox integrates OpenSandbox Execd as a Pool-defined Infra Component.
Fast Sandbox owns artifact delivery, process supervision, health, discovery,
and transparent routing. Execd and the official OpenSandbox SDK continue to own
command, file, PTY, SSE, and WebSocket semantics.

For the complete backend, readiness, Pool discovery, and direct-ingress model,
see [OpenSandbox integration](opensandbox-integration.md).

## Declare Execd

Use the complete sample:

```bash
kubectl apply -f config/samples/pool-container-execd.yaml
```

Its essential component definition is:

```yaml
infraComponents:
  - name: execd
    artifact:
      source:
        image:
          reference: sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd@sha256:1dc98c7de10b9a73450ac75aa0f200ad7972f2c40f5225f6a8998e166b45d6dd
      mappings:
        - sourcePath: /execd
          targetPath: /.fast/components/execd/execd
    process:
      command:
        - /.fast/components/execd/execd
        - --port
        - "44772"
      restartPolicy: OnFailure
      healthCheck:
        httpGet:
          path: /ping
        timeoutSeconds: 10
    endpoint:
      protocol: HTTP
      port: 44772
```

The OCI reference is immutable. Fastlet pulls it through the namespace Registry
configuration, verifies the digest, maps the selected file, and never starts
the artifact image as a separate container.

## Startup and readiness

```text
Fastlet prepares the Pool artifact revision
-> Runtime creates the Sandbox
-> sandbox-init starts Execd and the user process concurrently
-> RuntimeReady (optional early-return boundary)
-> Fastlet probes GET /ping locally
-> Fastlet Proxy acknowledges the named execd route
-> ComponentReady, DataPlaneReady, and aggregate Ready (default return)
```

The default Create observes aggregate Ready directly inside the assigned
Fastlet call. An adapter that explicitly requests RuntimeReady polls the live
`GetSandbox` view; it does not wait for CRD status projection.
If Execd later exits or becomes unhealthy, Fastlet revokes its route and
republishes it after restart and health recovery.

## Request path

The default central path is:

```text
OpenSandbox SDK
-> Sandbox Proxy
-> Fastlet Proxy
-> component execd
```

A trusted OpenSandbox ingress can request a direct route:

```text
OpenSandbox SDK
-> OpenSandbox ingress
-> Fastlet Proxy
-> component execd
```

Both routes use the logical name `execd`; clients do not need to know port
44772 for Fast Sandbox route selection. Proxies preserve the complete Execd
request and response protocol and perform no translation.

## Authentication

Execd is deliberately started without:

```text
EXECD_ACCESS_TOKEN
--access-token
X-EXECD-ACCESS-TOKEN
```

Fast Sandbox protects its internal route with the short-lived,
instance-fenced `X-Fast-Sandbox-Route-Credential`. OpenSandbox ingress may
independently enforce OpenSandbox secure access. Each gateway removes its own
credential before forwarding, so Execd receives neither platform credential
and user `Authorization` remains intact.

## fastctl

The adapter defaults to component `execd`. Endpoint resolution is non-blocking
and succeeds after aggregate Ready; a caller that requested the early
`RuntimeReady` Create boundary must poll `GetSandbox` or retry explicitly:

```bash
bin/fastctl opensandbox exec my-sandbox -- uname -a
bin/fastctl opensandbox cp ./input.txt my-sandbox:/tmp/input.txt
bin/fastctl opensandbox files stat my-sandbox /tmp/input.txt
```

For another component implementing the same Execd protocol:

```bash
bin/fastctl opensandbox exec my-sandbox \
  --component custom-execd -- uname -a
```

`--component` must appear before `--`; arguments after `--` belong to the
remote command.

## Runtime support

The inline component contract is runtime-neutral. Container, gVisor, Kata QEMU,
and Kata Cloud Hypervisor Quick Starts inject the same Execd definition.

Fast Sandbox does not ship envd or Rocklet. They can be added later as other
Pool components without changing the core routing protocol.
