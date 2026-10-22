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
        namespace: str = "fast-sandbox",
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
        expires_at_unix_seconds: int = 0,
        metadata: Optional[Mapping[str, str]] = None,
        failure_policy: str = "Manual",
        recovery_timeout_seconds: int = 60,
        action_bindings: Optional[Iterable[tuple[str, str]]] = None,
        completion: str = "Ready",
        cpu_limit: str = "",
        memory_limit: str = "",
    ) -> Sandbox:
        selected_namespace = namespace or self.namespace
        selected_request_id = request_id or name
        if selected_request_id != name:
            raise ValueError("request_id must equal the sandbox name")
        if bool(cpu_limit) != bool(memory_limit):
            raise ValueError("cpu_limit and memory_limit must be set together")
        request = fastpath_pb2.CreateSandboxRequest(
            request_id=selected_request_id,
            namespace=selected_namespace,
            image=image,
            pool_ref=pool,
            command=list(command or []), args=list(args or []),
            envs=dict(envs or {}), working_dir=working_dir,
            expires_at_unix_seconds=expires_at_unix_seconds,
            metadata=dict(metadata or {}),
            failure_policy=_failure_policy(failure_policy),
            recovery_timeout_seconds=recovery_timeout_seconds,
            action_bindings=_action_bindings(action_bindings or []),
            completion=_create_completion(completion),
        )
        if cpu_limit:
            request.resources.limits.cpu = cpu_limit
            request.resources.limits.memory = memory_limit
        response = self._stub.CreateSandbox(
            request,
            metadata=grpc_metadata(),
        )
        return Sandbox(
            client=self, name=response.sandbox.identity.name or name,
            sandbox_uid=response.sandbox.identity.uid,
            namespace=selected_namespace,
            generation=response.generation,
            info=response.sandbox,
        )

    def get(self, name: str, namespace: Optional[str] = None) -> Sandbox:
        selected_namespace = namespace or self.namespace
        response = self._stub.GetSandbox(
            fastpath_pb2.GetSandboxRequest(
                sandbox=_sandbox_reference(name, selected_namespace),
            ),
            metadata=grpc_metadata(),
        )
        return Sandbox(
            client=self, name=response.sandbox.identity.name or name,
            sandbox_uid=response.sandbox.identity.uid, namespace=selected_namespace,
            generation=response.generation, info=response.sandbox,
        )

    def delete(self, name: str, namespace: Optional[str] = None, *, expected_uid: str = "") -> bool:
        response = self._stub.DeleteSandbox(
            fastpath_pb2.DeleteRequest(
                sandbox=_sandbox_reference(name, namespace or self.namespace, expected_uid)
            ),
            metadata=grpc_metadata(),
        )
        return response is not None

    def replace_action_bindings(
        self,
        name: str,
        action_bindings: Iterable[tuple[str, str]],
        namespace: Optional[str] = None,
        *,
        expected_generation: int = 0,
        expected_uid: str = "",
    ):
        """Atomically replace the complete ordered Action Binding list."""
        return self._stub.UpdateSandbox(
            fastpath_pb2.UpdateSandboxRequest(
                sandbox=_sandbox_reference(name, namespace or self.namespace, expected_uid),
                expected_generation=expected_generation,
                action_bindings=fastpath_pb2.ReplaceActionBindings(
                    items=_action_bindings(action_bindings),
                ),
            ),
            metadata=grpc_metadata(),
        )

    def resolve_endpoint(
        self,
        name: str,
        target_port: int,
        namespace: Optional[str] = None,
    ) -> ResolvedRoute:
        """Return a transparent raw user-port route.

        Ports reserved by Pool Infra Components must be resolved by name.
        """
        return self._resolver.resolve_port(name, target_port, namespace or self.namespace)

    def resolve_component(
        self,
        name: str,
        component_name: str,
        namespace: Optional[str] = None,
    ) -> ResolvedRoute:
        """Resolve one named component if its Fastlet-local route is Ready."""
        return self._resolver.resolve_component(
            name,
            component_name,
            namespace or self.namespace,
        )

    def close(self) -> None:
        if self._owns_channel and self._channel is not None:
            self._channel.close()

    def __enter__(self) -> "Client":
        return self

    def __exit__(self, *_args) -> None:
        self.close()


def _failure_policy(value: str) -> int:
    normalized = value.replace("-", "").replace("_", "").lower()
    if normalized == "manual":
        return fastpath_pb2.MANUAL
    if normalized in {"auto", "autorecreate"}:
        return fastpath_pb2.AUTO_RECREATE
    raise ValueError("failure_policy must be Manual or AutoRecreate")


def _create_completion(value: str) -> int:
    normalized = value.replace("-", "").replace("_", "").lower()
    if normalized == "ready":
        return fastpath_pb2.CREATE_COMPLETION_READY
    if normalized == "runtimeready":
        return fastpath_pb2.CREATE_COMPLETION_RUNTIME_READY
    raise ValueError("completion must be Ready or RuntimeReady")


def _sandbox_reference(name: str, namespace: str, expected_uid: str = ""):
    return fastpath_pb2.SandboxReference(
        namespaced_name=fastpath_pb2.NamespacedName(namespace=namespace, name=name),
        expected_uid=expected_uid,
    )


def _action_bindings(values: Iterable[tuple[str, str]]):
    result = []
    for handler, value in values:
        if not isinstance(value, str):
            raise TypeError("Action Binding input must be an opaque string")
        result.append(fastpath_pb2.ActionBinding(handler=handler, input=value))
    return result
