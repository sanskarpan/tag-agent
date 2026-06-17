# PRD-100: Auto-Stop/Auto-Archive Lifecycle Policies for Idle Sandboxes (`tag sandbox policy`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** S (3-5 days)
**Category:** Sandbox & Execution Environment
**Affects:** `sandbox.py`
**Depends on:** PRD-028 (Sandbox Code Execution), PRD-091 (Configurable Sandbox TTL + Session Refresh), PRD-022 (Cron Scheduled Agents), PRD-013 (Agent Tracing / Observability), PRD-034 (Secret Scanning / Security), PRD-012 (Cost Tracking / Budget), PRD-039 (Token Budget Enforcement), PRD-040 (Notification Hooks)
**Inspired by:** Daytona workspace policies, Gitpod idle timeout, GitHub Codespaces timeout

---

## 1. Overview

TAG's sandbox subsystem (PRD-028) currently provides ephemeral command execution across three backends — restricted subprocess, Docker, and Modal — with per-invocation wall-clock timeouts. PRD-091 extended this model by introducing persistent sandbox sessions with configurable TTLs and per-session keepalive refreshes. However, these TTL controls are still reactive and per-session: there is no mechanism to declare an operator-level policy that applies uniformly across all sandboxes within a profile, enforces hard resource caps (cost, concurrency, archival age), and runs enforcement autonomously in the background without user intervention.

Cloud sandbox providers have each converged on a tiered policy model above the raw TTL primitive. Daytona exposes `WorkspacePolicies` at the profile or organization level with `auto_stop_interval`, `pinned_image`, and `inactivity_lock_duration` fields, allowing administrators to declare rules once and have the platform enforce them fleet-wide. Gitpod's idle timeout applies to all workspaces in a team plan, with per-workspace overrides; the daemon monitors CPU/network quiescence and stops workspaces that breach the threshold. GitHub Codespaces separates `idle_timeout` (default 30 minutes) from `retention_period` (default 30 days to deletion) and enforces both independently. All three providers treat policy as a first-class configuration entity distinct from per-session state, and all three enforce policies via an asynchronous daemon rather than requiring user-triggered cleanup.

This PRD introduces `tag sandbox policy` — a lifecycle policy subsystem for TAG's sandbox module. A **SandboxPolicy** is a named, profile-scoped configuration document stored in SQLite that captures four enforcement axes: (1) auto-stop after N minutes of idle time (no new commands executed), (2) auto-archive after M days since creation, (3) a maximum number of concurrently running sandboxes per profile, and (4) a maximum USD cost incurred by sandboxes within a rolling 24-hour window. The existing `cron_scheduler.py` daemon is extended with a `sandbox_policy_sweep` hook that fires every minute and applies all active policies to all matching sandboxes in the appropriate state transitions. Notifications via `notifications.py` fire pre-stop warnings and post-archive confirmations, giving users the same experience as commercial providers.

The implementation is additive and backward-compatible. Sandboxes without a matching policy continue to behave exactly as before. When a policy is attached to a profile, it applies to all new sandbox sessions created under that profile starting from the moment of policy creation; existing sessions are brought into compliance on the next sweep cycle. The `tag sandbox policy apply --now` command triggers an immediate enforcement sweep for operators who need instant compliance without waiting for the next cron tick.

The feature has low implementation complexity (difficulty 2/5) but provides meaningful operational value for teams running cost-sensitive or resource-constrained sandbox workloads: it prevents runaway costs from forgotten idle sandboxes, prevents resource contention from sandbox proliferation, and provides the audit trail needed for capacity planning. The `tag sandbox policy show --json` command exposes the active policy in a machine-readable form suitable for CI assertions and compliance reporting.

---

## 2. Problem Statement

### 2.1 Idle Sandboxes Accumulate Unbounded Costs and Resources

When a TAG agent finishes a code execution task, the associated Docker container or cloud sandbox session (E2B, Modal) remains in a `running` state until explicitly killed via `tag sandbox kill <id>` or until its per-session TTL expires (if one was set via PRD-091). In practice, users rarely set per-session TTLs for interactive workflows, and they frequently forget to kill sandboxes after agent tasks complete. The result is a proliferation of idle running sandboxes that consume Docker host CPU and memory, accumulate cloud billing seconds, and eventually exhaust the host's container scheduling capacity.

For cloud backends, the cost impact is direct: a forgotten E2B sandbox on the Pro tier billed at approximately $0.001/second for 24 hours costs $86.40 per sandbox per day. A developer running 10 agent tasks in parallel without cleanup can accumulate $864/day in idle sandbox charges. TAG currently provides no mechanism to detect or prevent this: `tag sandbox list` shows running sandboxes but provides no cost-per-hour estimate and no automatic action. The operator must manually identify and kill idle sessions.

### 2.2 No Fleet-Level Policy Means Per-Session Configuration Does Not Scale

PRD-091 introduced per-session TTL configuration via `tag sandbox run --ttl <seconds>`. This is useful for scripted invocations where the caller knows the expected lifetime in advance, but it does not address two important production scenarios: (a) agent-initiated sandbox creation where the calling code does not know the appropriate TTL for the workload, and (b) team or organization-level policies where an administrator wants to enforce hard limits on all sandboxes regardless of how they were created.

Daytona's and Gitpod's operational experience demonstrates that per-session configuration is insufficient for fleet management. Both platforms added profile-level and organization-level policy objects specifically because per-session TTLs create configuration sprawl and compliance gaps. TAG is at the same inflection point: as the number of concurrent sandbox sessions grows, per-session management becomes intractable and a declarative policy layer is required.

### 2.3 No Cost Cap Prevents Budget Overruns

TAG's budget module (PRD-039) enforces token budget limits for agent inference but has no visibility into sandbox execution costs. A cloud sandbox that idles for 8 hours can cost more than the inference tokens for the entire agent session that spawned it, yet PRD-039 would report zero cost overrun for that run because it does not track sandbox billing. There is no daily or rolling cost cap on sandbox spending per profile, no warning when sandbox costs approach a configured limit, and no automatic action (stop/archive) when the cap is reached.

GitHub Codespaces addresses this with per-user spending limits that automatically stop new Codespace creation when the monthly budget is exhausted. TAG needs the equivalent at the daily granularity: a `max_cost_daily_usd` cap that triggers sandbox stop actions when the rolling 24-hour sandbox spend for a given profile reaches the configured threshold.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | A single `tag sandbox policy set` command creates or replaces a policy for the current or named profile, accepting `--idle-timeout`, `--archive-after`, `--max-concurrent`, and `--max-cost-daily` as independent, composable constraints. |
| G2 | The policy daemon sweep runs every 60 seconds via `cron_scheduler.py` and autonomously stops sandboxes that have been idle beyond `idle_timeout_minutes` and archives (marks as `archived`) those that have exceeded `archive_after_days`. |
| G3 | `tag sandbox policy apply --now` triggers an immediate synchronous enforcement sweep for the active profile, reporting each action taken. |
| G4 | `tag sandbox policy show` and `tag sandbox policy show --json` display the effective policy for the active profile, including computed enforcement state (next sweep time, sandboxes currently in scope, projected cost impact). |
| G5 | When a policy's `max_concurrent` limit is reached, new `tag sandbox run` calls under that profile fail fast with a clear error message before allocating any backend resources. |
| G6 | When the rolling 24-hour sandbox cost for a profile reaches `max_cost_daily_usd`, the policy daemon stops the most-idle running sandboxes until the projected cost drops below the cap; a notification is fired via `notifications.py`. |
| G7 | A pre-stop warning notification is fired via `notifications.py` at least 60 seconds before an idle-timeout stop is executed, giving an interactive user the chance to send a keepalive. |
| G8 | All policy actions (stop, archive, warning) are recorded in a `sandbox_policy_events` SQLite table with a foreign key to `sandbox_runs`, enabling audit trails and `tag sandbox policy history` reporting. |
| G9 | Policies are profile-scoped: setting a policy on profile `coder` does not affect sandboxes created under profile `researcher`. A `--profile` flag on all policy subcommands allows cross-profile management. |
| G10 | Zero behavioral change for sandboxes created under profiles with no active policy. The sweep function is a no-op for those sessions. |

