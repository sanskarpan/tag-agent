# PRD-060: CI Failure Root-Cause Analysis + Auto-Fix PR (`tag ci diagnose --auto-fix`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (1-2 weeks)
**Category:** CI/CD & Agentic Dev Workflows
**Affects:** `ci.py`
**Depends on:** PRD-027 (eval framework / LLM-as-judge), PRD-028 (sandbox code execution), PRD-013 (agent tracing/observability), PRD-034 (secret scanning), PRD-016 (webhook event triggers), PRD-008 (background task queue), PRD-012 (cost tracking/budget), PRD-039 (token budget enforcement)
**Inspired by:** Devin CI repair, GitHub Copilot fix suggestions, BuildPulse

---

## 1. Overview

Modern software teams merge pull requests dozens of times per day, and each merge triggers CI pipelines that can fail for reasons spanning dependency drift, environment mismatches, flaky tests, broken import paths, type errors, or infrastructure hiccups. When a CI run fails, a developer must context-switch out of their current work, navigate to the GitHub Actions or GitLab CI UI, scroll through hundreds of log lines, identify the root cause, devise a fix, push a patch, and wait for the pipeline to re-run. For a single failure this costs 15–45 minutes. At scale, across a team of ten engineers each seeing two failures per week, this is 150–450 engineer-hours per year of low-value mechanical diagnosis work.

`tag ci diagnose` eliminates this context switch. Given a CI run ID (or the shorthand `--last-failed`), the command fetches the complete log corpus from GitHub Actions or GitLab CI, applies a structured extraction pipeline to isolate the failure signal from the noise, and invokes an LLM with a purpose-built root-cause analysis (RCA) system prompt to produce a structured diagnosis: the primary error type, the most likely root cause, the affected files and line numbers, a confidence score, and a ranked list of remediation steps. The entire analysis fits in a single terminal session, typically completing in under 30 seconds.

With `--auto-fix`, the feature escalates from diagnosis to repair. TAG checks out a fix branch, instructs an agentic loop (using the profile specified by `--profile`, defaulting to `coder`) to apply the recommended fix to the repository, runs the failing test suite locally in the sandbox (PRD-028) to verify the fix holds, and opens a pull request against the original branch. The PR body is populated with the structured RCA, the diff, and a summary of the local verification run. This mirrors what Devin does for CI repair but runs entirely within the developer's local environment and existing TAG installation — no external SaaS dependency.

The agentic loop that applies the fix is governed by the three mandatory stopping conditions from the cluster research context: success (local tests pass), failure (unrecoverable patch error or sandbox exit code ≠ 0 after N retries), and budget (configurable `--max-cost-usd`, `--max-steps`, `--timeout-seconds`). All three conditions are enforced; the loop cannot run away. Every step is traced (PRD-013), cost-attributed (PRD-012 / PRD-046), and stored in SQLite for later audit. The fix PR is never opened unless the local verification sandbox run exits 0.

This feature targets the CI/CD cluster (Cluster B) and is directly inspired by three production systems: Devin's CI repair agent, which autonomously patches failing pipelines by re-running them after applying code changes; GitHub Copilot's "Fix" suggestions in the Actions UI, which propose single-file patches but cannot apply them; and BuildPulse's flaky test detection, which surfaces the symptom but stops short of remediation. TAG's implementation combines all three into a single CLI command with local execution, audit trails, and cost controls.

---

## 2. Problem Statement

### 2.1 CI failure diagnosis is expensive manual work

When a GitHub Actions or GitLab CI pipeline fails, the developer receives a notification that contains a URL. To understand what went wrong, they must: open the browser, navigate to the pipeline, select the failing job, scroll through timestamped log output (often 1,000–10,000 lines), identify the first error (which may appear hundreds of lines before the actual failure exit), correlate that error with the source files implicated, reason about the root cause, and then decide on a fix. Experienced engineers develop pattern-matching intuitions for common failure types (import errors, type mismatches, missing env vars, lockfile conflicts), but that pattern-matching is purely manual and non-transferable. Junior engineers spend 2–4x longer on the same task.

No existing tool reads the full log, extracts the structured failure signal, and presents a ranked diagnosis in under 60 seconds. `gh run view --log-failed` retrieves the failure-annotated log but does no analysis. GitHub Copilot's fix suggestions are limited to the checked-in code changes in the PR diff and do not read CI logs at all.

### 2.2 Automated fix attempts lack verification gates

Devin and similar AI coding agents can propose patches to fix CI failures, but they operate as external SaaS platforms that require granting repository access, sending code to third-party servers, and accepting the platform's own cost model and trust boundary. The patch application is also not locally verifiable before the PR is opened — the developer must wait for the CI pipeline to re-run (another 5–15 minutes) to know whether the fix was correct.

TAG's `--auto-fix` mode applies the fix locally, runs the relevant tests in a sandboxed environment (PRD-028), and only opens a PR if the local verification succeeds. This eliminates the re-run wait and gives the developer machine-verifiable confidence before the patch touches GitHub.

### 2.3 No structured RCA trail for repeated failures

When the same CI failure recurs across multiple runs or multiple repositories, there is currently no system that accumulates a structured history of: what the root cause was, what fix was applied, whether the fix worked, and how long it took. Teams repeatedly diagnose the same failure types from scratch. TAG stores every diagnosis in SQLite with full metadata, enabling `tag ci diagnose --history` to surface recurring failure patterns, identify the most time-consuming failure categories, and measure MTTR (Mean Time To Resolution) across the team's CI history.

---

## 3. Goals and Non-Goals

### Goals

| # | Goal |
|---|------|
| G1 | Fetch complete run logs from GitHub Actions (via `gh` CLI) and GitLab CI (via GitLab REST API) given a run/pipeline ID or `--last-failed` shorthand. |
| G2 | Produce a structured, machine-readable RCA (root cause category, affected files, confidence score, remediation steps) in under 60 seconds for any log up to 100,000 lines. |
| G3 | `--auto-fix` mode applies the LLM-proposed fix via an agentic loop, verifies it in a local sandbox (PRD-028), and opens a PR only on sandbox verification success. |
| G4 | The agentic fix loop enforces all three stopping conditions: success, failure, and budget (`--max-cost-usd`, `--max-steps`, `--timeout-seconds`). |
| G5 | Every diagnosis and fix attempt is persisted to SQLite (`ci_diagnoses`, `ci_fix_attempts` tables) for audit and history queries. |
| G6 | `tag ci diagnose --history` surfaces recurring failure categories, MTTR trends, and fix success rates from the local SQLite database. |
| G7 | `--json` flag emits machine-readable output for CI pipeline integration (e.g., posting the RCA as a PR comment via another `tag` command). |
| G8 | Support `--repo owner/repo` flag to diagnose failures in any repository the user has `gh` access to, not just the current working directory. |
| G9 | Log fetching, LLM calls, and sandbox runs are all cost-attributed (PRD-012) and respect a configurable `--max-cost-usd` budget with a pre-execution cost estimate prompt. |
| G10 | Secret scanning (PRD-034) is applied to the extracted log content before any LLM call; detected secrets are redacted and the user is warned. |

### Non-Goals

| # | Non-Goal |
|----|----------|
| NG1 | Replacing a full CI platform. TAG does not re-run the CI pipeline; it diagnoses failures and proposes fixes that are verified locally. |
| NG2 | Supporting CI providers other than GitHub Actions and GitLab CI in this version. Jenkins, CircleCI, and Buildkite support are future work. |
| NG3 | Auto-merging the fix PR. TAG opens the PR with a `ready for review` label; merge is always a human action. |
| NG4 | Flaky test detection. Flaky test identification (requiring multi-run statistical analysis) is a separate feature. This PRD handles single-run failures. |
| NG5 | Running the full CI suite locally. The sandbox verification step runs only the failing test(s) identified in the RCA, not the full pipeline. |
| NG6 | Fixing infrastructure-level failures (AWS quota exceeded, DNS failures, container registry timeouts). LLM diagnosis is returned but `--auto-fix` exits with a clear message that the failure category is not code-fixable. |

---

