# PRD-037: OTel GenAI Span Cost Attribution

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** S (1–2 days)
**Affects:** `src/tag/tracing.py` (`export_spans_otlp`), `src/tag/config/otel_semconv_version.txt` (new), `src/tag/controller.py` (`cmd_trace` export subcommand, `cmd_config`)

---

## 1. Overview

TAG's `tracing.py` already captures `prompt_tokens` / `completion_tokens` on every inference span and ships them to any OTLP endpoint via `export_spans_otlp`. However, the internal field names (`prompt_tokens`, `completion_tokens`, `model_id`) are TAG-specific; they are not recognized by the emerging OpenTelemetry GenAI semantic conventions. As a result, TAG spans land in Jaeger, Grafana, and AgentOps as opaque blobs: token counts appear under unknown attributes, histograms are absent, and the `gen_ai.*` facets in those tools surface nothing.

This PRD remaps TAG's internal span attributes to the OTel GenAI semantic convention names at export time, so that any consumer that understands the OTel GenAI semconv — Jaeger, Grafana, AgentOps, OpenLIT, Arize Phoenix — receives correctly named attributes with zero operator configuration. A companion histogram metric (`gen_ai.client.token.usage`) is emitted alongside the trace payload.

**Stability caveat.** The OTel GenAI semantic conventions are in **Development** stability status as of June 2026 (semconv version 1.28.0). The attribute names and histogram definition may change before the conventions reach Stable. Implementation therefore version-pins the semconv revision and documents an explicit upgrade path.

---

## 2. Goals

1. **Semconv attribute alignment.** All OTLP trace exports from TAG include `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, `gen_ai.request.model`, `gen_ai.system`, and `gen_ai.operation.name` as span attributes, matching the OTel GenAI semconv names exactly.
2. **Histogram metric export.** Each `tag trace export` call also POSTs a `gen_ai.client.token.usage` OTLP histogram metric (one data point per span) to the same endpoint, enabling cost dashboards in Grafana and similar tools without additional configuration.
3. **Backward compatibility.** The original internal names (`prompt_tokens`, `completion_tokens`, `model_id`) are retained as additional attributes alongside the semconv names, so existing custom dashboards that read the old names continue to work unchanged.
4. **Version-pinned implementation.** The active semconv version is stored in `src/tag/config/otel_semconv_version.txt`. Upgrading requires changing a single file, not code. The pinned version is logged in the export payload's instrumentation scope.
5. **Automatic Grafana / Jaeger / AgentOps compatibility.** A TAG user who points `tag trace export` at a Grafana Tempo or Jaeger instance sees token usage under the standard `gen_ai.*` attribute names without writing a custom field-mapping transform.
6. **Opt-in semconv version override.** Users can override the active semconv version per-project via `tag config set otel.semconv_version 1.28.0`, enabling early adoption of a newer pinned revision without waiting for a TAG release.
7. **Development-stability caveat in docs.** All user-facing help text and config documentation prominently note that the pinned semconv version is in Development stability and the attribute schema may change when conventions are promoted to Stable.

---

## 3. Non-Goals

- **Full OTel SDK integration.** TAG uses a custom, zero-dependency OTLP exporter (`urllib.request`). This PRD does not introduce `opentelemetry-sdk`, `opentelemetry-exporter-otlp-proto-grpc`, or any OTel Python package as a dependency.
- **Server-side OTel collector setup.** TAG does not configure or ship an OpenTelemetry Collector. Users point `tag trace export` at any pre-existing OTLP-compatible endpoint.
- **Mandatory OTel for all users.** The semconv mapping is applied only when the OTLP export path is invoked. Local SQLite storage, the terminal flame-chart, and all other TAG commands are unaffected.
- **gRPC / protobuf transport.** The exporter continues to use OTLP JSON over HTTP (`/v1/traces`, `/v1/metrics`).
- **Metric collection for non-inference spans.** The histogram is emitted only for spans that carry token data (`prompt_tokens > 0` or `completion_tokens > 0`).
- **Automatic migration of historical span data.** Spans already stored in SQLite are not retroactively renamed; the mapping is applied at export time only.

---

## 4. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Platform engineer | point `tag trace export http://tempo:4318` at Grafana Tempo | I see TAG token usage on the built-in `gen_ai.*` dashboard panels with zero field-mapping configuration |
| U2 | DevOps engineer | query `gen_ai.usage.input_tokens` in Jaeger's attribute search | I can filter TAG traces by token count from Jaeger's standard facet sidebar, not a custom field |
| U3 | Developer | connect AgentOps to TAG's OTLP endpoint | TAG inference spans appear in AgentOps' cost attribution view automatically, since AgentOps natively consumes the OTel GenAI semconv |
| U4 | Developer with existing dashboards | run `tag trace export` after upgrading to this feature | My Grafana panels that read the old `prompt_tokens` / `completion_tokens` attribute names continue to work — the old names are still present alongside the new ones |
| U5 | Developer | run `tag config set otel.semconv_version 1.30.0` before exporting | I can test compatibility against a future semconv revision without waiting for a TAG release |

