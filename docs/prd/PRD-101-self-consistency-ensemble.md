# PRD-101: Self-Consistency Ensemble: Sample N, Majority-Vote (`tag submit --samples N --vote majority`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** S (3-5 days)
**Category:** Advanced Reasoning & Planning
**Affects:** `internal/agent/selfconsistency` (new package), `internal/cli` (flag wiring on `submit`/`run`), `internal/store` (new tables + Go migrations)
**Depends on:** PRD-027 (eval framework — quality scoring), PRD-028 (sandbox — isolated code execution per sample), PRD-013 (agent tracing — per-sample span attribution), PRD-034 (secret scanning — prompt content before sampling), PRD-012 (budget enforcement — N×cost guard), PRD-043 (vector tool retrieval — embedding-space aggregation), PRD-041 (OTel span cost attribution — per-sample cost tags), PRD-045 (LLM-as-judge — USC meta-judge path)
**Inspired by:** Self-consistency prompting (Wang et al. 2022), EMS paper 2025, multi-agent voting

---

## 1. Overview

Quality of LLM outputs is not deterministic. Given the same prompt, a model sampling at non-zero temperature produces different reasoning chains on every call — some leading to correct conclusions, others to plausible-but-wrong ones. The standard TAG workflow calls the model once and returns whatever it produces. This single-sample strategy is fast and cheap, but it means every run is exposed to the full variance of the model's distribution: one unlucky sampling event can produce a confidently wrong answer, a subtly broken code patch, or a security review that misses the critical finding.

Self-consistency, introduced by Wang et al. (2022), addresses this by sampling N independent completions from the same prompt (with temperature > 0 to enforce diversity), then selecting the final answer by majority vote over the discrete outputs — effectively marginalising out the intermediate reasoning chains. Empirically, N=10 raises GSM8K accuracy from 56.5 % to 74.4 % with no prompt engineering; N=40 reaches 74.4 %. The technique is model-agnostic and requires no fine-tuning or external judge. For code review and security audits — domains where a missed vulnerability has asymmetric cost — sampling multiple independent reasoning paths and requiring consensus substantially reduces the false-negative rate.

TAG must handle three answer types: discrete/closed-form (yes/no, vulnerability class labels, tool-call decisions), open-ended prose (code explanations, review summaries), and structured JSON (tool call arguments, diff patches). This PRD specifies a three-mode aggregation stack: (a) hard majority vote for discrete answers, (b) embedding-space centroid clustering via `sentence-transformers` (USC-embedding) for open-ended prose without an extra LLM call, and (c) LLM-as-judge meta-call (USC-LLM) for open-ended prose when an authoritative synthesis is preferred over the nearest-centroid. Mode selection is automatic based on answer-type detection with a manual override flag.

The feature integrates with existing TAG infrastructure at every layer: `internal/obs` (budget) gates total N×cost before the first sample fires; `internal/obs` tracing emits one child span per sample under a parent `ensemble` span; `internal/eval` treats ensemble runs as a first-class evaluation variant; `internal/security` scans the prompt before sampling to prevent secret leakage into N parallel API calls; and the `sc_samples` SQLite table stores all raw samples for debugging, cost attribution, and future fine-tuning datasets.

Early stopping (`--stop-early`) aborts remaining samples as soon as a consensus threshold is reached, reducing cost on easy queries. The stop-early check runs after each batch of concurrent samples and terminates the remaining futures before any network call is made.

---

## 2. Problem Statement

### 2.1 Single-Sample Variance Causes Silent Failures in High-Stakes Tasks

TAG's primary use cases — security review, code refactoring, and architecture audits — have asymmetric error costs. A false negative on a SQL injection check is not a minor inconvenience; it is a production vulnerability. The current `tag submit` dispatches exactly one inference call and returns its output. If the model's temperature is above zero (as it is for all non-deterministic profiles), there is no mechanism to detect when the returned answer is an outlier within the model's own distribution. Users have no signal indicating whether the output is robust (the model would answer identically on 9 of 10 draws) or fragile (this was a lucky 1-in-10 correct answer). High-stakes single-call outputs are therefore systematically under-trusted by experienced users and over-trusted by novices — neither outcome is acceptable.

### 2.2 No Structured Mechanism for Answer Confidence Beyond Token Probabilities

