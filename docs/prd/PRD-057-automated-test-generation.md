# PRD-057: Automated Test Generation on PR/Commit (`tag ci test-gen`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (1-2 weeks)
**Category:** CI/CD & Agentic Dev Workflows
**Affects:** `ci.py`
**Depends on:** PRD-020 (CI/CD integration), PRD-027 (eval framework), PRD-028 (sandbox code execution), PRD-013 (agent tracing/observability), PRD-034 (secret scanning), PRD-038 (diff-aware context injection), PRD-021 (agent loop/autonomous mode), PRD-039 (token budget enforcement)
**Inspired by:** GitHub Copilot test generation, CodiumAI, Diffblue Cover

---

## 1. Overview

Software quality gates enforced by CI are only as strong as the tests that exist. In practice, most pull requests ship with zero new tests — not because engineers do not value coverage, but because authoring tests for changed code is tedious, context-switching from implementation to test authorship is disruptive, and the blast radius of an untested change is invisible at merge time. The result is a systematic accumulation of coverage debt that only surfaces during production incidents.

`tag ci test-gen` is an agentic subcommand that closes this gap by treating test generation as a first-class CI workflow. Given a PR number, a staged commit, or a commit range, it fetches the unified diff, identifies changed Python functions, methods, and classes that lack corresponding test coverage, constructs a bounded agentic loop to write pytest or unittest cases scoped precisely to the changed code, validates those tests against a sandbox-isolated execution environment, and either opens a PR with the generated tests or writes them to a specified output file. The entire pipeline is diff-scoped: code that was not changed in this commit is not analyzed, keeping both LLM context windows and generated test surface focused.

The design draws directly from the SWE-agent Agent-Computer Interface pattern (arXiv:2405.15793): rather than giving the LLM raw shell access, every file interaction passes through bounded, structured tool operations — windowed file viewer, line-targeted reads, lint-on-generate validation — which have been demonstrated to nearly double model effectiveness on code tasks versus raw bash. This same ACI discipline is applied to test generation: the agent sees a diff-scoped view of the changed file, produces a test module, and receives immediate pytest feedback in the same turn, iterating until tests pass or the step budget is exhausted.

Three stopping conditions are enforced on every agentic generation loop — success (all generated tests pass in sandbox), failure (unrecoverable error or syntax rejection), and budget (max_steps, max_cost_usd, max_wall_seconds limits). This follows the agentic loop contract from PRD-021's `AgentLoopConfig` and is essential to prevent runaway generation. The generated test PR is opened automatically and linked back to the source PR via a comment, creating a traceable audit trail from changed code to its tests.

The feature is designed for three adoption modes: developer-local pre-commit enrichment (`--staged`), CI pipeline integration as a GitHub Actions step (`--pr 123 --repo owner/repo`), and post-hoc coverage gap remediation (`--since HEAD~N --min-coverage 80`). All three modes share the same diff ingestion, analysis, generation, and validation pipeline; they differ only in how the diff is sourced and where results are written.

---

## 2. Problem Statement

### 2.1 Pull Requests Routinely Merge Without Tests for Changed Code

Coverage tools like `coverage.py` measure aggregate coverage but do not enforce per-PR coverage of the specific lines changed. A PR that adds a 200-line feature module and also increases an existing well-tested utility by one line shows healthy aggregate coverage while introducing 200 lines of completely untested code. Developers must manually inspect coverage reports, find uncovered changed lines, and write tests — a process that almost never happens under deadline pressure.

`tag ci test-gen` inverts this by treating the diff as the scope of analysis. It identifies the exact functions and code paths introduced or modified in a commit and generates tests specifically for those paths, making it impossible for untested changed code to be invisible at review time.

### 2.2 Test Authorship Is a High-Context, High-Friction Task

Writing good tests requires understanding the function's inputs, expected outputs, edge cases, error conditions, and interaction with its dependencies. This context exists in the implementation and its surrounding code but must be mentally assembled by the author. For non-trivial functions, this assembly takes longer than writing the function itself.

LLMs excel at exactly this assembly task: given a function body and its diff context, they can enumerate realistic input classes, identify boundary conditions, construct mock strategies for external dependencies, and produce parametrized pytest cases that would take a human 20-30 minutes to write in under 10 seconds. `tag ci test-gen` automates this assembly and makes it available at every commit.

### 2.3 Existing CI Test Generation Tools Are Language-Specific or Tightly Coupled to External Platforms

GitHub Copilot's test generation requires VS Code or the Copilot plugin; CodiumAI requires their SaaS platform; Diffblue Cover requires a JVM project structure and a commercial license. None of these integrate into TAG's existing profile-based agent orchestration, SQLite-backed audit trail, or sandbox execution environment. Teams already using TAG for CI diagnosis (`tag ci diagnose`) and PR review (`tag ci review`) have no test generation step that fits the same workflow pattern.

`tag ci test-gen` fills this gap as a native TAG command: same `--profile` flag to control agent behavior, same sandbox isolation for test execution, same SQLite persistence of results, same `--json` output format for piping into other pipeline steps.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | Ingest a diff from a GitHub PR, staged index, or commit range and produce pytest/unittest test cases scoped exclusively to changed Python functions and classes. |
| G2 | Validate all generated tests against a sandbox-isolated `pytest` or `unittest` run before declaring success; never report a test as generated unless it passes in isolation. |
| G3 | Optionally open a GitHub PR containing the generated test file, linked back to the source PR via a comment, creating a two-PR review workflow (code PR + test PR). |
| G4 | Enforce three stopping conditions on every agentic generation loop: success (tests pass), failure (unrecoverable error), and budget (max_steps, max_cost_usd, max_wall_seconds). |
| G5 | Persist every test-gen run (diff source, files analyzed, tests generated, pass/fail status, cost, duration) to the `test_gen_runs` and `test_gen_cases` SQLite tables for auditability and trend analysis. |
| G6 | Support both `pytest` and `unittest` frameworks; default to pytest when available, fall back to unittest. |
| G7 | Enforce a `--min-coverage` threshold: after sandbox execution, parse coverage output and fail with exit code 2 if the generated tests do not cover at least `N%` of the changed lines. |
| G8 | Emit OpenTelemetry spans for each phase (diff ingestion, analysis, generation, validation) via the existing `tracing.py` integration (PRD-013). |
| G9 | Respect the secret-scanning blocklist from PRD-034 security module; never inject credential-matching file content into LLM prompts. |
| G10 | Produce `--json` output for pipeline integration: a machine-readable summary of tests generated, coverage achieved, and PR URL (if opened). |

---

