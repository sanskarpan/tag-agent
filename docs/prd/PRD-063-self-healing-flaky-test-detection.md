# PRD-063: Self-Healing Flaky Test Detection (`tag ci flaky-fix`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** L (2-4 weeks)
**Category:** CI/CD & Agentic Dev Workflows
**Affects:** `ci.py`
**Depends on:** PRD-013 (agent tracing/observability), PRD-021 (agent loop/autonomous mode), PRD-027 (eval framework), PRD-028 (sandbox code execution), PRD-033 (dependency-aware task queue), PRD-034 (secret scanning), PRD-038 (diff-aware context injection), PRD-039 (token budget enforcement), PRD-041 (OTel GenAI span cost attribution), PRD-055 (issue-to-PR autonomous loop)
**Inspired by:** BuildPulse, Trunk Flaky Tests, TestRail flaky detection
**GitHub Issue:** #344

---

## 1. Overview

Flaky tests are one of the most corrosive problems in modern software engineering. A test is flaky when it produces different pass/fail outcomes across multiple runs against the same code — typically due to time dependencies, race conditions, external network calls, random seeds, ordering assumptions, or shared state leakage between test cases. Flaky tests erode developer trust in the CI pipeline: when engineers know certain tests sometimes fail for no code-related reason, they begin ignoring CI failures entirely, which allows real regressions to slip through undetected. BuildPulse reports that engineering teams lose an average of 4.3 engineering hours per week per developer to flaky test investigation. Trunk Flaky Tests estimates that 25-50% of CI failures in mature codebases are caused by fewer than 5% of tests.

TAG already provides CI integration via `ci.py` (PRD-020), a sandboxed execution environment via `sandbox.py` (PRD-028), and a bounded agentic loop via `loop_agent.py` (PRD-021). `tag ci flaky-fix` synthesizes these primitives into an end-to-end self-healing pipeline: it runs a test suite multiple times against an unchanged codebase, applies statistical analysis to identify which tests exhibit non-deterministic outcomes, and then spawns a bounded agent for each flaky test to diagnose the root cause and propose (or automatically apply) a fix. The result is a structured report that can be committed to the repository, used to open GitHub issues, or applied automatically as a pull request — closing the loop between detection and remediation.

The detection engine is grounded in sound statistical practice drawn from the cluster research context: a minimum sample size of at least 3 runs (configurable, default 5), a flip signal based on outcome variation without code changes, and a disruption percentage threshold (configurable, default 30% — meaning a test that fails in fewer than 30% of runs is still investigated but flagged differently than one that fails in 70% of runs). Tests exceeding the quarantine threshold (default 50% failure rate) are emitted with a `quarantine: true` flag, which CI matrix configurations can use to exclude them from blocking gates. Tests that have been quarantined can be promoted back via a `--de-quarantine` check once they pass a sustained health window.

The remediation agent for each detected flaky test uses the ACI (Agent-Computer Interface) tool harness pattern — windowed file viewer, line-targeted edit with lint validation, structured search — rather than raw bash, consistent with SWE-agent research showing roughly 2x improvement on code editing tasks with this approach. Remediation is subject to the same three mandatory stopping conditions that govern all TAG agentic loops: success (the test passes deterministically across a validation run set), failure (unrecoverable error or model infeasibility), and budget (configurable ceiling on turns, wall-clock time, and USD cost). All execution is persisted to the `flaky_runs`, `flaky_tests`, and `flaky_fix_attempts` tables in the existing SQLite store at `~/.tag/runtime/tag.sqlite3`, using WAL mode for concurrent read access.

---

## 2. Problem Statement

### 2.1 No Structured Flakiness Signal Exists in TAG Today

TAG's existing `ci.py` can fetch CI logs, diagnose CI failures, and generate PR reviews — but it has no model of test flakiness as a first-class concept. When a test fails intermittently in CI, the current workflow requires a developer to manually identify the pattern by scrolling through CI run history, determine whether the failure is code-related or environmental, and then debug it manually. There is no command in TAG that can distinguish a deterministic failure (code is broken) from a non-deterministic one (test is flaky), and there is no automated remediation path. Developers who use TAG for CI assistance have a gap between "CI failed" and "flaky test detected and fixed".

### 2.2 Manual Flaky Test Investigation Is Expensive and Inconsistent

When a developer suspects a flaky test, the typical investigation is: re-run CI manually 2-3 times, observe whether the failure recurs, grep the test file for `time.sleep`, `datetime.now()`, `random`, or network calls, try to reproduce locally, and ultimately apply a heuristic fix without certainty about root cause. This process is inconsistent across engineers (senior engineers have developed pattern recognition; junior engineers burn hours), does not produce a documented artifact of what was found, and does not feed back into any test health database that would allow tracking whether the fix was durable. The entire effort is invisible to the rest of the team until the flaky test causes a CI failure that blocks a merge.

### 2.3 No Quarantine-Aware Test Health Tracking Exists

Flaky tests that cannot be immediately fixed should be quarantined — excluded from blocking CI gates — but still tracked. In practice, teams either delete flaky tests (losing coverage) or leave them in the blocking gate (causing noise). There is no mechanism in TAG to record which tests have been quarantined, why, when the quarantine was applied, and whether the test has since recovered. Without this audit trail, quarantines persist indefinitely and accumulate until the non-blocking test suite becomes so large that it provides no signal at all. A proper quarantine system requires a health database, a de-quarantine policy, and automated re-evaluation — none of which TAG currently provides.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | `tag ci flaky-fix --repo owner/repo --runs N` detects flaky tests by running the test suite N times against the same commit and computing per-test disruption percentages. |
| G2 | `tag ci flaky-fix --file tests/test_auth.py --runs 3 --auto-fix` spawns a bounded remediation agent for each detected flaky test and applies the fix as a commit. |
| G3 | `tag ci flaky-fix --last-ci-run --repo owner/repo` ingests the last N CI workflow run artifacts (JUnit XML) from GitHub Actions instead of re-running tests locally, avoiding redundant compute. |
| G4 | The detection engine requires minimum sample size, computes disruption percentage per test, applies flip signal detection, and emits quarantine flags for tests exceeding the quarantine threshold (default 50%). |
| G5 | Each remediation agent enforces all three mandatory stopping conditions: success (test passes deterministically in a validation rerun), failure (unrecoverable error), budget (configurable max turns, wall seconds, and USD cost). |
| G6 | All remediation agents use the ACI tool harness (windowed file viewer, line-targeted edit with lint, structured search) rather than raw bash for code editing tasks. |
| G7 | Every detection run and fix attempt is persisted to SQLite (`flaky_runs`, `flaky_tests`, `flaky_fix_attempts`) for audit, trending, and de-quarantine tracking. |
| G8 | `tag ci flaky-fix --report` generates a Markdown or JSON flakiness report that can be committed to the repo or posted as a PR comment. |
| G9 | OpenTelemetry spans are emitted for the detection phase, each agent invocation, and each validation rerun via `tracing.py` (PRD-013), with per-phase cost attribution via PRD-041. |
| G10 | `tag ci flaky-fix --de-quarantine` re-evaluates quarantined tests and promotes them back to the blocking gate if they pass a configurable sustained health window (default: pass rate ≥ 90% over last 20 recorded runs). |
| G11 | `--dry-run` mode prints the detected flaky tests and proposed agent tasks without running any remediation agents or writing any code. |
| G12 | JUnit XML ingestion supports both local file paths and GitHub Actions artifact download via the `gh` CLI. |

