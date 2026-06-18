# PRD-055: Issue-to-PR Autonomous Loop (`tag issue-solve`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (1-2 weeks)
**Category:** CI/CD & Agentic Dev Workflows
**Affects:** `issue_solver.py + controller.py`
**Depends on:** PRD-021 (agent loop / autonomous mode), PRD-027 (eval framework), PRD-028 (sandbox code execution), PRD-013 (agent tracing / observability), PRD-034 (secret scanning), PRD-038 (diff-aware context injection), PRD-020 (CI/CD integration), PRD-039 (token budget enforcement), PRD-008 (background task queue), PRD-041 (OTel GenAI span cost attribution)
**Inspired by:** Devin, GitHub Copilot Coding Agent, Linear Agent, SWE-agent
**GitHub Issue:** #344

---

## 1. Overview

Software issue resolution is the canonical agentic coding task: given a textual description of a bug or feature request, produce a correct, tested, reviewed pull request that closes it. Today, TAG users must manually context-switch between their issue tracker (GitHub, Linear, Jira), their editor, the test runner, and the GitHub PR workflow. Each handoff carries friction: copy-pasting issue text, running tests, interpreting failures, iterating on code, and finally opening a PR. This PRD specifies `tag issue-solve`, which compresses that entire workflow into a single command and lets TAG drive every step autonomously.

`tag issue-solve` implements the **Issue-to-PR Autonomous Loop**: a bounded, observable, multi-phase agent pipeline that (1) fetches the full issue context from the originating tracker, (2) plans the implementation using a structured reasoning pass, (3) edits code in a sandboxed workspace using the ACI (Agent-Computer Interface) tool harness from SWE-agent, (4) runs the repository's test suite and iterates until FAIL_TO_PASS cases pass and PASS_TO_PASS cases hold, and (5) opens a pull request with a machine-generated summary that links back to the original issue. In `--auto-pr` mode the entire pipeline runs without a single human approval gate; in default mode the agent pauses for confirmation before git push.

The agentic loop is governed by three mandatory stopping conditions drawn from the SWE-bench evaluation contract and general agentic-loop safety research: (a) **success** — tests pass and diff is within budget, (b) **failure** — unrecoverable error or explicit model declaration of infeasibility, (c) **budget** — configurable ceiling on turns, wall-clock seconds, and cumulative USD cost, any one of which terminates the loop with a graceful partial-commit. Missing any of these three conditions leads to runaway agents and unbounded API spend; this PRD makes all three mandatory and non-bypassable. The loop is implemented on top of the existing `loop_agent.py` (PRD-021), extending it with issue-aware planning, ACI tool dispatch, and PR creation steps.

Integration with three issue platforms (GitHub Issues, Linear, Jira) is supported from day one via a thin `IssueFetcher` abstraction. GitHub Issues are fetched via the `gh` CLI (already a TAG dependency); Linear issues via the Linear REST API using an existing PAT; Jira issues via the Jira REST API v3. Each platform authenticates through the TAG config store (`tag config set linear.api_key`, `tag config set jira.token`, etc.) and returns a normalized `IssueContext` dataclass so that all downstream steps are platform-agnostic.

The feature sits squarely in Cluster B (CI/CD & Agentic Dev Workflows) and is the highest-impact feature in that cluster (Impact 5/5). Its closest inspirations are Devin's end-to-end agentic coding workflow, SWE-agent's ACI tool harness, GitHub Copilot Coding Agent's PR-centric output contract, and Linear Agent's issue-native trigger model. TAG's existing infrastructure — `sandbox.py`, `ci.py`, `diff_context.py`, `loop_agent.py`, `budget.py`, `tracing.py` — provides all the lower-level primitives; `issue_solver.py` wires them into a cohesive pipeline.

---

## 2. Problem Statement

### 2.1 Issue resolution today is a context-fragmented, manual workflow

A developer assigned a GitHub or Linear issue must: open the issue in the browser, read and mentally parse the description and comments, open the repository in their editor, locate the relevant files, implement a fix, run tests locally, interpret failures, iterate, commit, push, open a PR, and write a PR description that references the issue. On average this is a 20-40 minute workflow for a trivial bug. TAG already has all the building blocks (LLM agent, sandbox, test runner, CI integration, diff context) to automate every step, but there is no command that chains them together. Users manually paste issue text into `tag run --prompt`, which loses all structured metadata (labels, assignees, linked PRs, related issues) and produces no PR artifact.

### 2.2 Agentic loops without budget guards are unsafe and expensive

Existing `tag loop` (PRD-021) runs a goal-directed agent with an iteration cap (`max_iters`), but `tag loop` is general-purpose and has no domain-specific awareness of tests, diffs, or pull requests. Users who try to use `tag loop --goal "fix issue #42"` get a blank-slate agent with no access to the issue text, no test-runner integration, and no PR output. More critically, `tag loop` does not enforce a cost ceiling in USD — only an iteration count — making it possible for a loop to spend $50 on a problem the model cannot solve by simply burning context tokens on repeated failed attempts. Three independent stopping conditions (success, failure, budget) are not all enforced.

### 2.3 No cross-tracker normalized issue model exists in TAG

TAG's `ci.py` module handles GitHub PRs via `gh` CLI; `integrations/` contains stubs for some providers; but there is no unified abstraction for fetching an issue from GitHub vs Linear vs Jira and normalizing the result. Every user who wants to use TAG with their issue tracker must write glue code. This means TAG cannot be the entry point for issue-driven development across organizations using different trackers.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | A single `tag issue-solve --issue <url-or-id>` command resolves a GitHub, Linear, or Jira issue end-to-end: fetch → plan → code → test → PR. |
| G2 | The agentic loop enforces all three mandatory stopping conditions: success (tests pass), failure (unrecoverable error or model infeasibility signal), budget (max turns, max wall-seconds, max USD cost). |
| G3 | All code editing inside the loop uses the ACI tool harness (windowed file viewer, line-targeted edit with lint validation, structured search) from SWE-agent, not raw bash, to maximize model effectiveness. |
| G4 | Issue context is fetched and normalized from GitHub Issues, Linear, and Jira via a single `IssueFetcher` abstraction; all downstream steps are platform-agnostic. |
| G5 | `--sandbox docker` routes all agent-executed commands through `sandbox.py` (PRD-028) for host filesystem isolation; sandbox is the default when Docker is available. |
| G6 | `--auto-pr` enables fully autonomous mode: no human approval gates between fetch and PR creation; branch push and `gh pr create` execute automatically. |
| G7 | Every issue-solve run is persisted to `issue_solve_runs` and `issue_solve_steps` in SQLite with full reproducibility data; `tag issue-solve status <run-id>` shows live progress. |
| G8 | OpenTelemetry spans are emitted for each phase (fetch, plan, edit_loop iteration, test, pr_create) via `tracing.py` (PRD-013), and cost per phase is attributed via PRD-041. |
| G9 | A `--dry-run` mode fetches the issue and prints the plan without writing any code, running any tests, or creating any PR. |
| G10 | `tag issue-solve --auto` monitors the authenticated user's assigned issues across all configured trackers and starts a solver loop for each newly assigned issue. |

---

## 4. Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Multi-issue batch solving in a single invocation. Each `tag issue-solve` handles exactly one issue. Batch workflows belong to `tag queue` (PRD-008). |
| NG2 | Automated deployment or merge after PR creation. The loop ends at PR creation; merge is a human gate. |
| NG3 | Visual code review UI within TAG TUI. PR review is done on the GitHub/Linear/Jira platform. |
| NG4 | Full Jira workflow automation (state transitions, sprint assignment). TAG reads Jira issue text; it does not write back to Jira except to post a comment with the PR link. |
| NG5 | Supporting issue trackers beyond GitHub Issues, Linear, and Jira in v1. GitLab Issues and Azure DevOps Boards are deferred. |
| NG6 | Building or publishing Docker images. `--sandbox docker` uses a caller-supplied or default `python:3.12-slim` image; TAG does not build images. |
| NG7 | Automatic flaky-test quarantine. Flaky test detection (from cluster research item 5) is a separate feature; here we simply retry once on test failure before declaring failure. |
| NG8 | SWE-bench benchmark harness integration in this PRD. SWE-bench evaluation is a stretch goal in the Testing Strategy section, not a shipped feature. |

