# PRD-053: TraceProcessor Lifecycle Hooks Protocol (`tag hooks trace`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** S (3-5 days)
**Category:** Evaluation & Observability
**Affects:** `tracing.py (TraceProcessor protocol)`
**Depends on:** PRD-013 (agent tracing/observability), PRD-016 (webhook event triggers), PRD-027 (eval framework), PRD-028 (sandbox code execution), PRD-034 (security hardening), PRD-040 (notification hooks), PRD-041 (OTel GenAI span cost attribution), PRD-044 (AgentOps session observability)
**Inspired by:** OpenAI Agents SDK TraceProcessor, OpenTelemetry SpanProcessor

---

## 1. Overview

TAG's tracing infrastructure (PRD-013) captures every agent step as structured `Span` objects persisted to SQLite and optionally exported to OTLP backends. The current architecture is monolithic: `controller.py` directly calls `open_span`, `close_span`, and `save_spans_to_db` from `tracing.py`. Any external code that wants to react to tracing events — to fan out spans to a second backend, compute real-time metrics, write to a message queue, or trigger alerting — must either fork the controller or poll SQLite after the fact. Neither approach is sustainable as the evaluation and observability ecosystem grows.

This PRD introduces a **TraceProcessor protocol** for `tracing.py`: a formal lifecycle hook system that fires typed callbacks at four precise moments in a span's lifetime — `on_trace_start`, `on_trace_end`, `on_span_start`, and `on_span_end`. The design mirrors OpenAI Agents SDK's `TraceProcessor` interface and OpenTelemetry's `SpanProcessor` contract, making it immediately familiar to developers who work across agent frameworks. Registered processors receive rich typed objects (a `Trace` dataclass and the existing `Span` dataclass) and can perform arbitrary work: forward to Langfuse, compute token budgets, trigger evaluations, stream to a WebSocket, or populate a cost-attribution table.

The `tag hooks trace` subcommand tree provides CLI management of registered processors. Processors are stored by fully-qualified Python dotted path (e.g. `my_module.MyProcessor`) in a new `trace_processors` SQLite table. At agent startup, `tracing.py` imports and instantiates each registered processor class, builds a `CompositeTraceProcessor` chain, and fans out lifecycle events in registration order. A processor that raises an exception is isolated: the error is logged, the processor is skipped for the remainder of the run, and the agent continues executing without interruption.

The motivation for this feature is architectural cleanliness, not raw capability. TAG already ships AgentOps integration (PRD-044) and OTLP export (PRD-041), both of which required adding code to `controller.py`. Every new observability integration repeats this pattern: import the SDK, find the right hook points in `controller.py`, patch them without breaking existing callers, and add tests. TraceProcessor lifecycle hooks replace that pattern with a stable interface: external integrations register a processor class once, and TAG's tracing layer calls it automatically without any controller changes. PRD-044 and PRD-041 are migration candidates to this interface in a future cleanup sprint.

---

## 2. Problem Statement

### 2.1 Every observability integration modifies `controller.py`

`controller.py` is already approximately 10,000 lines. PRD-044 (AgentOps) required injecting calls to `agentops.record_action()` at three sites in the controller; PRD-041 (OTel cost attribution) required patching `export_spans_otlp` and adding a new `gen_ai.*` attribute mapping. Each integration increases the file's complexity, adds conditional branches guarded by config checks, and creates new test surface that must be maintained. There is no stable abstraction layer between "agent execution" and "what observers do with execution events." The TraceProcessor protocol provides that layer: integrations live outside `controller.py` entirely.

### 2.2 Third-party and user-written processors have no registration mechanism

A developer building a custom TAG integration — say, a processor that writes spans to their company's internal data lake — has no clean hook point today. They must either subclass `controller.py` in a way the codebase does not support, monkey-patch `open_span`/`close_span` at import time, or post-process the SQLite `spans` table with a polling loop. All three approaches are fragile, undocumented, and break across TAG releases. `tag hooks trace add --processor` gives them a first-class, versioned registration mechanism that survives upgrades.

### 2.3 Real-time processing is impossible with poll-based SQLite reads

The current architecture writes spans to SQLite only when a run completes (via `save_spans_to_db`). A consumer that wants to react to span events in real time — to update a live dashboard, trigger a budget cut-off when token spend crosses a threshold mid-run, or stream span data to a WebSocket — cannot do so without polling the database. The `on_span_start` and `on_span_end` callbacks fire synchronously as spans open and close, enabling latency-sensitive processing with sub-millisecond hook invocation overhead in the common case.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | Define a `TraceProcessor` runtime protocol in `tracing.py` with four lifecycle methods: `on_trace_start`, `on_trace_end`, `on_span_start`, `on_span_end`. Any class implementing these four methods is a valid processor without inheriting from a base class. |
| G2 | Implement a `CompositeTraceProcessor` that fans out all four callbacks to an ordered list of registered processors, isolating exceptions per-processor so one failing processor cannot abort the chain. |
| G3 | Persist registered processor configurations to a `trace_processors` table in `~/.tag/runtime/tag.sqlite3` using `open_db()` as the access pattern. |
| G4 | Provide `tag hooks trace add --processor <dotted.path>`, `tag hooks trace list`, and `tag hooks trace remove <id>` as the CLI surface for managing registered processors. |
| G5 | Load and instantiate registered processors at agent startup (lazy import, guarded by `importlib.import_module`); skip any processor whose module fails to import with a clear warning. |
| G6 | The `Trace` dataclass (new in this PRD) provides a logical grouping object that correlates all spans belonging to one agent run, delivered to `on_trace_start` / `on_trace_end`. |
| G7 | Processor registration supports an optional `--config` JSON blob passed at `add` time, stored in the database and delivered to the processor's `__init__` as `**kwargs`. |
| G8 | Zero overhead when no processors are registered: the `CompositeTraceProcessor` short-circuits all four callback dispatches when its processor list is empty. |
| G9 | `tag hooks trace test --processor <dotted.path>` instantiates the processor class and fires a synthetic `on_span_start` / `on_span_end` pair to validate that the class is importable and callable before registration. |

---

