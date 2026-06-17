# PRD-048: Structured Tool-Call Child Spans with TOOL Kind (`tag trace show --kind tool`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** S (3-5 days)
**Category:** Evaluation & Observability
**Affects:** `tracing.py + controller.py`
**Depends on:** PRD-013 (agent tracing & observability), PRD-027 (eval framework), PRD-028 (sandbox code execution), PRD-034 (secret scanning), PRD-041 (OTel GenAI span cost attribution), PRD-044 (AgentOps session observability)
**Inspired by:** Arize Phoenix OpenInference, W&B Weave, Braintrust

---

## 1. Overview

TAG's tracing system (PRD-013) already captures every agent run as a tree of `Span` objects stored in `~/.tag/runtime/tag.sqlite3`. However, every span in the current schema carries identical structure regardless of whether it represents an LLM inference step, a tool call, an embedding computation, or an entire agent chain. There is no `kind` field to distinguish span types, no child-span granularity below the step level for tool dispatches, and no filtering mechanism in `tag trace show` to surface only tool-call spans. This makes it impossible to answer the basic observability question: "which tools did this run call, in what order, and how long did each take?"

Every major observability platform that has seriously addressed agentic AI workloads has converged on a span-kind taxonomy. Arize Phoenix's OpenInference specification defines `CHAIN`, `LLM`, `TOOL`, `EMBEDDING`, and `AGENT` as the canonical span kinds. W&B Weave uses the same TOOL/LLM distinction to power its call graph UI. Braintrust wraps every tool invocation in a nested span that appears as a child of the LLM call that requested it. All three platforms share the insight that tool spans carry structurally different metadata from LLM spans — they have `tool.name`, `tool.input`, `tool.output`, and sometimes `tool.error` attributes — and that filtering a trace to only tool spans is one of the most common debugging operations.

This PRD adds a `kind` field to TAG's `Span` dataclass and `spans` SQLite table. The field accepts the OpenInference vocabulary: `LLM`, `TOOL`, `CHAIN`, `AGENT`, `EMBEDDING`, defaulting to `LLM` for backward compatibility with all existing span-creation sites in `controller.py`. Every tool dispatch inside `controller.py`'s agent loop is instrumented to emit a child `Span` with `kind = "TOOL"` and a standard set of `tool.*` attributes following the OpenInference dot-notation convention. The `tag trace show` command gains a `--kind` flag to filter spans to a specific kind, and `tag stats --by tool` surfaces per-tool aggregates (call count, p50/p95 latency, error rate) across a configurable time window.

The design is deliberately minimal: no new Python dependencies, no schema breaking changes (the new `kind` column is added with `ALTER TABLE ... ADD COLUMN` guarded by a version check, with a default of `"LLM"` so all historical rows remain valid), and no changes to the OTLP export path beyond adding `kind` as a span attribute. The implementation touches exactly two files — `tracing.py` and `controller.py` — plus the schema migration path in `open_db`. Estimated net diff is under 300 lines of Python.

The business value is immediate. Platform engineers debugging a failed agent run will see the tool call waterfall at a glance. Eval authors building `tag eval` suites (PRD-027) can now correlate per-tool latency with task completion scores. Teams using the OTLP export path (PRD-041) will find their Phoenix / Jaeger dashboards automatically populated with structured TOOL spans rather than opaque blobs. The feature creates the foundation for a subsequent `tag eval --by tool` quality report and for the `ToolCorrectnessMetric` / `ArgumentCorrectnessMetric` agentic evaluators that require knowing which tool was called and what arguments were passed.

---

## 2. Problem Statement

### 2.1 Tool calls are invisible in the current trace tree

`tag trace show <trace-id>` renders a flame-chart tree where every node is a step-level span (kind not specified, implicitly LLM). Tool calls that the agent makes within a step — `bash`, `read_file`, `write_file`, `web_search`, `semantic_memory_search`, and any MCP-registered tool — produce no span of their own. The only record that a tool was called exists in the step's `attributes` JSON blob if the calling code happens to serialize the tool name there, which is inconsistent across controller code paths. A 30-tool agentic run looks like a single node in the flame-chart. There is nothing to drill into to understand tool behavior.

### 2.2 No kind taxonomy means no kind-specific aggregation or filtering

Without a `kind` field on spans, there is no efficient way to query "all tool spans in this trace" at the database level. Implementing a `--kind tool` filter today would require deserializing the `attributes` JSON blob of every span and applying a Python-level predicate — O(N) deserialization for what should be an O(1) indexed lookup. Downstream platforms that consume TAG's OTLP export (PRD-041) likewise have no `SpanKind` or equivalent signal to route tool spans to tool-specific dashboards; they see all spans as generic `INTERNAL` kind spans. Arize Phoenix's OpenInference-aware ingestion pipeline specifically looks for the `openinference.span.kind` attribute to drive its span-type facet filtering, and currently produces no useful output for TAG exports.

### 2.3 Tool-level performance and reliability analytics are not possible

Engineering teams running TAG in CI or production environments need to know which tools are slow, which tools fail most often, and whether tool error rates are correlated with task failure rates. None of this is possible today because the data simply does not exist in the database. `tag stats` reports run-level aggregates (total tokens, total cost, run duration) but has no concept of per-tool metrics. Adding `--by tool` to `tag stats` is blocked by the absence of `kind`-tagged TOOL spans. The result is that teams debug tool reliability the hard way: by grepping raw Hermes log files or by adding one-off print statements to `controller.py`.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | Add a `kind` field (`LLM` / `TOOL` / `CHAIN` / `AGENT` / `EMBEDDING`) to the `Span` dataclass and `spans` SQLite table, defaulting to `"LLM"` for backward compatibility. |
| G2 | Instrument every tool dispatch path in `controller.py` with a child `Span` of `kind="TOOL"`, carrying `tool.name`, `tool.input`, `tool.output`, `tool.error` attributes following OpenInference dot-notation. |
| G3 | Add `--kind <KIND>` to `tag trace show` so users can filter the rendered flame-chart to only spans of a given kind (e.g. `--kind tool`). |
| G4 | Add `--run-id <id>` as an alias for `--trace-id` / positional `TRACE_ID` in `tag trace show` to match the key CLI surface described in the GitHub issue. |
| G5 | Add `tag stats --by tool --since 7d [--json]` to surface per-tool call count, p50/p95 latency, and error rate aggregated across all runs in the time window. |
| G6 | Extend `tag otel-export` to include `kind` as the `openinference.span.kind` attribute on every exported span, enabling Phoenix/Jaeger span-type facet filtering. |
| G7 | Add the `kind` column to the `spans` table via a backward-compatible `ALTER TABLE ... ADD COLUMN` migration guarded in `open_db`, with `DEFAULT 'LLM'` so all historical rows remain valid. |
| G8 | Zero new required Python dependencies. The implementation must use only the stdlib and packages already present in TAG's dependency tree. |