---

## 5. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|-------------------|
| End-to-end success rate on internal test issues | ≥ 60% of tagged "good first issue" tickets produce a mergeable PR | Manual review of 20 test issues post-launch |
| Loop budget compliance | 100% of runs terminate within configured cost/turn/time budget | Automated assertion in integration tests |
| Issue fetch latency (all three platforms) | < 3 s p95 for fetching a single issue with full context | Benchmark test with mocked HTTP |
| ACI edit correctness | Lint-on-edit blocks 100% of syntactically invalid edits | Unit test suite for ACIToolHarness |
| PR creation success rate | ≥ 95% of runs that reach the PR phase successfully open a PR | Integration tests with real `gh` CLI in CI |
| Cost per resolved issue | Median < $0.30 for issues requiring < 5 file edits | Tracked via PRD-041 cost attribution spans |
| SQLite persistence | 100% of runs (including aborted runs) have a row in `issue_solve_runs` | Verified in every integration test |
| `--dry-run` produces no side effects | No git commits, no file changes, no API calls beyond issue fetch | Integration test asserting clean `git status` |
| `--auto` mode issue discovery latency | New assignment detected within 60 s of webhook or poll cycle | Integration test with mock webhook payload |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Backend engineer | run `tag issue-solve --issue https://github.com/owner/repo/issues/42 --profile coder --sandbox docker` | TAG fetches the issue, plans a fix, edits code in a Docker sandbox, runs tests, and opens a PR while I work on something else |
| U2 | Linear user | run `tag issue-solve --issue LINEAR-123 --platform linear --profile coder --auto-pr` | The entire cycle from issue to PR happens without any approval prompts, and my Linear ticket gets a PR link comment automatically |
| U3 | Jira shop developer | run `tag issue-solve --issue JIRA-456 --platform jira --dry-run` | I can see TAG's plan for implementing the ticket before committing any code changes |
| U4 | Platform engineer | run `tag issue-solve --auto --profile coder` | TAG monitors my assigned issues across GitHub and Linear and autonomously starts solving each newly assigned one |
| U5 | Tech lead | run `tag issue-solve --issue GH-42 --max-turns 20 --max-cost 0.50` | I can cap the agent's budget to prevent runaway spending on hard problems |
| U6 | DevOps engineer | trigger `tag issue-solve` from a GitHub Actions workflow on `issues.assigned` event | PRs are automatically created for issues assigned to the `@tag-bot` user without any local developer action |
| U7 | Developer | run `tag issue-solve status <run-id>` while the loop is running | I can see which phase the agent is in, how many turns it has taken, and what its current cost is |
| U8 | QA engineer | inspect `~/.tag/runtime/tag.sqlite3` after a run | I can audit the exact sequence of file edits, test results, and model outputs that produced the PR |
| U9 | Developer | see a `FAIL_TO_PASS` test report in the PR description | The PR description tells me which tests were previously failing and now pass, confirming the fix is real |
| U10 | Developer | run `tag issue-solve --issue GH-42 --branch feat/issue-42-fix` | The solver works on a specific branch name rather than an auto-generated one |

---

## 7. Proposed CLI Surface

### 7.1 Primary Command

```
tag issue-solve \
  --issue <url-or-id>          # GitHub URL, LINEAR-NNN, or JIRA-NNN
  [--platform github|linear|jira]   # auto-detected from --issue format if omitted
  [--profile <name>]           # TAG profile to use for the coding agent (default: coder)
  [--sandbox none|docker|e2b|modal]  # execution sandbox (default: docker if available)
  [--auto-pr]                  # fully autonomous mode; no approval gate before push/PR
  [--dry-run]                  # fetch + plan only; no code changes, no tests, no PR
  [--branch <name>]            # branch name (default: tag/issue-{id}-{slug})
  [--base <branch>]            # base branch for PR (default: repo default branch)
  [--max-turns N]              # max agent loop iterations (default: 30)
  [--max-cost USD]             # hard cost ceiling in USD (default: 2.00)
  [--max-seconds N]            # wall-clock timeout in seconds (default: 1800)
  [--max-diff-lines N]         # abort if diff exceeds N lines (default: 2000)
  [--no-tests]                 # skip test execution (use for doc-only issues)
  [--test-cmd <cmd>]           # override test command (default: auto-detected)
  [--yes]                      # skip all confirmation prompts
  [--json]                     # emit structured JSON progress to stdout
  [--run-id <id>]              # resume a previous run (restores iteration state)
  [--worktree]                 # use a git worktree instead of in-place branch switch
```

**Example — GitHub issue with Docker sandbox:**
```bash
$ tag issue-solve \
    --issue https://github.com/myorg/myrepo/issues/42 \
    --profile coder \
    --sandbox docker

TAG Issue Solver  [PRD-055]
────────────────────────────────────────────────────────────
  Issue    : GH#42 — "TypeError: NoneType object has no attribute 'strip'"
  Platform : github
  Profile  : coder
  Sandbox  : docker  (python:3.12-slim)
  Budget   : 30 turns / $2.00 / 1800 s
  Branch   : tag/issue-42-typeerror-nonetype-strip
  Base     : main
────────────────────────────────────────────────────────────

[1/5] Fetching issue context ...                    ✓  0.8 s
[2/5] Planning implementation ...                   ✓  4.2 s  ($0.008)
  Plan:
    • Edit src/myrepo/parser.py:142 — add None guard before .strip() call
    • Edit tests/test_parser.py — add regression test for None input
[3/5] Coding loop (turn 1/30) ...
  [OPEN]  src/myrepo/parser.py
  [EDIT]  142:142  (lint: OK)
  [OPEN]  tests/test_parser.py
  [EDIT]  89:89    (lint: OK)
  Turn 1 cost: $0.012  |  cumulative: $0.020
[4/5] Running tests ...
  $ pytest tests/test_parser.py -x -q
  FAIL_TO_PASS : test_parser_none_input         ✓ NOW PASSES
  PASS_TO_PASS : test_parser_basic              ✓
  PASS_TO_PASS : test_parser_unicode            ✓
  All 23 tests pass.
[5/5] Creating PR ...
  Branch pushed: tag/issue-42-typeerror-nonetype-strip

  ┌─ Approve PR creation? (--auto-pr to skip) [y/N]: y

  PR #87 created: https://github.com/myorg/myrepo/pull/87
  Issue GH#42 linked in PR body.

Run ID  : isr_01HZ9X3K7V
Cost    : $0.031   Turns: 1   Duration: 43 s
```

**Example — Linear issue, fully autonomous:**
```bash
$ tag issue-solve \
    --issue LINEAR-123 \
    --platform linear \
    --profile coder \
    --auto-pr

TAG Issue Solver  [PRD-055]
────────────────────────────────────────────────────────────
  Issue    : LINEAR-123 — "Add pagination to /api/users endpoint"
  Platform : linear
  Profile  : coder
  Sandbox  : docker  (auto-detected)
  Mode     : AUTO-PR  (no approval gates)
────────────────────────────────────────────────────────────

[1/5] Fetching issue ...                            ✓  1.1 s
[2/5] Planning ...                                  ✓  5.8 s  ($0.011)
[3/5] Coding loop ...  turns: 4  cost: $0.062
[4/5] Tests ...  27/27 pass  (FAIL_TO_PASS: 2)
[5/5] PR created: https://github.com/myorg/api/pull/51
      Linear comment posted: LINEAR-123 → PR link

Run ID: isr_01HZ9X4M2N  Cost: $0.073  Duration: 118 s
```