## 4. Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Generating tests for non-Python files (JavaScript, TypeScript, Go, Rust). Python-only in this PRD; multi-language support is a follow-on. |
| NG2 | Achieving 100% branch coverage on generated tests. The system targets the `--min-coverage` threshold (default 70%) over changed lines; exhaustive branch coverage is a manual engineering concern. |
| NG3 | Replacing human-authored test suites. Generated tests supplement the existing suite; they are committed to a separate branch and opened as a distinct PR for human review. |
| NG4 | Integration testing or end-to-end test generation. `tag ci test-gen` generates unit-level tests for individual functions; integration test generation requires system topology context that is out of scope. |
| NG5 | Auto-merging the generated test PR. The test PR is always opened in draft state and requires human review and merge. |
| NG6 | Generating tests for deleted code. Only added or modified lines (diff `+` hunks) are analyzed; deleted code is ignored. |
| NG7 | Supporting GitLab or Bitbucket as diff sources. GitHub via `gh` CLI is the only supported remote in this PRD. |
| NG8 | Real-time streaming of generated test code to the terminal during generation. Generation is buffered and shown after sandbox validation; streaming is a follow-on. |

---

## 5. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Time to first generated test file | < 60 seconds for a PR with <= 5 changed functions | Benchmark timing test |
| Generated test pass rate (sandbox) | >= 90% of generated tests pass on first attempt without re-generation | Integration test suite over 20 sample diffs |
| Coverage of changed lines | >= 70% of changed lines covered by generated tests (default threshold) | `coverage.py --json` parsed from sandbox run |
| Exit code correctness | Exit 0 on success, 1 on generation error, 2 on coverage threshold miss, 3 on budget exhaustion | Unit tests asserting exit code per scenario |
| PR open success | Generated test PR opened in < 5 seconds after validation | Integration test with `gh` CLI mock |
| SQLite persistence | Every run row written within 1 second of completion | Unit test against in-memory SQLite |
| OTel span emission | Every run emits >= 4 spans (ingest, analyze, generate, validate) | Unit test asserting span names |
| Secret filtering | No file matching a blocked pattern is ever read into LLM context | Property test over 50 synthetic diffs with seeded credential files |
| Budget enforcement | Agentic loop always terminates within max_steps * ~5s regardless of model behavior | Fuzz test with mock LLM that never produces passing tests |
| Cost estimate accuracy | Pre-run cost estimate within 30% of actual cost | 10-run benchmark with real API |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag ci test-gen --staged --output tests/test_generated.py` before committing | I can see generated tests for my staged changes and include them in the same commit, without waiting for CI |
| U2 | Tech lead | run `tag ci test-gen --pr 123 --repo owner/repo --profile coder` in a GitHub Actions workflow | CI automatically opens a companion test PR for every code PR that lacks tests, reducing the review burden of catching missing coverage manually |
| U3 | Developer | run `tag ci test-gen --since HEAD~3 --min-coverage 80` on a branch | I know whether the last 3 commits have been covered at 80% before opening a PR, and I see exactly which functions need tests |
| U4 | DevOps engineer | pipe `tag ci test-gen --pr 123 --json` into a status check script | The CI pipeline script can read structured output and post a coverage badge or status comment on the PR without screen-scraping |
| U5 | Developer | see a `--dry-run` preview of which functions will be analyzed and an estimated cost before running | I can confirm the scope and budget before spending money on LLM calls |
| U6 | Platform engineer | observe `test_gen_runs` in SQLite | I can audit which PRs triggered test generation, what coverage was achieved, and how much each run cost |
| U7 | Developer | run `tag ci test-gen --pr 123 --framework unittest` | I can get generated tests in unittest style when the project convention requires it instead of pytest |
| U8 | Developer | receive a meaningful error with suggested fix when the generated tests fail sandbox validation repeatedly | I understand whether to increase `--max-retries`, switch models, or investigate the diff manually |
| U9 | Security engineer | confirm no `.env` or `*.pem` content is ever injected into the LLM prompt | I can rely on the secret-scanning layer and do not need to audit each run manually |
| U10 | Developer | run `tag ci test-gen list` | I can see a history of test generation runs, their status, coverage achieved, and test PR URLs, for observability over time |

---

## 7. Proposed CLI Surface

All `test-gen` subcommands live under the `tag ci` namespace, added to `ci.py` and registered in `controller.py`.

### 7.1 `tag ci test-gen` — Primary Generation Command

```
tag ci test-gen \
  [--pr <number>]              # Fetch diff from GitHub PR
  [--repo <owner/repo>]        # Required with --pr
  [--staged]                   # Diff against index (git diff --cached)
  [--since <ref>]              # Diff since git ref, e.g. HEAD~1 or abc123
  [--profile <name>]           # TAG agent profile to use (default: coder)
  [--framework pytest|unittest]# Test framework (default: pytest if available)
  [--output <path>]            # Write tests to file (default: tests/test_generated.py)
  [--min-coverage <N>]         # Minimum coverage % of changed lines (default: 70)
  [--open-pr]                  # Open a GitHub PR with the generated tests
  [--draft]                    # Open generated test PR as draft (default: true)
  [--base-branch <branch>]     # Base branch for test PR (default: main)
  [--max-retries <N>]          # Max re-generation attempts per function (default: 3)
  [--max-steps <N>]            # Max agentic loop steps (default: 20)
  [--max-cost <USD>]           # Budget cap in USD (default: 2.00)
  [--max-wall-seconds <N>]     # Wall-clock timeout in seconds (default: 300)
  [--sandbox docker|restricted]# Sandbox backend for test execution (default: restricted)
  [--context-lines <N>]        # Lines of diff context per hunk (default: 10)
  [--max-files <N>]            # Max changed files to analyze (default: 20)
  [--dry-run]                  # Preview scope and estimated cost, no LLM calls
  [--yes]                      # Skip cost confirmation prompt
  [--json]                     # Machine-readable JSON output
  [--verbose]                  # Show generation progress and agent turns
```

**Example — PR mode:**
```
$ tag ci test-gen --pr 123 --repo acme/backend --profile coder --open-pr

TAG Test Generation
  Source : PR #123 (acme/backend)
  Profile: coder
  Model  : claude-sonnet-4-6

Fetching diff ...  3 files changed, 142 additions (+), 11 deletions (-)

Analyzing changed code ...
  src/tag/budget.py      : 2 functions (enforce_budget, _fmt_cost)
  src/tag/scheduler.py   : 1 class    (CronEntry), 3 methods
  src/tag/ci.py          : 1 function (fetch_pr_diff) — has existing tests, skipped

Estimated cost: $0.12  Proceed? [Y/n] y

Generating tests [step 1/20] ...
  budget.py::enforce_budget      -> 4 test cases ... PASS (sandbox)
  budget.py::_fmt_cost           -> 3 test cases ... PASS (sandbox)
  scheduler.py::CronEntry        -> 6 test cases ... PASS (sandbox)

Coverage of changed lines: 84% (threshold: 70%) PASS

