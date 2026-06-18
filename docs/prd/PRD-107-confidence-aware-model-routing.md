# PRD-107: Confidence-Aware Model Routing with Cost/Accuracy Pareto Optimization (`tag route optimize`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** L (2-4 weeks)
**Category:** Advanced Reasoning & Planning
**Affects:** `routing.py`
**Depends on:** PRD-027 (eval framework — historical accuracy signal), PRD-028 (sandbox — isolated cascade execution), PRD-013 (agent tracing — per-decision span attribution), PRD-034 (secret scanning — prompt content before routing), PRD-012 (budget enforcement — per-tier cost guard), PRD-031 (model fallback chains — cascade overlap concerns), PRD-043 (vector tool retrieval — task-type embedding), PRD-041 (OTel span cost attribution — routing decision spans), PRD-045 (LLM-as-judge — confidence scoring), PRD-101 (self-consistency ensemble — confidence signal source)
**Inspired by:** FrugalGPT, LLM routing (Martian, RouteLLM), Pareto-optimal cascades

---

## 1. Overview

Every TAG task dispatches to a fixed model determined by profile configuration. The `coder` profile always uses `claude-opus-4`; the `researcher` profile always uses `claude-sonnet-4-6`. This hardwired assignment is safe but economically wasteful: empirical evidence from FrugalGPT (Chen et al. 2023) and RouteLLM (Ong et al. 2024) demonstrates that 60–80% of queries in typical workloads can be resolved correctly by a smaller, cheaper model, and only the genuinely difficult tail requires the most capable (and expensive) model tier. TAG currently has no mechanism to make this distinction — every task pays the full premium regardless of whether it needed it.

The Confidence-Aware Model Routing feature introduces `routing.py`, a new first-class module that implements two complementary routing strategies: **pre-routing** (decide which model tier to use *before* any LLM call, in under 10 ms, using a lightweight BERT-class classifier trained on historical eval results) and **cascading** (call the cheapest tier first, score the response confidence, escalate to the next tier only when confidence falls below a threshold). These two strategies map directly to RouteLLM's binary pre-routing approach and FrugalGPT's sequential confidence-gated cascade respectively. TAG implements both, selectable per-task via CLI flags.

The routing system is trained continuously on TAG's own historical data. Every eval result stored by `eval_framework.py` (PRD-027) records which model answered which task type correctly. The router trains on this ground truth: a SentenceBERT classifier learns to predict, given a new task's embedding, the probability that each model tier will answer correctly. This produces a calibrated confidence score per tier per query. The `tag route optimize` command reads historical eval results, fits the classifier, and emits a Pareto-optimal routing policy: for each accuracy target (e.g., 0.90), it computes the model mix that minimizes expected cost while hitting that target. The result is stored as a policy in SQLite and applied automatically to subsequent runs.

Cascading (`tag route cascade`) is the reliability-first complement: call `haiku` → if confidence < threshold, call `sonnet` → if confidence < threshold, call `opus`. Unlike PRD-031's fallback chains (which trigger on *error conditions* like 429 or context overflow), cascading triggers on *quality conditions* — low confidence in the answer. Each cascade tier scores its own response using one of three confidence signals: (a) self-reported logit-based confidence markers parsed from structured output, (b) embedding-space consistency with self-consistency samples from PRD-101, or (c) a lightweight DistilBERT quality regression. The cascade exits as soon as a tier's response clears the threshold, paying only for the tiers actually invoked.

The `tag route stats` command surfaces the operational picture: routing policy in effect, per-tier invocation rates, cost saved vs. always-using-the-most-expensive-model, and per-task-type accuracy by tier. Together, `optimize`, `cascade`, and `stats` close the loop: engineers set an accuracy target, the optimizer finds the cheapest policy that hits it, the cascade enforces it at runtime, and stats proves it delivered.

---

## 2. Problem Statement

### 2.1 Uniform High-Cost Model Assignment Wastes Budget on Simple Tasks

A `tag run --profile coder "add a docstring to this function"` dispatches to `claude-opus-4` at roughly $15 per million output tokens. The same task would be answered equivalently by `claude-haiku-4-5` at $0.25 per million output tokens — a 60× cost difference. TAG has no mechanism to detect that this particular task falls into the "easy" category where a smaller model suffices. Over a typical engineering team's usage (hundreds of tasks per day), the accumulated overspend is substantial: empirically, FrugalGPT achieves 4× cost reduction at equal accuracy on MT-Bench by routing 70% of queries to cheaper models. TAG leaves this saving entirely on the table.

### 2.2 No Feedback Loop Between Eval Results and Routing Decisions

`eval_framework.py` (PRD-027) records, for every eval suite run, whether each model succeeded on each task type. This dataset is a natural training signal for a router: if `claude-haiku-4-5` achieves 0.93 accuracy on `add-docstring` tasks but only 0.61 accuracy on `security-audit` tasks, the router should route `add-docstring` to Haiku and `security-audit` to Opus. But today this signal is never consumed by the routing layer. Eval results accumulate in `eval_results` and go nowhere. The routing decision for every task remains hardcoded in the profile YAML, unchanged by any empirical evidence.

### 2.3 Cascade Failure Modes Are Not Separated from Quality Failure Modes

PRD-031 (Model Fallback Chains) handles routing on *error*: context overflow, 429, 5xx. There is no mechanism for routing on *quality*: "the small model answered, but its answer looks uncertain — escalate." These are fundamentally different triggers with different semantics. Error-based fallback fires on definite API failure; quality-based cascade fires on probabilistic confidence below a threshold. Conflating them in the same mechanism would produce incorrect routing in both directions: genuine quality escalation would be suppressed when there is no API error, and error recovery would be confused by confidence scoring. TAG needs a dedicated cascade pathway that is architecturally separate from error-based fallbacks.

---

## 3. Goals and Non-Goals

### 3.1 Goals

| # | Goal |
|---|------|
| G1 | `tag route optimize --profile <name> --accuracy-target <float>` reads historical eval results, fits a SentenceBERT classifier per task-type, and emits a Pareto-optimal routing policy stored in `routing_policies` SQLite table. |
| G2 | The routing policy is applied automatically at `tag run` time when `routing.auto_route: true` is set in the profile, selecting the cheapest predicted-adequate model tier in under 10 ms. |
| G3 | `tag route cascade --task <text> --start-model <id> --fallback-to <id> [--fallback-to <id>...]` executes a sequential confidence-gated cascade, calling each tier only if the previous tier's response confidence is below the configured threshold. |
| G4 | Three confidence signal modes are supported and auto-selected based on output type: `majority-vote` (discrete outputs), `embedding-consistency` (prose, no extra LLM call), `distilbert-quality` (regression score, lowest latency). |
| G5 | `tag route stats --json` reports per-profile routing decisions, tier invocation rates, estimated cost vs. always-opus baseline, and per-task-type accuracy by tier. |
| G6 | Every routing decision is written to the `routing_decisions` SQLite table for audit, analysis, and future retraining. |
| G7 | The classifier is retrained automatically when `tag route optimize` is run; no background daemon or separate training job is required. |
| G8 | `tag route optimize` integrates with `budget.py` (PRD-012): it computes projected monthly cost under the recommended policy and displays it before writing the policy. |
| G9 | All cascade calls are attributed to separate child spans under the parent run span, with `routing.tier`, `routing.confidence`, and `routing.escalated` OTel attributes (PRD-013, PRD-041). |
| G10 | The cascade is architecturally independent of PRD-031 fallback chains; the two co-exist without conflict. A cascade escalation does not consume a fallback hop. |
| G11 | `tag route calibrate --profile <name>` runs a held-out accuracy check on the current routing policy, reporting PGR (Performance Gap Recovered) and cost reduction metrics. |
| G12 | All confidence thresholds, cascade depths, and classifier hyperparameters are exposed in the TAG config YAML under a `routing:` key and documented with defaults. |

