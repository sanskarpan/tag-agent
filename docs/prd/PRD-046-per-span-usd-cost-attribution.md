# PRD-046: Per-Span USD Cost Attribution (`tag trace show --cost / tag stats --cost`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** XS (1-2 days)
**Category:** Evaluation & Observability
**Affects:** `cost_table.py + otel_semconv.py`
**Depends on:** PRD-013 (agent tracing/observability), PRD-012 (cost tracking & budget), PRD-037 (OTel GenAI span cost attribution), PRD-028 (sandbox code execution), PRD-034 (secret scanning)
**Inspired by:** LangSmith, W&B Weave, Braintrust, E2B billing

---

## 1. Overview

TAG already records spans for every agent inference step (PRD-013) and estimates run-level costs from OpenRouter's pricing catalog (PRD-012). However, these two systems are siloed: the `spans` table contains token counts but no dollar figures, and the `runs` table contains a coarse `estimated_cost_usd` that cannot be drilled down by individual LLM call, tool, or model. When a long `tag swarm` run costs $3.47, there is no way to answer "which span consumed the most budget?" or "how much did the `researcher` profile spend vs. the `coder` profile in the same run?"

This PRD introduces **per-span USD cost attribution**: a new `cost_table.py` module loads a bundled (and user-overridable) YAML pricing table keyed by model ID, computes a USD cost for every span at close time using `cost = (input_tokens × in_price_per_1m / 1_000_000) + (output_tokens × out_price_per_1m / 1_000_000)`, stores the result as a `cost_usd` column on the `spans` table, and surfaces it through two enhanced CLI surfaces: `tag trace show --cost` (per-span cost waterfall view) and `tag stats --cost --by model` (cross-run cost aggregation). Additionally, a `tag budget set` command enforces per-run USD hard limits at the profile level, and `tag costs --run-id <id> --json` provides machine-readable cost output for CI pipelines and billing dashboards.

The pricing table is a plain YAML file (`~/.tag/pricing.yaml`) seeded from a bundled default (`src/tag/assets/pricing.yaml`) that ships with the package. It stores `input_usd_per_1m_tokens` and `output_usd_per_1m_tokens` for each model ID, supports wildcard prefix matching (e.g. `claude-*`), and includes cache-read discount multipliers matching Anthropic's 0.1× rate and batch discount of 0.5×. Users and enterprises can extend or override the table without touching package code.

This feature is rated Difficulty 1/5 because the computational core is a straightforward arithmetic formula applied at `close_span()`, the storage change is a single `ALTER TABLE` migration (one new `REAL` column), and the pricing YAML is already a well-understood pattern in tools like LangSmith and Braintrust. The Impact is 4/5 because cost visibility at the span level is the single most requested observability feature by engineering teams managing multi-agent workloads, directly enabling cost optimization, budget enforcement, and chargeback reporting.

The design is deliberately local-first: no external pricing API is called at runtime. The bundled YAML ships known prices for all major Anthropic, OpenAI, Google Gemini, Mistral, and DeepSeek models as of June 2026. The user can run `tag pricing update` to pull the latest prices from a community-maintained YAML registry, but this is a voluntary, offline-safe workflow. All pricing lookups are dictionary-based (O(1)) with graceful fallback to `cost_usd = null` for unknown models.

---

## 2. Problem Statement

### 2.1 Token counts without dollar context are hard to reason about

TAG's `tag trace show <run-id>` currently displays `prompt_tokens` and `completion_tokens` for each span, but without the model's price per million tokens these numbers are nearly useless for financial reasoning. A span with 50,000 prompt tokens costs $0.15 on `claude-haiku-4-5` ($3/1M) but $1.50 on `claude-sonnet-4-6` ($30/1M input) — a 10× difference invisible in the current output. Engineers copy-paste token counts into a spreadsheet and multiply manually, a workflow that breaks immediately when models change.

### 2.2 Run-level cost aggregation misses attribution granularity

PRD-012 introduced `estimated_cost_usd` on the `runs` table, giving a single dollar number per run. This is useful for monthly spend reports but insufficient for cost optimization. A $5 run might contain one expensive planning span and dozens of cheap tool spans — but without per-span costs, the engineer cannot identify and eliminate the outlier. LangSmith, W&B Weave, and Braintrust all solved this at the span level years ago; TAG should close the gap.

### 2.3 No mechanism to prevent runaway per-run costs at enforcement time

PRD-012 proposed profile-level budget limits, but enforcement is coarse (it fires at run completion when the budget is already exceeded). With per-span `cost_usd` computed at `close_span()` time, a running cumulative total is available during the run, enabling true hard stops: when the cumulative `sum(cost_usd)` for the current trace exceeds the profile's `limit_usd_per_run`, the agent can be aborted cleanly before additional LLM calls are made. This is exactly how E2B bills and enforces sandbox compute budgets.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | A new `cost_table.py` module loads a YAML pricing table at startup and exposes a pure `compute_cost(model_id, input_tokens, output_tokens, *, cache_read=False, batch=False) -> float \| None` function. |
| G2 | `close_span()` in `tracing.py` accepts and stores a `cost_usd: float \| None` field; the existing call sites in `controller.py` are updated to pass the computed cost. |
| G3 | The `spans` table gains a `cost_usd REAL` column via a non-destructive `ALTER TABLE` migration in `open_db()`. |
| G4 | `tag trace show <run-id> --cost` displays a per-span cost waterfall: span name, model, tokens (in/out), and USD cost, plus a subtotal row. |
| G5 | `tag stats --cost --since <duration> --by <field> [--json]` aggregates cost from `spans` across runs, grouped by model, profile, or date. |
| G6 | `tag budget set --profile <name> --limit-usd <amount> --per-run` writes the limit to the profile config and enforces it inside the agent execution loop. |
| G7 | `tag costs --run-id <id> [--json]` emits an itemized cost breakdown for a single run, machine-readable for CI/billing pipelines. |
| G8 | The bundled `src/tag/assets/pricing.yaml` ships prices for ≥ 30 model IDs covering Anthropic, OpenAI, Google, Mistral, and DeepSeek families. |
| G9 | `tag pricing show [--model <id>]` displays the active pricing table so users can verify what rates are being applied. |
| G10 | Unknown models produce `cost_usd = null` (not an error); `tag trace show --cost` renders these as `—` and excludes them from subtotals. |

---

## 4. Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Real-time billing integration with Anthropic, OpenAI, or any provider API. Prices are static YAML; actual billing is the provider's source of truth. |
| NG2 | Retroactive cost computation for spans already in the database before this PRD ships. Old spans have `cost_usd = null`; there is no backfill job. |
| NG3 | Per-token streaming cost updates during an active span. Cost is computed once at `close_span()`. |
| NG4 | Currency conversion. All costs are in USD; multi-currency display is out of scope. |
| NG5 | Automatic pricing YAML updates. `tag pricing update` is a user-triggered command; no background fetching. |
| NG6 | Cost attribution for tool spans (non-LLM spans) beyond what is already on the span (e.g., E2B sandbox compute). Tool costs require provider-specific integrations tracked separately. |
| NG7 | Modifying the AgentOps bridge (PRD-044) to consume `cost_usd` from spans. That PRD already reads `cost_usd` from LLM events; the bridge is updated independently. |

---