Writing tests/test_generated_pr123.py (13 cases, 247 lines)
Opening PR: test: generated tests for PR #123 changes ...
  PR URL: https://github.com/acme/backend/pull/456

Comment posted on PR #123 linking to test PR #456.

Run ID: tg-8f3a2c1d
Cost  : $0.09
Wall  : 23s
```

**Example — staged mode:**
```
$ tag ci test-gen --staged --output tests/test_new_feature.py --min-coverage 80

TAG Test Generation
  Source : staged index (git diff --cached)
  Profile: default

Fetching staged diff ... 1 file changed, 38 additions (+)
  src/tag/queue_worker.py: 2 functions (enqueue_job, _validate_payload)

Estimated cost: $0.04  Proceed? [Y/n] y

Generating tests ...
  queue_worker.py::enqueue_job      -> 5 test cases ... PASS
  queue_worker.py::_validate_payload -> 4 test cases ... PASS

Coverage of changed lines: 91% PASS
Writing tests/test_new_feature.py
```

**Example — dry-run:**
```
$ tag ci test-gen --since HEAD~3 --dry-run

DRY RUN (no LLM calls will be made)

Diff source  : HEAD~3..HEAD
Changed files: 4 Python files
Functions    : 7 functions / 2 classes to analyze
Est. tokens  : ~3,200 prompt + ~1,800 completion per function
Est. cost    : $0.18 - $0.25 (depending on retry count)
Est. wall    : 45 - 90 seconds

Would write  : tests/test_generated.py
Would open PR: No (--open-pr not set)
```

**Example — JSON output:**
```json
{
  "run_id": "tg-8f3a2c1d",
  "status": "success",
  "source": {"type": "pr", "number": 123, "repo": "acme/backend"},
  "files_analyzed": 3,
  "functions_analyzed": 5,
  "tests_generated": 13,
  "tests_passing": 13,
  "coverage_pct": 84.2,
  "coverage_threshold": 70,
  "output_path": "tests/test_generated_pr123.py",
  "test_pr_url": "https://github.com/acme/backend/pull/456",
  "cost_usd": 0.09,
  "wall_seconds": 23,
  "model": "claude-sonnet-4-6"
}
```

### 7.2 `tag ci test-gen list` — History

```
tag ci test-gen list [--limit N] [--repo owner/repo] [--json]
```

```
Run ID           Date                 Source          Status    Coverage  Cost    Tests PR
tg-8f3a2c1d      2026-06-17 14:23     PR #123         success   84%       $0.09   #456
tg-7e2b1a0c      2026-06-16 09:11     staged          success   91%       $0.04   —
tg-6d1c0b9a      2026-06-15 16:44     HEAD~1          error     —         $0.01   —
```

### 7.3 `tag ci test-gen show` — Inspect a Run

```
tag ci test-gen show <run-id> [--json]
```

Displays full run record: diff source, functions analyzed, generated test cases (one per line with pass/fail), coverage breakdown, cost, spans.

---

## 8. Functional Requirements

| ID | Requirement | Testable Condition |
|----|-------------|-------------------|
| FR-01 | The `--pr` flag fetches the unified diff via `ci.py::fetch_pr_diff(repo, pr_number)` and raises `RuntimeError` if `gh pr diff` exits non-zero. | Unit test with mocked subprocess returning non-zero. |
| FR-02 | The `--staged` flag runs `git diff --cached --unified=<context-lines>` to enumerate staged changes. | Unit test with mocked git invocation. |
| FR-03 | The `--since <ref>` flag runs `git diff <ref>..HEAD --unified=<context-lines>` to enumerate changes since a ref. | Unit test with `HEAD~1` ref on a temp git repo. |
| FR-04 | Exactly one of `--pr`, `--staged`, or `--since` must be provided; providing none or more than one raises `UsageError` with a human-readable message. | Unit test asserting `SystemExit(1)` for each invalid combination. |
| FR-05 | The diff is parsed into `DiffHunk` objects (one per changed function/method/class) using Python's `ast` module to identify top-level and nested callable definitions in each changed file. | Unit test: synthetic diff with 3 functions produces 3 `DiffHunk` objects. |
| FR-06 | Files matching any pattern in the PRD-034 security blocklist (`*.env`, `*.key`, `*.pem`, `*secret*`, `*credential*`, `*.token`) are silently skipped and listed in the output as `[skipped: blocked pattern]`. | Unit test with a diff including a `config.env` file; assert it does not appear in analyzed functions. |
| FR-07 | Functions that already have a corresponding test in the existing test suite (detected via `pytest --collect-only -q` matching `test_<funcname>` or `<funcname>_test`) are skipped and listed as `[skipped: existing test found]`. | Unit test with a mock pytest collect output. |
| FR-08 | The agentic generation loop calls the configured TAG profile's LLM with a structured prompt containing: the function source (from the diff), its module docstring, parameter types (from annotations), and a pytest/unittest template scaffold. | Integration test asserting the system prompt contains all four sections. |
| FR-09 | The generation loop enforces `max_steps`, `max_cost_usd`, and `max_wall_seconds` stopping conditions; violation of any condition terminates the loop with `status='budget_exhausted'` and exit code 3. | Unit test: mock LLM that never produces passing tests; assert loop terminates before step 21. |
| FR-10 | Generated test code is validated by running `pytest <output_file> -x --tb=short` (or `python -m unittest discover`) in the sandbox environment before being accepted; failing tests trigger a re-generation attempt up to `--max-retries`. | Integration test: generate tests for a known function, assert sandbox run exits 0. |
| FR-11 | Coverage of changed lines is measured by running `coverage run -m pytest <output_file>` followed by `coverage json` in sandbox; the `coverage_pct` is the percentage of `+` lines from the diff covered by at least one test. | Integration test: synthetic function with 10 added lines; assert `coverage_pct` == 100 when all lines are hit. |
| FR-12 | If `coverage_pct < --min-coverage` after all retries, the command exits with code 2 and prints which specific lines were not covered. | Unit test asserting exit code 2 and uncovered line list in output. |
| FR-13 | When `--open-pr` is set, the command creates a new branch named `tag/test-gen/pr-<number>` (or `tag/test-gen/<run-id>` for non-PR sources), commits the generated test file, pushes the branch, and calls `gh pr create` with the generated PR body. | Integration test with mocked `gh` CLI. |
| FR-14 | When `--open-pr` is set and `--pr` was provided, the command posts a comment on the source PR via `ci.py::post_pr_comment` linking to the generated test PR. | Unit test asserting `post_pr_comment` is called with correct PR numbers. |
| FR-15 | Every run is persisted to `test_gen_runs` in SQLite within 1 second of completion, including `status`, `cost_usd`, `coverage_pct`, `tests_generated`, `tests_passing`, and `test_pr_url`. | Unit test against in-memory SQLite; assert row present after run. |
| FR-16 | Every analyzed function is persisted to `test_gen_cases` with its individual pass/fail status, retry count, and generated test code. | Unit test asserting one row per function in `test_gen_cases`. |
| FR-17 | `--dry-run` mode prints scope (file count, function count), estimated token count, and estimated cost without making any LLM API calls; exits 0. | Unit test asserting no HTTP calls and exit 0. |
| FR-18 | `--max-files` limits the number of changed files analyzed; files beyond the cap are listed as `[skipped: max-files limit]`. | Unit test: diff with 25 files and `--max-files 20`; assert 5 files in skipped list. |
| FR-19 | `--json` outputs a single JSON object (not pretty-printed) to stdout with all fields defined in Section 7.1; human-readable output goes to stderr when `--json` is active. | Unit test parsing stdout as JSON after a successful run. |
| FR-20 | OTel spans are emitted for phases: `test_gen.ingest`, `test_gen.analyze`, `test_gen.generate`, `test_gen.validate`, `test_gen.open_pr` via `tracing.py::open_span`. | Unit test asserting span names in the trace. |
| FR-21 | `tag ci test-gen list` reads from `test_gen_runs` and renders a table with columns: Run ID, Date, Source, Status, Coverage, Cost, Tests PR. | Unit test: insert 3 rows, assert 3 rows in tabular output. |
| FR-22 | `tag ci test-gen show <run-id>` renders the full run record including per-function case results from `test_gen_cases`. | Unit test: insert run + 2 cases, assert both cases in output. |
| FR-23 | When `pytest` is not installed in the current environment, the command emits a warning and falls back to `unittest` regardless of `--framework pytest`. | Unit test with mocked `shutil.which('pytest')` returning `None`. |
| FR-24 | Generated test files include a module-level comment `# Generated by tag ci test-gen (run_id: <id>, date: <iso>)` as the first line. | Unit test asserting comment presence in generated file. |