---

## 5. Proposed CLI Surface

### 5.1 Extend `tag trace export` with `--semconv`

The existing command signature:

```
tag trace export ENDPOINT [--trace-id ID] [--profile NAME]
```

is extended to:

```
tag trace export ENDPOINT [--trace-id ID] [--profile NAME] [--semconv VERSION]
```

**New flag:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--semconv VERSION` | `str` | value from `otel_semconv_version.txt` | Override the semconv version used for attribute name mapping in this export. Affects the `otel_scope_version` field in the OTLP payload only; does not change stored span data. |

**Example:**

```sh
# Export with default pinned semconv version (1.28.0)
tag trace export http://tempo.local:4318

# Export with an explicit override
tag trace export http://tempo.local:4318 --semconv 1.30.0
```

### 5.2 New `tag config set otel.semconv_version`

```
tag config set otel.semconv_version 1.28.0
tag config get otel.semconv_version
```

Writes the key `otel.semconv_version` into the active TAG config file (typically `~/.config/tag/default.yaml`). When present, this value is used in place of the `otel_semconv_version.txt` file for all subsequent `tag trace export` calls. The `--semconv` CLI flag takes precedence over both.

**Precedence chain (highest to lowest):**

1. `--semconv VERSION` flag on the CLI
2. `otel.semconv_version` in the TAG config file
3. Contents of `src/tag/config/otel_semconv_version.txt` (shipped with the package)

---

## 6. Functional Requirements

### FR-1: Attribute remapping table

At export time, `export_spans_otlp` maps the following TAG internal fields to OTel GenAI semconv attribute names. The mapping is applied for every span in the export payload.

| Internal name (SQLite / Span field) | OTel GenAI semconv attribute | Type | Notes |
|-------------------------------------|------------------------------|------|-------|
| `prompt_tokens` | `gen_ai.usage.input_tokens` | `intValue` | Maps the integer directly |
| `completion_tokens` | `gen_ai.usage.output_tokens` | `intValue` | Maps the integer directly |
| `model_id` | `gen_ai.request.model` | `stringValue` | Taken from the `model_id` span field |
| _(constant)_ | `gen_ai.system` | `stringValue` | Always set to `"openrouter"` |
| `name` | `gen_ai.operation.name` | `stringValue` | Span name used as-is (e.g. `"chat_step"`) |

### FR-2: Backward-compatible attribute retention

All five semconv attributes from FR-1 are added **alongside** the existing attributes, not as replacements. The original `prompt_tokens`, `completion_tokens`, and `model_id` values remain in the OTLP attribute list under their original key names. Consumers that use the old names are unaffected.

### FR-3: `gen_ai.system` value

The value `"openrouter"` is used because TAG routes all model calls through OpenRouter. If a future TAG configuration introduces direct provider calls, the mapping should be extended to select the appropriate `gen_ai.system` value based on a per-profile provider field.

### FR-4: `gen_ai.client.token.usage` histogram metric export

Alongside the `/v1/traces` POST, `export_spans_otlp` POSTs an OTLP MetricsData payload to `/v1/metrics` on the same endpoint. The payload contains one `gen_ai.client.token.usage` histogram instrument with:

- `description`: `"Measures number of input and output tokens used"`
- `unit`: `"{token}"`
- One `HistogramDataPoint` per inference span (spans with `prompt_tokens > 0` or `completion_tokens > 0`).
- Each data point carries `gen_ai.token.type` as an exemplar attribute: two data points per span — one for `"input"` (value = `prompt_tokens`) and one for `"output"` (value = `completion_tokens`).
- Metric attributes per data point: `gen_ai.request.model`, `gen_ai.system`, `gen_ai.operation.name`.

The histogram export is best-effort: a failure on `/v1/metrics` does not fail the trace export or return an error to the user; it logs a warning only.

### FR-5: Instrumentation scope versioning

The OTLP `scope` object in `scopeSpans` and `scopeMetrics` is populated as:

```json
{
  "name": "tag.tracing",
  "version": "<semconv_version>"
}
```

where `<semconv_version>` is the resolved semconv version string (see Section 5.2 precedence chain).

### FR-6: Version pin file

A file `src/tag/config/otel_semconv_version.txt` is created with the content `1.28.0`. This file is included in the Python package via `pyproject.toml`'s `[tool.setuptools.package-data]`. The file is read once at import time and cached; it is never written to at runtime.

### FR-7: Config key `otel.semconv_version`

`tag config set otel.semconv_version <value>` writes `otel.semconv_version: "<value>"` into the YAML config and `tag config get otel.semconv_version` reads it back. No special validation of the version string is performed beyond confirming it is non-empty.

### FR-8: `--semconv` flag on `tag trace export`

The `--semconv` argument is added to the `trace_export` argparse subparser. When provided, it is passed through to `export_spans_otlp` as the `semconv_version` keyword argument. The value is stored in the instrumentation scope version only; it does not alter which attributes are emitted (the remapping table in FR-1 is fixed for all semconv 1.x versions covered by this PRD).

### FR-9: Help text stability caveat

The `--semconv` flag's help string, the `tag config set otel.semconv_version` help string, and the `tag trace export` command's extended help paragraph must include the phrase: "OTel GenAI semconv is in Development stability as of v1.28.0 — attribute names may change when promoted to Stable."

### FR-10: Zero-emission guard

If `export_spans_otlp` is called with rows that contain no inference spans (all rows have `prompt_tokens = 0` and `completion_tokens = 0`), the `/v1/metrics` POST is skipped entirely. The `/v1/traces` POST proceeds normally.

### FR-11: Attribute emission guards

`gen_ai.request.model` is only emitted when `model_id` is non-null and non-empty. `gen_ai.usage.input_tokens` is only emitted when `prompt_tokens > 0`. `gen_ai.usage.output_tokens` is only emitted when `completion_tokens > 0`. This avoids polluting spans with zero-value or null attributes, which some backends treat as present-and-meaningful.

---

## 7. Non-Functional Requirements

### NFR-1: No performance impact on span export

The attribute remapping is a pure dictionary transform operating on already-fetched SQLite rows. It must add no measurable latency beyond the existing network round-trip. No new network calls are introduced beyond the already-existing `/v1/traces` POST and the new `/v1/metrics` POST (which is best-effort and non-blocking from the user's perspective in terms of exit code).

### NFR-2: Zero new runtime dependencies

The implementation uses only Python standard library modules already used by `tracing.py` (`json`, `urllib.request`, `urllib.error`). No OTel SDK packages are added to `pyproject.toml`.

### NFR-3: Version pin file as single source of truth

The file `src/tag/config/otel_semconv_version.txt` is the canonical default. It must be updated (a one-line change) when TAG adopts a new semconv revision. The CI test suite pins the expected version and will fail if the file is deleted or emptied, alerting maintainers to an accidental removal.

### NFR-4: Metrics POST failure is silent

A `urllib.error.URLError` or non-2xx HTTP response from `/v1/metrics` is caught, a single warning line is printed to stderr, and `export_spans_otlp` returns `True` (success for the trace export). The return value contract of `export_spans_otlp` is unchanged: `True` means the trace POST succeeded.

---

## 8. Technical Design

### 8.1 Changed files

| File | Change |
|------|--------|
| `src/tag/tracing.py` | Extend `export_spans_otlp` with semconv mapping and histogram emission |
| `src/tag/controller.py` | Add `--semconv` flag to `trace_export` argparse subparser; read `otel.semconv_version` from config; pass resolved version to `export_spans_otlp` |
| `src/tag/config/otel_semconv_version.txt` | New file; content: `1.28.0` |
| `pyproject.toml` | Add `"tag/config/otel_semconv_version.txt"` to `[tool.setuptools.package-data]` |

### 8.2 `export_spans_otlp` signature change

```python
def export_spans_otlp(
    rows: list[Any],
    endpoint: str,
    headers: dict[str, str] | None = None,
    semconv_version: str | None = None,   # NEW
) -> bool:
```

`semconv_version` defaults to `None`; when `None`, the function resolves it from the precedence chain (config file key, then `otel_semconv_version.txt`). For simplicity in the initial implementation, the function reads `otel_semconv_version.txt` as a fallback and accepts the caller-provided value directly. Config file resolution is the responsibility of `cmd_trace` in `controller.py`.

### 8.3 Semconv attribute injection (inside `export_spans_otlp`)

```python
_SEMCONV_MAP = {
    # (source_col, semconv_key, value_key_in_otlp)
    # Handled explicitly below because they require int, not string, OTLP value types.
}

