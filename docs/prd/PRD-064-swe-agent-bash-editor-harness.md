# PRD-064: SWE-Agent-Style Structured Bash+Editor Harness (`tag solve --harness swe`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (1-2 weeks)
**Category:** CI/CD & Agentic Dev Workflows
**Affects:** `swe_harness.py` (new), `controller.py` (new `cmd_solve`, `cmd_benchmark` extensions)
**Depends on:** PRD-027 (Eval Framework), PRD-028 (Sandbox Code Execution), PRD-013 (Agent Tracing / OTel), PRD-034 (Security / Secret Scanning), PRD-008 (Background Task Queue), PRD-019 (Token Budget Enforcement), PRD-021 (Agent Loop / Autonomous Mode), PRD-038 (Diff-Aware Context Injection), PRD-041 (OTel GenAI Span Cost Attribution), PRD-042 (Architect-Editor Agent Split), PRD-055 (Issue-to-PR Autonomous Loop)
**Inspired by:** SWE-agent (Princeton NLP, arXiv:2405.15793, NeurIPS 2024), SWE-bench, Aider structured edits
**GitHub Issue:** #344

---

## 1. Overview

SWE-agent (Princeton NLP, NeurIPS 2024) demonstrated a critical insight: the same GPT-4 model scores roughly 2x higher on software engineering benchmarks when interacting through a purpose-built Agent-Computer Interface (ACI) rather than raw bash access. The ACI replaces unbounded shell sessions with a small, bounded set of structured tool operations — windowed file viewing, line-targeted editing with lint validation, fuzzy file search, and bash execution — that are co-designed with the constraints of LLM context windows. Each tool call is predictable, reversible, and produces deterministic, compact output rather than the scrolling walls of text that raw bash generates.

TAG currently drives code changes through Hermes (Claude Code CLI) in pass-through mode. Hermes has its own internal tool set, but TAG cannot directly measure or benchmark the quality of that tool set against external standards. Critically, TAG cannot participate in SWE-bench evaluations — the de facto standard for agentic coding capability — because SWE-bench requires a specific prediction format (unified diffs in JSONL), a specific task interface (`SWEBenchInstance` with `repo`, `instance_id`, `problem_statement`, `base_commit`, `FAIL_TO_PASS`, `PASS_TO_PASS` fields), and an agentic loop that produces a patch artifact rather than a conversational output. Without SWE-bench compatibility, TAG's claim to be a production-grade agentic coding tool is unverifiable against the field's accepted benchmark.

This PRD specifies `swe_harness.py` — a self-contained ACI implementation inside TAG that provides the canonical SWE-agent tool set: `open`, `scroll_down`, `scroll_up`, `goto`, `edit`, `search_file`, `search_dir`, `find_file`, `create`, `submit`, plus a bounded bash executor. The harness wraps these tools in a TAG-native agentic loop with three mandatory stopping conditions (success, failure, budget), emits OpenTelemetry spans per tool call (PRD-013/PRD-041), integrates with `sandbox.py` (PRD-028) for filesystem isolation, and produces SWE-bench-compatible JSONL predictions. The harness is invoked via `tag solve --harness swe` for single issues and `tag benchmark --harness swe --dataset swe-bench-lite` for batch evaluation runs.

The ACI design philosophy enforced by this harness is: every tool operation is bounded in output size (100-line file window), targeted in scope (line ranges rather than full files), validated on write (lint after every edit), and stateful across turns (CURRENT_FILE + FIRST_LINE persisted in the harness session object). These constraints force the model to navigate code systematically rather than blindly appending or overwriting, which is why the 2x performance improvement is robust across model families. TAG adopts the same constraints verbatim, because the research evidence for ACI superiority over raw bash is strong and the implementation cost is bounded.

The `tag benchmark --harness swe` subcommand integrates with the SWE-bench evaluation protocol: it loads instances from the `swe-bench-lite` dataset (500 instances) or `swe-bench-verified` (500 curated instances), runs the harness against each instance in the configured workspace (Docker container per instance for isolation), collects unified diffs as predictions, writes the JSONL predictions file, and optionally submits to the SWE-bench evaluation infrastructure or runs local evaluation via `swebench.evaluation.run_evaluation`. Results are stored in `swe_benchmark_runs` and `swe_benchmark_results` SQLite tables following the same schema conventions as `eval_runs` / `eval_cases` (PRD-027).

---

## 2. Problem Statement

### 2.1 TAG has no standardized, benchmarkable code-editing capability

TAG's primary coding workflow delegates all file manipulation to Hermes (Claude Code CLI). There is no TAG-native mechanism to open a file at a specific line, make a bounded edit, verify the edit with a linter, and present the result to the model in a compact windowed format. This means TAG cannot be evaluated on SWE-bench, cannot replicate the ACI pattern that produces the field's best results, and cannot guarantee that its code editing is lint-safe before each turn. The absence of a self-contained ACI harness also means TAG cannot operate in environments where Hermes is unavailable (air-gapped CI, Docker eval containers, non-Claude backends).

### 2.2 Agentic loops without all three stopping conditions are unsafe

PRD-055 introduced an issue-to-PR loop with success / failure / budget stopping conditions, but the general `tag solve` surface (when `--harness` is omitted) delegates to Hermes and has no explicit turn budget, cost ceiling, or diff-size guard. A SWE-bench evaluation run over 500 instances with an unbounded per-instance loop can consume thousands of dollars and hours of wall time before any human notices. The SWE-bench harness must enforce all three stopping conditions as non-bypassable primitives: (a) success — `submit` tool called with a non-empty patch and FAIL_TO_PASS tests pass, (b) failure — model emits `INFEASIBLE` signal or unrecoverable tool error, (c) budget — any of `max_turns`, `max_cost_usd`, `max_wall_seconds` exceeded.

### 2.3 No unified diff prediction output exists for external evaluation

SWE-bench evaluation expects a `predictions.jsonl` file where each line is `{"instance_id": "...", "model_patch": "...", "model_name_or_path": "..."}`. TAG currently has no mechanism to produce this artifact. Without it, TAG cannot submit to the SWE-bench leaderboard, cannot run `swebench.evaluation.run_evaluation` locally, and cannot track its own resolution rate over time. The `tag benchmark --harness swe` command must produce this file as a primary output, making TAG's SWE-bench participation a first-class, one-command operation.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | Implement the canonical SWE-agent ACI tool set in `swe_harness.py`: `open`, `scroll_down`, `scroll_up`, `goto`, `edit` (with lint guard), `search_file`, `search_dir`, `find_file`, `create`, `submit`, plus bounded bash. |
| G2 | `tag solve --harness swe --issue <url>` resolves a GitHub issue end-to-end using the ACI harness, producing a local git patch and optionally a PR. |
| G3 | `tag benchmark --harness swe --dataset swe-bench-lite` runs the harness against SWE-bench-lite (500 instances), produces `predictions.jsonl`, and stores per-instance results in SQLite. |
| G4 | The harness loop enforces all three mandatory stopping conditions (success, failure, budget) as non-bypassable code paths; no configuration option disables all three simultaneously. |
| G5 | Every ACI tool call emits an OpenTelemetry span via `tracing.py` with `tool.name`, `tool.args`, `tool.exit_code`, and `tool.output_chars` attributes. |
| G6 | All agent-executed bash commands route through `sandbox.py` (PRD-028) when `--sandbox docker` is specified (default for benchmark runs). |
| G7 | The harness produces SWE-bench-compatible JSONL predictions: `{"instance_id", "model_patch", "model_name_or_path"}` per instance. |
| G8 | Lint-on-edit is enforced by default: after every `edit` call, `flake8 --select=E9,F` (syntax errors only) or `python -m py_compile` runs on the modified file; edits that introduce syntax errors are rejected and the model is informed. |
| G9 | `--dry-run` mode loads the issue / SWE-bench instance, builds the initial context, prints the estimated cost, and exits without making any LLM or tool calls. |
| G10 | Harness state (CURRENT_FILE, FIRST_LINE, WINDOW_SIZE, turn count, cost accumulator) is persisted to SQLite every turn so a crashed run can be resumed with `tag solve --resume <run-id>`. |