## 3.1 Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Retroactively back-filling `kind` on historical spans that predate this feature. Historical spans remain `kind = "LLM"` (the column default). |
| NG2 | Capturing tool call arguments that contain PII or secrets. `tool.input` serialization must respect the existing secret-scanning gate (PRD-034). |
| NG3 | Adding a new `TOOL` span for every MCP protocol message. Instrumentation covers the TAG-level dispatch layer, not MCP wire frames. |
| NG4 | Streaming tool span updates in real time to a remote OTLP endpoint. Spans are written to SQLite at close-time, as today. OTLP export remains a pull operation. |
| NG5 | Building a tool-quality LLM judge. Tool evaluation (ToolCorrectnessMetric, ArgumentCorrectnessMetric) is the domain of PRD-027's eval framework; this PRD only captures the structural trace data that those evaluators will consume. |
| NG6 | Adding `EMBEDDING` spans in this PRD. The `EMBEDDING` kind is added to the enum for forward-compatibility but no embedding dispatch is instrumented here; that is deferred to PRD-043 (vector-based tool retrieval). |
| NG7 | Modifying the `tag trace list` output. Only `tag trace show` and `tag stats` gain new surface area. |
| NG8 | Adding `kind` to `tag trace diff` or `tag trace replay` (PRD-032). Those commands operate on snapshot blobs and are out of scope. |

---

## 4. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| TOOL span coverage | >= 95% of tool dispatches in `controller.py`'s agent loop produce a `kind="TOOL"` child span | Integration test: run a task that triggers 10 distinct tools; assert 10 TOOL spans exist in the DB |
| `--kind tool` filter correctness | `tag trace show <id> --kind tool` returns only spans where `kind = "TOOL"` | Unit test: insert mixed-kind spans; assert filter returns only TOOL rows |
| Schema migration safety | Running `open_db()` on a pre-PRD-048 database does not raise an error or drop any data | Integration test: create DB without `kind` column; call `open_db()`; assert no error and all pre-existing rows present |
| Backward compatibility | All existing `tag trace show` invocations (without `--kind`) render identical output to pre-PRD-048 for traces with no TOOL spans | Regression test on fixture traces |
| `tag stats --by tool` correctness | Aggregated call counts and latencies match manually summed TOOL spans for a controlled test run | Integration test with deterministic tool durations |
| OTel export includes `kind` | OTLP JSON payload for a trace containing TOOL spans includes `openinference.span.kind = "TOOL"` as a string attribute on every TOOL span | Unit test on `spans_to_otlp_json` with kind-tagged input |
| Tool span overhead | Creating and closing a TOOL child span adds < 1 ms of wall time to a tool dispatch | Benchmark: `timeit` open_span + close_span + save_spans_to_db for a single TOOL span |
| `tag stats` query performance | `tag stats --by tool --since 7d` returns in < 200 ms on a DB with 50,000 spans | Benchmark test with seeded fixture data |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|------------|----------|
| U1 | Developer debugging a failed run | run `tag trace show --run-id abc123 --kind tool` | I immediately see the waterfall of tool calls — names, durations, inputs, outputs, errors — without manually grepping logs |
| U2 | Platform engineer | run `tag stats --by tool --since 7d --json` | I can identify which tools are the slowest or most error-prone across all runs in the last week and prioritize optimization work |
| U3 | Eval author | run `tag trace show --run-id abc123 --kind tool --json` and pipe to a script | I can extract the structured tool call sequence to compare against expected tool usage in a `ToolCorrectnessMetric` eval case (PRD-027) |
| U4 | DevOps engineer | export TAG traces to Arize Phoenix | Tool spans appear in Phoenix's span-type facet with `openinference.span.kind = "TOOL"`, enabling Phoenix's built-in tool call analytics dashboard without any mapping configuration |
| U5 | Developer | run `tag trace show --run-id abc123` without `--kind` | I see the complete trace tree, unchanged from today — TOOL child spans are displayed as nested children of their parent LLM step spans |
| U6 | Security engineer | inspect `tool.input` attributes on TOOL spans | I can audit what arguments the agent passed to sensitive tools like `bash` or `write_file`, with the guarantee that values flagged by PRD-034 secret scanning are redacted before storage |
| U7 | Team lead | run `tag stats --by tool --since 30d` in a weekly review | I can show tool usage trends — which tools are being called more or less often — as a proxy for agent behavior changes across profile updates |
| U8 | Developer | run `tag trace show --run-id abc123 --kind llm` | I can filter to only LLM inference spans to see token counts and latencies without the tool call noise |

---

## 6. Proposed CLI Surface

### 6.1 `tag trace show` — extended

**Current signature:**
```
tag trace show TRACE_ID [--json]
```

**New signature:**
```
tag trace show (TRACE_ID | --run-id <id>) [--kind <KIND>] [--json]
```

**New flags:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--run-id <id>` | `str` | `None` | Alias for the positional `TRACE_ID` argument. Accepts a TAG run ID (short hex, e.g. `abc123ef`) or a full trace UUID. Mutually exclusive with the positional argument. |
| `--kind <KIND>` | `str` | `None` (all kinds) | Filter displayed spans to those with `kind` matching the given value. Case-insensitive. Valid values: `llm`, `tool`, `chain`, `agent`, `embedding`. |

**Example — unfiltered (existing behavior, now shows TOOL child spans):**
```
$ tag trace show abc123ef
▸ agent_run  ████████████████████░░░░  12340ms  847↑1203↓
  ▸ step.1.orchestrator  ████████░░░░░░░░░░░░  4210ms  421↑388↓
    ▸ tool:bash  █░░░░░░░░░░░░░░░░░░░  380ms  0↑0↓
    ▸ tool:read_file  █░░░░░░░░░░░░░░░░░░░  42ms  0↑0↓
  ▸ step.2.orchestrator  ████████░░░░░░░░░░░░  5100ms  426↑815↓
    ▸ tool:write_file  ██░░░░░░░░░░░░░░░░░░  210ms  0↑0↓
    ▸ tool:bash  ██░░░░░░░░░░░░░░░░░░  290ms  0↑0↓
```

**Example — filtered to TOOL spans only:**
```
$ tag trace show abc123ef --kind tool
TOOL spans for trace abc123ef
──────────────────────────────────────────────────────────────────────────────
  tool          started_at              duration_ms  status  error
  bash          2026-06-17T09:01:22Z   380          ok
  read_file     2026-06-17T09:01:26Z   42           ok
  write_file    2026-06-17T09:01:31Z   210          ok
  bash          2026-06-17T09:01:34Z   290          error   exit code 1
