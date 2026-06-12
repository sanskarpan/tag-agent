# PRD-039: Token Budget Enforcement (`tag budget`)

**Status:** Proposed
**PRD Number:** 039
**Category:** Core
**Priority:** P1 High
**Estimated Effort:** S (3–5 days)
**Affects:** `controller.py` (new `budget` subcommands + pre-run check), new `src/tag/budget.py`, `tag.sqlite3` schema migration
**Dependencies:** PRD-012 (cost tracking — reads token counts from it)
**Integrates with:** PRD-021 (autonomous loop — primary runaway risk), PRD-040 (notification hooks — warning delivery)

---

## 1. Overview

A single TAG agent invocation operating in autonomous mode (PRD-021) can iterate through dozens of turns, each consuming tens of thousands of tokens. Without a hard ceiling, a misconfigured loop goal, a prompt injection that prevents goal-completion signaling, or a simple overnight forget can exhaust hundreds of dollars of API budget before the user wakes up.

PRD-012 introduced per-run token counting and cost estimation — but it is a recording system, not an enforcement system. It shows you what happened; it does not stop what is about to happen.

This PRD adds **token budget enforcement**: configurable per-profile hard limits on token consumption over a rolling time window (daily / weekly / monthly), enforced as a pre-run gate in `controller.py` before every agent invocation. When a profile is at 80% of its budget, a soft warning is emitted. When it reaches 100%, the invocation is rejected with a clear, actionable error. Budgets reset automatically based on a configurable cadence.

---

## 2. Problem Statement

### 2.1 The Runaway Agent Scenario

A developer sets up a loop:

```bash
tag loop --profile researcher --goal "survey all arxiv papers on diffusion models since 2022"
```

The researcher profile uses `claude-opus-4` at ~$15 / MTok input. The loop has no `--max-turns` flag set. The goal is open-ended. By morning, the loop has run 200 turns, consumed 4 million tokens, and charged $60 to the user's OpenRouter account.

There is currently no mechanism in TAG to have prevented this.

### 2.2 Why PRD-012 Alone Is Insufficient

PRD-012 records `prompt_tokens` and `completion_tokens` in the `runs` table after each run completes. It surfaces these as `tag costs`. It supports a `budget_limit_usd` soft warning in the profile config. But:

- The `budget_limit_usd` warning is advisory — it doesn't stop execution.
- Cost-based limits are approximate (model pricing can change; PRD-012 estimates are best-effort).
- Token-based limits are exact and provider-independent — a token is a token regardless of model price.
- PRD-012 records costs after a run; there is no check before a run starts that asks "will this run push the profile over budget?"

### 2.3 Problem Scope

| Scenario | Current behavior | Desired behavior |
|----------|-----------------|------------------|
| Autonomous loop runs 200 turns overnight | Unchecked; full cost incurred | Hard stop at budget; user notified |
| Developer profile hits daily limit mid-afternoon | No warning; continues | 80% warning at turn N; hard stop at 100% |
| Team shares a TAG install; one user's profile runs wild | Other profiles unaffected but no protection for the offending profile | Per-profile cap prevents single-profile runaway |
| Budget resets expected at midnight | No reset mechanism | Configurable reset cadence with auto-reset |

---

## 3. Goals

1. **Hard pre-run token gate**: before every `tag run` / `tag submit` / `tag loop` invocation, check whether the profile has remaining budget for the current period. Reject if at or over limit.
2. **Soft warning at 80%**: emit a visible warning (and optionally a desktop notification via PRD-040) when the profile has consumed 80% of its budget in the current period, but allow the run to proceed.
3. **Configurable budget levels**: `tag budget set --profile NAME --daily 100000` (tokens, not dollars). Also support `--weekly` and `--monthly` cadences.
4. **`tag budget` CLI**: full CRUD — `set`, `get`, `reset`, `status` (all profiles).
5. **Automatic period reset**: when `reset_at` (stored in the `budgets` table) is in the past, atomically reset `used_tokens = 0` and advance `reset_at` before the pre-run check.
6. **Graceful over-budget error**: returns exit code 1 with a human-readable message including tokens used, limit, reset time, and the command to reset manually.
7. **Integration with PRD-012 token counting**: reads `used_tokens` from the `runs` table (aggregated by profile + period) rather than maintaining a separate counter, to stay in sync with PRD-012's records. The `budgets` table stores the limit and reset schedule only.
8. **`tag budget reset --profile NAME`** allows a manual reset for operators who need to override the schedule.

