# PRD-022: Web Dashboard (`tag serve`)

**Status:** Proposed  
**Priority:** P1  
**Estimated Effort:** L (backend M, frontend L — 2–3 sprints)  
**Affects:** new `src/tag/api.py`, `src/tag/controller.py` (`cmd_serve`), `web/src/`, `pyproject.toml`, `MANIFEST.in`

---

## 1. Overview

TAG currently exposes all agent run data, cost analytics, queue state, and span traces exclusively through the terminal. This PRD adds `tag serve` — a local web dashboard backed by a FastAPI server and a React frontend — that gives users a real-time, browser-based view of everything happening inside their TAG installation. The server binds `localhost:8787` by default, auto-opens a browser tab, and serves the React build as static files bundled into the Python package. Live updates flow over Server-Sent Events (SSE) so the dashboard refreshes as agents run without requiring a page reload. All data is read from the existing `tag.sqlite3` schema (`runs`, `steps`, `spans`, `queue_jobs`, `events`, `benchmark_results`) — no new tables are required.

---

## 2. Goals

1. **Real-time run monitoring** — users can watch a `tag loop` or `tag swarm` run in progress from the browser, seeing status, elapsed time, and token counts update live.
2. **Cost charts** — cumulative and per-profile cost charts (daily, weekly, all-time) built from the `runs.estimated_cost_usd` column, so users understand spend without running `tag costs`.
3. **Queue DAG visualization** — the `queue_jobs` table rendered as a dependency graph (react-flow) showing pending, running, done, and failed jobs.
4. **Span waterfall view** — a per-run span waterfall (Gantt-style) built from the `spans` table, equivalent to the terminal `tag trace <run_id>` but interactive and filterable.
5. **Profile management** — list all profiles from `cli-config.yaml`, show per-profile cost totals, and allow setting the active profile from the browser.
6. **Localhost-only default security** — the server binds `127.0.0.1` unless `--host` is explicitly passed, preventing accidental LAN exposure.
7. **SSE-based live updates** — `/api/stream` pushes delta events (changed runs, new spans) every second so the browser tab stays current without polling.
8. **Shareable with explicit consent** — passing `--host 0.0.0.0 --auth-token <token>` enables remote access for pair-programming scenarios, with a clear startup warning.

---

## 3. Non-Goals

- **Multi-user authentication or RBAC** — this is a single-user local developer tool; no login system will be built.
- **Cloud hosting or deployment** — the dashboard is not designed to run on a remote server behind a load balancer; it is a local companion to the CLI.
- **Mobile app** — the React UI targets desktop browsers at 1024px+ viewport; no mobile-responsive breakpoints are required in v1.
- **Replacing terminal output** — `tag serve` is additive; it does not suppress, redirect, or replace `tag loop` / `tag swarm` terminal output.
- **Write operations beyond queue management** — the dashboard does not create runs, modify profiles on disk, or alter the SQLite schema; write operations are limited to queue job cancellation.
- **Persistent user preferences** — no settings or view state is persisted server-side; React component state is ephemeral within the browser session.

---

## 4. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | open the dashboard while `tag loop --profile coder` is running | I can monitor progress — step count, elapsed time, token/cost accumulation — without keeping a terminal window focused |
| U2 | Cost-conscious user | browse the cost history charts filtered by profile and date range | I can see which profiles and models are consuming the most budget over the past week before my next billing review |
| U3 | Developer debugging a slow run | click into a run from RunsTable and inspect the span waterfall | I can identify which tool call or model step took the most time and whether it returned an error |
| U4 | User managing a multi-job queue | use the queue view to cancel a stuck job and reprioritize the next one | I avoid restarting the entire queue worker just to intervene on one bad job |
| U5 | Developer pairing with a teammate | run `tag serve --host 0.0.0.0 --auth-token letmein` and share the URL | my teammate on the same LAN can view the live run state during the session without needing a local TAG install |
| U6 | User evaluating benchmark runs | open the benchmark results page to compare model quality scores and cost side-by-side | I can make a data-driven model selection without parsing CSV output |

---

## 5. Proposed CLI Surface

### 5.1 Primary command

```
tag serve [--port 8787] [--host localhost] [--no-browser] [--reload]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--port` | int | `8787` | TCP port to bind |
| `--host` | str | `localhost` | Bind address; `localhost` maps to `127.0.0.1` |
| `--no-browser` | flag | off | Skip auto-opening the default browser |
| `--reload` | flag | off | Enable uvicorn hot-reload for frontend/backend development |

### 5.2 Remote access variant

```
tag serve --host 0.0.0.0 --auth-token <token>
```

When `--host` is anything other than `localhost` / `127.0.0.1`, TAG **must** also receive `--auth-token`; otherwise the command prints an error and exits non-zero:

```
ERROR: --host 0.0.0.0 requires --auth-token to prevent unauthenticated LAN access.
       Run: tag serve --host 0.0.0.0 --auth-token <your-secret-token>
```

