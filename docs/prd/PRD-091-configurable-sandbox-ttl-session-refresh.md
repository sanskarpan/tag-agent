# PRD-091: Configurable Sandbox TTL + Session Refresh (`tag sandbox set-ttl`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** S (3-5 days)
**Category:** Sandbox & Execution Environment
**Affects:** `internal/sandbox`
**Depends on:** PRD-028 (Sandbox Code Execution), PRD-013 (Agent Tracing / Observability), PRD-034 (Security Hardening), PRD-012 (Cost Tracking / Budget), PRD-040 (Notification Hooks)
**Inspired by:** E2B sandbox TTL, Daytona workspace timeout, Gitpod timeout
**GitHub Issue:** #348

---

## 1. Overview

TAG's current sandbox implementation (`internal/sandbox`, PRD-028) supports ephemeral command execution across a tiered isolation ladder — a restricted tier (landlock-lsm/go-landlock + elastic/go-seccomp-bpf + google/nftables), Docker (docker/docker moby client), and cloud backends (E2B/Modal via HTTP) — with a single per-invocation `--timeout` wall-clock limit enforced through `context.WithTimeout`. Once `RunInSandbox()` returns, there is no persistent sandbox session and therefore no TTL concept: the sandbox exists only for the duration of the `os/exec` child process (or cloud call) and is immediately destroyed. This model works well for one-shot code execution tasks but breaks down for interactive or long-lived workflows where an agent iterates inside the same sandbox environment — running tests, building artifacts, and debugging across multiple tool calls without paying the container startup cost on every call.

Cloud sandbox providers have independently converged on a TTL-plus-keepalive model as the canonical lifecycle for long-lived sandboxes. E2B uses `timeout` at creation time (up to 86 400 s on Pro tier) with `sandbox.set_timeout()` and `sandbox.keep_alive()` for mid-session extension; every resume resets the idle timer. Daytona's workspace timeout is configured at the workspace level and can be updated via `UpdateWorkspaceDTO.auto_stop_minutes`; it expires on idle CPU/network quiescence. Gitpod uses per-workspace `timeout` strings (`"1h"`, `"30m"`) with optional `--extended` flags during active sessions. All three providers separately expose a session-level "refresh" or "keepalive" primitive so that programmatic automation can extend a running sandbox without creating a new one.

This PRD adds configurable per-sandbox TTL and session refresh to TAG's sandbox subsystem. The core idea is: a sandbox session is now a first-class persistent record in `modernc.org/sqlite` (WAL, single-writer) with a creation time, a configured TTL, a last-activity timestamp (`expires_at`), and a derived time-to-live. A long-lived reaper goroutine (driven by a `time.Ticker`, optionally scheduled via `go-co-op/gocron` v2) terminates sandboxes whose idle time has exceeded their TTL and fires a pre-expiry warning notification via the `notifications` package when a sandbox is within a configurable warning window (default: 60 seconds). The new `tag sandbox refresh <id>` command sends a keepalive that resets the last-activity clock, extending the session without changing the TTL contract. The new `tag sandbox set-ttl <id> --ttl <seconds>` command mutates the TTL for a running session, allowing operators to shorten or extend lifetime dynamically.

These additions are additive and backward compatible. Existing `tag sandbox run` invocations without `--ttl` default to the existing `--timeout` wall-clock behavior; TTL management only activates when a sandbox is launched with `--ttl` and enters the `running` state as a persistent session record. The change touches the `internal/sandbox` package for all new logic, the `tag sandbox` command group (Cobra) for the three new subcommands, and a one-time schema migration adds four new columns to `sandbox_runs`.

The feature has direct practical impact for agents that iterate inside a sandbox across multiple turns (code→test→fix cycles), for queue workers that reserve a Docker container for a batch job and release it when done, and for any scenario where the operator wants deterministic resource cleanup without relying on manual `tag sandbox kill` calls.

---

## 2. Problem Statement

### 2.1 No Lifecycle Management for Long-Lived Sandbox Sessions