---

## 4. Non-Goals

1. Dollar/cost-based budgets — those live in PRD-012's `budget_limit_usd` field. This PRD is token-based only, which is exact and model-agnostic.
2. Cross-profile aggregate budgets ("total for all profiles combined").
3. Per-run token limits (separate from cumulative budget) — `--max-tokens` per invocation is an agent-level concern for a future PRD.
4. Remote budget enforcement (enforcing budgets across multiple machines sharing a profile).
5. Budget alerts via email / Slack / webhook — delivery mechanisms belong to PRD-040. This PRD only triggers the notification hook.
6. Billing integration or automatic payment pause.

---

## 5. Success Metrics

| Metric | Target |
|--------|--------|
| Pre-run budget check latency | < 10 ms (single SQLite query) |
| Zero runs proceed when profile is over-budget | 100% gate fidelity in tests |
| 80% warning is emitted exactly once per period per profile | No duplicate warnings per period |
| Budget reset is idempotent (multiple concurrent calls safe) | Passes concurrent-reset test |
| `tag budget status` renders all profiles in < 100 ms | SQLite aggregation performance |
| User can set, check, and reset a budget in under 60 seconds | Measured in UX walkthrough |

---

## 6. User Stories

### US-01: Developer Sets a Daily Budget
**As a** developer who uses the `coder` profile heavily,
**I want** to run `tag budget set --profile coder --daily 100000`,
**so that** the profile can't consume more than 100k tokens per day regardless of how many loops I start.

**Acceptance Criteria:**
- The command writes a row to the `budgets` table with `period = "daily"`, `limit_tokens = 100000`, `reset_at = tomorrow midnight UTC`.
- `tag budget get --profile coder` shows: period, limit, used today, remaining, reset time.
- If the profile has no prior budget, creating one does not affect in-flight runs (enforcement is only prospective).

### US-02: Pre-run Budget Gate Blocks Over-Budget Invocation
**As a** developer whose `researcher` profile has consumed 100,000 / 100,000 tokens today,
**I want** `tag run --profile researcher "summarize this doc"` to immediately fail with a clear message,
**so that** no tokens are consumed and I know exactly what happened.

**Acceptance Criteria:**
- Exit code 1 with message including: profile name, tokens used, limit, cadence, reset_at timestamp.
- No Hermes process is spawned.
- `tag budget reset --profile researcher` succeeds and the subsequent run proceeds.

### US-03: 80% Warning During Loop
**As a** developer running a long autonomous loop,
**I want** to receive a warning when the `researcher` profile crosses 80,000 / 100,000 tokens,
**so that** I can decide whether to let the loop continue or abort it.

**Acceptance Criteria:**
- Warning is printed to stderr: `[budget] researcher: 80,000 / 100,000 tokens used (80%) — resets 2026-06-13 00:00 UTC`.
- If PRD-040 notification hooks are configured, a desktop/webhook notification is also sent.
- The warning fires at most once per period (stored in `budgets.warned_at`; not re-sent if already warned this period).
- The loop continues after the warning — it is not a hard stop.

### US-04: Team Lead Checks All Profile Budgets
**As a** team lead,
**I want** to run `tag budget status`,
**so that** I see a table of all profiles with configured budgets, their consumption this period, and reset times.

**Acceptance Criteria:**
- Table shows: profile, period, used_tokens, limit_tokens, % used (bar), reset_at.
- Profiles with no configured budget are omitted unless `--all` is passed.
- Profiles at or over 80% are highlighted in yellow/red.

### US-05: Weekly Budget for a Batch Profile
**As a** developer running weekly batch analysis,
**I want** to run `tag budget set --profile batch-analyzer --weekly 5000000`,
**so that** the batch profile can consume up to 5M tokens per week but no more.

**Acceptance Criteria:**
- `reset_at` is set to the next Monday 00:00 UTC from the time of the `set` command.
- After 7 days, the first call to `_check_and_reset_budget` advances `reset_at` by 7 days and sets `used_tokens = 0`.

### US-06: Budget Survives TAG Restart
**As a** developer who restarts their machine mid-day,
**I want** budget consumption to persist correctly across TAG restarts,
**so that** restarting TAG doesn't reset my daily counter.