---

## 9. Non-Functional Requirements

| ID | Requirement | Measure |
|----|-------------|---------|
| NFR-01 | The command must produce a first test within 60 seconds for a single-function diff on a warmed model. | Benchmark timing test over 10 runs. |
| NFR-02 | The SQLite write must use WAL mode and `busy_timeout = 5000` to handle concurrent access from CI parallel jobs. | Load test: 4 concurrent `test-gen` runs; assert no `SQLITE_BUSY` errors. |
| NFR-03 | LLM context sent per function must not exceed 8,000 tokens; the prompt builder truncates the function body and surrounding context if needed, logging a warning. | Unit test: 1,000-line function; assert prompt token count <= 8,000. |
| NFR-04 | The command must not import the LLM client library at module load time; all LLM imports must be deferred to inside the `cmd_test_gen` function body. | Assert `import tag.ci` does not add `anthropic` to `sys.modules`. |
| NFR-05 | Sandbox test execution must time out after 30 seconds per test file; runaway test loops or infinite waits must be killed. | Unit test: test file with `while True: pass`; assert sandbox exits within 35 seconds. |
| NFR-06 | The generated test PR body must include: summary of functions tested, coverage percentage, and a checklist of items for the human reviewer to verify. | Unit test asserting PR body contains the three sections. |
| NFR-07 | The cost estimate shown in `--dry-run` or pre-run confirmation must be within ±50% of the actual cost for a 10-run benchmark. | Benchmark 10 runs; assert `|estimate - actual| / actual < 0.5` for all. |
| NFR-08 | The command must work correctly when the working directory is not a git repository (e.g., `--pr` mode only); it must raise a clear error if git is required but unavailable. | Unit test in a temp directory without `.git`. |
| NFR-09 | Generated tests must be syntactically valid Python as verified by `ast.parse()` before the sandbox run; syntax errors must trigger immediate re-generation without a sandbox invocation. | Unit test: inject a syntax-error into the mock LLM output; assert sandbox not called on first attempt. |
| NFR-10 | All subprocess invocations to `gh`, `git`, `pytest`, and `coverage` must use `capture_output=True` and never inherit the parent process's terminal stdin; this prevents interactive prompt hangs in CI. | Code review assertion: grep for `subprocess.run` without `capture_output=True`. |

---

## 10. Technical Design

### 10.1 New Files

| File | Purpose |
|------|---------|
| `src/tag/ci.py` | Extended with `cmd_test_gen`, `TestGenRunner`, `DiffHunk`, `TestGenRun`, `TestGenCase`, `ensure_test_gen_schema`, `analyze_diff_for_functions`, `build_generation_prompt`, `validate_generated_tests`, `open_test_pr`. |
| `tests/test_ci_test_gen.py` | Unit and integration tests for the new test-gen functionality. |

No new top-level modules are created; all logic lives in `ci.py` consistent with the existing pattern.

### 10.2 SQLite DDL

```sql
-- Migration: add_test_gen_tables
-- Applied via open_db() + ensure_test_gen_schema()

CREATE TABLE IF NOT EXISTS test_gen_runs (
    id              TEXT PRIMARY KEY,             -- UUID prefixed 'tg-'
    diff_source     TEXT NOT NULL,                -- 'pr', 'staged', 'since'
    diff_ref        TEXT,                         -- PR number, git ref, or NULL for staged
    repo            TEXT,                         -- owner/repo or NULL
    profile         TEXT NOT NULL DEFAULT 'default',
    model_id        TEXT,
    framework       TEXT NOT NULL DEFAULT 'pytest',
    output_path     TEXT,
    status          TEXT NOT NULL DEFAULT 'running',
                                                  -- running|success|error|budget_exhausted|coverage_miss
    files_analyzed  INTEGER NOT NULL DEFAULT 0,
    fns_analyzed    INTEGER NOT NULL DEFAULT 0,
    tests_generated INTEGER NOT NULL DEFAULT 0,
    tests_passing   INTEGER NOT NULL DEFAULT 0,
    coverage_pct    REAL,
    coverage_threshold REAL NOT NULL DEFAULT 70.0,
    test_pr_url     TEXT,
    test_pr_branch  TEXT,
    source_pr_number INTEGER,
    cost_usd        REAL NOT NULL DEFAULT 0.0,
    prompt_tokens   INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    wall_seconds    REAL,
    error_msg       TEXT,
    created_at      TEXT NOT NULL,
    completed_at    TEXT
);

CREATE INDEX IF NOT EXISTS idx_tgr_status_created
    ON test_gen_runs(status, created_at);
CREATE INDEX IF NOT EXISTS idx_tgr_repo_created
    ON test_gen_runs(repo, created_at);

CREATE TABLE IF NOT EXISTS test_gen_cases (
    id              TEXT PRIMARY KEY,             -- UUID
    run_id          TEXT NOT NULL REFERENCES test_gen_runs(id),
    file_path       TEXT NOT NULL,
    function_name   TEXT NOT NULL,
    function_lineno INTEGER,
    tests_generated INTEGER NOT NULL DEFAULT 0,
    tests_passing   INTEGER NOT NULL DEFAULT 0,
    retry_count     INTEGER NOT NULL DEFAULT 0,
    status          TEXT NOT NULL DEFAULT 'pending',
                                                  -- pending|pass|fail|skipped|budget
    skip_reason     TEXT,                         -- 'existing_test'|'blocked_pattern'|'max_files'
    generated_code  TEXT,                         -- Full generated test source
    coverage_pct    REAL,
    sandbox_output  TEXT,                         -- Last pytest stdout/stderr
    cost_usd        REAL NOT NULL DEFAULT 0.0,
    created_at      TEXT NOT NULL,
    completed_at    TEXT
);

CREATE INDEX IF NOT EXISTS idx_tgc_run_id
    ON test_gen_cases(run_id, status);
CREATE INDEX IF NOT EXISTS idx_tgc_file_fn
    ON test_gen_cases(file_path, function_name);
```

