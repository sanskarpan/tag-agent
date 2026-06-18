# PRD-099: Per-Second Cost Attribution per Sandbox Run (`tag sandbox costs`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** S (3-5 days)
**Category:** Sandbox & Execution Environment
**Affects:** `sandbox.py + cost_table.py` (new file)
**Depends on:** PRD-028 (Sandbox Code Execution), PRD-012 (Cost Tracking & Budget), PRD-013 (Agent Tracing & Observability), PRD-034 (Security), PRD-039 (Token Budget Enforcement)
**GitHub Issue:** #348
**Inspired by:** E2B per-second billing, Modal per-second billing, AWS Lambda per-ms billing

---

## 1. Overview

Modern cloud sandbox providers — E2B, Modal, AWS Lambda — all bill at sub-second granularity. E2B charges per second of microVM wall-clock time. Modal charges per second of GPU or CPU time with a minimum billing increment. Lambda charges per millisecond of invocation time. This billing model creates a tight feedback loop between sandbox runtime behavior and infrastructure cost: a sandbox that hangs for 30 seconds costs 30x more than one that completes in 1 second, and that signal is immediately visible in the cost ledger.

TAG's existing sandbox subsystem (`sandbox.py`, PRD-028) records wall-clock start and end timestamps in `sandbox_runs.created_at` and `sandbox_runs.completed_at`, but performs no cost calculation, attribution, or reporting. The fields needed to derive runtime duration are present; the cost table, pricing model, and reporting surface are absent. Engineers and platform operators who use Docker, E2B, or Modal backends for sandbox execution have no visibility into sandbox-level spend: they cannot answer "how much did this week's sandbox runs cost?", "which backend is cheapest for my workload?", or "did this run exceed the compute budget?".

This PRD specifies `tag sandbox costs` — a reporting subcommand and the underlying `cost_table.py` module that implements per-second cost attribution for every sandbox run. The module reads `sandbox_runs` records, looks up backend-specific per-second pricing from a locally configurable rate table, computes the dollar cost as `duration_seconds × rate_per_second`, and stores the result in a new `sandbox_run_costs` table. The `tag sandbox costs` command surfaces this data as a human-readable table or machine-readable JSON, with filters by time range, run ID, and backend.

The cost attribution integrates with the existing `budget.py` module (PRD-039) via a new `sandbox_cost_budget` concept: operators can set a rolling daily/weekly/monthly dollar cap on sandbox compute spending. When the cap is reached, `run_in_sandbox()` raises `SandboxBudgetExceeded` before launching the next run, preventing runaway compute costs from a looping agent or misconfigured job queue. This closes a gap that PRD-039 acknowledged: token budgets gate LLM API spend, but compute (sandbox) spend has no enforcement mechanism.

Pricing for the `restricted` backend (local subprocess) defaults to $0.00/second because it consumes local compute with no third-party billing. The `docker` backend defaults to a configurable local-compute cost (default: `$0.000028/second`, derived from a `t3.medium` EC2 instance hourly rate divided by 3600 as a reference point for on-premise cost allocation). E2B and Modal rates are seeded from publicly documented pricing at the time of the PRD and are user-overridable in `~/.tag/config.yaml` so that operators with custom tier agreements can reflect their actual contracted rates.

---

## 2. Problem Statement

### 2.1 No Visibility into Sandbox Compute Spend

TAG dispatches sandbox runs through `run_in_sandbox()` in `sandbox.py`. Each run records a start timestamp (`created_at`) and end timestamp (`completed_at`), but the system never computes or persists a cost figure. An agent loop that spawns 200 sandbox runs in a single session (e.g., an autonomous coding agent running tests after every edit) may accumulate $20-$40 of E2B microVM time without any indicator in the CLI output. The operator only discovers this when the cloud billing dashboard catches up — often 24 hours later and with no per-run granularity.

### 2.2 No Sandbox-Level Budget Enforcement

`budget.py` provides token-based hard limits per profile (`max_tokens` over a `daily`/`weekly`/`monthly` window). This gate fires before an LLM API call and prevents overspending on inference. However, sandbox compute spending is entirely unguarded: a job queue agent that exits successfully in zero tokens (e.g., because it only invokes shell commands) can still generate substantial backend compute costs. The `sandbox_budget` column does not exist in `token_budgets`; there is no `check_sandbox_budget()` function; and `run_in_sandbox()` does not call any budget gate before spinning up a backend.

### 2.3 No Cross-Backend Cost Comparison

TAG supports four backends: `restricted`, `docker`, `modal`, and `e2b` (plus planned Daytona and cloud-VM backends per PRD-028). Each backend has a fundamentally different cost structure: `restricted` uses local CPU cycles, `docker` uses local disk and memory, `modal` bills per-second with a 1-second minimum and generous free tier, `e2b` bills per-second from the first millisecond. Operators choosing between backends for a given workload have no data-driven basis for the decision. A report like `--by backend` that shows total spend, average cost per run, and P95 duration per backend would immediately answer the question "is Docker or Modal cheaper for my 30-second test suite jobs?"

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Compute and persist a dollar cost for every completed sandbox run using per-second pricing |
| G2 | Expose `tag sandbox costs` with filters for time range, run ID, and backend; support both human-readable table and `--json` output |
| G3 | Integrate with `budget.py` to support a sandbox compute dollar cap per profile with the same `warn_pct` / hard-limit semantics as token budgets |
| G4 | Ship a default rate table covering all four current backends (`restricted`, `docker`, `modal`, `e2b`) with user-overridable pricing in `~/.tag/config.yaml` |
| G5 | Backfill cost records for all existing `sandbox_runs` rows that have `completed_at` populated but no cost record yet, via a migration function called on first schema access |
| G6 | Add cost fields to `run_in_sandbox()` return dict and to the `tag sandbox list` output so cost appears inline during operation |
| G7 | Zero new mandatory dependencies — pricing lookup and cost computation are pure Python; no external billing API |

---

## 4. Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Real-time cost streaming during execution — costs are attributed post-completion, not while the sandbox is running |
| NG2 | Integration with actual cloud billing APIs (AWS Cost Explorer, Modal billing API, E2B billing API) — all rates are locally configured, not pulled from provider APIs |
| NG3 | Per-CPU or per-memory cost breakdown (like AWS EC2 cost allocation) — a single per-second rate per backend is the model |
| NG4 | Cross-user or multi-tenant cost reporting — this is a single-user CLI; no user ID partitioning |
| NG5 | Invoice generation or export to accounting software (QuickBooks, NetSuite) |
| NG6 | Cost prediction before a run starts (estimated cost based on historical P50 duration) — deferred to a follow-on PRD |
| NG7 | GPU-specific pricing differentiation within Modal (e.g., H100 vs A100) — a single Modal rate per second is used; GPU surcharges require a follow-on |

