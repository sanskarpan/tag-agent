# PRD-013: Distributed Agent Tracing & Observability

**Status:** Proposed  
**Priority:** P1  
**Estimated Effort:** L (3–4 weeks)  
**Affects:** `controller.py` (`run_chat_step`, `cmd_submit`, `insert_step`), new `tag/tracing.py`, `tag.sqlite3`

---

## 1. Overview

Production AI agent systems require distributed tracing to debug failures, identify bottlenecks, and audit decision trails. TAG currently has no tracing: `tag runs` shows coarse-grained run records but nothing about tool calls, model reasoning steps, or inter-profile communication. This PRD adds OpenTelemetry-compatible span-level tracing to every agent step, persisted locally and optionally exported to Jaeger, Datadog, or any OTLP endpoint. On-device, `tag trace <run_id>` shows a flame-graph-style trace in the terminal.

---

## 2. Problem Statement

- When a `tag swarm` run fails after 30 minutes, there is no trace of what each profile did.
- Tool call failures are invisible — Hermes logs them internally but TAG surfaces nothing.
- Users cannot measure which step in the pipeline is the slowest.
- Enterprise teams need an audit trail of what the AI agents did and why.
- Debugging is entirely dependent on Hermes' raw log output, which is opaque and unstructured.

---

## 3. Goals

1. Every `run_chat_step()` generates a trace span with: profile, model, prompt tokens, completion tokens, latency, tool calls made, and exit status.
2. Spans are nested correctly: `run` → `step` → `tool_call`.
3. `tag trace <run_id>` displays a terminal flame-chart of the run.
4. `tag trace <run_id> --export otlp --endpoint http://localhost:4317` sends to any OTLP collector.
5. Trace data is stored locally in `tag.sqlite3` `spans` table.
6. Zero overhead when tracing is not configured — disabled by default.

---

## 4. Non-Goals

- Real-time streaming traces (batch-upload after run completion for v1).
- Custom instrumentation SDK for user code.
- AI-powered trace analysis (future).

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag trace abc123` | I see a timeline of every step, tool call, and model decision |
| U2 | DevOps | configure `--export otlp` | traces land in my Datadog dashboard |
| U3 | Developer | see "step 3 of 7 failed: tool_call code_search returned error" | I know exactly what went wrong |
| U4 | Manager | run `tag trace abc123 --export json` | I share the trace with the AI provider for support |
| U5 | Developer | disable tracing in CI | no overhead, no storage, no side effects |

---

## 6. Technical Design

### 6.1 Schema: `spans` table

```sql
CREATE TABLE IF NOT EXISTS spans (
    id          TEXT PRIMARY KEY,
    trace_id    TEXT NOT NULL,    -- FK to runs.id
    parent_id   TEXT,             -- parent span id (for nested tool calls)
    name        TEXT NOT NULL,    -- e.g. "chat_step", "tool_call:code_search"
    profile     TEXT,
    model_id    TEXT,
    started_at  TEXT NOT NULL,
    finished_at TEXT,
    duration_ms INTEGER,
    status      TEXT NOT NULL DEFAULT 'ok',  -- ok | error | timeout
    prompt_tokens INTEGER DEFAULT 0,
    completion_tokens INTEGER DEFAULT 0,
    attributes  TEXT,            -- JSON blob for extra key/value data
    error_msg   TEXT
);
CREATE INDEX IF NOT EXISTS idx_spans_trace ON spans(trace_id, started_at);
```

### 6.2 New module: `src/tag/tracing.py`

```python
"""Lightweight span-based tracing for TAG agent runs."""
from __future__ import annotations
import json, sqlite3, time, uuid
from contextlib import contextmanager
from dataclasses import dataclass, field, asdict
from typing import Any, Generator

@dataclass
class Span:
    id: str = field(default_factory=lambda: uuid.uuid4().hex[:16])
    trace_id: str = ""
    parent_id: str | None = None
    name: str = ""
    profile: str = ""
    model_id: str = ""
    started_at: float = field(default_factory=time.monotonic)
    finished_at: float | None = None
    status: str = "ok"
    prompt_tokens: int = 0
    completion_tokens: int = 0
    attributes: dict[str, Any] = field(default_factory=dict)
    error_msg: str | None = None
    
    @property
    def duration_ms(self) -> int | None:
        if self.finished_at is None:
            return None
        return int((self.finished_at - self.started_at) * 1000)
    
    def finish(self, *, status: str = "ok", error_msg: str | None = None) -> None:
        self.finished_at = time.monotonic()
        self.status = status
        self.error_msg = error_msg