──────────────────────────────────────────────────────────────────────────────
4 TOOL spans  |  922ms total  |  1 error (25.0%)
```

**Example — filtered to TOOL spans, JSON output:**
```
$ tag trace show abc123ef --kind tool --json
[
  {
    "id": "a1b2c3d4e5f6",
    "trace_id": "abc123ef...",
    "parent_id": "step1spanid",
    "name": "tool:bash",
    "kind": "TOOL",
    "started_at": "2026-06-17T09:01:22.413Z",
    "finished_at": "2026-06-17T09:01:22.793Z",
    "duration_ms": 380,
    "status": "ok",
    "attributes": {
      "tool.name": "bash",
      "tool.input": "{\"command\": \"ls -la /tmp\"}",
      "tool.output": "total 0\ndrwxr-xr-x ...",
      "tool.error": null
    }
  },
  ...
]
```

**Example — using `--run-id` alias:**
```
$ tag trace show --run-id abc123ef --kind tool
# equivalent to above
```

---

### 6.2 `tag stats --by tool`

**New flag on existing `tag stats` command:**

```
tag stats [--by tool] [--since <duration>] [--profile <name>] [--json]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--by tool` | flag | off | Aggregate stats at tool granularity instead of run granularity. |
| `--since <duration>` | `str` | `7d` | Time window. Accepts `Nd` (N days), `Nh` (N hours), `Nw` (N weeks). |
| `--profile <name>` | `str` | `None` | Restrict to spans owned by a specific TAG profile. |
| `--json` | flag | off | Output machine-readable JSON. |

**Example — human-readable:**
```
$ tag stats --by tool --since 7d
Tool usage — last 7 days
────────────────────────────────────────────────────────────────────────────
tool            calls  errors  err%    p50ms   p95ms   total_ms
────────────────────────────────────────────────────────────────────────────
bash            142    8       5.6%    310     1840    52710
read_file       98     0       0.0%    38      112     4122
write_file      71     2       2.8%    195     640     17105
web_search      34     4       11.8%   2100    8200    88440
semantic_query  22     0       0.0%    95      380     3090
────────────────────────────────────────────────────────────────────────────
Total           367    14      3.8%    —       —       165467ms (2.76 hrs)
```

**Example — JSON:**
```
$ tag stats --by tool --since 7d --json
{
  "window": "7d",
  "generated_at": "2026-06-17T09:05:00Z",
  "tools": [
    {
      "tool_name": "bash",
      "call_count": 142,
      "error_count": 8,
      "error_rate": 0.0563,
      "p50_ms": 310,
      "p95_ms": 1840,
      "total_ms": 52710
    },
    ...
  ],
  "totals": {
    "call_count": 367,
    "error_count": 14,
    "error_rate": 0.0381,
    "total_ms": 165467
  }
}
```

---

### 6.3 `tag otel-export status --json`

**New subcommand on existing `tag otel-export`:**

```
tag otel-export status [--json]
```

Reports the currently configured OTLP endpoint, whether it is reachable, the total number of spans eligible for export, and how many carry `kind` values. This is a read-only diagnostic.

**Example:**
```
$ tag otel-export status --json
{
  "endpoint": "http://localhost:4317",
  "reachable": true,
  "spans_total": 1847,
  "spans_by_kind": {
    "LLM": 412,
    "TOOL": 1203,
    "CHAIN": 232,
    "AGENT": 0,
    "EMBEDDING": 0,
    "unknown": 0
  },
  "semconv_version": "1.28.0"
}
```

---

## 7. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `Span` dataclass in `tracing.py` MUST have a `kind: str` field with default value `"LLM"`. | P0 |
| FR-02 | The `spans` SQLite table MUST have a `kind TEXT NOT NULL DEFAULT 'LLM'` column. The column is added via `ALTER TABLE spans ADD COLUMN kind TEXT NOT NULL DEFAULT 'LLM'` if not present, guarded by a `PRAGMA table_info` check in `open_db`. | P0 |
| FR-03 | `open_span()` in `tracing.py` MUST accept an optional `kind: str = "LLM"` parameter and set `span.kind` accordingly. | P0 |
| FR-04 | `save_spans_to_db()` MUST persist `span.kind` into the `kind` column. The `INSERT OR REPLACE` statement must be updated to include `kind`. | P0 |
| FR-05 | Every invocation of a tool in `controller.py`'s agent execution loop MUST create a child `Span` with `kind="TOOL"`, `parent_id` set to the enclosing step span's ID, and `name` set to `f"tool:{tool_name}"`. | P0 |
| FR-06 | Every TOOL span MUST carry these attributes (dot-notation, OpenInference convention): `tool.name` (str), `tool.input` (JSON string, redacted per FR-14), `tool.output` (str, truncated to 4096 chars), `tool.error` (str or null). | P0 |
| FR-07 | `tag trace show <id> --kind <KIND>` MUST filter rendered output to spans where `spans.kind = UPPER(KIND)`. The filter MUST be applied at the SQL query level (`WHERE kind = ?`), not in Python post-processing. | P0 |
| FR-08 | `tag trace show <id>` WITHOUT `--kind` MUST display all spans including TOOL child spans nested under their parent step spans in the existing flame-chart tree format. | P0 |
| FR-09 | `--run-id <id>` on `tag trace show` MUST be accepted as an alias for the positional `TRACE_ID` argument, resolving in the same way (first matched against `spans.trace_id`, then against `runs.id` via a join). | P1 |
| FR-10 | `tag stats --by tool --since <duration>` MUST query `spans` where `kind = 'TOOL'` and `started_at >= <cutoff>`, group by `JSON_EXTRACT(attributes, '$.tool.name')`, and compute `COUNT(*)`, `SUM(CASE WHEN status != 'ok' THEN 1 ELSE 0 END)`, and percentile latencies. | P1 |
| FR-11 | Percentile latency computation in `tag stats --by tool` MUST use a pure-SQL approach compatible with SQLite 3.38+ (window functions `PERCENT_RANK()` or the percentile approximation pattern). | P1 |
| FR-12 | `tag otel-export status --json` MUST perform a read-only database query to return span counts by kind, plus a TCP reachability check (socket connect with 2s timeout) of the configured OTLP endpoint. | P2 |
| FR-13 | OTLP export in `otel_semconv.py` (`spans_to_otlp_json`) MUST include `openinference.span.kind` as a string-typed span attribute on every exported span, set to the span's `kind` value. | P1 |
| FR-14 | Before storing `tool.input` on a TOOL span's `attributes`, the value MUST be passed through the secret-scanning redaction function from `security.py` (`scan_for_secrets` or equivalent). If a secret is detected, the attribute value is replaced with `"<redacted>"`. | P0 |
| FR-15 | The `kind` field on `Span` MUST be validated at construction time: only `"LLM"`, `"TOOL"`, `"CHAIN"`, `"AGENT"`, `"EMBEDDING"` are accepted; any other value raises `ValueError`. | P1 |
| FR-16 | `render_trace_terminal()` in `tracing.py` MUST include each span's `kind` as a short prefix badge (e.g. `[T]` for TOOL, `[L]` for LLM) in the flame-chart label when the span list contains mixed kinds. | P2 |
| FR-17 | `tag trace show --kind tool --json` MUST include the full `attributes` dict in each returned span record, not a truncated summary. | P1 |
| FR-18 | Closing a TOOL span MUST call `save_spans_to_db` (or the equivalent batch flush) before the enclosing step span is closed, so that partial traces written on crash contain TOOL span data. | P1 |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | TOOL span creation overhead must not exceed 1 ms wall time per tool dispatch (amortized across open + close + in-memory append). | < 1 ms per span |
| NFR-02 | `tag stats --by tool --since 7d` must return in under 200 ms on a database with 50,000 spans in WAL mode. The `kind` column must have an index supporting the WHERE clause. | < 200 ms |
| NFR-03 | The `kind` column index must be created as part of the schema migration: `CREATE INDEX IF NOT EXISTS idx_spans_kind ON spans(kind, started_at)`. | Required |
| NFR-04 | No new `pip install` dependencies are introduced by this PRD. All implementation uses `stdlib`, `sqlite3`, and existing TAG modules. | Hard constraint |
| NFR-05 | `tool.output` stored in `attributes` must be truncated to a maximum of 4096 characters to prevent unbounded database growth from large file reads. A truncation marker `" [truncated]"` is appended when truncation occurs. | 4096 char max |
| NFR-06 | `tool.input` JSON serialization must not fail on non-JSON-serializable tool arguments; non-serializable values are replaced with their `repr()` string. | No exceptions |
| NFR-07 | The schema migration (`ALTER TABLE ... ADD COLUMN`) must be idempotent: running it twice on the same database must not raise an error. | Idempotent |
| NFR-08 | All TOOL spans for a given step must share the same `trace_id` as their parent LLM step span and must have `parent_id` set to the step span's `id`. | Required for tree integrity |
| NFR-09 | Secret scanning of `tool.input` must complete in under 5 ms for inputs up to 10 KB. | < 5 ms |
| NFR-10 | The `tag trace show --kind tool` query path must use a prepared SQL statement with `WHERE kind = ?` and rely on `idx_spans_kind` for O(log N) lookup, not a full table scan. | Index-backed query |

---

## 9. Technical Design

### 9.1 Updated `Span` dataclass in `tracing.py`

```python
from __future__ import annotations

