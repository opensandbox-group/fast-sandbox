from __future__ import annotations

from dataclasses import dataclass
from typing import Iterable, Mapping, Optional, TYPE_CHECKING

if TYPE_CHECKING:
    from .client import Client


@dataclass(frozen=True)
class ExecResult:
    exit_code: int
    stdout: str
    stderr: str
    success: bool
    message: str = ""
    timed_out: bool = False


class Sandbox:
    def __init__(self, client: "Client", name: str, sandbox_uid: str = "", namespace: str = "default"):
        self._client = client
        self.name = name
        self.sandbox_uid = sandbox_uid
        self.namespace = namespace
        from .files import FilesClient

        self.files = FilesClient(self)

    def exec(
        self,
        command: Iterable[str],
        *,
        envs: Optional[Mapping[str, str]] = None,
        working_dir: str = "",
        timeout_seconds: float = 0,
        stdin: bytes | str = b"",
        tty: bool = False,
    ) -> ExecResult:
        if stdin:
            raise NotImplementedError("stdin requires a session-capable Execd adapter")
        if tty:
            raise NotImplementedError("PTY requires the Execd WebSocket extension adapter")
        execution = self._client.execd.run_command(
            self.name, command, namespace=self.namespace, envs=envs,
            working_dir=working_dir, timeout_seconds=timeout_seconds,
        )
        stdout = "".join(message.text for message in execution.stdout)
        stderr = "".join(message.text for message in execution.stderr)
        exit_code = execution.exit_code if execution.exit_code is not None else 1
        message = execution.error.value if execution.error else ""
        return ExecResult(
            exit_code=exit_code, stdout=stdout, stderr=stderr,
            success=exit_code == 0 and execution.error is None, message=message,
        )

    def resolve_envd(self):
        return self._client.envd.resolve(self.name, self.namespace)

    def delete(self) -> bool:
        return self._client.delete(self.name, namespace=self.namespace)
