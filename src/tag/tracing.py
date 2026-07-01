"""Lightweight span-based tracing for TAG agent runs (PRD-013, PRD-046, PRD-048).

Public API
----------
open_span(trace_id, name, profile=None, model_id=None, parent_id=None) -> Span
open_tool_span(trace_id, tool_name, parent_id=None, **attrs) -> Span
close_span(span, status='ok', prompt_tokens=0, completion_tokens=0, error_msg=None, cost_usd=None)
save_spans_to_db(db_path: Path, spans: list[Span]) -> None
render_trace_terminal(spans: list[Span]) -> str
migrate_spans_table(conn: sqlite3.Connection) -> None

No global state: callers own the list of Span objects.

Backward-compatible helpers (kept for controller.py integration):
  Tracer  -- context-manager-based tracer backed by an open sqlite3.Connection
  export_spans_otlp(rows, endpoint, headers) -> bool

PRD-046: kind field + cost_usd field + open_tool_span + TraceProcessor protocol + ProcessorChain
PRD-048: SPAN_KINDS export + migrate_spans_table
"""
from __future__ import annotations

import json
import sqlite3
import uuid
from contextlib import contextmanager
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Generator, Protocol


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _utc_now() -> str:
    """Return the current UTC time as an ISO-8601 string."""
    return datetime.now(timezone.utc).isoformat()


# ---------------------------------------------------------------------------
# Constants (PRD-048)
# ---------------------------------------------------------------------------

SPAN_KINDS: list[str] = ["llm", "tool", "agent", "chain", "embedding", "retrieval"]


# ---------------------------------------------------------------------------
# Span dataclass
# ---------------------------------------------------------------------------

@dataclass
class Span:
    """A single unit of traced work.

    Fields
    ------
    id              Short hex ID (first 12 chars of a UUID4).
    trace_id        Groups related spans together.
    parent_id       Parent span ID, or None for root spans.
    name            Human-readable operation name.
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
    kind            Span kind: one of SPAN_KINDS ('llm', 'tool', 'agent',
                    'chain', 'embedding', 'retrieval').  (PRD-046)
    cost_usd        Estimated cost in US dollars for this span, or None.  (PRD-046)
    """

    id: str = field(default_factory=lambda: uuid.uuid4().hex[:12])
    trace_id: str = ""
    parent_id: str | None = None
    name: str = ""
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
    kind: str = "llm"
    cost_usd: float | None = None


# ---------------------------------------------------------------------------
# Functional API (no global state)
# ---------------------------------------------------------------------------

def open_span(
    trace_id: str,
    name: str,
    profile: str | None = None,
    model_id: str | None = None,
    parent_id: str | None = None,
    kind: str = "llm",
) -> Span:
    """Create and return a new open Span.

    The caller is responsible for storing the returned Span in their own list
    and later calling :func:`close_span` on it.

    Parameters
    ----------
    trace_id:   Groups this span with other spans from the same logical run.
    name:       Human-readable label for the operation being traced.
    profile:    Optional TAG profile name.
    model_id:   Optional LLM model identifier.
    parent_id:  Optional ID of a parent Span (enables nested/tree traces).
    kind:       Span kind, one of SPAN_KINDS.  Defaults to 'llm'.
    """
    return Span(
        trace_id=trace_id,
        name=name,
        profile=profile,
        model_id=model_id,
        parent_id=parent_id,
        kind=kind,
    )


def open_tool_span(
    trace_id: str,
    tool_name: str,
    parent_id: str | None = None,
    **attrs: Any,
) -> Span:
    """Create and return a new open Span with kind='tool'.  (PRD-046)

    Convenience wrapper around :func:`open_span` for tool invocations.
    Any extra keyword arguments are stored as span attributes.

    Parameters
    ----------
    trace_id:   Groups this span with other spans from the same logical run.
    tool_name:  Human-readable name of the tool being invoked.
    parent_id:  Optional ID of a parent Span.
    **attrs:    Arbitrary key-value attributes stored on the span.
    """
    span = Span(
        trace_id=trace_id,
        name=tool_name,
        parent_id=parent_id,
        kind="tool",
    )
    if attrs:
        span.attributes.update(attrs)
    return span


