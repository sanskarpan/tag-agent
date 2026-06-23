# PRD-126: Inference-Time Multi-Model Tree Search (`tag solve`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** L (2–3 sprints, ~5 weeks)
**Category:** Advanced Reasoning & Planning (Cluster G extension)
**Affects:** `src/tag/solver.py` (new), `src/tag/controller.py` (`cmd_solve`), `tag.sqlite3` (new `solve_runs`, `solve_nodes` tables)
**Depends on:** PRD-023 (swarm context routing — subprocess runner), PRD-013 (agent tracing), PRD-012 (budget enforcement), PRD-027 (eval framework — judge scoring), PRD-101 (self-consistency ensemble — aggregation primitives), PRD-045 (LLM-as-judge — reviewer-as-judge path)
**Inspired by:** Sakana AI AB-MCTS (arXiv:2503.04412, NeurIPS 2025 Spotlight), TreeQuest multi-model tree search, OpenAI o3 inference-time scaling, Gemini 2.5 thinking budget

---

## 1. Overview

TAG today solves problems with one profile, one call, one result. For hard goals — multi-step coding tasks, architecture decisions, security audits with adversarial edge cases — the quality ceiling is the best single model response you happen to sample. There is no mechanism to iteratively refine, branch, explore alternatives, or combine insights across multiple models at inference time.

**AB-MCTS** (Adaptive Branching Monte Carlo Tree Search), published by Sakana AI at NeurIPS 2025, demonstrates that dynamically choosing whether to go *wide* (generate N independent candidate answers from scratch) or *deep* (refine the best existing candidate) at each step of a tree search substantially outperforms both pure repeated sampling and standard MCTS. Their multi-model variant — allocating budget across GPT-4o, Gemini 2.5 Pro, and DeepSeek-R1 via Thompson Sampling — achieved 30% Pass@250 on ARC-AGI-2 by solving problems no single model could crack.

This PRD introduces `tag solve`: a new top-level command that wraps any goal in an inference-time tree search loop, adapts exploration width vs depth per node based on an external verifier signal, and can allocate across multiple profiles (models) using a bandit strategy. The result is the highest-quality answer TAG can produce for a given token budget, at the cost of 3–20× the single-call compute.

`tag solve` is designed for hard, high-stakes goals where quality matters more than latency: architecture decisions, complex debugging sessions, security audits, competitive programming, and research synthesis.

---

## 2. Problem Statement

### 2.1 Single-Call Quality Ceiling for Complex Goals

For problems requiring multi-step reasoning, code synthesis + verification, or exploration of a large solution space, a single LLM call consistently produces lower-quality outputs than iterative search. The gap is not marginal — Sakana AB-MCTS shows 30%+ improvement on benchmark tasks where single-shot fails entirely. TAG has no mechanism to allocate additional compute to hard problems beyond manually rerunning the command.

### 2.2 No Multi-Model Budget Allocation

TAG profiles map to individual models. For any given subtask, there is one model assigned. If that model has a systematic weakness on the problem type, there is no fallback at inference time (only at routing time, via PRD-031 fallback chains). Sakana's Thompson Sampling allocator and Conductor's dynamic worker selection show that combining models with complementary strengths — one excels at long-context reasoning, another at code synthesis — consistently outperforms the best single model.

### 2.3 Exploration vs Exploitation Tradeoff Has No Knob

With `--samples N` (PRD-101), TAG generates N independent attempts and votes. This is pure width — it never refines. With `tag swarm run`, TAG decomposes into subtasks — this is pure decomposition, not iterative search. Neither adapts: if a width-3 attempt looks 80% correct, TAG cannot "go deeper" on that specific candidate rather than generating two new ones from scratch. AB-MCTS's key insight is that adaptive branching (decide per-node whether to branch or deepen based on score) consistently outperforms fixed strategies.

---

## 3. Goals