## 3.1 Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Replacing Hermes for general `tag run` or `tag chat` workflows. The SWE harness is opt-in via `--harness swe`; default behavior is unchanged. |
| NG2 | Building or managing Docker images for SWE-bench instances. The harness expects a pre-built instance image (as produced by `swebench.harness.docker_build`) to be present; TAG does not build images. |
| NG3 | Submitting predictions to the official SWE-bench leaderboard automatically. Leaderboard submission requires human review; TAG only produces the predictions file. |
| NG4 | SWE-bench evaluation of non-Python repositories in v1. The lint guard uses Python syntax checkers; non-Python languages get best-effort lint (ESLint for JS, `go build` for Go) but this is not guaranteed. |
| NG5 | Interactive / human-in-the-loop mode for the ACI harness. The harness is designed for autonomous operation; `--interactive` mode is a future extension. |
| NG6 | Fine-tuning models on SWE-bench trajectories collected by TAG. Trajectory collection is a side effect; fine-tuning is a separate system. |
| NG7 | Running SWE-bench-verified (full 2,294-instance set) without explicit `--dataset swe-bench-verified` flag and `--confirm-large`. The default dataset is `swe-bench-lite` (500 instances). |
| NG8 | Replacing PRD-055 (`tag issue-solve`). PRD-055 and PRD-064 solve overlapping problems at different abstraction levels. PRD-064 provides the ACI tool primitives; PRD-055 provides the issue-fetching and PR-creation wrapper. In v2, PRD-055 will delegate its code-editing loop to PRD-064's harness. |

---

## 4. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|-------------------|
| SWE-bench-lite resolution rate | ≥ 12% (competitive with open-source agents as of 2025) | `tag benchmark --harness swe --dataset swe-bench-lite` + `swebench.evaluation.run_evaluation` |
| ACI vs raw-bash improvement | Harness resolves ≥ 1.5x more instances than `tag solve` without `--harness swe` on a 50-instance SWE-bench sample | Controlled A/B run on 50 sampled instances |
| Turn budget enforcement | Zero benchmark runs exceed `max_turns` or `max_cost_usd`; verified across 500-instance run | Assertions in `swe_benchmark_results` table: `turns <= max_turns` for all rows |
| Lint-on-edit block rate | < 5% of edit calls produce a lint error that blocks the edit (indicating ACI is steering the model toward valid edits) | `swe_tool_calls` table: `lint_blocked = 1` count / total `edit` calls |
| Cost per SWE-bench-lite instance | < $0.50/instance median at `claude-sonnet-4-6` pricing | `swe_benchmark_results.cost_usd` percentiles |
| JSONL predictions file produced | 100% of benchmark runs produce a valid `predictions.jsonl` file with one entry per instance | File existence + JSON schema validation test |
| OTel span coverage | 100% of ACI tool calls produce a child span under the active harness trace | Unit test asserting span count = tool call count |
| Resume correctness | A run killed at turn N can be resumed and produces the same prediction as an uninterrupted run (deterministic seed) | Integration test: SIGKILL at turn 5, resume, compare final patch |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag solve --harness swe --issue https://github.com/owner/repo/issues/42` | TAG autonomously fixes the issue using structured ACI tools and produces a local patch and optional PR without me manually editing files |
| U2 | ML/AI researcher | run `tag benchmark --harness swe --dataset swe-bench-lite` | I can measure TAG's actual SWE-bench resolution rate and track it over time as models and profiles evolve |
| U3 | Platform engineer | set `--max-cost-usd 0.50 --max-turns 30` on a benchmark run | I have a hard cost and time ceiling; no single instance can blow my API budget regardless of model behavior |
| U4 | Developer | run `tag solve --harness swe --patch-file issue.patch --profile coder` | I can apply a pre-fetched issue patch through the ACI harness with a specific profile, useful for offline or air-gapped environments |
| U5 | Developer | run `tag solve --harness swe --issue <url> --dry-run` | I see the estimated cost and the initial context that would be sent to the model before spending any money |
| U6 | Researcher | inspect `swe_tool_calls` in SQLite after a benchmark run | I can analyze which ACI tools the model uses most, where it gets stuck, and which tool call patterns precede successful patches |
| U7 | Developer | run `tag solve --resume <run-id>` | A run killed mid-way by a timeout or crash continues from the last persisted turn rather than restarting from scratch |
| U8 | CI engineer | run `tag benchmark --harness swe --dataset swe-bench-lite --output-jsonl predictions.jsonl --json` in GitHub Actions | The benchmark produces a machine-readable JSON summary and a predictions file that CI can archive and compare against previous runs |
| U9 | Developer | observe `[turn 12/30] edit src/foo.py:45:67 — BLOCKED (SyntaxError: invalid syntax line 47)` in real-time TUI output | I understand that the lint guard caught an invalid edit and the model is being asked to try again, without the lint error propagating to the live codebase |
| U10 | Security engineer | run `tag solve --harness swe --sandbox docker --issue <url>` | All bash commands executed by the ACI harness run inside a Docker container, so even if the model generates malicious commands the host filesystem is safe |

---

## 6. Proposed CLI Surface

### 6.1 `tag solve --harness swe` (single-issue mode)

```bash
tag solve \
  --harness swe \
  --issue https://github.com/owner/repo/issues/42 \
  [--profile coder] \
  [--model anthropic/claude-sonnet-4-6] \
  [--max-turns 30] \
  [--max-cost-usd 2.00] \
  [--max-wall-seconds 1800] \
  [--sandbox docker] \
  [--sandbox-image python:3.12-slim] \
  [--window-size 100] \
  [--lint-cmd "flake8 --select=E9,F {file}"] \
  [--auto-pr] \
  [--branch-prefix swe/] \
  [--dry-run] \
  [--json] \
  [--yes]

# Patch-file mode (pre-fetched issue as unified diff or plain text)
tag solve \
  --harness swe \
  --patch-file issue.patch \
  --profile coder \
  [--repo-path /path/to/repo] \
  [--max-turns 30] \
  [--max-cost-usd 2.00]

# Resume a previously interrupted solve run
tag solve --resume <run-id> [--max-turns 30]
```

**Flags:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--harness` | str | (none) | Select harness; `swe` activates this feature |
| `--issue` | str | — | GitHub/Linear/Jira issue URL or ID |
| `--patch-file` | path | — | Pre-fetched issue text or unified diff file |
| `--profile` | str | `coder` | TAG profile to use for LLM routing |
| `--model` | str | profile default | Override LLM model for this run |
| `--max-turns` | int | `30` | Hard turn limit; harness exits on budget |
| `--max-cost-usd` | float | `2.00` | Hard cost ceiling in USD |
| `--max-wall-seconds` | int | `1800` | Hard wall-clock limit in seconds |
| `--sandbox` | str | `restricted` | Sandbox backend: `docker`, `modal`, `restricted` |
| `--sandbox-image` | str | `python:3.12-slim` | Docker image for sandbox backend |
| `--window-size` | int | `100` | Lines shown per `open`/`scroll` operation |
| `--lint-cmd` | str | `python -m py_compile {file}` | Lint command template; `{file}` is replaced |
| `--auto-pr` | flag | `false` | Open PR automatically on success |
| `--branch-prefix` | str | `swe/` | Git branch prefix for auto-PR mode |
| `--dry-run` | flag | `false` | Load context, estimate cost, exit |
| `--json` | flag | `false` | Machine-readable JSON output |
| `--yes` | flag | `false` | Skip cost confirmation |
| `--resume` | str | — | Resume a previous solve run by ID |

**Sample output (TTY mode):**

```
tag solve --harness swe --issue https://github.com/pallets/flask/issues/5432

Fetching issue #5432 from github.com/pallets/flask ...
  Title: Blueprint url_prefix not inherited by nested blueprints
  Labels: bug, needs-investigation
  Estimated cost: 30 turns × ~$0.03/turn = ~$0.90  Proceed? [y/N] y

Cloning repo pallets/flask @ commit a3f91c2 into /tmp/swe-pallets-flask-5432/ ...
Starting SWE harness (model: claude-sonnet-4-6, turns: 0/30, cost: $0.00)