## 4. Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Replacing TAG's existing SQLite span persistence. The `save_spans_to_db` path remains unchanged; TraceProcessor hooks are additional, not a replacement. |
| NG2 | Asynchronous (async/await) processor callbacks. All four lifecycle methods are synchronous. Async processors must manage their own event loop internally (e.g. via `asyncio.run()` or a background thread). |
| NG3 | Hot-reloading registered processors mid-run. Processor changes take effect on the next `tag run` invocation; in-flight runs are not affected. |
| NG4 | Migrating PRD-044 (AgentOps) or PRD-041 (OTel) to use TraceProcessor in this PRD. Those are tracked as future cleanup tasks once this protocol is stable. |
| NG5 | Access control or sandboxing of processor code. Processors run in the same process as TAG with full filesystem and network access. Security considerations are documented in Section 10. |
| NG6 | A built-in processor marketplace or registry. Processors are identified by dotted Python path; discovery is the user's responsibility. |
| NG7 | Span mutation by processors. Processors receive read-only views; they cannot modify `Span` attributes or suppress span recording. |
| NG8 | Cross-process or distributed processor dispatch. All processors run in the same OS process as the TAG agent. Remote dispatch is achievable by writing a processor that POSTs to a queue. |

---

## 5. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Hook invocation latency | `on_span_end` dispatch overhead < 1 ms when 0 processors registered; < 5 ms with 3 processors each doing a no-op | `time.perf_counter` benchmark, 1000 iterations |
| Processor isolation | One processor raising `RuntimeError` in `on_span_end` does not prevent other processors from receiving the callback | Unit test: 3-processor chain, middle processor raises, verify third processor still called |
| Import failure handling | Processor with unresolvable module path logs a `WARNING` and is skipped; agent run proceeds normally | Unit test with mocked `importlib.import_module` raising `ImportError` |
| CLI round-trip | `tag hooks trace add --processor my_mod.P && tag hooks trace list` shows the new entry; `tag hooks trace remove <id>` removes it | Integration test against temp SQLite DB |
| Zero-overhead guarantee | `tag run` with zero registered processors: `sys.getsizeof(composite._processors)` == 0 and no import of any processor module | Unit test asserting `sys.modules` does not contain processor module name |
| Test command | `tag hooks trace test --processor tag.tracing.LoggingTraceProcessor` exits 0 and prints "ok" | Integration test |
| Config round-trip | `tag hooks trace add --processor my_mod.P --config '{"dsn":"sqlite:///x"}'` stores config JSON; processor `__init__` receives `dsn="sqlite:///x"` | Unit test with mock processor class |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|------------|----------|
| U1 | Developer integrating a custom observability backend | write a Python class with `on_span_end` and register it via `tag hooks trace add --processor myco.observability.TagProcessor` | every TAG agent span is automatically forwarded to my company's data lake without modifying `controller.py` |
| U2 | Platform engineer building a live dashboard | receive `on_span_start` callbacks in real time | I can update a WebSocket-driven UI as each tool call begins, not minutes later when the run completes |
| U3 | ML engineer running eval pipelines | register a processor that accumulates token counts and fires a DeepEval metric evaluation when `on_trace_end` fires | I get automated quality scores at the end of every agent run without a separate polling job |
| U4 | Developer debugging a slow integration | run `tag hooks trace test --processor myco.observability.TagProcessor` before registering | I catch `ImportError` or `AttributeError` early, before a production run silently skips my processor |
| U5 | Security-conscious operator | run `tag hooks trace list` to see all registered processors before a run | I can audit exactly which third-party code will receive span data during execution |
| U6 | Developer maintaining multiple projects | register a processor with `--config '{"project":"research-bot","env":"staging"}'` | the processor receives project context without hardcoding it in the processor class itself |
| U7 | Developer cleaning up | run `tag hooks trace remove abc123` | I can deregister a processor I no longer need without editing config files manually |
| U8 | Contributor porting PRD-044 AgentOps to TraceProcessor | implement `AgentOpsTraceProcessor` in `src/tag/integrations/agentops_bridge.py` that implements the four-method protocol | the AgentOps integration lives in its own file and `controller.py` is not touched |
| U9 | Developer diagnosing a processor that intermittently fails | run `tag hooks trace list` and see the last-error column | I can identify which processor is raising exceptions in production without adding debug logging |

---

## 7. Proposed CLI Surface

### 7.1 `tag hooks trace add`

Register a new TraceProcessor.

```
tag hooks trace add \
  --processor <dotted.python.path> \
  [--config '<json>'] \
  [--name <human-label>] \
  [--enabled | --disabled] \
  [--profile <profile-name>]
```

**Options:**

- `--processor` (required): Fully-qualified Python dotted path to the processor class, e.g. `myco.obs.TagProcessor`. The class must be importable from the current Python environment (`sys.path`).
- `--config`: Optional JSON object (must be a flat `{}` object, no nested arrays at depth > 1). Parsed by `json.loads`; keys passed as `**kwargs` to the processor `__init__`. Stored verbatim in the `trace_processors` table.
- `--name`: Optional human-readable label (max 80 chars). Defaults to the class name extracted from the dotted path.
- `--enabled` / `--disabled`: Initial enablement state. Default: enabled. A disabled processor is stored but not instantiated at run time.
- `--profile`: Restrict this processor to runs using the named profile. When omitted, the processor fires for all profiles.

**Output (success):**

```
Registered TraceProcessor
  id:         a3f2b1c0
  processor:  myco.obs.TagProcessor
  name:       TagProcessor
  profile:    (all profiles)
  config:     {"dsn": "postgres://..."}
  enabled:    yes
  created_at: 2026-06-17T09:14:22Z

Run 'tag hooks trace test --id a3f2b1c0' to verify the processor is importable.
```

**Output (error — class not importable at add time, warning only):**

```
WARNING: Could not import myco.obs.TagProcessor (ModuleNotFoundError: No module named 'myco').
         The processor has been registered but will be skipped at run time unless the module
         becomes importable. Run 'tag hooks trace test --id a3f2b1c0' to re-verify.
```

Note: `add` does NOT fail hard on import errors; it registers and warns. This matches `opentelemetry-sdk` behavior for deferred processor loading.

---

### 7.2 `tag hooks trace list`

List all registered TraceProcessors.

```
tag hooks trace list [--json] [--profile <profile-name>] [--all]
```

**Options:**

- `--json`: Output machine-readable JSON array.
- `--profile`: Filter to processors registered for a specific profile (or omit to show all).
- `--all`: Include disabled processors (by default, only enabled processors are shown).

**Output (table):**

