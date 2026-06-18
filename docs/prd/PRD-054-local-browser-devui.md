# PRD-054: Local Browser-Based Agent Execution Visualizer (`tag devui`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** L (2-4 weeks)
**Category:** Evaluation & Observability
**Affects:** `devui.py + web/devui/`
**Depends on:** PRD-013 (agent tracing/observability — `spans` table), PRD-012 (cost tracking — `runs` cost columns), PRD-028 (sandbox — `sandbox_runs` table), PRD-027 (eval framework — `eval_runs` / `eval_cases` tables), PRD-034 (secret scanning — read-only data source), PRD-041 (OTel GenAI semconv — attribute naming), PRD-032 (trace snapshots — memory state reconstruction)
**Inspired by:** MAF DevUI, LangSmith trace viewer, LangGraph Studio

---

## 1. Overview

Debugging a multi-step agent run in TAG today requires cross-referencing at least three data sources: `tag trace <id>` in the terminal, `tag runs list` for status metadata, and raw SQLite queries for token counts and tool call arguments. Even skilled users spend 10–20 minutes reconstructing what happened in a failed 30-step run. The cognitive load of piecing together an execution story from fragmented CLI output is the single largest friction point in the TAG observability workflow.

`tag devui` eliminates that friction by launching a local HTTP server that serves a single-page browser application rendering a complete, interactive execution graph from the SQLite database already maintained at `~/.tag/runtime/tag.sqlite3`. The browser UI displays four complementary views of any agent run: a directed-acyclic graph of spans showing parent–child relationships, a flame chart (Gantt-style timeline) of span durations, a token cost breakdown panel, and a memory/attribute inspector that shows per-span attributes including tool call arguments, output tokens, and error messages. No cloud account, no telemetry export, no external dependency is required — all data originates from `open_db()`.