def close_span(
    span: Span,
    status: str = "ok",
    prompt_tokens: int = 0,
    completion_tokens: int = 0,
    error_msg: str | None = None,
    cost_usd: float | None = None,
) -> None:
    """Close an open Span, recording timing and outcome.

    Idempotent: calling this on an already-closed span is a no-op.

    Parameters
    ----------
    span:               The Span returned by :func:`open_span`.
    status:             'ok', 'error', or 'timeout'.
    prompt_tokens:      Tokens consumed as prompt/input.
    completion_tokens:  Tokens consumed as completion/output.
    error_msg:          Human-readable error message (if any).
    cost_usd:           Estimated cost in US dollars for this span.  (PRD-046)
    """
    if span.finished_at is not None:
        # Already closed; avoid double-closing.
        return

    finished_iso = _utc_now()
    span.finished_at = finished_iso
    span.status = status
    span.prompt_tokens = prompt_tokens
    span.completion_tokens = completion_tokens
    span.error_msg = error_msg
    span.cost_usd = cost_usd

    # Compute duration_ms from ISO strings.
    try:
        t_start = datetime.fromisoformat(span.started_at)
        t_end = datetime.fromisoformat(finished_iso)
        delta = t_end - t_start
        span.duration_ms = max(0, int(delta.total_seconds() * 1000))
    except Exception:
        span.duration_ms = None


# ---------------------------------------------------------------------------
# Persistence
# ---------------------------------------------------------------------------

_CREATE_SPANS_TABLE = """
CREATE TABLE IF NOT EXISTS spans (
    id                TEXT PRIMARY KEY,
    trace_id          TEXT NOT NULL,
    parent_id         TEXT,
    name              TEXT NOT NULL,
    profile           TEXT,
    model_id          TEXT,
    started_at        TEXT NOT NULL,
    finished_at       TEXT,
    duration_ms       INTEGER,
    status            TEXT NOT NULL DEFAULT 'ok',
    prompt_tokens     INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    attributes        TEXT NOT NULL DEFAULT '{}',
    error_msg         TEXT,
    kind              TEXT NOT NULL DEFAULT 'llm',
    cost_usd          REAL
);
"""

_INSERT_SPAN = """
INSERT OR REPLACE INTO spans
  (id, trace_id, parent_id, name, profile, model_id,
   started_at, finished_at, duration_ms, status,
   prompt_tokens, completion_tokens, attributes, error_msg,
   kind, cost_usd)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
"""


def save_spans_to_db(db_path: Path, spans: list[Span]) -> None:
    """Persist *spans* to the ``spans`` table in the SQLite database at *db_path*.

    Creates the database file and the ``spans`` table if they do not already
    exist.  Uses ``INSERT OR REPLACE`` so re-saving a span (e.g. after updating
    token counts) is safe.

    Parameters
    ----------
    db_path:  Absolute path to the SQLite database file.
    spans:    List of :class:`Span` objects to persist.
    """
    if not spans:
        return

    db_path = Path(db_path)
    db_path.parent.mkdir(parents=True, exist_ok=True)

    conn = sqlite3.connect(str(db_path), timeout=5)
    try:
        conn.executescript(_CREATE_SPANS_TABLE)
        conn.executemany(
            _INSERT_SPAN,
            [
                (
                    s.id,
                    s.trace_id,
                    s.parent_id,
                    s.name,
                    s.profile,
                    s.model_id,
                    s.started_at,
                    s.finished_at,
                    s.duration_ms,
                    s.status,
                    s.prompt_tokens,
                    s.completion_tokens,
                    json.dumps(s.attributes),
                    s.error_msg,
                    s.kind,
                    s.cost_usd,
                )
                for s in spans
            ],
        )
        conn.commit()
    finally:
        conn.close()


def migrate_spans_table(conn: sqlite3.Connection) -> None:
    """Add PRD-046/PRD-048 columns to an existing ``spans`` table.  (PRD-048)

    Safe to call on a database that already has the new columns; the
    ``ALTER TABLE`` errors are silently swallowed.

    Parameters
    ----------
    conn:  An open :class:`sqlite3.Connection` to the database to migrate.
    """
    for sql in (
        "ALTER TABLE spans ADD COLUMN kind TEXT NOT NULL DEFAULT 'llm'",
        "ALTER TABLE spans ADD COLUMN cost_usd REAL",
    ):
        try:
            conn.execute(sql)
            conn.commit()
        except sqlite3.OperationalError:
            # Column already exists — ignore.
            pass


# ---------------------------------------------------------------------------
# Terminal rendering
# ---------------------------------------------------------------------------