```
ID        NAME                PROCESSOR                   PROFILE   ENABLED  LAST_ERROR
────────  ──────────────────  ──────────────────────────  ────────  ───────  ──────────────────────────────
a3f2b1c0  TagProcessor        myco.obs.TagProcessor       (all)     yes      —
b7e9d234  LangfuseProcessor   myco.lf.LangfuseProcessor   coder     yes      2026-06-16T22:01:11Z: TimeoutError
c1a04f55  LoggingProcessor    tag.tracing.LoggingTrace…   (all)     no       —

3 processors registered (2 enabled).
```

**Output (JSON):**

```json
[
  {
    "id": "a3f2b1c0",
    "name": "TagProcessor",
    "processor": "myco.obs.TagProcessor",
    "profile": null,
    "enabled": true,
    "config": {"dsn": "postgres://..."},
    "created_at": "2026-06-17T09:14:22Z",
    "last_error": null,
    "last_error_at": null
  }
]
```

---

### 7.3 `tag hooks trace remove`

Deregister a TraceProcessor by its ID.

```
tag hooks trace remove <id> [--yes]
```

**Options:**

- `<id>` (required): The 8-character hex ID shown in `tag hooks trace list`. Prefix matching is supported (minimum 4 characters).
- `--yes`: Skip confirmation prompt.

**Output:**

```
Removed TraceProcessor 'TagProcessor' (a3f2b1c0).
```

**Error (ID not found):**

```
error: no processor with id matching 'zzzz'. Run 'tag hooks trace list' to see registered processors.
```

---

### 7.4 `tag hooks trace test`

Validate a processor is importable and callable, without running an agent.

```
tag hooks trace test \
  { --processor <dotted.path> | --id <id> } \
  [--config '<json>']
```

**Behavior:** Imports the processor class, instantiates it with the provided (or stored) config, constructs a synthetic `Trace` and two `Span` objects, fires `on_trace_start`, `on_span_start`, `on_span_end`, `on_trace_end` in sequence, and reports timing.

**Output (success):**

```
Testing TraceProcessor: myco.obs.TagProcessor
  import:           ok (12 ms)
  instantiate:      ok (2 ms)
  on_trace_start:   ok (0.3 ms)
  on_span_start:    ok (0.1 ms)
  on_span_end:      ok (148 ms)   <-- user implementation
  on_trace_end:     ok (0.4 ms)

All lifecycle methods passed. Processor is ready for registration.
```

**Output (failure):**

```
Testing TraceProcessor: myco.obs.TagProcessor
  import:           ok (12 ms)
  instantiate:      FAILED

  AttributeError: 'TagProcessor' object has no attribute 'on_span_end'

Processor class does not implement the TraceProcessor protocol.
Required methods: on_trace_start, on_trace_end, on_span_start, on_span_end
```

---

### 7.5 `tag hooks trace enable` / `tag hooks trace disable`

Toggle a registered processor without removing it.

```
tag hooks trace enable <id>
tag hooks trace disable <id>
```

**Output:**

```
TraceProcessor 'LangfuseProcessor' (b7e9d234) enabled.
```

---