def _semconv_attrs(s: dict, system: str = "openrouter") -> list[dict]:
    attrs = []
    if s.get("model_id"):
        attrs.append({"key": "gen_ai.request.model",
                      "value": {"stringValue": s["model_id"]}})
    attrs.append({"key": "gen_ai.system",
                  "value": {"stringValue": system}})
    attrs.append({"key": "gen_ai.operation.name",
                  "value": {"stringValue": s["name"]}})
    if (s.get("prompt_tokens") or 0) > 0:
        attrs.append({"key": "gen_ai.usage.input_tokens",
                      "value": {"intValue": str(s["prompt_tokens"])}})
    if (s.get("completion_tokens") or 0) > 0:
        attrs.append({"key": "gen_ai.usage.output_tokens",
                      "value": {"intValue": str(s["completion_tokens"])}})
    return attrs
```

These attributes are appended to the existing `attributes` list already built from the span's JSON blob and the legacy `prompt_tokens` / `completion_tokens` fields.

### 8.4 `gen_ai.client.token.usage` histogram OTLP payload structure

```json
{
  "resourceMetrics": [{
    "resource": {
      "attributes": [
        {"key": "service.name", "value": {"stringValue": "tag-agent"}}
      ]
    },
    "scopeMetrics": [{
      "scope": {"name": "tag.tracing", "version": "<semconv_version>"},
      "metrics": [{
        "name": "gen_ai.client.token.usage",
        "description": "Measures number of input and output tokens used",
        "unit": "{token}",
        "histogram": {
          "dataPoints": [
            {
              "attributes": [
                {"key": "gen_ai.request.model", "value": {"stringValue": "<model>"}},
                {"key": "gen_ai.system",         "value": {"stringValue": "openrouter"}},
                {"key": "gen_ai.operation.name", "value": {"stringValue": "<span_name>"}},
                {"key": "gen_ai.token.type",     "value": {"stringValue": "input"}}
              ],
              "count": "1",
              "sum": <prompt_tokens as float>,
              "bucketCounts": ["0", "1"],
              "explicitBounds": [<prompt_tokens as float>]
            },
            {
              "attributes": [
                {"key": "gen_ai.request.model", "value": {"stringValue": "<model>"}},
                {"key": "gen_ai.system",         "value": {"stringValue": "openrouter"}},
                {"key": "gen_ai.operation.name", "value": {"stringValue": "<span_name>"}},
                {"key": "gen_ai.token.type",     "value": {"stringValue": "output"}}
              ],
              "count": "1",
              "sum": <completion_tokens as float>,
              "bucketCounts": ["0", "1"],
              "explicitBounds": [<completion_tokens as float>]
            }
          ],
          "aggregationTemporality": 2
        }
      }]
    }]
  }]
}
```

`aggregationTemporality: 2` = DELTA, which is the correct value for per-export token counts.

### 8.5 Version pin file loading

```python
from importlib.resources import files  # Python 3.9+