1. **Tree-search quality** — A `tag solve` run on a hard goal produces measurably higher quality than single-call `tag submit` on the same goal.
2. **Adaptive branching** — The solver dynamically decides per node whether to generate new candidate responses (explore) or refine the best existing one (exploit), based on a verifier score.
3. **Multi-profile bandit allocation** — When `--profiles A,B,C` is specified, a Thompson Sampling bandit allocates calls across profiles based on empirical per-profile score history within the run.
4. **Budget-aware termination** — The solve loop respects `--max-calls N` and `--budget-usd X`; early termination when verifier returns score ≥ `--target-score`.
5. **Transparent trace** — Every node (candidate, score, parent, depth, profile used) stored in `solve_nodes` table; `tag solve status <id>` shows the tree.
6. **Verifier composability** — Verifier can be: LLM-as-judge (PRD-045), unit test output (pass/fail), regex/keyword match, or custom shell command. Scorer returns float in [0.0, 1.0].
7. **Resume** — A solve run can be interrupted (`Ctrl+C`) and resumed later from the best node checkpoint.
8. **Synthesis** — On termination, the highest-scoring leaf node is returned as the final answer, or optionally passed through a synthesis profile that integrates insights from all top-K nodes.

---

## 4. Non-Goals

- Replacing `tag submit` for routine tasks (solve is 3–20× more expensive; explicit opt-in required).
- Distributed execution across machines (single-machine concurrency via ThreadPoolExecutor, same as swarm).
- Learning or updating model weights (inference-time only; no fine-tuning).
- General neural architecture search or evolutionary algorithm over model parameters.

---

## 5. Feature Requirements

### 5.1 CLI Interface

```bash
# Basic: 3 candidate answers, reviewer-as-judge verifier
tag solve --goal "Find all SQL injection vulnerabilities in src/api.py" \
          --profile reviewer \
          --max-calls 12

# Multi-model bandit: allocate across 3 profiles
tag solve --goal "Implement a Redis-backed session store in < 100 lines" \
          --profiles coder,orchestrator,researcher \
          --max-calls 20 --budget-usd 2.00

# Custom verifier: run tests as the scorer
tag solve --goal "Fix the failing test in test_auth.py" \
          --profile coder \
          --verifier "pytest tests/test_auth.py --tb=no -q" \
          --max-calls 15 --target-score 1.0

# Deep mode: always refine, never branch (= iterative refinement)
tag solve --goal "Write a memory-safe Rust HTTP parser" \
          --profile coder --strategy deep --depth 5

# Wide mode: pure sampling (equivalent to tag submit --samples N)
tag solve --goal "Summarise the Q3 metrics doc" \
          --profile researcher --strategy wide --width 5

# Adaptive (default): AB-MCTS branching factor decisions per node
tag solve --goal "Design the auth module for a zero-trust microservices system" \
          --profiles orchestrator,researcher \
          --strategy adaptive --max-calls 30 \
          --synthesize --synthesis-profile orchestrator

# Inspect tree
tag solve status <solve-id>
tag solve status <solve-id> --json
tag solve results <solve-id>
tag solve list [--status running|completed|aborted]
tag solve abort <solve-id>

# Resume an interrupted run
tag solve resume <solve-id>
```

### 5.2 Search Strategies

| Strategy | Description | When to use |
|---|---|---|
| `adaptive` (default) | AB-MCTS: per-node score decides branch vs deepen | Hard goals, unknown structure |
| `wide` | Pure breadth: N independent samples, return best | Fast quality boost, simple goals |
| `deep` | Pure depth: N sequential refinements of best candidate | Goals with clear improvement signal |
| `beam` | Keep top-K at each level, expand each one level deeper | Structured goals with depth bound |
| `tournament` | Pairwise LLM-judge elimination bracket | High-stakes decisions, diverse candidates |

### 5.3 Verifier Types

```bash
--verifier judge              # LLM-as-judge using --verifier-profile (default: reviewer)
--verifier "pytest tests/"    # Shell command: exit code 0 = score 1.0, else 0.0
--verifier "pytest tests/" --score-from-output  # Parse score from stdout: "SCORE: 0.87"
--verifier keyword:<term>     # 1.0 if term in output, else 0.0
--verifier regex:<pattern>    # 1.0 if pattern matches output, else 0.0
--verifier none               # No verifier; stop at max-calls (returns last result)
```

### 5.4 Multi-Profile Thompson Sampling Bandit

When `--profiles A,B,C` is specified:

1. Initialise Beta(1,1) prior per profile.
2. Per node expansion: sample θ ~ Beta(α, β) for each profile; select argmax(θ).
3. After verifier scores the node: update Beta(α+score, β+(1−score)) for the selected profile.
4. Track per-profile call count, mean score, and 95% CI in `solve_nodes` table.
5. `tag solve status <id>` shows per-profile empirical win rate.

### 5.5 Database Schema

```sql
CREATE TABLE IF NOT EXISTS solve_runs (
    solve_id TEXT PRIMARY KEY,
    goal TEXT NOT NULL,
    strategy TEXT NOT NULL DEFAULT 'adaptive',
    failure_policy TEXT NOT NULL DEFAULT 'best_score',
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK(status IN ('pending','running','completed','aborted','budget_exceeded')),
    profiles_json TEXT NOT NULL,            -- JSON array of profile names
    verifier TEXT NOT NULL DEFAULT 'judge',
    max_calls INTEGER NOT NULL DEFAULT 12,
    budget_usd REAL,
    target_score REAL DEFAULT 1.0,
    synthesize INTEGER NOT NULL DEFAULT 0,  -- bool
    synthesis_profile TEXT,
    best_score REAL DEFAULT 0.0,
    best_node_id INTEGER,
    total_calls INTEGER DEFAULT 0,
    total_cost_usd REAL DEFAULT 0.0,
    final_output TEXT,
    started_at TEXT,
    completed_at TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS solve_nodes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    solve_id TEXT NOT NULL REFERENCES solve_runs(solve_id),
    parent_id INTEGER REFERENCES solve_nodes(id),
    depth INTEGER NOT NULL DEFAULT 0,
    profile TEXT NOT NULL,
    prompt_summary TEXT,
    output TEXT,
    score REAL,
    verifier_output TEXT,
    is_refinement INTEGER NOT NULL DEFAULT 0,   -- 1 = deepen, 0 = branch
    tokens_prompt INTEGER DEFAULT 0,
    tokens_completion INTEGER DEFAULT 0,
    cost_usd REAL DEFAULT 0.0,
    elapsed_seconds REAL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_solve_nodes_solve_id ON solve_nodes(solve_id);
CREATE INDEX IF NOT EXISTS idx_solve_nodes_score ON solve_nodes(solve_id, score DESC);
```

### 5.6 Adaptive Branching Decision (AB-MCTS Core)

```python
def _should_deepen(node_score: float, tree_best_score: float,
                   depth: int, max_depth: int,
                   calls_remaining: int) -> bool:
    """Return True to refine this node, False to branch from best leaf."""
    if depth >= max_depth:
        return False
    if calls_remaining <= 2:
        return False
    # Deepen if this node is close to or better than tree best
    proximity = node_score / max(tree_best_score, 1e-9)
    return proximity >= 0.85
```

This replicates AB-MCTS's core insight: nodes scoring ≥85% of tree-best get deepened; others trigger new branches from the current best leaf.

### 5.7 Synthesis Pass

When `--synthesize` is passed, after the search terminates, the top-K scoring nodes (default K=3) are passed to a synthesis agent:

```
You have {K} candidate solutions to: {goal}

--- Candidate 1 (score: {s1}) ---
{output_1}

--- Candidate 2 (score: {s2}) ---
{output_2}
...

Synthesize a final answer that integrates the strongest elements of each candidate.
Prioritise accuracy over completeness.
```

The synthesis output becomes `final_output` in `solve_runs`.

### 5.8 Resume Protocol

On `Ctrl+C` or `tag solve abort <id>`:
1. Mark solve as `aborted`; record `best_node_id` pointing to highest-scoring completed node.
2. On `tag solve resume <id>`: reload tree from `solve_nodes`, restore Thompson bandit state from per-profile empirical data, continue from remaining budget.

---

## 6. Architecture

```
cmd_solve (controller.py)
    │
    ▼
SolveRunner (solver.py)
    ├── _select_profile()      # Thompson Sampling bandit
    ├── _generate_candidate()  # invoke profile subprocess (reuses swarm._run_task pattern)
    ├── _score_candidate()     # verifier dispatch
    ├── _should_deepen()       # AB-MCTS decision
    ├── _branch()              # expand new candidate from best leaf
    ├── _deepen()              # refine existing node
    ├── _synthesize()          # top-K synthesis pass (optional)
    └── _checkpoint()          # write best node to solve_runs on each iteration
```

