# PRD-106: Speculative Action Execution for Latency Reduction (SPAgent Pattern) (`tag loop start --speculative`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** L (2-4 weeks)
**Category:** Advanced Reasoning & Planning
**Affects:** `loop_agent.py`
**Depends on:** PRD-027 (eval framework), PRD-028 (sandbox — isolated speculative execution), PRD-013 (agent tracing — speculation spans), PRD-034 (security — prompt content scanning before speculative dispatch), PRD-012 (budget enforcement — speculative cost accounting), PRD-043 (vector tool retrieval — next-action prediction via SentenceBERT), PRD-008 (background task queue — async tool execution), PRD-041 (OTel span cost attribution — per-speculation cost tags)
**Inspired by:** SPAgent paper (2024), speculative decoding, prefill optimization

---

## 1. Overview

Every iteration of a TAG loop agent follows a strict sequential pattern: the agent calls a tool, waits for that tool's result, then plans and dispatches the next tool call. For tool calls with high wall-clock latency — web searches, code execution in sandboxed environments, file system traversals, external API calls — the agent is idle while waiting. This idle time is pure latency overhead: the model's inference capacity, the user's wall clock, and the loop's iteration budget are all consumed without progress.

Speculative execution is a well-established technique in computer architecture (branch prediction, out-of-order execution) and more recently in LLM inference (speculative decoding, draft-model prefill). The SPAgent paper (2024) applies the same insight to agentic tool-call chains: while a slow tool is executing, a lightweight prediction model estimates the most probable next action given the current context and partial observation history, and that predicted next action is dispatched speculatively. If the speculative prediction is correct once the real result arrives, the agent has effectively hidden the tool's latency behind concurrent planning work. If the prediction is wrong, the speculative result is discarded and the agent falls back to the standard sequential path.

The IdleSpec variant (exploited in this PRD) further refines the approach using Thompson sampling with a Beta(α, β) prior over historical prediction accuracy per (tool, action-type) pair. Rather than committing to a single speculative draft, IdleSpec generates up to K=5 draft continuations during the tool's wait time. When the real observation arrives, a synthesizer pass selects the best-matching draft — or falls back to a fresh inference if none match. This avoids the binary commit-or-rollback model and instead treats speculation as a warm-start for the planning step.

This feature integrates deeply with TAG's existing infrastructure. The `loop_agent.py` worker manages the agent iteration loop; speculative execution layers on top of this loop as an optional execution mode activated by `--speculative`. All speculative attempts are recorded in a new `speculative_attempts` SQLite table for cost attribution, accuracy measurement, and Thompson sampling prior updates. The `tracing.py` module receives new span types (`speculative.draft`, `speculative.verify`, `speculative.hit`, `speculative.miss`) with OTel-compatible semantic conventions. Budget enforcement via `budget.py` accounts for speculative token spend separately from main-path spend, with a configurable multiplier cap. Security scanning via `security.py` runs on speculative prompts before dispatch to prevent secret leakage into parallel inference paths.

The feature is activated with `--speculative` on `tag loop start` and `tag submit`. When disabled (the default), zero overhead is added to the standard sequential path. When enabled, the system automatically degrades to sequential mode for any tool call whose expected latency (estimated from historical `tool_latencies` data) is below a configurable threshold, ensuring speculation overhead does not exceed the latency savings for fast tools.

---

## 2. Problem Statement

### 2.1 Sequential Tool-Call Chains Waste Idle Wait Time

In a typical multi-step agent loop, 40–70% of wall-clock time is consumed waiting for tool results. A `tag loop start --goal "audit this codebase for SQL injection"` task on a medium-sized repository will invoke tools in chains like: `bash("find . -name '*.py'")` → `bash("grep -n 'cursor.execute' ...")` → `bash("cat file.py")` → `bash("wc -l ...")`. Each shell command completes in under 200 ms, but a web search or sandbox code execution can take 3–15 seconds. During that wait, the loop agent process is blocked in `subprocess.run(...)` with no productive work occurring. Multiplied across a 10-iteration loop with 3 tool calls per iteration, this yields 30 sequential idle periods — each a wasted opportunity to advance planning.

The standard mitigation (reducing tool call timeout, caching results, using faster tools) is orthogonal to the planning latency problem: even if every tool were instantaneous, the model still needs inference time to produce its next action. But inference time and tool latency overlap almost perfectly in timeline — the model cannot start planning until it sees the tool result, and tool execution cannot begin until the model finishes planning the previous step. Speculative execution breaks this dependency by placing a probabilistic bet on the next action while the current tool runs.

### 2.2 No Historical Accuracy Signal for Next-Action Prediction

TAG currently has no mechanism to learn from past loop runs which next actions tend to follow which current actions. The `loop_iterations` table stores every iteration's input and output, but there is no structured extraction of tool call sequences, no measurement of prediction accuracy for any speculative attempt, and no per-(tool, action-type) accuracy prior. This means any speculative execution system built today would need to start from a uniform prior (equal probability for all next actions), which reduces speculation effectiveness on the first few runs of a given goal type and profile combination.

The `tool_retrieval.py` module (PRD-043) already embeds tool descriptions with SentenceBERT for semantic retrieval; this infrastructure can be extended to embed tool-call sequences and retrieve historically similar continuations. The `semantic_memory.py` module stores cross-session context; it can be queried for prior loop execution patterns matching the current goal. Neither integration exists today, leaving the agent with no historical signal to inform speculative dispatch.

### 2.3 Latency Reduction Has No Measurable Cost-Benefit Tracking

Tag users who care about agent loop latency have no mechanism today to measure how much time is spent in tool-wait vs. model-inference vs. planning overhead. The `tracing.py` span records show start/end times for individual operations, but there is no aggregated view of "% of loop wall time spent waiting for tools" vs. "% spent in model inference." Without this baseline, it is impossible to know whether speculative execution is providing benefit on a given workload, or whether speculation overhead (extra inference calls for draft generation) is consuming more tokens than it saves in latency.

---

## 3. Goals and Non-Goals

### 3.1 Goals

| # | Goal |
|---|------|
| G1 | `tag loop start --speculative` and `tag submit --speculative` activate speculative execution mode; the default behavior is unchanged. |
| G2 | During any tool call whose estimated latency exceeds `speculation.min_tool_latency_ms` (default 500 ms), the system speculatively generates up to `idlespec.draft_cap` (default 5) draft next-actions concurrently. |
| G3 | On tool result arrival, the best-matching speculative draft is selected via embedding cosine similarity (threshold 0.85); if a match is found, the draft's planned next action is used directly (speculation hit), skipping a full planning inference. |
| G4 | If no draft matches the threshold, the system falls back to a fresh planning inference (speculation miss), with zero behavioral difference from the non-speculative path. |
| G5 | Thompson sampling with a Beta(α, β) prior per (tool_name, action_type) pair updates after each speculation attempt, improving draft selection over time as the system accumulates accuracy history. |
| G6 | All speculative attempts (drafts generated, hit/miss outcome, latency saved, tokens spent) are persisted to a `speculative_attempts` SQLite table for cost attribution and accuracy reporting. |
| G7 | `tag loop speculative-stats [--loop-id ID] [--profile PROFILE]` displays hit rate, average latency saved, and token overhead for speculative runs. |
| G8 | Budget enforcement (PRD-012) tracks speculative token spend as a separate line item; a `speculation.max_overhead_multiplier` cap (default 1.5×) limits total speculation spend relative to main-path spend. |
| G9 | All speculative spans (`speculative.draft`, `speculative.verify`, `speculative.hit`, `speculative.miss`) are emitted to TAG's tracing infrastructure (PRD-013) with OTel-compatible semantic attributes. |
| G10 | `security.py` scans every speculative prompt before dispatch with the same secret-detection logic applied to primary prompts. |
| G11 | When speculation is active but `budget.py` projects that the next speculative batch would exceed the overhead cap, speculation degrades gracefully to sequential mode for that iteration only. |
| G12 | Speculation accuracy history is scoped per (profile, tool_name, action_type) triplet, enabling per-profile priors that reflect how different agent profiles use tools differently. |