### 10.3 Core Dataclasses

```python
from __future__ import annotations

import ast
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Literal


DiffSourceType = Literal["pr", "staged", "since"]
FrameworkType = Literal["pytest", "unittest"]
RunStatus = Literal[
    "running", "success", "error", "budget_exhausted", "coverage_miss"
]
CaseStatus = Literal["pending", "pass", "fail", "skipped", "budget"]


@dataclass
class DiffHunk:
    """A single changed callable identified from the unified diff."""
    file_path: str                  # Relative path, e.g. "src/tag/budget.py"
    function_name: str              # Qualified name, e.g. "EnforceBudget.check"
    lineno_start: int               # First line of the function def in the file
    lineno_end: int                 # Last line of the function def in the file
    source_code: str                # Full source of the function (not just diff lines)
    added_lines: list[int]          # Line numbers of `+` lines within function
    docstring: str | None           # Extracted docstring if present
    annotations: dict[str, str]     # {param: type_str} from ast.parse annotations
    is_method: bool = False         # True if inside a class body
    class_name: str | None = None   # Parent class name if is_method


@dataclass
class AgentLoopConfig:
    """Stopping conditions for the test generation agentic loop."""
    max_steps: int = 20
    max_cost_usd: float = 2.00
    max_wall_seconds: float = 300.0
    max_retries_per_fn: int = 3
    blocked_commands: list[str] = field(
        default_factory=lambda: ["rm", "curl", "wget", "ssh", "nc"]
    )


@dataclass
class TestGenRun:
    """In-memory representation of a test_gen_runs row."""
    id: str = field(default_factory=lambda: "tg-" + str(uuid.uuid4())[:8])
    diff_source: DiffSourceType = "staged"
    diff_ref: str | None = None
    repo: str | None = None
    profile: str = "default"
    model_id: str | None = None
    framework: FrameworkType = "pytest"
    output_path: str | None = None
    status: RunStatus = "running"
    files_analyzed: int = 0
    fns_analyzed: int = 0
    tests_generated: int = 0
    tests_passing: int = 0
    coverage_pct: float | None = None
    coverage_threshold: float = 70.0
    test_pr_url: str | None = None
    test_pr_branch: str | None = None
    source_pr_number: int | None = None
    cost_usd: float = 0.0
    prompt_tokens: int = 0
    completion_tokens: int = 0
    wall_seconds: float | None = None
    error_msg: str | None = None
    created_at: str = ""
    completed_at: str | None = None


@dataclass
class TestGenCase:
    """In-memory representation of a test_gen_cases row."""
    id: str = field(default_factory=lambda: str(uuid.uuid4()))
    run_id: str = ""
    file_path: str = ""
    function_name: str = ""
    function_lineno: int | None = None
    tests_generated: int = 0
    tests_passing: int = 0
    retry_count: int = 0
    status: CaseStatus = "pending"
    skip_reason: str | None = None
    generated_code: str | None = None
    coverage_pct: float | None = None
    sandbox_output: str | None = None
    cost_usd: float = 0.0
    created_at: str = ""
    completed_at: str | None = None


@dataclass
class SandboxResult:
    """Result of running pytest/unittest inside the sandbox."""
    exit_code: int
    stdout: str
    stderr: str
    tests_collected: int = 0
    tests_passed: int = 0
    tests_failed: int = 0
    coverage_pct: float | None = None
    timed_out: bool = False
```

### 10.4 Core Algorithm: `analyze_diff_for_functions`

```python
def analyze_diff_for_functions(
    diff_text: str,
    *,
    context_lines: int = 10,
    max_files: int = 20,
    blocked_patterns: list[str] | None = None,
) -> tuple[list[DiffHunk], list[str]]:
    """Parse a unified diff and return DiffHunk objects for changed callables.

    Returns (hunks, skipped_files). skipped_files lists files that were
    blocked by pattern matching or exceeded the max_files cap.

    Algorithm:
    1. Split diff into per-file sections using `--- a/` header lines.
    2. For each changed .py file (up to max_files):
       a. Check filename against blocked_patterns; skip if matched.
       b. Read the full file from disk using ast.parse().
       c. Walk the AST to find FunctionDef / AsyncFunctionDef nodes.
       d. For each callable, check if any added line ('+' prefix in diff)
          falls within [node.lineno, node.end_lineno].
       e. If yes: extract source via inspect.getsource-equivalent, collect
          all added lines, extract docstring and annotations.
       f. Yield a DiffHunk for that callable.
    3. Return (hunks, skipped).
    """
    ...
```

### 10.5 Core Algorithm: `build_generation_prompt`

The prompt uses a structured template split into two roles to maximize cache efficiency:

```python
SYSTEM_PROMPT_TEMPLATE = """\
You are an expert Python test engineer. You write complete, executable pytest test cases.

Rules:
- Use pytest fixtures and parametrize where appropriate.
- Mock all external I/O (network, filesystem, subprocess) using pytest-mock or unittest.mock.
- Test the happy path, at least two edge cases, and at least one error case.
- Do not import the function under test until inside the test body to avoid circular imports.
- Return ONLY the Python test code — no prose, no markdown fences, no explanation.
- The first line of your output must be: # Generated by tag ci test-gen
"""

USER_PROMPT_TEMPLATE = """\
Generate pytest tests for this Python function.

File: {file_path}
Function: {function_name}
Line: {lineno_start}

Source code:
```python
{source_code}
```

Type annotations: {annotations}
Docstring: {docstring}

Changed lines in this commit: {added_lines}
Framework: {framework}

Scaffold to complete (return the full file, not just the test body):
```python
# Generated by tag ci test-gen
import pytest
from {module_import_path} import {function_name}

# Your tests here
```
"""
```

