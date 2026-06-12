# PRD-022: Prompt Cache Analytics

**Status:** Proposed  
**Priority:** P1  
**Estimated Effort:** S (2–3 days)  
**Affects:** `controller.py` (schema migration, new `cmd_cache`, extend `cmd_costs`), `tag.sqlite3` schema (`runs` table)

---

## 1. Overview

Anthropic's prompt caching feature allows long system prompts and context blocks to be written to a server-side cache (costing 1.25x–2x base input price on first write) and then re-read from cache on subsequent calls (costing 0.1x base input price). This creates a meaningful savings opportunity for TAG profiles with stable, long system prompts — but TAG currently discards the two fields that Anthropic returns to quantify this effect:

- `cache_creation_input_tokens` — tokens written to cache this call (charged at write premium)
- `cache_read_input_tokens` — tokens retrieved from cache this call (charged at 10% of base)

Without recording these fields TAG users have no visibility into whether caching is working, how much money it is saving, or how to improve their cache hit rate. This PRD closes that gap with two new columns in the `runs` table, a Hermes response parsing extension, and a new `tag cache` subcommand family alongside an extension to `tag costs` output.

---

## 2. Goals

1. **Per-profile cache analytics** — record `cache_creation_tokens` and `cache_read_tokens` per run; aggregate by profile to show hit rates and token breakdowns.
2. **USD savings calculation** — compute realized savings vs. a no-cache baseline using the formula `savings_usd = sum(cache_read_tokens) * model_input_price_per_token * 0.9`, and display them in `tag cache stats` and `tag costs`.
3. **Cache eligibility warnings** — detect when a profile has a large, stable system prompt but near-zero cache reads, and surface an actionable warning so the user knows to add `cache_control` breakpoints.
4. **Time-series trending** — provide a daily hit-rate chart via `tag cache trend` so users can see whether recent changes to prompts degraded their cache efficiency.
5. **Integration with `tag costs`** — add `cache_read` and `cache_write` token columns and a `savings` column to the existing `tag costs` table output with no breaking changes to current output format.
6. **Improvement tips** — `tag cache tips --profile NAME` analyses a profile's run history and system prompt structure, then emits targeted advice (e.g., suggesting where to add `cache_control` breakpoints).
7. **No-op on non-Anthropic models** — all new columns default to `0`; the `tag cache` subcommand skips or labels non-Anthropic rows rather than erroring.

---

## 3. Non-Goals

- **Controlling cache behavior** — which tokens are cached is determined entirely by the placement of `cache_control` breakpoints in the system prompt and message blocks. TAG does not inject or move these breakpoints; it only measures outcomes.
- **Cross-provider cache analytics** — Google Gemini, OpenAI, and other providers either do not expose equivalent fields or expose them differently. This PRD is Anthropic-specific. A future PRD may extend the schema and UI for other providers.
- **Real-time cache telemetry** — TAG records cache token counts at run completion, not mid-stream. Sub-run granularity is out of scope.
- **Cache warming** — pre-populating the cache by issuing synthetic requests is not part of this PRD.
- **Modifying the Hermes prompt construction pipeline** — TAG does not restructure prompts to make them cache-eligible; it only advises the user when the prompt structure looks amenable.

---

## 4. User Stories

**US-1 — Checking cache eligibility for a new profile**  
As a TAG user who just created a profile with a 4 000-token system prompt, I want to run `tag cache tips --profile my-agent` and receive a message telling me whether my system prompt is long enough to benefit from caching and whether it contains a `cache_control` marker, so that I can decide whether to add one before paying for repeated runs.

**US-2 — Seeing weekly cache savings**  
As a power user running `tag submit` dozens of times per day, I want to run `tag cache stats --profile coding --since 7d` and see the total number of cache-read tokens, the effective hit rate (cache reads / total input tokens), and the dollar amount saved compared to a no-cache baseline, so that I can justify the time spent tuning my prompts.

