# PRD-099: Per-Second Cost Attribution per Sandbox Run (`tag sandbox costs`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** S (3-5 days)
**Category:** Sandbox & Execution Environment
**Affects:** `internal/sandbox` + `internal/obs` (new cost-attribution unit alongside `pricing.go`)
**Depends on:** PRD-028 (Sandbox Code Execution), PRD-012 (Cost Tracking & Budget), PRD-013 (Agent Tracing & Observability), PRD-034 (Security), PRD-039 (Token Budget Enforcement)
**GitHub Issue:** #348
**Inspired by:** E2B per-second billing, Modal per-second billing, AWS Lambda per-ms billing

---

## 1. Overview

Modern cloud sandbox providers — E2B, Modal, AWS Lambda — all bill at sub-second granularity. E2B charges per second of microVM wall-clock time. Modal charges per second of GPU or CPU time with a minimum billing increment. Lambda charges per millisecond of invocation time. This billing model creates a tight feedback loop between sandbox runtime behavior and infrastructure cost: a sandbox that hangs for 30 seconds costs 30x more than one that completes in 1 second, and that signal is immediately visible in the cost ledger.

TAG's existing sandbox subsystem (`internal/sandbox`, PRD-028) records wall-clock start and end timestamps in `sandbox_runs.created_at` and `sandbox_runs.completed_at`, but performs no cost calculation, attribution, or reporting. The fields needed to derive runtime duration are present; the cost table, pricing model, and reporting surface are absent. Engineers and platform operators who use Docker, E2B, or Modal backends for sandbox execution have no visibility into sandbox-level spend: they cannot answer "how much did this week's sandbox runs cost?", "which backend is cheapest for my workload?", or "did this run exceed the compute budget?".

This PRD specifies `tag sandbox costs` — a `cobra` reporting subcommand (`internal/cli`) backed by a new cost-attribution unit in `internal/obs` (living alongside the existing `pricing.go` cost tables) that implements per-second cost attribution for every sandbox run. The unit reads `sandbox_runs` records from the single `modernc.org/sqlite` store (`internal/store`), looks up backend-specific per-second pricing from a locally configurable rate table, computes the dollar cost as `billed_seconds × rate_per_second` in fixed-point micro-USD, and stores the result in a new `sandbox_run_costs` table. The `tag sandbox costs` command surfaces this data as a human-readable table or machine-readable JSON, with filters by time range, run ID, and backend.

The cost attribution integrates with the existing budget gate (`internal/obs`, PRD-039) via a new `sandbox_cost_budget` concept: operators can set a rolling daily/weekly/monthly dollar cap on sandbox compute spending. When the cap is reached, `RunInSandbox()` returns a wrapped `ErrSandboxBudgetExceeded` error before launching the next run, preventing runaway compute costs from a looping agent or misconfigured job queue. This closes a gap that PRD-039 acknowledged: token budgets gate LLM API spend, but compute (sandbox) spend has no enforcement mechanism.

Pricing for the `restricted` backend (local subprocess) defaults to $0.00/second because it consumes local compute with no third-party billing. The `docker` backend defaults to a configurable local-compute cost (default: `$0.000028/second`, derived from a `t3.medium` EC2 instance hourly rate divided by 3600 as a reference point for on-premise cost allocation). E2B and Modal rates are seeded from publicly documented pricing at the time of the PRD and are user-overridable in `~/.tag/tag.yaml` (loaded via `knadh/koanf/v2`) so that operators with custom tier agreements can reflect their actual contracted rates.

---

## 2. Problem Statement

### 2.1 No Visibility into Sandbox Compute Spend

TAG dispatches sandbox runs through `RunInSandbox()` in `internal/sandbox`. Each run records a start timestamp (`created_at`) and end timestamp (`completed_at`), but the system never computes or persists a cost figure. An agent loop that spawns 200 sandbox runs in a single session (e.g., an autonomous coding agent running tests after every edit) may accumulate $20-$40 of E2B microVM time without any indicator in the CLI output. The operator only discovers this when the cloud billing dashboard catches up — often 24 hours later and with no per-run granularity.

### 2.2 No Sandbox-Level Budget Enforcement

The budget gate in `internal/obs` provides token-based hard limits per profile (`max_tokens` over a `daily`/`weekly`/`monthly` window). This gate fires before an LLM API call and prevents overspending on inference. However, sandbox compute spending is entirely unguarded: a job queue agent that exits successfully in zero tokens (e.g., because it only invokes shell commands) can still generate substantial backend compute costs. The `sandbox_budget` column does not exist in `token_budgets`; there is no `CheckSandboxBudget()` function; and `RunInSandbox()` does not call any budget gate before spinning up a backend.

### 2.3 No Cross-Backend Cost Comparison