## 3.1 Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Full CI system integration as a hosted service. `tag ci flaky-fix` runs on the developer's local machine or a CI runner; there is no cloud backend. |
| NG2 | Flakiness detection via test result history from third-party platforms (TestRail, Allure, Sentry). Only JUnit XML artifacts and local re-runs are supported in v1. |
| NG3 | Automatic merge of remediation PRs. The fix loop ends at a committed patch or open PR; merge requires human approval. |
| NG4 | Detection of performance flakiness (tests that pass/fail based on timing thresholds). Only pass/fail outcome flakiness is in scope. |
| NG5 | Support for non-Python test runners that do not emit JUnit XML (e.g., Go `testing`, Rust `cargo test` in non-JUnit mode). JUnit XML adapters for those ecosystems are out of scope for v1. |
| NG6 | Automatic Linear/Jira ticket creation for each flaky test. Ticket creation is a stretch goal noted in Open Questions. |
| NG7 | Flakiness detection for tests that require a live database or external service (integration tests requiring real infrastructure). The sandbox environment must be self-contained. |
| NG8 | Running the detection suite on a remote CI runner. All test execution is local or via GitHub Actions artifact download; remote execution orchestration is deferred. |

---

## 4. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|-------------------|
| Detection accuracy (precision) | ≥ 90% of tests flagged as flaky are confirmed flaky by manual review | Manual audit of 20 flagged tests per release |
| Detection accuracy (recall) | ≥ 75% of known flaky tests in a benchmark repo are detected at `--runs 5` | Automated benchmark with injected flaky tests |
| Time to first flaky report | `tag ci flaky-fix --runs 5` on a 500-test suite completes within 10 minutes on a 4-core machine | Timing test in CI |
| Remediation success rate | Remediation agent produces a passing, lint-clean patch for ≥ 40% of detected flaky tests without human intervention | Measured across 20 internal test cases |
| SQLite write latency | Each test run result (JUnit XML parse + DB write) completes in < 100ms | Unit benchmark |
| Agent budget adherence | Zero remediation agents exceed their configured cost or turn budget | Integration test with budget-limited mock agent |
| Quarantine accuracy | De-quarantine promotion is triggered only when pass rate ≥ 90% over last 20 runs | Unit test against synthetic history |
| CLI startup time | `tag ci flaky-fix --help` responds in < 300ms | Timing assertion in CI |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Backend engineer | run `tag ci flaky-fix --runs 5` in my repo | I can quickly identify which tests are flaky without manually re-running CI five times and diffing results |
| U2 | Platform engineer | run `tag ci flaky-fix --last-ci-run --repo owner/repo` | I can analyze flakiness from CI artifacts already produced without spending extra compute on local re-runs |
| U3 | Developer | run `tag ci flaky-fix --file tests/test_auth.py --runs 3 --auto-fix` | I can fix the flaky tests in a specific file without investigating root cause manually, letting the agent diagnose and patch |
| U4 | Tech lead | run `tag ci flaky-fix --report --format markdown` and paste the output into a PR description | My team understands which tests are unreliable before the PR is reviewed |
| U5 | DevOps engineer | have quarantined tests listed in a machine-readable JSON file | I can configure my CI matrix to skip them in the blocking gate while keeping them in a non-blocking advisory suite |
| U6 | Developer | run `tag ci flaky-fix --de-quarantine` weekly | Flaky tests that have been fixed are automatically re-admitted to the blocking suite without manual bookkeeping |
| U7 | Team lead | see the flakiness trend for `tests/test_auth.py::test_login_timeout` over the last 30 days via `tag ci flaky-fix --history test_login_timeout` | I know whether a fix we applied 2 weeks ago is holding |
| U8 | Developer | run `tag ci flaky-fix --dry-run` before committing | I can see which tests would be investigated and what agent tasks would be spawned without paying for LLM calls |
| U9 | Developer | configure `--max-cost 2.00` to cap remediation spend | I get the best fixes the agent can find within my cost budget, with no surprise API bills |
| U10 | Platform engineer | receive an OTel span for each phase of `tag ci flaky-fix` | I can observe latency and cost breakdowns in my existing Jaeger/Grafana stack |

---

## 6. Proposed CLI Surface

All `tag ci flaky-fix` subcommands extend the existing `tag ci` namespace defined in `ci.py`. The primary entry point is `cmd_ci_flaky_fix` in `controller.py`.

### 6.1 Core Detection Command

```
tag ci flaky-fix \
  --repo owner/repo \
  --runs 5 \
  [--file tests/test_auth.py] \
  [--test TEST_NODE_ID] \
  [--runner "pytest -x"] \
  [--threshold 0.30] \
  [--quarantine-threshold 0.50] \
  [--format table|json|markdown] \
  [--output flaky-report.json] \
  [--dry-run] \
  [--json]
```

**Options:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--repo` | str | CWD git remote | GitHub `owner/repo` used for artifact download in `--last-ci-run` mode |
| `--runs` | int | 5 | Number of times to run the test suite for detection |
| `--file` | path | all tests | Restrict detection to a single test file |
| `--test` | str | all | Restrict detection to a single pytest node ID (e.g. `tests/test_auth.py::test_login`) |
| `--runner` | str | `pytest` | Shell command prefix used to invoke the test suite |
| `--threshold` | float | 0.30 | Minimum disruption percentage to classify a test as flaky |
| `--quarantine-threshold` | float | 0.50 | Disruption percentage above which `quarantine: true` is set in the report |
| `--format` | choice | `table` | Output format: `table` (Rich), `json`, or `markdown` |
| `--output` | path | none | Write report to this file in addition to stdout |
| `--dry-run` | flag | false | Print what would be done without running tests or agents |
| `--json` | flag | false | Force JSON output (equivalent to `--format json`) |

**Example output (table format):**
```
TAG Flaky Test Detection  runs=5  repo=acme/backend  commit=a1b2c3d

  Test                                       Runs  Pass  Fail  Disruption  Quarantine
  ─────────────────────────────────────────────────────────────────────────────────────
  tests/test_auth.py::test_login_timeout        5     3     2       40.0%       false
  tests/test_db.py::test_connection_pool        5     1     4       80.0%        true
  tests/test_jobs.py::test_retry_backoff        5     4     1       20.0%       false

  3 flaky tests detected (1 quarantined). Run with --auto-fix to remediate.
```

### 6.2 Remediation Command

```
tag ci flaky-fix \
  --file tests/test_auth.py \
  --runs 3 \
  --auto-fix \
  [--profile flaky-fixer] \
  [--max-turns 20] \
  [--max-cost 5.00] \
  [--max-seconds 300] \
  [--sandbox docker|none] \
  [--branch flaky-fix/{test-slug}] \
  [--open-pr] \
  [--validation-runs 3] \
  [--json]
```

**Additional flags for remediation:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--auto-fix` | flag | false | Spawn remediation agent for each detected flaky test |
| `--profile` | str | `default` | TAG profile to use for the remediation agent |
| `--max-turns` | int | 20 | Maximum agent turns per flaky test |
| `--max-cost` | float | 5.00 | Maximum USD cost per flaky test remediation |
| `--max-seconds` | int | 300 | Maximum wall-clock seconds per remediation agent |
| `--sandbox` | choice | `none` | Sandbox mode for test execution: `docker` or `none` |
| `--branch` | str | `flaky-fix/{slug}` | Git branch name pattern for fix commits |
| `--open-pr` | flag | false | Open a GitHub PR for each successful fix |
| `--validation-runs` | int | 3 | Number of re-runs to confirm fix is deterministic |