## 5. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Span cost coverage | ≥ 95% of `claude-*` and `gpt-4*` spans have non-null `cost_usd` after `close_span()` | Query `spans` table: `SELECT COUNT(*) WHERE model_id LIKE 'claude%' AND cost_usd IS NULL` |
| Pricing lookup latency | < 0.1 ms per `compute_cost()` call (pure dict lookup, no I/O) | `timeit` benchmark on 10,000 calls |
| `tag trace show --cost` render time | < 200 ms for a 200-span trace from SQLite query to terminal output | Integration test with synthetic 200-span fixture |
| Budget enforcement accuracy | Hard-stop fires within 1 LLM call after cumulative cost crosses `limit_usd_per_run` | Integration test: set $0.01 limit, run agent that would cost $0.05; assert abort at first over-limit span |
| Migration safety | `ALTER TABLE` migration on a live DB with 10,000 existing spans completes in < 1 second | Measured in CI against a seeded fixture DB |
| Pricing table coverage | Bundled YAML covers ≥ 30 distinct model IDs | `len(load_pricing_table())` assertion in unit tests |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|------------|----------|
| U1 | Developer | run `tag trace show abc123 --cost` after a long swarm run | I can identify which specific span consumed the most budget and optimize it |
| U2 | Engineering manager | run `tag stats --cost --since 30d --by model --json` | I can produce a monthly model cost breakdown for the finance team without manual spreadsheet work |
| U3 | DevOps engineer | run `tag budget set --profile researcher --limit-usd 2.00 --per-run` | The researcher profile auto-aborts if it exceeds $2 per run, preventing surprise bills |
| U4 | Platform engineer | run `tag costs --run-id abc123 --json` in a CI pipeline | I can assert that a test run's cost is below a threshold and fail the build if it is not |
| U5 | Developer | run `tag pricing show --model claude-sonnet-4-6` | I can verify TAG is using the correct price before trusting the cost output |
| U6 | Security engineer | run `tag pricing show` and see that unknown models show `—` for cost | I know the system fails gracefully for internal or experimental models not yet in the pricing table |
| U7 | Developer using caching | pass `--cache-read` flag or have Anthropic cache hits auto-detected | Cached prompt reads are correctly priced at 0.1× the input rate, not at full price |
| U8 | Developer | run `tag stats --cost --since 7d --by profile` | I can do per-profile chargeback reporting across all runs in the past week |
| U9 | Developer | run `tag costs --run-id <id>` without `--json` | I see a human-readable table with span name, model, tokens, and cost per span, formatted for TTY |

---

## 7. Proposed CLI Surface

### 7.1 `tag trace show --cost`

Display a cost-annotated span waterfall for a single run.

```
tag trace show --run-id <id> [--cost] [--json] [--min-cost-usd <float>]
```

**Options:**
- `--cost`: Append cost columns to the standard span table. Without this flag, the existing span display is unchanged.
- `--min-cost-usd <float>`: Filter to only show spans whose `cost_usd >= <float>`. Useful for large traces.
- `--json`: Output the full span list as JSON with `cost_usd` fields.

**Example TTY output (`tag trace show --run-id abc123 --cost`):**

```
Trace: abc123  profile: researcher  run_at: 2026-06-17T10:14:02Z
────────────────────────────────────────────────────────────────────────────────────
SPAN                         MODEL                  IN TOK   OUT TOK      COST USD
────────────────────────────────────────────────────────────────────────────────────
run:researcher               —                           —        —              —
  step:plan                  claude-sonnet-4-6       4,210    1,024         $0.0434
  tool_call:web_search       —                           —        —              —
  step:summarize             claude-haiku-4-5        2,100      512         $0.0040
  step:write_report          claude-sonnet-4-6       8,102    3,204         $0.2040
────────────────────────────────────────────────────────────────────────────────────
TOTAL                                               14,412    4,740         $0.2514
────────────────────────────────────────────────────────────────────────────────────
```

**Example `--json` output snippet:**

```json
{
  "run_id": "abc123",
  "profile": "researcher",
  "total_cost_usd": 0.2514,
  "spans": [
    {
      "id": "a1b2c3d4e5f6",
      "name": "step:plan",
      "model_id": "claude-sonnet-4-6",
      "prompt_tokens": 4210,
      "completion_tokens": 1024,
      "cost_usd": 0.0434,
      "started_at": "2026-06-17T10:14:02.111Z",
      "duration_ms": 3201,
      "status": "ok"
    }
  ]
}
```

### 7.2 `tag stats --cost`

Aggregate cost across multiple runs with grouping and time filtering.

```
tag stats --cost [--since <duration>] [--until <date>] [--by <field>] [--profile <name>] [--json]
```

**Options:**
- `--cost`: Required flag to activate cost aggregation mode.
- `--since <duration>`: Time window: `7d`, `30d`, `90d`, `2026-06-01` (ISO date). Default: `30d`.
- `--until <date>`: Upper bound for time window. Default: now.
- `--by <field>`: Grouping field. Valid values: `model`, `profile`, `day`, `week`. May be specified multiple times for multi-level grouping (e.g. `--by profile --by model`).
- `--profile <name>`: Filter to a specific profile.
- `--min-cost-usd <float>`: Exclude groups with total cost below this threshold.
- `--json`: Machine-readable output.

**Example TTY output (`tag stats --cost --since 7d --by model`):**

```
Cost Summary  (last 7 days, grouped by model)
──────────────────────────────────────────────────────────────────
MODEL                        SPANS    IN TOK       OUT TOK    TOTAL USD
──────────────────────────────────────────────────────────────────
claude-sonnet-4-6              142   1,204,102     312,044      $42.38
claude-haiku-4-5               389     841,203     201,302       $3.21
gpt-4o                          23      50,100      14,200       $1.48
claude-sonnet-4-6 (cached)      18     120,000           0       $0.36
──────────────────────────────────────────────────────────────────
TOTAL                          572   2,215,405     527,546      $47.43
──────────────────────────────────────────────────────────────────
```

**Example `--json` output:**

```json
{
  "since": "2026-06-10T00:00:00Z",
  "until": "2026-06-17T10:14:02Z",
  "by": ["model"],
  "total_cost_usd": 47.43,
  "groups": [
    {
      "model_id": "claude-sonnet-4-6",
      "span_count": 142,
      "input_tokens": 1204102,
      "output_tokens": 312044,
      "cost_usd": 42.38
    }
  ]
}
```

### 7.3 `tag budget set`

Set a hard per-run USD budget limit on a profile.

```
tag budget set --profile <name> --limit-usd <float> --per-run
tag budget set --profile <name> --limit-usd <float> --per-day
tag budget get --profile <name>
tag budget unset --profile <name>
```

**Options:**
- `--profile <name>`: Profile to apply the limit to (required).
- `--limit-usd <float>`: Hard limit in USD (required).
- `--per-run`: Enforce limit as cumulative cost for a single run (resets each run).
- `--per-day`: Enforce limit as total cost across all runs in a calendar day UTC.
- `--warn-at <pct>`: Emit a warning when cost reaches this percentage of the limit. Default: 80.

**Example:**

```bash
# Set a $5.00 hard limit per run on the researcher profile
tag budget set --profile researcher --limit-usd 5.00 --per-run --warn-at 80

# Verify:
tag budget get --profile researcher
# researcher  per_run_limit_usd=5.00  warn_at_pct=80  per_day_limit_usd=(unset)

# Remove:
tag budget unset --profile researcher
```

