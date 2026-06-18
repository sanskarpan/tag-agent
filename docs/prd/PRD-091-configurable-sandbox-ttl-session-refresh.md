# PRD-091: Configurable Sandbox TTL + Session Refresh (`tag sandbox set-ttl`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** S (3-5 days)
**Category:** Sandbox & Execution Environment
**Affects:** `sandbox.py`
**Depends on:** PRD-028 (Sandbox Code Execution), PRD-013 (Agent Tracing / Observability), PRD-034 (Security Hardening), PRD-012 (Cost Tracking / Budget), PRD-040 (Notification Hooks)
**Inspired by:** E2B sandbox TTL, Daytona workspace timeout, Gitpod timeout
**GitHub Issue:** #348

---

## 1. Overview

TAG's current sandbox implementation (`src/tag/sandbox.py`, PRD-028) supports ephemeral command execution in three backends — restricted subprocess, Docker, and Modal — with a single per-invocation `--timeout` wall-clock limit. Once `run_in_sandbox()` returns, there is no persistent sandbox session and therefore no TTL concept: the sandbox exists only for the duration of the subprocess call and is immediately destroyed. This model works well for one-shot code execution tasks but breaks down for interactive or long-lived workflows where an agent iterates inside the same sandbox environment — running tests, building artifacts, and debugging across multiple tool calls without paying the container startup cost on every call.

Cloud sandbox providers have independently converged on a TTL-plus-keepalive model as the canonical lifecycle for long-lived sandboxes. E2B uses `timeout` at creation time (up to 86 400 s on Pro tier) with `sandbox.set_timeout()` and `sandbox.keep_alive()` for mid-session extension; every resume resets the idle timer. Daytona's workspace timeout is configured at the workspace level and can be updated via `UpdateWorkspaceDTO.auto_stop_minutes`; it expires on idle CPU/network quiescence. Gitpod uses per-workspace `timeout` strings (`"1h"`, `"30m"`) with optional `--extended` flags during active sessions. All three providers separately expose a session-level "refresh" or "keepalive" primitive so that programmatic automation can extend a running sandbox without creating a new one.

This PRD adds configurable per-sandbox TTL and session refresh to TAG's sandbox subsystem. The core idea is: a sandbox session is now a first-class persistent record in SQLite with a creation time, a configured TTL, a last-activity timestamp, and a derived time-to-live. A background sweep (piggybacking on the cron scheduler from `cron_scheduler.py`) terminates sandboxes whose idle time has exceeded their TTL and fires a pre-expiry warning notification via `notifications.py` when a sandbox is within a configurable warning window (default: 60 seconds). The new `tag sandbox refresh <id>` command sends a keepalive that resets the last-activity clock, extending the session without changing the TTL contract. The new `tag sandbox set-ttl <id> --ttl <seconds>` command mutates the TTL for a running session, allowing operators to shorten or extend lifetime dynamically.

These additions are additive and backward compatible. Existing `tag sandbox run` invocations without `--ttl` default to the existing `--timeout` wall-clock behavior; TTL management only activates when a sandbox is launched with `--ttl` and enters the `running` state as a persistent session record. The change touches `sandbox.py` for all new logic, `controller.py` for the three new subcommands, and a one-time schema migration adds four new columns to `sandbox_runs`.

The feature has direct practical impact for agents that iterate inside a sandbox across multiple turns (code→test→fix cycles), for queue workers that reserve a Docker container for a batch job and release it when done, and for any scenario where the operator wants deterministic resource cleanup without relying on manual `tag sandbox kill` calls.

---

## 2. Problem Statement

### 2.1 No Lifecycle Management for Long-Lived Sandbox Sessions

`run_in_sandbox()` is a blocking call: it spawns the backend, waits for the command to complete, records the result, and returns. There is no concept of a "session" that survives across multiple agent tool calls. When a TAG agent needs to run three sequential commands in the same Docker container — install dependencies, run tests, capture coverage — it must start three separate containers, paying startup latency on each invocation and losing all in-memory state between calls. The sandbox audit trail in `sandbox_runs` records three unrelated rows with no common session identity.

For E2B and Modal backends, this is especially wasteful: a Firecracker micro-VM allocation costs ~150 ms of network round-trip and cloud compute; allocating one per tool call for a 10-step debug session adds 1.5 s of pure allocation overhead and can consume 10x the billed sandbox-hours that a single session would require.

### 2.2 No Automatic Cleanup Prevents Resource Exhaustion

Without a TTL mechanism, long-lived Docker containers launched by `tag sandbox run` in "detached mode" scenarios (or via queue workers that crash before cleanup) accumulate indefinitely. A developer running overnight queue jobs can return to find dozens of zombie containers consuming host memory and CPU. There is no `tag sandbox list` output field indicating how long a sandbox has been running or when it will be automatically reclaimed, because no such reclamation exists.

This is the same problem that motivated E2B's 1-hour Hobby / 24-hour Pro TTL caps: without enforced TTLs, the platform cannot bound resource consumption per user. For TAG's local Docker backend, the equivalent risk is host resource exhaustion; for E2B/Modal cloud backends, it is unbounded billing.

### 2.3 No Proactive Expiry Warning or Keepalive for Active Sessions

An agent actively using a sandbox for a long-running computation has no way to know that the sandbox TTL is about to expire, and no way to extend it without creating a new sandbox (which would require re-installing all dependencies and losing all in-process state). This creates a reliability cliff: a 30-minute benchmark run in a 30-minute TTL sandbox silently fails at the TTL boundary with no warning and no recourse. The session's results are lost.

E2B's `sandbox.keep_alive()` and Daytona's `UpdateWorkspaceDTO.auto_stop_minutes` address exactly this: they allow automated agents and CI jobs to heartbeat the sandbox during active computation, preventing premature termination. TAG has no equivalent today.

---

## 3. Goals and Non-Goals

### Goals

| # | Goal |
|---|------|
| G1 | Per-sandbox TTL: `tag sandbox run --ttl <seconds>` persists a TTL with the sandbox session record and schedules automatic termination when the idle period exceeds the TTL. |
| G2 | Session keepalive: `tag sandbox refresh <id>` resets the `last_activity_at` timestamp for a running sandbox, extending effective lifetime without altering the configured TTL. |
| G3 | Dynamic TTL mutation: `tag sandbox set-ttl <id> --ttl <seconds>` updates the TTL for a running sandbox session, effective immediately for the next TTL sweep. |
| G4 | `tag sandbox list` includes `ttl_s`, `ttl_remaining_s`, and `last_activity_at` in both human-readable table and `--json` output. |
| G5 | Pre-expiry notifications: when a sandbox has ≤ `ttl_warn_secs` seconds remaining (default 60), a warning is emitted to the terminal and via `notifications.py` hooks (Slack, webhook, etc.). |
| G6 | TTL sweep runs on a configurable interval (default 30 s) without requiring a daemon process: it is triggered lazily on any `cmd_sandbox` call and optionally by `cron_scheduler.py` when available. |
| G7 | Backward compatibility: existing `tag sandbox run` invocations without `--ttl` continue to work exactly as before, with `--timeout` governing the synchronous wall-clock limit. |
| G8 | All TTL events (created, refreshed, set-ttl, expired, warned) are appended to the sandbox audit log at `~/.tag/runtime/sandbox-audit.jsonl`. |
| G9 | Schema migration is additive (ALTER TABLE with DEFAULT values), requiring no data migration for existing `sandbox_runs` rows. |

### Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Pause/resume of sandbox state (filesystem + memory snapshots). TTL management handles termination; pause/resume is a separate feature requiring Firecracker snapshot integration. |
| NG2 | Daemon process or background service. The TTL sweep is lazy and cron-triggered, not a persistent systemd/launchd service. TAG remains a pure CLI tool with no server component. |
| NG3 | Network-level keepalives (TCP heartbeats, WebSocket pings). TAG's keepalive is a database timestamp update; it does not issue any network call to the sandbox runtime. |
| NG4 | Multi-tenant TTL quotas or per-user limits. This feature targets single-user TAG installations. |
| NG5 | TTL enforcement for Modal cloud sandboxes. Modal manages its own sandbox lifecycle via `sb.terminate()`; TAG cannot override Modal's own TTL. `--ttl` for Modal backend records the intent in SQLite but enforcement is advisory. |
| NG6 | Automatic sandbox recycling / restart after TTL expiry. Expired sandboxes are terminated and marked `expired`; no auto-restart is attempted. |
| NG7 | Real-time TTL countdown in a TUI (ncurses/Rich live display). The existing `tag sandbox list` table shows a snapshot `ttl_remaining_s`; a live updating view is out of scope. |

---

## 4. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| TTL enforcement latency | Expired sandbox terminated within `sweep_interval + 5 s` of its TTL deadline | Unit test: mock `_utc_now()` to advance past deadline, run sweep, assert status=`expired` |
| Keepalive extension accuracy | `sandbox refresh <id>` resets `last_activity_at` within 100 ms of call | Integration test: check DB timestamp delta |
| Pre-expiry warning lead time | Warning fires with `ttl_warn_secs ± sweep_interval` seconds remaining | Unit test with fixed clock |
| Backward compatibility | `tag sandbox run` without `--ttl` exits with identical behavior to pre-PRD-091 | Regression test against existing test suite |
| List output correctness | `ttl_remaining_s` in `--json` output is within ±1 s of actual remaining time | Integration test with fixed clock |
| Audit log completeness | All 5 TTL event types (`created`, `refreshed`, `set-ttl`, `expired`, `warned`) written to `sandbox-audit.jsonl` | Integration test: parse JSONL after each operation |
| Schema migration safety | Existing `sandbox_runs` rows retain all data after `ALTER TABLE` migration | Integration test: populate rows, run migration, verify row count and values |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Agent developer | run `tag sandbox run --code "pip install numpy && python bench.py" --ttl 300` | The sandbox is automatically reclaimed after 5 minutes of idle time, preventing zombie containers without manual cleanup |
| U2 | Queue operator | run `tag sandbox refresh <id>` from within a long-running job | The sandbox does not expire mid-computation while a slow test suite is actively executing |
| U3 | Platform engineer | run `tag sandbox list --json` and see `ttl_remaining_s` for each sandbox | I can monitor sandbox health and resource usage from a monitoring script without grepping log files |
| U4 | Agent developer | run `tag sandbox set-ttl <id> --ttl 600` after realizing my initial 5-minute TTL is too short | I can extend a running sandbox without killing and recreating it, preserving all installed dependencies and in-process state |
| U5 | DevOps engineer | receive a Slack notification when a sandbox is within 60 seconds of expiry | I can decide whether to refresh or let it expire before losing work, without polling `tag sandbox list` manually |
| U6 | CI pipeline | run `tag sandbox run --ttl 900 --refresh-interval 60` in a GitHub Actions job | The sandbox auto-refreshes every 60 seconds during active CI, and is cleaned up automatically if the pipeline crashes without calling `tag sandbox kill` |
| U7 | TAG user | see `[TTL: 4m 32s remaining]` in `tag sandbox list` output | I can quickly assess at a glance which sandboxes are near expiry and need attention |
| U8 | Security auditor | inspect `~/.tag/runtime/sandbox-audit.jsonl` | I can see a full audit trail of TTL changes including who called `set-ttl`, when keepalives were sent, and when sandboxes were auto-terminated |
| U9 | Developer | run `tag sandbox run --code "..." --ttl 60` and have it warn me at `t-60s` | I have enough time to refresh or save work before the sandbox terminates |

---

## 6. Proposed CLI Surface

All TTL-related subcommands live under `tag sandbox`. Existing subcommands (`run`, `list`, `result`, `kill`) are extended in place.

### 6.1 `tag sandbox run` (extended)

```
tag sandbox run [OPTIONS]

EXISTING OPTIONS (unchanged)
  --backend docker|e2b|modal|restricted|auto
  --image <image>
  --timeout <seconds>            # hard wall-clock for blocking mode (unchanged)
  --code <python_string>         # inline code shortcut
  --json

NEW OPTIONS
  --ttl <seconds>
      Per-sandbox idle TTL in seconds. When set, the sandbox is persisted as a
      long-lived session and is automatically terminated after <seconds> of
      inactivity (measured from last_activity_at). Default: unset (ephemeral,
      uses --timeout).
      Range: 30–86400.

  --ttl-warn <seconds>
      Seconds before TTL expiry at which to fire a warning notification.
      Default: 60. Must be < --ttl.

  --refresh-interval <seconds>
      If set, the CLI sends automatic keepalives every <seconds> in a background
      thread while the sandbox run command is in the foreground. Useful for
      long-running blocking invocations. Default: unset.
      Range: 10–3600.

  --detach
      Return immediately after starting the sandbox session (do not wait for
      command to complete). Prints the sandbox ID for later use with
      `sandbox refresh`, `sandbox result`, and `sandbox kill`. Requires --ttl.

EXAMPLE — ephemeral (existing behavior, unchanged):
  tag sandbox run --backend docker --code "python -c 'print(42)'" --timeout 30

EXAMPLE — TTL session:
  tag sandbox run --backend docker --code "pip install pytest && pytest tests/" \
    --ttl 300 --ttl-warn 60

EXAMPLE — detached TTL session:
  tag sandbox run --backend docker --image python:3.12 \
    --code "python long_benchmark.py" \
    --ttl 900 --detach
  # → sandbox abc123def456 started (TTL: 900s, expires at 14:32:07 UTC)

OUTPUT (non-JSON, TTL mode):
  Sandbox abc123def456 started  [backend: docker]  [TTL: 300s]
  Running: python -c 'import subprocess; ...'
  --- output ---
  <streaming output>
  ---
  Sandbox abc123def456 completed  exit=0  runtime=12.4s  TTL resets to 300s from now
```

### 6.2 `tag sandbox list` (extended)

```
tag sandbox list [OPTIONS]

OPTIONS (new additions)
  --active          Show only sandboxes in state running|paused.
  --expired         Show only sandboxes in state expired.
  --json            Emit JSON array (extended schema).

HUMAN-READABLE TABLE OUTPUT:
  ID              BACKEND    STATE       TTL_S   REMAINING   LAST_ACTIVITY
  ─────────────────────────────────────────────────────────────────────────
  abc123def456    docker     running     300     4m 32s      2026-06-17 14:27:35
  def456ghi789    e2b        running     900     12m 04s     2026-06-17 14:15:21
  xyz789abc012    docker     expired     60      —           2026-06-17 13:58:01
  mnp012qrs345    restricted done        —       —           2026-06-17 13:44:12

JSON OUTPUT:
  [
    {
      "id": "abc123def456",
      "backend": "docker",
      "state": "running",
      "command": "python bench.py",
      "created_at": "2026-06-17T14:22:35Z",
      "last_activity_at": "2026-06-17T14:27:35Z",
      "ttl_s": 300,
      "ttl_warn_s": 60,
      "ttl_remaining_s": 272,
      "expires_at": "2026-06-17T14:32:35Z",
      "exit_code": null,
      "image": "python:3.12-slim"
    }
  ]
```

### 6.3 `tag sandbox refresh <id>` (new)