import json
import sqlite3
import time
import uuid
from contextlib import contextmanager
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Generator

# Valid span kind values — OpenInference vocabulary
SPAN_KINDS = frozenset({"LLM", "TOOL", "CHAIN", "AGENT", "EMBEDDING"})


@dataclass
class Span:
    """A single unit of traced work.

    Fields
    ------
    id              Short hex ID (first 12 chars of a UUID4).
    trace_id        Groups related spans together.
    parent_id       Parent span ID, or None for root spans.
    name            Human-readable operation name.
    kind            OpenInference span kind: LLM | TOOL | CHAIN | AGENT | EMBEDDING.
    profile         TAG profile name that owns this span.
    model_id        LLM model identifier used during this span.
    started_at      ISO-8601 UTC timestamp when the span was opened.
    finished_at     ISO-8601 UTC timestamp when the span was closed (None if open).
    duration_ms     Wall-clock duration in milliseconds (None if open).
    status          'ok' | 'error' | 'timeout'.
    prompt_tokens   Tokens consumed as prompt/input.
    completion_tokens  Tokens consumed as completion/output.
    attributes      Arbitrary key-value metadata.
    error_msg       Human-readable error description (None when status == 'ok').
    """

    id: str = field(default_factory=lambda: uuid.uuid4().hex[:12])
    trace_id: str = ""
    parent_id: str | None = None
    name: str = ""
    kind: str = "LLM"
    profile: str | None = None
    model_id: str | None = None
    started_at: str = field(default_factory=_utc_now)
    finished_at: str | None = None
    duration_ms: int | None = None
    status: str = "ok"
    prompt_tokens: int = 0
    completion_tokens: int = 0
    attributes: dict[str, Any] = field(default_factory=dict)
    error_msg: str | None = None

    def __post_init__(self) -> None:
        if self.kind not in SPAN_KINDS:
            raise ValueError(
                f"Invalid span kind {self.kind!r}. Must be one of: {sorted(SPAN_KINDS)}"
            )
```

### 9.2 Updated `open_span()` signature

```python
def open_span(
    trace_id: str,
    name: str,
    kind: str = "LLM",
    profile: str | None = None,
    model_id: str | None = None,
    parent_id: str | None = None,
) -> Span:
    """Create and return a new open Span.

    Parameters
    ----------
    trace_id:   Groups this span with other spans from the same logical run.
    name:       Human-readable label for the operation being traced.
    kind:       OpenInference span kind (LLM, TOOL, CHAIN, AGENT, EMBEDDING).
    profile:    Optional TAG profile name.
    model_id:   Optional LLM model identifier.
    parent_id:  Optional ID of a parent Span (enables nested/tree traces).
    """
    return Span(
        trace_id=trace_id,
        name=name,
        kind=kind,
        profile=profile,
        model_id=model_id,
        parent_id=parent_id,
    )
```

### 9.3 Schema migration — `open_db()` in `controller.py`

The schema migration must be non-destructive and idempotent. The pattern used across TAG is to check `PRAGMA table_info` before issuing `ALTER TABLE`.

```python
def _migrate_spans_add_kind(conn: sqlite3.Connection) -> None:
    """Add `kind` column to spans table if not present (PRD-048 migration)."""
    cols = {row[1] for row in conn.execute("PRAGMA table_info(spans)")}
    if "kind" not in cols:
        conn.execute(
            "ALTER TABLE spans ADD COLUMN kind TEXT NOT NULL DEFAULT 'LLM'"
        )
        conn.execute(
            "CREATE INDEX IF NOT EXISTS idx_spans_kind ON spans(kind, started_at)"
        )
        conn.commit()
```

This function is called from `open_db()` immediately after the main `CREATE TABLE IF NOT EXISTS spans` DDL block.

### 9.4 Updated `spans` table DDL

The canonical DDL (used for fresh databases) gains the `kind` column and the new index:

```sql
CREATE TABLE IF NOT EXISTS spans (
    id                TEXT PRIMARY KEY,
    trace_id          TEXT NOT NULL,
    parent_id         TEXT,
    name              TEXT NOT NULL,
    kind              TEXT NOT NULL DEFAULT 'LLM',
    profile           TEXT,
    model_id          TEXT,
    started_at        TEXT NOT NULL,
    finished_at       TEXT,
    duration_ms       INTEGER,
    status            TEXT NOT NULL DEFAULT 'ok',
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    attributes        TEXT NOT NULL DEFAULT '{}',
    error_msg         TEXT
);
CREATE INDEX IF NOT EXISTS idx_spans_trace ON spans(trace_id, started_at);
CREATE INDEX IF NOT EXISTS idx_spans_kind  ON spans(kind, started_at);
```

### 9.5 Updated `save_spans_to_db()` — includes `kind`

```python
_INSERT_SPAN = """
INSERT OR REPLACE INTO spans
  (id, trace_id, parent_id, name, kind, profile, model_id,
   started_at, finished_at, duration_ms, status,
   prompt_tokens, completion_tokens, attributes, error_msg)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
"""

def save_spans_to_db(db_path: Path, spans: list[Span]) -> None:
    # ... (preamble unchanged) ...
    conn.executemany(
        _INSERT_SPAN,
        [
            (
                s.id, s.trace_id, s.parent_id, s.name, s.kind,
                s.profile, s.model_id, s.started_at, s.finished_at,
                s.duration_ms, s.status, s.prompt_tokens,
                s.completion_tokens, json.dumps(s.attributes), s.error_msg,
            )
            for s in spans
        ],
    )