[turn  1/30] bash: find . -name "*.py" | xargs grep -l "url_prefix" | head -20
             → 6 files, 847 chars
[turn  2/30] open src/flask/blueprints.py 1
             → Showing lines 1-100 of 912 (CURRENT_FILE=src/flask/blueprints.py FIRST_LINE=1)
[turn  3/30] search_file "url_prefix" src/flask/blueprints.py
             → 14 matches in src/flask/blueprints.py
[turn  4/30] goto 347
             → Showing lines 297-397 of 912
[turn  5/30] edit 352:361
             → Applying edit to lines 352-361 ...
             → Lint OK (python -m py_compile)
             → Edit applied (9 lines replaced with 12 lines)
[turn  6/30] bash: cd /tmp/swe-pallets-flask-5432 && python -m pytest tests/test_blueprints.py -x -q 2>&1 | tail -20
             → 47 passed, 0 failed in 3.2s
[turn  7/30] submit
             → Patch generated: 34 lines changed across 1 file

 Solve complete  (turns: 7/30, cost: $0.21, wall: 42s)
Patch written to: /tmp/swe-pallets-flask-5432.patch
Run-id: swe-a3f91c2-20260612-143201

Creating branch swe/issue-5432 and opening PR ...
PR opened: https://github.com/pallets/flask/pull/5489
```

### 6.2 `tag benchmark --harness swe` (batch evaluation mode)

```bash
tag benchmark \
  --harness swe \
  --dataset swe-bench-lite \
  [--dataset swe-bench-verified] \
  [--profile coder] \
  [--model anthropic/claude-sonnet-4-6] \
  [--max-turns 30] \
  [--max-cost-usd 0.50] \
  [--max-wall-seconds 900] \
  [--sandbox docker] \
  [--parallel 4] \
  [--instances 50] \
  [--instance-filter "django__django-*"] \
  [--output-jsonl predictions.jsonl] \
  [--output-dir ./swe-results/] \
  [--resume-run <run-id>] \
  [--confirm-large] \
  [--json]
```

**Flags (benchmark-specific):**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--dataset` | str | `swe-bench-lite` | Dataset: `swe-bench-lite` (500), `swe-bench-verified` (500), `swe-bench-full` (2294) |
| `--parallel` | int | `1` | Concurrent instance workers (each in own Docker container) |
| `--instances` | int | all | Limit to N instances (for smoke tests) |
| `--instance-filter` | str | — | Glob filter on `instance_id` (e.g. `django__*`) |
| `--output-jsonl` | path | `predictions.jsonl` | Output file for SWE-bench predictions |
| `--output-dir` | path | `./swe-results/` | Directory for per-instance trajectory logs |
| `--resume-run` | str | — | Resume a partial benchmark run by run-id |
| `--confirm-large` | flag | `false` | Required for datasets > 500 instances |

**Sample output (batch mode):**

```
tag benchmark --harness swe --dataset swe-bench-lite --parallel 4

Dataset: swe-bench-lite (500 instances)
Profile: coder  Model: claude-sonnet-4-6
Budget: 30 turns / $0.50 / 900s per instance
Estimated total cost: ~$250.00  Proceed? [y/N] y

Progress: ████████████░░░░░░░░  248/500 instances
  Resolved:  62 / 248 (25.0%)   [running baseline...]
  Budget hit: 18 / 248 (7.3%)
  Failed:      4 / 248 (1.6%)
  Elapsed: 2h 14m  Est. remaining: 2h 18m

[DONE] django__django-14667   resolved  (turns: 12, cost: $0.19, 44s)
[DONE] sympy__sympy-20639     timeout   (turns: 30, cost: $0.50, 903s)
[DONE] astropy__astropy-12907 resolved  (turns: 8, cost: $0.14, 31s)

Final Results (500/500 instances):
  Resolved:        147 / 500  (29.4%)
  Budget exceeded:  89 / 500  (17.8%)
  Failed:           12 / 500   (2.4%)
  No patch:        252 / 500  (50.4%)
  Total cost: $198.42  Wall time: 4h 37m

Predictions written to: predictions.jsonl (500 entries)
Run-id: bench-swe-20260612-090000
Results in SQLite: swe_benchmark_runs, swe_benchmark_results
```

### 6.3 `tag solve status <run-id>` (run inspection)

```bash
tag solve status swe-a3f91c2-20260612-143201 [--json]
tag solve list --harness swe [--last 20] [--json]
tag solve trajectory <run-id> [--json]
```

`tag solve trajectory <run-id>` prints the full turn-by-turn log: tool name, arguments, truncated output, and per-turn cost.

---