**Example — dry run:**
```bash
$ tag issue-solve --issue JIRA-456 --platform jira --dry-run

[DRY RUN] Fetching JIRA-456 ...
Title   : "Search returns stale results after cache invalidation"
Body    : (152 words)
Labels  : bug, search, cache
Priority: High

Implementation Plan (DRY RUN — no code will be written):
  1. Locate cache invalidation logic in src/search/cache.py
  2. Identify stale-read window in RedisClient.invalidate()
  3. Add synchronous flush before read in SearchService.query()
  4. Add integration test for post-invalidation freshness

No code changes. No tests. No PR. Exiting.
```

### 7.2 Status Subcommand

```
tag issue-solve status <run-id>
tag issue-solve status --last       # most recent run
tag issue-solve list [--limit N]    # list all runs
tag issue-solve cancel <run-id>     # send SIGTERM to running loop
tag issue-solve show <run-id>       # full JSON dump of run + all steps
```

**Status output:**
```
Run isr_01HZ9X3K7V
  Issue    : GH#42
  Phase    : coding_loop  (turn 7 / 30)
  Cost     : $0.091 / $2.00
  Duration : 83 s / 1800 s
  Status   : running
  Branch   : tag/issue-42-typeerror-nonetype-strip
  Last step: EDIT tests/test_parser.py:112:115 (lint: OK)
```

### 7.3 Auto-Monitor Mode

```
tag issue-solve --auto \
  [--profile coder] \
  [--platforms github,linear] \
  [--poll-interval 60] \
  [--max-parallel 3]
```

Runs a persistent monitor daemon that polls configured trackers (or ingests webhook events from PRD-016) for issues newly assigned to the authenticated user, and starts a `tag issue-solve` subprocess for each.

---

## 8. Functional Requirements

| ID | Requirement | Priority | Testable Assertion |
|----|-------------|----------|-------------------|
| FR-01 | `tag issue-solve --issue <github-url>` fetches the issue title, body, labels, and all comments from GitHub via `gh issue view --json` | P0 | Unit test with mocked `gh` output; assert `IssueContext.title` populated |
| FR-02 | `tag issue-solve --issue LINEAR-NNN` fetches the Linear issue via `GET /issues/LINEAR-NNN` using `linear.api_key` from TAG config | P0 | Unit test with mocked Linear API response |
| FR-03 | `tag issue-solve --issue JIRA-NNN` fetches the Jira issue via `GET /rest/api/3/issue/JIRA-NNN` using `jira.token` and `jira.base_url` from TAG config | P0 | Unit test with mocked Jira API response |
| FR-04 | Issue fetch normalizes all platform responses into a single `IssueContext` dataclass with fields: `id`, `title`, `body`, `labels`, `comments`, `platform`, `url`, `assignees` | P0 | Unit test asserting `IssueContext` field parity across all three platforms |
| FR-05 | The planning phase sends `IssueContext` to the configured profile and receives a structured `ImplementationPlan` (list of `PlanStep` with `action`, `file`, `description`) | P0 | Unit test with mocked Hermes response; assert plan is non-empty |
| FR-06 | The coding loop uses the ACI tool harness: `aci_open(file, lineno)`, `aci_scroll_down()`, `aci_scroll_up()`, `aci_goto(lineno)`, `aci_edit(start, end, content)`, `aci_find(pattern)`, `aci_search(pattern, path)` | P0 | Unit tests for each ACI tool; assert `aci_edit` runs linter and rejects invalid Python |
| FR-07 | `aci_edit` MUST run a linter (flake8 for Python, eslint for JS/TS, go vet for Go) after each edit and reject the edit (returning error to the model) if linting fails | P0 | Unit test: inject a syntax error via `aci_edit`; assert edit is blocked and file is unchanged |
| FR-08 | The loop enforces all three stopping conditions: (a) success when test suite passes, (b) failure on `IsfeasibleError` or 3 consecutive identical edits, (c) budget when any of `max_turns`, `max_cost_usd`, `max_wall_seconds` is exceeded | P0 | Integration test for each stopping condition independently |
| FR-09 | When `--sandbox docker` is set, all `aci_edit` applications and test commands are run via `sandbox.py` `run_docker()` with the specified image and no network access by default | P0 | Integration test: assert no host filesystem writes occur during a Docker-sandboxed run |
| FR-10 | The test phase auto-detects the test command by checking for `pytest.ini`, `setup.cfg [tool:pytest]`, `package.json scripts.test`, `Makefile test` target, `go.mod`, in that order | P1 | Unit test with fixture dirs containing each marker; assert correct cmd detected |
| FR-11 | The test phase records FAIL_TO_PASS and PASS_TO_PASS test outcomes by diffing the pre-loop test run baseline against the post-loop test run result | P0 | Integration test: seed a failing test; assert it appears in FAIL_TO_PASS list |
| FR-12 | `--auto-pr` pushes the branch and calls `gh pr create` with a generated PR title and body without any confirmation prompt; default mode pauses for confirmation before push | P0 | Integration test: assert `--auto-pr` calls `gh pr create`; assert default mode prints prompt |
| FR-13 | The PR body MUST include: issue URL, FAIL_TO_PASS test list, PASS_TO_PASS count, run ID, cost, turn count, and the `Closes #N` / `Closes LINEAR-N` closing keyword | P1 | Unit test on `build_pr_body()`; assert all required fields present |
| FR-14 | After PR creation, post a comment on the source issue (GitHub comment via `gh issue comment`, Linear comment via API, Jira comment via API v3) with the PR URL | P1 | Integration test with mocked platform APIs; assert comment posted |
| FR-15 | Every run persists a row in `issue_solve_runs` before any agent calls; every loop iteration persists a row in `issue_solve_steps` with input, output, tool calls, and cost | P0 | Integration test: assert rows exist in both tables for a completed run |
| FR-16 | `tag issue-solve status <run-id>` reads from `issue_solve_runs` and `issue_solve_steps` and displays current phase, turn count, cost, and last step | P1 | Integration test: start run, query status mid-run; assert phase field matches |
| FR-17 | `--dry-run` fetches the issue and produces a printed plan but makes zero git commits, zero file changes, and zero test executions | P0 | Integration test: assert `git status` is clean and no `issue_solve_steps` rows with `phase=coding_loop` exist |
| FR-18 | `--worktree` creates a `git worktree` at `~/.tag/worktrees/<run-id>/` for the solving branch, leaving the main workspace clean | P1 | Integration test: assert worktree directory exists and main workspace `git status` is clean |
| FR-19 | `tag issue-solve --auto` polls each configured tracker every `--poll-interval` seconds (default: 60) for issues newly assigned to the authenticated user, and starts a solver subprocess for each | P2 | Integration test with mocked poll responses; assert subprocess started per new assignment |
| FR-20 | When `--max-diff-lines` is exceeded mid-loop, the loop aborts with exit code 4 and a human-readable explanation; no PR is created | P1 | Integration test: set `--max-diff-lines 5` on a multi-file issue; assert exit code 4 |
| FR-21 | `tag issue-solve cancel <run-id>` sends `SIGTERM` to the solver subprocess and sets `status=cancelled` in `issue_solve_runs` | P1 | Integration test: start a run, cancel it, assert status field |
| FR-22 | Cost estimation is printed before the loop starts, showing estimated turns, cost per turn, and projected total; `--yes` or `CI=true` skips the prompt | P1 | Unit test on `estimate_cost()`; integration test asserting prompt appears without `--yes` |
| FR-23 | OTel spans are emitted for phases: `tag.issue_solve.fetch`, `tag.issue_solve.plan`, `tag.issue_solve.edit_loop` (one span per turn), `tag.issue_solve.test`, `tag.issue_solve.pr_create` | P1 | Unit test: assert span names and attributes match `otel_semconv.py` conventions |
| FR-24 | Secret scanning (PRD-034) is applied to every generated diff before commit; any diff containing a secret pattern is rejected and the agent is instructed to remove it | P0 | Integration test: inject a fake API key in generated code; assert commit is blocked |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Issue fetch (all platforms) | p95 < 3 s with live network |
| NFR-02 | Planning phase | p95 < 10 s (single LLM call) |
| NFR-03 | ACI edit + lint round-trip | p95 < 2 s per edit |
| NFR-04 | SQLite write for each step | < 5 ms (WAL mode); must not block agent loop |
| NFR-05 | Memory footprint of solver process | < 200 MB RSS at steady state |
| NFR-06 | `--json` output | Valid JSON on every line (NDJSON); parseable by `jq` |
| NFR-07 | Loop budget enforcement | Budget checks occur before every turn; overshoot cannot exceed 1 turn cost |
| NFR-08 | Graceful shutdown on SIGTERM | Loop commits current file state (no partial edits), writes final DB row, and exits within 5 s |
| NFR-09 | Idempotent re-run | Running with the same `--issue` twice detects the existing open PR and exits cleanly with a message instead of creating a duplicate |
| NFR-10 | Sandbox isolation | Docker-sandboxed commands have no access to `~/.ssh`, `~/.aws`, `~/.env`, or any path matching `security.py` blocked patterns |
| NFR-11 | Cross-platform | Core logic works on macOS (arm64, x86_64) and Linux (x86_64); Docker backend requires Docker Desktop or Docker Engine |
| NFR-12 | Observability | Every run produces OTel spans exportable to any OTLP backend; span attributes include `issue.id`, `issue.platform`, `loop.turn`, `sandbox.backend` |
| NFR-13 | Config validation | Missing `linear.api_key` / `jira.token` / `jira.base_url` surfaces a specific `ConfigError` with an actionable `tag config set` instruction before any API calls |