---

## 5. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| SM-01: Cost record completeness | 100% of sandbox runs with `completed_at != NULL` have a `sandbox_run_costs` row | `SELECT COUNT(*) FROM sandbox_runs WHERE completed_at IS NOT NULL AND id NOT IN (SELECT run_id FROM sandbox_run_costs)` = 0 after migration |
| SM-02: `--json` output contract stability | JSON schema passes jsonschema validation across all CLI invocations | CI schema validation test |
| SM-03: Budget gate fires correctly | `run_in_sandbox()` raises `SandboxBudgetExceeded` within 5ms of a profile hitting its sandbox dollar cap | Unit test with mocked time |
| SM-04: Duration accuracy | Cost record `duration_seconds` differs from `(completed_at - created_at)` by < 1ms | Parameterized unit test with known timestamps |
| SM-05: Backfill migration completes | All pre-existing `sandbox_runs` rows with `completed_at` get cost records on first `ensure_schema()` call | Integration test on a pre-seeded SQLite file |
| SM-06: P95 query latency | `tag sandbox costs --since 30d` completes in < 200ms for 10,000 cost records | Performance test with synthetic data |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|--------|-----------|----------|
| U1 | Platform engineer | run `tag sandbox costs --since 7d --json` | I can pipe the output to a dashboard script and see last week's sandbox compute spend broken down by run |
| U2 | Agent developer | run `tag sandbox costs --run-id abc123def456` | I can inspect the exact cost of one sandbox run while debugging a slow test execution |
| U3 | Team lead | run `tag sandbox costs --by backend --since 30d` | I can make a data-driven backend selection decision, choosing Modal or Docker based on actual cost data rather than guessing |
| U4 | Platform operator | set `sandbox_budget_usd: 5.00` per-profile in `~/.tag/config.yaml` | Autonomous agents are hard-stopped after $5 of sandbox spend in a day, preventing runaway compute bills |
| U5 | Developer | see cost printed inline after every `tag sandbox run` | I get instant cost feedback for every sandbox invocation without running a separate report |
| U6 | FinOps analyst | run `tag sandbox costs --since 90d --by backend --json` | I can calculate the total infrastructure cost for the quarter's sandbox usage across all backends |
| U7 | Operator | define a custom E2B rate in `~/.tag/config.yaml` to match my negotiated pricing | The cost attribution reflects my actual contract, not the default public list price |

---

## 6. Proposed CLI Surface

### 6.1 `tag sandbox costs`

Report costs for sandbox runs.

```
tag sandbox costs \
  [--since <duration>]      # e.g. 1h, 7d, 30d, 90d (default: 7d)
  [--run-id <id>]           # filter to a single run (12-char hex)
  [--by backend]            # aggregate by backend; shows total + avg + p95
  [--profile <name>]        # filter to runs attributed to a specific profile
  [--backend <name>]        # filter to a specific backend: restricted|docker|modal|e2b
  [--min-cost <usd>]        # only show runs costing more than this (e.g. 0.01)
  [--limit <n>]             # max rows in table output (default: 50)
  [--json]                  # machine-readable JSON output
```

**Human-readable table output (default):**

```
$ tag sandbox costs --since 7d

Sandbox Costs — last 7 days (14 runs)

 run_id        backend     duration    cost       status   command
 ─────────────────────────────────────────────────────────────────────────────
 a3f2b91c4e12  docker      12.4s       $0.000347  done     pytest tests/ -x
 9d1e7f3a8b05  e2b         31.2s       $0.004992  done     python bench.py
 c08fa5d22311  modal        8.9s       $0.009768  done     python train.py
 4b7c1209de44  restricted   0.3s       $0.000000  done     echo hello
 ...

 Total: $0.031847  |  Backends: docker(6) e2b(5) modal(2) restricted(1)
 Period: 2026-06-10T00:00:00Z — 2026-06-17T00:00:00Z
```

**`--by backend` aggregate output:**

```
$ tag sandbox costs --since 7d --by backend

Sandbox Costs by Backend — last 7 days

 backend      runs  total_cost   avg_cost   p50_dur   p95_dur   avg_rate
 ──────────────────────────────────────────────────────────────────────────
 e2b             5  $0.01843     $0.00369   28.4s     61.2s     $0.00016/s
 modal           2  $0.01953     $0.00977   9.0s      31.1s     $0.00110/s
 docker          6  $0.00208     $0.00035   11.2s     18.3s     $0.00003/s
 restricted      1  $0.00000     $0.00000    0.3s      0.3s     $0.00000/s
 ──────────────────────────────────────────────────────────────────────────
 TOTAL          14  $0.04004
```

**`--run-id` single-run output:**

```
$ tag sandbox costs --run-id a3f2b91c4e12

Run: a3f2b91c4e12
 Backend:      docker
 Image:        python:3.12-slim
 Command:      pytest tests/ -x
 Status:       done
 Exit Code:    0
 Started:      2026-06-15T14:23:01.441Z
 Completed:    2026-06-15T14:23:13.873Z
 Duration:     12.432s
 Rate:         $0.000028/s  (docker local-compute reference)
 Cost:         $0.000348
 Profile:      coder
```

**`--json` output schema:**

```json
{
  "period": {
    "since": "2026-06-10T00:00:00Z",
    "until": "2026-06-17T00:00:00Z"
  },
  "summary": {
    "total_runs": 14,
    "total_cost_usd": 0.031847,
    "total_duration_seconds": 198.3
  },
  "runs": [
    {
      "run_id": "a3f2b91c4e12",
      "backend": "docker",
      "image": "python:3.12-slim",
      "command": "pytest tests/ -x",
      "status": "done",
      "exit_code": 0,
      "profile": "coder",
      "started_at": "2026-06-15T14:23:01.441Z",
      "completed_at": "2026-06-15T14:23:13.873Z",
      "duration_seconds": 12.432,
      "rate_per_second_usd": 0.000028,
      "cost_usd": 0.000348,
      "billing_model": "per_second",
      "minimum_charge_seconds": 0
    }
  ]
}
```

**`--by backend --json` output schema:**

```json
{
  "period": { "since": "...", "until": "..." },
  "by_backend": [
    {
      "backend": "e2b",
      "runs": 5,
      "total_cost_usd": 0.01843,
      "avg_cost_usd": 0.003686,
      "p50_duration_seconds": 28.4,
      "p95_duration_seconds": 61.2,
      "rate_per_second_usd": 0.00016,
      "minimum_charge_seconds": 1
    }
  ]
}
```