```
tag sandbox refresh <SANDBOX_ID> [OPTIONS]

ARGUMENTS
  SANDBOX_ID    The sandbox ID (from `tag sandbox list` or `sandbox run` output).

OPTIONS
  --json        Emit JSON confirmation object.

BEHAVIOR
  Resets last_activity_at to UTC now for the given sandbox. The sandbox's TTL
  countdown restarts from this moment. If the sandbox is already expired or
  done/failed, the command exits non-zero with an error.

  Appends a `refreshed` event to sandbox-audit.jsonl.

EXAMPLE:
  $ tag sandbox refresh abc123def456
  Sandbox abc123def456 refreshed.  New expiry: 2026-06-17T14:32:07Z  (TTL: 300s)

JSON OUTPUT:
  {
    "id": "abc123def456",
    "last_activity_at": "2026-06-17T14:27:07Z",
    "ttl_s": 300,
    "ttl_remaining_s": 300,
    "expires_at": "2026-06-17T14:32:07Z"
  }

EXIT CODES
  0  Refresh succeeded.
  1  Sandbox not found or not in running state.
```

### 6.4 `tag sandbox set-ttl <id> --ttl <seconds>` (new)

```
tag sandbox set-ttl <SANDBOX_ID> [OPTIONS]

ARGUMENTS
  SANDBOX_ID    The sandbox ID.

OPTIONS
  --ttl <seconds>       Required. New TTL in seconds (30–86400).
  --ttl-warn <seconds>  New warning threshold (must be < --ttl). Optional.
  --json                Emit JSON confirmation object.

BEHAVIOR
  Updates the ttl_s (and optionally ttl_warn_s) for the given sandbox.
  Also resets last_activity_at to UTC now, so the new TTL starts from this call.
  Appends a `set-ttl` event to sandbox-audit.jsonl.

  If --ttl is shorter than the already-elapsed idle time, the sandbox is
  immediately expired and terminated (same as the sweep would do).

EXAMPLE — extend TTL:
  $ tag sandbox set-ttl abc123def456 --ttl 600
  Sandbox abc123def456 TTL updated: 300s → 600s.  New expiry: 2026-06-17T14:37:07Z

EXAMPLE — shorten TTL (triggers immediate expiry):
  $ tag sandbox set-ttl abc123def456 --ttl 10
  Sandbox abc123def456 TTL updated: 300s → 10s.  Idle time (45s) exceeds new TTL.
  Terminating sandbox...  [done]

JSON OUTPUT:
  {
    "id": "abc123def456",
    "old_ttl_s": 300,
    "new_ttl_s": 600,
    "last_activity_at": "2026-06-17T14:27:07Z",
    "expires_at": "2026-06-17T14:37:07Z"
  }

EXIT CODES
  0  Set-TTL succeeded.
  1  Sandbox not found or not in running state.
  2  Invalid TTL value (out of range or less than warn window).
```

---

## 7. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `tag sandbox run --ttl <N>` stores TTL in `sandbox_runs.ttl_s` and sets `last_activity_at = created_at` on insert. | Must |
| FR-02 | If `--ttl` is not specified, `sandbox_runs.ttl_s` is NULL and TTL management is skipped for that row; `--timeout` wall-clock behavior is unchanged. | Must |
| FR-03 | `tag sandbox list` computes `ttl_remaining_s = ttl_s - (now - last_activity_at)` at query time and includes it in both human-readable and JSON output; negative values are reported as `0` and the sandbox is marked for sweep. | Must |
| FR-04 | `tag sandbox refresh <id>` sets `last_activity_at = utc_now()` for a sandbox in state `running` and writes a `refreshed` audit event. It MUST reject sandboxes in states `done`, `failed`, `expired`, or `killed`. | Must |
| FR-05 | `tag sandbox set-ttl <id> --ttl <N>` updates `ttl_s` and resets `last_activity_at = utc_now()`. If the new TTL is already exceeded by the current idle time, the function calls `_terminate_sandbox()` immediately and sets state to `expired`. | Must |
| FR-06 | The TTL sweep function `sweep_expired_sandboxes(conn)` selects all rows where `ttl_s IS NOT NULL AND state = 'running' AND (strftime('%s','now') - strftime('%s', last_activity_at)) > ttl_s` and calls `_terminate_sandbox()` on each, updating state to `expired`. | Must |
| FR-07 | The TTL sweep is invoked lazily at the start of every `cmd_sandbox` call (any subcommand) and emits a log line at DEBUG level listing how many sandboxes were swept. | Must |
| FR-08 | Pre-expiry warning: for each sandbox where `ttl_remaining_s <= ttl_warn_s` and `state = 'running'` and `warned_at IS NULL`, emit a terminal warning and call `notifications.send_notification()` with message `"Sandbox <id> expires in <N>s"`. Set `warned_at = utc_now()` to prevent repeat warnings. | Must |
| FR-09 | Warning is re-armed after each `sandbox refresh` or `set-ttl` call by setting `warned_at = NULL`. | Must |
| FR-10 | `_terminate_sandbox(conn, run_id)` must call the appropriate backend cleanup: `docker kill <container_id>` for Docker; `sandbox.kill()` for E2B; no-op for restricted/modal (subprocess already completed). | Must |
| FR-11 | `--refresh-interval <seconds>` in `tag sandbox run` starts a `threading.Timer`-based background thread that calls `refresh_sandbox(conn, run_id)` every `<seconds>` while the main thread is blocking on the subprocess. The thread is cancelled on process exit. | Should |
| FR-12 | `tag sandbox list --active` filters to rows where `state = 'running'`. `--expired` filters to `state = 'expired'`. Without flags, all states are shown. | Should |
| FR-13 | `tag sandbox run --detach` inserts the sandbox record, starts the backend process as a non-blocking detached subprocess (using `subprocess.Popen` with stdout/stderr redirected to a temp file), and returns immediately, printing the sandbox ID. Requires `--ttl`. | Should |
| FR-14 | Every TTL event (`created`, `refreshed`, `set-ttl`, `expired`, `warned`, `terminated`) is appended to `~/.tag/runtime/sandbox-audit.jsonl` as a JSON object with fields: `event`, `sandbox_id`, `timestamp`, `ttl_s`, `caller` (subcommand name). | Must |
| FR-15 | `tag sandbox set-ttl` validates that `--ttl` is between 30 and 86400 (inclusive) and that `--ttl-warn`, if provided, is strictly less than `--ttl`. Out-of-range values exit with code 2 and a clear error message. | Must |
| FR-16 | `sandbox_runs.container_id` column (added by schema migration) stores the Docker container ID (obtained from `docker run --cidfile`) so that `_terminate_sandbox()` can issue `docker kill <container_id>` for long-lived Docker sessions. | Must |
| FR-17 | Schema migration uses `ALTER TABLE sandbox_runs ADD COLUMN IF NOT SUPPORTED` — since SQLite does not support `IF NOT EXISTS` for `ALTER TABLE`, migration is guarded by a `PRAGMA table_info` check in `ensure_schema()`. | Must |
| FR-18 | `cron_scheduler.py` integration: if the cron scheduler is available and running, register a cron entry `tag_sandbox_sweep` with interval 30 s that calls `sweep_expired_sandboxes`. This is advisory; the lazy sweep in `cmd_sandbox` is the primary mechanism. | Won't (v1) |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | `sweep_expired_sandboxes()` must complete in < 50 ms for up to 1000 active sandbox rows on a standard developer laptop (SQLite WAL mode). | Performance |
| NFR-02 | `sandbox refresh` end-to-end (CLI invocation to DB commit to JSON response) must complete in < 200 ms. | Performance |
| NFR-03 | TTL timestamp arithmetic uses ISO-8601 UTC strings and SQLite's `strftime('%s', ...)` for portable epoch arithmetic; no timezone-aware `datetime` objects are stored in the database. | Portability |
| NFR-04 | The `--refresh-interval` background thread must not prevent clean process exit: it must be a daemon thread (`daemon=True`) with a `threading.Event` stop signal. | Reliability |
| NFR-05 | All new functions are covered by unit tests that mock `_utc_now()` to simulate time advancement without `time.sleep()`. | Testability |
| NFR-06 | `sandbox-audit.jsonl` is written with `O_APPEND` semantics (open in `'a'` mode) and each write is a single `json.dumps(event) + '\n'` call, making it safe for concurrent writers on POSIX. | Reliability |
| NFR-07 | No new mandatory dependencies. Docker TTL management uses the existing `docker` CLI subprocess. E2B TTL uses the existing `e2b` SDK import guarded by `try/except ImportError`. | Dependency |
| NFR-08 | Human-readable `ttl_remaining_s` display formats seconds as `Xh Ym Zs`, `Xm Ys`, or `Xs` as appropriate (no raw second counts in the table). | UX |
| NFR-09 | `tag sandbox list --json` output is stable across minor version updates: new fields are always additive; no existing fields are renamed or removed. | Stability |
| NFR-10 | SQLite WAL mode is already enabled by `open_db()`; no additional locking or journaling configuration is required for TTL sweep concurrency. | Correctness |