When `--auth-token` is provided, every `/api/*` request requires the header `Authorization: Bearer <token>`; missing or wrong tokens receive HTTP 401.

### 5.3 Startup output

```
TAG Web Dashboard
  URL : http://localhost:8787
  DB  : /Users/alice/.tag/tag.sqlite3
  Live: SSE at /api/stream

Press Ctrl+C to stop.
```

---

## 6. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | `tag serve` starts a uvicorn ASGI server hosting the FastAPI app defined in `src/tag/api.py`. |
| FR-02 | The FastAPI app serves all `/api/*` endpoints with CORS restricted to the dashboard's own origin (`http://localhost:<port>`). |
| FR-03 | `GET /api/runs` returns a paginated list of runs from the `runs` table, sorted by `created_at DESC`, with query params `?limit=50&offset=0&status=&profile=`. |
| FR-04 | `GET /api/runs/{run_id}` returns a single run record with its associated `steps` array embedded. |
| FR-05 | `GET /api/spans?trace_id=<id>` returns all spans for a given `trace_id`, enabling the waterfall view; also supports `?run_id=<id>` to auto-resolve the trace_id. |
| FR-06 | `GET /api/costs` returns aggregated cost data: total `estimated_cost_usd`, `prompt_tokens`, `completion_tokens`, and `total_tokens` grouped by `master_profile` and by calendar day, suitable for charting. |
| FR-07 | `GET /api/queue` returns all `queue_jobs` rows with `?status=` filter; also exposes `POST /api/queue/{job_id}/cancel` to set a job's status to `cancelled`. |
| FR-08 | `GET /api/profiles` returns profile names and per-profile cost totals derived from `runs`; names are read from `cli-config.yaml` at request time (no caching). |
| FR-09 | `GET /api/stream` is a Server-Sent Events endpoint; it polls the SQLite database every 1 second and emits `data: <json>` events for any `runs` or `queue_jobs` rows whose `updated_at` (or `created_at`) changed since the last poll. The event `type` field is one of `run_update`, `queue_update`, or `heartbeat`. |
| FR-10 | `GET /api/ws` is a WebSocket endpoint providing the same live-update stream as SSE for clients that prefer WebSocket over SSE. |
| FR-11 | When `--no-browser` is not passed, `tag serve` calls `webbrowser.open("http://<host>:<port>")` after uvicorn reports it is ready (0.5s delay after bind). |
| FR-12 | When `--auth-token` is set, a FastAPI middleware checks `Authorization: Bearer <token>` on all `/api/*` requests; unauthenticated requests return HTTP 401 JSON `{"detail": "Unauthorized"}`. |
| FR-13 | The FastAPI app serves the compiled React build from `src/tag/assets/web/` as a `StaticFiles` mount at `/`; all unknown paths return `index.html` (SPA fallback). |
| FR-14 | `GET /api/benchmarks` returns `benchmark_comparisons` joined with `benchmark_results` aggregated by model, exposing `quality_score`, `latency_ms`, and `cost_usd` averages. |
| FR-15 | `GET /api/health` returns `{"status": "ok", "db": "<path>", "version": "<tag version>"}` with HTTP 200; used by the frontend to detect if the server is alive and by load-balancer health checks in the remote-access scenario. |
| FR-16 | `tag serve --reload` passes `reload=True` to uvicorn, enabling hot-reload for both FastAPI changes and (when Vite dev server is not used) static file changes. |
| FR-17 | Graceful shutdown: `SIGINT` / `SIGTERM` allows in-flight SSE connections to drain within 2 seconds before the process exits. |

---

## 7. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | All `/api/*` read endpoints must respond in under 100ms for databases up to 100,000 rows (spans), measured on commodity hardware (2018 MacBook Pro or equivalent). Queries must use existing indexed columns (`trace_id`, `status`, `created_at`). |
| NFR-02 | The SSE `/api/stream` endpoint must reconnect automatically on network interruption; the React `EventSource` client must include `reconnect` logic with exponential backoff (1s, 2s, 4s, cap 30s). |
| NFR-03 | Total process memory footprint of `tag serve` (uvicorn + FastAPI, no concurrent runs) must stay under 50MB RSS on macOS. This precludes loading all spans into memory; all queries must be streaming or paginated. |
| NFR-04 | The compiled React build (JS + CSS) must be bundled as a Python package asset in `src/tag/assets/web/` so that `pip install tag-agent` provides a fully working dashboard with no separate npm install step for end users. |
| NFR-05 | The React build output must be under 2MB gzipped, avoiding large dependency bundles. Tree-shaking must be enabled in the Vite config. |
| NFR-06 | API responses must be JSON with `Content-Type: application/json`; datetime fields must be ISO-8601 UTC strings; numeric cost fields must be rounded to 6 decimal places. |
| NFR-07 | The server must not write to the SQLite database (except for the `queue_jobs` cancel endpoint) to avoid interfering with concurrent agent runs. All reads use `check_same_thread=False` with a read-only connection. |

