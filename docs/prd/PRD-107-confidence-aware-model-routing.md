# PRD-107: Confidence-Aware Model Routing with Cost/Accuracy Pareto Optimization (`tag route optimize`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** L (2-4 weeks)
**Category:** Advanced Reasoning & Planning
**Affects:** `internal/llm` (routing + provider interface), `internal/obs` (cost/token attribution)
**Depends on:** PRD-027 (eval framework — historical accuracy signal), PRD-028 (sandbox — isolated cascade execution), PRD-013 (agent tracing — per-decision span attribution), PRD-034 (secret scanning — prompt content before routing), PRD-012 (budget enforcement — per-tier cost guard), PRD-031 (model fallback chains — cascade overlap concerns), PRD-043 (vector tool retrieval — task-type embedding), PRD-041 (OTel span cost attribution — routing decision spans), PRD-045 (LLM-as-judge — confidence scoring), PRD-101 (self-consistency ensemble — confidence signal source)
**Inspired by:** FrugalGPT, LLM routing (Martian, RouteLLM), Pareto-optimal cascades

---

## 1. Overview

Every TAG task dispatches to a fixed model determined by profile configuration. The `coder` profile always uses `claude-opus-4`; the `researcher` profile always uses `claude-sonnet-4-6`. This hardwired assignment is safe but economically wasteful: empirical evidence from FrugalGPT (Chen et al. 2023) and RouteLLM (Ong et al. 2024) demonstrates that 60–80% of queries in typical workloads can be resolved correctly by a smaller, cheaper model, and only the genuinely difficult tail requires the most capable (and expensive) model tier. TAG currently has no mechanism to make this distinction — every task pays the full premium regardless of whether it needed it.

The Confidence-Aware Model Routing feature adds routing logic to `internal/llm` (with cost attribution in `internal/obs`), implementing two complementary routing strategies: **pre-routing** (decide which model tier to use *before* any LLM call, in under 10 ms, using a lightweight embedding classifier trained on historical eval results) and **cascading** (call the cheapest tier first, score the response confidence, escalate to the next tier only when confidence falls below a threshold). These two strategies map directly to RouteLLM's binary pre-routing approach and FrugalGPT's sequential confidence-gated cascade respectively. TAG implements both, selectable per-task via CLI flags.

The routing system is trained continuously on TAG's own historical data. Every eval result stored by the eval framework (PRD-027) records which model answered which task type correctly. The router trains on this ground truth: a logistic-regression classifier over task-prompt embeddings (from the `internal/memory` `Embedder`) learns to predict, given a new task's embedding, the probability that each model tier will answer correctly. This produces a calibrated confidence score per tier per query. The `tag route optimize` command reads historical eval results, fits the classifier, and emits a Pareto-optimal routing policy: for each accuracy target (e.g., 0.90), it computes the model mix that minimizes expected cost while hitting that target. The result is stored as a policy in the `internal/store` SQLite database and applied automatically to subsequent runs.

Cascading (`tag route cascade`) is the reliability-first complement: call `haiku` → if confidence < threshold, call `sonnet` → if confidence < threshold, call `opus`. Unlike PRD-031's fallback chains (which trigger on *error conditions* like 429 or context overflow), cascading triggers on *quality conditions* — low confidence in the answer. Each cascade tier scores its own response using one of three confidence signals: (a) self-reported logit-based confidence markers parsed from structured output — where usable, per-token logprobs come from the provider API (OpenAI exposes them; Anthropic does not expose them uniformly, so this signal degrades to structured self-report), (b) embedding-space consistency with self-consistency samples from PRD-101, or (c) a lightweight quality-regression scorer (pure-Go, build-tag backend). The cascade exits as soon as a tier's response clears the threshold, paying only for the tiers actually invoked.

The `tag route stats` command surfaces the operational picture: routing policy in effect, per-tier invocation rates, cost saved vs. always-using-the-most-expensive-model, and per-task-type accuracy by tier. Together, `optimize`, `cascade`, and `stats` close the loop: engineers set an accuracy target, the optimizer finds the cheapest policy that hits it, the cascade enforces it at runtime, and stats proves it delivered.

---

## 2. Problem Statement

### 2.1 Uniform High-Cost Model Assignment Wastes Budget on Simple Tasks

A `tag run --profile coder "add a docstring to this function"` dispatches to `claude-opus-4` at roughly $15 per million output tokens. The same task would be answered equivalently by `claude-haiku-4-5` at $0.25 per million output tokens — a 60× cost difference. TAG has no mechanism to detect that this particular task falls into the "easy" category where a smaller model suffices. Over a typical engineering team's usage (hundreds of tasks per day), the accumulated overspend is substantial: empirically, FrugalGPT achieves 4× cost reduction at equal accuracy on MT-Bench by routing 70% of queries to cheaper models. TAG leaves this saving entirely on the table.

### 2.2 No Feedback Loop Between Eval Results and Routing Decisions