### 6.2 `tag sandbox list` (augmented)

The existing `tag sandbox list` command gains a `cost` column:

```
$ tag sandbox list --limit 5

 id            backend   status  exit  duration   cost      command
 ─────────────────────────────────────────────────────────────────────
 a3f2b91c4e12  docker    done    0     12.4s      $0.00035  pytest tests/ -x
 9d1e7f3a8b05  e2b       done    0     31.2s      $0.00499  python bench.py
 ...
```

### 6.3 `tag sandbox run` (augmented)

After a run completes, cost is printed inline:

```
$ tag sandbox run --backend e2b -- python benchmark.py

[sandbox] Starting e2b run a3f2b91c4e12...
[sandbox] stdout: ...
[sandbox] Completed in 31.2s (exit 0) — cost: $0.004992 (e2b @ $0.00016/s)
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | `sandbox_run_costs` table must be created by `cost_table.ensure_schema(conn)`, called automatically from `sandbox.ensure_schema()` so callers need no changes |
| FR-02 | Every call to `run_in_sandbox()` that produces a `completed_at` timestamp must write a corresponding row to `sandbox_run_costs` before returning |
| FR-03 | Duration must be computed as `(completed_at_epoch - created_at_epoch)` in floating-point seconds using `datetime.fromisoformat()` on both ISO timestamp strings |
| FR-04 | Cost must be computed as `max(duration_seconds, minimum_charge_seconds) * rate_per_second_usd` where `minimum_charge_seconds` is backend-specific (E2B: 1, Modal: 1, Docker: 0, restricted: 0) |
| FR-05 | Backend rate lookup must first check `cli_config["sandbox"]["pricing"][backend]` in `~/.tag/config.yaml`, falling back to the hardcoded defaults in `cost_table.BACKEND_RATES` |
| FR-06 | `BACKEND_RATES` must define entries for `restricted` ($0.000000/s), `docker` ($0.000028/s), `modal` ($0.001097/s), `e2b` ($0.000160/s) — rates documented in module docstring with source URLs and update date |
| FR-07 | `cmd_sandbox_costs()` in `controller.py` must parse `--since`, `--run-id`, `--by`, `--backend`, `--profile`, `--min-cost`, `--limit`, and `--json` flags |
| FR-08 | `--since` must accept duration strings in the format `<N>(s|m|h|d|w)` and compute the cutoff as `now - duration`; invalid format must produce a clear error with examples |
| FR-09 | `--by backend` must return aggregated rows: `backend`, `runs`, `total_cost_usd`, `avg_cost_usd`, `p50_duration_seconds`, `p95_duration_seconds`, `rate_per_second_usd`, `minimum_charge_seconds` |
| FR-10 | `backfill_costs(conn)` must compute and insert cost records for all `sandbox_runs` rows that have `completed_at IS NOT NULL` and no matching row in `sandbox_run_costs`; it must be called once from `cost_table.ensure_schema()` |
| FR-11 | `run_in_sandbox()` return dict must include `cost_usd` (float), `duration_seconds` (float), and `rate_per_second_usd` (float) |
| FR-12 | `tag sandbox list` must join `sandbox_run_costs` and display a `cost` column; runs without a cost record must display `—` |
| FR-13 | `tag sandbox costs --run-id <id>` for a run with `status='running'` (no `completed_at`) must display `in progress` for cost and duration with a note that cost is attributed on completion |
| FR-14 | `check_sandbox_budget(conn, profile)` in `cost_table.py` must mirror the interface of `budget.check_budget()`: return a status dict, raise `SandboxBudgetExceeded` at 100%, emit `SandboxBudgetWarning` at `warn_pct` |
| FR-15 | `run_in_sandbox()` must call `check_sandbox_budget()` before launching the backend if a sandbox budget is configured for the invoking profile; failure to find a profile is a no-op (no budget = no gate) |
| FR-16 | The `--json` output must be valid JSON conforming to the schemas defined in Section 6.1; the top-level key must always be `runs` for per-run output and `by_backend` for aggregate output, never mixed |
| FR-17 | `tag sandbox costs` with no filters and no `sandbox_run_costs` rows must print an empty state message: `No sandbox cost records found. Run 'tag sandbox run' to generate cost data.` |
| FR-18 | All monetary values in JSON output must be floats rounded to 6 decimal places; display values in human-readable output must use `$0.000000` format with 6 decimal places |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | `tag sandbox costs --since 30d` must complete in < 200ms for up to 10,000 cost records on a warm SQLite connection (SQLite index on `run_id` and `created_at`) |
| NFR-02 | `cost_table.py` must have zero mandatory imports beyond the Python standard library; `sqlite3`, `datetime`, `dataclasses`, and `typing` are sufficient |
| NFR-03 | Cost computation in `compute_cost()` must be deterministic: same `duration_seconds` and same `rate_per_second_usd` must always produce the same `cost_usd` with no floating-point non-determinism across Python versions (use `round(result, 6)`) |
| NFR-04 | `backfill_costs()` must be idempotent: running it multiple times on the same database must not create duplicate rows (use `INSERT OR IGNORE`) |
| NFR-05 | `sandbox_run_costs` writes must participate in the same SQLite transaction as `sandbox_runs` status updates; a crash between the two writes must leave the database in a consistent state (no orphaned cost record without a corresponding run record) |
| NFR-06 | The module must not import `modal`, `e2b`, or any cloud SDK; rate tables are pure data, not live API calls |
| NFR-07 | JSON output must be UTF-8 encoded and produced by `json.dumps(..., indent=2)` for human readability when piped to files, and `json.dumps(...)` (compact) when `--compact` flag is added in future |
| NFR-08 | `cost_table.py` must be independently testable without running a real sandbox; all functions that write cost records accept a `sqlite3.Connection` parameter and do not open their own connections |

---

## 9. Technical Design

### 9.1 New File: `src/tag/cost_table.py`

This module owns all cost attribution logic. It is imported by `sandbox.py` and by `controller.py`'s `cmd_sandbox_costs` handler.

```python
"""PRD-099: Per-Second Cost Attribution per Sandbox Run.

Provides cost computation, storage, and querying for sandbox runs.
All rates are per-second USD. Source and update date documented per constant.

Rate sources (as of 2026-06-17):
  E2B:        https://e2b.dev/pricing  ($0.000160/s = ~$0.576/hr for base tier)
  Modal:      https://modal.com/pricing ($0.001097/s ≈ CPU compute, shared tier)
  Docker:     Reference: t3.medium @ $0.0416/hr / 3600 = ~$0.0000116/s * 2.4 overhead = $0.000028/s
  restricted: $0.000000/s (local subprocess, no third-party billing)
"""
from __future__ import annotations