## 8. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | `tracing.py` MUST define a `TraceProcessor` runtime protocol class (using `typing.runtime_checkable` and `typing.Protocol`) with exactly four required methods: `on_trace_start(trace: Trace) -> None`, `on_trace_end(trace: Trace) -> None`, `on_span_start(span: Span) -> None`, `on_span_end(span: Span) -> None`. |
| FR-02 | `tracing.py` MUST define a `Trace` dataclass with fields: `id: str`, `profile: str | None`, `run_id: str | None`, `started_at: str`, `finished_at: str | None`, `span_count: int`, `status: str`. |
| FR-03 | `tracing.py` MUST define a `CompositeTraceProcessor` class that accepts `processors: list[TraceProcessor]` and implements the `TraceProcessor` protocol by iterating the list and calling each processor's method, catching and logging all exceptions per-processor. |
| FR-04 | `CompositeTraceProcessor.on_span_end` MUST call each registered processor's `on_span_end` even if a previous processor raises an exception. Exception isolation MUST be per-processor per-event, not per-trace. |
| FR-05 | When `CompositeTraceProcessor._processors` is empty, all four dispatch methods MUST return immediately with no iteration overhead (early return, not an empty loop). |
| FR-06 | `tracing.py` MUST expose a module-level `_global_composite: CompositeTraceProcessor` instance and `register_processor(p: TraceProcessor)` / `unregister_processor(p: TraceProcessor)` module-level functions for programmatic use. |
| FR-07 | `open_span()` MUST call `_global_composite.on_span_start(span)` after constructing the `Span` and before returning it. |
| FR-08 | `close_span()` MUST call `_global_composite.on_span_end(span)` after computing `duration_ms` and before returning. Callbacks MUST NOT fire if the span was already closed (idempotency preserved). |
| FR-09 | A `begin_trace(trace_id, profile, run_id) -> Trace` function MUST be added to `tracing.py`. It constructs a `Trace`, calls `_global_composite.on_trace_start(trace)`, and returns the `Trace`. |
| FR-10 | An `end_trace(trace: Trace, spans: list[Span]) -> None` function MUST be added to `tracing.py`. It sets `finished_at`, computes `span_count`, sets `status` (error if any span has `status == 'error'`, else ok), calls `_global_composite.on_trace_end(trace)`. |
| FR-11 | The `tag hooks trace add` command MUST write to the `trace_processors` table in the WAL-mode SQLite DB at `~/.tag/runtime/tag.sqlite3` using `open_db()`. |
| FR-12 | The `tag hooks trace add` command MUST attempt to import the processor class immediately after storing the record, and MUST print a warning (not fail) if the import fails. |
| FR-13 | The `tag hooks trace list` command MUST read from the `trace_processors` table and render as a Rich table (or plain text if Rich is not available) including `id`, `name`, `processor`, `profile`, `enabled`, and `last_error` columns. |
| FR-14 | The `tag hooks trace remove <id>` command MUST support prefix matching on the `id` column (minimum 4 characters) and MUST error with a helpful message when the prefix matches zero or more-than-one row. |
| FR-15 | The `tag hooks trace test` command MUST instantiate the processor with the stored or provided config JSON parsed as `**kwargs`, fire all four lifecycle methods with synthetic data, and report per-method timing. It MUST exit with code 1 if any lifecycle method raises. |
| FR-16 | At agent startup (in `controller.py`'s `cmd_run` or equivalent), ALL enabled processors in the `trace_processors` table MUST be loaded: each processor module imported via `importlib.import_module`, the class retrieved, instantiated with its stored config `**kwargs`, and registered via `register_processor`. |
| FR-17 | When `--profile <name>` is specified at `add` time, the processor MUST only be instantiated when the running profile matches; processors without a profile restriction MUST fire for all profiles. |
| FR-18 | The `tag hooks trace enable` and `tag hooks trace disable` commands MUST toggle the `enabled` column of the matching row and print confirmation. |
| FR-19 | Failed processor callbacks MUST write the exception class name, message (truncated to 200 chars), and UTC timestamp to the `last_error` and `last_error_at` columns of the corresponding `trace_processors` row. |
| FR-20 | `tracing.py` MUST ship a built-in `LoggingTraceProcessor` class that logs all four lifecycle events to Python's `logging` module at `DEBUG` level, usable as a reference implementation and for `tag hooks trace test`. |

---

## 9. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | The `CompositeTraceProcessor` dispatch overhead (with zero processors) MUST be less than 1 microsecond per `on_span_start` / `on_span_end` call, measured by `time.perf_counter` over 10,000 iterations. |
| NFR-02 | `tracing.py` MUST remain importable with zero third-party dependencies. `TraceProcessor`, `Trace`, `CompositeTraceProcessor`, `LoggingTraceProcessor`, and all new symbols MUST use only Python stdlib. |
| NFR-03 | Processor module imports MUST use `importlib.import_module`, never `eval` or `exec`, and MUST be deferred until agent startup (not at `import tag.tracing` time). |
| NFR-04 | The `trace_processors` table MUST use WAL journal mode (inherited from `open_db()`) and MUST define a UNIQUE constraint on `(processor, profile)` to prevent duplicate registrations. |
| NFR-05 | All four lifecycle callbacks MUST complete (including all processor dispatch) within the calling thread. TAG MUST NOT spawn background threads on behalf of processors; that is the processor author's responsibility. |
| NFR-06 | `tag hooks trace add` MUST complete (including the import-attempt and DB write) in under 2 seconds on a machine where the processor module is importable. |
| NFR-07 | `tag hooks trace list` MUST complete in under 100 ms for up to 100 registered processors. |
| NFR-08 | The `config` JSON column MUST NOT exceed 4 KB. `tag hooks trace add` MUST validate and reject configs exceeding this limit with a clear error message. |
| NFR-09 | `LoggingTraceProcessor` MUST be usable as a self-contained example without any TAG-specific imports beyond `tracing.Span` and `tracing.Trace`. |
| NFR-10 | Exception messages written to `last_error` MUST be truncated to 200 characters before storage to prevent oversized rows from unbounded stack traces. |

---

## 10. Technical Design

### 10.1 New and Modified Files

| File | Change |
|------|--------|
| `src/tag/tracing.py` | Add `Trace` dataclass, `TraceProcessor` protocol, `CompositeTraceProcessor`, `LoggingTraceProcessor`, `_global_composite`, `register_processor`, `unregister_processor`, `begin_trace`, `end_trace`. Patch `open_span` and `close_span` to call composite callbacks. |
| `src/tag/controller.py` | Add `cmd_hooks_trace` subcommand dispatcher and its `add`, `list`, `remove`, `test`, `enable`, `disable` implementations. Add processor loading at run startup. |
| `tests/test_traceprocessor.py` | New: unit tests for protocol, composite isolation, zero-overhead path, built-in LoggingTraceProcessor. |
| `tests/test_hooks_trace_cli.py` | New: integration tests for CLI subcommands against temp SQLite DB. |

### 10.2 SQLite DDL

```sql
-- Migration: add to the existing WAL-mode tag.sqlite3
-- Applied via open_db() on first access after upgrade.

CREATE TABLE IF NOT EXISTS trace_processors (
    id           TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(4)))),
    name         TEXT NOT NULL,
    processor    TEXT NOT NULL,           -- dotted Python path, e.g. "myco.obs.TagProcessor"
    profile      TEXT,                    -- NULL = all profiles
    enabled      INTEGER NOT NULL DEFAULT 1,  -- 0 = disabled, 1 = enabled
    config_json  TEXT NOT NULL DEFAULT '{}',  -- flat JSON object, max 4096 bytes
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    last_error   TEXT,                    -- last exception message (≤200 chars), NULL if clean
    last_error_at TEXT,                   -- ISO-8601 UTC of last exception, NULL if clean
    UNIQUE (processor, profile)           -- prevent duplicate registration
);

CREATE INDEX IF NOT EXISTS idx_trace_processors_enabled
    ON trace_processors (enabled, profile);
```

The `UNIQUE (processor, profile)` constraint uses SQLite's NULL semantics: two rows with the same `processor` and `profile = NULL` are treated as duplicates (SQLite does NOT consider NULL != NULL for unique constraints when using the partial index pattern — use `UNIQUE (processor, COALESCE(profile, ''))` or handle in application logic with an explicit check before insert).

Corrected DDL for the unique constraint:

```sql
-- Application-layer check before INSERT:
-- SELECT COUNT(*) FROM trace_processors WHERE processor = ? AND (profile = ? OR (profile IS NULL AND ? IS NULL))
-- If count > 0, raise "already registered" error.
```

### 10.3 Core Python Types

```python
# ---- src/tag/tracing.py additions ----

from __future__ import annotations
import importlib
import logging
import time
from dataclasses import dataclass, field
from typing import Any, Protocol, runtime_checkable

_log = logging.getLogger(__name__)


@dataclass
class Trace:
    """Logical grouping of all spans belonging to one agent run.

    Delivered to on_trace_start (with finished_at=None, span_count=0)
    and on_trace_end (with finished_at set and span_count populated).
    """
    id: str                         # matches trace_id on child Span objects
    profile: str | None = None      # TAG profile name, or None if not applicable
    run_id: str | None = None       # FK to the runs table row, if available
    started_at: str = field(default_factory=_utc_now)
    finished_at: str | None = None
    span_count: int = 0
    status: str = "ok"              # "ok" | "error"
    metadata: dict[str, Any] = field(default_factory=dict)


@runtime_checkable
class TraceProcessor(Protocol):
    """Protocol that any TraceProcessor must satisfy.

    All four methods must be present and callable. No inheritance required.
    Processors receive the actual Span/Trace objects (not copies); they MUST
    NOT mutate these objects.
    """

    def on_trace_start(self, trace: Trace) -> None:
        """Called immediately after begin_trace() constructs the Trace."""
        ...

    def on_trace_end(self, trace: Trace) -> None:
        """Called by end_trace() after finished_at and span_count are set."""
        ...

    def on_span_start(self, span: Span) -> None:
        """Called by open_span() after the Span is constructed, before return."""
        ...

    def on_span_end(self, span: Span) -> None:
        """Called by close_span() after duration_ms is computed, before return."""
        ...


class CompositeTraceProcessor:
    """Fan-out processor: dispatches lifecycle events to N registered processors.

    Exception isolation: a processor that raises during any callback is caught,
    logged at WARNING level, and skipped for that callback only. The processor
    remains in the list for future callbacks (it may recover). The `_error_counts`
    dict tracks cumulative failures per processor index for diagnostics.
    """

    __slots__ = ("_processors", "_error_counts")

    def __init__(self) -> None:
        self._processors: list[TraceProcessor] = []
        self._error_counts: dict[int, int] = {}

    def add(self, p: TraceProcessor) -> None:
        if not isinstance(p, TraceProcessor):
            raise TypeError(
                f"{type(p).__name__} does not implement the TraceProcessor protocol. "
                f"Required methods: on_trace_start, on_trace_end, on_span_start, on_span_end."
            )
        self._processors.append(p)

    def remove(self, p: TraceProcessor) -> None:
        try:
            self._processors.remove(p)
        except ValueError:
            pass

    def _dispatch(self, method_name: str, arg: Trace | Span) -> None:
        if not self._processors:        # FR-05: early return, zero overhead
            return
        for idx, proc in enumerate(self._processors):
            try:
                getattr(proc, method_name)(arg)
            except Exception as exc:
                self._error_counts[idx] = self._error_counts.get(idx, 0) + 1
                _log.warning(
                    "TraceProcessor %s.%s raised %s: %s",
                    type(proc).__name__,
                    method_name,
                    type(exc).__name__,
                    str(exc)[:200],
                )

    def on_trace_start(self, trace: Trace) -> None:
        self._dispatch("on_trace_start", trace)

    def on_trace_end(self, trace: Trace) -> None:
        self._dispatch("on_trace_end", trace)

    def on_span_start(self, span: Span) -> None:
        self._dispatch("on_span_start", span)

    def on_span_end(self, span: Span) -> None:
        self._dispatch("on_span_end", span)


# Module-level singleton — zero cost when processor list is empty.
_global_composite: CompositeTraceProcessor = CompositeTraceProcessor()


def register_processor(p: TraceProcessor) -> None:
    """Register *p* with the module-level composite processor."""
    _global_composite.add(p)


def unregister_processor(p: TraceProcessor) -> None:
    """Remove *p* from the module-level composite processor (no-op if not registered)."""
    _global_composite.remove(p)


def begin_trace(
    trace_id: str,
    profile: str | None = None,
    run_id: str | None = None,
    metadata: dict[str, Any] | None = None,
) -> Trace:
    """Construct a Trace, fire on_trace_start, and return the Trace."""
    trace = Trace(
        id=trace_id,
        profile=profile,
        run_id=run_id,
        metadata=metadata or {},
    )
    _global_composite.on_trace_start(trace)
    return trace


def end_trace(trace: Trace, spans: list[Span]) -> None:
    """Close *trace*, compute aggregate fields, fire on_trace_end."""
    trace.finished_at = _utc_now()
    trace.span_count = len(spans)
    trace.status = "error" if any(s.status == "error" for s in spans) else "ok"
    _global_composite.on_trace_end(trace)
```

### 10.4 Updated `open_span` and `close_span`

```python
def open_span(
    trace_id: str,
    name: str,
    profile: str | None = None,
    model_id: str | None = None,
    parent_id: str | None = None,
) -> Span:
    span = Span(
        trace_id=trace_id,
        name=name,
        profile=profile,
        model_id=model_id,
        parent_id=parent_id,
    )
    _global_composite.on_span_start(span)   # FR-07
    return span


def close_span(
    span: Span,
    status: str = "ok",
    prompt_tokens: int = 0,
    completion_tokens: int = 0,
    error_msg: str | None = None,
) -> None:
    if span.finished_at is not None:
        return  # idempotent — do NOT re-fire on_span_end
    finished_iso = _utc_now()
    span.finished_at = finished_iso
    span.status = status
    span.prompt_tokens = prompt_tokens
    span.completion_tokens = completion_tokens
    span.error_msg = error_msg
    try:
        t_start = datetime.fromisoformat(span.started_at)
        t_end = datetime.fromisoformat(finished_iso)
        span.duration_ms = max(0, int((t_end - t_start).total_seconds() * 1000))
    except Exception:
        span.duration_ms = None
    _global_composite.on_span_end(span)     # FR-08
```

### 10.5 Built-in `LoggingTraceProcessor`

```python
class LoggingTraceProcessor:
    """Reference implementation: logs all lifecycle events at DEBUG level.

    Satisfies the TraceProcessor protocol without inheriting from it.
    Usable immediately: tag hooks trace add --processor tag.tracing.LoggingTraceProcessor
    """

    def __init__(self, logger_name: str = "tag.trace", **kwargs: Any) -> None:
        self._log = logging.getLogger(logger_name)

    def on_trace_start(self, trace: Trace) -> None:
        self._log.debug(
            "TRACE START id=%s profile=%s run_id=%s",
            trace.id, trace.profile, trace.run_id,
        )

    def on_trace_end(self, trace: Trace) -> None:
        self._log.debug(
            "TRACE END id=%s status=%s spans=%d",
            trace.id, trace.status, trace.span_count,
        )

    def on_span_start(self, span: Span) -> None:
        self._log.debug(
            "SPAN START id=%s name=%s trace=%s",
            span.id, span.name, span.trace_id,
        )

    def on_span_end(self, span: Span) -> None:
        self._log.debug(
            "SPAN END id=%s name=%s status=%s duration_ms=%s tokens=%d+%d",
            span.id, span.name, span.status, span.duration_ms,
            span.prompt_tokens, span.completion_tokens,
        )
```

### 10.6 Processor Loading at Agent Startup

The following helper is called from `controller.py`'s run-initialisation path (immediately before the agent loop starts, after the profile is resolved):

```python
# src/tag/controller.py — new helper function

def _load_trace_processors(db: sqlite3.Connection, profile_name: str | None) -> None:
    """Import and register all enabled TraceProcessors from the DB.

    Called once per agent run, before the first open_span() call.
    Profile filtering: rows with profile=NULL fire for all profiles;
    rows with profile=<name> fire only when profile_name matches.
    """
    from tag import tracing as _tracing

    rows = db.execute(
        """
        SELECT id, processor, config_json, name
        FROM trace_processors
        WHERE enabled = 1
          AND (profile IS NULL OR profile = ?)
        ORDER BY rowid ASC
        """,
        (profile_name,),
    ).fetchall()

    for row_id, dotted_path, config_json_str, display_name in rows:
        config: dict = {}
        try:
            config = json.loads(config_json_str or "{}")
        except json.JSONDecodeError:
            _log.warning("trace_processors row %s: malformed config_json, ignoring", row_id)

        module_path, _, class_name = dotted_path.rpartition(".")
        if not module_path:
            _log.warning(
                "trace_processors row %s: invalid processor path '%s' (no module component)",
                row_id, dotted_path,
            )
            continue

        try:
            mod = importlib.import_module(module_path)
            cls = getattr(mod, class_name)
            processor = cls(**config)
        except (ImportError, AttributeError, TypeError, Exception) as exc:
            err_msg = f"{type(exc).__name__}: {str(exc)[:200]}"
            _log.warning(
                "Failed to load TraceProcessor '%s' (%s): %s — skipping.",
                display_name, dotted_path, err_msg,
            )
            # Update last_error in DB (best-effort, non-fatal)
            try:
                db.execute(
                    "UPDATE trace_processors SET last_error=?, last_error_at=? WHERE id=?",
                    (err_msg[:200], _utc_now(), row_id),
                )
                db.commit()
            except Exception:
                pass
            continue

        if not isinstance(processor, _tracing.TraceProcessor):
            _log.warning(
                "TraceProcessor '%s' (%s) does not implement the TraceProcessor protocol "
                "(missing: on_trace_start / on_trace_end / on_span_start / on_span_end). Skipping.",
                display_name, dotted_path,
            )
            continue

        _tracing.register_processor(processor)
        _log.debug("Registered TraceProcessor '%s' (%s)", display_name, dotted_path)
```

### 10.7 `tag hooks trace test` Algorithm

```
1. Resolve dotted path (from --processor or DB row).
2. importlib.import_module(module_path)      → time this
3. getattr(mod, class_name)(**config)        → time this
4. isinstance(processor, TraceProcessor)     → verify protocol
5. Construct synthetic Trace and two Spans.
6. Call on_trace_start(trace)               → time, catch exceptions
7. Call on_span_start(span1)                → time, catch exceptions
8. Call on_span_end(span1)                  → time, catch exceptions
9. Call on_span_start(span2)                → time, catch exceptions
10. Call on_span_end(span2)                 → time, catch exceptions
11. Call on_trace_end(trace)                → time, catch exceptions
12. Print timing table; exit 0 if all passed, exit 1 if any failed.
```

### 10.8 Integration with Existing PRD-013 Tracer Context Manager

The existing `Tracer` context-manager class in `tracing.py` wraps spans around a `sqlite3.Connection`. It already calls `open_span` and `close_span` internally, so it will automatically gain lifecycle hook dispatch without any changes. The `begin_trace` / `end_trace` wrappers should be called by `Tracer.__enter__` / `__exit__` respectively to fire `on_trace_start` / `on_trace_end` at the correct granularity.

### 10.9 Interaction with PRD-041 OTel Export and PRD-044 AgentOps

Neither PRD-041 nor PRD-044 are migrated in this PRD (per NG4). However, both are documented migration candidates:

- **PRD-041 migration path:** Implement `OtelCostAttributionProcessor(TraceProcessor)` in `src/tag/integrations/otel_processor.py`. On `on_span_end`, apply the `gen_ai.*` attribute mapping and enqueue to the OTLP export buffer. Remove the equivalent code from `controller.py`.
- **PRD-044 migration path:** Implement `AgentOpsTraceProcessor(TraceProcessor)` in `src/tag/integrations/agentops_bridge.py`. On `on_trace_start`, call `agentops.start_session()`; on `on_span_end`, call `agentops.record_action()`; on `on_trace_end`, call `agentops.end_session()`. Remove the equivalent injections from `controller.py`.

---

## 11. Security Considerations

1. **Arbitrary code execution surface.** `tag hooks trace add --processor` accepts a Python dotted path that is loaded via `importlib.import_module` and instantiated. Any code in the processor's `__init__` and lifecycle methods runs with full OS-level privileges of the TAG process. Users should treat processor registration with the same security posture as installing a Python package. The `tag hooks trace list` command provides an audit trail.

2. **Config JSON injection.** The `--config` JSON blob is parsed by `json.loads` and passed as `**kwargs` to the processor `__init__`. An attacker who can write to `~/.tag/runtime/tag.sqlite3` can inject arbitrary kwargs. Mitigation: `open_db()` sets restrictive file permissions (`0600`) on the SQLite file; the `config_json` column is limited to 4 KB (FR-08, NFR-08) to prevent oversized payloads.

3. **Processor exception leakage.** Exception messages from processor callbacks are stored in the `last_error` column (truncated to 200 chars). If a processor's exception message contains sensitive runtime data (tokens, API keys echoed in error strings), those substrings would land in SQLite. Truncation to 200 chars limits but does not eliminate this. Processor authors should avoid including sensitive data in exception messages.

4. **Supply chain.** A processor package installed via `pip` could be compromised. TAG does not pin or hash processor packages. Users are responsible for vetting processor packages. The `tag hooks trace list` command should be consulted before production runs to confirm no unexpected processors are registered.

5. **Profile-scoped processors and privilege escalation.** A processor registered with `profile=None` fires for all profiles, including profiles with elevated permissions (e.g., profiles granted sandbox execution). Operators who want to restrict processor scope should always set `--profile` at registration time.

6. **No network call interception.** Processors can make arbitrary network calls. A malicious processor could exfiltrate span data (including model outputs embedded in `attributes`) to an external server. This is by design for legitimate use cases (e.g., Langfuse, AgentOps). Operators in air-gapped or high-security environments should explicitly audit and whitelist processor packages.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`tests/test_traceprocessor.py`)