### 3.2 Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Speculative execution for `tag submit` single-shot tasks without tool calls. Speculation requires a tool-call gap to exploit; stateless single-inference tasks have no idle time. |
| NG2 | Rollback of already-executed speculative tool calls. This PRD implements IdleSpec (speculative planning only, not speculative tool dispatch). The agent speculatively plans the next action; it never speculatively executes a tool based on a draft observation. |
| NG3 | Training or fine-tuning a custom draft model. All draft generation uses the same model as the main-path agent, with a shorter context window and lower temperature to reduce cost. |
| NG4 | Speculative execution across parallel tool calls (fan-out). The initial implementation targets linear tool-call chains only. DAG-parallel tool calls (PRD-023) are out of scope. |
| NG5 | User-facing draft inspection or selection. Drafts are an internal optimization detail; the user sees only the final selected action and the outcome. |
| NG6 | Persistent draft caching across sessions. Drafts are ephemeral in-memory objects; only accuracy statistics are persisted to SQLite. |
| NG7 | Speculation for human-approval loops. When `--approval human` is set, speculation is automatically disabled because the human gate introduces unbounded latency that makes speculation useless. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Speculation hit rate (P50) | ≥ 40% of drafts hit threshold after 20 loop runs on the same goal type | Query `speculative_attempts` table; `hit_count / total_attempts` per profile |
| Latency reduction (P50) | ≥ 25% reduction in average per-iteration wall time for tool-heavy loops | Compare `loop_iterations.completed_at - created_at` for spec vs. non-spec runs on identical goals |
| Token overhead | Speculative token spend ≤ 50% of main-path token spend (i.e., overhead multiplier ≤ 1.5×) | Sum `speculative_attempts.draft_tokens` / sum of main-path tokens from `loop_iterations` spans |
| Beta prior convergence | `beta_alpha / (beta_alpha + beta_beta)` accuracy estimate within 10% of empirical hit rate after 50 attempts | Unit test with synthetic hit/miss sequence |
| Zero overhead when disabled | `tag loop start` without `--speculative` has statistically identical wall time to pre-feature baseline | Benchmark 20 runs, two-sample t-test |
| Cold start (first run) hit rate | ≥ 20% hit rate on first 5 speculative attempts (uniform prior) | Integration test with pre-seeded tool sequence fixture |
| Budget cap enforcement | Speculation automatically disables when overhead multiplier reaches cap; no budget overrun | Unit test asserting `_should_speculate()` returns False at 1.5× threshold |
| Secret leakage prevention | Zero secrets detected in speculative prompts in security audit | `security.py` scan on 100 synthetic prompts with injected secrets |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer running long agent loops | use `tag loop start --goal "refactor this codebase" --speculative --profile coder` | My 10-iteration loop finishes faster by overlapping tool wait time with planning for the next step |
| U2 | Platform engineer | see `tag loop speculative-stats --profile coder` in CI output | I can measure whether speculative execution is actually saving latency on our production workloads, not just in theory |
| U3 | Developer on a budget | set `speculation.max_overhead_multiplier = 1.2` in my config | The system never spends more than 20% extra tokens on speculative drafts that might be discarded |
| U4 | Agent loop power user | run `tag submit --speculative --prompt "search for X, then summarize findings"` | A single multi-tool submit also benefits from speculative planning without needing to set up a full loop |
| U5 | Security-conscious team | know that `security.py` scans speculative prompts exactly as it scans primary prompts | Sensitive context from tool results cannot leak into speculative parallel API calls without the same secret-detection checks |
| U6 | Developer debugging slow loops | run `tag loop show <loop-id>` and see per-iteration speculation hit/miss annotations | I understand which iterations benefited from speculation and which fell back to sequential planning |
| U7 | New user on a fresh install | run `--speculative` on the first loop execution | The system still works correctly with a uniform Beta prior and provides latency savings as the prior warms up over subsequent runs |
| U8 | DevOps engineer | set `speculation.min_tool_latency_ms = 1000` to avoid speculation overhead for fast tools | Speculative drafts are only generated when tools are genuinely slow, preventing wasted tokens on sub-second shell commands |

---

## 6. Proposed CLI Surface

### 6.1 `tag loop start --speculative`

Launch an autonomous loop with speculative execution enabled.

```
tag loop start \
  --goal "audit this codebase for SQL injection vulnerabilities" \
  --speculative \
  --profile coder \
  [--max-iters 10] \
  [--approval auto|human] \
  [--speculation-drafts 5] \
  [--speculation-min-latency-ms 500] \
  [--speculation-overhead-cap 1.5] \
  [--json]
```

**Flags specific to speculative mode:**

- `--speculative`: Activate speculative execution. Default: off.
- `--speculation-drafts N`: Maximum draft continuations to generate per tool wait (default: 5, range 1–10). Maps to `idlespec.draft_cap`.
- `--speculation-min-latency-ms MS`: Only speculate when estimated tool latency exceeds this threshold (default: 500). Tools estimated below this threshold execute sequentially.
- `--speculation-overhead-cap FLOAT`: Stop speculating for the current loop when cumulative speculation tokens exceed `FLOAT × main_path_tokens` (default: 1.5).

**Example output (non-JSON):**

```
TAG Loop starting  [speculative mode ON, drafts=5, min-latency=500ms]
Loop ID: loop-4f2a9c1d
Profile: coder | Goal: audit this codebase for SQL injection vulnerabilities

[iter 1/10] Planning...        0.8s
[iter 1/10] Tool: bash (est. 3.2s) — speculating 3 drafts...
[iter 1/10] Tool result arrived — speculation HIT (draft #2, similarity=0.91)
[iter 1/10] Next action: bash("grep -rn 'cursor.execute' src/")   [saved ~2.4s]

[iter 2/10] Tool: bash (est. 0.2s) — below threshold, sequential
...

Loop completed in 42.1s  (speculative saves: 3 hits × avg 2.1s = 6.3s saved)
Speculation stats: 6 attempts | 3 hits (50.0%) | 421 spec tokens | 0.31× overhead
```

### 6.2 `tag submit --speculative`

Single-task submit with speculation enabled.

```
tag submit \
  --speculative \
  --prompt "search the web for CVE-2024-1234, then summarize the patch" \
  --profile researcher \
  [--speculation-drafts 3] \
  [--json]
```

**Example output:**

```
[submit] Running: researcher profile | speculative=on
[tool wait] web_search (est. 4.1s) — generating 3 speculative drafts...
[spec hit] Draft #1 matched observation (sim=0.88) — next action pre-planned
Result: The CVE-2024-1234 patch addresses a buffer overflow in...
Spec stats: 1 attempt | 1 hit | 87 spec tokens | 0.12× overhead
```

### 6.3 `tag loop speculative-stats`

Display aggregated speculation statistics.

```
tag loop speculative-stats \
  [--loop-id LOOP_ID] \
  [--profile PROFILE] \
  [--since DATE] \
  [--json]
```

**Example output (table):**

```
Speculative Execution Stats  (profile: coder, last 30 days)
─────────────────────────────────────────────────────────────
Tool              Action Type    Attempts  Hits  Hit%   Avg Saved   Beta(α,β)
bash              file_read        48       31   64.6%    1.8s       Beta(32,18)
web_search        summarize        12        4   33.3%    5.2s       Beta(5,9)
sandbox_exec      interpret        9         2   22.2%    8.1s       Beta(3,8)
─────────────────────────────────────────────────────────────
Total             —               69       37   53.6%    2.9s avg   —
Token overhead: 1,842 spec tokens / 14,211 main tokens = 0.13× (cap: 1.50×)
```