### 3.2 Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Real-time provider health monitoring or latency-based routing. Routing decisions are quality- and cost-driven, not latency-driven. |
| NG2 | Multi-provider load balancing across equivalent models. The cascade is quality-gated, not load-distributed. |
| NG3 | Fine-tuning or RLHF on the routing classifier. The classifier is trained on TAG eval results, not on human preference labels. |
| NG4 | Replacing PRD-031 fallback chains. Error-based fallback and quality-based cascade are independent systems. |
| NG5 | Automatic A/B testing or shadow routing in production. Policy changes require explicit `tag route optimize` invocation. |
| NG6 | Cross-user or federated training on routing signals. Training data comes from the local SQLite database only. |
| NG7 | Real-time model pricing updates. Costs are read from the cached OpenRouter catalog; live pricing is not fetched at route time. |
| NG8 | Routing across more than 5 cascade tiers. Deep chains have diminishing returns and increase latency unpredictably. |

---

## 4. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Pre-routing latency | < 10 ms p99 for classifier inference | `time.perf_counter()` around `router.route()` in benchmark test |
| Cost reduction at 0.90 accuracy target | ≥ 30% cost reduction vs. always-opus baseline on TAG eval suite | `tag route stats --json` comparing `total_cost_usd` to `baseline_cost_usd` after 50+ routed tasks |
| Cascade accuracy preservation | Accuracy within 2 pp of always-opus baseline at `--accuracy-target 0.90` | `tag route calibrate --profile coder` on held-out eval cases |
| Routing decision audit completeness | 100% of routing decisions written to `routing_decisions` table | Integration test: count rows after N routed runs |
| Classifier training time | < 30 seconds on 1000 historical eval cases | Benchmark in CI with synthetic dataset |
| PGR metric | PGR ≥ 0.7 on coder profile after 50 eval cases | `tag route calibrate --json` after seeding eval data |
| Zero overhead when disabled | `tag run` wall time statistically identical when `routing.auto_route: false` | 20-run t-test benchmark |
| Cascade span attribution | Every cascade tier produces a distinct child span with `routing.tier` attribute | Integration test asserting span count = tiers invoked |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Platform engineer | run `tag route optimize --profile coder --accuracy-target 0.90` | I get a routing policy that uses the cheapest model mix meeting 90% accuracy, backed by real eval data, and projected cost is shown before I commit |
| U2 | Developer | run `tag route cascade --task "Review this PR" --start-model haiku --fallback-to sonnet --fallback-to opus` | My PR review uses Haiku when it is confident and automatically escalates to Opus only when needed, paying minimum cost per review |
| U3 | Team lead | run `tag route stats --json` after a week of auto-routing | I see a cost breakdown proving routing saved money vs. always-opus, with accuracy by tier, to justify the feature to finance |
| U4 | Developer | see routing decisions in `tag trace show <run_id>` output | I know which model tier was used for each run and why, without reading SQLite directly |
| U5 | Platform engineer | run `tag route calibrate --profile coder` after adding 20 new eval cases | I verify the policy still hits its accuracy target on fresh data before pushing it to production profiles |
| U6 | Developer | set `routing.auto_route: true` in a profile YAML | All subsequent `tag run` invocations with that profile use the routing policy without any extra CLI flags |
| U7 | Cost-conscious team | run `tag route optimize --profile researcher --accuracy-target 0.85 --json` | I get the full Pareto curve showing cost vs. accuracy tradeoff at different thresholds, exported as JSON for reporting |
| U8 | Developer | see cascade escalation events in the TUI as they happen | I understand in real time that the system tried Haiku, found low confidence, and escalated to Sonnet |
| U9 | DevOps engineer | run `tag route optimize` in CI after each eval suite run | The routing policy updates automatically when new eval data proves a model tier has degraded below its threshold |
| U10 | Security reviewer | inspect `routing_decisions` table | I can audit which tasks were routed to which models, with confidence scores, for compliance review |

---

## 6. Proposed CLI Surface

### 6.1 `tag route optimize`

Fit a routing policy from historical eval results and persist it to SQLite.

```
tag route optimize \
  --profile <name> \
  --accuracy-target <float>          # e.g. 0.90; required
  [--min-eval-cases <int>]           # default: 20; refuse to train with fewer cases
  [--models <id,id,...>]             # restrict to these model tiers; default: all known tiers
  [--pareto-curve]                   # output the full cost/accuracy Pareto frontier, not just the recommended point
  [--dry-run]                        # compute and display policy without writing to SQLite
  [--yes]                            # skip projected cost confirmation prompt
  [--json]
```

**Sample output (text):**

```
tag route optimize --profile coder --accuracy-target 0.90

Training router on 147 eval cases across 3 model tiers...
  Tier accuracy estimates:
    claude-haiku-4-5   : 0.84 on coding tasks  (n=52 cases)
    claude-sonnet-4-6  : 0.93 on coding tasks  (n=51 cases)
    claude-opus-4      : 0.97 on coding tasks  (n=44 cases)

  Recommended policy (accuracy target: 0.90):
    Route to claude-haiku-4-5   when confidence_haiku >= 0.72    (estimated 41% of queries)
    Route to claude-sonnet-4-6  when confidence_haiku <  0.72    (estimated 59% of queries)
    Accuracy estimate: 0.912
    Cost vs. always-opus: $0.0031/task vs. $0.0089/task  (-65%)

  Projected monthly cost (assuming 500 tasks/day): $46.50 vs. $133.50 baseline
  Policy ID: policy-coder-20260617-001

Write this policy to routing_policies? [y/N]: y
Policy written. To activate: set routing.auto_route: true in profile 'coder'.
```

**Sample JSON output (`--json`):**

```json
{
  "policy_id": "policy-coder-20260617-001",
  "profile": "coder",
  "accuracy_target": 0.90,
  "accuracy_estimate": 0.912,
  "tiers": [
    {
      "model_id": "anthropic/claude-haiku-4-5",
      "confidence_threshold": 0.72,
      "estimated_fraction": 0.41,
      "cost_per_1m_output": 0.25
    },
    {
      "model_id": "anthropic/claude-sonnet-4-6",
      "confidence_threshold": null,
      "estimated_fraction": 0.59,
      "cost_per_1m_output": 3.00
    }
  ],
  "cost_per_task_usd": 0.0031,
  "baseline_cost_per_task_usd": 0.0089,
  "cost_reduction_pct": 65.2,
  "pgr": 0.74,
  "eval_cases_used": 147,
  "trained_at": "2026-06-17T09:15:00Z"
}
```

### 6.2 `tag route cascade`

Execute a task through a sequential confidence-gated cascade of model tiers.

```
tag route cascade \
  --task <text> \
  --start-model <provider/model-id> \
  --fallback-to <provider/model-id> \
  [--fallback-to <provider/model-id>...] \   # repeatable; up to 4 additional tiers
  [--profile <name>]                          # profile for system prompt + tool grants
  [--threshold <float>]                       # confidence threshold per tier; default: 0.85
  [--confidence-mode majority|embedding|distilbert]  # default: auto (by output type)
  [--max-tiers <int>]                         # hard cap on tiers to invoke; default: 5
  [--json]
```

**Sample output (text):**

```
tag route cascade \
  --task "Review this PR" \
  --start-model haiku \
  --fallback-to sonnet \
  --fallback-to opus

[tier 1] claude-haiku-4-5 → confidence: 0.61 (threshold: 0.85) → ESCALATE
[tier 2] claude-sonnet-4-6 → confidence: 0.91 (threshold: 0.85) → ACCEPT

Final response (claude-sonnet-4-6, 2 tiers, 1.4s, $0.0028):
  The PR introduces a potential SQL injection in line 47 of db.py...

Cascade summary:
  tiers_invoked: 2 / 3
  cost_usd: 0.0028
  saved_vs_opus: 0.0061
```

### 6.3 `tag route stats`

Report routing policy performance across recent runs.

```
tag route stats \
  [--profile <name>]       # filter to one profile; default: all
  [--since <ISO date>]     # default: last 30 days
  [--last <int>]           # last N routing decisions; overrides --since
  [--json]
```

**Sample JSON output:**