**Budget enforcement runtime output (when limit nears):**

```
[TAG BUDGET] researcher: $4.03 / $5.00 (80.6%) — approaching run limit
[TAG BUDGET] researcher: $5.12 / $5.00 — hard limit exceeded, aborting run
```

### 7.4 `tag costs --run-id`

Itemized cost report for a single run, designed for CI consumption.

```
tag costs --run-id <id> [--json] [--by span|model]
```

**Options:**
- `--run-id <id>`: Run ID to report on (required).
- `--json`: Machine-readable JSON.
- `--by span`: Default. Show each span as a row.
- `--by model`: Aggregate by model within the run.

**Example (no `--json`):**

```
Run: abc123  profile: researcher  started: 2026-06-17 10:14:02 UTC
─────────────────────────────────────────────────────────────────────
SPAN NAME              MODEL               IN TOK   OUT TOK    COST USD
─────────────────────────────────────────────────────────────────────
step:plan              claude-sonnet-4-6    4,210    1,024      $0.0434
step:summarize         claude-haiku-4-5     2,100      512      $0.0040
step:write_report      claude-sonnet-4-6    8,102    3,204      $0.2040
─────────────────────────────────────────────────────────────────────
TOTAL                                      14,412    4,740      $0.2514
─────────────────────────────────────────────────────────────────────
```

### 7.5 `tag pricing show`

Inspect the active pricing table.

```
tag pricing show [--model <id>] [--json]
tag pricing update [--source <url>]
```

**Example (`tag pricing show`):**

```
Active pricing table  (source: ~/.tag/pricing.yaml  •  built-in fallback)
───────────────────────────────────────────────────────────────────────────────
MODEL ID                        IN $/1M TOK   OUT $/1M TOK   CACHE READ $/1M
───────────────────────────────────────────────────────────────────────────────
claude-sonnet-4-6                   $3.00        $15.00          $0.30
claude-haiku-4-5                    $0.80         $4.00          $0.08
claude-opus-4-5                    $15.00        $75.00          $1.50
gpt-4o                              $2.50        $10.00          $0.25
gpt-4o-mini                         $0.15         $0.60          $0.015
gemini-2.0-flash                    $0.075        $0.30            —
mistral-large-latest                $2.00         $6.00            —
deepseek-chat                       $0.014        $0.14          $0.007
───────────────────────────────────────────────────────────────────────────────
(30 models total. Run 'tag pricing update' to refresh.)
```

---

## 8. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | **`compute_cost()` function:** `src/tag/cost_table.py` must expose `compute_cost(model_id: str, input_tokens: int, output_tokens: int, *, cache_read: bool = False, batch: bool = False) -> float \| None`. Returns `None` if the model ID is not found in the pricing table. Must not raise exceptions for any input. |
| FR-02 | **Pricing table loading:** `cost_table.py` must implement `load_pricing_table(path: Path \| None = None) -> PricingTable`. The load order is: (1) `path` argument, (2) `~/.tag/pricing.yaml`, (3) bundled `src/tag/assets/pricing.yaml`. The first file found wins; entries from user YAML are merged on top of bundled entries so users can add/override specific models without losing the rest. |
| FR-03 | **Pricing YAML schema:** Each entry in the pricing YAML must have `model_id` (string, exact match or glob pattern), `input_usd_per_1m` (float), `output_usd_per_1m` (float), and optional `cache_read_multiplier` (float, default 0.1) and `batch_multiplier` (float, default 0.5). |
| FR-04 | **Glob prefix matching:** If `model_id` in the YAML contains a wildcard `*` (e.g. `claude-*`), it matches any model ID with that prefix. Exact matches take precedence over glob matches. This handles model version suffixes (e.g. `claude-sonnet-4-6-20251201`) without requiring every version to be listed. |
| FR-05 | **Cache-read pricing:** When `cache_read=True` is passed to `compute_cost()`, the input cost is multiplied by `cache_read_multiplier` (default 0.1). This matches Anthropic's prompt cache read pricing. The output cost is unchanged. |
| FR-06 | **Batch pricing:** When `batch=True`, both input and output costs are multiplied by `batch_multiplier` (default 0.5). Batch and cache-read discounts are multiplicative (stacked discounts). |
| FR-07 | **`close_span()` integration:** `tracing.py`'s `close_span()` signature must accept a new `cost_usd: float \| None = None` parameter. It stores the value in `span.cost_usd`. The caller (in `controller.py`) is responsible for computing and passing `cost_usd`; `close_span()` itself does not call `compute_cost()`. This keeps `tracing.py` free of pricing dependencies. |
| FR-08 | **`Span` dataclass extension:** The `Span` dataclass in `tracing.py` gains a `cost_usd: float \| None = None` field. |
| FR-09 | **`spans` table migration:** `open_db()` in `controller.py` must execute `ALTER TABLE spans ADD COLUMN cost_usd REAL` if the column does not exist. Migration uses the `PRAGMA table_info(spans)` guard pattern already present in `open_db()`. |
| FR-10 | **`save_spans_to_db()` update:** The `_INSERT_SPAN` SQL in `tracing.py` must include `cost_usd` in both the column list and VALUES list. `INSERT OR REPLACE` semantics are unchanged. |
| FR-11 | **`otel_semconv.py` attribute injection:** `map_span_attributes()` must inject `gen_ai.usage.cost_usd` as a `doubleValue` attribute when `cost_usd` is non-null on the span. This makes cost visible in OTLP exports (Datadog, Jaeger). |
| FR-12 | **`tag trace show --cost` rendering:** The `cmd_trace_show` function in `controller.py` must accept `--cost` flag. When set, the output table adds `IN TOK`, `OUT TOK`, and `COST USD` columns. Non-LLM spans with null `cost_usd` render `—` in the cost column. A `TOTAL` row sums all non-null costs. |
| FR-13 | **`tag stats --cost` aggregation:** `cmd_stats` must accept `--cost`, `--since`, `--until`, `--by`, `--profile`, and `--min-cost-usd` flags. The underlying SQL query (see §9.4) groups `SUM(cost_usd)` from the `spans` table joined with `runs` for profile filtering. |
| FR-14 | **`tag budget set` storage:** Budget limits are stored in the profile YAML under a `budget` key: `{per_run_usd: 5.00, per_day_usd: null, warn_at_pct: 80}`. `tag budget set` writes this key; `tag budget unset` removes it. `tag budget get` reads and displays it. |
| FR-15 | **Budget enforcement at close_span time:** After every `close_span()` call in `controller.py`, if the profile has a `per_run_usd` limit, the system queries `SELECT SUM(cost_usd) FROM spans WHERE trace_id = ? AND cost_usd IS NOT NULL` and compares to the limit. At `warn_at_pct`, it emits a warning to stderr. At 100%, it raises `BudgetExceededError` which the agent loop catches to abort cleanly. |
| FR-16 | **`tag costs --run-id` command:** `cmd_costs` queries `spans` for the given `trace_id` (matching the run ID), orders by `started_at`, and renders the result table. `--by model` applies a GROUP BY on `model_id`. |
| FR-17 | **`tag pricing show` command:** `cmd_pricing_show` loads the active pricing table via `load_pricing_table()` and renders it as a table or JSON. `--model <id>` filters to a single model, showing exact match plus any glob rule that would match. |
| FR-18 | **`tag pricing update` command:** Downloads a YAML file from a configurable URL (config key `pricing.update_url`, default: a community-maintained GitHub raw URL) and saves it to `~/.tag/pricing.yaml`. Validates the downloaded YAML against the schema before writing. No auto-update; purely user-triggered. |
| FR-19 | **Zero-overhead when tracing disabled:** If tracing is disabled (the `tracing.enabled` config flag is false), `cost_table.py` is never imported and `compute_cost()` is never called. The pricing YAML is not loaded. |
| FR-20 | **`--json` output completeness:** All `--json` outputs from `tag trace show --cost --json`, `tag stats --cost --json`, and `tag costs --run-id --json` must include `cost_usd` fields. JSON keys follow snake_case. |

