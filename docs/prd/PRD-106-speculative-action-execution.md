# PRD-106: Speculative Action Execution for Latency Reduction (SPAgent Pattern) (`tag loop start --speculative`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** L (2-4 weeks)
**Category:** Advanced Reasoning & Planning
**Affects:** `internal/agent` (inner + autoloop), `internal/tool` (tool dispatch), `internal/sandbox` (isolated speculative execution)
**Depends on:** PRD-027 (eval framework), PRD-028 (sandbox — isolated speculative execution), PRD-013 (agent tracing — speculation spans), PRD-034 (security — prompt content scanning before speculative dispatch), PRD-012 (budget enforcement — speculative cost accounting), PRD-043 (vector tool retrieval — next-action prediction via embedding retrieval), PRD-008 (background task queue — concurrent tool execution), PRD-041 (OTel span cost attribution — per-speculation cost tags)
**Inspired by:** SPAgent paper (2024), speculative decoding, prefill optimization

---

## 1. Overview

Every iteration of a TAG loop agent follows a strict sequential pattern: the agent calls a tool, waits for that tool's result, then plans and dispatches the next tool call. For tool calls with high wall-clock latency — web searches, code execution in sandboxed environments, file system traversals, external API calls — the agent is idle while waiting. This idle time is pure latency overhead: the model's inference capacity, the user's wall clock, and the loop's iteration budget are all consumed without progress.

Speculative execution is a well-established technique in computer architecture (branch prediction, out-of-order execution) and more recently in LLM inference (speculative decoding, draft-model prefill). The SPAgent paper (2024) applies the same insight to agentic tool-call chains: while a slow tool is executing, a lightweight prediction model estimates the most probable next action given the current context and partial observation history, and that predicted next action is dispatched speculatively. If the speculative prediction is correct once the real result arrives, the agent has effectively hidden the tool's latency behind concurrent planning work. If the prediction is wrong, the speculative result is discarded and the agent falls back to the standard sequential path.

The IdleSpec variant (exploited in this PRD) further refines the approach using Thompson sampling with a Beta(α, β) prior over historical prediction accuracy per (tool, action-type) pair. Rather than committing to a single speculative draft, IdleSpec generates up to K=5 draft continuations during the tool's wait time. When the real observation arrives, a synthesizer pass selects the best-matching draft — or falls back to a fresh inference if none match. This avoids the binary commit-or-rollback model and instead treats speculation as a warm-start for the planning step.

This feature integrates deeply with TAG's existing infrastructure. The `internal/agent` package owns the hand-rolled agent iteration loop (inner loop plus the PRD-021 autoloop); speculative execution layers on top of this loop as an optional execution mode activated by `--speculative`. All speculative attempts are recorded in a new `speculative_attempts` table in the single `internal/store` SQLite database for cost attribution, accuracy measurement, and Thompson sampling prior updates. The `internal/obs` package receives new span types (`speculative.draft`, `speculative.verify`, `speculative.hit`, `speculative.miss`) emitted through `go.opentelemetry.io/otel` with pinned `gen_ai.*` semantic conventions. Budget enforcement in `internal/obs` accounts for speculative token spend separately from main-path spend, with a configurable multiplier cap. Secret scanning in `internal/security` runs on speculative prompts before dispatch to prevent secret leakage into parallel inference paths.

The feature is activated with `--speculative` on `tag loop start` and `tag submit`. When disabled (the default), zero overhead is added to the standard sequential path. When enabled, the system automatically degrades to sequential mode for any tool call whose expected latency (estimated from historical `tool_latencies` data) is below a configurable threshold, ensuring speculation overhead does not exceed the latency savings for fast tools.

---

## 2. Problem Statement

### 2.1 Sequential Tool-Call Chains Waste Idle Wait Time

In a typical multi-step agent loop, 40–70% of wall-clock time is consumed waiting for tool results. A `tag loop start --goal "audit this codebase for SQL injection"` task on a medium-sized repository will invoke tools in chains like: `bash("find . -name '*.py'")` → `bash("grep -n 'cursor.execute' ...")` → `bash("cat file.py")` → `bash("wc -l ...")`. Each shell command completes in under 200 ms, but a web search or sandbox code execution can take 3–15 seconds. During that wait, the loop agent goroutine is blocked reading the tool subprocess result (`internal/tool` `os/exec` `CommandContext`) with no productive work occurring. Multiplied across a 10-iteration loop with 3 tool calls per iteration, this yields 30 sequential idle periods — each a wasted opportunity to advance planning.

The standard mitigation (reducing tool call timeout, caching results, using faster tools) is orthogonal to the planning latency problem: even if every tool were instantaneous, the model still needs inference time to produce its next action. But inference time and tool latency overlap almost perfectly in timeline — the model cannot start planning until it sees the tool result, and tool execution cannot begin until the model finishes planning the previous step. Speculative execution breaks this dependency by placing a probabilistic bet on the next action while the current tool runs.

### 2.2 No Historical Accuracy Signal for Next-Action Prediction

TAG currently has no mechanism to learn from past loop runs which next actions tend to follow which current actions. The `loop_iterations` table stores every iteration's input and output, but there is no structured extraction of tool call sequences, no measurement of prediction accuracy for any speculative attempt, and no per-(tool, action-type) accuracy prior. This means any speculative execution system built today would need to start from a uniform prior (equal probability for all next actions), which reduces speculation effectiveness on the first few runs of a given goal type and profile combination.

The `internal/toolindex` package (PRD-043) already embeds tool descriptions behind the `internal/memory` `Embedder` interface for semantic retrieval; this infrastructure can be extended to embed tool-call sequences and retrieve historically similar continuations. The `internal/memory` semantic store keeps cross-session context; it can be queried for prior loop execution patterns matching the current goal. Neither integration exists today, leaving the agent with no historical signal to inform speculative dispatch.

### 2.3 Latency Reduction Has No Measurable Cost-Benefit Tracking