## 4. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Time to structured RCA | p95 < 30 seconds for logs up to 10,000 lines | `ci_diagnoses.diagnosis_latency_ms` in SQLite |
| RCA category accuracy | ≥ 85% correct root-cause category on a 100-case labeled eval set | `tag eval run --suite evals/ci_diagnose.yaml` (PRD-027) |
| Auto-fix sandbox verification pass rate | ≥ 65% of fixable failure categories verified green on first attempt | `ci_fix_attempts.sandbox_exit_code = 0` rate |
| Fix PR quality (LLM-as-judge) | ≥ 4.0/5.0 on the "fix adequacy" rubric | PRD-045 LLM-as-judge evaluator on PR body |
| Secret redaction coverage | 0 known secret patterns leak to LLM | PRD-034 scan coverage in unit tests |
| Budget overrun rate | 0 runs exceed `--max-cost-usd` | Budget enforcement assertion in `AgentLoopConfig` |
| MTTR reduction (field report) | ≥ 40% reduction vs. manual diagnosis baseline | User survey at 30-day mark, N ≥ 20 respondents |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Backend engineer | run `tag ci diagnose --last-failed --repo my-org/api` after my PR breaks CI | I get a structured root-cause analysis in my terminal without leaving my editor or opening a browser |
| U2 | Senior engineer | run `tag ci diagnose --run-id 1234567 --auto-fix --profile coder` | TAG applies a fix, verifies it locally, and opens a PR so I can review a concrete patch rather than debug from scratch |
| U3 | Platform engineer | run `tag ci diagnose --last-failed --json` in a GitHub Actions workflow step | The structured RCA JSON is posted as a PR comment automatically, giving reviewers immediate context on the failure |
| U4 | Team lead | run `tag ci diagnose --history --repo my-org/api --last 90d` | I can see which failure categories are most common, which are auto-fixable, and what our MTTR trend looks like |
| U5 | Junior developer | run `tag ci diagnose --run-id 9876543` | I get a plain-English explanation of what went wrong and concrete remediation steps, even if I don't have deep CI/CD experience |
| U6 | DevOps engineer | configure `--max-cost-usd 0.50` on the auto-fix command | The agentic loop never spends more than 50 cents even if the failure is complex and requires many model calls |
| U7 | Security-conscious engineer | see PRD-034 secret scanning applied automatically before any log content is sent to an LLM | I have confidence that env vars, tokens, and API keys embedded in CI logs are never exfiltrated to LLM providers |
| U8 | Developer | run `tag ci diagnose --run-id 1234567 --provider gitlab --project-id 42` | I can diagnose failures in GitLab CI pipelines with the same command surface as GitHub Actions |

---

## 6. Proposed CLI Surface

All subcommands extend the existing `tag ci` namespace. New subcommands are `diagnose` and `ci history`.

### 6.1 `tag ci diagnose` — primary diagnosis command

```
tag ci diagnose \
  [--run-id <run_id>]              # GitHub Actions run ID or GitLab pipeline ID
  [--last-failed]                  # Fetch the most recent failed run for --repo
  [--repo <owner/repo>]            # GitHub repository; defaults to git remote origin
  [--provider {github,gitlab}]     # CI provider; auto-detected from remote URL if omitted
  [--project-id <id>]              # GitLab project ID (required for GitLab provider)
  [--branch <branch>]              # Filter --last-failed to a specific branch
  [--auto-fix]                     # Apply fix via agentic loop + open PR
  [--profile <profile>]            # TAG profile for the fix agent (default: coder)
  [--max-cost-usd <float>]         # Budget cap for the entire diagnose + fix flow (default: 2.00)
  [--max-steps <int>]              # Max agentic loop steps for --auto-fix (default: 20)
  [--timeout-seconds <int>]        # Wall-clock timeout for --auto-fix loop (default: 300)
  [--sandbox-backend {restricted,docker,modal}]  # Sandbox backend for verification (default: auto)
  [--no-verify]                    # Skip local sandbox verification (open PR immediately)
  [--draft]                        # Open fix PR as draft
  [--dry-run]                      # Fetch + diagnose; do not apply fix or open PR
  [--yes]                          # Skip the cost estimate confirmation prompt
  [--json]                         # Emit machine-readable JSON instead of rich terminal output
  [--output <file>]                # Write JSON output to a file
  [--redact-secrets / --no-redact-secrets]  # Toggle PRD-034 secret scanning (default: on)
```

**Example invocations:**

```bash
# Diagnose a specific GitHub Actions run
tag ci diagnose --run-id 1234567 --repo acme/api

# Diagnose the most recent failed run on the current branch
tag ci diagnose --last-failed

# Diagnose and auto-fix using the coder profile, cap at $1
tag ci diagnose --run-id 1234567 --auto-fix --profile coder --max-cost-usd 1.00

# Diagnose a GitLab pipeline
tag ci diagnose --run-id 987654 --provider gitlab --project-id 42

# Dry run — fetch logs and diagnose but do not apply any fix
tag ci diagnose --last-failed --auto-fix --dry-run

# JSON output for use in another script or CI step
tag ci diagnose --last-failed --json | jq '.root_cause.category'
```

**Example terminal output (diagnosis-only mode):**

```
tag ci diagnose --run-id 1234567 --repo acme/api

Fetching CI logs for run 1234567 (acme/api)...
  Jobs: build (✓), lint (✓), test (✗)
  Fetching logs for failing job: test [2,847 lines]
  Redacting secrets... 0 patterns detected.
  Estimated cost: ~$0.04  Proceed? [Y/n] Y

Analyzing root cause...

╔══════════════════════════════════════════════════════════════╗
║  CI Failure Root-Cause Analysis                             ║
║  Run: 1234567  |  Job: test  |  Branch: feat/user-auth      ║
╚══════════════════════════════════════════════════════════════╝

Root Cause Category : IMPORT_ERROR
Confidence          : 0.94
Affected File       : src/tag/auth.py:12
Affected Symbol     : from tag.utils import hash_password

Primary Error:
  ModuleNotFoundError: No module named 'tag.utils.hash_password'
  (hash_password was moved to tag.crypto in commit a3f1b29)

Remediation Steps:
  1. Update import in src/tag/auth.py:12 to:
       from tag.crypto import hash_password
  2. Run: python -m pytest tests/test_auth.py to verify
  3. Check for other files importing from tag.utils:
       grep -r "from tag.utils import" src/

Supporting Evidence:
  Log line 1,432: ModuleNotFoundError: No module named 'tag.utils'
  Log line 1,435: During handling of the above exception...
  Log line 1,441: FAILED tests/test_auth.py::test_login - ModuleNotFoundError

Diagnosis stored: diag-a1b2c3d4
Run `tag ci diagnose --auto-fix --run-id 1234567` to apply this fix.
```

**Example terminal output (auto-fix mode):**

```
tag ci diagnose --run-id 1234567 --auto-fix --profile coder

[Diagnosis already available: diag-a1b2c3d4 — skipping re-analysis]

Estimated fix cost: ~$0.18  Proceed? [Y/n] Y

Creating fix branch: fix/ci-diag-a1b2c3d4...
Starting agentic fix loop (profile: coder, max-steps: 20, budget: $2.00)...

  Step 1/20  [coder]  Reading src/tag/auth.py
  Step 2/20  [coder]  Editing src/tag/auth.py:12 (import path update)
  Step 3/20  [coder]  Searching for other tag.utils imports
  Step 4/20  [coder]  Editing src/tag/middleware.py:8 (same import path)
  Step 5/20  [coder]  Running linter on modified files... OK

Verifying fix in sandbox (backend: docker)...
  Running: python -m pytest tests/test_auth.py tests/test_middleware.py
  ...........
  2 passed in 3.41s

Fix verified. Opening pull request...

PR opened: https://github.com/acme/api/pull/892
  Title:    fix: update tag.utils import paths to tag.crypto [CI diag-a1b2c3d4]
  Branch:   fix/ci-diag-a1b2c3d4 → main
  Steps:    5
  Cost:     $0.07
  Duration: 28s

Fix attempt stored: fix-e5f6a7b8
```