---

## 9. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **Pricing lookup is synchronous and sub-millisecond.** `compute_cost()` must be a pure in-memory dictionary lookup with no I/O. The pricing table is loaded once at startup and cached in a module-level singleton. Reload only happens when `tag pricing update` is explicitly called. |
| NFR-02 | **Pricing table load time < 50 ms.** Even with a 1,000-entry user YAML, `load_pricing_table()` must complete in under 50 ms (YAML parse + dict construction). |
| NFR-03 | **`ALTER TABLE` migration is non-destructive.** Existing rows after migration have `cost_usd = NULL`; existing queries against the `spans` table that do not reference `cost_usd` are unaffected. Migration is guarded by `PRAGMA table_info` and runs at most once per DB lifecycle. |
| NFR-04 | **`cost_usd = null` never causes errors.** All SQL aggregations use `SUM(cost_usd)` (which ignores NULLs) and `COALESCE(cost_usd, 0)` where needed. CLI renders null as `—` in TTY mode and `null` in JSON mode. |
| NFR-05 | **Thread safety.** `load_pricing_table()` uses a module-level lock (`threading.Lock`) around the singleton initialization to prevent race conditions in multi-threaded agent loops. After initialization, all accesses are read-only and lock-free. |
| NFR-06 | **TTY vs. pipe output.** When stdout is not a TTY, all tabular output falls back to tab-separated rows unless `--json` is specified. This mirrors the existing `cmd_runs` convention. |
| NFR-07 | **Graceful degradation for unknown models.** `compute_cost()` returns `None` for unknown models. `close_span()` stores `None`. No warning is emitted per span (to avoid log spam for experimental models); a single summary warning appears in `tag trace show --cost` output when any span has `cost_usd = null`. |
| NFR-08 | **Pricing YAML is human-editable.** The YAML format is intentionally simple (no JSON Schema requirement at runtime). User YAML merges with bundled YAML; conflicting model IDs in user YAML override bundled ones. |
| NFR-09 | **`BudgetExceededError` results in clean span closure.** When the budget is exceeded, the current span is closed with `status='error'`, `error_msg='budget_exceeded'` before the exception propagates. No partial spans are left open. |
| NFR-10 | **No new required dependencies.** `cost_table.py` uses only `pathlib`, `yaml` (PyYAML, already in `pyproject.toml`), `threading`, and `fnmatch` (stdlib). No new package installations are required. |

---

## 10. Technical Design

### 10.1 New files

- **`src/tag/cost_table.py`** — Pricing table loader, `compute_cost()` function, `PricingEntry` and `PricingTable` dataclasses, singleton cache, `BudgetExceededError`. Entirely self-contained with no imports from other TAG modules.
- **`src/tag/assets/pricing.yaml`** — Bundled default pricing YAML. Committed to the repository. Contains ≥ 30 model entries. Updated by maintainers each time a major model pricing change occurs.

### 10.2 Modified files

- **`src/tag/tracing.py`** — Add `cost_usd: float | None = None` to `Span` dataclass; add `cost_usd: float | None = None` parameter to `close_span()`; update `_INSERT_SPAN` SQL and `save_spans_to_db()`.
- **`src/tag/otel_semconv.py`** — Update `map_span_attributes()` to inject `gen_ai.usage.cost_usd` when non-null.
- **`src/tag/controller.py`** — Update `close_span()` call sites to pass `cost_usd`; add `cmd_trace_show --cost` rendering; add `cmd_stats --cost` subcommand; add `cmd_budget` subcommand; add `cmd_costs` subcommand; add `cmd_pricing` subcommand; add budget enforcement logic after each `close_span()` call.

### 10.3 `PricingEntry` and `PricingTable` dataclasses

```python
# src/tag/cost_table.py

from __future__ import annotations

import fnmatch
import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


@dataclass
class PricingEntry:
    """Pricing for a single model (or glob pattern)."""
    model_id: str                    # Exact model ID or glob (e.g. 'claude-*')
    input_usd_per_1m: float          # USD per 1,000,000 input (prompt) tokens
    output_usd_per_1m: float         # USD per 1,000,000 output (completion) tokens
    cache_read_multiplier: float = 0.1   # Fraction of input price for cache reads
    batch_multiplier: float = 0.5        # Fraction for batch API calls


@dataclass
class PricingTable:
    """Container for all pricing entries, supporting exact and glob lookup."""
    entries: list[PricingEntry] = field(default_factory=list)
    _exact: dict[str, PricingEntry] = field(default_factory=dict, repr=False)
    _globs: list[PricingEntry] = field(default_factory=list, repr=False)

    def __post_init__(self) -> None:
        self._rebuild_index()

    def _rebuild_index(self) -> None:
        self._exact = {}
        self._globs = []
        for entry in self.entries:
            if '*' in entry.model_id or '?' in entry.model_id:
                self._globs.append(entry)
            else:
                self._exact[entry.model_id] = entry

    def lookup(self, model_id: str) -> PricingEntry | None:
        """Return the PricingEntry for model_id. Exact match takes priority."""
        if model_id in self._exact:
            return self._exact[model_id]
        for glob_entry in self._globs:
            if fnmatch.fnmatch(model_id, glob_entry.model_id):
                return glob_entry
        return None
```

### 10.4 `compute_cost()` and singleton loader

