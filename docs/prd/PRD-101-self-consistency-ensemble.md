# PRD-101: Self-Consistency Ensemble: Sample N, Majority-Vote (`tag submit --samples N --vote majority`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** S (3-5 days)
**Category:** Advanced Reasoning & Planning
**Affects:** `ensemble.py` (new), `src/tag/controller.py` (flag wiring), `tag.sqlite3` (new tables)
**Depends on:** PRD-027 (eval framework — quality scoring), PRD-028 (sandbox — isolated code execution per sample), PRD-013 (agent tracing — per-sample span attribution), PRD-034 (secret scanning — prompt content before sampling), PRD-012 (budget enforcement — N×cost guard), PRD-043 (vector tool retrieval — embedding-space aggregation), PRD-041 (OTel span cost attribution — per-sample cost tags), PRD-045 (LLM-as-judge — USC meta-judge path)
**Inspired by:** Self-consistency prompting (Wang et al. 2022), EMS paper 2025, multi-agent voting

---

## 1. Overview

Quality of LLM outputs is not deterministic. Given the same prompt, a model sampling at non-zero temperature produces different reasoning chains on every call — some leading to correct conclusions, others to plausible-but-wrong ones. The standard TAG workflow calls the model once and returns whatever it produces. This single-sample strategy is fast and cheap, but it means every run is exposed to the full variance of the model's distribution: one unlucky sampling event can produce a confidently wrong answer, a subtly broken code patch, or a security review that misses the critical finding.

Self-consistency, introduced by Wang et al. (2022), addresses this by sampling N independent completions from the same prompt (with temperature > 0 to enforce diversity), then selecting the final answer by majority vote over the discrete outputs — effectively marginalising out the intermediate reasoning chains. Empirically, N=10 raises GSM8K accuracy from 56.5 % to 74.4 % with no prompt engineering; N=40 reaches 74.4 %. The technique is model-agnostic and requires no fine-tuning or external judge. For code review and security audits — domains where a missed vulnerability has asymmetric cost — sampling multiple independent reasoning paths and requiring consensus substantially reduces the false-negative rate.

TAG must handle three answer types: discrete/closed-form (yes/no, vulnerability class labels, tool-call decisions), open-ended prose (code explanations, review summaries), and structured JSON (tool call arguments, diff patches). This PRD specifies a three-mode aggregation stack: (a) hard majority vote for discrete answers, (b) embedding-space centroid clustering via `sentence-transformers` (USC-embedding) for open-ended prose without an extra LLM call, and (c) LLM-as-judge meta-call (USC-LLM) for open-ended prose when an authoritative synthesis is preferred over the nearest-centroid. Mode selection is automatic based on answer-type detection with a manual override flag.

The feature integrates with existing TAG infrastructure at every layer: `budget.py` gates total N×cost before the first sample fires; `tracing.py` emits one child span per sample under a parent `ensemble` span; `eval_framework.py` treats ensemble runs as a first-class evaluation variant; `security.py` scans the prompt before sampling to prevent secret leakage into N parallel API calls; and the `sc_samples` SQLite table stores all raw samples for debugging, cost attribution, and future fine-tuning datasets.

Early stopping (`--stop-early`) aborts remaining samples as soon as a consensus threshold is reached, reducing cost on easy queries. The stop-early check runs after each batch of concurrent samples and terminates the remaining futures before any network call is made.

---

## 2. Problem Statement

### 2.1 Single-Sample Variance Causes Silent Failures in High-Stakes Tasks

TAG's primary use cases — security review, code refactoring, and architecture audits — have asymmetric error costs. A false negative on a SQL injection check is not a minor inconvenience; it is a production vulnerability. The current `tag submit` dispatches exactly one inference call and returns its output. If the model's temperature is above zero (as it is for all non-deterministic profiles), there is no mechanism to detect when the returned answer is an outlier within the model's own distribution. Users have no signal indicating whether the output is robust (the model would answer identically on 9 of 10 draws) or fragile (this was a lucky 1-in-10 correct answer). High-stakes single-call outputs are therefore systematically under-trusted by experienced users and over-trusted by novices — neither outcome is acceptable.

### 2.2 No Structured Mechanism for Answer Confidence Beyond Token Probabilities

Token log-probabilities are unavailable or unreliable as confidence signals for multi-step reasoning tasks. They reflect single-token prediction confidence, not the confidence of a multi-step conclusion. TAG currently has no mechanism to estimate answer confidence at the semantic level. The `eval_framework.py` (PRD-027) provides quality scoring after the fact using an LLM judge, but this is expensive ($0.01–$0.05 per eval call) and requires manual invocation. Self-consistency provides a cheap, automatic confidence proxy — the fraction of N samples that agree on the winning answer — without any judge model call for the majority-vote path.

### 2.3 Open-Ended Outputs Have No Aggregation Primitive

Wang et al.'s original majority vote applies only to closed-form answers. TAG routinely produces open-ended outputs: multi-paragraph security reviews, refactored code blocks, architectural recommendations. There is no existing mechanism to aggregate N such outputs into a single higher-quality response. Universal Self-Consistency (Chen et al. 2023, arXiv:2311.17311) and its embedding-space variant (arXiv:2606.12003) fill this gap but are not implemented in TAG. Without them, the self-consistency feature would be limited to toy classification tasks, excluding the majority of real TAG workloads.

---

## 3. Goals and Non-Goals

### 3.1 Goals

| # | Goal |
|---|------|
| G1 | `tag submit --samples N --vote majority` samples N independent completions and returns the majority-vote winner for discrete answers. |
| G2 | `--vote embedding` aggregates N open-ended completions via embedding-space agglomerative clustering; the completion nearest to the largest cluster's centroid is returned. |
| G3 | `--vote llm` aggregates N completions via a meta-prompt USC call using the same or a smaller judge model. |
| G4 | Answer-type detection (`detect_answer_type()`) automatically selects the aggregation mode when `--vote` is omitted, choosing `majority` for structured/discrete outputs and `embedding` for prose. |
| G5 | `--stop-early` halts remaining samples as soon as the consensus threshold (default ≥ 60 % agreement) is reached, cancelling pending futures before network calls. |
| G6 | Every sample is persisted to `sc_samples` SQLite table with its rank, vote count, embedding vector, latency, and token cost — enabling post-hoc analysis and dataset collection. |
| G7 | Total ensemble cost (N × per-sample estimate) is computed and displayed before the first API call, with `--yes` / `CI=true` bypass, consistent with PRD-027's cost gate pattern. |
| G8 | Each sample runs as a child span of an `ensemble` parent span under TAG's existing tracing infrastructure (PRD-013), carrying `sc.sample_index`, `sc.vote_winner`, and `sc.consensus_ratio` attributes. |
| G9 | `--samples` and `--vote` are supported on both `tag submit` and `tag run` surfaces. |
| G10 | Budget enforcement (PRD-012) accounts for N×estimated tokens before sampling begins; the run is blocked if it would exceed the active budget. |

