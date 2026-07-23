from __future__ import annotations

import unittest
from unittest import mock

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
        self.client = Client(
            namespace="tenant-a", proxy_endpoint="http://proxy.test:18080/prefix",
            stub=self.stub,
        )

    def test_create_uses_name_as_request_id(self):
        sandbox = self.client.create("sandbox-a", "alpine", command=["sleep", "60"])
        self.assertEqual("sandbox-a", sandbox.name)
        self.assertEqual("sandbox-a", self.stub.create_request.request_id)
        self.assertEqual("tenant-a", self.stub.create_request.namespace)

    def test_generic_route_hands_protocol_to_upstream_sdk(self):
        endpoint = self.client.get("sandbox-a").resolve_endpoint(44772)
        self.assertEqual(
            "http://proxy.test:18080/prefix/v1/sandboxes/uid-a/ports/44772",
            endpoint.endpoint,
        )
        self.assertEqual("Bearer route-token", endpoint.headers["Authorization"])
        self.assertEqual(44772, self.stub.resolve_requests[-1].target_port)

    def test_optional_opentelemetry_context_reaches_grpc(self):
        def inject(carrier):
            carrier["traceparent"] = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

        with mock.patch.object(telemetry, "_otel_inject", inject):
            self.client.create("sandbox-a", "alpine")
            self.client.get("sandbox-a").resolve_endpoint(44772)

        self.assertTrue(all(("traceparent", mock.ANY) in metadata for metadata in self.stub.metadata))


if __name__ == "__main__":
    unittest.main()