```python
# src/tag/cost_table.py (continued)

import yaml

_TABLE_LOCK = threading.Lock()
_TABLE: PricingTable | None = None
_TABLE_SOURCE: Path | None = None


class BudgetExceededError(RuntimeError):
    """Raised when a profile's per-run or per-day USD budget is exceeded."""
    def __init__(self, profile: str, limit_usd: float, actual_usd: float) -> None:
        self.profile = profile
        self.limit_usd = limit_usd
        self.actual_usd = actual_usd
        super().__init__(
            f"Budget exceeded for profile '{profile}': "
            f"${actual_usd:.4f} > limit ${limit_usd:.2f}"
        )


_BUNDLED_YAML = Path(__file__).parent / "assets" / "pricing.yaml"
_USER_YAML = Path.home() / ".tag" / "pricing.yaml"


def load_pricing_table(path: Path | None = None) -> PricingTable:
    """Load and return the active pricing table.

    Load order: path argument → ~/.tag/pricing.yaml → bundled asset.
    User YAML entries are merged on top of bundled entries (user wins on conflict).
    """
    global _TABLE, _TABLE_SOURCE

    with _TABLE_LOCK:
        # Return cached singleton unless an explicit path is provided
        if _TABLE is not None and path is None:
            return _TABLE

        bundled = _load_yaml(_BUNDLED_YAML) if _BUNDLED_YAML.exists() else {}
        user_override = {}

        if path is not None and path.exists():
            user_override = _load_yaml(path)
            source = path
        elif _USER_YAML.exists():
            user_override = _load_yaml(_USER_YAML)
            source = _USER_YAML
        else:
            source = _BUNDLED_YAML

        # Merge: user entries override bundled entries by model_id key
        merged: dict[str, dict] = {e["model_id"]: e for e in bundled.get("models", [])}
        for entry in user_override.get("models", []):
            merged[entry["model_id"]] = entry

        entries = [
            PricingEntry(
                model_id=e["model_id"],
                input_usd_per_1m=float(e["input_usd_per_1m"]),
                output_usd_per_1m=float(e["output_usd_per_1m"]),
                cache_read_multiplier=float(e.get("cache_read_multiplier", 0.1)),
                batch_multiplier=float(e.get("batch_multiplier", 0.5)),
            )
            for e in merged.values()
        ]
        table = PricingTable(entries=entries)

        if path is None:
            _TABLE = table
            _TABLE_SOURCE = source

        return table


def _load_yaml(path: Path) -> dict:
    with open(path, encoding="utf-8") as f:
        return yaml.safe_load(f) or {}


def compute_cost(
    model_id: str,
    input_tokens: int,
    output_tokens: int,
    *,
    cache_read: bool = False,
    batch: bool = False,
    table: PricingTable | None = None,
) -> float | None:
    """Compute USD cost for an LLM call.

    Returns None if the model_id is not found in the pricing table.
    Never raises exceptions (returns None on any error).

    Formula:
        input_effective = input_usd_per_1m * cache_read_multiplier (if cache_read)
        batch factor applied to both input and output if batch=True

        cost = (input_tokens * input_effective / 1_000_000)
             + (output_tokens * output_usd_per_1m * batch_mult / 1_000_000)
    """
    try:
        if table is None:
            table = load_pricing_table()
        entry = table.lookup(model_id)
        if entry is None:
            return None

        in_rate = entry.input_usd_per_1m
        if cache_read:
            in_rate *= entry.cache_read_multiplier

        out_rate = entry.output_usd_per_1m

        if batch:
            in_rate *= entry.batch_multiplier
            out_rate *= entry.batch_multiplier

        return (input_tokens * in_rate / 1_000_000) + (output_tokens * out_rate / 1_000_000)
    except Exception:
        return None
```

### 10.5 `Span` dataclass and `close_span()` signature update

```python
# src/tag/tracing.py (diff-style additions)

@dataclass
class Span:
    # ... existing fields unchanged ...
    cost_usd: float | None = None   # NEW: USD cost computed at close_span() time


def close_span(
    span: Span,
    status: str = "ok",
    prompt_tokens: int = 0,
    completion_tokens: int = 0,
    error_msg: str | None = None,
    cost_usd: float | None = None,  # NEW parameter
) -> None:
    """Close an open Span, recording timing, outcome, and USD cost."""
    if span.finished_at is not None:
        return

    finished_iso = _utc_now()
    span.finished_at = finished_iso
    span.status = status
    span.prompt_tokens = prompt_tokens
    span.completion_tokens = completion_tokens
    span.error_msg = error_msg
    span.cost_usd = cost_usd        # NEW

    try:
        t_start = datetime.fromisoformat(span.started_at)
        t_end = datetime.fromisoformat(finished_iso)
        delta = t_end - t_start
        span.duration_ms = max(0, int(delta.total_seconds() * 1000))
    except Exception:
        span.duration_ms = None
```

### 10.6 `spans` table DDL migration

```sql
-- Migration executed by open_db() in controller.py
-- Guard: SELECT * FROM pragma_table_info('spans') WHERE name='cost_usd'
ALTER TABLE spans ADD COLUMN cost_usd REAL;

CREATE INDEX IF NOT EXISTS idx_spans_cost
  ON spans(trace_id, cost_usd)
  WHERE cost_usd IS NOT NULL;
```

Migration code pattern (matches existing `open_db()` style):

```python
def _migrate_spans_cost_column(conn: sqlite3.Connection) -> None:
    """Add cost_usd column to spans if not present (idempotent)."""
    cursor = conn.execute("PRAGMA table_info(spans)")
    cols = {row[1] for row in cursor.fetchall()}
    if "cost_usd" not in cols:
        conn.execute("ALTER TABLE spans ADD COLUMN cost_usd REAL")
        conn.execute(
            "CREATE INDEX IF NOT EXISTS idx_spans_cost "
            "ON spans(trace_id, cost_usd) WHERE cost_usd IS NOT NULL"
        )
        conn.commit()
```

### 10.7 `otel_semconv.py` update

```python
# src/tag/otel_semconv.py: map_span_attributes() addition

def map_span_attributes(span: dict[str, Any]) -> dict[str, Any]:
    result = dict(span)
    attrs = dict(result.get("attributes", {}))

    # ... existing token/model mapping unchanged ...

    # NEW: inject cost attribute if present
    cost_usd = span.get("cost_usd")
    if cost_usd is not None:
        attrs["gen_ai.usage.cost_usd"] = float(cost_usd)

    result["attributes"] = attrs
    return result
```

### 10.8 `tag stats --cost` SQL query

```sql
-- Core aggregation query for tag stats --cost --by model
SELECT
    s.model_id,
    COUNT(s.id)                    AS span_count,
    SUM(s.prompt_tokens)           AS total_input_tokens,
    SUM(s.completion_tokens)       AS total_output_tokens,
    SUM(s.cost_usd)                AS total_cost_usd
FROM spans s
JOIN runs r ON s.trace_id = r.id
WHERE
    s.started_at >= :since
    AND s.started_at <= :until
    AND (:profile IS NULL OR r.profile = :profile)
    AND s.cost_usd IS NOT NULL
GROUP BY s.model_id
ORDER BY total_cost_usd DESC;
```

For `--by profile`:
```sql
SELECT
    r.profile,
    COUNT(s.id)                    AS span_count,
    SUM(s.prompt_tokens)           AS total_input_tokens,
    SUM(s.completion_tokens)       AS total_output_tokens,
    SUM(s.cost_usd)                AS total_cost_usd
FROM spans s
JOIN runs r ON s.trace_id = r.id
WHERE s.started_at >= :since AND s.started_at <= :until
GROUP BY r.profile
ORDER BY total_cost_usd DESC;
```

For `--by day`:
```sql
SELECT
    DATE(s.started_at)             AS day,
    COUNT(s.id)                    AS span_count,
    SUM(s.cost_usd)                AS total_cost_usd
FROM spans s
WHERE s.started_at >= :since AND s.started_at <= :until
GROUP BY DATE(s.started_at)
ORDER BY day DESC;
```

### 10.9 Budget enforcement integration point in `controller.py`