import datetime
import sqlite3
import warnings
from dataclasses import dataclass
from typing import Optional

# ---------------------------------------------------------------------------
# Rate table
# ---------------------------------------------------------------------------

@dataclass(frozen=True)
class BackendRate:
    """Per-second pricing configuration for a sandbox backend."""
    backend: str
    rate_per_second_usd: float
    minimum_charge_seconds: float
    billing_model: str          # "per_second" | "free" | "reference"
    source_url: str
    rate_as_of: str             # ISO date string


BACKEND_RATES: dict[str, BackendRate] = {
    "restricted": BackendRate(
        backend="restricted",
        rate_per_second_usd=0.0,
        minimum_charge_seconds=0.0,
        billing_model="free",
        source_url="https://docs.tag.ai/sandbox#restricted",
        rate_as_of="2026-06-17",
    ),
    "docker": BackendRate(
        backend="docker",
        rate_per_second_usd=0.000028,
        minimum_charge_seconds=0.0,
        billing_model="reference",  # local-compute reference cost
        source_url="https://aws.amazon.com/ec2/pricing/on-demand/",
        rate_as_of="2026-06-17",
    ),
    "modal": BackendRate(
        backend="modal",
        rate_per_second_usd=0.001097,
        minimum_charge_seconds=1.0,
        billing_model="per_second",
        source_url="https://modal.com/pricing",
        rate_as_of="2026-06-17",
    ),
    "e2b": BackendRate(
        backend="e2b",
        rate_per_second_usd=0.000160,
        minimum_charge_seconds=1.0,
        billing_model="per_second",
        source_url="https://e2b.dev/pricing",
        rate_as_of="2026-06-17",
    ),
}
```

### 9.2 SQLite DDL

**New table: `sandbox_run_costs`**

```sql
CREATE TABLE IF NOT EXISTS sandbox_run_costs (
    id                   TEXT PRIMARY KEY,          -- cost record UUID (hex12)
    run_id               TEXT NOT NULL UNIQUE,      -- FK → sandbox_runs.id
    backend              TEXT NOT NULL,             -- copied from sandbox_runs for fast aggregation
    profile              TEXT,                      -- profile that invoked the run (nullable)
    started_at           TEXT NOT NULL,             -- copy of sandbox_runs.created_at
    completed_at         TEXT NOT NULL,             -- copy of sandbox_runs.completed_at
    duration_seconds     REAL NOT NULL,             -- (completed_at - started_at) in seconds
    rate_per_second_usd  REAL NOT NULL,             -- effective rate used for this run
    minimum_charge_seconds REAL NOT NULL DEFAULT 0, -- backend minimum billing increment
    billed_seconds       REAL NOT NULL,             -- max(duration_seconds, minimum_charge_seconds)
    cost_usd             REAL NOT NULL,             -- billed_seconds * rate_per_second_usd
    billing_model        TEXT NOT NULL DEFAULT 'per_second',
    rate_source          TEXT,                      -- 'config_override' | 'default'
    created_at           TEXT NOT NULL              -- when this cost record was written
);

CREATE INDEX IF NOT EXISTS idx_src_run_id
    ON sandbox_run_costs(run_id);

CREATE INDEX IF NOT EXISTS idx_src_backend_started
    ON sandbox_run_costs(backend, started_at);

CREATE INDEX IF NOT EXISTS idx_src_profile_started
    ON sandbox_run_costs(profile, started_at);

CREATE INDEX IF NOT EXISTS idx_src_started
    ON sandbox_run_costs(started_at);