def render_trace_terminal(spans: list[Span]) -> str:
    """Render an ASCII flame-chart of *spans* using Rich Tree when available.

    Falls back to plain ASCII when Rich is not installed.

    Returns the rendered string (with ANSI escape codes when Rich is used and
    the output stream supports colour, or plain text otherwise).

    Parameters
    ----------
    spans:  List of :class:`Span` objects belonging to the same trace.
    """
    if not spans:
        return "(no spans)"

    # Build parent → children mapping.
    children: dict[str | None, list[Span]] = {}
    for s in spans:
        children.setdefault(s.parent_id, []).append(s)
    # Sort siblings by start time.
    for bucket in children.values():
        bucket.sort(key=lambda s: s.started_at)

    total_ms: int = max((s.duration_ms or 0 for s in spans), default=1) or 1

    def _bar(ms: int, width: int = 20) -> str:
        filled = max(1, int((ms / total_ms) * width))
        return "█" * filled + "░" * (width - filled)

    def _cost_str(s: Span) -> str:
        if s.cost_usd is None:
            return ""
        return f"  ${s.cost_usd:.4f}"

    def _label(s: Span) -> str:
        dur = s.duration_ms or 0
        bar = _bar(dur)
        tokens = f"{s.prompt_tokens}↑{s.completion_tokens}↓"
        cost = _cost_str(s)
        return f"{s.name}  {bar}  {dur}ms  {tokens}{cost}"

    def _status_color(status: str) -> str:
        return {"ok": "green", "timeout": "yellow"}.get(status, "red")

    # ---- Rich path ----------------------------------------------------------
    try:
        from rich.console import Console
        from rich.text import Text
        from rich.tree import Tree
        import io

        def _build_tree(span: Span, tree: Tree) -> None:
            color = _status_color(span.status)
            label = Text()
            label.append("▸ ", style=color)
            label.append(_label(span))
            branch = tree.add(label)
            for child in children.get(span.id, []):
                _build_tree(child, branch)

        roots = children.get(None, [])
        if not roots:
            return "(no root spans)"

        # Build one Tree per root, then render all to a single string buffer.
        buf = io.StringIO()
        console = Console(file=buf, highlight=False, no_color=False)
        for root in roots:
            color = _status_color(root.status)
            label = Text()
            label.append("▸ ", style=color)
            label.append(_label(root))
            tree = Tree(label)
            for child in children.get(root.id, []):
                _build_tree(child, tree)
            console.print(tree)

        return buf.getvalue().rstrip("\n")

    except Exception:
        pass

    # ---- Plain-text fallback ------------------------------------------------
    lines: list[str] = []

    def _render_plain(span: Span, depth: int) -> None:
        icon = "✓" if span.status == "ok" else ("⚠" if span.status == "timeout" else "✗")
        indent = "  " * depth
        lines.append(f"{indent}{icon} {_label(span)}")
        for child in children.get(span.id, []):
            _render_plain(child, depth + 1)

    for root in children.get(None, []):
        _render_plain(root, 0)

    return "\n".join(lines) if lines else "(no root spans)"


# ---------------------------------------------------------------------------
# TraceProcessor protocol and ProcessorChain  (PRD-046)
# ---------------------------------------------------------------------------

class TraceProcessor(Protocol):
    """Protocol for objects that observe trace and span lifecycle events.

    Implement this protocol to plug custom behaviour into the tracing system
    (e.g. streaming spans to an external observability platform, computing
    cost summaries, writing to a message queue, etc.).
    """

    def on_trace_start(self, trace_id: str, metadata: dict) -> None:
        """Called when a new trace begins."""
        ...

    def on_trace_end(self, trace_id: str, spans: list[Span]) -> None:
        """Called when a trace is finalised with all its spans."""
        ...

    def on_span_start(self, span: Span) -> None:
        """Called immediately after a span is opened."""
        ...

    def on_span_end(self, span: Span) -> None:
        """Called immediately after a span is closed."""
        ...


class ProcessorChain:
    """Fan-out container that forwards lifecycle events to multiple processors.

    Usage
    -----
    chain = ProcessorChain([MyProcessor(), AnotherProcessor()])
    chain.on_trace_start(trace_id, {})
    # ... run spans ...
    chain.on_trace_end(trace_id, spans)

    Any processor that raises an exception is skipped silently so that one
    misbehaving processor cannot interrupt tracing.
    """

    def __init__(self, processors: list[TraceProcessor] | None = None) -> None:
        self.processors: list[TraceProcessor] = list(processors or [])

    def add(self, processor: TraceProcessor) -> None:
        """Append *processor* to the chain."""
        self.processors.append(processor)

    def on_trace_start(self, trace_id: str, metadata: dict) -> None:
        for p in self.processors:
            try:
                p.on_trace_start(trace_id, metadata)
            except Exception:
                pass

    def on_trace_end(self, trace_id: str, spans: list[Span]) -> None:
        for p in self.processors:
            try:
                p.on_trace_end(trace_id, spans)
            except Exception:
                pass

    def on_span_start(self, span: Span) -> None:
        for p in self.processors:
            try:
                p.on_span_start(span)
            except Exception:
                pass

    def on_span_end(self, span: Span) -> None:
        for p in self.processors:
            try:
                p.on_span_end(span)
            except Exception:
                pass