Tag users who care about agent loop latency have no mechanism today to measure how much time is spent in tool-wait vs. model-inference vs. planning overhead. The `internal/obs` span records show start/end times for individual operations, but there is no aggregated view of "% of loop wall time spent waiting for tools" vs. "% spent in model inference." Without this baseline, it is impossible to know whether speculative execution is providing benefit on a given workload, or whether speculation overhead (extra inference calls for draft generation) is consuming more tokens than it saves in latency.

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
| G10 | `internal/security` scans every speculative prompt before dispatch with the same secret-detection logic applied to primary prompts. |
| G11 | When speculation is active but the `internal/obs` budget gate projects that the next speculative batch would exceed the overhead cap, speculation degrades gracefully to sequential mode for that iteration only. |
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
| Secret leakage prevention | Zero secrets detected in speculative prompts in security audit | `internal/security` scan on 100 synthetic prompts with injected secrets |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer running long agent loops | use `tag loop start --goal "refactor this codebase" --speculative --profile coder` | My 10-iteration loop finishes faster by overlapping tool wait time with planning for the next step |
| U2 | Platform engineer | see `tag loop speculative-stats --profile coder` in CI output | I can measure whether speculative execution is actually saving latency on our production workloads, not just in theory |
| U3 | Developer on a budget | set `speculation.max_overhead_multiplier = 1.2` in my config | The system never spends more than 20% extra tokens on speculative drafts that might be discarded |
| U4 | Agent loop power user | run `tag submit --speculative --prompt "search for X, then summarize findings"` | A single multi-tool submit also benefits from speculative planning without needing to set up a full loop |
| U5 | Security-conscious team | know that `internal/security` scans speculative prompts exactly as it scans primary prompts | Sensitive context from tool results cannot leak into speculative parallel API calls without the same secret-detection checks |
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
| FR-04 | Draft generation runs as goroutines coordinated by an `golang.org/x/sync/errgroup.Group`, each draft under its own `context.WithCancel` child branch; the agent loop goroutine blocks only on the tool-result channel, never on draft generation. When the tool result arrives, the branch contexts of any still-pending drafts are cancelled. | Must |
| FR-05 | When the tool result arrives, each completed draft is scored against the real observation using cosine similarity of their embeddings (produced through the `internal/memory` `Embedder` interface; scoring is an in-Go brute-force cosine loop). The draft with the highest similarity score above `speculation.similarity_threshold` (default 0.85) is selected as a speculation hit. | Must |
| FR-06 | On a speculation hit, the selected draft's planned next action is extracted and used as the agent's next tool call, skipping a full planning inference call. The hit draft's action string is logged to `loop_iterations.speculative_action_used = TRUE`. | Must |
| FR-07 | On a speculation miss (no draft exceeds the similarity threshold), the system performs a standard planning inference call with the full context including the real tool result. The miss is logged to `speculative_attempts` with `outcome = 'miss'`. | Must |
| FR-08 | Thompson sampling: after each attempt, the `speculative_attempts` table's per-(profile, tool_name, action_type) `beta_alpha` and `beta_beta` columns are updated: `+1` to `beta_alpha` on hit, `+1` to `beta_beta` on miss. | Must |
| FR-09 | Draft ordering for the synthesizer pass uses Thompson sampling: each (tool, action_type) pair's expected accuracy is sampled from a Beta(alpha, beta) variate (`gonum.org/v1/gonum/stat/distuv.Beta`); drafts are generated in descending order of sampled accuracy, so the most historically accurate (tool, action_type) pairing gets the first draft slot. | Should |
| FR-10 | `internal/security`'s prompt scanner (`Scan(ctx, prompt)`) is called on every speculative prompt before the draft inference call is dispatched. If secrets are detected, the draft is suppressed and a warning is logged to the tracing span without aborting the main tool execution. | Must |
| FR-11 | Budget enforcement: before launching each batch of draft goroutines, `internal/obs`'s budget gate is consulted with estimated draft tokens. If the overhead multiplier `(accumulated_spec_tokens / accumulated_main_tokens)` would exceed `speculation.max_overhead_multiplier`, speculation is disabled for the current iteration and a `speculation.budget_cap_reached` span event is emitted. | Must |
| FR-12 | All speculative attempts are persisted to the `speculative_attempts` table (schema defined in Section 9) through the single `internal/store` writer (modernc.org/sqlite, WAL mode, `_busy_timeout=5000`). | Must |
| FR-13 | `internal/obs` receives the following new span types: `speculative.draft_batch` (parent), `speculative.draft` (child per draft), `speculative.verify` (similarity scoring pass), `speculative.hit`, `speculative.miss`. All carry OTel attributes: `spec.draft_index`, `spec.similarity_score`, `spec.tokens_used`, `spec.outcome`. | Must |
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
| NFR-01 | **Zero overhead when disabled.** Because the whole feature compiles into the single static binary, zero-overhead is a runtime guard, not an import guard: the speculative path is behind an early `if !cfg.Enabled { return runSequential(...) }` branch, so no draft goroutines, embedder, or Thompson-sampling state are allocated when `--speculative` is absent. | Verified by a `testing.B` benchmark asserting no extra goroutines/allocs vs. the sequential path |
| NFR-02 | **Draft generation must not block the tool result handler.** All draft inference calls run as goroutines; the loop goroutine only selects on the tool-result channel (the tool runs via `internal/tool` `os/exec` `CommandContext` with process-group kill) and is never delayed by draft generation. | Verified by timing test: tool result must be processed within 10 ms of the tool goroutine completing |
| NFR-03 | **SQLite write contention.** Speculative attempt writes to `speculative_attempts` must not block iteration reads. They go through the single `internal/store` writer (WAL mode, `_busy_timeout=5000` on the modernc.org/sqlite DSN). | Verified by concurrent write test with 10 simultaneous draft writers |
| NFR-04 | **Memory footprint.** Draft text strings are discarded immediately after verification; only the selected draft's action string is retained. Total in-memory draft storage must be under 50 KB per iteration batch. | Verified by `runtime.ReadMemStats` / `testing.AllocsPerRun` assertion in unit test |
| NFR-05 | **Draft inference timeout.** Each individual draft goroutine runs under a `context.WithTimeout` of `min(tool_latency_estimate * 0.9, 30s)`. Timed-out drafts are silently dropped (context cancellation); the batch completes with however many drafts finished. | Verified by mock test with injected timeout |
| NFR-06 | **Embedder cold start.** The `internal/memory` `Embedder` used for draft similarity scoring is initialized lazily on the first speculative attempt and cached for the process lifetime. When an offline embedder (build-tag `cybertron` MiniLM) is compiled in, cold start must complete within 3 seconds. | Verified by timing test on CI hardware |
| NFR-07 | **Graceful degradation on provider error.** If draft `Stream(ctx, Request)` calls fail (rate limit, network error, provider unavailable), the system falls back to sequential planning with a `speculation.api_error` span event. The loop never fails due to speculation errors. | Verified by unit test with a mocked provider returning errors |
| NFR-08 | **Token cost transparency.** `--speculative` sessions display running speculation overhead in the progress line (e.g., `spec: 3 hits, 0.13× overhead`) so users can see live cost impact. | Verified by output format test |
| NFR-09 | **Beta prior persistence.** `beta_alpha` and `beta_beta` values in `speculative_attempts` accumulate across sessions; they are not reset between `tag loop start` invocations. Querying the current prior requires a `SUM(hit)` / `SUM(1-hit)` aggregation. | Verified by integration test across two loop sessions |
| NFR-10 | **Profile isolation.** Thompson sampling priors are keyed by `(profile, tool_name, action_type)`. Runs under the `coder` profile do not affect the `researcher` profile's priors, and vice versa. | Verified by unit test with cross-profile prior assertions |