## 7. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | `swe_harness.py` must implement all ten ACI tools verbatim: `open`, `scroll_down`, `scroll_up`, `goto`, `edit`, `search_file`, `search_dir`, `find_file`, `create`, `submit`. Each tool must accept the same argument signature as SWE-agent's bash script implementations. |
| FR-02 | The `open` tool must display exactly `WINDOW_SIZE` lines (default 100) centered on the requested line number, with line numbers in the left margin. It must set `session.current_file` and `session.first_line` on the `HarnessSession` object. |
| FR-03 | The `edit <start_line>:<end_line>` tool must accept the replacement text as a heredoc-delimited argument and replace exactly lines `start_line` to `end_line` (inclusive, 1-indexed) in `session.current_file`. If the file has fewer lines than `end_line`, the edit must be rejected with an error message. |
| FR-04 | After every successful `edit` call, the harness must run the configured lint command against the modified file. If the linter exits non-zero, the edit must be rolled back (the file restored to its pre-edit content), `session.lint_blocked_count` must be incremented, and the model must receive the linter output as the tool result with the prefix `LINT ERROR — edit rejected:`. |
| FR-05 | The `bash` tool must route all commands through `sandbox.py` when `--sandbox` is not `none`. The sandbox backend must be initialized once at harness startup and reused for all bash calls. The bash tool must cap output at 8192 characters (truncating with a `[... N more chars truncated]` suffix) to prevent context overflow. |
| FR-06 | All three stopping conditions must be checked at the start of every turn, before dispatching the next tool call: (a) success — `submit` was called, (b) failure — error count exceeds 5 consecutive tool errors, (c) budget — `session.turns >= max_turns` OR `session.cost_usd >= max_cost_usd` OR `session.wall_seconds >= max_wall_seconds`. |
| FR-07 | The `submit` tool must compute a unified diff between the working tree and the base commit, write it to `session.patch_path`, set `session.status = "resolved"`, and return the diff stats to the model as confirmation. If the diff is empty, `submit` must be rejected with `SUBMIT ERROR — patch is empty; make at least one edit before submitting`. |
| FR-08 | Every tool call must write one row to `swe_tool_calls` in SQLite: `(id, run_id, turn, tool_name, args_json, output_truncated, lint_blocked, exit_code, cost_usd, duration_ms, created_at)`. This write must happen even if the tool call raises an exception (status `error`). |
| FR-09 | `HarnessSession` state must be serialized to `swe_solve_runs.state_json` after every turn using `json.dumps(session.to_dict())`. On resume (`tag solve --resume <run-id>`), the harness must restore all session fields from this JSON and continue from `session.turns`. |
| FR-10 | `tag benchmark --harness swe` must load SWE-bench instances from the `princeton-nlp/SWE-bench_Lite` HuggingFace dataset (via `datasets` library) or from a local JSONL file specified by `--dataset-path`. It must spawn one `HarnessSolveRunner` per instance and collect results. |
| FR-11 | The benchmark runner must write one row per instance to `swe_benchmark_results`: `(id, benchmark_run_id, instance_id, repo, status, turns, cost_usd, wall_seconds, patch, created_at)`. Status must be one of: `resolved`, `timeout`, `budget`, `failed`, `no_patch`. |
| FR-12 | After all instances complete, `tag benchmark` must write `predictions.jsonl` where each line is `{"instance_id": "...", "model_patch": "...", "model_name_or_path": "..."}`. Instances with no patch must have `"model_patch": ""`. |
| FR-13 | `--parallel N` must spawn at most N concurrent harness runs using `asyncio.gather` or `concurrent.futures.ThreadPoolExecutor`. Each parallel run must use its own isolated working directory and `HarnessSession` object; no shared mutable state. |
| FR-14 | `--dry-run` must fetch the issue (or load the first SWE-bench instance), build the initial context string, compute estimated cost as `max_turns × estimated_cost_per_turn`, print the estimate and initial context, and exit 0 without making any LLM calls or tool calls. |
| FR-15 | `--instance-filter` must accept a glob pattern applied to `instance_id` using `fnmatch.fnmatch`. Only matching instances are run; the benchmark reports the total filtered count alongside the full dataset size. |
| FR-16 | OpenTelemetry spans must be emitted for each turn via `tracing.py`. Each turn span must be a child of the harness run span. Span attributes must include: `swe.turn`, `swe.tool_name`, `swe.tool_args`, `swe.exit_code`, `swe.output_chars`, `swe.cost_usd`, `swe.lint_blocked`. |
| FR-17 | `search_file <pattern> [<file>]` must run `grep -n <pattern> <file>` (or current file if omitted) inside the sandbox and return at most 100 matching lines, truncated with a count of remaining matches. |
| FR-18 | `find_file <filename> [<dir>]` must run `find <dir|.> -name "<filename>"` inside the sandbox and return at most 50 results. |
| FR-19 | The `create <path>` tool must create the file at the given path relative to the repo root (creating intermediate directories), open it in the window, and set `session.current_file`. If the file already exists, it must return an error: `FILE EXISTS — use open to view and edit existing files`. |
| FR-20 | `tag solve list --harness swe` must query `swe_solve_runs` and display: run ID, issue URL, status, turns, cost USD, wall time, created at. `--last N` limits to the N most recent rows. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **Context window safety:** Tool output returned to the model must never exceed 8192 characters per call. All tools must apply truncation before returning output to the LLM. The file window (100 lines × ~80 chars) fits comfortably within this limit. |
| NFR-02 | **SQLite WAL mode:** All writes to `swe_tool_calls`, `swe_solve_runs`, `swe_benchmark_runs`, `swe_benchmark_results` must go through `open_db(cfg)` which enforces WAL mode and `PRAGMA busy_timeout = 5000`. No direct `sqlite3.connect()` calls in `swe_harness.py`. |
| NFR-03 | **Benchmark parallelism isolation:** Each parallel benchmark worker must operate in its own `/tmp/swe-<instance_id>-<uuid>/` directory. Workers must not share file handles, SQLite connections, or Python mutable state. Thread-local storage or `asyncio` task context must be used for all per-worker state. |
| NFR-04 | **Deterministic turn ordering:** Within a single solve run, tool calls must be dispatched sequentially (one at a time). The harness must not issue parallel tool calls to the model or the sandbox within a single run (parallelism is only across benchmark instances). |
| NFR-05 | **Crash safety:** If the harness process is killed between turns, the most recently committed SQLite row in `swe_solve_runs.state_json` must be sufficient to resume the run. No turn may be considered "in progress" without a committed state row. |
| NFR-06 | **Lint command injection prevention:** The `--lint-cmd` flag value must be validated against a whitelist of safe command prefixes: `flake8`, `pylint`, `python -m py_compile`, `mypy`, `ruff`, `eslint`, `go build`. Arbitrary shell commands are rejected at startup with a descriptive error. |
| NFR-07 | **Cost tracking accuracy:** Per-turn cost must be computed from actual token counts (input + output) × model-specific pricing from TAG's pricing table (`budget.py`), not estimated. Cost accumulates in `session.cost_usd` (float) and is written to `swe_tool_calls.cost_usd` each turn. |
| NFR-08 | **Benchmark resumability:** If `--resume-run <run-id>` is specified, the benchmark must skip all instances with an existing row in `swe_benchmark_results` for that `benchmark_run_id` and only process the remaining instances. |
| NFR-09 | **No subprocess shell injection:** All `bash` tool commands must be executed as `subprocess.run(shlex.split(cmd), ...)` (list form, not `shell=True`) inside the sandbox. Shell metacharacters in the command string are passed as literal arguments, not interpreted. The sandbox's `blocked_patterns` list from PRD-028 applies. |
| NFR-10 | **Predictions file atomicity:** `predictions.jsonl` must be written atomically using a temp file + `os.replace()` to avoid partial writes that could corrupt downstream evaluation scripts. |

---

## 9. Technical Design

### 9.1 New Files

| File | Purpose |
|------|---------|
| `src/tag/swe_harness.py` | Core ACI harness: `HarnessSession`, all ACI tools, `HarnessSolveRunner`, `BenchmarkRunner` |
| `src/tag/swe_dataset.py` | SWE-bench dataset loader: HuggingFace + local JSONL; `SWEBenchInstance` dataclass |

### 9.2 Modified Files

| File | Change |
|------|--------|
| `src/tag/controller.py` | Add `cmd_solve` function; extend `cmd_benchmark` for `--harness swe`; register new SQLite tables in `open_db()` migration |

### 9.3 SQLite DDL

All tables are created inside `open_db()` via the existing migration pattern in `controller.py`. The DDL below uses `TEXT` for timestamps (ISO-8601 UTC, consistent with all existing TAG tables) and `REAL` for floating-point cost/score values.

```sql
-- PRD-064: SWE Harness solve runs
CREATE TABLE IF NOT EXISTS swe_solve_runs (
  id             TEXT PRIMARY KEY,
  issue_url      TEXT,
  patch_file     TEXT,
  repo_path      TEXT NOT NULL DEFAULT '',
  profile        TEXT NOT NULL DEFAULT 'coder',
  model          TEXT NOT NULL DEFAULT '',
  status         TEXT NOT NULL DEFAULT 'running',
  -- status: running | resolved | timeout | budget | failed | no_patch
  turns          INTEGER NOT NULL DEFAULT 0,
  max_turns      INTEGER NOT NULL DEFAULT 30,
  cost_usd       REAL NOT NULL DEFAULT 0.0,
  max_cost_usd   REAL NOT NULL DEFAULT 2.0,
  wall_seconds   REAL NOT NULL DEFAULT 0.0,
  max_wall_seconds INTEGER NOT NULL DEFAULT 1800,
  patch_path     TEXT,
  patch_diff     TEXT,
  lint_blocked   INTEGER NOT NULL DEFAULT 0,
  state_json     TEXT NOT NULL DEFAULT '{}',
  -- Serialized HarnessSession fields for resumability
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL,
  completed_at   TEXT
);
CREATE INDEX IF NOT EXISTS idx_swe_runs_status
  ON swe_solve_runs(status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_swe_runs_issue
  ON swe_solve_runs(issue_url, created_at DESC);

-- PRD-064: Per-turn ACI tool call log
CREATE TABLE IF NOT EXISTS swe_tool_calls (
  id              TEXT PRIMARY KEY,
  run_id          TEXT NOT NULL,
  turn            INTEGER NOT NULL,
  tool_name       TEXT NOT NULL,
  -- tool_name: open | scroll_down | scroll_up | goto | edit |
  --            search_file | search_dir | find_file | create | submit | bash
  args_json       TEXT NOT NULL DEFAULT '{}',
  output_truncated TEXT NOT NULL DEFAULT '',
  output_chars    INTEGER NOT NULL DEFAULT 0,
  lint_blocked    INTEGER NOT NULL DEFAULT 0,
  exit_code       INTEGER,
  cost_usd        REAL NOT NULL DEFAULT 0.0,
  duration_ms     INTEGER NOT NULL DEFAULT 0,
  created_at      TEXT NOT NULL,
  FOREIGN KEY(run_id) REFERENCES swe_solve_runs(id)
);
CREATE INDEX IF NOT EXISTS idx_swe_tc_run
  ON swe_tool_calls(run_id, turn);
CREATE INDEX IF NOT EXISTS idx_swe_tc_tool
  ON swe_tool_calls(tool_name, lint_blocked);

-- PRD-064: SWE-bench benchmark batch runs
CREATE TABLE IF NOT EXISTS swe_benchmark_runs (
  id              TEXT PRIMARY KEY,
  dataset         TEXT NOT NULL DEFAULT 'swe-bench-lite',
  profile         TEXT NOT NULL DEFAULT 'coder',
  model           TEXT NOT NULL DEFAULT '',
  instance_filter TEXT,
  parallel        INTEGER NOT NULL DEFAULT 1,
  max_turns       INTEGER NOT NULL DEFAULT 30,
  max_cost_usd    REAL NOT NULL DEFAULT 0.50,
  max_wall_seconds INTEGER NOT NULL DEFAULT 900,
  total_instances INTEGER NOT NULL DEFAULT 0,
  resolved        INTEGER NOT NULL DEFAULT 0,
  budget_exceeded INTEGER NOT NULL DEFAULT 0,
  failed          INTEGER NOT NULL DEFAULT 0,
  no_patch        INTEGER NOT NULL DEFAULT 0,
  total_cost_usd  REAL NOT NULL DEFAULT 0.0,
  predictions_path TEXT,
  status          TEXT NOT NULL DEFAULT 'running',
  created_at      TEXT NOT NULL,
  updated_at      TEXT NOT NULL,
  completed_at    TEXT
);
CREATE INDEX IF NOT EXISTS idx_swe_bench_status
  ON swe_benchmark_runs(status, created_at DESC);

-- PRD-064: Per-instance benchmark results
CREATE TABLE IF NOT EXISTS swe_benchmark_results (
  id                TEXT PRIMARY KEY,
  benchmark_run_id  TEXT NOT NULL,
  instance_id       TEXT NOT NULL,
  repo              TEXT NOT NULL DEFAULT '',
  solve_run_id      TEXT,
  status            TEXT NOT NULL DEFAULT 'no_patch',
  -- status: resolved | timeout | budget | failed | no_patch
  turns             INTEGER NOT NULL DEFAULT 0,
  cost_usd          REAL NOT NULL DEFAULT 0.0,
  wall_seconds      REAL NOT NULL DEFAULT 0.0,
  patch             TEXT NOT NULL DEFAULT '',
  fail_to_pass_json TEXT NOT NULL DEFAULT '[]',
  pass_to_pass_json TEXT NOT NULL DEFAULT '[]',
  created_at        TEXT NOT NULL,
  FOREIGN KEY(benchmark_run_id) REFERENCES swe_benchmark_runs(id)
);
CREATE INDEX IF NOT EXISTS idx_swe_results_bench
  ON swe_benchmark_results(benchmark_run_id, status);
CREATE INDEX IF NOT EXISTS idx_swe_results_instance
  ON swe_benchmark_results(instance_id, benchmark_run_id);
```