### 3.2 Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Multi-agent debate (agents critique each other across rounds). Debate is a separate feature; this PRD covers independent sampling only. |
| NG2 | Speculative decoding at the model-inference level. This is sampling at the prompt level, not at the token level. |
| NG3 | Fine-tuning on the `sc_samples` dataset. Storage is the responsibility of this PRD; training pipelines are out of scope. |
| NG4 | Automatic N selection based on task difficulty. N is always user-specified; adaptive N is a future extension. |
| NG5 | Aggregation of samples across different prompts or profiles. All N samples use the same prompt and profile. |
| NG6 | Parallel multi-model sampling (sample from GPT-4o, Claude, Gemini simultaneously). This PRD covers single-model ensembles only. |
| NG7 | Modifying the stop-early threshold via a per-task config key (only via CLI flag in this PRD). |

---

## 4. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Majority-vote correctness lift | ≥ 10 pp accuracy improvement vs. single-sample on TAG eval suite (PRD-027) for N=5, discrete prompts | Run `tag eval run` with `--samples 5 --vote majority` vs. baseline; compare `pass_count / total_count` |
| USC-embedding similarity | Winning sample cosine similarity to centroid ≥ 0.85 (all-MiniLM-L6-v2) | Assert in unit test with synthetic N=10 fixture |
| Stop-early cancellation | When consensus reached at sample K < N, remaining N-K futures are cancelled before any network bytes sent | Mock `httpx` at transport layer; assert call count = K |
| Cost gate accuracy | Displayed pre-flight cost estimate within ±15 % of actual spend | Compare estimate vs. `sc_samples.prompt_tokens + completion_tokens` sum after run |
| SQLite persistence | All N samples written to `sc_samples` within 100 ms of ensemble completion | Assert in integration test; measure with `time.perf_counter` |
| Span attribution | `ensemble` parent span + N child spans visible in `tag trace show` | Integration test asserting span count = N+1 |
| Budget enforcement | `tag submit --samples 10` blocked if N×estimated_cost > active budget cap | Unit test with mocked budget.get_remaining() = 0 |
| Wall-time overhead (N=1) | `--samples 1` latency ≤ 105 % of baseline `tag submit` (ensemble scaffolding overhead < 5 %) | Benchmark 20 runs; 95th-percentile ratio |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|--------|-----------|----------|
| U1 | Security engineer | run `tag submit --samples 5 --vote majority --stop-early --profile reviewer --prompt "SQL injection check"` | I get a consensus-backed finding with reduced false-negative risk; if 3 of 5 samples agree early, I save 40 % of the API cost |
| U2 | Developer | run `tag run --samples 3 --vote majority --profile coder "refactor the auth module"` | The refactoring output reflects agreement across 3 independent reasoning paths, not a single lucky (or unlucky) draw |
| U3 | Platform engineer | inspect `tag ensemble show <ensemble_id>` | I can see all N raw samples, their vote counts, latencies, and token costs for debugging and audit purposes |
| U4 | Cost-conscious team lead | see the pre-flight cost estimate before a 10-sample ensemble fires | I can approve or cancel before incurring N×cost, and set appropriate N values for the team's budget |
| U5 | QA engineer | run `tag eval run --suite evals/security.yaml --samples 5 --vote majority` | I can measure whether self-consistency improves the eval pass rate on our security benchmark suite |
| U6 | Developer | run `tag submit --samples 3 --vote embedding --prompt "Explain the tradeoffs of this design"` | I get the most representative open-ended answer from 3 independent completions without spending on an extra LLM judge call |
| U7 | Researcher | run `tag submit --samples 5 --vote llm --judge-model claude-haiku-4-5 --prompt "Summarise the security findings"` | A lightweight meta-model synthesises the best aspects of all 5 samples into a single authoritative summary |
| U8 | Developer | run `tag submit --samples 3 --vote majority --json` | I receive machine-readable JSON with the winning answer, consensus ratio, all sample IDs, and per-sample token cost for programmatic downstream use |
| U9 | Developer | omit `--vote` entirely on a discrete-answer prompt | answer-type detection automatically selects `majority` mode without requiring flag knowledge |
| U10 | Developer | run `tag ensemble list --last 10` | I can review recent ensemble runs with their N, vote mode, consensus ratio, and total cost at a glance |

---

## 6. Proposed CLI Surface

### 6.1 `tag submit` with ensemble flags

```bash
# Minimal — 3 samples, hard majority vote, discrete prompt
tag submit --samples 3 --vote majority --prompt "Is this a SQL injection vulnerability? Yes or No."

# Full security review — 5 samples, early stop, custom profile
tag submit \
  --samples 5 \
  --vote majority \
  --stop-early \
  --consensus-threshold 0.6 \
  --profile reviewer \
  --prompt "Review this security issue"

# Open-ended, embedding aggregation
tag submit \
  --samples 3 \
  --vote embedding \
  --embed-model all-MiniLM-L6-v2 \
  --prompt "Explain the tradeoffs of using Redis vs PostgreSQL for session storage"

# Open-ended, LLM-as-judge USC synthesis
tag submit \
  --samples 5 \
  --vote llm \
  --judge-model claude-haiku-4-5 \
  --prompt "Summarise the key risks in this architecture"

# Machine-readable output
tag submit --samples 3 --vote majority --json \
  --prompt "Does this function have a memory leak? Yes or No."

# Dry-run — show cost estimate, do not call API
tag submit --samples 10 --vote majority --dry-run \
  --prompt "Review this code for security issues"
```

**New flags on `tag submit` and `tag run`:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--samples N` | `int` | `1` | Number of independent completions to sample. N=1 disables ensemble logic entirely (zero overhead). |
| `--vote MODE` | `choice` | auto-detect | Aggregation mode: `majority`, `embedding`, `llm`, or `auto` (default, selects based on `detect_answer_type()`). |
| `--stop-early` | `flag` | `False` | Halt remaining samples once consensus threshold is reached. |
| `--consensus-threshold F` | `float` | `0.6` | Minimum fraction of samples that must agree for early stopping. Range: 0.5–1.0. |
| `--embed-model NAME` | `str` | `all-MiniLM-L6-v2` | Sentence-transformer model for `--vote embedding`. |
| `--judge-model MODEL_ID` | `str` | profile default | Model used for the USC meta-call in `--vote llm`. |
| `--samples-temperature F` | `float` | `0.7` | Sampling temperature. Must be > 0 for diversity; 0 collapses to greedy (not recommended). |
| `--dry-run` | `flag` | `False` | Print cost estimate and exit without making API calls. |
| `--yes` | `flag` | `False` | Skip cost confirmation prompt (auto-set when `CI=true`). |

### 6.2 `tag ensemble` subcommands

```bash
# List recent ensemble runs
tag ensemble list [--last N] [--profile NAME] [--json]

# Show all samples for a specific ensemble run
tag ensemble show <ensemble_id> [--json] [--sample-index K]

# Export all samples as JSONL (for fine-tuning datasets)
tag ensemble export <ensemble_id> --output samples.jsonl