**Acceptance Criteria:**
- `used_tokens` is computed from the `runs` table (PRD-012) by aggregating token counts for the profile in the current period, not from an in-memory counter.
- Even if the `budgets` table `used_tokens` cache diverges, the pre-run check re-derives the authoritative count from `runs`.

---

## 7. Technical Design

### 7.1 SQLite Schema

#### 7.1.1 New `budgets` Table

```sql
CREATE TABLE IF NOT EXISTS budgets (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    profile       TEXT    NOT NULL UNIQUE,
    period        TEXT    NOT NULL CHECK(period IN ('daily', 'weekly', 'monthly')),
    limit_tokens  INTEGER NOT NULL CHECK(limit_tokens > 0),
    used_tokens   INTEGER NOT NULL DEFAULT 0,   -- cached counter; authoritative source is runs table
    reset_at      TEXT    NOT NULL,             -- ISO-8601 UTC timestamp of next period reset
    warned_at     TEXT,                         -- ISO-8601 UTC; NULL if no warning sent this period
    created_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
```

**Schema design notes:**

- `UNIQUE` on `profile` means one budget row per profile. Multiple cadences per profile (e.g., both daily and monthly) are not supported in v1; this avoids ambiguity about which limit fires first.
- `used_tokens` is a cached counter updated after each successful run via `_record_token_usage`. The authoritative figure is derived from the `runs` table when the cache is suspect (e.g., after a manual reset or after detecting a divergence > 5%).
- `warned_at` is set when the 80% warning fires. It is cleared (set to NULL) when the period resets, allowing the warning to fire again in the new period.
- `reset_at` is stored as ISO-8601 UTC string for readability and portability; compared with `datetime.utcnow()` in Python.

#### 7.1.2 Schema Migration

The `budgets` table is created via the standard `_ensure_schema()` function in `controller.py`. No migration tooling is required because the table is new — `CREATE TABLE IF NOT EXISTS` is idempotent.

```python
# In _ensure_schema() in controller.py, append:
conn.execute("""
    CREATE TABLE IF NOT EXISTS budgets (
        id           INTEGER PRIMARY KEY AUTOINCREMENT,
        profile      TEXT    NOT NULL UNIQUE,
        period       TEXT    NOT NULL CHECK(period IN ('daily','weekly','monthly')),
        limit_tokens INTEGER NOT NULL CHECK(limit_tokens > 0),
        used_tokens  INTEGER NOT NULL DEFAULT 0,
        reset_at     TEXT    NOT NULL,
        warned_at    TEXT,
        created_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now')),
        updated_at   TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ','now'))
    )
""")
```

### 7.2 New Module: `src/tag/budget.py`

All budget logic is isolated in a dedicated module to keep `controller.py` focused on CLI routing. The module depends only on the Python standard library plus `sqlite3`.

#### 7.2.1 Core Functions