---

## 8. Technical Design

### 8.1 New files

| Path | Purpose |
|------|---------|
| `src/tag/api.py` | FastAPI application, all `/api/*` route handlers, SSE generator, auth middleware, static file mounting |
| `src/tag/assets/web/` | Compiled React build output (committed or generated at package build time) |
| `web/src/App.tsx` | React router root, sidebar navigation |
| `web/src/pages/RunsPage.tsx` | Runs list with RunsTable component |
| `web/src/pages/RunDetailPage.tsx` | Single run view with SpanWaterfall |
| `web/src/pages/CostsPage.tsx` | Cost charts with CostChart component |
| `web/src/pages/QueuePage.tsx` | Queue list with QueueDAG and cancel actions |
| `web/src/pages/ProfilesPage.tsx` | Profile list with per-profile cost totals |
| `web/src/pages/BenchmarksPage.tsx` | Benchmark comparison table |
| `web/src/components/RunsTable.tsx` | Sortable table of runs with status badges |
| `web/src/components/CostChart.tsx` | Recharts AreaChart of daily cost by profile |
| `web/src/components/QueueDAG.tsx` | react-flow graph of queue jobs with status colors |
| `web/src/components/SpanWaterfall.tsx` | Gantt-style waterfall of spans for a trace |
| `web/src/components/ProfileList.tsx` | Profile cards with cost totals |
| `web/src/hooks/useSSE.ts` | Custom hook wrapping `EventSource` with reconnect logic |
| `web/vite.config.ts` | Vite build config with output to `../src/tag/assets/web/` |

### 8.2 FastAPI application structure (`src/tag/api.py`)

```python
from fastapi import FastAPI, HTTPException, Depends, Request
from fastapi.middleware.cors import CORSMiddleware
from fastapi.staticfiles import StaticFiles
from fastapi.responses import FileResponse, StreamingResponse
import asyncio, sqlite3, json, time
from pathlib import Path
from typing import AsyncGenerator

def create_app(db_path: Path, auth_token: str | None = None) -> FastAPI:
    app = FastAPI(title="TAG Dashboard", version="0.3.0")

    # CORS: only allow the dashboard's own origin
    app.add_middleware(
        CORSMiddleware,
        allow_origins=["http://localhost:8787"],  # overridden at startup with actual port
        allow_methods=["GET", "POST"],
        allow_headers=["Authorization"],
    )

    # Auth middleware
    if auth_token:
        @app.middleware("http")
        async def bearer_auth(request: Request, call_next):
            if request.url.path.startswith("/api/"):
                header = request.headers.get("Authorization", "")
                if header != f"Bearer {auth_token}":
                    return JSONResponse({"detail": "Unauthorized"}, status_code=401)
            return await call_next(request)

    # ... route handlers ...

    # SPA fallback: serve React build
    assets = Path(__file__).parent / "assets" / "web"
    app.mount("/", StaticFiles(directory=assets, html=True), name="static")

    return app
```

### 8.3 Full OpenAPI endpoint specification

#### `GET /api/health`
```yaml
summary: Health check
responses:
  200:
    content:
      application/json:
        schema:
          type: object
          properties:
            status: {type: string, example: ok}
            db: {type: string, example: /Users/alice/.tag/tag.sqlite3}
            version: {type: string, example: 0.3.0}
```

#### `GET /api/runs`
```yaml
summary: List runs (paginated)
parameters:
  - name: limit, in: query, type: integer, default: 50
  - name: offset, in: query, type: integer, default: 0
  - name: status, in: query, type: string, enum: [running, done, error, ""]
  - name: profile, in: query, type: string
responses:
  200:
    content:
      application/json:
        schema:
          type: object
          properties:
            total: {type: integer}
            items:
              type: array
              items:
                $ref: "#/components/schemas/Run"
```

**Run schema:**
```yaml
Run:
  type: object
  properties:
    id: {type: string}
    created_at: {type: string, format: date-time}
    kind: {type: string}
    task_type: {type: string}
    execution: {type: string}
    master_profile: {type: string}
    board: {type: string}
    status: {type: string}
    prompt_tokens: {type: integer}
    completion_tokens: {type: integer}
    total_tokens: {type: integer}
    estimated_cost_usd: {type: number}
    model_id: {type: string}
    provider: {type: string}
```

Note: `prompt` and `route_json` fields are **omitted** from API responses to prevent accidental exposure of task content that may contain secrets.

#### `GET /api/runs/{run_id}`
```yaml
summary: Get single run with steps
parameters:
  - name: run_id, in: path, required: true, type: string
responses:
  200:
    content:
      application/json:
        schema:
          allOf:
            - $ref: "#/components/schemas/Run"
            - type: object
              properties:
                steps:
                  type: array
                  items:
                    $ref: "#/components/schemas/Step"
  404:
    description: Run not found
```