## 3.1 Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Organization-level or multi-user policy inheritance. Policies are per-profile in the local SQLite database; there is no server-side policy synchronization. |
| NG2 | Automatic cost retrieval from cloud provider billing APIs (E2B, Modal). Cost estimates use the stored `cost_usd` column in `sandbox_runs` populated by the execution backend, not live API queries. |
| NG3 | Policy enforcement across multiple TAG installations or remote agents. This PRD enforces policies only on the local TAG instance's sandboxes. |
| NG4 | Per-sandbox policy overrides. Policy is a profile-level construct; individual sandboxes cannot opt out of the profile policy. Per-session TTL (PRD-091) remains the mechanism for sandbox-specific lifetime control. |
| NG5 | Policy templates or inheritance hierarchies. There is one active policy per profile; there is no `extends` or `inherit` mechanism. |
| NG6 | Retroactive cost recovery. The max-cost-daily cap stops future sandbox creation and stops idle sandboxes; it does not issue refunds or modify cloud billing. |
| NG7 | Real-time resource metering inside sandboxes (CPU%, memory%). The idle signal is defined solely as time elapsed since the last command was submitted via `run_in_sandbox()`, not OS-level resource quiescence. |

---

## 4. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Idle cost reduction | Sandboxes stopped within `idle_timeout + 60s` of going idle | Integration test: create sandbox, wait `idle_timeout + 70s`, assert `status = stopped` |
| Sweep latency | p99 sweep execution time < 200 ms for up to 1 000 sandbox rows | Benchmark test: populate 1 000 `sandbox_runs` rows, time `policy_sweep()` call |
| Concurrency gate | `tag sandbox run` rejects with exit code 2 within 50 ms when `max_concurrent` is reached | Unit test: mock 5 running sandboxes, assert rejection before Docker call |
| Cost cap accuracy | Daily cost cap triggers stop action within one sweep cycle (≤ 60 s) of breach | Integration test: inject synthetic `cost_usd` rows summing to `> max_cost_daily_usd`, run sweep, assert stop actions |
| Notification timing | Pre-stop warning fires between 60 s and 120 s before actual stop action | Unit test: advance mock clock to `idle_timeout - 90s`, assert warning notification emitted |
| Policy persistence | Policy survives TAG process restart and re-read from SQLite | Integration test: set policy, close DB connection, reopen, assert policy row present |
| Zero-policy overhead | `run_in_sandbox()` latency with no active policy ≤ 1 ms increase over baseline | Benchmark: 100 calls, measure p99 latency delta |
| Audit trail completeness | Every stop and archive action has a corresponding row in `sandbox_policy_events` | Assert via `SELECT COUNT(*)` after integration test sweep |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag sandbox policy set --idle-timeout 30m` for my `coder` profile | Forgotten sandboxes from agent debugging sessions stop automatically instead of billing me overnight |
| U2 | Platform engineer | run `tag sandbox policy set --max-cost-daily 5.00` for a cost-sensitive profile | I get automatic enforcement of a daily sandbox budget without monitoring the dashboard manually |
| U3 | Team lead | run `tag sandbox policy set --max-concurrent 3 --profile research` | Parallel agent swarms spawned by the research profile cannot exhaust the host Docker daemon or cloud sandbox quota |
| U4 | Compliance officer | run `tag sandbox policy history --profile coder` | I can produce an audit report showing every sandbox stop and archive action taken in the last 30 days for security review |
| U5 | Developer | run `tag sandbox policy apply --now` after setting a new policy | I can immediately bring all running sandboxes into compliance without waiting for the next cron tick |
| U6 | DevOps engineer | run `tag sandbox policy show --json` in a CI assertion | I can verify the production policy configuration in a reproducible, machine-parsable way as part of infrastructure-as-code testing |
| U7 | Developer | receive a push notification 60 seconds before my sandbox is auto-stopped | I can send a keepalive (`tag sandbox refresh <id>`) if I am still actively using it |
| U8 | Developer | set `--archive-after 7d` for a profile | Stopped sandboxes older than 7 days are automatically archived and their Docker images pruned, reclaiming disk space |
| U9 | Developer | run `tag sandbox policy unset` | I can remove a policy and return the profile to unlimited sandbox behavior |
| U10 | Operator | run `tag sandbox policy list` | I can see at a glance which profiles have active policies and what their configured constraints are |

---

## 6. Proposed CLI Surface

### 6.1 `tag sandbox policy set`

Sets (creates or replaces) the lifecycle policy for the current profile. All flags are optional; omitted flags retain their previous values if a policy already exists, or are left unconstrained (null) for a new policy.

```
tag sandbox policy set [OPTIONS]

Options:
  --idle-timeout DURATION   Stop sandbox after this much idle time.
                            Accepts: 30m, 1h, 2h30m, 90, 5400 (integer = seconds).
                            Set to 0 to disable idle-timeout enforcement.
  --archive-after DURATION  Archive stopped sandboxes older than this.
                            Accepts: 7d, 14d, 30d, 720h.
                            Set to 0 to disable auto-archive.
  --max-concurrent INT      Maximum sandboxes in 'running' state simultaneously.
                            New sandbox creation blocked when limit reached.
                            Set to 0 to disable concurrency limit.
  --max-cost-daily FLOAT    Maximum USD to spend on sandboxes in a rolling 24h window.
                            New sandboxes blocked and idle ones stopped when cap reached.
                            Set to 0 to disable cost cap.
  --profile TEXT            Target profile name [default: active profile from config]
  --json                    Print resulting policy as JSON on success

Examples:
  tag sandbox policy set --idle-timeout 30m --max-cost-daily 5.00
  tag sandbox policy set --idle-timeout 1h --archive-after 14d --max-concurrent 5
  tag sandbox policy set --max-concurrent 3 --profile research
  tag sandbox policy set --idle-timeout 0  # disable idle-timeout only
```

**Output (default):**
```
Policy updated for profile 'coder'
  idle_timeout:    30m
  archive_after:   14d
  max_concurrent:  5
  max_cost_daily:  $5.00
Next sweep: 2026-06-17T14:23:00Z (in 43s)
```

**Output (--json):**
```json
{
  "id": "pol_a3f92b",
  "profile": "coder",
  "idle_timeout_minutes": 30,
  "archive_after_days": 14,
  "max_concurrent": 5,
  "max_cost_daily_usd": 5.00,
  "enabled": true,
  "created_at": "2026-06-17T14:22:17Z",
  "updated_at": "2026-06-17T14:22:17Z"
}
```

### 6.2 `tag sandbox policy show`

Displays the effective policy for the active or named profile, including live enforcement state.

```
tag sandbox policy show [OPTIONS]

Options:
  --profile TEXT   Target profile name [default: active profile]
  --json           Output as JSON

Examples:
  tag sandbox policy show
  tag sandbox policy show --profile research --json