```python
from __future__ import annotations
import datetime
import sqlite3
from dataclasses import dataclass
from pathlib import Path
from typing import Optional


@dataclass
class BudgetStatus:
    profile: str
    period: str                   # 'daily' | 'weekly' | 'monthly'
    limit_tokens: int
    used_tokens: int              # authoritative, from runs table
    reset_at: datetime.datetime   # UTC
    warned_at: Optional[datetime.datetime]

    @property
    def remaining_tokens(self) -> int:
        return max(0, self.limit_tokens - self.used_tokens)

    @property
    def pct_used(self) -> float:
        if self.limit_tokens == 0:
            return 0.0
        return self.used_tokens / self.limit_tokens

    @property
    def over_budget(self) -> bool:
        return self.used_tokens >= self.limit_tokens

    @property
    def warn_threshold_crossed(self) -> bool:
        """True if >= 80% used and no warning sent this period."""
        return self.pct_used >= 0.80 and self.warned_at is None


def _next_reset_at(period: str, from_dt: datetime.datetime) -> datetime.datetime:
    """Compute the next reset timestamp for a given period, relative to from_dt (UTC)."""
    if period == "daily":
        next_day = from_dt.date() + datetime.timedelta(days=1)
        return datetime.datetime(next_day.year, next_day.month, next_day.day, tzinfo=datetime.timezone.utc)
    elif period == "weekly":
        # Reset on next Monday 00:00 UTC
        days_until_monday = (7 - from_dt.weekday()) % 7 or 7
        next_monday = from_dt.date() + datetime.timedelta(days=days_until_monday)
        return datetime.datetime(next_monday.year, next_monday.month, next_monday.day, tzinfo=datetime.timezone.utc)
    elif period == "monthly":
        if from_dt.month == 12:
            return datetime.datetime(from_dt.year + 1, 1, 1, tzinfo=datetime.timezone.utc)
        return datetime.datetime(from_dt.year, from_dt.month + 1, 1, tzinfo=datetime.timezone.utc)
    else:
        raise ValueError(f"Unknown period: {period!r}")


def _authoritative_used_tokens(conn: sqlite3.Connection, profile: str, period_start: datetime.datetime) -> int:
    """
    Compute used_tokens for the current period by summing from the runs table.
    This is the authoritative source; the budgets.used_tokens column is a cache.
    Falls back to 0 if the runs table doesn't have token columns yet (pre-PRD-012).
    """
    try:
        row = conn.execute(
            """
            SELECT COALESCE(SUM(prompt_tokens + completion_tokens), 0)
            FROM runs
            WHERE profile = ?
              AND started_at >= ?
            """,
            (profile, period_start.isoformat()),
        ).fetchone()
        return int(row[0]) if row else 0
    except sqlite3.OperationalError:
        # runs table doesn't have token columns; return cached value
        return 0


def set_budget(conn: sqlite3.Connection, profile: str, period: str, limit_tokens: int) -> BudgetStatus:
    now = datetime.datetime.now(datetime.timezone.utc)
    reset_at = _next_reset_at(period, now)
    conn.execute(
        """
        INSERT INTO budgets (profile, period, limit_tokens, reset_at, used_tokens, warned_at, updated_at)
        VALUES (?, ?, ?, ?, 0, NULL, strftime('%Y-%m-%dT%H:%M:%SZ','now'))
        ON CONFLICT(profile) DO UPDATE SET
            period       = excluded.period,
            limit_tokens = excluded.limit_tokens,
            reset_at     = excluded.reset_at,
            used_tokens  = 0,
            warned_at    = NULL,
            updated_at   = strftime('%Y-%m-%dT%H:%M:%SZ','now')
        """,
        (profile, period, limit_tokens, reset_at.isoformat()),
    )
    conn.commit()
    return get_budget(conn, profile)


def get_budget(conn: sqlite3.Connection, profile: str) -> Optional[BudgetStatus]:
    row = conn.execute(
        "SELECT profile, period, limit_tokens, used_tokens, reset_at, warned_at FROM budgets WHERE profile = ?",
        (profile,),
    ).fetchone()
    if not row:
        return None
    reset_at = datetime.datetime.fromisoformat(row[4]).replace(tzinfo=datetime.timezone.utc)
    warned_at = (
        datetime.datetime.fromisoformat(row[5]).replace(tzinfo=datetime.timezone.utc)
        if row[5] else None
    )
    # Recompute used_tokens authoritatively
    period_start = _period_start(row[1], reset_at)
    authoritative_used = _authoritative_used_tokens(conn, profile, period_start)
    return BudgetStatus(
        profile=row[0],
        period=row[1],
        limit_tokens=row[2],
        used_tokens=authoritative_used,
        reset_at=reset_at,
        warned_at=warned_at,
    )


def _period_start(period: str, reset_at: datetime.datetime) -> datetime.datetime:
    """Work backwards from reset_at to find the start of the current period."""
    if period == "daily":
        return reset_at - datetime.timedelta(days=1)
    elif period == "weekly":
        return reset_at - datetime.timedelta(weeks=1)
    elif period == "monthly":
        # Back-compute: reset_at is 1st of next month; period start is 1st of this month
        if reset_at.month == 1:
            return reset_at.replace(year=reset_at.year - 1, month=12)
        return reset_at.replace(month=reset_at.month - 1)
    raise ValueError(f"Unknown period: {period!r}")


def check_and_reset_if_due(conn: sqlite3.Connection, profile: str) -> Optional[BudgetStatus]:
    """
    Check if the budget period has elapsed and reset if so.
    Returns the current BudgetStatus (post-reset if reset occurred), or None if no budget configured.
    This function is safe to call concurrently: uses a single UPDATE ... WHERE to do atomic reset.
    """
    now = datetime.datetime.now(datetime.timezone.utc)
    # Atomic reset: only update if reset_at <= now, to prevent double-reset in concurrent calls
    conn.execute(
        """
        UPDATE budgets
        SET used_tokens = 0,
            warned_at   = NULL,
            reset_at    = CASE period
                WHEN 'daily'   THEN datetime(reset_at, '+1 day')
                WHEN 'weekly'  THEN datetime(reset_at, '+7 days')
                WHEN 'monthly' THEN datetime(reset_at, '+1 month')
            END,
            updated_at  = strftime('%Y-%m-%dT%H:%M:%SZ','now')
        WHERE profile = ?
          AND reset_at <= strftime('%Y-%m-%dT%H:%M:%SZ','now')
        """,
        (profile,),
    )
    conn.commit()
    return get_budget(conn, profile)


def record_token_usage(conn: sqlite3.Connection, profile: str, tokens_used: int) -> None:
    """
    Increment the cached used_tokens counter.
    The authoritative value is always derived from the runs table; this is a cache for performance.
    """
    conn.execute(
        """
        UPDATE budgets
        SET used_tokens = used_tokens + ?,
            updated_at  = strftime('%Y-%m-%dT%H:%M:%SZ','now')
        WHERE profile = ?
        """,
        (tokens_used, profile),
    )
    conn.commit()


def mark_warned(conn: sqlite3.Connection, profile: str) -> None:
    conn.execute(
        "UPDATE budgets SET warned_at = strftime('%Y-%m-%dT%H:%M:%SZ','now'), updated_at = strftime('%Y-%m-%dT%H:%M:%SZ','now') WHERE profile = ?",
        (profile,),
    )
    conn.commit()


def reset_budget(conn: sqlite3.Connection, profile: str) -> Optional[BudgetStatus]:
    """Manual reset: zero used_tokens, clear warned_at, advance reset_at by one period."""
    conn.execute(
        """
        UPDATE budgets
        SET used_tokens = 0,
            warned_at   = NULL,
            reset_at    = CASE period
                WHEN 'daily'   THEN datetime(reset_at, '+1 day')
                WHEN 'weekly'  THEN datetime(reset_at, '+7 days')
                WHEN 'monthly' THEN datetime(reset_at, '+1 month')
            END,
            updated_at  = strftime('%Y-%m-%dT%H:%M:%SZ','now')
        WHERE profile = ?
        """,
        (profile,),
    )
    conn.commit()
    return get_budget(conn, profile)


def delete_budget(conn: sqlite3.Connection, profile: str) -> bool:
    """Remove a budget configuration entirely. Returns True if a row was deleted."""
    cursor = conn.execute("DELETE FROM budgets WHERE profile = ?", (profile,))
    conn.commit()
    return cursor.rowcount > 0


def list_budgets(conn: sqlite3.Connection) -> list[BudgetStatus]:
    rows = conn.execute(
        "SELECT profile FROM budgets ORDER BY profile"
    ).fetchall()
    statuses = []
    for (profile,) in rows:
        status = check_and_reset_if_due(conn, profile)
        if status:
            statuses.append(status)
    return statuses
```