TAG supports four backends: `restricted`, `docker`, `modal`, and `e2b` (plus planned Daytona and cloud-VM backends per PRD-028). Each backend has a fundamentally different cost structure: `restricted` uses local CPU cycles, `docker` uses local disk and memory, `modal` bills per-second with a 1-second minimum and generous free tier, `e2b` bills per-second from the first millisecond. Operators choosing between backends for a given workload have no data-driven basis for the decision. A report like `--by backend` that shows total spend, average cost per run, and P95 duration per backend would immediately answer the question "is Docker or Modal cheaper for my 30-second test suite jobs?"

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Compute and persist a dollar cost for every completed sandbox run using per-second pricing |
| G2 | Expose `tag sandbox costs` with filters for time range, run ID, and backend; support both human-readable table and `--json` output |
| G3 | Integrate with the `internal/obs` budget gate to support a sandbox compute dollar cap per profile with the same `warn_pct` / hard-limit semantics as token budgets |
| G4 | Ship a default rate table covering all four current backends (`restricted`, `docker`, `modal`, `e2b`) with user-overridable pricing in `~/.tag/config.yaml` |
| G5 | Backfill cost records for all existing `sandbox_runs` rows that have `completed_at` populated but no cost record yet, via a migration function called on first schema access |
| G6 | Add cost fields to the `RunInSandbox()` return struct and to the `tag sandbox list` output so cost appears inline during operation |
| G7 | Zero new external dependencies — pricing lookup and cost computation use only the Go stdlib plus the already-vendored `modernc.org/sqlite`; no external billing API |

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
| SM-03: Budget gate fires correctly | `RunInSandbox()` returns `ErrSandboxBudgetExceeded` within 5ms of a profile hitting its sandbox dollar cap | Unit test with an injected `Clock` |
| SM-04: Duration accuracy | Cost record `duration_seconds` differs from `(completed_at - created_at)` by < 1ms | Parameterized unit test with known timestamps |
| SM-05: Backfill migration completes | All pre-existing `sandbox_runs` rows with `completed_at` get cost records on first `EnsureSchema()` call | Integration test on a pre-seeded SQLite file |
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
| FR-01 | The `sandbox_run_costs` table must be created by `obs.EnsureCostSchema(ctx, db)`, called automatically from `sandbox.EnsureSchema()` so callers need no changes |
| FR-02 | Every call to `RunInSandbox()` that produces a `completed_at` timestamp must write a corresponding row to `sandbox_run_costs` before returning |
| FR-03 | Duration must be computed as `end.Sub(start).Seconds()` in `float64` seconds, parsing both ISO timestamp strings with `time.Parse(time.RFC3339Nano, …)` (normalising a trailing `Z` to `+00:00`) |
| FR-04 | Cost must be computed as `int64(math.Round(max(durationSeconds, minimumChargeSeconds) * float64(rateMicroUSDPerSec)))` micro-USD, where `minimumChargeSeconds` is backend-specific (E2B: 1, Modal: 1, Docker: 0, restricted: 0). Money is fixed-point `int64` micro-USD throughout; USD floats are derived only at the display/JSON boundary |
| FR-05 | Backend rate lookup must first read `sandbox.pricing.<backend>` from `~/.tag/tag.yaml` via the `koanf` config layer, falling back to the built-in defaults in `obs.BackendRates` |
| FR-06 | `obs.BackendRates` (a `map[string]BackendRate`) must define entries for `restricted` ($0.000000/s), `docker` ($0.000028/s), `modal` ($0.001097/s), `e2b` ($0.000160/s) — rates documented in a package doc comment with source URLs and update date |
| FR-07 | The `tag sandbox costs` `cobra` command (`internal/cli`) must define `--since`, `--run-id`, `--by`, `--backend`, `--profile`, `--min-cost`, `--limit`, and `--json` flags via its `*pflag.FlagSet` |
| FR-08 | `--since` must accept duration strings in the format `<N>(s\|m\|h\|d\|w)` and compute the cutoff as `clock.Now().Add(-d)`; invalid format must return a clear error with examples |
| FR-09 | `--by backend` must return aggregated rows: `backend`, `runs`, `total_cost_usd`, `avg_cost_usd`, `p50_duration_seconds`, `p95_duration_seconds`, `rate_per_second_usd`, `minimum_charge_seconds` |
| FR-10 | `BackfillCosts(ctx, db)` must compute and insert cost records for all `sandbox_runs` rows that have `completed_at IS NOT NULL` and no matching row in `sandbox_run_costs`; it must be called once from `obs.EnsureCostSchema()` |
| FR-11 | The `RunInSandbox()` return struct (`RunResult`) must include `CostUSD` (`float64`), `DurationSeconds` (`float64`), and `RatePerSecondUSD` (`float64`) fields, JSON-tagged `cost_usd`, `duration_seconds`, `rate_per_second_usd` |
| FR-12 | `tag sandbox list` must `LEFT JOIN sandbox_run_costs` and display a `cost` column; runs without a cost record must display `—` |
| FR-13 | `tag sandbox costs --run-id <id>` for a run with `status='running'` (no `completed_at`) must display `in progress` for cost and duration with a note that cost is attributed on completion |
| FR-14 | `CheckSandboxBudget(ctx, db, profile)` (`internal/obs`) must mirror the shape of the token `CheckBudget()` gate: return a `BudgetStatus` struct, return `*SandboxBudgetExceededError` (satisfying `error`, matchable via `errors.As`) at 100%, and set `Status.Warn=true` (plus a `slog.Warn` line) at `warn_pct` |
| FR-15 | `RunInSandbox()` must call `CheckSandboxBudget()` before launching the backend if a sandbox budget is configured for the invoking profile; an empty profile is a no-op (no budget = no gate) |
| FR-16 | The `--json` output must be valid JSON conforming to the schemas defined in Section 6.1; the top-level key must always be `runs` for per-run output and `by_backend` for aggregate output, never mixed. Marshalling uses `encoding/json` with typed output structs, not `map[string]any` |
| FR-17 | `tag sandbox costs` with no filters and no `sandbox_run_costs` rows must print an empty state message: `No sandbox cost records found. Run 'tag sandbox run' to generate cost data.` |
| FR-18 | All monetary values in JSON output must be `float64` rounded to 6 decimal places (derived as `microUSD / 1e6`); display values in human-readable output must use `$0.000000` format (`fmt.Sprintf("$%.6f", …)`) with 6 decimal places |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | `tag sandbox costs --since 30d` must complete in < 200ms for up to 10,000 cost records on a warm `modernc.org/sqlite` connection (SQLite index on `run_id` and `created_at`) |
| NFR-02 | The cost-attribution unit must add no new external module dependency: only the Go stdlib (`time`, `regexp`, `math`, `database/sql`, `encoding/json`) plus the already-vendored `modernc.org/sqlite` driver and `knadh/koanf/v2` config layer are used |
| NFR-03 | Cost computation in `ComputeCost()` must be deterministic: the same `durationSeconds` and same `rateMicroUSDPerSec` must always produce the same `int64` micro-USD result across platforms — integer micro-USD fixed-point plus a single `math.Round` at the micro boundary removes the float-drift class entirely |
| NFR-04 | `BackfillCosts()` must be idempotent: running it multiple times on the same database must not create duplicate rows (`INSERT OR IGNORE`, backed by the `UNIQUE(run_id)` constraint) |
| NFR-05 | `sandbox_run_costs` writes must participate in the same `*sql.Tx` as `sandbox_runs` status updates; a crash between the two writes must leave the database in a consistent state (no orphaned cost record without a corresponding run record) |
| NFR-06 | The unit must not import any cloud SDK (`modal`, `e2b`, docker/moby); rate tables are pure data (`obs.BackendRates`), not live API calls |
| NFR-07 | JSON output must be UTF-8 and produced with `json.MarshalIndent(v, "", "  ")` for human readability when piped to files, and `json.Marshal` (compact) when a future `--compact` flag is added |
| NFR-08 | The cost-attribution unit must be independently testable without running a real sandbox; all functions that write cost records take a `store.Querier` interface (satisfied by `*sql.DB` and `*sql.Tx`) as a parameter and do not open their own connections — enabling table-driven tests against an in-memory `modernc.org/sqlite` DB with an injected `Clock` |

---

## 9. Technical Design

### 9.1 New Unit: cost attribution in `internal/obs` (`cost.go`, alongside `pricing.go`)

Cost attribution lives in `internal/obs` next to the existing `pricing.go` cost tables, keeping all money logic in one package. It is imported by `internal/sandbox` and by the `internal/cli` `sandbox costs` `cobra` handler. Money is fixed-point `int64` **micro-USD** (1 USD = 1_000_000 micro-USD) end to end — the per-second rates are exact integers in micro-USD, and USD `float64` values are derived only at the display/JSON boundary. This mirrors `pricing.go`'s integer-per-1M-token model and eliminates the float-accumulation class the PRD's accuracy metrics (SM-04) depend on.