```json
{
  "period": "2026-05-17T00:00:00Z / 2026-06-17T00:00:00Z",
  "profile": "coder",
  "total_routed_tasks": 1247,
  "policy_id": "policy-coder-20260617-001",
  "tiers": {
    "anthropic/claude-haiku-4-5": {
      "invocations": 512,
      "fraction": 0.41,
      "escalated": 89,
      "escalation_rate": 0.174
    },
    "anthropic/claude-sonnet-4-6": {
      "invocations": 735,
      "fraction": 0.589,
      "escalated": 0,
      "escalation_rate": 0.0
    }
  },
  "cost_usd_actual": 1823.40,
  "cost_usd_baseline_always_opus": 5289.10,
  "cost_reduction_pct": 65.5,
  "accuracy_estimate": 0.908,
  "pgr": 0.74,
  "apgr": 0.71
}
```

### 6.4 `tag route calibrate`

Run a held-out accuracy check on the current policy.

```
tag route calibrate \
  --profile <name> \
  [--eval-suite <path>]    # YAML eval suite to use; default: all eval_results for profile
  [--holdout-frac <float>] # fraction of eval cases to use as test set; default: 0.2
  [--json]
```

### 6.5 `tag route policy list` / `tag route policy show` / `tag route policy delete`

```
tag route policy list [--json]
tag route policy show <policy-id> [--json]
tag route policy delete <policy-id>
tag route policy activate --profile <name> --policy <policy-id>
```

### 6.6 Config Keys

```yaml
# ~/.tag/profiles/coder.yaml
routing:
  auto_route: true                 # enable pre-routing at run time
  policy_id: policy-coder-20260617-001
  cascade:
    threshold: 0.85                # default confidence threshold for escalation
    confidence_mode: auto          # auto | majority | embedding | distilbert
    max_tiers: 3
  classifier:
    n_samples: 10                  # self-consistency samples for embedding-mode confidence
    temperature: 0.7
    distilbert_threshold: 0.65     # secondary gate when distilbert scorer used
```

---