`RunInSandbox()` is a blocking call: it spawns the backend (via `os/exec.CommandContext`), waits for the command to complete, records the result, and returns. There is no concept of a "session" that survives across multiple agent tool calls. When a TAG agent needs to run three sequential commands in the same Docker container — install dependencies, run tests, capture coverage — it must start three separate containers, paying startup latency on each invocation and losing all in-memory state between calls. The sandbox audit trail in `sandbox_runs` records three unrelated rows with no common session identity.

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
| G5 | Pre-expiry notifications: when a sandbox has ≤ `ttl_warn_secs` seconds remaining (default 60), a warning is emitted to the terminal and via the `internal/notifications` hooks (Slack, webhook, etc.). |
| G6 | TTL sweep runs on a configurable interval (default 30 s) without requiring a daemon process: it is triggered lazily on any `tag sandbox` command and by a reaper goroutine, and optionally by the `go-co-op/gocron` scheduler when available. |
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
| TTL enforcement latency | Expired sandbox terminated within `sweep_interval + 5 s` of its TTL deadline | Unit test: advance the fake `utcNow()` clock past deadline, run sweep, assert status=`expired` |
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
  --code <code_string>           # inline code shortcut (workload language is user-chosen)
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
| FR-05 | `tag sandbox set-ttl <id> --ttl <N>` updates `ttl_s` and resets `last_activity_at = utcNow()`. If the new TTL is already exceeded by the current idle time, the function calls `terminateSandbox()` immediately and sets state to `expired`. | Must |
| FR-06 | The TTL sweep function `SweepExpiredSandboxes(ctx, db)` selects all rows where `ttl_s IS NOT NULL AND state = 'running' AND (strftime('%s','now') - strftime('%s', last_activity_at)) > ttl_s` and calls `terminateSandbox()` on each, updating state to `expired`. | Must |
| FR-07 | The TTL sweep is invoked lazily at the start of every `sandbox` command (any subcommand) and by the reaper goroutine; it emits a `log/slog` line at DEBUG level listing how many sandboxes were swept. | Must |
| FR-08 | Pre-expiry warning: for each sandbox where `ttl_remaining_s <= ttl_warn_s` and `state = 'running'` and `warned_at IS NULL`, emit a terminal warning and call `notifications.Send()` with message `"Sandbox <id> expires in <N>s"`. Set `warned_at = utcNow()` to prevent repeat warnings. | Must |
| FR-09 | Warning is re-armed after each `sandbox refresh` or `set-ttl` call by setting `warned_at = NULL`. | Must |
| FR-10 | `terminateSandbox(ctx, db, runID)` must call the appropriate backend cleanup: `ContainerKill` via the moby client (docker/docker) for Docker; the E2B/Modal HTTP kill endpoint for cloud backends; process-group kill (`syscall.Kill(-pgid, SIGKILL)` on a `Setpgid` group) for the restricted tier if a child process survives; no-op for already-completed one-shots. | Must |
| FR-11 | `--refresh-interval <seconds>` in `tag sandbox run` starts a `time.Ticker`-driven goroutine that calls `RefreshSandbox(ctx, db, runID)` every `<seconds>` while the main goroutine is blocking on the child process. The goroutine is cancelled via a `context.Context` on process exit. | Should |
| FR-12 | `tag sandbox list --active` filters to rows where `state = 'running'`. `--expired` filters to `state = 'expired'`. Without flags, all states are shown. | Should |
| FR-13 | `tag sandbox run --detach` inserts the sandbox record, starts the backend process as a non-blocking detached child (`os/exec.Cmd` with `SysProcAttr{Setpgid: true}` and stdout/stderr redirected to a temp file), and returns immediately, printing the sandbox ID. Requires `--ttl`. | Should |
| FR-14 | Every TTL event (`created`, `refreshed`, `set-ttl`, `expired`, `warned`, `terminated`) is appended to `~/.tag/runtime/sandbox-audit.jsonl` as a JSON object (`encoding/json`) with fields: `event`, `sandbox_id`, `timestamp`, `ttl_s`, `caller` (subcommand name). | Must |
| FR-15 | `tag sandbox set-ttl` validates that `--ttl` is between 30 and 86400 (inclusive) and that `--ttl-warn`, if provided, is strictly less than `--ttl`. Out-of-range values exit with code 2 and a clear error message. | Must |
| FR-16 | `sandbox_runs.container_id` column (added by schema migration) stores the Docker container ID (returned directly by the moby client `ContainerCreate` call) so that `terminateSandbox()` can issue `ContainerKill` for long-lived Docker sessions. | Must |
| FR-17 | Schema migration uses guarded `ALTER TABLE sandbox_runs ADD COLUMN` statements — since SQLite does not support `IF NOT EXISTS` for `ALTER TABLE`, migration is guarded by a `PRAGMA table_info` check in `EnsureSchema()` (executed over `modernc.org/sqlite`). | Must |
| FR-18 | `go-co-op/gocron` v2 integration: if the scheduler is available and running, register a job `tag_sandbox_sweep` with interval 30 s that calls `SweepExpiredSandboxes`. This is advisory; the reaper goroutine plus lazy sweep in the `sandbox` command are the primary mechanism. | Won't (v1) |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | `sweep_expired_sandboxes()` must complete in < 50 ms for up to 1000 active sandbox rows on a standard developer laptop (SQLite WAL mode). | Performance |
| NFR-02 | `sandbox refresh` end-to-end (CLI invocation to DB commit to JSON response) must complete in < 200 ms. | Performance |
| NFR-03 | TTL timestamp arithmetic stores RFC3339 UTC strings (`time.Time` in `time.UTC`, formatted via `time.RFC3339`) and uses SQLite's `strftime('%s', ...)` for portable epoch arithmetic at query time; no wall-clock-local timestamps are persisted. | Portability |
| NFR-04 | The `--refresh-interval` goroutine must not prevent clean process exit: it is bound to a `context.Context` cancelled on shutdown, drains its `time.Ticker`, and is awaited via an `errgroup.Group`/`sync.WaitGroup` before exit. | Reliability |
| NFR-05 | All new functions are covered by unit tests that inject a fake clock (a `func() time.Time` seam replacing `utcNow()`) to simulate time advancement without `time.Sleep`. | Testability |
| NFR-06 | `sandbox-audit.jsonl` is written with `os.OpenFile(path, O_APPEND\|O_CREATE\|O_WRONLY, 0o600)` and each write is a single `json.Marshal(event)` + newline `Write`, making it safe for concurrent writers on POSIX. | Reliability |
| NFR-07 | No new mandatory dependencies beyond the canonical stack. Docker TTL management uses the already-vendored `docker/docker` moby client. Cloud (E2B/Modal) TTL uses `net/http` against the provider REST API, feature-detected at runtime. | Dependency |
| NFR-08 | Human-readable `ttl_remaining_s` display formats seconds as `Xh Ym Zs`, `Xm Ys`, or `Xs` as appropriate (no raw second counts in the table). | UX |
| NFR-09 | `tag sandbox list --json` output is stable across minor version updates: new fields are always additive; no existing fields are renamed or removed. | Stability |
| NFR-10 | SQLite WAL mode is already enabled by `OpenDB()` (over `modernc.org/sqlite`, single-writer); no additional locking or journaling configuration is required for TTL sweep concurrency. | Correctness |

---

## 9. Technical Design

### 9.1 Schema Migration

The existing `sandbox_runs` table (defined in `internal/sandbox.EnsureSchema()`, executed over a `database/sql` handle backed by `modernc.org/sqlite`) gains four new columns. Because SQLite does not support `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`, the migration function checks `PRAGMA table_info(sandbox_runs)` before each `ALTER TABLE`.

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

### 9.2 Core Types (structs)

Pointer fields (`*string`, `*int64`, `*time.Time`) model SQL `NULL`; ephemeral sandboxes leave `TTLSecs`/`LastActivityAt` nil. Derived values (`ttl_remaining_s`, `expires_at`) are computed by methods and injected into the JSON view via a marshaling shim, mirroring the pydantic computed-field behavior.

```go
// internal/sandbox/session.go  (additions for PRD-091)
package sandbox

import (
	"encoding/json"
	"fmt"
	"time"
)

// clock is the injectable time source; overridden in tests. Defaults to UTC now.
var utcNow = func() time.Time { return time.Now().UTC() }

// SandboxSession represents a persistent sandbox session with TTL lifecycle.
type SandboxSession struct {
	ID             string     `json:"id"`
	Command        string     `json:"command"`
	Backend        string     `json:"backend"`
	Image          *string    `json:"image"`
	ContainerID    *string    `json:"container_id"`
	State          string     `json:"state"` // running | done | failed | expired | killed
	ExitCode       *int       `json:"exit_code"`
	CreatedAt      time.Time  `json:"created_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	LastActivityAt *time.Time `json:"last_activity_at"` // nil for ephemeral
	TTLSecs        *int64     `json:"ttl_s"`            // nil = ephemeral
	TTLWarnSecs    int64      `json:"ttl_warn_s"`
	WarnedAt       *time.Time `json:"-"`
}

// IsEphemeral reports whether the session has no TTL.
func (s *SandboxSession) IsEphemeral() bool { return s.TTLSecs == nil }