class Tracer:
    """Thread-safe tracer that records spans to SQLite."""
    
    def __init__(self, db: sqlite3.Connection, trace_id: str, enabled: bool = True):
        self._db = db
        self._trace_id = trace_id
        self._enabled = enabled
        self._stack: list[str] = []   # span id stack for parent tracking
    
    @contextmanager
    def span(self, name: str, **attrs) -> Generator[Span, None, None]:
        if not self._enabled:
            yield Span()
            return
        
        parent_id = self._stack[-1] if self._stack else None
        s = Span(trace_id=self._trace_id, parent_id=parent_id, name=name)
        s.attributes.update(attrs)
        self._stack.append(s.id)
        try:
            yield s
            s.finish(status="ok")
        except Exception as exc:
            s.finish(status="error", error_msg=str(exc))
            raise
        finally:
            self._stack.pop()
            self._save(s)
    
    def _save(self, span: Span) -> None:
        from datetime import datetime, timezone
        import time as _time
        epoch = datetime(1970, 1, 1, tzinfo=timezone.utc)
        started_wall = (
            datetime.fromtimestamp(span.started_at - _time.monotonic() + _time.time(), tz=timezone.utc)
            .isoformat()
        )
        self._db.execute("""
            INSERT OR REPLACE INTO spans
            (id, trace_id, parent_id, name, profile, model_id, started_at, finished_at,
             duration_ms, status, prompt_tokens, completion_tokens, attributes, error_msg)
            VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
        """, (
            span.id, span.trace_id, span.parent_id, span.name,
            span.profile, span.model_id, started_wall,
            None,  # finished_at not needed in simplified form
            span.duration_ms, span.status,
            span.prompt_tokens, span.completion_tokens,
            json.dumps(span.attributes), span.error_msg,
        ))
        self._db.commit()
```

### 6.3 Integration with `run_chat_step`

```python
def run_chat_step(
    cfg: dict, profile_name: str, prompt: str,
    tracer: "Tracer | None" = None, **kwargs
) -> dict[str, Any]:
    with (tracer.span("chat_step", profile=profile_name) if tracer else nullcontext()) as span:
        result = _do_chat_step(cfg, profile_name, prompt, **kwargs)
        if span:
            span.prompt_tokens = result.get("prompt_tokens", 0)
            span.completion_tokens = result.get("completion_tokens", 0)
        return result
```

### 6.4 `tag trace` command

```python
def cmd_trace(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    db = open_db(cfg)
    run_id = args.run_id
    
    spans = db.execute(
        "SELECT * FROM spans WHERE trace_id = ? ORDER BY started_at",
        (run_id,)
    ).fetchall()
    
    if not spans:
        print(f"No trace data for run {run_id}", file=sys.stderr)
        return 1
    
    if getattr(args, "export_format", None) == "json":
        cols = [d[0] for d in db.execute("SELECT * FROM spans LIMIT 0").description]
        print(json.dumps([dict(zip(cols, row)) for row in spans], indent=2))
        return 0
    
    # Terminal flame chart
    _render_trace_terminal(spans)
    return 0


def _render_trace_terminal(spans: list) -> None:
    """Render a simple ASCII flame chart."""
    # Group by parent_id, build tree, render with indentation
    from tag.tui_output import get_console
    console = get_console()
    
    total_ms = max((s[-6] or 0) for s in spans)  # duration_ms column
    
    for span in spans:
        depth = 0  # TODO: calculate from parent chain
        indent = "  " * depth
        name = span[3]  # name column
        dur = span[8] or 0  # duration_ms
        status = span[9]  # status
        bar_len = int((dur / max(total_ms, 1)) * 30)
        bar = "█" * bar_len
        
        if console:
            color = "green" if status == "ok" else "red"
            console.print(f"{indent}[{color}]▸[/{color}] {name} [{color}]{bar}[/{color}] {dur}ms")
        else:
            print(f"{indent}▸ {name} {bar} {dur}ms")
```

### 6.5 OTLP export

```python
def export_trace_otlp(db: sqlite3.Connection, run_id: str, endpoint: str) -> None:
    """Export trace to any OTLP-compatible endpoint (Jaeger, Datadog, etc.)."""
    # Build minimal OTLP JSON payload
    # POST to endpoint/v1/traces
    ...
```

---

## 7. Implementation Plan

| Step | Task |
|------|------|
| 1 | Add `spans` table to `open_db()` |
| 2 | Create `src/tag/tracing.py` with `Span` and `Tracer` classes |
| 3 | Integrate `Tracer` into `run_chat_step` |
| 4 | Pass tracer through `cmd_submit` → `run_chat_step` calls |
| 5 | Implement `cmd_trace` with terminal renderer |
| 6 | Add OTLP export stub |
| 7 | Register `trace` parser |
| 8 | Tests: `test_tracer_records_span`, `test_tracer_nests_spans`, `test_cmd_trace_json_output` |

---

## 8. Success Metrics

- `tag trace <run_id>` shows spans for all steps of that run.
- Failed tool calls show `status: error` in trace.
- `tag trace --export json` produces valid OTLP-compatible JSON.
- Tracing disabled by default adds < 1ms overhead per run.

---

## 9. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Hermes doesn't expose tool call events | Parse from Hermes stdout; accept partial traces |
| Span table grows large over time | Auto-prune spans older than 30 days in `open_db()` |
| OTLP endpoint authentication varies | Support `--otlp-header X-API-Key=...` for auth headers |