## 7. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `routing.py` exports a `ModelRouter` class with a `route(query: str, task_type: str) -> ModelChoice` method that returns in under 10 ms using a pre-loaded SentenceBERT classifier. | P0 |
| FR-02 | `ModelRouter.route()` returns `ModelChoice(model_id, confidence, policy_id, reason)`. If no policy is active for the profile, it returns the profile's default model. | P0 |
| FR-03 | Every call to `ModelRouter.route()` writes a row to `routing_decisions` table including `query_hash`, `task_type`, `policy_id`, `chosen_model_id`, `confidence_score`, `tier_index`, `latency_ms`, and `created_at`. | P0 |
| FR-04 | `FrugalCascade` class accepts a list of `(model_id, scorer_fn, threshold)` tuples. It calls each tier in order, stopping when `scorer_fn(response) >= threshold` or all tiers exhausted. | P0 |
| FR-05 | Three scorer implementations are provided: `MajorityVoteScorer` (uses N=`self_consistency.n_samples` parallel calls, returns fraction agreeing with modal answer), `EmbeddingConsistencyScorer` (uses `sentence-transformers`, returns cosine similarity of response to centroid of N=5 samples), `DistilBERTQualityScorer` (loads `distilbert-base-uncased` fine-tuned on quality labels, returns regression score). | P0 |
| FR-06 | Auto-selection of scorer: if the response is valid JSON or a single-token discrete value, use `MajorityVoteScorer`; otherwise use `EmbeddingConsistencyScorer`. `DistilBERTQualityScorer` is only used when explicitly set via `--confidence-mode distilbert` or config. | P1 |
| FR-07 | `RoutingPolicyTrainer.fit(eval_results: list[EvalResult]) -> RoutingPolicy` trains a `sklearn.linear_model.LogisticRegression` classifier on SentenceBERT embeddings of task prompts, with labels = `(model_id, correct: bool)`. Requires minimum `min_eval_cases` (default: 20) samples per tier. | P0 |
| FR-08 | `RoutingPolicyTrainer.pareto_curve(models, eval_results) -> list[ParetoPoint]` computes the cost/accuracy Pareto frontier by sweeping the confidence threshold in 0.01 increments from 0.50 to 0.99, evaluating predicted accuracy and expected cost at each point. | P1 |
| FR-09 | `tag route optimize` reads from `eval_results` and `eval_cases` tables (PRD-027 schema). If fewer than `min_eval_cases` rows exist for the target profile, it prints an error and exits non-zero. | P0 |
| FR-10 | The trained policy is serialized to JSON and stored in `routing_policies` table with fields: `id`, `profile`, `accuracy_target`, `accuracy_estimate`, `policy_json`, `pgr`, `cost_per_task_usd`, `baseline_cost_usd`, `eval_cases_used`, `created_at`, `active`. Only one policy per profile may be `active = 1`. | P0 |
| FR-11 | `tag route optimize --dry-run` computes and displays the policy but does not write to SQLite. Exit code 0. | P1 |
| FR-12 | Cascade escalations are reported as WARNING-level log lines (matching PRD-031's substitution logging convention): `[routing] escalated from {from_model} to {to_model} (confidence={score:.3f} < threshold={threshold:.3f})`. | P1 |
| FR-13 | Each tier in a cascade run emits a child span with attributes `routing.tier=<int>`, `routing.confidence=<float>`, `routing.escalated=<bool>`, `routing.model_id=<str>`, under the parent run span (PRD-013). | P1 |
| FR-14 | `tag route stats` reads from `routing_decisions` table, aggregates by `(profile, model_id)`, computes invocation counts, escalation rates, total cost, and PGR/APGR metrics, and outputs the result in text or JSON. | P1 |
| FR-15 | `tag route calibrate` splits `eval_results` for the profile into train/test (80/20 default), refits the classifier on the train split, evaluates on the test split, and reports: test accuracy by tier, calibration error (ECE), PGR, and APGR. | P1 |
| FR-16 | When `routing.auto_route: true` is set in a profile and a matching active policy exists, `controller.py`'s run dispatch path calls `ModelRouter.route()` before constructing the Hermes call, and substitutes the returned `model_id` into the route. | P0 |
| FR-17 | `tag route policy delete <policy-id>` sets `active = 0` and `deleted_at = now()` on the policy row (soft delete). If the deleted policy was the active policy for its profile, `auto_route` is effectively disabled until a new `tag route optimize` is run. | P1 |
| FR-18 | The cascade hard-caps at 5 tiers regardless of `--fallback-to` count. If more than 5 `--fallback-to` flags are provided, the command errors with a clear message. | P0 |
| FR-19 | `security.py` (PRD-034) secret-scan the task text before routing. Routing decisions are never written with raw task text; only a SHA-256 hash of the task is stored in `query_hash`. | P0 |
| FR-20 | `budget.py` (PRD-012) is consulted before each cascade tier: if the cumulative cascade cost would exceed the active budget, the cascade aborts and returns the highest-confidence response seen so far. | P1 |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Pre-routing classifier inference (SentenceBERT embedding + logistic regression predict) must complete in under 10 ms p99 on a MacBook M-series CPU with the model loaded in memory. | < 10 ms p99 |
| NFR-02 | Classifier training (`RoutingPolicyTrainer.fit`) must complete in under 30 seconds on 1000 eval cases on a MacBook M-series CPU. | < 30 s |
| NFR-03 | `routing.py` must not import `torch`, `sentence_transformers`, or `sklearn` at module load time. All heavy imports happen inside the `ModelRouter.__init__` call (lazy import pattern). | No import-time overhead |
| NFR-04 | When `routing.auto_route: false` (the default), the routing module adds zero measurable overhead to `tag run`. | p50 overhead < 1 ms |
| NFR-05 | `routing_decisions` table writes use WAL-mode SQLite with a 5-second busy timeout (matching `open_db()` conventions). No routing decision write may block `tag run` for more than 5 seconds. | < 5 s write block |
| NFR-06 | The SentenceBERT model used for task embedding is `all-MiniLM-L6-v2` (22 MB) by default. The DistilBERT scorer uses `distilbert-base-uncased` (67 MB). Both are cached in `~/.tag/models/` on first use. | First-use download; cached thereafter |
| NFR-07 | All routing decisions written to SQLite are attributed with `run_id` where available, enabling foreign-key join to the `runs` table. | 100% attribution |
| NFR-08 | `routing.py` has test coverage ≥ 85% as measured by `pytest --cov`. | ≥ 85% coverage |
| NFR-09 | The `FrugalCascade` class must be thread-safe for concurrent cascade invocations (e.g., from `queue_worker.py`). Use per-invocation state; no shared mutable class-level state. | Thread-safe |
| NFR-10 | All user-facing cost figures are displayed to 4 decimal places in USD and prefixed with `$`. | Consistent formatting |

---

## 9. Technical Design

### 9.1 New File: `src/tag/routing.py`

This module is the sole owner of all routing and cascade logic. It does not import from `controller.py` to avoid circular dependencies; `controller.py` imports from `routing.py`.

### 9.2 SQLite DDL

Added to `open_db()` schema migration in `controller.py`:

```sql
-- Stores fitted routing policies per profile
CREATE TABLE IF NOT EXISTS routing_policies (
  id                   TEXT PRIMARY KEY,
  profile              TEXT NOT NULL,
  accuracy_target      REAL NOT NULL,
  accuracy_estimate    REAL NOT NULL,
  policy_json          TEXT NOT NULL,   -- JSON: list of {model_id, threshold, fraction}
  pgr                  REAL,            -- Performance Gap Recovered
  apgr                 REAL,            -- Area under PGR curve
  cost_per_task_usd    REAL NOT NULL,
  baseline_cost_usd    REAL NOT NULL,
  eval_cases_used      INTEGER NOT NULL,
  created_at           TEXT NOT NULL,
  deleted_at           TEXT,
  active               INTEGER NOT NULL DEFAULT 0,  -- 1 = active for profile
  CHECK (active IN (0, 1))
);
CREATE INDEX IF NOT EXISTS idx_rp_profile_active ON routing_policies(profile, active);

-- Audit log of every routing decision made at runtime
CREATE TABLE IF NOT EXISTS routing_decisions (
  id                   TEXT PRIMARY KEY,
  run_id               TEXT,            -- NULL if invoked outside a run (e.g. tag route cascade)
  profile              TEXT NOT NULL,
  policy_id            TEXT,            -- NULL if routed by default (no active policy)
  query_hash           TEXT NOT NULL,   -- SHA-256 of task text; never raw text
  task_type            TEXT,
  chosen_model_id      TEXT NOT NULL,
  confidence_score     REAL,
  tier_index           INTEGER NOT NULL DEFAULT 0,
  was_escalated        INTEGER NOT NULL DEFAULT 0,
  cascade_total_tiers  INTEGER,
  cost_usd             REAL,
  latency_ms           INTEGER,
  created_at           TEXT NOT NULL,
  FOREIGN KEY(run_id) REFERENCES runs(id),
  FOREIGN KEY(policy_id) REFERENCES routing_policies(id)
);
CREATE INDEX IF NOT EXISTS idx_rd_profile_created ON routing_decisions(profile, created_at);
CREATE INDEX IF NOT EXISTS idx_rd_run ON routing_decisions(run_id);

-- Per-tier cascade step log (child rows of routing_decisions)
CREATE TABLE IF NOT EXISTS cascade_steps (
  id                   INTEGER PRIMARY KEY AUTOINCREMENT,
  decision_id          TEXT NOT NULL,
  tier_index           INTEGER NOT NULL,
  model_id             TEXT NOT NULL,
  confidence_score     REAL NOT NULL,
  confidence_mode      TEXT NOT NULL,   -- majority | embedding | distilbert
  threshold            REAL NOT NULL,
  escalated            INTEGER NOT NULL DEFAULT 0,
  response_hash        TEXT NOT NULL,   -- SHA-256 of response text
  input_tokens         INTEGER,
  output_tokens        INTEGER,
  cost_usd             REAL,
  latency_ms           INTEGER NOT NULL,
  FOREIGN KEY(decision_id) REFERENCES routing_decisions(id)
);
CREATE INDEX IF NOT EXISTS idx_cs_decision ON cascade_steps(decision_id, tier_index);
```

### 9.3 Core Dataclasses

```python
# src/tag/routing.py
from __future__ import annotations
import hashlib
import json
import time
import uuid
from dataclasses import dataclass, field
from typing import Callable, Literal

ConfidenceMode = Literal["majority", "embedding", "distilbert", "auto"]


@dataclass
class ModelChoice:
    model_id: str
    confidence: float          # 0.0–1.0; 1.0 when no policy active (default routing)
    policy_id: str | None
    tier_index: int
    reason: str                # "policy", "default", "cascade-escalation"
    latency_ms: int


@dataclass
class CascadeStep:
    tier_index: int
    model_id: str
    confidence: float
    confidence_mode: ConfidenceMode
    threshold: float
    escalated: bool
    response: str
    response_hash: str
    input_tokens: int
    output_tokens: int
    cost_usd: float
    latency_ms: int


@dataclass
class CascadeResult:
    final_response: str
    final_model_id: str
    steps: list[CascadeStep]
    total_cost_usd: float
    total_latency_ms: int
    tiers_invoked: int
    escalation_count: int


@dataclass
class ParetoPoint:
    confidence_threshold: float
    accuracy_estimate: float
    cost_per_task_usd: float
    model_distribution: dict[str, float]  # model_id -> fraction of queries
    pgr: float


@dataclass
class RoutingPolicy:
    id: str
    profile: str
    accuracy_target: float
    accuracy_estimate: float
    tiers: list[PolicyTier]
    pgr: float
    apgr: float
    cost_per_task_usd: float
    baseline_cost_usd: float
    eval_cases_used: int
    created_at: str

    def to_json(self) -> str:
        return json.dumps({
            "id": self.id,
            "tiers": [
                {
                    "model_id": t.model_id,
                    "confidence_threshold": t.confidence_threshold,
                    "estimated_fraction": t.estimated_fraction,
                }
                for t in self.tiers
            ],
        })


@dataclass
class PolicyTier:
    model_id: str
    confidence_threshold: float | None  # None = final fallback tier (always accept)
    estimated_fraction: float
    cost_per_1m_output_usd: float
```

### 9.4 `ModelRouter` Class

```python
class ModelRouter:
    """
    Pre-routing classifier: given a task text, selects the cheapest model tier
    predicted to answer correctly at or above the policy's accuracy target.

    Lazy-imports sentence_transformers and sklearn on first construction.
    """

    def __init__(self, policy: RoutingPolicy) -> None:
        # Lazy imports to avoid module-load overhead
        from sentence_transformers import SentenceTransformer  # type: ignore
        from sklearn.linear_model import LogisticRegression   # type: ignore
        self._policy = policy
        self._encoder = SentenceTransformer("all-MiniLM-L6-v2")
        self._clf: LogisticRegression | None = None  # set by load_classifier()

    def load_classifier(self, clf: object) -> None:
        self._clf = clf

    def route(self, query: str, task_type: str | None = None) -> ModelChoice:
        t0 = time.perf_counter()
        if self._clf is None:
            # No trained classifier; fall back to cheapest tier with confidence=1.0
            tier = self._policy.tiers[0]
            return ModelChoice(
                model_id=tier.model_id,
                confidence=1.0,
                policy_id=self._policy.id,
                tier_index=0,
                reason="policy-no-classifier",
                latency_ms=int((time.perf_counter() - t0) * 1000),
            )
        embedding = self._encoder.encode([query])  # shape (1, 384)
        # clf predicts P(correct | tier) for each tier; pick cheapest above threshold
        for i, tier in enumerate(self._policy.tiers):
            proba = self._clf.predict_proba(embedding)[0]
            confidence = float(proba[i])
            if tier.confidence_threshold is None or confidence >= tier.confidence_threshold:
                return ModelChoice(
                    model_id=tier.model_id,
                    confidence=confidence,
                    policy_id=self._policy.id,
                    tier_index=i,
                    reason="policy",
                    latency_ms=int((time.perf_counter() - t0) * 1000),
                )
        # Exhausted all tiers with thresholds; use final fallback
        last = self._policy.tiers[-1]
        return ModelChoice(
            model_id=last.model_id,
            confidence=0.0,
            policy_id=self._policy.id,
            tier_index=len(self._policy.tiers) - 1,
            reason="policy-fallback",
            latency_ms=int((time.perf_counter() - t0) * 1000),
        )
```

### 9.5 `FrugalCascade` Class

```python
class FrugalCascade:
    """
    Sequential confidence-gated cascade.
    Calls model tiers in order, stopping when confidence >= threshold.
    Each (model_fn, scorer_fn, threshold) tuple is a tier.
    """

    def __init__(
        self,
        tiers: list[tuple[Callable[[str], tuple[str, int, int, float]], ConfidenceScorer, float]],
    ) -> None:
        if len(tiers) > 5:
            raise ValueError("FrugalCascade: maximum 5 tiers supported")
        self._tiers = tiers

    def run(self, task: str, budget_guard: Callable[[float], bool] | None = None) -> CascadeResult:
        steps: list[CascadeStep] = []
        total_cost = 0.0
        total_latency = 0
        best_response = ""
        best_confidence = 0.0

        for i, (model_fn, scorer, threshold) in enumerate(self._tiers):
            t0 = time.perf_counter()
            response, input_tok, output_tok, cost = model_fn(task)
            latency_ms = int((time.perf_counter() - t0) * 1000)
            total_cost += cost
            total_latency += latency_ms

            if budget_guard is not None and not budget_guard(total_cost):
                # Over budget — return best so far
                break

            confidence = scorer.score(task, response)
            response_hash = hashlib.sha256(response.encode()).hexdigest()
            is_last = i == len(self._tiers) - 1
            escalated = (confidence < threshold) and not is_last

            step = CascadeStep(
                tier_index=i,
                model_id=model_fn.__name__,  # caller sets __name__ to model_id
                confidence=confidence,
                confidence_mode=scorer.mode,
                threshold=threshold,
                escalated=escalated,
                response=response,
                response_hash=response_hash,
                input_tokens=input_tok,
                output_tokens=output_tok,
                cost_usd=cost,
                latency_ms=latency_ms,
            )
            steps.append(step)

            if confidence >= confidence:
                best_response = response
                best_confidence = confidence

            if not escalated:
                best_response = response
                break

        escalation_count = sum(1 for s in steps if s.escalated)
        return CascadeResult(
            final_response=best_response,
            final_model_id=steps[-1].model_id if steps else "",
            steps=steps,
            total_cost_usd=total_cost,
            total_latency_ms=total_latency,
            tiers_invoked=len(steps),
            escalation_count=escalation_count,
        )
```

### 9.6 Confidence Scorers

```python
class ConfidenceScorer:
    mode: ConfidenceMode

    def score(self, prompt: str, response: str) -> float:
        raise NotImplementedError


class MajorityVoteScorer(ConfidenceScorer):
    """
    Sample N responses from the same model; return fraction agreeing with modal answer.
    Requires a callable that produces the N additional samples.
    """
    mode: ConfidenceMode = "majority"

    def __init__(self, sample_fn: Callable[[str, int], list[str]], n: int = 10) -> None:
        self._sample_fn = sample_fn
        self._n = n

    def score(self, prompt: str, response: str) -> float:
        samples = self._sample_fn(prompt, self._n)
        # Normalize: strip whitespace and lower-case for discrete answers
        normalized = [s.strip().lower() for s in samples + [response]]
        modal = max(set(normalized), key=normalized.count)
        return normalized.count(modal) / len(normalized)


class EmbeddingConsistencyScorer(ConfidenceScorer):
    """
    Embed the response and N=5 samples; return cosine similarity of response to centroid.
    No extra LLM call.
    """
    mode: ConfidenceMode = "embedding"

    def __init__(self, sample_fn: Callable[[str, int], list[str]], n: int = 5) -> None:
        from sentence_transformers import SentenceTransformer  # lazy import
        import numpy as np
        self._encoder = SentenceTransformer("all-MiniLM-L6-v2")
        self._sample_fn = sample_fn
        self._n = n
        self._np = np

    def score(self, prompt: str, response: str) -> float:
        np = self._np
        samples = self._sample_fn(prompt, self._n)
        all_texts = samples + [response]
        embeddings = self._encoder.encode(all_texts)  # shape (n+1, 384)
        centroid = embeddings[:-1].mean(axis=0)
        response_emb = embeddings[-1]
        # Cosine similarity
        sim = float(
            np.dot(centroid, response_emb)
            / (np.linalg.norm(centroid) * np.linalg.norm(response_emb) + 1e-9)
        )
        return max(0.0, min(1.0, sim))


class DistilBERTQualityScorer(ConfidenceScorer):
    """
    Regression score from a fine-tuned DistilBERT quality predictor.
    Fastest inference; no extra LLM calls; requires pre-trained model file.
    """
    mode: ConfidenceMode = "distilbert"
    _MODEL_PATH = "~/.tag/models/distilbert-quality-scorer"

    def __init__(self) -> None:
        import os
        from pathlib import Path
        model_path = Path(os.path.expanduser(self._MODEL_PATH))
        if not model_path.exists():
            raise FileNotFoundError(
                f"DistilBERT quality scorer not found at {model_path}. "
                "Run: tag route models download --scorer distilbert"
            )
        # Lazy pipeline load
        from transformers import pipeline  # type: ignore
        self._pipe = pipeline("text-classification", model=str(model_path))

    def score(self, prompt: str, response: str) -> float:
        text = f"[PROMPT] {prompt[:512]} [RESPONSE] {response[:512]}"
        result = self._pipe(text, truncation=True)[0]
        # Model outputs label GOOD/BAD with score; map to 0.0–1.0
        return float(result["score"]) if result["label"] == "GOOD" else 1.0 - float(result["score"])
```

### 9.7 `RoutingPolicyTrainer` Class

```python
class RoutingPolicyTrainer:
    """
    Trains a logistic regression classifier on historical eval results
    to predict per-model-tier success probability for new task embeddings.
    """

    def __init__(self, models: list[str], costs: dict[str, float]) -> None:
        """
        models: ordered list of model_id strings (cheapest to most expensive).
        costs: model_id -> cost_per_1m_output_tokens_usd
        """
        self._models = models
        self._costs = costs

    def fit(
        self,
        eval_results: list[dict],  # rows from eval_results JOIN eval_cases
        accuracy_target: float,
        min_cases: int = 20,
    ) -> RoutingPolicy:
        from sentence_transformers import SentenceTransformer
        from sklearn.linear_model import LogisticRegression
        import numpy as np

        # Validate minimum data
        per_model_counts = {}
        for r in eval_results:
            m = r["model_id"]
            per_model_counts[m] = per_model_counts.get(m, 0) + 1
        for m in self._models:
            if per_model_counts.get(m, 0) < min_cases:
                raise ValueError(
                    f"Insufficient eval data for {m}: "
                    f"{per_model_counts.get(m, 0)} cases (need {min_cases})"
                )

        encoder = SentenceTransformer("all-MiniLM-L6-v2")
        prompts = [r["input_text"] for r in eval_results]
        embeddings = encoder.encode(prompts)

        # Train one-vs-rest classifier: P(correct) per model
        # Label: 1 if model answered correctly, 0 otherwise
        clfs: dict[str, LogisticRegression] = {}
        for model_id in self._models:
            model_rows = [r for r in eval_results if r["model_id"] == model_id]
            X = np.array([embeddings[i] for i, r in enumerate(eval_results)
                          if r["model_id"] == model_id])
            y = np.array([1 if r["passed"] else 0 for r in model_rows])
            clf = LogisticRegression(max_iter=1000)
            clf.fit(X, y)
            clfs[model_id] = clf

        # Compute Pareto curve by sweeping threshold
        pareto = self.pareto_curve(embeddings, eval_results, clfs, accuracy_target)

        # Select policy point closest to accuracy_target with minimum cost
        viable = [p for p in pareto if p.accuracy_estimate >= accuracy_target]
        if not viable:
            viable = [max(pareto, key=lambda p: p.accuracy_estimate)]
        best = min(viable, key=lambda p: p.cost_per_task_usd)

        # Compute PGR and APGR
        weak_acc = min(p.accuracy_estimate for p in pareto)
        strong_acc = max(p.accuracy_estimate for p in pareto)
        pgr = ((best.accuracy_estimate - weak_acc) / (strong_acc - weak_acc + 1e-9))
        apgr = float(np.trapz(
            [p.pgr for p in pareto],
            [p.confidence_threshold for p in pareto],
        )) / (pareto[-1].confidence_threshold - pareto[0].confidence_threshold + 1e-9)

        # Build policy tiers
        tiers = []
        for i, model_id in enumerate(self._models):
            threshold = best.confidence_threshold if i < len(self._models) - 1 else None
            fraction = best.model_distribution.get(model_id, 0.0)
            tiers.append(PolicyTier(
                model_id=model_id,
                confidence_threshold=threshold,
                estimated_fraction=fraction,
                cost_per_1m_output_usd=self._costs.get(model_id, 0.0),
            ))

        import datetime
        policy_id = f"policy-{uuid.uuid4().hex[:8]}"
        return RoutingPolicy(
            id=policy_id,
            profile="",  # set by caller
            accuracy_target=accuracy_target,
            accuracy_estimate=best.accuracy_estimate,
            tiers=tiers,
            pgr=pgr,
            apgr=apgr,
            cost_per_task_usd=best.cost_per_task_usd,
            baseline_cost_usd=self._costs.get(self._models[-1], 0.0) * 1000,
            eval_cases_used=len(eval_results),
            created_at=datetime.datetime.utcnow().isoformat() + "Z",
        )

    def pareto_curve(
        self,
        embeddings,
        eval_results: list[dict],
        clfs: dict,
        accuracy_target: float,
    ) -> list[ParetoPoint]:
        import numpy as np
        points: list[ParetoPoint] = []
        for threshold_int in range(50, 100):
            threshold = threshold_int / 100.0
            # Simulate routing at this threshold
            assigned: dict[str, int] = {m: 0 for m in self._models}
            correct_count = 0
            total_cost = 0.0
            for i, r in enumerate(eval_results):
                emb = embeddings[i].reshape(1, -1)
                chosen_model = self._models[-1]  # default to strongest
                for model_id in self._models[:-1]:
                    prob = clfs[model_id].predict_proba(emb)[0][1]
                    if prob >= threshold:
                        chosen_model = model_id
                        break
                assigned[chosen_model] += 1
                # Evaluate: was chosen_model correct on this task?
                model_results = [x for x in eval_results
                                 if x.get("model_id") == chosen_model
                                 and x.get("input_text") == r.get("input_text")]
                if model_results and model_results[0]["passed"]:
                    correct_count += 1
                # Simplified cost: cost_per_1m * estimated_tokens / 1e6
                est_tokens = len(r.get("output_text", "")) // 4
                total_cost += self._costs.get(chosen_model, 0.0) * est_tokens / 1e6

            n = len(eval_results)
            accuracy = correct_count / n if n > 0 else 0.0
            distribution = {m: assigned[m] / n for m in self._models}
            weak_acc = 0.5  # heuristic baseline
            strong_acc = 0.97
            pgr = (accuracy - weak_acc) / (strong_acc - weak_acc + 1e-9)
            points.append(ParetoPoint(
                confidence_threshold=threshold,
                accuracy_estimate=accuracy,
                cost_per_task_usd=total_cost / n if n > 0 else 0.0,
                model_distribution=distribution,
                pgr=max(0.0, pgr),
            ))
        return points
```

### 9.8 Integration with `controller.py`

The routing module integrates into the existing `cmd_run` dispatch path in `controller.py` at the point where the Hermes call is constructed, before the subprocess is launched:

```python
# In controller.py, inside the run dispatch path (simplified):
from tag.routing import ModelRouter, load_active_policy

if profile_cfg.get("routing", {}).get("auto_route"):
    policy = load_active_policy(db, profile_name=profile)
    if policy is not None:
        router = ModelRouter(policy)
        # Load pre-trained classifier from routing_policies.policy_json
        router.load_classifier(deserialize_clf(policy))
        choice = router.route(query=prompt, task_type=task_type)
        route = apply_route_model_overrides(route, master_model=choice.model_id)
        _write_routing_decision(db, run_id=run_id, choice=choice, profile=profile)
        log.info(
            "[routing] pre-routed to %s (confidence=%.3f, policy=%s)",
            choice.model_id, choice.confidence, choice.policy_id,
        )
```

### 9.9 OTel Span Attributes

All routing spans use the following attribute schema (extending PRD-013 conventions):

| Attribute Key | Type | Description |
|---------------|------|-------------|
| `routing.policy_id` | string | Active policy ID or `none` |
| `routing.chosen_model` | string | Model ID selected by router |
| `routing.confidence` | float | Confidence score at decision time |
| `routing.tier_index` | int | Index of chosen tier (0=cheapest) |
| `routing.escalated` | bool | True if this was a cascade escalation |
| `routing.cascade_depth` | int | Total cascade tiers invoked |
| `routing.cost_usd` | float | Cost of this tier's LLM call |

### 9.10 Hyperparameters Exposed in Config

```yaml
# Global section of ~/.tag/config.yaml or profile YAML
routing:
  auto_route: false                       # master switch for pre-routing
  policy_id: null                         # override active policy; null = use active=1 row
  cascade:
    threshold: 0.85                       # default escalation threshold
    confidence_mode: auto                 # auto | majority | embedding | distilbert
    max_tiers: 3                          # hard cap on cascade depth
    distilbert_threshold: 0.65            # secondary threshold when distilbert mode
  classifier:
    model: all-MiniLM-L6-v2              # SentenceBERT model name
    n_consistency_samples: 5              # samples for embedding-mode scorer
    majority_n_samples: 10               # samples for majority-vote scorer
    majority_temperature: 0.7            # sampling temperature
  training:
    min_eval_cases: 20                   # minimum cases per tier to train
    pareto_threshold_step: 0.01          # sweep granularity for Pareto curve
    holdout_fraction: 0.2                # fraction reserved for calibration
  costs:                                  # model costs in USD per 1M output tokens
    anthropic/claude-haiku-4-5: 0.25
    anthropic/claude-sonnet-4-6: 3.00
    anthropic/claude-opus-4: 15.00
```

---

## 10. Security Considerations

1. **No raw task text in routing_decisions.** The `query_hash` column stores only `SHA-256(task_text)`. The original task text is never persisted in routing tables. This prevents routing logs from becoming a secondary secret exfiltration vector independent of the `runs` table, which stores the prompt and is already gated by PRD-034 secret scanning.

2. **Secret scanning before routing.** `security.py` (PRD-034) must scan the task text before it is passed to `ModelRouter.route()`. If a secret is detected, the task is refused at the security gate before any routing decision is made. This ensures that even the hash of a secret-containing task is not persisted to `routing_decisions`.

3. **No model serialization with pickle.** `RoutingPolicyTrainer` stores trained classifiers serialized with `sklearn`'s `joblib` in the `routing_policies.policy_json` field, base64-encoded. The deserialization in `ModelRouter.load_classifier` must validate the schema and only load classifiers that were written by TAG itself. Loading classifiers from arbitrary user-supplied paths is not supported. This prevents the pickle deserialization RCE vector (GHSA-mhr3-j7m5-c7c9) from applying to routing policies.

4. **Policy write requires confirmation.** `tag route optimize` writes a routing policy that will affect all subsequent runs for a profile. The command requires interactive confirmation (or `--yes` / `CI=true`) before writing. This prevents accidental policy overwrites from automated pipelines.

5. **Cascade escalation is audited, not silent.** Every cascade escalation from a cheaper model to a more capable one is logged at WARNING level and written to `cascade_steps`. Routing to a cheaper model (downgrade) is also logged. Silent capability changes are not permitted, consistent with PRD-031's substitution logging requirement.

6. **Budget guard before each cascade tier.** `budget.py` (PRD-012) is consulted before each cascade tier call. If invoking the next tier would exceed the active budget, the cascade aborts and returns the best response seen. This prevents a misconfigured cascade from incurring unbounded cost by escalating through all tiers on every query.

7. **Classifier training data is local-only.** `RoutingPolicyTrainer` reads only from the local SQLite database (`eval_results`, `eval_cases`). No data is transmitted to external services during training. The trained classifier is stored locally in SQLite. There is no opt-in telemetry or federated training path in this PRD.

8. **Confidence threshold tuning requires eval data, not production traffic.** The routing policy is trained on `eval_results` (manually curated suites), not on production task text. This prevents training a model that learns to route based on sensitive content patterns in production prompts.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`tests/test_routing.py`)