# Compare consensus ratio across recent runs for a profile
tag ensemble stats --profile reviewer --last 20
```

### 6.3 Output Format

**Human-readable (default):**

```
Ensemble run: ens-7f3a2b (N=5, vote=majority, profile=reviewer)
Pre-flight cost estimate: ~$0.031 (5 × ~$0.0062 per sample) [y/N] y

  Sample 1/5  [████████████████████] 1.2s   512 tok   ✓
  Sample 2/5  [████████████████████] 1.4s   489 tok   ✓
  Sample 3/5  [████████████████████] 0.9s   501 tok   ✓ (consensus reached — stopping early)

Consensus: 3/3 samples agree (100.0%)
Vote winner: "Yes — SQL injection vulnerability present (unparameterised query at line 42)"

Actual cost: $0.019 (3 samples, early stop saved $0.012)
```

**JSON output (`--json`):**

```json
{
  "ensemble_id": "ens-7f3a2b",
  "n_requested": 5,
  "n_sampled": 3,
  "vote_mode": "majority",
  "consensus_ratio": 1.0,
  "stop_early_triggered": true,
  "winner": "Yes — SQL injection vulnerability present (unparameterised query at line 42)",
  "answer_type": "discrete",
  "samples": [
    {
      "index": 0,
      "text": "Yes — SQL injection...",
      "vote_key": "yes",
      "prompt_tokens": 312,
      "completion_tokens": 200,
      "latency_ms": 1201,
      "cost_usd": 0.0063
    }
  ],
  "total_cost_usd": 0.019,
  "estimated_cost_usd": 0.031,
  "created_at": "2026-06-17T09:14:22Z"
}
```

---

## 7. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | When `--samples 1` (or `--samples` is absent), `ensemble.py` is not imported and the code path is identical to the current single-call path with zero overhead. | Must |
| FR-02 | When N > 1, the system fires N concurrent API calls using `asyncio.gather` (or `ThreadPoolExecutor` for sync Hermes callers), each with an independent random seed derived from `uuid4()`. | Must |
| FR-03 | `majority_vote(samples)` normalises each sample's final answer by stripping punctuation and lowercasing, counts frequencies, and returns the modal answer plus its count and the total N. In the case of a tie, the sample with the lowest `latency_ms` among the tied answers is preferred. | Must |
| FR-04 | `embedding_vote(samples, model)` encodes all N sample texts with `SentenceTransformer(model)`, runs agglomerative clustering (`sklearn.cluster.AgglomerativeClustering`, `metric='cosine'`, `linkage='average'`), identifies the largest cluster, computes the centroid, and returns the sample with maximum cosine similarity to the centroid. | Must |
| FR-05 | `llm_vote(samples, judge_model, profile)` constructs a USC meta-prompt listing all N samples and calls the judge model once, returning its synthesis. The meta-prompt template is stored as `USC_META_PROMPT` in `ensemble.py`. | Must |
| FR-06 | `detect_answer_type(prompt_text)` returns `"discrete"` if the prompt contains decision-requesting patterns (regex: `r'\b(yes|no|true|false|correct|incorrect|vulnerable|safe)\b'` or ends with `?` and is < 200 chars), otherwise returns `"open_ended"`. This drives auto-mode selection. | Must |
| FR-07 | `--stop-early` checks consensus after each sample completes (not in a batch). If `winning_count / n_sampled >= consensus_threshold`, all pending futures are cancelled via `future.cancel()` before any HTTP request is dispatched. | Must |
| FR-08 | Before the first API call, `compute_preflight_cost(n, model_id, prompt_tokens)` estimates total cost as `n × (prompt_tokens × prompt_rate + max_completion_tokens × completion_rate)` using rates from `budget.py`. If `--yes` is not set and `CI` env var is not `"true"`, the user is prompted for confirmation. | Must |
| FR-09 | Budget enforcement calls `budget.check_and_reserve(profile, estimated_cost)` before sampling. If the budget would be exceeded, the command exits with code 1 and a human-readable error message showing remaining budget. | Must |
| FR-10 | Every sample is written to `sc_samples` (see SQLite DDL in §9.1) within the same `open_db()` transaction as the ensemble row, using `INSERT OR REPLACE`. | Must |
| FR-11 | Tracing: an `ensemble` parent span is opened via `tracing.open_span()` at the start; each sample opens a child span with attributes `sc.sample_index`, `sc.vote_key` (normalised answer for majority mode), `sc.prompt_tokens`, `sc.completion_tokens`; the parent span is closed with `sc.vote_winner` and `sc.consensus_ratio` attributes. | Must |
| FR-12 | Secret scanning (PRD-034): `security.scan_for_secrets(prompt_text)` is called before the pre-flight cost display. If secrets are detected, the command aborts with exit code 1 and does not display the prompt content in the error message. | Must |
| FR-13 | `tag ensemble list` reads from `sc_ensembles` ordered by `created_at DESC`, with `--last N` (default 20), `--profile NAME` filter, and `--json` flag. | Should |
| FR-14 | `tag ensemble show <ensemble_id>` reads from `sc_samples` for that ensemble, rendering each sample's text (truncated to 200 chars in human mode), vote key, latency, and cost. `--sample-index K` prints the full text for sample K. | Should |
| FR-15 | `tag ensemble export <ensemble_id> --output FILE` writes a JSONL file with one object per sample, formatted as `{"prompt": "...", "completion": "...", "vote_key": "...", "is_winner": true/false}`, suitable for SFT fine-tuning datasets. | Could |
| FR-16 | When `--vote embedding` is used and `sentence-transformers` is not installed, the command falls back to `majority` mode (if discrete) or `llm` mode (if open-ended), emitting a `print_warning()` with install instructions. | Must |
| FR-17 | The `--samples-temperature` value must be > 0 when N > 1; if the user sets `--samples-temperature 0`, the CLI emits a warning and overrides to 0.1 to preserve diversity. | Must |
| FR-18 | The `--judge-model` flag defaults to the value of `self_consistency.judge_model` in the TAG config, then to `claude-haiku-4-5` (cheapest capable model), then to the profile's default model. | Should |
| FR-19 | `tag ensemble stats --profile NAME --last N` computes mean consensus ratio, mean samples used, mean cost per ensemble, and p50/p95 latency across the last N ensemble runs for a given profile. | Could |
| FR-20 | `--dry-run` displays the cost estimate, the detected answer type, the selected vote mode, and exits with code 0 without making any API call. | Must |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | **Latency (wall time, N samples)** | Wall time for N concurrent samples ≤ 1.2× the single-sample wall time for N ≤ 5 on the same model, assuming sufficient API concurrency. |
| NFR-02 | **Memory footprint** | Embedding matrix for N=40 samples × 384-dim (MiniLM) = 61 KB; total peak memory increase from ensemble module < 50 MB including model load (model is lazy-loaded and cached across calls). |
| NFR-03 | **SQLite write performance** | All N sample rows inserted in a single transaction; total SQLite write overhead < 10 ms for N ≤ 40 on SSD. |
| NFR-04 | **Graceful degradation** | If any single sample's API call fails with a retriable error (5xx, timeout), that sample is retried once with 1 s backoff. If it fails again, it is recorded as `status='error'` in `sc_samples` and excluded from voting. Voting proceeds on the remaining samples if at least ⌈N/2⌉ succeed. |
| NFR-05 | **Cancellation correctness** | When stop-early triggers, cancelled futures must not result in any pending HTTP connection being left open; `httpx.AsyncClient` context managers must be properly exited. |
| NFR-06 | **Reproducibility** | Given `--samples-temperature 0` (overridden to 0.1 per FR-17), the majority-vote winner across runs for the same prompt should be stable (>= 80 % identical across 5 independent runs in CI). Not guaranteed but targeted. |
| NFR-07 | **Security** | The USC meta-prompt for `--vote llm` must not include secret-scanned tokens detected by `security.py`; raw sample texts passed to the judge are filtered through the same secret-masking logic. |
| NFR-08 | **Cost ceiling** | A hard-coded maximum N of 40 (matching Wang et al. 2022's largest experiment) is enforced; `--samples > 40` raises `ValueError` with a message referencing the paper's diminishing-returns finding. |
| NFR-09 | **Import isolation** | `import tag.ensemble` must not be triggered when `--samples 1` (or absent); the import is deferred inside `cmd_submit` / `cmd_run` behind `if args.samples > 1`. |
| NFR-10 | **OTel compatibility** | All ensemble spans conform to TAG's OTel semantic conventions (PRD-041); `sc.sample_index` is a standard integer attribute; `sc.consensus_ratio` is a float attribute; both are listed in `otel_semconv.py`. |

---

## 9. Technical Design

### 9.1 SQLite DDL

All tables use WAL-mode inherited from `open_db()`. Foreign key enforcement is enabled per connection.

```sql
-- Parent record: one row per ensemble invocation
CREATE TABLE IF NOT EXISTS sc_ensembles (
    id                  TEXT PRIMARY KEY,              -- ens-<uuid4>
    profile             TEXT NOT NULL,
    model_id            TEXT NOT NULL,
    prompt_sha256       TEXT NOT NULL,                 -- sha256(prompt_text) for dedup/analytics
    prompt_preview      TEXT NOT NULL,                 -- first 200 chars of prompt
    n_requested         INTEGER NOT NULL,
    n_sampled           INTEGER NOT NULL DEFAULT 0,    -- updated on completion
    vote_mode           TEXT NOT NULL,                 -- 'majority' | 'embedding' | 'llm'
    answer_type         TEXT NOT NULL,                 -- 'discrete' | 'open_ended' | 'structured'
    consensus_ratio     REAL,                          -- NULL until voting complete
    stop_early          INTEGER NOT NULL DEFAULT 0,    -- 1 if --stop-early was set
    stop_early_triggered INTEGER NOT NULL DEFAULT 0,   -- 1 if early stop actually fired
    winner_sample_id    TEXT,                          -- FK to sc_samples.id
    estimated_cost_usd  REAL,
    actual_cost_usd     REAL,
    status              TEXT NOT NULL DEFAULT 'running', -- 'running'|'done'|'error'
    created_at          TEXT NOT NULL,
    completed_at        TEXT
);
CREATE INDEX IF NOT EXISTS idx_sce_profile ON sc_ensembles(profile, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_sce_status  ON sc_ensembles(status, created_at DESC);

-- One row per sampled completion
CREATE TABLE IF NOT EXISTS sc_samples (
    id                  TEXT PRIMARY KEY,              -- scs-<uuid4>
    ensemble_id         TEXT NOT NULL REFERENCES sc_ensembles(id),
    sample_index        INTEGER NOT NULL,              -- 0-based position in sampling order
    prompt_tokens       INTEGER NOT NULL DEFAULT 0,
    completion_tokens   INTEGER NOT NULL DEFAULT 0,
    cost_usd            REAL NOT NULL DEFAULT 0.0,
    latency_ms          INTEGER NOT NULL DEFAULT 0,
    text                TEXT NOT NULL,                 -- full completion text
    vote_key            TEXT,                          -- normalised key for majority vote
    embedding_blob      BLOB,                          -- float32 array as raw bytes (optional)
    cluster_id          INTEGER,                       -- agglomerative cluster assignment
    cosine_to_centroid  REAL,                          -- similarity to cluster centroid
    is_winner           INTEGER NOT NULL DEFAULT 0,    -- 1 for the selected sample
    status              TEXT NOT NULL DEFAULT 'ok',    -- 'ok' | 'error' | 'cancelled'
    error_msg           TEXT,
    created_at          TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_scs_ensemble ON sc_samples(ensemble_id, sample_index);
CREATE INDEX IF NOT EXISTS idx_scs_winner   ON sc_samples(ensemble_id, is_winner);

-- Config keys stored in existing tag_config table (no schema change needed):
-- self_consistency.n_samples          INTEGER DEFAULT 1
-- self_consistency.temperature        REAL    DEFAULT 0.7
-- self_consistency.judge_model        TEXT    DEFAULT 'claude-haiku-4-5'
-- self_consistency.consensus_threshold REAL   DEFAULT 0.6
-- self_consistency.embed_model        TEXT    DEFAULT 'all-MiniLM-L6-v2'
```

### 9.2 Core Dataclasses

```python
# src/tag/ensemble.py
from __future__ import annotations

import asyncio
import hashlib
import re
import time
import uuid
from dataclasses import dataclass, field
from typing import Literal

AnswerType = Literal["discrete", "open_ended", "structured"]
VoteMode   = Literal["majority", "embedding", "llm", "auto"]


@dataclass
class SampleResult:
    """Result of a single sampled completion."""
    id: str                         # scs-<uuid4>
    ensemble_id: str
    sample_index: int
    text: str
    vote_key: str | None = None     # normalised key (majority mode)
    prompt_tokens: int = 0
    completion_tokens: int = 0
    cost_usd: float = 0.0
    latency_ms: int = 0
    status: str = "ok"              # 'ok' | 'error' | 'cancelled'
    error_msg: str | None = None
    embedding: list[float] | None = field(default=None, repr=False)
    cluster_id: int | None = None
    cosine_to_centroid: float | None = None
    is_winner: bool = False


@dataclass
class EnsembleResult:
    """Aggregated result of an N-sample ensemble run."""
    id: str                         # ens-<uuid4>
    profile: str
    model_id: str
    prompt_sha256: str
    prompt_preview: str
    n_requested: int
    vote_mode: VoteMode
    answer_type: AnswerType
    samples: list[SampleResult] = field(default_factory=list)
    winner: SampleResult | None = None
    consensus_ratio: float | None = None
    stop_early_triggered: bool = False
    estimated_cost_usd: float = 0.0
    actual_cost_usd: float = 0.0
    status: str = "running"


@dataclass
class EnsembleConfig:
    """Runtime configuration for a single ensemble invocation."""
    n: int = 1
    vote_mode: VoteMode = "auto"
    temperature: float = 0.7
    stop_early: bool = False
    consensus_threshold: float = 0.6
    embed_model: str = "all-MiniLM-L6-v2"
    judge_model: str | None = None
    max_completion_tokens: int = 2048
    dry_run: bool = False
    skip_confirm: bool = False
```

### 9.3 Core Algorithms

#### 9.3.1 `detect_answer_type`

```python
_DISCRETE_PATTERNS = re.compile(
    r"""
    \b(yes|no|true|false|correct|incorrect|vulnerable|safe|pass|fail)\b |
    \b(is\s+this|does\s+this|should\s+i|can\s+you\s+tell\s+me\s+if)\b |
    \?\s*$                    # ends with question mark
    """,
    re.IGNORECASE | re.VERBOSE,
)
_STRUCTURED_PATTERNS = re.compile(
    r"(json|yaml|xml|csv|tool.call|function.call|structured.output)",
    re.IGNORECASE,
)

def detect_answer_type(prompt_text: str) -> AnswerType:
    if _STRUCTURED_PATTERNS.search(prompt_text):
        return "structured"
    if _DISCRETE_PATTERNS.search(prompt_text) and len(prompt_text) < 500:
        return "discrete"
    return "open_ended"
```

#### 9.3.2 `majority_vote`

```python
import collections

_NORMALISE = re.compile(r"[^\w\s]")

def _normalise_key(text: str) -> str:
    """Strip punctuation, collapse whitespace, lowercase."""
    return _NORMALISE.sub("", text).strip().lower()

def majority_vote(samples: list[SampleResult]) -> tuple[SampleResult, float]:
    """
    Return the winning SampleResult and its consensus ratio.
    Ties broken by lowest latency_ms.
    """
    keyed: dict[str, list[SampleResult]] = collections.defaultdict(list)
    for s in samples:
        if s.status == "ok" and s.vote_key:
            keyed[s.vote_key].append(s)

    if not keyed:
        raise ValueError("No valid samples available for majority vote")

    # Sort by frequency desc, then min latency asc (stable tie-break)
    winner_key = max(
        keyed,
        key=lambda k: (len(keyed[k]), -min(s.latency_ms for s in keyed[k])),
    )
    winner_samples = keyed[winner_key]
    winner = min(winner_samples, key=lambda s: s.latency_ms)
    consensus_ratio = len(winner_samples) / len(samples)
    return winner, consensus_ratio
```

#### 9.3.3 `embedding_vote`

```python
import struct

def embedding_vote(
    samples: list[SampleResult],
    model_name: str = "all-MiniLM-L6-v2",
) -> tuple[SampleResult, float]:
    """
    Agglomerative clustering on sentence embeddings.
    Returns the sample nearest the centroid of the largest cluster.
    Requires: sentence-transformers, sklearn
    """
    from sentence_transformers import SentenceTransformer
    from sklearn.cluster import AgglomerativeClustering
    import numpy as np

    valid = [s for s in samples if s.status == "ok"]
    if len(valid) == 1:
        return valid[0], 1.0

    model = SentenceTransformer(model_name)
    texts = [s.text for s in valid]
    embs = model.encode(texts, normalize_embeddings=True)  # shape (N, D)

    # Store embeddings back on samples for persistence
    for s, emb in zip(valid, embs):
        s.embedding = emb.tolist()

    # Determine n_clusters: min(len(valid), 3) unless N < 3
    n_clusters = min(len(valid), max(2, len(valid) // 2))
    clusterer = AgglomerativeClustering(
        n_clusters=n_clusters,
        metric="cosine",
        linkage="average",
    )
    labels = clusterer.fit_predict(embs)

    # Find largest cluster
    counts = collections.Counter(labels)
    largest_label = counts.most_common(1)[0][0]

    # Compute centroid of largest cluster
    cluster_mask = labels == largest_label
    centroid = embs[cluster_mask].mean(axis=0)
    centroid /= np.linalg.norm(centroid) + 1e-9

    # Find sample closest to centroid in that cluster
    best_idx, best_cos = -1, -1.0
    for i, (s, emb, label) in enumerate(zip(valid, embs, labels)):
        s.cluster_id = int(label)
        cos = float(np.dot(emb, centroid))
        s.cosine_to_centroid = cos
        if label == largest_label and cos > best_cos:
            best_cos, best_idx = cos, i

    winner = valid[best_idx]
    consensus_ratio = counts[largest_label] / len(valid)
    return winner, consensus_ratio
```

#### 9.3.4 `llm_vote` (USC meta-call)

```python
USC_META_PROMPT = """\
You are given {n} independent responses to the following prompt:

---
{original_prompt}
---

Responses:
{responses}

Select the response that best answers the prompt. If multiple responses are \
consistent, synthesise them into a single authoritative answer. Output only \
the final answer — do not explain your selection process.
"""

def llm_vote(
    samples: list[SampleResult],
    original_prompt: str,
    judge_model: str,
    hermes_call: callable,
) -> tuple[SampleResult, float]:
    """
    Universal Self-Consistency via a single meta-LLM call.
    Returns a synthetic SampleResult containing the judge's synthesis.
    consensus_ratio is always 1.0 (judge is authoritative by design).
    """
    valid = [s for s in samples if s.status == "ok"]
    responses_block = "\n\n".join(
        f"[Response {i+1}]\n{s.text}" for i, s in enumerate(valid)
    )
    meta_prompt = USC_META_PROMPT.format(
        n=len(valid),
        original_prompt=original_prompt,
        responses=responses_block,
    )
    synthesis_text, usage = hermes_call(meta_prompt, model=judge_model)

    synthesis = SampleResult(
        id=f"scs-{uuid.uuid4()}",
        ensemble_id=valid[0].ensemble_id,
        sample_index=len(valid),   # sentinel index for the meta-call
        text=synthesis_text,
        prompt_tokens=usage.get("prompt_tokens", 0),
        completion_tokens=usage.get("completion_tokens", 0),
        is_winner=True,
        status="ok",
    )
    return synthesis, 1.0
```

#### 9.3.5 Stop-Early Logic

```python
async def run_ensemble_async(
    hermes_call_async: callable,
    prompt: str,
    cfg: EnsembleConfig,
    ensemble_id: str,
) -> list[SampleResult]:
    """
    Fire up to cfg.n samples concurrently. Cancel remaining tasks
    if consensus threshold is reached (stop_early=True).
    """
    tasks: list[asyncio.Task] = []
    results: list[SampleResult] = []
    n_done = 0

    async def _sample_one(index: int) -> SampleResult:
        t0 = time.perf_counter()
        try:
            text, usage = await hermes_call_async(
                prompt,
                temperature=cfg.temperature,
                max_tokens=cfg.max_completion_tokens,
            )
            latency_ms = int((time.perf_counter() - t0) * 1000)
            return SampleResult(
                id=f"scs-{uuid.uuid4()}",
                ensemble_id=ensemble_id,
                sample_index=index,
                text=text,
                vote_key=_normalise_key(text[:100]),  # first 100 chars for discrete
                prompt_tokens=usage.get("prompt_tokens", 0),
                completion_tokens=usage.get("completion_tokens", 0),
                cost_usd=usage.get("cost_usd", 0.0),
                latency_ms=latency_ms,
                status="ok",
            )
        except Exception as exc:
            return SampleResult(
                id=f"scs-{uuid.uuid4()}",
                ensemble_id=ensemble_id,
                sample_index=index,
                text="",
                status="error",
                error_msg=str(exc),
            )

    for i in range(cfg.n):
        tasks.append(asyncio.create_task(_sample_one(i)))

    for future in asyncio.as_completed(tasks):
        result = await future
        results.append(result)
        n_done += 1

        if cfg.stop_early and cfg.vote_mode == "majority":
            ok = [r for r in results if r.status == "ok"]
            if ok:
                from collections import Counter
                counts = Counter(r.vote_key for r in ok)
                top_count = counts.most_common(1)[0][1]
                if top_count / cfg.n >= cfg.consensus_threshold:
                    # Cancel remaining pending tasks
                    for t in tasks:
                        if not t.done():
                            t.cancel()
                    break

    return results
```

### 9.4 New File: `src/tag/ensemble.py`

The new file implements:

- Module-level constants: `MAX_SAMPLES = 40`, `USC_META_PROMPT`, regex patterns
- `detect_answer_type(prompt_text: str) -> AnswerType`
- `compute_preflight_cost(n, model_id, prompt_text, cfg) -> float`
- `normalise_vote_key(text: str) -> str`
- `majority_vote(samples) -> tuple[SampleResult, float]`
- `embedding_vote(samples, model_name) -> tuple[SampleResult, float]`
- `llm_vote(samples, original_prompt, judge_model, hermes_call) -> tuple[SampleResult, float]`
- `run_ensemble_async(hermes_call_async, prompt, cfg, ensemble_id) -> list[SampleResult]`
- `run_ensemble_sync(hermes_call, prompt, cfg, ensemble_id) -> list[SampleResult]` (wraps async in `asyncio.run`)
- `persist_ensemble(conn, ensemble: EnsembleResult) -> None`
- `persist_samples(conn, samples: list[SampleResult]) -> None`
- `cmd_ensemble_list(args, db_path) -> int`
- `cmd_ensemble_show(args, db_path) -> int`
- `cmd_ensemble_export(args, db_path) -> int`
- `cmd_ensemble_stats(args, db_path) -> int`

### 9.5 Integration Points in `controller.py`

```python
# In cmd_submit() and cmd_run(), after argument parsing:
if getattr(args, "samples", 1) > 1:
    from tag.ensemble import (
        EnsembleConfig, detect_answer_type, compute_preflight_cost,
        run_ensemble_sync, majority_vote, embedding_vote, llm_vote,
        persist_ensemble, persist_samples, EnsembleResult,
    )
    # ... ensemble path
else:
    # existing single-call path, unchanged
```

`cmd_ensemble` is wired as:
```python
# In build_parser() / main():
ensemble_parser = subparsers.add_parser("ensemble")
ensemble_sub = ensemble_parser.add_subparsers(dest="ensemble_cmd")
ensemble_sub.add_parser("list")
ensemble_sub.add_parser("show")
ensemble_sub.add_parser("export")
ensemble_sub.add_parser("stats")
```

### 9.6 Config Keys

New keys in the existing `tag_config` table (no schema change, uses existing KV store):

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `self_consistency.n_samples` | int | `1` | Default N when `--samples` is omitted |
| `self_consistency.temperature` | float | `0.7` | Default sampling temperature |
| `self_consistency.judge_model` | str | `claude-haiku-4-5` | Default USC meta-call model |
| `self_consistency.consensus_threshold` | float | `0.6` | Default stop-early threshold |
| `self_consistency.embed_model` | str | `all-MiniLM-L6-v2` | Default embedding model |

Set via: `tag config set self_consistency.n_samples 3`

### 9.7 OTel Semantic Conventions

New attributes added to `otel_semconv.py`:

```python
# Self-consistency ensemble attributes
SC_SAMPLE_INDEX       = "sc.sample_index"        # int: 0-based sample position
SC_VOTE_KEY           = "sc.vote_key"            # str: normalised answer key
SC_VOTE_MODE          = "sc.vote_mode"           # str: 'majority'|'embedding'|'llm'
SC_CONSENSUS_RATIO    = "sc.consensus_ratio"     # float: winner_count / n_sampled
SC_STOP_EARLY         = "sc.stop_early"          # bool: whether --stop-early was set
SC_STOP_EARLY_TRIGGERED = "sc.stop_early_triggered"  # bool: whether it fired
SC_N_REQUESTED        = "sc.n_requested"         # int: N from --samples
SC_N_SAMPLED          = "sc.n_sampled"           # int: actual samples used
SC_ANSWER_TYPE        = "sc.answer_type"         # str: detected answer type
```

---

## 10. Security Considerations

1. **Secret scanning before sampling.** The prompt is passed through `security.scan_for_secrets(prompt_text)` before the pre-flight cost display. If secrets (API keys, tokens, passwords matching known patterns) are found, the command aborts with exit code 1 and prints a generic "secrets detected" message — it does not echo the prompt or the matched secrets. This prevents N copies of a secret-containing prompt from reaching the API.

2. **USC meta-prompt injection.** When building the `USC_META_PROMPT` for `--vote llm`, raw sample texts from N model calls are embedded into a new prompt that is sent to the judge model. A malicious sample could contain prompt-injection instructions targeting the judge. Mitigation: each sample block is enclosed in literal XML-style delimiters (`<response_1>...</response_1>`) and the meta-prompt includes an explicit instruction to ignore formatting or instructions within response blocks.

3. **Embedding model supply-chain trust.** `SentenceTransformer("all-MiniLM-L6-v2")` downloads a model from HuggingFace Hub on first use. In air-gapped or security-hardened environments, this may fail or pull an untrusted model. Mitigation: `--embed-model` accepts a local path; documentation recommends pinning to a model SHA via `HF_HOME` and `TRANSFORMERS_OFFLINE=1`. No code change required, but noted in deployment docs.

4. **SQLite `text` column contains full completion text.** The `sc_samples.text` column stores raw model outputs. If a model completion contains sensitive information (e.g., the model reproduced a secret from its context), that content persists in `~/.tag/runtime/tag.sqlite3`. Mitigation: the same secret-scanning logic runs on each sample's text before writing to SQLite; flagged content is replaced with `[REDACTED:secret]`. This is an opt-in behaviour controlled by `security.redact_on_persist` config key (default: `true`).

5. **Budget bypass via concurrent N calls.** Without the pre-flight budget check (FR-08, FR-09), a user could accidentally exhaust their API budget with a large N. The budget reservation must be atomic: `budget.check_and_reserve()` checks and increments a `reserved_usd` counter in a single SQLite transaction, preventing concurrent `tag submit` calls from each individually clearing the check before the other has reserved its cost.

6. **Pickle-free embedding storage.** Embeddings are stored in `sc_samples.embedding_blob` as raw IEEE 754 float32 bytes (`struct.pack(f"{len(emb)}f", *emb)`), not pickled. This eliminates the pickle RCE vector (referenced in GHSA-mhr3-j7m5-c7c9) that would exist if numpy arrays were pickled to SQLite.

7. **USC judge call content filtering.** The synthesis text returned by `--vote llm` passes through the same output filtering as a normal `tag submit` response. There is no special trust level for judge outputs.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`tests/test_ensemble.py`)

| Test | Description |
|------|-------------|
| `test_detect_answer_type_discrete` | Verifies `detect_answer_type("Is this a SQL injection? Yes or No.")` returns `"discrete"`. |
| `test_detect_answer_type_open_ended` | Verifies `detect_answer_type("Explain the architectural tradeoffs of...")` returns `"open_ended"`. |
| `test_detect_answer_type_structured` | Verifies `detect_answer_type("Return a JSON object with...")` returns `"structured"`. |
| `test_majority_vote_clear_winner` | `["yes", "yes", "no"]` → winner is `"yes"`, consensus_ratio = 0.667. |
| `test_majority_vote_tie_broken_by_latency` | Two `"yes"` samples (latency 500 ms, 200 ms) and two `"no"` samples → tie resolved by 200 ms `"yes"` sample winning. |
| `test_majority_vote_no_valid_samples` | All samples `status='error'` → `ValueError` raised. |
| `test_normalise_key` | `"Yes! SQL injection."` → `"yes sql injection"`. |
| `test_embedding_vote_single_sample` | N=1 valid sample → returns it with consensus_ratio=1.0, no clustering attempted. |
| `test_embedding_vote_largest_cluster` | Synthetic embeddings with two clear clusters; largest cluster winner is selected. Requires `sentence-transformers` and `sklearn`. Marked `pytest.mark.optional`. |
| `test_llm_vote_meta_prompt_construction` | Mock `hermes_call`; assert the prompt passed to it contains all 3 sample texts wrapped in delimiters. |
| `test_stop_early_cancels_futures` | N=5, consensus reached at K=3; assert remaining 2 tasks have `cancelled()=True`. |
| `test_compute_preflight_cost` | N=5, 100 prompt tokens, known rate → assert estimate within 1 % of manual calculation. |
| `test_embedding_fallback_no_st` | Monkeypatch `_ST_AVAILABLE=False`; assert `embedding_vote` raises `ImportError` caught by caller, which falls back to `majority_vote`. |
| `test_temperature_zero_override` | `EnsembleConfig(temperature=0.0, n=3)` → `run_ensemble_sync` overrides temperature to 0.1. |
| `test_samples_max_exceeded` | `--samples 41` → `ValueError` with reference to N=40 cap. |
| `test_secret_scan_aborts` | Monkeypatch `security.scan_for_secrets` to return detections; assert `cmd_submit` exits with code 1 before any API call. |
| `test_sqlite_schema_creation` | `ensure_schema(conn)` on empty DB → both `sc_ensembles` and `sc_samples` tables exist with correct columns. |
| `test_persist_ensemble_and_samples` | Full `EnsembleResult` round-trip: persist → read back → assert all fields match. |
| `test_embedding_blob_no_pickle` | `embedding_blob` stored as raw bytes; assert `struct.unpack` recovers original values; assert no `pickle` import in `ensemble.py`. |

### 11.2 Integration Tests (`tests/integration/test_ensemble_integration.py`)

| Test | Description |
|------|-------------|
| `test_end_to_end_majority_N3` | Run `cmd_submit` with `--samples 3 --vote majority` against a mocked Hermes bridge returning deterministic responses; assert winner text, consensus ratio, and SQLite rows. |
| `test_stop_early_integration` | Mock Hermes to return `"yes"` for all samples; assert only 2 samples fired (first consensus at 2/3 > 0.6), third future cancelled. |
| `test_dry_run_no_api_call` | `--dry-run --samples 5` → assert Hermes mock call count = 0, exit code = 0. |
| `test_ensemble_list_cmd` | Seed two ensemble rows in SQLite; run `cmd_ensemble list`; assert both appear in output. |
| `test_ensemble_show_cmd` | Seed ensemble + 3 samples; run `cmd_ensemble show <id>`; assert all 3 samples visible. |
| `test_budget_enforcement` | Mock `budget.get_remaining()` = 0; assert command exits code 1 with budget-exceeded message. |
| `test_span_count` | After a 3-sample ensemble, assert `tag trace show` returns 4 spans (1 parent + 3 children). |
| `test_json_output_schema` | `--json` output parses as valid JSON; assert all required keys present (`ensemble_id`, `winner`, `consensus_ratio`, `samples`, `total_cost_usd`). |

### 11.3 Performance Tests (`tests/perf/test_ensemble_perf.py`)

| Test | Description |
|------|-------------|
| `test_sqlite_write_N40` | Insert 40 sample rows in a single transaction; assert total wall time < 10 ms on SSD. |
| `test_embedding_encode_N40` | Encode 40 × 200-token texts with MiniLM; assert total time < 2 s. |
| `test_ensemble_overhead_N1` | `--samples 1` wall time is within 5 % of baseline single-call path (20 run average). |
| `test_cancel_latency` | Stop-early cancel of 4 remaining asyncio tasks completes within 5 ms. |

---

## 12. Acceptance Criteria

| ID | Criterion | How to Verify |
|----|-----------|--------------|
| AC-01 | `tag submit --samples 3 --vote majority --prompt "Is X vulnerable? Yes or No."` with 2/3 responses returning "yes" returns `"yes"` as the winner. | Integration test `test_end_to_end_majority_N3` |
| AC-02 | `--stop-early` with N=5 and threshold=0.6 fires at most 4 samples when 3 agree on the same answer (3/5 ≥ 0.6 = 0.6). | Integration test `test_stop_early_integration` |
| AC-03 | `--dry-run` exits with code 0 and zero API calls for any N. | Integration test `test_dry_run_no_api_call` |
| AC-04 | Both `sc_ensembles` and `sc_samples` tables are created by `ensure_schema()` on first run with correct column names and indexes. | Unit test `test_sqlite_schema_creation` |
| AC-05 | `--samples 1` (default) produces no import of `tag.ensemble` and no additional SQLite writes vs. baseline. | Unit test asserting `sys.modules` excludes `tag.ensemble` after single-call `cmd_submit` |
| AC-06 | `--samples 41` exits with code 1 and a message mentioning the 40-sample cap. | Unit test `test_samples_max_exceeded` |
| AC-07 | A prompt containing a mock API key pattern is blocked by `security.scan_for_secrets` before any API call is made. | Unit test `test_secret_scan_aborts` |
| AC-08 | `--vote embedding` with `sentence-transformers` not installed falls back to `majority` (discrete) or `llm` (open-ended) and emits a warning. | Unit test `test_embedding_fallback_no_st` |
| AC-09 | `--json` output is valid JSON containing `ensemble_id`, `n_requested`, `n_sampled`, `consensus_ratio`, `winner`, `samples` array, and `total_cost_usd`. | Integration test `test_json_output_schema` |
| AC-10 | `tag ensemble list` shows the ensemble run after a successful `tag submit --samples 3` call. | Integration test `test_ensemble_list_cmd` |
| AC-11 | `tag ensemble show <id>` displays all 3 sample texts (truncated) with their vote keys and costs. | Integration test `test_ensemble_show_cmd` |
| AC-12 | Budget enforcement blocks a `--samples 10` run when remaining budget is $0.00. | Integration test `test_budget_enforcement` |
| AC-13 | Tracing produces N+1 spans (1 `ensemble` parent + N child sample spans) visible in `tag trace show`. | Integration test `test_span_count` |
| AC-14 | Embedding blobs are stored as raw float32 bytes, not pickle; `struct.unpack` recovers original values. | Unit test `test_embedding_blob_no_pickle` |
| AC-15 | Pre-flight cost estimate is printed before any API call and within ±15 % of actual spend. | Integration test with mock usage data |
| AC-16 | `--samples-temperature 0` is overridden to 0.1 with a printed warning. | Unit test `test_temperature_zero_override` |
| AC-17 | `--vote llm` meta-prompt wraps each sample in `<response_N>` delimiters. | Unit test `test_llm_vote_meta_prompt_construction` |
| AC-18 | `tag ensemble export <id> --output samples.jsonl` writes valid JSONL with one object per sample. | Integration test reading exported file |

---

## 13. Dependencies

| Dependency | Type | Required? | Version | Notes |
|------------|------|-----------|---------|-------|
| `sentence-transformers` | Python package | Optional | `>=2.2.0` | Required for `--vote embedding`. Graceful fallback if absent. |
| `scikit-learn` | Python package | Optional | `>=1.3.0` | `AgglomerativeClustering` for embedding mode. Pulled in by `sentence-transformers` transitively in most installs. |
| `numpy` | Python package | Optional | `>=1.24.0` | Centroid computation. Pulled in transitively. |
| `asyncio` | stdlib | Required | Python 3.11+ | Already required by TAG. |
| `budget.py` | Internal module | Required | PRD-012 | `check_and_reserve()`, cost rate lookup. |
| `tracing.py` | Internal module | Required | PRD-013 | `open_span()`, `close_span()`. |
| `security.py` | Internal module | Required | PRD-034 | `scan_for_secrets()`, secret redaction. |
| `eval_framework.py` | Internal module | Optional | PRD-027 | For `tag eval run --samples N` integration. |
| `tool_retrieval.py` | Internal module | Optional | PRD-043 | Shares `SentenceTransformer` instance; model cache should be shared to avoid double-load. |
| `otel_semconv.py` | Internal module | Required | PRD-041 | New `sc.*` attribute constants. |
| `hermes_bridge.py` | Internal module | Required | — | `hermes_call()` / `hermes_call_async()`. |
| GitHub Issue #349 | Tracker | — | — | Feature request tracking. |

---

## 14. Open Questions

| # | Question | Owner | Resolution Target |
|---|----------|-------|-------------------|
| OQ-01 | Should `--vote embedding` share the `SentenceTransformer` model instance with `tool_retrieval.py` (PRD-043) to avoid loading the model twice in the same process? The models may differ (`all-MiniLM-L6-v2` vs. whatever tool retrieval uses). | Implementer | Before implementation — check `tool_retrieval.EMBED_MODEL_NAME` and decide on a shared model cache in a new `embed_cache.py` helper or in `tool_retrieval.py`. |
| OQ-02 | Should `sc_samples.text` be encrypted at rest (e.g., using a key stored in the system keychain) given that it contains full model outputs that may include sensitive context? | Security | Before merge — current plan is secret redaction (§10 point 4); full encryption may be added via PRD-034 extension. |
| OQ-03 | For `--vote embedding` on structured JSON outputs, should clustering operate on the raw JSON string or on a normalised form (key-sorted, whitespace-collapsed)? Raw strings will produce spurious distance from formatting differences. | Implementer | Implement normalisation for `"structured"` answer type: `json.dumps(json.loads(text), sort_keys=True)` before encoding. |
| OQ-04 | What is the correct behaviour when all N samples fail (all `status='error'`)? Currently `majority_vote` raises `ValueError`. Should it instead fall back to a single retry with the standard call path? | Product | Decide before implementation. Current proposal: raise `ValueError`; let `cmd_submit` catch it and retry once with N=1. |
| OQ-05 | Should `tag ensemble export` support a `--winner-only` flag to export only the winning samples across all ensembles for a profile, producing a cleaner fine-tuning dataset? | Product | Post-MVP; add to backlog as enhancement to FR-15. |
| OQ-06 | The `USC_META_PROMPT` uses model-agnostic wording. Should there be judge-model-specific prompt templates (Claude vs. GPT-4o system prompt conventions differ)? | Implementer | Start with a single template; refine based on empirical quality testing. |
| OQ-07 | For the `--vote majority` tie-break (lowest latency), is latency a good proxy? On some API providers, latency correlates with token count, meaning shorter (potentially incomplete) answers win ties. Alternative: prefer the sample with the highest completion token count in a tie. | Implementer | Expose `--tie-break {latency, length}` in a follow-up. Default to `latency` for now per PRD spec. |

---

## 15. Complexity and Timeline

**Overall estimate:** S (3–5 days) — Difficulty 2/5

### Phase 1 — Schema and Core Dataclasses (Day 1)

- Create `src/tag/ensemble.py` with all dataclasses (`SampleResult`, `EnsembleResult`, `EnsembleConfig`).
- Write `ensure_schema()` with `sc_ensembles` and `sc_samples` DDL.
- Write `persist_ensemble()` and `persist_samples()`.
- Write `detect_answer_type()`.
- Write `compute_preflight_cost()`.
- Add new `sc.*` constants to `otel_semconv.py`.
- Unit tests: schema, dataclasses, `detect_answer_type`, `compute_preflight_cost`.

### Phase 2 — Aggregation Algorithms (Day 2)

- Implement `majority_vote()` with normalisation and tie-break.
- Implement `embedding_vote()` with agglomerative clustering and graceful ImportError fallback.
- Implement `llm_vote()` with USC meta-prompt and delimiter wrapping.
- Implement `run_ensemble_async()` with stop-early cancellation.
- Implement `run_ensemble_sync()` wrapper.
- Unit tests: all aggregation functions, stop-early cancellation, temperature override, max-N guard.

### Phase 3 — Controller Integration (Day 3)

- Wire `--samples`, `--vote`, `--stop-early`, `--consensus-threshold`, `--embed-model`, `--judge-model`, `--samples-temperature`, `--dry-run`, `--yes` flags onto `tag submit` and `tag run` argument parsers in `controller.py`.
- Deferred import of `tag.ensemble` behind `if args.samples > 1`.
- Pre-flight secret scan, cost estimate, budget reservation.
- Tracing span instrumentation (parent + child spans).
- `--json` output serialisation.
- Integration tests: end-to-end majority, stop-early, dry-run, budget enforcement, JSON output schema.

### Phase 4 — `tag ensemble` Subcommands (Day 4)

- Implement `cmd_ensemble_list`, `cmd_ensemble_show`, `cmd_ensemble_export`, `cmd_ensemble_stats`.
- Wire into controller subparser.
- Human-readable table rendering (Rich table via `tui_output.py`).
- Integration tests: list, show, export JSONL.

### Phase 5 — Hardening and Docs (Day 5)

- Security: secret redaction on `sc_samples.text` write path.
- Performance tests: SQLite write N=40, embedding encode N=40, overhead N=1.
- Config key documentation in `tag config list` output.
- OQ-03 resolution: JSON normalisation for structured mode.
- Final acceptance criteria verification sweep.
- Update `docs/prd/INDEX.md` with PRD-101 entry.

---

*End of PRD-101*