### 9.4 Core Dataclasses

```python
# src/tag/swe_harness.py
from __future__ import annotations

import json
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


@dataclass
class SWEBenchInstance:
    """Normalized SWE-bench task instance (matches princeton-nlp/SWE-bench schema)."""
    instance_id: str
    repo: str
    base_commit: str
    problem_statement: str
    hints_text: str
    fail_to_pass: list[str]  # Test IDs that must flip from FAIL to PASS
    pass_to_pass: list[str]  # Test IDs that must remain PASS (regression guard)
    environment_setup_commit: str = ""
    patch: str = ""           # Ground-truth patch (used in evaluation, not given to model)
    test_patch: str = ""      # Test patch (used in evaluation only)


@dataclass
class HarnessSession:
    """Mutable state for a single SWE harness solve run. Serialized to SQLite every turn."""
    run_id: str
    repo_path: Path
    base_commit: str
    model: str
    profile: str

    # ACI navigation state
    current_file: str = ""      # Relative path from repo_path
    first_line: int = 1         # First line of current window (1-indexed)
    window_size: int = 100

    # Budget tracking
    turns: int = 0
    max_turns: int = 30
    cost_usd: float = 0.0
    max_cost_usd: float = 2.0
    wall_seconds: float = 0.0
    max_wall_seconds: int = 1800
    start_time: float = field(default_factory=time.monotonic)

    # Quality tracking
    lint_blocked_count: int = 0
    consecutive_errors: int = 0

    # Outcome
    status: str = "running"     # running | resolved | timeout | budget | failed | no_patch
    patch_path: str = ""
    patch_diff: str = ""

    # Lint configuration
    lint_cmd: str = "python -m py_compile {file}"

    def to_dict(self) -> dict[str, Any]:
        return {
            "run_id": self.run_id,
            "current_file": self.current_file,
            "first_line": self.first_line,
            "window_size": self.window_size,
            "turns": self.turns,
            "max_turns": self.max_turns,
            "cost_usd": self.cost_usd,
            "max_cost_usd": self.max_cost_usd,
            "wall_seconds": self.wall_seconds,
            "max_wall_seconds": self.max_wall_seconds,
            "lint_blocked_count": self.lint_blocked_count,
            "consecutive_errors": self.consecutive_errors,
            "status": self.status,
            "patch_path": self.patch_path,
            "patch_diff": self.patch_diff,
            "lint_cmd": self.lint_cmd,
        }

    @classmethod
    def from_dict(cls, d: dict[str, Any], repo_path: Path) -> "HarnessSession":
        session = cls(
            run_id=d["run_id"],
            repo_path=repo_path,
            base_commit="",   # Restored from swe_solve_runs row
            model="",
            profile="",
        )
        for key in ("current_file", "first_line", "window_size", "turns",
                    "max_turns", "cost_usd", "max_cost_usd", "wall_seconds",
                    "max_wall_seconds", "lint_blocked_count", "consecutive_errors",
                    "status", "patch_path", "patch_diff", "lint_cmd"):
            if key in d:
                setattr(session, key, d[key])
        return session

    def budget_exceeded(self) -> str | None:
        """Return the exceeded budget dimension name, or None if within budget."""
        elapsed = time.monotonic() - self.start_time
        if self.turns >= self.max_turns:
            return "max_turns"
        if self.cost_usd >= self.max_cost_usd:
            return "max_cost_usd"
        if elapsed >= self.max_wall_seconds:
            return "max_wall_seconds"
        return None


@dataclass
class ToolResult:
    """Structured result from an ACI tool call."""
    tool_name: str
    args: dict[str, Any]
    output: str           # Truncated to MAX_OUTPUT_CHARS
    output_chars: int     # Total chars before truncation
    exit_code: int = 0    # 0 = success, non-zero = error or blocked
    lint_blocked: bool = False
    cost_usd: float = 0.0
    duration_ms: int = 0


MAX_OUTPUT_CHARS = 8192
SAFE_LINT_PREFIXES = frozenset([
    "flake8", "pylint", "python -m py_compile", "mypy",
    "ruff", "eslint", "go build", "tsc",
])
```

### 9.5 ACI Tool Implementation — Key Algorithms

#### `open` tool
```python
def tool_open(session: HarnessSession, file: str, lineno: int = 1) -> ToolResult:
    path = session.repo_path / file
    if not path.is_file():
        return ToolResult("open", {"file": file, "lineno": lineno},
                          f"FILE NOT FOUND: {file}", 0, exit_code=1)
    lines = path.read_text(errors="replace").splitlines()
    total = len(lines)
    # Center window on lineno; clamp to [1, total]
    first = max(1, min(lineno, total - session.window_size + 1))
    last = min(total, first + session.window_size - 1)
    window = "\n".join(
        f"{i+first:6d}  {lines[i+first-1]}"
        for i in range(last - first + 1)
    )
    header = f"[File: {file} ({total} lines total)] [Lines {first}-{last}]"
    output = f"{header}\n{window}"
    session.current_file = file
    session.first_line = first
    return ToolResult("open", {"file": file, "lineno": lineno},
                      _truncate(output), len(output))
```