| Test | What It Verifies |
|------|-----------------|
| `test_model_router_returns_cheapest_above_threshold` | Given a mock classifier returning high confidence for Haiku, `ModelRouter.route()` returns Haiku, not Sonnet or Opus. |
| `test_model_router_falls_back_on_low_confidence` | Given a mock classifier returning confidence < threshold for all tiers, returns the final fallback tier. |
| `test_model_router_latency_under_10ms` | `ModelRouter.route()` with cached SentenceBERT model completes in < 10 ms p99 over 100 calls. |
| `test_frugal_cascade_stops_at_first_confident_tier` | Cascade with two tiers, first returns confidence 0.92 (threshold 0.85): only one tier invoked. |
| `test_frugal_cascade_escalates_on_low_confidence` | Cascade with two tiers, first returns confidence 0.61: both tiers invoked, escalation logged. |
| `test_frugal_cascade_budget_guard_aborts` | Budget guard returns False after tier 1: cascade aborts, returns tier 1 response. |
| `test_cascade_hard_cap_at_5_tiers` | Constructing `FrugalCascade` with 6 tiers raises `ValueError`. |
| `test_majority_vote_scorer_discrete` | 8/10 samples agree on "yes": score = 0.8. |
| `test_embedding_consistency_scorer_identical_samples` | All N samples identical: cosine similarity = 1.0. |
| `test_routing_decision_written_to_sqlite` | After `ModelRouter.route()` call, `routing_decisions` table has one row with correct `query_hash`. |
| `test_query_hash_is_sha256_not_raw_text` | `routing_decisions.query_hash` matches `sha256(task_text)`, not the raw task text. |
| `test_no_heavy_imports_at_module_load` | `import tag.routing` does not load `torch`, `sklearn`, or `sentence_transformers` into `sys.modules`. |
| `test_policy_trainer_requires_min_cases` | `RoutingPolicyTrainer.fit()` with fewer than `min_cases` rows raises `ValueError`. |
| `test_pareto_curve_monotone_accuracy_vs_cost` | Higher confidence threshold => higher accuracy estimate AND higher cost per task. |
| `test_cascade_step_logs_written` | After a 2-tier cascade, `cascade_steps` table has 2 rows with correct `decision_id` FK. |