---

## 10. Technical Design

### 10.1 New Files

| File | Purpose |
|------|---------|
| `src/tag/issue_solver.py` | Main module: `IssueFetcher`, `ACIToolHarness`, `IssueSolverLoop`, `IssueContext`, `ImplementationPlan`, `LoopBudget`, `cmd_issue_solve` |
| `src/tag/controller.py` | Add `cmd_issue_solve` dispatcher and argparse subcommand registration |

### 10.2 SQLite Schema

```sql
-- WAL mode is already set globally by open_db(); no PRAGMA needed here.

CREATE TABLE IF NOT EXISTS issue_solve_runs (
    id               TEXT PRIMARY KEY,          -- isr_<ulid>
    issue_id         TEXT NOT NULL,             -- "GH#42", "LINEAR-123", "JIRA-456"
    issue_platform   TEXT NOT NULL,             -- "github" | "linear" | "jira"
    issue_url        TEXT,                      -- canonical URL
    issue_title      TEXT NOT NULL DEFAULT '',
    profile          TEXT NOT NULL DEFAULT 'default',
    sandbox          TEXT NOT NULL DEFAULT 'none', -- "none" | "docker" | "e2b" | "modal"
    branch           TEXT,                      -- git branch name
    base_branch      TEXT,
    status           TEXT NOT NULL DEFAULT 'running',
                                                -- running | success | failure | cancelled | budget_exceeded
    phase            TEXT NOT NULL DEFAULT 'fetch',
                                                -- fetch | plan | coding_loop | test | pr_create | done
    turn_count       INTEGER NOT NULL DEFAULT 0,
    max_turns        INTEGER NOT NULL DEFAULT 30,
    cost_usd         REAL NOT NULL DEFAULT 0.0,
    max_cost_usd     REAL NOT NULL DEFAULT 2.0,
    diff_lines       INTEGER NOT NULL DEFAULT 0,
    max_diff_lines   INTEGER NOT NULL DEFAULT 2000,
    pr_url           TEXT,
    pr_number        INTEGER,
    fail_to_pass     TEXT,                      -- JSON array of test names
    pass_to_pass     TEXT,                      -- JSON array of test names
    plan_json        TEXT,                      -- JSON serialization of ImplementationPlan
    error_message    TEXT,
    auto_pr          INTEGER NOT NULL DEFAULT 0, -- bool
    dry_run          INTEGER NOT NULL DEFAULT 0, -- bool
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    completed_at     TEXT
);

CREATE INDEX IF NOT EXISTS idx_isr_status ON issue_solve_runs(status, created_at);
CREATE INDEX IF NOT EXISTS idx_isr_issue  ON issue_solve_runs(issue_id, issue_platform);

CREATE TABLE IF NOT EXISTS issue_solve_steps (
    id               TEXT PRIMARY KEY,          -- step_<ulid>
    run_id           TEXT NOT NULL,
    phase            TEXT NOT NULL,             -- matches issue_solve_runs.phase values
    turn             INTEGER,                   -- null for non-loop phases
    tool_name        TEXT,                      -- "aci_open" | "aci_edit" | "aci_find" | ...
    tool_input       TEXT,                      -- JSON
    tool_output      TEXT,                      -- JSON or plain text
    model_input_tokens  INTEGER NOT NULL DEFAULT 0,
    model_output_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd         REAL NOT NULL DEFAULT 0.0,
    lint_result      TEXT,                      -- "ok" | "error: <msg>"
    created_at       TEXT NOT NULL,
    FOREIGN KEY(run_id) REFERENCES issue_solve_runs(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_iss_run ON issue_solve_steps(run_id, turn);
```

### 10.3 Core Dataclasses

```python
from __future__ import annotations
import dataclasses
from typing import Any

@dataclasses.dataclass
class IssueContext:
    """Normalized issue from any platform."""
    id: str                          # "42", "LINEAR-123", "JIRA-456"
    platform: str                    # "github" | "linear" | "jira"
    url: str
    title: str
    body: str
    labels: list[str]
    comments: list[dict[str, Any]]   # [{author, body, created_at}]
    assignees: list[str]
    repo: str | None                 # "owner/repo" for GitHub; None for Linear/Jira
    extra: dict[str, Any]            # platform-specific raw fields


@dataclasses.dataclass
class PlanStep:
    """One atomic implementation step produced by the planning phase."""
    action: str          # "edit_file" | "create_file" | "delete_file" | "run_command"
    file: str | None     # relative path; None for run_command
    start_line: int | None
    end_line: int | None
    description: str     # human-readable explanation
    command: str | None  # for run_command actions


@dataclasses.dataclass
class ImplementationPlan:
    steps: list[PlanStep]
    summary: str         # one-sentence plan overview
    estimated_turns: int


@dataclasses.dataclass
class LoopBudget:
    max_turns: int = 30
    max_cost_usd: float = 2.0
    max_wall_seconds: int = 1800
    max_diff_lines: int = 2000

    def check(
        self,
        turn: int,
        cost_usd: float,
        wall_seconds: float,
        diff_lines: int,
    ) -> str | None:
        """Return a stop reason string if any budget is exceeded, else None."""
        if turn >= self.max_turns:
            return f"max_turns ({self.max_turns}) reached"
        if cost_usd >= self.max_cost_usd:
            return f"max_cost_usd (${self.max_cost_usd:.2f}) reached"
        if wall_seconds >= self.max_wall_seconds:
            return f"max_wall_seconds ({self.max_wall_seconds}) reached"
        if diff_lines >= self.max_diff_lines:
            return f"max_diff_lines ({self.max_diff_lines}) reached"
        return None


@dataclasses.dataclass
class ACIState:
    """Persistent ACI state across tool calls within a single loop turn."""
    current_file: str | None = None
    first_line: int = 1
    window_size: int = 100
```

### 10.4 IssueFetcher Abstraction