```

**Output (default):**
```
Sandbox Policy — profile: coder
─────────────────────────────────────────────────────────────
Idle timeout:       30m     (enforced)
Archive after:      14d     (enforced)
Max concurrent:     5       (enforced)
Max cost/day:       $5.00   (enforced)

Current State
  Running sandboxes:  3 / 5
  Stopped sandboxes:  2
  Archived sandboxes: 7
  Cost (last 24h):    $1.23 / $5.00

Next sweep:         2026-06-17T14:23:00Z (in 43s)
Last sweep:         2026-06-17T14:22:00Z (0 actions taken)
```

**Output (--json):**
```json
{
  "policy": {
    "id": "pol_a3f92b",
    "profile": "coder",
    "idle_timeout_minutes": 30,
    "archive_after_days": 14,
    "max_concurrent": 5,
    "max_cost_daily_usd": 5.00,
    "enabled": true,
    "created_at": "2026-06-17T14:22:17Z",
    "updated_at": "2026-06-17T14:22:17Z"
  },
  "state": {
    "running_count": 3,
    "stopped_count": 2,
    "archived_count": 7,
    "cost_last_24h_usd": 1.23,
    "next_sweep_at": "2026-06-17T14:23:00Z",
    "last_sweep_at": "2026-06-17T14:22:00Z",
    "last_sweep_actions": 0
  }
}
```

### 6.3 `tag sandbox policy apply --now`

Triggers an immediate synchronous enforcement sweep for the active profile, bypassing the cron schedule.

```
tag sandbox policy apply [OPTIONS]

Options:
  --now            Run sweep immediately (required flag — prevents accidental invocation)
  --profile TEXT   Target profile name [default: active profile]
  --dry-run        Show what actions would be taken without executing them
  --json           Output actions as JSON array

Examples:
  tag sandbox policy apply --now
  tag sandbox policy apply --now --dry-run
  tag sandbox policy apply --now --profile research --json
```

**Output (default):**
```
Running policy sweep for profile 'coder'...

Actions taken (3):
  [STOPPED]  sb_8a2d1f  idle for 47m (limit: 30m)  backend=docker
  [STOPPED]  sb_3c91aa  idle for 1h12m (limit: 30m)  backend=docker
  [ARCHIVED] sb_7f04bc  age 16d (limit: 14d)  backend=restricted

Sweep complete in 142ms.
```

**Output (--dry-run):**
```
DRY RUN — no actions will be executed

Would take (3 actions):
  [WOULD STOP]    sb_8a2d1f  idle for 47m (limit: 30m)  backend=docker
  [WOULD STOP]    sb_3c91aa  idle for 1h12m (limit: 30m)  backend=docker
  [WOULD ARCHIVE] sb_7f04bc  age 16d (limit: 14d)  backend=restricted
```

**Output (--json):**
```json
[
  {"action": "stopped",  "sandbox_id": "sb_8a2d1f", "reason": "idle_timeout", "idle_minutes": 47, "backend": "docker"},
  {"action": "stopped",  "sandbox_id": "sb_3c91aa", "reason": "idle_timeout", "idle_minutes": 72, "backend": "docker"},
  {"action": "archived", "sandbox_id": "sb_7f04bc", "reason": "archive_after", "age_days": 16,   "backend": "restricted"}
]
```

### 6.4 `tag sandbox policy unset`

Removes the policy for the named profile. Running sandboxes are not affected; they continue running until explicitly killed.

```
tag sandbox policy unset [OPTIONS]

Options:
  --profile TEXT   Target profile [default: active profile]
  --yes            Skip confirmation prompt

Example:
  tag sandbox policy unset --profile coder
```

### 6.5 `tag sandbox policy list`

Lists all profiles with active policies.

```
tag sandbox policy list [OPTIONS]

Options:
  --json   Output as JSON array

Example:
  tag sandbox policy list
```

**Output:**
```
PROFILE       IDLE     ARCHIVE   MAX-CONC  MAX-COST/DAY  ENABLED
coder         30m      14d       5         $5.00         yes
research      1h       —         3         —             yes
infra         —        30d       —         $20.00        yes
```

### 6.6 `tag sandbox policy history`

Shows a log of all policy enforcement actions for a profile.

```
tag sandbox policy history [OPTIONS]

Options:
  --profile TEXT    Target profile [default: active profile]
  --limit INT       Number of events to show [default: 50]
  --since TEXT      ISO timestamp or relative duration (e.g. "7d", "24h")
  --json            Output as JSON

Example:
  tag sandbox policy history --profile coder --since 7d