### 11.2 Integration Tests (`tests/test_routing_integration.py`)

| Test | What It Verifies |
|------|-----------------|
| `test_optimize_reads_eval_results_and_writes_policy` | Seeds 30 `eval_results` rows for two model tiers; runs `cmd_route_optimize`; asserts `routing_policies` has one `active=1` row for the profile. |
| `test_auto_route_applied_on_cmd_run` | Profile has `routing.auto_route: true`; `cmd_run` is called; `routing_decisions` has one row with `policy_id` matching the active policy. |
| `test_cascade_cmd_produces_cascade_steps` | `cmd_route_cascade` is called with 3 tiers; `cascade_steps` table has ≥ 1 row. |
| `test_route_stats_aggregates_decisions` | Seeds 50 `routing_decisions` rows; `cmd_route_stats --json` output contains `total_routed_tasks: 50`. |
| `test_calibrate_holdout_accuracy_reported` | Seeds 100 eval results; `cmd_route_calibrate` output JSON contains `accuracy_estimate` field. |
| `test_policy_delete_disables_auto_route` | Active policy is deleted; subsequent `cmd_run` with `auto_route: true` uses profile default model. |

### 11.3 Performance Tests (`tests/test_routing_perf.py`)

| Test | What It Verifies |
|------|-----------------|
| `bench_router_route_p99_latency` | 200 calls to `ModelRouter.route()` with cached model: p99 < 10 ms. |
| `bench_trainer_fit_1000_cases` | `RoutingPolicyTrainer.fit()` with 1000 synthetic eval rows: wall time < 30 s. |
| `bench_embedding_scorer_5_samples` | `EmbeddingConsistencyScorer.score()` with 5 samples: < 500 ms. |
| `bench_cascade_2tier_overhead` | 2-tier cascade where tier 1 is always confident: total overhead vs. direct single call < 50 ms. |