```python
# controller.py — inside the agent execution loop, after each LLM call

def _check_budget(
    conn: sqlite3.Connection,
    trace_id: str,
    profile_cfg: dict,
    profile_name: str,
) -> None:
    """Raise BudgetExceededError if the trace has exceeded the profile's per-run limit."""
    budget = profile_cfg.get("budget", {})
    per_run_limit = budget.get("per_run_usd")
    if per_run_limit is None:
        return  # No limit configured

    warn_at_pct = budget.get("warn_at_pct", 80) / 100.0

    row = conn.execute(
        "SELECT COALESCE(SUM(cost_usd), 0.0) FROM spans "
        "WHERE trace_id = ? AND cost_usd IS NOT NULL",
        (trace_id,),
    ).fetchone()
    actual_usd = row[0] if row else 0.0

    ratio = actual_usd / per_run_limit if per_run_limit > 0 else 0.0

    if ratio >= warn_at_pct and ratio < 1.0:
        import sys
        print(
            f"[TAG BUDGET] {profile_name}: ${actual_usd:.4f} / ${per_run_limit:.2f} "
            f"({ratio * 100:.1f}%) — approaching run limit",
            file=sys.stderr,
        )
    elif actual_usd > per_run_limit:
        from tag.cost_table import BudgetExceededError
        raise BudgetExceededError(profile_name, per_run_limit, actual_usd)
```

### 10.10 Bundled `pricing.yaml` structure

```yaml
# src/tag/assets/pricing.yaml
# Pricing as of June 2026. Run 'tag pricing update' to refresh.
# Sources: Anthropic pricing page, OpenAI pricing page, provider docs.

schema_version: "1"
updated: "2026-06-17"

models:
  # Anthropic
  - model_id: "claude-opus-4-5"
    input_usd_per_1m: 15.00
    output_usd_per_1m: 75.00
    cache_read_multiplier: 0.1

  - model_id: "claude-sonnet-4-6"
    input_usd_per_1m: 3.00
    output_usd_per_1m: 15.00
    cache_read_multiplier: 0.1

  - model_id: "claude-haiku-4-5"
    input_usd_per_1m: 0.80
    output_usd_per_1m: 4.00
    cache_read_multiplier: 0.1

  # Wildcard for future Claude versions
  - model_id: "claude-*"
    input_usd_per_1m: 3.00
    output_usd_per_1m: 15.00
    cache_read_multiplier: 0.1

  # OpenAI
  - model_id: "gpt-4o"
    input_usd_per_1m: 2.50
    output_usd_per_1m: 10.00
    cache_read_multiplier: 0.5

  - model_id: "gpt-4o-mini"
    input_usd_per_1m: 0.15
    output_usd_per_1m: 0.60
    cache_read_multiplier: 0.5

  - model_id: "o3"
    input_usd_per_1m: 10.00
    output_usd_per_1m: 40.00

  # Google
  - model_id: "gemini-2.0-flash"
    input_usd_per_1m: 0.075
    output_usd_per_1m: 0.30

  - model_id: "gemini-2.5-pro"
    input_usd_per_1m: 1.25
    output_usd_per_1m: 10.00

  # Mistral
  - model_id: "mistral-large-latest"
    input_usd_per_1m: 2.00
    output_usd_per_1m: 6.00

  # DeepSeek
  - model_id: "deepseek-chat"
    input_usd_per_1m: 0.014
    output_usd_per_1m: 0.14
    cache_read_multiplier: 0.02

  - model_id: "deepseek-reasoner"
    input_usd_per_1m: 0.55
    output_usd_per_1m: 2.19
    cache_read_multiplier: 0.04
```

---

## 11. Security Considerations

1. **Pricing YAML path traversal:** The `--source` flag of `tag pricing update` and the custom `path` parameter of `load_pricing_table()` must resolve to an absolute path and reject any path containing `..` components that escape `~/.tag/` or the process working directory. Reject paths with `..` components unconditionally.

2. **No credentials in pricing YAML:** The pricing YAML contains only numeric pricing data. No API keys, tokens, or secrets should be stored in this file. `tag pricing update` downloads from an HTTPS URL only (reject HTTP). The downloaded content is validated against the pricing YAML schema before writing to disk.

3. **Budget limit storage in profile YAML:** Per-run budget limits are stored in the profile YAML file. TAG profile YAML files are user-owned files (`chmod 600`). The budget limit is a positive float; any non-positive value is rejected at `tag budget set` time with a descriptive error. There is no authentication mechanism for `tag budget unset`; it is a local file write.

4. **`BudgetExceededError` and partial data:** When `BudgetExceededError` is raised, any spans already written to SQLite remain written (they are committed per `close_span()`). The error is caught by the agent loop which closes the root span with `status='error'` and `error_msg='budget_exceeded'`. The partial run is visible in `tag runs` with `status=error`. No data is silently lost.

5. **Cost computation is client-side only:** `compute_cost()` is a local arithmetic function. It never contacts any external API to verify pricing. Users must be informed (via documentation and `tag pricing show` output) that TAG's cost estimates may differ from actual provider billing due to price changes, rounding, or features (e.g. prompt caching) that are not fully observable from token counts alone.

6. **No cost data in OTLP exports that leak to untrusted backends:** The `gen_ai.usage.cost_usd` attribute injected by `otel_semconv.py` is a float attribute. When exporting to OTLP endpoints, the cost figure is visible to the backend. Users operating in environments where internal cost data is sensitive should be aware that enabling OTLP export transmits cost estimates to the configured endpoint. Documentation should note this.

7. **`tag pricing update` network download:** The update command downloads a YAML file from a URL. The downloaded file is parsed by `yaml.safe_load()` (which is safe against code execution via YAML). The schema is validated before writing. The URL must use HTTPS. Users on restricted networks should set `pricing.update_url` to a self-hosted endpoint.

---

## 12. Testing Strategy

### 12.1 Unit tests (`tests/test_cost_table.py`)

```python
# Pricing formula correctness
def test_compute_cost_basic():
    entry = PricingEntry("claude-sonnet-4-6", 3.00, 15.00)
    table = PricingTable([entry])
    # 1000 input tokens @ $3/1M = $0.000003 * 1000 = $0.003
    # 500 output tokens @ $15/1M = $0.000015 * 500 = $0.0075
    cost = compute_cost("claude-sonnet-4-6", 1000, 500, table=table)
    assert abs(cost - 0.0105) < 1e-9

def test_compute_cost_cache_read():
    entry = PricingEntry("claude-sonnet-4-6", 3.00, 15.00, cache_read_multiplier=0.1)
    table = PricingTable([entry])
    cost = compute_cost("claude-sonnet-4-6", 10000, 0, cache_read=True, table=table)
    # 10000 * (3.00 * 0.1) / 1M = 10000 * 0.0000003 = 0.003
    assert abs(cost - 0.003) < 1e-9

def test_compute_cost_batch():
    entry = PricingEntry("gpt-4o", 2.50, 10.00, batch_multiplier=0.5)
    table = PricingTable([entry])
    cost = compute_cost("gpt-4o", 1000, 1000, batch=True, table=table)
    # (1000 * 1.25 / 1M) + (1000 * 5.00 / 1M) = 0.00125 + 0.005 = 0.00625
    assert abs(cost - 0.00625) < 1e-9

def test_compute_cost_unknown_model_returns_none():
    table = PricingTable([])
    assert compute_cost("unknown-model-xyz", 1000, 1000, table=table) is None

def test_glob_matching():
    entry = PricingEntry("claude-*", 3.00, 15.00)
    table = PricingTable([entry])
    assert compute_cost("claude-sonnet-4-6-20251201", 0, 0, table=table) is not None

def test_exact_match_beats_glob():
    glob_entry = PricingEntry("claude-*", 99.00, 99.00)
    exact_entry = PricingEntry("claude-haiku-4-5", 0.80, 4.00)
    table = PricingTable([glob_entry, exact_entry])
    cost = compute_cost("claude-haiku-4-5", 1_000_000, 0, table=table)
    assert abs(cost - 0.80) < 1e-9  # Used exact entry, not glob

def test_compute_cost_never_raises():
    # Should return None gracefully, not raise
    assert compute_cost(None, -1, -1) is None  # type: ignore
    assert compute_cost("", 0, 0) is None
```

