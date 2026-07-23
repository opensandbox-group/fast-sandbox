# Fast Sandbox Python SDK

The SDK owns Fast Sandbox lifecycle calls and generic route discovery. It does
not implement Execd, Envd, or another Infra Component's wire protocol.

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

For OpenSandbox command and file operations, use `fastctl opensandbox` or an
official OpenSandbox SDK with the resolved endpoint and required headers. No
Exec/File RPC is exposed by FastPath or Fastlet Control.

When `opentelemetry-api` is installed (or the `telemetry` extra is selected),
the SDK injects the current W3C Trace Context into FastPath gRPC metadata. The
telemetry hook remains a no-op otherwise:

```bash
pip install 'fast-sandbox[telemetry]'
```