---

## 9. Technical Design

### 9.1 Schema Migration

The existing `sandbox_runs` table (defined in `sandbox.py:ensure_schema()`) gains four new columns. Because SQLite does not support `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`, the migration function checks `PRAGMA table_info(sandbox_runs)` before each `ALTER TABLE`.

```sql
-- New columns added to sandbox_runs by ensure_schema() migration
ALTER TABLE sandbox_runs ADD COLUMN ttl_s        INTEGER;           -- NULL = ephemeral (no TTL)
ALTER TABLE sandbox_runs ADD COLUMN ttl_warn_s   INTEGER DEFAULT 60;
ALTER TABLE sandbox_runs ADD COLUMN last_activity_at TEXT;          -- ISO-8601 UTC; NULL for ephemeral
ALTER TABLE sandbox_runs ADD COLUMN warned_at    TEXT;              -- NULL until warning fires
ALTER TABLE sandbox_runs ADD COLUMN container_id TEXT;              -- Docker container ID for kill

-- New index for TTL sweep query (partial index on non-NULL ttl_s)
CREATE INDEX IF NOT EXISTS idx_sr_ttl_sweep
    ON sandbox_runs(state, last_activity_at)
    WHERE ttl_s IS NOT NULL;

-- New index for warning sweep
CREATE INDEX IF NOT EXISTS idx_sr_warn
    ON sandbox_runs(state, warned_at)
    WHERE ttl_s IS NOT NULL;
```

Full revised DDL (as would appear in `ensure_schema()`):

```sql
CREATE TABLE IF NOT EXISTS sandbox_runs (
    id               TEXT PRIMARY KEY,
    command          TEXT NOT NULL,
    backend          TEXT NOT NULL DEFAULT 'restricted',
    image            TEXT,
    container_id     TEXT,                        -- PRD-091: Docker container ID
    state            TEXT NOT NULL DEFAULT 'running',
    exit_code        INTEGER,
    output           TEXT NOT NULL DEFAULT '',
    error            TEXT,
    created_at       TEXT NOT NULL,
    completed_at     TEXT,
    last_activity_at TEXT,                        -- PRD-091: TTL idle anchor
    ttl_s            INTEGER,                     -- PRD-091: NULL = ephemeral
    ttl_warn_s       INTEGER DEFAULT 60,          -- PRD-091: warning threshold
    warned_at        TEXT                         -- PRD-091: NULL until warned
);

CREATE INDEX IF NOT EXISTS idx_sr_status   ON sandbox_runs(state, created_at);
CREATE INDEX IF NOT EXISTS idx_sr_ttl_sweep ON sandbox_runs(state, last_activity_at)
    WHERE ttl_s IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_sr_warn ON sandbox_runs(state, warned_at)
    WHERE ttl_s IS NOT NULL;
```

### 9.2 Core Dataclasses

```python
# src/tag/sandbox.py  (additions for PRD-091)
from __future__ import annotations

import dataclasses
import datetime
import json
import os
import threading
from pathlib import Path
from typing import Optional


@dataclasses.dataclass
class SandboxSession:
    """Represents a persistent sandbox session with TTL lifecycle."""
    id: str
    command: str
    backend: str
    image: Optional[str]
    container_id: Optional[str]
    state: str                        # running | done | failed | expired | killed
    exit_code: Optional[int]
    created_at: str                   # ISO-8601 UTC
    completed_at: Optional[str]
    last_activity_at: Optional[str]   # ISO-8601 UTC; None for ephemeral
    ttl_s: Optional[int]              # None = ephemeral
    ttl_warn_s: int = 60
    warned_at: Optional[str] = None

    @property
    def is_ephemeral(self) -> bool:
        return self.ttl_s is None

    @property
    def ttl_remaining_s(self) -> Optional[int]:
        """Seconds until expiry based on last_activity_at. None if ephemeral."""
        if self.ttl_s is None or self.last_activity_at is None:
            return None
        last = datetime.datetime.fromisoformat(self.last_activity_at.replace("Z", "+00:00"))
        now = datetime.datetime.now(datetime.timezone.utc)
        elapsed = (now - last).total_seconds()
        remaining = self.ttl_s - elapsed
        return max(0, int(remaining))

    @property
    def expires_at(self) -> Optional[str]:
        """ISO-8601 UTC expiry timestamp. None if ephemeral."""
        if self.ttl_s is None or self.last_activity_at is None:
            return None
        last = datetime.datetime.fromisoformat(self.last_activity_at.replace("Z", "+00:00"))
        expiry = last + datetime.timedelta(seconds=self.ttl_s)
        return expiry.isoformat().replace("+00:00", "Z")

    def format_remaining(self) -> str:
        """Human-readable TTL remaining string, e.g. '4m 32s'."""
        r = self.ttl_remaining_s
        if r is None:
            return "—"
        if r == 0:
            return "expired"
        h, rem = divmod(r, 3600)
        m, s = divmod(rem, 60)
        if h:
            return f"{h}h {m}m {s}s"
        if m:
            return f"{m}m {s}s"
        return f"{s}s"

    def to_dict(self) -> dict:
        return {
            "id": self.id,
            "backend": self.backend,
            "state": self.state,
            "command": self.command,
            "created_at": self.created_at,
            "last_activity_at": self.last_activity_at,
            "ttl_s": self.ttl_s,
            "ttl_warn_s": self.ttl_warn_s,
            "ttl_remaining_s": self.ttl_remaining_s,
            "expires_at": self.expires_at,
            "exit_code": self.exit_code,
            "image": self.image,
            "container_id": self.container_id,
        }


@dataclasses.dataclass
class TTLEvent:
    """Audit log entry for a TTL lifecycle event."""
    event: str          # created | refreshed | set-ttl | expired | warned | terminated
    sandbox_id: str
    timestamp: str      # ISO-8601 UTC
    ttl_s: Optional[int]
    caller: str         # subcommand name or "sweep"
    extra: dict = dataclasses.field(default_factory=dict)

    def to_jsonl(self) -> str:
        d = dataclasses.asdict(self)
        return json.dumps(d)
```

### 9.3 TTL Sweep Algorithm