// TTLRemaining returns seconds until expiry based on LastActivityAt, clamped to
// >= 0. The bool is false when the session is ephemeral.
func (s *SandboxSession) TTLRemaining() (int64, bool) {
	if s.TTLSecs == nil || s.LastActivityAt == nil {
		return 0, false
	}
	elapsed := int64(utcNow().Sub(*s.LastActivityAt).Seconds())
	remaining := *s.TTLSecs - elapsed
	if remaining < 0 {
		remaining = 0
	}
	return remaining, true
}

// ExpiresAt returns the RFC3339 UTC expiry timestamp; ok is false if ephemeral.
func (s *SandboxSession) ExpiresAt() (time.Time, bool) {
	if s.TTLSecs == nil || s.LastActivityAt == nil {
		return time.Time{}, false
	}
	return s.LastActivityAt.Add(time.Duration(*s.TTLSecs) * time.Second), true
}

// FormatRemaining renders a human-readable TTL string, e.g. "4m 32s".
func (s *SandboxSession) FormatRemaining() string {
	r, ok := s.TTLRemaining()
	if !ok {
		return "—"
	}
	if r == 0 {
		return "expired"
	}
	h, rem := r/3600, r%3600
	m, sec := rem/60, rem%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm %ds", h, m, sec)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, sec)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}

// MarshalJSON emits the stable list/refresh view, including derived fields.
func (s *SandboxSession) MarshalJSON() ([]byte, error) {
	type alias SandboxSession // avoid recursion
	var remaining *int64
	if r, ok := s.TTLRemaining(); ok {
		remaining = &r
	}
	var expires *time.Time
	if e, ok := s.ExpiresAt(); ok {
		expires = &e
	}
	return json.Marshal(struct {
		*alias
		TTLRemainingSecs *int64     `json:"ttl_remaining_s"`
		ExpiresAt        *time.Time `json:"expires_at"`
	}{alias: (*alias)(s), TTLRemainingSecs: remaining, ExpiresAt: expires})
}

// TTLEvent is an audit log entry for a TTL lifecycle event.
type TTLEvent struct {
	Event      string         `json:"event"` // created | refreshed | set-ttl | expired | warned | terminated
	SandboxID  string         `json:"sandbox_id"`
	Timestamp  time.Time      `json:"timestamp"`
	TTLSecs    *int64         `json:"ttl_s"`
	Caller     string         `json:"caller"` // subcommand name or "sweep"
	Extra      map[string]any `json:"extra,omitempty"`
}

// JSONL renders the event as a single JSON line (no trailing newline).
func (e TTLEvent) JSONL() ([]byte, error) { return json.Marshal(e) }
```

### 9.3 TTL Sweep Algorithm

The sweep runs both lazily (start of any `sandbox` command) and continuously from a reaper goroutine. All queries take a `context.Context` so the reaper can be cancelled on shutdown. Backend teardown fans out with a bounded `errgroup` and never blocks the DB transaction.

```go
// internal/sandbox/ttl.go
package sandbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

const (
	TTLMin       = 30
	TTLMax       = 86400
	auditLogName = "sandbox-audit.jsonl"
)

func auditDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".tag", "runtime")
}

// writeAudit appends a TTL event to the JSONL audit log (O_APPEND safe).
func writeAudit(ev TTLEvent) error {
	dir := auditDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	line, err := ev.JSONL()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, auditLogName),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// SweepExpiredSandboxes terminates all running sandboxes whose idle time exceeds
// their TTL and returns the number swept. Portable SQLite epoch predicate:
//
//	strftime('%s','now') - strftime('%s', last_activity_at) > ttl_s
func SweepExpiredSandboxes(ctx context.Context, db *sql.DB) (int, error) {
	now := utcNow()
	rows, err := db.QueryContext(ctx, `
		SELECT id, backend, container_id, ttl_s
		FROM   sandbox_runs
		WHERE  state = 'running'
		  AND  ttl_s IS NOT NULL
		  AND  last_activity_at IS NOT NULL
		  AND  (CAST(strftime('%s','now') AS INTEGER)
		        - CAST(strftime('%s', last_activity_at) AS INTEGER)) > ttl_s`)
	if err != nil {
		return 0, err
	}
	type victim struct {
		id, backend string
		containerID *string
		ttl         int64
	}
	var victims []victim
	for rows.Next() {
		var v victim
		if err := rows.Scan(&v.id, &v.backend, &v.containerID, &v.ttl); err != nil {
			rows.Close()
			return 0, err
		}
		victims = append(victims, v)
	}
	rows.Close()

	swept := 0
	for _, v := range victims {
		terminateSandboxBackend(ctx, v.backend, v.containerID)
		if _, err := db.ExecContext(ctx,
			`UPDATE sandbox_runs SET state='expired', completed_at=? WHERE id=?`,
			now.Format(time.RFC3339), v.id); err != nil {
			return swept, err
		}
		ttl := v.ttl
		_ = writeAudit(TTLEvent{
			Event: "expired", SandboxID: v.id, Timestamp: now,
			TTLSecs: &ttl, Caller: "sweep",
		})
		swept++
	}

	// Fire pre-expiry warnings for sandboxes approaching TTL.
	if err := sweepWarnings(ctx, db); err != nil {
		return swept, err
	}
	return swept, nil
}

// sweepWarnings fires pre-expiry warnings for sandboxes within their warn window.
func sweepWarnings(ctx context.Context, db *sql.DB) error {
	now := utcNow()
	rows, err := db.QueryContext(ctx, `
		SELECT id, ttl_s, ttl_warn_s,
		       (CAST(strftime('%s','now') AS INTEGER)
		        - CAST(strftime('%s', last_activity_at) AS INTEGER)) AS idle_s
		FROM   sandbox_runs
		WHERE  state = 'running'
		  AND  ttl_s IS NOT NULL
		  AND  last_activity_at IS NOT NULL
		  AND  warned_at IS NULL
		  AND  (ttl_s - (CAST(strftime('%s','now') AS INTEGER)
		                 - CAST(strftime('%s', last_activity_at) AS INTEGER))) <= ttl_warn_s
		  AND  (ttl_s - (CAST(strftime('%s','now') AS INTEGER)
		                 - CAST(strftime('%s', last_activity_at) AS INTEGER))) > 0`)
	if err != nil {
		return err
	}
	type warn struct {
		id   string
		ttl  int64
		idle int64
	}
	var warns []warn
	for rows.Next() {
		var w warn
		var warnSecs int64
		if err := rows.Scan(&w.id, &w.ttl, &warnSecs, &w.idle); err != nil {
			rows.Close()
			return err
		}
		warns = append(warns, w)
	}
	rows.Close()

	for _, w := range warns {
		remaining := w.ttl - w.idle
		emitTTLWarning(w.id, remaining)
		if _, err := db.ExecContext(ctx,
			`UPDATE sandbox_runs SET warned_at=? WHERE id=?`,
			now.Format(time.RFC3339), w.id); err != nil {
			return err
		}
		ttl := w.ttl
		_ = writeAudit(TTLEvent{
			Event: "warned", SandboxID: w.id, Timestamp: now,
			TTLSecs: &ttl, Caller: "sweep",
			Extra: map[string]any{"remaining_s": remaining},
		})
	}
	return nil
}