**US-3 — Getting warned when cache hit rate drops**  
As a team lead, I want `tag cache stats` to print a warning when a profile's 7-day cache hit rate falls below a configurable threshold (e.g., `--warn-threshold 0.2`), so that I am alerted if a recent prompt edit accidentally broke the cache structure.

**US-4 — Comparing cache efficiency across profiles**  
As a user managing multiple profiles (one for coding, one for research, one for data), I want to run `tag cache stats` without a `--profile` flag and see a table comparing hit rates, write costs, read savings, and net savings per profile, ranked by savings descending, so that I can identify which profile benefits most from caching.

**US-5 — Viewing cache stats inside `tag costs`**  
As a user who already uses `tag costs` for budget tracking, I want the existing costs table to include columns for `cache_write_tok`, `cache_read_tok`, and `savings_usd` without requiring me to learn a new command, so that cache economics are surfaced in the workflow I already use.

**US-6 — Tracking cache trend after a prompt change**  
As a developer who just edited a profile's system prompt, I want to run `tag cache trend --profile coding --days 14` and see a day-by-day bar chart of cache hit rate so I can confirm whether the edit caused a regression, typically visible as a spike in write tokens and a drop in read tokens on the day of the change.

**US-7 — Understanding the cost of cache writes**  
As a budget-conscious user, I want `tag cache stats` to show both the extra cost of cache writes (the 1.25x–2x premium over plain input tokens) and the savings from cache reads, so that I can see the net cost impact, not just gross savings.

---

## 5. Proposed CLI Surface

### 5.1 `tag cache stats`

```
tag cache stats [--profile NAME] [--since DURATION] [--model MODEL_ID]
                [--warn-threshold FLOAT] [--json]
```

Flags:
- `--profile NAME` — filter to a single profile; omit to show all profiles aggregated in a table
- `--since DURATION` — time window expressed as `Nd` (days), `Nw` (weeks), `Nm` (months); defaults to `7d`
- `--model MODEL_ID` — filter to a specific model reference (e.g., `anthropic/claude-sonnet-4-6`)
- `--warn-threshold FLOAT` — print a warning if any shown profile's hit rate is below this fraction (default: none)
- `--json` — emit machine-readable JSON instead of table

Example output (table mode, single profile):
```
Profile: coding | Window: last 7d | Model: anthropic/claude-sonnet-4-6

  Metric                    Value
  ─────────────────────────────────────────────────────
  Runs with cache data      47 / 51
  Total input tokens        1 284 000
  Cache write tokens        62 000       ( 4.8%)
  Cache read tokens         891 000      (69.4%)
  Effective hit rate        69.4%
  Cache write premium       $0.0349
  Cache read savings        $2.4003
  Net savings               $2.3654
```

### 5.2 `tag cache trend`

```
tag cache trend [--profile NAME] [--days N]
```

Flags:
- `--profile NAME` — filter to a single profile
- `--days N` — number of trailing calendar days to show (default: 30)

Outputs an ASCII bar chart of daily cache hit rate (cache_read_tokens / total_input_tokens), one bar per day, with dates on the x-axis. Days with zero runs are shown as empty bars. Example:

```
Cache hit rate — coding — last 14 days

2026-05-30  ██████████████████████  72%
2026-05-31  █████████████████████   69%
2026-06-01  ████████████████████    64%
2026-06-02  ████                    14%  ← prompt edit
2026-06-03  ████████                28%
...
```

### 5.3 `tag cache tips`

```
tag cache tips --profile NAME
```

Inspects the profile's most recent system prompt (stored in the profile config), the SHA stability across the last 20 runs, and the observed hit rate, then prints ranked recommendations:

```
Cache tips for profile: coding

[WARN] Cache hit rate is 14% over the last 7 days (threshold: 30%)
[INFO] System prompt is 3 842 tokens — large enough to benefit from caching

Recommendations:
  1. Add a cache_control breakpoint at the end of your static system prompt block.
     Example: include {"cache_control": {"type": "ephemeral"}} in your system message.
  2. Your system prompt SHA changed in 12 / 20 recent runs. A volatile system prompt
     prevents cache reuse. Consider moving dynamic content to a user-turn message.
  3. The cache window is 5 minutes (default). If your runs are spaced more than 5
     minutes apart, upgrade to an extended cache TTL by using a supported model.
```

