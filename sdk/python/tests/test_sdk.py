from __future__ import annotations

import unittest
from unittest import mock

from fast_sandbox import Client
from fast_sandbox import telemetry
from fast_sandbox.proto import fastpath_pb2


class FakeFastPathStub:
    def __init__(self):
        self.create_request = None
        self.delete_requests = []
        self.resolve_requests = []
        self.update_requests = []
        self.metadata = []

    def CreateSandbox(self, request, metadata=()):
        self.metadata.append(tuple(metadata))
        self.create_request = request
        return fastpath_pb2.CreateSandboxResponse(
            sandbox=fastpath_pb2.SandboxInfo(
                identity=fastpath_pb2.SandboxIdentity(
                    uid="uid-a", name=request.request_id, namespace=request.namespace
                ),
                runtime=fastpath_pb2.RuntimeInfo(state=fastpath_pb2.RUNTIME_STATE_READY),
                ready=True,
            ),
            generation=1,
            completion=request.completion,
        )

    def GetSandbox(self, request, metadata=()):
        self.metadata.append(tuple(metadata))
        return fastpath_pb2.GetSandboxResponse(
            sandbox=fastpath_pb2.SandboxInfo(
                identity=fastpath_pb2.SandboxIdentity(
                    uid="uid-a",
                    name=request.sandbox.namespaced_name.name,
                    namespace=request.sandbox.namespaced_name.namespace,
                )
            ),
            generation=1,
        )

    def DeleteSandbox(self, request, metadata=()):
        self.metadata.append(tuple(metadata))
        self.delete_requests.append(request)
        return fastpath_pb2.DeleteResponse()

    def UpdateSandbox(self, request, metadata=()):
        self.metadata.append(tuple(metadata))
        self.update_requests.append(request)
        return fastpath_pb2.UpdateSandboxResponse(
            sandbox=fastpath_pb2.SandboxIdentity(
                uid="uid-a", name=request.sandbox.namespaced_name.name
            ),
            committed_generation=2,
        )

    def ResolveEndpoint(self, request, metadata=()):
        self.metadata.append(tuple(metadata))
        self.resolve_requests.append(request)
        component_name = request.target.component_name
        port = request.target.port or 44772
        if component_name:
            path = f"/v2/sandboxes/uid-a/components/{component_name}"
        else:
            path = f"/v2/sandboxes/uid-a/ports/{port}"
        return fastpath_pb2.ResolveEndpointResponse(
            sandbox_uid="uid-a",
            endpoint=fastpath_pb2.ResolvedEndpoint(
                component_name=component_name,
                protocol="HTTP",
                port=port,
            ),
            proxy_endpoint=f"http://sandbox-proxy.svc{path}",
            required_headers={
                "X-Fast-Sandbox-Route-Credential": "route-token"
            },
            route_generation=4,
        )


class SDKTest(unittest.TestCase):
    def setUp(self):
        self.stub = FakeFastPathStub()
        self.client = Client(
            namespace="tenant-a",
            proxy_endpoint="http://proxy.test:18080/prefix",
            stub=self.stub,
        )

    def test_create_sends_atomic_initial_intent(self):
        sandbox = self.client.create(
            "sandbox-a",
            "alpine",
            command=["sleep", "60"],
            expires_at_unix_seconds=1785373200,
            metadata={"owner": "team-a"},
            failure_policy="AutoRecreate",
            recovery_timeout_seconds=120,
            action_bindings=[("egress", '{"policy":{"defaultAction":"deny"}}')],
            cpu_limit="2",
            memory_limit="6Gi",
        )
        self.assertEqual("sandbox-a", sandbox.name)
        request = self.stub.create_request
        self.assertEqual("sandbox-a", request.request_id)
        self.assertEqual("tenant-a", request.namespace)
        self.assertEqual(1785373200, request.expires_at_unix_seconds)
        self.assertEqual({"owner": "team-a"}, dict(request.metadata))
        self.assertEqual(fastpath_pb2.AUTO_RECREATE, request.failure_policy)
        self.assertEqual(120, request.recovery_timeout_seconds)
        self.assertEqual("egress", request.action_bindings[0].handler)
        self.assertEqual(
            '{"policy":{"defaultAction":"deny"}}',
            request.action_bindings[0].input,
        )
        self.assertEqual(fastpath_pb2.CREATE_COMPLETION_READY, request.completion)
        self.assertEqual("2", request.resources.limits.cpu)
        self.assertEqual("6Gi", request.resources.limits.memory)

    def test_create_resource_limits_must_be_set_together(self):
        with self.assertRaisesRegex(ValueError, "must be set together"):
            self.client.create("sandbox-a", "alpine", cpu_limit="1")

    def test_atomic_ordered_action_binding_replace(self):
        sandbox = self.client.get("sandbox-a")
        sandbox.replace_action_bindings(
            [("audit", "enabled: true"), ("egress", "null")]
        )
        request = self.stub.update_requests[-1]
        self.assertEqual(1, request.expected_generation)
        self.assertEqual(
            ["audit", "egress"],
            [item.handler for item in request.action_bindings.items],
        )
        self.assertEqual("null", request.action_bindings.items[1].input)
        self.assertEqual(2, sandbox.generation)

    def test_sandbox_methods_fence_replacement_uid(self):
        sandbox = self.client.get("sandbox-a")
        self.assertTrue(sandbox.delete())
        request = self.stub.delete_requests[-1]
        self.assertEqual("sandbox-a", request.sandbox.namespaced_name.name)
        self.assertEqual("uid-a", request.sandbox.expected_uid)

    def test_action_input_must_be_an_opaque_string(self):
        with self.assertRaises(TypeError):
            self.client.create(
                "sandbox-a",
                "alpine",
                action_bindings=[("egress", {"policy": "deny"})],
            )

    def test_named_component_preserves_route_path(self):
        route = self.client.get("sandbox-a").resolve_component("execd")
        self.assertEqual(
            "http://proxy.test:18080/prefix/v2/sandboxes/uid-a/components/execd",
            route.endpoint,
        )
        self.assertEqual(
            "route-token",
            route.headers["X-Fast-Sandbox-Route-Credential"],
        )
        request = self.stub.resolve_requests[-1]
        self.assertEqual("execd", request.target.component_name)
        self.assertEqual("tenant-a", request.sandbox.namespaced_name.namespace)

    def test_raw_port_does_not_claim_component_readiness(self):
        route = self.client.resolve_endpoint("sandbox-a", 8080)
        self.assertEqual(8080, route.resolved_port)
        request = self.stub.resolve_requests[-1]
        self.assertEqual(8080, request.target.port)

    def test_optional_opentelemetry_context_reaches_grpc(self):
        def inject(carrier):
            carrier["traceparent"] = (
                "00-4bf92f3577b34da6a3ce929d0e0e4736-"
                "00f067aa0ba902b7-01"
            )

        with mock.patch.object(telemetry, "_otel_inject", inject):
            self.client.create("sandbox-a", "alpine")
            self.client.get("sandbox-a").resolve_component("execd")

        self.assertTrue(
            all(("traceparent", mock.ANY) in metadata for metadata in self.stub.metadata)
        )


if __name__ == "__main__":
    unittest.main()
