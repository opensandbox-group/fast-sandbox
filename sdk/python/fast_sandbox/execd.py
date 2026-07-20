from __future__ import annotations

import io
import json
import os
import shlex
from dataclasses import dataclass, field
from pathlib import Path
from typing import BinaryIO, Callable, Iterable, Mapping

import httpx

from .telemetry import inject_headers

from .route import EndpointResolver

EXECD_PORT = 44772


@dataclass(frozen=True)
class OutputMessage:
    text: str
    timestamp: int = 0


@dataclass(frozen=True)
class ExecutionError:
    name: str = ""
    value: str = ""
    timestamp: int = 0
    traceback: tuple[str, ...] = ()


@dataclass
class Execution:
    id: str = ""
    stdout: list[OutputMessage] = field(default_factory=list)
    stderr: list[OutputMessage] = field(default_factory=list)
    error: ExecutionError | None = None
    exit_code: int | None = None
    complete: bool = False


@dataclass(frozen=True)
class FileInfo:
    path: str
    size: int = 0
    modified_at: str = ""
    created_at: str = ""
    owner: str = ""
    group: str = ""
    mode: int = 0


class ExecdAdapter:
    def __init__(self, resolver: EndpointResolver, http_client: httpx.Client, port: int = EXECD_PORT):
        self._resolver = resolver
        self._http = http_client
        self._port = port

    def run_command(
        self,
        sandbox_name: str,
        command: Iterable[str],
        *,
        namespace: str = "",
        envs: Mapping[str, str] | None = None,
        working_dir: str = "",
        timeout_seconds: float = 0,
        on_stdout: Callable[[OutputMessage], None] | None = None,
        on_stderr: Callable[[OutputMessage], None] | None = None,
    ) -> Execution:
        arguments = list(command)
        if not arguments:
            raise ValueError("command must not be empty")
        if timeout_seconds < 0:
            raise ValueError("timeout_seconds must not be negative")
        route = self._resolver.resolve(sandbox_name, self._port, namespace)
        body = {
            "command": shlex.join(arguments),
            "cwd": working_dir or None,
            "timeout": int(timeout_seconds * 1000) or None,
            "envs": dict(envs or {}) or None,
        }
        body = {name: value for name, value in body.items() if value is not None}
        execution = Execution()
        event_count = 0
        with self._http.stream(
            "POST", route.url("/command"), headers=inject_headers(route.headers), json=body
        ) as response:
            _raise_for_status(response)
            data_lines: list[str] = []
            for line in response.iter_lines():
                if line == "":
                    if data_lines:
                        _consume_event(execution, "\n".join(data_lines), on_stdout, on_stderr)
                        event_count += 1
                        data_lines = []
                    continue
                if line.startswith(":"):
                    continue
                if line.startswith("{"):
                    data_lines.append(line)
                    continue
                field, separator, value = line.partition(":")
                if separator and field == "data":
                    data_lines.append(value.removeprefix(" "))
            if data_lines:
                _consume_event(execution, "\n".join(data_lines), on_stdout, on_stderr)
                event_count += 1
        if event_count == 0:
            raise RuntimeError("execd returned an empty event stream")
        if not execution.complete and execution.error is None:
            raise RuntimeError("execd command stream ended without a completion event")
        return execution

    def stat(self, sandbox_name: str, path: str, namespace: str = "") -> FileInfo:
        route = self._resolver.resolve(sandbox_name, self._port, namespace)
        response = self._http.get(route.url("/files/info", {"path": path}), headers=inject_headers(route.headers))
        _raise_for_status(response)
        raw = response.json()
        if path not in raw:
            raise RuntimeError(f"execd response omitted file {path!r}")
        return _file_info(raw[path])

    def list(self, sandbox_name: str, path: str, namespace: str = "") -> list[FileInfo]:
        route = self._resolver.resolve(sandbox_name, self._port, namespace)
        response = self._http.get(route.url("/directories/list", {"path": path}), headers=inject_headers(route.headers))
        _raise_for_status(response)
        return [_file_info(value) for value in response.json()]

    def read(self, sandbox_name: str, path: str, namespace: str = "") -> bytes:
        destination = io.BytesIO()
        self.download(sandbox_name, path, destination, namespace=namespace)
        return destination.getvalue()

    def download(
        self, sandbox_name: str, path: str, destination: BinaryIO, *, namespace: str = ""
    ) -> int:
        route = self._resolver.resolve(sandbox_name, self._port, namespace)
        written = 0
        with self._http.stream(
            "GET", route.url("/files/download", {"path": path}), headers=inject_headers(route.headers)
        ) as response:
            _raise_for_status(response)
            for chunk in response.iter_bytes():
                written += destination.write(chunk)
        return written

    def write(
        self,
        sandbox_name: str,
        path: str,
        source: bytes | BinaryIO,
        *,
        namespace: str = "",
        mode: int = 0o644,
    ) -> None:
        route = self._resolver.resolve(sandbox_name, self._port, namespace)
        metadata = json.dumps({"path": path, "mode": _octal_digits(mode)})
        files = [
            ("metadata", ("metadata", metadata, "application/json")),
            ("file", (os.path.basename(path), source, "application/octet-stream")),
        ]
        response = self._http.post(route.url("/files/upload"), headers=inject_headers(route.headers), files=files)
        _raise_for_status(response)

    def mkdir(self, sandbox_name: str, path: str, namespace: str = "", mode: int = 0o755) -> None:
        route = self._resolver.resolve(sandbox_name, self._port, namespace)
        response = self._http.post(
            route.url("/directories"), headers=inject_headers(route.headers),
            json={path: {"mode": _octal_digits(mode)}},
        )
        _raise_for_status(response)

    def delete(self, sandbox_name: str, path: str, namespace: str = "", directory: bool = False) -> None:
        route = self._resolver.resolve(sandbox_name, self._port, namespace)
        endpoint = "/directories" if directory else "/files"
        response = self._http.delete(route.url(endpoint, {"path": path}), headers=inject_headers(route.headers))
        _raise_for_status(response)