```

### 9.6 Tool span context manager in `tracing.py`

A lightweight context manager eliminates repetitive open/close boilerplate at every tool call site in `controller.py`:

```python
from contextlib import contextmanager
from typing import Generator

@contextmanager
def tool_span(
    trace_id: str,
    tool_name: str,
    tool_input: dict[str, Any],
    parent_id: str | None = None,
    db_path: Path | None = None,
    _redact_fn=None,   # injected by controller.py to avoid circular import
) -> Generator[Span, None, None]:
    """Context manager that opens a TOOL span, yields it, then closes and persists it.

    Usage in controller.py:
        with tool_span(trace_id, "bash", {"command": cmd}, parent_id=step_span.id,
                       db_path=db_path, _redact_fn=scan_for_secrets) as ts:
            result = _run_bash(cmd)
            ts.attributes["tool.output"] = _truncate(result, 4096)

    Exceptions inside the with-block set status="error" and store the
    exception message in ts.attributes["tool.error"] and ts.error_msg.
    """
    raw_input = _safe_json(tool_input)
    redacted_input = _redact_fn(raw_input) if _redact_fn else raw_input

    span = open_span(
        trace_id=trace_id,
        name=f"tool:{tool_name}",
        kind="TOOL",
        parent_id=parent_id,
    )
    span.attributes.update({
        "tool.name": tool_name,
        "tool.input": redacted_input,
        "tool.output": None,
        "tool.error": None,
    })
    try:
        yield span
        close_span(span, status="ok")
    except Exception as exc:
        span.attributes["tool.error"] = str(exc)
        close_span(span, status="error", error_msg=str(exc))
        raise
    finally:
        if db_path is not None:
            save_spans_to_db(db_path, [span])


def _safe_json(obj: Any) -> str:
    """Serialize obj to JSON, replacing non-serializable values with repr()."""
    try:
        return json.dumps(obj)
    except (TypeError, ValueError):
        def _default(v):
            return repr(v)
        return json.dumps(obj, default=_default)


def _truncate(text: str, max_chars: int = 4096) -> str:
    """Truncate text to max_chars, appending a truncation marker if needed."""
    if len(text) <= max_chars:
        return text
    return text[:max_chars] + " [truncated]"
```

### 9.7 Instrumentation in `controller.py`

The tool dispatch layer in `controller.py` wraps every tool call site. The primary tool dispatch paths in the current codebase are:

1. **Hermes-mediated tool calls** — tool calls returned by the Hermes model response are dispatched in the step-execution loop.
2. **Direct tool calls** — a small number of `cmd_*` functions call internal helpers directly (e.g. `bash`, `write_file` within swarm orchestration).

Pattern for Hermes-mediated dispatch (pseudocode reflecting existing controller structure):

```python
# In the step-execution loop, after receiving a tool_use block from Hermes:
from tag.tracing import tool_span, _truncate
from tag.security import scan_for_secrets  # PRD-034

for tool_use in response.tool_uses:
    tool_name = tool_use["name"]
    tool_input = tool_use.get("input", {})

    with tool_span(
        trace_id=current_trace_id,
        tool_name=tool_name,
        tool_input=tool_input,
        parent_id=current_step_span.id,
        db_path=db_path,
        _redact_fn=lambda s: scan_for_secrets(s, redact=True),
    ) as ts:
        result = _dispatch_tool(tool_name, tool_input)
        ts.attributes["tool.output"] = _truncate(str(result.output), 4096)
```

### 9.8 `cmd_trace show` — kind filtering in `controller.py`

The SQL query in `cmd_trace` for the `show` subcommand gains an optional `WHERE kind = ?` clause:

```python
if sub == "show":
    trace_id = getattr(args, "run_id", None) or args.trace_id
    kind_filter = getattr(args, "kind", None)
    kind_filter = kind_filter.upper() if kind_filter else None

    if kind_filter:
        rows = conn.execute(
            "SELECT id, trace_id, parent_id, name, kind, profile, model_id, "
            "started_at, finished_at, duration_ms, status, prompt_tokens, "
            "completion_tokens, attributes, error_msg "
            "FROM spans WHERE trace_id = ? AND kind = ? ORDER BY started_at",
            (trace_id, kind_filter),
        ).fetchall()
    else:
        rows = conn.execute(
            "SELECT id, trace_id, parent_id, name, kind, profile, model_id, "
            "started_at, finished_at, duration_ms, status, prompt_tokens, "
            "completion_tokens, attributes, error_msg "
            "FROM spans WHERE trace_id = ? ORDER BY started_at",
            (trace_id,),
        ).fetchall()

    if not rows:
        kind_msg = f" with kind={kind_filter}" if kind_filter else ""
        print(f"No spans found for trace {trace_id}{kind_msg}")
        return 1

    if getattr(args, "json", False):
        col = ["id","trace_id","parent_id","name","kind","profile","model_id",
               "started_at","finished_at","duration_ms","status",
               "prompt_tokens","completion_tokens","attributes","error_msg"]
        records = [dict(zip(col, r)) for r in rows]
        for rec in records:
            rec["attributes"] = json.loads(rec["attributes"] or "{}")
        print(json.dumps(records, indent=2))
        return 0

    # If kind filter is TOOL, use a table renderer instead of the flame-chart
    if kind_filter == "TOOL":
        _render_tool_spans_table(rows, trace_id)
        return 0

    # Default: flame-chart tree (unchanged path, now includes TOOL child nodes)
    from tag.tracing import Span, render_trace_terminal
    spans = [_row_to_span(r) for r in rows]
    print(render_trace_terminal(spans))
    return 0
```

### 9.9 `tag stats --by tool` SQL query

```sql
-- Per-tool aggregates using JSON_EXTRACT (SQLite 3.38+)
-- Percentile approximation: collect all durations per tool, sort, index at 50th/95th %
SELECT
    JSON_EXTRACT(attributes, '$.tool.name')            AS tool_name,
    COUNT(*)                                           AS call_count,
    SUM(CASE WHEN status != 'ok' THEN 1 ELSE 0 END)   AS error_count,
    MIN(duration_ms)                                   AS min_ms,
    AVG(duration_ms)                                   AS avg_ms,
    MAX(duration_ms)                                   AS max_ms,
    SUM(duration_ms)                                   AS total_ms
FROM spans
WHERE kind = 'TOOL'
  AND started_at >= ?           -- cutoff timestamp
  AND (:profile IS NULL OR profile = :profile)
GROUP BY tool_name
ORDER BY call_count DESC;
```

Because SQLite lacks native `PERCENTILE_CONT`, p50/p95 are computed in Python from the per-tool `duration_ms` values fetched in a secondary query:

```python
def _compute_percentile(values: list[int], pct: float) -> int:
    """Return the pct-th percentile of values (0.0–1.0 scale)."""
    if not values:
        return 0
    sorted_vals = sorted(values)
    k = (len(sorted_vals) - 1) * pct
    lo, hi = int(k), min(int(k) + 1, len(sorted_vals) - 1)
    return int(sorted_vals[lo] + (sorted_vals[hi] - sorted_vals[lo]) * (k - lo))