**Example output during remediation:**
```
[1/3] Fixing: tests/test_auth.py::test_login_timeout  (disruption=40%)
  Turn 1  open tests/test_auth.py:45
  Turn 2  Identified: datetime.now() used in assertion without timezone normalization
  Turn 3  edit 47:51 — replace with datetime.now(timezone.utc)
  Turn 4  Running validation (3 runs)...  PASS PASS PASS
  Fix applied. Branch: flaky-fix/test-login-timeout

[2/3] Fixing: tests/test_db.py::test_connection_pool  (disruption=80%, quarantined)
  Turn 1  open tests/test_db.py:102
  Turn 8  Root cause: race condition in pool.close() — requires architectural change
  Turn 8  Budget: 4/20 turns, $0.34/$5.00  FAILED (infeasibility)
  Patch saved to .tag/patches/test_connection_pool.diff — review manually.

[3/3] Fixing: tests/test_jobs.py::test_retry_backoff  (disruption=20%)
  Turn 2  Identified: time.sleep(0.1) in test is too tight for CI runner jitter
  Turn 3  edit 88:88 — replace with pytest-timeout + mock time
  Turn 4  Running validation (3 runs)...  PASS PASS PASS
  Fix applied. Branch: flaky-fix/test-retry-backoff

Summary: 2/3 fixes applied, 1 failed (saved as patch). Total cost: $0.87
```

### 6.3 Last-CI-Run Mode

```
tag ci flaky-fix \
  --last-ci-run \
  --repo owner/repo \
  [--workflow-name "CI"] \
  [--artifact-name "test-results"] \
  [--runs 10] \
  [--auto-fix] \
  [--format table|json|markdown]
```

This mode downloads JUnit XML artifacts from the last N GitHub Actions workflow runs (not re-runs of the current commit) and ingests them for flakiness analysis without triggering any new test execution.

**Example:**
```
tag ci flaky-fix --last-ci-run --repo acme/backend --runs 10

Fetching last 10 runs of workflow "CI" from acme/backend...
  Run #1842  commit a1b2c3d  2026-06-15 14:22  passed (312 tests)
  Run #1841  commit a1b2c3d  2026-06-15 11:05  failed (310/312 tests)
  ...
  Run #1833  commit 9f8e7d6  2026-06-14 09:01  passed (312 tests)

Analyzing 312 tests across 10 runs...
  test_login_timeout: passed 7/10  (disruption=30%)
  test_connection_pool: passed 2/10  (disruption=80%, quarantined)
```

### 6.4 Historical Trend Command

```
tag ci flaky-fix --history [TEST_NODE_ID] \
  [--last 30] \
  [--format table|json|spark]
```

### 6.5 De-quarantine Command

```
tag ci flaky-fix --de-quarantine \
  [--health-window 20] \
  [--pass-rate 0.90] \
  [--dry-run]
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | `tag ci flaky-fix --runs N` MUST execute the test runner command exactly N times against the same working tree without any code modifications between runs. |
| FR-02 | Each test run MUST produce a JUnit XML file; if the test runner does not emit one by default, TAG MUST append `--junitxml=.tag/junit/{run_index}.xml` to the pytest invocation. |
| FR-03 | JUnit XML MUST be parsed using `xml.etree.ElementTree` and stored in the `test_run_results` table with columns `(flaky_run_id, run_index, test_id, outcome, duration_ms, error_message)`. |
| FR-04 | A test MUST be classified as flaky if and only if its outcome varies (at least one pass AND at least one failure) across the N runs, subject to the `--threshold` disruption percentage filter. |
| FR-05 | Disruption percentage MUST be computed as `failures / total_runs * 100`; tests with disruption below `--threshold` (default 30%) MUST be excluded from the flaky set even if they show outcome variation. |
| FR-06 | Tests with disruption >= `--quarantine-threshold` (default 50%) MUST have `quarantine=true` set in their `flaky_tests` row and in all report outputs. |
| FR-07 | In `--last-ci-run` mode, JUnit XML artifacts MUST be downloaded via `gh run download` from the GitHub Actions API; the `--workflow-name` and `--artifact-name` flags control which workflow and artifact name pattern to target. |
| FR-08 | Each remediation agent MUST receive as its initial context: the test file content (windowed view), the failing test function(s), all failure error messages and tracebacks from the detection runs, and the relevant module imports. |
| FR-09 | The remediation agent MUST use the ACI tool set: `aci_open(file, lineno)`, `aci_scroll(direction)`, `aci_goto(lineno)`, `aci_edit(start, end, replacement)`, `aci_search(pattern, path)` — NOT raw subprocess bash for file editing. |
| FR-10 | After each `aci_edit` call, a lint validation step (flake8 or ruff) MUST run on the modified file; if lint fails, the edit MUST be rejected and the agent notified of the lint error before its next turn. |
| FR-11 | Every remediation agent MUST enforce three stopping conditions: (a) success — test passes in all `--validation-runs` reruns; (b) failure — agent declares infeasibility or hits an unrecoverable error; (c) budget — any of `--max-turns`, `--max-cost`, or `--max-seconds` is reached. |
| FR-12 | On budget exhaustion, the agent MUST write its current patch (even if incomplete) to `.tag/patches/{test_slug}.diff` and emit a structured log entry recording the reason for termination. |
| FR-13 | Successful fixes MUST be committed to the git branch specified by `--branch` (default: `flaky-fix/{test-slug}`); the commit message MUST include the test node ID, disruption percentage, and root cause summary. |
| FR-14 | `--open-pr` MUST call `gh pr create` with a body that includes the flakiness report section for the fixed test, the agent's root cause diagnosis, and a link to the `flaky_fix_attempts` row ID. |
| FR-15 | All detection runs and fix attempts MUST be persisted to SQLite using the DDL defined in Section 9; no data may be written only to stdout. |
| FR-16 | `tag ci flaky-fix --de-quarantine` MUST query `flaky_tests` for rows with `quarantine=true`, compute the pass rate across the last `--health-window` (default 20) recorded runs in `test_run_results`, and update `quarantine=false` for tests meeting the `--pass-rate` threshold. |
| FR-17 | `--dry-run` MUST print all detection results and planned agent tasks to stdout and exit 0 without writing to SQLite, executing any test runs, or invoking any agents. |
| FR-18 | `--format json` MUST emit a JSON object conforming to the `FlakyReport` schema defined in Section 9; the schema MUST be stable across minor TAG versions. |
| FR-19 | OTel spans MUST be emitted for: `flaky_fix.detect` (covering all test runs), `flaky_fix.parse_junit` (per-run), `flaky_fix.agent.{test_slug}` (per remediation agent), and `flaky_fix.validate` (per validation rerun). |
| FR-20 | The command MUST exit 0 when no flaky tests are found, exit 1 on unrecoverable errors, and exit 2 when flaky tests are detected but no `--auto-fix` was requested. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | JUnit XML parsing for a 1,000-test suite MUST complete in < 500ms per file. |
| NFR-02 | The detection loop (N=5 runs, 500 tests) MUST not increase peak memory beyond 200MB on the TAG process; JUnit XML files are processed and discarded, not held in memory simultaneously. |
| NFR-03 | SQLite writes MUST use WAL mode (already enforced by `open_db()`); concurrent `tag ci flaky-fix --history` queries MUST not block detection run writes. |
| NFR-04 | Remediation agents MUST be isolated from one another; a crash or hang in one agent MUST NOT affect other agents or the detection state. Each agent runs in its own subprocess. |
| NFR-05 | The `--sandbox docker` mode MUST prevent test runs from writing outside the repository directory or making network calls to non-approved hosts. |
| NFR-06 | All JUnit XML parsing MUST use `defusedxml` or equivalent safe parser to prevent XML entity expansion attacks on CI-produced artifacts. |
| NFR-07 | The `aci_edit` function MUST produce a `git diff`-compatible patch for every change, enabling full rollback via `git checkout` if the fix is rejected. |
| NFR-08 | `tag ci flaky-fix --runs 5` on a zero-flaky suite MUST emit exit code 0 and produce no SQLite writes to `flaky_tests` (only the `flaky_runs` header row). |
| NFR-09 | Secrets present in test output (e.g., API keys in error messages) MUST be masked by `security.py` before being included in the agent context or written to SQLite. |
| NFR-10 | The `--last-ci-run` GitHub API calls MUST respect `GITHUB_TOKEN` from the environment and fall back to the `gh` CLI auth token; no token MUST be stored in the TAG config for this feature. |
| NFR-11 | All timestamps stored in SQLite MUST use ISO-8601 UTC format (e.g., `2026-06-17T10:23:45.123Z`); no local-time storage. |
| NFR-12 | The feature MUST function without network access in `--file` or `--test` mode; network calls are only made in `--last-ci-run` mode and `--open-pr` mode. |

---

## 9. Technical Design

### 9.1 New Files

| File | Purpose |
|------|---------|
| `src/tag/flaky_detector.py` | Core detection engine: test runner invocation, JUnit XML ingestion, statistical analysis, quarantine logic |
| `src/tag/aci_tools.py` | ACI tool harness: `aci_open`, `aci_scroll`, `aci_goto`, `aci_edit`, `aci_search` with lint validation |
| `src/tag/flaky_agent.py` | Remediation agent orchestration: context assembly, agent loop, validation rerun, patch writing |

`controller.py` gains `cmd_ci_flaky_fix()`. `ci.py` gains helper functions for JUnit XML ingestion and GitHub Actions artifact download.

### 9.2 SQLite DDL

```sql
-- WAL mode is already enforced by open_db(); these tables follow existing conventions.