### 10.6 Core Algorithm: `validate_generated_tests`

```python
def validate_generated_tests(
    generated_code: str,
    output_path: Path,
    *,
    framework: FrameworkType,
    sandbox_backend: str,
    timeout_seconds: int = 30,
) -> SandboxResult:
    """
    1. ast.parse(generated_code) — reject immediately if SyntaxError.
    2. Write generated_code to a temp file at output_path.
    3. Run via sandbox.run_command():
       - pytest: ["python", "-m", "pytest", str(output_path), "-x",
                  "--tb=short", "--json-report", "--json-report-file=/tmp/report.json",
                  f"--cov={source_module}", "--cov-report=json:/tmp/cov.json"]
       - unittest: ["python", "-m", "pytest", "--collect-only"] then
                   ["python", "-m", "unittest", "discover", ...]
    4. Parse /tmp/report.json for test counts.
    5. Parse /tmp/cov.json for coverage of changed lines.
    6. Return SandboxResult.
    """
    ...
```

### 10.7 Agentic Loop with Three Stopping Conditions

```python
def _generation_loop(
    hunk: DiffHunk,
    case: TestGenCase,
    run: TestGenRun,
    config: AgentLoopConfig,
    *,
    profile: str,
    framework: FrameworkType,
    output_path: Path,
    sandbox_backend: str,
    cost_tracker: CostTracker,
    tracer: Tracer,
) -> TestGenCase:
    """Agentic generation loop for a single DiffHunk.

    Stopping conditions (FR-09):
      - SUCCESS: sandbox_result.tests_passed == sandbox_result.tests_collected > 0
      - FAILURE: retry_count >= config.max_retries_per_fn
      - BUDGET:  cost_tracker.total_usd >= config.max_cost_usd
                 OR time.monotonic() - start >= config.max_wall_seconds
                 OR step >= config.max_steps
    """
    start = time.monotonic()
    for step in range(config.max_steps):
        # Budget guard
        if cost_tracker.total_usd >= config.max_cost_usd:
            case.status = "budget"
            return case
        if time.monotonic() - start >= config.max_wall_seconds:
            case.status = "budget"
            return case

        # Generate
        prompt = build_generation_prompt(hunk, framework, case.retry_count)
        response, usage = call_llm(profile, prompt)
        cost_tracker.add(usage)
        case.cost_usd += usage.cost_usd
        run.prompt_tokens += usage.prompt_tokens
        run.completion_tokens += usage.completion_tokens

        # Validate syntax (NFR-09)
        try:
            ast.parse(response)
        except SyntaxError as exc:
            case.sandbox_output = f"SyntaxError: {exc}"
            case.retry_count += 1
            if case.retry_count >= config.max_retries_per_fn:
                case.status = "fail"
                return case
            continue  # re-generate without sandbox invocation

        # Validate in sandbox (FR-10)
        result = validate_generated_tests(
            response, output_path,
            framework=framework,
            sandbox_backend=sandbox_backend,
        )
        case.generated_code = response
        case.sandbox_output = result.stdout + result.stderr
        case.coverage_pct = result.coverage_pct

        if result.exit_code == 0 and result.tests_passed > 0:
            case.status = "pass"
            case.tests_generated = result.tests_collected
            case.tests_passing = result.tests_passed
            return case

        # Tests failed — retry
        case.retry_count += 1
        if case.retry_count >= config.max_retries_per_fn:
            case.status = "fail"
            return case

    case.status = "budget"
    return case
```

### 10.8 PR Opening Flow

```python
def open_test_pr(
    run: TestGenRun,
    output_path: Path,
    *,
    repo: str,
    base_branch: str = "main",
    draft: bool = True,
) -> str:
    """Commit generated tests to a branch and open a GitHub PR.

    Branch name convention: tag/test-gen/pr-<source_pr> or tag/test-gen/<run_id>
    Returns the PR URL.
    """
    branch = (
        f"tag/test-gen/pr-{run.source_pr_number}"
        if run.source_pr_number
        else f"tag/test-gen/{run.id}"
    )
    subprocess.run(["git", "checkout", "-b", branch], check=True, capture_output=True)
    subprocess.run(["git", "add", str(output_path)], check=True, capture_output=True)
    subprocess.run(
        ["git", "commit", "-m",
         f"test: generated tests for {'PR #' + str(run.source_pr_number) if run.source_pr_number else run.id} [tag ci test-gen]"],
        check=True, capture_output=True,
    )
    subprocess.run(["git", "push", "-u", "origin", branch], check=True, capture_output=True)

    pr_body = _build_pr_body(run)
    result = subprocess.run(
        ["gh", "pr", "create",
         "--repo", repo,
         "--base", base_branch,
         "--head", branch,
         "--title", f"test: generated tests for {'PR #' + str(run.source_pr_number) if run.source_pr_number else run.id}",
         "--body", pr_body,
         *(["--draft"] if draft else [])],
        capture_output=True, text=True, check=True,
    )
    return pr_body, result.stdout.strip()  # stdout is the PR URL
```

### 10.9 Integration Points

| Module | Integration |
|--------|-------------|
| `ci.py::fetch_pr_diff` | Reused directly for `--pr` diff ingestion (FR-01). |
| `ci.py::post_pr_comment` | Called to link test PR back to source PR (FR-14). |
| `diff_context.py::get_changed_files` | Reused for `--staged` and `--since` file enumeration. |
| `sandbox.py::run_command` | Wraps pytest/coverage execution (FR-10, NFR-05). |
| `security.py::blocked_patterns` | Applied to filter files before content is read (FR-06, G9). |
| `tracing.py::open_span` / `Tracer` | Spans emitted per phase (FR-20, G8). |
| `budget.py::CostTracker` | Accumulates cost across all LLM calls in the loop (FR-09). |
| `loop_agent.py::AgentLoopConfig` | Pattern borrowed; `AgentLoopConfig` dataclass defined locally in `ci.py` with same field names for consistency. |
| `open_db()` | Database connection for `ensure_test_gen_schema`, run/case persistence (FR-15, FR-16). |

---

## 11. Security Considerations

1. **Secret scanning on diff content**: Before any file content is read into the LLM prompt, every filename in the diff is checked against the PRD-034 blocklist patterns (`*.env`, `*.key`, `*.pem`, `*.token`, `*secret*`, `*credential*`, `~/.ssh/*`, `~/.aws/*`). Matching files are skipped entirely; their content is never read from disk. This is enforced at the `analyze_diff_for_functions` level before any I/O occurs.

2. **Sandbox isolation for test execution**: Generated test code executes inside the sandbox environment (PRD-028). The default backend is `restricted` (subprocess with resource limits and blocked command patterns); `docker` is recommended for CI use. Generated tests cannot call `rm`, `curl`, `wget`, `ssh`, `nc`, or other destructive commands because the sandbox `blocked_commands` list rejects them before execution.