### 6.4 `tag loop show` (extended with speculation annotations)

The existing `tag loop show <loop-id>` command is extended to display per-iteration speculation outcomes.

```
tag loop show loop-4f2a9c1d

Loop loop-4f2a9c1d  |  profile: coder  |  status: completed
Goal: audit this codebase for SQL injection vulnerabilities

Iter  Status       Tool         Spec?   Outcome   Saved   Similarity
1     completed    bash         yes     HIT        2.4s    0.91
2     completed    bash         no(fast)  —          —       —
3     completed    web_search   yes     MISS        0s      0.42
4     completed    bash         yes     HIT        1.9s    0.87
...
```

### 6.5 Config Keys (tag config set / get)

```bash
tag config set speculation.min_tool_latency_ms 500
tag config set speculation.draft_cap 5
tag config set speculation.max_overhead_multiplier 1.5
tag config set speculation.similarity_threshold 0.85
tag config set speculation.draft_temperature 0.3
tag config set speculation.enabled_profiles coder,researcher
```

---

## 7. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `tag loop start --speculative` activates speculative mode; without this flag the loop behaves identically to the pre-feature implementation. | Must |
| FR-02 | Before dispatching a tool call, the system estimates the tool's expected latency from the `tool_latencies` table (P50 of historical durations for this tool_name). If no history exists, a configurable default `speculation.default_latency_estimate_ms` (default: 800 ms) is used. | Must |
| FR-03 | When estimated latency > `speculation.min_tool_latency_ms`, the system concurrently launches up to `speculation.draft_cap` draft-generation inference calls using a compressed prompt (current context + partial tool invocation, no tool result) at `speculation.draft_temperature` (default 0.3). | Must |
| FR-04 | Draft generation is implemented as asyncio `Task` objects launched in a background event loop thread; the main iteration thread blocks only on the tool subprocess, not on draft generation. | Must |
| FR-05 | When the tool result arrives, each draft is scored against the real observation using cosine similarity of their `sentence-transformers` embeddings. The draft with the highest similarity score above `speculation.similarity_threshold` (default 0.85) is selected as a speculation hit. | Must |
| FR-06 | On a speculation hit, the selected draft's planned next action is extracted and used as the agent's next tool call, skipping a full planning inference call. The hit draft's action string is logged to `loop_iterations.speculative_action_used = TRUE`. | Must |
| FR-07 | On a speculation miss (no draft exceeds the similarity threshold), the system performs a standard planning inference call with the full context including the real tool result. The miss is logged to `speculative_attempts` with `outcome = 'miss'`. | Must |
| FR-08 | Thompson sampling: after each attempt, the `speculative_attempts` table's per-(profile, tool_name, action_type) `beta_alpha` and `beta_beta` columns are updated: `+1` to `beta_alpha` on hit, `+1` to `beta_beta` on miss. | Must |
| FR-09 | Draft ordering for the synthesizer pass uses Thompson sampling: each (tool, action_type) pair's expected accuracy is sampled from Beta(alpha, beta); drafts are generated in descending order of sampled accuracy, so the most historically accurate (tool, action_type) pairing gets the first draft slot. | Should |
| FR-10 | `security.py`'s `scan_prompt()` is called on every speculative prompt before the draft inference call is dispatched. If secrets are detected, the draft is suppressed and a warning is logged to the tracing span without aborting the main tool execution. | Must |
| FR-11 | Budget enforcement: before launching each batch of draft calls, `budget.py`'s `check_budget()` is called with estimated draft tokens. If the overhead multiplier `(accumulated_spec_tokens / accumulated_main_tokens)` would exceed `speculation.max_overhead_multiplier`, speculation is disabled for the current iteration and a `speculation.budget_cap_reached` span event is emitted. | Must |
| FR-12 | All speculative attempts are persisted to `speculative_attempts` SQLite table (schema defined in Section 9) within the same WAL-mode connection used by `loop_agent.py`. | Must |
| FR-13 | The `tracing.py` module receives the following new span types: `speculative.draft_batch` (parent), `speculative.draft` (child per draft), `speculative.verify` (similarity scoring pass), `speculative.hit`, `speculative.miss`. All carry OTel attributes: `spec.draft_index`, `spec.similarity_score`, `spec.tokens_used`, `spec.outcome`. | Must |
| FR-14 | `tag loop speculative-stats` queries `speculative_attempts` and displays hit rate, avg latency saved, token overhead ratio, and per-(tool, action_type) Beta priors. Supports `--loop-id`, `--profile`, `--since`, and `--json` flags. | Must |
| FR-15 | `tag loop show <loop-id>` output is extended to include per-iteration `spec_outcome` (HIT/MISS/SKIPPED/DISABLED) and `spec_similarity` columns when speculative mode was used. | Should |
| FR-16 | When `--approval human` is set, `--speculative` is silently ignored and a warning is emitted: `WARNING: speculative mode is incompatible with human approval gates; disabling speculation`. | Must |
| FR-17 | Tool latency estimation writes a new row to `tool_latencies` after each tool call completes, recording `tool_name`, `duration_ms`, `loop_id`, `profile`, and `created_at`. The P50 query reads from this table. | Must |
| FR-18 | `tag submit --speculative` supports the same speculative flags as `tag loop start --speculative` and follows the same hit/miss/budget logic for single-shot multi-tool prompts. | Should |
| FR-19 | When `speculation.enabled_profiles` is set in config, speculation is only activated for profiles in that list, regardless of the `--speculative` flag for other profiles. | Could |
| FR-20 | Draft context is a compressed version of the current agent context: system prompt, last 3 iteration summaries, the current tool call being awaited (but not its result), and a speculation-specific suffix: `"Predict the most likely next action after this tool returns a typical result."` | Must |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | **Zero overhead when disabled.** `tag loop start` without `--speculative` must not import asyncio event loop management, Thompson sampling, or sentence-transformers embedding code. Guard with `if not speculative_mode: return`. | Verified by `sys.modules` assertion in unit test |
| NFR-02 | **Draft generation must not block the tool result handler.** All draft inference calls run as daemon asyncio tasks; the main thread's `subprocess.run(tool_cmd)` is never delayed by draft generation. | Verified by timing test: tool result must be processed within 10 ms of subprocess completion |
| NFR-03 | **SQLite write contention.** Speculative attempt writes to `speculative_attempts` must not block iteration reads. Use WAL mode (already enabled in `_open_db()`) and a `PRAGMA busy_timeout = 5000`. | Verified by concurrent write test with 10 simultaneous draft writers |
| NFR-04 | **Memory footprint.** Draft text strings are discarded immediately after verification; only the selected draft's action string is retained. Total in-memory draft storage must be under 50 KB per iteration batch. | Verified by `tracemalloc` assertion in unit test |
| NFR-05 | **Draft inference timeout.** Each individual draft inference call has a timeout of `min(tool_latency_estimate * 0.9, 30s)`. Timed-out drafts are silently dropped; the batch completes with however many drafts finished. | Verified by mock test with injected timeout |
| NFR-06 | **Embedding model cold start.** The SentenceBERT model used for draft similarity scoring is loaded lazily on the first speculative attempt and cached for the process lifetime. Cold start must complete within 3 seconds. | Verified by timing test on CI hardware |
| NFR-07 | **Graceful degradation on API error.** If draft inference calls fail (rate limit, network error, API unavailable), the system falls back to sequential planning with a `speculation.api_error` span event. The loop never fails due to speculation errors. | Verified by unit test with mocked API failures |
| NFR-08 | **Token cost transparency.** `--speculative` sessions display running speculation overhead in the progress line (e.g., `spec: 3 hits, 0.13× overhead`) so users can see live cost impact. | Verified by output format test |
| NFR-09 | **Beta prior persistence.** `beta_alpha` and `beta_beta` values in `speculative_attempts` accumulate across sessions; they are not reset between `tag loop start` invocations. Querying the current prior requires a `SUM(hit)` / `SUM(1-hit)` aggregation. | Verified by integration test across two loop sessions |
| NFR-10 | **Profile isolation.** Thompson sampling priors are keyed by `(profile, tool_name, action_type)`. Runs under the `coder` profile do not affect the `researcher` profile's priors, and vice versa. | Verified by unit test with cross-profile prior assertions |