The design philosophy is borrowed from LangSmith's trace viewer (structured span hierarchy with clickable drill-down) and LangGraph Studio's node graph (visual layout of the agent's decision graph), but with a zero-infrastructure implementation constraint: the server is a plain Python `http.server.HTTPServer` serving pre-built static assets bundled into `web/devui/`, and the data API is a handful of JSON endpoints implemented in `devui.py` that query SQLite directly. The frontend is compiled to static HTML/CSS/JS so `pip install tag` carries the full UI; no Node.js runtime is required by end users.

For teams building on TAG, `tag devui export --run-id <id> --format html` produces a self-contained offline HTML file (all data inlined as JSON, all rendering JS inlined) that can be emailed, attached to a GitHub issue, or stored alongside a CI artifact. This export path is the primary mechanism for sharing execution traces with collaborators who do not have TAG installed.

A secondary invocation mode, `tag devui start --run-id <id>`, opens the browser directly to a specific run — useful when wired into `tag submit`'s `--on-complete` hook or triggered from `tag notify` webhook payloads, enabling post-run inspection without navigating a run list.

---

## 2. Problem Statement

### 2.1 Terminal flame charts are insufficient for multi-step debugging

`tag trace <run_id>` from PRD-013 renders a text-mode flame chart using Unicode box-drawing characters. For runs with fewer than 15 spans this is readable. For a typical `tag swarm` run with 60–120 spans across 4 profiles, the terminal output is 300+ lines, requires horizontal scrolling, and provides no interactivity — clicking on a span to see its attributes is not possible. Users pipe the output to a file and use `grep`, which defeats the purpose of a structured trace.

### 2.2 Cost attribution requires manual computation

PRD-012 records `prompt_tokens`, `completion_tokens`, `cache_read_tokens`, and `cache_creation_tokens` on the `runs` table, and per-span token counts on `spans`. Converting these to dollar figures requires knowing the per-model pricing, computing `(input_tokens * in_price) + (output_tokens * out_price)` with cache multipliers (cache reads at 0.1× input price for Anthropic models), and summing across all spans. Nothing in the current CLI does this automatically at the span level — `tag costs` operates at the run level. A UI that renders a color-coded cost-per-span breakdown within the flame chart is immediately actionable for identifying expensive steps.

### 2.3 Memory and attribute state is opaque across steps

The `spans.attributes` column stores a JSON blob of all OpenInference-namespaced attributes (tool names, tool arguments, tool output, LLM input messages, LLM output messages). Extracting the arguments passed to a tool call in step 7 of a 20-step run requires: `sqlite3 ~/.tag/runtime/tag.sqlite3 "SELECT attributes FROM spans WHERE trace_id='...' AND name LIKE '%tool_call%' LIMIT 20"` and then manually parsing the JSON. This is expert-only workflow. A UI that shows the attribute JSON in a formatted tree viewer for each selected span reduces the skill floor significantly.

### 2.4 No shareable execution trace format

When a TAG-based agent fails in a customer's environment, the current debugging workflow is: ask the customer to paste `tag trace <id>` output into a Slack message or GitHub issue. This output is often truncated by chat clients, loses formatting, and contains no interactive drill-down. A self-contained HTML export (analogous to LangSmith's shareable trace links, but offline-first) solves this for users who cannot or will not share database access.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | `tag devui start` launches a local HTTP server within 2 seconds and opens the system browser to the run list page. |
| G2 | The run list page renders all runs from the `runs` table with profile, status, duration, total tokens, and estimated USD cost. |
| G3 | The run detail page renders a flame chart of all spans for that run, with horizontal time axis scaled to real duration. |
| G4 | The run detail page renders a DAG view of span parent–child relationships, with profile-colored nodes and edge labels showing handoffs. |
| G5 | Clicking any span node/bar in either view opens a side panel showing all attributes, token counts, and the full error message if status is `error`. |
| G6 | The cost panel shows per-span cost estimates computed from bundled model pricing data (USD, using PRD-012's formula), totaled and broken down by profile and model. |
| G7 | `tag devui start --run-id <id>` opens the browser directly to the detail page for that specific run. |
| G8 | `tag devui export --run-id <id> --format html` writes a self-contained HTML file with all data inlined; no network requests at render time. |
| G9 | The server reads data exclusively from the local SQLite file via `open_db()`; no external API calls, no cloud dependency. |
| G10 | All SQL queries are read-only (`SELECT` only); the server never issues `INSERT`, `UPDATE`, or `DELETE`. |
| G11 | The server binds only to `127.0.0.1` by default; no LAN exposure without explicit `--host 0.0.0.0` flag. |
| G12 | Live-reload: when `--watch` flag is set, the run list and detail pages auto-refresh every 5 seconds via a `GET /api/v1/poll` endpoint that returns the latest run status. |

## 3.1 Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Replacing `tag trace` terminal output. The terminal flame chart (PRD-013) remains the zero-dependency fallback; `devui` is a separate, additive command. |
| NG2 | Writing data back to SQLite. The devui server is strictly read-only. Annotation, labeling, and feedback features are deferred to a future PRD (annotation queue, PRD cluster B). |
| NG3 | Serving the UI over a public hostname or HTTPS. SSL termination, authentication, and public exposure are explicitly out of scope for v1. |
| NG4 | Rendering LLM conversation message bodies as chat UI. The attribute panel shows raw message text; a polished chat interface is out of scope. |
| NG5 | Comparing two runs side-by-side in the same view. Per-run drill-down is the only supported view for v1; cross-run comparison belongs in PRD-017 (multi-model benchmarking). |
| NG6 | Streaming spans in real time during an active run. The API serves data already committed to SQLite; in-progress spans require `--watch` polling, not a WebSocket push. |
| NG7 | Packaging the frontend build toolchain (Vite, esbuild, etc.) as a TAG dependency. The compiled static assets are committed to the repository and distributed in the package. |
| NG8 | Supporting eval metric drill-down from `eval_runs`/`eval_cases`. A basic link to eval results is included; deep eval UI is deferred to a follow-on cluster. |

---

## 4. Success Metrics

| Metric | Target | How Measured |
|--------|--------|--------------|
| Time-to-first-render | Run list page renders in < 1 second for up to 500 runs | Automated playwright test measuring time from `GET /` response to `DOMContentLoaded` |
| Span render scale | Flame chart renders 500 spans without layout jank (< 100ms layout time) | Synthetic dataset, `performance.now()` before and after layout |
| Export file size | Self-contained HTML export for a 200-span run is < 2 MB | File size assertion in integration test |
| Server startup latency | `tag devui start` prints "Listening on http://127.0.0.1:3000" in < 2 seconds on cold start | CI timing in integration test |
| Read-only guarantee | No `INSERT`/`UPDATE`/`DELETE` statement is ever issued against the database | Static analysis + runtime SQL hook in test mode |
| LAN bind rejection | Default start without `--host` refuses connections from any IP other than 127.0.0.1 | Integration test with socket connection from 0.0.0.0 |
| Export offline fidelity | Exported HTML renders identical flame chart to live UI when opened without network | Playwright snapshot comparison of live vs. offline HTML |
| PRD-013 span compatibility | All columns of the existing `spans` table are rendered without schema migration | Integration test against production tag.sqlite3 snapshot |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer debugging a failed swarm run | run `tag devui start --run-id run-abc123` and see a flame chart in my browser | I can click each span to read the tool arguments and error message without writing SQLite queries |
| U2 | Developer optimizing agent cost | open the cost panel and see cost-per-span bars sorted by USD descending | I immediately know which tool call or inference step is the most expensive and can optimize it |
| U3 | Team lead reviewing an agent run | run `tag devui export --run-id run-abc123 --format html` and attach the file to a GitHub issue | My colleague can open the HTML offline and see the full execution trace with no TAG installation |
| U4 | Platform engineer monitoring a long run | run `tag devui start --watch` and leave it open in a browser tab | The page auto-refreshes every 5 seconds so I can see the run progressing without hitting reload manually |
| U5 | Developer with no browser | use `tag trace <id>` as before | The terminal flame chart from PRD-013 is unaffected and continues to work as the zero-dependency fallback |
| U6 | Security-conscious developer | confirm that `tag devui start` binds only to 127.0.0.1 | My local agent traces are not exposed to other machines on my LAN |
| U7 | Developer debugging a memory regression | click a span in the DAG view and see the full `memory_journal` snapshot at that step | I can understand exactly what semantic memory the agent had available when it made a particular decision |
| U8 | CI engineer generating run reports | run `tag devui export --run-id $RUN_ID --format html --output artifacts/trace.html` in a CI job | The HTML trace file is uploaded as a CI artifact alongside test results for later debugging |
| U9 | Developer debugging eval regressions | see an eval score badge on the run list next to runs that have associated `eval_runs` records | I can correlate execution quality (span-level) with evaluation scores (eval_runs-level) in one view |
| U10 | Developer on a slow machine | run `tag devui start` and have it work even if the DB has 10,000 spans | Pagination and lazy-loading ensure the UI stays responsive even for large historical databases |

---

## 6. Proposed CLI Surface

All `devui` subcommands live under the `tag devui` namespace. The entry point is `cmd_devui(args, cfg)` in `controller.py`, dispatching to `devui.py`.

### 6.1 `tag devui start`

Launch the local HTTP server and open the browser.

```
tag devui start
    [--port PORT]           # TCP port (default: 3000)
    [--host HOST]           # Bind address (default: 127.0.0.1)
    [--run-id RUN_ID]       # Open browser directly to run detail page
    [--watch]               # Enable auto-refresh polling (5s interval)
    [--no-open]             # Start server but do not open browser
    [--db PATH]             # Override SQLite path (default: ~/.tag/runtime/tag.sqlite3)
    [--timeout SECONDS]     # Server auto-shutdown after N seconds of inactivity (default: 0 = disabled)
```

**Example invocations:**

```bash
# Start with defaults, opens http://127.0.0.1:3000 in system browser
tag devui start

# Start on port 8080, jump directly to run run-abc123
tag devui start --port 8080 --run-id run-abc123

# Start and auto-refresh; useful during active agent runs
tag devui start --watch

# Headless server mode (useful for remote SSH dev environments)
tag devui start --no-open --port 3000

# Use an alternative DB (e.g., staging environment)
tag devui start --db /tmp/staging-tag.sqlite3
```

**Terminal output:**

```
TAG DevUI starting...
  Database : /Users/user/.tag/runtime/tag.sqlite3
  Runs     : 47 runs (3 active)
  Spans    : 8,234 spans across all runs

Listening on http://127.0.0.1:3000
Opening browser...
Press Ctrl+C to stop.
```

### 6.2 `tag devui export`

Generate a self-contained offline HTML (or JSON) report for a specific run.

```
tag devui export
    --run-id RUN_ID         # Required: the run to export
    [--format FORMAT]       # html (default) | json
    [--output PATH]         # Output file path (default: tag-run-<id>.<format>)
    [--db PATH]             # Override SQLite path
    [--open]                # Open exported HTML in browser after writing
```

**Example invocations:**

```bash
# Export as self-contained HTML (default)
tag devui export --run-id run-abc123

# Export as JSON (useful for programmatic consumption or custom renderers)
tag devui export --run-id run-abc123 --format json --output traces/run-abc123.json

# Export and immediately open in browser
tag devui export --run-id run-abc123 --open

# CI artifact generation
tag devui export --run-id $TAG_RUN_ID --output artifacts/devui-trace.html
```

**Terminal output (HTML format):**

```
Exporting run run-abc123...
  Spans    : 87 spans
  Steps    : 14 conversation steps
  Duration : 4m 32s
  Cost     : $0.0234 (estimated)

Written: tag-run-abc123.html (1.4 MB, self-contained)
```

### 6.3 `tag devui stop`

Stop a running devui server (uses PID file at `~/.tag/runtime/devui.pid`).

```
tag devui stop
    [--port PORT]           # Match specific port if multiple servers running
```

### 6.4 `tag devui status`

Check if a devui server is currently running.

```
tag devui status
```

**Output:**

```
DevUI server: RUNNING
  PID  : 54321
  Port : 3000
  URL  : http://127.0.0.1:3000
  DB   : /Users/user/.tag/runtime/tag.sqlite3
```

---

## 7. REST API Endpoints

The embedded server exposes a minimal JSON API consumed by the frontend. All endpoints are `GET`-only.

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/` | Serve `index.html` (run list page) |
| `GET` | `/run/:id` | Serve `index.html` (SPA handles routing) |
| `GET` | `/api/v1/runs` | Paginated run list with cost totals |
| `GET` | `/api/v1/runs/:id` | Run metadata + all spans for that run |
| `GET` | `/api/v1/runs/:id/spans` | Spans only (paginated, for large runs) |
| `GET` | `/api/v1/runs/:id/steps` | Conversation steps from `steps` table |
| `GET` | `/api/v1/runs/:id/memory` | `memory_journal` rows active at run time |
| `GET` | `/api/v1/runs/:id/cost` | Per-span cost breakdown JSON |
| `GET` | `/api/v1/poll` | Latest run statuses (used by `--watch` polling) |
| `GET` | `/api/v1/health` | `{"status":"ok","db":"connected"}` |
| `GET` | `/static/*` | Bundled static assets (JS, CSS, fonts) |

**Query parameters for `/api/v1/runs`:**

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `page` | int | 1 | Page number (1-indexed) |
| `per_page` | int | 50 | Rows per page (max 200) |
| `profile` | str | — | Filter by profile name |
| `status` | str | — | Filter by status (`ok`, `error`, `running`) |
| `since` | ISO-8601 | — | Return only runs created after this timestamp |

---

## 8. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `tag devui start` MUST launch a Python `HTTPServer` bound to `127.0.0.1:<port>` and open the system browser via `webbrowser.open()` within 2 seconds. | Must |
| FR-02 | The server MUST refuse to bind to any non-loopback address unless `--host` is explicitly passed. Passing `--host 0.0.0.0` MUST print a security warning before binding. | Must |
| FR-03 | `GET /api/v1/runs` MUST return a JSON array of run objects ordered by `created_at DESC`, including: `id`, `created_at`, `status`, `master_profile`, `prompt` (truncated to 120 chars), `total_prompt_tokens`, `total_completion_tokens`, `estimated_cost_usd`. | Must |
| FR-04 | `GET /api/v1/runs/:id` MUST return all columns from `runs` plus the full `spans` array for that `trace_id`, each span including all columns from the `spans` table plus a computed `cost_usd` field. | Must |
| FR-05 | Cost computation MUST use the formula `(prompt_tokens * in_price) + (completion_tokens * out_price) + (cache_read_tokens * in_price * 0.1)`, with pricing loaded from a bundled `web/devui/pricing.json` file keyed by `model_id`. | Must |
| FR-06 | The flame chart view MUST render each span as a horizontal bar whose left edge is `(span.started_at - run.created_at)` ms from the left margin and whose width is `span.duration_ms` pixels at the configured px/ms scale. | Must |
| FR-07 | The DAG view MUST render spans as nodes in a directed graph where edges represent `parent_id → id` relationships. Nodes MUST be color-coded by `profile`. | Must |
| FR-08 | Clicking any span in either view MUST open a details side panel showing: `name`, `status`, `profile`, `model_id`, `duration_ms`, `prompt_tokens`, `completion_tokens`, `cost_usd`, `error_msg` (if set), and the `attributes` JSON rendered as a collapsible tree. | Must |
| FR-09 | `tag devui export --format html` MUST produce a single `.html` file with all span data inlined as a `<script type="application/json" id="__TAG_DATA__">` block and all rendering JavaScript inlined as `<script>` tags. The file MUST render correctly when opened via `file://` URI with no network access. | Must |
| FR-10 | `tag devui export --format json` MUST produce a JSON file with the same structure as `GET /api/v1/runs/:id` response. | Must |
| FR-11 | When `--watch` is passed, the frontend MUST poll `GET /api/v1/poll` every 5 seconds and refresh the active run's spans if the `last_updated` timestamp has changed. | Must |
| FR-12 | The server MUST write its PID and port to `~/.tag/runtime/devui.pid` on startup and delete it on graceful shutdown (SIGINT/SIGTERM). `tag devui stop` reads this file to send SIGTERM. | Must |
| FR-13 | All SQL issued by `devui.py` MUST be read-only `SELECT` statements. A test-mode hook MUST verify this by intercepting `conn.execute()` and asserting no `INSERT`/`UPDATE`/`DELETE` keywords appear. | Must |
| FR-14 | `GET /api/v1/runs/:id/memory` MUST return `memory_journal` rows where `profile = run.master_profile` ordered by `created_at`. | Should |
| FR-15 | The run list MUST show a badge "eval: 0.82" next to any run that has a corresponding `eval_runs` record with `suite_name` and average score from `eval_cases`. | Should |
| FR-16 | The flame chart MUST support horizontal zoom (scroll-wheel or pinch gesture) and horizontal pan (click-drag). | Should |
| FR-17 | The DAG view MUST support zoom and pan. Nodes with more than 20 children MUST be collapsible. | Should |
| FR-18 | `tag devui start --run-id <id>` MUST cause the browser to open to `/run/<id>` instead of `/`. | Must |
| FR-19 | The server MUST set `Content-Security-Policy: default-src 'self'` on all responses to block exfiltration of rendered data via injected scripts. | Must |
| FR-20 | Pagination: `GET /api/v1/runs/:id/spans?page=2&per_page=100` MUST be supported for runs with > 200 spans. The run detail page MUST lazy-load subsequent pages as the user scrolls the flame chart. | Should |
| FR-21 | `tag devui start` with `--timeout 3600` MUST auto-shutdown the server after 3600 seconds of inactivity (no `GET /api/v1/*` request received), printing a shutdown message to the terminal. | Could |
| FR-22 | The server MUST handle concurrent requests correctly using `ThreadingHTTPServer` (Python `http.server.ThreadingHTTPServer`). | Must |
| FR-23 | `tag devui status` MUST read `~/.tag/runtime/devui.pid`, verify the process is alive via `os.kill(pid, 0)`, and print status. If the PID file exists but the process is dead (stale PID), it MUST print "DevUI server: STOPPED (stale PID file removed)" and delete the file. | Must |
| FR-24 | All span `attributes` JSON values MUST be rendered as an expandable tree (collapsed by default for objects with > 5 keys). | Should |
| FR-25 | Sandbox runs from the `sandbox_runs` table associated with a run MUST be shown in the span detail panel when the span name contains `sandbox_run`. | Could |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Server startup time (cold start, empty DB) | < 2 seconds |
| NFR-02 | Run list API response time (`/api/v1/runs`, 500 runs) | < 200ms |
| NFR-03 | Run detail API response time (`/api/v1/runs/:id`, 500 spans) | < 500ms |
| NFR-04 | Flame chart render time (200 spans, 1920px viewport) | < 100ms in browser (measured via `performance.now()`) |
| NFR-05 | Exported HTML file size (200 spans) | < 2 MB |
| NFR-06 | Memory footprint of Python server process | < 50 MB RSS |
| NFR-07 | Python version compatibility | Python 3.10+ (same as TAG CLI) |
| NFR-08 | Zero mandatory new pip dependencies | `devui.py` uses only Python stdlib (`http.server`, `json`, `sqlite3`, `threading`, `webbrowser`, `signal`) and existing TAG modules |
| NFR-09 | Frontend bundle size | < 500 KB gzipped (all JS + CSS) |
| NFR-10 | Concurrent request handling | `ThreadingHTTPServer` with thread pool; supports ≥ 10 concurrent connections |
| NFR-11 | DB read-only guarantee | Verified by static analysis + runtime assertion in test mode |
| NFR-12 | Browser compatibility | Chrome 110+, Firefox 115+, Safari 16+, Edge 110+ |
| NFR-13 | CORS headers | `Access-Control-Allow-Origin: 127.0.0.1:<port>` only; wildcard origin rejected |
| NFR-14 | Graceful shutdown | SIGINT / SIGTERM trigger cleanup within 1 second; PID file deleted |
| NFR-15 | Log output | Server request logs go to `~/.tag/runtime/devui.log` (not stdout); only startup/shutdown messages go to stdout |
| NFR-16 | Content-Security-Policy | `default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'` (inline required for bundled assets) |

---

## 10. Technical Design

### 10.1 New Files

| File | Purpose |
|------|---------|
| `src/tag/devui.py` | Python HTTP server, API handlers, export logic, `cmd_devui` dispatch |
| `web/devui/index.html` | SPA shell loaded for all routes |
| `web/devui/bundle.js` | Compiled frontend JavaScript (runs, DAG, flame chart, cost panel) |
| `web/devui/bundle.css` | Compiled frontend CSS |
| `web/devui/pricing.json` | Bundled model pricing data (see §10.5) |
| `web/devui/src/` | Frontend source (TypeScript + D3.js; build artifacts committed) |
| `web/devui/src/RunList.ts` | Run list page component |
| `web/devui/src/RunDetail.ts` | Run detail page (flame chart + DAG + cost + memory panels) |
| `web/devui/src/FlameChart.ts` | D3-based flame chart renderer |
| `web/devui/src/DagView.ts` | D3-based DAG renderer |
| `web/devui/src/CostPanel.ts` | Cost breakdown component |
| `web/devui/src/AttributeTree.ts` | JSON tree inspector for span attributes |
| `web/devui/src/api.ts` | Typed fetch wrappers for `/api/v1/*` |
| `tests/test_devui.py` | Unit and integration tests |

### 10.2 SQLite DDL — New Tables

PRD-054 introduces one new table: `devui_export_log`. All other data is read from existing tables (`runs`, `steps`, `spans`, `memory_journal`, `eval_runs`, `eval_cases`, `sandbox_runs`).

```sql
-- Track exports for audit and deduplication
CREATE TABLE IF NOT EXISTS devui_export_log (
    id          TEXT PRIMARY KEY,          -- UUID4
    run_id      TEXT NOT NULL,             -- FK to runs.id
    format      TEXT NOT NULL DEFAULT 'html',  -- 'html' | 'json'
    output_path TEXT NOT NULL,
    file_size   INTEGER NOT NULL DEFAULT 0,    -- bytes
    span_count  INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_del_run ON devui_export_log(run_id, created_at);
```

No DDL migration is required for existing tables. `devui.py` reads but never writes to `runs`, `steps`, `spans`, `memory_journal`, `eval_runs`, `eval_cases`, and `sandbox_runs`.

### 10.3 Core Dataclasses

```python
# src/tag/devui.py

from __future__ import annotations

import json
import os
import signal
import sqlite3
import threading
import time
import uuid
import webbrowser
from dataclasses import asdict, dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, urlparse


@dataclass
class SpanView:
    """Span row enriched with computed cost_usd for API responses."""
    id: str
    trace_id: str
    parent_id: str | None
    name: str
    profile: str | None
    model_id: str | None
    started_at: str
    finished_at: str | None
    duration_ms: int | None
    status: str
    prompt_tokens: int
    completion_tokens: int
    attributes: dict[str, Any]
    error_msg: str | None
    cost_usd: float = 0.0  # computed, not stored

    @classmethod
    def from_row(cls, row: sqlite3.Row, pricing: dict[str, ModelPricing]) -> SpanView:
        attrs = json.loads(row["attributes"] or "{}")
        model_id = row["model_id"] or ""
        cost = _compute_span_cost(
            prompt_tokens=row["prompt_tokens"],
            completion_tokens=row["completion_tokens"],
            model_id=model_id,
            pricing=pricing,
        )
        return cls(
            id=row["id"],
            trace_id=row["trace_id"],
            parent_id=row["parent_id"],
            name=row["name"],
            profile=row["profile"],
            model_id=model_id or None,
            started_at=row["started_at"],
            finished_at=row["finished_at"],
            duration_ms=row["duration_ms"],
            status=row["status"],
            prompt_tokens=row["prompt_tokens"],
            completion_tokens=row["completion_tokens"],
            attributes=attrs,
            error_msg=row["error_msg"],
            cost_usd=cost,
        )


@dataclass
class ModelPricing:
    """Per-model pricing in USD per 1M tokens."""
    model_id: str
    input_price_per_1m: float     # USD per 1M input tokens
    output_price_per_1m: float    # USD per 1M output tokens
    cache_read_multiplier: float = 0.1   # Anthropic/OpenAI: 0.1×
    cache_write_multiplier: float = 1.25  # Anthropic prompt cache write: 1.25×


@dataclass
class RunSummary:
    """Lightweight run row for the run list page."""
    id: str
    created_at: str
    status: str
    master_profile: str
    prompt_preview: str           # first 120 chars of prompt
    kind: str
    execution: str
    total_prompt_tokens: int = 0
    total_completion_tokens: int = 0
    estimated_cost_usd: float = 0.0
    eval_score: float | None = None
    eval_suite: str | None = None
    span_count: int = 0


@dataclass
class RunDetail:
    """Full run data for the detail page."""
    run: dict[str, Any]           # all columns from runs table
    steps: list[dict[str, Any]]   # from steps table
    spans: list[SpanView]         # all spans for this trace_id
    memory: list[dict[str, Any]]  # memory_journal rows
    cost_breakdown: CostBreakdown
    eval_result: dict[str, Any] | None = None


@dataclass
class CostBreakdown:
    """Aggregated cost data for a run's cost panel."""
    total_usd: float
    by_profile: dict[str, float]   # profile_name → USD
    by_model: dict[str, float]     # model_id → USD
    by_span: list[tuple[str, float]]  # [(span_id, cost_usd)] sorted desc
    input_tokens: int
    output_tokens: int
    cache_read_tokens: int
    cache_write_tokens: int


@dataclass
class DevUIServerConfig:
    """Runtime config for the HTTP server."""
    host: str = "127.0.0.1"
    port: int = 3000
    db_path: Path = field(default_factory=lambda: Path.home() / ".tag/runtime/tag.sqlite3")
    watch: bool = False
    watch_interval_seconds: int = 5
    timeout_seconds: int = 0          # 0 = disabled
    static_dir: Path = field(default_factory=lambda: Path(__file__).parent.parent.parent / "web/devui")
    pid_file: Path = field(default_factory=lambda: Path.home() / ".tag/runtime/devui.pid")
    log_file: Path = field(default_factory=lambda: Path.home() / ".tag/runtime/devui.log")
```

### 10.4 Cost Computation Algorithm

The cost formula follows PRD-012 and the cluster research:

```python
def _load_pricing(static_dir: Path) -> dict[str, ModelPricing]:
    """Load model pricing from web/devui/pricing.json."""
    pricing_path = static_dir / "pricing.json"
    if not pricing_path.exists():
        return {}
    raw = json.loads(pricing_path.read_text())
    return {
        k: ModelPricing(
            model_id=k,
            input_price_per_1m=v["input_price_per_1m"],
            output_price_per_1m=v["output_price_per_1m"],
            cache_read_multiplier=v.get("cache_read_multiplier", 0.1),
            cache_write_multiplier=v.get("cache_write_multiplier", 1.25),
        )
        for k, v in raw.items()
    }


def _compute_span_cost(
    prompt_tokens: int,
    completion_tokens: int,
    model_id: str,
    pricing: dict[str, ModelPricing],
    cache_read_tokens: int = 0,
    cache_write_tokens: int = 0,
) -> float:
    """Compute estimated USD cost for a span.

    Formula (from cluster research §4 and PRD-012):
        cost = (input_tokens * in_price / 1M)
             + (output_tokens * out_price / 1M)
             + (cache_read_tokens * in_price * cache_read_multiplier / 1M)
             + (cache_write_tokens * in_price * cache_write_multiplier / 1M)

    Cache reads are 0.1× input price for Anthropic models.
    Returns 0.0 if no pricing data is available for the model.
    """
    p = pricing.get(model_id)
    if p is None:
        # Try prefix match (e.g., "claude-sonnet-4-6" → "claude-sonnet-4")
        for key in pricing:
            if model_id.startswith(key):
                p = pricing[key]
                break
    if p is None:
        return 0.0

    return (
        (prompt_tokens * p.input_price_per_1m / 1_000_000)
        + (completion_tokens * p.output_price_per_1m / 1_000_000)
        + (cache_read_tokens * p.input_price_per_1m * p.cache_read_multiplier / 1_000_000)
        + (cache_write_tokens * p.input_price_per_1m * p.cache_write_multiplier / 1_000_000)
    )
```

### 10.5 `pricing.json` Schema

Stored at `web/devui/pricing.json`. Seeded from Anthropic/OpenRouter list prices at the time of each TAG release. Can be overridden by placing a custom file at `~/.tag/devui-pricing.json`.

```json
{
  "claude-sonnet-4-6": {
    "input_price_per_1m": 3.00,
    "output_price_per_1m": 15.00,
    "cache_read_multiplier": 0.1,
    "cache_write_multiplier": 1.25
  },
  "claude-opus-4": {
    "input_price_per_1m": 15.00,
    "output_price_per_1m": 75.00,
    "cache_read_multiplier": 0.1,
    "cache_write_multiplier": 1.25
  },
  "claude-haiku-3-5": {
    "input_price_per_1m": 0.80,
    "output_price_per_1m": 4.00,
    "cache_read_multiplier": 0.1,
    "cache_write_multiplier": 1.25
  },
  "gpt-4o": {
    "input_price_per_1m": 2.50,
    "output_price_per_1m": 10.00,
    "cache_read_multiplier": 0.5,
    "cache_write_multiplier": 1.0
  },
  "gpt-4o-mini": {
    "input_price_per_1m": 0.15,
    "output_price_per_1m": 0.60,
    "cache_read_multiplier": 0.5,
    "cache_write_multiplier": 1.0
  }
}
```

### 10.6 HTTP Server Architecture

```python
class DevUIHandler(BaseHTTPRequestHandler):
    """HTTP request handler for the DevUI server."""

    server_config: DevUIServerConfig
    pricing: dict[str, ModelPricing]
    _db_lock: threading.Lock

    def do_GET(self) -> None:
        parsed = urlparse(self.path)
        path = parsed.path
        qs = parse_qs(parsed.query)

        # API routes
        if path == "/api/v1/runs":
            self._handle_run_list(qs)
        elif path.startswith("/api/v1/runs/") and path.endswith("/spans"):
            run_id = path.split("/")[4]
            self._handle_run_spans(run_id, qs)
        elif path.startswith("/api/v1/runs/") and path.endswith("/steps"):
            run_id = path.split("/")[4]
            self._handle_run_steps(run_id, qs)
        elif path.startswith("/api/v1/runs/") and path.endswith("/memory"):
            run_id = path.split("/")[4]
            self._handle_run_memory(run_id, qs)
        elif path.startswith("/api/v1/runs/") and path.endswith("/cost"):
            run_id = path.split("/")[4]
            self._handle_run_cost(run_id)
        elif path.startswith("/api/v1/runs/") and "/" not in path[len("/api/v1/runs/"):]:
            run_id = path[len("/api/v1/runs/"):]
            self._handle_run_detail(run_id)
        elif path == "/api/v1/poll":
            self._handle_poll()
        elif path == "/api/v1/health":
            self._send_json({"status": "ok", "db": "connected"})
        # Static assets
        elif path.startswith("/static/"):
            self._serve_static(path[len("/static/"):])
        # SPA catch-all: serve index.html for all other paths
        else:
            self._serve_index()

    def _send_json(self, data: Any, status: int = 200) -> None:
        body = json.dumps(data, default=str).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Content-Security-Policy", "default-src 'self'")
        self.send_header("X-Content-Type-Options", "nosniff")
        self.end_headers()
        self.wfile.write(body)

    def _open_db_readonly(self) -> sqlite3.Connection:
        """Open the SQLite DB in read-only URI mode."""
        uri = f"file:{self.server_config.db_path}?mode=ro"
        conn = sqlite3.connect(uri, uri=True, timeout=5)
        conn.row_factory = sqlite3.Row
        return conn

    def log_message(self, fmt: str, *args: Any) -> None:
        """Redirect access logs to devui.log, not stdout."""
        with open(self.server_config.log_file, "a") as f:
            f.write(f"{self.log_date_time_string()} - {fmt % args}\n")
```

### 10.7 DAG Layout Algorithm

The DAG view uses a hierarchical layout algorithm based on span `parent_id` relationships:

1. **Root detection:** Spans with `parent_id IS NULL` or `parent_id` not in the span set are root nodes.
2. **BFS level assignment:** Each span is assigned a `depth` level via BFS from roots.
3. **X-position:** Spans at the same depth are distributed horizontally by their order within that depth level, separated by `NODE_WIDTH + NODE_GAP` (120px + 20px).
4. **Y-position:** Each depth level occupies a 100px vertical band (`depth * 100`).
5. **Edge rendering:** Bezier curves connect parent center-bottom to child center-top using SVG `<path>` elements.
6. **Color coding:** A 12-color categorical palette (ColorBrewer Set3) is assigned to profiles in order of first appearance; the mapping is stored as `profile → color` in `window.__TAG_PROFILE_COLORS`.

For runs with > 50 nodes the layout switches to a force-directed simulation (D3 `forceSimulation`) with:
- `forceManyBody` strength: -300
- `forceLink` distance: 150px, strength: 0.8
- `forceCenter` at canvas center
- Collision radius: 60px

### 10.8 Flame Chart Rendering

The flame chart uses an SVG canvas with a linear time axis:

```
┌─────────────────────────────────────────────────────────┐
│ 0ms          500ms        1000ms       1500ms    2000ms  │
├──── run  ───────────────────────────────────────────────┤
│   ├── step:1  chat_step ──────────────────────────────  │
│   │     ├── tool_call:code_search ────────────          │
│   │     └── tool_call:bash ──────────────────────       │
│   └── step:2  chat_step ─────────────────────           │
│         └── tool_call:write_file ──────────────────     │
└─────────────────────────────────────────────────────────┘
```

Key rendering constants:
- `PX_PER_MS`: computed as `(canvas_width - LEFT_MARGIN) / total_run_duration_ms`
- `LEFT_MARGIN`: 200px (for span name labels)
- `ROW_HEIGHT`: 22px
- `ROW_GAP`: 2px
- Minimum visible width: 2px (spans shorter than 2px / PX_PER_MS are still shown at 2px)

Zoom: `PX_PER_MS` is multiplied by a zoom factor (`1.0` to `50.0`); horizontal scroll position adjusts accordingly.

### 10.9 Self-Contained HTML Export

The export algorithm:

1. Query `RunDetail` from SQLite.
2. Serialize to JSON via `json.dumps(asdict(run_detail), default=str)`.
3. Read `web/devui/bundle.js` and `web/devui/bundle.css`.
4. Write a single HTML file using a template:

```python
HTML_EXPORT_TEMPLATE = """<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>TAG Run {run_id} — DevUI Export</title>
  <meta http-equiv="Content-Security-Policy"
        content="default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'">
  <style>{bundle_css}</style>
</head>
<body>
  <div id="app" data-export-mode="true"></div>
  <script type="application/json" id="__TAG_EXPORT_DATA__">
{export_json}
  </script>
  <script>{bundle_js}</script>
</body>
</html>
"""
```

The frontend bundle detects `document.getElementById('__TAG_EXPORT_DATA__')` and, when present, uses the inline data instead of fetching from the API. This ensures the file renders identically offline.

### 10.10 Integration with `controller.py`

```python
# In controller.py — new subcommand dispatcher

def cmd_devui(args: argparse.Namespace, cfg: dict[str, Any]) -> None:
    """Dispatch tag devui subcommands."""
    from tag.devui import (
        cmd_devui_start,
        cmd_devui_stop,
        cmd_devui_status,
        cmd_devui_export,
    )
    sub = getattr(args, "devui_sub", None)
    if sub == "start":
        cmd_devui_start(args, cfg)
    elif sub == "stop":
        cmd_devui_stop(args, cfg)
    elif sub == "status":
        cmd_devui_status(args, cfg)
    elif sub == "export":
        cmd_devui_export(args, cfg)
    else:
        print_error("Usage: tag devui {start|stop|status|export}")
        raise SystemExit(1)
```

The `devui` subparser is added in the main argument parser setup:

```python
p_devui = sub.add_parser("devui", help="Local browser UI for agent run visualization")
devui_sub = p_devui.add_subparsers(dest="devui_sub")

p_devui_start = devui_sub.add_parser("start", help="Start the DevUI server")
p_devui_start.add_argument("--port", type=int, default=3000)
p_devui_start.add_argument("--host", default="127.0.0.1")
p_devui_start.add_argument("--run-id", default=None)
p_devui_start.add_argument("--watch", action="store_true")
p_devui_start.add_argument("--no-open", action="store_true")
p_devui_start.add_argument("--db", default=None)
p_devui_start.add_argument("--timeout", type=int, default=0)

p_devui_export = devui_sub.add_parser("export", help="Export a run as HTML or JSON")
p_devui_export.add_argument("--run-id", required=True)
p_devui_export.add_argument("--format", choices=["html", "json"], default="html")
p_devui_export.add_argument("--output", default=None)
p_devui_export.add_argument("--db", default=None)
p_devui_export.add_argument("--open", action="store_true")

p_devui_stop = devui_sub.add_parser("stop", help="Stop the running DevUI server")
p_devui_stop.add_argument("--port", type=int, default=None)

p_devui_status = devui_sub.add_parser("status", help="Show DevUI server status")
```

### 10.11 PID File Management

```python
def _write_pid_file(pid_file: Path, port: int) -> None:
    pid_file.parent.mkdir(parents=True, exist_ok=True)
    pid_file.write_text(json.dumps({"pid": os.getpid(), "port": port}))


def _read_pid_file(pid_file: Path) -> tuple[int, int] | None:
    """Return (pid, port) or None if file missing or stale."""
    if not pid_file.exists():
        return None
    try:
        data = json.loads(pid_file.read_text())
        pid, port = data["pid"], data["port"]
        os.kill(pid, 0)  # raises ProcessLookupError if process is dead
        return pid, port
    except (KeyError, ProcessLookupError, json.JSONDecodeError):
        pid_file.unlink(missing_ok=True)
        return None


def _register_shutdown_handler(server: ThreadingHTTPServer, pid_file: Path) -> None:
    def _shutdown(signum: int, frame: Any) -> None:
        server.shutdown()
        pid_file.unlink(missing_ok=True)
        print("\nDevUI server stopped.")
        raise SystemExit(0)

    signal.signal(signal.SIGINT, _shutdown)
    signal.signal(signal.SIGTERM, _shutdown)
```

---

## 11. Security Considerations

1. **Loopback-only binding by default.** The server binds to `127.0.0.1` unless `--host` is explicitly passed. Binding to `0.0.0.0` prints a red security warning: "WARNING: DevUI is accessible to all network interfaces. Do not use on untrusted networks."

2. **Read-only database access.** `devui.py` opens the SQLite file using the read-only URI mode: `sqlite3.connect("file:path?mode=ro", uri=True)`. This prevents the HTTP server process from accidentally modifying the database if a bug in the request handler issues a write. A complementary runtime assertion in test mode hooks `sqlite3.Connection.execute` and fails the test if any non-`SELECT` SQL is detected.

3. **Content-Security-Policy header.** All API responses include `Content-Security-Policy: default-src 'self'`. The exported HTML uses `default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'` because all assets are inlined. This prevents script injection via span attribute data that contains `<script>` tags — attributes are rendered as text nodes, not innerHTML.

4. **No authentication.** The DevUI server has no authentication mechanism. This is acceptable because it binds to loopback only by default. Users who bind to a non-loopback interface must accept the security warning and understand the risk.

5. **Span attribute data sanitization.** All span attribute values displayed in the UI are text-escaped before insertion into the DOM. The `AttributeTree` component uses `document.createTextNode()` for leaf values and `element.textContent` for labels — never `innerHTML`. Tool arguments that contain user-controlled data (e.g., file paths, prompt text) cannot execute as scripts.

6. **PID file race condition.** If two `tag devui start` processes race, the second will detect a live PID file and print "DevUI already running on port N. Use `tag devui stop` to stop it." and exit. The PID file write is non-atomic (write-then-rename is not used); a future hardening pass should use `O_CREAT | O_EXCL` via `os.open` to make the PID file creation atomic.

7. **Pricing data integrity.** The `pricing.json` file is bundled in the package and cannot be modified by network requests. A user-supplied override at `~/.tag/devui-pricing.json` is loaded if present, but its values are used only for display-side cost estimation — they have no effect on agent execution or billing.

8. **CORS restriction.** CORS response headers are set to `Access-Control-Allow-Origin: http://127.0.0.1:<port>` explicitly. Wildcard CORS (`*`) is never set, preventing cross-origin requests from other browser tabs loaded from external sites.

9. **Sensitive data in span attributes.** Span `attributes` blobs may contain excerpts of tool call arguments including file paths, partial file contents, or prompt text. The DevUI renders this data as read-only text; it does not redact secrets. Users on shared machines should be aware that `tag devui start` exposes this data to anyone with access to `127.0.0.1:<port>` on that machine (i.e., any process running as any user on the same host). A future hardening pass (separate PRD) could add `--auth-token` Bearer authentication.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`tests/test_devui.py`)

| Test | What it verifies |
|------|-----------------|
| `test_span_view_from_row` | `SpanView.from_row()` correctly deserializes all fields including `attributes` JSON |
| `test_cost_computation_anthropic` | `_compute_span_cost()` returns correct USD for Claude Sonnet 4.6 pricing (input $3/1M, output $15/1M) |
| `test_cost_computation_cache_read` | Cache read at 0.1× multiplier is applied correctly |
| `test_cost_computation_unknown_model` | Returns 0.0 for unknown model without raising |
| `test_cost_computation_prefix_match` | Prefix matching on `model_id` (e.g., `"claude-sonnet-4-6-20251101"` matches `"claude-sonnet-4-6"`) |
| `test_load_pricing_valid_json` | `_load_pricing()` returns correct `ModelPricing` objects from fixture `pricing.json` |
| `test_load_pricing_missing_file` | Returns empty dict when file does not exist (no crash) |
| `test_pid_file_write_read` | `_write_pid_file` + `_read_pid_file` round-trip with correct PID and port |
| `test_pid_file_stale` | `_read_pid_file` returns `None` and deletes file for a PID that no longer exists |
| `test_html_export_template_inline` | Generated HTML contains `__TAG_EXPORT_DATA__` script tag with valid JSON |
| `test_html_export_no_network_requests` | Exported HTML contains no `http://`, `https://`, `//` URLs in `src=` or `href=` attributes |
| `test_read_only_assertion` | Monkey-patching `conn.execute` to record SQL; assert no `INSERT`/`UPDATE`/`DELETE` appears in any `_handle_*` call |
| `test_cost_breakdown_aggregation` | `CostBreakdown` `by_profile` and `by_model` correctly sum across spans |

### 12.2 Integration Tests

| Test | What it verifies |
|------|-----------------|
| `test_server_starts_and_health` | Start server with fixture DB → `GET /api/v1/health` returns `{"status":"ok"}` within 2s |
| `test_run_list_api` | `GET /api/v1/runs` returns array with correct field names and types |
| `test_run_detail_api` | `GET /api/v1/runs/<id>` returns run + spans + cost breakdown |
| `test_run_spans_pagination` | `GET /api/v1/runs/<id>/spans?page=2&per_page=10` returns correct page |
| `test_poll_endpoint` | `GET /api/v1/poll` returns current run statuses |
| `test_static_assets_served` | `GET /static/bundle.js` returns 200 with `application/javascript` Content-Type |
| `test_spa_catchall` | `GET /run/abc123` returns `index.html` (SPA routing) |
| `test_loopback_only_default` | With default config, server only accepts connections from `127.0.0.1`; connections from other IPs are refused (tested via `socket.connect`) |
| `test_host_flag_warning` | Starting with `--host 0.0.0.0` prints security warning to stdout |
| `test_cors_header_not_wildcard` | Response `Access-Control-Allow-Origin` header is never `*` |
| `test_csp_header_present` | All API responses include `Content-Security-Policy` header |
| `test_export_html_renders` | Exported HTML file opens with Playwright and flame chart SVG is present in DOM |
| `test_export_json_schema` | JSON export contains `run`, `spans`, `steps`, `cost_breakdown` keys |
| `test_pid_file_lifecycle` | Start server → PID file exists → stop server → PID file deleted |
| `test_server_shutdown_sigterm` | `os.kill(pid, SIGTERM)` causes server to exit within 1s |

### 12.3 Performance Tests

| Test | Threshold |
|------|-----------|
| `bench_run_list_api_500_runs` | `GET /api/v1/runs` with 500 runs in fixture DB: < 200ms p99 |
| `bench_run_detail_500_spans` | `GET /api/v1/runs/:id` with 500 spans: < 500ms p99 |
| `bench_export_html_200_spans` | `tag devui export --format html` with 200-span run: completes in < 3s |
| `bench_flame_chart_render` | Playwright: 200-span flame chart `DOMContentLoaded` to paint: < 1s |

### 12.4 Frontend Tests

The frontend is tested with a Vitest + Playwright setup in `web/devui/`:

- `FlameChart.test.ts`: Asserts span bars are positioned correctly for known timestamps.
- `DagView.test.ts`: Asserts root nodes have no incoming edges; all child nodes have exactly one incoming edge (from parent).
- `CostPanel.test.ts`: Asserts total cost matches sum of per-span costs.
- `AttributeTree.test.ts`: Asserts XSS payload in attribute value is rendered as text, not executed.
- Playwright E2E: Launch server with fixture DB, navigate to run detail, click span, assert side panel shows correct `duration_ms`.

---

## 13. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | `tag devui start` opens `http://127.0.0.1:3000` in the system browser within 2 seconds of the command being entered | Manual timing + CI integration test |
| AC-02 | The run list page shows all runs from the fixture database with profile, status, created_at, and estimated_cost_usd | Integration test asserting `GET /api/v1/runs` response fields |
| AC-03 | The flame chart for a 50-span run shows all 50 spans as horizontal bars with correct left-offset proportional to start time | Playwright pixel assertion + span count check |
| AC-04 | Clicking a span bar opens the details panel showing name, duration_ms, prompt_tokens, completion_tokens, cost_usd, and attributes | Playwright click + DOM assertion |
| AC-05 | The DAG view renders parent → child edges correctly for a known 3-level span hierarchy | DOM edge count assertion in Playwright |
| AC-06 | `tag devui export --run-id X --format html` produces a `.html` file that renders the flame chart when opened via `file://` URI (no network) | Playwright with `--offline` flag; asserts SVG elements present |
| AC-07 | `tag devui export --run-id X --format json` produces valid JSON matching the `RunDetail` schema | `json.loads()` + `jsonschema.validate()` in integration test |
| AC-08 | Starting the server with default settings and attempting to connect from a non-loopback address is refused | Socket test from alternate bind address |
| AC-09 | Starting the server with `--host 0.0.0.0` prints a security warning containing "WARNING" before binding | Subprocess stdout capture |
| AC-10 | No `INSERT`, `UPDATE`, or `DELETE` SQL is ever issued by any `devui.py` code path | SQL intercept hook in unit tests; confirmed for all `_handle_*` methods |
| AC-11 | `tag devui stop` terminates the server process within 1 second and removes the PID file | Integration test: start → stop → assert process dead + PID file gone |
| AC-12 | `tag devui status` with a running server prints PID, port, and URL | Subprocess stdout capture + field assertions |
| AC-13 | `tag devui status` with a stale PID file prints "STOPPED (stale PID file removed)" and deletes the PID file | Unit test with mock dead PID |
| AC-14 | The `--watch` flag causes the run list to refresh within 6 seconds of a new run being inserted into the fixture DB | Playwright: insert row → wait 6s → assert new run appears |
| AC-15 | The cost panel shows `total_usd` matching the sum of all per-span `cost_usd` values to within $0.0001 floating-point tolerance | Unit test with known pricing data |
| AC-16 | Runs with associated `eval_runs` records show an eval score badge on the run list | Integration test: insert `eval_runs` row → assert badge in run list API response |
| AC-17 | `GET /api/v1/runs/:id/memory` returns memory_journal rows for the run's master_profile | Integration test with fixture memory_journal data |
| AC-18 | Exported HTML `<script>` block containing `__TAG_EXPORT_DATA__` is valid JSON parseable by `JSON.parse()` | Unit test on `_build_html_export()` output |
| AC-19 | `tag devui start --run-id X` causes the browser to open to `/run/X` not `/` | Integration test: mock `webbrowser.open`, assert URL contains `/run/X` |
| AC-20 | `pyproject.toml` installs `tag devui` without any new mandatory dependencies beyond the existing TAG requirements | `pip install -e .` in a clean venv; `tag devui status` runs without `ImportError` |

---

## 14. Dependencies

| Dependency | Type | Already Present | Notes |
|------------|------|-----------------|-------|
| `http.server.ThreadingHTTPServer` | Python stdlib | Yes | Zero new dependencies |
| `sqlite3` | Python stdlib | Yes | Read-only URI mode: `file:path?mode=ro` |
| `webbrowser` | Python stdlib | Yes | `webbrowser.open(url)` |
| `signal` | Python stdlib | Yes | SIGINT/SIGTERM handler |
| `json` | Python stdlib | Yes | API serialization |
| `threading` | Python stdlib | Yes | `ThreadingHTTPServer` |
| `tag.controller.open_db` | Internal | Yes | DB connection factory |
| `tag.controller.runtime_db_path` | Internal | Yes | Default DB path |
| `spans` table | SQLite schema | Yes (PRD-013) | Core data source |
| `runs` table | SQLite schema | Yes (PRD-012 columns) | Cost columns: `cache_read_tokens`, `cache_creation_tokens` |
| `steps` table | SQLite schema | Yes | Conversation steps |
| `memory_journal` table | SQLite schema | Yes | Memory state |
| `eval_runs` + `eval_cases` tables | SQLite schema | Yes (PRD-027) | Eval score badges |
| `sandbox_runs` table | SQLite schema | Yes (PRD-028) | Optional sandbox span detail |
| D3.js v7 | Frontend (bundled) | No | DAG and flame chart rendering; bundled into `bundle.js`, not a runtime pip dep |
| Vitest | Dev dependency only | No | Frontend unit tests; not installed in end-user environment |
| Playwright | Dev/CI dependency | No | Browser E2E tests; not installed in end-user environment |

---

## 15. Open Questions

| # | Question | Owner | Resolution Needed By |
|---|----------|-------|---------------------|
| OQ-1 | Should the frontend use D3.js (more powerful, larger bundle) or a lightweight Canvas-only renderer (smaller bundle, less interactive)? D3 produces a ~300 KB bundle contribution. | Frontend lead | Phase 1 kickoff |
| OQ-2 | Should `tag devui start` block the terminal (server in foreground) or daemonize (server in background, returns control to shell)? Foreground is simpler and avoids orphaned processes; background is more convenient for long sessions. | CLI UX | Phase 1 kickoff |
| OQ-3 | For `--watch` mode, should the UI use polling (`setInterval` + `GET /api/v1/poll`) or WebSockets? Polling is simpler (no additional server code); WebSockets enable true push-based live updates but require `asyncio` server or a separate thread per connection. | Architecture | Phase 2 |
| OQ-4 | Should `pricing.json` be auto-updated at server startup from a known URL (e.g., a TAG-maintained pricing API), or remain a static bundled file updated only at release time? Auto-update adds a network call and uptime dependency; static is simpler but may drift. | Product | Phase 1 |
| OQ-5 | Should `tag devui export --format html` support embedding the full conversation prompt/output text in the HTML, or redact it by default and require `--include-prompts` to include? Prompts may contain sensitive data; redaction is safer but loses diagnostic value. | Security | Phase 2 |
| OQ-6 | For runs with > 1,000 spans (e.g., a long `tag loop` run), should the flame chart render all spans (potentially slow) or cap at 500 and offer a "load more" control? | Performance | Phase 2 |
| OQ-7 | Should the DevUI include a diff view comparing two runs' span structures side-by-side? This overlaps with PRD-017 (multi-model benchmarking) and PRD-032 (time-travel debugging). Defer to follow-on PRD. | Product | Backlog |
| OQ-8 | Should `tag devui stop` support `--all` to kill all devui processes (in case of multiple instances)? Requires iterating over all PID files in `~/.tag/runtime/`. | CLI UX | Phase 2 |
| OQ-9 | What is the retention policy for `devui_export_log` rows? Should they be pruned alongside old spans (currently 30 days via `_prune_old_spans`)? | Data | Phase 2 |

---

## 16. Complexity and Timeline

### Phase 1 — Core server + flame chart (Days 1–7)

| Day | Task |
|-----|------|
| 1 | Scaffold `src/tag/devui.py`: `DevUIServerConfig`, `DevUIHandler`, `cmd_devui_*` stubs; add argparse subparsers to `controller.py`; add `CREATE TABLE devui_export_log` to `_migrate_prd_033_044_tables` |
| 2 | Implement all SQL queries: `_query_run_list`, `_query_run_detail`, `_query_run_spans`, `_query_run_steps`, `_query_run_memory`, `_query_run_cost`; all read-only, tested with in-memory SQLite fixture |
| 3 | Implement `pricing.json` schema and `_load_pricing` + `_compute_span_cost`; unit tests for all pricing edge cases (unknown model, prefix match, cache multipliers) |
| 4 | Implement `ThreadingHTTPServer` setup, PID file management, SIGINT/SIGTERM handlers, `tag devui stop`, `tag devui status` |
| 5 | Build frontend: `index.html`, `bundle.js` stub with run list table using vanilla JS + `fetch("/api/v1/runs")`; test end-to-end with a real fixture DB |
| 6 | Implement D3 flame chart in `FlameChart.ts`: horizontal time axis, span bars, click handler for details panel; compile to `bundle.js` |
| 7 | Write unit tests and integration tests for Phase 1; fix bugs; verify `pyproject.toml` requires no new deps |

### Phase 2 — DAG view + cost panel + export (Days 8–14)

| Day | Task |
|-----|------|
| 8 | Implement DAG view in `DagView.ts`: BFS depth layout for small graphs (< 50 nodes), D3 force layout for large graphs; profile color coding |
| 9 | Implement cost panel in `CostPanel.ts`: per-span cost bars, by-profile table, by-model table, total USD display |
| 10 | Implement attribute tree inspector in `AttributeTree.ts`: collapsible JSON tree, XSS-safe text rendering, max 5-key auto-collapse |
| 11 | Implement `tag devui export --format html`: `_build_html_export()`, inline JSON data block, inline JS/CSS; offline rendering test |
| 12 | Implement `tag devui export --format json`: serialize `RunDetail` dataclass to JSON; schema validation test |
| 13 | Implement `--watch` flag: `GET /api/v1/poll` endpoint + frontend `setInterval` polling; Playwright E2E test for live refresh |
| 14 | Performance profiling (500-run list, 500-span detail); optimize SQL with `EXPLAIN QUERY PLAN`; add indexes if needed; fix any issues found |

### Phase 3 — Polish, eval badges, security hardening (Days 15–21)

| Day | Task |
|-----|------|
| 15 | Eval score badges: query `eval_runs` and `eval_cases` for score badge in `GET /api/v1/runs`; display in run list page |
| 16 | Memory inspector: `GET /api/v1/runs/:id/memory` endpoint; memory panel in run detail page showing `memory_journal` rows at run time |
| 17 | Security hardening: CSP headers on all responses; CORS restriction; `mode=ro` SQLite URI enforcement; HTML export sanitization review |
| 18 | Browser compatibility testing: Chrome, Firefox, Safari, Edge; fix any rendering issues |
| 19 | `tag devui start --run-id <id>` direct-open flow; `tag devui start --timeout` auto-shutdown; server request log to file |
| 20 | Playwright E2E test suite: full user journey (start → list → detail → flame chart click → panel → export → offline HTML); CI integration |
| 21 | Documentation: `tag devui --help` text; `docs/devui.md` usage guide; update `docs/prd/INDEX.md`; final code review |

**Total estimate: 21 days (L size, consistent with 2–4 weeks)**

---

*Filed as GitHub issue #343. Questions and design feedback welcome before Phase 1 kickoff.*