3. **No credential injection into generated tests**: The prompt builder never injects environment variable values, connection strings, or API keys into the generation prompt. Type annotations are included as strings; default values containing credential patterns are redacted to `<REDACTED>`.

4. **Branch push protection**: The `open_test_pr` function always pushes to a new branch (`tag/test-gen/...`) and never force-pushes. It explicitly checks that the branch does not already exist before creating it; if it exists, it appends a numeric suffix.

5. **Generated test code review gate**: The test PR is always opened as `--draft` by default, requiring a human reviewer to un-draft and merge. The `--draft` flag cannot be disabled without explicitly passing `--no-draft`, ensuring no generated code is auto-merged.

6. **LLM prompt injection guard**: The function source code injected into the LLM prompt is wrapped in a markdown code fence and truncated at 6,000 tokens. Any attempt by malicious code comments to escape the fence or inject additional instructions is mitigated by the structured template format and token cap.

7. **Subprocess command construction uses list form**: All `subprocess.run` calls use list arguments (not shell strings) to prevent shell injection via crafted file paths or PR titles (NFR-10).

8. **SQLite WAL mode**: The database uses WAL mode to prevent write contention from parallel CI jobs corrupting the database; `busy_timeout = 5000` ensures a graceful retry on lock contention rather than an immediate crash.

9. **Cost cap enforcement**: The `max_cost_usd` budget guard in the agentic loop (FR-09) prevents runaway LLM spending due to adversarially crafted diffs that produce functions requiring many retries.

10. **Diff size limit**: Diffs larger than 500 KB are rejected with an error before any parsing occurs, preventing memory exhaustion from maliciously large patch files piped into the command.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`tests/test_ci_test_gen.py`)

- `test_analyze_diff_single_function`: Provide a synthetic diff adding one function; assert one `DiffHunk` returned with correct `function_name` and `added_lines`.
- `test_analyze_diff_blocked_pattern`: Include a `secrets.env` file in the diff; assert it appears in `skipped_files` and no hunk for it.
- `test_analyze_diff_max_files_cap`: Provide 25 changed files with `max_files=20`; assert exactly 5 in `skipped_files`.
- `test_generation_loop_budget_steps`: Mock LLM returning broken code on every call; assert loop exits after `max_steps` with `status='budget'`.
- `test_generation_loop_budget_cost`: Mock LLM with `cost_usd=1.50` per call and `max_cost_usd=2.00`; assert loop exits after 2nd call.
- `test_generation_loop_success`: Mock LLM returning valid passing pytest code; assert `status='pass'` and case persisted.
- `test_syntax_error_no_sandbox`: Mock LLM returning invalid Python; assert sandbox `run_command` is not called on first retry.
- `test_exit_code_success`: End-to-end mock run; assert exit code 0.
- `test_exit_code_coverage_miss`: Mock coverage 60% with threshold 70%; assert exit code 2.
- `test_exit_code_budget`: Mock loop always exhausting budget; assert exit code 3.
- `test_dry_run_no_llm_calls`: Assert no HTTP client invoked with `--dry-run`.
- `test_json_output_schema`: Parse stdout as JSON; assert all required keys present.
- `test_sqlite_persistence`: Run against in-memory SQLite; assert `test_gen_runs` row + `test_gen_cases` rows.
- `test_otel_spans`: Assert 4 spans emitted with correct names.
- `test_secret_redaction_in_prompt`: Function with `password='secret123'` default; assert `<REDACTED>` in generated prompt.

### 12.2 Integration Tests

- `test_pr_diff_ingestion`: Against a real (or mocked) GitHub PR diff; assert `DiffHunk` list non-empty.
- `test_sandbox_pytest_execution`: Generate a trivially correct test for a known-good function; assert sandbox exits 0.
- `test_coverage_measurement`: Generate tests for a function with 10 lines; assert `coverage_pct` computed and non-zero.
- `test_open_pr_branch_creation`: Mock `gh pr create`; assert branch name pattern `tag/test-gen/pr-<N>`.
- `test_post_pr_comment_called`: With `--open-pr --pr 123`; assert `post_pr_comment` called with PR #123 and test PR URL.
- `test_framework_fallback_unittest`: With `shutil.which('pytest')` returning `None`; assert unittest runner used.
- `test_staged_diff_empty`: Stage no changes; assert zero hunks and graceful exit 0 with informative message.

### 12.3 Performance Tests

- `test_single_function_under_60s`: End-to-end run (with real LLM, short function); assert wall time < 60s.
- `test_parallel_ci_runs_no_db_errors`: Spawn 4 concurrent `cmd_test_gen` processes against shared SQLite; assert zero `SQLITE_BUSY` exceptions in any log.
- `test_prompt_token_cap`: Function with 10,000-line body; assert prompt token count <= 8,000 and truncation warning emitted.

---

## 13. Acceptance Criteria

| ID | Criterion | Pass Condition |
|----|-----------|---------------|
| AC-01 | `tag ci test-gen --staged --output /tmp/tests.py` on a repo with 1 staged function produces a valid Python test file. | File exists, `ast.parse()` succeeds, `pytest /tmp/tests.py` exits 0 in a clean venv. |
| AC-02 | `tag ci test-gen --pr 123 --repo owner/repo --profile coder --open-pr` opens a GitHub PR with generated tests. | `gh pr view` confirms a PR exists on branch `tag/test-gen/pr-123`. |
| AC-03 | A PR comment on the source PR #123 links to the test PR. | `gh pr comments 123` includes a comment containing the test PR URL. |
| AC-04 | `tag ci test-gen --staged --min-coverage 99` on a single-function diff exits with code 2 when generated tests cover < 99% of added lines. | Exit code is 2; stderr contains uncovered line numbers. |
| AC-05 | `tag ci test-gen --since HEAD~1 --max-cost 0.00001` exits with code 3 after exhausting the budget. | Exit code is 3; `test_gen_runs.status == 'budget_exhausted'` in SQLite. |
| AC-06 | `tag ci test-gen --staged --dry-run` prints scope and estimated cost without any LLM calls. | `--verbose` log shows no HTTP requests; output contains "DRY RUN". |
| AC-07 | `tag ci test-gen --staged --json` outputs valid JSON to stdout with all required keys. | `json.loads(stdout)` succeeds; `status`, `coverage_pct`, `cost_usd` keys present. |
| AC-08 | A staged diff containing a file named `config.env` results in that file being listed as `[skipped: blocked pattern]` and no `.env` content in any LLM prompt. | Log output contains `config.env` in skipped list; no `.env` content in tracer's prompt field. |
| AC-09 | `tag ci test-gen list` after 3 runs shows 3 rows with correct Run IDs, statuses, and coverage values. | Output row count == 3; each row's coverage matches corresponding SQLite `coverage_pct`. |
| AC-10 | `tag ci test-gen show <run-id>` shows per-function case results. | Output contains function names from `test_gen_cases`; pass/fail status per function displayed. |
| AC-11 | Generated test files always begin with `# Generated by tag ci test-gen`. | `head -1 <output_file>` matches the comment pattern. |
| AC-12 | `tag ci test-gen --pr 123 --staged` (both diff sources) exits with code 1 and a usage error message. | `sys.exit(1)` called; stderr contains "exactly one of --pr, --staged, --since". |
| AC-13 | Agentic loop always terminates within `max_wall_seconds + 5s` even when the sandbox hangs. | Test with `max_wall_seconds=10` and a hang-inducing test body; process exits within 16 seconds. |
| AC-14 | When `pytest` is not on PATH, the command falls back to `unittest` and still produces a passing test file. | Mock `shutil.which('pytest')` to return None; output file is valid unittest; exit code 0. |
| AC-15 | OTel spans are written to the `traces` table in SQLite after every run. | `SELECT COUNT(*) FROM traces WHERE name LIKE 'test_gen.%'` returns >= 4 after one run. |