---

## 9. Technical Design

### 9.1 New File

**`src/tag/speculative.py`** — all speculation logic lives in this module. `loop_agent.py` imports from it only when `--speculative` is active.

### 9.2 SQLite DDL

The following tables are added to the `_open_db()` schema in `loop_agent.py` (and conditionally in `speculative.py`'s own `ensure_schema()`):

```sql
-- Persists per-attempt outcomes for Thompson sampling and reporting.
CREATE TABLE IF NOT EXISTS speculative_attempts (
  id              TEXT PRIMARY KEY,          -- uuid4().hex[:12]
  loop_id         TEXT,                      -- FK to loop_runs.id (NULL for submit mode)
  iteration       INTEGER,                   -- loop iteration number (NULL for submit mode)
  profile         TEXT NOT NULL,
  tool_name       TEXT NOT NULL,             -- e.g. 'bash', 'web_search', 'sandbox_exec'
  action_type     TEXT NOT NULL,             -- extracted action category: 'file_read', 'grep', etc.
  draft_index     INTEGER NOT NULL,          -- 0-indexed rank of this draft in the batch
  draft_tokens    INTEGER NOT NULL DEFAULT 0,
  similarity      REAL,                      -- cosine similarity vs real observation (NULL = not scored)
  outcome         TEXT NOT NULL,             -- 'hit', 'miss', 'timeout', 'security_blocked', 'budget_cap'
  latency_saved_ms INTEGER,                  -- estimated ms saved (hit only; NULL otherwise)
  created_at      TEXT NOT NULL,
  FOREIGN KEY(loop_id) REFERENCES loop_runs(id)
);
CREATE INDEX IF NOT EXISTS idx_sa_profile_tool
  ON speculative_attempts(profile, tool_name, action_type, outcome);
CREATE INDEX IF NOT EXISTS idx_sa_loop
  ON speculative_attempts(loop_id, iteration);

-- Accumulates Beta prior parameters per (profile, tool_name, action_type).
-- Materialized for fast lookup; updated incrementally after each attempt.
CREATE TABLE IF NOT EXISTS speculation_priors (
  id          TEXT PRIMARY KEY,    -- profile || '|' || tool_name || '|' || action_type
  profile     TEXT NOT NULL,
  tool_name   TEXT NOT NULL,
  action_type TEXT NOT NULL,
  beta_alpha  REAL NOT NULL DEFAULT 1.0,   -- successes + 1 (Beta conjugate prior)
  beta_beta   REAL NOT NULL DEFAULT 1.0,   -- failures + 1
  updated_at  TEXT NOT NULL,
  UNIQUE(profile, tool_name, action_type)
);

-- Historical tool latencies for estimation (also used by FR-02).
CREATE TABLE IF NOT EXISTS tool_latencies (
  id          TEXT PRIMARY KEY,
  loop_id     TEXT,
  profile     TEXT NOT NULL,
  tool_name   TEXT NOT NULL,
  duration_ms INTEGER NOT NULL,
  created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tl_tool
  ON tool_latencies(profile, tool_name, created_at);

-- Extended loop_iterations columns (ALTER TABLE on existing schema):
-- speculative_mode   INTEGER NOT NULL DEFAULT 0   (0/1 bool)
-- spec_outcome       TEXT                          ('hit','miss','skipped','disabled')
-- spec_similarity    REAL
-- spec_action_used   TEXT                          (the speculative action string, if hit)
```

Migration is handled via `ensure_schema(conn)` in `speculative.py` which runs `ALTER TABLE loop_iterations ADD COLUMN IF NOT EXISTS ...` guarded by a `PRAGMA table_info` check.

### 9.3 Core Dataclasses

```python
# src/tag/speculative.py
from __future__ import annotations
import asyncio
from dataclasses import dataclass, field
from typing import Optional


@dataclass
class SpecConfig:
    """Runtime configuration for one speculative session."""
    enabled: bool = False
    draft_cap: int = 5
    min_tool_latency_ms: int = 500
    max_overhead_multiplier: float = 1.5
    similarity_threshold: float = 0.85
    draft_temperature: float = 0.3
    draft_timeout_s: float = 30.0
    default_latency_estimate_ms: int = 800


@dataclass
class Draft:
    """One speculative continuation generated during a tool's wait time."""
    index: int
    action_type: str          # coarse label extracted by _classify_action()
    action_text: str          # the full predicted next tool call string
    tokens_used: int
    embedding: Optional[list[float]] = None   # populated by _embed()
    similarity: Optional[float] = None        # populated by _score_draft()
    timed_out: bool = False
    security_blocked: bool = False


@dataclass
class SpeculationResult:
    """Outcome of one speculation batch (one tool call wait)."""
    tool_name: str
    tool_latency_ms: int
    drafts: list[Draft] = field(default_factory=list)
    selected_draft: Optional[Draft] = None
    outcome: str = "miss"          # 'hit', 'miss', 'skipped', 'disabled', 'budget_cap'
    latency_saved_ms: Optional[int] = None
    total_spec_tokens: int = 0


@dataclass
class BudgetGuard:
    """Tracks speculation token spend relative to main-path spend."""
    main_tokens: int = 0
    spec_tokens: int = 0
    cap: float = 1.5

    @property
    def overhead_ratio(self) -> float:
        if self.main_tokens == 0:
            return 0.0
        return self.spec_tokens / self.main_tokens

    def would_exceed_cap(self, additional_spec_tokens: int) -> bool:
        if self.main_tokens == 0:
            return False
        return (self.spec_tokens + additional_spec_tokens) / self.main_tokens > self.cap
```

### 9.4 Core Algorithm: `speculate_during_tool()`

```python
# src/tag/speculative.py  (simplified; full implementation adds tracing, security, budget)

import asyncio
import concurrent.futures
import time
from typing import Callable

_embed_model = None   # lazy-loaded SentenceBERT instance


def _get_embed_model():
    global _embed_model
    if _embed_model is None:
        from sentence_transformers import SentenceTransformer
        _embed_model = SentenceTransformer("all-MiniLM-L6-v2")
    return _embed_model


def _cosine_sim(a: list[float], b: list[float]) -> float:
    import math
    dot = sum(x * y for x, y in zip(a, b))
    mag_a = math.sqrt(sum(x**2 for x in a))
    mag_b = math.sqrt(sum(x**2 for x in b))
    if mag_a == 0 or mag_b == 0:
        return 0.0
    return dot / (mag_a * mag_b)


def _build_draft_prompt(context: str, tool_call_str: str) -> str:
    """Compressed prompt for speculative draft generation."""
    return (
        f"{context}\n\n"
        f"[AWAITING TOOL RESULT FOR]: {tool_call_str}\n\n"
        "Predict the single most likely next action this agent will take "
        "after the tool returns a typical successful result. "
        "Output only the next tool call or final answer, no explanation."
    )


def _classify_action(action_text: str) -> str:
    """Coarse action type for Thompson sampling key."""
    action_lower = action_text.lower()
    for keyword, label in [
        ("grep", "grep"), ("cat ", "file_read"), ("find ", "file_find"),
        ("wc ", "file_stat"), ("web_search", "web_search"),
        ("sandbox", "sandbox_exec"), ("GOAL_ACHIEVED", "goal_achieved"),
    ]:
        if keyword in action_lower:
            return label
    return "other"


def _sample_beta(alpha: float, beta: float) -> float:
    """Sample from Beta(alpha, beta) for Thompson sampling."""
    import random
    # Inverse transform via beta variate
    return random.betavariate(alpha, beta)


def _rank_drafts_by_prior(
    draft_cap: int,
    action_types: list[str],
    priors: dict[str, tuple[float, float]],   # action_type -> (alpha, beta)
) -> list[tuple[int, str]]:
    """Return (slot_index, action_type) ordered by Thompson-sampled accuracy."""
    scored = []
    for i in range(draft_cap):
        # Cycle through action types if fewer types than draft slots
        atype = action_types[i % len(action_types)] if action_types else "other"
        alpha, beta_val = priors.get(atype, (1.0, 1.0))
        sampled_acc = _sample_beta(alpha, beta_val)
        scored.append((sampled_acc, i, atype))
    scored.sort(reverse=True)
    return [(idx, atype) for _, idx, atype in scored]


async def _generate_draft(
    draft_index: int,
    prompt: str,
    infer_fn: Callable[[str, float], tuple[str, int]],
    temperature: float,
    timeout_s: float,
    security_scan_fn: Callable[[str], bool],
) -> Draft:
    """Generate a single speculative draft asynchronously."""
    if not security_scan_fn(prompt):
        return Draft(index=draft_index, action_type="other",
                     action_text="", tokens_used=0, security_blocked=True)
    try:
        action_text, tokens = await asyncio.wait_for(
            asyncio.get_event_loop().run_in_executor(
                None, infer_fn, prompt, temperature
            ),
            timeout=timeout_s,
        )
        action_type = _classify_action(action_text)
        return Draft(index=draft_index, action_type=action_type,
                     action_text=action_text, tokens_used=tokens)
    except asyncio.TimeoutError:
        return Draft(index=draft_index, action_type="other",
                     action_text="", tokens_used=0, timed_out=True)


async def speculate_during_tool(
    context: str,
    tool_call_str: str,
    tool_name: str,
    tool_future: concurrent.futures.Future,
    cfg: SpecConfig,
    infer_fn: Callable[[str, float], tuple[str, int]],
    security_scan_fn: Callable[[str], bool],
    priors: dict[str, tuple[float, float]],
    budget: BudgetGuard,
) -> tuple[str, SpeculationResult]:
    """
    Launch speculative drafts concurrently with tool execution.

    Returns (tool_observation, SpeculationResult).
    """
    draft_prompt = _build_draft_prompt(context, tool_call_str)
    # Rough token estimate: 4 chars per token, draft_cap drafts
    estimated_spec_tokens = (len(draft_prompt) // 4) * cfg.draft_cap

    if budget.would_exceed_cap(estimated_spec_tokens):
        # Wait for tool result synchronously; no speculation
        obs = await asyncio.get_event_loop().run_in_executor(None, tool_future.result)
        return obs, SpeculationResult(
            tool_name=tool_name, tool_latency_ms=0,
            outcome="budget_cap"
        )

    action_types = list(priors.keys()) or ["other"]
    ranked_slots = _rank_drafts_by_prior(cfg.draft_cap, action_types, priors)

    draft_tasks = [
        asyncio.create_task(
            _generate_draft(
                draft_index=idx,
                prompt=draft_prompt,
                infer_fn=infer_fn,
                temperature=cfg.draft_temperature,
                timeout_s=cfg.draft_timeout_s,
                security_scan_fn=security_scan_fn,
            )
        )
        for idx, _ in ranked_slots
    ]

    t0 = time.monotonic()
    # Wait for tool result; drafts run concurrently
    obs = await asyncio.get_event_loop().run_in_executor(None, tool_future.result)
    tool_latency_ms = int((time.monotonic() - t0) * 1000)

    # Cancel any still-pending drafts (tool finished before all drafts did)
    for task in draft_tasks:
        if not task.done():
            task.cancel()

    drafts: list[Draft] = []
    for task in draft_tasks:
        try:
            draft = await asyncio.shield(task)
            drafts.append(draft)
        except (asyncio.CancelledError, Exception):
            pass

    # Score completed drafts against real observation
    model = _get_embed_model()
    obs_emb = model.encode(obs, convert_to_tensor=False).tolist()

    selected: Optional[Draft] = None
    for draft in drafts:
        if draft.timed_out or draft.security_blocked or not draft.action_text:
            continue
        draft.embedding = model.encode(draft.action_text, convert_to_tensor=False).tolist()
        draft.similarity = _cosine_sim(obs_emb, draft.embedding)
        if draft.similarity >= cfg.similarity_threshold:
            if selected is None or draft.similarity > (selected.similarity or 0):
                selected = draft

    total_spec_tokens = sum(d.tokens_used for d in drafts)
    budget.spec_tokens += total_spec_tokens

    outcome = "hit" if selected else "miss"
    latency_saved = tool_latency_ms if selected else None

    return obs, SpeculationResult(
        tool_name=tool_name,
        tool_latency_ms=tool_latency_ms,
        drafts=drafts,
        selected_draft=selected,
        outcome=outcome,
        latency_saved_ms=latency_saved,
        total_spec_tokens=total_spec_tokens,
    )
```

### 9.5 Thompson Sampling Prior Update

```python
def update_prior(
    conn: sqlite3.Connection,
    profile: str,
    tool_name: str,
    action_type: str,
    hit: bool,
) -> None:
    """Increment Beta(alpha, beta) prior in speculation_priors table."""
    key = f"{profile}|{tool_name}|{action_type}"
    now = _utc_now()
    conn.execute("""
        INSERT INTO speculation_priors(id, profile, tool_name, action_type,
                                       beta_alpha, beta_beta, updated_at)
        VALUES (?, ?, ?, ?, 2.0, 1.0, ?)
        ON CONFLICT(profile, tool_name, action_type) DO UPDATE SET
            beta_alpha = beta_alpha + ?,
            beta_beta  = beta_beta  + ?,
            updated_at = ?
    """, (key, profile, tool_name, action_type, now,
          1.0 if hit else 0.0,
          0.0 if hit else 1.0,
          now))
    conn.commit()


def load_priors(
    conn: sqlite3.Connection,
    profile: str,
    tool_name: str,
) -> dict[str, tuple[float, float]]:
    """Return {action_type: (alpha, beta)} for Thompson sampling."""
    rows = conn.execute("""
        SELECT action_type, beta_alpha, beta_beta
        FROM speculation_priors
        WHERE profile = ? AND tool_name = ?
    """, (profile, tool_name)).fetchall()
    return {row["action_type"]: (row["beta_alpha"], row["beta_beta"]) for row in rows}
```

### 9.6 Tool Latency Estimation

```python
def estimate_tool_latency_ms(
    conn: sqlite3.Connection,
    profile: str,
    tool_name: str,
    default_ms: int = 800,
) -> int:
    """Return P50 historical latency for (profile, tool_name), or default."""
    rows = conn.execute("""
        SELECT duration_ms FROM tool_latencies
        WHERE profile = ? AND tool_name = ?
        ORDER BY created_at DESC
        LIMIT 100
    """, (profile, tool_name)).fetchall()
    if not rows:
        return default_ms
    durations = sorted(r["duration_ms"] for r in rows)
    mid = len(durations) // 2
    return durations[mid]


def record_tool_latency(
    conn: sqlite3.Connection,
    loop_id: Optional[str],
    profile: str,
    tool_name: str,
    duration_ms: int,
) -> None:
    import uuid
    conn.execute("""
        INSERT INTO tool_latencies(id, loop_id, profile, tool_name, duration_ms, created_at)
        VALUES (?, ?, ?, ?, ?, ?)
    """, (uuid.uuid4().hex[:12], loop_id, profile, tool_name, duration_ms, _utc_now()))
    conn.commit()
```

### 9.7 Integration Point in `loop_agent.py`

The existing `_run_iteration()` function is extended to support speculative mode. The modification is a clean wrapper pattern that leaves the non-speculative code path untouched:

```python
# In loop_agent.py _run_iteration() — speculative path

def _run_iteration_speculative(
    loop_id: str,
    iteration: int,
    goal: str,
    profile: str,
    config_path: str,
    previous_output: str,
    spec_cfg: "SpecConfig",
    conn: sqlite3.Connection,
    budget: "BudgetGuard",
) -> tuple[str, int]:
    """Speculative variant of _run_iteration. Falls back to sequential on errors."""
    from tag.speculative import (
        speculate_during_tool, update_prior, load_priors,
        estimate_tool_latency_ms, record_tool_latency,
        SpeculationResult,
    )
    from tag.security import scan_prompt
    import concurrent.futures

    # Build prompt identical to non-speculative path
    prompt = _build_prompt(goal, iteration, previous_output)

    # Phase 1: Plan the first action (normal inference)
    planning_output, plan_rc = _infer(prompt, profile, config_path)
    if plan_rc != 0:
        return planning_output, plan_rc

    tool_call, tool_name = _extract_tool_call(planning_output)
    if not tool_call:
        return planning_output, 0   # No tool call, return as-is

    # Estimate tool latency
    est_latency = estimate_tool_latency_ms(conn, profile, tool_name)
    should_speculate = (
        spec_cfg.enabled
        and est_latency >= spec_cfg.min_tool_latency_ms
        and not budget.would_exceed_cap(spec_cfg.draft_cap * 200)  # rough estimate
    )

    if not should_speculate:
        # Standard sequential execution
        t0 = time.monotonic()
        tool_output, tool_rc = _execute_tool(tool_call, config_path)
        duration_ms = int((time.monotonic() - t0) * 1000)
        record_tool_latency(conn, loop_id, profile, tool_name, duration_ms)
        # Continue with normal planning from tool_output...
        return _continue_from_observation(
            goal, iteration, planning_output, tool_output, profile, config_path
        )

    # Speculative path: run tool in thread, draft concurrently
    priors = load_priors(conn, profile, tool_name)
    executor = concurrent.futures.ThreadPoolExecutor(max_workers=1)
    tool_future = executor.submit(_execute_tool, tool_call, config_path)

    context = _build_context_for_draft(goal, iteration, previous_output, planning_output)
    loop = asyncio.new_event_loop()
    try:
        t0 = time.monotonic()
        obs, spec_result = loop.run_until_complete(
            speculate_during_tool(
                context=context,
                tool_call_str=tool_call,
                tool_name=tool_name,
                tool_future=tool_future,
                cfg=spec_cfg,
                infer_fn=lambda p, t: _infer_raw(p, profile, config_path, temperature=t),
                security_scan_fn=lambda p: scan_prompt(p, raise_on_secret=False),
                priors=priors,
                budget=budget,
            )
        )
    finally:
        loop.close()
        executor.shutdown(wait=False)

    duration_ms = spec_result.tool_latency_ms
    record_tool_latency(conn, loop_id, profile, tool_name, duration_ms)

    # Persist all draft attempts
    _persist_speculation_result(conn, loop_id, iteration, profile, tool_name, spec_result)

    # Update Thompson sampling priors
    for draft in spec_result.drafts:
        if not draft.timed_out and not draft.security_blocked and draft.action_text:
            is_hit = (spec_result.selected_draft is not None
                      and draft.index == spec_result.selected_draft.index)
            update_prior(conn, profile, tool_name, draft.action_type, hit=is_hit)

    if spec_result.outcome == "hit" and spec_result.selected_draft:
        # Skip full planning inference: use the speculative action directly
        budget.main_tokens += spec_result.selected_draft.tokens_used  # already paid
        return spec_result.selected_draft.action_text, 0
    else:
        # Miss: full planning inference with real observation
        return _continue_from_observation(
            goal, iteration, planning_output, obs, profile, config_path
        )
```

### 9.8 Tracing Integration

New OTel-compatible attributes added to `otel_semconv.py`:

```python
# src/tag/otel_semconv.py additions

SPEC_DRAFT_CAP        = "speculation.draft_cap"
SPEC_DRAFT_INDEX      = "speculation.draft_index"
SPEC_SIMILARITY       = "speculation.similarity_score"
SPEC_OUTCOME          = "speculation.outcome"           # hit | miss | skipped | disabled
SPEC_TOKENS           = "speculation.tokens_used"
SPEC_LATENCY_SAVED_MS = "speculation.latency_saved_ms"
SPEC_OVERHEAD_RATIO   = "speculation.overhead_ratio"
SPEC_TOOL_NAME        = "speculation.tool_name"
SPEC_ACTION_TYPE      = "speculation.action_type"
SPEC_BETA_ALPHA       = "speculation.beta_alpha"
SPEC_BETA_BETA        = "speculation.beta_beta"
```

Span names follow the existing `tag.*` convention:
- `tag.speculation.draft_batch` — root span for one speculation batch
- `tag.speculation.draft` — one draft generation call
- `tag.speculation.verify` — embedding similarity scoring
- `tag.speculation.hit` — emitted when a draft is selected
- `tag.speculation.miss` — emitted when no draft meets threshold

### 9.9 Dependency on `tool_retrieval.py`

The SentenceBERT model already used in `tool_retrieval.py` (PRD-043) is reused for draft embedding. `speculative.py` calls `tool_retrieval.get_embed_model()` (or its equivalent) rather than loading a second model instance. If `tool_retrieval.py` is not available (e.g., the user has not installed `sentence-transformers`), speculative mode falls back to a string-overlap similarity heuristic using `difflib.SequenceMatcher`:

```python
def _fallback_similarity(a: str, b: str) -> float:
    import difflib
    return difflib.SequenceMatcher(None, a.lower(), b.lower()).ratio()
```

---

## 10. Security Considerations

1. **Secret leakage into speculative prompts.** Speculative prompts include the current agent context, which may contain partial tool results from prior iterations. `security.py`'s `scan_prompt()` must be called on every draft prompt before dispatch. If secrets are detected, the draft is suppressed (`security_blocked=True`) and a `WARN` log entry is written. The main tool execution continues unaffected.

2. **Pickle/serialization risks.** Speculative embeddings are stored as Python `list[float]` in memory and are never serialized to SQLite as binary blobs. The `speculative_attempts` table stores only text fields and numeric scores. There is no pickle deserialization in this feature. (Contrast with LangGraph cache PRD-G6 risk noted in cluster research context.)

3. **Speculative action injection.** A hit draft's `action_text` is used as the next tool call. An adversarial tool result that manipulates speculative draft content could theoretically cause action injection. Mitigation: the synthesizer only accepts the draft's `action_text` string (planned next action) and passes it through the same tool-call validation path as any LLM-generated tool call. No raw draft content is executed without validation.

4. **API key exposure in draft prompts.** If the agent context includes API keys or bearer tokens (e.g., from environment variable injection), these may appear in draft prompts sent to the inference API. Mitigation: `security.py` secret scanning (item 1 above) and the existing `PROMPT_REDACT_PATTERNS` list in `security.py` applies to speculative prompts identically to primary prompts.

5. **Rate limit amplification.** With `draft_cap=5`, speculative mode can quintuple the number of API calls per tool wait. This increases the risk of hitting per-minute rate limits on the configured model endpoint. Mitigation: `BudgetGuard.would_exceed_cap()` limits total speculation spend; individual draft timeouts prevent runaway concurrent calls; and the `speculation.max_overhead_multiplier` config key caps aggregate spend.

6. **SQLite WAL write amplification.** Each speculation attempt batch writes up to `draft_cap` rows to `speculative_attempts` plus one update to `speculation_priors`. For a 10-iteration loop with 5 drafts each, this is 50 extra writes per loop run. This is within SQLite WAL mode's safe concurrency limits for the usage pattern (single writer, WAL reader concurrency). The `PRAGMA busy_timeout = 5000` in `_open_db()` prevents write contention failures.

7. **Inference isolation.** Draft inference calls use the same API endpoint and credentials as main-path inference. They are not sandboxed. If the main-path inference is protected by a firewall or network policy (PRD-094 egress firewall), draft inference calls must also comply with that policy. No special exemption is granted to speculative traffic.

---

## 11. Testing Strategy

### 11.1 Unit Tests

**File:** `tests/test_speculative.py`

- `test_spec_config_defaults`: Assert all `SpecConfig` fields have expected defaults.
- `test_cosine_sim_orthogonal`: `_cosine_sim([1,0], [0,1])` returns 0.0.
- `test_cosine_sim_identical`: `_cosine_sim([1,1], [1,1])` returns 1.0.
- `test_classify_action_grep`: `_classify_action('bash("grep -n ...")') == "grep"`.
- `test_classify_action_unknown`: `_classify_action('do something weird') == "other"`.
- `test_budget_guard_cap`: Assert `BudgetGuard(main_tokens=1000, spec_tokens=1499, cap=1.5).would_exceed_cap(2)` is True.
- `test_budget_guard_no_cap`: Assert `BudgetGuard(main_tokens=0).would_exceed_cap(1000)` is False (no division by zero).
- `test_thompson_sampling_hit_increases_alpha`: After `update_prior(..., hit=True)`, `speculation_priors.beta_alpha` increments by 1.
- `test_thompson_sampling_miss_increases_beta`: After `update_prior(..., hit=False)`, `speculation_priors.beta_beta` increments by 1.
- `test_fallback_similarity_identical`: `_fallback_similarity("bash ls", "bash ls")` returns 1.0.
- `test_draft_security_block`: Mock `security_scan_fn` returning False; assert resulting Draft has `security_blocked=True` and `tokens_used=0`.
- `test_draft_timeout`: Mock `infer_fn` sleeping longer than `timeout_s`; assert Draft has `timed_out=True`.
- `test_speculation_disabled_when_human_approval`: Assert `_run_iteration_speculative()` returns warning and falls back to sequential when `approval == 'human'`.
- `test_no_speculation_below_latency_threshold`: With `est_latency=200, min_tool_latency_ms=500`, assert `should_speculate=False`.
- `test_spec_miss_falls_back_to_sequential`: Mock all drafts with similarity 0.3; assert SpeculationResult.outcome == 'miss' and a fresh infer call is made.
- `test_spec_hit_skips_infer`: Mock draft #0 with similarity 0.9; assert SpeculationResult.outcome == 'hit' and no fresh infer call.

### 11.2 Integration Tests

**File:** `tests/test_speculative_integration.py`

- `test_loop_speculative_end_to_end`: Start a loop with `--speculative` on a fixture goal with a mocked tool that sleeps 1s; assert at least one `speculative_attempts` row with `outcome IN ('hit', 'miss')` is written.
- `test_prior_persists_across_sessions`: Run two speculative loops sequentially; assert `speculation_priors.beta_alpha + beta_beta` in second loop equals first loop's final values.
- `test_budget_cap_stops_speculation`: Set `max_overhead_multiplier=1.0`; run loop; assert after first iteration, subsequent iterations have `outcome='budget_cap'` in `speculative_attempts`.
- `test_tool_latency_recorded`: After any tool call in speculative mode, assert `tool_latencies` table has a new row for that `tool_name`.
- `test_speculative_stats_output`: After a speculative loop, run `tag loop speculative-stats`; assert stdout contains "hit" and "miss" columns and numeric values.
- `test_zero_overhead_when_disabled`: Benchmark 5 iterations without `--speculative`; assert no rows in `speculative_attempts` are written.

### 11.3 Performance Tests

**File:** `tests/perf/test_speculative_perf.py`

- `test_draft_generation_does_not_block_tool_result`: Instrument with `time.monotonic()`; assert tool result is processed within 15 ms of the mock tool subprocess completing, regardless of draft count.
- `test_embedding_cold_start_under_3s`: Call `_get_embed_model()` from cold start; assert completion time < 3 s.
- `test_spec_loop_walltime_vs_sequential`: Run 10-iteration loop with mock 2s tool against speculative (draft_cap=3) and sequential; assert speculative wall time is ≤ sequential wall time × 0.85 (15% improvement minimum on synthetic workload with 40% hit rate).
- `test_sqlite_write_concurrency`: Simulate 10 concurrent draft writers to `speculative_attempts`; assert no `OperationalError: database is locked` under WAL mode with `busy_timeout=5000`.

---

## 12. Acceptance Criteria

| ID | Criterion | How Tested |
|----|-----------|------------|
| AC-01 | `tag loop start --speculative --goal X --profile P` writes at least one row to `speculative_attempts` for a goal that invokes a tool with latency > 500 ms. | Integration test |
| AC-02 | Without `--speculative`, `tag loop start` writes zero rows to `speculative_attempts` and the wall-time benchmark shows no statistically significant difference from pre-feature baseline. | Performance test + unit test |
| AC-03 | A speculation hit (similarity ≥ 0.85) results in no additional planning inference call for that iteration; `loop_iterations.spec_outcome = 'hit'` is set. | Unit test with mocked infer_fn call counter |
| AC-04 | A speculation miss results in a standard planning inference call; `loop_iterations.spec_outcome = 'miss'` is set; loop continues normally. | Unit test |
| AC-05 | `security.py`'s `scan_prompt()` is called exactly once per draft; a draft with detected secrets has `security_blocked = TRUE` in `speculative_attempts` and is never sent to the inference API. | Unit test with mocked security_scan_fn |
| AC-06 | When cumulative `spec_tokens / main_tokens > max_overhead_multiplier`, all subsequent iterations in the same loop have `spec_outcome = 'budget_cap'` and no draft inference calls are made. | Integration test |
| AC-07 | `tag loop speculative-stats --profile coder` output includes: total attempts, hit count, hit percentage, average latency saved (ms), and token overhead ratio. All values match aggregates from `speculative_attempts` table. | Integration test with known fixture data |
| AC-08 | `tag loop show <loop-id>` output includes `spec_outcome` and `spec_similarity` columns for each iteration when the loop was run in speculative mode. | Integration test |
| AC-09 | After 20 speculation attempts with 10 hits, `speculation_priors.beta_alpha = 11.0` and `speculation_priors.beta_beta = 11.0` for the corresponding (profile, tool_name, action_type) row. | Unit test |
| AC-10 | `tag loop start --speculative --approval human` emits a warning and runs in sequential mode; no `speculative_attempts` rows are written. | Unit test |
| AC-11 | Draft inference calls respect the `speculation.draft_timeout_s` (default 30 s); drafts that time out are marked `outcome = 'timeout'` in `speculative_attempts` and dropped from the scoring pass. | Unit test with mock timeout |
| AC-12 | The SentenceBERT model is loaded at most once per process lifetime; a second speculative iteration in the same loop does not reload the model. | Unit test asserting `_embed_model is not None` after first call and identity equality after second call |
| AC-13 | `tag config set speculation.draft_cap 3` is reflected in `SpecConfig.draft_cap` for the next loop start; at most 3 draft tasks are launched per tool wait. | Integration test checking `len(spec_result.drafts) <= 3` |
| AC-14 | Tool latency history is accumulated in `tool_latencies` across multiple loop sessions; `estimate_tool_latency_ms()` returns the P50 of the last 100 recorded durations for that tool. | Unit test with synthetic duration list |
| AC-15 | `tag submit --speculative --prompt "..."` activates speculative mode for a single multi-tool prompt and writes results to `speculative_attempts` with `loop_id = NULL`. | Integration test |

---

## 13. Dependencies

| Dependency | Type | Version | Notes |
|------------|------|---------|-------|
| `sentence-transformers` | Python package | ≥ 2.2.0 | Already required by PRD-043 (tool_retrieval.py). Lazy-loaded; absent package triggers fallback to `difflib` similarity. |
| `asyncio` | Python stdlib | ≥ 3.10 | Used for concurrent draft generation. Already available in TAG's runtime environment. |
| `concurrent.futures` | Python stdlib | ≥ 3.10 | `ThreadPoolExecutor` for tool subprocess in async context. |
| `PRD-013` (tracing.py) | Internal | current | New span types added; requires `open_span()` / `close_span()` API. |
| `PRD-012` (budget.py) | Internal | current | `check_budget()` called before each draft batch. |
| `PRD-034` (security.py) | Internal | current | `scan_prompt()` called on every speculative prompt. |
| `PRD-043` (tool_retrieval.py) | Internal | current | SentenceBERT model instance reuse via `get_embed_model()`. |
| `PRD-028` (sandbox.py) | Internal | current | Sandbox tool calls must be observable by the tool latency recorder; no structural change needed. |
| `PRD-027` (eval_framework.py) | Internal | current | Speculative loop runs are eval-able as first-class variants; `eval_results` entries carry `speculative=true` tag. |
| GitHub Issue #349 | External | — | Tracks this feature; acceptance criteria map to issue milestones. |

---

## 14. Open Questions

| # | Question | Owner | Target Resolution |
|---|----------|-------|-------------------|
| OQ-01 | Should draft generation use a smaller/cheaper model (e.g., Haiku vs. Sonnet) for the speculative path to reduce cost overhead? The cluster research context mentions "draft model" but this PRD assumes same-model drafts for simplicity. A `speculation.draft_model` config key could allow override. | Engine team | Before implementation start |
| OQ-02 | The similarity threshold 0.85 is chosen from the SPAgent paper defaults. Is this threshold appropriate for TAG's specific tool-call action space (shell commands, web search queries)? Should it be calibrated per tool_name? | ML team | After initial integration test data available |
| OQ-03 | Should the `tool_latencies` table be shared across profiles, or keyed per-profile? The same tool (`bash`) may behave differently in different profiles (e.g., `coder` profile runs heavier grep patterns than `researcher` profile). Currently keyed per-profile (FR-17). | Architecture | Before DB schema freeze |
| OQ-04 | When speculative mode is active and the budget cap is reached mid-loop, should the remaining iterations complete sequentially (current design) or should the loop abort with an informational message? Sequential fallback is safer but reduces predictability. | UX | Before FR-11 implementation |
| OQ-05 | `_classify_action()` uses a keyword heuristic. Should this be replaced with a SentenceBERT-based classifier trained on TAG's historical `loop_iterations.output` data to improve Thompson sampling key quality? | ML team | Post-v1 enhancement |
| OQ-06 | Is `K=5` drafts the right cap for `idlespec.draft_cap`? The IdleSpec paper uses K=5 with Beta(1,1) prior. For very fast (2-3s) tools, K=3 may be more cost-efficient. Should the cap be dynamically computed as `min(5, floor(est_latency_s / avg_draft_time_s))`? | Engine team | Before implementation start |
| OQ-07 | The `speculative_attempts` table will grow unboundedly. Should there be a retention policy (e.g., delete rows older than 90 days, keep only the last 1000 rows per profile)? The `tool_latencies` table has the same issue. | Infrastructure | Before GA release |
| OQ-08 | `tag submit --speculative` with a single-tool prompt will never trigger speculation (no tool gap to exploit). Should the CLI warn the user, or silently ignore `--speculative` for single-tool prompts? | UX | Before CLI surface freeze |

---

## 15. Complexity and Timeline

**Total estimate: L (2–4 weeks)**

### Phase 1: Schema and Infrastructure (Days 1–4)

- Add `speculative_attempts`, `speculation_priors`, and `tool_latencies` DDL to `loop_agent.py`'s `_open_db()` and a new `speculative.py`'s `ensure_schema()`.
- Implement `ALTER TABLE` migration for existing `loop_iterations` table.
- Add new OTel semantic convention constants to `otel_semconv.py`.
- Add `SpecConfig`, `Draft`, `SpeculationResult`, `BudgetGuard` dataclasses.
- Unit tests: dataclass defaults, schema creation, migration idempotency.

### Phase 2: Core Speculation Engine (Days 5–10)

- Implement `_build_draft_prompt()`, `_classify_action()`, `_cosine_sim()`, `_fallback_similarity()`.
- Implement `_generate_draft()` async coroutine with security scanning, timeout, and error handling.
- Implement `speculate_during_tool()` async orchestrator with tool-future concurrency.
- Implement `_sample_beta()` and `_rank_drafts_by_prior()` for Thompson sampling ordering.
- Unit tests: FR-03 through FR-09 coverage, mock infer_fn, mock security_scan_fn.

### Phase 3: Thompson Sampling Persistence (Days 11–13)

- Implement `update_prior()`, `load_priors()` with SQLite upsert pattern.
- Implement `estimate_tool_latency_ms()`, `record_tool_latency()`.
- Implement `_persist_speculation_result()` for batch row insertion.
- Unit tests: prior convergence test with 50 synthetic hit/miss sequence.

### Phase 4: `loop_agent.py` Integration (Days 14–17)

- Implement `_run_iteration_speculative()` wrapper in `loop_agent.py`.
- Wire `--speculative` CLI flag in `controller.py` `cmd_loop_start()`.
- Wire `--approval human` incompatibility guard.
- Wire `BudgetGuard` overhead cap enforcement (FR-11).
- Integration test: end-to-end loop with mocked tool subprocess.

### Phase 5: `tag submit --speculative` and Stats Command (Days 18–21)

- Extend `controller.py`'s `cmd_submit()` to pass `SpecConfig` when `--speculative` is set.
- Implement `cmd_loop_speculative_stats()` with `--loop-id`, `--profile`, `--since`, `--json` flags.
- Extend `tag loop show` output with speculation columns.
- Integration tests: stats command output format, `tag submit --speculative` end-to-end.

### Phase 6: Performance Validation and Security Review (Days 22–25)

- Performance tests: tool result processing latency, embedding cold start, SQLite write concurrency.
- Security review: secret leakage test with injected secrets in 100 synthetic prompts.
- Benchmark: 10-iteration loop wall time comparison speculative vs. sequential on CI hardware.
- Final acceptance criteria validation against AC-01 through AC-15.

### Phase 7: Documentation and Config Wiring (Days 26–28)

- Add `speculation.*` config keys to `tag config` schema and help text.
- Update `tag loop start --help` with speculative flag descriptions.
- Update `docs/prd/INDEX.md` with PRD-106 entry.
- Create entry in `CHANGELOG.md` under `Unreleased`.