// emitTTLWarning prints a terminal warning and calls the notifications hook.
func emitTTLWarning(sandboxID string, remainingS int64) {
	msg := fmt.Sprintf(
		"[TAG WARNING] Sandbox %s expires in %s — run `tag sandbox refresh %s` to extend.",
		sandboxID, fmtSeconds(remainingS), sandboxID)
	fmt.Fprintln(os.Stderr, msg)
	// notifications are best-effort; errors are logged, not propagated.
	if err := notifications.Send(notifications.Message{
		Title: "Sandbox Expiry Warning",
		Body:  msg,
		Level: "warning",
	}); err != nil {
		slog.Debug("ttl warning notification failed", "err", err)
	}
}

// terminateSandboxBackend issues backend-specific termination. Errors are
// logged at DEBUG and swallowed (best-effort teardown).
func terminateSandboxBackend(ctx context.Context, backend string, containerID *string) {
	switch backend {
	case "docker":
		if containerID != nil {
			cli, err := dockerClient() // docker/docker moby client, cached
			if err == nil {
				kctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				_ = cli.ContainerKill(kctx, *containerID, "KILL")
			}
		}
	case "e2b", "modal":
		// Cloud backends: containerID holds the provider sandbox ID; issue a
		// DELETE against the provider REST API via net/http (feature-detected).
		if containerID != nil {
			_ = killCloudSandbox(ctx, backend, *containerID)
		}
	// restricted tier: one-shot child already reaped via process-group kill; no-op.
	default:
	}
}

// fmtSeconds formats seconds as a human-readable string.
func fmtSeconds(s int64) string {
	h, rem := s/3600, s%3600
	m, sec := rem/60, rem%60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm %ds", h, m, sec)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, sec)
	default:
		return fmt.Sprintf("%ds", sec)
	}
}
```

### 9.4 Refresh and Set-TTL Functions

Sentinel errors (`ErrNotFound`, `ErrNotRunning`, `ErrEphemeral`, `ErrInvalidTTL`) let the command layer map failures to exit codes via `errors.Is`, replacing Python's `ValueError` string matching.

```go
// internal/sandbox/refresh.go
package sandbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrNotFound   = errors.New("sandbox not found")
	ErrNotRunning = errors.New("sandbox is not in running state")
	ErrEphemeral  = errors.New("sandbox is ephemeral (no TTL)")
	ErrInvalidTTL = errors.New("invalid TTL value")
)

// RefreshSandbox resets last_activity_at for a running sandbox (keepalive).
func RefreshSandbox(ctx context.Context, db *sql.DB, runID string) (*SandboxSession, error) {
	var state string
	var ttl *int64
	err := db.QueryRowContext(ctx,
		`SELECT state, ttl_s FROM sandbox_runs WHERE id=?`, runID).Scan(&state, &ttl)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, runID)
	} else if err != nil {
		return nil, err
	}
	if state != "running" {
		return nil, fmt.Errorf("%w: %q is %q", ErrNotRunning, runID, state)
	}
	if ttl == nil {
		return nil, fmt.Errorf("%w: %q", ErrEphemeral, runID)
	}

	now := utcNow()
	if _, err := db.ExecContext(ctx,
		`UPDATE sandbox_runs SET last_activity_at=?, warned_at=NULL WHERE id=?`,
		now.Format(time.RFC3339), runID); err != nil {
		return nil, err
	}
	_ = writeAudit(TTLEvent{
		Event: "refreshed", SandboxID: runID, Timestamp: now, TTLSecs: ttl, Caller: "refresh",
	})
	return getSession(ctx, db, runID)
}

// SetSandboxTTL updates the TTL for a running sandbox and resets
// last_activity_at. If newTTLSecs is already exceeded by the current idle time,
// the sandbox is terminated immediately by the follow-up sweep.
func SetSandboxTTL(ctx context.Context, db *sql.DB, runID string, newTTLSecs int64, newWarnSecs *int64) (*SandboxSession, error) {
	if newTTLSecs < TTLMin || newTTLSecs > TTLMax {
		return nil, fmt.Errorf("%w: --ttl must be between %d and %d, got %d",
			ErrInvalidTTL, TTLMin, TTLMax, newTTLSecs)
	}

	var state string
	var oldTTL, oldWarn *int64
	var backend string
	var containerID *string
	err := db.QueryRowContext(ctx,
		`SELECT state, ttl_s, ttl_warn_s, backend, container_id FROM sandbox_runs WHERE id=?`,
		runID).Scan(&state, &oldTTL, &oldWarn, &backend, &containerID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, runID)
	} else if err != nil {
		return nil, err
	}
	if state != "running" {
		return nil, fmt.Errorf("%w: %q is %q", ErrNotRunning, runID, state)
	}

	warn := oldWarn
	if newWarnSecs != nil {
		warn = newWarnSecs
	}
	if warn != nil && *warn >= newTTLSecs {
		return nil, fmt.Errorf("%w: --ttl-warn (%ds) must be less than --ttl (%ds)",
			ErrInvalidTTL, *warn, newTTLSecs)
	}

	now := utcNow()
	if _, err := db.ExecContext(ctx,
		`UPDATE sandbox_runs
		   SET ttl_s=?, ttl_warn_s=?, last_activity_at=?, warned_at=NULL
		   WHERE id=?`,
		newTTLSecs, warn, now.Format(time.RFC3339), runID); err != nil {
		return nil, err
	}
	_ = writeAudit(TTLEvent{
		Event: "set-ttl", SandboxID: runID, Timestamp: now, TTLSecs: &newTTLSecs, Caller: "set-ttl",
		Extra: map[string]any{"old_ttl_s": oldTTL, "new_warn_s": warn},
	})

	// Run sweep immediately: if newTTLSecs < current idle time, expires now.
	if _, err := SweepExpiredSandboxes(ctx, db); err != nil {
		return nil, err
	}
	return getSession(ctx, db, runID)
}