### 12.2 Unit tests (`tests/test_tracing_cost.py`)

```python
def test_close_span_stores_cost_usd():
    span = open_span("trace1", "step:test", model_id="claude-sonnet-4-6")
    close_span(span, prompt_tokens=1000, completion_tokens=500, cost_usd=0.0105)
    assert span.cost_usd == 0.0105
    assert span.finished_at is not None

def test_close_span_cost_usd_defaults_to_none():
    span = open_span("trace1", "tool_call:search")
    close_span(span)
    assert span.cost_usd is None

def test_save_spans_to_db_persists_cost_usd(tmp_path):
    db_path = tmp_path / "test.sqlite3"
    span = open_span("trace1", "step", model_id="claude-haiku-4-5")
    close_span(span, prompt_tokens=2000, completion_tokens=400, cost_usd=0.0042)
    save_spans_to_db(db_path, [span])
    conn = sqlite3.connect(str(db_path))
    row = conn.execute("SELECT cost_usd FROM spans WHERE id = ?", (span.id,)).fetchone()
    assert abs(row[0] - 0.0042) < 1e-9
    conn.close()
```

### 12.3 Unit tests (`tests/test_otel_semconv_cost.py`)

```python
def test_map_span_attributes_injects_cost():
    span = {"model_id": "claude-sonnet-4-6", "prompt_tokens": 100,
            "completion_tokens": 50, "cost_usd": 0.1234}
    result = map_span_attributes(span)
    assert result["attributes"]["gen_ai.usage.cost_usd"] == 0.1234

def test_map_span_attributes_no_cost_no_injection():
    span = {"model_id": "claude-haiku-4-5", "prompt_tokens": 100,
            "completion_tokens": 50}
    result = map_span_attributes(span)
    assert "gen_ai.usage.cost_usd" not in result["attributes"]
```

### 12.4 Integration tests (`tests/test_cost_integration.py`)

- Seed the `spans` table with 10 spans (5 with `cost_usd`, 5 with `null`). Run `tag stats --cost --by model --json` via subprocess; assert JSON output sums only the 5 non-null spans and excludes the `null` ones.
- Run `tag trace show <run-id> --cost --json`; assert the JSON includes a `total_cost_usd` summing only non-null span costs.
- Run `tag budget set --profile test --limit-usd 0.01 --per-run`; verify the profile YAML contains the correct `budget.per_run_usd` value.
- Simulate a run that would exceed $0.01 by inserting spans totaling $0.005, then calling `_check_budget` with a $0.01 limit; assert no exception. Insert another span to bring total to $0.011; assert `BudgetExceededError` is raised.

### 12.5 `ALTER TABLE` migration test

```python
def test_migrate_spans_cost_column(tmp_path):
    """Verify migration adds cost_usd to a pre-existing spans table."""
    db_path = tmp_path / "old.sqlite3"
    conn = sqlite3.connect(str(db_path))
    # Create spans table WITHOUT cost_usd (simulates pre-PRD-046 schema)
    conn.execute("""CREATE TABLE spans (
        id TEXT PRIMARY KEY, trace_id TEXT, parent_id TEXT, name TEXT,
        profile TEXT, model_id TEXT, started_at TEXT, finished_at TEXT,
        duration_ms INTEGER, status TEXT, prompt_tokens INTEGER,
        completion_tokens INTEGER, attributes TEXT, error_msg TEXT
    )""")
    conn.commit()
    conn.close()

    _migrate_spans_cost_column(sqlite3.connect(str(db_path)))

    conn = sqlite3.connect(str(db_path))
    cols = {row[1] for row in conn.execute("PRAGMA table_info(spans)")}
    assert "cost_usd" in cols
    conn.close()
```

### 12.6 Performance tests

- `timeit` benchmark: 10,000 calls to `compute_cost("claude-sonnet-4-6", 1000, 500)` should complete in < 100 ms total (< 0.01 ms per call).
- `tag trace show --cost` with a 200-span trace: measure wall time from SQLite query to first byte of output; assert < 200 ms.

---

## 13. Acceptance Criteria

| ID | Criterion |
|----|-----------|
| AC-01 | `compute_cost("claude-sonnet-4-6", 1_000_000, 1_000_000)` returns `18.0` (input $3.00 + output $15.00). |
| AC-02 | `compute_cost("unknown-xyz", 1000, 1000)` returns `None` without raising any exception. |
| AC-03 | After `close_span(span, prompt_tokens=1000, completion_tokens=500, cost_usd=0.0105)`, `span.cost_usd == 0.0105` and the value is persisted to the `spans` table `cost_usd` column. |
| AC-04 | Running `open_db()` against a pre-PRD-046 `spans` table (no `cost_usd` column) adds the column without data loss; existing rows have `cost_usd = NULL`. |
| AC-05 | `map_span_attributes({"cost_usd": 0.1234, "model_id": "claude-haiku-4-5", ...})["attributes"]["gen_ai.usage.cost_usd"]` equals `0.1234`. |
| AC-06 | `tag trace show --run-id <id> --cost` prints a table with `COST USD` column; non-LLM spans show `—`; the `TOTAL` row is the sum of all non-null costs. |
| AC-07 | `tag trace show --run-id <id> --cost --json` returns valid JSON with `total_cost_usd` and a `spans` array each containing `cost_usd`. |
| AC-08 | `tag stats --cost --since 7d --by model` returns rows grouped by `model_id` with correct `total_cost_usd` sums from the `spans` table. |
| AC-09 | `tag stats --cost --since 7d --by model --json` produces valid JSON with `groups` array and `total_cost_usd` top-level field. |
| AC-10 | `tag budget set --profile coder --limit-usd 5.00 --per-run` writes `budget.per_run_usd: 5.0` to the `coder` profile YAML. |
| AC-11 | When cumulative `SUM(cost_usd)` for a trace exceeds `per_run_usd`, `_check_budget()` raises `BudgetExceededError`; the agent loop catches it, closes the root span with `status='error'`, and exits with code 1. |
| AC-12 | `tag costs --run-id <id>` displays a per-span cost table with subtotal; exit code 0. |
| AC-13 | `tag costs --run-id <id> --json` produces valid JSON with a `spans` array and `total_cost_usd` field; parseable by `jq`. |
| AC-14 | `tag pricing show` displays the active pricing table with ≥ 10 model rows and indicates the source file path. |
| AC-15 | `tag pricing show --model gpt-4o` displays exactly the `gpt-4o` entry and any glob that would match it. |
| AC-16 | Cache-read pricing: `compute_cost("claude-sonnet-4-6", 1_000_000, 0, cache_read=True)` returns `0.30` (= $3.00 × 0.1 per 1M). |
| AC-17 | Batch + cache-read stacked: `compute_cost("claude-sonnet-4-6", 1_000_000, 1_000_000, cache_read=True, batch=True)` returns `0.30 * 0.5 + 15.00 * 0.5 = 0.15 + 7.50 = 7.65`. |
| AC-18 | `import tag.controller` does not import `cost_table` when `tracing.enabled = false` in config (zero-overhead guarantee). |
| AC-19 | A glob entry `claude-*` matches `claude-sonnet-4-6-20251201` but is overridden by an exact entry `claude-sonnet-4-6` when the model ID is exactly `claude-sonnet-4-6`. |
| AC-20 | `tag pricing update --source https://example.com/pricing.yaml` with an HTTP (non-HTTPS) URL is rejected with a descriptive error before any network call. |