### 7.3 Pre-run Budget Check Algorithm

The pre-run check is a single function called from every agent-dispatching path in `controller.py` (`cmd_run`, `cmd_submit`, `cmd_loop`, `cmd_benchmark`).

```python
# In controller.py (or imported from budget.py)

def enforce_budget(conn: sqlite3.Connection, profile: str) -> None:
    """
    Pre-run gate. Call before any agent invocation.
    - If no budget configured: returns immediately (no budget = no limit).
    - If period has elapsed: resets automatically, returns.
    - If at or over budget: raises SystemExit(1) with clear message.
    - If >= 80% used: prints warning to stderr, continues.
    Raises: SystemExit with exit code 1 if over budget.
    """
    from tag.budget import check_and_reset_if_due, mark_warned

    status = check_and_reset_if_due(conn, profile)
    if status is None:
        return  # no budget configured; allow run

    if status.over_budget:
        raise SystemExit(
            f"\n[budget] Profile '{profile}' is over its {status.period} token budget.\n"
            f"  Used:    {status.used_tokens:,} tokens\n"
            f"  Limit:   {status.limit_tokens:,} tokens\n"
            f"  Resets:  {status.reset_at.strftime('%Y-%m-%d %H:%M UTC')}\n"
            f"\nTo reset manually: tag budget reset --profile {profile}\n"
            f"To increase limit: tag budget set --profile {profile} --{status.period} <new_limit>\n"
        )

    if status.warn_threshold_crossed:
        pct = int(status.pct_used * 100)
        print(
            f"[budget] WARNING: {profile}: {status.used_tokens:,} / {status.limit_tokens:,} "
            f"tokens used ({pct}%) — resets {status.reset_at.strftime('%Y-%m-%d %H:%M UTC')}",
            file=sys.stderr,
        )
        mark_warned(conn, profile)
        # Optionally trigger PRD-040 notification hook here
        _maybe_send_budget_notification(profile, status)
```