// getSession fetches a SandboxSession from the database by ID.
func getSession(ctx context.Context, db *sql.DB, runID string) (*SandboxSession, error) {
	var s SandboxSession
	var warn *int64
	err := db.QueryRowContext(ctx, `
		SELECT id, command, backend, image, container_id, state, exit_code,
		       created_at, completed_at, last_activity_at, ttl_s, ttl_warn_s, warned_at
		FROM sandbox_runs WHERE id=?`, runID).Scan(
		&s.ID, &s.Command, &s.Backend, &s.Image, &s.ContainerID, &s.State, &s.ExitCode,
		&s.CreatedAt, &s.CompletedAt, &s.LastActivityAt, &s.TTLSecs, &warn, &s.WarnedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, runID)
	} else if err != nil {
		return nil, err
	}
	if warn != nil {
		s.TTLWarnSecs = *warn
	} else {
		s.TTLWarnSecs = 60
	}
	return &s, nil
}
```

### 9.5 Background Refresh Goroutine

A goroutine driven by a `time.Ticker` calls `RefreshSandbox` every `interval` while `tag sandbox run --refresh-interval` blocks on the child process. It shares the single WAL-mode `*sql.DB` handle (the connection pool is safe for concurrent use, so no per-call connection factory is needed) and is torn down via `context.Context` cancellation — the goroutine cannot outlive the command.

```go
// internal/sandbox/keepalive.go
package sandbox

import (
	"context"
	"database/sql"
	"log/slog"
	"time"
)

// StartRefresher launches a keepalive goroutine that refreshes runID every
// interval until ctx is cancelled. The returned func blocks until the goroutine
// has exited, giving the caller a clean shutdown barrier.
func StartRefresher(ctx context.Context, db *sql.DB, runID string, interval time.Duration) (stop func()) {
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(ctx)

	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := RefreshSandbox(ctx, db, runID); err != nil {
					// Log and continue; a background failure must not crash the run.
					slog.Debug("keepalive refresh failed", "sandbox", runID, "err", err)
				}
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}
```

### 9.6 Command Integration Points

The `tag sandbox` command group (Cobra, `cmd/tag/sandbox.go`) gains three new leaf commands — `refresh`, `set-ttl`, and an updated `list`. A `PersistentPreRunE` on the group runs the lazy sweep before every subcommand. Cobra's `RunE` returns errors, which are mapped to process exit codes by a small `errors.Is` switch in `main`; the sentinel errors from §9.4 drive codes 1 and 2.

```go
// cmd/tag/sandbox.go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/tag-agent/tag/internal/sandbox"
)

func newSandboxCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sandbox",
		Short: "Manage sandbox execution sessions",
		// PRD-091: lazy TTL sweep before every sandbox subcommand.
		PersistentPreRunE: func(c *cobra.Command, _ []string) error {
			_, err := sandbox.SweepExpiredSandboxes(c.Context(), app.DB)
			return err
		},
	}
	cmd.AddCommand(newSandboxRefreshCmd(app), newSandboxSetTTLCmd(app) /* + run/list/result */)
	return cmd
}

func newSandboxRefreshCmd(app *App) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "refresh SANDBOX_ID",
		Short: "Reset TTL idle timer for a running sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			sess, err := sandbox.RefreshSandbox(c.Context(), app.DB, args[0])
			if err != nil {
				return err // exit-code mapper turns ErrNotRunning/ErrNotFound into 1
			}
			return printSession(c, sess, asJSON, "refreshed")
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON confirmation object")
	return c
}

func newSandboxSetTTLCmd(app *App) *cobra.Command {
	var ttl, ttlWarn int64
	var asJSON bool
	c := &cobra.Command{
		Use:   "set-ttl SANDBOX_ID --ttl SECONDS",
		Short: "Update TTL for a running sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			var warn *int64
			if c.Flags().Changed("ttl-warn") {
				warn = &ttlWarn
			}
			sess, err := sandbox.SetSandboxTTL(c.Context(), app.DB, args[0], ttl, warn)
			if err != nil {
				return err // ErrInvalidTTL → exit 2; ErrNotFound/ErrNotRunning → exit 1
			}
			return printSession(c, sess, asJSON, "set-ttl")
		},
	}
	c.Flags().Int64Var(&ttl, "ttl", 0, "new TTL in seconds (30–86400)")
	c.Flags().Int64Var(&ttlWarn, "ttl-warn", 60, "new warning threshold (must be < --ttl)")
	c.Flags().BoolVar(&asJSON, "json", false, "emit JSON confirmation object")
	_ = c.MarkFlagRequired("ttl")
	return c
}

// printSession renders either JSON or a one-line human summary.
func printSession(c *cobra.Command, s *sandbox.SandboxSession, asJSON bool, verb string) error {
	if asJSON {
		b, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(c.OutOrStdout(), string(b))
		return nil
	}
	exp, _ := s.ExpiresAt()
	fmt.Fprintf(c.OutOrStdout(), "Sandbox %s %s.  New expiry: %s\n",
		s.ID, verb, exp.Format(time.RFC3339))
	return nil
}

// mapExitCode (in main): errors.Is(err, sandbox.ErrInvalidTTL) → 2; other
// sentinel errors → 1; nil → 0.
var _ = context.Background // ctx flows from cobra's c.Context()
```

### 9.7 Cobra Flag Registration

Beyond the `refresh` and `set-ttl` commands in §9.6, the existing `run` and `list` commands gain new flags. `--ttl` uses `0` as the "unset" sentinel (ephemeral); flag values are validated in `RunE`, and `--detach` requires `--ttl` (enforced with `cobra.MarkFlagsRequiredTogether` / an explicit check).

```go
// cmd/tag/sandbox.go — flag registration on the run and list commands

func addRunTTLFlags(run *cobra.Command, o *runOpts) {
	f := run.Flags()
	f.Int64Var(&o.TTL, "ttl", 0,
		"per-sandbox idle TTL in seconds (30–86400); 0 = ephemeral, enables session persistence")
	f.Int64Var(&o.TTLWarn, "ttl-warn", 60,
		"warn N seconds before TTL expiry")
	f.DurationVar(&o.RefreshInterval, "refresh-interval", 0,
		"auto-keepalive interval, e.g. 60s (requires --ttl)")
	f.BoolVar(&o.Detach, "detach", false,
		"return immediately after starting sandbox (requires --ttl)")
	// --detach is meaningless without --ttl; enforced in RunE:
	//   if o.Detach && o.TTL == 0 { return errors.New("--detach requires --ttl") }
}

func addListFilterFlags(list *cobra.Command, o *listOpts) {
	f := list.Flags()
	f.BoolVar(&o.Active, "active", false, "show only running sandboxes")
	f.BoolVar(&o.Expired, "expired", false, "show only expired sandboxes")
	f.BoolVar(&o.JSON, "json", false, "emit JSON array (extended schema)")
}
```

### 9.8 Docker Container ID Capture

For `terminateSandboxBackend` to issue `ContainerKill`, the container ID must be captured at launch. Unlike the shell `docker run --cidfile` dance, the moby client (`docker/docker`) returns the container ID directly from `ContainerCreate`, so no temp file is needed. The existing Docker backend is modified to create-then-start-then-wait and return the ID for persistence in `sandbox_runs.container_id`.

```go
// internal/sandbox/backend_docker.go
package sandbox

