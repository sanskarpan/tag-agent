# PRD-021: Streaming TUI Dashboard (`tag serve` / `tag dashboard`)

**Status:** Proposed  
**Priority:** P1 (High Impact, Differentiating)  
**Estimated Effort:** L (TUI sub-feature: M ~1 sprint; Web bridge sub-feature: L ~2 sprints; total: 2–3 sprints)  
**Affects:** New `src/tag/dashboard.py`, new `src/tag/api.py`, `controller.py` (new subcommands `serve` and `dashboard`), `web/` directory  
**Depends on:** PRD-003 (Rich TUI, delivered), PRD-008 (Background Queue, delivered), PRD-012 (Cost Tracking), PRD-013 (Tracing / Spans)

---

## 1. Overview

TAG currently executes agents and emits all output in batch: Hermes runs, finishes, and the terminal shows results. There is no live view of what the agent is doing, how many tokens it has consumed, which tools it called, what is queued, or how much the run has cost so far. Users must wait until a run completes before they can learn anything about it.

This PRD defines a **Streaming TUI Dashboard** — a real-time terminal dashboard (and optional web bridge) that overlays every running Hermes agent session with:

- A live **token stream panel** showing model output as it arrives
- A **cost ticker** accumulating spend in real time
- A **tool call inspector** showing the last N tool invocations with arguments and results
- An **agent status panel** listing every active profile and its current state
- A **queue status panel** showing pending, running, and completed background jobs
- A **span waterfall** panel (when tracing is enabled) showing per-span latency bars

The terminal dashboard (`tag dashboard`) requires no server and no browser: it runs entirely in the terminal using Rich's `Live` layout. The web bridge (`tag serve`) starts a FastAPI server that exposes the same data over SSE and WebSocket so teammates and CI runners can observe agent activity through a browser.

---

## 2. Goals

1. **Real-time token streaming.** Model output tokens appear in the dashboard as they are produced by Hermes, not after the run completes. Target: first token visible within 200 ms of Hermes emitting it.
2. **Cost accumulation display.** A persistent cost ticker shows cumulative spend (input tokens × price + output tokens × price) updated every Hermes turn, using the model's published per-token rates.
3. **Tool call visualization.** Every tool call Hermes makes (shell, read_file, search, MCP tools, etc.) appears in a dedicated panel with the tool name, abbreviated arguments, and return status. Supports real-time inspection without interrupting the run.
4. **Multi-profile parallel view.** When multiple profiles are running simultaneously (e.g. a swarm or parallel `tag submit`), the dashboard shows each profile in its own labeled row in the agent status panel, with per-profile token and cost counters.
5. **Web bridge for non-terminal users.** `tag serve` starts a localhost FastAPI server with an SSE endpoint and a React SPA (the existing `web/` directory) so CI systems, remote operators, or teammates on the same machine can observe the same live data in a browser without SSH.
6. **Profile-filtered view.** `tag dashboard --profile <name>` restricts all panels to a single profile, reducing noise during focused debugging sessions.
7. **Graceful degradation.** If Hermes does not expose a streaming stdout hook, the dashboard falls back to polling the SQLite database (spans table from PRD-013, queue table from PRD-008) at a configurable interval, delivering a near-real-time experience even in batch mode.
8. **Zero new core dependencies for the TUI path.** `rich==14.3.3` is already a core dependency. The TUI dashboard (`tag dashboard`) requires no additional packages. The web path (`tag serve`) requires `fastapi`, `uvicorn`, and `websockets`, which are already declared in the `[web]` optional extra in `pyproject.toml`.

---

## 3. Non-Goals

- **Replacing the existing simple output mode.** `tag chat`, `tag submit`, and all other commands continue to work exactly as today. The dashboard is opt-in. `TAG_NO_COLOR=1` and non-TTY pipes are unaffected.
- **Mobile app.** The web bridge targets a desktop browser on localhost. A mobile-optimized responsive UI is a future enhancement.
- **Historical replay / post-mortem viewer.** Replaying a completed run's event stream from storage is a distinct feature with its own storage and scrubbing requirements. That belongs in a separate PRD. This PRD covers live sessions only.
- **Remote access over the internet.** `tag serve` binds to `localhost` by default. Making it reachable over the public internet (reverse proxy, tunneling, auth hardening) is out of scope.
- **Hermes internals modification.** This PRD does not modify Hermes agent code. It wraps Hermes's subprocess stdout/stderr and reads existing SQLite tables. If Hermes later exposes a native streaming hook, that is an upgrade, not a prerequisite.
- **Authentication beyond a session token.** The web bridge uses a single randomly-generated session token per server start. OAuth, SSO, and multi-user access control are out of scope.

---

## 4. User Stories

### US-1 — Watching a loop run's token stream live
> As a developer running `tag submit --loop "refactor all tests"`, I want to watch the model's output tokens appear in my terminal as they stream out of Hermes, so that I can abort the run early if the model is heading in the wrong direction without waiting for the full response.

**Acceptance:** `tag dashboard` is launched before or alongside `tag submit`. The token stream panel updates character-by-character (or in small chunks) as Hermes emits stdout. The user can press `q` to exit the dashboard without killing the agent.

---

### US-2 — Monitoring parallel swarm agents
> As a team lead running a 5-agent swarm (`tag swarm ...`), I want to see each agent's current status (thinking / calling tool / idle / error) in a single terminal screen, so that I can identify which agents are stuck or which ones have errored without tailing five separate log files.