### 6.2 `tag ci history` — RCA and fix history

```
tag ci history \
  [--repo <owner/repo>]
  [--last <N>]                     # Show last N diagnoses (default: 20)
  [--since <date>]                 # ISO date filter, e.g. 2026-01-01
  [--category <category>]          # Filter by root cause category
  [--fixed-only]                   # Show only diagnoses that resulted in a merged fix PR
  [--json]
```

**Example terminal output:**

```
tag ci history --repo acme/api --last 10

  ID            Date         Category         Confidence  Fixed?  PR
  diag-a1b2c3d4 2026-06-10   IMPORT_ERROR     0.94        Yes     #892
  diag-b2c3d4e5 2026-06-08   DEP_CONFLICT     0.81        No      —
  diag-c3d4e5f6 2026-06-05   TYPE_ERROR       0.88        Yes     #880
  diag-d4e5f6a7 2026-06-01   ENV_VAR_MISSING  0.92        No      —

Top categories (last 90d): IMPORT_ERROR (7), TYPE_ERROR (4), DEP_CONFLICT (3)
Auto-fix success rate: 62%  |  Avg MTTR: 4m 32s
```

---

## 7. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `tag ci diagnose` MUST fetch all failing job logs for a given run ID from GitHub Actions using `gh run view --log-failed --repo` or the equivalent REST API call. | Must |
| FR-02 | `tag ci diagnose` MUST fetch failing job logs from GitLab CI using the GitLab Pipelines API (`GET /projects/:id/pipelines/:pipeline_id/jobs` + `GET /projects/:id/jobs/:job_id/trace`) when `--provider gitlab` is specified. | Must |
| FR-03 | `--last-failed` MUST query the most recent run with a `failure` conclusion on the specified branch (or default branch if `--branch` is not given). | Must |
| FR-04 | Log content MUST be processed by PRD-034 secret scanning before any LLM call; detected patterns MUST be replaced with `[REDACTED:<pattern_name>]` and a warning printed to stderr. `--no-redact-secrets` skips this step with an explicit warning. | Must |
| FR-05 | Logs exceeding 100,000 lines MUST be windowed using the ACI windowed viewer pattern: retain the first 200 lines (job setup context) + the last 500 lines (failure context) + any lines matching error-signal patterns (`ERROR`, `FAILED`, `Traceback`, `ModuleNotFoundError`, `AssertionError`, `exit code`). Total extracted log MUST fit within 32,000 tokens. | Must |
| FR-06 | The LLM diagnosis call MUST use a structured output schema (`CIDiagnosis` dataclass) enforced via JSON mode or tool-call response format. | Must |
| FR-07 | The `CIDiagnosis` output MUST include: `run_id`, `provider`, `repo`, `job_name`, `root_cause_category` (enum), `confidence` (0.0–1.0), `primary_error_text`, `affected_files` (list of `FileRef`), `remediation_steps` (ordered list of strings), `is_auto_fixable` (bool), `fix_unfixable_reason` (str or None). | Must |
| FR-08 | `root_cause_category` MUST be one of the enum values defined in `CIFailureCategory`: `IMPORT_ERROR`, `TYPE_ERROR`, `TEST_ASSERTION`, `DEP_CONFLICT`, `LINT_ERROR`, `BUILD_ERROR`, `ENV_VAR_MISSING`, `INFRA_ERROR`, `TIMEOUT`, `FLAKY_TEST`, `UNKNOWN`. | Must |
| FR-09 | `--auto-fix` MUST be a no-op (with a clear user-facing message) when `is_auto_fixable` is `False` or when `root_cause_category` is `INFRA_ERROR`, `TIMEOUT`, or `FLAKY_TEST`. | Must |
| FR-10 | `--auto-fix` MUST create a new git branch named `fix/ci-diag-<diagnosis_id>` from the HEAD of the failing run's branch before invoking the agentic loop. | Must |
| FR-11 | The agentic fix loop MUST enforce all three stopping conditions: (a) success = sandbox verification exits 0, (b) failure = loop returns a `FAILED` status after max retries, (c) budget = any of `--max-steps`, `--max-cost-usd`, `--timeout-seconds` exceeded. | Must |
| FR-12 | Sandbox verification MUST use the PRD-028 sandbox subsystem. The test command to run is derived from the `remediation_steps` or inferred from the failing test file paths in `affected_files`. | Must |
| FR-13 | `--no-verify` MUST skip sandbox verification and open the PR immediately; this flag MUST print a prominent warning that no local verification was performed. | Must |
| FR-14 | The fix PR body MUST include: the structured RCA summary, the diff produced by the agentic loop, the local verification result (if performed), the `diagnosis_id`, and a link to the original failing run. | Must |
| FR-15 | All diagnosis and fix-attempt records MUST be persisted to the `ci_diagnoses` and `ci_fix_attempts` SQLite tables (see Section 9 for DDL). | Must |
| FR-16 | `--json` output MUST be valid JSON conforming to the `CIDiagnosis` schema, serializable with `dataclasses.asdict()`. | Must |
| FR-17 | `tag ci history` MUST query `ci_diagnoses` and `ci_fix_attempts` tables and display results in a Rich table or JSON. | Must |
| FR-18 | Before any LLM call, the estimated cost MUST be printed and the user prompted for confirmation, unless `--yes` is passed or the `CI` environment variable is set to a truthy value. | Must |
| FR-19 | `--auto-fix` MUST add the label `tag-auto-fix` to the opened PR. If `--draft` is passed, the PR MUST be opened as a draft. | Should |
| FR-20 | Every agentic loop step for `--auto-fix` MUST emit an OpenTelemetry span (PRD-013) with attributes `ci.diagnosis_id`, `ci.fix_attempt_id`, `ci.step_index`. | Should |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Diagnosis latency (log fetch + LLM call) for a 10,000-line log | p95 < 30 seconds |
| NFR-02 | Maximum log size processed without truncation | 100,000 lines |
| NFR-03 | Maximum total cost for a diagnosis-only run (no auto-fix) | < $0.10 at default model |
| NFR-04 | Maximum total cost for an auto-fix run (default budget) | User-configurable; default cap $2.00 |
| NFR-05 | SQLite writes MUST use WAL mode and the existing `open_db()` helper; no direct `sqlite3.connect()` calls | Always |
| NFR-06 | All network calls (GitHub API, GitLab API, LLM API) MUST respect a 30-second per-call timeout with exponential backoff (max 3 retries) | Always |
| NFR-07 | Secret scanning (FR-04) MUST complete in < 2 seconds for a 100,000-line log | Always |
| NFR-08 | The `--json` output format MUST be stable across patch versions; breaking schema changes require a minor version bump | Always |
| NFR-09 | The agentic fix loop MUST never modify files outside the current git repository root | Always |
| NFR-10 | `gh` CLI and `GITHUB_TOKEN` / `GITLAB_TOKEN` are the only required external credentials; the feature gracefully degrades with a clear error message if either is absent | Always |
| NFR-11 | The feature MUST work with Python 3.10+ and all existing TAG dependencies; no new mandatory dependencies | Always |
| NFR-12 | Optional dependency: `httpx` for direct GitLab REST API calls (already present in TAG's optional extras) | Optional |

---

## 9. Technical Design

### 9.1 New and modified files

| File | Change |
|------|--------|
| `src/tag/ci.py` | Primary target — add all new functions for log fetching, log windowing, LLM diagnosis, agentic fix loop, PR creation, and history queries |
| `src/tag/controller.py` | Add `cmd_ci_diagnose()` and `cmd_ci_history()` command handlers; wire up to the `tag ci` Click group |
| `~/.tag/runtime/tag.sqlite3` | Add `ci_diagnoses` and `ci_fix_attempts` tables (see DDL below) |
| `evals/ci_diagnose.yaml` | New eval suite (PRD-027 format) with 20 labeled CI log → RCA test cases |

### 9.2 SQLite DDL

```sql
-- Stores one row per diagnosis invocation.
CREATE TABLE IF NOT EXISTS ci_diagnoses (
    id                   TEXT PRIMARY KEY,          -- UUID, e.g. "diag-a1b2c3d4"
    repo                 TEXT NOT NULL,             -- "owner/repo"
    provider             TEXT NOT NULL DEFAULT 'github',  -- 'github' | 'gitlab'
    run_id               TEXT NOT NULL,             -- CI platform run/pipeline ID
    branch               TEXT,                      -- branch the run was on
    job_name             TEXT,                      -- specific failing job name
    log_lines_total      INTEGER,                   -- total lines in the fetched log
    log_lines_sent       INTEGER,                   -- lines actually sent to LLM (after windowing)
    root_cause_category  TEXT NOT NULL DEFAULT 'UNKNOWN',
    confidence           REAL NOT NULL DEFAULT 0.0,
    primary_error_text   TEXT,
    affected_files       TEXT,                      -- JSON array of FileRef dicts
    remediation_steps    TEXT,                      -- JSON array of strings
    is_auto_fixable      INTEGER NOT NULL DEFAULT 0,  -- BOOLEAN
    fix_unfixable_reason TEXT,
    model_used           TEXT,
    prompt_tokens        INTEGER,
    completion_tokens    INTEGER,
    cost_usd             REAL,
    diagnosis_latency_ms INTEGER,
    secrets_redacted     INTEGER NOT NULL DEFAULT 0,  -- count of redacted patterns
    raw_llm_response     TEXT,                      -- stored for audit
    created_at           TIMESTAMPTZ NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    -- store the full windowed log for reproducibility / re-diagnosis without re-fetching
    windowed_log_sha256  TEXT                       -- SHA-256 of the log content sent to LLM
);
CREATE INDEX IF NOT EXISTS idx_ci_diag_repo ON ci_diagnoses(repo, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_ci_diag_category ON ci_diagnoses(root_cause_category, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_ci_diag_run ON ci_diagnoses(run_id);

-- Stores one row per --auto-fix attempt (one diagnosis can have multiple fix attempts).
CREATE TABLE IF NOT EXISTS ci_fix_attempts (
    id                   TEXT PRIMARY KEY,          -- UUID, e.g. "fix-e5f6a7b8"
    diagnosis_id         TEXT NOT NULL REFERENCES ci_diagnoses(id),
    fix_branch           TEXT NOT NULL,             -- git branch name
    profile              TEXT NOT NULL DEFAULT 'coder',
    status               TEXT NOT NULL DEFAULT 'running',  -- running|success|failed|budget_exceeded|aborted
    steps_taken          INTEGER NOT NULL DEFAULT 0,
    max_steps            INTEGER NOT NULL DEFAULT 20,
    cost_usd             REAL NOT NULL DEFAULT 0.0,
    max_cost_usd         REAL NOT NULL DEFAULT 2.0,
    timeout_seconds      INTEGER NOT NULL DEFAULT 300,
    sandbox_backend      TEXT,
    sandbox_exit_code    INTEGER,
    sandbox_output       TEXT,
    pr_url               TEXT,
    pr_number            INTEGER,
    diff_stat            TEXT,                      -- e.g. "2 files changed, 5 insertions(+), 2 deletions(-)"
    stop_reason          TEXT,                      -- success|max_steps|max_cost|timeout|error
    error_message        TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    completed_at         TIMESTAMPTZ,
    trace_id             TEXT                       -- OTel trace ID for the fix loop
);
CREATE INDEX IF NOT EXISTS idx_ci_fix_diag ON ci_fix_attempts(diagnosis_id);
CREATE INDEX IF NOT EXISTS idx_ci_fix_status ON ci_fix_attempts(status, created_at DESC);
```

### 9.3 Core dataclasses

```python
from __future__ import annotations

import enum
import uuid
from dataclasses import dataclass, field
from typing import Optional


class CIProvider(str, enum.Enum):
    GITHUB = "github"
    GITLAB = "gitlab"


class CIFailureCategory(str, enum.Enum):
    IMPORT_ERROR    = "IMPORT_ERROR"
    TYPE_ERROR      = "TYPE_ERROR"
    TEST_ASSERTION  = "TEST_ASSERTION"
    DEP_CONFLICT    = "DEP_CONFLICT"
    LINT_ERROR      = "LINT_ERROR"
    BUILD_ERROR     = "BUILD_ERROR"
    ENV_VAR_MISSING = "ENV_VAR_MISSING"
    INFRA_ERROR     = "INFRA_ERROR"
    TIMEOUT         = "TIMEOUT"
    FLAKY_TEST      = "FLAKY_TEST"
    UNKNOWN         = "UNKNOWN"

    @property
    def is_auto_fixable(self) -> bool:
        """Return True for categories that are amenable to code-level auto-fix."""
        return self in {
            CIFailureCategory.IMPORT_ERROR,
            CIFailureCategory.TYPE_ERROR,
            CIFailureCategory.TEST_ASSERTION,
            CIFailureCategory.DEP_CONFLICT,
            CIFailureCategory.LINT_ERROR,
            CIFailureCategory.BUILD_ERROR,
            CIFailureCategory.ENV_VAR_MISSING,
        }


@dataclass
class FileRef:
    path: str
    line_number: Optional[int] = None
    symbol: Optional[str] = None


@dataclass
class CIDiagnosis:
    """Structured output from the LLM root-cause analysis call."""
    id: str = field(default_factory=lambda: f"diag-{uuid.uuid4().hex[:8]}")
    run_id: str = ""
    provider: CIProvider = CIProvider.GITHUB
    repo: str = ""
    branch: Optional[str] = None
    job_name: Optional[str] = None
    root_cause_category: CIFailureCategory = CIFailureCategory.UNKNOWN
    confidence: float = 0.0
    primary_error_text: str = ""
    affected_files: list[FileRef] = field(default_factory=list)
    remediation_steps: list[str] = field(default_factory=list)
    is_auto_fixable: bool = False
    fix_unfixable_reason: Optional[str] = None
    # Populated after the LLM call
    model_used: Optional[str] = None
    prompt_tokens: int = 0
    completion_tokens: int = 0
    cost_usd: float = 0.0
    diagnosis_latency_ms: int = 0
    secrets_redacted: int = 0


@dataclass
class CIFixAttempt:
    """Tracks a single --auto-fix agentic loop execution."""
    id: str = field(default_factory=lambda: f"fix-{uuid.uuid4().hex[:8]}")
    diagnosis_id: str = ""
    fix_branch: str = ""
    profile: str = "coder"
    status: str = "running"       # running|success|failed|budget_exceeded|aborted
    steps_taken: int = 0
    max_steps: int = 20
    cost_usd: float = 0.0
    max_cost_usd: float = 2.0
    timeout_seconds: int = 300
    sandbox_backend: Optional[str] = None
    sandbox_exit_code: Optional[int] = None
    sandbox_output: Optional[str] = None
    pr_url: Optional[str] = None
    pr_number: Optional[int] = None
    diff_stat: Optional[str] = None
    stop_reason: Optional[str] = None
    error_message: Optional[str] = None
    trace_id: Optional[str] = None


@dataclass
class AgentLoopConfig:
    """Stopping conditions for the auto-fix agentic loop (FR-11)."""
    max_steps: int = 20
    max_cost_usd: float = 2.0
    max_wall_seconds: int = 300
    max_diff_lines: int = 500     # refuse fix if patch exceeds this many changed lines
    blocked_commands: list[str] = field(default_factory=lambda: [
        "git push", "git push --force", "rm -rf", "curl", "wget",
        "pip install", "npm install",  # disallow dependency changes without human review
    ])

    def check_budget(self, steps: int, cost: float, elapsed: float) -> Optional[str]:
        """Return the stop reason string if any limit is exceeded, else None."""
        if steps >= self.max_steps:
            return "max_steps"
        if cost >= self.max_cost_usd:
            return "max_cost"
        if elapsed >= self.max_wall_seconds:
            return "timeout"
        return None
```

### 9.4 Log fetching layer

**GitHub Actions — `fetch_github_run_logs()`:**

```python
def fetch_github_run_logs(repo: str, run_id: str) -> dict[str, str]:
    """Return a dict of {job_name: log_text} for all failing jobs in a run.

    Uses `gh run view --log-failed` to retrieve only failing job logs.
    Falls back to the GitHub REST API if gh CLI is unavailable.

    Returns:
        Dict mapping job name to raw log text.

    Raises:
        RuntimeError: if gh CLI fails and REST API is inaccessible.
    """
    result = subprocess.run(
        ["gh", "run", "view", run_id, "--repo", repo, "--log-failed"],
        capture_output=True, text=True, timeout=30,
    )
    if result.returncode != 0:
        raise RuntimeError(
            f"gh run view failed for {repo} run {run_id}: {result.stderr.strip()}"
        )
    # Parse the interleaved log output into per-job sections
    return _parse_gh_run_log_output(result.stdout)


def fetch_last_failed_run_id(repo: str, branch: Optional[str] = None) -> str:
    """Return the run ID of the most recent failed workflow run."""
    args = [
        "gh", "run", "list", "--repo", repo,
        "--status", "failure", "--limit", "1",
        "--json", "databaseId",
    ]
    if branch:
        args += ["--branch", branch]
    result = subprocess.run(args, capture_output=True, text=True, timeout=30)
    if result.returncode != 0:
        raise RuntimeError(f"gh run list failed: {result.stderr.strip()}")
    runs = json.loads(result.stdout)
    if not runs:
        raise ValueError(f"No failed runs found for {repo}" + (f" on {branch}" if branch else ""))
    return str(runs[0]["databaseId"])
```

**GitLab CI — `fetch_gitlab_job_logs()`:**

```python
def fetch_gitlab_job_logs(
    project_id: str | int,
    pipeline_id: str | int,
    gitlab_url: str = "https://gitlab.com",
) -> dict[str, str]:
    """Fetch failing job traces from a GitLab CI pipeline.

    Uses httpx with GITLAB_TOKEN from the environment. Applies the
    GitLab Jobs API: GET /projects/:id/pipelines/:pipeline_id/jobs
    followed by GET /projects/:id/jobs/:job_id/trace for each failed job.

    Returns:
        Dict mapping job name to raw trace text.
    """
    import httpx
    token = os.environ.get("GITLAB_TOKEN", "")
    headers = {"PRIVATE-TOKEN": token} if token else {}
    base = f"{gitlab_url}/api/v4/projects/{project_id}"

    with httpx.Client(timeout=30) as client:
        resp = client.get(
            f"{base}/pipelines/{pipeline_id}/jobs",
            headers=headers,
            params={"scope[]": "failed"},
        )
        resp.raise_for_status()
        jobs = resp.json()

        result: dict[str, str] = {}
        for job in jobs:
            trace_resp = client.get(f"{base}/jobs/{job['id']}/trace", headers=headers)
            trace_resp.raise_for_status()
            result[job["name"]] = trace_resp.text
    return result
```

### 9.5 Log windowing algorithm (ACI-inspired, FR-05)

The ACI pattern from SWE-agent research shows that structured, bounded log extraction dramatically improves LLM accuracy vs. raw log dumps. The windowing algorithm retains:

1. **Header window** — first 200 lines (environment setup, dependency install, job initialization)
2. **Error-signal lines** — all lines matching the error signal regex (deduplicated, preserving original position)
3. **Tail window** — last 500 lines (failure context, test summary, exit code)

```python
import re
import hashlib

_ERROR_PATTERNS = re.compile(
    r"(ERROR|FAILED|Traceback|ModuleNotFoundError|ImportError|TypeError|"
    r"AssertionError|AttributeError|SyntaxError|NameError|ValueError|"
    r"RuntimeError|exit code [1-9]|FATAL|CRITICAL|fatal error)",
    re.IGNORECASE,
)

def window_log(
    log_text: str,
    max_total_lines: int = 1000,
    head_lines: int = 200,
    tail_lines: int = 500,
) -> tuple[str, int, int]:
    """Apply the ACI windowing algorithm to a CI log.

    Returns:
        (windowed_text, original_line_count, sent_line_count)
    """
    lines = log_text.splitlines()
    total = len(lines)

    if total <= max_total_lines:
        return log_text, total, total

    head = lines[:head_lines]
    tail = lines[-tail_lines:]
    tail_start_idx = total - tail_lines

    # Collect error-signal lines from the middle section
    middle_error_lines: list[str] = []
    for i, line in enumerate(lines[head_lines:tail_start_idx], start=head_lines):
        if _ERROR_PATTERNS.search(line):
            middle_error_lines.append(f"[line {i+1}] {line}")

    sections = []
    sections.append("\n".join(head))
    if middle_error_lines:
        sections.append(
            f"\n[... middle section ({tail_start_idx - head_lines} lines) — "
            f"error signals extracted below ...]\n" +
            "\n".join(middle_error_lines[:200])  # cap middle error lines at 200
        )
    else:
        sections.append(
            f"\n[... {tail_start_idx - head_lines} middle lines omitted (no error signals detected) ...]"
        )
    sections.append("\n[... tail ...]\n" + "\n".join(tail))

    windowed = "\n".join(sections)
    sent = len(windowed.splitlines())
    return windowed, total, sent


def log_sha256(log_text: str) -> str:
    return hashlib.sha256(log_text.encode("utf-8")).hexdigest()
```

### 9.6 LLM diagnosis call

The diagnosis prompt uses a JSON-mode tool call to enforce the `CIDiagnosis` output schema. The system prompt is constructed from `_DIAGNOSE_SYSTEM` (already in `ci.py`) extended with the structured output contract.

```python
_DIAGNOSE_STRUCTURED_SYSTEM = """
You are an expert DevOps engineer performing root-cause analysis on a failing CI/CD pipeline.

You MUST return a JSON object conforming to this schema:
{
  "root_cause_category": "<one of: IMPORT_ERROR|TYPE_ERROR|TEST_ASSERTION|DEP_CONFLICT|LINT_ERROR|BUILD_ERROR|ENV_VAR_MISSING|INFRA_ERROR|TIMEOUT|FLAKY_TEST|UNKNOWN>",
  "confidence": <float 0.0-1.0>,
  "primary_error_text": "<the exact error message from the log>",
  "job_name": "<name of the failing CI job>",
  "affected_files": [
    {"path": "<relative file path>", "line_number": <int or null>, "symbol": "<symbol or null>"}
  ],
  "remediation_steps": [
    "<concrete, actionable step 1>",
    "<concrete, actionable step 2>"
  ],
  "is_auto_fixable": <true|false>,
  "fix_unfixable_reason": "<explanation if is_auto_fixable is false, else null>"
}

Rules:
- Set is_auto_fixable=false for INFRA_ERROR, TIMEOUT, FLAKY_TEST, and UNKNOWN categories.
- remediation_steps must be ordered from most to least impactful.
- confidence reflects how certain you are that you have identified the PRIMARY root cause
  (not just a symptom). Use 0.9+ only when the error message directly names the file and symbol.
- Limit affected_files to the 3 most relevant files.
- Limit remediation_steps to 5 steps.
"""


def diagnose_from_log(
    windowed_log: str,
    repo: str,
    run_id: str,
    provider: CIProvider,
    model: str = "claude-sonnet-4-6",
) -> CIDiagnosis:
    """Call the LLM with the windowed log and parse the structured diagnosis."""
    import anthropic
    import time

    client = anthropic.Anthropic()
    t0 = time.monotonic()

    response = client.messages.create(
        model=model,
        max_tokens=1024,
        system=_DIAGNOSE_STRUCTURED_SYSTEM,
        messages=[{
            "role": "user",
            "content": (
                f"Repository: {repo}\nProvider: {provider.value}\nRun ID: {run_id}\n\n"
                f"## CI Log\n```\n{windowed_log}\n```"
            ),
        }],
    )

    latency_ms = int((time.monotonic() - t0) * 1000)
    raw_json = response.content[0].text
    data = json.loads(raw_json)

    diag = CIDiagnosis(
        run_id=run_id,
        provider=provider,
        repo=repo,
        root_cause_category=CIFailureCategory(data["root_cause_category"]),
        confidence=float(data["confidence"]),
        primary_error_text=data.get("primary_error_text", ""),
        job_name=data.get("job_name"),
        affected_files=[FileRef(**f) for f in data.get("affected_files", [])],
        remediation_steps=data.get("remediation_steps", []),
        is_auto_fixable=bool(data.get("is_auto_fixable", False)),
        fix_unfixable_reason=data.get("fix_unfixable_reason"),
        model_used=model,
        prompt_tokens=response.usage.input_tokens,
        completion_tokens=response.usage.output_tokens,
        diagnosis_latency_ms=latency_ms,
    )
    return diag
```

### 9.7 Agentic fix loop

The fix loop uses the existing TAG profile system and agentic capabilities. It implements the three stopping conditions (G4, FR-11) as a check before each step.

```python
def run_auto_fix(
    diag: CIDiagnosis,
    profile: str,
    config: AgentLoopConfig,
    sandbox_backend: str = "auto",
    dry_run: bool = False,
    draft_pr: bool = False,
) -> CIFixAttempt:
    """Execute the agentic fix loop for a given diagnosis.

    1. Creates a fix branch from HEAD.
    2. Constructs a fix prompt from the diagnosis (remediation_steps + affected_files).
    3. Invokes the TAG agentic loop (profile) with the ACI tool set.
    4. On each step, checks AgentLoopConfig stopping conditions.
    5. On success condition (step produces a git diff), runs sandbox verification.
    6. If sandbox exits 0: opens PR. If not: increments retry counter, continues loop.
    7. Returns a CIFixAttempt with the final status.
    """
    import time

    attempt = CIFixAttempt(
        diagnosis_id=diag.id,
        fix_branch=f"fix/ci-{diag.id}",
        profile=profile,
        max_steps=config.max_steps,
        max_cost_usd=config.max_cost_usd,
        timeout_seconds=config.max_wall_seconds,
        sandbox_backend=sandbox_backend,
    )

    if dry_run:
        attempt.status = "dry_run"
        attempt.stop_reason = "dry_run"
        return attempt

    # Create the fix branch
    _create_fix_branch(attempt.fix_branch)

    t0 = time.monotonic()

    # Build the initial agent prompt from the diagnosis
    fix_prompt = _build_fix_prompt(diag)

    # Agentic loop: delegate to the existing loop_agent machinery
    from tag.loop_agent import run_loop_step  # type: ignore
    agent_state: dict = {"prompt": fix_prompt, "history": [], "cost_usd": 0.0}

    while True:
        elapsed = time.monotonic() - t0
        stop = config.check_budget(attempt.steps_taken, attempt.cost_usd, elapsed)
        if stop:
            attempt.status = "budget_exceeded"
            attempt.stop_reason = stop
            _cleanup_fix_branch(attempt.fix_branch)
            break

        step_result = run_loop_step(agent_state, profile=profile, blocked=config.blocked_commands)
        attempt.steps_taken += 1
        attempt.cost_usd += step_result.get("cost_usd", 0.0)
        agent_state["history"].append(step_result)

        if step_result.get("is_complete"):
            # Verify in sandbox
            test_cmd = _infer_test_command(diag)
            exit_code, output = _run_sandbox_verification(test_cmd, sandbox_backend)
            attempt.sandbox_exit_code = exit_code
            attempt.sandbox_output = output[:4000]  # truncate for storage

            if exit_code == 0:
                attempt.status = "success"
                attempt.stop_reason = "success"
                if not dry_run:
                    pr_url, pr_number = _open_fix_pr(diag, attempt, draft=draft_pr)
                    attempt.pr_url = pr_url
                    attempt.pr_number = pr_number
                break
            else:
                # Sandbox failed: inject failure output into agent history and continue
                agent_state["prompt"] = (
                    f"The sandbox verification failed with exit code {exit_code}.\n"
                    f"Output:\n```\n{output[-2000:]}\n```\n"
                    f"Please revise the fix."
                )

        if step_result.get("is_error"):
            attempt.status = "failed"
            attempt.stop_reason = "error"
            attempt.error_message = step_result.get("error")
            _cleanup_fix_branch(attempt.fix_branch)
            break

    attempt.completed_at = _utc_now()
    return attempt


def _build_fix_prompt(diag: CIDiagnosis) -> str:
    """Build the initial agent prompt for the fix loop from a diagnosis."""
    files_section = "\n".join(
        f"  - {f.path}" + (f":{f.line_number}" if f.line_number else "") +
        (f" ({f.symbol})" if f.symbol else "")
        for f in diag.affected_files
    ) or "  (no specific files identified)"

    steps_section = "\n".join(
        f"  {i+1}. {step}" for i, step in enumerate(diag.remediation_steps)
    )

    return (
        f"A CI pipeline has failed with the following root cause analysis:\n\n"
        f"**Category:** {diag.root_cause_category.value}\n"
        f"**Primary Error:** {diag.primary_error_text}\n"
        f"**Confidence:** {diag.confidence:.0%}\n\n"
        f"**Affected Files:**\n{files_section}\n\n"
        f"**Recommended Remediation Steps:**\n{steps_section}\n\n"
        f"Please apply the fix to the codebase. After each file edit, run the linter. "
        f"When you believe the fix is complete, output DONE and nothing else."
    )


def _infer_test_command(diag: CIDiagnosis) -> str:
    """Infer a minimal test command from the diagnosis to use in sandbox verification."""
    if not diag.affected_files:
        return "python -m pytest"

    # Find test files that correspond to the affected source files
    test_files = []
    for fref in diag.affected_files:
        p = Path(fref.path)
        # Convention: tests/test_<module>.py or tests/<module>/test_*.py
        candidate = Path("tests") / f"test_{p.stem}.py"
        if candidate.exists():
            test_files.append(str(candidate))

    if test_files:
        return "python -m pytest " + " ".join(test_files)
    # Fall back to running the affected source file's test module
    return f"python -m pytest tests/ -k {diag.affected_files[0].path.replace('/', '.')}"
```

### 9.8 Fix PR body template

```python
_FIX_PR_BODY_TEMPLATE = """\
## CI Failure Auto-Fix

This PR was generated automatically by `tag ci diagnose --auto-fix` in response to a
failing CI run.

### Root Cause Analysis

| Field | Value |
|-------|-------|
| Run ID | `{run_id}` |
| Provider | {provider} |
| Category | `{category}` |
| Confidence | {confidence:.0%} |
| Diagnosis ID | `{diag_id}` |

**Primary Error:**
```
{primary_error}
```

**Affected Files:**
{affected_files}

**Remediation Applied:**
{remediation_steps}

### Local Verification

{verification_result}

### References

- [Original failing run]({run_url})
- Diagnosis: `{diag_id}`
- Fix attempt: `{fix_id}`

---
*Generated by [TAG](https://github.com/your-org/tag) — CI Failure Root-Cause Analysis (PRD-060)*
"""
```

### 9.9 Secret scanning integration (FR-04)

The existing `security.py` scanner is invoked on the raw log content before any LLM call:

```python
def redact_log_secrets(log_text: str) -> tuple[str, int]:
    """Apply PRD-034 secret scanning to CI log content before LLM submission.

    Returns:
        (redacted_text, count_of_redacted_patterns)
    """
    from tag.security import scan_for_secrets, redact_secrets  # type: ignore
    findings = scan_for_secrets(log_text)
    if not findings:
        return log_text, 0
    redacted = redact_log_secrets(log_text, findings)
    return redacted, len(findings)
```

### 9.10 Integration with tracing (PRD-013) and cost tracking (PRD-012)

Every call in the diagnosis + fix flow emits an OTel span using the existing `tracing.py` helpers:

```python
from tag.tracing import get_tracer
from tag.otel_semconv import TAG_CI_DIAG_ID, TAG_CI_FIX_ID  # new semconv constants

tracer = get_tracer("tag.ci.diagnose")

with tracer.start_as_current_span("ci.diagnose") as span:
    span.set_attribute("ci.repo", repo)
    span.set_attribute("ci.run_id", run_id)
    span.set_attribute("ci.provider", provider.value)
    diag = diagnose_from_log(windowed_log, repo, run_id, provider)
    span.set_attribute(TAG_CI_DIAG_ID, diag.id)
    span.set_attribute("ci.root_cause_category", diag.root_cause_category.value)
    span.set_attribute("ci.confidence", diag.confidence)
    span.set_attribute("gen_ai.usage.input_tokens", diag.prompt_tokens)
    span.set_attribute("gen_ai.usage.output_tokens", diag.completion_tokens)
    span.set_attribute("gen_ai.usage.cost_usd", diag.cost_usd)
```

---

## 10. Security Considerations

1. **Secret redaction is mandatory by default (FR-04).** CI logs frequently contain secrets: API tokens printed by misconfigured setup steps, `.env` file contents echoed by `cat`, AWS credentials in environment dumps, database passwords in connection strings. The PRD-034 secret scanner runs synchronously before any content is sent to an LLM provider. `--no-redact-secrets` is an explicit opt-out that prints a prominent warning to stderr and requires confirmation unless `--yes` is passed.

2. **The agentic fix loop's `blocked_commands` list prevents destructive operations.** The `AgentLoopConfig.blocked_commands` field (Section 9.3) blocks `git push`, `git push --force`, `rm -rf`, `curl`, `wget`, and dependency-install commands. This prevents a malicious or confused fix agent from pushing to the remote, exfiltrating data via curl, or introducing dependency changes without human review.

3. **Fix branches are local-only until the user confirms sandbox verification.** The fix branch is never pushed to the remote unless the sandbox exits 0 (or `--no-verify` is explicitly passed). The remote push and PR creation happen only as the final step. This means no code reaches the CI pipeline unless a human has reviewed the local verification result.

4. **GitLab tokens are read from the environment (`GITLAB_TOKEN`), not stored in TAG's config or SQLite.** GitHub access is via `gh` CLI, which manages its own token storage securely. Neither token appears in `ci_diagnoses` or `ci_fix_attempts` rows.

5. **The raw LLM response is stored in `ci_diagnoses.raw_llm_response` for audit.** This column may contain rephrased versions of log content (post-redaction). Operators running TAG in shared environments should consider the sensitivity of this column and may truncate it via a periodic cleanup job.

6. **`--max-diff-lines` in `AgentLoopConfig` (default: 500) prevents the fix agent from generating excessively large patches** that could obscure malicious changes among legitimate ones. Patches exceeding the limit are rejected and the attempt is marked `failed` with `stop_reason = "diff_too_large"`.

7. **Sandbox verification (PRD-028) runs the test command in a resource-isolated environment.** The sandbox backend (Docker by default when available) prevents the test suite from reading host credentials, spawning network connections outside the repository, or persisting state to the host filesystem.

8. **Log content sent to the LLM provider is subject to that provider's data processing terms.** Users diagnosing CI failures in repositories with proprietary code or regulated data should ensure their LLM provider agreements cover this use case, or use a locally-hosted model via TAG's provider configuration.

---

## 11. Testing Strategy

### 11.1 Unit tests (`tests/test_ci_diagnose.py`)

- `test_window_log_no_truncation` — logs under 1,000 lines are returned unchanged
- `test_window_log_preserves_head_and_tail` — head and tail windows are always present in windowed output
- `test_window_log_extracts_error_signals` — lines matching `_ERROR_PATTERNS` are extracted from middle section
- `test_failure_category_auto_fixable` — `CIFailureCategory.is_auto_fixable` returns correct values for all enum members
- `test_redact_log_secrets_replaces_patterns` — mock `security.scan_for_secrets` returns findings; verify replacement format
- `test_diagnose_from_log_parses_schema` — mock `anthropic.Anthropic.messages.create`; assert `CIDiagnosis` fields are populated correctly
- `test_diagnose_from_log_handles_unknown_category` — LLM returns unexpected category string; assert `UNKNOWN` fallback
- `test_agent_loop_config_check_budget` — each of the three stopping conditions triggers the correct `stop_reason`
- `test_build_fix_prompt_includes_remediation_steps` — fix prompt contains all steps from diagnosis
- `test_infer_test_command_with_test_files` — affected file `src/tag/auth.py` → `tests/test_auth.py` if it exists
- `test_infer_test_command_fallback` — no matching test file → `python -m pytest tests/`
- `test_parse_gh_run_log_output` — parses the interleaved multi-job output format from `gh run view --log-failed`

### 11.2 Integration tests (`tests/integration/test_ci_diagnose_integration.py`)

- `test_fetch_github_run_logs_real` — marked `@pytest.mark.integration`; requires `GITHUB_TOKEN` and a known public repo run ID; asserts non-empty dict returned
- `test_fetch_gitlab_job_logs_real` — marked `@pytest.mark.integration`; requires `GITLAB_TOKEN` and a known public project pipeline ID
- `test_fetch_last_failed_run_id` — calls `gh run list` on a repo known to have recent failures; asserts a numeric run ID is returned
- `test_diagnose_import_error_end_to_end` — uses a fixture log file (`tests/fixtures/ci_logs/import_error.log`) with a known `ModuleNotFoundError`; calls `diagnose_from_log` with a real LLM call (marked `@pytest.mark.llm`); asserts `root_cause_category == IMPORT_ERROR` and `confidence >= 0.80`
- `test_auto_fix_dry_run` — invokes `run_auto_fix(..., dry_run=True)` on a known fixable diagnosis; asserts `status == "dry_run"` and no git branch is created
- `test_auto_fix_budget_stop` — configures `AgentLoopConfig(max_steps=1)`; asserts the loop stops after 1 step with `stop_reason == "max_steps"`

### 11.3 Eval suite (`evals/ci_diagnose.yaml`)

Twenty labeled cases spanning all `CIFailureCategory` values, each with:
- `input.log_file` — path to a fixture CI log in `tests/fixtures/ci_logs/`
- `expected.root_cause_category` — ground-truth category label
- `expected.is_auto_fixable` — ground-truth fixability flag
- `threshold.confidence` — minimum required LLM confidence for the case to pass

The suite is runnable via `tag eval run --suite evals/ci_diagnose.yaml` (PRD-027). CI gating asserts category accuracy ≥ 85% across all 20 cases (Success Metric 2).

### 11.4 Performance tests

- Feed a 100,000-line synthetic log through `window_log()`; assert runtime < 500ms (Python `time.perf_counter`)
- Feed the windowed output of the same log through `redact_log_secrets()`; assert runtime < 2,000ms (NFR-07)
- Measure end-to-end `diagnose_from_log()` wall time on a 10,000-line log with a mocked LLM response; assert < 5,000ms excluding actual network latency

---

## 12. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|--------------|
| AC-01 | `tag ci diagnose --run-id <id> --repo owner/repo` completes without error and prints a structured RCA to the terminal for a real failing GitHub Actions run | Manual test against a known failing public repo run |
| AC-02 | `tag ci diagnose --last-failed` auto-detects the repo from `git remote get-url origin` and returns the correct run ID | Unit test + manual test in a git repo |
| AC-03 | Logs exceeding 1,000 lines are windowed; the output contains a `[... middle section ... lines omitted ...]` marker | `test_window_log_preserves_head_and_tail` |
| AC-04 | A CI log containing a `GITHUB_TOKEN=ghp_abc123` string has the token replaced with `[REDACTED:github_pat]` before the LLM call | `test_redact_log_secrets_replaces_patterns` |
| AC-05 | `--json` output is valid JSON parseable by `json.loads()` and contains all fields of the `CIDiagnosis` dataclass | `test_diagnose_from_log_parses_schema` |
| AC-06 | `tag ci diagnose --run-id <id> --auto-fix --profile coder --max-cost-usd 0.01` stops with `status == "budget_exceeded"` and `stop_reason == "max_cost"` after the first LLM call exceeds the budget | `test_agent_loop_config_check_budget` + integration test |
| AC-07 | `--auto-fix` on a diagnosis with `is_auto_fixable == False` exits immediately with a user-friendly message and does not create a branch | `test_auto_fix_not_fixable_categories` |
| AC-08 | `--auto-fix` creates a git branch `fix/ci-diag-<id>` and the branch exists after the command exits (even on failure) | Integration test |
| AC-09 | The opened fix PR body contains the diagnosis ID, the root cause category, the primary error text, and the local verification result | Manual inspection of a test PR in a sandboxed repo |
| AC-10 | `tag ci diagnose` writes one row to `ci_diagnoses` table in `~/.tag/runtime/tag.sqlite3` after every successful diagnosis | `test_diagnose_persists_to_sqlite` |
| AC-11 | `--auto-fix` writes one row to `ci_fix_attempts` table with correct `diagnosis_id` foreign key | `test_auto_fix_persists_to_sqlite` |
| AC-12 | `tag ci history --last 5` returns the 5 most recent diagnoses in descending `created_at` order | Unit test querying a fixture SQLite database |
| AC-13 | `--provider gitlab --project-id 42` correctly calls the GitLab Jobs API and returns non-empty log content for a failing pipeline | Integration test with a real GitLab project (marked `@pytest.mark.integration`) |
| AC-14 | `--dry-run` with `--auto-fix` prints the diagnosis and the proposed fix prompt, but makes zero git branch changes and zero PR API calls | `test_auto_fix_dry_run` |
| AC-15 | Category accuracy on the 20-case eval suite (`evals/ci_diagnose.yaml`) is ≥ 85% | `tag eval run --suite evals/ci_diagnose.yaml` exits 0 |

---

## 13. Dependencies

| Dependency | Type | Used for | Already in TAG? |
|------------|------|----------|-----------------|
| `gh` CLI | External binary | GitHub Actions log fetch, PR creation | Yes (used in `ci.py`) |
| `anthropic` Python SDK | Runtime | LLM diagnosis call (JSON mode) | Yes |
| `httpx` | Runtime (optional extra) | GitLab REST API calls | Yes (optional) |
| PRD-028 sandbox | Internal module | Sandbox verification of fix | Implemented |
| PRD-034 secret scanning | Internal module | Log redaction before LLM call | Implemented |
| PRD-013 tracing | Internal module | OTel spans for diagnosis + fix loop | Implemented |
| PRD-012 cost tracking | Internal module | Per-call cost attribution | Implemented |
| PRD-039 token budget | Internal module | `--max-cost-usd` enforcement | Implemented |
| PRD-027 eval framework | Internal module | `evals/ci_diagnose.yaml` acceptance suite | Implemented |
| `GITHUB_TOKEN` env var | Credential | `gh` CLI authentication | Standard |
| `GITLAB_TOKEN` env var | Credential | GitLab API authentication | New (documented) |
| `loop_agent.py` | Internal module | Agentic fix loop step execution | Implemented |

---

## 14. Open Questions

| # | Question | Owner | Resolution target |
|---|----------|-------|-------------------|
| OQ-1 | Should `--last-failed` scope to only workflow runs triggered by the current HEAD commit, or any recent failure on the branch? The current design uses "most recent failure on branch" which may return a stale run if the developer has pushed since. | Product | Before implementation start |
| OQ-2 | The `max_diff_lines` guard (Section 9.3) rejects fixes larger than 500 changed lines. Is this the right default for monorepos where a "fix" might require touching many generated files? Should this be profile-configurable? | Engineering | During implementation |
| OQ-3 | `ci_diagnoses.raw_llm_response` stores the full LLM output including rephrased log content. For shared-machine deployments, should this column be encrypted at rest? TAG's current SQLite is not encrypted. | Security | Before GA |
| OQ-4 | The `--no-verify` flag skips sandbox verification entirely. Is this too permissive for enterprise deployments? Should it require a `--force` or `ALLOW_UNVERIFIED_FIX=1` env var? | Security | Before GA |
| OQ-5 | GitLab CI pipeline IDs are numeric; GitHub Actions run IDs are also numeric but from a different namespace. The `run_id` column in `ci_diagnoses` is TEXT to accommodate both. Should the table include a `(provider, run_id)` unique constraint, or is it valid to diagnose the same run multiple times? | Engineering | During implementation |
| OQ-6 | Should the fix PR be labeled with the root cause category (e.g., `ci-fix/import-error`) in addition to `tag-auto-fix`? This would enable filtering in GitHub's PR list view. | Product | After initial implementation |
| OQ-7 | The eval suite (`evals/ci_diagnose.yaml`) requires 20 real CI log fixtures. Where do these come from — synthetic construction, scraping public GitHub Actions logs, or contributor-donated anonymized logs? | DevRel | Before eval suite authoring |
| OQ-8 | When `--auto-fix` loop terminates with `sandbox_exit_code != 0` after exhausting `max_steps`, should the partial fix branch be pushed as a draft PR for human review, or discarded entirely? The current design discards it (calls `_cleanup_fix_branch`). | Product | Before implementation |

---

## 15. Complexity and Timeline

**Total estimated effort: M — 8-10 engineering days**

### Phase 1: Log Fetching and Windowing (Days 1–2)

- Implement `fetch_github_run_logs()` and `fetch_last_failed_run_id()` in `ci.py`
- Implement `fetch_gitlab_job_logs()` with `httpx` (gated on `GITLAB_TOKEN`)
- Implement `window_log()` with ACI-inspired head/error-signal/tail pattern
- Implement `log_sha256()` and `_parse_gh_run_log_output()`
- Unit tests for all log-fetching and windowing functions
- Fixture CI log files in `tests/fixtures/ci_logs/` (5 files covering common failure types)

### Phase 2: LLM Diagnosis and SQLite Persistence (Days 3–4)

- Implement `CIDiagnosis`, `FileRef`, `CIFailureCategory` dataclasses in `ci.py`
- Implement `redact_log_secrets()` integration with PRD-034 `security.py`
- Implement `_DIAGNOSE_STRUCTURED_SYSTEM` system prompt
- Implement `diagnose_from_log()` with JSON-mode LLM call and schema parsing
- Add `ci_diagnoses` and `ci_fix_attempts` table DDL to `open_db()` migration block
- Implement `persist_diagnosis()` and `persist_fix_attempt()` helpers
- Unit tests for diagnosis parsing, secret redaction, and SQLite persistence

### Phase 3: Controller Command and CLI Surface (Day 5)

- Add `cmd_ci_diagnose()` and `cmd_ci_history()` to `controller.py`
- Wire to existing `tag ci` Click group
- Implement cost-estimate prompt (pre-LLM confirmation, suppressed by `--yes` / `CI`)
- Implement `--json` / `--output` modes
- Implement auto-detection of `--repo` from `git remote get-url origin`
- Manual end-to-end smoke test against a known failing public GitHub Actions run

### Phase 4: Agentic Fix Loop (Days 6–7)

- Implement `AgentLoopConfig` with `check_budget()` and `blocked_commands`
- Implement `run_auto_fix()` orchestrating branch creation, loop, sandbox, PR open
- Implement `_build_fix_prompt()`, `_infer_test_command()`, `_open_fix_pr()`
- Implement `_FIX_PR_BODY_TEMPLATE` rendering
- Implement OTel span emission for each fix loop step (PRD-013)
- Integration test: `test_auto_fix_dry_run`, `test_auto_fix_budget_stop`

### Phase 5: Eval Suite and Acceptance Tests (Days 8–9)

- Author `evals/ci_diagnose.yaml` with 20 labeled cases
- Create fixture log files for all remaining `CIFailureCategory` values
- Run `tag eval run --suite evals/ci_diagnose.yaml` and iterate on system prompt until category accuracy ≥ 85%
- Execute full acceptance criteria checklist (AC-01 through AC-15)
- Resolve any OQ-1, OQ-5 open questions that have blocking answers

### Phase 6: Documentation and Hardening (Day 10)

- Add `tag ci diagnose` entry to `docs/cli-reference.md`
- Add `GITLAB_TOKEN` to `docs/configuration.md` credential reference
- Verify `--no-redact-secrets` warning is prominent and requires confirmation
- Performance test: 100,000-line log through windowing + redaction pipeline
- Address any P0 issues surfaced in Phase 5

---

*GitHub Issue: #344*
*PRD authors: TAG core team*
*Last updated: 2026-06-17*