```

**Output:**
```
TIMESTAMP                  SANDBOX       ACTION    REASON           DETAIL
2026-06-17T13:45:02Z       sb_8a2d1f    stopped   idle_timeout     idle 47m / limit 30m
2026-06-17T13:44:59Z       sb_3c91aa    stopped   idle_timeout     idle 72m / limit 30m
2026-06-16T09:12:31Z       sb_7f04bc    archived  archive_after    age 16d / limit 14d
2026-06-15T22:01:14Z       sb_2b88ef    stopped   cost_cap         24h cost $5.08 / cap $5.00
2026-06-15T22:01:14Z       (policy)     warning   cost_cap         24h cost $4.92 (98% of $5.00)
```

---

## 7. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `sandbox_policies` SQLite table is created by `ensure_schema()` on first call; schema is idempotent (uses `CREATE TABLE IF NOT EXISTS`). | P0 |
| FR-02 | `sandbox_policy_events` SQLite table records every enforcement action (stop, archive, warning, concurrency-block, cost-block) with `sandbox_id`, `action`, `reason`, `detail`, and `timestamp`. | P0 |
| FR-03 | `tag sandbox policy set` creates a new policy row if none exists for the profile, or updates the existing row if one exists, preserving unspecified fields. | P0 |
| FR-04 | `tag sandbox policy set --idle-timeout 0` sets `idle_timeout_minutes = NULL`, disabling idle-timeout enforcement for that policy without removing other constraints. | P1 |
| FR-05 | Duration arguments accept integer seconds, `Nm` (minutes), `Nh` (hours), `Nd` (days), and compound forms `NhMm`. An invalid duration string causes a parse error before any DB write. | P1 |
| FR-06 | `policy_sweep(conn, profile)` selects all `sandbox_runs` rows where `profile = ?` and `status = 'running'`, computes idle duration as `NOW() - last_activity_at`, and stops any sandbox where `idle_duration > idle_timeout_minutes`. | P0 |
| FR-07 | `policy_sweep()` fires a pre-stop warning notification via `notifications.notify()` for any sandbox within 60 seconds of its idle-timeout threshold, if a warning has not already been fired for that sandbox in the current window. | P1 |
| FR-08 | `policy_sweep()` selects all `sandbox_runs` rows where `profile = ?` and `status IN ('stopped', 'done', 'failed')` and archives rows where `age_days > archive_after_days`. Archive sets `status = 'archived'`. | P0 |
| FR-09 | Before a new `run_in_sandbox()` call, the concurrency gate queries `COUNT(*) WHERE profile = ? AND status = 'running'`. If the count meets or exceeds `max_concurrent`, the function raises `PolicyViolationError` with code `concurrency_limit` before allocating any backend resource. | P0 |
| FR-10 | The cost gate queries `SUM(cost_usd) WHERE profile = ? AND created_at >= NOW() - INTERVAL 24h`. If the sum meets or exceeds `max_cost_daily_usd`, `run_in_sandbox()` raises `PolicyViolationError` with code `cost_cap`. | P0 |
| FR-11 | When the `max_cost_daily_usd` cap is breached mid-sweep (i.e., a running sandbox's accumulated cost causes the rolling sum to exceed the cap), `policy_sweep()` stops the most-idle running sandbox and fires a cost-cap notification. | P1 |
| FR-12 | `policy_sweep()` is registered as a named cron job `__sandbox_policy_sweep__` with schedule `* * * * *` (every minute) via `cron_scheduler.py` when a policy is first set. It is deregistered when all policies are removed. | P1 |
| FR-13 | `tag sandbox policy apply --now` calls `policy_sweep()` synchronously and prints each action. The `--dry-run` flag calls a read-only `policy_sweep_preview()` that returns actions without executing them. | P1 |
| FR-14 | `tag sandbox policy show --json` returns a JSON object with both the policy configuration and the live enforcement state (running count, stopped count, archived count, rolling 24h cost). | P1 |
| FR-15 | `tag sandbox policy list` returns all rows from `sandbox_policies` where `enabled = 1`, formatted as a table. | P2 |
| FR-16 | `tag sandbox policy history` queries `sandbox_policy_events` with profile filter and optional `since` timestamp filter, ordered by `timestamp DESC`. | P2 |
| FR-17 | `tag sandbox policy unset` deletes the policy row for the given profile and deregisters the cron sweep job if no other policies remain. It does NOT stop or archive running sandboxes. | P1 |
| FR-18 | The `sandbox_runs` table gains a `profile` column (TEXT, nullable, indexed) and a `cost_usd` column (REAL, nullable) via a `ALTER TABLE` migration executed by `ensure_schema()`. | P0 |
| FR-19 | The `sandbox_runs` table gains a `last_activity_at` column (TEXT, nullable) updated on every `run_in_sandbox()` call for an existing session. For legacy rows where `last_activity_at IS NULL`, idle-timeout computes from `created_at`. | P0 |
| FR-20 | All policy enforcement actions emit an OpenTelemetry span via `tracing.py` with attributes `sandbox.policy.action`, `sandbox.policy.reason`, and `sandbox.id`, enabling correlation with existing TAG traces. | P2 |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Sweep latency | `policy_sweep()` completes in < 200 ms for up to 1 000 sandbox rows at p99. Uses a single indexed `SELECT` for candidate selection, not N individual queries. |
| NFR-02 | Zero import cost | `import tag.sandbox` does not import `cron_scheduler` or `notifications` at module load time; these are imported lazily inside `policy_sweep()` and `_fire_warning()`. |
| NFR-03 | Atomicity | Each stop or archive action within a sweep is committed to SQLite in its own transaction. A failure on sandbox N does not roll back actions on sandboxes 1..N-1. |
| NFR-04 | Idempotency | Running `policy_sweep()` twice in rapid succession for the same profile produces the same final state without duplicate actions or events. Warning notifications are deduplicated using a `warned_at` column on `sandbox_runs`. |
| NFR-05 | Thread safety | `policy_sweep()` acquires a `threading.Lock` keyed on the profile name for the duration of the sweep to prevent concurrent sweeps from racing on the same profile's sandboxes. |
| NFR-06 | WAL-mode compatibility | All DB writes use `conn.execute()` with positional parameters; no use of SQLite `BEGIN EXCLUSIVE`. Compatible with the WAL-mode database at `~/.tag/runtime/tag.sqlite3`. |
| NFR-07 | Duration parse accuracy | Duration parser correctly handles: `"30m"` → 30, `"1h"` → 60, `"2h30m"` → 150, `"90"` → 1.5, `"7d"` → 10 080. Float seconds are rounded to nearest integer minute. |
| NFR-08 | Backward compatibility | Existing `sandbox_runs` rows without `profile`, `cost_usd`, `last_activity_at`, or `warned_at` columns are handled gracefully; `ensure_schema()` adds the columns with `ALTER TABLE ... ADD COLUMN` which is a no-op if already present. |
| NFR-09 | Error isolation | A stop action that fails (e.g., Docker container already removed) logs a warning and records a `stop_failed` event but does not abort the sweep. The sweep continues to the next candidate. |
| NFR-10 | Policy visibility | Every policy check that results in a block (concurrency, cost) prints a human-readable error to stderr: `PolicyViolationError: concurrency limit reached (3/3 sandboxes running for profile 'coder')`. |

---

## 9. Technical Design

### 9.1 New and Modified Files

| File | Change Type | Description |
|------|-------------|-------------|
| `src/tag/sandbox.py` | Modified | Primary implementation target. Adds `ensure_schema()` extension, `SandboxPolicy` dataclass, `PolicyViolationError`, `set_policy()`, `get_policy()`, `delete_policy()`, `list_policies()`, `policy_sweep()`, `policy_sweep_preview()`, `_parse_duration()`, `_get_enforcement_state()`, `_fire_warning()`, concurrency/cost gates in `run_in_sandbox()`. |
| `src/tag/controller.py` | Modified | Adds `cmd_sandbox_policy_set`, `cmd_sandbox_policy_show`, `cmd_sandbox_policy_apply`, `cmd_sandbox_policy_unset`, `cmd_sandbox_policy_list`, `cmd_sandbox_policy_history` command handlers and registers them under the `tag sandbox policy` subcommand group. |

### 9.2 SQLite DDL

The following DDL is added to `ensure_schema()` in `sandbox.py`. All `ALTER TABLE` statements use the `ADD COLUMN IF NOT EXISTS` pattern (SQLite 3.37+); for older SQLite, the migration catches `OperationalError` with message `duplicate column name` and ignores it.

```sql
-- sandbox_policies: one row per profile, declarative lifecycle policy
CREATE TABLE IF NOT EXISTS sandbox_policies (
    id                    TEXT PRIMARY KEY,       -- "pol_" + 6-char hex
    profile               TEXT NOT NULL UNIQUE,   -- tag profile name
    idle_timeout_minutes  INTEGER,                -- NULL = no idle-timeout enforcement
    archive_after_days    INTEGER,                -- NULL = no auto-archive
    max_concurrent        INTEGER,                -- NULL = no concurrency limit
    max_cost_daily_usd    REAL,                   -- NULL = no daily cost cap
    enabled               INTEGER NOT NULL DEFAULT 1,
    created_at            TEXT NOT NULL,          -- ISO 8601 UTC
    updated_at            TEXT NOT NULL           -- ISO 8601 UTC
);
CREATE INDEX IF NOT EXISTS idx_sp_profile ON sandbox_policies(profile);
CREATE INDEX IF NOT EXISTS idx_sp_enabled  ON sandbox_policies(enabled);

