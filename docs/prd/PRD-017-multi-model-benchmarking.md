# PRD-017: Multi-Model Benchmarking & Comparison

**Status:** Proposed  
**Priority:** P2  
**Estimated Effort:** M (2 weeks)  
**Affects:** `controller.py` (`cmd_benchmark`, new `cmd_compare`), `benchmark-suite.yaml`

---

## 1. Overview

TAG already has a benchmark system (`tag benchmark`) that runs a suite of cases against one profile/model. But it cannot compare multiple models head-to-head. In 2025, the model landscape has 30+ viable options with wildly different cost/quality/latency tradeoffs. This PRD extends the benchmark harness to run the same tasks across multiple models in parallel, score outputs with an LLM judge, and produce a ranked comparison table — giving teams data to drive model selection for each agent role.

---

## 2. Problem Statement

- `tag benchmark` tests one model at a time — comparing models requires manual runs and spreadsheet comparison.
- There is no automated way to answer "should researcher use deepseek-v4-flash or qwen3-coder?"
- New models ship every week; teams need a systematic way to evaluate them against known tasks.
- The existing `case_passed()` function uses simple regex matching — there is no quality scoring beyond pass/fail.
- Benchmark results have no cost data, so the cost/quality tradeoff is invisible.

---

## 3. Goals

1. `tag compare --models m1,m2,m3 --suite <file>` runs all benchmark cases against all models in parallel.
2. Each case output is scored by a configurable judge model (another LLM call) on a 1–5 quality scale.
3. Results table: model × case × (pass/fail, quality score, latency, cost).
4. Summary: model ranking by quality, cost, latency.
5. Results saved to `tag.sqlite3` `benchmark_runs` table.
6. `tag compare --profile researcher --candidates deepseek/deepseek-v4-flash,anthropic/claude-sonnet-4-6` uses the profile's current task suite.

---

## 4. Non-Goals

- Hosting benchmark infrastructure.
- Automated model selection/deployment based on results.
- Custom LLM judge training.

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag compare --models deepseek/v4-flash,claude-sonnet-4-6` | I pick the best model for researcher |
| U2 | Team lead | run `tag compare --suite research-suite.yaml --save` | I have a dataset of model performance over time |
| U3 | Developer | see cost per task for each model | I understand the quality/cost tradeoff |
| U4 | Developer | configure the judge model | I use the most capable judge for critical evals |

---

## 6. Technical Design

### 6.1 Schema additions

```sql
CREATE TABLE IF NOT EXISTS benchmark_comparisons (
    id           TEXT PRIMARY KEY,
    suite_path   TEXT NOT NULL,
    models       TEXT NOT NULL,  -- JSON array of model IDs
    judge_model  TEXT,
    created_at   TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'running'
);

CREATE TABLE IF NOT EXISTS benchmark_results (
    id             TEXT PRIMARY KEY,
    comparison_id  TEXT NOT NULL,
    model_id       TEXT NOT NULL,
    case_id        TEXT NOT NULL,
    output         TEXT,
    passed         INTEGER,       -- 0/1 (regex match)
    quality_score  REAL,          -- 1.0-5.0 from LLM judge, NULL if no judge
    latency_ms     INTEGER,
    prompt_tokens  INTEGER,
    completion_tokens INTEGER,
    cost_usd       REAL,
    error          TEXT
);
```

### 6.2 Judge scoring

```python
def score_output_with_judge(
    cfg: dict[str, Any],
    judge_model: str,
    judge_profile: str,
    case: dict[str, Any],
    output: str,
) -> float | None:
    """Use LLM as judge to score output quality 1-5."""
    prompt = f"""You are evaluating the quality of an AI agent's response.

Task: {case['prompt']}

Expected criteria: {case.get('expected', 'N/A')}

Response to evaluate:
---
{output[:2000]}
---

Score this response from 1 (completely wrong/unhelpful) to 5 (excellent, fully correct).
Respond with ONLY a number: 1, 2, 3, 4, or 5."""
    
    try:
        result = run_chat_step(
            cfg,
            judge_profile,
            prompt,
            extra_args=["--model", judge_model],
        )
        output_text = normalize_chat_output(result.get("output", ""))
        score_str = output_text.strip().split()[0]
        return max(1.0, min(5.0, float(score_str)))
    except (ValueError, Exception):
        return None


