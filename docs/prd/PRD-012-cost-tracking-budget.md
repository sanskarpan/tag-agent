# PRD-012: Cost Tracking & Budget Management

**Status:** Proposed  
**Priority:** P1  
**Estimated Effort:** M (2 weeks)  
**Affects:** `controller.py` (new `cmd_costs`), `tag.sqlite3` schema, `run_chat_step`, `cmd_submit`

---

## 1. Overview

AI agent costs can grow unbounded — a single `tag swarm` run can consume millions of tokens across multiple profiles and models. TAG has no cost visibility: there are no token counts, no dollar estimates, and no budget limits. This PRD adds a cost tracking layer that records per-run token usage, estimates costs based on OpenRouter and Anthropic pricing, shows cost summaries in `tag runs`, and enforces soft/hard budget limits per profile.

---

## 2. Problem Statement

- Users have no idea how much a `tag submit` or `tag benchmark` costs until they see the monthly bill.
- There is no way to set a budget limit to prevent runaway agent costs.
- `tag runs` shows task names and statuses but zero cost information.
- Multi-model teams (different models per profile) have no way to compare model costs.
- Benchmarks that run many cases can hit unexpected cost spikes.

---

## 3. Goals

1. Per-run token usage (prompt tokens, completion tokens) is recorded in the `runs` table.
2. `tag costs` command shows cost summary by profile, model, date range.
3. Per-profile budget limits: `budget_limit_usd: 5.00` in profile config triggers warnings at 80% and hard stops at 100%.
4. `tag costs --run <id>` shows itemized cost for a specific run.
5. Model pricing is fetched from OpenRouter's public catalog (already used in `load_openrouter_catalog()`).
6. Cost estimates are best-effort — actual billing is the source of truth.

---

## 4. Non-Goals

- Integrating with payment systems or billing APIs.
- Real-time cost streaming (estimated at end of run).
- Supporting models not in OpenRouter catalog (manual rate override possible).

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag costs` | I see how much I've spent this month per profile |
| U2 | Developer | set `budget_limit_usd: 2.00` on researcher profile | researcher stops at $2 and asks me to continue |
| U3 | Manager | run `tag costs --from 2025-01-01 --to 2025-01-31` | I prepare the team's monthly AI spend report |
| U4 | Developer | run `tag costs --run abc123` | I see exact token/cost breakdown for that task |
| U5 | Developer | run `tag benchmark` and see total cost at the end | I know benchmark ROI |

---

## 6. Technical Design

### 6.1 Schema additions to `runs` table

```sql
-- Add columns to existing runs table via migration
ALTER TABLE runs ADD COLUMN prompt_tokens INTEGER DEFAULT 0;
ALTER TABLE runs ADD COLUMN completion_tokens INTEGER DEFAULT 0;
ALTER TABLE runs ADD COLUMN total_tokens INTEGER DEFAULT 0;
ALTER TABLE runs ADD COLUMN estimated_cost_usd REAL;
ALTER TABLE runs ADD COLUMN model_id TEXT;
ALTER TABLE runs ADD COLUMN provider TEXT;
```

Add migration logic to `open_db()`:
```python
def _migrate_runs_table(conn: sqlite3.Connection) -> None:
    """Add cost columns if they don't exist."""
    cursor = conn.execute("PRAGMA table_info(runs)")
    existing_cols = {row[1] for row in cursor.fetchall()}
    new_cols = {
        "prompt_tokens": "INTEGER DEFAULT 0",
        "completion_tokens": "INTEGER DEFAULT 0",
        "total_tokens": "INTEGER DEFAULT 0",
        "estimated_cost_usd": "REAL",
        "model_id": "TEXT",
        "provider": "TEXT",
    }
    for col, type_def in new_cols.items():
        if col not in existing_cols:
            conn.execute(f"ALTER TABLE runs ADD COLUMN {col} {type_def}")
    conn.commit()
```

### 6.2 Token extraction from Hermes output

Hermes outputs token usage in its JSON response (or in stdout). Extract via regex:

```python
TOKEN_USAGE_PATTERN = re.compile(
    r'"usage"\s*:\s*\{[^}]*"prompt_tokens"\s*:\s*(\d+)[^}]*"completion_tokens"\s*:\s*(\d+)',
    re.DOTALL,
)

def extract_token_usage(output: str) -> tuple[int, int]:
    """Extract (prompt_tokens, completion_tokens) from Hermes output."""
    match = TOKEN_USAGE_PATTERN.search(output)
    if match:
        return int(match.group(1)), int(match.group(2))
    return 0, 0
```

Also parse Hermes' `--verbose` output which may include `Tokens: 1234 prompt / 567 completion`.

### 6.3 Cost calculation

```python
# Pricing per million tokens (USD) — fetched/cached from OpenRouter catalog
DEFAULT_PRICING: dict[str, dict[str, float]] = {
    "deepseek/deepseek-v4-flash": {"prompt": 0.14, "completion": 0.28},
    "qwen/qwen3-coder": {"prompt": 0.30, "completion": 0.90},
    "deepseek/deepseek-v4-pro": {"prompt": 0.27, "completion": 1.10},
    "anthropic/claude-sonnet-4-6": {"prompt": 3.00, "completion": 15.00},
}

def estimate_cost(
    model_id: str,
    prompt_tokens: int,
    completion_tokens: int,
    pricing: dict | None = None,
) -> float | None:
    """Return estimated cost in USD, or None if model not in pricing table."""
    p = (pricing or DEFAULT_PRICING).get(model_id)
    if not p:
        return None
    return (prompt_tokens * p["prompt"] + completion_tokens * p["completion"]) / 1_000_000
