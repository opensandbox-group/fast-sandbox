from __future__ import annotations

from typing import Mapping

try:
    from opentelemetry.propagate import inject as _otel_inject
except ImportError:  # OpenTelemetry remains an optional SDK integration.
    _otel_inject = None


def inject_headers(headers: Mapping[str, str] | None = None) -> dict[str, str]:
    carrier = dict(headers or {})
    if _otel_inject is not None:
        _otel_inject(carrier)
    return carrier


def grpc_metadata() -> tuple[tuple[str, str], ...]:
    carrier = inject_headers()
    return tuple((name.lower(), value) for name, value in carrier.items())
