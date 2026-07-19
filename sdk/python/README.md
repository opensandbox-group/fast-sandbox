# Fast Sandbox Python SDK

The SDK keeps lifecycle calls on FastPath and sends command/file traffic to an
injected component through Sandbox Proxy.

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

    result = sandbox.exec(["python", "-c", "print('hello')"])
    sandbox.files.write("/tmp/input.txt", b"hello")
    print(sandbox.files.read("/tmp/input.txt"))

    # E2B envd keeps its native Connect/protobuf protocol. Fast Sandbox only
    # hands the official E2B client a routed URL and required headers.
    envd = sandbox.resolve_envd()
    print(envd.base_url, envd.headers)
```

`Sandbox.exec` and `Sandbox.files` currently select the OpenSandbox Execd
adapter. No Exec/File RPC is exposed by FastPath or Fastlet Control.
