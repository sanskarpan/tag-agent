# PRD-127: Evolutionary Profile Configuration Optimization (`tag evolve`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** L (2–3 sprints, ~5 weeks)
**Category:** Advanced Reasoning & Planning (Cluster G extension)
**Affects:** `internal/runtime/evolve` (new package: `EvolveRunner`), `internal/cli` (`evolve` command tree), `internal/store` (new `evolve_runs`, `evolve_generations`, `evolve_individuals` tables + migration)
**Depends on:** PRD-027 (eval framework — fitness scoring), PRD-013 (agent tracing), PRD-012 (budget enforcement), PRD-023 (swarm — goroutine task runner for fitness evaluation), PRD-045 (LLM-as-judge — quality scoring as fitness), PRD-001 (structured memory — profile config persistence)
**Inspired by:** Sakana AI Evolutionary Model Merging (arXiv:2403.13187, Nature Machine Intelligence), CycleQD (arXiv:2410.14735, ICLR 2025), ShinkaEvolve (arXiv:2509.19349), LLM² / DiscoPOP (arXiv:2406.08414, NeurIPS 2024)

---

## 1. Overview

Sakana AI's core research insight — formalized across four publications — is that evolutionary search over configuration space finds high-performing solutions without gradient-based training. Their Evolutionary Model Merging work searches weight-space of open-source models; CycleQD evolves a population of model adapters using Quality Diversity; ShinkaEvolve discovers algorithms via program evolution. The common thread: **when the search space is discrete and combinatorial, evolutionary algorithms outperform hand-tuned configurations**.

TAG's "models" are not weight matrices — they are YAML profile configurations: system prompts, model IDs, temperature, tool enablement, context budget, delegation depth, delegation provider, and more. These configurations are combinatorial and have complex interactions. A profile that works perfectly for short-burst code review may be catastrophically slow for long-horizon research synthesis because its context budget is too small, its delegation depth is 1, or its model is optimized for instruction-following rather than reasoning.

Today, TAG profile authors tune these configurations by hand, relying on intuition and trial-and-error. There is no mechanism to systematically explore the configuration space, evaluate candidates against a fitness function, and converge on configurations that maximize performance on a specific task type.

**`tag evolve`** introduces an evolutionary optimization loop for TAG profiles:

1. **Define a task suite** as the fitness function — a set of prompts and expected outputs (or an LLM-as-judge scorer).
2. **Initialize a population** of profile configurations — either random perturbations of a seed profile, or loaded from existing profile YAML files.
3. **Evaluate each individual** by running the task suite against the profile configuration and computing a fitness score.
4. **Select, crossover, and mutate** the best individuals to produce the next generation.
5. **Repeat** for N generations or until convergence.
6. **Export** the best-performing profile configuration to a new YAML file for immediate use.

This closes the gap with Sakana AI's evolutionary line of research at the software layer — no GPU compute required, no model weights modified. The evolutionary search happens over configuration space using existing TAG infrastructure.

---

## 2. Problem Statement

### 2.1 Profile Configuration Is Hand-Tuned and Non-Systematic

TAG ships with five default profiles (orchestrator, researcher, coder, reviewer, codex-runtime-master). Each was hand-authored based on intuition. There is no evidence that these configurations are optimal for any specific task type. A user building a production coding pipeline has no tools to discover whether `temperature=0.2` or `temperature=0.7` produces better code for their codebase, whether delegation depth 1 or 2 is optimal for their task complexity, or whether DeepSeek-V4-Flash or Qwen3-Coder produces higher-quality outputs on their specific test suite.

### 2.2 No Population-Level Exploration

Every other parameter-optimization tool in the ML ecosystem (hyperparameter search via Optuna, Neural Architecture Search, LoRA hyperparameter sweep) involves population-level parallel search. TAG has zero equivalent — the unit of exploration is a single human making a single YAML edit.

### 2.3 Profile Improvements Are Not Shared Across Task Types

Even when a user discovers a better temperature for coding tasks, they have no mechanism to know whether that improvement also helps for review tasks, or whether there is a trade-off. Evolutionary multi-objective optimization (optimizing for both quality and cost) would reveal these Pareto fronts automatically.

### 2.4 No Evolutionary Crossover of Profile Knowledge