```python
# src/tag/sandbox.py

TTL_MIN = 30
TTL_MAX = 86400
AUDIT_LOG_NAME = "sandbox-audit.jsonl"


def _audit_dir() -> Path:
    return Path.home() / ".tag" / "runtime"


def _write_audit(event: TTLEvent) -> None:
    """Append a TTL event to the JSONL audit log (O_APPEND safe)."""
    log_path = _audit_dir() / AUDIT_LOG_NAME
    log_path.parent.mkdir(parents=True, exist_ok=True)
    with open(log_path, "a", encoding="utf-8") as fh:
        fh.write(event.to_jsonl() + "\n")


def sweep_expired_sandboxes(conn: sqlite3.Connection) -> int:
    """
    Terminate all running sandboxes whose idle time exceeds their TTL.

    Returns the number of sandboxes swept.

    SQL predicate (portable SQLite epoch arithmetic):
        strftime('%s','now') - strftime('%s', last_activity_at) > ttl_s
    """
    now_str = _utc_now()
    expired_rows = conn.execute(
        """
        SELECT id, backend, container_id, ttl_s
        FROM   sandbox_runs
        WHERE  state = 'running'
          AND  ttl_s IS NOT NULL
          AND  last_activity_at IS NOT NULL
          AND  (CAST(strftime('%s','now') AS INTEGER)
                - CAST(strftime('%s', last_activity_at) AS INTEGER)) > ttl_s
        """,
    ).fetchall()

    swept = 0
    for row_id, backend, container_id, ttl_s in expired_rows:
        _terminate_sandbox_backend(backend, container_id)
        conn.execute(
            "UPDATE sandbox_runs SET state='expired', completed_at=? WHERE id=?",
            (now_str, row_id),
        )
        _write_audit(TTLEvent(
            event="expired",
            sandbox_id=row_id,
            timestamp=now_str,
            ttl_s=ttl_s,
            caller="sweep",
        ))
        swept += 1

    if swept:
        conn.commit()

    # Fire pre-expiry warnings for sandboxes approaching TTL
    _sweep_warnings(conn)

    return swept


def _sweep_warnings(conn: sqlite3.Connection) -> None:
    """Fire pre-expiry warnings for sandboxes within their warn window."""
    now_str = _utc_now()
    warn_rows = conn.execute(
        """
        SELECT id, ttl_s, ttl_warn_s,
               (CAST(strftime('%s','now') AS INTEGER)
                - CAST(strftime('%s', last_activity_at) AS INTEGER)) AS idle_s
        FROM   sandbox_runs
        WHERE  state = 'running'
          AND  ttl_s IS NOT NULL
          AND  last_activity_at IS NOT NULL
          AND  warned_at IS NULL
          AND  (ttl_s - (CAST(strftime('%s','now') AS INTEGER)
                         - CAST(strftime('%s', last_activity_at) AS INTEGER)))
               <= ttl_warn_s
          AND  (ttl_s - (CAST(strftime('%s','now') AS INTEGER)
                         - CAST(strftime('%s', last_activity_at) AS INTEGER))) > 0
        """,
    ).fetchall()

    for row_id, ttl_s, ttl_warn_s, idle_s in warn_rows:
        remaining = ttl_s - idle_s
        _emit_ttl_warning(row_id, remaining)
        conn.execute(
            "UPDATE sandbox_runs SET warned_at=? WHERE id=?",
            (now_str, row_id),
        )
        _write_audit(TTLEvent(
            event="warned",
            sandbox_id=row_id,
            timestamp=now_str,
            ttl_s=ttl_s,
            caller="sweep",
            extra={"remaining_s": remaining},
        ))

    if warn_rows:
        conn.commit()


def _emit_ttl_warning(sandbox_id: str, remaining_s: int) -> None:
    """Print terminal warning and call notifications hook."""
    import sys
    msg = (
        f"[TAG WARNING] Sandbox {sandbox_id} expires in "
        f"{_fmt_seconds(remaining_s)} — run `tag sandbox refresh {sandbox_id}` to extend."
    )
    print(msg, file=sys.stderr)
    try:
        from tag.notifications import send_notification
        send_notification(
            title="Sandbox Expiry Warning",
            body=msg,
            level="warning",
        )
    except Exception:
        pass  # notifications are best-effort


def _terminate_sandbox_backend(backend: str, container_id: Optional[str]) -> None:
    """Issue backend-specific termination. Errors are swallowed (best-effort)."""
    try:
        if backend == "docker" and container_id:
            subprocess.run(
                ["docker", "kill", container_id],
                capture_output=True,
                timeout=10,
            )
        elif backend == "e2b":
            # E2B: sandbox.kill() requires the SDK object, which is not
            # serializable. In practice, E2B sandboxes are identified by ID;
            # use the E2B REST API or SDK reconnect pattern here.
            try:
                from e2b import Sandbox
                sb = Sandbox.connect(container_id)  # container_id = e2b sandbox_id
                sb.kill()
            except Exception:
                pass
        # restricted and modal: subprocess already completed; no-op.
    except Exception:
        pass


def _fmt_seconds(s: int) -> str:
    """Format seconds as human-readable string."""
    h, rem = divmod(s, 3600)
    m, sec = divmod(rem, 60)
    if h:
        return f"{h}h {m}m {sec}s"
    if m:
        return f"{m}m {sec}s"
    return f"{sec}s"
```

### 9.4 Refresh and Set-TTL Functions

```python
def refresh_sandbox(conn: sqlite3.Connection, run_id: str) -> SandboxSession:
    """
    Reset last_activity_at for a running sandbox (keepalive).

    Raises ValueError if sandbox is not found or not in 'running' state.
    """
    row = conn.execute(
        "SELECT state, ttl_s FROM sandbox_runs WHERE id=?", (run_id,)
    ).fetchone()
    if row is None:
        raise ValueError(f"Sandbox {run_id!r} not found.")
    state, ttl_s = row
    if state != "running":
        raise ValueError(
            f"Sandbox {run_id!r} is in state {state!r}; only 'running' sandboxes can be refreshed."
        )
    if ttl_s is None:
        raise ValueError(
            f"Sandbox {run_id!r} is ephemeral (no TTL); refresh is not applicable."
        )

    now_str = _utc_now()
    conn.execute(
        "UPDATE sandbox_runs SET last_activity_at=?, warned_at=NULL WHERE id=?",
        (now_str, run_id),
    )
    conn.commit()
    _write_audit(TTLEvent(
        event="refreshed",
        sandbox_id=run_id,
        timestamp=now_str,
        ttl_s=ttl_s,
        caller="refresh",
    ))
    return _get_session(conn, run_id)


def set_sandbox_ttl(
    conn: sqlite3.Connection,
    run_id: str,
    new_ttl_s: int,
    new_warn_s: Optional[int] = None,
) -> SandboxSession:
    """
    Update TTL for a running sandbox. Resets last_activity_at.
    If new_ttl_s is already exceeded by current idle time, terminates immediately.

    Raises ValueError for invalid inputs or wrong state.
    """
    if not (TTL_MIN <= new_ttl_s <= TTL_MAX):
        raise ValueError(f"--ttl must be between {TTL_MIN} and {TTL_MAX}, got {new_ttl_s}.")

    row = conn.execute(
        "SELECT state, ttl_s, ttl_warn_s, backend, container_id FROM sandbox_runs WHERE id=?",
        (run_id,),
    ).fetchone()
    if row is None:
        raise ValueError(f"Sandbox {run_id!r} not found.")
    state, old_ttl_s, old_warn_s, backend, container_id = row
    if state != "running":
        raise ValueError(
            f"Sandbox {run_id!r} is in state {state!r}; only 'running' sandboxes can have TTL changed."
        )

    warn_s = new_warn_s if new_warn_s is not None else old_warn_s
    if warn_s is not None and warn_s >= new_ttl_s:
        raise ValueError(
            f"--ttl-warn ({warn_s}s) must be less than --ttl ({new_ttl_s}s)."
        )

    now_str = _utc_now()
    conn.execute(
        """UPDATE sandbox_runs
           SET ttl_s=?, ttl_warn_s=?, last_activity_at=?, warned_at=NULL
           WHERE id=?""",
        (new_ttl_s, warn_s, now_str, run_id),
    )
    conn.commit()

    _write_audit(TTLEvent(
        event="set-ttl",
        sandbox_id=run_id,
        timestamp=now_str,
        ttl_s=new_ttl_s,
        caller="set-ttl",
        extra={"old_ttl_s": old_ttl_s, "new_warn_s": warn_s},
    ))

    # Run sweep immediately: if new_ttl_s < current idle time, expires now.
    sweep_expired_sandboxes(conn)

    return _get_session(conn, run_id)


def _get_session(conn: sqlite3.Connection, run_id: str) -> SandboxSession:
    """Fetch a SandboxSession from the database by ID."""
    row = conn.execute(
        """SELECT id, command, backend, image, container_id, state, exit_code,
                  created_at, completed_at, last_activity_at, ttl_s, ttl_warn_s, warned_at
           FROM sandbox_runs WHERE id=?""",
        (run_id,),
    ).fetchone()
    if row is None:
        raise ValueError(f"Sandbox {run_id!r} not found.")
    return SandboxSession(
        id=row[0], command=row[1], backend=row[2], image=row[3],
        container_id=row[4], state=row[5], exit_code=row[6],
        created_at=row[7], completed_at=row[8], last_activity_at=row[9],
        ttl_s=row[10], ttl_warn_s=row[11] or 60, warned_at=row[12],
    )
```

