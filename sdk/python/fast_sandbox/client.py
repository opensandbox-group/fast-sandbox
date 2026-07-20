from __future__ import annotations

import uuid
from typing import Iterable, Mapping, Optional

import grpc
import httpx

from .envd import EnvdAdapter
from .execd import ExecdAdapter
from .proto import fastpath_pb2, fastpath_pb2_grpc
from .route import EndpointResolver
from .sandbox import Sandbox
from .telemetry import grpc_metadata


class Client:
    def __init__(
        self,
        endpoint: str = "localhost:9090",
        namespace: str = "default",
        *,
        proxy_endpoint: str = "",
        channel: Optional[grpc.Channel] = None,
        stub=None,
        http_client: Optional[httpx.Client] = None,
    ):
        self.endpoint = endpoint
        self.namespace = namespace
        self._owns_channel = channel is None and stub is None
        self._channel = channel or (None if stub is not None else grpc.insecure_channel(endpoint))
        self._stub = stub or fastpath_pb2_grpc.FastPathServiceStub(self._channel)
        self._owns_http = http_client is None
        self._http = http_client or httpx.Client(timeout=None)
        self._resolver = EndpointResolver(self._stub, namespace, proxy_endpoint)
        self.execd = ExecdAdapter(self._resolver, self._http)
        self.envd = EnvdAdapter(self._resolver)

    @property
    def stub(self):
        return self._stub

    def create(
        self,
        name: str,
        image: str,
        pool: str = "default-pool",
        command: Optional[Iterable[str]] = None,
        args: Optional[Iterable[str]] = None,
        envs: Optional[Mapping[str, str]] = None,
        working_dir: str = "",
        namespace: Optional[str] = None,
        request_id: Optional[str] = None,
    ) -> Sandbox:
        selected_namespace = namespace or self.namespace
        response = self._stub.CreateSandbox(
            fastpath_pb2.CreateRequest(
                name=name, image=image, pool_ref=pool,
                command=list(command or []), args=list(args or []),
                envs=dict(envs or {}), working_dir=working_dir,
                namespace=selected_namespace,
                request_id=request_id or str(uuid.uuid4()),
            ),
            metadata=grpc_metadata(),
        )
        return Sandbox(
            client=self, name=response.sandbox_name or name,
            sandbox_id=response.sandbox_uid or response.sandbox_id,
            namespace=selected_namespace,
        )

    def get(self, name: str, namespace: Optional[str] = None) -> Sandbox:
        selected_namespace = namespace or self.namespace
        response = self._stub.GetSandbox(
            fastpath_pb2.GetRequest(sandbox_name=name, namespace=selected_namespace),
            metadata=grpc_metadata(),
        )
        return Sandbox(
            client=self, name=response.sandbox_name or name,
            sandbox_id=response.sandbox_id, namespace=selected_namespace,
        )

    def delete(self, name: str, namespace: Optional[str] = None) -> bool:
        response = self._stub.DeleteSandbox(
            fastpath_pb2.DeleteRequest(sandbox_name=name, namespace=namespace or self.namespace),
            metadata=grpc_metadata(),
        )
        return response.success

    def close(self) -> None:
        if self._owns_http:
            self._http.close()
        if self._owns_channel and self._channel is not None:
            self._channel.close()

    def __enter__(self) -> "Client":
        return self

    def __exit__(self, *_args) -> None:
        self.close()
