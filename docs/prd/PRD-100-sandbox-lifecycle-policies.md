# PRD-100: Auto-Stop/Auto-Archive Lifecycle Policies for Idle Sandboxes (`tag sandbox policy`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** S (3-5 days)
**Category:** Sandbox & Execution Environment
**Affects:** `internal/sandbox` (with `internal/cron`, `internal/notify`, `internal/obs`)
**Depends on:** PRD-028 (Sandbox Code Execution), PRD-091 (Configurable Sandbox TTL + Session Refresh), PRD-022 (Cron Scheduled Agents), PRD-013 (Agent Tracing / Observability), PRD-034 (Secret Scanning / Security), PRD-012 (Cost Tracking / Budget), PRD-039 (Token Budget Enforcement), PRD-040 (Notification Hooks)
**Inspired by:** Daytona workspace policies, Gitpod idle timeout, GitHub Codespaces timeout

---

## 1. Overview

TAG's sandbox subsystem (PRD-028) currently provides ephemeral command execution across three backends — restricted subprocess, Docker, and Modal — with per-invocation wall-clock timeouts. PRD-091 extended this model by introducing persistent sandbox sessions with configurable TTLs and per-session keepalive refreshes. However, these TTL controls are still reactive and per-session: there is no mechanism to declare an operator-level policy that applies uniformly across all sandboxes within a profile, enforces hard resource caps (cost, concurrency, archival age), and runs enforcement autonomously in the background without user intervention.

Cloud sandbox providers have each converged on a tiered policy model above the raw TTL primitive. Daytona exposes `WorkspacePolicies` at the profile or organization level with `auto_stop_interval`, `pinned_image`, and `inactivity_lock_duration` fields, allowing administrators to declare rules once and have the platform enforce them fleet-wide. Gitpod's idle timeout applies to all workspaces in a team plan, with per-workspace overrides; the daemon monitors CPU/network quiescence and stops workspaces that breach the threshold. GitHub Codespaces separates `idle_timeout` (default 30 minutes) from `retention_period` (default 30 days to deletion) and enforces both independently. All three providers treat policy as a first-class configuration entity distinct from per-session state, and all three enforce policies via an asynchronous daemon rather than requiring user-triggered cleanup.

This PRD introduces `tag sandbox policy` — a lifecycle policy subsystem in `internal/sandbox`. A **SandboxPolicy** is a named, profile-scoped configuration document stored in the `modernc.org/sqlite` state store (`internal/store`) that captures four enforcement axes: (1) auto-stop after N minutes of idle time (no new commands executed), (2) auto-archive after M days since creation, (3) a maximum number of concurrently running sandboxes per profile, and (4) a maximum USD cost incurred by sandboxes within a rolling 24-hour window. The scheduler (`internal/cron`, backed by `go-co-op/gocron v2`) runs a `sandbox-policy-sweep` job every minute — a background reaper goroutine driven by the scheduler — that applies all active policies to all matching sandboxes in the appropriate state transitions. Notifications via `internal/notify` fire pre-stop warnings and post-archive confirmations, giving users the same experience as commercial providers. Policies may also be seeded from `tag.yaml` via the `koanf`/`yaml.v3` config layer, but the SQLite table is the authoritative durable store and audit sink.

The implementation is additive and backward-compatible. Sandboxes without a matching policy continue to behave exactly as before. When a policy is attached to a profile, it applies to all new sandbox sessions created under that profile starting from the moment of policy creation; existing sessions are brought into compliance on the next sweep cycle. The `tag sandbox policy apply --now` command triggers an immediate enforcement sweep for operators who need instant compliance without waiting for the next scheduler tick.

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
| G2 | The policy reaper sweep runs every 60 seconds as a `go-co-op/gocron v2` job in `internal/cron` and autonomously stops sandboxes that have been idle beyond `idle_timeout_minutes` and archives (marks as `archived`) those that have exceeded `archive_after_days`. |
| G3 | `tag sandbox policy apply --now` triggers an immediate synchronous enforcement sweep for the active profile, reporting each action taken. |
| G4 | `tag sandbox policy show` and `tag sandbox policy show --json` display the effective policy for the active profile, including computed enforcement state (next sweep time, sandboxes currently in scope, projected cost impact). |
| G5 | When a policy's `max_concurrent` limit is reached, new `tag sandbox run` calls under that profile fail fast with a clear error message before allocating any backend resources. |
| G6 | When the rolling 24-hour sandbox cost for a profile reaches `max_cost_daily_usd`, the policy reaper stops the most-idle running sandboxes until the projected cost drops below the cap; a notification is fired via `internal/notify`. |
| G7 | A pre-stop warning notification is fired via `internal/notify` at least 60 seconds before an idle-timeout stop is executed, giving an interactive user the chance to send a keepalive. |
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
| NG7 | Real-time resource metering inside sandboxes (CPU%, memory%). The idle signal is defined solely as time elapsed since the last command was submitted via `RunInSandbox()`, not OS-level resource quiescence. |

---