#### `edit` tool with lint guard
```python
def tool_edit(
    session: HarnessSession,
    start_line: int,
    end_line: int,
    replacement: str,
    sandbox: Any,
) -> ToolResult:
    if not session.current_file:
        return ToolResult("edit", {}, "NO OPEN FILE — call open first", 0, exit_code=1)
    path = session.repo_path / session.current_file
    lines = path.read_text(errors="replace").splitlines(keepends=True)
    if end_line > len(lines):
        return ToolResult(
            "edit", {"start": start_line, "end": end_line},
            f"LINE RANGE ERROR: file has {len(lines)} lines, end_line={end_line}", 0,
            exit_code=1,
        )
    # Save backup for rollback
    backup = "".join(lines)
    new_lines = (
        lines[:start_line - 1]
        + [replacement if replacement.endswith("\n") else replacement + "\n"]
        + lines[end_line:]
    )
    path.write_text("".join(new_lines))

    # Lint gate
    lint_cmd = session.lint_cmd.format(file=str(path))
    lint_result = subprocess.run(
        shlex.split(lint_cmd), capture_output=True, text=True, timeout=10
    )
    if lint_result.returncode != 0:
        # Rollback
        path.write_text(backup)
        session.lint_blocked_count += 1
        err = (lint_result.stdout + lint_result.stderr)[:2000]
        return ToolResult(
            "edit", {"start": start_line, "end": end_line},
            f"LINT ERROR — edit rejected:\n{err}", len(err),
            exit_code=2, lint_blocked=True,
        )

    old_count = end_line - start_line + 1
    new_count = replacement.count("\n") + 1
    output = (
        f"Edit applied to {session.current_file} lines {start_line}-{end_line}.\n"
        f"Replaced {old_count} lines with {new_count} lines. Lint: OK."
    )
    return ToolResult("edit", {"start": start_line, "end": end_line},
                      output, len(output))
```

#### `submit` tool
```python
def tool_submit(session: HarnessSession) -> ToolResult:
    result = subprocess.run(
        ["git", "diff", session.base_commit, "--", "."],
        capture_output=True, text=True, cwd=session.repo_path,
    )
    diff = result.stdout
    if not diff.strip():
        return ToolResult("submit", {}, "SUBMIT ERROR — patch is empty; make at least one edit before submitting", 0, exit_code=1)
    patch_path = Path(f"/tmp/swe-{session.run_id}.patch")
    patch_path.write_text(diff)
    session.patch_path = str(patch_path)
    session.patch_diff = diff
    session.status = "resolved"
    lines_changed = sum(1 for l in diff.splitlines() if l.startswith(("+", "-")) and not l.startswith(("+++", "---")))
    output = f"Patch submitted: {len(diff)} bytes, ~{lines_changed} lines changed. Saved to {patch_path}."
    return ToolResult("submit", {}, output, len(output))
```

### 9.6 Main Agentic Loop

```python
def run_harness_loop(
    session: HarnessSession,
    issue_text: str,
    conn: sqlite3.Connection,
    llm_client: Any,
    sandbox: Any,
) -> HarnessSession:
    """Main turn loop. Returns session with final status set."""
    system_prompt = _build_system_prompt(session)
    messages: list[dict] = [{"role": "user", "content": issue_text}]

    while True:
        # --- Check all three stopping conditions first ---
        exceeded = session.budget_exceeded()
        if exceeded:
            session.status = "budget"
            _persist_session(conn, session)
            break
        if session.consecutive_errors >= 5:
            session.status = "failed"
            _persist_session(conn, session)
            break
        if session.status == "resolved":
            break

        # --- LLM inference ---
        t0 = time.monotonic()
        response = llm_client.complete(
            model=session.model,
            system=system_prompt,
            messages=messages,
        )
        turn_cost = _compute_cost(response, session.model)
        session.cost_usd += turn_cost
        session.turns += 1

        # --- Parse tool call from response ---
        tool_name, tool_args = _parse_tool_call(response.content)
        if tool_name is None:
            # Model produced no tool call — count as error
            session.consecutive_errors += 1
            messages.append({"role": "assistant", "content": response.content})
            messages.append({"role": "user", "content": "Please call one of the available tools."})
            _persist_session(conn, session)
            continue

        # --- Dispatch tool ---
        result = _dispatch_tool(tool_name, tool_args, session, sandbox)
        duration_ms = int((time.monotonic() - t0) * 1000)

        if result.exit_code == 0:
            session.consecutive_errors = 0
        else:
            session.consecutive_errors += 1

        # --- Persist tool call to SQLite ---
        _write_tool_call(conn, session.run_id, session.turns,
                         result, turn_cost, duration_ms)
        _persist_session(conn, session)

        # --- Emit OTel span ---
        _emit_tool_span(session, result, turn_cost)

        # --- Append to message history ---
        messages.append({"role": "assistant", "content": response.content})
        messages.append({"role": "user", "content": result.output})

        if session.status == "resolved":
            break

    return session
```

### 9.7 SWE-bench Predictions Output

```python
def write_predictions_jsonl(
    results: list[dict],
    output_path: Path,
    model_name: str,
) -> None:
    """Write SWE-bench-compatible predictions file atomically."""
    import os, tempfile
    lines = []
    for r in results:
        lines.append(json.dumps({
            "instance_id": r["instance_id"],
            "model_patch": r.get("patch", ""),
            "model_name_or_path": model_name,
        }))
    tmp = Path(tempfile.mktemp(dir=output_path.parent, suffix=".jsonl.tmp"))
    tmp.write_text("\n".join(lines) + "\n")
    os.replace(tmp, output_path)
```

### 9.8 Integration Points

| System | Integration |
|--------|-------------|
| `sandbox.py` (PRD-028) | `tool_bash()` calls `sandbox.run(cmd, workdir=session.repo_path)` using the configured backend; the sandbox instance is constructed once at harness startup and passed to all tool functions |
| `tracing.py` (PRD-013) | `_emit_tool_span()` calls `tracing.start_span("swe.tool_call", ...)` and sets GenAI semantic convention attributes per PRD-041 |
| `budget.py` (PRD-019) | `_compute_cost()` calls `budget.compute_turn_cost(model, input_tokens, output_tokens)` to get accurate per-turn USD cost |
| `diff_context.py` (PRD-038) | Issue fetching (for `--issue <url>`) is handled by the same `IssueFetcher` used in PRD-055, returning a normalized `IssueContext`; the problem statement is formatted as the initial user message |
| `eval_framework.py` (PRD-027) | Benchmark results in `swe_benchmark_results` feed into `tag eval history` if a corresponding eval suite is configured; resolution rate is surfaced as a metric |
| `ci.py` (PRD-020/PRD-042) | `--auto-pr` uses `ci.create_pull_request(branch, title, body, issue_url)` to open the PR after a successful solve |
| `controller.py` `open_db()` | All new tables are created inside the migration chain in `open_db()` following the `_migrate_prd_XXX_tables(conn)` pattern; no direct `sqlite3.connect()` in `swe_harness.py` |
| `loop_agent.py` (PRD-021) | The harness `run_harness_loop()` is architecturally aligned with `loop_agent.py`'s `_run_iteration()` pattern; future v2 will unify them |

---

## 10. Security Considerations

1. **Lint command injection prevention:** The `--lint-cmd` flag is validated against `SAFE_LINT_PREFIXES` at harness startup. Any value not starting with a safe prefix causes immediate exit with error code 1 and a message listing allowed values. This prevents `--lint-cmd "rm -rf / &&"` style attacks.

2. **Sandbox routing for bash tool:** All `bash` tool calls route through `sandbox.py` when `--sandbox` is not `none`. The sandbox's `blocked_patterns` list (from PRD-028/PRD-034) filters commands matching `*.env`, `*.key`, `~/.ssh/*`, `~/.aws/*`, and shell metacharacters before execution. Raw `subprocess.run(shell=True)` is never used for model-provided commands.

3. **Repo path confinement:** All file tools (`open`, `edit`, `create`, `find_file`, `search_file`) resolve the user-provided path relative to `session.repo_path` using `(session.repo_path / file).resolve()` and then assert that the resolved path is still under `session.repo_path`. Path traversal attempts (`../../etc/passwd`) are blocked with a `PATH TRAVERSAL BLOCKED` error returned to the model.

4. **Patch size limit:** The `submit` tool rejects patches larger than 500 KB (configurable via `swe.max_patch_bytes` in cli-config.yaml). Oversized patches indicate either a runaway edit loop or a model attempting to write large binary content.

5. **Working directory isolation:** Each solve run and each parallel benchmark instance operates in its own `/tmp/swe-<instance_id>-<uuid>/` directory created with `tempfile.mkdtemp()`. Cleanup on exit uses `shutil.rmtree()` inside a `finally` block, preventing accumulation of stale working trees.