**Step schema:**
```yaml
Step:
  type: object
  properties:
    id: {type: integer}
    role: {type: string}
    profile: {type: string}
    model_ref: {type: string}
    status: {type: string}
    started_at: {type: string, format: date-time}
    finished_at: {type: string, format: date-time}
    duration_ms: {type: integer}
```

Note: `prompt` and `output` fields are **omitted** from the Step API response.

#### `GET /api/spans`
```yaml
summary: List spans for a trace or run
parameters:
  - name: trace_id, in: query, type: string
  - name: run_id, in: query, type: string
  - name: limit, in: query, type: integer, default: 500
responses:
  200:
    content:
      application/json:
        schema:
          type: object
          properties:
            items:
              type: array
              items:
                $ref: "#/components/schemas/Span"
```

**Span schema:**
```yaml
Span:
  type: object
  properties:
    id: {type: string}
    trace_id: {type: string}
    parent_id: {type: string, nullable: true}
    name: {type: string}
    profile: {type: string, nullable: true}
    model_id: {type: string, nullable: true}
    started_at: {type: string, format: date-time}
    finished_at: {type: string, format: date-time, nullable: true}
    duration_ms: {type: integer, nullable: true}
    status: {type: string, enum: [ok, error, timeout]}
    prompt_tokens: {type: integer}
    completion_tokens: {type: integer}
    error_msg: {type: string, nullable: true}
```

#### `GET /api/costs`
```yaml
summary: Aggregated cost analytics
parameters:
  - name: profile, in: query, type: string
  - name: days, in: query, type: integer, default: 30
responses:
  200:
    content:
      application/json:
        schema:
          type: object
          properties:
            total_usd: {type: number}
            total_tokens: {type: integer}
            by_profile:
              type: array
              items:
                type: object
                properties:
                  profile: {type: string}
                  cost_usd: {type: number}
                  prompt_tokens: {type: integer}
                  completion_tokens: {type: integer}
            by_day:
              type: array
              items:
                type: object
                properties:
                  date: {type: string, format: date}
                  cost_usd: {type: number}
                  run_count: {type: integer}
```

#### `GET /api/queue`
```yaml
summary: List queue jobs
parameters:
  - name: status, in: query, type: string, enum: [queued, running, done, failed, cancelled, ""]
responses:
  200:
    content:
      application/json:
        schema:
          type: object
          properties:
            items:
              type: array
              items:
                $ref: "#/components/schemas/QueueJob"
```

**QueueJob schema:**
```yaml
QueueJob:
  type: object
  properties:
    id: {type: string}
    profile: {type: string}
    task_type: {type: string}
    status: {type: string}
    priority: {type: integer}
    created_at: {type: string, format: date-time}
    started_at: {type: string, format: date-time, nullable: true}
    finished_at: {type: string, format: date-time, nullable: true}
    pid: {type: integer, nullable: true}
    exit_code: {type: integer, nullable: true}
    error: {type: string, nullable: true}
```

Note: `task` field (raw task text) is **omitted** from API responses.

#### `POST /api/queue/{job_id}/cancel`
```yaml
summary: Cancel a queued or running job
parameters:
  - name: job_id, in: path, required: true, type: string
responses:
  200:
    content:
      application/json:
        schema:
          type: object
          properties:
            ok: {type: boolean}
            job_id: {type: string}
  404:
    description: Job not found
  409:
    description: Job already in terminal state (done/failed/cancelled)
```

#### `GET /api/profiles`
```yaml
summary: List profiles with cost summaries
responses:
  200:
    content:
      application/json:
        schema:
          type: object
          properties:
            items:
              type: array
              items:
                type: object
                properties:
                  name: {type: string}
                  run_count: {type: integer}
                  total_cost_usd: {type: number}
                  last_run_at: {type: string, format: date-time, nullable: true}
```

#### `GET /api/benchmarks`
```yaml
summary: Benchmark comparison results
parameters:
  - name: comparison_id, in: query, type: string
responses:
  200:
    content:
      application/json:
        schema:
          type: object
          properties:
            comparisons:
              type: array
              items:
                type: object
                properties:
                  id: {type: string}
                  suite_path: {type: string}
                  status: {type: string}
                  created_at: {type: string, format: date-time}
                  models:
                    type: array
                    items:
                      type: object
                      properties:
                        model_id: {type: string}
                        avg_quality_score: {type: number}
                        avg_latency_ms: {type: number}
                        total_cost_usd: {type: number}
                        pass_rate: {type: number}
```

#### `GET /api/stream` (SSE)
```yaml
summary: Server-Sent Events live update stream
responses:
  200:
    content:
      text/event-stream:
        description: |
          Emits events every 1 second. Event types:
            run_update   — a run row changed (fields: id, status, estimated_cost_usd, total_tokens)
            queue_update — a queue_job row changed (fields: id, status, pid)
            heartbeat    — emitted every 10s if no data changes; payload: {"ts": "<iso>"}
```