- **Protocol check:** `isinstance(LoggingTraceProcessor(), TraceProcessor)` returns `True`; a class missing `on_span_end` returns `False`.
- **Composite isolation:** Chain of 3 processors where processor at index 1 raises `RuntimeError` on every callback; assert processor at index 2 still receives all four callbacks.
- **Zero-overhead path:** `CompositeTraceProcessor` with empty `_processors`: call `on_span_end` 10,000 times; assert total wall time < 10 ms.
- **Idempotency:** Call `close_span` twice on the same span; assert `on_span_end` is called exactly once.
- **begin_trace / end_trace:** Assert `on_trace_start` receives `Trace` with `finished_at=None`; `on_trace_end` receives same `Trace` with `finished_at` set and `span_count == len(spans)`.
- **Error count tracking:** After 5 failures on processor 0, `composite._error_counts[0] == 5`.
- **Module-level register/unregister:** `register_processor(p)` adds to `_global_composite`; `unregister_processor(p)` removes it; duplicate `unregister_processor` is a no-op.
- **LoggingTraceProcessor output:** Patch `logging.Logger.debug`, fire all four events, assert 4 log calls with correct format strings.

### 12.2 Integration Tests (`tests/test_hooks_trace_cli.py`)

All integration tests operate against a temp directory with a fresh SQLite DB.

