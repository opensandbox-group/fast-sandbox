from __future__ import annotations

import json
import tempfile
import unittest
from unittest import mock
from urllib.parse import parse_qs

import httpx

from fast_sandbox import Client
from fast_sandbox import telemetry
from fast_sandbox.proto import fastpath_pb2


class FakeFastPathStub:
    def __init__(self):
        self.create_request = None
        self.resolve_requests = []
        self.metadata = []

    def CreateSandbox(self, request, metadata=()):
        self.metadata.append(tuple(metadata))
        self.create_request = request
        return fastpath_pb2.CreateResponse(
            sandbox_uid="uid-a", sandbox_name=request.name
        )

    def GetSandbox(self, request, metadata=()):
        self.metadata.append(tuple(metadata))
        return fastpath_pb2.SandboxInfo(sandbox_uid="uid-a", sandbox_name=request.sandbox_name)

    def DeleteSandbox(self, _request, metadata=()):
        self.metadata.append(tuple(metadata))
        return fastpath_pb2.DeleteResponse(success=True)

    def ResolveEndpoint(self, request, metadata=()):
        self.metadata.append(tuple(metadata))
        self.resolve_requests.append(request)
        return fastpath_pb2.ResolveEndpointResponse(
            sandbox_uid=request.sandbox_uid,
            target_port=request.target_port,
            proxy_endpoint=(
                f"http://sandbox-proxy.svc/v1/sandboxes/{request.sandbox_uid}"
                f"/ports/{request.target_port}"
            ),
            required_headers={"Authorization": "Bearer route-token"},
            route_generation=4,
        )


class SDKTest(unittest.TestCase):
    def setUp(self):
        self.stub = FakeFastPathStub()
        self.requests = []
        self.http = httpx.Client(transport=httpx.MockTransport(self._handle))
        self.client = Client(
            namespace="tenant-a", proxy_endpoint="http://proxy.test:18080/prefix",
            stub=self.stub, http_client=self.http,
        )

    def tearDown(self):
        self.http.close()

    def _handle(self, request: httpx.Request) -> httpx.Response:
        self.requests.append(request)
        self.assertEqual("proxy.test:18080", request.url.netloc.decode())
        self.assertEqual("Bearer route-token", request.headers["Authorization"])
        path = request.url.path
        if path.endswith("/command"):
            body = json.loads(request.content)
            self.assertEqual("python -c 'print(1)'", body["command"])
            return httpx.Response(
                200,
                headers={"Content-Type": "text/event-stream"},
                content=(
                    b'data: {"type":"init","text":"cmd-a"}\n\n'
                    b'{"type":"stdout","text":"1\\n"}\n\n'
                    b'data: {"type":"execution_complete","exit_code":0}\n\n'
                ),
            )
        if path.endswith("/files/info"):
            self.assertEqual(["/tmp/value"], parse_qs(request.url.query.decode())["path"])
            return httpx.Response(200, json={"/tmp/value": {"path": "/tmp/value", "size": 5, "mode": 644}})
        if path.endswith("/files/download"):
            return httpx.Response(200, content=b"value")
        if path.endswith("/directories/list"):
            return httpx.Response(200, json=[{"path": "/tmp/value", "size": 5}])
        return httpx.Response(200)

    def test_create_generates_request_id(self):
        sandbox = self.client.create("sandbox-a", "alpine", command=["sleep", "60"])
        self.assertEqual("sandbox-a", sandbox.name)
        self.assertTrue(self.stub.create_request.request_id)
        self.assertEqual("tenant-a", self.stub.create_request.namespace)

    def test_exec_and_files_use_resolved_proxy_route(self):
        sandbox = self.client.get("sandbox-a")
        result = sandbox.exec(["python", "-c", "print(1)"])
        self.assertTrue(result.success)
        self.assertEqual("1\n", result.stdout)
        self.assertEqual(b"value", sandbox.files.read("/tmp/value"))
        with tempfile.TemporaryDirectory() as directory:
            path = f"{directory}/downloaded"
            sandbox.files.download("/tmp/value", path)
            with open(path, "rb") as downloaded:
                self.assertEqual(b"value", downloaded.read())
        self.assertEqual(5, sandbox.files.stat("/tmp/value").size)
        self.assertEqual(["/tmp/value"], [entry.path for entry in sandbox.files.list("/tmp")])
        self.assertTrue(all(request.target_port == 44772 for request in self.stub.resolve_requests))

    def test_envd_hands_native_protocol_route_to_e2b_client(self):
        endpoint = self.client.get("sandbox-a").resolve_envd()
        self.assertEqual(
            "http://proxy.test:18080/prefix/v1/sandboxes/uid-a/ports/49983",
            endpoint.base_url,
        )
        self.assertEqual("Bearer route-token", endpoint.headers["Authorization"])

    def test_optional_opentelemetry_context_reaches_grpc_and_http(self):
        def inject(carrier):
            carrier["traceparent"] = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

        with mock.patch.object(telemetry, "_otel_inject", inject):
            self.client.create("sandbox-a", "alpine")
            self.client.get("sandbox-a").files.stat("/tmp/value")

        self.assertTrue(all(("traceparent", mock.ANY) in metadata for metadata in self.stub.metadata))
        self.assertEqual(
            "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
            self.requests[-1].headers["traceparent"],
        )


if __name__ == "__main__":
    unittest.main()