# ---------------------------------------------------------------------------
# Backward-compatible Tracer (uses an open sqlite3.Connection)
# ---------------------------------------------------------------------------

class Tracer:
    """Context-manager-based tracer backed by an already-open sqlite3.Connection.

    Kept for backward compatibility with controller.py code that passes an
    open connection.  New code should prefer the functional API above.
    """

    def __init__(self, db: sqlite3.Connection, trace_id: str, enabled: bool = True):
        self._db = db
        self._trace_id = trace_id
        self._enabled = enabled
        self._stack: list[str] = []

    @contextmanager
    def span(self, name: str, **attrs: Any) -> Generator[Span, None, None]:
        if not self._enabled:
            yield Span()
            return

        parent_id = self._stack[-1] if self._stack else None
        s = open_span(
            trace_id=self._trace_id,
            name=name,
            parent_id=parent_id,
        )
        s.attributes.update(attrs)
        self._stack.append(s.id)
        try:
            yield s
            close_span(s, status="ok")
        except Exception as exc:
            close_span(s, status="error", error_msg=str(exc))
            raise
        finally:
            self._stack.pop()
            self._save(s)

    def _save(self, span: Span) -> None:
        self._db.execute(
            _INSERT_SPAN,
            (
                span.id,
                span.trace_id,
                span.parent_id,
                span.name,
                span.profile,
                span.model_id,
                span.started_at,
                span.finished_at,
                span.duration_ms,
                span.status,
                span.prompt_tokens,
                span.completion_tokens,
                json.dumps(span.attributes),
                span.error_msg,
                span.kind,
                span.cost_usd,
            ),
        )
        self._db.commit()


# ---------------------------------------------------------------------------
# OTLP export helper
# ---------------------------------------------------------------------------

def export_spans_otlp(
    rows: list[Any],
    endpoint: str,
    headers: dict[str, str] | None = None,
) -> bool:
    """POST *rows* (raw SQLite row tuples) as minimal OTLP JSON to *endpoint*.

    Returns True on success, False on any network/HTTP error.
    """
    import urllib.error
    import urllib.request

    col_names = [
        "id", "trace_id", "parent_id", "name", "profile", "model_id",
        "started_at", "finished_at", "duration_ms", "status",
        "prompt_tokens", "completion_tokens", "attributes", "error_msg",
        "kind", "cost_usd",
    ]

    spans_json = [dict(zip(col_names, r)) for r in rows]

    resource_spans = [
        {
            "resource": {
                "attributes": [
                    {"key": "service.name", "value": {"stringValue": "tag-agent"}}
                ]
            },
            "scopeSpans": [
                {
                    "scope": {"name": "tag.tracing"},
                    "spans": [
                        {
                            "traceId": s["trace_id"],
                            "spanId": s["id"],
                            "parentSpanId": s.get("parent_id") or "",
                            "name": s["name"],
                            "startTimeUnixNano": 0,
                            "endTimeUnixNano": int(
                                (s.get("duration_ms") or 0) * 1_000_000
                            ),
                            "status": {
                                "code": 1 if s["status"] == "ok" else 2
                            },
                            "attributes": [
                                {
                                    "key": k,
                                    "value": {"stringValue": str(v)},
                                }
                                for k, v in (
                                    json.loads(s.get("attributes") or "{}") or {}
                                ).items()
                            ],
                        }
                        for s in spans_json
                    ],
                }
            ],
        }
    ]

    payload = json.dumps({"resourceSpans": resource_spans}).encode()
    url = endpoint.rstrip("/") + "/v1/traces"
    req = urllib.request.Request(
        url,
        data=payload,
        method="POST",
        headers={"Content-Type": "application/json", **(headers or {})},
    )
    try:
        with urllib.request.urlopen(req, timeout=10):
            return True
    except urllib.error.URLError:
        return False