CREATE TABLE IF NOT EXISTS flaky_runs (
  id              TEXT PRIMARY KEY,          -- UUID, e.g. "fr-20260617-abc123"
  repo            TEXT NOT NULL,             -- "owner/repo" or local path
  commit_sha      TEXT NOT NULL,             -- git rev-parse HEAD at detection time
  runner_cmd      TEXT NOT NULL,             -- full command used, e.g. "pytest -x --junitxml=..."
  num_runs        INTEGER NOT NULL,          -- N from --runs
  threshold       REAL NOT NULL DEFAULT 0.30,
  quarantine_thr  REAL NOT NULL DEFAULT 0.50,
  source          TEXT NOT NULL DEFAULT 'local',  -- 'local' | 'github_actions'
  status          TEXT NOT NULL DEFAULT 'running',-- 'running' | 'completed' | 'failed'
  created_at      TEXT NOT NULL,
  completed_at    TEXT
);
CREATE INDEX IF NOT EXISTS idx_fr_repo_commit ON flaky_runs(repo, commit_sha, created_at);

CREATE TABLE IF NOT EXISTS test_run_results (
  id              TEXT PRIMARY KEY,
  flaky_run_id    TEXT NOT NULL REFERENCES flaky_runs(id),
  run_index       INTEGER NOT NULL,          -- 0-based index within this flaky_run
  test_id         TEXT NOT NULL,             -- pytest node ID, e.g. "tests/test_auth.py::test_login"
  test_file       TEXT NOT NULL,
  test_class      TEXT,
  test_name       TEXT NOT NULL,
  outcome         TEXT NOT NULL,             -- 'passed' | 'failed' | 'error' | 'skipped'
  duration_ms     INTEGER,
  error_type      TEXT,                      -- exception class name if failed
  error_message   TEXT,                      -- truncated to 2000 chars
  traceback       TEXT,                      -- truncated to 4000 chars
  recorded_at     TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_trr_run ON test_run_results(flaky_run_id, run_index);
CREATE INDEX IF NOT EXISTS idx_trr_test ON test_run_results(test_id, recorded_at);

CREATE TABLE IF NOT EXISTS flaky_tests (
  id                  TEXT PRIMARY KEY,
  flaky_run_id        TEXT NOT NULL REFERENCES flaky_runs(id),
  test_id             TEXT NOT NULL,
  test_file           TEXT NOT NULL,
  test_name           TEXT NOT NULL,
  disruption_pct      REAL NOT NULL,         -- failures / total_runs * 100
  pass_count          INTEGER NOT NULL,
  fail_count          INTEGER NOT NULL,
  error_count         INTEGER NOT NULL,
  quarantine          INTEGER NOT NULL DEFAULT 0,  -- BOOLEAN: 1 if quarantine_thr exceeded
  first_seen_at       TEXT NOT NULL,
  last_seen_at        TEXT NOT NULL,
  resolved_at         TEXT,                  -- set when de-quarantine succeeds
  UNIQUE(flaky_run_id, test_id)
);
CREATE INDEX IF NOT EXISTS idx_ft_test ON flaky_tests(test_id, last_seen_at);
CREATE INDEX IF NOT EXISTS idx_ft_quarantine ON flaky_tests(quarantine, last_seen_at);

CREATE TABLE IF NOT EXISTS flaky_fix_attempts (
  id              TEXT PRIMARY KEY,
  flaky_test_id   TEXT NOT NULL REFERENCES flaky_tests(id),
  profile         TEXT NOT NULL,
  status          TEXT NOT NULL DEFAULT 'running',  -- 'running'|'success'|'failed'|'budget'
  stop_reason     TEXT,                      -- 'success'|'infeasibility'|'max_turns'|'max_cost'|'max_seconds'
  turns_used      INTEGER NOT NULL DEFAULT 0,
  cost_usd        REAL NOT NULL DEFAULT 0.0,
  wall_seconds    INTEGER NOT NULL DEFAULT 0,
  root_cause      TEXT,                      -- agent's diagnosis summary (free text)
  patch_path      TEXT,                      -- path to .tag/patches/{slug}.diff if partial
  branch          TEXT,                      -- git branch if fix committed
  pr_url          TEXT,                      -- GitHub PR URL if --open-pr
  validation_runs INTEGER NOT NULL DEFAULT 0,
  validation_pass INTEGER NOT NULL DEFAULT 0,
  created_at      TEXT NOT NULL,
  completed_at    TEXT
);
CREATE INDEX IF NOT EXISTS idx_ffa_test ON flaky_fix_attempts(flaky_test_id, created_at);
CREATE INDEX IF NOT EXISTS idx_ffa_status ON flaky_fix_attempts(status, created_at);
```

### 9.3 Core Dataclasses

```python
# src/tag/flaky_detector.py
from __future__ import annotations

import dataclasses
from typing import Literal

Outcome = Literal["passed", "failed", "error", "skipped"]


@dataclasses.dataclass
class TestResult:
    """Result of a single test case from one JUnit XML run."""
    test_id: str          # pytest node ID: "tests/test_auth.py::TestClass::test_method"
    test_file: str        # relative path
    test_class: str | None
    test_name: str        # bare function name
    outcome: Outcome
    duration_ms: int
    error_type: str | None       # exception class name
    error_message: str | None    # truncated to 2000 chars
    traceback: str | None        # truncated to 4000 chars


@dataclasses.dataclass
class FlakyTest:
    """A test identified as exhibiting non-deterministic outcomes."""
    test_id: str
    test_file: str
    test_name: str
    disruption_pct: float          # failures / total_runs * 100
    pass_count: int
    fail_count: int
    error_count: int
    quarantine: bool               # True if disruption_pct >= quarantine_threshold
    # Representative error context for agent
    sample_tracebacks: list[str]   # up to 3 distinct tracebacks
    sample_error_types: list[str]


@dataclasses.dataclass
class FlakyRunConfig:
    """Configuration for a single flaky detection run."""
    repo: str
    commit_sha: str
    runner_cmd: str
    num_runs: int
    threshold: float = 0.30
    quarantine_threshold: float = 0.50
    source: Literal["local", "github_actions"] = "local"
    file_filter: str | None = None
    test_filter: str | None = None
    dry_run: bool = False
    sandbox: Literal["docker", "none"] = "none"


@dataclasses.dataclass
class FlakyReport:
    """Top-level output structure for --format json."""
    run_id: str
    repo: str
    commit_sha: str
    num_runs: int
    total_tests: int
    flaky_tests: list[FlakyTest]
    quarantined_count: int
    generated_at: str   # ISO-8601 UTC


@dataclasses.dataclass
class AgentLoopConfig:
    """Budget constraints for a single remediation agent. Mirrors PRD-055 pattern."""
    max_turns: int = 20
    max_cost_usd: float = 5.00
    max_wall_seconds: int = 300
    max_diff_lines: int = 200
    blocked_commands: list[str] = dataclasses.field(
        default_factory=lambda: ["git push", "rm -rf", "curl", "wget"]
    )


@dataclasses.dataclass
class RemediationResult:
    """Outcome of a single remediation agent run."""
    flaky_test_id: str
    status: Literal["success", "failed", "budget"]
    stop_reason: str
    turns_used: int
    cost_usd: float
    wall_seconds: int
    root_cause: str | None
    patch_path: str | None
    branch: str | None
    pr_url: str | None
    validation_pass: int
    validation_total: int
```

### 9.4 ACI Tool Harness

```python
# src/tag/aci_tools.py
"""ACI (Agent-Computer Interface) tool harness for TAG code editing.

Implements the windowed file viewer + line-targeted edit + lint-on-edit pattern
from SWE-agent (arXiv:2405.15793). Each function is a tool callable by the
remediation agent; all state is maintained in an ACIState dataclass.
"""
from __future__ import annotations

import dataclasses
import subprocess
from pathlib import Path


WINDOW_SIZE = 100


@dataclasses.dataclass
class ACIState:
    current_file: Path | None = None
    first_line: int = 1     # 1-based, top of current window


def aci_open(state: ACIState, file: str, lineno: int = 1) -> str:
    """Open a file at lineno; return a windowed view of WINDOW_SIZE lines."""
    path = Path(file)
    if not path.exists():
        return f"ERROR: file not found: {file}"
    lines = path.read_text(encoding="utf-8").splitlines()
    state.current_file = path
    state.first_line = max(1, lineno)
    return _render_window(lines, state.first_line)


def aci_scroll(state: ACIState, direction: str) -> str:
    """Scroll current window up or down by WINDOW_SIZE lines."""
    if state.current_file is None:
        return "ERROR: no file open; call aci_open first"
    lines = state.current_file.read_text(encoding="utf-8").splitlines()
    if direction == "down":
        state.first_line = min(len(lines), state.first_line + WINDOW_SIZE)
    else:
        state.first_line = max(1, state.first_line - WINDOW_SIZE)
    return _render_window(lines, state.first_line)


def aci_goto(state: ACIState, lineno: int) -> str:
    """Jump to a specific line in the current file."""
    if state.current_file is None:
        return "ERROR: no file open"
    lines = state.current_file.read_text(encoding="utf-8").splitlines()
    state.first_line = max(1, min(lineno, len(lines)))
    return _render_window(lines, state.first_line)


def aci_edit(state: ACIState, start_line: int, end_line: int, replacement: str) -> str:
    """Replace lines start_line..end_line (1-based, inclusive) with replacement.

    Runs ruff/flake8 after edit; returns the lint output if lint fails so the
    agent can correct the edit. Returns 'OK' on success.
    """
    if state.current_file is None:
        return "ERROR: no file open"
    path = state.current_file
    lines = path.read_text(encoding="utf-8").splitlines(keepends=True)
    # Validate bounds
    if start_line < 1 or end_line > len(lines) or start_line > end_line:
        return f"ERROR: invalid range {start_line}:{end_line} (file has {len(lines)} lines)"
    new_lines = (
        lines[:start_line - 1]
        + [replacement if replacement.endswith("\n") else replacement + "\n"]
        + lines[end_line:]
    )
    new_content = "".join(new_lines)
    # Write atomically via temp file
    tmp = path.with_suffix(".aci_tmp")
    tmp.write_text(new_content, encoding="utf-8")
    # Lint validation
    lint_result = subprocess.run(
        ["ruff", "check", "--quiet", str(tmp)],
        capture_output=True, text=True
    )
    if lint_result.returncode != 0:
        tmp.unlink(missing_ok=True)
        return f"LINT_ERROR: edit rejected\n{lint_result.stdout}\n{lint_result.stderr}"
    tmp.replace(path)
    state.first_line = max(1, start_line)
    return "OK"


def aci_search(pattern: str, path: str = ".") -> str:
    """Search for a regex pattern in files under path; return matching file:line context."""
    result = subprocess.run(
        ["grep", "-rn", "--include=*.py", "-m", "20", pattern, path],
        capture_output=True, text=True
    )
    return result.stdout[:4000] if result.stdout else "No matches found."


def _render_window(lines: list[str], first_line: int) -> str:
    end = min(len(lines), first_line + WINDOW_SIZE - 1)
    numbered = [
        f"{i+1:>6} | {lines[i]}" for i in range(first_line - 1, end)
    ]
    header = f"[File: {first_line}-{end} of {len(lines)} lines]"
    return header + "\n" + "\n".join(numbered)
```

### 9.5 Detection Algorithm

```python
# src/tag/flaky_detector.py (core detection logic)

def compute_flaky_tests(
    results: list[list[TestResult]],   # outer = run_index, inner = test results
    config: FlakyRunConfig,
) -> list[FlakyTest]:
    """Apply disruption-percentage analysis to detect flaky tests.

    Algorithm:
      1. Aggregate outcomes per test_id across all run_index slices.
      2. A test is a candidate if it has >= 1 pass AND >= 1 fail/error.
      3. disruption_pct = (fail_count + error_count) / total_runs * 100
      4. Filter by threshold; set quarantine flag.
      5. Collect up to 3 distinct tracebacks as agent context.
    """
    from collections import defaultdict
    per_test: dict[str, list[TestResult]] = defaultdict(list)
    for run in results:
        for r in run:
            per_test[r.test_id].append(r)

    flaky = []
    for test_id, runs in per_test.items():
        total = len(runs)
        passes = sum(1 for r in runs if r.outcome == "passed")
        failures = sum(1 for r in runs if r.outcome in ("failed", "error"))
        skips = sum(1 for r in runs if r.outcome == "skipped")
        errors = sum(1 for r in runs if r.outcome == "error")

        # Flip signal: must have both passing and non-passing outcomes
        if passes == 0 or failures == 0:
            continue

        disruption_pct = failures / total * 100
        if disruption_pct < config.threshold * 100:
            continue

        # Collect distinct tracebacks for agent context
        tracebacks = list({r.traceback for r in runs if r.traceback})[:3]
        error_types = list({r.error_type for r in runs if r.error_type})

        flaky.append(FlakyTest(
            test_id=test_id,
            test_file=runs[0].test_file,
            test_name=runs[0].test_name,
            disruption_pct=disruption_pct,
            pass_count=passes,
            fail_count=failures,
            error_count=errors,
            quarantine=disruption_pct >= config.quarantine_threshold * 100,
            sample_tracebacks=tracebacks,
            sample_error_types=error_types,
        ))

    return sorted(flaky, key=lambda t: t.disruption_pct, reverse=True)
```

### 9.6 JUnit XML Ingestion

```python
# src/tag/ci.py additions

import xml.etree.ElementTree as ET  # stdlib; supplemented by defusedxml at runtime


def parse_junit_xml(xml_path: str) -> list[TestResult]:
    """Parse a JUnit XML file and return a list of TestResult objects.

    Supports pytest, JUnit4, and Surefire report formats.
    Uses defusedxml if available to prevent entity expansion attacks on
    untrusted CI artifacts.
    """
    try:
        import defusedxml.ElementTree as SafeET
        tree = SafeET.parse(xml_path)
    except ImportError:
        tree = ET.parse(xml_path)

    root = tree.getroot()
    results = []

    # Handle both <testsuites><testsuite>... and bare <testsuite>...
    suites = root.findall(".//testsuite") if root.tag == "testsuites" else [root]

    for suite in suites:
        suite_name = suite.get("name", "")
        for case in suite.findall("testcase"):
            classname = case.get("classname", suite_name)
            name = case.get("name", "")
            duration_ms = int(float(case.get("time", "0")) * 1000)

            # Build pytest node ID
            if classname and classname != name:
                test_id = f"{classname.replace('.', '/')}::{name}"
            else:
                test_id = name

            # Determine outcome
            failure = case.find("failure")
            error = case.find("error")
            skipped = case.find("skipped")

            if failure is not None:
                outcome = "failed"
                error_type = failure.get("type", "AssertionError")
                error_message = (failure.get("message") or "")[:2000]
                traceback = (failure.text or "")[:4000]
            elif error is not None:
                outcome = "error"
                error_type = error.get("type", "Exception")
                error_message = (error.get("message") or "")[:2000]
                traceback = (error.text or "")[:4000]
            elif skipped is not None:
                outcome = "skipped"
                error_type = error_message = traceback = None
            else:
                outcome = "passed"
                error_type = error_message = traceback = None

            # Extract test_file from classname heuristic
            test_file = classname.replace(".", "/") + ".py" if classname else ""

            results.append(TestResult(
                test_id=test_id,
                test_file=test_file,
                test_class=classname or None,
                test_name=name,
                outcome=outcome,
                duration_ms=duration_ms,
                error_type=error_type,
                error_message=error_message,
                traceback=traceback,
            ))

    return results
```

### 9.7 GitHub Actions Artifact Download

```python
# src/tag/ci.py additions

def download_ci_junit_artifacts(
    repo: str,
    workflow_name: str,
    artifact_name_pattern: str,
    num_runs: int,
    dest_dir: str,
) -> list[tuple[int, str, str]]:
    """Download JUnit XML artifacts from the last num_runs GitHub Actions runs.

    Returns list of (run_id, commit_sha, xml_path) tuples.
    Uses `gh run list` and `gh run download` via subprocess.
    """
    import json as _json
    import tempfile

    list_result = subprocess.run(
        [
            "gh", "run", "list",
            "--repo", repo,
            "--workflow", workflow_name,
            "--limit", str(num_runs),
            "--json", "databaseId,headSha,status,conclusion",
        ],
        capture_output=True, text=True
    )
    if list_result.returncode != 0:
        raise RuntimeError(f"gh run list failed: {list_result.stderr.strip()}")

    runs = _json.loads(list_result.stdout)
    results = []
    for run in runs:
        run_id = run["databaseId"]
        commit_sha = run["headSha"]
        run_dest = Path(dest_dir) / str(run_id)
        run_dest.mkdir(parents=True, exist_ok=True)

        dl_result = subprocess.run(
            [
                "gh", "run", "download", str(run_id),
                "--repo", repo,
                "--name", artifact_name_pattern,
                "--dir", str(run_dest),
            ],
            capture_output=True, text=True
        )
        if dl_result.returncode != 0:
            continue  # skip runs with missing artifacts, log warning

        # Find XML files recursively
        for xml_file in run_dest.rglob("*.xml"):
            results.append((run_id, commit_sha, str(xml_file)))

    return results
```

### 9.8 Integration Points

| System | Integration |
|--------|-------------|
| `ci.py` | `parse_junit_xml`, `download_ci_junit_artifacts`, `fetch_pr_metadata` for `--open-pr` |
| `loop_agent.py` | Remediation agent loop reuses `AgentLoopConfig`-style budget enforcement; agent subprocess is launched per flaky test |
| `sandbox.py` (PRD-028) | Test runner invocations in `--sandbox docker` mode route through `sandbox.run_command()` |
| `budget.py` (PRD-012/039) | `BudgetTracker` tracks per-agent cost; integration with `--max-cost` enforcer |
| `tracing.py` (PRD-013) | `tracer.start_span("flaky_fix.detect")` wraps the full detection phase; child spans per JUnit parse and per agent |
| `security.py` (PRD-034) | `mask_secrets(text)` applied to all error messages and tracebacks before writing to SQLite or passing to agent |
| `diff_context.py` (PRD-038) | Diff-aware context injection for the agent: the test file diff since the last green CI run is prepended to agent context |
| `notifications.py` (PRD-040) | On completion, notify via configured channels: `--open-pr` result, quarantine summary, budget exhaustion |
| `otel_semconv.py` (PRD-041) | `FLAKY_TEST_ID`, `FLAKY_DISRUPTION_PCT`, `AGENT_STOP_REASON` semantic conventions added |

---

## 10. Security Considerations

1. **XML Entity Expansion**: JUnit XML files from CI systems are third-party inputs and MUST be parsed with `defusedxml` to prevent XML entity expansion (billion laughs) attacks. The fallback to stdlib `xml.etree.ElementTree` is acceptable only when `defusedxml` is not installed and the XML source is trusted (local test runner output).

2. **Secret Masking in Tracebacks**: Test failure tracebacks often include API keys, tokens, and database connection strings printed by the test framework. All traceback text MUST be passed through `security.py`'s `mask_secrets()` before being written to SQLite or sent to the LLM agent context. The masking patterns from PRD-034 (regex-based credential detection) apply here.

3. **Agent Command Injection via Test Node IDs**: Test node IDs from JUnit XML (attacker-controlled in adversarial scenarios) are used to construct branch names and commit messages. Node IDs MUST be sanitized: only alphanumeric characters, hyphens, underscores, slashes, and double colons are permitted; all other characters are stripped before any shell interpolation.

4. **`--sandbox docker` Isolation**: When running tests for remediation validation in `--sandbox docker` mode, the Docker container MUST have no network access (unless the test suite requires it, in which case `--allow-network` must be explicitly set). The host filesystem MUST be mounted read-only except for the repository directory.

5. **GitHub Token Scope**: The `--last-ci-run` and `--open-pr` modes use the `gh` CLI's authenticated token. TAG MUST NOT prompt users to provide a GitHub PAT directly; it relies solely on `gh auth status`. If the token lacks required scopes (`repo`, `actions:read`), TAG emits a clear error with the required scope list.

6. **Agent Patch Safety**: Before applying a remediation patch (committing to a branch), the patch MUST be checked against the `blocked_commands` list in `AgentLoopConfig`. If the diff contains any modifications outside the test file(s) identified during detection, the user MUST be prompted for confirmation even in `--auto-fix` mode.

7. **Rate Limiting on LLM Calls**: Multiple remediation agents running in sequence could rapidly exhaust API rate limits. The `flaky_agent.py` orchestrator MUST enforce a minimum 1-second delay between agent invocations when running more than 5 agents, and MUST respect `429` / `Retry-After` responses from the LLM provider.

8. **SQLite Injection**: All SQLite queries MUST use parameterized statements (`?` placeholders); no string interpolation of user-controlled data into SQL text. Test node IDs, repo names, and branch names are all user/environment-controlled.

---

## 11. Testing Strategy

### 11.1 Unit Tests

| Test | Coverage |
|------|---------|
| `test_compute_flaky_tests_basic` | Test with 5 runs: 3 pass, 2 fail → disruption=40%, not quarantined |
| `test_compute_flaky_tests_quarantine` | Test with 5 runs: 1 pass, 4 fail → disruption=80%, quarantined |
| `test_compute_flaky_tests_deterministic` | Test with 5 pass, 0 fail → not in flaky set |
| `test_compute_flaky_tests_threshold_filter` | disruption=15% with threshold=0.30 → excluded |
| `test_parse_junit_xml_pytest` | Parse real pytest JUnit XML; verify TestResult fields |
| `test_parse_junit_xml_junit4` | Parse JUnit4-style XML with classname attributes |
| `test_parse_junit_xml_defusedxml` | XXE attack payload is rejected when `defusedxml` is available |
| `test_aci_open` | Returns windowed view starting at lineno with correct line numbers |
| `test_aci_edit_success` | Edit is applied; lint passes; file content updated |
| `test_aci_edit_lint_failure` | Bad Python syntax → lint rejects edit; file unchanged |
| `test_aci_edit_bounds_check` | start_line > total_lines → returns error string |
| `test_secret_masking_in_traceback` | AWS key in traceback is masked before SQLite write |
| `test_test_node_id_sanitization` | Malicious node ID with shell metacharacters is sanitized for branch name |
| `test_flaky_report_json_schema` | FlakyReport serializes to valid JSON with all required fields |
| `test_de_quarantine_threshold` | 18/20 passes (90%) → de-quarantined; 17/20 (85%) → stays quarantined |

### 11.2 Integration Tests

| Test | Description |
|------|------------|
| `test_end_to_end_detection_local` | Inject a known-flaky test using `random.random()` into a temp pytest suite; run with `--runs 5`; verify detection |
| `test_end_to_end_detection_file_filter` | `--file tests/test_a.py` only analyzes tests from that file |
| `test_remediation_agent_time_based_flaky` | Agent correctly identifies `datetime.now()` without timezone and applies `timezone.utc` fix |
| `test_remediation_agent_budget_exhaustion` | Agent with `--max-turns 2` on a hard problem terminates gracefully, writes patch file |
| `test_remediation_validation_rerun` | After fix, validation reruns confirm determinism; `flaky_fix_attempts.validation_pass = 3` |
| `test_sqlite_persistence` | After detection, `flaky_runs`, `flaky_tests`, and `test_run_results` rows exist with correct values |
| `test_de_quarantine_integration` | After injecting 20 passing result rows, `--de-quarantine` updates `quarantine=0` |
| `test_dry_run_no_writes` | `--dry-run` prints output but no rows in `flaky_tests` table |
| `test_json_output_valid` | `--format json` output parses as valid `FlakyReport` |

### 11.3 Performance Tests

| Benchmark | Target | Method |
|-----------|--------|--------|
| JUnit XML parse latency | < 500ms per 1,000 tests | `timeit` over synthetic XML |
| SQLite write throughput | 10,000 `test_run_results` rows in < 2s | Bulk insert benchmark |
| Detection loop memory | < 200MB peak for 500-test / 5-run suite | `tracemalloc` |
| Agent startup overhead | < 1s per remediation agent (subprocess spawn) | Timing test |

### 11.4 Benchmark Repo for Recall Testing

A synthetic benchmark repository at `tests/fixtures/flaky_benchmark/` contains 20 tests with injected flakiness patterns:
- `random.random()` comparison without seed
- `datetime.now()` used in assertion
- `time.sleep(0.01)` too tight for CI jitter
- Shared mutable module-level state between tests
- Network call without mock (fails when network unavailable)

The benchmark tests are tagged with `@pytest.mark.flaky` for ground-truth labeling. `test_detection_recall` asserts that detection at `--runs 5` captures >= 15/20 of these.

---

## 12. Acceptance Criteria

| ID | Criterion | Pass Condition |
|----|-----------|---------------|
| AC-01 | `tag ci flaky-fix --runs 5` detects a known-flaky test that fails in 2 of 5 runs | Test appears in output with disruption_pct=40.0% |
| AC-02 | `tag ci flaky-fix --runs 5` on a deterministic suite exits 0 with "0 flaky tests detected" | Exit code 0; no `flaky_tests` rows inserted |
| AC-03 | A test failing in 4/5 runs (80%) is marked `quarantine=true` in the report and DB | `flaky_tests.quarantine=1`, report shows "quarantined" |
| AC-04 | `--threshold 0.50` excludes a test failing in 2/5 runs (40% disruption) from the output | Test not present in `flaky_tests` table |
| AC-05 | `--auto-fix` on a time-based flaky test produces a committed branch with a green validation run | `flaky_fix_attempts.status='success'`, branch exists in local git |
| AC-06 | Agent exceeding `--max-turns 5` terminates, writes `.tag/patches/{slug}.diff`, exits gracefully | `stop_reason='max_turns'`, patch file exists, process exits 0 |
| AC-07 | `aci_edit` with invalid Python syntax is rejected by lint; file is unchanged | Return value starts with "LINT_ERROR:"; file diff is empty |
| AC-08 | `--last-ci-run --repo owner/repo` downloads 10 CI run artifacts and computes disruption correctly | `flaky_runs.source='github_actions'`; `flaky_runs.num_runs=10` |
| AC-09 | `--format json` output is valid JSON and conforms to the `FlakyReport` schema | `json.loads()` succeeds; all required keys present |
| AC-10 | `--de-quarantine` promotes a test with 19/20 passing recent runs; does not promote one with 17/20 | Only the 19/20 test has `quarantine=0` after the command |
| AC-11 | An AWS access key in a test traceback is masked before SQLite write and agent context | `test_run_results.error_message` does not contain `AKIA` patterns |
| AC-12 | `--dry-run` mode prints detection results but writes zero rows to SQLite | `flaky_runs` table empty after dry-run |
| AC-13 | `--open-pr` creates a GitHub PR and stores the URL in `flaky_fix_attempts.pr_url` | `gh pr view` returns 200; DB row has `pr_url` set |
| AC-14 | `tag ci flaky-fix --history test_login_timeout` prints a trend table with at least 3 historical data points | Output contains the test node ID and timestamp column |
| AC-15 | OTel span `flaky_fix.detect` is emitted with attributes `flaky.run_id`, `flaky.num_runs`, `flaky.flaky_count` | `tracing.py` span captured in test exporter |

---

## 13. Dependencies

| Dependency | Type | Usage | Notes |
|------------|------|-------|-------|
| `defusedxml` | Python package (optional) | Safe JUnit XML parsing | Graceful fallback to stdlib ET for local runs |
| `ruff` or `flake8` | CLI tool | Lint validation after `aci_edit` | `ruff` preferred; falls back to `flake8` |
| `pytest` | CLI tool | Test runner for local detection runs | Already a TAG dev dependency |
| `gh` CLI | System binary | `gh run list`, `gh run download`, `gh pr create` | Already required by `ci.py` |
| `git` | System binary | Branch creation, commit, diff generation | Already required |
| `docker` | System binary (optional) | `--sandbox docker` test isolation | Only needed when sandbox mode is requested |
| PRD-013 tracing | Internal | OTel span emission | Required for observability |
| PRD-021 loop_agent | Internal | Remediation agent loop pattern | `AgentLoopConfig` budget enforcement |
| PRD-028 sandbox | Internal | `--sandbox docker` test execution isolation | Optional at runtime |
| PRD-034 security | Internal | `mask_secrets()` for tracebacks | Required for secret safety |
| PRD-038 diff_context | Internal | Diff-aware context for agent | Improves agent effectiveness |
| PRD-039 budget | Internal | Per-agent cost tracking and enforcement | Required for `--max-cost` |
| PRD-041 otel_semconv | Internal | Flaky-test-specific semantic conventions | Required for structured spans |

---

## 14. Open Questions

| ID | Question | Owner | Resolution Target |
|----|----------|-------|------------------|
| OQ-01 | Should `--last-ci-run` support GitLab CI pipelines (via GitLab CI Lint API / artifact download) in addition to GitHub Actions? GitLab has a significant enterprise user base. | Platform team | v2 scope decision |
| OQ-02 | Should the remediation agent be allowed to modify test fixtures and conftest.py, or only the test file itself? Broader edits are more effective but riskier (false positives). | Core team | Before GA |
| OQ-03 | Should detected flaky tests automatically create Linear or Jira tickets with the flakiness report attached? This would require Linear/Jira API credentials and PRD-016 webhook integration. | Product team | v2 stretch goal |
| OQ-04 | Is a minimum of 3 runs sufficient for statistical confidence in low-flakiness tests (e.g., 5% failure rate)? Cluster research recommends 10+ runs for production flakiness databases. Should `--runs` have a minimum of 5 with a warning below 5? | Engineering team | Before Beta |
| OQ-05 | Should `--auto-fix` by default open a single PR with all fixes, or one PR per flaky test? One-PR-per-test is safer for review but noisier; a single batched PR is lower friction. | Product team | UX decision before GA |
| OQ-06 | Can the ACI tool harness (`aci_tools.py`) be shared with PRD-055 (issue-to-PR loop) to avoid duplication? Both features need the same windowed-editor tools. | Core team | Pre-implementation |
| OQ-07 | Should `flaky_tests` have a foreign key to a repository table (for multi-repo environments) or is the `repo` string in `flaky_runs` sufficient? Multi-repo support may require schema change. | Data team | Before schema freeze |
| OQ-08 | What is the right UX for a test that is quarantined AND has a fix branch open? Should the de-quarantine check automatically close the branch? | Core team | Before GA |

---

## 15. Complexity and Timeline

**Overall Estimate:** L — 2-4 weeks for a single engineer

### Phase 1: Detection Engine (Days 1-5)

- [ ] SQLite DDL: create `flaky_runs`, `test_run_results`, `flaky_tests` tables in `open_db()` migration
- [ ] `parse_junit_xml()` in `ci.py` with `defusedxml` support and bounds-checked field truncation
- [ ] `FlakyRunConfig`, `TestResult`, `FlakyTest`, `FlakyReport` dataclasses in `flaky_detector.py`
- [ ] `compute_flaky_tests()` statistical analysis function with threshold + quarantine logic
- [ ] Local test runner orchestration: N-run loop with JUnit XML capture
- [ ] Unit tests for detection algorithm (deterministic, flaky, threshold, quarantine cases)

### Phase 2: CLI Surface + Reporting (Days 6-9)

- [ ] `cmd_ci_flaky_fix()` in `controller.py` with full flag parsing
- [ ] `--format table` (Rich table), `--format json` (FlakyReport serialization), `--format markdown` outputs
- [ ] `--dry-run` mode (no DB writes, no agent invocations)
- [ ] `--last-ci-run` mode: `download_ci_junit_artifacts()` via `gh` CLI
- [ ] `--history` subcommand querying historical `flaky_tests` rows
- [ ] Exit code semantics: 0 (clean), 1 (error), 2 (flaky detected, no fix)
- [ ] Integration tests for end-to-end detection with synthetic flaky benchmark

### Phase 3: ACI Tool Harness (Days 10-13)

- [ ] `aci_tools.py`: `aci_open`, `aci_scroll`, `aci_goto`, `aci_edit`, `aci_search`
- [ ] Lint validation hook in `aci_edit` (ruff primary, flake8 fallback)
- [ ] `ACIState` dataclass with `current_file` + `first_line` state
- [ ] Unit tests for all ACI tools including lint rejection and bounds checking
- [ ] Coordinate with PRD-055 team on potential shared `aci_tools.py` (OQ-06)

### Phase 4: Remediation Agent + Budget Enforcement (Days 14-18)

- [ ] `flaky_agent.py`: `RemediationResult`, `AgentLoopConfig`, agent context assembly
- [ ] Per-agent subprocess launch with three stopping conditions (success, failure, budget)
- [ ] `flaky_fix_attempts` table writes (start, turn updates, completion)
- [ ] `--validation-runs` rerun check after agent proposes a fix
- [ ] Patch write to `.tag/patches/{slug}.diff` on budget exhaustion or failure
- [ ] Branch creation + commit on success; `--open-pr` via `gh pr create`
- [ ] Integration tests: time-based flaky fix, budget exhaustion, validation rerun

### Phase 5: Observability + Security (Days 19-21)

- [ ] OTel spans: `flaky_fix.detect`, `flaky_fix.parse_junit`, `flaky_fix.agent.*`, `flaky_fix.validate`
- [ ] `otel_semconv.py` additions: `FLAKY_TEST_ID`, `FLAKY_DISRUPTION_PCT`, `AGENT_STOP_REASON`
- [ ] Secret masking integration in `flaky_agent.py` context assembly
- [ ] Test node ID sanitization for branch names and commit messages
- [ ] `--de-quarantine` command with pass-rate threshold check
- [ ] `notifications.py` integration: completion summary on configured channels

### Phase 6: Polish + GA (Days 22-25)

- [ ] Performance benchmarks: XML parse latency, SQLite write throughput, memory ceiling
- [ ] `defusedxml` optional dependency handling with graceful fallback
- [ ] `tag ci flaky-fix --de-quarantine --dry-run` mode
- [ ] Documentation update: `docs/ci-flaky-fix.md` usage guide
- [ ] Acceptance criteria verification across all AC-01 through AC-15
- [ ] Code review against security checklist (items 1-8 in Section 10)

**Risk factors:**
- ACI tool harness sharing with PRD-055 (OQ-06) could add coordination overhead if not resolved early — resolve by Day 5.
- `defusedxml` adding a new optional dependency may require pyproject.toml updates and CI matrix changes.
- `--sandbox docker` mode depends on PRD-028 API stability; if PRD-028 is mid-refactor, defer sandbox support to Phase 6.
- Statistical minimum sample size debate (OQ-04) should be resolved before Phase 1 ships to avoid schema migration for a minimum-runs constraint.