### 9.5 Background Refresh Thread

```python
class _RefreshThread:
    """
    Background thread that calls refresh_sandbox() every `interval_s` seconds.
    Used by `tag sandbox run --refresh-interval`.
    """

    def __init__(
        self,
        conn_factory,          # callable → sqlite3.Connection (new connection per call)
        run_id: str,
        interval_s: int,
    ) -> None:
        self._factory = conn_factory
        self._run_id = run_id
        self._interval = interval_s
        self._stop = threading.Event()
        self._thread = threading.Thread(
            target=self._loop,
            daemon=True,
            name=f"sandbox-refresh-{run_id[:8]}",
        )

    def start(self) -> None:
        self._thread.start()

    def stop(self) -> None:
        self._stop.set()
        self._thread.join(timeout=self._interval + 2)

    def _loop(self) -> None:
        while not self._stop.wait(timeout=self._interval):
            try:
                conn = self._factory()
                refresh_sandbox(conn, self._run_id)
                conn.close()
            except Exception:
                pass  # Don't crash the background thread
```

### 9.6 Controller Integration Points

In `src/tag/controller.py`, the `cmd_sandbox` function (line ~7443) is extended with three new subcommand branches: `refresh`, `set-ttl`, and updated `list`. The lazy sweep is prepended to the existing dispatch logic:

```python
def cmd_sandbox(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(getattr(args, "config", None)))
    ensure_runtime_dirs(cfg)
    db = open_db(cfg)
    sub = getattr(args, "sandbox_subcommand", "list")

    try:
        import tag.sandbox as _sandbox
    except ImportError as exc:
        db.close()
        print_error(f"tag.sandbox not available: {exc}")
        return 1

    # PRD-091: lazy TTL sweep on every cmd_sandbox invocation
    _sandbox.sweep_expired_sandboxes(db)

    if sub == "refresh":
        run_id = getattr(args, "run_id", None)
        if not run_id:
            db.close()
            print_error("SANDBOX_ID required")
            return 1
        try:
            session = _sandbox.refresh_sandbox(db, run_id)
        except ValueError as exc:
            db.close()
            print_error(str(exc))
            return 1
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(session.to_dict(), indent=2))
        else:
            print(
                f"Sandbox {session.id} refreshed.  "
                f"New expiry: {session.expires_at}  (TTL: {session.ttl_s}s)"
            )
        return 0

    if sub == "set-ttl":
        run_id = getattr(args, "run_id", None)
        new_ttl = getattr(args, "ttl", None)
        new_warn = getattr(args, "ttl_warn", None)
        if not run_id or new_ttl is None:
            db.close()
            print_error("SANDBOX_ID and --ttl are required")
            return 1
        try:
            session = _sandbox.set_sandbox_ttl(db, run_id, new_ttl, new_warn)
        except ValueError as exc:
            db.close()
            print_error(str(exc))
            return 2
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(session.to_dict(), indent=2))
        else:
            print(
                f"Sandbox {session.id} TTL updated → {session.ttl_s}s.  "
                f"New expiry: {session.expires_at}"
            )
        return 0
    # ... existing run / list / result branches follow
```

### 9.7 Argparse Registration

New subcommands are registered in the `tag sandbox` subparser group in `controller.py` (around line 9851):

```python
# In the sandbox subparser registration block:

# tag sandbox refresh
p_refresh = sandbox_sub.add_parser("refresh", help="Reset TTL idle timer for a running sandbox")
p_refresh.add_argument("run_id", metavar="SANDBOX_ID")
p_refresh.add_argument("--json", action="store_true")
p_refresh.set_defaults(sandbox_subcommand="refresh")

# tag sandbox set-ttl
p_set_ttl = sandbox_sub.add_parser("set-ttl", help="Update TTL for a running sandbox")
p_set_ttl.add_argument("run_id", metavar="SANDBOX_ID")
p_set_ttl.add_argument("--ttl", type=int, required=True, metavar="SECONDS",
                        help=f"New TTL in seconds ({TTL_MIN}–{TTL_MAX})")
p_set_ttl.add_argument("--ttl-warn", type=int, dest="ttl_warn", metavar="SECONDS",
                        help="New warning threshold (must be < --ttl)")
p_set_ttl.add_argument("--json", action="store_true")
p_set_ttl.set_defaults(sandbox_subcommand="set-ttl")

# Extend tag sandbox run with --ttl flags
p_run.add_argument("--ttl", type=int, default=None, metavar="SECONDS",
                   help="Per-sandbox idle TTL in seconds. Enables session persistence.")
p_run.add_argument("--ttl-warn", type=int, dest="ttl_warn", default=60, metavar="SECONDS",
                   help="Warn N seconds before TTL expiry (default: 60)")
p_run.add_argument("--refresh-interval", type=int, dest="refresh_interval",
                   default=None, metavar="SECONDS",
                   help="Auto-keepalive interval in seconds (requires --ttl)")
p_run.add_argument("--detach", action="store_true",
                   help="Return immediately after starting sandbox (requires --ttl)")

# Extend tag sandbox list with filter flags
p_list.add_argument("--active", action="store_true", help="Show only running sandboxes")
p_list.add_argument("--expired", action="store_true", help="Show only expired sandboxes")
```

### 9.8 Docker Container ID Capture

For `_terminate_sandbox_backend` to issue `docker kill <container_id>`, the container ID must be captured at `docker run` time. The existing `_run_docker()` function is modified to use `--cidfile`:

```python
def _run_docker_session(
    command: list[str],
    image: str,
    *,
    timeout: int = 60,
    run_id: str,
) -> tuple[int, str, str, str]:
    """
    Run command inside a Docker container, capturing the container ID.
    Returns (exit_code, stdout, stderr, container_id).
    """
    import tempfile
    with tempfile.NamedTemporaryFile(suffix=".cid", delete=False) as cid_file:
        cid_path = cid_file.name

    docker_cmd = [
        "docker", "run",
        "--rm",
        "--network=none",
        "--memory=512m",
        "--cpus=1",
        f"--stop-timeout={timeout}",
        f"--cidfile={cid_path}",
        "--label", f"tag.sandbox.id={run_id}",
        image,
    ] + command

    try:
        proc = subprocess.run(
            docker_cmd,
            capture_output=True,
            text=True,
            timeout=timeout + 30,
        )
        container_id = ""
        try:
            container_id = Path(cid_path).read_text().strip()
        except OSError:
            pass
        return proc.returncode, proc.stdout, proc.stderr, container_id
    except FileNotFoundError:
        return 1, "", "docker not found — install Docker or use --backend restricted", ""
    except subprocess.TimeoutExpired:
        return 124, "", f"Docker run timed out after {timeout}s", ""
    except Exception as exc:
        return 1, "", str(exc), ""
    finally:
        try:
            os.unlink(cid_path)
        except OSError:
            pass
```