Token log-probabilities are unavailable or unreliable as confidence signals for multi-step reasoning tasks. They reflect single-token prediction confidence, not the confidence of a multi-step conclusion. TAG currently has no mechanism to estimate answer confidence at the semantic level. The `internal/eval` framework (PRD-027) provides quality scoring after the fact using an LLM judge, but this is expensive ($0.01–$0.05 per eval call) and requires manual invocation. Self-consistency provides a cheap, automatic confidence proxy — the fraction of N samples that agree on the winning answer — without any judge model call for the majority-vote path.

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
| Stop-early cancellation | When consensus reached at sample K < N, remaining N-K goroutines are cancelled before any network bytes sent | Fake `Provider` at the transport layer; assert call count = K |
| Cost gate accuracy | Displayed pre-flight cost estimate within ±15 % of actual spend | Compare estimate vs. `sc_samples.prompt_tokens + completion_tokens` sum after run |
| SQLite persistence | All N samples written to `sc_samples` within 100 ms of ensemble completion | Assert in integration test; measure with `time.Since` |
| Span attribution | `ensemble` parent span + N child spans visible in `tag trace show` | Integration test asserting span count = N+1 |
| Budget enforcement | `tag submit --samples 10` blocked if N×estimated_cost > active budget cap | Unit test with a fake budget reporting remaining = 0 |
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
| FR-01 | When `--samples 1` (or `--samples` is absent), the `internal/agent/selfconsistency` package is never entered — the CLI dispatches straight to the current single-call path with zero overhead. | Must |
| FR-02 | When N > 1, the system fans out N concurrent completions as goroutines over the `internal/llm` provider `Stream(ctx, Request) -> <-chan Event` interface, coordinated by an `golang.org/x/sync/errgroup.Group` with bounded concurrency (`errgroup.SetLimit`), each goroutine carrying an independent random seed derived from a fresh `uuid.NewString()`. Per-sample results are collected over a channel. | Must |
| FR-03 | `majority_vote(samples)` normalises each sample's final answer by stripping punctuation and lowercasing, counts frequencies, and returns the modal answer plus its count and the total N. In the case of a tie, the sample with the lowest `latency_ms` among the tied answers is preferred. | Must |
| FR-04 | `EmbeddingVote(samples, model)` encodes all N sample texts through the `internal/memory/embed` `Embedder` interface (provider embedding API by default; build-tag offline MiniLM), runs agglomerative clustering (average-linkage over cosine distance, ported as plain Go slices/loops), identifies the largest cluster, computes the centroid, and returns the sample with maximum cosine similarity to the centroid. | Must |
| FR-05 | `LLMVote(samples, judgeModel, profile)` constructs a USC meta-prompt listing all N samples and calls the judge model once through the `internal/llm` provider interface, returning its synthesis. The meta-prompt template is a package-level `const USCMetaPrompt` in `internal/agent/selfconsistency`. | Must |
| FR-06 | `DetectAnswerType(promptText)` returns `AnswerDiscrete` if the prompt contains decision-requesting patterns (RE2 regexp: `\b(yes|no|true|false|correct|incorrect|vulnerable|safe)\b` or ends with `?` and is < 200 chars), otherwise returns `AnswerOpenEnded`. This drives auto-mode selection. | Must |
| FR-07 | `--stop-early` checks consensus after each sample completes (not in a batch). If `winningCount / nSampled >= consensusThreshold`, the shared `context.Context` passed to the errgroup is cancelled, aborting all in-flight goroutines and preventing any not-yet-dispatched provider request. | Must |
| FR-08 | Before the first API call, `ComputePreflightCost(n, modelID, promptTokens)` estimates total cost as `n × (promptTokens × promptRate + maxCompletionTokens × completionRate)` using the `internal/obs` pricing table. If `--yes` is not set and the `CI` env var is not `"true"`, the user is prompted for confirmation. | Must |
| FR-09 | Budget enforcement calls `obs.Budget.CheckAndReserve(profile, estimatedCost)` before sampling. If the budget would be exceeded, the command returns a non-nil error, exits with code 1, and prints a human-readable message showing remaining budget. | Must |
| FR-10 | Every sample is written to `sc_samples` (see SQLite DDL in §9.1) within the same `internal/store` transaction (`*sql.Tx`) as the ensemble row, via an upsert (`INSERT ... ON CONFLICT DO UPDATE`). | Must |
| FR-11 | Tracing: an `ensemble` parent span is started via `otel` (`tracer.Start(ctx, ...)`) at the start; each sample opens a child span with attributes `sc.sample_index`, `sc.vote_key` (normalised answer for majority mode), `sc.prompt_tokens`, `sc.completion_tokens`; the parent span is ended with `sc.vote_winner` and `sc.consensus_ratio` attributes. | Must |
| FR-12 | Secret scanning (PRD-034): `security.ScanForSecrets(promptText)` is called before the pre-flight cost display. If secrets are detected, the command returns an error, exits with code 1, and does not display the prompt content in the error message. | Must |
| FR-13 | `tag ensemble list` reads from `sc_ensembles` ordered by `created_at DESC`, with `--last N` (default 20), `--profile NAME` filter, and `--json` flag. | Should |
| FR-14 | `tag ensemble show <ensemble_id>` reads from `sc_samples` for that ensemble, rendering each sample's text (truncated to 200 chars in human mode), vote key, latency, and cost. `--sample-index K` prints the full text for sample K. | Should |
| FR-15 | `tag ensemble export <ensemble_id> --output FILE` writes a JSONL file with one object per sample, formatted as `{"prompt": "...", "completion": "...", "vote_key": "...", "is_winner": true/false}`, suitable for SFT fine-tuning datasets. | Could |
| FR-16 | When `--vote embedding` is used and no `Embedder` is available (offline with no provider embedding endpoint reachable and the offline-MiniLM build tag absent), the command falls back to `majority` mode (if discrete) or `llm` mode (if open-ended), emitting a warning with configuration guidance. | Must |
| FR-17 | The `--samples-temperature` value must be > 0 when N > 1; if the user sets `--samples-temperature 0`, the CLI emits a warning and overrides to 0.1 to preserve diversity. | Must |
| FR-18 | The `--judge-model` flag defaults to the value of `self_consistency.judge_model` in the TAG config, then to `claude-haiku-4-5` (cheapest capable model), then to the profile's default model. | Should |
| FR-19 | `tag ensemble stats --profile NAME --last N` computes mean consensus ratio, mean samples used, mean cost per ensemble, and p50/p95 latency across the last N ensemble runs for a given profile. | Could |
| FR-20 | `--dry-run` displays the cost estimate, the detected answer type, the selected vote mode, and exits with code 0 without making any API call. | Must |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | **Latency (wall time, N samples)** | Wall time for N goroutine-fanned samples ≤ 1.2× the single-sample wall time for N ≤ 5 on the same model, assuming sufficient API concurrency (errgroup limit ≥ N). |
| NFR-02 | **Memory footprint** | Embedding matrix for N=40 samples × 384-dim (MiniLM) = 61 KB held as `[]float32`; total peak memory increase from the selfconsistency package < 50 MB including any offline model load (offline model lazily initialised once behind a `sync.Once` and cached across calls). |
| NFR-03 | **SQLite write performance** | All N sample rows inserted in a single `modernc.org/sqlite` transaction; total SQLite write overhead < 10 ms for N ≤ 40 on SSD (allow headroom for modernc's ~2× query cost vs CGO drivers). |
| NFR-04 | **Graceful degradation** | If any single sample's provider call fails with a retriable error (5xx, timeout), that sample is retried once with 1 s backoff (`cenkalti/backoff/v4` at orchestration level). If it fails again, it is recorded as `status='error'` in `sc_samples` and excluded from voting. Voting proceeds on the remaining samples if at least ⌈N/2⌉ succeed. |
| NFR-05 | **Cancellation correctness** | When stop-early triggers, cancelling the shared `context.Context` must propagate to every in-flight goroutine so the provider `Stream` channels are drained/closed and no HTTP connection is leaked; `errgroup.Wait()` must return after all goroutines observe cancellation. |
| NFR-06 | **Reproducibility** | Given `--samples-temperature 0` (overridden to 0.1 per FR-17), the majority-vote winner across runs for the same prompt should be stable (>= 80 % identical across 5 independent runs in CI). Not guaranteed but targeted. |
| NFR-07 | **Security** | The USC meta-prompt for `--vote llm` must not include secret-scanned tokens detected by `internal/security`; raw sample texts passed to the judge are filtered through the same secret-masking logic. |
| NFR-08 | **Cost ceiling** | A hard-coded maximum N of 40 (matching Wang et al. 2022's largest experiment) is enforced; `--samples > 40` returns an error with a message referencing the paper's diminishing-returns finding. |
| NFR-09 | **Path isolation** | The `internal/agent/selfconsistency` package must not be entered when `--samples 1` (or absent); the CLI branches on `samples > 1` in the `submit`/`run` cobra handlers before any selfconsistency call, so its cost/tracing/DDL machinery is inert on the single-call path. |
| NFR-10 | **OTel compatibility** | All ensemble spans conform to TAG's OTel semantic conventions (PRD-041); `sc.sample_index` is an `attribute.Int` value; `sc.consensus_ratio` is an `attribute.Float64` value; both constants live in `internal/obs` alongside the hardcoded `gen_ai.*` table and pinned `SEMCONV_VERSION`. |

---

## 9. Technical Design

### 9.1 SQLite DDL

Persistence uses `modernc.org/sqlite` (pure-Go, CGO_ENABLED=0), the project-wide driver. The DDL below is unchanged SQL, applied as idempotent Go migrations under `internal/store/migrate/` (`CREATE TABLE IF NOT EXISTS` / `CREATE INDEX IF NOT EXISTS`). All tables use WAL-mode inherited from the single `internal/store` connection; foreign-key enforcement is set via `PRAGMA foreign_keys=ON` per connection. Writes route through the single-writer store handle (flock + `os.Rename` atomic RMW discipline).

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

### 9.2 Core Structs

```go
// internal/agent/selfconsistency/types.go
package selfconsistency

// AnswerType is a typed string constant (replaces the Python Literal).
type AnswerType string

const (
	AnswerDiscrete  AnswerType = "discrete"
	AnswerOpenEnded AnswerType = "open_ended"
	AnswerStructured AnswerType = "structured"
)

// VoteMode is a typed string constant.
type VoteMode string

const (
	VoteMajority  VoteMode = "majority"
	VoteEmbedding VoteMode = "embedding"
	VoteLLM       VoteMode = "llm"
	VoteAuto      VoteMode = "auto"
)

// SampleResult is the result of a single sampled completion.
type SampleResult struct {
	ID               string    // scs-<uuid>
	EnsembleID       string
	SampleIndex      int
	Text             string
	VoteKey          string    // normalised key (majority mode); "" if unset
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
	LatencyMS        int
	Status           string    // "ok" | "error" | "cancelled"
	ErrorMsg         string
	Embedding        []float32 // nil unless embedding mode
	ClusterID        int       // -1 if unassigned
	CosineToCentroid float64
	IsWinner         bool
}

// EnsembleResult is the aggregated result of an N-sample ensemble run.
type EnsembleResult struct {
	ID                 string   // ens-<uuid>
	Profile            string
	ModelID            string
	PromptSHA256       string
	PromptPreview      string
	NRequested         int
	VoteMode           VoteMode
	AnswerType         AnswerType
	Samples            []SampleResult
	Winner             *SampleResult
	ConsensusRatio     float64  // valid once voting completes
	StopEarlyTriggered bool
	EstimatedCostUSD   float64
	ActualCostUSD      float64
	Status             string   // "running" | "done" | "error"
}

// Config is the runtime configuration for a single ensemble invocation.
type Config struct {
	N                   int
	VoteMode            VoteMode
	Temperature         float64
	StopEarly           bool
	ConsensusThreshold  float64
	EmbedModel          string
	JudgeModel          string
	MaxCompletionTokens int
	DryRun              bool
	SkipConfirm         bool
}

// DefaultConfig mirrors the Python dataclass defaults.
func DefaultConfig() Config {
	return Config{
		N:                   1,
		VoteMode:            VoteAuto,
		Temperature:         0.7,
		ConsensusThreshold:  0.6,
		EmbedModel:          "all-MiniLM-L6-v2",
		MaxCompletionTokens: 2048,
	}
}
```

### 9.3 Core Algorithms

#### 9.3.1 `DetectAnswerType`

Go's `regexp` (RE2) has no verbose/inline-comment mode, so the patterns are written as plain case-insensitive alternations and compiled once at package init via `regexp.MustCompile`.

```go
var (
	discretePatterns = regexp.MustCompile(`(?i)\b(yes|no|true|false|correct|incorrect|vulnerable|safe|pass|fail)\b|\b(is\s+this|does\s+this|should\s+i|can\s+you\s+tell\s+me\s+if)\b|\?\s*$`)
	structuredPatterns = regexp.MustCompile(`(?i)(json|yaml|xml|csv|tool.call|function.call|structured.output)`)
)

func DetectAnswerType(promptText string) AnswerType {
	if structuredPatterns.MatchString(promptText) {
		return AnswerStructured
	}
	if discretePatterns.MatchString(promptText) && len(promptText) < 500 {
		return AnswerDiscrete
	}
	return AnswerOpenEnded
}
```

#### 9.3.2 `MajorityVote`

Voting is plain arithmetic ported 1:1 — a `map[string][]SampleResult` bucket, frequency count, and a lowest-latency tie-break. Errors are returned, not raised.

```go
var normalisePattern = regexp.MustCompile(`[^\w\s]`)

func normaliseKey(text string) string {
	return strings.ToLower(strings.TrimSpace(normalisePattern.ReplaceAllString(text, "")))
}

// MajorityVote returns the winning sample and its consensus ratio.
// Ties are broken by lowest LatencyMS.
func MajorityVote(samples []SampleResult) (SampleResult, float64, error) {
	keyed := map[string][]SampleResult{}
	for _, s := range samples {
		if s.Status == "ok" && s.VoteKey != "" {
			keyed[s.VoteKey] = append(keyed[s.VoteKey], s)
		}
	}
	if len(keyed) == 0 {
		return SampleResult{}, 0, errors.New("no valid samples available for majority vote")
	}

	// Pick the key with the highest frequency; break ties by lowest min latency.
	var winnerKey string
	var best []SampleResult
	for k, group := range keyed {
		if best == nil ||
			len(group) > len(best) ||
			(len(group) == len(best) && minLatency(group) < minLatency(best)) {
			winnerKey, best = k, group
		}
	}
	_ = winnerKey

	winner := best[0]
	for _, s := range best[1:] {
		if s.LatencyMS < winner.LatencyMS {
			winner = s
		}
	}
	consensus := float64(len(best)) / float64(len(samples))
	return winner, consensus, nil
}

func minLatency(group []SampleResult) int {
	m := group[0].LatencyMS
	for _, s := range group[1:] {
		if s.LatencyMS < m {
			m = s.LatencyMS
		}
	}
	return m
}
```

#### 9.3.3 `EmbeddingVote`

There is no numpy/sklearn in Go, but none is needed: at N ≤ 40 the clustering is plain arithmetic over `[]float32` slices — normalised embeddings, average-linkage agglomerative clustering, centroid, and cosine — all ported 1:1 as loops (no `gonum` required; it would only be justified for real linear algebra at large scale). Encoding goes through the `internal/memory/embed.Embedder` interface (provider embedding API by default; build-tag offline MiniLM), so the reasoning layer never imports an ML SDK directly.

```go
// Embedder is satisfied by internal/memory/embed (provider default / offline MiniLM).
type Embedder interface {
	Encode(ctx context.Context, texts []string) ([][]float32, error) // rows L2-normalised
}

// EmbeddingVote clusters sample embeddings and returns the sample nearest the
// centroid of the largest cluster, plus its consensus ratio.
func EmbeddingVote(ctx context.Context, samples []SampleResult, emb Embedder) (SampleResult, float64, error) {
	valid := valid(samples)
	if len(valid) == 1 {
		return valid[0], 1.0, nil
	}

	texts := make([]string, len(valid))
	for i, s := range valid {
		texts[i] = s.Text
	}
	embs, err := emb.Encode(ctx, texts) // shape (N, D), rows normalised
	if err != nil {
		return SampleResult{}, 0, err
	}
	for i := range valid {
		valid[i].Embedding = embs[i] // persist back on the sample
	}

	// n_clusters := min(len, max(2, len/2)) — same heuristic as the original.
	nClusters := min(len(valid), max(2, len(valid)/2))
	labels := agglomerativeCosine(embs, nClusters) // average-linkage, plain loops

	// Largest cluster.
	counts := map[int]int{}
	for _, l := range labels {
		counts[l]++
	}
	largest, largestN := -1, -1
	for l, c := range counts {
		if c > largestN {
			largest, largestN = l, c
		}
	}

	// Centroid of the largest cluster (mean of member rows), then normalise.
	dim := len(embs[0])
	centroid := make([]float32, dim)
	for i, l := range labels {
		if l == largest {
			for d := 0; d < dim; d++ {
				centroid[d] += embs[i][d]
			}
		}
	}
	var norm float64
	for d := 0; d < dim; d++ {
		centroid[d] /= float32(largestN)
		norm += float64(centroid[d]) * float64(centroid[d])
	}
	inv := float32(1.0 / (math.Sqrt(norm) + 1e-9))
	for d := 0; d < dim; d++ {
		centroid[d] *= inv
	}

	// Sample in the largest cluster closest to the centroid.
	bestIdx, bestCos := -1, -1.0
	for i := range valid {
		valid[i].ClusterID = labels[i]
		cos := dot(embs[i], centroid)
		valid[i].CosineToCentroid = cos
		if labels[i] == largest && cos > bestCos {
			bestCos, bestIdx = cos, i
		}
	}

	consensus := float64(largestN) / float64(len(valid))
	return valid[bestIdx], consensus, nil
}

func dot(a, b []float32) float64 {
	var s float64
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}
```

`agglomerativeCosine` is a ~40-line bottom-up merge (each point starts as its own cluster; repeatedly merge the two clusters with the highest average pairwise cosine until `nClusters` remain) — arithmetic only, deterministic given the input embeddings.

#### 9.3.4 `LLMVote` (USC meta-call)

The meta-prompt is a package-level `const` and Go's `text/template` (or `fmt.Sprintf`) fills it. The judge model is invoked through the `internal/llm` provider `Stream` interface — never a provider SDK directly — and the accumulated text + `Usage` event give tokens.

```go
const USCMetaPrompt = `You are given %d independent responses to the following prompt:

---
%s
---

Responses:
%s

Select the response that best answers the prompt. If multiple responses are consistent, synthesise them into a single authoritative answer. Output only the final answer — do not explain your selection process.
`

// Provider is the reasoning layer's view of internal/llm.
type Provider interface {
	Stream(ctx context.Context, req llm.Request) (<-chan llm.Event, error)
}

// LLMVote performs Universal Self-Consistency via a single meta-LLM call.
// Returns a synthetic SampleResult holding the judge's synthesis; consensus is
// always 1.0 (the judge is authoritative by design).
func LLMVote(ctx context.Context, samples []SampleResult, originalPrompt, judgeModel string, p Provider) (SampleResult, float64, error) {
	vs := valid(samples)
	var b strings.Builder
	for i, s := range vs {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "<response_%d>\n%s\n</response_%d>", i+1, s.Text, i+1)
	}
	metaPrompt := fmt.Sprintf(USCMetaPrompt, len(vs), originalPrompt, b.String())

	text, usage, err := collect(ctx, p, llm.Request{Model: judgeModel, Prompt: metaPrompt})
	if err != nil {
		return SampleResult{}, 0, err
	}

	synth := SampleResult{
		ID:               "scs-" + uuid.NewString(),
		EnsembleID:       vs[0].EnsembleID,
		SampleIndex:      len(vs), // sentinel index for the meta-call
		Text:             text,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		IsWinner:         true,
		Status:           "ok",
	}
	return synth, 1.0, nil
}

// valid returns the ok samples; collect drains a provider stream into (text, usage).
func valid(samples []SampleResult) []SampleResult {
	out := make([]SampleResult, 0, len(samples))
	for _, s := range samples {
		if s.Status == "ok" {
			out = append(out, s)
		}
	}
	return out
}
```

#### 9.3.5 Stop-Early Logic (goroutine fan-out)

This is the headline Python→Go swap: `asyncio.gather` / `asyncio.as_completed` becomes a goroutine fan-out over the `internal/llm` provider, coordinated by `errgroup` with bounded concurrency (`SetLimit`). Each goroutine emits its `SampleResult` on a channel; a collector reads them, and when the consensus threshold is hit it calls the `context.CancelFunc`, which propagates through every in-flight `Stream` call and stops the remaining workers before their provider request completes. `sync.Once` guards the single cancel; no future-by-future `.cancel()` bookkeeping is needed.

```go
// RunEnsemble fans out up to cfg.N samples as goroutines over the provider,
// cancelling the shared context once the consensus threshold is reached
// (StopEarly). Returns the collected results in completion order.
func RunEnsemble(ctx context.Context, p Provider, prompt string, cfg Config, ensembleID string) ([]SampleResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(cfg.N) // bound concurrency; raise the ceiling here if desired

	out := make(chan SampleResult, cfg.N)
	for i := 0; i < cfg.N; i++ {
		i := i
		g.Go(func() error {
			t0 := time.Now()
			text, usage, err := collect(gctx, p, llm.Request{
				Prompt:      prompt,
				Temperature: cfg.Temperature,
				MaxTokens:   cfg.MaxCompletionTokens,
			})
			latency := int(time.Since(t0).Milliseconds())
			if err != nil {
				// context cancellation (stop-early) is expected, not a failure.
				status := "error"
				if errors.Is(err, context.Canceled) {
					status = "cancelled"
				}
				out <- SampleResult{ID: "scs-" + uuid.NewString(), EnsembleID: ensembleID, SampleIndex: i, Status: status, ErrorMsg: err.Error()}
				return nil // one failed sample must not tear down the group
			}
			key := ""
			if len(text) > 0 {
				n := min(100, len(text)) // first 100 chars for discrete keying
				key = normaliseKey(text[:n])
			}
			out <- SampleResult{
				ID: "scs-" + uuid.NewString(), EnsembleID: ensembleID, SampleIndex: i,
				Text: text, VoteKey: key,
				PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
				CostUSD: usage.CostUSD, LatencyMS: latency, Status: "ok",
			}
			return nil
		})
	}
	go func() { _ = g.Wait(); close(out) }()

	var (
		results []SampleResult
		once    sync.Once
		counts  = map[string]int{}
	)
	for r := range out {
		results = append(results, r)
		if cfg.StopEarly && cfg.VoteMode == VoteMajority && r.Status == "ok" {
			counts[r.VoteKey]++
			top := 0
			for _, c := range counts {
				if c > top {
					top = c
				}
			}
			if float64(top)/float64(cfg.N) >= cfg.ConsensusThreshold {
				once.Do(cancel) // stop the remaining goroutines
			}
		}
	}
	return results, nil
}
```

### 9.4 New Package: `internal/agent/selfconsistency`

The package exports (Go has no async/sync split — a single `context`-aware `RunEnsemble` replaces the Python async+`asyncio.run` wrapper pair):

- Package constants: `MaxSamples = 40`, `USCMetaPrompt`, package-init `regexp` patterns
- `DetectAnswerType(promptText string) AnswerType`
- `ComputePreflightCost(n int, modelID, promptText string, cfg Config) float64`
- `normaliseKey(text string) string`
- `MajorityVote(samples []SampleResult) (SampleResult, float64, error)`
- `EmbeddingVote(ctx, samples []SampleResult, emb Embedder) (SampleResult, float64, error)`
- `LLMVote(ctx, samples []SampleResult, originalPrompt, judgeModel string, p Provider) (SampleResult, float64, error)`
- `RunEnsemble(ctx, p Provider, prompt string, cfg Config, ensembleID string) ([]SampleResult, error)`
- `PersistEnsemble(ctx, tx *sql.Tx, e EnsembleResult) error`
- `PersistSamples(ctx, tx *sql.Tx, samples []SampleResult) error`

The `tag ensemble` subcommand handlers (`list`/`show`/`export`/`stats`) live in `internal/cli` and call query helpers in `internal/store`, returning an `error` (cobra maps a non-nil error to exit code 1).

### 9.5 Integration Points in `internal/cli`

```go
// In the submit/run cobra RunE, after flag binding:
if samples > 1 {
	cfg := selfconsistency.Config{ /* from flags + koanf defaults */ }
	// ... ensemble path: DetectAnswerType, ComputePreflightCost,
	// RunEnsemble, MajorityVote/EmbeddingVote/LLMVote,
	// PersistEnsemble + PersistSamples in one store.Tx.
	return runEnsemble(cmd.Context(), cfg)
}
// else: existing single-call path, unchanged (selfconsistency never entered)
```

`tag ensemble` is wired as a cobra command tree:
```go
// In internal/cli:
ensembleCmd := &cobra.Command{Use: "ensemble"}
ensembleCmd.AddCommand(ensembleListCmd, ensembleShowCmd, ensembleExportCmd, ensembleStatsCmd)
rootCmd.AddCommand(ensembleCmd)
```

### 9.6 Config Keys

New keys layered through `internal/config` (koanf v2 read + yaml.v3/flock/os.Rename atomic write-back), no schema change:

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `self_consistency.n_samples` | int | `1` | Default N when `--samples` is omitted |
| `self_consistency.temperature` | float | `0.7` | Default sampling temperature |
| `self_consistency.judge_model` | str | `claude-haiku-4-5` | Default USC meta-call model |
| `self_consistency.consensus_threshold` | float | `0.6` | Default stop-early threshold |
| `self_consistency.embed_model` | str | `all-MiniLM-L6-v2` | Default embedding model |

Set via: `tag config set self_consistency.n_samples 3`

### 9.7 OTel Semantic Conventions

New attribute-key constants added to `internal/obs` (alongside the hardcoded `gen_ai.*` table and pinned `SEMCONV_VERSION`), used with `go.opentelemetry.io/otel/attribute`:

```go
// Self-consistency ensemble attribute keys.
const (
	SCSampleIndex        = "sc.sample_index"          // attribute.Int: 0-based sample position
	SCVoteKey            = "sc.vote_key"              // attribute.String: normalised answer key
	SCVoteMode           = "sc.vote_mode"             // attribute.String: majority|embedding|llm
	SCConsensusRatio     = "sc.consensus_ratio"       // attribute.Float64: winnerCount / nSampled
	SCStopEarly          = "sc.stop_early"            // attribute.Bool: whether --stop-early was set
	SCStopEarlyTriggered = "sc.stop_early_triggered"  // attribute.Bool: whether it fired
	SCNRequested         = "sc.n_requested"           // attribute.Int: N from --samples
	SCNSampled           = "sc.n_sampled"             // attribute.Int: actual samples used
	SCAnswerType         = "sc.answer_type"           // attribute.String: detected answer type
)
```

---

## 10. Security Considerations

1. **Secret scanning before sampling.** The prompt is passed through `security.scan_for_secrets(prompt_text)` before the pre-flight cost display. If secrets (API keys, tokens, passwords matching known patterns) are found, the command aborts with exit code 1 and prints a generic "secrets detected" message — it does not echo the prompt or the matched secrets. This prevents N copies of a secret-containing prompt from reaching the API.

2. **USC meta-prompt injection.** When building the `USC_META_PROMPT` for `--vote llm`, raw sample texts from N model calls are embedded into a new prompt that is sent to the judge model. A malicious sample could contain prompt-injection instructions targeting the judge. Mitigation: each sample block is enclosed in literal XML-style delimiters (`<response_1>...</response_1>`) and the meta-prompt includes an explicit instruction to ignore formatting or instructions within response blocks.

3. **Embedding model supply-chain trust.** In the default CGO-free binary, `--vote embedding` calls a provider embedding API through the `Embedder` interface — no model download. The optional offline path (build-tag MiniLM via cybertron/hugot) fetches model weights on first use; in air-gapped or hardened environments this may fail or pull an untrusted model. Mitigation: `--embed-model` accepts a local path, and the offline build documents pinning weights by digest and pre-provisioning them. No code change required, but noted in deployment docs.

4. **SQLite `text` column contains full completion text.** The `sc_samples.text` column stores raw model outputs. If a model completion contains sensitive information (e.g., the model reproduced a secret from its context), that content persists in `~/.tag/runtime/tag.sqlite3`. Mitigation: the same secret-scanning logic runs on each sample's text before writing to SQLite; flagged content is replaced with `[REDACTED:secret]`. This is controlled by the `security.redact_on_persist` config key (default: `true`).

5. **Budget bypass via concurrent N calls.** Without the pre-flight budget check (FR-08, FR-09), a user could accidentally exhaust their API budget with a large N. The budget reservation must be atomic: `budget.check_and_reserve()` checks and increments a `reserved_usd` counter in a single SQLite transaction, preventing concurrent `tag submit` calls from each individually clearing the check before the other has reserved its cost.

6. **Serialization-free embedding storage.** Embeddings are stored in `sc_samples.embedding_blob` as raw IEEE 754 float32 little-endian bytes (`binary.Write(buf, binary.LittleEndian, emb)`), never through `encoding/gob` or any reflective deserializer. Reading back is a fixed-width `binary.Read` into `[]float32`. This keeps the column a plain byte array with no deserialization-RCE surface (the Go analogue of the pickle-RCE class that motivated GHSA-mhr3-j7m5-c7c9 in the Python design).

7. **USC judge call content filtering.** The synthesis text returned by `--vote llm` passes through the same output filtering as a normal `tag submit` response. There is no special trust level for judge outputs.

---

## 11. Testing Strategy

Tests use the standard-library `testing` package with table-driven cases. Determinism comes from dependency injection, not monkeypatching: a fake `Provider` implementing `internal/llm`'s `Stream(ctx, Request) -> <-chan Event` returns canned samples so voting/aggregation is exercised offline, and a fake `Embedder` returns fixed vectors. The store is a temp `modernc.org/sqlite` file (or `:memory:`). Provider streaming suites use `go-vcr` cassettes where a real event ordering is needed.

### 11.1 Unit Tests (`internal/agent/selfconsistency/*_test.go`)

| Test | Description |
|------|-------------|
| `TestDetectAnswerTypeDiscrete` | Verifies `DetectAnswerType("Is this a SQL injection? Yes or No.")` returns `AnswerDiscrete`. |
| `TestDetectAnswerTypeOpenEnded` | Verifies `DetectAnswerType("Explain the architectural tradeoffs of...")` returns `AnswerOpenEnded`. |
| `TestDetectAnswerTypeStructured` | Verifies `DetectAnswerType("Return a JSON object with...")` returns `AnswerStructured`. |
| `TestMajorityVoteClearWinner` | `["yes", "yes", "no"]` → winner is `"yes"`, consensus ratio ≈ 0.667. |
| `TestMajorityVoteTieBrokenByLatency` | Two `"yes"` samples (latency 500 ms, 200 ms) and two `"no"` samples → tie resolved by the 200 ms `"yes"` sample winning. |
| `TestMajorityVoteNoValidSamples` | All samples `status='error'` → non-nil error returned. |
| `TestNormaliseKey` | `"Yes! SQL injection."` → `"yes sql injection"`. |
| `TestEmbeddingVoteSingleSample` | N=1 valid sample → returns it with consensus 1.0, no clustering attempted. |
| `TestEmbeddingVoteLargestCluster` | Fake `Embedder` yields two clear clusters; the largest-cluster winner is selected. No ML dependency — pure fixture vectors. |
| `TestLLMVoteMetaPromptConstruction` | Fake `Provider` records its request; assert the meta-prompt contains all 3 sample texts wrapped in `<response_N>` delimiters. |
| `TestStopEarlyCancelsGoroutines` | N=5, consensus reached at K=3; assert the shared context is cancelled and the remaining samples land as `status='cancelled'`. |
| `TestComputePreflightCost` | N=5, 100 prompt tokens, known rate → estimate within 1 % of the manual calculation. |
| `TestEmbeddingFallbackNoEmbedder` | Inject a nil/unavailable `Embedder`; assert `EmbeddingVote` returns an error the caller handles by falling back to `MajorityVote`. |
| `TestTemperatureZeroOverride` | `Config{Temperature: 0.0, N: 3}` → `RunEnsemble` overrides temperature to 0.1. |
| `TestSamplesMaxExceeded` | `--samples 41` → error referencing the N=40 cap. |
| `TestSecretScanAborts` | Fake secret scanner returns detections; assert the `submit` handler returns an error (exit 1) before any provider call. |
| `TestSQLiteSchemaCreation` | Running the migrations on an empty DB creates both `sc_ensembles` and `sc_samples` with correct columns. |
| `TestPersistEnsembleAndSamples` | Full `EnsembleResult` round-trip: persist → read back → assert all fields match. |
| `TestEmbeddingBlobNoGob` | `embedding_blob` stored as raw float32 bytes; assert `binary.Read` recovers the original values; assert no `encoding/gob` import in the package. |

### 11.2 Integration Tests (`internal/agent/selfconsistency/integration_test.go`)

| Test | Description |
|------|-------------|
| `TestEndToEndMajorityN3` | Run the `submit` handler with `--samples 3 --vote majority` against a fake `Provider` returning deterministic responses; assert winner text, consensus ratio, and SQLite rows. |
| `TestStopEarlyIntegration` | Fake `Provider` returns `"yes"` for all samples; assert consensus at 2/3 > 0.6 cancels the context and remaining samples are `cancelled`. |
| `TestDryRunNoProviderCall` | `--dry-run --samples 5` → assert fake provider call count = 0, exit code 0. |
| `TestEnsembleListCmd` | Seed two ensemble rows in SQLite; run `tag ensemble list`; assert both appear in output. |
| `TestEnsembleShowCmd` | Seed ensemble + 3 samples; run `tag ensemble show <id>`; assert all 3 samples visible. |
| `TestBudgetEnforcement` | Fake budget with remaining = 0; assert command exits code 1 with budget-exceeded message. |
| `TestSpanCount` | After a 3-sample ensemble, assert an in-memory otel span recorder holds 4 spans (1 parent + 3 children). |
| `TestJSONOutputSchema` | `--json` output parses as valid JSON; assert required keys present (`ensemble_id`, `winner`, `consensus_ratio`, `samples`, `total_cost_usd`). |

### 11.3 Benchmarks (`internal/agent/selfconsistency/bench_test.go`)

| Benchmark | Description |
|------|-------------|
| `BenchmarkSQLiteWriteN40` | Insert 40 sample rows in a single transaction; assert total wall time < 10 ms on SSD (modernc headroom). |
| `BenchmarkEmbeddingEncodeN40` | Encode 40 × 200-token texts via the offline MiniLM `Embedder`; assert total time < 2 s. |
| `BenchmarkEnsembleOverheadN1` | `--samples 1` wall time within 5 % of the baseline single-call path (`b.N` iterations). |
| `BenchmarkCancelLatency` | Stop-early cancel of 4 remaining goroutines settles within 5 ms after `cancel()`. |

---

## 12. Acceptance Criteria

| ID | Criterion | How to Verify |
|----|-----------|--------------|
| AC-01 | `tag submit --samples 3 --vote majority --prompt "Is X vulnerable? Yes or No."` with 2/3 responses returning "yes" returns `"yes"` as the winner. | Integration test `TestEndToEndMajorityN3` |
| AC-02 | `--stop-early` with N=5 and threshold=0.6 fires at most 4 samples when 3 agree on the same answer (3/5 ≥ 0.6 = 0.6). | Integration test `TestStopEarlyIntegration` |
| AC-03 | `--dry-run` exits with code 0 and zero API calls for any N. | Integration test `TestDryRunNoProviderCall` |
| AC-04 | Both `sc_ensembles` and `sc_samples` tables are created by the `internal/store` migrations on first run with correct column names and indexes. | Unit test `TestSQLiteSchemaCreation` |
| AC-05 | `--samples 1` (default) never enters `internal/agent/selfconsistency` and performs no additional SQLite writes vs. baseline. | Unit test asserting the `submit` handler takes the single-call branch (fake provider called exactly once) |
| AC-06 | `--samples 41` exits with code 1 and a message mentioning the 40-sample cap. | Unit test `TestSamplesMaxExceeded` |
| AC-07 | A prompt containing a mock API key pattern is blocked by `security.ScanForSecrets` before any API call is made. | Unit test `TestSecretScanAborts` |
| AC-08 | `--vote embedding` with no available `Embedder` falls back to `majority` (discrete) or `llm` (open-ended) and emits a warning. | Unit test `TestEmbeddingFallbackNoEmbedder` |
| AC-09 | `--json` output is valid JSON containing `ensemble_id`, `n_requested`, `n_sampled`, `consensus_ratio`, `winner`, `samples` array, and `total_cost_usd`. | Integration test `TestJSONOutputSchema` |
| AC-10 | `tag ensemble list` shows the ensemble run after a successful `tag submit --samples 3` call. | Integration test `TestEnsembleListCmd` |
| AC-11 | `tag ensemble show <id>` displays all 3 sample texts (truncated) with their vote keys and costs. | Integration test `TestEnsembleShowCmd` |
| AC-12 | Budget enforcement blocks a `--samples 10` run when remaining budget is $0.00. | Integration test `TestBudgetEnforcement` |
| AC-13 | Tracing produces N+1 spans (1 `ensemble` parent + N child sample spans) visible in `tag trace show`. | Integration test `TestSpanCount` |
| AC-14 | Embedding blobs are stored as raw float32 bytes (no `encoding/gob`); `binary.Read` recovers original values. | Unit test `TestEmbeddingBlobNoGob` |
| AC-15 | Pre-flight cost estimate is printed before any API call and within ±15 % of actual spend. | Integration test with fake usage data |
| AC-16 | `--samples-temperature 0` is overridden to 0.1 with a printed warning. | Unit test `TestTemperatureZeroOverride` |
| AC-17 | `--vote llm` meta-prompt wraps each sample in `<response_N>` delimiters. | Unit test `TestLLMVoteMetaPromptConstruction` |
| AC-18 | `tag ensemble export <id> --output samples.jsonl` writes valid JSONL with one object per sample. | Integration test reading exported file |

---

## 13. Dependencies

| Dependency | Type | Required? | Version | Notes |
|------------|------|-----------|---------|-------|
| `golang.org/x/sync/errgroup` | Go module | Required | latest | Bounded goroutine fan-out (`SetLimit`) + first-error/context cancellation for the N-sample sampling. |
| `github.com/google/uuid` | Go module | Required | latest | Ensemble/sample IDs and per-sample seed derivation. |
| `internal/memory/embed` (Embedder) | Internal package | Optional | PRD-043/072 | Provider embedding API default; build-tag offline MiniLM (`nlpodyssey/cybertron`/`knights-analytics/hugot`). Required only for `--vote embedding`; graceful fallback if unavailable. |
| `github.com/cenkalti/backoff/v4` | Go module | Optional | v4 | Orchestration-level retry of a failed sample (NFR-04). |
| stdlib `math`, `sort`, `encoding/binary`, `regexp` | stdlib | Required | Go 1.24+ | Cosine/centroid arithmetic, tie-break ordering, float32 blob (de)serialization, RE2 answer-type detection. |
| `internal/llm` (Provider) | Internal package | Required | — | `Stream(ctx, Request) -> <-chan Event`; anthropic-sdk-go + openai-go/v3 behind the interface (never called directly from the reasoning layer). |
| `internal/obs` (budget + pricing) | Internal package | Required | PRD-012/041/046 | `CheckAndReserve()`, cost rate lookup, `sc.*` OTel attribute keys. |
| `go.opentelemetry.io/otel` | Go module | Required | PRD-013/041 | Parent + per-sample child spans. |
| `internal/security` | Internal package | Required | PRD-034 | `ScanForSecrets()`, secret redaction. |
| `internal/eval` | Internal package | Optional | PRD-027 | For `tag eval run --samples N` integration. |
| `internal/store` | Internal package | Required | — | `modernc.org/sqlite` connection, migrations, single-writer atomic RMW. |
| GitHub Issue #349 | Tracker | — | — | Feature request tracking. |

---

## 14. Open Questions

| # | Question | Owner | Resolution Target |
|---|----------|-------|-------------------|
| OQ-01 | Should `--vote embedding` share the `Embedder` instance with the tool-index (PRD-043) to avoid initialising the offline model twice in the same process? The models may differ (`all-MiniLM-L6-v2` vs. whatever tool retrieval uses). | Implementer | Before implementation — check the tool-index embed model name and decide on a shared `Embedder` provided via DI in `internal/memory/embed` rather than two independent initialisations. |
| OQ-02 | Should `sc_samples.text` be encrypted at rest (e.g., using a key stored in the system keychain) given that it contains full model outputs that may include sensitive context? | Security | Before merge — current plan is secret redaction (§10 point 4); full encryption may be added via PRD-034 extension. |
| OQ-03 | For `--vote embedding` on structured JSON outputs, should clustering operate on the raw JSON string or on a normalised form (key-sorted, whitespace-collapsed)? Raw strings will produce spurious distance from formatting differences. | Implementer | Implement normalisation for the `AnswerStructured` type: decode into `map[string]any` and re-`json.Marshal` (Go marshals map keys in sorted order) before encoding. |
| OQ-04 | What is the correct behaviour when all N samples fail (all `status='error'`)? Currently `MajorityVote` returns a non-nil error. Should it instead fall back to a single retry with the standard call path? | Product | Decide before implementation. Current proposal: return the error; let the `submit` handler detect it and retry once with N=1. |
| OQ-05 | Should `tag ensemble export` support a `--winner-only` flag to export only the winning samples across all ensembles for a profile, producing a cleaner fine-tuning dataset? | Product | Post-MVP; add to backlog as enhancement to FR-15. |
| OQ-06 | The `USC_META_PROMPT` uses model-agnostic wording. Should there be judge-model-specific prompt templates (Claude vs. GPT-4o system prompt conventions differ)? | Implementer | Start with a single template; refine based on empirical quality testing. |
| OQ-07 | For the `--vote majority` tie-break (lowest latency), is latency a good proxy? On some API providers, latency correlates with token count, meaning shorter (potentially incomplete) answers win ties. Alternative: prefer the sample with the highest completion token count in a tie. | Implementer | Expose `--tie-break {latency, length}` in a follow-up. Default to `latency` for now per PRD spec. |

---

## 15. Complexity and Timeline

**Overall estimate:** S (3–5 days) — Difficulty 2/5

### Phase 1 — Schema and Core Structs (Day 1)

- Create the `internal/agent/selfconsistency` package with all structs (`SampleResult`, `EnsembleResult`, `Config`) and typed constants.
- Add `sc_ensembles` and `sc_samples` DDL as idempotent `internal/store/migrate` migrations.
- Write `PersistEnsemble()` and `PersistSamples()` over `*sql.Tx`.
- Write `DetectAnswerType()`.
- Write `ComputePreflightCost()`.
- Add new `sc.*` attribute-key constants to `internal/obs`.
- Unit tests: migrations, structs, `DetectAnswerType`, `ComputePreflightCost`.

### Phase 2 — Aggregation Algorithms (Day 2)

- Implement `MajorityVote()` with normalisation and tie-break.
- Implement `EmbeddingVote()` with plain-Go agglomerative clustering and graceful fallback when the `Embedder` is unavailable.
- Implement `LLMVote()` with USC meta-prompt and `<response_N>` delimiter wrapping.
- Implement `RunEnsemble()` with the errgroup goroutine fan-out + context-cancellation stop-early (replaces the async+sync Python pair).
- Table-driven unit tests: all aggregation functions, stop-early cancellation, temperature override, max-N guard.

### Phase 3 — CLI Integration (Day 3)

- Wire `--samples`, `--vote`, `--stop-early`, `--consensus-threshold`, `--embed-model`, `--judge-model`, `--samples-temperature`, `--dry-run`, `--yes` flags onto the `submit` and `run` cobra commands in `internal/cli`.
- Branch on `samples > 1` before entering `selfconsistency` (zero-overhead single-call path).
- Pre-flight secret scan, cost estimate, budget reservation.
- Tracing span instrumentation (parent + child otel spans).
- `--json` output serialisation via `encoding/json`.
- Integration tests: end-to-end majority, stop-early, dry-run, budget enforcement, JSON output schema.

### Phase 4 — `tag ensemble` Subcommands (Day 4)

- Implement `ensemble list`, `ensemble show`, `ensemble export`, `ensemble stats` cobra handlers in `internal/cli` over `internal/store` queries.
- Wire the subcommand tree onto the root command.
- Human-readable table rendering via lipgloss/bubbles (or a plain tabwriter in non-TTY mode).
- Integration tests: list, show, export JSONL.

### Phase 5 — Hardening and Docs (Day 5)

- Security: secret redaction on the `sc_samples.text` write path.
- Benchmarks: SQLite write N=40, embedding encode N=40, overhead N=1.
- Config key documentation in `tag config list` output.
- OQ-03 resolution: JSON normalisation for structured mode (`json.Marshal` of a decoded, key-sorted value).
- Final acceptance criteria verification sweep.
- Update `docs/prd/INDEX.md` with PRD-101 entry.

---

---

## Enhancement: Diverse-Profile Ensemble with Reviewer-Judge (Conductor-Inspired)

**Added:** v0.7.2 planning cycle — inspired by Sakana AI Conductor (ICLR 2026) and Fugu multi-model orchestration product.

### Background

The base PRD-101 samples N outputs from a *single profile* and majority-votes. Sakana AI's **Conductor** allocates across a *pool of frontier models*, writing different specialist instructions for each worker model per call, then using an LLM judge to select or synthesize the best output. Their **Fugu** product demonstrates 83.9% LiveCodeBench by combining GPT-5, Gemini 2.5, and DeepSeek-R1 outputs through a trained orchestrator.

TAG cannot train a 7B Conductor model. But the *diverse-profile ensemble with reviewer-judge* pattern — run the same goal through N *different* profiles, not N identical calls to the same profile — is implementable using TAG's existing profile system and the LLM-as-judge evaluator (PRD-045).

The key insight: diverse profiles produce *complementary* errors. A coding profile may produce syntactically correct code with subtle logic bugs; a reviewer profile may identify the bugs but produce verbose explanations; an orchestrator profile may produce the cleanest architecture at the expense of implementation detail. A judge that sees all three outputs can synthesize a result better than any single profile.

### New Flags

```bash
# Diverse-profile ensemble: same goal, different profiles, judge selects best
tag submit --prompt "Implement a debounce function in TypeScript" \
           --profiles coder,orchestrator,reviewer \
           --vote judge \
           --judge-profile reviewer

# Judge synthesizes the best elements from all profiles (not just selects)
tag submit --prompt "Write a security review of this OAuth flow" \
           --profiles reviewer,orchestrator \
           --vote synthesize \
           --judge-profile reviewer

# Per-profile custom instructions (Conductor-inspired: specialist instructions per worker)
tag submit --prompt "Design the caching layer" \
           --profiles coder,architect,reviewer \
           --profile-instructions '{"coder": "Focus on Redis implementation.", "architect": "Focus on cache invalidation strategy.", "reviewer": "Focus on consistency guarantees."}' \
           --vote judge

# Combination: diverse profiles + N samples per profile + embedding vote
tag submit --prompt "Find all edge cases in this parsing function" \
           --profiles coder,reviewer \
           --samples-per-profile 2 \
           --vote embedding
```

### Extended Ensemble Modes

| Mode | Flag | Description |
|---|---|---|
| `majority` (existing) | `--vote majority` | Majority vote, same profile N samples |
| `embedding` (existing) | `--vote embedding` | Centroid clustering over embeddings |
| `judge` (new) | `--vote judge` | LLM judge selects best from N diverse-profile outputs |
| `synthesize` (new) | `--vote synthesize` | LLM judge synthesizes all outputs into one final answer |
| `tournament` (new) | `--vote tournament` | Pairwise LLM-judge elimination bracket among outputs |
| `pareto` (new) | `--vote pareto` | Run judge on quality + cost; select Pareto-optimal output |

### Judge Vote Implementation

```go
// JudgeVote calls judgeProfile to select or synthesize from N diverse outputs.
func JudgeVote(ctx context.Context, samples []SampleResult, judgeProfile, goal, mode string, p Provider) (SampleResult, error) {
	var cands strings.Builder
	for i, s := range samples {
		if i > 0 {
			cands.WriteString("\n\n")
		}
		fmt.Fprintf(&cands, "--- Candidate %d (profile: %s) ---\n%s", i+1, s.Profile, s.Text)
	}
	prompt := fmt.Sprintf(JudgeVotePrompt, goal, cands.String(), mode)

	resultText, err := invokeProfile(ctx, p, judgeProfile, prompt)
	if err != nil {
		return SampleResult{}, err
	}
	if mode == "select" {
		idx, err := parseSelectionIndex(resultText, len(samples))
		if err != nil {
			return SampleResult{}, err
		}
		return samples[idx], nil
	}
	// synthesize
	return SampleResult{Text: resultText, Profile: "synthesized", CosineToCentroid: 1.0}, nil
}
```

### Tournament Mode

For N profiles/samples, run pairwise LLM-judge elimination:
- Round 1: pair (0,1), (2,3), ... → judge picks winner of each pair
- Repeat until one candidate remains
- Works for any N; O(N log N) judge calls
- Logged to `sc_samples` with `round` and `match_id` columns

### Profile-Specific Instructions

When `--profile-instructions` is passed, each profile receives a different system instruction prepended to the shared goal:

```go
for profile, instruction := range profileInstructions {
	fullPrompt := instruction + "\n\n" + goal
	s := runSample(ctx, p, profile, fullPrompt, cfg.Temperature)
	samples = append(samples, s)
}
```

This replicates Conductor's core differentiator — specialist instructions per worker — at zero training cost. (Each `runSample` fans out as its own goroutine under the same errgroup as the base ensemble.)

### Updated DB Schema

Applied as additional idempotent `internal/store/migrate` steps (ALTER-if-missing, replayed verbatim); the SQL is unchanged:

```sql
-- New columns on sc_samples (ALTER TABLE)
ALTER TABLE sc_samples ADD COLUMN profile TEXT;
ALTER TABLE sc_samples ADD COLUMN profile_instruction TEXT;
ALTER TABLE sc_samples ADD COLUMN tournament_round INTEGER;
ALTER TABLE sc_samples ADD COLUMN tournament_match_id INTEGER;
ALTER TABLE sc_samples ADD COLUMN judge_selected INTEGER DEFAULT 0;  -- 1 if this sample was selected by judge

-- New ensemble run metadata
ALTER TABLE sc_ensembles ADD COLUMN vote_mode_ext TEXT DEFAULT 'majority';
ALTER TABLE sc_ensembles ADD COLUMN judge_profile TEXT;
ALTER TABLE sc_ensembles ADD COLUMN profiles_json TEXT;  -- JSON array of profiles used
```

### Performance Characteristics

- **N profiles × 1 sample each:** Same token cost as N samples on one profile; different error modes.
- **Judge call overhead:** 1 extra call for `judge`/`synthesize` modes; 2K–4K tokens for the judge prompt (judge reads all N outputs).
- **Tournament overhead:** ceil(log2(N)) rounds × 1 judge call per match = O(N) judge calls total.
- **Quality vs single-profile:** Conductor paper shows 15–20% improvement on hard reasoning tasks vs best single model; similar gains expected for diverse-profile ensembles.

### New Testing Requirements

| Test | Assertion |
|---|---|
| `TestJudgeVoteSelectsValid` | Returns one of the input samples; index in range |
| `TestSynthesizeModeReturnsText` | Synthesize mode returns non-empty text |
| `TestTournamentN4` | 4 candidates → 2 rounds → 1 winner |
| `TestTournamentN1` | 1 candidate → returned immediately, no judge call |
| `TestDiverseProfilesAllCalled` | With `--profiles A,B,C`, each profile called exactly once (fake `Provider` records per-profile calls) |
| `TestProfileInstructionsPrepended` | Each sample's prompt contains its profile-specific instruction |
| `TestParetoVoteRespectsCost` | Pareto vote prefers lower-cost sample when quality is equal |

*End of PRD-101*

