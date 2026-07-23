from __future__ import annotations

from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from .client import Client


class Sandbox:
    def __init__(self, client: "Client", name: str, sandbox_uid: str = "", namespace: str = "default"):
        self._client = client
        self.name = name
        self.sandbox_uid = sandbox_uid
        self.namespace = namespace
    def resolve_endpoint(self, target_port: int):
        """Hand a transparent route to an upstream component SDK."""
        return self._client.resolve_endpoint(self.name, target_port, self.namespace)

    def delete(self) -> bool:
        return self._client.delete(self.name, namespace=self.namespace)