## 4. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Idle cost reduction | Sandboxes stopped within `idle_timeout + 60s` of going idle | Integration test: create sandbox, wait `idle_timeout + 70s`, assert `status = stopped` |
| Sweep latency | p99 sweep execution time < 200 ms for up to 1 000 sandbox rows | Benchmark test: populate 1 000 `sandbox_runs` rows, time `PolicySweep()` call |
| Concurrency gate | `tag sandbox run` rejects with exit code 2 within 50 ms when `max_concurrent` is reached | Unit test: fake 5 running sandboxes, assert rejection before the Docker client call |
| Cost cap accuracy | Daily cost cap triggers stop action within one sweep cycle (≤ 60 s) of breach | Integration test: inject synthetic `cost_usd` rows summing to `> max_cost_daily_usd`, run sweep, assert stop actions |
| Notification timing | Pre-stop warning fires between 60 s and 120 s before actual stop action | Unit test: set injected `Clock` to `idle_timeout - 90s`, assert warning notification emitted |
| Policy persistence | Policy survives TAG process restart and re-read from SQLite | Integration test: set policy, close DB handle, reopen, assert policy row present |
| Zero-policy overhead | `RunInSandbox()` latency with no active policy ≤ 1 ms increase over baseline | Benchmark: 100 calls, measure p99 latency delta |
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
| FR-01 | The `sandbox_policies` SQLite table is created by `EnsureSchema(ctx, db)` on first call; schema is idempotent (uses `CREATE TABLE IF NOT EXISTS` on the `modernc.org/sqlite` handle). | P0 |
| FR-02 | The `sandbox_policy_events` SQLite table records every enforcement action (stop, archive, warning, concurrency-block, cost-block) with `sandbox_id`, `action`, `reason`, `detail`, and `timestamp`. | P0 |
| FR-03 | `tag sandbox policy set` creates a new policy row if none exists for the profile, or updates the existing row if one exists, preserving unspecified fields (`nil` pointer fields on the update struct leave the column untouched). | P0 |
| FR-04 | `tag sandbox policy set --idle-timeout 0` sets `idle_timeout_minutes = NULL`, disabling idle-timeout enforcement for that policy without removing other constraints. | P1 |
| FR-05 | Duration arguments accept integer seconds, `Nm` (minutes), `Nh` (hours), `Nd` (days), and compound forms `NhMm`. An invalid duration string returns a parse error before any DB write. | P1 |
| FR-06 | `PolicySweep(ctx, db, clock, profile)` selects all `sandbox_runs` rows where `profile = ?` and `status = 'running'`, computes idle duration as `clock.Now() - last_activity_at`, and stops any sandbox where `idle_duration > idle_timeout_minutes`. | P0 |
| FR-07 | `PolicySweep()` fires a pre-stop warning notification via the injected `notify.Notifier` for any sandbox within 60 seconds of its idle-timeout threshold, if a warning has not already been fired for that sandbox in the current window. | P1 |
| FR-08 | `PolicySweep()` selects all `sandbox_runs` rows where `profile = ?` and `status IN ('stopped', 'done', 'failed')` and archives rows where `age_days > archive_after_days`. Archive sets `status = 'archived'`. | P0 |
| FR-09 | Before a new `RunInSandbox()` call, the concurrency gate queries `COUNT(*) WHERE profile = ? AND status = 'running'`. If the count meets or exceeds `max_concurrent`, the function returns a `*PolicyViolationError` with `Code = "concurrency_limit"` before allocating any backend resource. | P0 |
| FR-10 | The cost gate queries `SUM(cost_usd) WHERE profile = ? AND created_at >= ?` (24h window computed in Go). If the sum meets or exceeds `max_cost_daily_usd`, `RunInSandbox()` returns a `*PolicyViolationError` with `Code = "cost_cap"`. | P0 |
| FR-11 | When the `max_cost_daily_usd` cap is breached mid-sweep (i.e., a running sandbox's accumulated cost causes the rolling sum to exceed the cap), `PolicySweep()` stops the most-idle running sandbox and fires a cost-cap notification. | P1 |
| FR-12 | The sweep is registered as a named `go-co-op/gocron v2` job `sandbox-policy-sweep` at `* * * * *` (every minute, `WithSingletonMode`) in `internal/cron` when a policy is first set; the job is removed when all policies are removed. On process start, `internal/cron` re-registers it iff any enabled policy row exists. | P1 |
| FR-13 | `tag sandbox policy apply --now` calls `PolicySweep()` synchronously and prints each action. The `--dry-run` flag passes `dryRun=true` through `PolicySweep()` so it returns the actions without executing them. | P1 |
| FR-14 | `tag sandbox policy show --json` returns a JSON object (typed structs, `encoding/json`) with both the policy configuration and the live enforcement state (running count, stopped count, archived count, rolling 24h cost). | P1 |
| FR-15 | `tag sandbox policy list` returns all rows from `sandbox_policies` where `enabled = 1`, formatted as a table. | P2 |
| FR-16 | `tag sandbox policy history` queries `sandbox_policy_events` with profile filter and optional `since` timestamp filter, ordered by `timestamp DESC`. | P2 |
| FR-17 | `tag sandbox policy unset` deletes the policy row for the given profile and deregisters the gocron sweep job if no other policies remain. It does NOT stop or archive running sandboxes. | P1 |
| FR-18 | The `sandbox_runs` table gains a `profile` column (TEXT, nullable, indexed) and a `cost_usd` column (REAL, nullable) via `ALTER TABLE` migrations run in Go and guarded by a `duplicate column name` error check. | P0 |
| FR-19 | The `sandbox_runs` table gains a `last_activity_at` column (TEXT, nullable) updated on every `RunInSandbox()` call for an existing session. For legacy rows where `last_activity_at IS NULL`, idle-timeout computes from `created_at`. | P0 |
| FR-20 | All policy enforcement actions emit an OpenTelemetry span via `internal/obs` (`go.opentelemetry.io/otel`) with attributes `sandbox.policy.action`, `sandbox.policy.reason`, and `sandbox.id`, enabling correlation with existing TAG traces. | P2 |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Sweep latency | `PolicySweep()` completes in < 200 ms for up to 1 000 sandbox rows at p99. Uses a single indexed `SELECT` for candidate selection, not N individual queries. |
| NFR-02 | Decoupling | `internal/sandbox` does not take a hard package dependency on `internal/cron` or `internal/notify`; the scheduler and the `notify.Notifier` are supplied through interfaces via dependency injection, so the sweep and warning paths are testable with fakes and impose no init-time cost. |
| NFR-03 | Atomicity | Each stop or archive action within a sweep is committed to SQLite in its own `*sql.Tx`. A failure on sandbox N does not roll back actions on sandboxes 1..N-1. |
| NFR-04 | Idempotency | Running `PolicySweep()` twice in rapid succession for the same profile produces the same final state without duplicate actions or events. Warning notifications are deduplicated using a `warned_at` column on `sandbox_runs`. |
| NFR-05 | Concurrency safety | `PolicySweep()` acquires a per-profile `*sync.Mutex` (from a mutex map guarded by its own mutex) for the duration of the sweep; the gocron job additionally runs in `WithSingletonMode` so overlapping ticks cannot double-fire. |
| NFR-06 | WAL-mode compatibility | All DB writes go through the single `internal/store` handle with `?` positional parameters; no `BEGIN EXCLUSIVE`. Compatible with the WAL-mode `modernc.org/sqlite` database at `~/.tag/runtime/tag.sqlite3`. |
| NFR-07 | Duration parse accuracy | Duration parser correctly handles: `"30m"` → 30, `"1h"` → 60, `"2h30m"` → 150, `"90"` → 90 (bare integer = minutes), `"7d"` → 10 080. Sub-minute seconds are truncated to whole minutes via integer division. |
| NFR-08 | Backward compatibility | Existing `sandbox_runs` rows without `profile`, `cost_usd`, `last_activity_at`, or `warned_at` columns are handled gracefully; `EnsureSchema()` runs each `ALTER TABLE … ADD COLUMN` in Go, swallowing the `duplicate column name` error so re-runs are no-ops. |
| NFR-09 | Error isolation | A stop action that fails (e.g., Docker container already removed) logs via `slog.Warn` and records a `stop_failed` event but does not abort the sweep. The sweep continues to the next candidate. |
| NFR-10 | Policy visibility | Every policy check that results in a block (concurrency, cost) surfaces a human-readable error (the `*PolicyViolationError.Error()` string) to stderr: `policy violation: concurrency limit reached (3/3 sandboxes running for profile 'coder')`. |

---

## 9. Technical Design

### 9.1 New and Modified Files

| File | Change Type | Description |
|------|-------------|-------------|
| `internal/sandbox/policy.go` | New | Primary implementation target. Adds the `EnsureSchema()` extension, the `SandboxPolicy` struct, `PolicyViolationError`, `SetPolicy()`, `GetPolicy()`, `DeletePolicy()`, `ListPolicies()`, `PolicySweep()` (with a `dryRun` param), `parseDurationMinutes()`, `enforcementState()`, `fireWarning()`, and the concurrency/cost gates called from `RunInSandbox()` (in `internal/sandbox/run.go`). |
| `internal/cli/sandbox_policy.go` | New | Adds the `set`, `show`, `apply`, `unset`, `list`, `history` `cobra` subcommands under the `tag sandbox policy` command group. |
| `internal/cron/policy.go` | New | Registers/deregisters the `sandbox-policy-sweep` `go-co-op/gocron v2` job and provides `PolicySweepAllProfiles(ctx)`. |

### 9.2 SQLite DDL

The following DDL is applied by `EnsureSchema()` in `internal/sandbox` against the single `modernc.org/sqlite` handle (CGO_ENABLED=0, WAL). `CREATE TABLE`/`CREATE INDEX` use the idempotent `IF NOT EXISTS` form. Each `ALTER TABLE … ADD COLUMN` is executed from Go and wrapped by an `addColumn` helper that swallows an error whose message contains `duplicate column name`, making re-runs no-ops:

```go
func addColumn(ctx context.Context, db store.Querier, ddl string) error {
	if _, err := db.ExecContext(ctx, ddl); err != nil &&
		!strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	return nil
}
```

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

### 9.3 Core Go Types

Nullable columns map to pointer fields (`*int`, `*float64`) — a `nil` field means "constraint disabled". `sql.NullString`/`COALESCE` handle nullable reads. `PolicyViolationError` is a Go error type with an inspectable `Code`, matchable via `errors.As`. JSON field names are set with struct tags to match the §6 output contract exactly.

```go
package sandbox

// SandboxPolicy is a declarative lifecycle policy for sandboxes under a profile.
type SandboxPolicy struct {
	ID                 string   `json:"id"`
	Profile            string   `json:"profile"`
	IdleTimeoutMinutes *int     `json:"idle_timeout_minutes"` // nil = disabled
	ArchiveAfterDays   *int     `json:"archive_after_days"`   // nil = disabled
	MaxConcurrent      *int     `json:"max_concurrent"`       // nil = disabled
	MaxCostDailyUSD    *float64 `json:"max_cost_daily_usd"`   // nil = disabled
	Enabled            bool     `json:"enabled"`
	CreatedAt          string   `json:"created_at"`
	UpdatedAt          string   `json:"updated_at"`
}

// scanPolicy scans one row into a SandboxPolicy (nullable columns via pointers).
func scanPolicy(s interface{ Scan(...any) error }) (SandboxPolicy, error) {
	var p SandboxPolicy
	err := s.Scan(&p.ID, &p.Profile, &p.IdleTimeoutMinutes, &p.ArchiveAfterDays,
		&p.MaxConcurrent, &p.MaxCostDailyUSD, &p.Enabled, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// PolicyEnforcementState is live enforcement state computed at query time.
type PolicyEnforcementState struct {
	Policy           SandboxPolicy `json:"policy"`
	RunningCount     int           `json:"running_count"`
	StoppedCount     int           `json:"stopped_count"`
	ArchivedCount    int           `json:"archived_count"`
	CostLast24hUSD   float64       `json:"cost_last_24h_usd"`
	NextSweepAt      string        `json:"next_sweep_at,omitempty"`
	LastSweepAt      string        `json:"last_sweep_at,omitempty"`
	LastSweepActions int           `json:"last_sweep_actions"`
}

// PolicyAction is a single action taken (or previewed) during a sweep.
type PolicyAction struct {
	Action    string `json:"action"`     // stopped|archived|warning|skipped
	SandboxID string `json:"sandbox_id"`
	Reason    string `json:"reason"`     // idle_timeout|archive_after|cost_cap
	Detail    string `json:"detail"`     // human-readable
	DryRun    bool   `json:"-"`
}

// PolicyViolationError is returned when a new sandbox creation is blocked.
type PolicyViolationError struct {
	Code    string // "concurrency_limit" | "cost_cap"
	Message string
}

func (e *PolicyViolationError) Error() string { return "policy violation: " + e.Message }
```

### 9.4 Duration Parser

Returns `(*int, error)`: a `nil` pointer means "unset" (empty input); a `0` value means "explicitly disabled". This distinguishes "field omitted" from "field set to disable" for the update semantics in FR-03/FR-04.

```go
var durationRe = regexp.MustCompile(`(?i)^(?:(\d+)d)?(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s?)?$`)

// parseDurationMinutes parses a duration string to integer minutes.
//
//	""       -> nil  (unset, leave existing value)
//	"0"      -> 0     (explicitly disable)
//	"30"     -> 30    (bare integer = minutes)
//	"30m"    -> 30
//	"1h"     -> 60
//	"2h30m"  -> 150
//	"7d"     -> 10080
//	"1d12h"  -> 2160
func parseDurationMinutes(s string) (*int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if s == "0" {
		zero := 0
		return &zero, nil
	}
	if n, err := strconv.Atoi(s); err == nil { // bare integer = minutes
		return &n, nil
	}
	m := durationRe.FindStringSubmatch(s)
	if m == nil || (m[1] == "" && m[2] == "" && m[3] == "" && m[4] == "") {
		return nil, fmt.Errorf("unrecognised duration %q: use formats like 30m, 1h, 2h30m, 7d", s)
	}
	atoi := func(x string) int { n, _ := strconv.Atoi(x); return n }
	total := atoi(m[1])*1440 + atoi(m[2])*60 + atoi(m[3]) + atoi(m[4])/60
	if total == 0 {
		return nil, fmt.Errorf("duration %q resolves to 0 minutes; use 0 to disable enforcement", s)
	}
	return &total, nil
}
```

### 9.5 Policy Sweep Algorithm

The sweep is a goroutine-safe function. Per-profile serialization uses a `*sync.Mutex` from a map guarded by its own mutex; a non-blocking `TryLock` skips a profile whose sweep is already running (the gocron job additionally uses `WithSingletonMode`). Time comes from the injected `Clock`. Backend teardown is behind a `Reaper` interface so tests inject a fake.

```go
var (
	sweepMu    sync.Mutex
	sweepLocks = map[string]*sync.Mutex{}
)

func profileLock(profile string) *sync.Mutex {
	sweepMu.Lock()
	defer sweepMu.Unlock()
	l, ok := sweepLocks[profile]
	if !ok {
		l = &sync.Mutex{}
		sweepLocks[profile] = l
	}
	return l
}

// PolicySweep enforces the active policy for profile against all matching
// sandbox_runs rows. When dryRun is true it computes actions without executing
// DB writes, notifications, or backend teardown.
//
// Algorithm:
//  1. Load the SandboxPolicy; return nil if none or disabled.
//  2. Acquire the per-profile lock (skip if a sweep is already in progress).
//  3. IDLE-TIMEOUT: running sandboxes ordered by last_activity_at ASC.
//     >= idle_timeout_minutes-1 && warned_at IS NULL -> warn + set warned_at.
//     >= idle_timeout_minutes                        -> stop.
//  4. ARCHIVE: stopped/done/failed rows older than archive_after_days -> archived.
//  5. COST CAP: rolling 24h cost >= max_cost_daily_usd -> stop most-idle running sandbox.
//  6. Record each action in sandbox_policy_events; return []PolicyAction.
func (s *Service) PolicySweep(ctx context.Context, profile string, dryRun bool) ([]PolicyAction, error) {
	lock := profileLock(profile)
	if !lock.TryLock() {
		return nil, nil // sweep already in progress for this profile
	}
	defer lock.Unlock()

	policy, err := s.GetPolicy(ctx, profile)
	if err != nil || policy == nil || !policy.Enabled {
		return nil, err
	}
	now := s.clock.Now().UTC()
	var actions []PolicyAction

	// 1. Idle-timeout enforcement.
	if policy.IdleTimeoutMinutes != nil {
		limit := float64(*policy.IdleTimeoutMinutes)
		rows, err := s.db.QueryContext(ctx,
			`SELECT id, backend, last_activity_at, created_at, warned_at
			   FROM sandbox_runs
			  WHERE profile = ? AND status = 'running'
			  ORDER BY COALESCE(last_activity_at, created_at) ASC`, profile)
		if err != nil {
			return nil, err
		}
		type cand struct {
			id, backend string
			lastAct     sql.NullString
			createdAt   string
			warnedAt    sql.NullString
		}
		var cands []cand
		for rows.Next() {
			var c cand
			if err := rows.Scan(&c.id, &c.backend, &c.lastAct, &c.createdAt, &c.warnedAt); err != nil {
				rows.Close()
				return nil, err
			}
			cands = append(cands, c)
		}
		rows.Close()

		for _, c := range cands {
			ref := c.createdAt
			if c.lastAct.Valid {
				ref = c.lastAct.String
			}
			refDt, _ := parseISO(ref)
			idleMin := now.Sub(refDt).Minutes()

			if idleMin >= limit-1 && !c.warnedAt.Valid {
				s.fireWarning(ctx, policy, c.id, idleMin, limit, dryRun)
				actions = append(actions, PolicyAction{
					Action: "warning", SandboxID: c.id, Reason: "idle_timeout",
					Detail: fmt.Sprintf("idle %.1fm / limit %.0fm", idleMin, limit), DryRun: dryRun,
				})
			}
			if idleMin >= limit {
				s.stopSandbox(ctx, policy, c.id, c.backend, "idle_timeout",
					fmt.Sprintf("idle %.1fm / limit %.0fm", idleMin, limit), dryRun)
				actions = append(actions, PolicyAction{
					Action: "stopped", SandboxID: c.id, Reason: "idle_timeout",
					Detail: fmt.Sprintf("idle %.1fm", idleMin), DryRun: dryRun,
				})
			}
		}
	}

	// 2. Archive enforcement.
	if policy.ArchiveAfterDays != nil {
		cutoff := now.AddDate(0, 0, -*policy.ArchiveAfterDays).Format(isoMicros)
		rows, err := s.db.QueryContext(ctx,
			`SELECT id, created_at FROM sandbox_runs
			  WHERE profile = ? AND status IN ('stopped','done','failed') AND created_at < ?`,
			profile, cutoff)
		if err != nil {
			return nil, err
		}
		type arch struct{ id, createdAt string }
		var toArchive []arch
		for rows.Next() {
			var a arch
			if err := rows.Scan(&a.id, &a.createdAt); err != nil {
				rows.Close()
				return nil, err
			}
			toArchive = append(toArchive, a)
		}
		rows.Close()

		for _, a := range toArchive {
			createdDt, _ := parseISO(a.createdAt)
			ageDays := int(now.Sub(createdDt).Hours() / 24)
			s.archiveSandbox(ctx, policy, a.id,
				fmt.Sprintf("age %dd / limit %dd", ageDays, *policy.ArchiveAfterDays), dryRun)
			actions = append(actions, PolicyAction{
				Action: "archived", SandboxID: a.id, Reason: "archive_after",
				Detail: fmt.Sprintf("age %dd", ageDays), DryRun: dryRun,
			})
		}
	}

	// 3. Cost-cap mid-sweep enforcement (epsilon guard against float rounding).
	if policy.MaxCostDailyUSD != nil {
		windowStart := now.Add(-24 * time.Hour).Format(isoMicros)
		var rolling float64
		if err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(cost_usd), 0.0) FROM sandbox_runs WHERE profile = ? AND created_at >= ?`,
			profile, windowStart).Scan(&rolling); err != nil {
			return nil, err
		}
		if rolling >= *policy.MaxCostDailyUSD-0.001 {
			var id, backend string
			err := s.db.QueryRowContext(ctx,
				`SELECT id, backend FROM sandbox_runs
				  WHERE profile = ? AND status = 'running'
				  ORDER BY COALESCE(last_activity_at, created_at) ASC LIMIT 1`, profile).
				Scan(&id, &backend)
			if err == nil {
				s.stopSandbox(ctx, policy, id, backend, "cost_cap",
					fmt.Sprintf("24h cost $%.2f / cap $%.2f", rolling, *policy.MaxCostDailyUSD), dryRun)
				actions = append(actions, PolicyAction{
					Action: "stopped", SandboxID: id, Reason: "cost_cap",
					Detail: fmt.Sprintf("24h cost $%.2f", rolling), DryRun: dryRun,
				})
			}
		}
	}

	return actions, nil
}
```

`stopSandbox` sets `status='stopped', completed_at=NOW()` in its own `*sql.Tx` (always) and then tears down the backend via the injected `Reaper` (skipped when `dryRun`): for `docker`/`gvisor` it calls the `docker/moby` client `ContainerStop(ctx, id, container.StopOptions{Timeout})` then `ContainerRemove`; for `firecracker` it calls the `firecracker-go-sdk` machine `Shutdown`/`StopVMM`; for `modal` it calls the provider's terminate HTTP endpoint; for `restricted` it cancels the process `context.Context` / kills the process group (`syscall.Kill(-pgid, SIGTERM)`) — no external call. A teardown failure is logged via `slog.Warn` and recorded as a `stop_failed` event but does not abort the sweep (NFR-09).

### 9.6 Concurrency and Cost Gate in `RunInSandbox()`

```go
// checkPolicyGates is called at the top of RunInSandbox() before any backend
// allocation. It returns a *PolicyViolationError if the active policy blocks the
// new sandbox. No-op if profile is empty or no policy exists.
func (s *Service) checkPolicyGates(ctx context.Context, profile string) error {
	if profile == "" {
		return nil
	}
	policy, err := s.GetPolicy(ctx, profile)
	if err != nil || policy == nil || !policy.Enabled {
		return err
	}

	// Concurrency gate.
	if policy.MaxConcurrent != nil {
		var count int
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sandbox_runs WHERE profile = ? AND status = 'running'`,
			profile).Scan(&count); err != nil {
			return err
		}
		if count >= *policy.MaxConcurrent {
			return &PolicyViolationError{
				Code: "concurrency_limit",
				Message: fmt.Sprintf(
					"concurrency limit reached (%d/%d sandboxes running for profile %q). "+
						"Run `tag sandbox kill <id>` or wait for idle-timeout enforcement.",
					count, *policy.MaxConcurrent, profile),
			}
		}
	}

	// Cost gate.
	if policy.MaxCostDailyUSD != nil {
		windowStart := s.clock.Now().UTC().Add(-24 * time.Hour).Format(isoMicros)
		var rolling float64
		if err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(cost_usd), 0.0) FROM sandbox_runs WHERE profile = ? AND created_at >= ?`,
			profile, windowStart).Scan(&rolling); err != nil {
			return err
		}
		if rolling >= *policy.MaxCostDailyUSD-0.001 { // epsilon guard
			return &PolicyViolationError{
				Code: "cost_cap",
				Message: fmt.Sprintf(
					"daily cost cap reached ($%.2f/$%.2f in last 24h for profile %q). "+
						"Use `tag sandbox policy show` to review spend.",
					rolling, *policy.MaxCostDailyUSD, profile),
			}
		}
	}
	return nil
}
```

`RunInSandbox()` calls `checkPolicyGates` first and returns the `*PolicyViolationError` unwrapped so `internal/cli` can map `Code` to exit code 2. Callers detect it via `errors.As(err, &*PolicyViolationError)`.

### 9.7 Scheduler Integration (`go-co-op/gocron v2`)

The sweep is a `go-co-op/gocron v2` job (not a `cron_jobs` sentinel row): `internal/cron` registers a named, singleton, one-minute job whose task is a Go closure — no `__internal__:` command-string dispatch is needed. The job is added when the first policy is created and removed when the last policy is deleted. Because gocron's schedule lives in memory, `internal/cron` re-registers the job on process start iff `SELECT COUNT(*) FROM sandbox_policies WHERE enabled = 1 > 0`, preserving the persisted-across-restart behaviour via the SQLite policy table.

```go
const sweepJobName = "sandbox-policy-sweep"