---

## 14. Dependencies

| Dependency | Type | Relationship |
|------------|------|--------------|
| PRD-013 Agent Tracing | Existing — required | `spans` table and `Span` / `close_span()` are the storage layer this PRD extends. Must be implemented and deployed before PRD-046 migrations run. |
| PRD-012 Cost Tracking | Existing — complementary | PRD-012 provides run-level `estimated_cost_usd` on `runs`. PRD-046 provides span-level `cost_usd` on `spans`. They are additive; PRD-046 does not replace PRD-012 aggregates. |
| PRD-037 OTel GenAI Span Cost Attribution | Existing — complementary | PRD-037 defined `map_span_attributes()` in `otel_semconv.py`. PRD-046 adds `gen_ai.usage.cost_usd` to that function's output. PRD-037 must ship first. |
| PRD-028 Sandbox Code Execution | Existing — non-blocking | `BudgetExceededError` aborting the agent loop must be compatible with sandbox cleanup hooks. Sandbox cleanup is triggered by the agent loop's `finally` block, which runs even on `BudgetExceededError`. |
| PRD-034 Secret Scanning | Existing — advisory | Pricing YAML should be added to PRD-034's allowlist (it contains no secrets). The `tag pricing update` download URL should be validated to not contain credential-style query parameters. |
| `PyYAML` | Python package — existing | Already in `pyproject.toml`. Used by `cost_table.py` for YAML parsing. No version change needed. |
| `fnmatch` | Python stdlib | Used for glob pattern matching in `PricingTable.lookup()`. No installation needed. |
| `src/tag/assets/` | Directory — new | Must be created and included in the package manifest (`MANIFEST.in` or `pyproject.toml` `[tool.setuptools.package-data]`). |

---

## 15. Open Questions

| ID | Question | Impact | Owner |
|----|----------|--------|-------|
| OQ-01 | **Cache-read auto-detection:** Anthropic's API response includes a `cache_read_input_tokens` field separate from `input_tokens`. Should TAG detect this field from the Hermes inference response and pass `cache_read=True` automatically to `compute_cost()`, or should it require users to opt in per span? Auto-detection is more accurate but requires parsing the Hermes response schema. | Pricing accuracy for users relying on prompt caching | Engineering |
| OQ-02 | **`tag pricing update` registry URL:** Should the community-maintained pricing YAML live in the TAG GitHub repo (e.g., `raw.githubusercontent.com/tag-agent/tag/main/src/tag/assets/pricing.yaml`) or a separate maintained repository? A separate repo allows faster pricing updates without a full TAG release cycle. | Update velocity vs. governance | Team |
| OQ-03 | **Per-day budget enforcement mechanics:** `per_day_usd` enforcement requires querying `SUM(cost_usd)` across all runs today (UTC). This query is correct but runs against the full `spans` table without a date-range index on `started_at`. Should we add `CREATE INDEX idx_spans_started_at ON spans(started_at)` to support this query efficiently for users with many spans? | Query performance at scale | Engineering |
| OQ-04 | **Cost display in `tag runs` table:** Should the existing `tag runs` output gain a `COST` column that sums `cost_usd` from spans per run? This would be a natural extension of PRD-012's `estimated_cost_usd` column with better granularity, but it adds a JOIN to every `tag runs` invocation. | UX vs. performance | Product |
| OQ-05 | **OpenRouter pricing catalog integration:** PRD-012 already calls `load_openrouter_catalog()` to fetch OpenRouter pricing. Should `cost_table.py` optionally delegate to this catalog for models not in the bundled YAML, effectively making the OpenRouter catalog a fallback? This would expand coverage to all ~300 models in the OpenRouter catalog. | Model coverage | Engineering |
| OQ-06 | **`tag budget` warning de-duplication:** The `warn_at_pct` warning could fire on every `close_span()` call once the threshold is crossed. Should it fire only once per run (using a flag stored in the trace state), or on every call above the threshold? Repeated warnings may be annoying for long runs with many spans; a single warning is less noisy. | UX | Engineering |

---

## 16. Complexity and Timeline

**Overall Complexity:** XS — This is fundamentally a new YAML-backed dictionary lookup, an `ALTER TABLE` migration, a new column in a `SELECT` query, and CLI rendering additions. No new external dependencies, no architectural changes.

**Estimated Effort:** 1–2 days total.

| Phase | Tasks | Hours |
|-------|-------|-------|
| **Phase 1: Core** (Day 1, AM) | Create `src/tag/assets/pricing.yaml` with ≥ 30 models; implement `PricingEntry`, `PricingTable`, `load_pricing_table()`, `compute_cost()` in `cost_table.py`; unit tests for all formula variants (basic, cache, batch, stacked, unknown, glob, exact-beats-glob). | 3 |
| **Phase 2: Span integration** (Day 1, PM) | Add `cost_usd` field to `Span` dataclass; update `close_span()` signature; update `_INSERT_SPAN` SQL and `save_spans_to_db()`; add `ALTER TABLE` migration in `open_db()`; update `close_span()` call sites in `controller.py` to call `compute_cost()` and pass result; unit tests for span persistence and migration. | 3 |
| **Phase 3: CLI surface** (Day 2, AM) | Implement `--cost` flag in `cmd_trace_show`; implement `tag stats --cost` with `--by model/profile/day` SQL queries; implement `tag costs --run-id`; `--json` output for all three; TTY table rendering with Rich; integration tests. | 3 |
| **Phase 4: Budget + Pricing CLI** (Day 2, PM) | Implement `cmd_budget` (set/get/unset); `_check_budget()` enforcement hook; `cmd_pricing` (show/update); `otel_semconv.py` `gen_ai.usage.cost_usd` injection; `BudgetExceededError` propagation test; final documentation pass. | 3 |
| **Total** | | **12 hours** |

**Risks:**

- **`close_span()` call site enumeration:** `controller.py` at ~10,000 lines has many `close_span()` call sites. Missing one means some spans have `cost_usd = null` unexpectedly. Mitigation: `grep -n "close_span(" src/tag/controller.py` to enumerate all call sites before starting Phase 2; add a linting assertion in CI.
- **PyYAML `safe_load` vs. large YAML files:** If `~/.tag/pricing.yaml` is very large (unlikely, but possible if users add thousands of models), the 50 ms parse time NFR may be violated. Mitigation: document a recommended maximum of 500 entries; add a size warning in `tag pricing update`.
- **Hermes response schema for cache token counts (OQ-01):** If auto-detecting cache reads is pursued, it requires understanding Hermes' response format. Deferring OQ-01 to a follow-up PRD de-risks Phase 2.