-- sandbox_policy_events: append-only audit log of enforcement actions
CREATE TABLE IF NOT EXISTS sandbox_policy_events (
    id          TEXT PRIMARY KEY,    -- "spe_" + 8-char hex
    policy_id   TEXT NOT NULL REFERENCES sandbox_policies(id),
    sandbox_id  TEXT,                -- NULL for policy-level events (e.g. cost warnings)
    action      TEXT NOT NULL,       -- stopped|archived|warning|concurrency_blocked|cost_blocked|stop_failed
    reason      TEXT NOT NULL,       -- idle_timeout|archive_after|cost_cap|max_concurrent
    detail      TEXT,                -- human-readable detail string
    timestamp   TEXT NOT NULL        -- ISO 8601 UTC
);
CREATE INDEX IF NOT EXISTS idx_spe_policy_id  ON sandbox_policy_events(policy_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_spe_sandbox_id ON sandbox_policy_events(sandbox_id);
CREATE INDEX IF NOT EXISTS idx_spe_timestamp  ON sandbox_policy_events(timestamp);

-- Extend sandbox_runs with lifecycle columns (migration-safe)
ALTER TABLE sandbox_runs ADD COLUMN profile          TEXT;
ALTER TABLE sandbox_runs ADD COLUMN cost_usd         REAL;
ALTER TABLE sandbox_runs ADD COLUMN last_activity_at TEXT;
ALTER TABLE sandbox_runs ADD COLUMN warned_at        TEXT;  -- ISO timestamp when pre-stop warning fired

CREATE INDEX IF NOT EXISTS idx_sr_profile_status
    ON sandbox_runs(profile, status, last_activity_at);
```

### 9.3 Core Python Dataclasses

```python
from __future__ import annotations
import dataclasses
import datetime
from typing import Optional


@dataclasses.dataclass
class SandboxPolicy:
    """Declarative lifecycle policy for sandboxes under a given profile."""
    id: str
    profile: str
    idle_timeout_minutes: Optional[int]   # None = disabled
    archive_after_days: Optional[int]     # None = disabled
    max_concurrent: Optional[int]         # None = disabled
    max_cost_daily_usd: Optional[float]   # None = disabled
    enabled: bool
    created_at: str
    updated_at: str

    def to_dict(self) -> dict:
        return dataclasses.asdict(self)

    @classmethod
    def from_row(cls, row: tuple) -> "SandboxPolicy":
        return cls(
            id=row[0], profile=row[1],
            idle_timeout_minutes=row[2], archive_after_days=row[3],
            max_concurrent=row[4], max_cost_daily_usd=row[5],
            enabled=bool(row[6]), created_at=row[7], updated_at=row[8],
        )


@dataclasses.dataclass
class PolicyEnforcementState:
    """Live enforcement state computed at query time."""
    policy: SandboxPolicy
    running_count: int
    stopped_count: int
    archived_count: int
    cost_last_24h_usd: float
    next_sweep_at: Optional[str]
    last_sweep_at: Optional[str]
    last_sweep_actions: int


@dataclasses.dataclass
class PolicyAction:
    """A single action taken (or to be taken) during a policy sweep."""
    action: str          # stopped|archived|warning|skipped
    sandbox_id: str
    reason: str          # idle_timeout|archive_after|cost_cap
    detail: str          # human-readable
    dry_run: bool = False


class PolicyViolationError(Exception):
    """Raised when a new sandbox creation is blocked by an active policy."""
    def __init__(self, code: str, message: str):
        self.code = code   # concurrency_limit|cost_cap
        super().__init__(message)
```

### 9.4 Duration Parser

```python
import re

_DURATION_RE = re.compile(
    r"^(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s?)?$",
    re.IGNORECASE,
)

def _parse_duration_minutes(s: str) -> Optional[int]:
    """
    Parse a duration string to integer minutes.

    Accepted forms:
      "0"          → 0 (disable)
      "30"         → 30 (bare integer = minutes)
      "30m"        → 30
      "1h"         → 60
      "2h30m"      → 150
      "7d"         → 10080
      "1d12h"      → 2160

    Returns None if s is empty or None.
    Raises ValueError on unrecognised format.
    """
    if not s:
        return None
    s = s.strip()
    if s == "0":
        return 0
    # bare integer without suffix = minutes
    if s.isdigit():
        return int(s)
    m = _DURATION_RE.match(s)
    if not m or not any(m.groups()):
        raise ValueError(f"Unrecognised duration: {s!r}. "
                         "Use formats like 30m, 1h, 2h30m, 7d.")
    days    = int(m.group(1) or 0)
    hours   = int(m.group(2) or 0)
    minutes = int(m.group(3) or 0)
    seconds = int(m.group(4) or 0)
    total = days * 1440 + hours * 60 + minutes + seconds // 60
    if total == 0:
        raise ValueError(f"Duration {s!r} resolves to 0 minutes; "
                         "use --idle-timeout 0 to disable enforcement.")
    return total
```

### 9.5 Policy Sweep Algorithm

```python
import threading
import sqlite3
import uuid

_sweep_locks: dict[str, threading.Lock] = {}

def policy_sweep(
    conn: sqlite3.Connection,
    profile: str,
    *,
    dry_run: bool = False,
) -> list[PolicyAction]:
    """
    Enforce the active policy for *profile* against all matching sandbox_runs rows.

    Algorithm:
      1. Load the SandboxPolicy for this profile; return [] if none or disabled.
      2. Acquire per-profile lock (prevents concurrent sweeps for same profile).
      3. IDLE-TIMEOUT: SELECT running sandboxes ordered by last_activity_at ASC.
         For each: if idle_minutes >= idle_timeout_minutes → stop it.
                   if idle_minutes >= idle_timeout_minutes - 1 and warned_at IS NULL
                      → fire warning notification, set warned_at.
      4. ARCHIVE: SELECT stopped/done/failed sandboxes older than archive_after_days.
         For each: set status = 'archived'.
      5. COST CAP: Compute rolling 24h cost sum. For each running sandbox, if
         cost_sum >= max_cost_daily_usd, stop the most-idle running sandbox
         and fire a cost-cap notification.
      6. Record each action in sandbox_policy_events.
      7. Return list[PolicyAction].

    Stopping a sandbox:
      - status = 'stopped', completed_at = NOW()  (DB update, always)
      - If backend == 'docker': subprocess.run(['docker', 'stop', container_id],
        timeout=10, capture_output=True). Failure logged but does not abort sweep.
      - If backend == 'modal': imported lazily, modal.Sandbox.from_id(id).terminate()
      - If backend == 'restricted': no external action needed (subprocess already done).
    """
    lock = _sweep_locks.setdefault(profile, threading.Lock())
    if not lock.acquire(blocking=False):
        return []   # sweep already in progress for this profile

    try:
        return _sweep_inner(conn, profile, dry_run=dry_run)
    finally:
        lock.release()


def _sweep_inner(
    conn: sqlite3.Connection,
    profile: str,
    *,
    dry_run: bool,
) -> list[PolicyAction]:
    policy = get_policy(conn, profile)
    if policy is None or not policy.enabled:
        return []

    now = datetime.datetime.now(datetime.timezone.utc)
    actions: list[PolicyAction] = []

    # ── 1. Idle-timeout enforcement ─────────────────────────────────────────
    if policy.idle_timeout_minutes is not None:
        candidates = conn.execute(
            """SELECT id, backend, last_activity_at, created_at, warned_at
               FROM sandbox_runs
               WHERE profile = ? AND status = 'running'
               ORDER BY COALESCE(last_activity_at, created_at) ASC""",
            (profile,),
        ).fetchall()

        for row in candidates:
            sb_id, backend, last_act, created_at, warned_at = row
            ref_ts = last_act or created_at
            ref_dt = datetime.datetime.fromisoformat(ref_ts.replace("Z", "+00:00"))
            idle_minutes = (now - ref_dt).total_seconds() / 60

            warn_threshold = policy.idle_timeout_minutes - 1  # 1 minute before stop
            if idle_minutes >= warn_threshold and warned_at is None:
                _fire_warning(conn, policy, sb_id, idle_minutes,
                              policy.idle_timeout_minutes, dry_run=dry_run)
                actions.append(PolicyAction(
                    action="warning", sandbox_id=sb_id,
                    reason="idle_timeout",
                    detail=f"idle {idle_minutes:.1f}m / limit {policy.idle_timeout_minutes}m",
                    dry_run=dry_run,
                ))

            if idle_minutes >= policy.idle_timeout_minutes:
                _stop_sandbox(conn, policy, sb_id, backend,
                              reason="idle_timeout",
                              detail=f"idle {idle_minutes:.1f}m / limit {policy.idle_timeout_minutes}m",
                              dry_run=dry_run)
                actions.append(PolicyAction(
                    action="stopped", sandbox_id=sb_id,
                    reason="idle_timeout",
                    detail=f"idle {idle_minutes:.1f}m",
                    dry_run=dry_run,
                ))

    # ── 2. Archive enforcement ───────────────────────────────────────────────
    if policy.archive_after_days is not None:
        cutoff = (now - datetime.timedelta(days=policy.archive_after_days)).isoformat()
        archive_candidates = conn.execute(
            """SELECT id FROM sandbox_runs
               WHERE profile = ?
                 AND status IN ('stopped', 'done', 'failed')
                 AND created_at < ?""",
            (profile, cutoff),
        ).fetchall()

        for (sb_id,) in archive_candidates:
            age_days = (now - datetime.datetime.fromisoformat(
                conn.execute("SELECT created_at FROM sandbox_runs WHERE id=?",
                             (sb_id,)).fetchone()[0].replace("Z", "+00:00")
            )).days
            _archive_sandbox(conn, policy, sb_id,
                             detail=f"age {age_days}d / limit {policy.archive_after_days}d",
                             dry_run=dry_run)
            actions.append(PolicyAction(
                action="archived", sandbox_id=sb_id,
                reason="archive_after",
                detail=f"age {age_days}d",
                dry_run=dry_run,
            ))

    # ── 3. Cost-cap mid-sweep enforcement ───────────────────────────────────
    if policy.max_cost_daily_usd is not None:
        window_start = (now - datetime.timedelta(hours=24)).isoformat()
        row = conn.execute(
            """SELECT COALESCE(SUM(cost_usd), 0.0)
               FROM sandbox_runs
               WHERE profile = ? AND created_at >= ?""",
            (profile, window_start),
        ).fetchone()
        rolling_cost = row[0] if row else 0.0

        if rolling_cost >= policy.max_cost_daily_usd:
            # Stop the most-idle running sandbox to reduce projected cost
            idle_sb = conn.execute(
                """SELECT id, backend FROM sandbox_runs
                   WHERE profile = ? AND status = 'running'
                   ORDER BY COALESCE(last_activity_at, created_at) ASC
                   LIMIT 1""",
                (profile,),
            ).fetchone()
            if idle_sb:
                sb_id, backend = idle_sb
                _stop_sandbox(conn, policy, sb_id, backend,
                              reason="cost_cap",
                              detail=f"24h cost ${rolling_cost:.2f} / cap ${policy.max_cost_daily_usd:.2f}",
                              dry_run=dry_run)
                actions.append(PolicyAction(
                    action="stopped", sandbox_id=sb_id,
                    reason="cost_cap",
                    detail=f"24h cost ${rolling_cost:.2f}",
                    dry_run=dry_run,
                ))

    return actions
```

### 9.6 Concurrency and Cost Gate in `run_in_sandbox()`

```python
def _check_policy_gates(conn: sqlite3.Connection, profile: Optional[str]) -> None:
    """
    Called at the top of run_in_sandbox() before any backend allocation.
    Raises PolicyViolationError if the active policy blocks the new sandbox.
    No-op if profile is None or no policy exists for the profile.
    """
    if not profile:
        return
    policy = get_policy(conn, profile)
    if policy is None or not policy.enabled:
        return

    # Concurrency gate
    if policy.max_concurrent is not None:
        count = conn.execute(
            "SELECT COUNT(*) FROM sandbox_runs WHERE profile = ? AND status = 'running'",
            (profile,),
        ).fetchone()[0]
        if count >= policy.max_concurrent:
            raise PolicyViolationError(
                "concurrency_limit",
                f"concurrency limit reached ({count}/{policy.max_concurrent} sandboxes "
                f"running for profile {profile!r}). "
                "Run `tag sandbox kill <id>` or wait for idle-timeout enforcement.",
            )

    # Cost gate
    if policy.max_cost_daily_usd is not None:
        window_start = (
            datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(hours=24)
        ).isoformat()
        row = conn.execute(
            "SELECT COALESCE(SUM(cost_usd), 0.0) FROM sandbox_runs "
            "WHERE profile = ? AND created_at >= ?",
            (profile, window_start),
        ).fetchone()
        rolling = row[0] if row else 0.0
        if rolling >= policy.max_cost_daily_usd:
            raise PolicyViolationError(
                "cost_cap",
                f"daily cost cap reached (${rolling:.2f}/${policy.max_cost_daily_usd:.2f} "
                f"in last 24h for profile {profile!r}). "
                "Use `tag sandbox policy show` to review spend.",
            )
```

### 9.7 Cron Integration

The sweep is registered as a one-minute cron job using the existing `cron_scheduler.py` infrastructure when the first policy is created for any profile. The job is removed when the last policy is deleted.

```python
_SWEEP_JOB_NAME = "__sandbox_policy_sweep__"
_SWEEP_CRON_EXPR = "* * * * *"   # every minute

def _register_sweep_cron(conn: sqlite3.Connection) -> None:
    """Ensure the policy sweep cron job exists in the scheduler table."""
    from tag.cron_scheduler import ensure_cron_schema
    ensure_cron_schema(conn)
    existing = conn.execute(
        "SELECT id FROM cron_jobs WHERE name = ?", (_SWEEP_JOB_NAME,)
    ).fetchone()
    if not existing:
        conn.execute(
            """INSERT INTO cron_jobs(id, name, schedule, command, enabled, created_at)
               VALUES(?, ?, ?, ?, 1, ?)""",
            (
                "cj_" + uuid.uuid4().hex[:8],
                _SWEEP_JOB_NAME,
                _SWEEP_CRON_EXPR,
                "__internal__:sandbox_policy_sweep",
                _utc_now(),
            ),
        )
        conn.commit()

def _deregister_sweep_cron_if_no_policies(conn: sqlite3.Connection) -> None:
    count = conn.execute(
        "SELECT COUNT(*) FROM sandbox_policies WHERE enabled = 1"
    ).fetchone()[0]
    if count == 0:
        conn.execute(
            "DELETE FROM cron_jobs WHERE name = ?", (_SWEEP_JOB_NAME,)
        )
        conn.commit()
```

The `controller.py` cron dispatcher recognises the sentinel command `__internal__:sandbox_policy_sweep` and calls `policy_sweep_all_profiles(conn)` which iterates over all active policy profiles and calls `policy_sweep(conn, profile)` for each.

### 9.8 Integration Points

| Integration | Mechanism |
|-------------|-----------|
| `cron_scheduler.py` | `cron_jobs` table row with `name = '__sandbox_policy_sweep__'`; the dispatcher calls the sweep function |
| `notifications.py` | `notifications.notify(title, body, level)` called by `_fire_warning()` and cost-cap events |
| `tracing.py` | Each sweep action is wrapped in `tracing.start_span("sandbox.policy.sweep")` with relevant attributes |
| `budget.py` | Cost gate reads `cost_usd` from `sandbox_runs`; budget.py token budgets remain independent |
| `controller.py` | Six new command handlers registered under `tag sandbox policy` subcommand group using existing Typer app patterns |

---

## 10. Security Considerations

1. **Policy manipulation requires local file access.** `sandbox_policies` rows are stored in `~/.tag/runtime/tag.sqlite3`, readable only by the local OS user. No remote API can modify policies; this is not a security surface in the threat model but should be documented for multi-user host deployments.

2. **Policy bypass via `--profile` spoofing is not possible.** The `profile` column is set by the caller at sandbox creation time; if a caller intentionally passes a different profile name to bypass concurrency limits, they are effectively self-misclassifying their sandbox and only bypass enforcement for themselves.

3. **Stop actions against Docker containers use `docker stop` (SIGTERM then SIGKILL).** This is the same signal sequence as manual `docker rm -f`. The container ID is read from the SQLite row; if the row's `container_id` no longer exists in Docker (e.g., manually removed), the stop command fails silently and the DB row is still updated to `stopped` status, preventing zombie rows.

4. **Pre-stop warning notifications must not disclose policy configuration to untrusted processes.** The notification body includes sandbox ID and idle duration but not the raw `max_cost_daily_usd` or other policy parameters, to prevent information leakage in shared notification channels.

5. **Cost values are stored as REAL (float) in SQLite.** Floating-point arithmetic for cost comparisons uses `>= cap - 0.001` (epsilon) to avoid false negatives from IEEE 754 rounding. This is documented explicitly in the sweep code.

6. **The sweep lock (`threading.Lock`) prevents TOCTOU races** between the concurrency gate check and the sandbox insertion. However, SQLite WAL mode does not provide cross-process mutual exclusion; if two TAG processes run simultaneously for the same profile, the concurrency gate may be bypassed. Operators running TAG in multi-process mode should be aware of this limitation.

7. **`policy_sweep_preview()` (dry-run) must not call `docker stop` or modify any SQLite row.** The dry-run path is enforced by passing `dry_run=True` through the entire call stack; all DB write operations and external process calls are gated behind `if not dry_run:` checks verified by unit tests.

---

## 11. Testing Strategy

### 11.1 Unit Tests

| Test | Description |
|------|-------------|
| `test_parse_duration_minutes` | Table-driven: `"30m"→30`, `"1h"→60`, `"2h30m"→150`, `"7d"→10080`, `"0"→0`, `"invalid"→ValueError` |
| `test_set_policy_creates_row` | Assert row exists in `sandbox_policies` after `set_policy()` call |
| `test_set_policy_updates_row` | Call `set_policy()` twice; assert `updated_at` changed and only one row exists |
| `test_get_policy_none` | Assert `get_policy()` returns `None` for profile with no policy |
| `test_sweep_noop_no_policy` | Call `policy_sweep()` with no policy; assert returns `[]` |
| `test_sweep_idle_timeout_stops` | Insert running sandbox row with `last_activity_at = 40m ago`, policy idle=30m; assert sweep stops it |
| `test_sweep_idle_timeout_no_action` | Insert running sandbox row idle for 20m, policy idle=30m; assert sweep returns `[]` |
| `test_sweep_warning_fired` | Advance mock clock to `idle - 1m`; assert warning action in result and `warned_at` set |
| `test_sweep_warning_not_duplicated` | Run sweep twice at warning threshold; assert only one warning event in `sandbox_policy_events` |
| `test_sweep_archive` | Insert `status='stopped'` row with `created_at = 16d ago`, policy archive=14d; assert `status='archived'` |
| `test_sweep_archive_skips_running` | Running sandbox older than archive threshold is not archived |
| `test_concurrency_gate_blocks` | Insert 3 running rows, policy max_concurrent=3; assert `PolicyViolationError(code='concurrency_limit')` |
| `test_concurrency_gate_allows` | Insert 2 running rows, policy max_concurrent=3; assert no error |
| `test_cost_gate_blocks` | Insert rows summing to `cost_usd = 5.10` in last 24h, policy max_cost_daily=5.00; assert `PolicyViolationError(code='cost_cap')` |
| `test_cost_gate_allows` | Insert rows summing to `cost_usd = 4.99`, policy max_cost_daily=5.00; assert no error |
| `test_sweep_dry_run` | Dry-run sweep; assert actions returned but `sandbox_runs.status` unchanged and no events in `sandbox_policy_events` |
| `test_sweep_stop_failure_continues` | Simulate `docker stop` failing for sandbox 1; assert sandbox 2 is still processed |
| `test_policy_events_recorded` | After sweep stops 2 sandboxes; assert 2 rows in `sandbox_policy_events` with correct `reason` and `action` |

### 11.2 Integration Tests

| Test | Description |
|------|-------------|
| `test_cmd_policy_set_show_roundtrip` | Run `cmd_sandbox_policy_set()`, then `cmd_sandbox_policy_show()`, assert JSON output fields match input |
| `test_cmd_policy_apply_now` | Create 2 idle sandboxes, set policy, run `cmd_sandbox_policy_apply(now=True)`, assert both stopped in DB |
| `test_cmd_policy_unset_removes_row` | Set policy, unset it, assert `get_policy()` returns None and cron job deregistered |
| `test_cron_registration` | After `set_policy()`, assert `__sandbox_policy_sweep__` row in `cron_jobs` table |
| `test_cron_deregistration` | After `delete_policy()` for last profile, assert cron row deleted |
| `test_schema_migration_idempotent` | Call `ensure_schema()` twice on same DB; assert no error and no duplicate rows |
| `test_policy_list_shows_all` | Create 3 policies for different profiles; assert all 3 appear in `list_policies()` output |
| `test_history_query` | Insert 5 events, query history with `since=3d`; assert correct subset returned |

### 11.3 Performance Tests

| Test | Threshold |
|------|-----------|
| `bench_sweep_1000_sandboxes` | Populate 1 000 `sandbox_runs` rows (500 running, 300 stopped, 200 archived); time `policy_sweep()`; assert p99 < 200 ms |
| `bench_concurrency_gate` | Call `_check_policy_gates()` 10 000 times; assert p99 < 1 ms per call |
| `bench_schema_migration` | Call `ensure_schema()` 100 times on existing DB; assert total time < 100 ms |

---

## 12. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|--------------|
| AC-01 | `tag sandbox policy set --idle-timeout 30m` creates a policy row with `idle_timeout_minutes = 30` in `sandbox_policies`. | `SELECT idle_timeout_minutes FROM sandbox_policies WHERE profile = ?` returns 30. |
| AC-02 | Running sandbox idle for more than `idle_timeout_minutes` is set to `status = 'stopped'` within 60 seconds of the next cron tick. | Integration test: insert idle sandbox, advance clock, run sweep, assert status. |
| AC-03 | A pre-stop warning notification is emitted between 60 s and 120 s before the idle-timeout stop action. | Unit test with mock clock and mock `notifications.notify()`; assert call timing. |
| AC-04 | `tag sandbox policy set --max-concurrent 3` causes the 4th concurrent `run_in_sandbox()` call to raise `PolicyViolationError` with `code = 'concurrency_limit'` before any Docker invocation. | Unit test: mock 3 running rows, call `_check_policy_gates()`, assert exception before `subprocess.run` call. |
| AC-05 | `tag sandbox policy set --max-cost-daily 5.00` causes `run_in_sandbox()` to raise `PolicyViolationError` with `code = 'cost_cap'` when the rolling 24h `SUM(cost_usd)` for the profile meets or exceeds 5.00. | Unit test: insert cost rows summing to 5.01, call gate, assert exception. |
| AC-06 | `tag sandbox policy apply --now --dry-run` prints the list of actions that would be taken without modifying any `sandbox_runs` row or inserting any `sandbox_policy_events` row. | Integration test: assert DB unchanged after dry-run call. |
| AC-07 | Stopped sandboxes older than `archive_after_days` have `status = 'archived'` after a sweep. | Integration test: insert stopped row with `created_at = archive_after_days + 1d ago`, run sweep, assert status. |
| AC-08 | `tag sandbox policy unset` removes the policy row; subsequent `run_in_sandbox()` calls under that profile are not blocked by any gate. | Integration test: set policy, unset, call `_check_policy_gates()`, assert no exception. |
| AC-09 | `tag sandbox policy show --json` returns valid JSON with all policy fields and live state fields without error. | `json.loads()` succeeds on stdout; assert required keys present. |
| AC-10 | All policy enforcement actions appear in `sandbox_policy_events` with correct `action`, `reason`, and `sandbox_id`. | After sweep, `SELECT COUNT(*) FROM sandbox_policy_events WHERE policy_id = ?` matches expected action count. |
| AC-11 | Sandboxes created under a profile with no active policy are not affected by sweep execution. | Insert sandboxes under `profile = 'no-policy'`; call `policy_sweep(conn, 'no-policy')`; assert returns `[]`. |
| AC-12 | `ensure_schema()` called on a DB that already has the new columns does not raise an exception. | Call twice; assert no `OperationalError`. |
| AC-13 | The cron job `__sandbox_policy_sweep__` is registered in `cron_jobs` after the first `set_policy()` call and removed after the last `delete_policy()` call. | Assert row presence/absence via `SELECT COUNT(*) FROM cron_jobs WHERE name = ?`. |
| AC-14 | `tag sandbox policy history --since 7d` returns only events with `timestamp >= NOW() - 7d`. | Insert events at `now - 3d` and `now - 10d`; assert only one returned. |
| AC-15 | `policy_sweep()` completes in under 200 ms for 1 000 candidate rows (benchmark test). | `time.perf_counter()` before and after; assert delta < 0.200. |

---

## 13. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| PRD-028 (Sandbox Code Execution) | Blocking | `sandbox_runs` table, `run_in_sandbox()` function, backend abstractions must exist |
| PRD-091 (Configurable Sandbox TTL) | Blocking | `last_activity_at` column may overlap; coordinate schema to avoid conflicts |
| PRD-022 (Cron Scheduled Agents) | Blocking | `cron_jobs` table and `cron_scheduler.py` module required for automated sweep registration |
| PRD-040 (Notification Hooks) | Soft | Pre-stop warning notifications require `notifications.py`; gracefully degraded if absent |
| PRD-013 (Agent Tracing) | Soft | Sweep actions emit OTel spans if tracing is enabled; no-op if `tracing.py` not configured |
| PRD-012 / PRD-039 (Budget) | Informational | Cost cap is independent of token budget; both coexist without conflict |
| Python `threading` | Stdlib | Lock per profile; no third-party dependency |
| Python `re` | Stdlib | Duration parser |
| SQLite 3.25+ | Runtime | `CREATE INDEX IF NOT EXISTS` and WAL mode; standard in Python 3.8+ |

---

## 14. Open Questions

| # | Question | Owner | Target Resolution |
|---|----------|-------|-------------------|
| OQ-1 | Should `cost_usd` be populated automatically by `run_in_sandbox()` based on cloud provider metadata, or should callers be responsible for updating the column after a run completes? Modal and E2B expose per-run cost in their response objects; the Docker backend has no billing signal. | @sandbox-lead | Before implementation starts |
| OQ-2 | Should the sweep be a pull (cron calls sweep function) or a push (sandbox creation registers a per-sandbox timer)? The cron approach has 60-second granularity but zero idle overhead; per-sandbox timers are more precise but add state management complexity. Current design uses cron. | @infra | Design review |
| OQ-3 | The concurrency gate reads `status = 'running'` which is set synchronously at INSERT time. If two `run_in_sandbox()` calls race before either updates status, the gate may allow both through. Is a SQLite `UNIQUE` constraint on `(profile, status='running')` feasible, or should we accept the race condition as acceptable at the current concurrency level? | @db | Before implementation |
| OQ-4 | Should `tag sandbox policy set` validate that the active profile exists in the TAG config before creating a policy? If a user typos the profile name, the policy silently does nothing. | @ux | Before implementation |
| OQ-5 | For the Docker backend, `_stop_sandbox()` calls `docker stop <container_id>`. The `container_id` must be stored in `sandbox_runs`, but PRD-028's schema only stores the image name and run parameters, not the Docker container ID. Should we add a `container_id` column now, or resolve container ID via `docker ps --filter label=tag.run_id=<id>`? | @sandbox-lead | Schema review |
| OQ-6 | Should `archive_after_days` also trigger pruning of Docker images that are no longer referenced by any non-archived sandbox? Image pruning is disk-impactful and could be disruptive on shared Docker installations. | @platform | Before implementation |
| OQ-7 | Is the 60-second sweep interval (one cron tick per minute) acceptable for idle-timeout durations as short as 5 minutes? The worst-case latency is idle_timeout + 60s. If users want sub-minute accuracy, we would need a higher-frequency mechanism. | @product | Spec review |

---

## 15. Complexity and Timeline

**Overall estimate:** S (3–5 engineering days)

### Phase 1 — Schema and Core Data Layer (Day 1)

- Add `ensure_schema()` extension for `sandbox_policies`, `sandbox_policy_events`, and `sandbox_runs` migration columns in `sandbox.py`
- Implement `SandboxPolicy` dataclass and `PolicyViolationError`
- Implement `_parse_duration_minutes()` with full test coverage
- Implement `set_policy()`, `get_policy()`, `delete_policy()`, `list_policies()`
- Write unit tests for all data layer functions

**Exit criterion:** All unit tests for data layer pass; `sandbox_policies` table created correctly on a fresh DB.

### Phase 2 — Sweep Engine and Gates (Days 2–3)

- Implement `policy_sweep()` with idle-timeout, archive, and cost-cap enforcement
- Implement `policy_sweep_preview()` (dry-run path)
- Implement `_check_policy_gates()` (concurrency and cost gates in `run_in_sandbox()`)
- Implement `_fire_warning()` with `notifications.py` integration
- Implement `_stop_sandbox()` with Docker, Modal, and restricted backend handlers
- Register and deregister sweep cron job via `cron_scheduler.py`
- Write unit tests for sweep logic (idle, archive, cost) and gate logic

**Exit criterion:** `policy_sweep()` correctly stops/archives sandboxes in unit tests; gates reject violating calls; dry-run leaves DB unchanged.

### Phase 3 — CLI Commands and Controller Integration (Day 4)

- Implement `cmd_sandbox_policy_set`, `cmd_sandbox_policy_show`, `cmd_sandbox_policy_apply`, `cmd_sandbox_policy_unset`, `cmd_sandbox_policy_list`, `cmd_sandbox_policy_history` in `controller.py`
- Wire `__internal__:sandbox_policy_sweep` sentinel to `policy_sweep_all_profiles()` in the cron dispatcher
- Add `--profile` flag propagation to `run_in_sandbox()` callers that do not yet pass profile
- Write integration tests for CLI round-trips

**Exit criterion:** All six CLI subcommands produce correct output; integration tests pass end-to-end.

### Phase 4 — Testing, Benchmarks, and Documentation (Day 5)

- Run performance benchmark for 1 000-row sweep; optimise if needed (batch SELECT, indexed scan)
- Add OTel span emission for sweep actions via `tracing.py`
- Verify backward compatibility: existing `sandbox_runs` rows with null columns handled gracefully
- Update `docs/prd/INDEX.md` with PRD-100 entry
- Final review and cleanup

**Exit criterion:** All acceptance criteria verified; benchmark under 200 ms; no regressions in existing sandbox tests.