### 9.9 Integration with Existing Modules

| Module | Integration point | Change |
|--------|-------------------|--------|
| `notifications.py` | `_emit_ttl_warning()` calls `send_notification()` with `level="warning"` | Caller-side only; `notifications.py` unchanged |
| `cron_scheduler.py` | `sweep_expired_sandboxes` registered as advisory cron (30 s interval) | Deferred to v1.1; lazy sweep is primary |
| `tracing.py` | TTL events use existing `_utc_now()` pattern; no OTel spans added for sweep | No change |
| `budget.py` | Sandbox session duration contributes to cost attribution in a future PRD; no change in this PRD | No change |
| `controller.py` | `cmd_sandbox` extended with `refresh`, `set-ttl` branches; lazy sweep prepended to dispatch | See §9.6 |

---

## 10. Security Considerations

1. **TTL manipulation by unprivileged callers.** `set_sandbox_ttl` validates the `run_id` against the `sandbox_runs` table but does not check ownership (since TAG is single-user). In a future multi-user deployment, ownership must be enforced by adding a `owner_uid` column and comparing against `os.getuid()`.

2. **Audit log integrity.** `sandbox-audit.jsonl` is written at `~/.tag/runtime/`, which is writable only by the owning user. On shared systems, the directory permissions should be `700`. The `ensure_runtime_dirs()` call in `controller.py` already sets `0o700` on the runtime directory; no change required, but this should be verified in the doctor check.

3. **Container ID spoofing in `docker kill`.** The `container_id` stored in `sandbox_runs` is retrieved from a `--cidfile` written by `docker run`. On a POSIX filesystem, the temp file is created by the TAG process and cleaned up after read, providing no meaningful attack surface for unprivileged users on a single-user system.

4. **`--ttl` range enforcement prevents resource exhaustion.** The 30–86400 second range is validated server-side (in `set_sandbox_ttl`) and client-side (argparse `type=int` + range check). Values outside this range exit 2 with a clear error; there is no silent truncation.

5. **Background refresh thread and credential leakage.** The `_RefreshThread` holds a `conn_factory` callable but no credentials. It opens new SQLite connections using `open_db(cfg)` which reads from the config file, not from an in-memory credential store. This is safe.

6. **Notification content.** TTL warning notifications include the sandbox ID and remaining seconds but never the command string, output, or any data that might contain secrets. This prevents credential leakage via Slack/webhook notification bodies.

7. **`--detach` and orphan risk.** Detached sandboxes that exceed their TTL are terminated by the sweep. However, if TAG is never invoked again (no `cmd_sandbox` calls), the lazy sweep never fires. Users who rely on deterministic cleanup for billing reasons should use the `cron_scheduler.py` integration (v1.1) or call `tag sandbox list` periodically.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`tests/test_sandbox_ttl.py`)

```python
# Tests use in-memory SQLite and mock _utc_now() to simulate time advancement

def test_refresh_sandbox_resets_last_activity():
    """refresh_sandbox() updates last_activity_at and clears warned_at."""

def test_refresh_rejects_non_running_state():
    """refresh_sandbox() raises ValueError for done/failed/expired state."""

def test_refresh_rejects_ephemeral_sandbox():
    """refresh_sandbox() raises ValueError when ttl_s is NULL."""

def test_set_ttl_updates_ttl_and_resets_activity():
    """set_sandbox_ttl() updates ttl_s and resets last_activity_at."""

def test_set_ttl_validates_range():
    """set_sandbox_ttl() raises ValueError for TTL < 30 or > 86400."""

def test_set_ttl_validates_warn_less_than_ttl():
    """set_sandbox_ttl() raises ValueError when warn_s >= new_ttl_s."""

def test_set_ttl_triggers_immediate_expiry():
    """set_sandbox_ttl() with a TTL shorter than idle time expires the sandbox."""

def test_sweep_expired_sandboxes_marks_expired():
    """sweep_expired_sandboxes() transitions overdue running sandboxes to expired."""

def test_sweep_does_not_affect_ephemeral_rows():
    """sweep_expired_sandboxes() ignores rows where ttl_s IS NULL."""

def test_warning_fires_once():
    """_sweep_warnings() sets warned_at on first warning and skips on second sweep."""

def test_warning_rearmed_after_refresh():
    """refresh_sandbox() clears warned_at; next sweep can re-fire warning."""

def test_ttl_remaining_s_computation():
    """SandboxSession.ttl_remaining_s returns correct value and clamps to 0."""

def test_format_remaining_human_readable():
    """SandboxSession.format_remaining() returns 'Xh Ym Zs' / 'Xm Ys' / 'Xs'."""

def test_audit_jsonl_written_for_all_events():
    """Each TTL operation writes the correct event type to the JSONL audit log."""

def test_schema_migration_is_idempotent():
    """ensure_schema() on a DB with existing sandbox_runs does not drop data."""

def test_backward_compat_ephemeral_run():
    """run_in_sandbox() without --ttl writes ttl_s=NULL and is unaffected by sweep."""
```

### 11.2 Integration Tests

- **`test_cli_sandbox_refresh`**: Invoke `tag sandbox run --ttl 300 --backend restricted --code "echo hi"`, capture sandbox ID from output, then call `tag sandbox refresh <id>`, verify `last_activity_at` updated in DB.
- **`test_cli_sandbox_set_ttl`**: Create sandbox with `--ttl 300`, call `tag sandbox set-ttl <id> --ttl 600`, verify DB row has `ttl_s=600`.
- **`test_cli_sandbox_list_json_ttl_fields`**: Create sandbox with `--ttl 300`, call `tag sandbox list --json`, assert JSON contains `ttl_remaining_s`, `ttl_s`, `last_activity_at`, `expires_at`.
- **`test_cli_sandbox_set_ttl_immediate_expiry`**: Advance mock clock 60 s past a 30 s TTL, call `tag sandbox set-ttl <id> --ttl 10`, verify sandbox ends in state `expired`.
- **`test_refresh_interval_thread`**: Verify that `_RefreshThread` updates `last_activity_at` at the configured interval and stops cleanly on `stop()`.

### 11.3 Performance Tests

- **Sweep performance**: Populate 1000 rows with `state='running'` and `ttl_s` set. Call `sweep_expired_sandboxes()`. Assert wall time < 50 ms on SQLite WAL mode.
- **Concurrent refresh safety**: Spawn 10 threads each calling `refresh_sandbox()` concurrently on the same sandbox ID. Assert no `sqlite3.OperationalError` (WAL mode handles concurrent writers gracefully).

---

## 12. Acceptance Criteria