---

## 12. Acceptance Criteria

| ID | Criterion | How to Test |
|----|-----------|-------------|
| AC-01 | `tag route optimize --profile coder --accuracy-target 0.90` with ≥ 20 eval cases per tier writes one `routing_policies` row with `active=1` and `accuracy_estimate >= 0.90`. | Integration test; inspect SQLite. |
| AC-02 | `tag route optimize --dry-run` does not write any rows to `routing_policies`. | Integration test; assert table is empty after call. |
| AC-03 | `tag route optimize --json` outputs valid JSON with `policy_id`, `accuracy_estimate`, `cost_per_task_usd`, `baseline_cost_usd`, `pgr`, and `tiers` array. | Parse output with `json.loads`; check schema. |
| AC-04 | `tag route cascade --task X --start-model haiku --fallback-to sonnet` where haiku confidence < threshold: both `haiku` and `sonnet` are called; `cascade_steps` has 2 rows; escalation is logged at WARNING. | Integration test with mock LLM; assert call count and log output. |
| AC-05 | `tag route cascade --task X --start-model haiku --fallback-to sonnet` where haiku confidence >= threshold: only `haiku` is called; `cascade_steps` has 1 row. | Integration test with mock LLM; assert call count = 1. |
| AC-06 | `tag route cascade` with 6 `--fallback-to` flags exits with code 1 and prints error about 5-tier limit. | CLI invocation test. |
| AC-07 | `tag route stats --json` returns correct `total_routed_tasks` count matching `routing_decisions` table row count for the profile. | Seed table; run command; parse JSON. |
| AC-08 | `routing_decisions.query_hash` equals `hashlib.sha256(task_text.encode()).hexdigest()` for every row; `routing_decisions` has no column containing raw task text. | Schema inspection; unit test. |
| AC-09 | `ModelRouter.route()` completes in < 10 ms p99 over 200 calls with loaded model. | Performance benchmark test. |
| AC-10 | `import tag.routing` does not add `torch`, `sklearn`, `sentence_transformers`, or `transformers` to `sys.modules`. | `sys.modules` assertion in unit test immediately after import. |
| AC-11 | With `routing.auto_route: true` in a profile with an active policy, `tag run --profile coder "add docstring"` dispatches to the model chosen by the router, not the profile's default model, and writes to `routing_decisions`. | Integration test with mock Hermes; inspect route sent to subprocess. |
| AC-12 | `tag route calibrate --profile coder` outputs `accuracy_estimate`, `pgr`, and `ece` fields in JSON. | Integration test; seed eval data; parse output. |
| AC-13 | `tag route policy delete <policy-id>` sets `active=0` and `deleted_at` is non-null in the database. | Integration test; inspect SQLite after command. |
| AC-14 | Every cascade tier call produces a distinct child span in the `spans` table with `routing.tier` attribute. | Integration test; query spans table after cascade run. |
| AC-15 | When budget guard fires mid-cascade, cascade returns the highest-confidence response from completed tiers; `routing_decisions.cost_usd` is below the budget limit. | Integration test with mock budget guard returning False after tier 1. |
| AC-16 | `tag route optimize` with fewer than `min_eval_cases` (default: 20) eval cases per tier exits non-zero and prints the count of available cases and the minimum required. | CLI invocation test with sparse eval data. |

