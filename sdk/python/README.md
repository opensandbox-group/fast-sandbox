# Fast Sandbox Python SDK

The Python package owns Fast Sandbox lifecycle calls and generic Infra
Component route discovery. It does not implement Execd, Envd, or another
component's wire protocol.

## Install for development

```bash
python -m pip install -e 'sdk/python[dev]'
```

Run the package tests from the repository root:

```bash
make test SCOPE=python
```

## Lifecycle and route discovery

```python
from fast_sandbox import Client

with Client(
    endpoint="localhost:9090",
    proxy_endpoint="http://localhost:18080",  # optional port-forward override
) as client:
    sandbox = client.create(
        name="demo",
        image="python:3.12",
        command=["sleep", "3600"],
    )

    # Hand this route to the upstream component SDK. Fast Sandbox resolves and
    # authenticates it, but does not parse the service protocol.
    route = sandbox.resolve_endpoint(44772)
    print(route.endpoint, route.headers)
```

For OpenSandbox command and file operations, use `fastctl opensandbox` or the
official OpenSandbox SDK with the resolved endpoint and required headers.
FastPath and Fastlet Control expose no Exec/File RPC.

See:

- [Quick Start](../../docs/getting-started/quickstart.md)
- [API reference](../../docs/reference/api.md)
- [Data plane](../../docs/concepts/data-plane.md)
- [OpenSandbox Execd](../../docs/guides/opensandbox-execd.md)

When `opentelemetry-api` is installed (or the `telemetry` extra is selected),
the SDK injects the current W3C Trace Context into FastPath gRPC metadata. The
telemetry hook remains a no-op otherwise:

```bash
pip install 'fast-sandbox[telemetry]'
```