```go
// Package obs — per-second sandbox cost attribution (PRD-099).
//
// All rates are per-second, expressed in micro-USD (1 USD = 1e6). Source and
// update date are documented per entry.
//
// Rate sources (as of 2026-06-17):
//   E2B:        https://e2b.dev/pricing   ($0.000160/s = ~$0.576/hr, base tier)
//   Modal:      https://modal.com/pricing ($0.001097/s ~ CPU compute, shared tier)
//   Docker:     t3.medium @ $0.0416/hr / 3600 = ~$0.0000116/s * 2.4 overhead = $0.000028/s
//   restricted: $0.000000/s (local subprocess, no third-party billing)
package obs

// BillingModel is a typed string enum for how a backend charges wall-clock time.
type BillingModel string

const (
	BillingPerSecond BillingModel = "per_second"
	BillingFree      BillingModel = "free"
	BillingReference BillingModel = "reference"
)

// BackendRate is per-second pricing configuration for a sandbox backend.
type BackendRate struct {
	Backend              string
	RateMicroUSDPerSec   int64        // micro-USD per wall-clock second (28 == $0.000028)
	MinimumChargeSeconds float64      // backend minimum billing increment
	BillingModel         BillingModel
	SourceURL            string
	RateAsOf             string       // ISO date string
}

// BackendRates is the built-in default table. Overridable per-backend via the
// koanf config layer (sandbox.pricing.<backend>). Backend lookup is an exact
// map key — no glob needed here (pricing.go uses gobwas/glob for model IDs).
var BackendRates = map[string]BackendRate{
	"restricted": {
		Backend: "restricted", RateMicroUSDPerSec: 0, MinimumChargeSeconds: 0,
		BillingModel: BillingFree,
		SourceURL:    "https://docs.tag.ai/sandbox#restricted", RateAsOf: "2026-06-17",
	},
	"docker": {
		Backend: "docker", RateMicroUSDPerSec: 28, MinimumChargeSeconds: 0,
		BillingModel: BillingReference, // local-compute reference cost
		SourceURL:    "https://aws.amazon.com/ec2/pricing/on-demand/", RateAsOf: "2026-06-17",
	},
	"modal": {
		Backend: "modal", RateMicroUSDPerSec: 1097, MinimumChargeSeconds: 1,
		BillingModel: BillingPerSecond,
		SourceURL:    "https://modal.com/pricing", RateAsOf: "2026-06-17",
	},
	"e2b": {
		Backend: "e2b", RateMicroUSDPerSec: 160, MinimumChargeSeconds: 1,
		BillingModel: BillingPerSecond,
		SourceURL:    "https://e2b.dev/pricing", RateAsOf: "2026-06-17",
	},
}
```

### 9.2 SQLite DDL (`modernc.org/sqlite`)

Schema DDL stays plain SQL and is applied through `internal/store` on the single pure-Go `modernc.org/sqlite` handle (CGO_ENABLED=0, WAL). Money columns are stored as `INTEGER` micro-USD to preserve fixed-point accuracy; USD floats are derived at read time. `CREATE TABLE`/`CREATE INDEX` use the `IF NOT EXISTS` idempotent form; `ALTER TABLE` migrations are run in Go and guarded by a `duplicate column name` error check (see below).

**New table: `sandbox_run_costs`**

```sql
CREATE TABLE IF NOT EXISTS sandbox_run_costs (
    id                     TEXT PRIMARY KEY,          -- cost record ID (hex12)
    run_id                 TEXT NOT NULL UNIQUE,      -- FK -> sandbox_runs.id
    backend                TEXT NOT NULL,             -- copied from sandbox_runs for fast aggregation
    profile                TEXT,                      -- profile that invoked the run (nullable)
    started_at             TEXT NOT NULL,             -- copy of sandbox_runs.created_at
    completed_at           TEXT NOT NULL,             -- copy of sandbox_runs.completed_at
    duration_seconds       REAL NOT NULL,             -- (completed_at - started_at) in seconds
    rate_micro_usd_per_sec INTEGER NOT NULL,          -- effective rate used for this run (micro-USD/s)
    minimum_charge_seconds REAL NOT NULL DEFAULT 0,   -- backend minimum billing increment
    billed_seconds         REAL NOT NULL,             -- max(duration_seconds, minimum_charge_seconds)
    cost_micro_usd         INTEGER NOT NULL,          -- round(billed_seconds * rate_micro_usd_per_sec)
    billing_model          TEXT NOT NULL DEFAULT 'per_second',
    rate_source            TEXT,                      -- 'config_override' | 'default'
    created_at             TEXT NOT NULL              -- when this cost record was written
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

The JSON/table output contract (`cost_usd`, `rate_per_second_usd` as `float64`) is preserved by deriving `cost_usd = cost_micro_usd / 1e6` and `rate_per_second_usd = rate_micro_usd_per_sec / 1e6` in the query layer.

**New table: `sandbox_budgets`** (money in micro-USD)

```sql
CREATE TABLE IF NOT EXISTS sandbox_budgets (
    id                 TEXT PRIMARY KEY,
    profile            TEXT NOT NULL UNIQUE,
    period             TEXT NOT NULL DEFAULT 'daily',   -- 'daily' | 'weekly' | 'monthly'
    max_cost_micro_usd INTEGER NOT NULL,
    warn_pct           REAL NOT NULL DEFAULT 0.8,
    enabled            INTEGER NOT NULL DEFAULT 1,
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sb_profile ON sandbox_budgets(profile);
```

**Migration to `sandbox_runs` table: add `profile` column**

```sql
ALTER TABLE sandbox_runs ADD COLUMN profile TEXT;
```

Because `modernc.org/sqlite` does not support `ADD COLUMN IF NOT EXISTS` universally, the `ALTER` runs in Go and swallows the "already applied" case:

```go
func addColumn(ctx context.Context, db store.Querier, ddl string) error {
	_, err := db.ExecContext(ctx, ddl)
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	return nil // no-op if the column already exists
}
```

The `profile` column is populated by `RunInSandbox()` when a non-empty `profile` is passed (a new field on the `RunOptions` struct).

### 9.3 Core Algorithms

Time is sourced through an injected `Clock` interface (`Now() time.Time`) so tests are deterministic without a `freezegun` analogue; production uses a `realClock`.

```go
type Clock interface{ Now() time.Time }
```

**Duration computation:**

```go
// parseISO parses an ISO-8601 timestamp with an optional trailing Z.
func parseISO(ts string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, strings.Replace(ts, "Z", "+00:00", 1))
}