def run_model_benchmark_case(
    cfg: dict[str, Any],
    model_id: str,
    case: dict[str, Any],
    profile_name: str,
) -> dict[str, Any]:
    """Run a single benchmark case for a specific model."""
    import time
    start = time.monotonic()
    
    env = profile_exec_env(cfg, profile_name)
    # Override model for this run
    env["HERMES_MODEL_OVERRIDE"] = model_id  # if Hermes supports this env var
    
    try:
        result = run_chat_step(cfg, profile_name, case["prompt"], env_overrides=env)
        output = normalize_chat_output(result.get("output", ""))
        passed, _ = case_passed(case, output)
        latency = int((time.monotonic() - start) * 1000)
        return {
            "model_id": model_id,
            "case_id": case.get("id", case["prompt"][:40]),
            "output": output,
            "passed": passed,
            "latency_ms": latency,
            "error": None,
        }
    except Exception as e:
        return {
            "model_id": model_id,
            "case_id": case.get("id", "?"),
            "output": "",
            "passed": False,
            "latency_ms": int((time.monotonic() - start) * 1000),
            "error": str(e),
        }
```

### 6.3 `cmd_compare` command

```python
def cmd_compare(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    
    models = [m.strip() for m in args.models.split(",")]
    suite_path = benchmark_suite_path(getattr(args, "suite", None))
    cases = load_benchmark_suite(suite_path)
    judge_model = getattr(args, "judge_model", None)
    judge_profile = getattr(args, "judge_profile", cfg["defaults"]["master_profile"])
    profile = getattr(args, "profile", cfg["defaults"]["master_profile"])
    
    comparison_id = str(uuid.uuid4())[:8]
    db = open_db(cfg)
    
    # Run all model×case combinations in parallel
    tasks = [(model, case) for model in models for case in cases]
    results = []
    
    from tag.tui_output import make_benchmark_progress
    progress = make_benchmark_progress()
    
    with (progress or contextlib.nullcontext()):
        task_bar = progress.add_task("comparing", total=len(tasks)) if progress else None
        
        with ThreadPoolExecutor(max_workers=min(len(models) * 2, 8)) as executor:
            futures = {
                executor.submit(run_model_benchmark_case, cfg, model, case, profile): (model, case)
                for model, case in tasks
            }
            for future in as_completed(futures):
                result = future.result()
                
                # Optional: judge scoring
                if judge_model and not result.get("error"):
                    case = futures[future][1]
                    result["quality_score"] = score_output_with_judge(
                        cfg, judge_model, judge_profile, case, result["output"]
                    )
                
                results.append(result)
                if progress and task_bar is not None:
                    progress.advance(task_bar)
    
    # Render comparison table
    _render_comparison_table(results, models, cases)
    
    db.close()
    return 0


def _render_comparison_table(results, models, cases) -> None:
    """Print a model × case comparison table."""
    from tag.tui_output import get_console
    console = get_console()
    
    # Group results by model
    by_model: dict[str, list] = {}
    for r in results:
        by_model.setdefault(r["model_id"], []).append(r)
    
    print(f"\n{'Model':<45} {'Pass%':>6} {'Avg Quality':>12} {'Avg Lat':>10} {'Est Cost':>10}")
    print("─" * 87)
    
    for model in models:
        model_results = by_model.get(model, [])
        if not model_results:
            continue
        passed = sum(1 for r in model_results if r.get("passed"))
        total = len(model_results)
        pass_pct = (passed / total * 100) if total else 0
        
        quality_scores = [r["quality_score"] for r in model_results if r.get("quality_score")]
        avg_quality = sum(quality_scores) / len(quality_scores) if quality_scores else None
        
        avg_lat = sum(r.get("latency_ms", 0) for r in model_results) / max(total, 1)
        
        model_short = model.split("/")[-1][:40]
        quality_str = f"{avg_quality:.1f}/5.0" if avg_quality else "N/A"
        
        print(f"{model_short:<45} {pass_pct:>5.0f}% {quality_str:>12} {avg_lat:>9,.0f}ms {'N/A':>10}")
    
    print()
```

---

## 7. Implementation Plan

| Step | Task |
|------|------|
| 1 | Add `benchmark_comparisons` and `benchmark_results` tables to `open_db()` |
| 2 | Implement `run_model_benchmark_case` |
| 3 | Implement `score_output_with_judge` |
| 4 | Implement `cmd_compare` |
| 5 | Register `compare` parser |
| 6 | Add `--judge-model` and `--judge-profile` args |
| 7 | Tests: `test_compare_runs_all_model_case_combos`, `test_judge_score_clamped_1_5` |
| 8 | Update README with compare command |

---

## 8. Success Metrics

- `tag compare --models m1,m2` runs N×M tasks and produces comparison table.
- `--judge-model` adds quality scores to results.
- Results stored in SQLite after comparison.
- Parallel execution completes 10 cases × 3 models in < 3× single-model time.

---

## 9. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Model override env var not supported by Hermes | Use `hermes chat --model <id>` flag instead |
| Judge LLM returns non-numeric score | Robust parsing with fallback to None |
| Parallel runs hit rate limits | Configurable `--max-concurrent` (default: 4) |
| Large output stored in SQLite | Truncate to 5000 chars in `benchmark_results.output` |