import (
	"context"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

type dockerResult struct {
	ExitCode    int
	Stdout      string
	Stderr      string
	ContainerID string
}

// runDockerSession runs command inside a Docker container, capturing the
// container ID so a long-lived TTL session can later be killed by ID.
func runDockerSession(ctx context.Context, cli *client.Client, cmd []string, image string, timeout time.Duration, runID string) (dockerResult, error) {
	stopTimeout := int(timeout.Seconds())
	create, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:  image,
			Cmd:    cmd,
			Labels: map[string]string{"tag.sandbox.id": runID},
		},
		&container.HostConfig{
			AutoRemove:  true,
			NetworkMode: "none",
			Resources: container.Resources{
				Memory:   512 * 1024 * 1024, // 512m
				NanoCPUs: 1_000_000_000,     // 1 cpu
			},
			StopTimeout: &stopTimeout,
		},
		nil, nil, "")
	if err != nil {
		return dockerResult{}, err // e.g. client.IsErrConnectionFailed → "docker not available; use --backend restricted"
	}
	res := dockerResult{ContainerID: create.ID}

	if err := cli.ContainerStart(ctx, create.ID, container.StartOptions{}); err != nil {
		return res, err
	}

	// Wait bounded by ctx (caller sets context.WithTimeout(timeout+grace)).
	statusCh, errCh := cli.ContainerWait(ctx, create.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		return res, err
	case st := <-statusCh:
		res.ExitCode = int(st.StatusCode)
	case <-ctx.Done():
		res.ExitCode = 124 // timed out
		return res, ctx.Err()
	}

	// Stdout/stderr collected via cli.ContainerLogs(ctx, create.ID, ...) with the
	// stdcopy demultiplexer; omitted here for brevity.
	return res, nil
}
```

### 9.9 Integration with Existing Modules

| Package | Integration point | Change |
|--------|-------------------|--------|
| `internal/notifications` | `emitTTLWarning()` calls `notifications.Send()` with `Level: "warning"` | Caller-side only; `internal/notifications` unchanged |
| `internal/scheduler` (`go-co-op/gocron` v2) | `SweepExpiredSandboxes` registered as an advisory 30 s job | Deferred to v1.1; reaper goroutine + lazy sweep are primary |
| `internal/tracing` | TTL events reuse the injectable `utcNow()` seam; no OTel spans added for sweep | No change |
| `internal/budget` | Sandbox session duration contributes to cost attribution in a future PRD; no change in this PRD | No change |
| `cmd/tag` (Cobra) | `sandbox` group gains `refresh` and `set-ttl` commands; lazy sweep runs in `PersistentPreRunE` | See §9.6 |

---

## 10. Security Considerations

1. **TTL manipulation by unprivileged callers.** `SetSandboxTTL` validates the `runID` against the `sandbox_runs` table but does not check ownership (since TAG is single-user). In a future multi-user deployment, ownership must be enforced by adding an `owner_uid` column and comparing against `os.Getuid()`.

2. **Audit log integrity.** `sandbox-audit.jsonl` is written at `~/.tag/runtime/`, which is writable only by the owning user. On shared systems, the directory permissions should be `0700`. `writeAudit()` creates the directory with `os.MkdirAll(dir, 0o700)` and the log file with mode `0o600`; this should be verified in the doctor check.

3. **Container ID spoofing in kill.** The `container_id` stored in `sandbox_runs` is returned directly by the moby client `ContainerCreate` call over the local Docker socket — it never transits a temp file, removing the `--cidfile` race surface entirely. On a single-user system this presents no meaningful attack surface.

4. **`--ttl` range enforcement prevents resource exhaustion.** The 30–86400 second range is validated in `SetSandboxTTL` (returning `ErrInvalidTTL`) and echoed by the Cobra `Int64Var` flag + `RunE` range check. Values outside this range exit 2 with a clear error; there is no silent truncation.

5. **Background refresh goroutine and credential leakage.** The keepalive goroutine shares the process-wide `*sql.DB` handle and holds no credentials. The pool reads its DSN from the config-derived path (`modernc.org/sqlite`), not from an in-memory credential store. This is safe.

6. **Notification content.** TTL warning notifications include the sandbox ID and remaining seconds but never the command string, output, or any data that might contain secrets. This prevents credential leakage via Slack/webhook notification bodies.

7. **`--detach` and orphan risk.** Detached sandboxes that exceed their TTL are terminated by the sweep. However, if TAG is never invoked again (no `sandbox` command) and no long-lived process hosts the reaper goroutine, the lazy sweep never fires. Users who rely on deterministic cleanup for billing reasons should use the `go-co-op/gocron` scheduler integration (v1.1) or call `tag sandbox list` periodically.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`internal/sandbox/ttl_test.go`)

Tests run against a real `modernc.org/sqlite` DB opened at `file::memory:?cache=shared` (or a `t.TempDir()` file) and override the package `utcNow` seam to simulate time advancement without `time.Sleep`. A `t.Cleanup` restores the real clock. Idiomatic Go: table-driven subtests via `t.Run`, `errors.Is` on sentinels, and `testing.T` helpers.

```go
// internal/sandbox/ttl_test.go
package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

// withClock swaps utcNow for a controllable fake and restores it on cleanup.
func withClock(t *testing.T, start time.Time) *time.Time {
	t.Helper()
	cur := start
	orig := utcNow
	utcNow = func() time.Time { return cur }
	t.Cleanup(func() { utcNow = orig })
	return &cur
}

func TestRefreshSandbox_ResetsLastActivity(t *testing.T) {} // updates last_activity_at, clears warned_at
func TestRefreshSandbox_RejectsNonRunning(t *testing.T)    {} // errors.Is(err, ErrNotRunning) for done/failed/expired
func TestRefreshSandbox_RejectsEphemeral(t *testing.T)     {} // errors.Is(err, ErrEphemeral) when ttl_s IS NULL

func TestSetSandboxTTL_UpdatesAndResetsActivity(t *testing.T) {} // ttl_s updated, last_activity_at reset
func TestSetSandboxTTL_ValidatesRange(t *testing.T)          {} // errors.Is(err, ErrInvalidTTL) for <30 or >86400
func TestSetSandboxTTL_ValidatesWarnLessThanTTL(t *testing.T) {} // ErrInvalidTTL when warn >= ttl
func TestSetSandboxTTL_TriggersImmediateExpiry(t *testing.T)  {} // TTL shorter than idle → state expired