// ComputeDuration returns wall-clock duration in float64 seconds.
func ComputeDuration(startedAt, completedAt string) (float64, error) {
	start, err := parseISO(startedAt)
	if err != nil {
		return 0, fmt.Errorf("parse started_at: %w", err)
	}
	end, err := parseISO(completedAt)
	if err != nil {
		return 0, fmt.Errorf("parse completed_at: %w", err)
	}
	return end.Sub(start).Seconds(), nil
}
```

**Cost computation** (fixed-point micro-USD; single `math.Round` at the micro boundary):

```go
type CostResult struct {
	BilledSeconds float64
	CostMicroUSD  int64
	RateSource    string // "config_override" | "default"
}

// ComputeCost computes the billed cost for a sandbox run. overrideMicro is the
// per-second micro-USD rate from config (nil to use the default table rate).
func ComputeCost(durationSeconds float64, rate BackendRate, overrideMicro *int64) CostResult {
	rateMicro := rate.RateMicroUSDPerSec
	source := "default"
	if overrideMicro != nil {
		rateMicro = *overrideMicro
		source = "config_override"
	}
	billed := math.Max(durationSeconds, rate.MinimumChargeSeconds)
	cost := int64(math.Round(billed * float64(rateMicro)))
	return CostResult{BilledSeconds: billed, CostMicroUSD: cost, RateSource: source}
}
```

**Duration parsing for the `--since` flag** (uses the injected `Clock`):

```go
var (
	sinceRe      = regexp.MustCompile(`^(\d+(?:\.\d+)?)(s|m|h|d|w)$`)
	sinceUnitSec = map[string]float64{"s": 1, "m": 60, "h": 3600, "d": 86400, "w": 604800}
)

// ParseSince parses "7d", "2h", "30m" into an absolute cutoff time.
func ParseSince(clock Clock, value string) (time.Time, error) {
	m := sinceRe.FindStringSubmatch(strings.ToLower(strings.TrimSpace(value)))
	if m == nil {
		return time.Time{}, fmt.Errorf(
			"invalid --since value %q: expected format <N>(s|m|h|d|w), e.g. 7d, 2h, 30m", value)
	}
	n, _ := strconv.ParseFloat(m[1], 64)
	d := time.Duration(n * sinceUnitSec[m[2]] * float64(time.Second))
	return clock.Now().UTC().Add(-d), nil
}
```

**P50/P95 computation** (pure Go, no external stats lib):

```go
func percentile(values []float64, pct float64) float64 {
	if len(values) == 0 {
		return 0
	}
	s := append([]float64(nil), values...)
	sort.Float64s(s)
	idx := (pct / 100) * float64(len(s)-1)
	lo := int(idx)
	hi := lo + 1
	if hi > len(s)-1 {
		hi = len(s) - 1
	}
	frac := idx - float64(lo)
	return round3(s[lo] + frac*(s[hi]-s[lo]))
}
```

**`WriteCostRecord()`** — computes and persists a cost record for a completed run. It takes a `store.Querier` (so it can run inside the caller's `*sql.Tx`) and a `ConfigRates` map (backend name -> micro-USD/s, loaded from `tag.yaml sandbox.pricing`):

```go
type WrittenCost struct {
	RunID            string
	DurationSeconds  float64
	BilledSeconds    float64
	CostUSD          float64
	RatePerSecondUSD float64
	BillingModel     BillingModel
	RateSource       string
}