def _default_semconv_version() -> str:
    try:
        return (
            files("tag.config")
            .joinpath("otel_semconv_version.txt")
            .read_text(encoding="utf-8")
            .strip()
        )
    except Exception:
        return "1.28.0"  # hard-coded fallback
```

### 8.6 `cmd_trace` controller changes

In `controller.py`, the `trace_export` argparse subparser gains:

```python
trace_export.add_argument(
    "--semconv",
    metavar="VERSION",
    dest="semconv_version",
    default=None,
    help=(
        "OTel GenAI semconv version to record in the instrumentation scope "
        "(default: from config otel.semconv_version or bundled otel_semconv_version.txt). "
        "NOTE: OTel GenAI semconv is in Development stability as of v1.28.0 — "
        "attribute names may change when promoted to Stable."
    ),
)
```

The resolved version is computed in `cmd_trace` before calling `export_spans_otlp`:

```python
semconv_version = (
    getattr(args, "semconv_version", None)
    or cfg.get("otel", {}).get("semconv_version")
    or _default_semconv_version()
)
ok = export_spans_otlp(rows, endpoint, semconv_version=semconv_version)
```

---

## 9. Security Considerations

1. **No new attack surface.** The semconv mapping is a pure string substitution on data already present in local SQLite rows. It does not introduce new network listeners, file writes, subprocess calls, or external data fetches. The OTLP endpoint is user-supplied and was already trusted before this feature.
2. **Semconv version pin prevents unexpected schema drift.** By shipping a pinned `otel_semconv_version.txt` and requiring an explicit `tag config set otel.semconv_version` or `--semconv` flag to override it, TAG cannot silently emit attribute names that belong to an untested semconv revision. If the OTel GenAI conventions change an attribute name in a future revision, the change will not appear in TAG spans until a maintainer deliberately updates the pin file and the mapping table together, with a corresponding test update.

---

## 10. Testing Strategy

### 10.1 Unit tests — attribute mapping (`tests/test_otel_semconv.py`)

- **`test_semconv_attrs_all_fields`**: construct a span dict with all fields populated; assert that the returned OTLP attribute list contains exactly the five semconv keys with correct values.
- **`test_semconv_attrs_missing_model`**: span dict with `model_id = None`; assert `gen_ai.request.model` is absent from the output.
- **`test_semconv_attrs_zero_tokens`**: span dict with `prompt_tokens = 0`, `completion_tokens = 0`; assert neither `gen_ai.usage.input_tokens` nor `gen_ai.usage.output_tokens` appears.
- **`test_semconv_backward_compat`**: call `export_spans_otlp` with a mock HTTP server; assert the raw JSON payload contains both `prompt_tokens` (old) and `gen_ai.usage.input_tokens` (new) as separate attributes on the same span.
- **`test_semconv_gen_ai_system_constant`**: assert `gen_ai.system` is always `"openrouter"` regardless of model_id value.

### 10.2 OTLP JSON schema validation tests

- **`test_otlp_trace_payload_valid`**: call `export_spans_otlp` against a mock HTTP handler that captures the request body; parse the JSON and validate the OTLP `resourceSpans` structure (keys `traceId`, `spanId`, `attributes` present; each attribute has `key` and `value`).
- **`test_otlp_metrics_payload_valid`**: same mock handler captures `/v1/metrics` body; validate the `resourceMetrics` → `scopeMetrics` → `metrics[0].histogram` path exists and `dataPoints` are non-empty for a span with tokens.
- **`test_otlp_metrics_skipped_no_tokens`**: span with zero tokens; assert `/v1/metrics` is never called (mock asserts call count = 0).

### 10.3 Histogram format tests

- **`test_histogram_delta_temporality`**: assert `aggregationTemporality` in the metrics payload is `2` (DELTA).
- **`test_histogram_two_data_points_per_span`**: for a span with both `prompt_tokens > 0` and `completion_tokens > 0`, assert exactly two data points are generated with `gen_ai.token.type` values `"input"` and `"output"` respectively.

### 10.4 Version pin file tests

- **`test_version_pin_file_exists`**: assert `importlib.resources` can locate `otel_semconv_version.txt` and that its content is a non-empty string matching the pattern `^\d+\.\d+\.\d+$`.
- **`test_semconv_version_cli_override`**: call `export_spans_otlp` with `semconv_version="9.99.0"`; assert the OTLP scope `version` field in both trace and metrics payloads equals `"9.99.0"`.

---

## 11. Acceptance Criteria

| # | Criterion | How to verify |
|---|-----------|---------------|
| AC-1 | `tag trace export http://localhost:4318` sends an OTLP JSON payload containing `gen_ai.usage.input_tokens` and `gen_ai.usage.output_tokens` as integer attributes on each inference span | Capture the POST body with a local netcat listener or the test mock; inspect JSON |
| AC-2 | The same payload retains `prompt_tokens` and `completion_tokens` as attributes alongside the semconv names | Same JSON inspection as AC-1 |
| AC-3 | A `/v1/metrics` POST is made to the same endpoint with a valid `gen_ai.client.token.usage` histogram containing one `"input"` and one `"output"` data point per inference span | Capture the metrics POST body; parse JSON; assert structure |
| AC-4 | `tag trace export http://localhost:4318 --semconv 9.99.0` sets the OTLP instrumentation scope `version` field to `"9.99.0"` in both the trace and metrics payloads | Capture and inspect both POST bodies |
| AC-5 | `tag config set otel.semconv_version 1.29.0` followed by `tag trace export http://localhost:4318` uses `"1.29.0"` as the scope version (without passing `--semconv`) | Inspect scope version field in captured POST body |
| AC-6 | Spans with `prompt_tokens = 0` and `completion_tokens = 0` do not emit `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, or a `/v1/metrics` POST | Unit test + integration test with zero-token span rows |
| AC-7 | `tag trace export` still returns exit code 0 when the `/v1/metrics` POST fails (connection refused on the metrics endpoint) | Integration test: start HTTP server that rejects POST to `/v1/metrics` but accepts `/v1/traces`; assert exit code 0 and a warning line on stderr |
| AC-8 | `src/tag/config/otel_semconv_version.txt` ships as part of the installed package and is readable via `importlib.resources` | `pip install -e .` in a clean venv; `python -c "from importlib.resources import files; print(files('tag.config').joinpath('otel_semconv_version.txt').read_text())"` |

---

## 12. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| `src/tag/tracing.py` — `export_spans_otlp` | Internal | The existing function is extended in-place; the call signature is backward-compatible (new `semconv_version` kwarg defaults to `None`) |
| `src/tag/controller.py` — `cmd_trace` export path | Internal | Reads `otel.semconv_version` from the loaded config dict and passes it to the updated `export_spans_otlp` |
| `src/tag/config/` directory | Internal | Already exists (`default.yaml`, etc.); `otel_semconv_version.txt` is added here |
| `pyproject.toml` `[tool.setuptools.package-data]` | Build | Must include `tag/config/*.txt` or `tag/config/otel_semconv_version.txt` explicitly for the pin file to be included in the wheel |
| Python `importlib.resources` (`files` API) | Stdlib | Available since Python 3.9; TAG already requires ≥ 3.9 |

---

## 13. Open Questions

| # | Question | Owner | Notes |
|---|----------|-------|-------|
| OQ-1 | When will OTel GenAI semconv reach Stable stability? | OTel SIG | As of June 2026 the conventions are in Development. The TAG maintainer must monitor the [opentelemetry-specification releases](https://github.com/open-telemetry/semantic-conventions/releases) and update `otel_semconv_version.txt` plus the mapping table when Stable is declared. Recommend subscribing to GitHub release notifications for `open-telemetry/semantic-conventions`. |
| OQ-2 | How should TAG handle a breaking semconv change (e.g., attribute renamed from `gen_ai.usage.input_tokens` to something else)? | TAG maintainers | The mapping table in `export_spans_otlp` and the version pin file are the only two places that need updating. A minor TAG release bumping the pin and updating the table is sufficient. Because the old names are retained (FR-2), existing dashboards continue to work through any transition period. |
| OQ-3 | Should `gen_ai.system` be configurable per-profile for users who route some profiles directly to Anthropic or OpenAI rather than through OpenRouter? | Product | Out of scope for this PRD. The constant `"openrouter"` is correct for the current TAG architecture. If direct-provider routing is added (see PRD-036), the `gen_ai.system` value should be derived from a `provider` field in the profile config. |
| OQ-4 | Should the histogram use cumulative (`aggregationTemporality: 1`) instead of delta (`2`)? | Engineering | Delta is correct for a per-export batch where each call represents a discrete set of spans. Cumulative would require maintaining a running counter across export calls, which the current stateless `export_spans_otlp` function does not support. |

---

## 14. Complexity and Timeline

**Complexity:** S

**Estimated implementation time:** 1–2 days

| Task | Hours |
|------|-------|
| Add `_semconv_attrs` helper and inject into `export_spans_otlp` trace payload | 1 |
| Add histogram metrics POST in `export_spans_otlp` | 2 |
| Add `semconv_version` parameter and version pin file loading | 1 |
| Add `--semconv` flag and config key resolution in `controller.py` | 1 |
| Create `src/tag/config/otel_semconv_version.txt`; update `pyproject.toml` package-data | 0.5 |
| Write unit and integration tests (`tests/test_otel_semconv.py`) | 2 |
| Update `tag trace export` help text with stability caveat | 0.5 |
| **Total** | **8 hours** |

The implementation is entirely confined to `tracing.py` and the export path in `controller.py`. No schema migrations, no new SQLite tables, no new commands, and no new dependencies are required. Risk of regression to existing functionality is low because `export_spans_otlp` adds attributes without removing any and the new metrics POST is best-effort.