---

## 9. Technical Design

### 9.1 New Package Layout

Speculation logic lives in `internal/agent/speculative.go` (orchestration + draft scoring), reusing:

- `internal/tool` — tool dispatch via `os/exec` `CommandContext` with `Setpgid` process-group kill and output caps; the tool call under speculation runs here.
- `internal/sandbox` — the isolation ladder under which the *committed* action executes; speculative drafts never touch the filesystem (see §10).
- `internal/llm` — the provider-neutral `Stream(ctx, Request) -> <-chan Event` interface for draft-generation inference.
- `internal/memory` — the `Embedder` interface (provider API default, build-tag offline MiniLM) used for draft/observation embeddings, scored with an in-Go cosine loop.
- `internal/store` — the single modernc.org/sqlite writer for the new tables.
- `internal/obs` — OTel spans + budget gate.

The `internal/agent` inner loop calls into `speculative.go` only when `cfg.Enabled` is true; the sequential path is otherwise untouched (a runtime branch, not a build-time or import-time switch).

### 9.2 SQLite DDL

The following tables are added to the `internal/store` migration set (applied by `db.go`'s migrator against the single modernc.org/sqlite store):

```sql
-- Persists per-attempt outcomes for Thompson sampling and reporting.
CREATE TABLE IF NOT EXISTS speculative_attempts (
  id              TEXT PRIMARY KEY,          -- 12-char hex id (github.com/google/uuid)
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

Migration is a numbered migration in `internal/store/migrate/`; the added `loop_iterations` columns are applied with `ALTER TABLE loop_iterations ADD COLUMN ...`, each guarded by a `PRAGMA table_info(loop_iterations)` probe (SQLite has no `ADD COLUMN IF NOT EXISTS`), consistent with the single-writer migration convention.

### 9.3 Core Structs

```go
// internal/agent/speculative.go
package agent

import (
	"sync"
	"time"
)

// SpecConfig is the runtime configuration for one speculative session.
type SpecConfig struct {
	Enabled               bool
	DraftCap              int           // default 5
	MinToolLatency        time.Duration // default 500ms
	MaxOverheadMultiplier float64       // default 1.5
	SimilarityThreshold   float64       // default 0.85
	DraftTemperature      float64       // default 0.3
	DraftTimeout          time.Duration // default 30s
	DefaultLatencyEst     time.Duration // default 800ms
}

// DefaultSpecConfig returns the documented defaults (disabled).
func DefaultSpecConfig() SpecConfig {
	return SpecConfig{
		DraftCap:              5,
		MinToolLatency:        500 * time.Millisecond,
		MaxOverheadMultiplier: 1.5,
		SimilarityThreshold:   0.85,
		DraftTemperature:      0.3,
		DraftTimeout:          30 * time.Second,
		DefaultLatencyEst:     800 * time.Millisecond,
	}
}

// Draft is one speculative continuation generated during a tool's wait time.
type Draft struct {
	Index           int
	ActionType      string    // coarse label from classifyAction()
	ActionText      string    // the full predicted next tool call string
	TokensUsed      int
	Embedding       []float32 // populated by the Embedder
	Similarity      float64   // populated by scoreDraft(); -1 = unscored
	TimedOut        bool
	SecurityBlocked bool
}

// SpeculationResult is the outcome of one speculation batch (one tool-call wait).
type SpeculationResult struct {
	ToolName       string
	ToolLatency    time.Duration
	Drafts         []Draft
	Selected       *Draft
	Outcome        string        // "hit","miss","skipped","disabled","budget_cap"
	LatencySaved   time.Duration // hit only
	TotalSpecToks  int
}

// BudgetGuard tracks speculation token spend relative to main-path spend.
// Safe for concurrent draft goroutines updating SpecTokens under mu.
type BudgetGuard struct {
	mu         sync.Mutex
	MainTokens int
	SpecTokens int
	Cap        float64 // default 1.5
}

func (b *BudgetGuard) OverheadRatio() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.MainTokens == 0 {
		return 0
	}
	return float64(b.SpecTokens) / float64(b.MainTokens)
}

func (b *BudgetGuard) WouldExceedCap(additionalSpecTokens int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.MainTokens == 0 {
		return false
	}
	return float64(b.SpecTokens+additionalSpecTokens)/float64(b.MainTokens) > b.Cap
}
```

### 9.4 Core Algorithm: `SpeculateDuringTool()`

The Python `asyncio`/`concurrent.futures` design maps to goroutines + channels + `errgroup` + per-branch `context.WithCancel`: the tool runs in one goroutine and returns its observation on a channel; each draft runs in its own goroutine under a cancellable child context; when the observation arrives, the losing draft branches are cancelled.

```go
// internal/agent/speculative.go  (simplified; full impl adds tracing, security, budget)

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
	"gonum.org/v1/gonum/stat/distuv"

	"github.com/tag-agent/tag/internal/llm"
	"github.com/tag-agent/tag/internal/memory"
)

// InferFunc runs one draft-generation call through internal/llm and returns the
// accumulated action text plus tokens used. Bound by the caller to a profile.
type InferFunc func(ctx context.Context, prompt string, temperature float64) (string, int, error)

// ScanFunc is internal/security's prompt scanner; returns false if secrets found.
type ScanFunc func(ctx context.Context, prompt string) bool

func cosine(a, b []float32) float64 {
	var dot, ma, mb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		ma += float64(a[i]) * float64(a[i])
		mb += float64(b[i]) * float64(b[i])
	}
	if ma == 0 || mb == 0 {
		return 0
	}
	return dot / (math.Sqrt(ma) * math.Sqrt(mb))
}

func buildDraftPrompt(context, toolCall string) string {
	return context + "\n\n[AWAITING TOOL RESULT FOR]: " + toolCall +
		"\n\nPredict the single most likely next action this agent will take " +
		"after the tool returns a typical successful result. " +
		"Output only the next tool call or final answer, no explanation."
}