SSE event wire format:
```
event: run_update
data: {"id": "abc123", "status": "done", "estimated_cost_usd": 0.002341, "total_tokens": 1500}

event: heartbeat
data: {"ts": "2026-06-12T10:00:00Z"}
```

#### `GET /api/ws` (WebSocket)
```yaml
summary: WebSocket live update stream (same payload as SSE)
description: >
  Clients that prefer WebSocket over SSE can connect here.
  The server sends JSON text frames with the same event structure as SSE.
  Clients should send a ping frame every 30s; the server will echo a pong.
```

### 8.4 React component specifications

**RunsTable** (`web/src/components/RunsTable.tsx`)
- Columns: Run ID (truncated, clickable), Profile, Status badge (color-coded), Model, Cost, Tokens, Started At, Duration
- Sortable by any column; client-side sort for current page
- Real-time row highlight animation when `run_update` SSE event arrives matching that run's ID
- Pagination controls (limit/offset)

**CostChart** (`web/src/components/CostChart.tsx`)
- Built with `recharts` AreaChart
- X-axis: calendar dates from `/api/costs?days=30`
- Y-axis: USD cost
- Multi-series: one area per profile (stacked)
- Profile filter dropdown above chart; date range picker (7d / 30d / 90d / all)
- Tooltip shows: date, per-profile cost, total cost, run count

**QueueDAG** (`web/src/components/QueueDAG.tsx`)
- Built with `react-flow`
- One node per `queue_job`; node color encodes status (gray=queued, blue=running, green=done, red=failed, yellow=cancelled)
- No explicit dependency edges in v1 (queue_jobs has no parent_id column); nodes arranged in priority-descending layout
- "Cancel" button on running/queued nodes triggers `POST /api/queue/{id}/cancel`
- Real-time node color updates via SSE `queue_update` events

**SpanWaterfall** (`web/src/components/SpanWaterfall.tsx`)
- Gantt-style horizontal bar chart built with SVG (no external chart library)
- Y-axis: span names, indented by nesting depth (parent_id tree)
- X-axis: elapsed milliseconds from root span `started_at`
- Bar color: green=ok, red=error, yellow=timeout
- Hover tooltip: span name, model_id, duration_ms, prompt_tokens, completion_tokens, error_msg
- Root span shown at top; children indented 16px per depth level

**ProfileList** (`web/src/components/ProfileList.tsx`)
- Card grid, one card per profile from `/api/profiles`
- Each card: profile name, run count, total cost, last run timestamp
- Clicking a card navigates to RunsPage filtered by that profile

### 8.5 SSE polling implementation

The SSE generator in `api.py` maintains a lightweight "watermark" of the last-seen `created_at` for runs and queue_jobs. Every 1 second it executes two queries:

```sql
-- New or updated runs since last poll
SELECT id, status, estimated_cost_usd, total_tokens, master_profile
FROM runs
WHERE created_at > ?
ORDER BY created_at DESC
LIMIT 50;

-- New or updated queue jobs since last poll
SELECT id, status, pid, finished_at
FROM queue_jobs
WHERE created_at > ? OR (started_at > ?) OR (finished_at > ?)
LIMIT 100;
```

The watermark advances to the latest `created_at` seen. This approach is compatible with SQLite's lack of `LISTEN`/`NOTIFY`; it avoids polling the entire table by relying on the existing `idx_qj_status` and `idx_spans_trace` indices.

### 8.6 Authentication design

When `--auth-token <token>` is passed to `tag serve`:
1. The token value is stored in a `_AUTH_TOKEN` module-level variable in `api.py` (not in the database or config files).
2. A FastAPI HTTP middleware intercepts every request whose path starts with `/api/`. Requests to `/` (static files) are exempt.
3. The middleware reads `Authorization: Bearer <token>` and compares using `secrets.compare_digest` (constant-time comparison to prevent timing attacks).
4. The SSE endpoint respects auth: the client must pass the token as a query parameter `?token=<token>` when establishing the `EventSource`, since the `EventSource` API does not support custom request headers in browsers.

### 8.7 Static file bundling strategy

The React build is generated via `vite build` with `outDir` set to `../../src/tag/assets/web/` in `web/vite.config.ts`. The compiled output (HTML, JS, CSS, source maps) is:
- Committed to the repository under `src/tag/assets/web/` (checked-in build artifact).
- Listed in `MANIFEST.in` as `recursive-include src/tag/assets/web *`.
- Declared in `pyproject.toml` under `[tool.setuptools.package-data]` as `{"tag": ["assets/web/**"]}`.