// RegisterSweep ensures the one-minute policy-sweep job exists on the scheduler.
func RegisterSweep(sched gocron.Scheduler, svc *sandbox.Service) error {
	for _, j := range sched.Jobs() {
		if j.Name() == sweepJobName {
			return nil // already registered
		}
	}
	_, err := sched.NewJob(
		gocron.CronJob("* * * * *", false), // every minute
		gocron.NewTask(func(ctx context.Context) { PolicySweepAllProfiles(ctx, svc) }),
		gocron.WithName(sweepJobName),
		gocron.WithSingletonMode(gocron.LimitModeReschedule), // no overlapping ticks
	)
	return err
}

// DeregisterSweepIfNoPolicies removes the job when no enabled policies remain.
func DeregisterSweepIfNoPolicies(ctx context.Context, sched gocron.Scheduler, svc *sandbox.Service) error {
	n, err := svc.CountEnabledPolicies(ctx)
	if err != nil || n > 0 {
		return err
	}
	for _, j := range sched.Jobs() {
		if j.Name() == sweepJobName {
			return sched.RemoveJob(j.ID())
		}
	}
	return nil
}

// PolicySweepAllProfiles fans out over every enabled-policy profile.
func PolicySweepAllProfiles(ctx context.Context, svc *sandbox.Service) {
	profiles, err := svc.EnabledPolicyProfiles(ctx)
	if err != nil {
		slog.Warn("policy sweep: list profiles", "err", err)
		return
	}
	for _, p := range profiles {
		if _, err := svc.PolicySweep(ctx, p, false); err != nil {
			slog.Warn("policy sweep failed", "profile", p, "err", err)
		}
	}
}
```

### 9.8 Integration Points

| Integration | Mechanism |
|-------------|-----------|
| `internal/cron` (`go-co-op/gocron v2`) | Named singleton job `sandbox-policy-sweep` at `* * * * *`; its task closure fans out to `PolicySweepAllProfiles` |
| `internal/notify` | The injected `notify.Notifier` interface (`Notify(ctx, Notification)`) called by `fireWarning()` and cost-cap events |
| `internal/obs` (`go.opentelemetry.io/otel`) | Each sweep action is wrapped in a span `tracer.Start(ctx, "sandbox.policy.sweep")` with the relevant attributes |
| `internal/obs` budget gate | Cost gate reads `cost_usd` from `sandbox_runs`; the token-budget gate remains independent |
| `internal/cli` (`spf13/cobra`) | Six new subcommands registered under the `tag sandbox policy` command group |

---

## 10. Security Considerations

1. **Policy manipulation requires local file access.** `sandbox_policies` rows are stored in `~/.tag/runtime/tag.sqlite3`, readable only by the local OS user. No remote API can modify policies; this is not a security surface in the threat model but should be documented for multi-user host deployments.

2. **Policy bypass via `--profile` spoofing is not possible.** The `profile` column is set by the caller at sandbox creation time; if a caller intentionally passes a different profile name to bypass concurrency limits, they are effectively self-misclassifying their sandbox and only bypass enforcement for themselves.

3. **Stop actions against Docker containers use the `docker/moby` client `ContainerStop` (SIGTERM then SIGKILL after the grace timeout) followed by `ContainerRemove`.** This is the same signal sequence as a manual `docker rm -f`. The container ID is read from the SQLite row; if the ID no longer exists in Docker (e.g., manually removed), the moby call returns a not-found error that is logged and ignored, and the DB row is still updated to `stopped` status, preventing zombie rows.

4. **Pre-stop warning notifications must not disclose policy configuration to untrusted processes.** The notification body includes sandbox ID and idle duration but not the raw `max_cost_daily_usd` or other policy parameters, to prevent information leakage in shared notification channels.

5. **Cost values are stored as REAL (float) in SQLite.** Floating-point arithmetic for cost comparisons uses `>= cap - 0.001` (epsilon) to avoid false negatives from IEEE 754 rounding. This is documented explicitly in the sweep code.

6. **The per-profile `*sync.Mutex` (plus gocron `WithSingletonMode`) prevents in-process TOCTOU races** between the concurrency gate check and the sandbox insertion. However, this is single-process only: WAL mode does not provide cross-process mutual exclusion, so if two TAG processes run simultaneously for the same profile the concurrency gate may be bypassed. The single-writer/wire-protocol invariant (one `tag serve` daemon owns the store) mitigates this; operators running TAG in a multi-process configuration should be aware of the limitation.

7. **The dry-run path (`dryRun=true`) must not call the backend `Reaper` or modify any SQLite row.** `dryRun` is threaded through the entire call stack; all DB writes, notifications, and backend teardown are gated behind `if !dryRun {` checks, verified by unit tests that assert the DB and the fake `Reaper` are untouched.

---

## 11. Testing Strategy

Tests use the Go `testing` package with table-driven cases, an in-memory `modernc.org/sqlite` DB, an injected fake `Clock`, and fake `Notifier`/`Reaper` implementations (interfaces, not monkeypatching). CLI round-trips drive the `cobra` command via `cmd.SetArgs(...)` with a captured `OutOrStdout()`.

### 11.1 Unit Tests (`internal/sandbox/policy_test.go`)

| Test | Description |
|------|-------------|
| `TestParseDurationMinutes` | Table-driven: `"30m"→30`, `"1h"→60`, `"2h30m"→150`, `"7d"→10080`, `"0"→0`, `""→nil`, `"invalid"→error` |
| `TestSetPolicyCreatesRow` | Assert row exists in `sandbox_policies` after `SetPolicy()` |
| `TestSetPolicyUpdatesRow` | Call `SetPolicy()` twice; assert `updated_at` changed, `nil` fields preserved, and only one row exists |
| `TestGetPolicyNone` | Assert `GetPolicy()` returns `(nil, nil)` for a profile with no policy |
| `TestSweepNoopNoPolicy` | Call `PolicySweep()` with no policy; assert returns `nil` |
| `TestSweepIdleTimeoutStops` | Insert running row with `last_activity_at = 40m ago` (fake clock), policy idle=30m; assert sweep stops it |
| `TestSweepIdleTimeoutNoAction` | Insert running row idle for 20m, policy idle=30m; assert sweep returns `nil` |
| `TestSweepWarningFired` | Set injected `Clock` to `idle - 1m`; assert warning action in result and `warned_at` set |
| `TestSweepWarningNotDuplicated` | Run sweep twice at warning threshold; assert only one warning event in `sandbox_policy_events` |
| `TestSweepArchive` | Insert `status='stopped'` row `created_at = 16d ago`, policy archive=14d; assert `status='archived'` |
| `TestSweepArchiveSkipsRunning` | Running sandbox older than archive threshold is not archived |
| `TestConcurrencyGateBlocks` | Insert 3 running rows, policy max_concurrent=3; assert `errors.As` matches `*PolicyViolationError` with `Code=="concurrency_limit"` |
| `TestConcurrencyGateAllows` | Insert 2 running rows, policy max_concurrent=3; assert no error |
| `TestCostGateBlocks` | Insert rows summing to `cost_usd = 5.10` in last 24h, policy max_cost_daily=5.00; assert `Code=="cost_cap"` |
| `TestCostGateAllows` | Insert rows summing to `cost_usd = 4.99`, policy max_cost_daily=5.00; assert no error |
| `TestSweepDryRun` | Dry-run sweep; assert actions returned but `sandbox_runs.status` unchanged, no events, and the fake `Reaper` never called |
| `TestSweepStopFailureContinues` | Fake `Reaper` returns an error for sandbox 1; assert sandbox 2 is still processed and a `stop_failed` event recorded |
| `TestPolicyEventsRecorded` | After sweep stops 2 sandboxes; assert 2 rows in `sandbox_policy_events` with correct `reason`/`action` |

### 11.2 Integration Tests (`internal/cli/sandbox_policy_test.go`)

| Test | Description |
|------|-------------|
| `TestCmdPolicySetShowRoundtrip` | Run `set` then `show --json` via `cmd.SetArgs`; `json.Unmarshal` output and assert fields match input |
| `TestCmdPolicyApplyNow` | Create 2 idle sandboxes, set policy, run `apply --now`, assert both stopped in DB |
| `TestCmdPolicyUnsetRemovesRow` | Set policy, unset it; assert `GetPolicy()` returns nil and the gocron job is deregistered |
| `TestCronRegistration` | After `SetPolicy()`, assert `sched.Jobs()` contains a job named `sandbox-policy-sweep` |
| `TestCronDeregistration` | After `DeletePolicy()` for the last profile, assert the job is gone |
| `TestSchemaMigrationIdempotent` | Call `EnsureSchema()` twice on the same DB; assert no error and no duplicate columns |
| `TestPolicyListShowsAll` | Create 3 policies for different profiles; assert all 3 appear in `ListPolicies()` output |
| `TestHistoryQuery` | Insert 5 events, query history with `since=3d`; assert the correct subset returned |

### 11.3 Performance Tests (`internal/sandbox/policy_bench_test.go`)

| Test | Threshold |
|------|-----------|
| `BenchmarkSweep1000Sandboxes` | Populate 1 000 `sandbox_runs` rows (500 running, 300 stopped, 200 archived); benchmark `PolicySweep()`; assert `NsPerOp` implies p99 < 200 ms |
| `BenchmarkConcurrencyGate` | Call `checkPolicyGates()` in a `b.N` loop; assert < 1 ms per call |
| `BenchmarkSchemaMigration` | Call `EnsureSchema()` 100 times on an existing DB; assert total time < 100 ms |

---

## 12. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|--------------|
| AC-01 | `tag sandbox policy set --idle-timeout 30m` creates a policy row with `idle_timeout_minutes = 30` in `sandbox_policies`. | `SELECT idle_timeout_minutes FROM sandbox_policies WHERE profile = ?` returns 30. |
| AC-02 | Running sandbox idle for more than `idle_timeout_minutes` is set to `status = 'stopped'` within 60 seconds of the next cron tick. | Integration test: insert idle sandbox, advance clock, run sweep, assert status. |
| AC-03 | A pre-stop warning notification is emitted between 60 s and 120 s before the idle-timeout stop action. | Unit test with an injected `Clock` and a fake `Notifier`; assert call timing. |
| AC-04 | `tag sandbox policy set --max-concurrent 3` causes the 4th concurrent `RunInSandbox()` call to return a `*PolicyViolationError` with `Code == "concurrency_limit"` before any Docker client call. | Unit test: seed 3 running rows, call `checkPolicyGates()`, assert `errors.As` before the fake backend's launch method. |
| AC-05 | `tag sandbox policy set --max-cost-daily 5.00` causes `RunInSandbox()` to return a `*PolicyViolationError` with `Code == "cost_cap"` when the rolling 24h `SUM(cost_usd)` for the profile meets or exceeds 5.00. | Unit test: insert cost rows summing to 5.01, call gate, assert `errors.As`. |
| AC-06 | `tag sandbox policy apply --now --dry-run` prints the list of actions that would be taken without modifying any `sandbox_runs` row or inserting any `sandbox_policy_events` row. | Integration test: assert DB unchanged and fake `Reaper` uncalled after dry-run. |
| AC-07 | Stopped sandboxes older than `archive_after_days` have `status = 'archived'` after a sweep. | Integration test: insert stopped row with `created_at = archive_after_days + 1d ago`, run sweep, assert status. |
| AC-08 | `tag sandbox policy unset` removes the policy row; subsequent `RunInSandbox()` calls under that profile are not blocked by any gate. | Integration test: set policy, unset, call `checkPolicyGates()`, assert nil error. |
| AC-09 | `tag sandbox policy show --json` returns valid JSON with all policy fields and live state fields without error. | `json.Unmarshal` succeeds on stdout; assert required keys present. |
| AC-10 | All policy enforcement actions appear in `sandbox_policy_events` with correct `action`, `reason`, and `sandbox_id`. | After sweep, `SELECT COUNT(*) FROM sandbox_policy_events WHERE policy_id = ?` matches expected action count. |
| AC-11 | Sandboxes created under a profile with no active policy are not affected by sweep execution. | Insert sandboxes under `profile = 'no-policy'`; call `PolicySweep(ctx, "no-policy", false)`; assert returns `nil`. |
| AC-12 | `EnsureSchema()` called on a DB that already has the new columns does not return an error. | Call twice; assert the `duplicate column name` case is swallowed and `err == nil`. |
| AC-13 | The gocron job `sandbox-policy-sweep` is registered after the first `SetPolicy()` call and removed after the last `DeletePolicy()` call. | Assert presence/absence in `sched.Jobs()` by job name. |
| AC-14 | `tag sandbox policy history --since 7d` returns only events with `timestamp >= clock.Now() - 7d`. | Insert events at `now - 3d` and `now - 10d`; assert only one returned. |
| AC-15 | `PolicySweep()` completes in under 200 ms for 1 000 candidate rows (benchmark test). | `go test -bench`; assert `NsPerOp` implies < 200 ms. |

---

## 13. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| PRD-028 (Sandbox Code Execution) | Blocking | `sandbox_runs` table, `RunInSandbox()` (`internal/sandbox`), backend abstractions must exist |
| PRD-091 (Configurable Sandbox TTL) | Blocking | `last_activity_at` column may overlap; coordinate schema to avoid conflicts |
| PRD-022 (Cron Scheduled Agents) | Blocking | `internal/cron` (`go-co-op/gocron v2`) required for automated sweep registration |
| PRD-040 (Notification Hooks) | Soft | Pre-stop warning notifications use the `internal/notify` `Notifier`; degrades gracefully if a no-op notifier is injected |
| PRD-013 (Agent Tracing) | Soft | Sweep actions emit OTel spans via `internal/obs` (`go.opentelemetry.io/otel`) if a tracer is configured; no-op otherwise |
| PRD-012 / PRD-039 (Budget) | Informational | Cost cap is independent of the token budget; both coexist without conflict |
| Go stdlib `sync` | Stdlib | `*sync.Mutex` per profile; no third-party dependency |
| Go stdlib `regexp` / `strconv` | Stdlib | Duration parser |
| `go-co-op/gocron v2` | Runtime | Singleton one-minute sweep job (replaces apscheduler/threading.Timer) |
| `docker/moby` client / `firecracker-go-sdk` | Runtime | Backend teardown (`ContainerStop`/`ContainerRemove`, microVM `Shutdown`) |
| `modernc.org/sqlite` (`internal/store`) | Runtime | Pure-Go, CGO_ENABLED=0, WAL, `CREATE INDEX IF NOT EXISTS` |

---

## 14. Open Questions

| # | Question | Owner | Target Resolution |
|---|----------|-------|-------------------|
| OQ-1 | Should `cost_usd` be populated automatically by `RunInSandbox()` based on cloud provider metadata (or the PRD-099 per-second attribution), or should callers update the column after a run completes? Modal and E2B expose per-run cost in their responses; the Docker backend has no billing signal. | @sandbox-lead | Before implementation starts |
| OQ-2 | Should the sweep be a pull (cron calls sweep function) or a push (sandbox creation registers a per-sandbox timer)? The cron approach has 60-second granularity but zero idle overhead; per-sandbox timers are more precise but add state management complexity. Current design uses cron. | @infra | Design review |
| OQ-3 | The concurrency gate reads `status = 'running'` which is set synchronously at INSERT time. If two `RunInSandbox()` goroutines race before either updates status, the gate may allow both through. Is a SQLite partial `UNIQUE` index on `(profile) WHERE status='running'` feasible, or should we accept the race at the current concurrency level (mitigated by the single-writer daemon)? | @db | Before implementation |
| OQ-4 | Should `tag sandbox policy set` validate that the active profile exists in the TAG config before creating a policy? If a user typos the profile name, the policy silently does nothing. | @ux | Before implementation |
| OQ-5 | For the Docker backend, `stopSandbox()` calls the moby client `ContainerStop(ctx, containerID, …)`. The `container_id` must be stored in `sandbox_runs`, but PRD-028's schema only stores the image name and run parameters, not the container ID. Should we add a `container_id` column now, or resolve it via the moby `ContainerList` filter `label=tag.run_id=<id>`? | @sandbox-lead | Schema review |
| OQ-6 | Should `archive_after_days` also trigger pruning of Docker images that are no longer referenced by any non-archived sandbox? Image pruning is disk-impactful and could be disruptive on shared Docker installations. | @platform | Before implementation |
| OQ-7 | Is the 60-second sweep interval (one cron tick per minute) acceptable for idle-timeout durations as short as 5 minutes? The worst-case latency is idle_timeout + 60s. If users want sub-minute accuracy, we would need a higher-frequency mechanism. | @product | Spec review |

---

## 15. Complexity and Timeline

**Overall estimate:** S (3–5 engineering days)

### Phase 1 — Schema and Core Data Layer (Day 1)

- Add the `EnsureSchema()` extension for `sandbox_policies`, `sandbox_policy_events`, and the `sandbox_runs` migration columns (via the `addColumn` guard) in `internal/sandbox/policy.go`
- Implement the `SandboxPolicy` struct and `PolicyViolationError`
- Implement `parseDurationMinutes()` (returning `(*int, error)`) with full table-driven coverage
- Implement `SetPolicy()`, `GetPolicy()`, `DeletePolicy()`, `ListPolicies()`
- Write unit tests for all data-layer functions

**Exit criterion:** All data-layer unit tests pass; `sandbox_policies` table created correctly on a fresh `modernc.org/sqlite` DB.

### Phase 2 — Sweep Engine and Gates (Days 2–3)

- Implement `PolicySweep(ctx, profile, dryRun)` with idle-timeout, archive, and cost-cap enforcement (per-profile `*sync.Mutex`)
- Implement `checkPolicyGates()` (concurrency and cost gates called from `RunInSandbox()`)
- Implement `fireWarning()` against the injected `notify.Notifier`
- Implement `stopSandbox()` / the `Reaper` interface with docker (moby `ContainerStop`/`ContainerRemove`), firecracker (`Shutdown`/`StopVMM`), modal (HTTP terminate), and restricted (process-group kill) handlers
- Register/deregister the `sandbox-policy-sweep` `go-co-op/gocron v2` job in `internal/cron`
- Write unit tests for sweep logic (idle, archive, cost) and gate logic with a fake `Clock`/`Reaper`

**Exit criterion:** `PolicySweep()` correctly stops/archives sandboxes in unit tests; gates return the violating error; dry-run leaves DB and `Reaper` untouched.

### Phase 3 — CLI Commands and Scheduler Integration (Day 4)

- Implement the `set`, `show`, `apply`, `unset`, `list`, `history` `cobra` subcommands in `internal/cli/sandbox_policy.go`
- Wire `PolicySweepAllProfiles()` as the gocron job task closure (no sentinel command string)
- Add `Profile` propagation to `RunInSandbox()` callers that do not yet pass it
- Write integration tests for CLI round-trips (`cmd.SetArgs` + captured output)

**Exit criterion:** All six CLI subcommands produce correct output; integration tests pass end-to-end.

### Phase 4 — Testing, Benchmarks, and Documentation (Day 5)

- Run the 1 000-row sweep benchmark (`go test -bench`); optimise if needed (batch SELECT, indexed scan)
- Add OTel span emission for sweep actions via `internal/obs`
- Verify backward compatibility: existing `sandbox_runs` rows with null columns handled gracefully
- Update `docs/prd/INDEX.md` with the PRD-100 entry
- Final review and cleanup

**Exit criterion:** All acceptance criteria verified; benchmark under 200 ms; no regressions in existing sandbox tests.

