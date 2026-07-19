from __future__ import annotations

from dataclasses import dataclass
from typing import Mapping

from .route import EndpointResolver

ENVD_PORT = 49983


@dataclass(frozen=True)
class EnvdEndpoint:
    """Route hand-off for the official E2B envd Connect client."""

    base_url: str
    headers: Mapping[str, str]
    sandbox_uid: str
    route_generation: int


class EnvdAdapter:
    def __init__(self, resolver: EndpointResolver, port: int = ENVD_PORT):
        self._resolver = resolver
        self._port = port

    def resolve(self, sandbox_name: str, namespace: str = "") -> EnvdEndpoint:
        route = self._resolver.resolve(sandbox_name, self._port, namespace)
        return EnvdEndpoint(
            base_url=route.endpoint,
            headers=dict(route.headers),
            sandbox_uid=route.sandbox_uid,
            route_generation=route.route_generation,
        )