End users who install `tag-agent` via `pip` receive the compiled React frontend as part of the package; no npm is required at install time. Developers who want to modify the frontend run `npm run build` from `web/` to regenerate `src/tag/assets/web/`, then reinstall the Python package in editable mode.

---

## 9. Security Considerations

| ID | Consideration | Mitigation |
|----|--------------|-----------|
| S-01 | Default binding exposes dashboard to all local interfaces on some systems | Bind to `127.0.0.1` (not `0.0.0.0`) when `--host localhost` (the default) is used; IPv6 loopback `::1` also bound |
| S-02 | Explicit `--host 0.0.0.0` without a token exposes the database to any LAN peer | `cmd_serve` enforces that `--host != localhost` requires `--auth-token`; exits non-zero if token is absent |
| S-03 | Terminal warning for remote access | When `--host 0.0.0.0` is used, print a prominent startup warning: `WARNING: Binding to 0.0.0.0 — dashboard is accessible on your network. Protect with --auth-token.` |
| S-04 | Bearer token for remote access | All `/api/*` endpoints check `Authorization: Bearer <token>` via constant-time `secrets.compare_digest`; HTTP 401 on failure |
| S-05 | CORS restricted to dashboard origin | `CORSMiddleware` `allow_origins` is set to `["http://localhost:<port>"]` (or `["http://<host>:<port>"]` for remote); wildcard `*` is never used |
| S-06 | API keys in LLM provider config must never appear in responses | `/api/profiles` and `/api/runs` must not expose `route_json`, `metadata_json`, or any config field containing API key patterns; these columns are excluded from SELECT statements |
| S-07 | XSS prevention in React | All user-generated content (run prompts, error messages, task descriptions) rendered inside React components uses React's default text escaping; `dangerouslySetInnerHTML` is forbidden in this codebase |
| S-08 | CSRF protection | The API is stateless (no cookies, no sessions); Bearer token auth is not susceptible to CSRF. No state-changing endpoints accept `application/x-www-form-urlencoded` bodies |
| S-09 | Read-only database access | The FastAPI app opens the SQLite file with `?mode=ro` URI parameter for all non-cancel endpoints; the cancel endpoint uses a separate read-write connection scoped to that request |
| S-10 | SSE auth via query param | Since browsers cannot set `Authorization` headers on `EventSource`, the token is accepted as `?token=<value>` on `/api/stream`; the token is never logged or echoed back in responses |
| S-11 | No filesystem traversal via static serving | FastAPI `StaticFiles` with a fixed directory prevents path traversal attacks; the `web/` build output contains only `.js`, `.css`, `.html`, and `.svg` files |
| S-12 | Dependency supply chain | `fastapi`, `uvicorn[standard]`, and `websockets` are pinned to semver ranges in `pyproject.toml`; `recharts` and `react-flow` are pinned in `package.json` |

---

## 10. Testing Strategy

### 10.1 Backend (FastAPI)

All API tests use FastAPI's `TestClient` (based on `httpx`) with a temporary in-memory SQLite database seeded with fixture data.

```python
# tests/test_api.py
from fastapi.testclient import TestClient
from tag.api import create_app
import sqlite3, tempfile, pathlib

@pytest.fixture
def client(tmp_path):
    db = tmp_path / "test.sqlite3"
    # seed fixture data
    conn = sqlite3.connect(db)
    _init_schema(conn)
    _seed_runs(conn, n=5)
    _seed_spans(conn)
    conn.close()
    app = create_app(db_path=db, auth_token=None)
    return TestClient(app)

def test_runs_list_returns_paginated(client):
    r = client.get("/api/runs?limit=3")
    assert r.status_code == 200
    body = r.json()
    assert body["total"] >= 5
    assert len(body["items"]) == 3

def test_spans_waterfall_structure(client):
    r = client.get("/api/spans?trace_id=trace001")
    assert r.status_code == 200
    spans = r.json()["items"]
    # Verify parent_id references are internally consistent
    ids = {s["id"] for s in spans}
    for s in spans:
        if s["parent_id"] is not None:
            assert s["parent_id"] in ids

def test_auth_middleware_rejects_missing_token():
    app = create_app(db_path=pathlib.Path(":memory:"), auth_token="secret")
    c = TestClient(app, raise_server_exceptions=False)
    r = c.get("/api/runs")
    assert r.status_code == 401

def test_auth_middleware_accepts_valid_token():
    app = create_app(db_path=pathlib.Path(":memory:"), auth_token="secret")
    c = TestClient(app)
    r = c.get("/api/health", headers={"Authorization": "Bearer secret"})
    assert r.status_code == 200

def test_queue_cancel_job(client):
    # seed a queued job
    r = client.post("/api/queue/job001/cancel")
    assert r.status_code == 200
    assert r.json()["ok"] is True

def test_costs_aggregation(client):
    r = client.get("/api/costs?days=30")
    assert r.status_code == 200
    body = r.json()
    assert "total_usd" in body
    assert isinstance(body["by_day"], list)
    assert isinstance(body["by_profile"], list)
```