6. **Credential stripping from issue context:** The `IssueFetcher` output passes through `security.strip_secrets(text)` (PRD-034) before being included in the initial system prompt. GitHub tokens, API keys, and other credential patterns in issue bodies are replaced with `[REDACTED]` before the model sees them.

7. **Output truncation prevents prompt injection amplification:** All tool outputs are truncated to `MAX_OUTPUT_CHARS = 8192` before being appended to the message history. This limits the attack surface for prompt injection attacks embedded in file contents or grep output, which could otherwise fill the context window and hijack the agent's next action.

8. **SWE-bench dataset integrity:** When loading from HuggingFace, the dataset is loaded with `trust_remote_code=False` (the default). When loading from a local JSONL file, each line is parsed with `json.loads()` and validated against the `SWEBenchInstance` field schema before use. Malformed lines are skipped with a warning, not silently accepted.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`tests/test_swe_harness.py`)

| Test | Coverage |
|------|----------|
| `test_open_tool_window_centering` | Verifies that `open(file, lineno=50)` with `window_size=100` on a 200-line file shows lines 1-100 centered on line 50 |
| `test_open_tool_nonexistent_file` | Verifies `FILE NOT FOUND` error and non-zero exit code |
| `test_edit_tool_line_replacement` | Verifies that `edit(10, 15, new_text)` replaces exactly lines 10-15 and updates file on disk |
| `test_edit_tool_lint_block` | Injects a lint command that always fails; verifies file is restored to pre-edit state and `lint_blocked=True` |
| `test_edit_tool_out_of_range` | Verifies `LINE RANGE ERROR` when `end_line > len(lines)` |
| `test_submit_empty_diff` | Verifies `SUBMIT ERROR — patch is empty` when no edits have been made |
| `test_submit_produces_patch` | Verifies that `submit()` after an edit produces a non-empty unified diff and sets `session.status = "resolved"` |
| `test_path_traversal_blocked` | Calls `tool_open(session, "../../etc/passwd")`; verifies `PATH TRAVERSAL BLOCKED` error |
| `test_budget_max_turns` | Sets `max_turns=3`; verifies loop exits after 3 turns with `status="budget"` |
| `test_budget_max_cost` | Sets `max_cost_usd=0.01`; mocks `_compute_cost` to return 0.005/turn; verifies loop exits after 2 turns |
| `test_session_serialization` | Calls `session.to_dict()` after several tool calls; reconstructs with `from_dict()`; asserts all fields equal |
| `test_lint_cmd_injection_blocked` | Passes `--lint-cmd "rm -rf /"` to the harness constructor; verifies `SystemExit(1)` |
| `test_bash_output_truncation` | Mocks sandbox to return 20000-char output; verifies tool result is exactly `MAX_OUTPUT_CHARS` chars |
| `test_create_existing_file` | Calls `tool_create(session, "existing.py")`; verifies `FILE EXISTS` error |
| `test_scroll_down_clamps` | Calls `scroll_down` when already at last window; verifies `first_line` does not exceed `total_lines - window_size + 1` |

### 11.2 Integration Tests (`tests/test_swe_harness_integration.py`)

| Test | Coverage |
|------|----------|
| `test_end_to_end_toy_bug` | Uses a tiny in-repo Python file with a deliberate bug; runs the full harness loop with a real (or mocked) LLM; verifies `status="resolved"` and the patch applies cleanly via `git apply` |
| `test_sqlite_persistence` | Runs 3 turns; asserts `swe_tool_calls` has 3 rows and `swe_solve_runs.state_json` is valid JSON matching session state |
| `test_resume_after_sigkill` | Runs 5 turns; sends SIGKILL; re-creates session from SQLite; resumes; verifies final patch equals the uninterrupted run (with deterministic mocked LLM) |
| `test_benchmark_runner_parallel` | Runs `BenchmarkRunner` with `--parallel 2` on 4 toy instances; asserts no shared state corruption and 4 rows in `swe_benchmark_results` |
| `test_predictions_jsonl_schema` | Runs benchmark on 2 instances; loads `predictions.jsonl`; validates each line has `instance_id`, `model_patch`, `model_name_or_path` keys |
| `test_sandbox_docker_routing` | With Docker available: verifies bash tool calls go to Docker sandbox, not host subprocess; asserts `sandbox_runs` table has entries |
| `test_otel_span_emission` | Configures in-memory OTLP exporter; runs 3 turns; asserts 3 spans with `swe.tool_name` attribute exported |

### 11.3 Performance Tests

| Test | Target |
|------|--------|
| `test_open_large_file_latency` | `open()` on a 10,000-line file completes in < 50ms on macOS M-series; measured with `time.perf_counter()` |
| `test_parallel_benchmark_throughput` | `--parallel 4` processes 20 toy instances in < 2x the wall time of `--parallel 1` on 8 cores |
| `test_sqlite_write_throughput` | 500 `swe_tool_calls` inserts complete in < 1 second with WAL mode |

### 11.4 SWE-bench Smoke Test

A CI job in `.github/workflows/swe-smoke.yml` runs weekly:

```yaml
- name: SWE-bench smoke
  run: |
    tag benchmark --harness swe \
      --dataset swe-bench-lite \
      --instances 10 \
      --instance-filter "django__django-*" \
      --max-turns 15 \
      --max-cost-usd 0.30 \
      --sandbox docker \
      --output-jsonl /tmp/smoke_predictions.jsonl \
      --json > /tmp/smoke_results.json
    python -c "
    import json, sys
    r = json.load(open('/tmp/smoke_results.json'))
    assert r['resolved'] >= 1, 'smoke test: zero instances resolved'
    print(f\"Smoke: {r['resolved']}/10 resolved\")
    "
```

---

## 12. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | `tag solve --harness swe --issue <github-url>` produces a non-empty patch file at the path printed in the success output | Manual test on a known-good toy issue in a test repo |
| AC-02 | `tag benchmark --harness swe --dataset swe-bench-lite --instances 10 --output-jsonl p.jsonl` writes a file `p.jsonl` with exactly 10 JSON lines, each having `instance_id`, `model_patch`, `model_name_or_path` keys | `python -c "import json; lines=[json.loads(l) for l in open('p.jsonl')]; assert len(lines)==10"` |
| AC-03 | A run with `--max-turns 5` terminates after exactly 5 turns with `status="budget"` in `swe_solve_runs` | `SELECT status, turns FROM swe_solve_runs WHERE id=?` |
| AC-04 | An `edit` call that introduces a Python syntax error is rejected; the file on disk matches its pre-edit content; `swe_tool_calls` has `lint_blocked=1` for that row | Integration test `test_edit_tool_lint_block` passes |
| AC-05 | A `submit` call with no prior edits returns `SUBMIT ERROR — patch is empty` and does not write a patch file | Unit test `test_submit_empty_diff` passes |
| AC-06 | `tool_open(session, "../../etc/passwd")` returns an output containing `PATH TRAVERSAL BLOCKED` and does not read the file | Unit test `test_path_traversal_blocked` passes |
| AC-07 | After a run is killed at turn N and resumed with `tag solve --resume <run-id>`, the `turns` field in the new session starts at N (not 0) | Integration test `test_resume_after_sigkill` passes |
| AC-08 | With `--sandbox docker`, every `bash` tool call appears as a row in `sandbox_runs` table with `backend="docker"` | Integration test `test_sandbox_docker_routing` passes |
| AC-09 | With `--parallel 4`, `tag benchmark` processes 4 instances concurrently with no shared-state corruption (asserted by unique `swe_solve_runs.id` per instance and no `swe_tool_calls` rows with mismatched `run_id`) | Integration test `test_benchmark_runner_parallel` passes |
| AC-10 | `--lint-cmd "rm -rf /"` causes `tag solve` to exit 1 with a message listing safe lint prefixes before any LLM call is made | Unit test `test_lint_cmd_injection_blocked` passes |
| AC-11 | `tag solve --harness swe --dry-run --issue <url>` prints estimated cost and the first 500 chars of the initial context, then exits 0 without writing any SQLite rows | Assert `swe_solve_runs` is empty after dry-run call |
| AC-12 | After a successful 10-instance benchmark run, `tag benchmark --harness swe --json` output includes `resolved`, `budget_exceeded`, `failed`, `no_patch`, `total_cost_usd` keys | JSON schema assertion in CI smoke test |
| AC-13 | OTel spans are emitted for every tool call in a 5-turn test run: exactly 5 spans with attribute `swe.tool_name` present | Integration test `test_otel_span_emission` with in-memory exporter |
| AC-14 | `predictions.jsonl` is written atomically: a SIGKILL during write does not produce a partial file (verified by `json.loads` of every line after recovery) | Integration test using `os.kill(os.getpid(), signal.SIGKILL)` in a subprocess |
| AC-15 | `tag solve list --harness swe --last 5` displays the 5 most recent solve runs with `id`, `issue_url`, `status`, `turns`, `cost_usd` columns | `SELECT id, issue_url, status, turns, cost_usd FROM swe_solve_runs ORDER BY created_at DESC LIMIT 5` produces matching output |