Two profiles may each have complementary strengths: one has a better system prompt for code synthesis, the other has a better model+temperature combination for long-context reasoning. No tool exists to produce a "child" profile that inherits the best configuration genes from both parents. Evolutionary crossover enables this without human curation.

---

## 3. Goals

1. **Automated profile configuration search** — Find profile configurations that maximize a user-defined fitness function (task suite + scorer) without manual YAML editing.
2. **Population diversity** — Maintain a diverse gene pool; prevent premature convergence to local optima via mutation pressure and Quality Diversity selection (CycleQD-inspired).
3. **Multi-objective optimization** — Support Pareto optimization over (quality, cost-usd, latency) simultaneously.
4. **Crossover** — Produce child profiles that combine configuration genes from two parent profiles.
5. **Exportable results** — Best-performing profile exported to YAML for immediate use; importable into TAG config.
6. **Budget-aware** — Each fitness evaluation costs tokens; the evolver respects a total budget ceiling.
7. **Warm-start** — Existing profiles can be used as the seed population, accelerating convergence from a known good baseline.
8. **Novelty-based rejection sampling** — ShinkaEvolve pattern: reject individuals that are too similar to existing population members (measured by configuration distance), enforcing diversity.

---

## 4. Non-Goals

- Modifying model weights (inference-time config search only).
- Automated prompt engineering or prompt mutation beyond configuration parameters.
- Multi-machine distributed evolution (single-machine bounded goroutine pool / `errgroup` for parallel fitness evaluation).
- Replacing human profile authorship for non-evolutionary use cases.

---

## 5. Gene Space (What Gets Evolved)

| Gene | Type | Range / Options |
|---|---|---|
| `model.provider` | categorical | `openai-codex`, `openrouter`, `anthropic` |
| `model.default` | categorical | all models registered in the go:embed'd pricing table (`internal/obs`) |
| `temperature` | float | [0.0, 1.5] |
| `max_tokens` | int | [512, 32768] |
| `context_budget_tokens` | int | [2048, 131072] |
| `delegation.max_concurrent_children` | int | [1, 8] |
| `delegation.max_spawn_depth` | int | [1, 4] |
| `delegation.provider` | categorical | same as model.provider |
| `delegation.model` | categorical | all models |
| `kanban.dispatch_interval_seconds` | int | [10, 300] |
| `kanban.max_in_progress_per_profile` | int | [1, 6] |
| `system_prompt_addendum` | str | LLM-mutated persona/instruction suffix (optional) |
| `tool_allowlist` | set | subset of all registered tools |
| `tui_statusbar` | categorical | `top`, `bottom`, `off` |

Genes are encoded as a flat `map[string]any` (the genome); crossover is uniform (each gene inherited from parent A or B with probability 0.5); mutation perturbs one randomly chosen gene. Randomness comes from a per-run `*math/rand.Rand` seeded from a `--seed` flag (deterministic tests). The genome round-trips to a profile via `gopkg.in/yaml.v3` marshal.

---

## 6. Feature Requirements

### 6.1 CLI Interface

```bash
# Initialize an evolve run from a seed profile
tag evolve run \
    --seed-profile coder \
    --task-suite evals/coding-suite.yaml \
    --generations 10 \
    --population-size 8 \
    --fitness judge \
    --fitness-profile reviewer \
    --budget-usd 20.00

# Warm-start from multiple existing profiles (cross-pollinate)
tag evolve run \
    --seed-profiles coder,orchestrator,researcher \
    --task-suite evals/coding-suite.yaml \
    --generations 15 --population-size 12

# Multi-objective: optimize for quality AND cost
tag evolve run \
    --seed-profile coder \
    --task-suite evals/coding-suite.yaml \
    --objectives quality,cost \
    --generations 10 \
    --pareto-front-size 5

# Custom fitness: shell command returning score 0.0–1.0 on stdout
tag evolve run \
    --seed-profile coder \
    --task-suite evals/coding-suite.yaml \
    --fitness cmd:"./scripts/run_eval.sh {profile_yaml}" \
    --generations 8

# Inspect evolution progress
tag evolve status <run-id>
tag evolve status <run-id> --json
tag evolve results <run-id>                     # show best individual per generation
tag evolve results <run-id> --pareto            # show Pareto front (multi-objective)
tag evolve list

# Export the best-performing profile
tag evolve export <run-id> --name evolved-coder
tag evolve export <run-id> --generation 7 --rank 2   # second-best from gen 7
# Writes: ~/.tag/profiles/evolved-coder.yaml  (importable via tag profile import)

# Resume an interrupted evolve run
tag evolve resume <run-id>

# Abort
tag evolve abort <run-id>
```

