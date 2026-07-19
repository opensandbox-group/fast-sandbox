from __future__ import annotations

from pathlib import Path
from typing import TYPE_CHECKING

from .execd import FileInfo

if TYPE_CHECKING:
    from .sandbox import Sandbox


class FilesClient:
    def __init__(self, sandbox: "Sandbox"):
        self._sandbox = sandbox

    def stat(self, path: str) -> FileInfo:
        return self._adapter.stat(self._sandbox.name, path, self._sandbox.namespace)

    def list(self, path: str) -> list[FileInfo]:
        return self._adapter.list(self._sandbox.name, path, self._sandbox.namespace)

    def read(self, path: str) -> bytes:
        return self._adapter.read(self._sandbox.name, path, self._sandbox.namespace)

    def write(self, path: str, data: bytes | str, mode: int = 0o644) -> None:
        if isinstance(data, str):
            data = data.encode()
        self._adapter.write(self._sandbox.name, path, data, namespace=self._sandbox.namespace, mode=mode)

    def upload(self, local_path: str | Path, remote_path: str, mode: int = 0o644) -> None:
        with open(local_path, "rb") as source:
            self._adapter.write(
                self._sandbox.name, remote_path, source,
                namespace=self._sandbox.namespace, mode=mode,
            )

    def download(self, remote_path: str, local_path: str | Path) -> None:
        with open(local_path, "wb") as destination:
            self._adapter.download(
                self._sandbox.name, remote_path, destination,
                namespace=self._sandbox.namespace,
            )

    def mkdir(self, path: str, mode: int = 0o755) -> None:
        self._adapter.mkdir(self._sandbox.name, path, self._sandbox.namespace, mode)

    def delete(self, path: str, directory: bool = False) -> None:
        self._adapter.delete(self._sandbox.name, path, self._sandbox.namespace, directory)

    @property
    def _adapter(self):
        return self._sandbox._client.execd