```

To avoid fetching all rows for large datasets, a second query using `NTILE(20)` (window function, SQLite 3.25+) is used as a percentile estimator when row count exceeds 1000:

```sql
-- Percentile estimator via NTILE bucketing
WITH ranked AS (
    SELECT
        JSON_EXTRACT(attributes, '$.tool.name') AS tool_name,
        duration_ms,
        NTILE(20) OVER (
            PARTITION BY JSON_EXTRACT(attributes, '$.tool.name')
            ORDER BY duration_ms
        ) AS bucket
    FROM spans
    WHERE kind = 'TOOL' AND started_at >= ?
)
SELECT
    tool_name,
    MAX(CASE WHEN bucket = 10 THEN duration_ms END) AS p50_ms,
    MAX(CASE WHEN bucket = 19 THEN duration_ms END) AS p95_ms
FROM ranked
GROUP BY tool_name;
```

### 9.10 `render_trace_terminal()` — kind badges

The existing `_label()` helper in `tracing.py` gains a kind badge prefix:

```python
_KIND_BADGE = {
    "LLM": "[L]",
    "TOOL": "[T]",
    "CHAIN": "[C]",
    "AGENT": "[A]",
    "EMBEDDING": "[E]",
}

def _label(s: Span, show_kind: bool = False) -> str:
    dur = s.duration_ms or 0
    bar = _bar(dur)
    tokens = f"{s.prompt_tokens}↑{s.completion_tokens}↓"
    badge = f"{_KIND_BADGE.get(s.kind, '[?]')} " if show_kind else ""
    return f"{badge}{s.name}  {bar}  {dur}ms  {tokens}"
```

`show_kind` is set to `True` when the span list contains more than one distinct `kind` value.

### 9.11 OTel export extension in `otel_semconv.py`

`spans_to_otlp_json` (PRD-041) gains one additional attribute per span:

```python
# In map_span_attributes(), after existing gen_ai.* mappings:
span_kind = span.get("kind", "LLM")
attrs["openinference.span.kind"] = span_kind
```

This places the OpenInference span kind attribute on every exported span so that Phoenix and other OpenInference-aware backends can route spans to type-specific dashboards.

### 9.12 `tag otel-export status` implementation

New subcommand in `cmd_otel_export`:

```python
if sub == "status":
    # Count spans by kind
    kind_counts = dict(
        db.execute(
            "SELECT kind, COUNT(*) FROM spans GROUP BY kind"
        ).fetchall()
    )
    total = sum(kind_counts.values())

    # TCP reachability check
    endpoint = cfg.get("otel", {}).get("endpoint", "")
    reachable = False
    if endpoint:
        import socket, urllib.parse
        parsed = urllib.parse.urlparse(endpoint)
        host = parsed.hostname or ""
        port = parsed.port or (443 if parsed.scheme == "https" else 4317)
        try:
            with socket.create_connection((host, port), timeout=2):
                reachable = True
        except OSError:
            reachable = False

    result = {
        "endpoint": endpoint or None,
        "reachable": reachable,
        "spans_total": total,
        "spans_by_kind": {
            "LLM": kind_counts.get("LLM", 0),
            "TOOL": kind_counts.get("TOOL", 0),
            "CHAIN": kind_counts.get("CHAIN", 0),
            "AGENT": kind_counts.get("AGENT", 0),
            "EMBEDDING": kind_counts.get("EMBEDDING", 0),
            "unknown": sum(v for k, v in kind_counts.items()
                           if k not in {"LLM","TOOL","CHAIN","AGENT","EMBEDDING"}),
        },
        "semconv_version": SEMCONV_VERSION,
    }
    print(json.dumps(result, indent=2))
    return 0
```

### 9.13 Integration with `Tracer` context-manager (backward-compat class)

The existing `Tracer` class in `tracing.py` (used as a context-manager-backed tracer by some `controller.py` paths) gains a `tool_span()` method that is a thin wrapper around the module-level `tool_span()` context manager:

```python
class Tracer:
    # ... existing __init__, __enter__, __exit__ ...

    def tool_span(
        self,
        tool_name: str,
        tool_input: dict[str, Any],
        parent_id: str | None = None,
    ):
        """Return a tool_span() context manager scoped to this Tracer's trace_id."""
        return tool_span(
            trace_id=self.trace_id,
            tool_name=tool_name,
            tool_input=tool_input,
            parent_id=parent_id,
            db_path=self.db_path,
        )