### 6.2 Fitness Evaluation Modes

| Mode | Flag | Description |
|---|---|---|
| LLM-as-judge | `--fitness judge` | Each task in suite run against profile; reviewer scores output [0.0, 1.0] |
| Shell command | `--fitness cmd:"..."` | Shell receives profile YAML path; reads float from stdout |
| Eval suite pass rate | `--fitness eval-suite` | Run existing `tag eval run` suite; fitness = pass_rate |
| Test suite | `--fitness test:"pytest tests/"` | Exit code 0 = 1.0; else 0.0 |
| Cost-adjusted | `--fitness cost-adjusted` | fitness = quality / cost_usd (Pareto proxy) |

### 6.3 Selection Strategies

| Strategy | Description |
|---|---|
| `tournament` (default) | K=3 tournament selection; winner is the fittest of K random draws |
| `elitism` | Top-E individuals survive unchanged to next generation |
| `roulette` | Probability proportional to relative fitness |
| `quality-diversity` | CycleQD-inspired: maintain archive of (behavior, fitness) pairs; select to maximize coverage |

### 6.4 Crossover and Mutation

```go
type Genome map[string]any

// crossover: each gene from parent A or B with p=0.5 (uniform).
func (r *EvolveRunner) crossover(a, b Genome) Genome {
	child := make(Genome, len(GeneSpace))
	for _, gene := range GeneSpace {
		if r.rng.Float64() < 0.5 {
			child[gene] = a[gene]
		} else {
			child[gene] = b[gene]
		}
	}
	return child
}

// mutate perturbs each gene with probability mutationRate.
func (r *EvolveRunner) mutate(ind Genome, mutationRate float64) Genome {
	out := maps.Clone(ind)
	for _, gene := range GeneSpace {
		if r.rng.Float64() < mutationRate {
			out[gene] = r.randomGeneValue(gene)
		}
	}
	return out
}
```

### 6.5 Novelty-Based Rejection Sampling (ShinkaEvolve Pattern)

Before adding a mutant to the next-generation pool, compute its configuration distance from existing population members:

```go
// isNovelEnough rejects a candidate too similar to any existing member.
func isNovelEnough(candidate Genome, population []Genome, threshold float64) bool {
	for _, existing := range population {
		if configDistance(candidate, existing) < threshold {
			return false
		}
	}
	return true
}
```

`configDistance` is a normalized Hamming distance over categorical genes + normalized Manhattan distance over numerical genes (implemented with `math.Abs` over the gene ranges declared in the Gene Space table; the numeric aggregation reuses `gonum/floats` for vector norms).

### 6.6 Pareto Front (Multi-Objective)

When `--objectives quality,cost` is specified, selection uses hand-rolled non-dominated sorting (NSGA-II simplified) over `[]Individual`:
1. For each individual, count how many others dominate it (better on ALL objectives).
2. Rank by domination count (sort with `sort.Slice`).
3. Within the same rank, use crowding distance (per-objective normalized spacing, `gonum/floats` for the numeric spans) to maintain diversity on the Pareto front.
4. `tag evolve results <id> --pareto` shows the non-dominated set with per-objective scores.

### 6.7 Database Schema