---

## 13. Dependencies

| Dependency | Type | Reason | Already in `pyproject.toml`? |
|-----------|------|--------|------------------------------|
| `datasets` (HuggingFace) | Optional (`pip install tag[swe]`) | Loading SWE-bench dataset from HuggingFace Hub | No — add to `[project.optional-dependencies]` as `swe` extra |
| `swebench` | Optional (`pip install tag[swe]`) | `swebench.evaluation.run_evaluation` for local scoring | No — add to `swe` extra |
| `flake8` or `ruff` | Optional (recommended) | Default lint command for Python files | No — user-installed; fallback is `python -m py_compile` |
| `sandbox.py` (PRD-028) | Internal | Bash tool routing; already ships in `src/tag/sandbox.py` | Yes |
| `tracing.py` (PRD-013) | Internal | OTel span emission | Yes |
| `budget.py` (PRD-019) | Internal | Per-turn cost computation | Yes |
| `diff_context.py` (PRD-038) | Internal | Issue fetching + context injection | Yes |
| `security.py` (PRD-034) | Internal | `strip_secrets()` on issue body | Yes |
| `ci.py` (PRD-020) | Internal | `--auto-pr` PR creation | Yes |
| `gh` CLI | External (optional) | Issue fetching from GitHub; PR creation | Runtime dependency, not Python |
| `git` | External (required) | `git diff`, `git checkout`, branch management | Runtime dependency |
| `docker` | External (optional) | `--sandbox docker` backend | Runtime dependency |

---

## 14. Open Questions

| # | Question | Owner | Deadline |
|---|----------|-------|---------|
| OQ-1 | Should the ACI harness support non-Python lint targets (ESLint, `go build`, `cargo check`) out of the box, or require the user to supply the full lint command? A whitelist approach is safe but reduces out-of-the-box usability for polyglot repos. | @eng-lead | Before Phase 1 completion |
| OQ-2 | SWE-bench-lite resolution rate benchmarks must be run in isolated Docker containers per instance (the official evaluation contract). Do we ship a `swebench_harness` Docker image, or expect users to provide instance images from `princeton-nlp/SWE-bench`? | @devops | Before Phase 2 start |
| OQ-3 | Should `tag solve --harness swe` replace the `--harness` section of PRD-055 (`tag issue-solve`) in v2, or should both code paths be maintained? Maintaining two code paths diverges; replacing requires a PRD-055 migration. | @product | v2 planning |
| OQ-4 | The SWE-agent paper uses a 100-line window size. On modern 200K+ context models this is conservative. Should `--window-size` default to 200 for large-context models, or keep 100 for consistency with the SWE-bench evaluation baseline? | @research | Phase 1 |
| OQ-5 | `--parallel N` for the benchmark runner: should we use `asyncio` (requires async-compatible LLM client) or `ThreadPoolExecutor` (works with any sync client but has GIL constraints)? For I/O-bound LLM calls, both should work; `asyncio` is architecturally cleaner but requires changes to `llm_client`. | @eng | Phase 2 start |
| OQ-6 | Should trajectory logs (full turn-by-turn message history) be stored in SQLite or written to `.jsonl` files in `--output-dir`? SQLite has size limits that may be hit by 500-instance benchmark runs with long trajectories. | @infra | Phase 2 |
| OQ-7 | The SWE-bench evaluation requires Docker to run test suites in the instance container. Should `tag benchmark --harness swe` run `swebench.evaluation.run_evaluation` inline (blocking), or emit the `predictions.jsonl` and let the user run evaluation separately? Inline is more convenient; separate is more portable (no Docker requirement for prediction generation). | @product | Phase 2 |

---

## 15. Complexity and Timeline

### Phase 1 — Core ACI Harness (Days 1-5)

**Goal:** `tag solve --harness swe --patch-file <file>` works end-to-end on a local repo.

| Task | Est. Days | Owner |
|------|-----------|-------|
| Scaffold `swe_harness.py`: `HarnessSession`, `ToolResult`, all dataclasses | 0.5 | eng |
| Implement `open`, `scroll_down`, `scroll_up`, `goto` tools with path confinement | 0.5 | eng |
| Implement `edit` tool with lint guard and rollback | 1.0 | eng |
| Implement `bash` tool with sandbox routing and output truncation | 0.5 | eng |
| Implement `search_file`, `search_dir`, `find_file`, `create`, `submit` tools | 0.5 | eng |
| Implement `run_harness_loop` with all three stopping conditions | 1.0 | eng |
| SQLite schema migration in `open_db()`: all four new tables | 0.5 | eng |
| `cmd_solve` in `controller.py`: argparse, `--patch-file` and `--resume` modes | 0.5 | eng |
| Unit tests (all 15 in §11.1) | 1.0 | eng |
| **Phase 1 total** | **6.0** | |

### Phase 2 — Issue Fetching, Benchmark Runner, SWE-bench Integration (Days 6-10)

**Goal:** `tag solve --harness swe --issue <url>` and `tag benchmark --harness swe --dataset swe-bench-lite` both work.

| Task | Est. Days | Owner |
|------|-----------|-------|
| `swe_dataset.py`: HuggingFace dataset loader + local JSONL; `SWEBenchInstance` | 0.5 | eng |
| GitHub issue fetcher via `gh CLI`; `IssueFetcher` integration with PRD-055 | 0.5 | eng |
| `BenchmarkRunner`: parallel instance execution, `--instance-filter`, progress bar | 1.0 | eng |
| `write_predictions_jsonl()` with atomic write | 0.25 | eng |
| Extend `cmd_benchmark` in `controller.py` for `--harness swe` | 0.5 | eng |
| OTel span emission in `_emit_tool_span()` via `tracing.py` | 0.25 | eng |
| `--auto-pr` integration with `ci.py` | 0.5 | eng |
| Integration tests (all 7 in §11.2) | 1.0 | eng |
| CI smoke test workflow (`.github/workflows/swe-smoke.yml`) | 0.5 | eng |
| **Phase 2 total** | **5.0** | |

### Phase 3 — Hardening, Security Audit, Docs (Days 11-14)

**Goal:** Production-ready: all acceptance criteria pass, security items verified, docs written.

| Task | Est. Days | Owner |
|------|-----------|-------|
| Security review: path confinement, lint injection, credential stripping | 0.5 | security |
| Performance tests (§11.3): latency, throughput, SQLite write speed | 0.5 | eng |
| `tag solve trajectory <run-id>` and `tag solve list` subcommands | 0.5 | eng |
| `pyproject.toml`: add `[swe]` optional extra with `datasets`, `swebench` | 0.25 | eng |
| CLI help text, docstrings, `docs/prd/INDEX.md` update | 0.25 | eng |
| Code review + PR merge | 0.5 | eng-lead |
| **Phase 3 total** | **2.5** | |

**Total estimated effort: 13.5 engineering days (~2.5 weeks)**

The estimate is M (1-2 weeks) for a single focused engineer. With two engineers splitting Phase 1 and Phase 2, the calendar time compresses to ~8 days.

---

*PRD-064 authored for TAG vNext | Cluster B: CI/CD & Agentic Dev Workflows | GitHub Issue #344*