- **add → list → remove round-trip:** Register `tag.tracing.LoggingTraceProcessor`, assert `list` shows it, `remove` by ID succeeds, `list` shows empty.
- **Duplicate registration:** `add` the same `--processor` twice; assert second `add` fails with "already registered" error and the table has exactly one row.
- **Profile scoping:** Register with `--profile coder`; assert list filtered by `--profile coder` shows it; assert list filtered by `--profile researcher` shows nothing.
- **Config round-trip:** Register with `--config '{"logger_name":"my.logger"}'`; query DB directly; assert `config_json == '{"logger_name": "my.logger"}'`.
- **enable / disable:** `disable` sets `enabled=0`; `enable` sets `enabled=1`; verify with direct DB query.
- **test command success:** `tag hooks trace test --processor tag.tracing.LoggingTraceProcessor` exits 0.
- **test command failure (bad path):** `tag hooks trace test --processor no.such.Module.Cls` exits 1.
- **Prefix matching on remove:** Insert row with `id='a3f2b1c0'`; `remove a3f2` succeeds; `remove a` fails with "ambiguous prefix".
- **last_error update:** Mock `importlib.import_module` to raise `ImportError` for a registered processor; call `_load_trace_processors`; assert `last_error` column is set in DB.
- **Config size limit:** `add --config '<5001-byte JSON>'` exits 1 with size error.