**Acceptance:** The agent status panel shows one row per active profile. Each row displays: profile name, current state label, elapsed time for the current turn, and cumulative token count. Rows update in real time.

---

### US-3 — Seeing per-turn cost accumulate
> As a cost-conscious user running an automated coding task overnight, I want to see the cumulative cost of the current run tick upward in real time, and receive a visual warning if it crosses a configurable threshold, so that I can kill runaway sessions before they accrue large charges.

**Acceptance:** The cost ticker panel shows: current turn cost, session cumulative cost, and (if PRD-012 budget is configured) percentage of budget consumed. The ticker turns yellow at 80% of budget and red at 100%. The user can configure the threshold via `~/.tag/config.yaml` under `dashboard.budget_warn_usd`.

---

### US-4 — Inspecting tool arguments in real time
> As a developer debugging a tool-heavy coding agent, I want to see each tool call's name, arguments (truncated to fit the panel), and return status as it happens, so that I can spot incorrect tool usage (wrong file path, bad arguments) without reading the full transcript after the run.

**Acceptance:** The tool calls panel shows a scrollable ring buffer of the last 20 tool invocations. Each entry shows: timestamp, tool name, abbreviated args (max 80 chars), and a status badge (pending / ok / error). Entries are color-coded: green for ok, red for error, yellow for pending.

---

### US-5 — Sharing a dashboard URL with a teammate
> As a pair programmer, I want to start `tag serve` on my laptop and send a URL to my teammate so they can watch my agent run in their browser (on the same network) without needing to SSH into my machine or install TAG, so that we can collaborate on live debugging.

**Acceptance:** `tag serve --host 0.0.0.0` starts the server bound to all interfaces. The startup output prints a URL with a one-time session token parameter (`?token=<hex>`). Opening that URL in a browser on the same network shows the React SPA with live data from the SSE/WebSocket endpoints. The session token is required; requests without it receive HTTP 401.

---

### US-6 — Filtering to one profile during focused debugging
> As a developer who has three profiles running simultaneously, I want to open a second terminal with `tag dashboard --profile coder` to see only the `coder` profile's token stream and tool calls, so that I can focus on debugging that specific agent without the noise of the other two.

**Acceptance:** `tag dashboard --profile coder` renders all panels filtered to the `coder` profile. The header shows `[profile: coder]`. Other profiles' events are silently dropped from all panels.

---

## 5. Proposed CLI Surface

### 5.1 `tag serve` — Web bridge + optional TUI

```
tag serve [--port 8787] [--host localhost] [--no-browser]
          [--tls-cert PATH --tls-key PATH]
          [--profile PROFILE_NAME]
          [--token TOKEN]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--port` | `8787` | TCP port to bind. |
| `--host` | `localhost` | Bind address. Use `0.0.0.0` for LAN access. |
| `--no-browser` | off | Suppress automatic browser open on start. |
| `--tls-cert` | — | Path to TLS certificate PEM file. Enables HTTPS. |
| `--tls-key` | — | Path to TLS private key PEM file. Required with `--tls-cert`. |
| `--profile` | all profiles | Filter all endpoints to a single profile. |
| `--token` | auto-generated | Provide a fixed session token instead of generating one. Useful for scripted access. |

On start, prints:
```
TAG dashboard running at http://localhost:8787/?token=<hex32>
Press Ctrl-C to stop.
```

---

### 5.2 `tag dashboard` — Pure terminal TUI (no server)

```
tag dashboard [--profile PROFILE_NAME]
              [--refresh-ms 200]
              [--no-cost]
              [--no-tools]
              [--no-queue]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--profile` | all profiles | Filter all panels to a single profile name. |
| `--refresh-ms` | `200` | Dashboard refresh interval in milliseconds. Lower values use more CPU. |
| `--no-cost` | off | Hide the cost ticker panel (useful in narrow terminals). |
| `--no-tools` | off | Hide the tool calls panel. |
| `--no-queue` | off | Hide the queue status panel. |

Keybindings while the dashboard is live:
- `q` / `Ctrl-C` — exit the dashboard (does not kill running agents)
- `↑` / `↓` — scroll the token stream panel
- `t` — toggle the tool calls panel
- `c` — toggle the cost ticker
- `s` — toggle span waterfall (requires PRD-013)

---

## 6. Functional Requirements

### FR-01 — Rich Live Layout
The terminal dashboard MUST use `rich.live.Live` with a `rich.layout.Layout` tree. The root layout MUST be structured as:

```
┌─────────────────────── HEADER ───────────────────────────┐
│  TAG Dashboard  |  profile: all  |  session: 2m 14s      │
├──────────────────────┬───────────────────────────────────┤
│   AGENT STATUS       │         TOKEN STREAM              │
│  (left, 1/3 width)   │       (right, 2/3 width)         │
├──────────┬───────────┴──────────────────────────────────┤
│  COST    │           TOOL CALLS                          │
│  TICKER  │           (scrollable ring buffer)            │
├──────────┴──────────────────────────────────────────────┤
│                    QUEUE STATUS                           │
└──────────────────────────────────────────────────────────┘
```

When span waterfall is enabled (key `s`), a SPANS panel replaces the bottom half of the body.

---

### FR-02 — Agent Status Panel
The agent status panel MUST display one row per active agent profile. Each row MUST include:
- Profile name (bold, colored by profile index)
- Current state: `thinking` / `tool-call` / `idle` / `error` / `done`
- Elapsed time for current turn (live counter)
- Cumulative tokens this session (in + out)
- Model ID (abbreviated to 20 chars)