---

## 13. Dependencies

| Dependency | Type | Version / Notes | Required By |
|------------|------|-----------------|-------------|
| `sentence-transformers` | Python package | `>=2.2.0`; `all-MiniLM-L6-v2` model (22 MB, auto-downloaded to `~/.tag/models/`) | `ModelRouter`, `EmbeddingConsistencyScorer`, `RoutingPolicyTrainer` |
| `scikit-learn` | Python package | `>=1.3.0`; `LogisticRegression` | `RoutingPolicyTrainer` |
| `numpy` | Python package | `>=1.24.0`; already a transitive dep of sentence-transformers | `RoutingPolicyTrainer`, `EmbeddingConsistencyScorer` |
| `transformers` | Python package | `>=4.35.0`; only required for `DistilBERTQualityScorer` (optional scorer) | `DistilBERTQualityScorer` |
| PRD-027 (eval framework) | Internal module | `eval_results`, `eval_cases` SQLite tables used as training data | `RoutingPolicyTrainer`, `tag route optimize` |
| PRD-012 (budget enforcement) | Internal module | `budget.py` — consulted before each cascade tier | `FrugalCascade` budget guard |
| PRD-013 (agent tracing) | Internal module | `tracing.py` — child spans for each routing decision and cascade tier | Span emission |
| PRD-034 (secret scanning) | Internal module | `security.py` — scans task text before routing; must be called by dispatcher | `ModelRouter.route()` caller |
| PRD-031 (model fallback chains) | Internal module | Co-existing routing path; must not conflict on `route` table or `cmd_route` dispatch | Architecture boundary |
| PRD-043 (vector tool retrieval) | Internal module | `tool_retrieval.py` — `SentenceTransformer` already cached; share model instance | `ModelRouter`, `EmbeddingConsistencyScorer` |
| PRD-041 (OTel span cost attribution) | Internal module | `otel_semconv.py` — routing span attributes follow GenAI semconv conventions | Span attributes |
| `joblib` | Python package | `>=1.3.0`; already a transitive dep of scikit-learn; used for classifier serialization | `RoutingPolicyTrainer` |

---

## 14. Open Questions

| # | Question | Owner | Resolution Target |
|---|----------|-------|-------------------|
| OQ-1 | Should `ModelRouter` share the `SentenceTransformer` model instance with `tool_retrieval.py` (PRD-043) to avoid loading two copies of `all-MiniLM-L6-v2` into memory? If so, what is the module-level singleton pattern that avoids circular imports? | Routing + Tool Retrieval owners | Before implementation start |
| OQ-2 | `DistilBERTQualityScorer` requires a fine-tuned model file at `~/.tag/models/distilbert-quality-scorer`. Does TAG ship this model, host it for download, or require users to supply it? If hosted, what is the distribution mechanism? | Platform team | Before beta release |
| OQ-3 | The Pareto curve sweep is O(N_thresholds × N_eval_cases × N_models). At N=100 cases, 3 models, 50 threshold points, this is 15,000 operations — fast. At N=10,000 cases it becomes 1.5M. Is there a training data size cap, or should the trainer sample from eval_results when |eval_results| > some limit? | Engineering | Before implementation |
| OQ-4 | PRD-031 fallback chains and PRD-107 cascade are architecturally separate, but both can override the model used for a run. If a cascade escalation produces an error that would normally trigger a PRD-031 fallback hop, which takes precedence? Proposed: PRD-031 fires after PRD-107 cascade exhausts its tiers. | Architecture review | Before implementation |
| OQ-5 | Should `tag route optimize` also consume `steps` table data (actual run outputs) in addition to `eval_results`? Steps are not labeled with pass/fail, but their duration and token count are available as proxy signals. | Product | Phase 2 planning |
| OQ-6 | The `MajorityVoteScorer` requires N extra LLM calls to score confidence. At N=10, this multiplies the cost of the cheapest tier by 10× before deciding whether to escalate — potentially more expensive than just calling Opus directly. Should the default N be 3 or 5 for the scorer path (distinct from PRD-101's sampling for output quality)? | Engineering | Before implementation |
| OQ-7 | `cascade_steps.response_hash` stores a SHA-256 hash of each tier's response. Is this sufficient for debugging, or should the full response be stored? Storing full responses could expose PII. Proposed: store hash only; full responses are available in `steps` via `run_id` FK. | Security + Engineering | Before implementation |
| OQ-8 | PGR and APGR metrics require a defined "weak baseline" and "strong baseline" accuracy. Currently these are hardcoded heuristics (0.50, 0.97). Should they be computed dynamically from the eval results (worst-model accuracy, best-model accuracy)? | Engineering | Before implementation |

---

## 15. Complexity and Timeline

### Phase 1: Core Infrastructure (Days 1–5)

- Define `routing_policies`, `routing_decisions`, `cascade_steps` SQLite tables in `open_db()` schema migration.
- Implement `RoutingPolicy`, `PolicyTier`, `ModelChoice`, `CascadeStep`, `CascadeResult`, `ParetoPoint` dataclasses.
- Implement `ModelRouter` with lazy SentenceBERT import and `route()` method (returning `ModelChoice`).
- Implement `routing_decisions` write helper and wire into the `auto_route` dispatch path in `controller.py`.
- Unit tests: `test_model_router_*` and `test_routing_decision_written_to_sqlite`.

### Phase 2: Cascade & Scorers (Days 6–10)

- Implement `FrugalCascade` class with `run()` method, budget guard integration, and 5-tier hard cap.
- Implement `MajorityVoteScorer`, `EmbeddingConsistencyScorer`, and `DistilBERTQualityScorer`.
- Implement `cmd_route_cascade` CLI handler and parser registration in `controller.py`.
- OTel span attribution for each cascade tier (PRD-013 integration).
- Unit tests: `test_frugal_cascade_*`, `test_*_scorer_*`.

### Phase 3: Policy Training & Optimize (Days 11–16)

- Implement `RoutingPolicyTrainer` with `fit()` and `pareto_curve()` methods.
- Implement `cmd_route_optimize` CLI handler: read eval_results, train, display Pareto point, confirm, write policy.
- Implement `cmd_route_calibrate` CLI handler: holdout split, refit, evaluate, report ECE and PGR.
- Implement `cmd_route_policy_*` (list, show, delete, activate) CLI handlers.
- Unit tests: `test_policy_trainer_*`, `test_pareto_curve_*`.

### Phase 4: Stats, Integration, and Config (Days 17–21)

- Implement `cmd_route_stats` CLI handler with aggregation over `routing_decisions`.
- Wire all config keys under `routing:` into the config schema and validation.
- Integration tests: `test_optimize_reads_eval_results_and_writes_policy`, `test_auto_route_applied_on_cmd_run`, `test_cascade_cmd_produces_cascade_steps`, etc.
- Performance benchmarks: `bench_router_route_p99_latency`, `bench_trainer_fit_1000_cases`.
- Documentation: update `tag route --help` and add `routing` config key docs.

### Phase 5: Hardening and Edge Cases (Days 22–26)

- Budget guard integration with `budget.py` (PRD-012): pre-cascade and per-tier checks.
- Secret scanning integration: ensure `security.py` is called before `ModelRouter.route()` in all dispatch paths.
- Soft delete for `routing_policies` (`deleted_at` pattern, `active = 0`).
- Shared SentenceBERT singleton with `tool_retrieval.py` (OQ-1 resolution).
- Coverage audit: ensure ≥ 85% line coverage across `routing.py`.
- Review against acceptance criteria AC-01 through AC-16; close all open questions with owners.

**Total estimate: 26 working days (~5.5 weeks).** Effort estimate is L (2–4 weeks for implementation core, extended to 5.5 weeks including hardening and integration). Two engineers working in parallel on Phases 2 and 3 could compress the schedule to ~18 days.