```sql
CREATE TABLE IF NOT EXISTS evolve_runs (
    run_id TEXT PRIMARY KEY,
    seed_profiles_json TEXT NOT NULL,
    task_suite TEXT NOT NULL,
    fitness_mode TEXT NOT NULL DEFAULT 'judge',
    fitness_profile TEXT,
    objectives_json TEXT NOT NULL DEFAULT '["quality"]',
    selection_strategy TEXT NOT NULL DEFAULT 'tournament',
    population_size INTEGER NOT NULL DEFAULT 8,
    generations INTEGER NOT NULL DEFAULT 10,
    mutation_rate REAL NOT NULL DEFAULT 0.15,
    crossover_rate REAL NOT NULL DEFAULT 0.7,
    elitism_count INTEGER NOT NULL DEFAULT 2,
    budget_usd REAL,
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK(status IN ('pending','running','completed','aborted','budget_exceeded')),
    best_individual_id INTEGER,
    best_fitness REAL DEFAULT 0.0,
    total_evaluations INTEGER DEFAULT 0,
    total_cost_usd REAL DEFAULT 0.0,
    started_at TEXT,
    completed_at TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS evolve_generations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL REFERENCES evolve_runs(run_id),
    generation INTEGER NOT NULL,
    best_fitness REAL,
    mean_fitness REAL,
    diversity_score REAL,
    population_json TEXT,           -- JSON array of individual IDs in this generation
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    UNIQUE(run_id, generation)
);

CREATE TABLE IF NOT EXISTS evolve_individuals (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL REFERENCES evolve_runs(run_id),
    generation INTEGER NOT NULL,
    genome_json TEXT NOT NULL,          -- full gene dict as JSON
    profile_yaml TEXT,                  -- rendered YAML for this genome
    fitness REAL,
    objectives_json TEXT,               -- {"quality": 0.87, "cost": 0.003}
    parent_ids_json TEXT,               -- [parent_a_id, parent_b_id] or null (seed)
    origin TEXT NOT NULL DEFAULT 'seed'
        CHECK(origin IN ('seed','crossover','mutation','elitism')),
    evaluation_cost_usd REAL DEFAULT 0.0,
    evaluation_tokens INTEGER DEFAULT 0,
    evaluated_at TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE INDEX IF NOT EXISTS idx_evolve_individuals_run ON evolve_individuals(run_id, generation, fitness DESC);
```

### 6.8 Profile Export Format

```yaml
# Exported by: tag evolve export <run-id> --name evolved-coder
# Evolved via: evolve run abc123 | generation 8, rank 1 | fitness: 0.924
# Task suite: evals/coding-suite.yaml
# Origin: crossover(individual_42, individual_37)
evolved-coder:
  description: "Evolved coder profile — optimized for coding-suite.yaml (fitness: 0.924)"
  tags: [coding, terminal, implementation, evolved]
  evolved_from:
    run_id: abc123
    generation: 8
    fitness: 0.924
    parents: [42, 37]
  config:
    model:
      provider: openrouter
      default: qwen/qwen3-coder
    temperature: 0.23
    max_tokens: 8192
    delegation:
      max_concurrent_children: 2
      max_spawn_depth: 1
```

---

## 7. Architecture

```
internal/cli  (evolve command tree: run/status/results/list/abort/resume/export)
    │
    ▼
evolve.EvolveRunner  (internal/runtime/evolve)
    ├── initializePopulation() # seed + random perturbations
    ├── evaluateGeneration()   # parallel fitness evals (errgroup, SetLimit(N))
    │       ├── runFitnessEval()   # run profile via internal/llm per task (no subprocess)
    │       └── scoreFitness()     # judge/shell(os/exec)/eval-suite/test dispatcher
    ├── selectParents()        # tournament / roulette / QD
    ├── crossover()            # uniform crossover
    ├── mutate()               # random gene perturbation (per-run *rand.Rand)
    ├── noveltyFilter()        # reject too-similar candidates (configDistance)
    ├── paretoSort()           # NSGA-II front (multi-objective)
    ├── exportProfile()        # genome → YAML profile (yaml.v3)
    └── checkpoint()           # persist generation summary to internal/store
```

Concurrency: `evaluateGeneration` fans out one goroutine per individual through an `errgroup` with `SetLimit(--parallelism)`, each running the fitness task suite via the `internal/llm` provider interface; results are collected and the generation is scored before selection. The whole run is cancellable via `context.Context` (Ctrl+C/abort/budget). Fitness evaluation reuses the swarm goroutine-task pattern (PRD-023) rather than forking subprocesses — the Go binary owns the runtime.

---

## 8. Implementation Plan