**Call sites in `controller.py`:**
- `cmd_run`: immediately before the Hermes subprocess is spawned
- `cmd_submit`: immediately before the benchmark job is dispatched
- `cmd_loop` (PRD-021): at the start of each loop turn (not just the first), to catch budget exhaustion mid-loop
- `cmd_benchmark`: once at the start, with an estimated-tokens warning if the benchmark corpus is large

**Token recording after a run:**

```python
# After a run completes successfully and token counts are available from PRD-012:
from tag.budget import record_token_usage
tokens_this_run = prompt_tokens + completion_tokens
record_token_usage(conn, profile, tokens_this_run)
```

### 7.4 CLI Surface

All budget commands are grouped under the `budget` subcommand in `controller.py`.

#### `tag budget set`

```
tag budget set --profile NAME (--daily | --weekly | --monthly) TOKENS

Options:
  --profile NAME    Profile to set budget for (required)
  --daily TOKENS    Set daily token limit
  --weekly TOKENS   Set weekly token limit
  --monthly TOKENS  Set monthly token limit

Only one cadence may be active per profile. Setting a new cadence replaces the old one.
The used_tokens counter resets to 0 when a new budget is set.

Example:
  tag budget set --profile coder --daily 100000
  tag budget set --profile researcher --weekly 2000000
```

#### `tag budget get`

```
tag budget get --profile NAME [--json]

Shows the current budget status for a single profile.

Output (default):
  Profile:    coder
  Period:     daily
  Limit:      100,000 tokens
  Used:       42,351 tokens  (42%)
  Remaining:  57,649 tokens
  Resets:     2026-06-13 00:00 UTC
  Warning:    not sent this period

Output (--json): see Section 7.5
```

#### `tag budget reset`

```
tag budget reset --profile NAME [--confirm]

Manually resets used_tokens to 0 and advances reset_at by one period.
Prompts for confirmation unless --confirm is passed.
```

#### `tag budget delete`

```
tag budget delete --profile NAME [--confirm]

Removes the budget configuration for the profile entirely.
After deletion, the profile has no token limit.
```

#### `tag budget status`

```
tag budget status [--all] [--json]

Shows a summary table of all profiles with configured budgets.
--all also shows profiles with no budget (from the profiles directory).

Output (default):
  PROFILE       PERIOD   USED        LIMIT        PCT   RESETS
  coder         daily    42,351      100,000      42%   2026-06-13 00:00
  researcher    weekly   1,234,567   2,000,000    62%   2026-06-16 00:00
  batch         monthly  8,901,234   10,000,000   89%   2026-07-01 00:00  ← WARNING
```

Profiles at >= 80% are highlighted in yellow; at 100% in red (using Rich if available).

### 7.5 JSON Output Format

`tag budget get --json` and `tag budget status --json` return:

```json
{
  "profile": "coder",
  "period": "daily",
  "limit_tokens": 100000,
  "used_tokens": 42351,
  "remaining_tokens": 57649,
  "pct_used": 0.42351,
  "over_budget": false,
  "warn_threshold_crossed": false,
  "reset_at": "2026-06-13T00:00:00Z",
  "warned_at": null
}
```

`tag budget status --json` returns a JSON array of the above objects.

### 7.6 PRD-040 Notification Integration

When a profile crosses the 80% warning threshold, `enforce_budget` optionally calls the notification hook:

```python
def _maybe_send_budget_notification(profile: str, status: BudgetStatus) -> None:
    """Fire PRD-040 notification hook if available."""
    try:
        from tag.notifications import fire_hook  # PRD-040
        fire_hook(
            event="budget.warning",
            payload={
                "profile": profile,
                "period": status.period,
                "used_tokens": status.used_tokens,
                "limit_tokens": status.limit_tokens,
                "pct_used": round(status.pct_used, 4),
                "reset_at": status.reset_at.isoformat(),
            },
        )
    except ImportError:
        pass  # PRD-040 not yet installed; skip silently
    except Exception:
        pass  # Never fail a run because of a notification error
```