func classifyAction(actionText string) string {
	l := strings.ToLower(actionText)
	for _, kv := range []struct{ kw, label string }{
		{"grep", "grep"}, {"cat ", "file_read"}, {"find ", "file_find"},
		{"wc ", "file_stat"}, {"web_search", "web_search"},
		{"sandbox", "sandbox_exec"}, {"goal_achieved", "goal_achieved"},
	} {
		if strings.Contains(l, kv.kw) {
			return kv.label
		}
	}
	return "other"
}

// sampleBeta draws a Thompson-sampling variate from Beta(alpha, beta).
func sampleBeta(alpha, beta float64) float64 {
	return distuv.Beta{Alpha: alpha, Beta: beta}.Rand()
}

type prior struct{ alpha, beta float64 }

// rankDraftsByPrior returns (slotIndex, actionType) ordered by sampled accuracy.
func rankDraftsByPrior(draftCap int, actionTypes []string, priors map[string]prior) []struct {
	idx   int
	aType string
} {
	if len(actionTypes) == 0 {
		actionTypes = []string{"other"}
	}
	type scored struct {
		acc   float64
		idx   int
		aType string
	}
	out := make([]scored, 0, draftCap)
	for i := 0; i < draftCap; i++ {
		at := actionTypes[i%len(actionTypes)]
		p, ok := priors[at]
		if !ok {
			p = prior{1, 1}
		}
		out = append(out, scored{sampleBeta(p.alpha, p.beta), i, at})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].acc > out[j].acc })
	ranked := make([]struct {
		idx   int
		aType string
	}, len(out))
	for i, s := range out {
		ranked[i] = struct {
			idx   int
			aType string
		}{s.idx, s.aType}
	}
	return ranked
}

func generateDraft(ctx context.Context, index int, prompt string, cfg SpecConfig,
	infer InferFunc, scan ScanFunc) Draft {

	if !scan(ctx, prompt) {
		return Draft{Index: index, ActionType: "other", SecurityBlocked: true}
	}
	dctx, cancel := context.WithTimeout(ctx, cfg.DraftTimeout)
	defer cancel()
	text, tokens, err := infer(dctx, prompt, cfg.DraftTemperature)
	if err != nil { // includes context deadline / cancellation
		return Draft{Index: index, ActionType: "other", TimedOut: true}
	}
	return Draft{Index: index, ActionType: classifyAction(text), ActionText: text, TokensUsed: tokens}
}

// SpeculateDuringTool launches speculative draft goroutines concurrently with the
// tool goroutine, then scores completed drafts against the real observation.
func SpeculateDuringTool(
	ctx context.Context,
	agentContext, toolCall, toolName string,
	toolCh <-chan string, // tool goroutine sends its observation here
	cfg SpecConfig,
	infer InferFunc,
	scan ScanFunc,
	embed memory.Embedder,
	priors map[string]prior,
	budget *BudgetGuard,
) (string, SpeculationResult) {

	draftPrompt := buildDraftPrompt(agentContext, toolCall)
	estTokens := (len(draftPrompt) / 4) * cfg.DraftCap // 4 chars/token heuristic

	if budget.WouldExceedCap(estTokens) {
		obs := <-toolCh // wait synchronously; no speculation
		return obs, SpeculationResult{ToolName: toolName, Outcome: "budget_cap"}
	}

	actionTypes := make([]string, 0, len(priors))
	for at := range priors {
		actionTypes = append(actionTypes, at)
	}
	ranked := rankDraftsByPrior(cfg.DraftCap, actionTypes, priors)

	// Per-branch cancellation: cancelling draftCtx aborts losing drafts once the
	// tool result is in hand.
	draftCtx, cancelDrafts := context.WithCancel(ctx)
	defer cancelDrafts()

	drafts := make([]Draft, len(ranked))
	var g errgroup.Group
	for i, slot := range ranked {
		i, slot := i, slot
		g.Go(func() error {
			drafts[i] = generateDraft(draftCtx, slot.idx, draftPrompt, cfg, infer, scan)
			return nil
		})
	}

	t0 := time.Now()
	obs := <-toolCh // block only on the tool result
	toolLatency := time.Since(t0)

	cancelDrafts() // cancel any still-pending drafts
	_ = g.Wait()   // collect whatever finished; cancelled ones are marked TimedOut

	// Score completed drafts against the real observation.
	obsEmb, _ := embed.Embed(ctx, obs)
	var selected *Draft
	total := 0
	for i := range drafts {
		d := &drafts[i]
		total += d.TokensUsed
		if d.TimedOut || d.SecurityBlocked || d.ActionText == "" {
			d.Similarity = -1
			continue
		}
		emb, _ := embed.Embed(ctx, d.ActionText)
		d.Embedding = emb
		d.Similarity = cosine(obsEmb, emb)
		if d.Similarity >= cfg.SimilarityThreshold &&
			(selected == nil || d.Similarity > selected.Similarity) {
			selected = d
		}
	}

	budget.mu.Lock()
	budget.SpecTokens += total
	budget.mu.Unlock()

	res := SpeculationResult{
		ToolName: toolName, ToolLatency: toolLatency,
		Drafts: drafts, Selected: selected, TotalSpecToks: total, Outcome: "miss",
	}
	if selected != nil {
		res.Outcome = "hit"
		res.LatencySaved = toolLatency
	}
	return obs, res
}
```

### 9.5 Thompson Sampling Prior Update

All persistence goes through the single `internal/store` writer; `Store` wraps the `*sql.DB` (modernc.org/sqlite). SQLite's `ON CONFLICT ... DO UPDATE` upsert ports verbatim.

```go
// internal/agent/priors.go
package agent

import (
	"context"
	"time"

	"github.com/tag-agent/tag/internal/store"
)

// UpdatePrior increments the Beta(alpha, beta) prior in speculation_priors.
func UpdatePrior(ctx context.Context, s *store.Store, profile, toolName, actionType string, hit bool) error {
	key := profile + "|" + toolName + "|" + actionType
	now := time.Now().UTC().Format(time.RFC3339)
	dAlpha, dBeta := 0.0, 1.0
	if hit {
		dAlpha, dBeta = 1.0, 0.0
	}
	return s.Exec(ctx, `
		INSERT INTO speculation_priors(id, profile, tool_name, action_type,
		                               beta_alpha, beta_beta, updated_at)
		VALUES (?, ?, ?, ?, 1.0+?, 1.0+?, ?)
		ON CONFLICT(profile, tool_name, action_type) DO UPDATE SET
			beta_alpha = beta_alpha + ?,
			beta_beta  = beta_beta  + ?,
			updated_at = ?`,
		key, profile, toolName, actionType, dAlpha, dBeta, now,
		dAlpha, dBeta, now)
}