| ID | Criteria | Verified By |
|----|----------|-------------|
| AC-01 | `tag sandbox run --ttl 300 --backend restricted --code "echo hi"` exits 0 and stores `ttl_s=300` and `last_activity_at IS NOT NULL` in `sandbox_runs`. | `test_cli_sandbox_ttl_stored` |
| AC-02 | `tag sandbox run` without `--ttl` stores `ttl_s=NULL` and is not affected by subsequent sweep calls. | `test_backward_compat_ephemeral_run` |
| AC-03 | `tag sandbox list --json` returns objects with `ttl_s`, `ttl_remaining_s`, `last_activity_at`, and `expires_at` fields for TTL-enabled sandboxes. | `test_cli_sandbox_list_json_ttl_fields` |
| AC-04 | `tag sandbox refresh <id>` on a running sandbox exits 0 and updates `last_activity_at` to within 1 s of UTC now. | `test_cli_sandbox_refresh` |
| AC-05 | `tag sandbox refresh <id>` on a non-running sandbox (state=`done`) exits 1 with a human-readable error message. | `test_refresh_rejects_non_running_state` |
| AC-06 | `tag sandbox set-ttl <id> --ttl 600` exits 0, updates `ttl_s=600`, resets `last_activity_at`, and clears `warned_at`. | `test_cli_sandbox_set_ttl` |
| AC-07 | `tag sandbox set-ttl <id> --ttl 10` when the sandbox has been idle for 45 s exits 0, immediately calls `_terminate_sandbox_backend()`, and sets `state='expired'`. | `test_cli_sandbox_set_ttl_immediate_expiry` |
| AC-08 | `tag sandbox set-ttl <id> --ttl 29` exits 2 with error message containing "between 30 and 86400". | `test_set_ttl_validates_range` |
| AC-09 | `sweep_expired_sandboxes()` transitions all rows where `idle_s > ttl_s` to state `expired` and writes `expired` events to `sandbox-audit.jsonl`. | `test_sweep_expired_sandboxes_marks_expired` |
| AC-10 | A warning is emitted to stderr and to `notifications.send_notification()` when `ttl_remaining_s <= ttl_warn_s`, and `warned_at` is set to prevent duplicate warnings. | `test_warning_fires_once` |
| AC-11 | After `sandbox refresh`, `warned_at` is cleared and the next sweep can re-fire the warning if the new idle time enters the warn window. | `test_warning_rearmed_after_refresh` |
| AC-12 | `ensure_schema()` on a DB with pre-existing `sandbox_runs` rows (no TTL columns) adds all four new columns without data loss. | `test_schema_migration_is_idempotent` |
| AC-13 | `sandbox-audit.jsonl` contains one entry per event: `created`, `refreshed`, `set-ttl`, `expired`, `warned`, with correct `sandbox_id` and `ttl_s` fields. | `test_audit_jsonl_written_for_all_events` |
| AC-14 | `tag sandbox list` human-readable table shows `REMAINING` column formatted as `Xm Ys` (not raw seconds) for TTL-enabled sandboxes. | Manual / `test_format_remaining_human_readable` |
| AC-15 | `_RefreshThread` daemon thread stops cleanly within `interval_s + 2` seconds of `stop()` being called. | `test_refresh_interval_thread` |

---

## 13. Dependencies

| Dependency | Type | Version / Notes |
|------------|------|-----------------|
| PRD-028 (Sandbox Code Execution) | Blocking prerequisite | `sandbox.py` and `sandbox_runs` table must exist before TTL columns can be added |
| PRD-013 (Agent Tracing) | Soft dependency | Audit log pattern follows tracing conventions; `_utc_now()` from `sandbox.py` is already used |
| PRD-034 (Security Hardening) | Informational | Container ID capture and `docker kill` must comply with PRD-034 blocked-path validation |
| PRD-040 (Notification Hooks) | Soft dependency | `notifications.send_notification()` is called for pre-expiry warnings; if not available, warnings degrade to stderr-only |
| PRD-012 (Cost Tracking) | Future | Sandbox session duration will feed cost attribution in a follow-on PRD; no blocking dependency |
| `docker` CLI | Optional runtime | Must be on `$PATH` for Docker backend TTL enforcement; degraded gracefully if absent |
| `e2b` Python SDK | Optional runtime | Required for E2B backend TTL enforcement via `Sandbox.connect().kill()`; guarded by `try/except ImportError` |
| Python `threading` | stdlib | Used by `_RefreshThread`; no new dependency |
| SQLite WAL mode | Already enabled | `open_db()` in `controller.py` enables WAL; no change required |

---

## 14. Open Questions

| # | Question | Owner | Resolution Target |
|---|----------|-------|-------------------|
| OQ-1 | Should `--ttl` reset on every sandbox command invocation that produces output (auto-activity tracking), or only on explicit `sandbox refresh`? Auto-tracking would require hooking `run_in_sandbox()` to update `last_activity_at` after every call; explicit is simpler but requires callers to opt in. | Sandbox team | Before implementation start |
| OQ-2 | E2B SDK `Sandbox.connect(sandbox_id)` reconnect pattern: is the E2B sandbox ID the same as the TAG `run_id` or does it need to be stored separately as `container_id`? Clarify the mapping in `_get_session()`. | Backend integration | Sprint 1 |
| OQ-3 | Should `tag sandbox list` compute `ttl_remaining_s` at the SQL layer (using `strftime` arithmetic) or in Python post-fetch? SQL is more efficient for large lists; Python is simpler to test. | Implementation | Sprint 1 |
| OQ-4 | What is the right behavior when `--detach` is used without `--ttl`? Options: (a) error, (b) require `--ttl`, (c) default TTL of 3600 s. Current spec says error. | CLI design | Before implementation start |
| OQ-5 | Should the cron scheduler integration (FR-18) be deferred to v1.1 or included in the initial implementation? The lazy sweep covers all interactive use cases; the cron version is only needed for unattended operation. | Team | Sprint 1 planning |
| OQ-6 | Pre-expiry warning: should the warning be rate-limited to fire at most once per warn window (current design) or fire on every sweep cycle while in the warn window? Current design fires once and requires a refresh to re-arm; continuous warnings would be more visible but noisier. | UX | Before implementation start |
| OQ-7 | How should TTL interact with `tag sandbox run --timeout`? Current design: `--timeout` is the per-invocation wall-clock limit for the subprocess; `--ttl` is the idle-session TTL. For detached sessions, `--timeout` has no meaning. Clarify in CLI help text. | Docs | Sprint 1 |
| OQ-8 | Should `sandbox-audit.jsonl` rotate? Large installs with many short-TTL sandboxes could produce a large JSONL file. Consider a max-size or daily-rotation policy. | Ops | v1.1 |

---

## 15. Complexity and Timeline

**Overall Estimate:** S — 3–5 days for one engineer familiar with `sandbox.py` and `controller.py`.

### Phase 1 — Schema & Core Logic (Day 1–2)

| Task | Effort |
|------|--------|
| Add TTL columns to `ensure_schema()` with `PRAGMA table_info` migration guard | 1 h |
| Implement `SandboxSession` and `TTLEvent` dataclasses | 1 h |
| Implement `sweep_expired_sandboxes()` and `_sweep_warnings()` | 2 h |
| Implement `refresh_sandbox()` and `set_sandbox_ttl()` | 2 h |
| Implement `_terminate_sandbox_backend()` with Docker cidfile capture | 1.5 h |
| Implement `_write_audit()` and audit log integration | 1 h |
| Unit tests for all core functions (mock clock) | 3 h |

### Phase 2 — CLI Surface (Day 3)

| Task | Effort |
|------|--------|
| Add `--ttl`, `--ttl-warn`, `--refresh-interval`, `--detach` to `sandbox run` argparse | 1 h |
| Add `refresh` subcommand argparse + `cmd_sandbox` branch | 1 h |
| Add `set-ttl` subcommand argparse + `cmd_sandbox` branch | 1 h |
| Extend `sandbox list` with TTL columns (table + JSON) | 1.5 h |
| Prepend lazy sweep to `cmd_sandbox` dispatch | 0.5 h |
| Implement `_RefreshThread` for `--refresh-interval` | 1.5 h |

### Phase 3 — Integration & Validation (Day 4–5)

| Task | Effort |
|------|--------|
| Integration tests (CLI end-to-end, DB state verification) | 3 h |
| Performance test (1000-row sweep < 50 ms) | 1 h |
| Manual test against real Docker backend (cidfile capture, `docker kill`) | 1.5 h |
| Update `sandbox-audit.jsonl` format in `CHANGELOG` / module docstring | 0.5 h |
| Code review and iteration | 2 h |
| Verify backward compatibility with existing `tag sandbox run` tests | 1 h |

### Milestone Summary

| Milestone | Target Day |
|-----------|------------|
| Core logic + unit tests passing | Day 2 EOD |
| CLI surface wired + all new subcommands functional | Day 3 EOD |
| Integration tests passing + Docker backend validated | Day 4 EOD |
| PR open, review feedback incorporated | Day 5 EOD |