```python
import abc
import subprocess, json, os
from typing import Protocol

class IssueFetcher(abc.ABC):
    @abc.abstractmethod
    def fetch(self, issue_id: str) -> IssueContext: ...

class GitHubIssueFetcher(IssueFetcher):
    """Uses `gh issue view` — requires `gh` CLI authenticated."""
    def fetch(self, issue_id: str) -> IssueContext:
        # issue_id may be a full URL or "owner/repo#N"
        result = subprocess.run(
            ["gh", "issue", "view", issue_id, "--json",
             "number,title,body,labels,comments,assignees,url,repository"],
            capture_output=True, text=True, check=True,
        )
        raw = json.loads(result.stdout)
        return IssueContext(
            id=str(raw["number"]),
            platform="github",
            url=raw["url"],
            title=raw["title"],
            body=raw.get("body") or "",
            labels=[l["name"] for l in raw.get("labels", [])],
            comments=[
                {"author": c["author"]["login"], "body": c["body"],
                 "created_at": c["createdAt"]}
                for c in raw.get("comments", [])
            ],
            assignees=[a["login"] for a in raw.get("assignees", [])],
            repo=raw["repository"]["nameWithOwner"],
            extra=raw,
        )

class LinearIssueFetcher(IssueFetcher):
    """Uses Linear REST API with PAT from `tag config get linear.api_key`."""
    BASE = "https://api.linear.app/graphql"
    def __init__(self, api_key: str):
        self._key = api_key

    def fetch(self, issue_id: str) -> IssueContext:
        import urllib.request
        query = """
        query($id: String!) {
          issue(id: $id) {
            id identifier title description
            labels { nodes { name } }
            comments { nodes { body user { name } createdAt } }
            assignee { name }
            url
          }
        }
        """
        # ... GraphQL call, response normalization ...
        raise NotImplementedError  # full impl in issue_solver.py

class JiraIssueFetcher(IssueFetcher):
    """Uses Jira REST API v3 with Basic auth token."""
    def __init__(self, base_url: str, token: str, email: str):
        self._base = base_url.rstrip("/")
        self._token = token
        self._email = email

    def fetch(self, issue_id: str) -> IssueContext:
        import urllib.request, base64
        url = f"{self._base}/rest/api/3/issue/{issue_id}"
        auth = base64.b64encode(
            f"{self._email}:{self._token}".encode()
        ).decode()
        req = urllib.request.Request(
            url, headers={"Authorization": f"Basic {auth}",
                          "Accept": "application/json"}
        )
        # ... fetch, normalize ADF body to markdown, return IssueContext ...
        raise NotImplementedError  # full impl in issue_solver.py


def make_fetcher(platform: str, cfg: dict) -> IssueFetcher:
    if platform == "github":
        return GitHubIssueFetcher()
    if platform == "linear":
        return LinearIssueFetcher(api_key=cfg["linear.api_key"])
    if platform == "jira":
        return JiraIssueFetcher(
            base_url=cfg["jira.base_url"],
            token=cfg["jira.token"],
            email=cfg["jira.email"],
        )
    raise ValueError(f"Unknown platform: {platform}")
```

### 10.5 ACI Tool Harness

The ACI harness is the key differentiator from raw bash execution. It implements the SWE-agent Agent-Computer Interface pattern: a windowed file viewer, line-targeted editing with lint gating, and structured search. The model never sees raw tool schemas; it calls structured functions whose outputs are token-bounded.

```python
import pathlib, subprocess, textwrap, re
from dataclasses import dataclass, field

LINTERS = {
    ".py":  ["python", "-m", "py_compile"],
    ".js":  ["node", "--check"],
    ".ts":  ["npx", "tsc", "--noEmit", "--allowJs"],
    ".go":  ["go", "vet"],
    ".rb":  ["ruby", "-c"],
    ".sh":  ["bash", "-n"],
}

class ACIToolHarness:
    """Agent-Computer Interface tool harness (SWE-agent pattern)."""

    def __init__(
        self,
        workdir: pathlib.Path,
        state: ACIState,
        sandbox_run_fn=None,  # callable(cmd: list[str]) -> (rc, stdout, stderr)
    ):
        self._workdir = workdir
        self._state = state
        self._run = sandbox_run_fn or self._local_run

    def aci_open(self, filepath: str, lineno: int = 1) -> str:
        """Open file at lineno; return windowed view with line numbers."""
        path = (self._workdir / filepath).resolve()
        self._assert_safe(path)
        lines = path.read_text(errors="replace").splitlines()
        self._state.current_file = filepath
        self._state.first_line = max(1, lineno)
        return self._render_window(lines)

    def aci_scroll_down(self) -> str:
        if not self._state.current_file:
            return "ERROR: no file open"
        self._state.first_line += self._state.window_size
        return self._rerender()

    def aci_scroll_up(self) -> str:
        if not self._state.current_file:
            return "ERROR: no file open"
        self._state.first_line = max(
            1, self._state.first_line - self._state.window_size
        )
        return self._rerender()

    def aci_goto(self, lineno: int) -> str:
        if not self._state.current_file:
            return "ERROR: no file open"
        self._state.first_line = max(1, lineno)
        return self._rerender()

    def aci_edit(
        self, start_line: int, end_line: int, new_content: str
    ) -> str:
        """Replace lines start_line..end_line (1-indexed, inclusive) with new_content.
        Runs linter; BLOCKS edit and returns error message on lint failure.
        """
        if not self._state.current_file:
            return "ERROR: no file open — call aci_open first"
        path = (self._workdir / self._state.current_file).resolve()
        self._assert_safe(path)
        lines = path.read_text(errors="replace").splitlines(keepends=True)
        # Replace target slice
        new_lines = new_content.splitlines(keepends=True)
        if not new_lines[-1].endswith("\n"):
            new_lines[-1] += "\n"
        updated = lines[:start_line - 1] + new_lines + lines[end_line:]
        candidate = "".join(updated)
        # Lint before committing
        suffix = path.suffix.lower()
        if suffix in LINTERS:
            lint_cmd = LINTERS[suffix]
            import tempfile, os
            with tempfile.NamedTemporaryFile(
                suffix=suffix, delete=False, mode="w"
            ) as tf:
                tf.write(candidate)
                tf_name = tf.name
            try:
                rc, out, err = self._run(lint_cmd + [tf_name])
                if rc != 0:
                    return f"LINT ERROR (edit blocked):\n{err or out}"
            finally:
                os.unlink(tf_name)
        path.write_text(candidate)
        return f"OK: lines {start_line}–{end_line} replaced ({len(new_lines)} lines)"

    def aci_find(self, pattern: str, filepath: str | None = None) -> str:
        """grep -n pattern in current file or given file; return up to 50 matches."""
        target = filepath or self._state.current_file
        if not target:
            return "ERROR: no file specified"
        path = (self._workdir / target).resolve()
        self._assert_safe(path)
        rc, out, _ = self._run(["grep", "-n", "--", pattern, str(path)])
        lines = out.strip().splitlines()[:50]
        return "\n".join(lines) or "(no matches)"

    def aci_search(self, pattern: str, directory: str = ".") -> str:
        """rg/grep -rn pattern in directory; return up to 50 matches."""
        dirpath = (self._workdir / directory).resolve()
        self._assert_safe(dirpath)
        cmd = ["grep", "-rn", "--include=*.py", "--include=*.js",
               "--include=*.ts", "--include=*.go", "--", pattern, str(dirpath)]
        rc, out, _ = self._run(cmd)
        lines = out.strip().splitlines()[:50]
        return "\n".join(lines) or "(no matches)"

    def _render_window(self, lines: list[str]) -> str:
        start = self._state.first_line - 1
        end = start + self._state.window_size
        window = lines[start:end]
        header = (
            f"[File: {self._state.current_file}  "
            f"Lines {self._state.first_line}-"
            f"{min(self._state.first_line + len(window) - 1, len(lines))} "
            f"of {len(lines)}]"
        )
        numbered = "\n".join(
            f"{self._state.first_line + i:6d}\t{l.rstrip()}"
            for i, l in enumerate(window)
        )
        return f"{header}\n{numbered}"

    def _rerender(self) -> str:
        path = (self._workdir / self._state.current_file).resolve()
        lines = path.read_text(errors="replace").splitlines()
        return self._render_window(lines)

    def _assert_safe(self, path: pathlib.Path) -> None:
        """Reject paths outside workdir (path traversal guard)."""
        try:
            path.relative_to(self._workdir)
        except ValueError:
            raise PermissionError(
                f"Path {path} is outside workdir {self._workdir}"
            )

    @staticmethod
    def _local_run(cmd: list[str]) -> tuple[int, str, str]:
        r = subprocess.run(cmd, capture_output=True, text=True)
        return r.returncode, r.stdout, r.stderr
```