| Step | Task | File | Est. |
|---|---|---|---|
| 1 | Schema: `evolve_runs`, `evolve_generations`, `evolve_individuals` + migration | `internal/store/migrate` | 0.5d |
| 2 | Gene space definition + genome encode/decode to YAML (yaml.v3) | `internal/runtime/evolve` | 1d |
| 3 | Fitness evaluators: judge (`internal/llm`), eval-suite, shell (`os/exec`), test | `internal/runtime/evolve` | 2d |
| 4 | Crossover + mutation + novelty filter (`configDistance`) | `internal/runtime/evolve` | 1d |
| 5 | Selection strategies: tournament, elitism, roulette, QD | `internal/runtime/evolve` | 1d |
| 6 | Main evolution loop: generations × population over `errgroup` pool | `internal/runtime/evolve` | 1.5d |
| 7 | Multi-objective Pareto front (NSGA-II simplified, gonum/floats) | `internal/runtime/evolve` | 1.5d |
| 8 | Profile export to YAML with provenance metadata | `internal/runtime/evolve` | 0.5d |
| 9 | Resume protocol: checkpoint/restore generation state from `internal/store` | `internal/runtime/evolve` | 1d |
| 10 | `evolve` cobra command tree: `run`, `status`, `results`, `list`, `abort`, `resume`, `export` (+ `--json`) | `internal/cli` | 1.5d |
| 11 | Tests: crossover, mutation, novelty filter, fitness modes, export (fake `llm.Provider`) | `*_test.go` | 2d |
| 12 | Budget enforcement integration (`internal/obs`) | `internal/obs` | 0.5d |

**Total:** ~14 dev-days (~3 sprints)

---

## 9. Testing Requirements

| Test | Assertion |
|---|---|
| `test_evolve_crossover` | Child genome has each gene from exactly one of two parents |
| `test_evolve_mutation` | Mutant differs from parent on ≥1 gene |
| `test_evolve_novelty_filter` | Identical genome rejected; sufficiently different genome accepted |
| `test_evolve_tournament_selects_fitter` | Over 100 tournaments, fitter individual wins ≥70% |
| `test_evolve_elitism_preserves` | Best individual always present in next generation |
| `test_evolve_generation_improves` | Mean fitness of generation 5 ≥ mean fitness of generation 1 (mock fitness) |
| `test_evolve_budget_terminates` | Aborts when total_cost_usd ≥ budget_usd |
| `test_evolve_pareto_front` | Non-dominated set contains only individuals not dominated by others |
| `test_evolve_export_yaml` | Exported YAML parses correctly; genome fields match individual's genome |
| `test_evolve_resume` | Abort at generation 3; resume continues from generation 4 |
| `test_evolve_warm_start` | Multiple seed profiles all present in generation 0 |
| `test_evolve_json_output` | `--json` returns valid JSON with run_id, status, best_fitness |

---

## 10. Risks

| Risk | Mitigation |
|---|---|
| Each fitness eval costs tokens; large populations are expensive | Default population=8, generations=10; `--budget-usd` hard cap; dry-run mode shows cost estimate |
| Premature convergence to local optima | Elitism count ≤ 2; novelty filter prevents identical individuals; mutation rate 15% |
| System prompt addendum gene produces harmful prompts | `internal/security` scanner (`ScanText`, PRD-034) gates all generated system-prompt addenda before evaluation |
| Evolved profiles may be unpredictable in production | Exports include full provenance metadata; `--dry-run` test runs before export |
| Gene space combinatorics may be too large to converge in budget | Restrict to highest-impact genes first; optional `--gene-subset model,temperature,context_budget` |

---

## 11. Future Enhancements

- **LLM-driven mutation** — Instead of random gene perturbation, use an LLM to propose mutations: "Given this profile performed poorly on long-context tasks, suggest a better configuration." Replicates Sakana's DiscoPOP / LLM² pattern.
- **Cross-team evolution** — Evolve the entire *team configuration* (which profiles are in the team, what roles they play) rather than individual profiles. Bridges to PRD-082 Trinity extension.
- **Model-space merging** — If adapter weights become available via Transformer² (PRD-128, future), evolve over adapter space to match Sakana's actual weight-space evolutionary merging.
- **Online evolution** — Continuously evolve production profiles based on live run quality signals; automatically deploy improved configurations (with human approval gate).
- **Shared leaderboard** — Publish evolved profile fitness scores to a shared registry (PRD-026 marketplace); community can download and fine-tune winning configurations.