func WriteCostRecord(
	ctx context.Context,
	q store.Querier,
	clock Clock,
	runID, backend, profile, startedAt, completedAt string,
	configRates map[string]int64,
) (WrittenCost, error) {
	rate, ok := BackendRates[backend]
	if !ok {
		rate = BackendRates["restricted"]
	}
	var override *int64
	if v, ok := configRates[backend]; ok {
		override = &v
	}
	dur, err := ComputeDuration(startedAt, completedAt)
	if err != nil {
		return WrittenCost{}, err
	}
	res := ComputeCost(dur, rate, override)
	rateMicro := rate.RateMicroUSDPerSec
	if override != nil {
		rateMicro = *override
	}

	_, err = q.ExecContext(ctx,
		`INSERT OR IGNORE INTO sandbox_run_costs
		   (id, run_id, backend, profile, started_at, completed_at,
		    duration_seconds, rate_micro_usd_per_sec, minimum_charge_seconds,
		    billed_seconds, cost_micro_usd, billing_model, rate_source, created_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		newID(), runID, backend, nullStr(profile), startedAt, completedAt,
		round6(dur), rateMicro, rate.MinimumChargeSeconds,
		round6(res.BilledSeconds), res.CostMicroUSD,
		string(rate.BillingModel), res.RateSource, clock.Now().UTC().Format(isoMicros),
	)
	if err != nil {
		return WrittenCost{}, err
	}
	return WrittenCost{
		RunID:            runID,
		DurationSeconds:  round6(dur),
		BilledSeconds:    round6(res.BilledSeconds),
		CostUSD:          float64(res.CostMicroUSD) / 1e6,
		RatePerSecondUSD: float64(rateMicro) / 1e6,
		BillingModel:     rate.BillingModel,
		RateSource:       res.RateSource,
	}, nil
}
```

`INSERT OR IGNORE` plus the `UNIQUE(run_id)` constraint makes re-writes idempotent (NFR-04). `isoMicros` is the `2006-01-02T15:04:05.000000Z07:00` layout the golden harness pins.

### 9.4 Integration with `internal/sandbox`

`RunInSandbox()` takes a `RunOptions` struct; two fields are added:

```go
type RunOptions struct {
	Command     string
	Backend     string        // default "restricted"
	Image       string        // default "python:3.12-slim"
	Timeout     time.Duration // default 60s
	Workdir     string
	Profile     string           // NEW: budget check + cost attribution ("" = none)
	ConfigRates map[string]int64 // NEW: from tag.yaml sandbox.pricing (micro-USD/s)
}

func RunInSandbox(ctx context.Context, db *sql.DB, opt RunOptions) (RunResult, error)
```

Changes to `RunInSandbox()` body:

1. After `EnsureSchema(ctx, db)`, call `obs.EnsureCostSchema(ctx, db)`.
2. Before launching the backend, call `obs.CheckSandboxBudget(ctx, db, opt.Profile)` if `opt.Profile != ""` and return any `*SandboxBudgetExceededError` (wrapped) to the caller.
3. Store `opt.Profile` in the `sandbox_runs` INSERT.
4. After updating `sandbox_runs` with `completed_at`, call `obs.WriteCostRecord(...)` inside the same `*sql.Tx`.
5. Merge the returned cost fields into `RunResult`.

**Transaction safety:** the status update and the cost-record write share one `*sql.Tx`, so they commit atomically (NFR-05):

```go
tx, err := db.BeginTx(ctx, nil)
if err != nil {
	return RunResult{}, err
}
defer tx.Rollback() // no-op after Commit

if _, err = tx.ExecContext(ctx,
	`UPDATE sandbox_runs SET status=?, exit_code=?, output=?, completed_at=? WHERE id=?`,
	status, exitCode, truncate(output, 50000), completedAt, runID); err != nil {
	return RunResult{}, err
}

cost, err := obs.WriteCostRecord(ctx, tx, clock, runID, opt.Backend, opt.Profile, startedAt, completedAt, opt.ConfigRates)
if err != nil {
	return RunResult{}, err
}
if err = tx.Commit(); err != nil { // single commit covers both writes
	return RunResult{}, err
}
```

### 9.5 Integration with the `internal/obs` budget gate

Go uses error values instead of exceptions. `SandboxBudgetExceededError` implements `error` and is matchable with `errors.As`; there is no `warnings` analogue, so the warn condition is surfaced as a `BudgetStatus.Warn` flag plus a `slog.Warn` line.

```go
// SandboxBudgetExceededError is returned (wrapped) when a profile has exhausted
// its sandbox compute budget. Money in micro-USD.
type SandboxBudgetExceededError struct {
	Profile       string
	UsedMicroUSD  int64
	LimitMicroUSD int64
	Period        string
}

func (e *SandboxBudgetExceededError) Error() string {
	return fmt.Sprintf("sandbox compute budget exceeded for profile %q: $%.4f / $%.4f used (%s)",
		e.Profile, float64(e.UsedMicroUSD)/1e6, float64(e.LimitMicroUSD)/1e6, e.Period)
}

type BudgetStatus struct {
	Allowed       bool
	HasBudget     bool
	Profile       string
	UsedMicroUSD  int64
	LimitMicroUSD int64
	Period        string
	Pct           float64
	Warn          bool
}
```

**`CheckSandboxBudget()`** — mirrors the shape of the token `CheckBudget()` gate:

```go
func CheckSandboxBudget(ctx context.Context, db store.Querier, clock Clock, profile string) (BudgetStatus, error) {
	if err := EnsureCostSchema(ctx, db); err != nil {
		return BudgetStatus{}, err
	}
	var (
		limitMicro int64
		warnPct    float64
		period     string
		enabled    bool
	)
	err := db.QueryRowContext(ctx,
		`SELECT max_cost_micro_usd, warn_pct, period, enabled FROM sandbox_budgets WHERE profile=?`,
		profile).Scan(&limitMicro, &warnPct, &period, &enabled)
	if errors.Is(err, sql.ErrNoRows) || !enabled {
		return BudgetStatus{Allowed: true, HasBudget: false}, nil
	}
	if err != nil {
		return BudgetStatus{}, err
	}

	windowStart := windowStart(clock, period)
	var usedMicro int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_micro_usd), 0)
		   FROM sandbox_run_costs WHERE profile=? AND started_at >= ?`,
		profile, windowStart).Scan(&usedMicro); err != nil {
		return BudgetStatus{}, err
	}

	pct := 0.0
	if limitMicro > 0 {
		pct = float64(usedMicro) / float64(limitMicro)
	}
	st := BudgetStatus{
		Allowed: true, HasBudget: true, Profile: profile,
		UsedMicroUSD: usedMicro, LimitMicroUSD: limitMicro, Period: period,
		Pct: round1(pct * 100),
	}
	if pct >= 1.0 {
		st.Allowed = false
		return st, &SandboxBudgetExceededError{profile, usedMicro, limitMicro, period}
	}
	if pct >= warnPct {
		st.Warn = true
		slog.Warn("sandbox budget threshold reached",
			"profile", profile, "pct", int(pct*100),
			"used_usd", float64(usedMicro)/1e6, "limit_usd", float64(limitMicro)/1e6, "period", period)
	}
	return st, nil
}
```

### 9.6 Query Functions for the `sandbox costs` command

Filters are assembled into a `[]string` of `?`-parameterised clauses plus a `[]any` args slice (never string-interpolated values), and results scan into typed structs. USD floats are derived from the stored micro-USD integers.

```go
type CostFilter struct {
	Since       *time.Time
	RunID       string
	Backend     string
	Profile     string
	MinCostUSD  float64
	Limit       int
}

type CostRow struct {
	RunID            string  `json:"run_id"`
	Backend          string  `json:"backend"`
	Profile          string  `json:"profile,omitempty"`
	StartedAt        string  `json:"started_at"`
	CompletedAt      string  `json:"completed_at"`
	DurationSeconds  float64 `json:"duration_seconds"`
	BilledSeconds    float64 `json:"billed_seconds"`
	RatePerSecondUSD float64 `json:"rate_per_second_usd"`
	CostUSD          float64 `json:"cost_usd"`
	BillingModel     string  `json:"billing_model"`
	MinChargeSeconds float64 `json:"minimum_charge_seconds"`
	Command          string  `json:"command"`
	Image            string  `json:"image"`
	Status           string  `json:"status"`
	ExitCode         int     `json:"exit_code"`
}

