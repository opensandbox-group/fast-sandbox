from __future__ import annotations

from typing import Iterable, Mapping, Optional

import grpc

from .proto import fastpath_pb2, fastpath_pb2_grpc
from .route import EndpointResolver, ResolvedRoute
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
    ):
        self.endpoint = endpoint
        self.namespace = namespace
        self._owns_channel = channel is None and stub is None
        self._channel = channel or (None if stub is not None else grpc.insecure_channel(endpoint))
        self._stub = stub or fastpath_pb2_grpc.FastPathServiceStub(self._channel)
        self._resolver = EndpointResolver(self._stub, namespace, proxy_endpoint)

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
        selected_request_id = request_id or name
        if selected_request_id != name:
            raise ValueError("request_id must equal the sandbox name")
        response = self._stub.CreateSandbox(
            fastpath_pb2.CreateRequest(
                name=name, image=image, pool_ref=pool,
                command=list(command or []), args=list(args or []),
                envs=dict(envs or {}), working_dir=working_dir,
                namespace=selected_namespace,
                request_id=selected_request_id,
            ),
            metadata=grpc_metadata(),
        )
        return Sandbox(
            client=self, name=response.sandbox_name or name,
            sandbox_uid=response.sandbox_uid,
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
            sandbox_uid=response.sandbox_uid, namespace=selected_namespace,
        )

    def delete(self, name: str, namespace: Optional[str] = None) -> bool:
        response = self._stub.DeleteSandbox(
            fastpath_pb2.DeleteRequest(sandbox_name=name, namespace=namespace or self.namespace),
            metadata=grpc_metadata(),
        )
        return response.success

    def resolve_endpoint(
        self,
        name: str,
        target_port: int,
        namespace: Optional[str] = None,
    ) -> ResolvedRoute:
        """Return a transparent route for an upstream Infra Component SDK.

        Fast Sandbox does not interpret the protocol spoken on target_port.
        """
        return self._resolver.resolve(name, target_port, namespace or self.namespace)

    def close(self) -> None:
        if self._owns_channel and self._channel is not None:
            self._channel.close()

    def __enter__(self) -> "Client":
        return self

    def __exit__(self, *_args) -> None:
        self.close()