### 10.6 IssueSolverLoop: Main Pipeline

```python
import time, uuid, json
from pathlib import Path

class IssueSolverLoop:
    """Orchestrates the five-phase issue-to-PR pipeline."""

    PHASES = ["fetch", "plan", "coding_loop", "test", "pr_create", "done"]

    def __init__(
        self,
        run_id: str,
        issue_id: str,
        platform: str,
        profile: str,
        budget: LoopBudget,
        workdir: Path,
        db_conn,
        fetcher: IssueFetcher,
        sandbox_backend: str = "none",
        auto_pr: bool = False,
        dry_run: bool = False,
        branch: str | None = None,
        base_branch: str | None = None,
        test_cmd: str | None = None,
    ):
        self._run_id = run_id
        self._issue_id = issue_id
        self._platform = platform
        self._profile = profile
        self._budget = budget
        self._workdir = workdir
        self._db = db_conn
        self._fetcher = fetcher
        self._sandbox = sandbox_backend
        self._auto_pr = auto_pr
        self._dry_run = dry_run
        self._branch = branch
        self._base_branch = base_branch
        self._test_cmd = test_cmd
        self._start_time = time.monotonic()
        self._cost_usd = 0.0
        self._turn = 0

    def run(self) -> int:
        """Execute all phases. Returns exit code: 0=success, 1=failure,
        2=budget_exceeded, 3=cancelled, 4=diff_lines_exceeded."""
        try:
            issue = self._phase_fetch()
            plan  = self._phase_plan(issue)
            if self._dry_run:
                self._print_plan(plan)
                self._update_run(status="success", phase="done")
                return 0
            baseline_tests = self._run_tests(baseline=True)
            self._phase_coding_loop(issue, plan)
            result_tests   = self._run_tests(baseline=False)
            fail_to_pass, pass_to_pass = self._classify_tests(
                baseline_tests, result_tests
            )
            pr_url = self._phase_pr_create(
                issue, fail_to_pass, pass_to_pass
            )
            self._update_run(
                status="success", phase="done",
                pr_url=pr_url,
                fail_to_pass=json.dumps(fail_to_pass),
                pass_to_pass=json.dumps(pass_to_pass),
            )
            return 0
        except BudgetExceededError as e:
            self._update_run(status="budget_exceeded", error_message=str(e))
            return 2
        except InfeasibleError as e:
            self._update_run(status="failure", error_message=str(e))
            return 1
        except DiffLinesExceededError as e:
            self._update_run(status="failure", error_message=str(e))
            return 4
        except Exception as e:
            self._update_run(status="failure", error_message=repr(e))
            raise

    def _check_budget(self) -> None:
        reason = self._budget.check(
            turn=self._turn,
            cost_usd=self._cost_usd,
            wall_seconds=time.monotonic() - self._start_time,
            diff_lines=self._count_diff_lines(),
        )
        if reason:
            if "diff_lines" in reason:
                raise DiffLinesExceededError(reason)
            raise BudgetExceededError(reason)

    def _count_diff_lines(self) -> int:
        r = subprocess.run(
            ["git", "diff", "--stat", "HEAD"],
            cwd=self._workdir, capture_output=True, text=True,
        )
        # Parse "N insertions(+), M deletions(-)" from stat summary
        import re
        m = re.search(r"(\d+) insertion", r.stdout)
        n = re.search(r"(\d+) deletion", r.stdout)
        return int(m.group(1) if m else 0) + int(n.group(1) if n else 0)

class BudgetExceededError(Exception): pass
class InfeasibleError(Exception): pass
class DiffLinesExceededError(Exception): pass
```

### 10.7 Auto-detect Platform from Issue ID

```python
import re

def detect_platform(issue_ref: str) -> tuple[str, str]:
    """Return (platform, canonical_id) from an issue reference string."""
    # GitHub full URL
    m = re.match(
        r"https://github\.com/[\w.-]+/[\w.-]+/issues/(\d+)", issue_ref
    )
    if m:
        return "github", issue_ref  # pass full URL to gh CLI

    # Linear: LINEAR-NNN or [TEAM]-NNN
    if re.match(r"[A-Z]+-\d+", issue_ref) and not issue_ref.startswith("JIRA"):
        return "linear", issue_ref

    # Jira: PROJECT-NNN (heuristic: platform flag overrides)
    if re.match(r"[A-Z]+-\d+", issue_ref):
        return "jira", issue_ref

    raise ValueError(
        f"Cannot detect platform from '{issue_ref}'. "
        "Use --platform github|linear|jira to specify explicitly."
    )
```

### 10.8 PR Body Builder

```python
def build_pr_body(
    issue: IssueContext,
    run_id: str,
    fail_to_pass: list[str],
    pass_to_pass: list[str],
    cost_usd: float,
    turns: int,
) -> str:
    closing_kw = {
        "github": f"Closes {issue.url}",
        "linear": f"Closes {issue.id}",
        "jira": f"Resolves {issue.id}",
    }.get(issue.platform, f"Ref: {issue.url}")

    ftp_section = (
        "\n".join(f"- `{t}`" for t in fail_to_pass)
        if fail_to_pass else "_none_"
    )

    return f"""## Summary

Automated fix for [{issue.title}]({issue.url}) generated by `tag issue-solve`.

{closing_kw}

## Tests

**FAIL → PASS** ({len(fail_to_pass)} tests now fixed):
{ftp_section}

**PASS → PASS** (regressions guarded): {len(pass_to_pass)} tests

## Run Metadata

| Field | Value |
|-------|-------|
| Run ID | `{run_id}` |
| Cost | ${cost_usd:.4f} |
| Turns | {turns} |
| Platform | {issue.platform} |
| Issue | [{issue.id}]({issue.url}) |

---
*Generated by [TAG](https://github.com/tagcli/tag) `tag issue-solve` (PRD-055)*
"""
```

### 10.9 Integration Points with Existing Modules

| Existing Module | How `issue_solver.py` Uses It |
|-----------------|-------------------------------|
| `sandbox.py` | `run_docker()` / `run_restricted()` as the `sandbox_run_fn` injected into `ACIToolHarness` |
| `loop_agent.py` | `IssueSolverLoop._phase_coding_loop()` invokes the Hermes agent via the same loop infrastructure; `loop_runs` table is written for observability cross-reference |
| `ci.py` | `fetch_pr_metadata()` for idempotency check (existing PR detection); `parse_test_output()` for structured test result parsing |
| `diff_context.py` | Injects changed file contents as structured context turns at the start of each coding loop iteration |
| `budget.py` | `LoopBudget.check()` delegates to the existing `BudgetEnforcer` for cost tracking |
| `tracing.py` | `@trace_span("tag.issue_solve.fetch")` decorators on each phase method |
| `otel_semconv.py` | `ISSUE_ID`, `ISSUE_PLATFORM`, `LOOP_TURN`, `SANDBOX_BACKEND` attribute keys |
| `security.py` | `scan_diff_for_secrets()` called before every `git commit` in the loop |
| `semantic_memory.py` | Issue body + plan stored as semantic memory entries for cross-session recall |
| `notifications.py` | Notifies on PR creation and loop completion/failure |

---

## 11. Security Considerations

1. **Sandbox-first execution.** All agent-generated shell commands execute inside a sandbox (Docker by default when available). The sandbox has `--network none`, mounts only the workdir, and inherits no host environment variables. Secret scanning (FR-24) blocks any diff that would commit a secret.