```

### 6.4 Budget enforcement

In `profile_exec_env()`, check budget before allowing a run:

```python
def check_profile_budget(
    cfg: dict[str, Any],
    profile_name: str,
    db: sqlite3.Connection,
) -> dict[str, Any]:
    """Check if profile has exceeded its budget. Returns {ok, spent, limit, pct}."""
    profile_cfg = cfg.get("profiles", {}).get(profile_name, {})
    limit = profile_cfg.get("config", {}).get("budget_limit_usd")
    if limit is None:
        return {"ok": True, "spent": 0.0, "limit": None}
    
    # Sum costs for this profile this month
    month_start = dt.datetime.utcnow().replace(day=1, hour=0, minute=0, second=0).isoformat()
    cursor = db.execute(
        "SELECT COALESCE(SUM(estimated_cost_usd), 0) FROM runs "
        "WHERE profile = ? AND created_at >= ? AND estimated_cost_usd IS NOT NULL",
        (profile_name, month_start),
    )
    spent = cursor.fetchone()[0]
    pct = (spent / limit) * 100 if limit > 0 else 0
    
    return {"ok": spent < limit, "spent": spent, "limit": limit, "pct": pct}
```

### 6.5 `cmd_costs` command

```python
def cmd_costs(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    db = open_db(cfg)
    
    if getattr(args, "run_id", None):
        # Show single run
        row = db.execute(
            "SELECT profile, model_id, prompt_tokens, completion_tokens, estimated_cost_usd "
            "FROM runs WHERE id = ?", (args.run_id,)
        ).fetchone()
        if not row:
            print(f"Run {args.run_id} not found", file=sys.stderr)
            return 1
        print(f"Profile:     {row[0]}")
        print(f"Model:       {row[1] or 'unknown'}")
        print(f"Tokens:      {row[2]:,} prompt / {row[3]:,} completion")
        cost = row[4]
        print(f"Est. cost:   {'${:.4f}'.format(cost) if cost else 'unknown'}")
        return 0
    
    # Aggregate by profile
    from_date = getattr(args, "from_date", None)
    to_date = getattr(args, "to_date", None)
    
    query = """
        SELECT profile, 
               COUNT(*) as runs,
               COALESCE(SUM(prompt_tokens), 0) as prompt_tokens,
               COALESCE(SUM(completion_tokens), 0) as completion_tokens,
               COALESCE(SUM(estimated_cost_usd), 0) as cost_usd
        FROM runs
        WHERE estimated_cost_usd IS NOT NULL
    """
    params = []
    if from_date:
        query += " AND created_at >= ?"
        params.append(from_date)
    if to_date:
        query += " AND created_at <= ?"
        params.append(to_date + "T23:59:59")
    query += " GROUP BY profile ORDER BY cost_usd DESC"
    
    rows = db.execute(query, params).fetchall()
    
    if args.json:
        print(json.dumps([{
            "profile": r[0], "runs": r[1],
            "prompt_tokens": r[2], "completion_tokens": r[3],
            "estimated_cost_usd": round(r[4], 4)
        } for r in rows], indent=2))
        return 0
    
    total_cost = sum(r[4] for r in rows)
    print(f"\n{'Profile':<20} {'Runs':>6} {'Prompt Tok':>12} {'Compl Tok':>12} {'Est. Cost':>10}")
    print("─" * 64)
    for r in rows:
        print(f"{r[0]:<20} {r[1]:>6} {r[2]:>12,} {r[3]:>12,} {'${:.4f}'.format(r[4]):>10}")
    print("─" * 64)
    print(f"{'TOTAL':<20} {'':>6} {'':>12} {'':>12} {'${:.4f}'.format(total_cost):>10}\n")
    db.close()
    return 0
```

### 6.6 default.yaml schema extension

```yaml
profiles:
  researcher:
    config:
      budget_limit_usd: 5.00      # monthly limit; null = unlimited
      budget_warn_pct: 80         # warn at this percentage
```

---

## 7. Implementation Plan

| Step | Task |
|------|------|
| 1 | Add migration for cost columns to `open_db()` |
| 2 | Implement `extract_token_usage`, `estimate_cost` |
| 3 | Update `insert_run` and `update_run_status` to store token/cost data |
| 4 | Implement `check_profile_budget` |
| 5 | Add budget check to `cmd_submit` and `cmd_chat` (warn only, no hard block in v1) |
| 6 | Implement `cmd_costs` |
| 7 | Register `costs` parser with `--run`, `--from`, `--to`, `--profile`, `--json` args |
| 8 | Integrate pricing fetch into `load_openrouter_catalog()` caching |
| 9 | Tests: `test_estimate_cost_known_model`, `test_budget_check_warn_threshold`, `test_cmd_costs_aggregates_by_profile` |

---

## 8. Success Metrics

- `tag costs` shows non-zero data after running at least one `tag submit`.
- Budget warning prints when a profile has used > 80% of its limit.
- `tag costs --run <id>` shows token counts.
- `tag costs --json` produces valid JSON.

---

## 9. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Token counts not available in Hermes output | Parse best-effort; show "unknown" when unavailable; file Hermes issue |
| Pricing table becomes stale | Fetch from OpenRouter catalog on demand; cache with 24h TTL |
| Users rely on cost estimates for billing | Add prominent "Estimates only. Check provider dashboards for actual billing." disclaimer |
| Hard budget stop could break ongoing work | v1 warns only; hard stop is opt-in via `--enforce-budget` flag |