```

---

## 10. Security Considerations

1. **Secret leakage via `tool.input`.** Tool arguments may contain file paths, environment variable values, API keys, or user-supplied data that includes secrets. The `tool.input` attribute MUST be passed through `scan_for_secrets()` from `security.py` (PRD-034) before storage. If a secret pattern is detected, the entire value is replaced with `"<redacted>"`, not partially masked, to prevent partial reconstruction.

2. **`tool.output` may contain PII.** The output of tools like `read_file` or `bash` can contain arbitrary file contents. Output is truncated to 4096 characters (NFR-05) which limits the exposure window, but no PII scanning is applied to output. Operators who require PII scrubbing of outputs should configure database encryption at rest separately. This is consistent with TAG's existing span attribute policy.

3. **No secrets in OTel export.** Since TOOL spans stored in SQLite have already had `tool.input` redacted, the OTel export path inherits the clean values. No additional redaction is required at export time. However, operators should review their OTLP endpoint's TLS configuration, as the JSON payload includes `tool.output` which may contain sensitive content.

4. **`tag otel-export status` TCP probe.** The TCP reachability check performs a socket connection to the configured endpoint. This connection attempt appears in network logs and could reveal the configured endpoint address if logs are accessible to third parties. Users in zero-egress environments should be aware that `tag otel-export status` makes an outbound connection.

5. **SQL injection via `--kind` flag.** The `kind` value from `--kind` is normalized to uppercase and validated against the `SPAN_KINDS` frozenset before use in the SQL query. Invalid values are rejected with a user-facing error before any database interaction.

6. **`JSON_EXTRACT` in `tag stats --by tool`.** SQLite's `JSON_EXTRACT` applied to the `attributes` column extracts `tool.name`. There is no injection risk here because the function is parameterized at the value level (the JSON path `$.tool.name` is a string literal in the query, not user-supplied), but the `--profile` parameter is bound via prepared statement placeholder to prevent injection.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`tests/test_prd_048_tool_spans.py`)

| Test | Description |
|------|-------------|
| `test_span_kind_default` | `Span()` with no `kind` argument has `kind == "LLM"`. |
| `test_span_kind_tool` | `Span(kind="TOOL")` creates successfully; `span.kind == "TOOL"`. |
| `test_span_kind_invalid` | `Span(kind="INVALID")` raises `ValueError`. |
| `test_open_span_kind_param` | `open_span(..., kind="TOOL")` returns span with `kind == "TOOL"`. |
| `test_save_load_spans_with_kind` | Round-trip: save spans with mixed kinds; reload from DB; assert `kind` values preserved. |
| `test_migration_idempotent` | Call `_migrate_spans_add_kind()` twice on same connection; no error raised; table has exactly one `kind` column. |
| `test_migration_preserves_rows` | Insert rows without `kind` column present; run migration; assert all rows still present with `kind == "LLM"`. |
| `test_tool_span_context_ok` | `tool_span()` context manager sets `kind="TOOL"`, `tool.name`, `tool.input`; on normal exit sets `status="ok"`. |
| `test_tool_span_context_error` | Exception inside `tool_span()` context sets `status="error"`, `tool.error` to exception message; exception is re-raised. |
| `test_tool_input_redaction` | `tool_span()` with `_redact_fn` that detects `"SECRET"` stores `"<redacted>"` in `tool.input`. |
| `test_tool_output_truncation` | `_truncate("x" * 5000, 4096)` returns 4096 + len(" [truncated]") chars. |
| `test_safe_json_non_serializable` | `_safe_json({"k": object()})` returns valid JSON without raising. |
| `test_render_trace_terminal_kind_badges` | When spans contain both LLM and TOOL kinds, rendered output includes `[L]` and `[T]` badge prefixes. |
| `test_otel_export_includes_kind` | `map_span_attributes({"kind": "TOOL", ...})` result contains `openinference.span.kind == "TOOL"`. |

### 11.2 Integration Tests (`tests/test_prd_048_integration.py`)

| Test | Description |
|------|-------------|
| `test_trace_show_kind_filter_sql` | Insert 10 LLM spans and 5 TOOL spans; call `cmd_trace` with `--kind tool`; assert exactly 5 rows returned; assert SQL uses index via `EXPLAIN QUERY PLAN` containing `idx_spans_kind`. |
| `test_trace_show_no_filter_includes_all` | Insert mixed spans; `tag trace show <id>` returns all 15 spans. |
| `test_trace_show_run_id_alias` | Insert spans with `trace_id = run_abc123`; `tag trace show --run-id run_abc123` returns same result as `tag trace show run_abc123`. |
| `test_stats_by_tool` | Seed 50 TOOL spans across 5 tool names with known durations; assert `tag stats --by tool` returns correct call counts and exact p50/p95 values. |
| `test_stats_by_tool_since_filter` | Insert TOOL spans split across two time windows; `--since 1d` returns only the recent subset. |
| `test_stats_by_tool_json_schema` | `tag stats --by tool --json` output parses as valid JSON and matches expected schema (`tool_name`, `call_count`, `error_count`, `error_rate`, `p50_ms`, `p95_ms`, `total_ms`). |
| `test_otel_export_status_json` | Mock database with 3 LLM and 7 TOOL spans; `tag otel-export status --json` returns correct `spans_by_kind` counts. |

### 11.3 Performance Tests

| Test | Description |
|------|-------------|
| `bench_tool_span_overhead` | `timeit` 1000 iterations of `open_span(..., kind="TOOL") + close_span()`; assert mean < 0.5 ms. |
| `bench_stats_50k_spans` | Seed DB with 50,000 TOOL spans across 10 tool names; assert `tag stats --by tool --since 30d` returns in < 200 ms. |
| `bench_trace_show_kind_filter` | DB with 10,000 spans (8,000 LLM, 2,000 TOOL); `--kind tool` returns in < 50 ms. |

---

## 12. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|--------------|
| AC-01 | `Span` dataclass has `kind: str = "LLM"` field; `Span(kind="INVALID")` raises `ValueError`. | Unit test `test_span_kind_invalid` |
| AC-02 | Running `open_db()` on a pre-PRD-048 database (no `kind` column) adds the column with `DEFAULT 'LLM'` and does not drop or alter any existing rows. | Integration test `test_migration_preserves_rows` |
| AC-03 | `open_db()` migration is idempotent: calling it twice on the same database does not raise an error. | Unit test `test_migration_idempotent` |
| AC-04 | `CREATE INDEX IF NOT EXISTS idx_spans_kind ON spans(kind, started_at)` is present in the DB after first `open_db()` call. | Assert via `PRAGMA index_list(spans)` |
| AC-05 | Running a TAG agent task that triggers at least 3 tools produces exactly N child TOOL spans in the `spans` table (where N = tool calls made), each with `parent_id` pointing to the step span and `kind = "TOOL"`. | Integration test `test_trace_show_kind_filter_sql` |
| AC-06 | Each TOOL span's `attributes` JSON contains `tool.name`, `tool.input`, `tool.output`, and `tool.error` keys. | Integration test: assert `json.loads(attrs).keys() >= {"tool.name", "tool.input", "tool.output", "tool.error"}` |
| AC-07 | `tool.input` containing a known secret pattern (e.g. `AKIAIOSFODNN7EXAMPLE`) is stored as `"<redacted>"` in `attributes`. | Unit test `test_tool_input_redaction` |
| AC-08 | `tag trace show <id> --kind tool` returns only TOOL spans in both human-readable and `--json` modes; no LLM spans appear in the output. | Integration test `test_trace_show_kind_filter_sql` |
| AC-09 | `tag trace show <id>` without `--kind` renders TOOL spans as nested children of their parent LLM step spans in the flame-chart. | Visual assertion in integration test: rendered string contains `tool:` prefixed names indented under their parent |
| AC-10 | `tag trace show --run-id <id>` produces the same output as `tag trace show <id>`. | Integration test `test_trace_show_run_id_alias` |
| AC-11 | `tag stats --by tool --since 7d` prints a table with columns: `tool`, `calls`, `errors`, `err%`, `p50ms`, `p95ms`, `total_ms`. | Integration test: parse output and assert column presence |
| AC-12 | `tag stats --by tool --since 7d --json` output is valid JSON and contains a `tools` array where each element has `tool_name`, `call_count`, `error_count`, `error_rate`, `p50_ms`, `p95_ms`, `total_ms`. | Integration test `test_stats_by_tool_json_schema` |
| AC-13 | `tag otel-export status --json` returns a JSON object with keys `endpoint`, `reachable`, `spans_total`, `spans_by_kind`, `semconv_version`. | Integration test `test_otel_export_status_json` |
| AC-14 | OTLP export JSON for a TOOL span includes attribute `openinference.span.kind = "TOOL"`. | Unit test `test_otel_export_includes_kind` |
| AC-15 | `tool.output` values longer than 4096 characters are stored as the first 4096 chars followed by ` [truncated]`. | Unit test `test_tool_output_truncation` |
| AC-16 | `tag stats --by tool --since 7d` returns in under 200 ms on a DB with 50,000 TOOL spans. | Performance test `bench_stats_50k_spans` |
| AC-17 | No new entries appear in `pip install tag` transitive dependency tree as a result of this PRD. | `pip show tag` + `pipdeptree` before/after assertion in CI |

---

## 13. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| PRD-013 — Agent Tracing & Observability | Hard predecessor | Provides the `Span` dataclass, `tracing.py`, `spans` table, `open_span`/`close_span`/`save_spans_to_db` API, and `render_trace_terminal`. PRD-048 extends all of these. |
| PRD-034 — Secret Scanning | Hard dependency | `scan_for_secrets()` (or equivalent) from `security.py` is called on every `tool.input` before storage. If PRD-034 is not implemented, `tool.input` is stored without redaction (acceptable fallback: log a warning, skip redaction). |
| PRD-041 — OTel GenAI Span Cost Attribution | Soft dependency | `spans_to_otlp_json()` in `otel_semconv.py` is extended to include `openinference.span.kind`. If PRD-041 is not deployed, the OTel extension does not apply but all other PRD-048 features are unaffected. |
| PRD-028 — Sandbox Code Execution | Soft dependency | Tool dispatches that run within the sandbox (PRD-028) should still emit TOOL spans; the sandbox entry/exit may itself warrant a `CHAIN`-kind span wrapping the inner TOOL spans. Not required for initial implementation. |
| PRD-027 — Eval Framework | Consumer | `tag eval` will consume TOOL spans from `tag trace show --kind tool --json` to construct tool call sequences for `ToolCorrectnessMetric` evaluation. PRD-027 is not a blocker for PRD-048 but benefits from it. |
| PRD-044 — AgentOps Session Observability | Consumer | AgentOps bridge emits tool call events; aligning AgentOps tool event names with TAG's `tool.name` attribute enables cross-reference between AgentOps sessions and TAG TOOL spans. Not a blocker. |
| SQLite >= 3.25 | Runtime requirement | Window functions (`NTILE`) used in percentile query require SQLite 3.25+. SQLite 3.38+ required for `JSON_EXTRACT` in the `GROUP BY` clause. macOS ships SQLite 3.43+ as of 2025; Linux CI runners typically have 3.40+. |

---

## 14. Open Questions

| ID | Question | Owner | Target Resolution |
|----|----------|-------|-------------------|
| OQ-1 | Should `tool.input` truncation be applied before or after secret redaction? Truncating first could leave a secret fragment; redacting first on a very large input could be slow. | Security / engineering | Before Phase 1 kickoff |
| OQ-2 | For tools dispatched within the sandbox (PRD-028), should the sandbox entry itself be a `CHAIN`-kind span wrapping the inner TOOL spans, or should TOOL spans be direct children of the step span? | Architecture review | Phase 1 design review |
| OQ-3 | Should `tag trace show --kind tool` render a table (as shown in §6.1) or use the same flame-chart tree as unfiltered output? The table format is more readable for pure-tool views; the tree format loses context about which step each tool was called in. | UX / user feedback | Phase 2 |
| OQ-4 | MCP-registered tools dispatched via the Hermes bridge (`hermes_bridge.py`) — are they dispatched through the same tool dispatch code path in `controller.py`, or through a separate path that requires additional instrumentation? | @owner of hermes_bridge.py | Phase 1 kickoff |
| OQ-5 | The `--run-id` alias maps to `trace_id` in the `spans` table. But TAG's `runs` table uses a short hex `run_id` which may differ from the `trace_id` used in `spans`. What is the exact join/lookup logic to resolve `--run-id` to a `trace_id`? | Engineering | Phase 1 |
| OQ-6 | Should `tag stats --by tool` be a top-level `tag stats` flag or a dedicated `tag tool stats` subcommand? The current PRD adds it as a flag on `tag stats` to avoid proliferating top-level subcommands, but `tag tool stats` is more discoverable. | Product / CLI design | Before Phase 1 kickoff |
| OQ-7 | For the `tag otel-export status` TCP probe, should a failed probe (unreachable endpoint) return exit code 0 (with `"reachable": false` in JSON) or exit code 1? The current design returns 0 to avoid false CI failures when OTLP is intentionally not deployed. | Engineering | Phase 1 |
| OQ-8 | Should `tool.output` be stored only on successful (status=ok) tool spans, or also on error spans where partial output was produced? Some tools (bash) produce stdout before exiting with a non-zero code. | Engineering | Phase 1 design review |

---

## 15. Complexity and Timeline

**Total estimate: 3-5 days (S)**

### Phase 1 — Core `Span.kind` + schema migration (Day 1)

- Add `kind` field to `Span` dataclass with validation (`__post_init__`).
- Update `open_span()` to accept `kind` parameter.
- Update `save_spans_to_db()` `INSERT OR REPLACE` statement to include `kind`.
- Implement `_migrate_spans_add_kind()` and wire into `open_db()`.
- Update canonical `CREATE TABLE` DDL to include `kind` column and `idx_spans_kind`.
- Update `save_spans_to_db` and `Tracer` class serialization/deserialization.
- Write unit tests: `test_span_kind_*`, `test_migration_*`, `test_save_load_spans_with_kind`.
- **Deliverable:** `kind` column exists in DB; all existing spans have `kind = "LLM"`.

### Phase 2 — `tool_span()` context manager + controller instrumentation (Days 2-3)

- Implement `tool_span()` context manager in `tracing.py` including `_safe_json`, `_truncate`.
- Implement `Tracer.tool_span()` method.
- Instrument all tool dispatch sites in `controller.py` (Hermes tool_use response dispatch, direct tool calls in swarm/submit paths).
- Wire `scan_for_secrets` redaction into `tool_span()` via `_redact_fn` parameter.
- Write unit tests: `test_tool_span_context_*`, `test_tool_input_redaction`, `test_tool_output_truncation`.
- Write integration test: `test_trace_show_kind_filter_sql` (TOOL spans appear in DB).
- **Deliverable:** Every tool dispatch produces a TOOL child span in `spans` table.

### Phase 3 — CLI surface: `tag trace show --kind`, `--run-id`, kind badges (Day 3-4)

- Add `--kind` flag to `tag trace show` argparser entry; implement SQL-level filter in `cmd_trace`.
- Add `--run-id` alias to `tag trace show` argparser; implement run-id resolution logic.
- Update `render_trace_terminal()` to display kind badges when mixed kinds are present.
- Implement `_render_tool_spans_table()` for the `--kind tool` table view.
- Update `cmd_trace` show deserialization to read `kind` column (column index shift from adding `kind`).
- Write integration tests: `test_trace_show_kind_filter_sql`, `test_trace_show_run_id_alias`, `test_trace_show_no_filter_includes_all`.
- **Deliverable:** `tag trace show --kind tool` and `tag trace show --run-id` work end-to-end.

### Phase 4 — `tag stats --by tool` + OTel + status (Day 4-5)

- Implement `tag stats --by tool --since <duration>` subcommand with SQL aggregation query and Python percentile computation.
- Extend `map_span_attributes()` in `otel_semconv.py` to add `openinference.span.kind`.
- Implement `tag otel-export status --json` subcommand with TCP probe.
- Write integration tests: `test_stats_by_tool`, `test_stats_by_tool_json_schema`, `test_otel_export_status_json`, `test_otel_export_includes_kind`.
- Write performance tests: `bench_stats_50k_spans`, `bench_trace_show_kind_filter`, `bench_tool_span_overhead`.
- **Deliverable:** All AC criteria pass; performance benchmarks green.

---

*GitHub Issue: #343 — Structured Tool-Call Child Spans with TOOL Kind*