func TestSweepExpiredSandboxes_MarksExpired(t *testing.T)   {} // overdue running rows → expired
func TestSweepExpiredSandboxes_IgnoresEphemeral(t *testing.T) {} // rows with ttl_s IS NULL untouched

func TestSweepWarnings_FiresOnce(t *testing.T)         {} // warned_at set on first warn, skipped on second sweep
func TestSweepWarnings_RearmedAfterRefresh(t *testing.T) {} // RefreshSandbox clears warned_at; next sweep re-fires

func TestTTLRemaining_Computation(t *testing.T)  {} // (int64, bool) correct value, clamps to 0
func TestFormatRemaining_HumanReadable(t *testing.T) {} // "Xh Ym Zs" / "Xm Ys" / "Xs"

func TestWriteAudit_AllEventTypes(t *testing.T)   {} // each op appends the correct event to the JSONL log
func TestEnsureSchema_Idempotent(t *testing.T)    {} // migration on existing sandbox_runs preserves rows
func TestBackwardCompat_EphemeralRun(t *testing.T) {} // RunInSandbox w/o --ttl writes ttl_s=NULL, unaffected by sweep
```

### 11.2 Integration Tests

CLI end-to-end tests build the binary once (`go test` + a `TestMain` that `go build`s `cmd/tag`, or exercise the root Cobra command in-process via `cmd.SetArgs(...)` + `cmd.Execute()`), then assert against DB state.

- **`TestCLI_SandboxRefresh`**: Invoke `tag sandbox run --ttl 300 --backend restricted --code "echo hi"`, capture the sandbox ID from output, then call `tag sandbox refresh <id>`, verify `last_activity_at` updated in DB.
- **`TestCLI_SandboxSetTTL`**: Create sandbox with `--ttl 300`, call `tag sandbox set-ttl <id> --ttl 600`, verify DB row has `ttl_s=600`.
- **`TestCLI_SandboxListJSONTTLFields`**: Create sandbox with `--ttl 300`, call `tag sandbox list --json`, `json.Unmarshal` and assert `ttl_remaining_s`, `ttl_s`, `last_activity_at`, `expires_at` are present.
- **`TestCLI_SandboxSetTTLImmediateExpiry`**: Advance the fake clock 60 s past a 30 s TTL, call `tag sandbox set-ttl <id> --ttl 10`, verify the sandbox ends in state `expired`.
- **`TestKeepaliveGoroutine`**: Verify that `StartRefresher` updates `last_activity_at` at the configured interval and that the returned `stop()` func blocks until the goroutine exits (no leak; assert with `goleak` or a context-deadline check).

### 11.3 Performance / Concurrency Tests

- **Sweep performance** (`BenchmarkSweep` or a timed test): Populate 1000 rows with `state='running'` and `ttl_s` set. Call `SweepExpiredSandboxes()`. Assert wall time < 50 ms on SQLite WAL mode.
- **Concurrent refresh safety**: Launch 10 goroutines (coordinated via `sync.WaitGroup`) each calling `RefreshSandbox()` on the same sandbox ID; run under `go test -race`. Assert no `SQLITE_BUSY`/locking error — the single-writer WAL pool serializes writers gracefully.

---

## 12. Acceptance Criteria

| ID | Criteria | Verified By |
|----|----------|-------------|
| AC-01 | `tag sandbox run --ttl 300 --backend restricted --code "echo hi"` exits 0 and stores `ttl_s=300` and `last_activity_at IS NOT NULL` in `sandbox_runs`. | `TestCLI_SandboxTTLStored` |
| AC-02 | `tag sandbox run` without `--ttl` stores `ttl_s=NULL` and is not affected by subsequent sweep calls. | `TestBackwardCompat_EphemeralRun` |
| AC-03 | `tag sandbox list --json` returns objects with `ttl_s`, `ttl_remaining_s`, `last_activity_at`, and `expires_at` fields for TTL-enabled sandboxes. | `TestCLI_SandboxListJSONTTLFields` |
| AC-04 | `tag sandbox refresh <id>` on a running sandbox exits 0 and updates `last_activity_at` to within 1 s of UTC now. | `TestCLI_SandboxRefresh` |
| AC-05 | `tag sandbox refresh <id>` on a non-running sandbox (state=`done`) exits 1 with a human-readable error (`errors.Is(err, ErrNotRunning)`). | `TestRefreshSandbox_RejectsNonRunning` |
| AC-06 | `tag sandbox set-ttl <id> --ttl 600` exits 0, updates `ttl_s=600`, resets `last_activity_at`, and clears `warned_at`. | `TestCLI_SandboxSetTTL` |
| AC-07 | `tag sandbox set-ttl <id> --ttl 10` when the sandbox has been idle for 45 s exits 0, immediately calls `terminateSandboxBackend()`, and sets `state='expired'`. | `TestCLI_SandboxSetTTLImmediateExpiry` |
| AC-08 | `tag sandbox set-ttl <id> --ttl 29` exits 2 (`errors.Is(err, ErrInvalidTTL)`) with error message containing "between 30 and 86400". | `TestSetSandboxTTL_ValidatesRange` |
| AC-09 | `SweepExpiredSandboxes()` transitions all rows where `idle_s > ttl_s` to state `expired` and writes `expired` events to `sandbox-audit.jsonl`. | `TestSweepExpiredSandboxes_MarksExpired` |
| AC-10 | A warning is emitted to stderr and to `notifications.Send()` when `ttl_remaining_s <= ttl_warn_s`, and `warned_at` is set to prevent duplicate warnings. | `TestSweepWarnings_FiresOnce` |
| AC-11 | After `sandbox refresh`, `warned_at` is cleared and the next sweep can re-fire the warning if the new idle time enters the warn window. | `TestSweepWarnings_RearmedAfterRefresh` |
| AC-12 | `EnsureSchema()` on a DB with pre-existing `sandbox_runs` rows (no TTL columns) adds all four new columns without data loss. | `TestEnsureSchema_Idempotent` |
| AC-13 | `sandbox-audit.jsonl` contains one entry per event: `created`, `refreshed`, `set-ttl`, `expired`, `warned`, with correct `sandbox_id` and `ttl_s` fields. | `TestWriteAudit_AllEventTypes` |
| AC-14 | `tag sandbox list` human-readable table shows `REMAINING` column formatted as `Xm Ys` (not raw seconds) for TTL-enabled sandboxes. | Manual / `TestFormatRemaining_HumanReadable` |
| AC-15 | The keepalive goroutine stops cleanly (returned `stop()` returns, no leaked goroutine) after `context` cancellation. | `TestKeepaliveGoroutine` |

---

## 13. Dependencies

| Dependency | Type | Version / Notes |
|------------|------|-----------------|
| PRD-028 (Sandbox Code Execution) | Blocking prerequisite | `internal/sandbox` package and `sandbox_runs` table must exist before TTL columns can be added |
| PRD-013 (Agent Tracing) | Soft dependency | Audit log pattern follows tracing conventions; the `utcNow()` seam from `internal/sandbox` is reused |
| PRD-034 (Security Hardening) | Informational | Container ID capture and `ContainerKill` must comply with PRD-034 blocked-path validation |
| PRD-040 (Notification Hooks) | Soft dependency | `notifications.Send()` is called for pre-expiry warnings; if it errors, warnings degrade to stderr-only |
| PRD-012 (Cost Tracking) | Future | Sandbox session duration will feed cost attribution in a follow-on PRD; no blocking dependency |
| `modernc.org/sqlite` | Go module (existing) | Pure-Go SQLite driver (CGO_ENABLED=0); backs `sandbox_runs`; WAL enabled by `OpenDB()` |
| `github.com/docker/docker` (moby client) | Go module (existing) | Docker backend TTL enforcement via `ContainerCreate`/`ContainerKill`; feature-detected, degrades gracefully if the daemon is absent |
| `github.com/spf13/cobra` | Go module (existing) | `tag sandbox refresh` / `set-ttl` commands and flag parsing |
| E2B / Modal REST API | Optional runtime | Cloud backend TTL enforcement via `net/http` DELETE against the provider API; enforcement is advisory (see NG5) |
| `github.com/go-co-op/gocron/v2` | Go module (v1.1) | Advisory 30 s sweep job; not required for v1 (reaper goroutine is stdlib `time.Ticker`) |
| `time` / `context` / `sync` | stdlib | Reaper + keepalive goroutines, TTL deadlines, clean shutdown; no new dependency |
| SQLite WAL mode | Already enabled | `OpenDB()` enables WAL (single-writer); no change required |

---

## 14. Open Questions

| # | Question | Owner | Resolution Target |
|---|----------|-------|-------------------|
| OQ-1 | Should `--ttl` reset on every sandbox command invocation that produces output (auto-activity tracking), or only on explicit `sandbox refresh`? Auto-tracking would require hooking `RunInSandbox()` to update `last_activity_at` after every call; explicit is simpler but requires callers to opt in. | Sandbox team | Before implementation start |
| OQ-2 | E2B/Modal REST reconnect pattern: is the provider sandbox ID the same as the TAG `runID` or does it need to be stored separately in `container_id`? Clarify the mapping in `getSession()`. | Backend integration | Sprint 1 |
| OQ-3 | Should `tag sandbox list` compute `ttl_remaining_s` at the SQL layer (using `strftime` arithmetic) or in Go post-scan (via `SandboxSession.TTLRemaining()`)? SQL is more efficient for large lists; Go is simpler to test with the fake clock. | Implementation | Sprint 1 |
| OQ-4 | What is the right behavior when `--detach` is used without `--ttl`? Options: (a) error, (b) require `--ttl`, (c) default TTL of 3600 s. Current spec says error. | CLI design | Before implementation start |
| OQ-5 | Should the `go-co-op/gocron` scheduler integration (FR-18) be deferred to v1.1 or included in the initial implementation? The reaper goroutine + lazy sweep cover all interactive use cases; the gocron version is only needed for unattended, long-idle operation. | Team | Sprint 1 planning |
| OQ-6 | Pre-expiry warning: should the warning be rate-limited to fire at most once per warn window (current design) or fire on every sweep cycle while in the warn window? Current design fires once and requires a refresh to re-arm; continuous warnings would be more visible but noisier. | UX | Before implementation start |
| OQ-7 | How should TTL interact with `tag sandbox run --timeout`? Current design: `--timeout` is the per-invocation wall-clock limit for the child process (a `context.WithTimeout` deadline); `--ttl` is the idle-session TTL. For detached sessions, `--timeout` has no meaning. Clarify in CLI help text. | Docs | Sprint 1 |
| OQ-8 | Should `sandbox-audit.jsonl` rotate? Large installs with many short-TTL sandboxes could produce a large JSONL file. Consider a max-size or daily-rotation policy. | Ops | v1.1 |

---

## 15. Complexity and Timeline

**Overall Estimate:** S — 3–5 days for one engineer familiar with `internal/sandbox` and the `cmd/tag` Cobra command tree.

### Phase 1 — Schema & Core Logic (Day 1–2)

| Task | Effort |
|------|--------|
| Add TTL columns to `EnsureSchema()` with `PRAGMA table_info` migration guard | 1 h |
| Implement `SandboxSession` and `TTLEvent` structs (+ `MarshalJSON` shim) | 1 h |
| Implement `SweepExpiredSandboxes()` and `sweepWarnings()` | 2 h |
| Implement `RefreshSandbox()` and `SetSandboxTTL()` with sentinel errors | 2 h |
| Implement `terminateSandboxBackend()` with moby `ContainerCreate`/`ContainerKill` capture | 1.5 h |
| Implement `writeAudit()` and audit log integration | 1 h |
| Unit tests for all core functions (fake clock seam) | 3 h |

### Phase 2 — CLI Surface (Day 3)

| Task | Effort |
|------|--------|
| Add `--ttl`, `--ttl-warn`, `--refresh-interval`, `--detach` flags to `sandbox run` (Cobra) | 1 h |
| Add `refresh` Cobra command + `RunE` handler | 1 h |
| Add `set-ttl` Cobra command + `RunE` handler + exit-code mapping | 1 h |
| Extend `sandbox list` with TTL columns (table + JSON) | 1.5 h |
| Add lazy sweep to the `sandbox` group `PersistentPreRunE` | 0.5 h |
| Implement `StartRefresher` keepalive goroutine for `--refresh-interval` | 1.5 h |

### Phase 3 — Integration & Validation (Day 4–5)

| Task | Effort |
|------|--------|
| Integration tests (CLI end-to-end, DB state verification; `go test -race`) | 3 h |
| Performance test (1000-row sweep < 50 ms; `go test -bench`) | 1 h |
| Manual test against real Docker backend (moby `ContainerCreate` ID capture, `ContainerKill`) | 1.5 h |
| Update `sandbox-audit.jsonl` format in `CHANGELOG` / package doc comment | 0.5 h |
| Code review and iteration | 2 h |
| Verify backward compatibility with existing `tag sandbox run` tests | 1 h |

### Milestone Summary

| Milestone | Target Day |
|-----------|------------|
| Core logic + unit tests passing | Day 2 EOD |
| CLI surface wired + all new subcommands functional | Day 3 EOD |
| Integration tests passing + Docker backend validated | Day 4 EOD |
| PR open, review feedback incorporated | Day 5 EOD |