---

## 7. Implementation Plan

| Step | Task | File | Est. |
|---|---|---|---|
| 1 | Schema: `solve_runs`, `solve_nodes`, migration in `open_db()` | `controller.py` | 0.5d |
| 2 | `SolveRunner` class: wide strategy (pure sampling baseline) | `solver.py` | 1d |
| 3 | Verifier dispatch: judge, shell, keyword, regex | `solver.py` | 1d |
| 4 | Deep strategy: sequential refinement loop | `solver.py` | 0.5d |
| 5 | Adaptive strategy: `_should_deepen()` + tree traversal | `solver.py` | 2d |
| 6 | Thompson Sampling bandit for multi-profile | `solver.py` | 1d |
| 7 | Beam and tournament strategies | `solver.py` | 1d |
| 8 | Synthesis pass | `solver.py` | 0.5d |
| 9 | Resume protocol: `solve abort` + `solve resume` | `solver.py` + `controller.py` | 1d |
| 10 | `cmd_solve` + argparse: `solve`, `status`, `results`, `list`, `abort`, `resume` | `controller.py` | 1d |
| 11 | Tests: wide/deep/adaptive, verifier types, bandit allocation, resume | `tests/` | 2d |
| 12 | Integration: budget enforcement, tracing child spans per node | `budget.py`, `tracing.py` | 0.5d |

**Total:** ~11.5 dev-days (~2.5 sprints)

---

## 8. Testing Requirements

| Test | Assertion |
|---|---|
| `test_solve_wide_returns_best` | N=3 wide solve returns the highest-scored candidate |
| `test_solve_deep_refines` | Deep strategy depth=3 produces successively refined outputs |
| `test_solve_adaptive_deepen` | Node at 90% of tree-best triggers deepen, not branch |
| `test_solve_adaptive_branch` | Node at 50% of tree-best triggers branch |
| `test_solve_shell_verifier` | Shell verifier exit 0 → 1.0, exit 1 → 0.0 |
| `test_solve_budget_termination` | Solve aborts after budget_usd exceeded |
| `test_solve_target_score_early_stop` | Solve stops when first node hits target_score |
| `test_solve_bandit_allocates` | Multi-profile run distributes calls across all profiles |
| `test_solve_bandit_concentrates` | After 10 calls, bandit concentrates on higher-performing profile |
| `test_solve_resume` | Abort + resume produces same best_node_id, continues from checkpoint |
| `test_solve_tournament` | 4 candidates → 2 pairwise matches → 1 winner |
| `test_solve_synthesize` | Top-K nodes passed to synthesis profile; output stored in final_output |
| `test_solve_list` | Lists runs filtered by status |
| `test_solve_json_output` | `--json` returns valid JSON with solve_id, status, best_score |

---

## 9. Risks

| Risk | Mitigation |
|---|---|
| Runaway cost with many profiles × many calls | Hard `--budget-usd` cap enforced before each candidate generation; default max-calls=12 |
| Verifier LLM call doubles cost | Judge verifier cached by output hash; `--verifier none` skips entirely |
| Thompson bandit starves weak profiles early | Exploration floor: every profile guaranteed ≥2 calls before bandit fully concentrates |
| Resume after code change produces inconsistent tree | Resume checks `solver.py` hash; warns if code changed since run |
| Long-running solve blocks terminal | `--background` flag runs as queue job (PRD-008); status via `tag solve status` |

---

## 10. Future Enhancements

- **MCTS with UCB1** — full upper-confidence bound tree policy instead of simple proximity heuristic.
- **Evolutionary solve** — use genetic crossover between top-K nodes to create novel candidates (bridges to PRD-127).
- **Multimodal verifier** — screenshot diff, rendered HTML comparison, image similarity score.
- **Solve replay** — replay a solve run with a different verifier to retroactively score historical candidates.
- **Online learning** — persist per-profile per-task-type empirical win rates to SQLite; pre-warm Thompson priors across runs.