### 5.4 `tag costs` extensions

The existing `tag costs` table gains three new right-aligned columns when cache data is present:

```
Run ID                   Profile              Model                                    Tokens     Cost   CacheW    CacheR  Savings
──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
run-abc123               coding               anthropic/claude-sonnet-4-6               12 400   $0.037   1 200    8 900   $0.024
run-def456               research             anthropic/claude-opus-4-8                  8 200   $0.123       0       0     n/a
──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────
TOTAL                                                                                  20 600   $0.160   1 200    8 900   $0.024
```

Columns `CacheW`, `CacheR`, and `Savings` are hidden (shown as absent, not empty) for models where the values are all zero to avoid clutter for non-Anthropic runs.

---

## 6. Functional Requirements

**FR-1 Schema migration** — `_migrate_runs_cost_columns` must be extended to add two new columns to the `runs` table if absent:
  - `cache_creation_tokens INTEGER DEFAULT 0`
  - `cache_read_tokens INTEGER DEFAULT 0`

The migration must be idempotent (guarded by `PRAGMA table_info` column check) and wrapped in the same try/except pattern as existing migrations.

**FR-2 Hermes response parsing** — After each Hermes call that produces a JSON response, the response parser must extract `usage.cache_creation_input_tokens` and `usage.cache_read_input_tokens` (both integers, defaulting to 0 if absent). These values must be stored alongside the existing `prompt_tokens` and `completion_tokens`.

**FR-3 Runs table update** — The `UPDATE runs SET ...` statement executed at run completion must be extended to include `cache_creation_tokens` and `cache_read_tokens` using the values extracted in FR-2.

**FR-4 Hit rate calculation** — The hit rate for a set of runs is defined as:
  `hit_rate = sum(cache_read_tokens) / (sum(prompt_tokens) + sum(cache_read_tokens))`
  where the denominator represents total input tokens sent to the model (excluding cache write tokens, which are a subset of prompt tokens in the Anthropic model). If the denominator is zero, hit rate is reported as `null` / `n/a`.

**FR-5 USD savings formula** — Cache read savings are calculated as:
  `savings_usd = sum(cache_read_tokens) / 1_000 * model_input_price_per_token_per_1k * 0.9`
  The factor `0.9` reflects that cache reads cost 0.1x base vs. 1.0x base (a 90% discount). The `model_input_price_per_token_per_1k` is taken from `_COST_TABLE` keyed on `model_id`; unknown models use the default fallback rate.

**FR-6 Cache write premium calculation** — The extra cost of writing to cache is:
  `write_premium_usd = sum(cache_creation_tokens) / 1_000 * model_input_price_per_1k * (write_multiplier - 1.0)`
  where `write_multiplier` is 1.25 for most Anthropic models (2.0 for Claude 3 Haiku). The net savings column in `tag costs` is `savings_usd - write_premium_usd`.

**FR-7 Cache eligibility check** — `tag cache tips` computes the SHA-256 of the system prompt string for each of the last 20 runs of the profile. If fewer than 50% of consecutive run pairs share the same SHA, the tips command emits a "volatile system prompt" warning.