```

**New table: `sandbox_budgets`**

```sql
CREATE TABLE IF NOT EXISTS sandbox_budgets (
    id           TEXT PRIMARY KEY,
    profile      TEXT NOT NULL UNIQUE,
    period       TEXT NOT NULL DEFAULT 'daily',    -- 'daily' | 'weekly' | 'monthly'
    max_cost_usd REAL NOT NULL,
    warn_pct     REAL NOT NULL DEFAULT 0.8,
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sb_profile ON sandbox_budgets(profile);
```

**Migration to `sandbox_runs` table: add `profile` column**

```sql
ALTER TABLE sandbox_runs ADD COLUMN profile TEXT;
```

This column is populated by `run_in_sandbox()` when a `profile` argument is passed (added to the function signature as an optional keyword argument).

### 9.3 Core Algorithms

**Duration computation:**

```python
def _parse_iso(ts: str) -> datetime.datetime:
    """Parse ISO-8601 timestamp with optional trailing Z."""
    ts = ts.replace("Z", "+00:00")
    return datetime.datetime.fromisoformat(ts)


def compute_duration(started_at: str, completed_at: str) -> float:
    """Return wall-clock duration in floating-point seconds."""
    start = _parse_iso(started_at)
    end = _parse_iso(completed_at)
    return (end - start).total_seconds()
```

**Cost computation:**

```python
def compute_cost(
    duration_seconds: float,
    rate: BackendRate,
    config_override: Optional[float] = None,
) -> tuple[float, float, str]:
    """Compute billed cost for a sandbox run.

    Returns (billed_seconds, cost_usd, rate_source).
    """
    effective_rate = config_override if config_override is not None else rate.rate_per_second_usd
    rate_source = "config_override" if config_override is not None else "default"
    billed = max(duration_seconds, rate.minimum_charge_seconds)
    cost = round(billed * effective_rate, 6)
    return billed, cost, rate_source
```

**Duration parsing for `--since` flag:**

```python
import re

_DURATION_RE = re.compile(r"^(\d+(?:\.\d+)?)(s|m|h|d|w)$")
_MULTIPLIERS = {"s": 1, "m": 60, "h": 3600, "d": 86400, "w": 604800}

def parse_since(value: str) -> datetime.datetime:
    """Parse '7d', '2h', '30m' → cutoff UTC datetime."""
    m = _DURATION_RE.match(value.strip().lower())
    if not m:
        raise ValueError(
            f"Invalid --since value: {value!r}. "
            "Expected format: <N>(s|m|h|d|w), e.g. 7d, 2h, 30m"
        )
    n, unit = float(m.group(1)), m.group(2)
    delta = datetime.timedelta(seconds=n * _MULTIPLIERS[unit])
    return datetime.datetime.now(datetime.timezone.utc) - delta
```

**P50/P95 computation (pure Python, no numpy):**

```python
def _percentile(values: list[float], pct: float) -> float:
    """Compute a percentile of a sorted list. pct in [0, 100]."""
    if not values:
        return 0.0
    s = sorted(values)
    idx = (pct / 100) * (len(s) - 1)
    lo, hi = int(idx), min(int(idx) + 1, len(s) - 1)
    frac = idx - lo
    return round(s[lo] + frac * (s[hi] - s[lo]), 3)
```

**`write_cost_record()`:**

```python
def write_cost_record(
    conn: sqlite3.Connection,
    run_id: str,
    backend: str,
    profile: Optional[str],
    started_at: str,
    completed_at: str,
    config_rates: Optional[dict] = None,
) -> dict:
    """Compute and persist a cost record for a completed sandbox run.

    config_rates: optional dict mapping backend name → rate_per_second_usd,
    loaded from cli-config.yaml sandbox.pricing section.
    """
    rate = BACKEND_RATES.get(backend, BACKEND_RATES["restricted"])
    override = (config_rates or {}).get(backend)
    duration = compute_duration(started_at, completed_at)
    billed, cost, rate_source = compute_cost(duration, rate, override)
    now = datetime.datetime.now(datetime.timezone.utc).isoformat()
    record_id = _new_id()

    conn.execute(
        """INSERT OR IGNORE INTO sandbox_run_costs
           (id, run_id, backend, profile, started_at, completed_at,
            duration_seconds, rate_per_second_usd, minimum_charge_seconds,
            billed_seconds, cost_usd, billing_model, rate_source, created_at)
           VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)""",
        (
            record_id, run_id, backend, profile,
            started_at, completed_at,
            round(duration, 6),
            override if override is not None else rate.rate_per_second_usd,
            rate.minimum_charge_seconds,
            round(billed, 6),
            cost,
            rate.billing_model,
            rate_source,
            now,
        ),
    )
    return {
        "run_id": run_id,
        "duration_seconds": round(duration, 6),
        "billed_seconds": round(billed, 6),
        "cost_usd": cost,
        "rate_per_second_usd": override if override is not None else rate.rate_per_second_usd,
        "billing_model": rate.billing_model,
        "rate_source": rate_source,
    }
```

### 9.4 Integration with `sandbox.py`

`run_in_sandbox()` signature changes:

```python
def run_in_sandbox(
    conn: sqlite3.Connection,
    command_str: str,
    *,
    backend: str = "restricted",
    image: str = "python:3.12-slim",
    timeout: int = 60,
    workdir: Path | None = None,
    profile: str | None = None,           # NEW: for budget check + cost attribution
    config_rates: dict | None = None,     # NEW: from cli-config.yaml sandbox.pricing
) -> dict:
```

Changes to `run_in_sandbox()` body:

1. After `ensure_schema(conn)`, call `cost_table.ensure_schema(conn)`.
2. Before launching the backend, call `cost_table.check_sandbox_budget(conn, profile)` if `profile` is not None.
3. Store `profile` in the `sandbox_runs` INSERT.
4. After updating `sandbox_runs` with `completed_at`, call `cost_table.write_cost_record(conn, ...)` inside the same transaction.
5. Merge the returned cost fields into the result dict.

**Transaction safety:** The existing code calls `conn.commit()` after updating `sandbox_runs`. The cost record write is added before that commit, so both the status update and cost record are committed atomically:

```python
    conn.execute(
        """UPDATE sandbox_runs SET status=?, exit_code=?, output=?, completed_at=?
           WHERE id=?""",
        (status, exit_code, output[:50000], completed_at, run_id),
    )
    cost_info = cost_table.write_cost_record(
        conn, run_id, backend, profile, now, completed_at, config_rates
    )
    conn.commit()   # single commit covers both writes
```

### 9.5 Integration with `budget.py`

New exception classes in `cost_table.py`:

```python
class SandboxBudgetExceeded(Exception):
    """Raised when a profile has exhausted its sandbox compute budget."""
    def __init__(self, profile: str, used_usd: float, limit_usd: float, period: str):
        super().__init__(
            f"Sandbox compute budget exceeded for profile '{profile}': "
            f"${used_usd:.4f} / ${limit_usd:.4f} used ({period})"
        )
        self.profile = profile
        self.used_usd = used_usd
        self.limit_usd = limit_usd
        self.period = period


class SandboxBudgetWarning(UserWarning):
    pass
```

**`check_sandbox_budget()` function:**

```python
def check_sandbox_budget(conn: sqlite3.Connection, profile: str) -> dict:
    """Check whether profile can run another sandbox. Mirrors budget.check_budget().

    Returns a status dict. Raises SandboxBudgetExceeded if hard cap is hit.
    Emits SandboxBudgetWarning at warn_pct threshold.
    """
    ensure_schema(conn)
    row = conn.execute(
        "SELECT max_cost_usd, warn_pct, period, enabled FROM sandbox_budgets WHERE profile=?",
        (profile,),
    ).fetchone()
    if not row or not row[3]:
        return {"allowed": True, "budget": None}

    limit_usd, warn_pct, period, _ = row
    window_start = _window_start(period)
    used_row = conn.execute(
        """SELECT COALESCE(SUM(cost_usd), 0.0)
           FROM sandbox_run_costs
           WHERE profile=? AND started_at >= ?""",
        (profile, window_start),
    ).fetchone()
    used_usd = float(used_row[0])
    pct = used_usd / limit_usd if limit_usd > 0 else 0.0

    result = {
        "allowed": True,
        "profile": profile,
        "used_usd": round(used_usd, 6),
        "limit_usd": limit_usd,
        "period": period,
        "pct": round(pct * 100, 1),
        "warn": False,
    }

    if pct >= 1.0:
        raise SandboxBudgetExceeded(profile, used_usd, limit_usd, period)

    if pct >= warn_pct:
        result["warn"] = True
        warnings.warn(
            f"Sandbox budget for '{profile}' at {pct * 100:.0f}% "
            f"(${used_usd:.4f} / ${limit_usd:.4f} {period})",
            SandboxBudgetWarning,
            stacklevel=2,
        )

    return result
```

### 9.6 Query Functions for `cmd_sandbox_costs`

```python
def query_costs(
    conn: sqlite3.Connection,
    *,
    since: Optional[datetime.datetime] = None,
    run_id: Optional[str] = None,
    backend: Optional[str] = None,
    profile: Optional[str] = None,
    min_cost_usd: float = 0.0,
    limit: int = 50,
) -> list[dict]:
    """Return a list of cost records matching the given filters."""
    clauses = []
    params: list = []

    if since:
        clauses.append("c.started_at >= ?")
        params.append(since.isoformat())
    if run_id:
        clauses.append("c.run_id = ?")
        params.append(run_id)
    if backend:
        clauses.append("c.backend = ?")
        params.append(backend)
    if profile:
        clauses.append("c.profile = ?")
        params.append(profile)
    if min_cost_usd > 0:
        clauses.append("c.cost_usd >= ?")
        params.append(min_cost_usd)

    where = ("WHERE " + " AND ".join(clauses)) if clauses else ""
    params.append(limit)

    rows = conn.execute(
        f"""SELECT
              c.run_id, c.backend, c.profile,
              c.started_at, c.completed_at,
              c.duration_seconds, c.billed_seconds,
              c.rate_per_second_usd, c.cost_usd,
              c.billing_model, c.rate_source,
              r.command, r.image, r.status, r.exit_code
            FROM sandbox_run_costs c
            JOIN sandbox_runs r ON r.id = c.run_id
            {where}
            ORDER BY c.started_at DESC
            LIMIT ?""",
        params,
    ).fetchall()

    cols = [
        "run_id", "backend", "profile", "started_at", "completed_at",
        "duration_seconds", "billed_seconds", "rate_per_second_usd", "cost_usd",
        "billing_model", "rate_source", "command", "image", "status", "exit_code",
    ]
    return [dict(zip(cols, r)) for r in rows]


def query_costs_by_backend(
    conn: sqlite3.Connection,
    *,
    since: Optional[datetime.datetime] = None,
    profile: Optional[str] = None,
) -> list[dict]:
    """Aggregate cost records by backend."""
    clauses = []
    params: list = []

    if since:
        clauses.append("started_at >= ?")
        params.append(since.isoformat())
    if profile:
        clauses.append("profile = ?")
        params.append(profile)

    where = ("WHERE " + " AND ".join(clauses)) if clauses else ""

    rows = conn.execute(
        f"""SELECT
              backend,
              COUNT(*) AS runs,
              SUM(cost_usd) AS total_cost_usd,
              AVG(cost_usd) AS avg_cost_usd,
              SUM(duration_seconds) AS total_duration_seconds,
              rate_per_second_usd,
              minimum_charge_seconds
            FROM sandbox_run_costs
            {where}
            GROUP BY backend
            ORDER BY total_cost_usd DESC""",
        params,
    ).fetchall()

    # Compute p50/p95 per backend with a second query
    result = []
    for r in rows:
        backend = r[0]
        dur_rows = conn.execute(
            "SELECT duration_seconds FROM sandbox_run_costs WHERE backend=?"
            + (" AND started_at >= ?" if since else ""),
            [backend] + ([since.isoformat()] if since else []),
        ).fetchall()
        durations = [x[0] for x in dur_rows]
        result.append({
            "backend": backend,
            "runs": r[1],
            "total_cost_usd": round(r[2], 6),
            "avg_cost_usd": round(r[3], 6),
            "total_duration_seconds": round(r[4], 3),
            "p50_duration_seconds": _percentile(durations, 50),
            "p95_duration_seconds": _percentile(durations, 95),
            "rate_per_second_usd": r[5],
            "minimum_charge_seconds": r[6],
        })
    return result
```

### 9.7 Controller Integration (`controller.py`)

A new `cmd_sandbox_costs()` function is added to `controller.py` following the existing `cmd_sandbox_*` pattern:

```python
def cmd_sandbox_costs(args: argparse.Namespace, conn: sqlite3.Connection) -> int:
    """Handler for `tag sandbox costs`."""
    from tag import cost_table

    cost_table.ensure_schema(conn)

    since = None
    if args.since:
        try:
            since = cost_table.parse_since(args.since)
        except ValueError as exc:
            print(f"error: {exc}", file=sys.stderr)
            return 1

    by_backend = getattr(args, "by", None) == "backend"

    if by_backend:
        rows = cost_table.query_costs_by_backend(
            conn,
            since=since,
            profile=getattr(args, "profile", None),
        )
        if args.json:
            period = {
                "since": since.isoformat() if since else None,
                "until": datetime.datetime.now(datetime.timezone.utc).isoformat(),
            }
            print(json.dumps({"period": period, "by_backend": rows}, indent=2))
        else:
            _print_by_backend_table(rows)
    else:
        rows = cost_table.query_costs(
            conn,
            since=since,
            run_id=getattr(args, "run_id", None),
            backend=getattr(args, "backend", None),
            profile=getattr(args, "profile", None),
            min_cost_usd=float(getattr(args, "min_cost", 0.0)),
            limit=int(getattr(args, "limit", 50)),
        )
        if not rows and not args.json:
            print(
                "No sandbox cost records found. "
                "Run 'tag sandbox run' to generate cost data."
            )
            return 0
        if args.json:
            total_cost = sum(r["cost_usd"] for r in rows)
            total_dur = sum(r["duration_seconds"] for r in rows)
            period = {
                "since": since.isoformat() if since else None,
                "until": datetime.datetime.now(datetime.timezone.utc).isoformat(),
            }
            print(json.dumps({
                "period": period,
                "summary": {
                    "total_runs": len(rows),
                    "total_cost_usd": round(total_cost, 6),
                    "total_duration_seconds": round(total_dur, 3),
                },
                "runs": rows,
            }, indent=2))
        else:
            _print_costs_table(rows)
    return 0
```

### 9.8 `cli-config.yaml` Schema Addition

```yaml
sandbox:
  pricing:
    # Override per-second rates (USD). Keys match backend names.
    # Remove a key to use the built-in default.
    # e2b: 0.000160      # default: E2B public pricing
    # modal: 0.001097    # default: Modal CPU compute
    # docker: 0.000028   # default: reference local-compute cost
    # restricted: 0.0    # always free
  budgets:
    # Per-profile sandbox compute dollar caps (same period syntax as token_budgets)
    # coder:
    #   period: daily
    #   max_cost_usd: 5.00
    #   warn_pct: 0.8
```

---

## 10. Security Considerations

1. **No secrets in cost records:** `sandbox_run_costs` stores only timing, rate, and cost data. The `command` field is stored in `sandbox_runs`, not duplicated in `sandbox_run_costs`. The join for reporting is read-only. No credentials or environment variables appear in the cost table.

2. **Rate override validation:** `config_rates` values loaded from `cli-config.yaml` must be validated as non-negative floats before use. Negative rates (which would produce negative costs, confusing budget checks) must be rejected with a clear config error.

3. **SQLite injection prevention:** All queries use parameterized statements (`?` placeholders). No string interpolation is used in SQL query construction, with the sole exception of the `WHERE` clause assembly which uses a whitelist-validated set of column names and operators.

4. **Budget bypass prevention:** `check_sandbox_budget()` is called inside `run_in_sandbox()` before any backend subprocess is launched. Code paths that bypass `run_in_sandbox()` (e.g., direct subprocess calls in `controller.py`) are not protected by this gate — a follow-on security audit should enumerate all such call sites.

5. **File permission of `tag.sqlite3`:** The existing database at `~/.tag/runtime/tag.sqlite3` is created with mode `0600` by `open_db()`. The new `sandbox_run_costs` and `sandbox_budgets` tables inherit this file's permissions. No additional file permission changes are needed.

6. **Denial-of-service via budget bypass:** A misconfigured `warn_pct` of 1.0 (identical to the hard limit) would result in no warning before the hard limit fires. Validation must enforce `0.0 < warn_pct < 1.0` with a minimum gap of 0.01 (i.e., warn at no later than 99%).

---

## 11. Testing Strategy

### 11.1 Unit Tests (`tests/test_cost_table.py`)

```python
# Key test cases (not exhaustive):

def test_compute_duration_utc():
    """Duration between two ISO timestamps is correct to millisecond precision."""
    d = compute_duration("2026-06-15T14:00:00.000Z", "2026-06-15T14:00:12.432Z")
    assert abs(d - 12.432) < 0.001

def test_compute_cost_with_minimum_charge():
    """Short E2B run is billed for the 1-second minimum."""
    rate = BACKEND_RATES["e2b"]
    billed, cost, source = compute_cost(0.3, rate)
    assert billed == 1.0                  # minimum charge applies
    assert cost == round(1.0 * 0.000160, 6)
    assert source == "default"

def test_compute_cost_no_minimum_docker():
    """Docker run has no minimum charge."""
    rate = BACKEND_RATES["docker"]
    billed, cost, _ = compute_cost(0.5, rate)
    assert billed == 0.5

def test_compute_cost_config_override():
    """Custom rate from config overrides default."""
    rate = BACKEND_RATES["e2b"]
    _, cost, source = compute_cost(10.0, rate, config_override=0.000200)
    assert cost == round(10.0 * 0.000200, 6)
    assert source == "config_override"

def test_write_cost_record_idempotent(tmp_sqlite):
    """Writing a cost record twice does not create a duplicate (INSERT OR IGNORE)."""
    _seed_sandbox_run(tmp_sqlite, "run001", "docker")
    write_cost_record(tmp_sqlite, "run001", "docker", None,
                      "2026-06-15T14:00:00Z", "2026-06-15T14:00:12Z")
    write_cost_record(tmp_sqlite, "run001", "docker", None,
                      "2026-06-15T14:00:00Z", "2026-06-15T14:00:12Z")
    tmp_sqlite.commit()
    count = tmp_sqlite.execute(
        "SELECT COUNT(*) FROM sandbox_run_costs WHERE run_id='run001'"
    ).fetchone()[0]
    assert count == 1

def test_backfill_costs(tmp_sqlite):
    """backfill_costs() generates cost records for all completed runs."""
    for i in range(5):
        _seed_sandbox_run(tmp_sqlite, f"run{i:03d}", "e2b",
                          created="2026-06-10T10:00:00Z",
                          completed="2026-06-10T10:00:30Z")
    backfill_costs(tmp_sqlite)
    tmp_sqlite.commit()
    count = tmp_sqlite.execute("SELECT COUNT(*) FROM sandbox_run_costs").fetchone()[0]
    assert count == 5

def test_check_sandbox_budget_exceeded(tmp_sqlite):
    """SandboxBudgetExceeded is raised when limit is hit."""
    set_sandbox_budget(tmp_sqlite, "coder", max_cost_usd=0.001, period="daily")
    # Seed a cost record that exhausts the budget
    _seed_cost_record(tmp_sqlite, "run001", "coder", cost_usd=0.002)
    tmp_sqlite.commit()
    with pytest.raises(SandboxBudgetExceeded):
        check_sandbox_budget(tmp_sqlite, "coder")

def test_parse_since_formats():
    """All duration format variants parse correctly."""
    now = datetime.datetime.now(datetime.timezone.utc)
    for value, expected_delta in [
        ("30s", 30), ("5m", 300), ("2h", 7200),
        ("7d", 604800), ("1w", 604800),
    ]:
        result = parse_since(value)
        assert abs((now - result).total_seconds() - expected_delta) < 2

def test_parse_since_invalid():
    """Invalid --since formats raise ValueError with helpful message."""
    with pytest.raises(ValueError, match="Expected format"):
        parse_since("3x")
```

### 11.2 Integration Tests (`tests/test_sandbox_costs_integration.py`)

- Run `run_in_sandbox()` with `backend="restricted"` and verify a cost record appears in `sandbox_run_costs` with `cost_usd=0.0`.
- Run `run_in_sandbox()` with a mocked Docker backend and verify cost is computed from timestamps.
- Call `cmd_sandbox_costs()` with `--json` flag and validate the output parses as JSON with the correct top-level keys.
- Run `backfill_costs()` on a pre-populated SQLite file with 50 sandbox_runs rows; verify all 50 get cost records and no duplicates.
- Set a sandbox budget, run enough sandbox runs to exhaust it, and verify the next `run_in_sandbox()` call raises `SandboxBudgetExceeded`.

### 11.3 Performance Tests (`tests/test_cost_perf.py`)

```python
def test_query_costs_10k_rows(tmp_sqlite):
    """query_costs() completes in under 200ms for 10,000 cost records."""
    _seed_n_cost_records(tmp_sqlite, n=10_000)
    start = time.monotonic()
    rows = query_costs(tmp_sqlite, since=parse_since("30d"), limit=50)
    elapsed_ms = (time.monotonic() - start) * 1000
    assert elapsed_ms < 200
    assert len(rows) <= 50
```

---

## 12. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|--------------|
| AC-01 | `run_in_sandbox()` return dict includes `cost_usd`, `duration_seconds`, `rate_per_second_usd` for all backends | Unit test: assert keys present in return dict |
| AC-02 | A completed sandbox run always has exactly one row in `sandbox_run_costs` | Integration test: SELECT COUNT after run |
| AC-03 | `tag sandbox costs --since 7d` produces a valid human-readable table with a total cost summary line | Manual + snapshot test |
| AC-04 | `tag sandbox costs --since 7d --json` produces valid JSON with `period`, `summary`, and `runs` top-level keys | `json.loads()` in CI test |
| AC-05 | `tag sandbox costs --run-id <id>` shows exactly one row for a known run | Integration test |
| AC-06 | `tag sandbox costs --by backend --json` produces valid JSON with `by_backend` array, each entry having `p50_duration_seconds` and `p95_duration_seconds` | Schema validation test |
| AC-07 | E2B run of 0.3s is billed for 1.0s (minimum charge), not 0.3s | Unit test on `compute_cost()` |
| AC-08 | Restricted backend run always shows `$0.000000` in cost output | Unit test + integration test |
| AC-09 | Config override in `cli-config.yaml sandbox.pricing.e2b` is used instead of default | Unit test with patched config |
| AC-10 | `backfill_costs()` populates cost records for all pre-existing `sandbox_runs` with `completed_at` | Integration test on seeded DB |
| AC-11 | `SandboxBudgetExceeded` is raised before backend launch when profile is over its sandbox dollar cap | Unit test: no subprocess spawned after exception |
| AC-12 | `tag sandbox costs` with no records shows the empty-state message, exit 0 | Integration test |
| AC-13 | `--since` with invalid format (e.g., `--since 3x`) exits 1 with a message containing "Expected format" | Integration test: subprocess exit code + stderr |
| AC-14 | `sandbox_run_costs` write and `sandbox_runs` status update are committed atomically (single `conn.commit()`) | Code review + unit test: simulate crash between writes |
| AC-15 | `tag sandbox list` shows a `cost` column with `—` for runs without cost records | Integration test on a pre-schema-migration DB |
| AC-16 | `cost_table.py` imports only stdlib modules (`sqlite3`, `datetime`, `dataclasses`, `re`, `uuid`, `warnings`, `typing`) | Import audit in CI: `pipreqs` or `importlib` check |

---

## 13. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| PRD-028 (Sandbox Code Execution) | Hard upstream | `sandbox_runs` table and `run_in_sandbox()` API must exist |
| PRD-039 (Token Budget Enforcement) | Design reference | `sandbox_budgets` mirrors `token_budgets` schema; `check_sandbox_budget()` mirrors `check_budget()` interface |
| PRD-012 (Cost Tracking & Budget) | Design reference | `tag costs` command pattern; `tag sandbox costs` follows the same display conventions |
| PRD-013 (Agent Tracing & Observability) | Optional integration | `tracing.py` Span objects could be extended with a `sandbox_cost_usd` attribute for OTel export; deferred |
| `open_db()` in `controller.py` | Runtime | WAL-mode SQLite connection; `cost_table.ensure_schema()` must be called before any reads/writes |
| Python `datetime.fromisoformat()` | Runtime | Available Python 3.7+; Z-suffix handling requires the `.replace("Z", "+00:00")` workaround for Python < 3.11 |
| `cli-config.yaml` loader in `controller.py` | Runtime | Must pass `config_rates` dict to `run_in_sandbox()` if `sandbox.pricing` section is present |

---

## 14. Open Questions

| ID | Question | Owner | Resolution Path |
|----|----------|-------|-----------------|
| OQ-01 | Should `restricted` backend default to `$0.000000/s` always, or should it inherit a configurable "local CPU cost" to support teams that want to track internal compute cost allocation? | Engineering | Default to $0.00; add `local_cpu_rate_per_second_usd` as optional config key; document use case |
| OQ-02 | Modal pricing varies by resource type (CPU, GPU A10G, H100, etc.). Should the `modal` backend rate in the rate table be a single flat rate, or should it accept a `resource_type` tag on the sandbox run? | Product | V1: single flat rate. V2: `run_in_sandbox()` accepts optional `resource_type` param that selects from a sub-table of Modal GPU rates |
| OQ-03 | Should `tag sandbox costs` be able to query cost by `task_id` (cross-referencing the `tasks` table) so that per-task total cost includes both LLM token cost and sandbox compute cost? | Engineering | Depends on PRD-012 `tasks` table schema; coordinate with cost tracking work |
| OQ-04 | What happens when a sandbox run is killed (status='killed') before it writes `completed_at`? Should `completed_at` be written to `sandbox_runs` on kill, and should a cost record be written for the partial run? | Engineering | Yes: record `completed_at=now` on kill; attribute cost for actual elapsed time regardless of exit path |
| OQ-05 | Rate table values will become stale as E2B and Modal update pricing. Should the default rates be fetched from a remote manifest (e.g., `https://tag.ai/sandbox-rates.json`) with a 24h TTL and local cache? | Product | V1: hardcoded defaults + config override; V2: optional remote rate manifest with fallback |
| OQ-06 | Should `tag sandbox costs --export csv` be supported for spreadsheet import? | Product | Out of scope for V1; `--json` piped to `jq` covers this; add `--csv` if user demand materializes |

---

## 15. Complexity and Timeline

**Overall estimate:** S (3–5 days for a single engineer)

### Phase 1 — Schema + Core Cost Engine (Day 1)

- Create `src/tag/cost_table.py` with `BACKEND_RATES`, `BackendRate` dataclass, `ensure_schema()`, `compute_duration()`, `compute_cost()`, `write_cost_record()`, `backfill_costs()`
- Add `sandbox_run_costs` and `sandbox_budgets` DDL
- Add `ALTER TABLE sandbox_runs ADD COLUMN profile TEXT` migration (idempotent: wrapped in `try/except OperationalError`)
- Unit tests for all computation functions (`tests/test_cost_table.py`)

### Phase 2 — Sandbox Integration (Day 2)

- Update `sandbox.py`: add `profile` and `config_rates` params to `run_in_sandbox()`
- Add `cost_table.ensure_schema(conn)` call to `sandbox.ensure_schema()`
- Add `check_sandbox_budget()` pre-flight in `run_in_sandbox()`
- Add cost fields to return dict
- Add cost column to `list_sandbox_runs()` via LEFT JOIN
- Integration tests: `run_in_sandbox()` produces cost records for all backends
- Integration test: `SandboxBudgetExceeded` fires correctly

### Phase 3 — CLI Surface (Day 3)

- Add `cmd_sandbox_costs()` to `controller.py` with argparse registration
- Implement `query_costs()`, `query_costs_by_backend()` in `cost_table.py`
- Implement `parse_since()` with full format test coverage
- Implement `_print_costs_table()` and `_print_by_backend_table()` display helpers
- Augment `tag sandbox run` post-completion output with cost line
- Augment `tag sandbox list` with cost column

### Phase 4 — Budget Management + Config (Day 4)

- Implement `set_sandbox_budget()`, `remove_sandbox_budget()`, `check_sandbox_budget()` in `cost_table.py`
- Add `cli-config.yaml` parsing for `sandbox.pricing` and `sandbox.budgets` sections
- Wire config override into `cmd_sandbox_run()` → `run_in_sandbox(config_rates=...)`
- Integration test: budget warns at `warn_pct`, blocks at `max_cost_usd`

### Phase 5 — Polish + Performance (Day 5)

- Performance test: 10,000 cost records query under 200ms
- `backfill_costs()` integration test on seeded SQLite file
- JSON schema validation tests (`jsonschema` or manual key assertions)
- Documentation: `--help` text for all new flags, docstrings on all public functions
- Update `docs/prd/INDEX.md` to add PRD-099
- Code review and merge