Rows MUST update on every dashboard refresh cycle. Profiles that have been idle for more than 60 seconds MUST be shown in dim style. Profiles with `error` state MUST be shown in red.

---

### FR-03 — Token Stream Panel
The token stream panel MUST display model output text as it arrives from Hermes stdout capture. It MUST:
- Use a ring buffer of configurable size (default: 2000 characters, configurable via `dashboard.token_ring_buffer_chars` in `~/.tag/config.yaml`).
- Auto-scroll to the bottom as new tokens arrive.
- Support manual scroll-up (`↑`) without interrupting the live update.
- Show a `[PROFILE: name]` label at the start of each new agent response.
- Render model Markdown as Rich markup (bold, italic, code blocks via `rich.markdown.Markdown`).

---

### FR-04 — Cost Ticker Panel
The cost ticker MUST:
- Compute cost as `(input_tokens × input_price_per_1k) + (output_tokens × output_price_per_1k)` using a bundled model price table in `src/tag/dashboard.py`.
- Update on every turn completion event received from the event queue.
- Display: current-turn cost, session cumulative cost, and (if PRD-012 budget is configured) budget remaining.
- Display currency as USD with 4 decimal places (e.g. `$0.0034`).
- Flash yellow when session cost crosses 80% of configured budget; flash red at 100%.
- Persist the session cumulative cost across dashboard restarts by reading the spans table (PRD-013) on startup.

---

### FR-05 — Tool Calls Panel
The tool calls panel MUST maintain a ring buffer of the last 20 tool invocations. Each entry MUST show:
- Wall-clock timestamp (HH:MM:SS)
- Tool name (e.g. `shell`, `read_file`, `mcp:brave_search`)
- Arguments preview: first 80 chars of the JSON-serialized args, truncated with `…`
- Status badge: `PENDING` (yellow), `OK` (green), `ERROR` (red)
- Duration (ms) for completed calls

Tool call events MUST be sourced from the hermes event queue (FR-10). If hermes does not expose structured tool call events, the panel MUST fall back to parsing stdout lines matching the pattern `[tool_call]` or JSON lines with `"type": "tool_use"`.

---

### FR-06 — Queue Status Panel
The queue status panel MUST read the `queue_jobs` SQLite table (defined by PRD-008) and display:
- Count of jobs by status: `queued`, `running`, `done`, `failed`
- For each `running` job: job ID (first 8 chars), profile name, elapsed time, PID
- For each `queued` job: job ID, profile name, prompt preview (first 60 chars)
- Last 3 completed jobs with final status and duration

Queue data MUST be refreshed from SQLite on each dashboard refresh cycle (not cached).

---

### FR-07 — Span Waterfall Panel (optional, requires PRD-013)
When toggled with `s`, the bottom section of the layout MUST be replaced by a waterfall chart. Each row is one span from the current trace. The waterfall MUST:
- Show span name, duration bar scaled to the longest span, and numeric duration (ms).
- Use indentation to show parent-child relationships.
- Update as new spans are written to the SQLite spans table.
- Be disabled (and the toggle key `s` no-ops) if the spans table does not exist.

---