---

## 8. Implementation Plan

### Phase 1 — Core Module + Schema (Day 1)

| Task | File | Notes |
|------|------|-------|
| Create `src/tag/budget.py` | new | `BudgetStatus`, `set_budget`, `get_budget`, `check_and_reset_if_due`, `record_token_usage`, `reset_budget`, `list_budgets`, `mark_warned`, `delete_budget` |
| Add `budgets` table to `_ensure_schema()` | `controller.py` | `CREATE TABLE IF NOT EXISTS budgets ...` |
| Unit tests for all budget functions | `tests/test_budget.py` | In-memory SQLite; test each function; concurrent reset test |

### Phase 2 — CLI Commands (Day 2)

| Task | File | Notes |
|------|------|-------|
| `tag budget set` command | `controller.py` | `cmd_budget_set`; argparse routing |
| `tag budget get` command | `controller.py` | `cmd_budget_get`; plain + `--json` |
| `tag budget reset` command | `controller.py` | `cmd_budget_reset`; confirmation prompt |
| `tag budget delete` command | `controller.py` | `cmd_budget_delete`; confirmation prompt |
| `tag budget status` command | `controller.py` | `cmd_budget_status`; Rich table + `--json` |

### Phase 3 — Pre-run Integration (Day 3)

| Task | File | Notes |
|------|------|-------|
| `enforce_budget()` gate in `cmd_run` | `controller.py` | Before Hermes spawn |
| `enforce_budget()` gate in `cmd_submit` | `controller.py` | Before job dispatch |
| `enforce_budget()` gate per-turn in `cmd_loop` (PRD-021) | `controller.py` / `loop.py` | Called at start of each turn |
| `record_token_usage()` after run completes | `controller.py` | Reads token counts from PRD-012 run record |

### Phase 4 — Hardening + Notification (Day 4–5)

| Task | File | Notes |
|------|------|-------|
| Concurrent reset test (two threads calling `check_and_reset_if_due` simultaneously) | `tests/test_budget.py` | Assert no double-reset, no corruption |
| PRD-040 notification hook integration | `controller.py` | `_maybe_send_budget_notification`; graceful failure |
| `tag budget status` Rich table formatting | `controller.py` | Color coding at 80%/100% |
| Add budget status to `tag doctor` output | `controller.py` | Warn if any profile is over budget |
| Integration test: full run-with-budget cycle | `tests/test_budget_integration.py` | Set budget, run mock agent, check counter increments, run over budget, check blocked |
| Update CLI help text and README | `controller.py`, `README.md` | Document `tag budget` subcommand group |

---

## 9. Risks and Mitigations

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| Budget check race condition in concurrent `tag loop` turns | Medium | Low (double counting, not under-counting) | Atomic `UPDATE ... WHERE reset_at <= now` for reset; SQLite WAL mode for concurrent writes |
| Clock skew on daily reset (system timezone vs UTC) | Low | Low | All timestamps stored and compared in UTC explicitly via `datetime.timezone.utc` |
| User confusion: input vs output vs total tokens | High | Medium | `tag budget` help text explicitly states "total tokens = prompt_tokens + completion_tokens"; UI shows breakdown |
| `used_tokens` cache diverges from `runs` table | Medium | Low | `get_budget` always recomputes from `runs` table via `_authoritative_used_tokens`; cache is for fast increment only |
| PRD-012 not yet deployed; `runs` table lacks token columns | Medium | Low | `_authoritative_used_tokens` catches `sqlite3.OperationalError` and falls back to 0 gracefully |
| User sets limit = 1 by mistake; completely locked out | Low | Medium | `tag budget set` warns if limit is suspiciously low (< 1000 tokens); requires `--confirm-low` flag |
| Monthly reset: different month lengths cause off-by-one | Low | Low | Use `+1 month` in SQLite's `datetime()` function, which handles month-length variation correctly |
| Budget enforcement blocks CI pipelines unexpectedly | Low | High | Document that CI pipelines should either configure a budget appropriate for their use or omit a budget entirely; `--no-budget-check` escape hatch for `tag run` in CI contexts |

### 9.1 Concurrency Detail