### 10.2 Frontend (React / Vitest)

React components are tested with `vitest` + `@testing-library/react` + `msw` (Mock Service Worker) for API mocking.

Key test cases:
- `RunsTable` renders correct row count and status badges from fixture data.
- `RunsTable` highlights a row when a `run_update` SSE event is dispatched to `window`.
- `CostChart` renders without throwing when `by_day` is an empty array (no runs yet).
- `SpanWaterfall` correctly nests child spans under parents using `parent_id`.
- `QueueDAG` renders cancel button only for `queued` / `running` nodes.
- `useSSE` hook reconnects when `EventSource` emits an `error` event.
- `ProfileList` renders "No runs yet" placeholder when `run_count` is 0.

### 10.3 Integration tests

`tests/test_api_integration.py` runs `tag serve --no-browser` in a subprocess against a real `tag.sqlite3` fixture database, waits for the health endpoint to return 200, then exercises all read endpoints:

```python
def test_serve_integration(fixture_db, unused_port):
    proc = subprocess.Popen(
        ["python", "-m", "tag", "serve", "--port", str(unused_port), "--no-browser"],
        env={**os.environ, "TAG_DB": str(fixture_db)},
    )
    try:
        _wait_for_health(f"http://localhost:{unused_port}/api/health")
        r = requests.get(f"http://localhost:{unused_port}/api/runs")
        assert r.status_code == 200
        assert len(r.json()["items"]) > 0
    finally:
        proc.terminate()
        proc.wait(timeout=5)
```

---

## 11. Acceptance Criteria

| ID | Criterion | How to verify |
|----|-----------|--------------|
| AC-01 | `tag serve` starts without error and prints the dashboard URL | Run `tag serve --no-browser`; observe stdout |
| AC-02 | `GET /api/health` returns HTTP 200 with `{"status": "ok"}` within 500ms of server start | `curl http://localhost:8787/api/health` |
| AC-03 | `GET /api/runs` returns all runs from `tag.sqlite3` with correct pagination | Seed DB with 60 runs; `GET /api/runs?limit=10` returns 10 items and `total=60` |
| AC-04 | The React SPA loads at `http://localhost:8787/` without console errors | Open in Chrome; check DevTools console |
| AC-05 | RunsTable live-updates when a new run is inserted into the database while the dashboard is open | Start `tag loop`; observe RunsTable row appears within 2 seconds |
| AC-06 | `GET /api/spans?trace_id=<id>` returns spans with correct parent_id nesting | Inspect waterfall in browser; root span has no parent, children indent correctly |
| AC-07 | `GET /api/costs` returns `by_day` array with one entry per calendar day that has runs | Verify dates match actual run `created_at` dates in DB |
| AC-08 | `POST /api/queue/{id}/cancel` on a queued job sets its status to `cancelled` in the DB | Verify via `tag queue` CLI after cancellation |
| AC-09 | `tag serve` with no `--auth-token` on `localhost` does not require any auth header | `curl http://localhost:8787/api/runs` succeeds without `Authorization` header |
| AC-10 | `tag serve --host 0.0.0.0` without `--auth-token` prints an error and exits with code 1 | Run command; observe error message and non-zero exit |
| AC-11 | `tag serve --host 0.0.0.0 --auth-token secret` requires `Authorization: Bearer secret`; requests without it return 401 | `curl http://<lan-ip>:8787/api/runs` returns 401; with header returns 200 |
| AC-12 | `pip install tag-agent` followed by `tag serve --no-browser` serves the React UI without any npm step | Install in a fresh virtualenv; verify `GET /` returns HTML with `<div id="root">` |
| AC-13 | Process RSS stays under 50MB during idle operation with a 50,000-row spans table | Measure with `ps aux` after 60s idle |
| AC-14 | API responses do not contain `route_json`, `prompt`, `task`, `metadata_json`, or any field matching `*_key*` or `*token*` (secret tokens) | Inspect all endpoint responses in the integration test suite |

---

## 12. Dependencies

### 12.1 Python (new additions to `pyproject.toml`)

```toml
[project.optional-dependencies]
serve = [
    "fastapi>=0.111.0,<1.0",
    "uvicorn[standard]>=0.29.0,<1.0",
    "websockets>=12.0,<14.0",
]
```

The `serve` extras group is installable via `pip install tag-agent[serve]`. The base `tag-agent` package does not pull in FastAPI to avoid bloating the default install.

`cmd_serve` imports `fastapi` and `uvicorn` lazily (inside the function body) and prints a helpful message if they are not installed:

```python
def cmd_serve(args):
    try:
        import fastapi, uvicorn
    except ImportError:
        print("ERROR: Web dashboard requires extra dependencies.")
        print("  pip install tag-agent[serve]")
        return 1
    ...
```

### 12.2 JavaScript (new additions to `web/package.json`)