func QueryCosts(ctx context.Context, db store.Querier, f CostFilter) ([]CostRow, error) {
	var clauses []string
	var args []any
	if f.Since != nil {
		clauses = append(clauses, "c.started_at >= ?")
		args = append(args, f.Since.Format(isoMicros))
	}
	if f.RunID != "" {
		clauses = append(clauses, "c.run_id = ?")
		args = append(args, f.RunID)
	}
	if f.Backend != "" {
		clauses = append(clauses, "c.backend = ?")
		args = append(args, f.Backend)
	}
	if f.Profile != "" {
		clauses = append(clauses, "c.profile = ?")
		args = append(args, f.Profile)
	}
	if f.MinCostUSD > 0 {
		clauses = append(clauses, "c.cost_micro_usd >= ?")
		args = append(args, int64(math.Round(f.MinCostUSD*1e6)))
	}
	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	args = append(args, limit)

	rows, err := db.QueryContext(ctx, `
		SELECT c.run_id, c.backend, COALESCE(c.profile,''),
		       c.started_at, c.completed_at,
		       c.duration_seconds, c.billed_seconds, c.minimum_charge_seconds,
		       c.rate_micro_usd_per_sec, c.cost_micro_usd,
		       c.billing_model,
		       r.command, r.image, r.status, r.exit_code
		  FROM sandbox_run_costs c
		  JOIN sandbox_runs r ON r.id = c.run_id
		  `+where+`
		 ORDER BY c.started_at DESC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CostRow
	for rows.Next() {
		var (
			cr                 CostRow
			rateMicro, costMic int64
		)
		if err := rows.Scan(&cr.RunID, &cr.Backend, &cr.Profile,
			&cr.StartedAt, &cr.CompletedAt,
			&cr.DurationSeconds, &cr.BilledSeconds, &cr.MinChargeSeconds,
			&rateMicro, &costMic, &cr.BillingModel,
			&cr.Command, &cr.Image, &cr.Status, &cr.ExitCode); err != nil {
			return nil, err
		}
		cr.RatePerSecondUSD = float64(rateMicro) / 1e6
		cr.CostUSD = float64(costMic) / 1e6
		out = append(out, cr)
	}
	return out, rows.Err()
}
```

`QueryCostsByBackend()` groups by backend and computes p50/p95 durations in Go via `percentile()` (a second per-backend `SELECT duration_seconds` feeds the sorted slice), returning `[]BackendAgg` with `TotalCostUSD`/`AvgCostUSD` derived from `SUM`/`AVG` of `cost_micro_usd` divided by `1e6`. The aggregate query and the per-backend duration query are both fully parameterised.

### 9.7 CLI Integration (`internal/cli`, `cobra`)

`tag sandbox costs` is a `cobra` subcommand under the `sandbox` command group, following the existing `sandbox *` pattern. Flags are bound on the command's `*pflag.FlagSet`; the handler is the `RunE` closure (returning `error`, not an int exit code — `cobra`/root maps errors to exit status). Output structs are marshalled with `encoding/json`.

```go
func newSandboxCostsCmd(app *App) *cobra.Command {
	var f struct {
		since, runID, by, backend, profile string
		minCost                            float64
		limit                              int
		asJSON                             bool
	}
	cmd := &cobra.Command{
		Use:   "costs",
		Short: "Report per-second costs for sandbox runs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := obs.EnsureCostSchema(ctx, app.DB); err != nil {
				return err
			}

			var since *time.Time
			if f.since != "" {
				t, err := obs.ParseSince(app.Clock, f.since)
				if err != nil {
					return err // cobra prints "Error: ..." to stderr, exit 1
				}
				since = &t
			}
			period := obs.Period{Since: isoPtr(since), Until: app.Clock.Now().UTC().Format(isoMicros)}

			if f.by == "backend" {
				aggs, err := obs.QueryCostsByBackend(ctx, app.DB, since, f.profile)
				if err != nil {
					return err
				}
				if f.asJSON {
					return writeJSON(cmd.OutOrStdout(), obs.ByBackendReport{Period: period, ByBackend: aggs})
				}
				return printByBackendTable(cmd.OutOrStdout(), aggs)
			}

			rows, err := obs.QueryCosts(ctx, app.DB, obs.CostFilter{
				Since: since, RunID: f.runID, Backend: f.backend,
				Profile: f.profile, MinCostUSD: f.minCost, Limit: f.limit,
			})
			if err != nil {
				return err
			}
			if len(rows) == 0 && !f.asJSON {
				fmt.Fprintln(cmd.OutOrStdout(),
					"No sandbox cost records found. Run 'tag sandbox run' to generate cost data.")
				return nil
			}
			if f.asJSON {
				var totalCost, totalDur float64
				for _, r := range rows {
					totalCost += r.CostUSD
					totalDur += r.DurationSeconds
				}
				return writeJSON(cmd.OutOrStdout(), obs.CostReport{
					Period: period,
					Summary: obs.CostSummary{
						TotalRuns: len(rows), TotalCostUSD: round6(totalCost), TotalDurationSeconds: round3(totalDur),
					},
					Runs: rows,
				})
			}
			return printCostsTable(cmd.OutOrStdout(), rows)
		},
	}
	fs := cmd.Flags()
	fs.StringVar(&f.since, "since", "", "time window, e.g. 1h, 7d, 30d")
	fs.StringVar(&f.runID, "run-id", "", "filter to a single run")
	fs.StringVar(&f.by, "by", "", "aggregate by (backend)")
	fs.StringVar(&f.backend, "backend", "", "filter to a backend")
	fs.StringVar(&f.profile, "profile", "", "filter to a profile")
	fs.Float64Var(&f.minCost, "min-cost", 0, "minimum cost in USD")
	fs.IntVar(&f.limit, "limit", 50, "max rows in table output")
	fs.BoolVar(&f.asJSON, "json", false, "machine-readable JSON output")
	return cmd
}
```

`writeJSON` uses `json.MarshalIndent(v, "", "  ")`. The `App` struct carries the `*sql.DB` handle, the injected `Clock`, and the koanf-loaded config; the `sandbox costs`, `sandbox list`, and `sandbox run` commands share it.

### 9.8 `tag.yaml` Schema Addition (loaded via `knadh/koanf/v2`)

The `sandbox` block is read from `~/.tag/tag.yaml` through the existing `koanf` config layer (yaml.v3 provider) and unmarshalled into a typed `SandboxConfig` struct. Rates are declared in USD for author convenience and parsed into `int64` micro-USD (`round(usd * 1e6)`) on load; a negative rate is rejected at load time.

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

```go
type SandboxConfig struct {
	Pricing map[string]float64                 `koanf:"pricing"` // backend -> USD/s (parsed to micro-USD)
	Budgets map[string]SandboxBudgetConfig     `koanf:"budgets"`
}
type SandboxBudgetConfig struct {
	Period     string  `koanf:"period"`
	MaxCostUSD float64 `koanf:"max_cost_usd"`
	WarnPct    float64 `koanf:"warn_pct"`
}
```

---

## 10. Security Considerations

1. **No secrets in cost records:** `sandbox_run_costs` stores only timing, rate, and cost data. The `command` field is stored in `sandbox_runs`, not duplicated in `sandbox_run_costs`. The join for reporting is read-only. No credentials or environment variables appear in the cost table.

2. **Rate override validation:** `sandbox.pricing` values loaded from `tag.yaml` must be validated as non-negative before use; the koanf unmarshal step rejects negative rates (which would produce negative costs, confusing budget checks) with a clear config error, before any micro-USD conversion.

3. **SQLite injection prevention:** All queries use parameterized statements (`?` placeholders bound through `database/sql`). No string interpolation of values is used in SQL construction; only the `WHERE`-clause skeleton is assembled from a whitelist-validated set of column names and operators, with all values passed as `args ...any`.

4. **Budget bypass prevention:** `CheckSandboxBudget()` is called inside `RunInSandbox()` before any backend process is launched. Code paths that bypass `RunInSandbox()` (e.g., direct `os/exec` calls in `internal/cli`) are not protected by this gate — a follow-on security audit should enumerate all such call sites.

5. **File permission of `tag.sqlite3`:** The existing database at `~/.tag/runtime/tag.sqlite3` is created with mode `0600` by `internal/store` on open. The new `sandbox_run_costs` and `sandbox_budgets` tables inherit this file's permissions. No additional file permission changes are needed.

6. **Denial-of-service via budget bypass:** A misconfigured `warn_pct` of 1.0 (identical to the hard limit) would result in no warning before the hard limit fires. Validation must enforce `0.0 < warn_pct < 1.0` with a minimum gap of 0.01 (i.e., warn at no later than 99%).

---

## 11. Testing Strategy

Tests use the Go `testing` package with table-driven cases, an injected fake `Clock` for determinism (no `freezegun`), and an in-memory `modernc.org/sqlite` DB (`sql.Open("sqlite", ":memory:")`) seeded via helpers. Backends and notifiers are behind interfaces so no real sandbox runs.

### 11.1 Unit Tests (`internal/obs/cost_test.go`)

```go
func TestComputeDuration(t *testing.T) {
	d, err := ComputeDuration("2026-06-15T14:00:00.000Z", "2026-06-15T14:00:12.432Z")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(d-12.432) > 0.001 {
		t.Fatalf("got %v, want ~12.432", d)
	}
}

func TestComputeCost(t *testing.T) {
	tests := []struct {
		name        string
		dur         float64
		rate        BackendRate
		override    *int64
		wantBilled  float64
		wantMicro   int64
		wantSource  string
	}{
		{"e2b minimum charge applies", 0.3, BackendRates["e2b"], nil, 1.0, 160, "default"},
		{"docker no minimum", 0.5, BackendRates["docker"], nil, 0.5, 14, "default"}, // round(0.5*28)
		{"e2b config override", 10.0, BackendRates["e2b"], ptr(int64(200)), 10.0, 2000, "config_override"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeCost(tt.dur, tt.rate, tt.override)
			if got.BilledSeconds != tt.wantBilled || got.CostMicroUSD != tt.wantMicro || got.RateSource != tt.wantSource {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestWriteCostRecordIdempotent(t *testing.T) {
	db := newMemDB(t)
	clk := fakeClock{t0}
	seedSandboxRun(t, db, "run001", "docker")
	for i := 0; i < 2; i++ {
		if _, err := WriteCostRecord(ctx, db, clk, "run001", "docker", "",
			"2026-06-15T14:00:00Z", "2026-06-15T14:00:12Z", nil); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, db, `SELECT COUNT(*) FROM sandbox_run_costs WHERE run_id='run001'`); n != 1 {
		t.Fatalf("INSERT OR IGNORE broke: got %d rows", n)
	}
}

func TestCheckSandboxBudgetExceeded(t *testing.T) {
	db := newMemDB(t)
	setSandboxBudget(t, db, "coder", /*max micro-USD*/ 1000, "daily")
	seedCostRecord(t, db, "run001", "coder", /*micro-USD*/ 2000)
	_, err := CheckSandboxBudget(ctx, db, fakeClock{t0}, "coder")
	var budgetErr *SandboxBudgetExceededError
	if !errors.As(err, &budgetErr) {
		t.Fatalf("want SandboxBudgetExceededError, got %v", err)
	}
}

func TestParseSince(t *testing.T) {
	clk := fakeClock{t0}
	for _, tt := range []struct {
		in   string
		secs float64
	}{{"30s", 30}, {"5m", 300}, {"2h", 7200}, {"7d", 604800}, {"1w", 604800}} {
		got, err := ParseSince(clk, tt.in)
		if err != nil {
			t.Fatal(err)
		}
		if d := clk.Now().UTC().Sub(got).Seconds(); math.Abs(d-tt.secs) > 1 {
			t.Fatalf("%s: got %v", tt.in, d)
		}
	}
	if _, err := ParseSince(clk, "3x"); err == nil {
		t.Fatal("expected error for invalid --since")
	}
}
```

Also table-driven: `BackfillCosts` generates one record per completed run (and is idempotent on re-run).

### 11.2 Integration Tests (`internal/sandbox/costs_integration_test.go`)

- Run `RunInSandbox()` with `Backend:"restricted"` and verify a `sandbox_run_costs` row with `cost_micro_usd=0`.
- Run `RunInSandbox()` against a fake Docker backend (interface impl) and verify cost is computed from timestamps.
- Invoke the `sandbox costs` cobra command with `--json` (via `cmd.SetArgs` + captured `OutOrStdout()`) and `json.Unmarshal` the output into the typed report, asserting the top-level keys.
- Run `BackfillCosts()` on a pre-seeded DB with 50 `sandbox_runs` rows; verify 50 cost records and no duplicates.
- Set a sandbox budget, run enough sandbox runs to exhaust it, and verify the next `RunInSandbox()` returns an error matching `*SandboxBudgetExceededError` via `errors.As`.

### 11.3 Performance Tests (`internal/obs/cost_bench_test.go`)

```go
func BenchmarkQueryCosts10k(b *testing.B) {
	db := newMemDB(b)
	seedNCostRecords(b, db, 10_000)
	since := t0.Add(-30 * 24 * time.Hour)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := QueryCosts(ctx, db, CostFilter{Since: &since, Limit: 50})
		if err != nil || len(rows) > 50 {
			b.Fatal(err)
		}
	}
}
```

The NFR-01 budget (< 200ms per query for 10k rows) is asserted from `b.Elapsed()/b.N` in a wrapper test, or via `testing.Benchmark(...).NsPerOp()` compared to the threshold.

---

## 12. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|--------------|
| AC-01 | `RunInSandbox()` `RunResult` includes `CostUSD`, `DurationSeconds`, `RatePerSecondUSD` for all backends | Unit test: assert fields populated on the returned struct |
| AC-02 | A completed sandbox run always has exactly one row in `sandbox_run_costs` | Integration test: SELECT COUNT after run |
| AC-03 | `tag sandbox costs --since 7d` produces a valid human-readable table with a total cost summary line | Manual + snapshot test |
| AC-04 | `tag sandbox costs --since 7d --json` produces valid JSON with `period`, `summary`, and `runs` top-level keys | `json.Unmarshal` into the typed report in a CI test |
| AC-05 | `tag sandbox costs --run-id <id>` shows exactly one row for a known run | Integration test |
| AC-06 | `tag sandbox costs --by backend --json` produces valid JSON with `by_backend` array, each entry having `p50_duration_seconds` and `p95_duration_seconds` | Schema validation test |
| AC-07 | E2B run of 0.3s is billed for 1.0s (minimum charge), not 0.3s | Unit test on `ComputeCost()` |
| AC-08 | Restricted backend run always shows `$0.000000` in cost output | Unit test + integration test |
| AC-09 | Config override in `tag.yaml sandbox.pricing.e2b` is used instead of default | Unit test with an injected `ConfigRates` map |
| AC-10 | `backfill_costs()` populates cost records for all pre-existing `sandbox_runs` with `completed_at` | Integration test on seeded DB |
| AC-11 | `ErrSandboxBudgetExceeded` is returned before backend launch when profile is over its sandbox dollar cap | Unit test: fake backend's launch method never called after the error |
| AC-12 | `tag sandbox costs` with no records shows the empty-state message, exit 0 | Integration test |
| AC-13 | `--since` with invalid format (e.g., `--since 3x`) exits 1 with a message containing "Expected format" | Integration test: subprocess exit code + stderr |
| AC-14 | `sandbox_run_costs` write and `sandbox_runs` status update are committed atomically (single `tx.Commit()`) | Code review + unit test: inject a failure between the two `ExecContext` calls and assert rollback |
| AC-15 | `tag sandbox list` shows a `cost` column with `—` for runs without cost records | Integration test on a pre-schema-migration DB |
| AC-16 | The cost-attribution unit imports only the Go stdlib plus `modernc.org/sqlite` (driver) and `knadh/koanf/v2` — no cloud SDK | Import audit in CI: `go list -deps ./internal/obs` check |

---

## 13. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| PRD-028 (Sandbox Code Execution) | Hard upstream | `sandbox_runs` table and `RunInSandbox()` API (`internal/sandbox`) must exist |
| PRD-039 (Token Budget Enforcement) | Design reference | `sandbox_budgets` mirrors `token_budgets` schema; `CheckSandboxBudget()` mirrors the `CheckBudget()` gate |
| PRD-012 (Cost Tracking & Budget) | Design reference | `tag costs` command pattern; `tag sandbox costs` follows the same display conventions; shares `internal/obs` money helpers (`pricing.go`, micro-USD) |
| PRD-013 (Agent Tracing & Observability) | Optional integration | `internal/obs` (go.opentelemetry.io/otel) spans could carry a `sandbox.cost_usd` attribute for OTLP export; deferred |
| `internal/store` (`modernc.org/sqlite`) | Runtime | Pure-Go, CGO_ENABLED=0, WAL SQLite handle; `EnsureCostSchema()` must run before any reads/writes |
| Go stdlib `time` | Runtime | `time.Parse(time.RFC3339Nano, …)` for ISO timestamps; a trailing `Z` is normalised to `+00:00` before parsing |
| `knadh/koanf/v2` config layer (`internal/config`) | Runtime | Unmarshals `sandbox.pricing` into a `ConfigRates` (micro-USD) map passed to `RunInSandbox()` when present |
| `spf13/cobra` (`internal/cli`) | Runtime | `sandbox costs` subcommand + flag binding (replaces argparse) |

---

## 14. Open Questions

| ID | Question | Owner | Resolution Path |
|----|----------|-------|-----------------|
| OQ-01 | Should `restricted` backend default to `$0.000000/s` always, or should it inherit a configurable "local CPU cost" to support teams that want to track internal compute cost allocation? | Engineering | Default to $0.00; add `local_cpu_rate_per_second_usd` as optional config key; document use case |
| OQ-02 | Modal pricing varies by resource type (CPU, GPU A10G, H100, etc.). Should the `modal` backend rate in the rate table be a single flat rate, or should it accept a `resource_type` tag on the sandbox run? | Product | V1: single flat rate. V2: `RunInSandbox()` accepts an optional `ResourceType` field on `RunOptions` that selects from a sub-table of Modal GPU rates |
| OQ-03 | Should `tag sandbox costs` be able to query cost by `task_id` (cross-referencing the `tasks` table) so that per-task total cost includes both LLM token cost and sandbox compute cost? | Engineering | Depends on PRD-012 `tasks` table schema; coordinate with cost tracking work |
| OQ-04 | What happens when a sandbox run is killed (status='killed') before it writes `completed_at`? Should `completed_at` be written to `sandbox_runs` on kill, and should a cost record be written for the partial run? | Engineering | Yes: record `completed_at=now` on kill; attribute cost for actual elapsed time regardless of exit path |
| OQ-05 | Rate table values will become stale as E2B and Modal update pricing. Should the default rates be fetched from a remote manifest (e.g., `https://tag.ai/sandbox-rates.json`) with a 24h TTL and local cache? | Product | V1: hardcoded defaults + config override; V2: optional remote rate manifest with fallback |
| OQ-06 | Should `tag sandbox costs --export csv` be supported for spreadsheet import? | Product | Out of scope for V1; `--json` piped to `jq` covers this; add `--csv` if user demand materializes |

---

## 15. Complexity and Timeline

**Overall estimate:** S (3–5 days for a single engineer)

### Phase 1 — Schema + Core Cost Engine (Day 1)

- Create `internal/obs/cost.go` with `BackendRates`, the `BackendRate`/`BillingModel` types, `EnsureCostSchema()`, `ComputeDuration()`, `ComputeCost()`, `WriteCostRecord()`, `BackfillCosts()`, and the `Clock` interface
- Add `sandbox_run_costs` and `sandbox_budgets` DDL (micro-USD integer columns)
- Add the `ALTER TABLE sandbox_runs ADD COLUMN profile TEXT` migration via the `addColumn` helper (idempotent — swallows `duplicate column name`)
- Table-driven unit tests for all computation functions (`internal/obs/cost_test.go`)

### Phase 2 — Sandbox Integration (Day 2)

- Update `internal/sandbox`: add `Profile` and `ConfigRates` fields to `RunOptions`
- Call `obs.EnsureCostSchema(ctx, db)` from `sandbox.EnsureSchema()`
- Add the `obs.CheckSandboxBudget()` pre-flight in `RunInSandbox()`
- Add cost fields to `RunResult`; write the cost record inside the status-update `*sql.Tx`
- Add cost column to the sandbox-list query via `LEFT JOIN`
- Integration tests: `RunInSandbox()` produces cost records for all backends; `errors.As(err, &*SandboxBudgetExceededError)` fires correctly

### Phase 3 — CLI Surface (Day 3)

- Add the `sandbox costs` `cobra` subcommand (`internal/cli`) with flag binding
- Implement `QueryCosts()`, `QueryCostsByBackend()` in `internal/obs`
- Implement `ParseSince()` with full table-driven coverage
- Implement `printCostsTable()` and `printByBackendTable()` writers (to `cmd.OutOrStdout()`)
- Augment `tag sandbox run` post-completion output with the cost line
- Augment `tag sandbox list` with the cost column

### Phase 4 — Budget Management + Config (Day 4)

- Implement `SetSandboxBudget()`, `RemoveSandboxBudget()`, `CheckSandboxBudget()` in `internal/obs`
- Add koanf unmarshalling of `sandbox.pricing` and `sandbox.budgets` from `tag.yaml` (USD -> micro-USD, negative-rate rejection)
- Wire config override into the `sandbox run` command -> `RunInSandbox(RunOptions{ConfigRates: …})`
- Integration test: budget sets `Status.Warn` at `warn_pct`, returns the error at the cap

### Phase 5 — Polish + Performance (Day 5)

- Benchmark: 10,000-record query under 200ms (`go test -bench`)
- `BackfillCosts()` integration test on a seeded SQLite file
- JSON contract tests via `json.Unmarshal` into typed structs (key assertions)
- Documentation: `--help`/`Short`/`Long` text for all new flags, doc comments on all exported symbols
- Update `docs/prd/INDEX.md` to add PRD-099
- Code review and merge