SQLite in WAL mode supports concurrent readers with a single writer. The `check_and_reset_if_due` function uses a single `UPDATE ... WHERE reset_at <= now` statement. Because SQLite serializes writes, if two loop turns call this simultaneously:
- The first write acquires the write lock, performs the update, and commits.
- The second write acquires the lock after the first commits; at that point `reset_at` has been advanced and `reset_at <= now` is false, so the UPDATE is a no-op.

This is safe without application-level locking.

### 9.2 Token Counting Consistency

The budget module counts `prompt_tokens + completion_tokens` as reported by the model provider via PRD-012. Cached input tokens (Anthropic prompt cache) count at their discounted rate in billing but at their full rate in token counting — this is intentional. Token limits are about context window and usage rate, not cost. Dollar limits for cost control remain in PRD-012.

---

## 10. Open Questions

| # | Question | Owner | Target resolution |
|---|----------|-------|-------------------|
| OQ-1 | Should we support multiple simultaneous cadences per profile (e.g., daily 100k AND monthly 2M)? Adds complexity; blocked by UNIQUE constraint in v1. | @sanskarpan | Post-v1 |
| OQ-2 | Should `tag budget set` accept `--input-only` to count only prompt tokens (not completion tokens) toward the budget? This mirrors Anthropic's billing for cached prompts. | @sanskarpan | Before Phase 1 |
| OQ-3 | Should the 80% warning threshold be configurable (`--warn-at 70`)? | @sanskarpan | Nice-to-have; default 80% is industry standard |
| OQ-4 | Should there be a global (cross-profile) daily token cap as an installation-level safety net? Useful for team installs. | TBD | Separate PRD |
| OQ-5 | How should `tag loop` (PRD-021) handle the case where it's mid-turn when the budget is exhausted? (Current design: check at turn start, so a turn that starts under budget may complete over budget.) | @sanskarpan | Acceptable for v1; per-turn streaming enforcement is a follow-on |
| OQ-6 | Should `--no-budget-check` be a flag on `tag run` for CI contexts? Risk: users abuse it to bypass safety. | @sanskarpan | Before Phase 3 |

---

## 11. Security Considerations

### 11.1 Budget Bypass

The `--no-budget-check` escape hatch (OQ-6) and `tag budget reset` both allow bypassing budget enforcement. These are intentional operator controls. They are not security issues in the threat model (the threat is accidental runaway cost, not adversarial cost attack). However:
- `tag budget reset` requires interactive confirmation by default.
- `--no-budget-check` should be logged to the run record so post-hoc audits can identify bypasses.

### 11.2 Budget Data Is Not Sensitive

The `budgets` table contains token counts and limits — no API keys, no prompts, no personal data. No special access controls are required beyond the existing `~/.tag/tag.sqlite3` file permissions (typically `600`).

### 11.3 Notification Payload

The PRD-040 webhook payload for `budget.warning` events contains token counts but not prompt content. This is safe to send to external webhook endpoints. However, users who configure external webhooks should be aware the payload includes the profile name, which may be organizational metadata.

---

## 12. Acceptance Criteria Summary

- [ ] `tag budget set --profile coder --daily 100000` creates a row in the `budgets` table
- [ ] `tag budget get --profile coder` shows used/limit/remaining/reset_at correctly
- [ ] `tag run --profile coder` is blocked (exit 1, clear message) when `used_tokens >= limit_tokens`
- [ ] 80% warning is printed to stderr when `used_tokens >= 0.80 * limit_tokens` and `warned_at IS NULL`
- [ ] 80% warning fires at most once per period (subsequent runs above 80% are not warned again)
- [ ] `tag budget reset --profile coder` zeros `used_tokens`, clears `warned_at`, advances `reset_at`
- [ ] `tag budget status` renders a table of all profiles with budgets; highlights >= 80% profiles
- [ ] `--json` flag on `get` and `status` outputs valid JSON matching the schema in Section 7.5
- [ ] Daily reset happens automatically on first run after `reset_at` passes (no manual intervention)
- [ ] Concurrent reset calls do not double-reset or corrupt the counter
- [ ] `used_tokens` counter is re-derived from `runs` table on each `get_budget` call (not stale cache)
- [ ] PRD-040 `budget.warning` event fires at 80% if notification hooks are configured
- [ ] `tag budget delete --profile coder` removes the budget; subsequent runs proceed without limit
- [ ] `tag doctor` warns if any profile has `used_tokens >= limit_tokens`
- [ ] `src/tag/budget.py` unit test coverage >= 90% for all public functions