2. **Path traversal prevention in ACIToolHarness.** `_assert_safe()` calls `path.relative_to(self._workdir)` before any file operation. Paths containing `..` or symlinks pointing outside the workdir are rejected with `PermissionError` before any read or write occurs.

3. **Credentials never passed to sandbox.** `sandbox.py`'s `run_docker()` does not inject `TAG_API_KEY`, `GITHUB_TOKEN`, `LINEAR_API_KEY`, or any secret env var into the container. The agent can run tests but cannot exfiltrate credentials.

4. **Secret scanning before every commit.** `security.py:scan_diff_for_secrets()` is called on the full diff before any `git commit` in the loop. If a secret pattern (private key, bearer token, password assignment) matches, the commit is aborted and the agent is given an error message instructing it to remove the secret.

5. **HMAC verification on webhook triggers.** If `tag issue-solve --auto` is driven by incoming webhooks (PRD-016), each payload is verified with `hmac.compare_digest` using the platform's webhook secret before any processing. Raw body is read before JSON parsing. Constant-time comparison is mandatory.

6. **Issue URL validation.** GitHub issue URLs are validated against `https://github.com/<owner>/<repo>/issues/<N>` before being passed to `gh`. Linear IDs are validated against `[A-Z]+-\d+`. Jira IDs against the same pattern. Malformed inputs are rejected with a `ValueError` before any subprocess or network call.

7. **PR creation requires explicit confirmation or `--auto-pr`.** Without `--auto-pr`, the agent pauses and prints the proposed PR title, body, and branch before calling `gh pr create`. The user must type `y`. This gate cannot be bypassed programmatically without passing `--auto-pr` or `--yes`.

8. **Budget ceiling is non-bypassable.** `LoopBudget.check()` is called at the top of every iteration, including the first. If `max_cost_usd` is set to 0.0 the loop immediately raises `BudgetExceededError`. There is no flag to disable budget enforcement at runtime.

9. **Diff size limit prevents data exfiltration via large patches.** `max_diff_lines` (default 2000) aborts the loop if the diff grows unexpectedly large. This prevents a prompt-injected agent from generating a massive file replacement that obscures malicious content in a sea of whitespace changes.

10. **Platform API keys are stored in TAG config (not env vars) and masked in display.** `tag config get linear.api_key` shows only the last 4 characters. Keys are never echoed in logs, OTel spans, or `issue_solve_steps.tool_input`.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`tests/test_issue_solver.py`)

| Test | What it asserts |
|------|-----------------|
| `test_detect_platform_github_url` | Full GitHub URL maps to `("github", url)` |
| `test_detect_platform_linear` | `LINEAR-123` maps to `("linear", "LINEAR-123")` |
| `test_detect_platform_jira` | `JIRA-456` maps to `("jira", "JIRA-456")` |
| `test_github_fetcher_mocked` | `GitHubIssueFetcher.fetch()` with mocked subprocess; assert `IssueContext` fields |
| `test_linear_fetcher_mocked` | `LinearIssueFetcher.fetch()` with mocked HTTP; assert `IssueContext` fields |
| `test_jira_fetcher_mocked` | `JiraIssueFetcher.fetch()` with mocked HTTP; assert ADF body converted to markdown |
| `test_aci_open_and_scroll` | Open fixture file; scroll down; assert line numbers in output |
| `test_aci_edit_valid` | Edit a valid Python file; assert content changed, no error returned |
| `test_aci_edit_lint_blocks_invalid` | Inject a syntax error via `aci_edit`; assert `"LINT ERROR"` returned and file unchanged |
| `test_aci_edit_path_traversal_blocked` | `aci_edit` with `../../../etc/passwd` path; assert `PermissionError` |
| `test_loop_budget_all_three_conditions` | Parametrize over max_turns=0, max_cost_usd=0, max_wall_seconds=0; assert each raises `BudgetExceededError` |
| `test_build_pr_body_completeness` | `build_pr_body()` output contains closing keyword, FAIL_TO_PASS section, run ID, cost |
| `test_classify_tests_fail_to_pass` | Baseline has 1 failure; result has 0 failures; assert failing test in FAIL_TO_PASS |
| `test_issue_context_normalization` | All three fetchers produce `IssueContext` with identical field names |
| `test_auto_detect_test_cmd` | Fixture dirs with `pytest.ini`, `package.json`, `Makefile`; assert correct cmd detected |
| `test_secret_scan_blocks_commit` | Inject `SECRET_KEY = "sk-..."` in diff; assert commit aborted |
| `test_budget_check_diff_lines` | `LoopBudget(max_diff_lines=5).check(diff_lines=6)`; assert `DiffLinesExceededError` |

### 12.2 Integration Tests (`tests/integration/test_issue_solver_integration.py`)

These tests use a real git repository in a temp directory, a mocked Hermes (no live API calls), and mocked platform APIs.

| Test | Scenario |
|------|----------|
| `test_full_loop_success_github` | Inject a failing pytest; mock Hermes to return a correct edit; assert loop exits 0, PR body written to stdout, `issue_solve_runs.status == "success"` |
| `test_dry_run_no_side_effects` | `--dry-run` with a real issue fixture; assert `git status` clean, no `coding_loop` steps in DB |
| `test_budget_exceeded_max_turns` | Set `max_turns=2`; mock Hermes to return incomplete edits; assert exit code 2, `status=budget_exceeded` |
| `test_sandbox_docker_isolation` | Real Docker run (skip if no Docker); assert host `~/.ssh` not readable inside sandbox |
| `test_auto_pr_no_prompt` | `--auto-pr`; assert `gh pr create` called without `input()` |
| `test_idempotent_rerun_detects_existing_pr` | Create a PR; run again with same `--issue`; assert "PR already exists" message, exit 0, no new PR |
| `test_worktree_flag_creates_worktree` | `--worktree`; assert `~/.tag/worktrees/<run-id>/` exists; assert main workspace `git status` clean |
| `test_cancel_terminates_loop` | Start loop; call `tag issue-solve cancel <run-id>`; assert `status=cancelled` |
| `test_otel_spans_emitted` | Run with in-memory OTel exporter; assert span names for all five phases |
| `test_linear_comment_posted_after_pr` | Mock Linear API; complete a run; assert comment POST called with PR URL |

### 12.3 Performance Tests

| Test | Threshold |
|------|-----------|
| `test_issue_fetch_latency` | 100 runs with mocked HTTP; assert p95 < 50 ms (mocked) |
| `test_aci_edit_latency` | 1000 edits on a 500-line file; assert p95 < 10 ms per edit (no lint) |
| `test_sqlite_step_write_latency` | 1000 sequential step writes; assert p95 < 5 ms |
| `test_diff_line_count_latency` | Repo with 10,000 changed lines; `_count_diff_lines()` < 200 ms |

### 12.4 SWE-bench Compatibility (Stretch Goal)

`issue_solver.py` is designed to be compatible with the SWE-bench evaluation contract. A separate `scripts/swebench_harness.py` can wrap `IssueSolverLoop` to accept a `SWEBenchInstance` dataclass and produce a `predictions.jsonl` entry. This enables offline measurement of TAG's issue resolution capability against the SWE-bench Lite benchmark without shipping the harness as a user-facing feature.

---

## 13. Acceptance Criteria