def _consume_event(
    execution: Execution,
    payload: str,
    on_stdout: Callable[[OutputMessage], None] | None,
    on_stderr: Callable[[OutputMessage], None] | None,
) -> None:
    try:
        event = json.loads(payload)
    except json.JSONDecodeError:
        message = OutputMessage(payload)
        execution.stdout.append(message)
        if on_stdout:
            on_stdout(message)
        return
    event_type = event.get("type", "")
    if event_type == "init":
        execution.id = event.get("text", "")
    elif event_type == "stdout":
        message = OutputMessage(event.get("text", ""), event.get("timestamp", 0))
        execution.stdout.append(message)
        if on_stdout:
            on_stdout(message)
    elif event_type == "stderr":
        message = OutputMessage(event.get("text", ""), event.get("timestamp", 0))
        execution.stderr.append(message)
        if on_stderr:
            on_stderr(message)
    elif event_type == "error":
        error = event.get("error") or event
        execution.error = ExecutionError(
            name=error.get("ename", ""), value=error.get("evalue", ""),
            timestamp=event.get("timestamp", 0), traceback=tuple(error.get("traceback", ())),
        )
        exit_code = event.get("exit_code")
        if exit_code is None:
            try:
                exit_code = int(execution.error.value)
            except ValueError:
                pass
        execution.exit_code = exit_code
    elif event_type == "execution_complete":
        execution.complete = True
        if event.get("exit_code") is not None:
            execution.exit_code = int(event["exit_code"])
        elif execution.error is None and execution.exit_code is None:
            execution.exit_code = 0


def _file_info(value: Mapping[str, object]) -> FileInfo:
    return FileInfo(
        path=str(value.get("path", "")), size=int(value.get("size", 0)),
        modified_at=str(value.get("modified_at", "")), created_at=str(value.get("created_at", "")),
        owner=str(value.get("owner", "")), group=str(value.get("group", "")), mode=int(value.get("mode", 0)),
    )


def _octal_digits(mode: int) -> int:
    return int(f"{mode:o}")


def _raise_for_status(response: httpx.Response) -> None:
    if response.is_error:
        try:
            content = response.content
        except httpx.ResponseNotRead:
            content = response.read()
        detail = content[: 64 * 1024].decode(errors="replace").strip()
        raise RuntimeError(f"execd returned HTTP {response.status_code}: {detail}")