### FR-08 — FastAPI Application (`src/tag/api.py`)
`tag serve` MUST start a FastAPI application (`src/tag/api.py`) with the following HTTP endpoints:

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/` | Serves the React SPA `index.html` from `web/dist/`. |
| `GET` | `/api/runs` | Returns JSON list of active and recent agent runs. |
| `GET` | `/api/spans` | Returns JSON list of spans for the current or most recent trace. |
| `GET` | `/api/costs` | Returns JSON cost summary: per-profile and session totals. |
| `GET` | `/api/queue` | Returns JSON queue status (mirrors FR-06 data). |
| `GET` | `/api/stream` | SSE endpoint: emits newline-delimited JSON events as they occur. |
| `WebSocket` | `/api/ws` | WebSocket endpoint: same event stream as `/api/stream`, bidirectional. |
| `GET` | `/api/health` | Returns `{"status": "ok", "version": "<tag version>"}`. No auth required. |

All endpoints EXCEPT `/api/health` and `GET /` (static) MUST require the session token via `Authorization: Bearer <token>` header or `?token=<token>` query parameter.

---

### FR-09 — SSE Event Schema
Events emitted on `/api/stream` and `/api/ws` MUST follow this schema:

```json
{
  "type": "<event_type>",
  "ts": "<ISO-8601 UTC>",
  "profile": "<profile_name_or_null>",
  "data": { ... }
}
```

Event types:
- `token` — `data: {"text": "...", "delta": true}`
- `tool_call` — `data: {"tool": "...", "args": {...}, "status": "pending|ok|error", "duration_ms": N}`
- `turn_end` — `data: {"input_tokens": N, "output_tokens": N, "cost_usd": N}`
- `agent_state` — `data: {"state": "thinking|tool-call|idle|error|done"}`
- `queue_update` — `data: {"running": N, "queued": N, "done": N, "failed": N}`
- `cost_update` — `data: {"session_usd": N, "turn_usd": N, "budget_usd": N|null}`
- `span` — `data: {"span_id": "...", "name": "...", "duration_ms": N, "status": "ok|error"}`

---

### FR-10 — Hermes Stdout Capture and Event Queue
The dashboard MUST bridge between Hermes's subprocess stdout and the event consumers (Rich panels and API SSE). The bridge MUST:
- Wrap each Hermes subprocess with a `stdout=subprocess.PIPE` capture.
- Feed captured lines into a `queue.Queue` (Python stdlib, thread-safe) shared between the Rich Live updater thread and the FastAPI event loop.
- Parse JSON lines from Hermes stdout: if a line is valid JSON with a `type` field, emit it as a structured event; otherwise emit it as a raw `token` event.
- Use a secondary SQLite polling path (polling the spans and queue tables every `--refresh-ms` ms) as a fallback when structured JSON events are absent.
- The queue MUST be bounded: `maxsize=10000`. When the queue is full, new events MUST be dropped (not block the Hermes subprocess).

---

### FR-11 — Multi-Agent Parallel View
When multiple profiles are running simultaneously (swarm or parallel submit), the dashboard MUST:
- Show one row per profile in the agent status panel.
- Interleave token stream output from all profiles, labeled with `[PROFILE: name]` separators.
- Aggregate cost across all profiles in the cost ticker.
- Show all profiles' tool calls in the tool calls panel, with a `[profile]` column.

The `--profile` flag restricts all of the above to a single profile.

---

### FR-12 — Auto-Refresh Rate
The Rich `Live` context MUST refresh at the rate specified by `--refresh-ms` (default 200 ms, minimum 50 ms, maximum 2000 ms). The FastAPI SSE endpoint MUST flush events as they arrive (not batch them by refresh rate). The WebSocket endpoint MUST send events immediately on receipt.

---

### FR-13 — Graceful Shutdown
On `Ctrl-C` or `SIGTERM`:
- The Rich `Live` context MUST be exited cleanly (calling `live.stop()`) so the terminal is restored to normal state.
- The FastAPI server MUST send a `{"type": "server_shutdown"}` event on all open SSE and WebSocket connections before closing.
- Running agent subprocesses MUST NOT be killed. The dashboard is a viewer, not a controller.
- The event queue MUST be drained (up to 1 second timeout) before the process exits.

---

### FR-14 — web/ React Frontend
The `web/` directory MUST contain a React SPA served by `tag serve`. The SPA MUST:
- Connect to `/api/ws` using the native browser WebSocket API.
- Render the same four panels as the terminal dashboard: agent status, token stream, cost ticker, tool calls.
- Reconnect automatically on WebSocket disconnect (exponential backoff: 1s, 2s, 4s, max 30s).
- Display a connection status badge: `LIVE` (green), `RECONNECTING` (yellow), `DISCONNECTED` (red).
- Accept the session token via the `?token=` URL query parameter and pass it in all API requests.
- Be built to `web/dist/` via `npm run build` (or equivalent). The `tag serve` command MUST serve from `web/dist/` if it exists, or return a 404 with a helpful message ("Run `npm run build` in the `web/` directory to build the frontend") if it does not.

---

### FR-15 — Profile-Filtered Startup
`tag dashboard --profile <name>` and `tag serve --profile <name>` MUST:
- Validate that the named profile exists in `~/.tag/config.yaml` at startup. If it does not, exit with a clear error message.
- Pass the profile filter to the event queue consumer so only events for that profile are relayed to panels and API endpoints.
- Include the profile name in the dashboard header and all SSE events.

---

## 7. Non-Functional Requirements

### NFR-01 — Dashboard Refresh Latency
The Rich `Live` layout MUST refresh within 500 ms of an event arriving in the queue. At the default 200 ms refresh rate, this provides margin for one missed cycle. Measured from Hermes stdout emission to panel update.

### NFR-02 — SSE Reconnect on Disconnect
The SSE endpoint MUST support the `Last-Event-ID` HTTP header. When a client reconnects with a `Last-Event-ID`, the server MUST replay any events from the ring buffer that occurred after that ID. The replay buffer MUST hold the last 500 events.

### NFR-03 — Localhost-Only Default
`tag serve` MUST bind to `127.0.0.1` by default. Binding to `0.0.0.0` or any external interface MUST require the explicit `--host 0.0.0.0` flag. On startup with a non-localhost host, the CLI MUST print a warning: `WARNING: Dashboard is accessible on all network interfaces. Ensure firewall rules are in place.`

### NFR-04 — Memory Bounded Ring Buffer
The token stream panel's ring buffer MUST be bounded. Default size: 2000 characters. The tool calls panel ring buffer MUST be bounded to 20 entries. The SSE replay buffer MUST be bounded to 500 events. All three limits MUST be configurable in `~/.tag/config.yaml` under `dashboard.*`.

### NFR-05 — No Credential Exposure
Token stream text MUST NOT be scanned or filtered for credentials. However, the tool calls panel MUST redact argument values for tools named `set_secret`, `store_credential`, or any tool whose name contains `secret`, `password`, `token`, or `key` (case-insensitive), replacing the argument value with `[REDACTED]`.

### NFR-06 — CPU Usage
The terminal dashboard MUST NOT exceed 5% sustained CPU on a single core when idle (no active Hermes processes). The Rich `Live` refresh MUST use a blocking `queue.get(timeout=refresh_interval)` call rather than a busy-wait loop.

### NFR-07 — Terminal Restoration
If the dashboard process crashes (unhandled exception), the terminal MUST be restored to a usable state. The `rich.live.Live` context manager handles this; the implementation MUST ensure `live.stop()` is called in a `finally` block, not only on clean exit.

### NFR-08 — Python 3.11+ Compatibility
All new code MUST be compatible with Python 3.11–3.13 (per `pyproject.toml` `requires-python`). No Python 3.12+ syntax (e.g. `type` statement) unless guarded by a version check.

---

## 8. Technical Design

### 8.1 New Files

| File | Purpose |
|------|---------|
| `src/tag/dashboard.py` | Rich Live layout, panel renderers, event queue consumer, `cmd_dashboard()` entry point |
| `src/tag/api.py` | FastAPI application, SSE endpoint, WebSocket endpoint, REST endpoints, session token auth |

Existing files modified:
- `src/tag/controller.py` — add `cmd_serve()` and `cmd_dashboard()` dispatch, new argparse subcommands `serve` and `dashboard`
- `src/tag/queue_worker.py` — no changes required (dashboard reads the existing `queue_jobs` table)
- `src/tag/tracing.py` — no changes required (dashboard reads the existing spans table)

---

### 8.2 Rich Layout Structure (`src/tag/dashboard.py`)

```python
from rich.layout import Layout
from rich.live import Live
from rich.panel import Panel
from rich.table import Table
from rich.text import Text
import queue, threading