// LoadPriors returns action_type -> prior{alpha, beta} for Thompson sampling.
func LoadPriors(ctx context.Context, s *store.Store, profile, toolName string) (map[string]prior, error) {
	rows, err := s.Query(ctx, `
		SELECT action_type, beta_alpha, beta_beta
		FROM speculation_priors WHERE profile = ? AND tool_name = ?`, profile, toolName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]prior{}
	for rows.Next() {
		var at string
		var a, b float64
		if err := rows.Scan(&at, &a, &b); err != nil {
			return nil, err
		}
		out[at] = prior{a, b}
	}
	return out, rows.Err()
}
```

### 9.6 Tool Latency Estimation

```go
// EstimateToolLatency returns the P50 historical latency for (profile, tool), or def.
func EstimateToolLatency(ctx context.Context, s *store.Store, profile, toolName string, def time.Duration) (time.Duration, error) {
	rows, err := s.Query(ctx, `
		SELECT duration_ms FROM tool_latencies
		WHERE profile = ? AND tool_name = ?
		ORDER BY created_at DESC LIMIT 100`, profile, toolName)
	if err != nil {
		return def, err
	}
	defer rows.Close()
	var d []int
	for rows.Next() {
		var ms int
		if err := rows.Scan(&ms); err != nil {
			return def, err
		}
		d = append(d, ms)
	}
	if len(d) == 0 {
		return def, nil
	}
	sort.Ints(d)
	return time.Duration(d[len(d)/2]) * time.Millisecond, nil
}

