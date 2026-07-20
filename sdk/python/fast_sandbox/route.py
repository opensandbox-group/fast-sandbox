from __future__ import annotations

from dataclasses import dataclass
from typing import Mapping
from urllib.parse import urlencode, urlsplit, urlunsplit

from .proto import fastpath_pb2
from .telemetry import grpc_metadata


@dataclass(frozen=True)
class ResolvedRoute:
    sandbox_uid: str
    target_port: int
    endpoint: str
    headers: Mapping[str, str]
    route_generation: int
    expires_at_unix_seconds: int

    def url(self, path: str, query: Mapping[str, str] | None = None) -> str:
        if not path.startswith("/"):
            raise ValueError("Infra Component path must be absolute")
        parsed = urlsplit(self.endpoint)
        route_path = parsed.path.rstrip("/") + path
        return urlunsplit((parsed.scheme, parsed.netloc, route_path, urlencode(query or {}), ""))


class EndpointResolver:
    def __init__(self, stub, namespace: str = "default", proxy_endpoint: str = ""):
        self._stub = stub
        self._namespace = namespace or "default"
        self._proxy_endpoint = proxy_endpoint

    def resolve(self, sandbox_name: str, target_port: int, namespace: str = "") -> ResolvedRoute:
        if not sandbox_name:
            raise ValueError("sandbox_name is required")
        if not 0 < target_port <= 65535:
            raise ValueError("target_port must be between 1 and 65535")
        selected_namespace = namespace or self._namespace
        info = self._stub.GetSandbox(
            fastpath_pb2.GetRequest(sandbox_name=sandbox_name, namespace=selected_namespace),
            metadata=grpc_metadata(),
        )
        if not info.sandbox_id:
            raise RuntimeError(f"Sandbox {selected_namespace}/{sandbox_name} has no CRD UID")
        response = self._stub.ResolveEndpoint(
            fastpath_pb2.ResolveEndpointRequest(
                sandbox_uid=info.sandbox_id,
                target_port=target_port,
                protocol="http",
            ),
            metadata=grpc_metadata(),
        )
        if response.sandbox_uid != info.sandbox_id or response.target_port != target_port:
            raise RuntimeError("FastPath returned a route for a different Sandbox or target port")
        endpoint = _replace_authority(response.proxy_endpoint, self._proxy_endpoint)
        return ResolvedRoute(
            sandbox_uid=response.sandbox_uid,
            target_port=response.target_port,
            endpoint=endpoint,
            headers=dict(response.required_headers),
            route_generation=response.route_generation,
            expires_at_unix_seconds=response.expires_at_unix_seconds,
        )


def _replace_authority(route_endpoint: str, override: str) -> str:
    route = urlsplit(route_endpoint)
    if not route.scheme or not route.netloc:
        raise RuntimeError(f"FastPath returned invalid proxy endpoint {route_endpoint!r}")
    if not override:
        return route_endpoint
    base = urlsplit(override)
    if not base.scheme or not base.netloc or base.query or base.fragment:
        raise ValueError(f"invalid Sandbox Proxy base URL {override!r}")
    path = base.path.rstrip("/") + route.path
    return urlunsplit((base.scheme, base.netloc, path, route.query, ""))