### 12.3 Performance Tests

```python
# tests/test_traceprocessor_perf.py
import time
from tag.tracing import CompositeTraceProcessor, Span, open_span, close_span, _global_composite

def test_zero_processor_overhead():
    """on_span_end with 0 processors must complete 10k calls in < 10ms."""
    assert len(_global_composite._processors) == 0
    spans = [open_span(trace_id="t", name=f"s{i}") for i in range(10_000)]
    t0 = time.perf_counter()
    for s in spans:
        _global_composite.on_span_end(s)
    elapsed_ms = (time.perf_counter() - t0) * 1000
    assert elapsed_ms < 10, f"overhead too high: {elapsed_ms:.1f}ms for 10k calls"

def test_three_noop_processor_overhead():
    """on_span_end with 3 no-op processors must complete 1k calls in < 5ms."""
    class NoopProcessor:
        def on_trace_start(self, t): pass
        def on_trace_end(self, t): pass
        def on_span_start(self, s): pass
        def on_span_end(self, s): pass

    comp = CompositeTraceProcessor()
    for _ in range(3):
        comp.add(NoopProcessor())
    spans = [open_span(trace_id="t", name=f"s{i}") for i in range(1_000)]
    t0 = time.perf_counter()
    for s in spans:
        comp.on_span_end(s)
    elapsed_ms = (time.perf_counter() - t0) * 1000
    assert elapsed_ms < 5, f"overhead too high: {elapsed_ms:.1f}ms for 1k calls with 3 processors"
```

---

## 13. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|--------------|
| AC-01 | `TraceProcessor` is a `typing.Protocol` with `@runtime_checkable`; `isinstance(LoggingTraceProcessor(), TraceProcessor)` returns `True` | Unit test |
| AC-02 | A class missing `on_span_end` fails `isinstance` check against `TraceProcessor` | Unit test |
| AC-03 | `CompositeTraceProcessor` with 3 processors where index 1 always raises: index 2 still receives all 4 callbacks | Unit test |
| AC-04 | `open_span` fires `on_span_start`; `close_span` fires `on_span_end` exactly once per span | Unit test |
| AC-05 | `close_span` called twice on the same span fires `on_span_end` exactly once | Unit test |
| AC-06 | `begin_trace` fires `on_trace_start` with `Trace.finished_at == None` | Unit test |
| AC-07 | `end_trace` fires `on_trace_end` with `Trace.finished_at` set, `Trace.span_count == len(spans)`, `Trace.status == 'error'` if any child span has `status='error'` | Unit test |
| AC-08 | `tag hooks trace add --processor tag.tracing.LoggingTraceProcessor` exits 0 and writes one row to `trace_processors` | Integration test |
| AC-09 | `tag hooks trace list` displays the registered processor with all columns | Integration test |
| AC-10 | `tag hooks trace remove <id>` removes the row; subsequent `list` shows empty | Integration test |
| AC-11 | Registering the same `--processor` and `--profile` twice fails with a clear "already registered" error on the second attempt | Integration test |
| AC-12 | `tag hooks trace test --processor tag.tracing.LoggingTraceProcessor` exits 0 and prints per-method timing | Integration test |
| AC-13 | `tag hooks trace test --processor no.such.Module.Processor` exits 1 | Integration test |
| AC-14 | `_load_trace_processors` with a processor whose module cannot be imported: agent run proceeds normally, `last_error` column is updated, warning is logged | Integration test with mocked `importlib` |
| AC-15 | `_load_trace_processors` with `profile='coder'`: a processor registered with `profile='researcher'` is NOT loaded; a processor with `profile=NULL` IS loaded | Integration test |
| AC-16 | `CompositeTraceProcessor` with 0 processors: 10,000 `on_span_end` calls complete in < 10 ms | Performance test |
| AC-17 | `CompositeTraceProcessor` with 3 no-op processors: 1,000 `on_span_end` calls complete in < 5 ms | Performance test |
| AC-18 | `tag hooks trace add --config '<5001-byte JSON>'` exits 1 with a size-limit error | Integration test |
| AC-19 | `tag hooks trace disable <id>` sets `enabled=0`; subsequent `_load_trace_processors` does not instantiate that processor | Integration test |
| AC-20 | `tag.tracing` is importable with zero third-party dependencies (stdlib only) | CI: `pip install --no-deps tag && python -c "import tag.tracing"` |

