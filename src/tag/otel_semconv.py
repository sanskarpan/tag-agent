"""PRD-041: OTel GenAI Span Cost Attribution.

Remaps TAG's internal span attributes to OTel GenAI semantic convention
names at OTLP export time. The pinned semconv version is stored here and
reflects Development-stability spec (may change before reaching Stable).

Internal name        → OTel GenAI semconv name
-------------------------------------------------
prompt_tokens        → gen_ai.usage.input_tokens
completion_tokens    → gen_ai.usage.output_tokens
model_id             → gen_ai.request.model
(TAG)                → gen_ai.system = "anthropic" (or detected)
(TAG)                → gen_ai.operation.name = "chat"

Original names are preserved alongside OTel names for backwards compat.
"""
from __future__ import annotations

import json
from typing import Any

# Active pinned OTel GenAI semconv version
SEMCONV_VERSION = "1.28.0"

# Instrumentation scope sent in OTLP payloads
try:
    from tag import __version__ as _tag_version
except Exception:
    _tag_version = "0.0.0"

INSTRUMENTATION_SCOPE = {
    "name": "tag-agent",
    "version": _tag_version,
    "attributes": [
        {"key": "otel.semconv.version", "value": {"stringValue": SEMCONV_VERSION}},
        {"key": "otel.semconv.stability", "value": {"stringValue": "Development"}},
    ],
}

# Model-to-provider mapping for gen_ai.system
_PROVIDER_MAP: dict[str, str] = {
    "claude": "anthropic",
    "gpt": "openai",
    "gemini": "google",
    "mistral": "mistral",
    "llama": "meta",
    "command": "cohere",
}


def detect_provider(model_id: str) -> str:
    """Infer gen_ai.system from model ID prefix."""
    m = model_id.lower()
    for prefix, provider in _PROVIDER_MAP.items():
        if m.startswith(prefix):
            return provider
    return "unknown"


def map_span_attributes(span: dict[str, Any]) -> dict[str, Any]:
    """Return a copy of *span* with OTel GenAI attributes added.

    Preserves all original attributes; adds gen_ai.* alongside them.
    """
    result = dict(span)
    attrs = dict(result.get("attributes", {}))

    # Core token mapping
    prompt_tokens = span.get("prompt_tokens") or attrs.get("prompt_tokens", 0)
    completion_tokens = span.get("completion_tokens") or attrs.get("completion_tokens", 0)
    model_id = span.get("model_id") or attrs.get("model_id", "")

    if prompt_tokens or completion_tokens:
        attrs["gen_ai.usage.input_tokens"] = int(prompt_tokens or 0)
        attrs["gen_ai.usage.output_tokens"] = int(completion_tokens or 0)

    if model_id:
        attrs["gen_ai.request.model"] = model_id
        attrs["gen_ai.system"] = detect_provider(model_id)

    attrs["gen_ai.operation.name"] = "chat"
    attrs["otel.semconv.version"] = SEMCONV_VERSION

    result["attributes"] = attrs
    return result


def spans_to_otlp_json(spans: list[dict[str, Any]], *, include_metrics: bool = True) -> dict:
    """Build an OTLP JSON payload from a list of span dicts.

    Applies OTel GenAI attribute mapping and optionally includes a
    gen_ai.client.token.usage histogram metric payload.
    """
    import uuid

    resource_spans = []
    scope_spans: list[dict] = []

    for span in spans:
        mapped = map_span_attributes(span)
        attrs = mapped.get("attributes", {})

        otlp_attrs = [
            _kv(k, v) for k, v in attrs.items()
            if v is not None
        ]

        trace_id = (mapped.get("trace_id") or uuid.uuid4().hex).replace("-", "")
        span_id = (mapped.get("id") or uuid.uuid4().hex[:16]).replace("-", "")
        parent_id = (mapped.get("parent_id") or "").replace("-", "") or None

        otlp_span: dict[str, Any] = {
            "traceId": trace_id[:32].zfill(32),
            "spanId": span_id[:16].zfill(16),
            "name": mapped.get("name", "inference"),
            "kind": 3,  # CLIENT
            "startTimeUnixNano": str(_iso_to_ns(mapped.get("started_at", ""))),
            "endTimeUnixNano": str(_iso_to_ns(mapped.get("finished_at", ""))),
            "attributes": otlp_attrs,
            "status": {"code": 1 if mapped.get("status") == "ok" else 2},
        }
        if parent_id:
            otlp_span["parentSpanId"] = parent_id[:16].zfill(16)

        scope_spans.append(otlp_span)

    resource_spans = [{
        "resource": {
            "attributes": [_kv("service.name", "tag-agent")]
        },
        "scopeSpans": [{
            "scope": INSTRUMENTATION_SCOPE,
            "spans": scope_spans,
        }],
    }]

    payload: dict[str, Any] = {"resourceSpans": resource_spans}

    if include_metrics:
        payload["resourceMetrics"] = _build_token_metrics(spans)

    return payload


def _build_token_metrics(spans: list[dict]) -> list[dict]:
    """Build OTLP metric payload for gen_ai.client.token.usage histogram."""
    data_points = []
    for span in spans:
        pt = span.get("prompt_tokens") or 0
        ct = span.get("completion_tokens") or 0
        if not (pt or ct):
            continue
        model_id = span.get("model_id", "")
        data_points.append({
            "attributes": [
                _kv("gen_ai.request.model", model_id),
                _kv("gen_ai.system", detect_provider(model_id)),
                _kv("gen_ai.token.type", "input"),
            ],
            "asInt": str(int(pt)),
            "startTimeUnixNano": str(_iso_to_ns(span.get("started_at", ""))),
            "timeUnixNano": str(_iso_to_ns(span.get("finished_at", ""))),
        })
        data_points.append({
            "attributes": [
                _kv("gen_ai.request.model", model_id),
                _kv("gen_ai.system", detect_provider(model_id)),
                _kv("gen_ai.token.type", "output"),
            ],
            "asInt": str(int(ct)),
            "startTimeUnixNano": str(_iso_to_ns(span.get("started_at", ""))),
            "timeUnixNano": str(_iso_to_ns(span.get("finished_at", ""))),
        })

    if not data_points:
        return []

    return [{
        "resource": {"attributes": [_kv("service.name", "tag-agent")]},
        "scopeMetrics": [{
            "scope": INSTRUMENTATION_SCOPE,
            "metrics": [{
                "name": "gen_ai.client.token.usage",
                "description": "Token usage for GenAI client calls (OTel semconv)",
                "unit": "{token}",
                "sum": {
                    "dataPoints": data_points,
                    "aggregationTemporality": 2,  # CUMULATIVE
                    "isMonotonic": True,
                },
            }],
        }],
    }]


def _kv(key: str, value: Any) -> dict:
    """Build an OTLP key-value attribute."""
    if isinstance(value, bool):
        return {"key": key, "value": {"boolValue": value}}
    if isinstance(value, int):
        return {"key": key, "value": {"intValue": str(value)}}
    if isinstance(value, float):
        return {"key": key, "value": {"doubleValue": value}}
    return {"key": key, "value": {"stringValue": str(value)}}


def _iso_to_ns(iso: str) -> int:
    """Convert ISO timestamp to Unix nanoseconds."""
    if not iso:
        return 0
    try:
        from datetime import datetime, timezone
        dt = datetime.fromisoformat(iso)
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return int(dt.timestamp() * 1_000_000_000)
    except Exception:
        return 0