// RecordToolLatency appends one observation to tool_latencies (google/uuid id).
func RecordToolLatency(ctx context.Context, s *store.Store, loopID, profile, toolName string, dur time.Duration) error {
	return s.Exec(ctx, `
		INSERT INTO tool_latencies(id, loop_id, profile, tool_name, duration_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		newID12(), loopID, profile, toolName, dur.Milliseconds(),
		time.Now().UTC().Format(time.RFC3339))
}
```

### 9.7 Integration Point in `internal/agent`

The inner loop's `runIteration` gains a speculative variant. It is a clean wrapper that leaves the sequential path untouched (the `if !spec.Enabled` guard in NFR-01). The tool runs in its own goroutine that publishes its observation on a channel; drafts run concurrently (§9.4). Tool execution goes through `internal/tool` (`os/exec` `CommandContext`, process-group kill) under the `internal/sandbox` isolation ladder.

```go
// internal/agent/iteration.go — speculative variant

func (a *Agent) runIterationSpeculative(
	ctx context.Context,
	loopID string, iteration int, goal, profile, prevOutput string,
	spec SpecConfig, budget *BudgetGuard,
) (string, error) {

	prompt := a.buildPrompt(goal, iteration, prevOutput)

	// Phase 1: plan the first action (normal inference via internal/llm).
	planning, err := a.infer(ctx, prompt, profile)
	if err != nil {
		return planning, err
	}
	toolCall, toolName := extractToolCall(planning)
	if toolCall == "" {
		return planning, nil // no tool call, return as-is
	}

	est, _ := EstimateToolLatency(ctx, a.store, profile, toolName, spec.DefaultLatencyEst)
	shouldSpeculate := spec.Enabled &&
		est >= spec.MinToolLatency &&
		!budget.WouldExceedCap(spec.DraftCap*200) // rough estimate

	if !shouldSpeculate {
		// Standard sequential execution.
		t0 := time.Now()
		toolOut, err := a.tools.Run(ctx, toolCall) // internal/tool + internal/sandbox
		if err != nil {
			return toolOut, err
		}
		_ = RecordToolLatency(ctx, a.store, loopID, profile, toolName, time.Since(t0))
		return a.continueFromObservation(ctx, goal, iteration, planning, toolOut, profile)
	}

	// Speculative path: tool in its own goroutine, drafts concurrent.
	priors, _ := LoadPriors(ctx, a.store, profile, toolName)
	toolCh := make(chan string, 1)
	go func() {
		out, err := a.tools.Run(ctx, toolCall)
		if err != nil {
			out = "" // miss-forcing; error handled by caller path
		}
		toolCh <- out
	}()

	draftCtx := a.buildDraftContext(goal, iteration, prevOutput, planning)
	obs, res := SpeculateDuringTool(ctx, draftCtx, toolCall, toolName, toolCh, spec,
		func(c context.Context, p string, t float64) (string, int, error) {
			return a.inferRaw(c, p, profile, t) // internal/llm Stream(ctx,Request)
		},
		func(c context.Context, p string) bool { return a.security.Scan(c, p) },
		a.embedder, priors, budget)

	_ = RecordToolLatency(ctx, a.store, loopID, profile, toolName, res.ToolLatency)
	_ = a.persistSpeculationResult(ctx, loopID, iteration, profile, toolName, res)

	// Update Thompson-sampling priors for each scored draft.
	for i := range res.Drafts {
		d := res.Drafts[i]
		if d.TimedOut || d.SecurityBlocked || d.ActionText == "" {
			continue
		}
		isHit := res.Selected != nil && d.Index == res.Selected.Index
		_ = UpdatePrior(ctx, a.store, profile, toolName, d.ActionType, isHit)
	}

	if res.Outcome == "hit" && res.Selected != nil {
		budget.mu.Lock()
		budget.MainTokens += res.Selected.TokensUsed // already paid
		budget.mu.Unlock()
		return res.Selected.ActionText, nil // skip full planning inference
	}
	// Miss: full planning inference with the real observation.
	return a.continueFromObservation(ctx, goal, iteration, planning, obs, profile)
}
```

### 9.8 Tracing Integration

New OTel attribute-key constants added to `internal/obs` (registered alongside the pinned `gen_ai.*` semconv table, `SEMCONV_VERSION` 1.28.0); spans are created via `go.opentelemetry.io/otel`:

```go
// internal/obs/semconv_spec.go
const (
	SpecDraftCap       = "speculation.draft_cap"
	SpecDraftIndex     = "speculation.draft_index"
	SpecSimilarity     = "speculation.similarity_score"
	SpecOutcome        = "speculation.outcome" // hit | miss | skipped | disabled
	SpecTokens         = "speculation.tokens_used"
	SpecLatencySavedMs = "speculation.latency_saved_ms"
	SpecOverheadRatio  = "speculation.overhead_ratio"
	SpecToolName       = "speculation.tool_name"
	SpecActionType     = "speculation.action_type"
	SpecBetaAlpha      = "speculation.beta_alpha"
	SpecBetaBeta       = "speculation.beta_beta"
)
```

Span names follow the existing `tag.*` convention:
- `tag.speculation.draft_batch` — root span for one speculation batch
- `tag.speculation.draft` — one draft generation call
- `tag.speculation.verify` — embedding similarity scoring
- `tag.speculation.hit` — emitted when a draft is selected
- `tag.speculation.miss` — emitted when no draft meets threshold

### 9.9 Dependency on `internal/toolindex` / the `Embedder` interface

Draft/observation embeddings use the same `internal/memory` `Embedder` instance that `internal/toolindex` (PRD-043) uses for tool retrieval — a shared, lazily-initialized singleton rather than a second model load. By default `Embedder` calls a provider embedding API; the build-tag `cybertron` MiniLM backend gives a pure-Go offline embedder. Because a per-draft network embedding round-trip inside the latency window can erode the very latency it aims to hide, the hot scoring path prefers the offline embedder when compiled in, and otherwise falls back to an in-process string-overlap heuristic (a Go port of the `difflib.SequenceMatcher` ratio) so speculation never adds a network hop it cannot afford:

```go
// stringRatio: 0..1 overlap ratio; no network, no embedder.
func stringRatio(a, b string) float64 {
	a, b = strings.ToLower(a), strings.ToLower(b)
	// longest-common-subsequence ratio (SequenceMatcher.ratio() equivalent):
	// 2*M / (len(a)+len(b)), M = matched runes.
	m := lcsLen(a, b)
	if len(a)+len(b) == 0 {
		return 1
	}
	return 2 * float64(m) / float64(len(a)+len(b))
}
```

---

## 10. Security Considerations

1. **Secret leakage into speculative prompts.** Speculative prompts include the current agent context, which may contain partial tool results from prior iterations. `internal/security`'s `Scan(ctx, prompt)` must be called on every draft prompt before dispatch. If secrets are detected, the draft is suppressed (`SecurityBlocked=true`) and a `WARN` log entry is written. The main tool execution continues unaffected.

2. **Serialization risks.** Speculative embeddings are held as `[]float32` in memory only and are never persisted. The `speculative_attempts` table stores only text fields and numeric scores. Go has no `pickle`-equivalent unsafe-deserialization path here — no `gob`, `encoding/json` into `interface{}`, or `unsafe` decode of untrusted data is used. (Contrast with the LangGraph cache PRD-G6 pickle-RCE risk noted in cluster research context, which does not apply to a Go build.)

3. **Speculative action injection.** A hit draft's `action_text` is used as the next tool call. An adversarial tool result that manipulates speculative draft content could theoretically cause action injection. Mitigation: the synthesizer only accepts the draft's `action_text` string (planned next action) and passes it through the same tool-call validation path as any LLM-generated tool call. No raw draft content is executed without validation.

4. **API key exposure in draft prompts.** If the agent context includes API keys or bearer tokens (e.g., from environment variable injection), these may appear in draft prompts sent to the provider. Mitigation: `internal/security` secret scanning (item 1 above) and its existing prompt-redaction pattern set apply to speculative prompts identically to primary prompts.

5. **Rate limit amplification.** With `draft_cap=5`, speculative mode can quintuple the number of API calls per tool wait. This increases the risk of hitting per-minute rate limits on the configured model endpoint. Mitigation: `BudgetGuard.would_exceed_cap()` limits total speculation spend; individual draft timeouts prevent runaway concurrent calls; and the `speculation.max_overhead_multiplier` config key caps aggregate spend.

6. **SQLite WAL write amplification.** Each speculation attempt batch writes up to `draft_cap` rows to `speculative_attempts` plus one update to `speculation_priors`. For a 10-iteration loop with 5 drafts each, this is 50 extra writes per loop run. This is within SQLite WAL mode's safe concurrency limits for the usage pattern (the `internal/store` single-writer contract plus WAL reader concurrency). The `_busy_timeout=5000` pragma on the modernc.org/sqlite DSN prevents write contention failures.

7. **Inference isolation.** Draft inference calls use the same API endpoint and credentials as main-path inference. They are not sandboxed. If the main-path inference is protected by a firewall or network policy (PRD-094 egress firewall), draft inference calls must also comply with that policy. No special exemption is granted to speculative traffic.

---

## 11. Testing Strategy

### 11.1 Unit Tests

**File:** `internal/agent/speculative_test.go` (table-driven `testing` tests; provider, tool, embedder, and scanner are injected as interfaces with fakes).

- `TestSpecConfigDefaults`: assert `DefaultSpecConfig()` fields match the documented defaults.
- `TestCosine`: table of vectors incl. orthogonal `[1,0]/[0,1]` -> 0.0 and identical `[1,1]/[1,1]` -> 1.0.
- `TestClassifyAction`: table incl. `bash("grep -n ...")` -> `"grep"` and `"do something weird"` -> `"other"`.
- `TestBudgetGuardCap`: `(&BudgetGuard{MainTokens:1000, SpecTokens:1499, Cap:1.5}).WouldExceedCap(2)` is true.
- `TestBudgetGuardNoCap`: `(&BudgetGuard{MainTokens:0}).WouldExceedCap(1000)` is false (no divide-by-zero).
- `TestThompsonHitIncreasesAlpha`: after `UpdatePrior(..., hit=true)`, `speculation_priors.beta_alpha` increments by 1 (in-memory modernc.org/sqlite temp store).
- `TestThompsonMissIncreasesBeta`: after `UpdatePrior(..., hit=false)`, `beta_beta` increments by 1.
- `TestStringRatioIdentical`: `stringRatio("bash ls", "bash ls")` returns 1.0.
- `TestDraftSecurityBlock`: fake `ScanFunc` returning false; resulting `Draft.SecurityBlocked` is true and `TokensUsed==0`.
- `TestDraftTimeout`: fake `InferFunc` that blocks past `DraftTimeout` (respects `ctx`); `Draft.TimedOut` is true.
- `TestSpeculationDisabledWhenHumanApproval`: `runIterationSpeculative` warns and falls back to sequential when approval mode is `human`.
- `TestNoSpeculationBelowLatencyThreshold`: with `est=200ms, MinToolLatency=500ms`, `shouldSpeculate` is false.
- `TestSpecMissFallsBackToSequential`: all drafts scored 0.3; `Outcome=="miss"` and the fake provider records a fresh planning call.
- `TestSpecHitSkipsInfer`: draft #0 scored 0.9; `Outcome=="hit"` and no fresh planning call (provider call counter unchanged).

### 11.2 Integration Tests

**File:** `internal/agent/speculative_integration_test.go` (real `internal/store` temp DB; fake tool that sleeps 1s; fake provider).

- `TestLoopSpeculativeEndToEnd`: run a loop with `--speculative` on a fixture goal; assert ≥1 `speculative_attempts` row with `outcome IN ('hit','miss')`.
- `TestPriorPersistsAcrossSessions`: run two speculative loops against the same store; assert `beta_alpha + beta_beta` in the second equals the first loop's final values.
- `TestBudgetCapStopsSpeculation`: set `MaxOverheadMultiplier=1.0`; after the first iteration, subsequent iterations record `outcome='budget_cap'`.
- `TestToolLatencyRecorded`: after any tool call in speculative mode, `tool_latencies` has a new row for that `tool_name`.
- `TestSpeculativeStatsOutput`: after a speculative loop, `tag loop speculative-stats` stdout contains the `hit`/`miss` columns and numeric values.
- `TestZeroOverheadWhenDisabled`: run 5 iterations without `--speculative`; assert zero `speculative_attempts` rows.

### 11.3 Performance Tests / Benchmarks

**File:** `internal/agent/speculative_bench_test.go` (`testing.B`).

- `TestDraftGenDoesNotBlockToolResult`: instrument with `time.Now()`; assert the observation is handled within 15 ms of the fake tool goroutine sending on `toolCh`, regardless of draft count.
- `TestEmbedderColdStartUnder3s`: initialize the offline (`cybertron` build-tag) `Embedder` cold; assert < 3 s.
- `BenchmarkSpecVsSequentialWalltime`: 10-iteration loop with a fake 2s tool, speculative (`DraftCap=3`) vs. sequential; assert speculative wall time ≤ sequential × 0.85 (≥15% improvement on a synthetic 40%-hit workload).
- `TestSQLiteWriteConcurrency`: 10 concurrent draft writers to `speculative_attempts` through the single `internal/store` writer; assert no `SQLITE_BUSY` under WAL with `_busy_timeout=5000`.

---

## 12. Acceptance Criteria

| ID | Criterion | How Tested |
|----|-----------|------------|
| AC-01 | `tag loop start --speculative --goal X --profile P` writes at least one row to `speculative_attempts` for a goal that invokes a tool with latency > 500 ms. | Integration test |
| AC-02 | Without `--speculative`, `tag loop start` writes zero rows to `speculative_attempts` and the wall-time benchmark shows no statistically significant difference from pre-feature baseline. | Performance test + unit test |
| AC-03 | A speculation hit (similarity ≥ 0.85) results in no additional planning inference call for that iteration; `loop_iterations.spec_outcome = 'hit'` is set. | Unit test with a fake provider call counter |
| AC-04 | A speculation miss results in a standard planning inference call; `loop_iterations.spec_outcome = 'miss'` is set; loop continues normally. | Unit test |
| AC-05 | `internal/security`'s `Scan(ctx, prompt)` is called exactly once per draft; a draft with detected secrets has `security_blocked = TRUE` in `speculative_attempts` and is never sent to the provider. | Unit test with a fake `ScanFunc` |
| AC-06 | When cumulative `spec_tokens / main_tokens > max_overhead_multiplier`, all subsequent iterations in the same loop have `spec_outcome = 'budget_cap'` and no draft inference calls are made. | Integration test |
| AC-07 | `tag loop speculative-stats --profile coder` output includes: total attempts, hit count, hit percentage, average latency saved (ms), and token overhead ratio. All values match aggregates from `speculative_attempts` table. | Integration test with known fixture data |
| AC-08 | `tag loop show <loop-id>` output includes `spec_outcome` and `spec_similarity` columns for each iteration when the loop was run in speculative mode. | Integration test |
| AC-09 | After 20 speculation attempts with 10 hits, `speculation_priors.beta_alpha = 11.0` and `speculation_priors.beta_beta = 11.0` for the corresponding (profile, tool_name, action_type) row. | Unit test |
| AC-10 | `tag loop start --speculative --approval human` emits a warning and runs in sequential mode; no `speculative_attempts` rows are written. | Unit test |
| AC-11 | Draft inference calls respect `speculation.draft_timeout` (default 30 s) via `context.WithTimeout`; drafts that time out are marked `outcome = 'timeout'` in `speculative_attempts` and dropped from the scoring pass. | Unit test with a blocking fake `InferFunc` |
| AC-12 | The `Embedder` is initialized at most once per process lifetime; a second speculative iteration in the same loop reuses the same instance. | Unit test asserting the singleton is non-nil after the first call and pointer-identical after the second |
| AC-13 | `tag config set speculation.draft_cap 3` is reflected in `SpecConfig.draft_cap` for the next loop start; at most 3 draft tasks are launched per tool wait. | Integration test checking `len(spec_result.drafts) <= 3` |
| AC-14 | Tool latency history is accumulated in `tool_latencies` across multiple loop sessions; `estimate_tool_latency_ms()` returns the P50 of the last 100 recorded durations for that tool. | Unit test with synthetic duration list |
| AC-15 | `tag submit --speculative --prompt "..."` activates speculative mode for a single multi-tool prompt and writes results to `speculative_attempts` with `loop_id = NULL`. | Integration test |

---

## 13. Dependencies

| Dependency | Type | Version | Notes |
|------------|------|---------|-------|
| `github.com/tag-agent/tag/internal/memory` (`Embedder`) | Internal (Go) | current | Draft/observation embeddings; provider API by default, build-tag `cybertron` MiniLM offline. Shared singleton with `internal/toolindex` (PRD-043). Falls back to `stringRatio` when no embedder is available. |
| `golang.org/x/sync/errgroup` | Go module | latest | Bounded concurrent draft goroutines. |
| `context` (stdlib) | Go stdlib | 1.24+ | Per-branch `WithCancel`/`WithTimeout` for speculative drafts and tool cancellation. |
| `gonum.org/v1/gonum/stat/distuv` | Go module | GA | `Beta` variate sampling for Thompson ordering. |
| `github.com/tag-agent/tag/internal/llm` | Internal (Go) | current | `Stream(ctx, Request)->chan Event` provider interface for draft inference; token counts via tiktoken-go (OpenAI) / len/4 (Anthropic). |
| `github.com/tag-agent/tag/internal/tool` | Internal (Go) | current | Tool dispatch via `os/exec` `CommandContext` with process-group kill; produces the observation the drafts race against. |
| `PRD-013` (`internal/obs`) | Internal | current | New OTel span types via `go.opentelemetry.io/otel`. |
| `PRD-012` (`internal/obs` budget) | Internal | current | Budget gate consulted before each draft batch. |
| `PRD-034` (`internal/security`) | Internal | current | `Scan(ctx, prompt)` called on every speculative prompt. |
| `PRD-043` (`internal/toolindex`) | Internal | current | Shared `Embedder` instance. |
| `PRD-028` (`internal/sandbox`) | Internal | current | Committed action executes under the isolation ladder; latency recorder observes tool calls; no structural change needed. |
| `PRD-027` (eval framework) | Internal | current | Speculative loop runs are eval-able as first-class variants; `eval_results` entries carry `speculative=true` tag. |
| GitHub Issue #349 | External | — | Tracks this feature; acceptance criteria map to issue milestones. |

---

## 14. Open Questions

| # | Question | Owner | Target Resolution |
|---|----------|-------|-------------------|
| OQ-01 | Should draft generation use a smaller/cheaper model (e.g., Haiku vs. Sonnet) for the speculative path to reduce cost overhead? The cluster research context mentions "draft model" but this PRD assumes same-model drafts for simplicity. A `speculation.draft_model` config key could allow override. | Engine team | Before implementation start |
| OQ-02 | The similarity threshold 0.85 is chosen from the SPAgent paper defaults. Is this threshold appropriate for TAG's specific tool-call action space (shell commands, web search queries)? Should it be calibrated per tool_name? | ML team | After initial integration test data available |
| OQ-03 | Should the `tool_latencies` table be shared across profiles, or keyed per-profile? The same tool (`bash`) may behave differently in different profiles (e.g., `coder` profile runs heavier grep patterns than `researcher` profile). Currently keyed per-profile (FR-17). | Architecture | Before DB schema freeze |
| OQ-04 | When speculative mode is active and the budget cap is reached mid-loop, should the remaining iterations complete sequentially (current design) or should the loop abort with an informational message? Sequential fallback is safer but reduces predictability. | UX | Before FR-11 implementation |
| OQ-05 | `classifyAction()` uses a keyword heuristic. Should this be replaced with an embedding-based classifier (over the `internal/memory` `Embedder`) trained on TAG's historical `loop_iterations.output` data to improve Thompson sampling key quality? | ML team | Post-v1 enhancement |
| OQ-06 | Is `K=5` drafts the right cap for `idlespec.draft_cap`? The IdleSpec paper uses K=5 with Beta(1,1) prior. For very fast (2-3s) tools, K=3 may be more cost-efficient. Should the cap be dynamically computed as `min(5, floor(est_latency_s / avg_draft_time_s))`? | Engine team | Before implementation start |
| OQ-07 | The `speculative_attempts` table will grow unboundedly. Should there be a retention policy (e.g., delete rows older than 90 days, keep only the last 1000 rows per profile)? The `tool_latencies` table has the same issue. | Infrastructure | Before GA release |
| OQ-08 | `tag submit --speculative` with a single-tool prompt will never trigger speculation (no tool gap to exploit). Should the CLI warn the user, or silently ignore `--speculative` for single-tool prompts? | UX | Before CLI surface freeze |

---

## 15. Complexity and Timeline

**Total estimate: L (2–4 weeks)**

### Phase 1: Schema and Infrastructure (Days 1–4)

- Add `speculative_attempts`, `speculation_priors`, and `tool_latencies` DDL as numbered migrations in `internal/store/migrate/`.
- Implement the `ALTER TABLE loop_iterations ADD COLUMN` migration with `PRAGMA table_info` probes.
- Add the new OTel attribute-key constants to `internal/obs`.
- Add `SpecConfig`, `Draft`, `SpeculationResult`, `BudgetGuard` structs to `internal/agent`.
- Unit tests: struct defaults, migration application, migration idempotency.

### Phase 2: Core Speculation Engine (Days 5–10)

- Implement `buildDraftPrompt()`, `classifyAction()`, `cosine()`, `stringRatio()`.
- Implement `generateDraft()` with security scanning, `context.WithTimeout`, and error handling.
- Implement `SpeculateDuringTool()` orchestrator with the tool goroutine + `errgroup` draft branches + per-branch cancellation.
- Implement `sampleBeta()` (distuv.Beta) and `rankDraftsByPrior()` for Thompson-sampling ordering.
- Unit tests: FR-03 through FR-09 coverage with a fake `InferFunc` and fake `ScanFunc`.

### Phase 3: Thompson Sampling Persistence (Days 11–13)

- Implement `UpdatePrior()`, `LoadPriors()` with the SQLite upsert pattern through `internal/store`.
- Implement `EstimateToolLatency()`, `RecordToolLatency()`.
- Implement `persistSpeculationResult()` for batch row insertion.
- Unit tests: prior convergence test with a 50-element synthetic hit/miss sequence.

### Phase 4: `internal/agent` Integration (Days 14–17)

- Implement the `runIterationSpeculative()` wrapper in `internal/agent`.
- Wire the `--speculative` flag into the `internal/cli` `loop start` cobra command.
- Wire the `--approval human` incompatibility guard.
- Wire `BudgetGuard` overhead-cap enforcement (FR-11).
- Integration test: end-to-end loop with a fake tool goroutine.

### Phase 5: `tag submit --speculative` and Stats Command (Days 18–21)

- Extend the `internal/cli` `submit` command to pass `SpecConfig` when `--speculative` is set.
- Implement the `loop speculative-stats` command with `--loop-id`, `--profile`, `--since`, `--json` flags.
- Extend `tag loop show` output with speculation columns.
- Integration tests: stats command output format, `tag submit --speculative` end-to-end.

### Phase 6: Performance Validation and Security Review (Days 22–25)

- Benchmarks (`testing.B`): tool result processing latency, embedder cold start, SQLite write concurrency.
- Security review: secret leakage test with injected secrets in 100 synthetic prompts.
- Benchmark: 10-iteration loop wall time comparison speculative vs. sequential on CI hardware.
- Final acceptance criteria validation against AC-01 through AC-15.

### Phase 7: Documentation and Config Wiring (Days 26–28)

- Add `speculation.*` config keys to the koanf/v2 config schema and `tag config` help text.
- Update `tag loop start --help` with speculative flag descriptions.
- Update `docs/prd/INDEX.md` with PRD-106 entry.
- Create entry in `CHANGELOG.md` under `Unreleased`.