---

## 14. Dependencies

| Dependency | Type | Notes |
|-----------|------|-------|
| PRD-013 (agent tracing/observability) | Hard prerequisite | `Span` dataclass, `open_span`, `close_span`, SQLite `spans` table, and `Tracer` context manager must exist. This PRD extends those symbols. |
| PRD-016 (webhook event triggers) | Soft reference | The `_fire_hooks` pattern in PRD-016 is the closest existing analogue to processor dispatch. No hard code dependency. |
| PRD-027 (eval framework) | Future integration | An `EvalTraceProcessor` that triggers eval scoring on `on_trace_end` is a natural extension, documented as a migration candidate. |
| PRD-028 (sandbox code execution) | Informational | Processor code runs outside the sandbox. Operators running sandboxed agents should be aware that processors escape the sandbox boundary. |
| PRD-034 (security hardening) | Informational | `open_db()` file permission model (0600) is assumed. Processor registration adds a new code-execution surface that the security review should consider. |
| PRD-040 (notification hooks) | Soft reference | Similar CLI pattern (`tag hooks notify add/list/remove`). The `tag hooks trace` subcommand tree mirrors this naming convention for consistency. |
| PRD-041 (OTel GenAI cost attribution) | Migration candidate | `OtelCostAttributionProcessor` can replace the inline cost-mapping logic in `controller.py` in a future sprint. |
| PRD-044 (AgentOps session observability) | Migration candidate | `AgentOpsTraceProcessor` can replace the inline AgentOps session calls in a future sprint. |
| Python `importlib` | Stdlib | Used for deferred processor loading. No third-party packages required. |
| Python `typing.Protocol` | Stdlib (3.8+) | Used for structural subtyping. TAG already requires Python ≥ 3.10. |

---

## 15. Open Questions

| # | Question | Owner | Target Resolution |
|---|----------|-------|------------------|
| OQ-1 | Should `on_span_start` and `on_span_end` be called for ALL spans (including internal diagnostic spans) or only for spans belonging to active user-initiated runs? Calling for all spans simplifies the implementation but may overwhelm processors during high-frequency internal bookkeeping. | Eng lead | Before implementation starts |
| OQ-2 | Should `CompositeTraceProcessor` automatically disable a processor (set `enabled=0` in DB) after N consecutive failures, to prevent a broken processor from generating log noise on every span? A threshold of 10 consecutive failures is a reasonable candidate. | Eng + Ops | Sprint planning |
| OQ-3 | Should `tag hooks trace add` perform a test-run (equivalent to `tag hooks trace test`) automatically before committing to DB, rather than registering-and-warning? This would give immediate feedback at the cost of requiring the module to be importable at registration time (vs. deferred import at run time). | UX | Before implementation starts |
| OQ-4 | The `UNIQUE (processor, profile)` SQLite constraint has subtle NULL semantics. Should TAG use a sentinel value (e.g. empty string `''`) for "all profiles" instead of `NULL` to get predictable duplicate detection? This changes the `--profile` filter queries. | Eng | Before DDL is committed |
| OQ-5 | Should there be a maximum number of registered processors (e.g. 20)? Unbounded registration could create meaningful startup overhead if each processor does expensive initialization. | Eng | During implementation |
| OQ-6 | Should processor registration be profile-config-scoped (stored in the profile YAML) rather than global in SQLite? Profile YAML scoping would enable per-project processor sets checked into version control, but would diverge from the global `tag hooks` pattern established by PRD-040. | Product | Before implementation starts |
| OQ-7 | Should `tag hooks trace list` display a `last_ok_at` column (timestamp of last successful callback) alongside `last_error_at`, to give operators a sense of how recently a processor was working? Requires an additional DB column. | UX | Nice-to-have, defer to v1.1 |
| OQ-8 | `on_span_start` fires with `finished_at=None` and `duration_ms=None`. Some processors (e.g. a streaming dashboard) need the span ID to later correlate with `on_span_end`. Should the `Span` be deep-copied before dispatch to prevent processors from accidentally holding a reference that mutates? | Eng | Security review |

---

## 16. Complexity and Timeline

**Overall estimate:** S — 3 to 5 engineering days.

### Phase 1: Core protocol (Day 1)

- Add `Trace` dataclass to `tracing.py`.
- Add `TraceProcessor` protocol and `CompositeTraceProcessor` to `tracing.py`.
- Add `LoggingTraceProcessor` built-in implementation.
- Add `_global_composite`, `register_processor`, `unregister_processor`.
- Patch `open_span` and `close_span` to call `_global_composite`.
- Add `begin_trace` and `end_trace` functions.
- Unit tests for protocol, composite isolation, idempotency (AC-01 through AC-07, AC-16, AC-17).

### Phase 2: SQLite persistence and CLI (Day 2)

- Add `trace_processors` DDL and migration to `open_db()`.
- Implement `cmd_hooks_trace_add`, `cmd_hooks_trace_list`, `cmd_hooks_trace_remove`, `cmd_hooks_trace_enable`, `cmd_hooks_trace_disable` in `controller.py`.
- Wire `tag hooks trace` subcommand tree into the main argument parser.
- Integration tests for CLI round-trip (AC-08 through AC-15, AC-18, AC-19).

### Phase 3: Startup loader and test command (Day 3)

- Implement `_load_trace_processors` helper in `controller.py`.
- Call `_load_trace_processors` in the agent run initialisation path.
- Implement `tag hooks trace test` subcommand.
- Integration tests for loader with mocked `importlib` (AC-14, AC-15), test command (AC-12, AC-13).

### Phase 4: Polish and docs (Day 4 — optional, if time permits)

- Update `Tracer` context manager to call `begin_trace` / `end_trace`.
- Add `tag hooks trace` to the `tag hooks` help output alongside `tag hooks notify`.
- Add `LoggingTraceProcessor` to the `tag hooks trace test` default processor for smoke testing.
- Verify AC-20 (zero third-party dependency import check) in CI.
- Address OQ-1 (span scope) and OQ-3 (auto-test at add time) based on resolution of open questions.

### Phase 5: Documentation and migration notes (Day 5 — optional)

- Document the TraceProcessor protocol contract in `tracing.py` module docstring.
- Write migration guides for PRD-041 and PRD-044 as future candidates.
- Update `docs/prd/INDEX.md` with PRD-053 entry.