```json
{
  "dependencies": {
    "react": "^18.3.0",
    "react-dom": "^18.3.0",
    "react-router-dom": "^6.24.0",
    "recharts": "^2.12.0",
    "reactflow": "^11.11.0",
    "lucide-react": "^0.400.0"
  },
  "devDependencies": {
    "vite": "^5.3.0",
    "@vitejs/plugin-react": "^4.3.0",
    "vitest": "^1.6.0",
    "@testing-library/react": "^16.0.0",
    "@testing-library/user-event": "^14.5.0",
    "msw": "^2.3.0",
    "typescript": "^5.5.0"
  }
}
```

The existing `web/src/pages/McpPage.tsx` and `web/src/pages/ChannelsPage.tsx` reference `@nous-research/ui` components. The new TAG dashboard pages do **not** use `@nous-research/ui`; they use Tailwind CSS utility classes directly (already present in the repo) to avoid the external dependency.

---

## 13. Open Questions

| ID | Question | Options | Recommendation |
|----|---------|---------|---------------|
| OQ-01 | **React build bundling strategy** — should the compiled JS/CSS be committed to the repo, generated at `python -m build` time via a custom setuptools hook, or downloaded from a CDN at install time? | (a) Commit build artifacts to repo (simple, no CI complexity, inflates repo size by ~1MB); (b) Generate at build time via `hatch-npm-version` or custom `build.py` hook (clean repo, requires npm in CI); (c) CDN (never offline-compatible) | Recommend (a) for v1 — keeps installation simple and offline-capable; revisit for v2 with a Hatch build hook |
| OQ-02 | **SSE vs WebSocket for live stream** — should the primary live-update mechanism be SSE (`/api/stream`) or WebSocket (`/api/ws`)? | SSE: simpler, auto-reconnects, HTTP/2 compatible, no upgrade handshake; WebSocket: bidirectional, better for future write operations from the browser | Implement SSE as primary; WebSocket endpoint as optional secondary for power users; React UI uses SSE by default |
| OQ-03 | **Auth mechanism for SSE** — since `EventSource` does not support custom headers, should auth use query-param token, cookies, or a pre-flight token-exchange endpoint? | (a) Query param `?token=<value>` (simple, appears in server logs); (b) Cookie set by a `POST /api/auth` pre-flight (more browser-standard, no log exposure); (c) No auth on SSE (acceptable if SSE only sends non-sensitive delta fields) | Recommend (a) for v1; SSE events emit only IDs and status values (not prompts or keys), so log exposure risk is limited |
| OQ-04 | **Queue job cancel — SIGTERM vs DB flag** — should `POST /api/queue/{id}/cancel` send SIGTERM to `pid` (if running) or only set a DB flag for the worker to poll? | SIGTERM is immediate but requires the dashboard process to have permission to signal the worker PID; DB flag is safe but may have up to 5s delay | Implement DB flag first (safe, no permission issues); optionally SIGTERM the pid if `os.kill` succeeds without exception |
| OQ-05 | **Profile list source** — should `/api/profiles` read profile names from `cli-config.yaml` on disk or derive them from distinct `master_profile` values in the `runs` table? | Config file is authoritative but requires knowing the config path; DB is self-contained but may miss profiles with no runs yet | Read from config file at request time (re-read on every request, no caching, so new profiles appear immediately); fall back to DB-derived list if config file is not found |

---

## 14. Complexity and Timeline

**Overall complexity: L**

| Component | Complexity | Estimated effort |
|-----------|-----------|-----------------|
| FastAPI app (`api.py`), all read endpoints | M | 3–4 days |
| SSE polling generator | S | 1 day |
| Auth middleware + `cmd_serve` CLI wiring | S | 1 day |
| React scaffold + router + layout | S | 1 day |
| RunsTable + live SSE hook | M | 2 days |
| CostChart (recharts) | S | 1–2 days |
| SpanWaterfall (custom SVG) | M | 2–3 days |
| QueueDAG (react-flow) + cancel action | M | 2 days |
| ProfileList | S | 1 day |
| BenchmarksPage | S | 1 day |
| Vite build config + Python asset bundling | S | 1 day |
| Backend tests (TestClient) | S | 1–2 days |
| Frontend tests (Vitest + MSW) | S | 1–2 days |
| Integration test + CI wiring | S | 1 day |
| **Total** | **L** | **~3 sprints (6–7 weeks at 2 engineers)** |

Sprint breakdown:
- **Sprint 1** — FastAPI backend complete (all endpoints, SSE, auth, health), backend tests passing, `tag serve` CLI command wired.
- **Sprint 2** — React scaffold + RunsTable + CostChart + SpanWaterfall + useSSE hook; Vite build output committed to `src/tag/assets/web/`.
- **Sprint 3** — QueueDAG + cancel, ProfileList, BenchmarksPage, frontend tests, integration test, documentation update, security review.