| ID | Criterion | Tested By |
|----|-----------|-----------|
| AC-01 | `tag issue-solve --issue https://github.com/o/r/issues/42` fetches the issue, prints the plan, runs tests, and outputs a PR URL in under 5 minutes for a one-file fix | Integration test + manual test |
| AC-02 | `tag issue-solve --dry-run` leaves `git status` clean and produces no DB rows with `phase=coding_loop` | Integration test |
| AC-03 | `tag issue-solve --max-cost 0.01 --issue GH-42` exits with code 2 and prints budget-exceeded message before spending more than 1 turn beyond $0.01 | Integration test |
| AC-04 | `aci_edit` with invalid Python (missing colon after `def`) returns `LINT ERROR` and leaves the file unchanged | Unit test |
| AC-05 | `tag issue-solve --sandbox docker` runs tests inside a Docker container; host `~/.ssh/id_rsa` is not accessible inside the container | Integration test (requires Docker) |
| AC-06 | `--auto-pr` creates a PR without any `input()` call and posts a comment on the source issue | Integration test with mocked platform APIs |
| AC-07 | PR body contains `Closes <issue-url>`, FAIL_TO_PASS list, run ID, and cost | Unit test on `build_pr_body()` |
| AC-08 | Injecting a fake secret (`sk-test-abc123`) into a generated edit causes the commit to be blocked and `status=failure` with `error_message` referencing the secret scan | Integration test |
| AC-09 | `tag issue-solve status <run-id>` displays correct phase and turn count while the loop is running | Integration test with concurrent status query |
| AC-10 | `tag issue-solve --issue LINEAR-123 --platform linear` posts a comment on the Linear issue after PR creation | Integration test with mocked Linear API |
| AC-11 | Running `tag issue-solve --issue GH-42` twice (second run after PR already exists) exits cleanly with a message and does not create a duplicate PR | Integration test |
| AC-12 | `tag issue-solve --worktree` leaves the main working tree clean throughout the run | Integration test |
| AC-13 | OTel spans for all five phases (`tag.issue_solve.fetch`, `.plan`, `.edit_loop`, `.test`, `.pr_create`) are emitted with correct `issue.id` and `issue.platform` attributes | Integration test with in-memory OTLP exporter |
| AC-14 | Missing `linear.api_key` when `--platform linear` is used prints `ConfigError: set linear.api_key with: tag config set linear.api_key <key>` and exits 1 without making any API call | Unit test |
| AC-15 | `tag issue-solve --auto` discovers a newly assigned GitHub issue within 60 s (mocked poll) and starts a solver subprocess for it | Integration test |

---

## 14. Dependencies

| Dependency | Type | Source | Notes |
|------------|------|--------|-------|
| `gh` CLI | Runtime (hard) | Homebrew / GitHub releases | Required for GitHub issue fetch and PR creation |
| `sandbox.py` | Internal | PRD-028 | Required for `--sandbox docker/e2b/modal` |
| `loop_agent.py` | Internal | PRD-021 | Coding loop iteration infrastructure |
| `ci.py` | Internal | PRD-020 | PR metadata fetch; idempotency check |
| `diff_context.py` | Internal | PRD-038 | Diff-aware context injection per loop turn |
| `budget.py` | Internal | PRD-012 | Cost tracking and enforcement |
| `tracing.py` | Internal | PRD-013 | OTel span emission |
| `otel_semconv.py` | Internal | PRD-041 | Span attribute naming conventions |
| `security.py` | Internal | PRD-034 | Secret scanning before each commit |
| `semantic_memory.py` | Internal | PRD-025 | Issue + plan storage for cross-session recall |
| `notifications.py` | Internal | PRD-040 | PR creation and loop failure notifications |
| `flake8` / `py_compile` | Runtime (soft) | pip | Python linting in ACI harness; falls back to `py_compile` if flake8 absent |
| `urllib.request` | Stdlib | Python stdlib | Linear and Jira API calls (no extra deps) |
| `docker` CLI | Runtime (soft) | Docker Desktop | Required for `--sandbox docker` backend |

---

## 15. Open Questions

| ID | Question | Owner | Target |
|----|----------|-------|--------|
| OQ-01 | Should the ACI window size (100 lines) be configurable per profile, or fixed? SWE-agent uses 100 as the canonical value; larger windows improve accuracy but increase token cost. | Engineering | Before FR implementation |
| OQ-02 | Should `--auto` mode use polling or webhook ingestion (PRD-016)? Polling is simpler; webhooks have lower latency but require a public endpoint. Both should be supported; webhook takes priority when configured. | Product | Sprint planning |
| OQ-03 | What is the right default test command detection order? The current proposal is: pytest.ini → setup.cfg → pyproject.toml → package.json → Makefile → go.mod. Should we also detect Gradle, Maven, Cargo? | Engineering | Before FR-10 implementation |
| OQ-04 | Should `issue_solve_steps.tool_input` store the full file content for `aci_open` calls, or just the filename + window range? Full content enables perfect replay but increases DB size significantly on large files. | Engineering | Before schema finalization |
| OQ-05 | For `--platform jira`, Jira issue bodies are in Atlassian Document Format (ADF), not Markdown. The current design converts ADF to Markdown with a lightweight converter. Should TAG vendor a full ADF parser or use a regexp-based approximation? | Engineering | Before FR-03 implementation |
| OQ-06 | Should `tag issue-solve --auto` have a `--max-parallel` option (default 3) with a semaphore, or should it delegate to `tag queue` for parallelism management? | Architecture | Sprint planning |
| OQ-07 | Is SWE-bench Lite evaluation (stretch goal in §12.4) in scope for v1 or deferred? If in scope, it requires Docker with network access to pull SWE-bench images, conflicting with `--network none` sandbox default. | Product/Eng | v1 scoping |
| OQ-08 | Should the PR description template be configurable per profile (stored in profile YAML), or fixed? Allowing per-profile templates enables org-specific PR conventions but adds complexity. | Product | v1 scoping |

---

## 16. Complexity and Timeline

**Total estimate: 8-10 working days (M: 1-2 weeks)**

### Phase 1: Foundation (Days 1-3)

- Day 1: `IssueContext` dataclass + `IssueFetcher` abstraction; `GitHubIssueFetcher` implementation using `gh` CLI; `detect_platform()` auto-detection; unit tests for all three.
- Day 2: `LinearIssueFetcher` (GraphQL) + `JiraIssueFetcher` (REST v3 + ADF-to-markdown); config validation with `ConfigError`; unit tests with mocked HTTP.
- Day 3: SQLite schema (`issue_solve_runs` + `issue_solve_steps`); `open_db()` integration; `LoopBudget` dataclass with all three stopping conditions; `tag issue-solve status` and `tag issue-solve list` subcommands.

### Phase 2: ACI Tool Harness (Days 4-5)

- Day 4: `ACIToolHarness` full implementation: `aci_open`, `aci_scroll_down/up`, `aci_goto`, `aci_find`, `aci_search`; `_render_window()`; `_assert_safe()` path traversal guard.
- Day 5: `aci_edit` with lint-on-edit blocking; linter dispatch table for Python/JS/TS/Go/Sh; unit tests for all ACI tools including lint-blocking and path-traversal rejection.

### Phase 3: Loop Pipeline (Days 6-7)

- Day 6: `IssueSolverLoop` five-phase pipeline: `_phase_fetch`, `_phase_plan`, `_phase_coding_loop` (integration with `loop_agent.py` and `diff_context.py`), `_phase_test` (test command auto-detection + FAIL_TO_PASS classification).
- Day 7: `_phase_pr_create` (`build_pr_body`, `gh pr create`, issue comment posting); secret scan integration (`security.py`); budget enforcement at every turn; OTel span emission via `tracing.py`.

### Phase 4: CLI Wiring and Sandbox Integration (Days 8-9)

- Day 8: `cmd_issue_solve` in `controller.py`; argparse subcommand registration; `--dry-run`, `--auto-pr`, `--worktree` flag handling; cost estimation display; confirmation prompt.
- Day 9: `sandbox.py` integration for `--sandbox docker/e2b/modal`; idempotency check (existing PR detection via `ci.py`); `tag issue-solve cancel`; `--auto` monitor mode (poll loop).

### Phase 5: Testing and Hardening (Day 10)

- Day 10: Integration test suite (all AC-01 through AC-15); performance benchmarks; security hardening review (path traversal, secret scan, sandbox isolation); documentation update in `docs/prd/INDEX.md`.

---

*PRD-055 written 2026-06-17. Review against PRD-021 (loop), PRD-028 (sandbox), PRD-034 (security) before implementation kickoff.*