---

## 14. Dependencies

| Dependency | Type | Required | Notes |
|------------|------|----------|-------|
| `gh` CLI | Runtime | Yes (for `--pr` and `--open-pr`) | Must be authenticated. `--staged` and `--since` work without it. |
| `git` CLI | Runtime | Yes (for `--staged` and `--since`) | Must be called from inside a git repository. |
| `pytest` | Runtime | Recommended | Falls back to `unittest` if absent (FR-23). |
| `coverage` (Python package) | Runtime | Optional | Required for `--min-coverage`; if absent, coverage check is skipped with a warning. |
| `pytest-mock` | Runtime | Optional | Recommended for generated mock fixtures; generated tests use `unittest.mock` if absent. |
| `pytest-json-report` | Runtime | Optional | Used for structured test result parsing; falls back to parsing pytest stdout if absent. |
| PRD-028 sandbox | Feature | Yes | Provides isolated test execution backend. |
| PRD-020 ci.py | Feature | Yes | Provides `fetch_pr_diff`, `post_pr_comment`, `fetch_pr_metadata`. |
| PRD-038 diff_context.py | Feature | Yes | Provides `get_changed_files` for `--staged`/`--since`. |
| PRD-013 tracing.py | Feature | Yes | OTel span emission. |
| PRD-034 security.py | Feature | Yes | Blocked-pattern enforcement for file filtering. |
| PRD-039 budget.py | Feature | Yes | `CostTracker` for cost accumulation and budget enforcement. |
| Anthropic Claude API | Runtime | Yes | LLM backend for test generation; model configured via profile. |

---

## 15. Open Questions

| # | Question | Owner | Target Resolution |
|---|----------|-------|-------------------|
| OQ-1 | Should generated tests be committed directly to the source PR branch (same PR) or always to a separate test PR? A single-PR model simplifies the workflow but risks auto-merge of AI-generated code without review. | Tech lead | Sprint planning before implementation |
| OQ-2 | Should `--min-coverage` measure coverage of changed lines only (diff coverage) or the full function body? Diff-only is more actionable; full-function is a stronger safety signal. | Engineering | Before FR-11 is implemented |
| OQ-3 | What is the right default `--max-cost` cap? $2.00 is generous for a small PR but insufficient for a large refactor. Should it scale with `fns_analyzed`? | Product | Before GA |
| OQ-4 | Should the command support detecting and testing async functions differently (e.g., injecting `@pytest.mark.asyncio`)? Current design treats async functions the same as sync. | Engineering | Can be deferred to v2 |
| OQ-5 | Is the `tag/test-gen/pr-<N>` branch naming convention safe when the source PR is on a fork? `gh pr create --head` behaves differently for fork branches. | Engineering | Before `--open-pr` is shipped |
| OQ-6 | Should `tag ci test-gen` support a `--watch` mode that re-runs automatically on each new commit to the PR branch? This would require a polling loop or webhook trigger (PRD-016). | Product | Backlog |
| OQ-7 | How should the command handle test files that already exist at `--output`? Current design overwrites; an `--append` mode that merges new cases into an existing file may be safer. | Engineering | Before FR-24 is implemented |
| OQ-8 | Should generated tests be deduplicated across runs (e.g., if the same function is analyzed in two separate runs)? The `test_gen_cases` table tracks by `(file_path, function_name)` which enables dedup logic but it is not currently enforced. | Engineering | Sprint 2 |

---

## 16. Complexity and Timeline

**Total estimate: 8 working days (M, 1-2 weeks)**

### Phase 1: Diff Ingestion and Function Analysis (Days 1-2)

- Implement `analyze_diff_for_functions` with `ast` module parsing.
- Implement `DiffHunk`, `TestGenRun`, `TestGenCase`, `SandboxResult` dataclasses.
- Implement `ensure_test_gen_schema` with DDL from Section 10.2.
- Wire `--pr`, `--staged`, `--since` diff source flags.
- Apply secret-scanning blocklist integration.
- Unit tests: FR-01 through FR-07, FR-18.

### Phase 2: Prompt Construction and Agentic Generation Loop (Days 3-4)

- Implement `build_generation_prompt` with the two-role template.
- Implement `_generation_loop` with all three stopping conditions.
- Implement `AgentLoopConfig` and `CostTracker` integration.
- Wire `--max-steps`, `--max-cost`, `--max-wall-seconds`, `--max-retries`.
- Implement syntax validation pre-sandbox guard (NFR-09).
- Unit tests: FR-08 through FR-09, FR-17, NFR-09.

### Phase 3: Sandbox Validation and Coverage Measurement (Days 5-6)

- Implement `validate_generated_tests` with sandbox invocation.
- Implement coverage parsing from `coverage json` output.
- Wire `--min-coverage` enforcement and exit code 2.
- Implement framework auto-detection and fallback.
- Unit tests: FR-10 through FR-14, FR-23, NFR-05.

### Phase 4: PR Opening, Persistence, and Output (Days 7-8)

- Implement `open_test_pr` branch creation and `gh pr create` invocation.
- Implement `post_pr_comment` call linking test PR to source PR.
- Implement SQLite persistence for runs and cases.
- Implement OTel span emission per phase.
- Implement `--json` output, `list` subcommand, `show` subcommand.
- Implement `--dry-run` mode.
- Integration tests: FR-15 through FR-22, AC-01 through AC-15.
- Performance tests: NFR-01, NFR-02.

### Rollout

After Phase 4: internal dogfooding on the TAG repository itself (`tag ci test-gen --pr <next-pr>`). Collect coverage_pct distribution across 10 real PRs. Adjust default `--min-coverage` based on observed baselines before public release.