The eval framework (PRD-027) records, for every eval suite run, whether each model succeeded on each task type. This dataset is a natural training signal for a router: if `claude-haiku-4-5` achieves 0.93 accuracy on `add-docstring` tasks but only 0.61 accuracy on `security-audit` tasks, the router should route `add-docstring` to Haiku and `security-audit` to Opus. But today this signal is never consumed by the routing layer. Eval results accumulate in `eval_results` and go nowhere. The routing decision for every task remains hardcoded in the profile YAML, unchanged by any empirical evidence.

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
| G8 | `tag route optimize` integrates with the `internal/obs` budget gate (PRD-012): it computes projected monthly cost under the recommended policy and displays it before writing the policy. |
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
| FR-01 | `internal/llm` exports a `ModelRouter` with a `Route(ctx, query, taskType string) -> ModelChoice` method that returns in under 10 ms using a pre-loaded embedding classifier (`internal/memory` `Embedder` + in-memory logistic-regression weights). | P0 |
| FR-02 | `ModelRouter.Route()` returns `ModelChoice{ModelID, Confidence, PolicyID, Reason}`. If no policy is active for the profile, it returns the profile's default model. | P0 |
| FR-03 | Every call to `ModelRouter.Route()` writes a row to `routing_decisions` including `query_hash`, `task_type`, `policy_id`, `chosen_model_id`, `confidence_score`, `tier_index`, `latency_ms`, and `created_at`, through the single `internal/store` writer. | P0 |
| FR-04 | The `FrugalCascade` struct accepts a slice of tiers `{model func, scorer, threshold}`. It calls each tier in order, stopping when `scorer.Score(...) >= threshold` or all tiers exhausted. | P0 |
| FR-05 | Three scorer implementations are provided: `MajorityVoteScorer` (issues N=`classifier.majority_n_samples` concurrent `Stream` calls via `errgroup`, returns the fraction agreeing with the modal answer), `EmbeddingConsistencyScorer` (uses the `Embedder` + an in-Go cosine loop, returns cosine similarity of the response to the centroid of N=5 samples), `QualityRegressionScorer` (a pure-Go text classifier via the build-tag `hugot`/`cybertron` backend, returns a regression score). | P0 |
| FR-06 | Auto-selection of scorer: if the response is valid JSON or a single-token discrete value, use `MajorityVoteScorer`; otherwise use `EmbeddingConsistencyScorer`. `QualityRegressionScorer` is only used when explicitly set via `--confidence-mode distilbert` or config. | P1 |
| FR-07 | `RoutingPolicyTrainer.Fit(evalResults []EvalResult) -> RoutingPolicy` trains a logistic-regression classifier (hand-rolled gradient descent over `gonum` vectors) on `Embedder` embeddings of task prompts, with labels = `(model_id, correct bool)`. Requires a minimum of `min_eval_cases` (default: 20) samples per tier. | P0 |
| FR-08 | `RoutingPolicyTrainer.ParetoCurve(models, evalResults) -> []ParetoPoint` computes the cost/accuracy Pareto frontier by sweeping the confidence threshold in 0.01 increments from 0.50 to 0.99, evaluating predicted accuracy and expected cost at each point. | P1 |
| FR-09 | `tag route optimize` reads from `eval_results` and `eval_cases` (PRD-027 schema). If fewer than `min_eval_cases` rows exist for the target profile, it prints an error and exits non-zero. | P0 |
| FR-10 | The trained policy is serialized to JSON and stored in `routing_policies` with fields: `id`, `profile`, `accuracy_target`, `accuracy_estimate`, `policy_json`, `pgr`, `cost_per_task_usd`, `baseline_cost_usd`, `eval_cases_used`, `created_at`, `active`. The classifier weights are stored as plain JSON float arrays inside `policy_json` (no binary/pickle blob). Only one policy per profile may be `active = 1`. | P0 |
| FR-11 | `tag route optimize --dry-run` computes and displays the policy but does not write to SQLite. Exit code 0. | P1 |
| FR-12 | Cascade escalations are reported as WARNING-level log lines (matching PRD-031's substitution logging convention): `[routing] escalated from {from_model} to {to_model} (confidence={score:.3f} < threshold={threshold:.3f})`. | P1 |
| FR-13 | Each tier in a cascade run emits a child span with attributes `routing.tier=<int>`, `routing.confidence=<float>`, `routing.escalated=<bool>`, `routing.model_id=<str>`, under the parent run span (PRD-013). | P1 |
| FR-14 | `tag route stats` reads from `routing_decisions` table, aggregates by `(profile, model_id)`, computes invocation counts, escalation rates, total cost, and PGR/APGR metrics, and outputs the result in text or JSON. | P1 |
| FR-15 | `tag route calibrate` splits `eval_results` for the profile into train/test (80/20 default), refits the classifier on the train split, evaluates on the test split, and reports: test accuracy by tier, calibration error (ECE), PGR, and APGR. | P1 |
| FR-16 | When `routing.auto_route: true` is set in a profile and a matching active policy exists, the `internal/agent` / `internal/runtime` run-dispatch path calls `ModelRouter.Route()` before constructing the provider `Request`, and substitutes the returned `model_id` into the route. | P0 |
| FR-17 | `tag route policy delete <policy-id>` sets `active = 0` and `deleted_at = now()` on the policy row (soft delete). If the deleted policy was the active policy for its profile, `auto_route` is effectively disabled until a new `tag route optimize` is run. | P1 |
| FR-18 | The cascade hard-caps at 5 tiers regardless of `--fallback-to` count. If more than 5 `--fallback-to` flags are provided, the command errors with a clear message. | P0 |
| FR-19 | `internal/security` (PRD-034) secret-scans the task text before routing. Routing decisions are never written with raw task text; only a `crypto/sha256` hash of the task is stored in `query_hash`. | P0 |
| FR-20 | The `internal/obs` budget gate (PRD-012) is consulted before each cascade tier: if the cumulative cascade cost would exceed the active budget, the cascade aborts and returns the highest-confidence response seen so far. | P1 |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Pre-routing classifier inference (embedding + logistic-regression predict) must complete in under 10 ms p99 on a MacBook M-series CPU with the embedder loaded in memory. (With a provider embedding API this excludes the network embed; the offline `cybertron` build-tag embedder meets it end-to-end.) | < 10 ms p99 |
| NFR-02 | Classifier training (`RoutingPolicyTrainer.Fit`) must complete in under 30 seconds on 1000 eval cases on a MacBook M-series CPU. | < 30 s |
| NFR-03 | Because everything compiles into the single binary, there is no import-time cost: the heavy `Embedder` and any build-tag model backend are initialized lazily inside `NewModelRouter`, not at package init; `routing`-adjacent packages must not force embedder init on import. | No init-time overhead |
| NFR-04 | When `routing.auto_route: false` (the default), the routing path adds zero measurable overhead to `tag run` (guarded by an early return before any router construction). | p50 overhead < 1 ms |
| NFR-05 | `routing_decisions` writes go through the single `internal/store` writer (modernc.org/sqlite, WAL, `_busy_timeout=5000`). No routing decision write may block `tag run` for more than 5 seconds. | < 5 s write block |
| NFR-06 | The default embedding model is `all-MiniLM-L6-v2` (22 MB) via the `Embedder` (provider API, or offline `cybertron` build tag). The optional quality-regression scorer uses a small pure-Go text classifier (`hugot`/`cybertron`, ~67 MB). Local model files are cached under `~/.tag/models/` on first use. | First-use download; cached thereafter |
| NFR-07 | All routing decisions written to SQLite are attributed with `run_id` where available, enabling a foreign-key join to the `runs` table. | 100% attribution |
| NFR-08 | The routing packages have test coverage ≥ 85% as measured by `go test -cover`. | ≥ 85% coverage |
| NFR-09 | `FrugalCascade` must be safe for concurrent cascade invocations (e.g., from `internal/queue` workers). Use per-invocation state passed by value/receiver; no shared mutable package-level state. | Goroutine-safe |
| NFR-10 | All user-facing cost figures are displayed to 4 decimal places in USD and prefixed with `$`. | Consistent formatting |

---

## 9. Technical Design

### 9.1 New Package: `internal/llm/routing`

Routing and cascade logic live in `internal/llm` (`routing.go`, `cascade.go`, `scorer.go`, `trainer.go`), with cost/token attribution delegated to `internal/obs`. The router depends on `internal/memory` (`Embedder`), `internal/store` (persistence), `internal/security` (pre-route secret scan), and `internal/obs` (pricing table + spans). It does not depend on `internal/agent` or `internal/cli`; those depend on it — the same acyclic direction as the Python `controller.py` → `routing.py` boundary.

### 9.2 SQLite DDL

Added as numbered migrations in `internal/store/migrate/` (applied by the single-writer migrator):

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

### 9.3 Core Structs

Python dataclasses map to Go structs; `Optional[float]` fields become `*float64` (nil = "final fallback tier, always accept"); JSON serialization uses `encoding/json` struct tags.

```go
// internal/llm/routing.go
package llm

type ConfidenceMode string

const (
	ModeMajority   ConfidenceMode = "majority"
	ModeEmbedding  ConfidenceMode = "embedding"
	ModeDistilBERT ConfidenceMode = "distilbert" // pure-Go quality-regression backend
	ModeAuto       ConfidenceMode = "auto"
)

type ModelChoice struct {
	ModelID    string
	Confidence float64 // 0.0–1.0; 1.0 when no policy active (default routing)
	PolicyID   string  // "" when routed by default
	TierIndex  int
	Reason     string        // "policy","default","cascade-escalation"
	Latency    time.Duration
}

type CascadeStep struct {
	TierIndex      int
	ModelID        string
	Confidence     float64
	ConfidenceMode ConfidenceMode
	Threshold      float64
	Escalated      bool
	Response       string
	ResponseHash   string
	InputTokens    int
	OutputTokens   int
	CostUSD        float64
	Latency        time.Duration
}

type CascadeResult struct {
	FinalResponse   string
	FinalModelID    string
	Steps           []CascadeStep
	TotalCostUSD    float64
	TotalLatency    time.Duration
	TiersInvoked    int
	EscalationCount int
}

type ParetoPoint struct {
	ConfidenceThreshold float64
	AccuracyEstimate    float64
	CostPerTaskUSD      float64
	ModelDistribution   map[string]float64 // model_id -> fraction of queries
	PGR                 float64
}

type PolicyTier struct {
	ModelID             string   `json:"model_id"`
	ConfidenceThreshold *float64 `json:"confidence_threshold"` // nil = final fallback
	EstimatedFraction   float64  `json:"estimated_fraction"`
	CostPer1MOutputUSD  float64  `json:"-"`
}

type RoutingPolicy struct {
	ID               string       `json:"id"`
	Profile          string       `json:"-"`
	AccuracyTarget   float64      `json:"-"`
	AccuracyEstimate float64      `json:"-"`
	Tiers            []PolicyTier `json:"tiers"`
	PGR              float64      `json:"-"`
	APGR             float64      `json:"-"`
	CostPerTaskUSD   float64      `json:"-"`
	BaselineCostUSD  float64      `json:"-"`
	EvalCasesUsed    int          `json:"-"`
	CreatedAt        string       `json:"-"`
	// ClassifierWeights holds the fitted logistic-regression weights per tier,
	// serialized as plain JSON float arrays (no pickle/gob).
	ClassifierWeights map[string][]float64 `json:"classifier_weights"`
}

func (p *RoutingPolicy) MarshalJSON() ([]byte, error) { /* json.Marshal of id/tiers/weights */ }
```

### 9.4 `ModelRouter`

Pre-routing classifier: given a task text, selects the cheapest model tier predicted to answer correctly at or above the policy's accuracy target. The `sklearn` classifier becomes a small in-process `logreg` (weights loaded from the policy JSON); the SentenceBERT encoder becomes the injected `Embedder`.

```go
// logreg holds fitted per-tier weights; PredictProba is a plain sigmoid(w·x+b).
type logreg struct {
	weights map[string][]float64 // model_id -> [w0..wN, bias]
}

func (c *logreg) predict(modelID string, emb []float32) float64 {
	w := c.weights[modelID]
	z := w[len(w)-1] // bias
	for i, x := range emb {
		z += w[i] * float64(x)
	}
	return 1.0 / (1.0 + math.Exp(-z))
}

type ModelRouter struct {
	policy   RoutingPolicy
	embedder memory.Embedder
	clf      *logreg // nil until loadClassifier(); rebuilt from policy JSON
}

func NewModelRouter(policy RoutingPolicy, embedder memory.Embedder) *ModelRouter {
	r := &ModelRouter{policy: policy, embedder: embedder}
	if len(policy.ClassifierWeights) > 0 {
		r.clf = &logreg{weights: policy.ClassifierWeights}
	}
	return r
}

func (r *ModelRouter) Route(ctx context.Context, query, taskType string) (ModelChoice, error) {
	t0 := time.Now()
	if r.clf == nil {
		// No trained classifier; fall back to cheapest tier with confidence=1.0.
		t := r.policy.Tiers[0]
		return ModelChoice{ModelID: t.ModelID, Confidence: 1.0, PolicyID: r.policy.ID,
			TierIndex: 0, Reason: "policy-no-classifier", Latency: time.Since(t0)}, nil
	}
	emb, err := r.embedder.Embed(ctx, query) // 384-dim MiniLM
	if err != nil {
		return ModelChoice{}, err
	}
	for i, tier := range r.policy.Tiers {
		conf := r.clf.predict(tier.ModelID, emb) // P(correct | tier)
		if tier.ConfidenceThreshold == nil || conf >= *tier.ConfidenceThreshold {
			return ModelChoice{ModelID: tier.ModelID, Confidence: conf, PolicyID: r.policy.ID,
				TierIndex: i, Reason: "policy", Latency: time.Since(t0)}, nil
		}
	}
	last := r.policy.Tiers[len(r.policy.Tiers)-1]
	return ModelChoice{ModelID: last.ModelID, Confidence: 0, PolicyID: r.policy.ID,
		TierIndex: len(r.policy.Tiers) - 1, Reason: "policy-fallback", Latency: time.Since(t0)}, nil
}
```

### 9.5 `FrugalCascade`

Sequential confidence-gated cascade. Calls model tiers in order, stopping when `confidence >= threshold`. Each tier is a `{Model, Scorer, Threshold}` triple. `Model` runs one turn through the `internal/llm` provider `Stream(ctx, Request)`; token counts feed `internal/obs` cost attribution. The cascade holds no shared mutable state (NFR-09) and honours `ctx` cancellation.

```go
type ModelFunc func(ctx context.Context, task string) (resp string, inTok, outTok int, cost float64, err error)

type CascadeTier struct {
	ModelID   string
	Model     ModelFunc
	Scorer    ConfidenceScorer
	Threshold float64
}

type FrugalCascade struct{ tiers []CascadeTier }

func NewFrugalCascade(tiers []CascadeTier) (*FrugalCascade, error) {
	if len(tiers) > 5 {
		return nil, fmt.Errorf("FrugalCascade: maximum 5 tiers supported, got %d", len(tiers))
	}
	return &FrugalCascade{tiers: tiers}, nil
}

// budgetGuard reports whether the running total is still within budget.
func (c *FrugalCascade) Run(ctx context.Context, task string, budgetGuard func(totalCost float64) bool) (CascadeResult, error) {
	var (
		steps    []CascadeStep
		total    float64
		totalLat time.Duration
		best     string
		bestConf float64
	)
	for i, tier := range c.tiers {
		t0 := time.Now()
		resp, inTok, outTok, cost, err := tier.Model(ctx, task)
		if err != nil {
			return CascadeResult{}, err
		}
		lat := time.Since(t0)
		total += cost
		totalLat += lat

		if budgetGuard != nil && !budgetGuard(total) {
			break // over budget — return best so far
		}

		conf := tier.Scorer.Score(ctx, task, resp)
		isLast := i == len(c.tiers)-1
		escalated := conf < tier.Threshold && !isLast

		steps = append(steps, CascadeStep{
			TierIndex: i, ModelID: tier.ModelID, Confidence: conf,
			ConfidenceMode: tier.Scorer.Mode(), Threshold: tier.Threshold, Escalated: escalated,
			Response: resp, ResponseHash: sha256Hex(resp),
			InputTokens: inTok, OutputTokens: outTok, CostUSD: cost, Latency: lat,
		})

		if conf >= bestConf { // track best-so-far for budget/abort paths
			best, bestConf = resp, conf
		}
		if !escalated {
			best = resp
			break
		}
	}

	escalations := 0
	finalModel := ""
	for _, s := range steps {
		if s.Escalated {
			escalations++
		}
	}
	if len(steps) > 0 {
		finalModel = steps[len(steps)-1].ModelID
	}
	return CascadeResult{
		FinalResponse: best, FinalModelID: finalModel, Steps: steps,
		TotalCostUSD: total, TotalLatency: totalLat,
		TiersInvoked: len(steps), EscalationCount: escalations,
	}, nil
}
```

### 9.6 Confidence Scorers

`ConfidenceScorer` is an interface. `SampleFunc` produces N additional samples through the `internal/llm` provider (concurrent via `errgroup`).

```go
type SampleFunc func(ctx context.Context, prompt string, n int) []string

type ConfidenceScorer interface {
	Mode() ConfidenceMode
	Score(ctx context.Context, prompt, response string) float64
}

// MajorityVoteScorer: sample N responses; return the fraction agreeing with the modal answer.
type MajorityVoteScorer struct {
	sample SampleFunc
	n      int // default 10
}

func (s MajorityVoteScorer) Mode() ConfidenceMode { return ModeMajority }
func (s MajorityVoteScorer) Score(ctx context.Context, prompt, response string) float64 {
	all := append(s.sample(ctx, prompt, s.n), response)
	counts := map[string]int{}
	for _, x := range all {
		counts[strings.ToLower(strings.TrimSpace(x))]++
	}
	best := 0
	for _, c := range counts {
		if c > best {
			best = c
		}
	}
	return float64(best) / float64(len(all))
}

// EmbeddingConsistencyScorer: cosine similarity of the response to the centroid of N=5 samples.
// No extra LLM call beyond the samples; embeds via the injected Embedder.
type EmbeddingConsistencyScorer struct {
	embedder memory.Embedder
	sample   SampleFunc
	n        int // default 5
}

func (s EmbeddingConsistencyScorer) Mode() ConfidenceMode { return ModeEmbedding }
func (s EmbeddingConsistencyScorer) Score(ctx context.Context, prompt, response string) float64 {
	samples := s.sample(ctx, prompt, s.n)
	dim := 0
	embs := make([][]float32, 0, len(samples))
	for _, t := range samples {
		e, err := s.embedder.Embed(ctx, t)
		if err == nil {
			embs = append(embs, e)
			dim = len(e)
		}
	}
	respEmb, err := s.embedder.Embed(ctx, response)
	if err != nil || len(embs) == 0 {
		return 0
	}
	centroid := make([]float32, dim)
	for _, e := range embs {
		for i := range e {
			centroid[i] += e[i] / float32(len(embs))
		}
	}
	sim := cosine(centroid, respEmb) // shared in-Go cosine
	if sim < 0 {
		return 0
	}
	if sim > 1 {
		return 1
	}
	return sim
}

// QualityRegressionScorer: a pure-Go text-classification model (build-tag hugot/cybertron
// backend) fine-tuned on quality labels. Replaces the Python HF-transformers DistilBERT
// pipeline; no CGO, no Python runtime. Fastest tier — no extra LLM calls.
type QualityRegressionScorer struct {
	clf textClassifier // loaded from ~/.tag/models/quality-scorer; nil-guarded at construction
}

func (s QualityRegressionScorer) Mode() ConfidenceMode { return ModeDistilBERT }
func (s QualityRegressionScorer) Score(ctx context.Context, prompt, response string) float64 {
	text := "[PROMPT] " + trunc(prompt, 512) + " [RESPONSE] " + trunc(response, 512)
	label, score := s.clf.Classify(ctx, text) // "GOOD"/"BAD" + confidence
	if label == "GOOD" {
		return score
	}
	return 1.0 - score
}
```

> Note: `QualityRegressionScorer` is the single piece with no 1:1 Go analog — there is no `transformers.pipeline` in Go. It is implemented on the pure-Go `hugot`/`cybertron` inference backend behind a build tag and is optional (opt-in via `--confidence-mode distilbert`). If the backend is not compiled in, construction returns an error and the cascade auto-selects `MajorityVoteScorer`/`EmbeddingConsistencyScorer` instead.

### 9.7 `RoutingPolicyTrainer`

Trains a per-tier logistic-regression classifier on historical eval results to predict per-model success probability for new task embeddings. `sklearn.LogisticRegression` becomes a hand-rolled batch gradient-descent fit over `gonum` vectors; `SentenceTransformer` becomes the injected `Embedder`; `numpy.trapz` becomes a plain trapezoidal sum. Fitted weights serialize to plain JSON float arrays (§9.3), eliminating the Python pickle/joblib deserialization vector entirely.

```go
type EvalResult struct {
	ModelID    string
	InputText  string
	OutputText string
	Passed     bool
}

type RoutingPolicyTrainer struct {
	models []string             // cheapest -> most expensive
	costs  map[string]float64   // model_id -> USD per 1M output tokens
	embed  memory.Embedder
}

func (t *RoutingPolicyTrainer) Fit(ctx context.Context, evalResults []EvalResult, accuracyTarget float64, minCases int) (RoutingPolicy, error) {
	counts := map[string]int{}
	for _, r := range evalResults {
		counts[r.ModelID]++
	}
	for _, m := range t.models {
		if counts[m] < minCases {
			return RoutingPolicy{}, fmt.Errorf("insufficient eval data for %s: %d cases (need %d)", m, counts[m], minCases)
		}
	}

	// Embed all prompts once; cache index-aligned with evalResults.
	embs := make([][]float32, len(evalResults))
	for i, r := range evalResults {
		e, err := t.embed.Embed(ctx, r.InputText)
		if err != nil {
			return RoutingPolicy{}, err
		}
		embs[i] = e
	}

	// One classifier per model tier: P(correct | tier). trainLogReg = batch GD.
	weights := map[string][]float64{}
	for _, modelID := range t.models {
		var X [][]float32
		var y []float64
		for i, r := range evalResults {
			if r.ModelID != modelID {
				continue
			}
			X = append(X, embs[i])
			if r.Passed {
				y = append(y, 1)
			} else {
				y = append(y, 0)
			}
		}
		weights[modelID] = trainLogReg(X, y, 1000) // maxIter=1000
	}
	clf := &logreg{weights: weights}

	pareto := t.ParetoCurve(embs, evalResults, clf)

	// Cheapest viable point at/above the accuracy target (else best available accuracy).
	var viable []ParetoPoint
	for _, p := range pareto {
		if p.AccuracyEstimate >= accuracyTarget {
			viable = append(viable, p)
		}
	}
	if len(viable) == 0 {
		best := pareto[0]
		for _, p := range pareto[1:] {
			if p.AccuracyEstimate > best.AccuracyEstimate {
				best = p
			}
		}
		viable = []ParetoPoint{best}
	}
	best := viable[0]
	for _, p := range viable[1:] {
		if p.CostPerTaskUSD < best.CostPerTaskUSD {
			best = p
		}
	}

	// PGR + APGR (trapezoidal integral of PGR over the swept thresholds).
	weakAcc, strongAcc := pareto[0].AccuracyEstimate, pareto[0].AccuracyEstimate
	for _, p := range pareto {
		weakAcc = math.Min(weakAcc, p.AccuracyEstimate)
		strongAcc = math.Max(strongAcc, p.AccuracyEstimate)
	}
	pgr := (best.AccuracyEstimate - weakAcc) / (strongAcc - weakAcc + 1e-9)
	apgr := trapz(pgrSeries(pareto), thresholdSeries(pareto)) /
		(pareto[len(pareto)-1].ConfidenceThreshold - pareto[0].ConfidenceThreshold + 1e-9)

	tiers := make([]PolicyTier, len(t.models))
	for i, modelID := range t.models {
		var thr *float64
		if i < len(t.models)-1 {
			v := best.ConfidenceThreshold
			thr = &v
		}
		tiers[i] = PolicyTier{
			ModelID: modelID, ConfidenceThreshold: thr,
			EstimatedFraction: best.ModelDistribution[modelID],
			CostPer1MOutputUSD: t.costs[modelID],
		}
	}

	return RoutingPolicy{
		ID: "policy-" + newID8(), AccuracyTarget: accuracyTarget,
		AccuracyEstimate: best.AccuracyEstimate, Tiers: tiers, PGR: pgr, APGR: apgr,
		CostPerTaskUSD: best.CostPerTaskUSD, BaselineCostUSD: t.costs[t.models[len(t.models)-1]] * 1000,
		EvalCasesUsed: len(evalResults), CreatedAt: time.Now().UTC().Format(time.RFC3339),
		ClassifierWeights: weights,
	}, nil
}

// ParetoCurve sweeps the confidence threshold 0.50..0.99 in 0.01 steps.
func (t *RoutingPolicyTrainer) ParetoCurve(embs [][]float32, evalResults []EvalResult, clf *logreg) []ParetoPoint {
	// Index passed(model,input) for O(1) correctness lookup.
	passed := map[string]bool{}
	for _, r := range evalResults {
		passed[r.ModelID+"\x00"+r.InputText] = r.Passed
	}
	points := make([]ParetoPoint, 0, 50)
	n := len(evalResults)
	for ti := 50; ti < 100; ti++ {
		threshold := float64(ti) / 100.0
		assigned := map[string]int{}
		correct := 0
		totalCost := 0.0
		for i, r := range evalResults {
			chosen := t.models[len(t.models)-1] // default to strongest
			for _, m := range t.models[:len(t.models)-1] {
				if clf.predict(m, embs[i]) >= threshold {
					chosen = m
					break
				}
			}
			assigned[chosen]++
			if passed[chosen+"\x00"+r.InputText] {
				correct++
			}
			estTokens := len(r.OutputText) / 4
			totalCost += t.costs[chosen] * float64(estTokens) / 1e6
		}
		dist := map[string]float64{}
		for _, m := range t.models {
			dist[m] = float64(assigned[m]) / float64(n)
		}
		acc := float64(correct) / float64(n)
		const weakAcc, strongAcc = 0.5, 0.97 // heuristic baselines (see OQ-8)
		pgr := (acc - weakAcc) / (strongAcc - weakAcc + 1e-9)
		points = append(points, ParetoPoint{
			ConfidenceThreshold: threshold, AccuracyEstimate: acc,
			CostPerTaskUSD: totalCost / float64(n), ModelDistribution: dist,
			PGR: math.Max(0, pgr),
		})
	}
	return points
}
```

### 9.8 Integration with the run dispatch path

The router integrates into the `internal/agent` / `internal/runtime` run-dispatch path at the point where the provider `Request` is constructed, before the `Stream(ctx, Request)` call. `deserialize_clf` is gone — the classifier weights are rebuilt from the policy JSON float arrays in `NewModelRouter` (no pickle path).

```go
// internal/agent (run dispatch, simplified):
if profileCfg.Routing.AutoRoute {
	policy, err := llm.LoadActivePolicy(ctx, store, profile)
	if err == nil && policy != nil {
		router := llm.NewModelRouter(*policy, embedder) // weights rebuilt from policy JSON
		choice, err := router.Route(ctx, prompt, taskType)
		if err == nil {
			req.Model = choice.ModelID // substitute the routed model into the Request
			_ = writeRoutingDecision(ctx, store, runID, choice, profile)
			slog.Info("routing: pre-routed",
				"model", choice.ModelID, "confidence", choice.Confidence, "policy", choice.PolicyID)
		}
	}
}
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

2. **Secret scanning before routing.** `internal/security` (PRD-034) must scan the task text before it is passed to `ModelRouter.Route()`. If a secret is detected, the task is refused at the security gate before any routing decision is made. This ensures that even the hash of a secret-containing task is not persisted to `routing_decisions`.

3. **No code deserialization in classifier loading.** The Go build removes the entire pickle/joblib deserialization class of risk: `RoutingPolicyTrainer` stores only the fitted logistic-regression weights as plain JSON float arrays inside `routing_policies.policy_json`. `NewModelRouter` reconstructs the classifier by reading those numbers into a struct via `encoding/json` — there is no code/object deserialization path, so the pickle-RCE vector (GHSA-mhr3-j7m5-c7c9) that motivated this consideration in Python does not exist here. The loader still validates the schema (dimension/tier count) and refuses malformed weight arrays.

4. **Policy write requires confirmation.** `tag route optimize` writes a routing policy that will affect all subsequent runs for a profile. The command requires interactive confirmation (or `--yes` / `CI=true`) before writing. This prevents accidental policy overwrites from automated pipelines.

5. **Cascade escalation is audited, not silent.** Every cascade escalation from a cheaper model to a more capable one is logged at WARNING level and written to `cascade_steps`. Routing to a cheaper model (downgrade) is also logged. Silent capability changes are not permitted, consistent with PRD-031's substitution logging requirement.

6. **Budget guard before each cascade tier.** The `internal/obs` budget gate (PRD-012) is consulted before each cascade tier call. If invoking the next tier would exceed the active budget, the cascade aborts and returns the best response seen. This prevents a misconfigured cascade from incurring unbounded cost by escalating through all tiers on every query.

7. **Classifier training data is local-only.** `RoutingPolicyTrainer` reads only from the local `internal/store` SQLite database (`eval_results`, `eval_cases`). No data is transmitted to external services during training (aside from the `Embedder` call when a provider embedding API is configured; the offline build-tag embedder keeps training fully local). The trained classifier is stored locally in SQLite. There is no opt-in telemetry or federated training path in this PRD.

8. **Confidence threshold tuning requires eval data, not production traffic.** The routing policy is trained on `eval_results` (manually curated suites), not on production task text. This prevents training a model that learns to route based on sensitive content patterns in production prompts.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`internal/llm/routing_test.go`)

Table-driven `testing` tests; the `Embedder`, provider `Stream`, scorer, and budget guard are injected as interfaces with fakes.

| Test | What It Verifies |
|------|-----------------|
| `TestModelRouterReturnsCheapestAboveThreshold` | Given a fake classifier returning high confidence for Haiku, `Route()` returns Haiku, not Sonnet or Opus. |
| `TestModelRouterFallsBackOnLowConfidence` | Given a fake classifier returning confidence < threshold for all tiers, returns the final fallback tier. |
| `TestModelRouterLatencyUnder10ms` | `Route()` with a preloaded (offline) embedder completes in < 10 ms p99 over 100 calls (`testing.B` p99 harness). |
| `TestFrugalCascadeStopsAtFirstConfidentTier` | Cascade with two tiers, first returns confidence 0.92 (threshold 0.85): only one tier invoked. |
| `TestFrugalCascadeEscalatesOnLowConfidence` | Cascade with two tiers, first returns confidence 0.61: both tiers invoked, escalation logged. |
| `TestFrugalCascadeBudgetGuardAborts` | Budget guard returns false after tier 1: cascade aborts, returns tier 1 response. |
| `TestCascadeHardCapAt5Tiers` | `NewFrugalCascade` with 6 tiers returns a non-nil error. |
| `TestMajorityVoteScorerDiscrete` | 8/10 samples agree on "yes": score = 0.8. |
| `TestEmbeddingConsistencyScorerIdenticalSamples` | All N samples identical: cosine similarity = 1.0. |
| `TestRoutingDecisionWrittenToSQLite` | After a `Route()` call, `routing_decisions` has one row with the correct `query_hash`. |
| `TestQueryHashIsSHA256NotRawText` | `routing_decisions.query_hash` matches `sha256Hex(task_text)`, not the raw task text. |
| `TestClassifierWeightsAreJSONFloatsNotPickle` | The persisted `policy_json` is valid JSON of float arrays; no binary/base64 blob; round-trips through `NewModelRouter` without any code deserialization. |
| `TestPolicyTrainerRequiresMinCases` | `RoutingPolicyTrainer.Fit()` with fewer than `minCases` rows returns an error. |
| `TestParetoCurveMonotoneAccuracyVsCost` | Higher confidence threshold => higher accuracy estimate AND higher cost per task. |
| `TestCascadeStepLogsWritten` | After a 2-tier cascade, `cascade_steps` has 2 rows with the correct `decision_id` FK. |

### 11.2 Integration Tests (`internal/llm/routing_integration_test.go`)

Real `internal/store` temp DB; fake provider `Stream`; offline embedder.

| Test | What It Verifies |
|------|-----------------|
| `TestOptimizeReadsEvalResultsAndWritesPolicy` | Seeds 30 `eval_results` rows for two model tiers; runs the `route optimize` handler; asserts `routing_policies` has one `active=1` row for the profile. |
| `TestAutoRouteAppliedOnRun` | Profile has `routing.auto_route: true`; the run-dispatch path is invoked; `routing_decisions` has one row with `policy_id` matching the active policy. |
| `TestCascadeCmdProducesCascadeSteps` | The `route cascade` handler is called with 3 tiers; `cascade_steps` has ≥ 1 row. |
| `TestRouteStatsAggregatesDecisions` | Seeds 50 `routing_decisions` rows; `route stats --json` output contains `total_routed_tasks: 50`. |
| `TestCalibrateHoldoutAccuracyReported` | Seeds 100 eval results; `route calibrate` output JSON contains an `accuracy_estimate` field. |
| `TestPolicyDeleteDisablesAutoRoute` | Active policy is deleted; a subsequent run with `auto_route: true` uses the profile default model. |

### 11.3 Benchmarks (`internal/llm/routing_bench_test.go`, `testing.B`)

| Benchmark | What It Verifies |
|------|-----------------|
| `BenchmarkRouterRouteP99Latency` | 200 calls to `Route()` with a preloaded offline embedder: p99 < 10 ms. |
| `BenchmarkTrainerFit1000Cases` | `RoutingPolicyTrainer.Fit()` with 1000 synthetic eval rows: wall time < 30 s. |
| `BenchmarkEmbeddingScorer5Samples` | `EmbeddingConsistencyScorer.Score()` with 5 samples: < 500 ms. |
| `BenchmarkCascade2TierOverhead` | 2-tier cascade where tier 1 is always confident: total overhead vs. a direct single call < 50 ms. |

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
| AC-08 | `routing_decisions.query_hash` equals `sha256Hex(task_text)` (`crypto/sha256`) for every row; `routing_decisions` has no column containing raw task text. | Schema inspection; unit test. |
| AC-09 | `ModelRouter.Route()` completes in < 10 ms p99 over 200 calls with a preloaded offline embedder. | `testing.B` benchmark. |
| AC-10 | Importing the routing packages performs no init-time embedder/model load; `NewModelRouter` is the first point that touches the embedder (verified by a fake embedder whose init counter is 0 until `NewModelRouter`). | Init-counter assertion in unit test. |
| AC-11 | With `routing.auto_route: true` in a profile with an active policy, `tag run --profile coder "add docstring"` dispatches to the model chosen by the router, not the profile's default model, and writes to `routing_decisions`. | Integration test with a fake provider; inspect the `Request.Model` sent to `Stream`. |
| AC-12 | `tag route calibrate --profile coder` outputs `accuracy_estimate`, `pgr`, and `ece` fields in JSON. | Integration test; seed eval data; parse output. |
| AC-13 | `tag route policy delete <policy-id>` sets `active=0` and `deleted_at` is non-null in the database. | Integration test; inspect SQLite after command. |
| AC-14 | Every cascade tier call produces a distinct child span in the `spans` table with `routing.tier` attribute. | Integration test; query spans table after cascade run. |
| AC-15 | When budget guard fires mid-cascade, cascade returns the highest-confidence response from completed tiers; `routing_decisions.cost_usd` is below the budget limit. | Integration test with mock budget guard returning False after tier 1. |
| AC-16 | `tag route optimize` with fewer than `min_eval_cases` (default: 20) eval cases per tier exits non-zero and prints the count of available cases and the minimum required. | CLI invocation test with sparse eval data. |

---

## 13. Dependencies

| Dependency | Type | Version / Notes | Required By |
|------------|------|-----------------|-------------|
| `internal/memory` (`Embedder`) | Internal (Go) | `all-MiniLM-L6-v2` (22 MB); provider embedding API by default, offline via build-tag `cybertron`; cached in `~/.tag/models/`. Shared singleton with `internal/toolindex` (PRD-043). | `ModelRouter`, `EmbeddingConsistencyScorer`, `RoutingPolicyTrainer` |
| `gonum.org/v1/gonum` | Go module | BSD-3, GA; float vector ops for the hand-rolled logistic-regression fit + trapezoidal APGR integral | `RoutingPolicyTrainer` |
| `golang.org/x/sync/errgroup` | Go module | Concurrent sample calls for `MajorityVoteScorer` | `MajorityVoteScorer` |
| `hugot` / `nlpodyssey/cybertron` | Go module | Pure-Go text-classification backend behind a build tag; replaces the Python HF-transformers DistilBERT pipeline; optional scorer | `QualityRegressionScorer` |
| `internal/llm` provider interface | Internal (Go) | `Stream(ctx, Request)->chan Event` over anthropic-sdk-go v1.55 / openai-go/v3; token counts via tiktoken-go (OpenAI) / len/4 (Anthropic) | Cascade tiers, sample funcs |
| `crypto/sha256`, `encoding/json` | Go stdlib | `query_hash`/`response_hash`; policy weight (de)serialization — no pickle/gob | `routing_decisions`, `RoutingPolicy` |
| PRD-027 (eval framework) | Internal module | `eval_results`, `eval_cases` SQLite tables used as training data | `RoutingPolicyTrainer`, `tag route optimize` |
| PRD-012 (`internal/obs` budget) | Internal module | Budget gate consulted before each cascade tier | `FrugalCascade` budget guard |
| PRD-013 (`internal/obs` tracing) | Internal module | `go.opentelemetry.io/otel` child spans for each routing decision and cascade tier | Span emission |
| PRD-034 (`internal/security`) | Internal module | Scans task text before routing; called by the dispatcher | `ModelRouter.Route()` caller |
| PRD-031 (model fallback chains) | Internal module | Co-existing routing path; must not conflict on the `route` overrides or `route` command dispatch | Architecture boundary |
| PRD-043 (`internal/toolindex`) | Internal module | Shares the cached `Embedder` instance | `ModelRouter`, `EmbeddingConsistencyScorer` |
| PRD-041 (`internal/obs` cost attribution) | Internal module | Routing span attributes follow the pinned GenAI semconv table + go:embed pricing table (gobwas/glob) | Span attributes, cost |

---

## 14. Open Questions

| # | Question | Owner | Resolution Target |
|---|----------|-------|-------------------|
| OQ-1 | Should `ModelRouter` share the `Embedder` instance with `internal/toolindex` (PRD-043) to avoid loading two copies of `all-MiniLM-L6-v2` into memory? If so, where does the shared singleton live (e.g., an `internal/memory`-level provider) to keep the package dependency graph acyclic? | Routing + Tool Retrieval owners | Before implementation start |
| OQ-2 | `QualityRegressionScorer` requires a pure-Go quality-classifier model at `~/.tag/models/quality-scorer` (hugot/cybertron backend). Does TAG ship this model in the binary via `go:embed`, host it for download, or require users to supply it? If hosted, what is the distribution mechanism, and is it gated behind the build tag? | Platform team | Before beta release |
| OQ-3 | The Pareto curve sweep is O(N_thresholds × N_eval_cases × N_models). At N=100 cases, 3 models, 50 threshold points, this is 15,000 operations — fast. At N=10,000 cases it becomes 1.5M. Is there a training data size cap, or should the trainer sample from eval_results when |eval_results| > some limit? | Engineering | Before implementation |
| OQ-4 | PRD-031 fallback chains and PRD-107 cascade are architecturally separate, but both can override the model used for a run. If a cascade escalation produces an error that would normally trigger a PRD-031 fallback hop, which takes precedence? Proposed: PRD-031 fires after PRD-107 cascade exhausts its tiers. | Architecture review | Before implementation |
| OQ-5 | Should `tag route optimize` also consume `steps` table data (actual run outputs) in addition to `eval_results`? Steps are not labeled with pass/fail, but their duration and token count are available as proxy signals. | Product | Phase 2 planning |
| OQ-6 | The `MajorityVoteScorer` requires N extra LLM calls to score confidence. At N=10, this multiplies the cost of the cheapest tier by 10× before deciding whether to escalate — potentially more expensive than just calling Opus directly. Should the default N be 3 or 5 for the scorer path (distinct from PRD-101's sampling for output quality)? | Engineering | Before implementation |
| OQ-7 | `cascade_steps.response_hash` stores a SHA-256 hash of each tier's response. Is this sufficient for debugging, or should the full response be stored? Storing full responses could expose PII. Proposed: store hash only; full responses are available in `steps` via `run_id` FK. | Security + Engineering | Before implementation |
| OQ-8 | PGR and APGR metrics require a defined "weak baseline" and "strong baseline" accuracy. Currently these are hardcoded heuristics (0.50, 0.97). Should they be computed dynamically from the eval results (worst-model accuracy, best-model accuracy)? | Engineering | Before implementation |

---

## 15. Complexity and Timeline

### Phase 1: Core Infrastructure (Days 1–5)

- Define `routing_policies`, `routing_decisions`, `cascade_steps` as numbered migrations in `internal/store/migrate/`.
- Implement `RoutingPolicy`, `PolicyTier`, `ModelChoice`, `CascadeStep`, `CascadeResult`, `ParetoPoint` structs in `internal/llm`.
- Implement `ModelRouter` with the injected `Embedder`, the in-process `logreg`, and the `Route()` method (returning `ModelChoice`).
- Implement the `routing_decisions` write helper and wire it into the `auto_route` dispatch path in `internal/agent`.
- Unit tests: `TestModelRouter*` and `TestRoutingDecisionWrittenToSQLite`.

### Phase 2: Cascade & Scorers (Days 6–10)

- Implement `FrugalCascade` with `Run()`, budget-guard integration, and the 5-tier hard cap.
- Implement `MajorityVoteScorer`, `EmbeddingConsistencyScorer`, and `QualityRegressionScorer` (build-tag backend).
- Implement the `route cascade` cobra command in `internal/cli`.
- OTel span attribution for each cascade tier via `internal/obs` (PRD-013 integration).
- Unit tests: `TestFrugalCascade*`, `Test*Scorer*`.

### Phase 3: Policy Training & Optimize (Days 11–16)

- Implement `RoutingPolicyTrainer` with `Fit()` (gonum logistic regression) and `ParetoCurve()`.
- Implement the `route optimize` command: read eval_results, train, display the Pareto point, confirm, write policy.
- Implement the `route calibrate` command: holdout split, refit, evaluate, report ECE and PGR.
- Implement `route policy` subcommands (list, show, delete, activate).
- Unit tests: `TestPolicyTrainer*`, `TestParetoCurve*`.

### Phase 4: Stats, Integration, and Config (Days 17–21)

- Implement the `route stats` command with aggregation over `routing_decisions`.
- Wire all config keys under `routing:` into the koanf/v2 config schema and validation.
- Integration tests: `TestOptimizeReadsEvalResultsAndWritesPolicy`, `TestAutoRouteAppliedOnRun`, `TestCascadeCmdProducesCascadeSteps`, etc.
- Benchmarks: `BenchmarkRouterRouteP99Latency`, `BenchmarkTrainerFit1000Cases`.
- Documentation: update `tag route --help` and add `routing` config key docs.

### Phase 5: Hardening and Edge Cases (Days 22–26)

- Budget-guard integration with `internal/obs` (PRD-012): pre-cascade and per-tier checks.
- Secret-scanning integration: ensure `internal/security` is called before `ModelRouter.Route()` in all dispatch paths.
- Soft delete for `routing_policies` (`deleted_at` pattern, `active = 0`).
- Shared `Embedder` singleton with `internal/toolindex` (OQ-1 resolution).
- Coverage audit: ensure ≥ 85% line coverage (`go test -cover`) across the routing packages.
- Review against acceptance criteria AC-01 through AC-16; close all open questions with owners.

**Total estimate: 26 working days (~5.5 weeks).** Effort estimate is L (2–4 weeks for implementation core, extended to 5.5 weeks including hardening and integration). Two engineers working in parallel on Phases 2 and 3 could compress the schedule to ~18 days.