**FR-8 System prompt length threshold** — A system prompt is considered "large enough to cache" if its token count exceeds 1 024 tokens (Anthropic's documented minimum for caching to be cost-effective). The tips command must estimate token count using a simple word-count heuristic (`len(prompt.split()) * 1.3`) when an exact count is unavailable.

**FR-9 `tag cache stats` aggregation** — The stats command must support aggregation at three levels: single profile + single model, single profile (all models), and all profiles (all models). The per-profile table must be sorted by net savings descending.

**FR-10 `tag cache trend` rendering** — The trend chart must use Unicode block characters (`█`) scaled to the width of the terminal (default 40 columns if terminal width is unavailable). Each row must show the date, bar, and percentage. Days with no runs must show `(no data)` rather than a 0% bar to avoid misleading impressions.

**FR-11 `--warn-threshold` flag** — When supplied to `tag cache stats`, if any displayed profile's hit rate is below the threshold, the command must print a warning line and exit with status code `1` so that CI pipelines can use the check as a quality gate. Without the flag, the command always exits `0`.

**FR-12 `tag costs` backward compatibility** — The new cache columns in `tag costs` must not change the width or position of existing columns when all cache_creation_tokens and cache_read_tokens values are zero (i.e., for pre-migration rows and non-Anthropic models). The columns are only appended to the right when at least one row in the result set has non-zero cache data.

**FR-13 `--json` output for `tag cache stats`** — JSON output must include the following keys at the top level:
  `profile`, `window_days`, `runs_total`, `runs_with_cache_data`, `cache_write_tokens`, `cache_read_tokens`, `prompt_tokens`, `hit_rate`, `savings_usd`, `write_premium_usd`, `net_savings_usd`.

---

## 7. Non-Functional Requirements

**NFR-1 Backward-compatible schema migration** — Existing TAG installations must upgrade seamlessly. The two new columns default to `0` so all historic rows remain queryable without errors. The migration must complete in under 50 ms on databases with up to 100 000 run rows (SQLite `ALTER TABLE ADD COLUMN` without a full rewrite satisfies this).

**NFR-2 No-op for non-Anthropic models** — All logic that touches `cache_creation_tokens` and `cache_read_tokens` must be guarded by a provider check (`model_id.startswith("anthropic/")` or `provider == "anthropic"`). Non-Anthropic runs silently leave these columns at `0`. The `tag cache` subcommand must print a clear message (not an error) when a profile uses only non-Anthropic models.

**NFR-3 Performance** — `tag cache stats` must respond in under 200 ms for a database with 10 000 runs. All queries must use the existing `master_profile` and `created_at` columns which are already present; no new indexes are required for correctness, but a composite index `(master_profile, created_at)` on the `runs` table is recommended if it does not already exist.

**NFR-4 No new runtime dependencies** — The implementation must use only Python standard library modules and the SQLite connection already established in `controller.py`. No new packages may be added to `pyproject.toml`.

**NFR-5 Zero-config defaults** — All new subcommands must work without any additional configuration in `cli-config.yaml`. The feature is opt-in by usage, not by configuration.

---

## 8. Technical Design

### 8.1 Changed files

| File | Change |
|---|---|
| `src/tag/controller.py` | Schema migration extension, `cmd_cache` (stats/trend/tips), extend `cmd_costs` |

No new files are required. All logic lives in `controller.py`, following the established pattern for `cmd_costs`, `cmd_trace`, and `cmd_runs`.

### 8.2 Schema migration

Extend `_migrate_runs_cost_columns` to add two rows to the `migrations` list:

```python
("cache_creation_tokens", "INTEGER DEFAULT 0"),
("cache_read_tokens",     "INTEGER DEFAULT 0"),
```

No other schema change is needed. The `runs` table after migration:

```sql
runs (
  id TEXT PRIMARY KEY,
  created_at TEXT NOT NULL,
  ...existing columns...,
  prompt_tokens      INTEGER DEFAULT 0,
  completion_tokens  INTEGER DEFAULT 0,
  total_tokens       INTEGER DEFAULT 0,
  estimated_cost_usd REAL,
  model_id           TEXT,
  provider           TEXT,
  cache_creation_tokens INTEGER DEFAULT 0,   -- NEW
  cache_read_tokens     INTEGER DEFAULT 0    -- NEW
)
```

### 8.3 Hermes response parsing

Anthropic returns usage metadata in the top-level `usage` object of the response JSON. The existing code that extracts `prompt_tokens` and `completion_tokens` must be extended:

```python
cache_write = int((usage.get("cache_creation_input_tokens") or 0))
cache_read  = int((usage.get("cache_read_input_tokens") or 0))
```

These values are then passed through to the run-completion `UPDATE runs SET` statement alongside existing token columns.

### 8.4 Savings formula

Full formula with all components:

```python
def _cache_savings(
    cache_read_tokens: int,
    cache_creation_tokens: int,
    model_id: str,
) -> tuple[float, float, float]:
    """Returns (savings_usd, write_premium_usd, net_savings_usd)."""
    entry = _COST_TABLE.get(model_id, {"prompt": 0.001, "completion": 0.002})
    input_rate = entry["prompt"]          # USD per 1k tokens

    # Reads cost 0.1x base; saving vs. full price is 0.9x
    savings = (cache_read_tokens / 1_000) * input_rate * 0.9

    # Writes cost 1.25x base for most models; extra vs. base is 0.25x
    # claude-haiku models use 2.0x multiplier
    write_mult = 2.0 if "haiku" in model_id else 1.25
    write_premium = (cache_creation_tokens / 1_000) * input_rate * (write_mult - 1.0)

    return savings, write_premium, savings - write_premium
```

### 8.5 Cache eligibility check (tips logic)

```python
def _cache_tips(profile: str, conn: sqlite3.Connection) -> list[str]:
    tips = []
    rows = conn.execute(
        "SELECT prompt, cache_read_tokens, prompt_tokens, created_at "
        "FROM runs WHERE master_profile = ? ORDER BY created_at DESC LIMIT 20",
        (profile,)
    ).fetchall()

    if not rows:
        return ["No run history found for this profile."]

    # SHA stability check
    shas = [hashlib.sha256(r[0].encode()).hexdigest() for r in rows]
    stable = sum(a == b for a, b in zip(shas, shas[1:])) / max(len(shas) - 1, 1)
    if stable < 0.5:
        tips.append("VOLATILE_PROMPT")

    # Hit rate check
    total_input = sum(r[3] for r in rows)
    cache_read  = sum(r[1] for r in rows)
    hit_rate = cache_read / total_input if total_input > 0 else 0.0
    if hit_rate < 0.3:
        tips.append("LOW_HIT_RATE")

    # Length check (use most recent prompt as proxy)
    recent_prompt = rows[0][0]
    estimated_tokens = len(recent_prompt.split()) * 1.3
    if estimated_tokens > 1024:
        tips.append("LONG_PROMPT_NO_CACHE_CONTROL")

    return tips
```

Tip codes are mapped to human-readable messages with concrete examples in the `cmd_cache_tips` renderer.

### 8.6 `cmd_cache` structure

```python
def cmd_cache(args: argparse.Namespace) -> int:
    sub = getattr(args, "cache_subcommand", None)
    if sub == "stats":
        return _cmd_cache_stats(args)
    if sub == "trend":
        return _cmd_cache_trend(args)
    if sub == "tips":
        return _cmd_cache_tips(args)
    # default: print help
    ...
```

### 8.7 `tag costs` extension

`cmd_costs` detects whether any rows in the result set have non-zero cache columns and conditionally appends three columns:

```python
has_cache = any((r[8] or 0) + (r[9] or 0) > 0 for r in rows)
```

When `has_cache` is True, the header and per-row format strings gain `CacheW`, `CacheR`, and `Savings` columns. When False, output is identical to the current format.

### 8.8 Argument parser additions

```python
# Under existing `costs` subparser — no new args needed beyond what is there

# New top-level subcommand:
cache_cmd = sub.add_parser("cache", help="Prompt cache analytics")
cache_sub = cache_cmd.add_subparsers(dest="cache_subcommand")

stats_p = cache_sub.add_parser("stats", help="Show cache hit rates and savings")
stats_p.add_argument("--profile")
stats_p.add_argument("--since", default="7d")
stats_p.add_argument("--model")
stats_p.add_argument("--warn-threshold", type=float)
stats_p.add_argument("--json", action="store_true")

trend_p = cache_sub.add_parser("trend", help="Daily hit rate chart")
trend_p.add_argument("--profile")
trend_p.add_argument("--days", type=int, default=30)

tips_p = cache_sub.add_parser("tips", help="Suggestions to improve cache hit rate")
tips_p.add_argument("--profile", required=True)
```

---

## 9. Security Considerations

**SC-1 No new attack surface** — `tag cache` is a read-only analytics command. It queries the local SQLite database and reads profile config files that `tag` already accesses for all other commands. No new network calls, no new file writes, no new IPC channels. The threat model is unchanged.

**SC-2 Cache analytics are read-only** — `cmd_cache` and its sub-functions never modify the `runs` table or any profile configuration. Write operations are strictly limited to the schema migration (`ALTER TABLE ADD COLUMN`), which runs once at database open time and is the same pattern used by all existing migrations.

---

## 10. Testing Strategy

**T-1 Schema migration tests** — Verify that `_migrate_runs_cost_columns` adds `cache_creation_tokens` and `cache_read_tokens` to a database that does not have them, that it is idempotent (running it twice does not error), and that a database with pre-existing rows has those columns default to `0`.

**T-2 Hermes response parsing tests** — Given a mock Hermes JSON response containing `usage.cache_creation_input_tokens = 500` and `usage.cache_read_input_tokens = 3000`, verify that the parsed values are correctly propagated into the in-memory run record and written to the database.

**T-3 Savings calculation tests** — Unit-test `_cache_savings` against known values:
  - `cache_read=10_000, cache_creation=0, model="anthropic/claude-sonnet-4-6"` → `savings = 10_000 / 1000 * 0.003 * 0.9 = $0.027`
  - `cache_read=0, cache_creation=5_000, model="anthropic/claude-sonnet-4-6"` → `write_premium = 5_000 / 1000 * 0.003 * 0.25 = $0.00375`
  - Non-Anthropic model (zero cache tokens) → all outputs `0.0`

**T-4 Hit rate formula tests** — Verify edge cases: all-zero denominator returns `None`; 100% hit rate; typical 70% hit rate; profile with zero runs.

**T-5 Tips generation tests** — Create mock run histories and verify:
  - Stable SHA across 20 runs → no `VOLATILE_PROMPT` tip
  - SHA changes in 15 of 19 consecutive pairs → `VOLATILE_PROMPT` tip present
  - System prompt with 2 000 estimated tokens and hit_rate < 0.3 → `LONG_PROMPT_NO_CACHE_CONTROL` tip present
  - Hit rate of 0.8 → `LOW_HIT_RATE` tip absent

**T-6 `tag cache stats` CLI tests** — Verify that `cmd_cache_stats` with `--profile` and `--since 7d` returns exit code `0` and contains expected header strings; with `--json` returns valid JSON; with `--warn-threshold 0.5` and a low hit rate returns exit code `1`.

**T-7 `tag costs` backward compatibility tests** — Verify that when all `cache_creation_tokens` and `cache_read_tokens` are `0`, the output of `cmd_costs` is byte-for-byte identical to the current output format.

**T-8 Trend chart tests** — Verify that `_cmd_cache_trend` correctly groups runs by calendar day, that days with no runs show `(no data)`, and that the bar width scales correctly given a known terminal width.

---

## 11. Acceptance Criteria

**AC-1** — After `tag submit` against an Anthropic model, the resulting row in the `runs` table has non-null `cache_creation_tokens` and `cache_read_tokens` values (which may be `0` if the system prompt is not cached).

**AC-2** — `tag cache stats --profile coding --since 7d` prints a table containing at minimum: run count, cache write tokens, cache read tokens, hit rate as a percentage, savings in USD, and write premium in USD.

**AC-3** — `tag cache stats --profile coding --since 7d --warn-threshold 0.5` exits with code `1` when the observed hit rate is below 0.5, and with code `0` when above.

**AC-4** — `tag cache stats --json` outputs valid JSON that parses without error and contains all keys defined in FR-13.

**AC-5** — `tag cache trend --profile coding --days 14` outputs a chart with exactly 14 rows (one per day), each containing the date string in `YYYY-MM-DD` format.

**AC-6** — `tag cache tips --profile coding` outputs at least one line of advice when the profile has run history; it exits `0` and never crashes even when the profile has zero runs.

**AC-7** — `tag costs` output is unchanged (column count, column widths, values) for a database where all `cache_creation_tokens` and `cache_read_tokens` values are `0`.

**AC-8** — `tag costs` output includes `CacheW`, `CacheR`, and `Savings` columns when at least one run in the result set has non-zero cache token counts.

**AC-9** — Running `tag cache stats` against a profile using a non-Anthropic model (e.g., `google/gemini-2.5-pro`) prints a clear informational message and exits `0` without raising an exception.

**AC-10** — The schema migration runs without error on an existing database that pre-dates this feature, and all existing rows have `cache_creation_tokens = 0` and `cache_read_tokens = 0`.

---

## 12. Dependencies

No new runtime dependencies. All implementation uses:
- Python standard library (`sqlite3`, `hashlib`, `datetime`, `json`, `argparse`)
- Existing internal modules already imported in `controller.py`
- Existing `_COST_TABLE` dict for per-model pricing

No changes to `pyproject.toml` or `package.json` are required.

---

## 13. Open Questions

**OQ-1 Detecting `cache_control` breakpoints in system prompts** — The tips logic in FR-7 and FR-8 currently inspects the raw system prompt string for token length and SHA stability. It does not parse the structured message format for the presence of `{"cache_control": {"type": "ephemeral"}}` blocks, because TAG stores the raw prompt text, not the structured Hermes message. Should TAG also store the serialised message structure (or a flag indicating whether cache_control was sent) so that tips can distinguish "cache breakpoint present but miss" from "no breakpoint at all"?

**OQ-2 5-minute vs. 1-hour cache window tracking** — Anthropic's cache TTL is 5 minutes by default, extended to 1 hour for certain model/tier combinations. TAG currently records run timestamps but not inter-run gaps. If runs are spaced more than 5 minutes apart, cache misses are expected and the tips logic should not flag them as fixable. Should TAG record whether the run interval exceeded the cache TTL window, and suppress the `LOW_HIT_RATE` warning accordingly?

**OQ-3 Multi-provider cache analytics** — Google Gemini and potentially other providers are adding context caching features with their own billing models. Should the `cache_creation_tokens` and `cache_read_tokens` columns be designed as generic cache accounting columns (applicable to any provider), or should they remain Anthropic-specific with provider-specific columns added in a future PRD?

**OQ-4 Cache write premium multiplier per model** — The 1.25x and 2.0x write multipliers are hardcoded by model family name heuristic. Anthropic has changed these rates in the past and may do so again. Should the write multiplier be stored in `_COST_TABLE` alongside `prompt` and `completion` rates, or kept as a runtime heuristic? Hardcoding in `_COST_TABLE` would require updating that dict each time Anthropic changes pricing.

**OQ-5 Per-step cache tracking** — A `tag swarm` run may involve multiple Hermes calls across different profiles and models, each with their own cache token counts. The current design aggregates cache tokens at the run level in the `runs` table. Should cache tokens also be tracked at the `steps` level (in the `steps` table) for swarm transparency, or is run-level aggregation sufficient for V1?

---

## 14. Complexity and Timeline

**Complexity:** S

**Estimated implementation time:** 2–3 days

| Task | Estimated time |
|---|---|
| Extend `_migrate_runs_cost_columns` (FR-1) | 0.5 h |
| Hermes response parsing extension (FR-2, FR-3) | 1 h |
| `_cache_savings` and `_cache_tips` helper functions | 2 h |
| `cmd_cache` with stats/trend/tips subcommands | 3 h |
| Extend `cmd_costs` with cache columns (FR-12) | 1 h |
| Argument parser wiring | 0.5 h |
| Unit tests (T-1 through T-8) | 3 h |
| Manual QA and edge-case fixes | 1 h |
| **Total** | **~12 h** |

The implementation is self-contained within `controller.py`, requires no new files, no new dependencies, and no changes to the Hermes configuration format. It is safe to ship behind a standard feature branch with a single PR.