EVENT_QUEUE: queue.Queue = queue.Queue(maxsize=10_000)

def make_layout() -> Layout:
    layout = Layout(name="root")
    layout.split_column(
        Layout(name="header", size=3),
        Layout(name="body"),
        Layout(name="footer", size=6),
    )
    layout["body"].split_row(
        Layout(name="agent_status", ratio=1),
        Layout(name="token_stream", ratio=2),
    )
    layout["footer"].split_row(
        Layout(name="cost_ticker", ratio=1),
        Layout(name="tool_calls", ratio=3),
    )
    return layout

def run_dashboard(profile: str | None, refresh_ms: int, ...) -> None:
    layout = make_layout()
    state = DashboardState(profile_filter=profile)
    with Live(layout, refresh_per_second=1000 // refresh_ms, screen=True) as live:
        try:
            _consume_events(layout, state, live)
        finally:
            live.stop()
```

`DashboardState` is a dataclass holding:
- `agents: dict[str, AgentRow]` — per-profile state
- `token_ring: collections.deque[str]` — bounded token buffer
- `tool_calls: collections.deque[ToolCallEntry]` — bounded tool call buffer
- `session_cost_usd: float`
- `queue_summary: QueueSummary`
- `spans: list[Span]` — from PRD-013

---

### 8.3 Hermes Stdout Capture and Relay

TAG spawns Hermes as a subprocess. The bridge:

```python
import subprocess, threading, queue

def _capture_hermes_stdout(proc: subprocess.Popen, eq: queue.Queue, profile: str) -> None:
    """Runs in a daemon thread. Reads Hermes stdout line by line."""
    for raw_line in proc.stdout:
        line = raw_line.decode("utf-8", errors="replace").rstrip()
        try:
            event = json.loads(line)
            event.setdefault("profile", profile)
            event.setdefault("ts", _utc_now())
        except json.JSONDecodeError:
            event = {"type": "token", "profile": profile,
                     "ts": _utc_now(), "data": {"text": line + "\n", "delta": True}}
        try:
            eq.put_nowait(event)
        except queue.Full:
            pass  # drop on overflow; never block Hermes
```

This thread is started immediately after `subprocess.Popen` in `controller.py`'s run path. The `EVENT_QUEUE` module-level singleton is shared between the capture thread, the dashboard consumer thread, and the FastAPI background task.

---

### 8.4 FastAPI Application (`src/tag/api.py`)

```python
from fastapi import FastAPI, Depends, HTTPException, WebSocket
from fastapi.responses import StreamingResponse
from fastapi.staticfiles import StaticFiles
import asyncio, json

app = FastAPI(title="TAG Dashboard API")

# Auth dependency
def require_token(token: str = Query(None), authorization: str = Header(None)):
    supplied = token or (authorization.removeprefix("Bearer ") if authorization else None)
    if supplied != SESSION_TOKEN:
        raise HTTPException(status_code=401, detail="Invalid or missing token")

@app.get("/api/stream")
async def sse_stream(dep=Depends(require_token)):
    async def generator():
        async_q = asyncio.Queue()
        _register_async_consumer(async_q)
        try:
            while True:
                event = await asyncio.wait_for(async_q.get(), timeout=30)
                eid = event.get("id", "")
                yield f"id: {eid}\ndata: {json.dumps(event)}\n\n"
        except asyncio.TimeoutError:
            yield "data: {\"type\":\"heartbeat\"}\n\n"
        finally:
            _unregister_async_consumer(async_q)
    return StreamingResponse(generator(), media_type="text/event-stream",
                             headers={"Cache-Control": "no-cache",
                                      "X-Accel-Buffering": "no"})

@app.websocket("/api/ws")
async def ws_endpoint(websocket: WebSocket):
    token = websocket.query_params.get("token")
    if token != SESSION_TOKEN:
        await websocket.close(code=4001)
        return
    await websocket.accept()
    async_q = asyncio.Queue()
    _register_async_consumer(async_q)
    try:
        while True:
            event = await asyncio.wait_for(async_q.get(), timeout=30)
            await websocket.send_text(json.dumps(event))
    except (asyncio.TimeoutError, Exception):
        pass
    finally:
        _unregister_async_consumer(async_q)
        await websocket.close()
```

A thread-safe bridge pushes events from the stdlib `queue.Queue` (fed by capture threads) into all registered `asyncio.Queue` instances (one per SSE/WS consumer) using `loop.call_soon_threadsafe`.

---

### 8.5 FastAPI REST Endpoints (summary)

- `GET /api/runs` — queries the SQLite spans table for distinct `trace_id` values, returns list of `{trace_id, profile, started_at, status, token_count, cost_usd}`.
- `GET /api/spans` — returns all spans for a given `?trace_id=` query param (or the most recent trace if omitted).
- `GET /api/costs` — aggregates `prompt_tokens` and `completion_tokens` from the spans table, applies model price table, returns `{per_profile: {...}, session_total_usd: N}`.
- `GET /api/queue` — queries the `queue_jobs` table, returns `{running: [...], queued: [...], recent: [...]}`.

All four endpoints return JSON and are protected by the session token.

---

### 8.6 web/ React Frontend

The `web/` directory MUST contain a Vite + React project. It connects to `/api/ws` and renders:
- `<AgentStatusTable />` — one row per profile, live state badge
- `<TokenStream />` — scrollable textarea-like component, appends incoming `token` events
- `<CostTicker />` — live cumulative USD display with color-coded budget bar
- `<ToolCallsPanel />` — scrollable list of tool call entries

State management: React `useReducer` or Zustand. WebSocket messages dispatch typed actions into the store.

Build output goes to `web/dist/`. The `tag serve` FastAPI app mounts `StaticFiles(directory="web/dist")` at `/`.

---

## 9. Security Considerations

### SC-01 — Localhost-Only Default
`tag serve` MUST bind to `127.0.0.1` by default (NFR-03). This prevents unintentional LAN exposure. The `--host` flag MUST require the user to explicitly opt in to broader binding.

### SC-02 — Session Token Authentication
A cryptographically random 32-byte session token MUST be generated at server startup using `secrets.token_hex(32)`. All API endpoints (except `/api/health`) MUST reject requests without a valid token with HTTP 401. The token is printed once at startup and is never stored to disk in plaintext.

### SC-03 — WebSocket Origin Checking
The WebSocket endpoint MUST validate the `Origin` header against an allowlist. By default, the allowlist is `["http://localhost:8787", "http://127.0.0.1:8787"]`. When `--host 0.0.0.0` is used, a warning is printed and the origin check is relaxed to allow any origin, but the token check remains mandatory.

### SC-04 — SSE Rate Limiting
The SSE endpoint MUST limit concurrent connections to 10 (configurable via `dashboard.max_sse_connections`). Additional connections MUST receive HTTP 429 with a `Retry-After: 5` header. This prevents accidental resource exhaustion from reconnect storms.

### SC-05 — No Credential Exposure in Stream
Tool call arguments for credential-adjacent tools MUST be redacted in all outputs: the Rich panel, the SSE stream, the WebSocket stream, and the REST API responses. See NFR-05 for the redaction rule.

### SC-06 — TLS Support
When `--tls-cert` and `--tls-key` are provided, uvicorn MUST be started with `ssl_certfile` and `ssl_keyfile`. The server MUST refuse to start if only one of the two is provided (both required or neither). This allows localhost TLS for browser environments that require HTTPS for certain APIs.

### SC-07 — No Sensitive Data in Logs
The session token MUST NOT appear in uvicorn access logs. The startup message prints the token once to the user's terminal (stderr) and never to any log file.

### SC-08 — CORS Policy
FastAPI's `CORSMiddleware` MUST be configured with `allow_origins` matching the session token allowlist. Cross-origin requests from arbitrary origins MUST be rejected with HTTP 403. This applies even when `--host 0.0.0.0` is used.

### SC-09 — Input Validation on WebSocket Messages
If the WebSocket endpoint accepts incoming messages in the future (bidirectional use), all incoming JSON MUST be validated against a strict schema. Unrecognized message types MUST be silently dropped, not executed. (Current scope is read-only; this is a forward-looking guard.)

---

## 10. Testing Strategy

### Unit Tests

**Rich layout rendering (`tests/test_dashboard_layout.py`)**
- Instantiate `make_layout()` and assert the layout tree has the expected named sections.
- Instantiate `DashboardState` and call each panel renderer (`render_agent_status()`, `render_token_stream()`, etc.) with mock state; assert the returned Rich `Renderable` has expected text content.
- Test ring buffer overflow: feed 3000 characters into a 2000-char ring buffer and assert the oldest chars are dropped.
- Test cost computation: assert `compute_cost("claude-opus-4-5", input_tokens=1000, output_tokens=500)` returns the expected float given a mock price table.
- Test credential redaction: assert tool call args for a tool named `set_secret` are redacted in the rendered panel.

**SSE event tests (`tests/test_api_sse.py`)**
- Use FastAPI's `TestClient` with `stream=True` to connect to `/api/stream`.
- Assert that without a token, the endpoint returns HTTP 401.
- Assert that with a valid token, the endpoint returns `Content-Type: text/event-stream`.
- Push a mock event into the event queue and assert it appears in the SSE stream within 1 second.
- Assert `Last-Event-ID` replay: push 5 events, connect with `Last-Event-ID: 3`, assert events 4 and 5 are replayed.
- Assert heartbeat: if no events arrive for 30 seconds, a `{"type":"heartbeat"}` event is sent.

**WebSocket tests (`tests/test_api_ws.py`)**
- Use FastAPI's `TestClient` WebSocket support.
- Assert that connecting without a token closes with code 4001.
- Assert that connecting with a valid token receives events pushed to the queue.
- Assert that on server shutdown, a `{"type":"server_shutdown"}` event is sent before the connection closes.

**Concurrent stream tests (`tests/test_api_concurrent.py`)**
- Open 10 simultaneous SSE connections and assert all receive the same event.
- Open an 11th connection and assert it receives HTTP 429.
- Verify that a slow consumer (delayed `.read()`) does not block a fast consumer.

### Integration Tests (tagged `@pytest.mark.integration`)

- Start `tag serve` as a subprocess, wait for startup, fetch `/api/health`, assert `200 OK`.
- Run a short `tag chat` command alongside `tag serve`, assert that token events appear on the SSE stream.

---

## 11. Acceptance Criteria

| ID | Criterion | Testable via |
|----|-----------|-------------|
| AC-01 | `tag dashboard` starts without error when no Hermes processes are running; shows empty panels. | Manual / `pytest` |
| AC-02 | `tag dashboard` shows token stream output within 500 ms of Hermes emitting a line to stdout. | Integration test (measure latency) |
| AC-03 | `tag dashboard --profile X` shows only events from profile `X`. | Unit test (DashboardState filter) |
| AC-04 | `tag dashboard --profile NONEXISTENT` exits with a non-zero code and a clear error message. | `pytest` subprocess |
| AC-05 | The cost ticker updates after each `turn_end` event with the correct USD value (±0.0001). | Unit test (mock event) |
| AC-06 | The tool calls panel shows `[REDACTED]` for args of a tool named `set_secret`. | Unit test |
| AC-07 | `tag serve` starts and `/api/health` returns `{"status": "ok"}` within 3 seconds of process start. | `pytest` subprocess |
| AC-08 | `GET /api/stream` without a token returns HTTP 401. | Unit test (TestClient) |
| AC-09 | `GET /api/stream` with a valid token streams events in SSE format. | Unit test (TestClient) |
| AC-10 | `WebSocket /api/ws` without a token closes with code 4001. | Unit test (TestClient) |
| AC-11 | On `Ctrl-C`, the terminal is fully restored (no broken cursor, no alternate screen). | Manual verification |
| AC-12 | `tag serve` bound to `localhost` does not accept TCP connections from `127.0.0.2` or external IPs. | Integration (network) |
| AC-13 | The ring buffer for the token stream never exceeds the configured character limit. | Unit test (overflow test) |
| AC-14 | Opening 11 SSE connections returns HTTP 429 for the 11th. | Unit test |
| AC-15 | `tag dashboard` exits with `q` without killing any running agent subprocesses. | Manual / integration |

---

## 12. Dependencies

### Core (already installed, no new packages required for TUI path)
- `rich==14.3.3` — already a core dependency in `pyproject.toml`. `rich.live.Live`, `rich.layout.Layout`, `rich.panel.Panel`, `rich.table.Table`, `rich.markdown.Markdown`.

### Web path (already declared in `pyproject.toml [web]` extra)
- `fastapi==0.133.1` — REST API, SSE via `StreamingResponse`, static file serving.
- `uvicorn[standard]==0.41.0` — ASGI server with WebSocket support.
- `starlette==1.0.1` — transitive; pinned for CVE-2026-48710.

The `websockets` package is pulled in by `uvicorn[standard]`. No additional package entries are needed in `pyproject.toml`.

### Frontend (`web/` directory)
- `react` + `react-dom` (any 18.x)
- `vite` (build tool)
- No runtime CDN fetches; all dependencies bundled at build time.

### Indirect dependencies (already present)
- Python `stdlib`: `queue`, `threading`, `asyncio`, `secrets`, `json`, `subprocess`, `sqlite3`, `collections.deque`
- `src/tag/tracing.py` — spans table schema (PRD-013 must be shipped or the span waterfall panel is simply disabled)
- `src/tag/queue_worker.py` — `queue_jobs` table schema (PRD-008; if not present, the queue panel shows "Queue not available")

---

## 13. Open Questions

### OQ-01 — Hermes Streaming API Availability
Does the current Hermes release emit structured JSON lines (e.g. `{"type": "token", "text": "..."}`) to stdout, or only raw text? If raw text only, the dashboard will work but token attribution per-profile will be unreliable in multi-agent scenarios. **Resolution needed:** inspect Hermes stdout in a live session and document the actual output format before implementing FR-10.

### OQ-02 — SSE vs WebSocket as Primary Web Protocol
The web frontend could use SSE (simpler, HTTP/1.1 compatible, auto-reconnect in browser) or WebSocket (bidirectional, more compatible with future control features). Current PRD specifies both. **Decision needed:** should the React frontend default to WebSocket (for future bidirectionality) or SSE (simpler)? Recommendation: WebSocket primary, SSE as fallback for clients behind HTTP/1.1 proxies.

### OQ-03 — Auth Mechanism for Web Dashboard on LAN
When `--host 0.0.0.0` is used (LAN sharing), a single shared session token printed to stdout is not secure for shared environments. Should we support: (a) a time-limited OTP link, (b) basic auth, or (c) a separate `--auth-mode` flag? **Resolution needed before shipping LAN mode.**

### OQ-04 — Cost Ticker Price Table Maintenance
The bundled model price table in `src/tag/dashboard.py` will go stale as Anthropic and OpenAI update pricing. Options: (a) hard-code a static table and accept staleness, (b) fetch prices from a known endpoint at startup (requires network, may fail), (c) read prices from a user-configurable `~/.tag/model_prices.yaml`. Recommendation: ship a static table as default, support override via `~/.tag/model_prices.yaml`.

### OQ-05 — Dashboard Availability During `tag chat` Interactive Mode
`tag chat` runs in an interactive REPL using `prompt_toolkit`. Running `tag dashboard` simultaneously in a second terminal should work (it reads the same SQLite DB and event queue). However, if `tag chat` is later modified to feed the event queue directly (single process), the dashboard could run in-process. Which architecture should we target?

### OQ-06 — `web/` Build Step in CI
The `web/dist/` directory is either (a) committed to git and kept in sync, (b) built as part of `tag serve` startup (slow), or (c) built in a separate CI step and included in the wheel via `package-data`. Option (c) is the right answer for PyPI distribution but requires a Node.js build step in the release pipeline. **Resolution needed before the web feature ships.**

---

## 14. Complexity & Timeline

**Overall estimate:** L — 2–3 sprints

| Sub-feature | Effort | Sprint |
|-------------|--------|--------|
| Rich Live layout scaffolding (`dashboard.py` skeleton, `make_layout()`, empty panels) | S | Sprint 1 |
| Hermes stdout capture bridge + event queue (`_capture_hermes_stdout`, `EVENT_QUEUE`) | M | Sprint 1 |
| Agent status panel + token stream panel (FR-02, FR-03) | M | Sprint 1 |
| Cost ticker panel (FR-04, price table) | S | Sprint 1 |
| Tool calls panel with redaction (FR-05, SC-05) | S | Sprint 1 |
| Queue status panel (FR-06, SQLite reads) | S | Sprint 1 |
| `tag dashboard` CLI integration (`controller.py` dispatch, argparse) | S | Sprint 1 |
| **Sprint 1 total** | **M** | |
| FastAPI app scaffolding (`api.py`, session token, `/api/health`) | S | Sprint 2 |
| SSE endpoint with replay buffer (FR-08, FR-09, FR-12, NFR-02) | M | Sprint 2 |
| WebSocket endpoint (FR-08, SC-03) | S | Sprint 2 |
| REST endpoints: `/api/runs`, `/api/spans`, `/api/costs`, `/api/queue` | M | Sprint 2 |
| `tag serve` CLI integration + auto-browser open | S | Sprint 2 |
| Security hardening (SC-01–SC-08) | S | Sprint 2 |
| Unit tests: layout rendering, SSE, WebSocket, concurrent (FR-10 test suite) | M | Sprint 2 |
| **Sprint 2 total** | **L** | |
| React SPA (`web/` Vite project, AgentStatusTable, TokenStream, CostTicker, ToolCallsPanel) | M | Sprint 3 |
| WebSocket state management in React | S | Sprint 3 |
| Span waterfall panel (FR-07, requires PRD-013 spans table) | M | Sprint 3 |
| Integration tests + CI Node.js build step | S | Sprint 3 |
| Documentation: `tag dashboard --help`, `tag serve --help`, README section | XS | Sprint 3 |
| **Sprint 3 total** | **M** | |

**Total:** Sprint 1 (M) + Sprint 2 (L) + Sprint 3 (M) = **L overall**

Sprint 1 is deliverable as a standalone TUI-only feature without any web dependency. Sprint 2 can be reviewed independently as the web API layer. Sprint 3 completes the full web experience. This phasing allows the TUI dashboard to ship early while the web frontend is developed.

---

## Appendix A — Model Price Table (initial values for `src/tag/dashboard.py`)

```python
# Prices in USD per 1,000 tokens. Last updated: 2026-06.
# Override via ~/.tag/model_prices.yaml
MODEL_PRICES: dict[str, dict[str, float]] = {
    "claude-opus-4-5":        {"input": 0.015,   "output": 0.075},
    "claude-sonnet-4-5":      {"input": 0.003,   "output": 0.015},
    "claude-haiku-3-5":       {"input": 0.00025, "output": 0.00125},
    "gpt-4o":                 {"input": 0.005,   "output": 0.015},
    "gpt-4o-mini":            {"input": 0.00015, "output": 0.0006},
    "gpt-4.1":                {"input": 0.002,   "output": 0.008},
    "o3":                     {"input": 0.010,   "output": 0.040},
    "o4-mini":                {"input": 0.0011,  "output": 0.0044},
    # Fallback for unknown models
    "_default":               {"input": 0.005,   "output": 0.015},
}
```

---

## Appendix B — Event Queue Architecture Diagram (ASCII)

```
Hermes subprocess (stdout) ──► _capture_hermes_stdout() [daemon thread]
                                          │
                                          ▼
                              EVENT_QUEUE (queue.Queue, maxsize=10_000)
                                    ┌─────┴──────────┐
                                    │                │
                         _dashboard_consumer()   _api_bridge()
                         [dashboard thread]      [asyncio bridge thread]
                                    │                │
                              Rich Live        asyncio.Queue (per SSE/WS client)
                              panels               │
                                             FastAPI SSE / WebSocket endpoints
                                                     │
                                             Browser / curl client
```

The SQLite polling path (dotted fallback) reads `queue_jobs` and `spans` tables on each refresh cycle independently of the event queue, providing near-real-time data even when Hermes does not emit structured stdout events.
